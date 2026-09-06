package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportNagareV1PreservesFallbacksNamespacesAndRanking(t *testing.T) {
	result := ImportNagareV1([]byte(`
schema: 1
id: test_feed
name: Test Feed
homepage: https://feed.example.org
request:
  url: https://feed.example.org/rss?q={{query}}
  timeout_seconds: 12
  headers:
    Accept: application/rss+xml
format: xml
namespaces:
  torrent: https://example.org/torrent/1.0
items: rss/channel/item
fields:
  title: title
  magnet:
    any:
      - { path: enclosure@url }
      - { path: link }
    transforms: [trim]
  size: { path: torrent:contentLength, transforms: [format_bytes] }
  date: pubDate
  fansub: { path: $title, transforms: [parse_fansub] }
capabilities:
  seeders: true
  priority: 10
selftest:
  query: frieren
`), testOptions())

	if result.HasErrors() {
		t.Fatalf("import returned errors: %+v", result.Diagnostics)
	}
	if len(result.Sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(result.Sources))
	}
	source := result.Sources[0]
	if source["id"] != "test-feed" {
		t.Fatalf("legacy underscore was not normalized: %v", source["id"])
	}
	search := objectValue(source["search"])
	fields := objectValue(search["fields"])
	magnet := objectValue(fields["magnet"])
	if len(arrayValue(magnet["any"])) != 2 {
		t.Fatalf("fallback extractors were not preserved: %+v", magnet)
	}
	if _, ok := search["namespaces"]; !ok {
		t.Fatal("XML namespaces were not preserved")
	}
	ranking := objectValue(source["ranking"])
	if ranking["priority"] != 10 || ranking["has_seeders"] != true {
		t.Fatalf("ranking capabilities were not preserved: %+v", ranking)
	}
	published := objectValue(fields["published_at"])
	if !hasTransform(published, "parse_datetime") {
		t.Fatal("legacy date did not receive canonical datetime normalization")
	}
	assertValidSource(t, source)
}

func TestImportNagareV1ReportsUnsupportedTransform(t *testing.T) {
	result := ImportNagareV1([]byte(`
schema: 1
id: bad
name: Bad
request: { url: "https://feed.example.org/?q={{query}}" }
format: json
items: $
fields:
  title: title
  magnet: { path: magnet, transforms: [execute_javascript] }
`), testOptions())
	if !result.HasErrors() || len(result.Sources) != 0 {
		t.Fatalf("unsupported transform was not rejected: %+v", result)
	}
	found := false
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Category == "unsupported_rule" && strings.Contains(diagnostic.Message, "execute_javascript") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing structured unsupported_rule diagnostic: %+v", result.Diagnostics)
	}
}

func TestOverlayAppliesPatchAndRejectsStaleDigest(t *testing.T) {
	imported := ImportAnimeko([]byte(`{
		"factoryId":"rss",
		"version":1,
		"arguments":{"name":"Overlay Feed","searchConfig":{"searchUrl":"https://feed.example.org/?q={keyword}"}}
	}`), testOptions())
	if imported.HasErrors() || len(imported.Sources) != 1 {
		t.Fatalf("fixture import failed: %+v", imported)
	}
	source := imported.Sources[0]
	digest := stringValue(objectValue(source["origin"])["digest"])
	directory := t.TempDir()
	overlay := `schema: nagare-source-overlay/v1
source_id: overlay-feed
origin_digest: ` + digest + `
reason: Use a stable health-check query.
patch:
  tier: 2
  selftest:
    request: { query: frieren }
    expect: { min_candidates: 1, transport: torrent }
`
	if err := os.WriteFile(filepath.Join(directory, "overlay-feed.yaml"), []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	patched, diagnostics := ApplyOverlays(imported.Sources, directory)
	if len(patched) != 1 || intValue(patched[0]["tier"], -1) != 2 {
		t.Fatalf("overlay was not applied: sources=%+v diagnostics=%+v", patched, diagnostics)
	}
	if _, ok := patched[0]["selftest"]; !ok {
		t.Fatal("overlay selftest patch is missing")
	}
	assertValidSource(t, patched[0])

	stale := strings.Replace(overlay, digest, "sha256:"+strings.Repeat("0", 64), 1)
	if err := os.WriteFile(filepath.Join(directory, "overlay-feed.yaml"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	patched, diagnostics = ApplyOverlays(imported.Sources, directory)
	if len(patched) != 0 || len(diagnostics) != 1 || diagnostics[0].Category != "invalid_overlay" {
		t.Fatalf("stale overlay was not rejected: sources=%+v diagnostics=%+v", patched, diagnostics)
	}
}
