package importer

import "testing"

func TestImportNagareV1ProducesValidBTSource(t *testing.T) {
	result := ImportNagareV1([]byte(`
schema: 1
id: legacy-rss
name: Legacy RSS
homepage: https://feed.example.org/
request:
  url: https://feed.example.org/search?q={{query}}
  timeout_seconds: 15
format: xml
items: rss/channel/item
fields:
  title: title
  magnet: link
  size:
    path: enclosure@length
    transforms: [format_bytes]
capabilities:
  seeders: false
  priority: 2
selftest:
  query: Frieren
`), testOptions())

	if result.HasErrors() {
		t.Fatalf("import returned errors: %+v", result.Diagnostics)
	}
	if len(result.Sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(result.Sources))
	}
	source := result.Sources[0]
	if source["kind"] != "bt" || source["id"] != "legacy-rss" {
		t.Fatalf("unexpected source identity: id=%v kind=%v", source["id"], source["kind"])
	}
	search := objectValue(source["search"])
	request := objectValue(search["request"])
	if request["url"] != "https://feed.example.org/search?q={{title}}" {
		t.Fatalf("legacy query template was not converted: %v", request["url"])
	}
	assertValidSource(t, source)
}

func TestImportNagareV1RejectsUnsupportedTransform(t *testing.T) {
	result := ImportNagareV1([]byte(`
schema: 1
id: legacy-rss
name: Legacy RSS
request:
  url: https://feed.example.org/search?q={{query}}
format: xml
items: rss/channel/item
fields:
  title: title
  magnet:
    path: link
    transforms: [execute_script]
`), testOptions())

	if !result.HasErrors() || len(result.Sources) != 0 {
		t.Fatalf("unsupported transform result: %+v", result)
	}
}
