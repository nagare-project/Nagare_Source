// Package btindex validates normalized BT records and builds the compressed
// SQLite artifact published with a Nagare Source repository release.
package btindex

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

const (
	RecordSchema  = "nagare-bt-record/v1"
	SchemaVersion = 1
)

var (
	sourceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	languagePattern = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)
	resolutions     = map[string]bool{"360P": true, "480P": true, "720P": true, "1080P": true, "1440P": true, "2160P": true}
)

// Record is one normalized release emitted by a BT crawler. Pointer fields
// preserve the distinction between a missing value and a real zero.
type Record struct {
	Schema            string   `json:"schema"`
	SourceID          string   `json:"sourceId"`
	InfoHash          string   `json:"infoHash"`
	Magnet            string   `json:"magnet,omitempty"`
	TorrentURL        string   `json:"torrentUrl,omitempty"`
	Title             string   `json:"title"`
	Episode           *float64 `json:"episode,omitempty"`
	PublishedAt       string   `json:"publishedAt,omitempty"`
	SizeBytes         *int64   `json:"sizeBytes,omitempty"`
	Fansub            string   `json:"fansub,omitempty"`
	Seeders           *int64   `json:"seeders,omitempty"`
	Resolution        string   `json:"resolution,omitempty"`
	SubtitleLanguages []string `json:"subtitleLanguages,omitempty"`
}

type Artifact struct {
	Records int
	Digest  string
}

// LoadJSONL decodes, validates, and normalizes a crawler result. Every
// non-empty line must contain exactly one record object.
func LoadJSONL(path string) ([]Record, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var records []Record
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		var record Record
		if err := decoder.Decode(&record); err != nil {
			return nil, fmt.Errorf("%s:%d: decode record: %w", path, lineNumber, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return nil, fmt.Errorf("%s:%d: multiple JSON values", path, lineNumber)
			}
			return nil, fmt.Errorf("%s:%d: trailing content: %w", path, lineNumber, err)
		}
		if err := normalizeRecord(&record); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := rejectDuplicates(records); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return records, nil
}

