package importer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestImportKazumiXPathConvertsSearchAndEpisodes(t *testing.T) {
	result := ImportKazumi([]byte(`{
		"api":"4", "type":"anime", "name":"XPath Rule", "version":"1.0",
		"baseURL":"https://video.example.org/",
		"searchURL":"https://video.example.org/search?wd=@keyword",
		"searchList":"//article", "searchName":"//h2/a", "searchResult":"//h2/a",
		"chapterRoads":"//div[@class='road']", "chapterResult":"//a",
		"muliSources":true, "referer":"https://video.example.org/"
	}`), testOptions())

	if result.HasErrors() || len(result.Sources) != 1 {
		t.Fatalf("import returned unexpected result: %+v", result)
	}
	source := result.Sources[0]
	request := objectValue(objectValue(source["search"])["request"])
	if request["url"] != "https://video.example.org/search?wd={{title}}" {
		t.Fatalf("unexpected search URL: %v", request["url"])
	}
	lines := objectValue(objectValue(source["episodes"])["lines"])
	episodes := objectValue(lines["episodes"])
	fields := objectValue(episodes["fields"])
	if objectValue(fields["play_url"])["attribute"] != "href" {
		t.Fatalf("XPath play URL did not retain href extraction: %#v", fields["play_url"])
	}
	assertValidSource(t, source)
}

func TestImportKazumiAPIConvertsBodyVariablesAndEpisodePage(t *testing.T) {
	result := ImportKazumi([]byte(`{
		"api":"8", "type":"anime", "name":"API Rule", "version":"2.1",
		"baseURL":"https://watch.example.org/", "searchMode":"api", "chapterMode":"api",
		"searchApiConfig":{
			"request":{"method":"POST","url":"https://api.example.org/search","bodyType":"json","body":{"q":"@keyword","page":1}},
			"listPath":"$.items[*]","namePath":"$.name","sourcePath":"$.id"
		},
		"chapterApiConfig":{
			"request":{"method":"GET","url":"https://api.example.org/anime/@source"}, "format":"nested",
			"roadsPath":"$.data.roads[*]","roadNamePath":"$.name","episodesPath":"$.episodes[*]",
			"episodeNamePath":"$.name","episodeUrlPath":"$.slug","variables":{"seriesSlug":"$.data.slug"},
			"episodePage":{"url":"https://watch.example.org/@seriesSlug/@episodeUrl","query":{"road":"@roadIndex","episode":"@episodeNumber"}}
		}
	}`), testOptions())

	if result.HasErrors() || len(result.Sources) != 1 {
		t.Fatalf("import returned unexpected result: %+v", result)
	}
	source := result.Sources[0]
	searchRequest := objectValue(objectValue(source["search"])["request"])
	body := stringValue(searchRequest["body"])
	if !strings.Contains(body, `"q":"{{title}}"`) || searchRequest["method"] != "POST" {
		t.Fatalf("API JSON body was not converted: %v", searchRequest)
	}
	episodes := objectValue(source["episodes"])
	if _, ok := objectValue(episodes["variables"])["series_slug"]; !ok {
		t.Fatalf("API response variable was not normalized: %#v", episodes["variables"])
	}
	fields := objectValue(objectValue(objectValue(episodes["lines"])["episodes"])["fields"])
	playURL := stringValue(objectValue(fields["play_url"])["expression"])
	for _, fragment := range []string{"{{series_slug}}", "{{episode_url}}", "road={{line_index}}", "episode={{episode_number}}"} {
		if !strings.Contains(playURL, fragment) {
			t.Fatalf("episode page %q does not contain %q", playURL, fragment)
		}
	}
	assertValidSource(t, source)
}

func TestImportKazumiDisablesInteractiveDeprecatedAndCredentialRules(t *testing.T) {
	result := ImportKazumi([]byte(`{
		"api":"8", "type":"anime", "name":"Guarded API", "version":"1.0", "deprecated":true,
		"baseURL":"https://watch.example.org/", "searchMode":"api", "chapterMode":"api",
		"searchApiConfig":{
			"request":{"method":"GET","url":"https://api.example.org/search","headers":{"Authorization":"Bearer fixed","X-Public":"yes"}},
			"listPath":"$.items[*]","namePath":"$.name","sourcePath":"$.id"
		},
		"chapterApiConfig":{
			"request":{"method":"GET","url":"https://api.example.org/anime/@source"},
			"format":"nested","roadsPath":"$.roads[*]","roadNamePath":"$.name","episodesPath":"$.episodes[*]","episodeNamePath":"$.name","episodeUrlPath":"$.url"
		},
		"antiCrawlerConfig":{"enabled":true,"captchaScript":"unsafe()"}
	}`), testOptions())

	if result.HasErrors() || len(result.Sources) != 1 {
		t.Fatalf("guarded rule should be safely importable: %+v", result)
	}
	if result.Sources[0]["enabled"] != false {
		t.Fatal("guarded source was not disabled")
	}
	headers := objectValue(objectValue(objectValue(result.Sources[0]["search"])["request"])["headers"])
	if _, ok := headers["Authorization"]; ok || headers["X-Public"] != "yes" {
		t.Fatalf("credential filtering returned unexpected headers: %#v", headers)
	}
	for _, category := range []string{"deprecated_source", "interactive_required", "sensitive_header_dropped"} {
		if !containsCategory(result.Diagnostics, category) {
			t.Fatalf("missing %s diagnostic: %+v", category, result.Diagnostics)
		}
	}
	assertValidSource(t, result.Sources[0])
}

