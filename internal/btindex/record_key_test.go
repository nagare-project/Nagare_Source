package btindex

import (
	"strings"
	"testing"
	"time"
)

// 只有 .torrent 地址、没有 infohash 的条目要能通过规范化（acg.rip 这类源），
// 但不能进发布索引；带 magnet 的条目仍然必须有 infohash。
func TestNormalizeRecordAllowsTorrentURLWithoutInfoHash(t *testing.T) {
	record, err := NormalizeRecord(Record{Schema: RecordSchema, SourceID: "acg", Title: "[G] Show - 01", TorrentURL: "https://acg.rip/t/1.torrent"})
	if err != nil {
		t.Fatal(err)
	}
	if record.InfoHash != "" || record.Key() != "acg/url:https://acg.rip/t/1.torrent" {
		t.Fatalf("unexpected normalized record: %+v key=%s", record, record.Key())
	}
	if _, err := NormalizeRecord(Record{Schema: RecordSchema, SourceID: "acg", Title: "x", Magnet: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}); err == nil || !strings.Contains(err.Error(), "infoHash") {
		t.Fatalf("magnet without infoHash must be rejected, got %v", err)
	}
	if _, err := Build([]Record{record}, time.Unix(0, 0).UTC(), t.TempDir()+"/index.sqlite.zst"); err == nil || !strings.Contains(err.Error(), "published index") {
		t.Fatalf("index build must reject records without infoHash, got %v", err)
	}
	withHash, err := NormalizeRecord(Record{Schema: RecordSchema, SourceID: "garden", Title: "x", InfoHash: "0123456789abcdef0123456789abcdef01234567", TorrentURL: "https://x.example/a.torrent"})
	if err != nil {
		t.Fatal(err)
	}
	if withHash.Key() != "garden/0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("key must prefer infohash: %s", withHash.Key())
	}
}
