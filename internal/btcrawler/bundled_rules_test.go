package btcrawler_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nagare-project/Nagare_Source/internal/btcrawler"
	"github.com/nagare-project/Nagare_Source/internal/repository"
)

func loadBundledRule(t *testing.T, id string) map[string]any {
	t.Helper()
	value, err := repository.LoadDocument(filepath.Join("..", "..", "sources", "bt", id+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

// Anime Garden 的条目只要有标题和磁力就该留下：没有字幕组（接口不给、标题也没有 [组名]）、
// 没有体积或时间的发布此前被整条丢掉。体积按字节解释（2026-10-09 实测接口给的是字节）。
func TestGardenRuleKeepsReleasesWithoutOptionalFields(t *testing.T) {
	response := `{"status":"OK","resources":[
		{"title":"【悠哈璃羽字幕社】描绘直至生命尽头 06","magnet":"magnet:?xt=urn:btih:67YRWF5ZLMADCPW63XVQ2LBAK7ATGCPJ"},
		{"title":"[LoliHouse] Kore Kaite Shine - 06 [1080p]","magnet":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		 "provider":"dmhy","size":564762624,"createdAt":"2026-08-11T22:07:00.000Z","fansub":{"name":"LoliHouse"}}
	]}`
	result, err := btcrawler.Crawl(context.Background(), loadBundledRule(t, "garden"),
		btcrawler.Query{Title: "描绘直至生命尽头", Page: 1, Unfiltered: true},
		btcrawler.StaticFetcher{Response: btcrawler.Response{Body: []byte(response)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("releases without optional fields were dropped: %+v diagnostics=%+v", result.Records, result.Diagnostics)
	}
	for _, record := range result.Records {
		if record.Fansub == "LoliHouse" && (record.SizeBytes == nil || *record.SizeBytes != 564762624) {
			t.Fatalf("garden size must be read as bytes: %+v", record)
		}
		if record.Fansub == "" && (record.SizeBytes != nil || record.PublishedAt != "") {
			t.Fatalf("missing optional fields must stay absent: %+v", record)
		}
	}
}

func TestMikanRuleKeepsReleasesWithoutSize(t *testing.T) {
	response := `<?xml version="1.0"?><rss version="2.0" xmlns:mikan="https://mikanani.me/0.1/"><channel><item>
		<title>[北宇治字幕组] 葬送的芙莉莲 [38][WebRip][HEVC_AAC][简繁日内封]</title>
		<link>https://mikanani.me/Home/Episode/0123456789abcdef0123456789abcdef01234567</link>
		<enclosure url="https://mikanani.me/Download/20260101/0123456789abcdef0123456789abcdef01234567.torrent"/>
	</item></channel></rss>`
	result, err := btcrawler.Crawl(context.Background(), loadBundledRule(t, "mikan"),
		btcrawler.Query{Title: "葬送的芙莉莲", Page: 1, Unfiltered: true},
		btcrawler.StaticFetcher{Response: btcrawler.Response{Body: []byte(response)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 || result.Records[0].SizeBytes != nil {
		t.Fatalf("mikan release without contentLength was dropped: %+v diagnostics=%+v", result.Records, result.Diagnostics)
	}
}
