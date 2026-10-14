package btcrawler_test

import (
	"context"
	"path/filepath"
	"strings"
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

// Mikan 走网页搜索（RSS 搜索只给最新 100 条）。行里没有 data-magnet 时用标题链接末尾的
// infohash 拼磁力；体积、时间、种子链接都可缺；时间是不带时区的北京时间。
func TestMikanRuleParsesSearchPageRows(t *testing.T) {
	response := `<table><tbody>
	<tr class="js-search-results-row">
		<td><input type="checkbox" class="js-episode-select" data-magnet="magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&amp;tr=http%3a%2f%2ftracker.example.invalid%2fannounce" /></td>
		<td><a href="/Home/Episode/0123456789abcdef0123456789abcdef01234567" class="magnet-link-wrap">[&#x5317;&#x5B87;&#x6CBB;&#x5B57;&#x5E55;&#x7EC4;] 葬送的芙莉莲 [38][WebRip][HEVC_AAC][简繁日内封]</a></td>
		<td>1.4GB</td>
		<td>2026/08/28 05:32</td>
		<td><a href="/Download/20260828/0123456789abcdef0123456789abcdef01234567.torrent"><img src="/images/download_icon_blue.svg"></a></td>
		<td><a href="https://play.example.invalid/x"><i class="fa fa-play-circle"></i></a></td>
	</tr>
	<tr class="js-search-results-row">
		<td></td>
		<td><a href="/Home/Episode/FEDCBA9876543210FEDCBA9876543210FEDCBA98" class="magnet-link-wrap">[7&#xB3;ACG] 葬送的芙莉莲 S01 | 01-28 [简繁字幕] BDrip 1080p</a></td>
		<td></td>
		<td></td>
		<td></td>
		<td></td>
	</tr>
	</tbody></table>`
	result, err := btcrawler.Crawl(context.Background(), loadBundledRule(t, "mikan"),
		btcrawler.Query{Title: "葬送的芙莉莲", Page: 1, Unfiltered: true},
		btcrawler.StaticFetcher{Response: btcrawler.Response{URL: "https://mikanani.me/Home/Search?searchstr=x", Body: []byte(response)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("mikan rows were dropped: %+v diagnostics=%+v", result.Records, result.Diagnostics)
	}
	full, bare := result.Records[0], result.Records[1]
	if full.Title != "[北宇治字幕组] 葬送的芙莉莲 [38][WebRip][HEVC_AAC][简繁日内封]" || full.Fansub != "北宇治字幕组" {
		t.Fatalf("title entities must be decoded: %+v", full)
	}
	if full.InfoHash != "0123456789abcdef0123456789abcdef01234567" || !strings.Contains(full.Magnet, "tracker.example.invalid") {
		t.Fatalf("row magnet must be used as-is: %+v", full)
	}
	if full.SizeBytes == nil || *full.SizeBytes != 1503238554 || full.PublishedAt != "2026-08-27T21:32:00Z" ||
		full.TorrentURL != "https://mikanani.me/Download/20260828/0123456789abcdef0123456789abcdef01234567.torrent" ||
		full.Episode == nil || *full.Episode != 38 {
		t.Fatalf("optional fields were not parsed: %+v", full)
	}
	if bare.InfoHash != "fedcba9876543210fedcba9876543210fedcba98" || !strings.HasPrefix(bare.Magnet, "magnet:?") ||
		bare.Title != "[7³ACG] 葬送的芙莉莲 S01 | 01-28 [简繁字幕] BDrip 1080p" {
		t.Fatalf("magnet must fall back to the episode link infohash: %+v", bare)
	}
	if bare.SizeBytes != nil || bare.PublishedAt != "" || bare.TorrentURL != "" {
		t.Fatalf("missing optional fields must stay absent: %+v", bare)
	}
}
