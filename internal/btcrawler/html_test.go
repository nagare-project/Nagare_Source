package btcrawler

import (
	"context"
	"strings"
	"testing"
)

func htmlSource() map[string]any {
	source := baseSource("example-html", "html")
	search := objectValue(source["search"])
	search["items"] = map[string]any{"type": "css", "expression": "ul.results > li"}
	search["fields"] = map[string]any{
		"title": map[string]any{"type": "css", "expression": "a.title"},
		"magnet": map[string]any{"any": []any{
			map[string]any{"type": "css", "expression": "a.magnet", "attribute": "href"},
			map[string]any{"type": "css", "expression": ":scope", "attribute": "data-hash", "transforms": []any{
				map[string]any{"type": "magnet", "display_name_from": "title"},
			}},
		}},
		"torrent_url": map[string]any{"type": "css", "expression": "a.title", "attribute": "HREF", "required": false,
			"transforms": []any{"absolute_url"}},
		"episode": map[string]any{"type": "regex", "expression": `第(\d+)集`, "required": false,
			"transforms": []any{map[string]any{"type": "regex", "pattern": `\d+`}}},
		"resolution": map[string]any{"type": "css", "scope": "document", "expression": "meta[name=quality]",
			"attribute": "content", "required": false},
	}
	return source
}

func TestCrawlHTMLWithCSSSelectors(t *testing.T) {
	body := `<!DOCTYPE html><html><head><meta name="quality" content="720P"></head><body><ul class="results">
		<li><a class="title" href="/t/1.torrent">
			Example &amp; Friends
			第3集
		</a><a class="magnet" href="magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&amp;tr=https%3a%2f%2ftracker.example.invalid%2fannounce">磁力</a></li>
		<li data-hash="fedcba9876543210fedcba9876543210fedcba98"><a class="title">Example 无磁力链接</a></li>
	</ul></body></html>`
	result, err := Crawl(context.Background(), htmlSource(), Query{}, StaticFetcher{Response: Response{Body: []byte(body)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 2 || len(result.Diagnostics) != 0 {
		t.Fatalf("unexpected crawl result: %+v", result)
	}
	first, second := result.Records[0], result.Records[1]
	if first.Title != "Example & Friends 第3集" {
		t.Fatalf("text must be entity-decoded and whitespace-collapsed: %q", first.Title)
	}
	if !strings.Contains(first.Magnet, "tracker.example.invalid") || first.InfoHash != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("attribute value must be read as-is: %+v", first)
	}
	if first.TorrentURL != "https://feed.example.invalid/t/1.torrent" {
		t.Fatalf("attribute names are case-insensitive and relative URLs resolve against the request: %q", first.TorrentURL)
	}
	if first.Episode == nil || *first.Episode != 3 {
		t.Fatalf("regex extractor must read the item text: %+v", first)
	}
	if first.Resolution != "720P" || second.Resolution != "720P" {
		t.Fatalf("scope: document must select from the whole page: %+v / %+v", first, second)
	}
	if second.InfoHash != "fedcba9876543210fedcba9876543210fedcba98" || !strings.Contains(second.Magnet, "dn=") || second.TorrentURL != "" {
		t.Fatalf(":scope must read the item itself and missing attributes stay empty: %+v", second)
	}
}

func TestCSSRequiresHTMLResponse(t *testing.T) {
	source := baseSource("css-on-xml", "xml")
	search := objectValue(source["search"])
	search["items"] = map[string]any{"type": "css", "expression": "item"}
	search["fields"] = map[string]any{"title": map[string]any{"type": "css", "expression": "title"}}
	if _, err := Crawl(context.Background(), source, Query{}, StaticFetcher{Response: Response{Body: []byte(`<rss><item><title>x</title></item></rss>`)}}); err == nil {
		t.Fatal("CSS items on an XML response must fail instead of silently returning nothing")
	}
}