func TestImportKazumiRejectsDelimitedChapterFormat(t *testing.T) {
	result := ImportKazumi([]byte(`{
		"api":"8", "type":"anime", "name":"Delimited", "version":"1.0",
		"baseURL":"https://watch.example.org/", "searchMode":"api", "chapterMode":"api",
		"searchApiConfig":{"request":{"url":"https://api.example.org/search"},"listPath":"$.items[*]","namePath":"$.name","sourcePath":"$.id"},
		"chapterApiConfig":{"request":{"url":"https://api.example.org/anime/@source"},"format":"delimited"}
	}`), testOptions())
	if !result.HasErrors() || len(result.Sources) != 0 || !containsCategory(result.Diagnostics, "unsupported_rule") {
		t.Fatalf("delimited format unexpectedly imported: %+v", result)
	}
}

func TestKazumiPostAndFormRequestsPreserveTemplates(t *testing.T) {
	request, _, diagnostics := kazumiXPathSearchRequest("https://post.example.org/search?wd=@keyword&page=1", true)
	if hasDiagnosticError(diagnostics) {
		t.Fatalf("XPath POST conversion failed: %+v", diagnostics)
	}
	form := objectValue(request["form"])
	if request["method"] != "POST" || request["url"] != "https://post.example.org/search" || form["wd"] != "{{title}}" || form["page"] != "1" {
		t.Fatalf("unexpected XPath POST request: %#v", request)
	}

	request, _, diagnostics = kazumiAPIRequest(map[string]any{
		"method":   "POST",
		"url":      "https://api.example.org/items/@source",
		"bodyType": "form",
		"body": map[string]any{
			"source": "@source",
			"limit":  json.Number("20"),
		},
	}, map[string]string{"source": "subject_key"}, "request")
	if hasDiagnosticError(diagnostics) {
		t.Fatalf("API form conversion failed: %+v", diagnostics)
	}
	form = objectValue(request["form"])
	if request["url"] != "https://api.example.org/items/{{subject_key}}" || form["source"] != "{{subject_key}}" || fmt.Sprint(form["limit"]) != "20" {
		t.Fatalf("unexpected API form request: %#v", request)
	}
}

func TestAnimekoAndKazumiEquivalentSiteNormalizeToOneIdentity(t *testing.T) {
	animeko := ImportAnimeko([]byte(`{
		"factoryId":"web-selector","version":2,"arguments":{"name":"Shared Site","searchConfig":{
			"searchUrl":"https://shared.example.org/search?q={keyword}","rawBaseUrl":"https://shared.example.org/",
			"subjectFormatId":"a","channelFormatId":"no-channel","matchVideo":{}
		}}
	}`), testOptions())
	kazumi := ImportKazumi([]byte(`{
		"api":"4","type":"anime","name":"Shared Site","version":"1.0","baseURL":"https://shared.example.org/",
		"searchURL":"https://shared.example.org/search?q=@keyword","searchList":"//article","searchName":"//a","searchResult":"//a",
		"chapterRoads":"//section","chapterResult":"//a"
	}`), testOptions())
	if animeko.HasErrors() || kazumi.HasErrors() || len(animeko.Sources) != 1 || len(kazumi.Sources) != 1 {
		t.Fatalf("equivalent fixtures failed to import: animeko=%+v kazumi=%+v", animeko, kazumi)
	}
	a := animeko.Sources[0]
	k := kazumi.Sources[0]
	if a["id"] != k["id"] || a["homepage"] != k["homepage"] {
		t.Fatalf("same site did not converge on one identity: animeko=%v/%v kazumi=%v/%v", a["id"], a["homepage"], k["id"], k["homepage"])
	}
	for _, source := range []map[string]any{a, k} {
		resolve := objectValue(source["resolve"])
		if source["kind"] != "web" || resolve["mode"] != "browser_sniff" || resolve["transport"] != "auto" {
			t.Fatalf("equivalent site lost its common runtime contract: %#v", source)
		}
	}
}
