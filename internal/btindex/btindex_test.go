package btindex

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestBuildIsDeterministicAndQueryable(t *testing.T) {
	episode := 1.0
	size := int64(1_073_741_824)
	seeders := int64(12)
	records := []Record{{
		Schema:            RecordSchema,
		SourceID:          "example-rss",
		InfoHash:          "0123456789ABCDEF0123456789ABCDEF01234567",
		Magnet:            "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		Title:             "[Example]  Show - 01",
		Episode:           &episode,
		PublishedAt:       "2026-01-02T14:04:05+11:00",
		SizeBytes:         &size,
		Fansub:            "Example",
		Seeders:           &seeders,
		Resolution:        "1080P",
		SubtitleLanguages: []string{"zh-Hant", "zh-Hans"},
	}}
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first := filepath.Join(t.TempDir(), "first.sqlite.zst")
	second := filepath.Join(t.TempDir(), "second.sqlite.zst")
	artifact, err := Build(records, when, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(records, when, second); err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("identical records produced different compressed databases")
	}
	if artifact.Records != 1 || !strings.HasPrefix(artifact.Digest, "sha256:") {
		t.Fatalf("unexpected artifact metadata: %+v", artifact)
	}

	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	databaseBytes, err := decoder.DecodeAll(firstBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "index.sqlite")
	if err := os.WriteFile(databasePath, databaseBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var title, normalizedTitle, infoHash, languages string
	var publishedAt int64
	if err := database.QueryRow(`SELECT title, normalized_title, info_hash, subtitle_languages, published_at
		FROM releases WHERE source_id = ? AND episode = ?`, "example-rss", 1).Scan(
		&title, &normalizedTitle, &infoHash, &languages, &publishedAt,
	); err != nil {
		t.Fatal(err)
	}
	if title != "[Example]  Show - 01" || normalizedTitle != "example show 01" {
		t.Fatalf("unexpected title values: %q, %q", title, normalizedTitle)
	}
	if infoHash != "0123456789abcdef0123456789abcdef01234567" || languages != `["zh-Hans","zh-Hant"]` {
		t.Fatalf("record was not normalized: hash=%q languages=%s", infoHash, languages)
	}
	if publishedAt != when.UnixMilli() {
		t.Fatalf("published_at=%d, want %d", publishedAt, when.UnixMilli())
	}
	var schema, generatedAt string
	if err := database.QueryRow("SELECT value FROM meta WHERE key = 'schema'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT value FROM meta WHERE key = 'generated_at'").Scan(&generatedAt); err != nil {
		t.Fatal(err)
	}
	if schema != RecordSchema || generatedAt != when.Format(time.RFC3339) {
		t.Fatalf("unexpected metadata: schema=%q generated_at=%q", schema, generatedAt)
	}
}

func TestLoadJSONLRejectsDuplicateAndUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	record := `{"schema":"nagare-bt-record/v1","sourceId":"example-rss","infoHash":"0123456789abcdef0123456789abcdef01234567","magnet":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567","title":"Example"}`
	if err := os.WriteFile(path, []byte(record+"\n"+record+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJSONL(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate returned unexpected error: %v", err)
	}
	unknown := strings.TrimSuffix(record, "}") + `,"secret":"value"}`
	if err := os.WriteFile(path, []byte(unknown+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJSONL(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field returned unexpected error: %v", err)
	}
}

func TestLoadJSONLRejectsMismatchedMagnetHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	record := `{"schema":"nagare-bt-record/v1","sourceId":"example-rss","infoHash":"0123456789abcdef0123456789abcdef01234567","magnet":"magnet:?xt=urn:btih:1123456789abcdef0123456789abcdef01234567","title":"Example"}`
	if err := os.WriteFile(path, []byte(record+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJSONL(path); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched magnet returned unexpected error: %v", err)
	}
}

func TestNormalizeRecordRejectsPrivateTorrentURL(t *testing.T) {
	record := Record{
		Schema: RecordSchema, SourceID: "example-rss", Title: "Example",
		InfoHash:   "0123456789abcdef0123456789abcdef01234567",
		TorrentURL: "http://127.0.0.1/example.torrent",
	}
	if _, err := NormalizeRecord(record); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("private torrent URL returned unexpected error: %v", err)
	}
}
