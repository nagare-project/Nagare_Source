package btcrawler

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nagare-project/Nagare_Source/internal/btindex"
)

func TestCrawlRSSAndCheckSelfTest(t *testing.T) {
	source := rssSource()
	response := `<?xml version="1.0"?><rss><channel><item>
		<title>[Example Fansub] Example Animation EP 03 [1080P]</title>
		<link>magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&amp;dn=Example</link>
		<pubDate>Fri, 02 Jan 2026 03:04:05 GMT</pubDate>
	</item></channel></rss>`
	query, err := SelfTestQuery(source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Crawl(context.Background(), source, query, StaticFetcher{Response: Response{Body: []byte(response)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSelfTest(source, result); err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 || len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected crawl result: %+v", result)
	}
	record := result.Records[0]
	if record.InfoHash != "0123456789abcdef0123456789abcdef01234567" || record.Episode == nil || *record.Episode != 3 {
		t.Fatalf("record was not extracted: %+v", record)
	}
	if record.PublishedAt != "2026-01-02T03:04:05Z" || record.Fansub != "Example Fansub" {
		t.Fatalf("record was not transformed: %+v", record)
	}
}

func TestCrawlJSONPreservesKnownZeroAndNormalizesFields(t *testing.T) {
	source := baseSource("json-source", "json")
	search := objectValue(source["search"])
	search["items"] = map[string]any{"type": "jsonpath", "expression": "$"}
	search["fields"] = map[string]any{
		"title":      map[string]any{"type": "jsonpath", "expression": "$.title", "transforms": []any{"trim"}},
		"magnet":     map[string]any{"type": "jsonpath", "expression": "$.magnet"},
		"info_hash":  map[string]any{"type": "jsonpath", "expression": "$.hash", "transforms": []any{"normalize_infohash"}},
		"episode":    map[string]any{"type": "field", "expression": "title", "transforms": []any{"parse_episode"}},
		"size_bytes": map[string]any{"type": "jsonpath", "expression": "$.size", "transforms": []any{map[string]any{"type": "parse_size", "unit": "B"}}},
		"published_at": map[string]any{"type": "jsonpath", "expression": "$.timestamp", "transforms": []any{
			map[string]any{"type": "parse_datetime", "unit": "unix_seconds"},
		}},
		"seeders": map[string]any{"type": "jsonpath", "expression": "$.seeders"},
		"fansub":  map[string]any{"type": "field", "expression": "title", "transforms": []any{"parse_fansub"}},
	}
	body := `[{"title":" [JSON Fansub] JSON Show - 07 ","magnet":"magnet:?xt=urn:btih:1123456789abcdef0123456789abcdef01234567","hash":"1123456789ABCDEF0123456789ABCDEF01234567","size":2048,"timestamp":1767323045,"seeders":0}]`
	result, err := Crawl(context.Background(), source, Query{Title: "JSON Show", Episode: 7}, StaticFetcher{Response: Response{Body: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("unexpected crawl result: %+v", result)
	}
	record := result.Records[0]
	if record.SizeBytes == nil || *record.SizeBytes != 2048 || record.Seeders == nil || *record.Seeders != 0 {
		t.Fatalf("numeric fields lost presence or value: %+v", record)
	}
	if record.PublishedAt != "2026-01-02T03:04:05Z" || record.Fansub != "JSON Fansub" {
		t.Fatalf("unexpected transformed fields: %+v", record)
	}
}

func TestCrawlXMLNamespaceAndMagnetTransform(t *testing.T) {
	source := baseSource("namespace-source", "rss")
	search := objectValue(source["search"])
	search["namespaces"] = map[string]any{"torrent": "https://example.invalid/torrent"}
	search["items"] = map[string]any{"type": "xpath", "expression": "/rss/channel/item"}
	search["fields"] = map[string]any{
		"title": map[string]any{"type": "xpath", "expression": "./title/text()"},
		"hash":  map[string]any{"type": "xpath", "expression": "./torrent:infoHash/text()"},
		"magnet": map[string]any{"type": "field", "expression": "hash", "transforms": []any{
			map[string]any{"type": "magnet", "display_name_from": "title", "trackers": []any{"https://tracker.example.invalid/announce"}},
		}},
	}
	body := `<rss xmlns:torrent="https://example.invalid/torrent"><channel><item><title>Namespaced EP 01</title><torrent:infoHash>2123456789abcdef0123456789abcdef01234567</torrent:infoHash></item></channel></rss>`
	result, err := Crawl(context.Background(), source, Query{}, StaticFetcher{Response: Response{Body: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("unexpected crawl result: %+v", result)
	}
	record := result.Records[0]
	if record.InfoHash != "2123456789abcdef0123456789abcdef01234567" || !strings.Contains(record.Magnet, "tracker.example.invalid") {
		t.Fatalf("namespace or magnet transform failed: %+v", record)
	}
}

func TestInvalidItemIsDiagnosedAndSkipped(t *testing.T) {
	source := rssSource()
	body := `<rss><channel><item><title>missing download</title></item></channel></rss>`
	result, err := Crawl(context.Background(), source, Query{}, StaticFetcher{Response: Response{Body: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 0 || len(result.Diagnostics) != 1 || result.Diagnostics[0].Category != "item_extraction_failed" {
		t.Fatalf("unexpected rejected-item result: %+v", result)
	}
}

func TestRequestSecurityAndStaticSizeLimit(t *testing.T) {
	if err := validateRequestTarget("http://127.0.0.1/feed", []string{"127.0.0.1"}); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("private target returned unexpected error: %v", err)
	}
	if err := validateRequestTarget("https://example.invalid/feed", []string{"other.invalid"}); err == nil || !strings.Contains(err.Error(), "allowed_hosts") {
		t.Fatalf("host escape returned unexpected error: %v", err)
	}
	fetcher := StaticFetcher{Response: Response{Body: []byte("12345")}}
	if _, err := fetcher.Fetch(context.Background(), Request{MaxBytes: 4}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize fixture returned unexpected error: %v", err)
	}
	sanitized := sanitizeRequestError(&url.Error{Op: "Get", URL: "https://example.invalid/?token=secret", Err: errors.New("network unavailable")})
	if strings.Contains(sanitized.Error(), "secret") || !strings.Contains(sanitized.Error(), "network unavailable") {
		t.Fatalf("request error was not safely sanitized: %v", sanitized)
	}
}

func TestJSONRequestBodyEscapesQueryValues(t *testing.T) {
	source := rssSource()
	search := objectValue(source["search"])
	search["response"] = "json"
	search["items"] = "$.items[*]"
	requestDocument := objectValue(search["request"])
	requestDocument["method"] = "POST"
	requestDocument["headers"] = map[string]any{"Content-Type": "application/json"}
	requestDocument["body"] = `{"keyword":"{{title}}","episode":{{episode}}}`
	var captured Request
	fetcher := FetcherFunc(func(_ context.Context, request Request) (Response, error) {
		captured = request
		return Response{Body: []byte(`{"items":[]}`)}, nil
	})
	_, err := Crawl(context.Background(), source, Query{Title: `x"},"admin":true,"x":"`, Episode: 3}, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(captured.Body, &body); err != nil {
		t.Fatalf("rendered BT request body is invalid JSON: %s: %v", captured.Body, err)
	}
	if body["keyword"] != `x"},"admin":true,"x":"` || body["episode"] != float64(3) || len(body) != 2 {
		t.Fatalf("BT JSON template value escaped its field: %#v", body)
	}
}

func TestTemplateDoesNotReinterpretQueryText(t *testing.T) {
	value, err := renderTemplate("q={{title}}", map[string]string{"title": "{{title}}"}, false)
	if err != nil || value != "q={{title}}" {
		t.Fatalf("replacement was interpreted as another template: value=%q err=%v", value, err)
	}
}

type FetcherFunc func(context.Context, Request) (Response, error)

func (function FetcherFunc) Fetch(ctx context.Context, request Request) (Response, error) {
	return function(ctx, request)
}

func TestWriteJSONLDoesNotReplaceOutputOnInvalidRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	if err := os.WriteFile(path, []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONL(path, []btindex.Record{{}}); err == nil {
		t.Fatal("invalid record unexpectedly passed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing\n" {
		t.Fatalf("failed write replaced output: %q", data)
	}
}

func rssSource() map[string]any {
	source := baseSource("example-rss", "rss")
	search := objectValue(source["search"])
	search["items"] = map[string]any{"type": "xpath", "expression": "/rss/channel/item"}
	search["fields"] = map[string]any{
		"title":   map[string]any{"type": "xpath", "expression": "./title/text()", "transforms": []any{"trim"}},
		"magnet":  map[string]any{"type": "xpath", "expression": "./link/text()", "transforms": []any{"trim"}},
		"episode": map[string]any{"type": "field", "expression": "title", "transforms": []any{"parse_episode"}},
		"info_hash": map[string]any{"type": "field", "expression": "magnet", "transforms": []any{
			map[string]any{"type": "regex", "pattern": `(?i)btih:([a-f0-9]{40}|[a-z2-7]{32})`, "group": 1}, "normalize_infohash",
		}},
		"published_at": map[string]any{"type": "xpath", "expression": "./pubDate/text()", "transforms": []any{
			map[string]any{"type": "parse_datetime", "unit": "auto"},
		}},
		"fansub": map[string]any{"type": "field", "expression": "title", "transforms": []any{"parse_fansub"}},
	}
	source["matching"] = map[string]any{"require_episode": true, "require_subject": true}
	source["selftest"] = map[string]any{
		"request": map[string]any{"titles": []any{"Example Animation"}, "episode": float64(3)},
		"expect":  map[string]any{"min_candidates": float64(1), "transport": "torrent"},
	}
	return source
}

func baseSource(id, responseType string) map[string]any {
	return map[string]any{
		"schema": "nagare-source/v1", "id": id, "kind": "bt", "enabled": true,
		"search": map[string]any{
			"request": map[string]any{
				"method": "GET", "url": "https://feed.example.invalid/search", "allowed_hosts": []any{"feed.example.invalid"},
			},
			"response": responseType,
		},
		"defaults": map[string]any{"resolution": "1080P", "subtitle_languages": []any{"zh-Hans"}},
		"limits":   map[string]any{"timeout_ms": float64(5000), "max_response_bytes": float64(1024 * 1024), "max_redirects": float64(1)},
	}
}