// NormalizeRecord validates one record and returns its canonical form without
// mutating the caller's value.
func NormalizeRecord(record Record) (Record, error) {
	record.SubtitleLanguages = append([]string(nil), record.SubtitleLanguages...)
	if err := normalizeRecord(&record); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Key 是同一来源内的去重键：有 infohash 用 infohash，只有 .torrent 地址的用地址。
func (r Record) Key() string {
	if r.InfoHash != "" {
		return r.SourceID + "/" + r.InfoHash
	}
	return r.SourceID + "/url:" + r.TorrentURL
}

// Build writes a deterministic SQLite database and wraps it in a deterministic
// Zstandard frame. The returned digest covers the compressed release artifact.
func Build(records []Record, generatedAt time.Time, outputPath string) (Artifact, error) {
	records = append([]Record(nil), records...)
	for index := range records {
		if strings.TrimSpace(records[index].InfoHash) == "" {
			// 发布索引以 (source_id, info_hash) 为主键，只有地址的条目进不了快照，只能实时抓取。
			return Artifact{}, fmt.Errorf("record %d: infoHash is required in the published index", index+1)
		}
		normalized, err := NormalizeRecord(records[index])
		if err != nil {
			return Artifact{}, fmt.Errorf("record %d: %w", index+1, err)
		}
		records[index] = normalized
	}
	if err := rejectDuplicates(records); err != nil {
		return Artifact{}, err
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].SourceID != records[j].SourceID {
			return records[i].SourceID < records[j].SourceID
		}
		return records[i].InfoHash < records[j].InfoHash
	})

	parent := filepath.Dir(outputPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Artifact{}, err
	}
	workDir, err := os.MkdirTemp(parent, ".nagare-bt-index-")
	if err != nil {
		return Artifact{}, err
	}
	defer os.RemoveAll(workDir)
	databasePath := filepath.Join(workDir, "bt-index.sqlite")
	if err := buildDatabase(databasePath, records, generatedAt.UTC()); err != nil {
		return Artifact{}, err
	}
	databaseBytes, err := os.ReadFile(databasePath)
	if err != nil {
		return Artifact{}, err
	}

	temporary, err := os.CreateTemp(parent, ".bt-index-*.sqlite.zst")
	if err != nil {
		return Artifact{}, err
	}
	temporaryPath := temporary.Name()
	succeeded := false
	defer func() {
		temporary.Close()
		if !succeeded {
			_ = os.Remove(temporaryPath)
		}
	}()
	encoder, err := zstd.NewWriter(temporary,
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
		zstd.WithEncoderCRC(true),
	)
	if err != nil {
		return Artifact{}, err
	}
	if _, err := encoder.Write(databaseBytes); err != nil {
		encoder.Close()
		return Artifact{}, err
	}
	if err := encoder.Close(); err != nil {
		return Artifact{}, err
	}
	if err := temporary.Sync(); err != nil {
		return Artifact{}, err
	}
	if err := temporary.Close(); err != nil {
		return Artifact{}, err
	}
	compressed, err := os.ReadFile(temporaryPath)
	if err != nil {
		return Artifact{}, err
	}
	if err := os.Rename(temporaryPath, outputPath); err != nil {
		return Artifact{}, err
	}
	succeeded = true
	digest := sha256.Sum256(compressed)
	return Artifact{Records: len(records), Digest: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func buildDatabase(path string, records []Record, generatedAt time.Time) error {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	database.SetMaxOpenConns(1)
	defer database.Close()

	for _, statement := range []string{
		"PRAGMA page_size = 4096",
		"PRAGMA journal_mode = OFF",
		"PRAGMA synchronous = OFF",
		"PRAGMA temp_store = MEMORY",
		"PRAGMA auto_vacuum = NONE",
		"PRAGMA application_id = 1312903762",
		"PRAGMA user_version = 1",
		`CREATE TABLE meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) WITHOUT ROWID`,
		`CREATE TABLE releases (
			source_id TEXT NOT NULL,
			info_hash TEXT NOT NULL,
			magnet TEXT,
			torrent_url TEXT,
			title TEXT NOT NULL,
			normalized_title TEXT NOT NULL,
			episode REAL,
			published_at INTEGER,
			size_bytes INTEGER,
			fansub TEXT,
			seeders INTEGER,
			resolution TEXT,
			subtitle_languages TEXT NOT NULL,
			PRIMARY KEY (source_id, info_hash)
		) WITHOUT ROWID`,
		"CREATE INDEX releases_title_episode ON releases(normalized_title, episode)",
		"CREATE INDEX releases_published_at ON releases(published_at DESC)",
	} {
		if _, err := database.Exec(statement); err != nil {
			return fmt.Errorf("initialize BT index: %w", err)
		}
	}

	transaction, err := database.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for _, pair := range [][2]string{
		{"schema", RecordSchema},
		{"schema_version", fmt.Sprint(SchemaVersion)},
		{"generated_at", generatedAt.Format(time.RFC3339)},
		{"records", fmt.Sprint(len(records))},
	} {
		if _, err := transaction.Exec("INSERT INTO meta(key, value) VALUES (?, ?)", pair[0], pair[1]); err != nil {
			return err
		}
	}
	statement, err := transaction.Prepare(`INSERT INTO releases(
		source_id, info_hash, magnet, torrent_url, title, normalized_title,
		episode, published_at, size_bytes, fansub, seeders, resolution, subtitle_languages
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, record := range records {
		languages, err := json.Marshal(record.SubtitleLanguages)
		if err != nil {
			return err
		}
		var publishedAt any
		if record.PublishedAt != "" {
			parsed, _ := time.Parse(time.RFC3339, record.PublishedAt)
			publishedAt = parsed.UnixMilli()
		}
		if _, err := statement.Exec(
			record.SourceID, record.InfoHash, nullableString(record.Magnet), nullableString(record.TorrentURL),
			record.Title, NormalizeTitle(record.Title), nullableFloat(record.Episode), publishedAt,
			nullableInt(record.SizeBytes), nullableString(record.Fansub), nullableInt(record.Seeders),
			nullableString(record.Resolution), string(languages),
		); err != nil {
			return fmt.Errorf("insert %s/%s: %w", record.SourceID, record.InfoHash, err)
		}
	}
	if err := statement.Close(); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	if _, err := database.Exec("VACUUM"); err != nil {
		return fmt.Errorf("compact BT index: %w", err)
	}
	return database.Close()
}

func normalizeRecord(record *Record) error {
	if record.Schema != RecordSchema {
		return fmt.Errorf("schema must be %q", RecordSchema)
	}
	if !sourceIDPattern.MatchString(record.SourceID) {
		return errors.New("sourceId is invalid")
	}
	// infoHash 与 torrentUrl 二选一：acg.rip 一类的 RSS 只给 .torrent 地址，没有 infohash；
	// 种子文件里自带 info 和 tracker，播放端下载它即可（照 Animeko 的 HttpTorrentFile 路径）。
	hash := ""
	if strings.TrimSpace(record.InfoHash) != "" || record.TorrentURL == "" {
		normalized, err := normalizeInfoHash(record.InfoHash)
		if err != nil {
			return err
		}
		hash = normalized
	}
	record.InfoHash = hash
	record.Title = strings.TrimSpace(record.Title)
	if record.Title == "" {
		return errors.New("title must not be empty")
	}
	if NormalizeTitle(record.Title) == "" {
		return errors.New("title must contain at least one letter or number")
	}
	if record.Magnet == "" && record.TorrentURL == "" {
		return errors.New("magnet or torrentUrl is required")
	}
	if record.Magnet != "" {
		if hash == "" {
			return errors.New("infoHash is required when magnet is present")
		}
		if err := validateMagnet(record.Magnet, hash); err != nil {
			return err
		}
	}
	if record.TorrentURL != "" {
		parsed, err := url.Parse(record.TorrentURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
			return errors.New("torrentUrl must be an HTTP(S) URL without credentials")
		}
		host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
			return errors.New("torrentUrl must not target a local host")
		}
		if ip := net.ParseIP(host); ip != nil && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
			return errors.New("torrentUrl must not target a non-public IP")
		}
	}
	if record.Episode != nil && (math.IsNaN(*record.Episode) || math.IsInf(*record.Episode, 0) || *record.Episode <= 0) {
		return errors.New("episode must be greater than zero")
	}
	if record.PublishedAt != "" {
		parsed, err := time.Parse(time.RFC3339, record.PublishedAt)
		if err != nil {
			return fmt.Errorf("publishedAt must be RFC 3339: %w", err)
		}
		record.PublishedAt = parsed.UTC().Format(time.RFC3339)
	}
	if record.SizeBytes != nil && *record.SizeBytes < 0 {
		return errors.New("sizeBytes must not be negative")
	}
	if record.Seeders != nil && *record.Seeders < 0 {
		return errors.New("seeders must not be negative")
	}
	if record.Resolution != "" && !resolutions[record.Resolution] {
		return errors.New("resolution is invalid")
	}
	record.Fansub = strings.TrimSpace(record.Fansub)
	seenLanguages := make(map[string]bool, len(record.SubtitleLanguages))
	for _, language := range record.SubtitleLanguages {
		if !languagePattern.MatchString(language) {
			return fmt.Errorf("invalid subtitle language %q", language)
		}
		if seenLanguages[language] {
			return fmt.Errorf("duplicate subtitle language %q", language)
		}
		seenLanguages[language] = true
	}
	sort.Strings(record.SubtitleLanguages)
	if record.SubtitleLanguages == nil {
		record.SubtitleLanguages = []string{}
	}
	return nil
}

func normalizeInfoHash(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) == 40 {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != 20 {
			return "", errors.New("infoHash must contain 40 hexadecimal or 32 base32 characters")
		}
		return strings.ToLower(value), nil
	}
	if len(value) == 32 {
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(value))
		if err != nil || len(decoded) != 20 {
			return "", errors.New("infoHash must contain 40 hexadecimal or 32 base32 characters")
		}
		return hex.EncodeToString(decoded), nil
	}
	return "", errors.New("infoHash must contain 40 hexadecimal or 32 base32 characters")
}

func validateMagnet(raw, expectedHash string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "magnet" {
		return errors.New("magnet must be a magnet URI")
	}
	foundBTIH := false
	for _, exactTopic := range parsed.Query()["xt"] {
		const prefix = "urn:btih:"
		if !strings.HasPrefix(strings.ToLower(exactTopic), prefix) {
			continue
		}
		foundBTIH = true
		actualHash, err := normalizeInfoHash(exactTopic[len(prefix):])
		if err != nil {
			return fmt.Errorf("magnet btih is invalid: %w", err)
		}
		if actualHash != expectedHash {
			return errors.New("magnet btih does not match infoHash")
		}
	}
	if !foundBTIH {
		return errors.New("magnet must contain an xt=urn:btih topic")
	}
	return nil
}

func rejectDuplicates(records []Record) error {
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		key := record.SourceID + "/" + record.InfoHash
		if seen[key] {
			return fmt.Errorf("duplicate sourceId/infoHash %s", key)
		}
		seen[key] = true
	}
	return nil
}

// NormalizeTitle is the stable key clients use for local title lookup.
func NormalizeTitle(value string) string {
	var builder strings.Builder
	separator := false
	for _, character := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			if separator && builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			builder.WriteRune(character)
			separator = false
		} else {
			separator = true
		}
	}
	return builder.String()
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
