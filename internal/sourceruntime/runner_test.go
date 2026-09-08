package sourceruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/btcrawler"
	"github.com/nagare-project/Nagare_Source/internal/repository"
	"github.com/nagare-project/Nagare_Source/internal/webresolver"
)

type responseFetcher struct {
	mu        sync.Mutex
	responses map[string]btcrawler.Response
	requests  []btcrawler.Request
}

func (fetcher *responseFetcher) Fetch(ctx context.Context, request btcrawler.Request) (btcrawler.Response, error) {
	if err := ctx.Err(); err != nil {
		return btcrawler.Response{}, err
	}
	fetcher.mu.Lock()
	defer fetcher.mu.Unlock()
	fetcher.requests = append(fetcher.requests, request)
	response, ok := fetcher.responses[request.URL]
	if !ok {
		return btcrawler.Response{}, fmt.Errorf("no fixture for %s", request.URL)
	}
	if response.URL == "" {
		response.URL = request.URL
	}
	return response, nil
}

func TestSpecRunnerExecutesJSONWebPipeline(t *testing.T) {
	fetcher := &responseFetcher{responses: map[string]btcrawler.Response{
		"https://api.example.test/search?q=Example+Animation": {Body: []byte(`{"items":[{"id":"series-1","title":"Example Animation"}]}`)},
		"https://api.example.test/series/series-1":            {Body: []byte(`{"slug":"example","lines":[{"name":"Main","episodes":[{"slug":"e2","label":"EP02"},{"slug":"e3","label":"EP03"}]}]}`)},
		"https://cdn.example.test/example/e3.m3u8":            {Body: []byte("#EXTM3U\n")},
	}}
	source := jsonWebSource()
	runner := NewSpecRunner(source, RunnerOptions{Fetcher: fetcher})
	request := ResolveRequest{
		Schema:  "nagare-resolve-request/v1",
		Subject: Subject{IDs: map[string]string{"bangumi": "400602"}, Titles: []string{"Example Animation"}},
		Episode: Episode{Number: "3", Absolute: pointer(3.0)},
	}
	var candidates []Candidate
	err := runner.Candidates(context.Background(), request, func(candidate Candidate) error {
		candidates = append(candidates, candidate)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates: %+v", len(candidates), candidates)
	}
	candidate := candidates[0]
	if candidate.Transport.Type != "hls" || candidate.Transport.URL != "https://cdn.example.test/example/e3.m3u8" {
		t.Fatalf("unexpected transport: %+v", candidate.Transport)
	}
	if candidate.Metadata.Channel != "Main" || candidate.Metadata.Episode == nil || *candidate.Metadata.Episode != 3 {
		t.Fatalf("line or episode metadata was lost: %+v", candidate.Metadata)
	}
	if candidate.ID != "web-json:400602:3:Main" || candidate.MatchConfidence != 1 {
		t.Fatalf("candidate identity/match is unstable: %+v", candidate)
	}
	fetcher.mu.Lock()
	defer fetcher.mu.Unlock()
	if len(fetcher.requests) != 3 || fetcher.requests[2].URL != "https://cdn.example.test/example/e3.m3u8" {
		t.Fatalf("direct media was not verified: %+v", fetcher.requests)
	}
}

func TestExtractsAnimekoCSSAndKazumiXPathShapes(t *testing.T) {
	searchStage := map[string]any{
		"response": "html", "mode": "zipped", "base_url": "https://video.example.test/",
		"fields": map[string]any{
			"title":       map[string]any{"type": "css", "expression": ".result .title", "scope": "document", "transforms": []any{"trim"}},
			"subject_url": map[string]any{"type": "css", "expression": ".result > a", "scope": "document", "attribute": "href", "transforms": []any{"absolute_url"}},
		},
	}
	rows, err := extractSearch(searchStage, "https://video.example.test/search", []byte(`
		<div class="result"><a href="/one"><span class="title"> One </span></a></div>
		<div class="result"><a href="/two"><span class="title"> Two </span></a></div>`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1]["title"] != "Two" || rows[1]["subject_url"] != "https://video.example.test/two" {
		t.Fatalf("CSS zipped extraction failed: %+v", rows)
	}

	episodeStage := map[string]any{
		"response": "html", "base_url": "https://xpath.example.test/",
		"lines": map[string]any{
			"items":  map[string]any{"type": "xpath", "expression": `//section[@class='playlist']`},
			"fields": map[string]any{"channel": map[string]any{"type": "template", "expression": "Line {{line_number}}"}},
			"episodes": map[string]any{
				"items": map[string]any{"type": "xpath", "expression": ".//a"},
				"fields": map[string]any{
					"number":   map[string]any{"type": "xpath", "expression": ".", "transforms": []any{"trim", "parse_episode"}},
					"play_url": map[string]any{"type": "xpath", "expression": ".", "attribute": "href", "transforms": []any{"absolute_url"}},
				},
			},
		},
	}
	episodes, err := extractEpisodes(episodeStage, "https://xpath.example.test/show", []byte(`
		<section class="playlist"><a href="/e2">第 2 集</a><a href="/e3">第 3 集</a></section>`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 || episodes[1]["number"] != "3" || episodes[1]["play_url"] != "https://xpath.example.test/e3" || episodes[1]["channel"] != "Line 1" {
		t.Fatalf("XPath line extraction failed: %+v", episodes)
	}
}

func TestHTMLStringExtractorsDefaultToCSSAndZippedUsesShortestRequiredColumn(t *testing.T) {
	stage := map[string]any{
		"response": "html", "mode": "zipped",
		"fields": map[string]any{
			"title": `.result .title`,
			"url":   map[string]any{"expression": `.result > a`, "attribute": "href"},
		},
	}
	rows, err := extractSearch(stage, "https://example.test/search", []byte(`
		<div class="result"><a href="/one"><span class="title">One</span></a></div>
		<div class="result"><a href="/two"><span class="title">Two</span></a></div>
		<div class="result"><span class="title">Incomplete</span></div>`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1]["title"] != "Two" || rows[1]["url"] != "/two" {
		t.Fatalf("HTML shorthand or zipped truncation is wrong: %+v", rows)
	}
}

func TestSpecRunnerConvertsBTRecordsToCandidates(t *testing.T) {
	value, err := repository.LoadDocument("../../sources/bt/example-rss.yaml")
	if err != nil {
		t.Fatal(err)
	}
	source := value.(map[string]any)
	fixture := `<?xml version="1.0"?><rss><channel><item><title>[Group] Example Animation EP03 [1080P]</title><link>magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567</link><pubDate>Fri, 29 Sep 2023 12:00:00 +0000</pubDate></item></channel></rss>`
	runner := NewSpecRunner(source, RunnerOptions{Fetcher: btcrawler.StaticFetcher{Response: btcrawler.Response{Body: []byte(fixture)}}})
	request := ResolveRequest{Schema: "nagare-resolve-request/v1", Subject: Subject{IDs: map[string]string{"test": "1"}, Titles: []string{"Example Animation"}}, Episode: Episode{Number: "3", Absolute: pointer(3.0)}}
	var candidate Candidate
	err = runner.Candidates(context.Background(), request, func(value Candidate) error { candidate = value; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Transport.Type != "torrent" || candidate.Transport.InfoHash != "0123456789abcdef0123456789abcdef01234567" || candidate.Metadata.Episode == nil || *candidate.Metadata.Episode != 3 {
		t.Fatalf("unexpected BT candidate: %+v", candidate)
	}
}

func jsonWebSource() map[string]any {
	return map[string]any{
		"schema": "nagare-source/v1", "id": "web-json", "name": "JSON Web", "kind": "web", "tier": 1, "enabled": true,
		"origin": map[string]any{"version": "1"},
		"search": map[string]any{
			"response": "json",
			"request":  map[string]any{"method": "GET", "url": "https://api.example.test/search", "query": map[string]any{"q": "{{title}}"}, "allowed_hosts": []any{"api.example.test"}},
			"items":    map[string]any{"type": "jsonpath", "expression": "$.items[*]"},
			"fields":   map[string]any{"title": "$.title", "subject_key": "$.id"},
		},
		"episodes": map[string]any{
			"response": "json", "base_url": "https://cdn.example.test/",
			"request":   map[string]any{"method": "GET", "url": "https://api.example.test/series/{{subject_key}}", "allowed_hosts": []any{"api.example.test"}},
			"variables": map[string]any{"series_slug": map[string]any{"type": "jsonpath", "expression": "$.slug", "scope": "document"}},
			"lines": map[string]any{
				"items":  map[string]any{"type": "jsonpath", "expression": "$.lines[*]"},
				"fields": map[string]any{"channel": "$.name"},
				"episodes": map[string]any{
					"items": map[string]any{"type": "jsonpath", "expression": "$.episodes[*]"},
					"fields": map[string]any{
						"number":      map[string]any{"type": "jsonpath", "expression": "$.label", "transforms": []any{"parse_episode"}},
						"episode_url": "$.slug",
						"play_url":    map[string]any{"type": "template", "expression": "https://cdn.example.test/{{series_slug}}/{{episode_url}}.m3u8"},
					},
				},
			},
		},
		"resolve":  map[string]any{"mode": "direct", "transport": "hls", "url": "{{play_url}}", "allowed_hosts": []any{"cdn.example.test"}},
		"defaults": map[string]any{"resolution": "1080P", "subtitle_languages": []any{"zh-Hans"}, "channel_tier": 0},
		"limits":   map[string]any{"concurrency": 2, "requests_per_minute": 60000, "timeout_ms": 1000, "max_response_bytes": 65536, "max_redirects": 2},
	}
}

func TestRenderRequestURLDoesNotAllowQueryInjection(t *testing.T) {
	rendered, err := renderRequestURL("https://example.test/search?q={{title}}", map[string]string{"title": "one&admin=true"})
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(rendered)
	if parsed.Query().Get("q") != "one&admin=true" || parsed.Query().Get("admin") != "" {
		t.Fatalf("URL template query was injectable: %s", rendered)
	}
	if strings.Contains(rendered, "admin=true") && !strings.Contains(rendered, "%26admin%3Dtrue") {
		t.Fatalf("query value was not encoded: %s", rendered)
	}
}

func TestStageRequestEscapesJSONTemplateValues(t *testing.T) {
	stage := map[string]any{"request": map[string]any{
		"method": "POST", "url": "https://api.example.test/search",
		"headers": map[string]any{"Content-Type": "application/json"},
		"body":    `{"keyword":"{{title}}","episode":{{episode}}}`,
	}}
	request, err := stageRequest(stage, jsonWebSource(), map[string]string{
		"title": `x"},"admin":true,"x":"`, "episode": "3",
	})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(request.Body, &document); err != nil {
		t.Fatalf("rendered request body is not JSON: %s: %v", request.Body, err)
	}
	if document["keyword"] != `x"},"admin":true,"x":"` || document["episode"] != float64(3) || len(document) != 2 {
		t.Fatalf("JSON template value escaped its field: %#v", document)
	}
}

func TestRequestIDsCannotOverrideReservedTemplateVariables(t *testing.T) {
	variables := requestVariables(jsonWebSource(), ResolveRequest{
		Subject: Subject{IDs: map[string]string{"title": "attacker-title", "source": "attacker-source"}, Titles: []string{"Trusted Title"}},
		Episode: Episode{Number: "3"},
	})
	if variables["title"] != "Trusted Title" || variables["source"] != "web-json" {
		t.Fatalf("subject IDs overrode reserved runtime variables: %#v", variables)
	}
}

func TestTemplateDoesNotReinterpretReplacementText(t *testing.T) {
	value, err := render("before {{title}} after", func(string) (string, error) {
		return "{{title}}", nil
	})
	if err != nil || value != "before {{title}} after" {
		t.Fatalf("replacement was interpreted as another template: value=%q err=%v", value, err)
	}
}

type blockingFetcher struct{}

func (blockingFetcher) Fetch(ctx context.Context, _ btcrawler.Request) (btcrawler.Response, error) {
	<-ctx.Done()
	return btcrawler.Response{}, ctx.Err()
}

func TestSpecRunnerDeadlineCoversWholePipeline(t *testing.T) {
	source := jsonWebSource()
	object(source["limits"])["timeout_ms"] = 25
	runner := NewSpecRunner(source, RunnerOptions{Fetcher: blockingFetcher{}})
	started := time.Now()
	err := runner.Candidates(context.Background(), ResolveRequest{
		Schema: "nagare-resolve-request/v1", Subject: Subject{IDs: map[string]string{"test": "1"}, Titles: []string{"Example Animation"}},
		Episode: Episode{Number: "3", Absolute: pointer(3.0)},
	}, func(Candidate) error { return nil })
	var typed *Error
	if !errors.As(err, &typed) || typed.Category != "search_timeout" {
		t.Fatalf("got %T %v, want search_timeout", err, err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("source deadline was not enforced: %s", time.Since(started))
	}
}

type mediaBrowser struct {
	mu      sync.Mutex
	request webresolver.BrowseRequest
}

func (browser *mediaBrowser) Browse(ctx context.Context, request webresolver.BrowseRequest, emit func(webresolver.NetworkEvent)) error {
	browser.mu.Lock()
	browser.request = request
	browser.mu.Unlock()
	emit(webresolver.NetworkEvent{Kind: webresolver.EventRequest, RequestID: "media", URL: "https://203.0.113.10/media/e3.m3u8", Headers: map[string]string{"Referer": request.URL}})
	emit(webresolver.NetworkEvent{Kind: webresolver.EventResponse, RequestID: "media", URL: "https://203.0.113.10/media/e3.m3u8", Status: 200, MIMEType: "application/vnd.apple.mpegurl"})
	<-ctx.Done()
	return ctx.Err()
}

func TestSpecRunnerUsesBrowserResolverForSniffSources(t *testing.T) {
	fetcher := &responseFetcher{responses: map[string]btcrawler.Response{
		"https://api.example.test/search?q=Example+Animation": {Body: []byte(`{"items":[{"id":"series-1","title":"Example Animation"}]}`)},
		"https://api.example.test/series/series-1":            {Body: []byte(`{"slug":"example","lines":[{"name":"Main","episodes":[{"slug":"e3","label":"EP03"}]}]}`)},
	}}
	source := jsonWebSource()
	source["resolve"] = map[string]any{
		"mode": "browser_sniff", "transport": "auto", "url": "https://203.0.113.10/watch/{{episode_number}}",
		"allowed_hosts": []any{"203.0.113.10"}, "cookie_policy": "none", "timeout_ms": 500,
		"match": map[string]any{"include": []any{`\.m3u8(?:\?|$)`}},
	}
	browser := &mediaBrowser{}
	runner := NewSpecRunner(source, RunnerOptions{Fetcher: fetcher, Browser: webresolver.New(browser)})
	var candidate Candidate
	err := runner.Candidates(context.Background(), ResolveRequest{
		Schema: "nagare-resolve-request/v1", Subject: Subject{IDs: map[string]string{"bangumi": "400602"}, Titles: []string{"Example Animation"}},
		Episode: Episode{Number: "3", Absolute: pointer(3.0)},
	}, func(value Candidate) error { candidate = value; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Transport.Type != "hls" || candidate.Transport.URL != "https://203.0.113.10/media/e3.m3u8" || candidate.Transport.Headers["Referer"] != "https://203.0.113.10/watch/3" {
		t.Fatalf("browser media was not converted to a candidate: %+v", candidate.Transport)
	}
	browser.mu.Lock()
	defer browser.mu.Unlock()
	if browser.request.URL != "https://203.0.113.10/watch/3" {
		t.Fatalf("unexpected browser entry URL: %s", browser.request.URL)
	}
}
