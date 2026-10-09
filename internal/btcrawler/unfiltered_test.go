package btcrawler

import (
	"context"
	"testing"
)

// 整部作品搜索（/v1/releases）不按集号或作品名复核：没有集号、标题也不含查询词的发布照样返回。
func TestCrawlUnfilteredSkipsMatchingRules(t *testing.T) {
	source := rssSource() // require_episode 与 require_subject 都开着
	// 与内置规则一致：集号解不出不算抽取失败，交给 matching 决定去留。
	objectValue(objectValue(source["search"])["fields"])["episode"] = map[string]any{
		"type": "field", "expression": "title", "required": false, "transforms": []any{"parse_episode"},
	}
	response := `<?xml version="1.0"?><rss><channel>
		<item><title>[Group] 别名写法 合集 [1080P]</title><link>magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567</link><pubDate>Fri, 02 Jan 2026 03:04:05 GMT</pubDate></item>
		<item><title>[Group] Example Animation - 04 [1080P]</title><link>magnet:?xt=urn:btih:1123456789abcdef0123456789abcdef01234567</link><pubDate>Fri, 02 Jan 2026 03:04:05 GMT</pubDate></item>
	</channel></rss>`
	fetcher := StaticFetcher{Response: Response{Body: []byte(response)}}

	filtered, err := Crawl(context.Background(), source, Query{Title: "Example Animation", Episode: 3}, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Records) != 0 {
		t.Fatalf("matching rules should drop both releases for episode 3: %+v", filtered.Records)
	}

	unfiltered, err := Crawl(context.Background(), source, Query{Title: "Example Animation", Page: 1, Unfiltered: true}, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered.Records) != 2 {
		t.Fatalf("unfiltered crawl must keep every extracted release: %+v", unfiltered.Records)
	}
}
