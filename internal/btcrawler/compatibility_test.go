package btcrawler_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nagare-project/Nagare_Source/internal/btcrawler"
	"github.com/nagare-project/Nagare_Source/internal/importer"
)

func TestImportedAnimekoRSSCanBeCrawled(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join(root, "fixtures", "importers", "animeko", "subscription.json"))
	if err != nil {
		t.Fatal(err)
	}
	imported := importer.ImportAnimeko(fixture, importer.Options{
		Upstream: "https://example.invalid/subscription.json",
		License:  "MIT",
	})
	if imported.HasErrors() {
		t.Fatalf("fixture import failed: %+v", imported.Diagnostics)
	}
	var source map[string]any
	for _, candidate := range imported.Sources {
		if candidate["kind"] == "bt" {
			source = candidate
			break
		}
	}
	if source == nil {
		t.Fatal("import produced no BT source")
	}
	response := `<rss><channel><item><title>[Fixture] Imported Show EP 01</title><enclosure url="magnet:?xt=urn:btih:3123456789abcdef0123456789abcdef01234567" length="4096"/><pubDate>Fri, 02 Jan 2026 03:04:05 GMT</pubDate></item></channel></rss>`
	result, err := btcrawler.Crawl(context.Background(), source, btcrawler.Query{}, btcrawler.StaticFetcher{Response: btcrawler.Response{Body: []byte(response)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("imported rule did not produce one record: %+v", result)
	}
	record := result.Records[0]
	if record.SourceID != "fixture-rss" || record.InfoHash != "3123456789abcdef0123456789abcdef01234567" || record.SizeBytes == nil || *record.SizeBytes != 4096 {
		t.Fatalf("unexpected imported-rule record: %+v", record)
	}
}
