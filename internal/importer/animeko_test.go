package importer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nagare-project/Nagare_Source/internal/repository"
)

func TestImportAnimekoRSSProducesValidBTSource(t *testing.T) {
	result := ImportAnimeko([]byte(`{
		"factoryId": "rss",
		"version": 1,
		"arguments": {
			"name": "Anime Garden",
			"tier": 1,
			"searchConfig": {
				"searchUrl": "https://feed.example.org/search?q={keyword}",
				"filterByEpisodeSort": true,
				"filterBySubjectName": true
			}
		}
	}`), testOptions())

	if result.HasErrors() {
		t.Fatalf("import returned errors: %+v", result.Diagnostics)
	}
	if len(result.Sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(result.Sources))
	}
	source := result.Sources[0]
	if source["kind"] != "bt" || source["id"] != "anime-garden" {
		t.Fatalf("unexpected source identity: id=%v kind=%v", source["id"], source["kind"])
	}
	search := objectValue(source["search"])
	request := objectValue(search["request"])
	if request["url"] != "https://feed.example.org/search?q={{title}}" {
		t.Fatalf("Animeko template was not converted: %v", request["url"])
	}
	assertValidSource(t, source)
}

func TestImportAnimekoWebSelectorProducesValidBrowserSource(t *testing.T) {
	result := ImportAnimeko([]byte(`{
		"factoryId": "web-selector",
		"version": 2,
		"arguments": {
			"name": "Example Web",
			"tier": 0,
			"channelTiers": {"线路一": 0, "线路二": 2},
			"searchConfig": {
				"searchUrl": "https://video.example.org/search/{keyword}",
				"rawBaseUrl": "https://video.example.org/",
				"subjectFormatId": "a",
				"channelFormatId": "no-channel",
				"defaultResolution": "1080P",
				"defaultSubtitleLanguage": "CHS",
				"matchVideo": {
					"matchVideoUrl": "https://cdn\\.example\\.org/.+\\.m3u8",
					"addHeadersToVideo": {
						"referer": "https://video.example.org/"
					}
				}
			}
		}
	}`), testOptions())

	if result.HasErrors() {
		t.Fatalf("import returned errors: %+v", result.Diagnostics)
	}
	if len(result.Sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(result.Sources))
	}
	source := result.Sources[0]
	resolve := objectValue(source["resolve"])
	if source["kind"] != "web" || resolve["mode"] != "browser_sniff" {
		t.Fatalf("unexpected web source: kind=%v mode=%v", source["kind"], resolve["mode"])
	}
	if resolve["cookie_policy"] != nil {
		t.Fatalf("empty Animeko cookies unexpectedly enabled a cookie policy")
	}
	assertValidSource(t, source)
}

func TestImportAnimekoRejectsUnsupportedFactory(t *testing.T) {
	result := ImportAnimeko([]byte(`{
		"factoryId": "custom-script",
		"version": 1,
		"arguments": {"name": "Unsafe Source"}
	}`), testOptions())

	if !result.HasErrors() || len(result.Sources) != 0 {
		t.Fatalf("unsupported factory result: %+v", result)
	}
	encoded, err := EncodeDiagnostics(result.Diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic Diagnostic
	if err := json.Unmarshal(encoded[:len(encoded)-1], &diagnostic); err != nil {
		t.Fatal(err)
	}
	if diagnostic.Category != "unsupported_factory" || !strings.Contains(diagnostic.Message, "custom-script") {
		t.Fatalf("unexpected diagnostic: %+v", diagnostic)
	}
}

func assertValidSource(t *testing.T, source map[string]any) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	validator, err := repository.NewValidator(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sources", stringValue(source["kind"]), stringValue(source["id"])+".yaml")
	if err := validator.ValidateSource(root, path, source); err != nil {
		t.Fatalf("imported source does not satisfy Source Spec v1: %v", err)
	}
}

func testOptions() Options {
	return Options{
		Upstream: "https://rules.example.org/animeko.json",
		License:  "MIT",
	}
}
