// Package btcrawler executes the BT-search subset of Source Spec v1 and emits
// normalized records suitable for the deterministic BT index builder.
package btcrawler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nagare-project/Nagare_Source/internal/btindex"
)

type Query struct {
	Title   string
	Episode float64
	Page    int
}

type Request struct {
	Method       string
	URL          string
	Headers      http.Header
	Body         []byte
	AllowedHosts []string
	TimeoutMS    int
	MaxBytes     int64
	MaxRedirects int
	ResponseType string
}

type Response struct {
	URL  string
	Body []byte
}

type Fetcher interface {
	Fetch(context.Context, Request) (Response, error)
}

type Diagnostic struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	Source   string `json:"source"`
	Item     int    `json:"item,omitempty"`
	Message  string `json:"message"`
}

type Result struct {
	Records     []btindex.Record
	Diagnostics []Diagnostic
}

// Crawl performs exactly one Source Spec search request. Pagination and title
// alias scheduling remain the caller's responsibility.
func Crawl(ctx context.Context, source map[string]any, query Query, fetcher Fetcher) (Result, error) {
	if stringValue(source["schema"]) != "nagare-source/v1" {
		return Result{}, errors.New("source schema must be nagare-source/v1")
	}
	if stringValue(source["kind"]) != "bt" {
		return Result{}, errors.New("BT crawler only accepts kind=bt sources")
	}
	if enabled, exists := source["enabled"].(bool); exists && !enabled {
		return Result{}, errors.New("source is disabled")
	}
	if fetcher == nil {
		return Result{}, errors.New("fetcher is required")
	}
	request, err := buildRequest(source, query)
	if err != nil {
		return Result{}, err
	}
	response, err := fetcher.Fetch(ctx, request)
	if err != nil {
		return Result{}, fmt.Errorf("fetch %s: %w", redactedURL(request.URL), err)
	}
	if response.URL == "" {
		response.URL = request.URL
	}
	rows, rowDiagnostics, err := extractRows(source, request.ResponseType, response)
	if err != nil {
		return Result{}, err
	}

	sourceID := stringValue(source["id"])
	result := Result{Diagnostics: rowDiagnostics}
	byKey := make(map[string]btindex.Record)
	for index, row := range rows {
		record, err := recordFromRow(source, row)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{
				Severity: "warning", Category: "item_rejected", Source: sourceID,
				Item: index + 1, Message: err.Error(),
			})
			continue
		}
		if !matchesQuery(source, record, query) {
			continue
		}
		key := record.Key()
		if previous, exists := byKey[key]; exists {
			previousJSON, _ := json.Marshal(previous)
			currentJSON, _ := json.Marshal(record)
			if bytes.Compare(currentJSON, previousJSON) < 0 {
				byKey[key] = record
			}
			result.Diagnostics = append(result.Diagnostics, Diagnostic{
				Severity: "warning", Category: "duplicate_collapsed", Source: sourceID,
				Item: index + 1, Message: "duplicate sourceId/infoHash collapsed deterministically",
			})
			continue
		}
		byKey[key] = record
	}
	result.Records = make([]btindex.Record, 0, len(byKey))
	for _, record := range byKey {
		result.Records = append(result.Records, record)
	}
	sort.Slice(result.Records, func(i, j int) bool {
		if result.Records[i].SourceID != result.Records[j].SourceID {
			return result.Records[i].SourceID < result.Records[j].SourceID
		}
		return result.Records[i].InfoHash < result.Records[j].InfoHash
	})
	return result, nil
}

func buildRequest(source map[string]any, query Query) (Request, error) {
	search := objectValue(source["search"])
	requestDocument := objectValue(search["request"])
	if len(requestDocument) == 0 {
		return Request{}, errors.New("search.request is required")
	}
	if query.Page <= 0 {
		query.Page = 1
	}
	variables := map[string]string{
		"title":          query.Title,
		"keyword":        query.Title,
		"source":         stringValue(source["id"]),
		"episode":        formatNumber(query.Episode),
		"episode_number": formatNumber(query.Episode),
		"page":           strconv.Itoa(query.Page),
	}
	rawURL, err := renderTemplate(stringValue(requestDocument["url"]), variables, true)
	if err != nil {
		return Request{}, fmt.Errorf("search.request.url: %w", err)
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return Request{}, fmt.Errorf("search.request.url: %w", err)
	}
	queryValues := parsedURL.Query()
	queryDocument := objectValue(requestDocument["query"])
	queryKeys := sortedKeys(queryDocument)
	for _, key := range queryKeys {
		value, err := renderTemplate(fmt.Sprint(queryDocument[key]), variables, false)
		if err != nil {
			return Request{}, fmt.Errorf("search.request.query.%s: %w", key, err)
		}
		queryValues.Set(key, value)
	}
	parsedURL.RawQuery = queryValues.Encode()

	headers := make(http.Header)
	headerDocument := objectValue(requestDocument["headers"])
	for _, key := range sortedKeys(headerDocument) {
		value, err := renderTemplate(fmt.Sprint(headerDocument[key]), variables, false)
		if err != nil {
			return Request{}, fmt.Errorf("search.request.headers.%s: %w", key, err)
		}
		headers.Set(key, value)
	}
	method := stringValue(requestDocument["method"])
	var body []byte
	if rawBody, exists := requestDocument["body"].(string); exists {
		var rendered string
		var err error
		if strings.Contains(strings.ToLower(headers.Get("Content-Type")), "application/json") {
			rendered, err = renderJSONTemplate(rawBody, variables)
		} else {
			rendered, err = renderTemplate(rawBody, variables, false)
		}
		if err != nil {
			return Request{}, fmt.Errorf("search.request.body: %w", err)
		}
		if strings.Contains(strings.ToLower(headers.Get("Content-Type")), "application/json") && !json.Valid([]byte(rendered)) {
			return Request{}, errors.New("search.request.body: rendered body is not valid JSON")
		}
		body = []byte(rendered)
	}
	if formDocument := objectValue(requestDocument["form"]); len(formDocument) > 0 {
		form := make(url.Values)
		for _, key := range sortedKeys(formDocument) {
			value, err := renderTemplate(fmt.Sprint(formDocument[key]), variables, false)
			if err != nil {
				return Request{}, fmt.Errorf("search.request.form.%s: %w", key, err)
			}
			form.Set(key, value)
		}
		body = []byte(form.Encode())
		headers.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	limits := objectValue(source["limits"])
	request := Request{
		Method:       method,
		URL:          parsedURL.String(),
		Headers:      headers,
		Body:         body,
		AllowedHosts: stringSlice(requestDocument["allowed_hosts"]),
		TimeoutMS:    intOr(requestDocument["timeout_ms"], intOr(limits["timeout_ms"], 15000)),
		MaxBytes:     int64(intOr(requestDocument["max_bytes"], intOr(limits["max_response_bytes"], 5*1024*1024))),
		MaxRedirects: intOr(requestDocument["max_redirects"], intOr(limits["max_redirects"], 3)),
		ResponseType: stringValue(search["response"]),
	}
	if err := validateRequestTarget(request.URL, request.AllowedHosts); err != nil {
		return Request{}, fmt.Errorf("search.request.url: %w", err)
	}
	return request, nil
}

func recordFromRow(source map[string]any, row map[string]string) (btindex.Record, error) {
	downloadURL := firstValue(row, "download_url")
	magnet := firstValue(row, "magnet")
	torrentURL := firstValue(row, "torrent_url")
	if downloadURL != "" {
		switch {
		case strings.HasPrefix(strings.ToLower(downloadURL), "magnet:?") && magnet == "":
			magnet = downloadURL
		case (strings.HasPrefix(strings.ToLower(downloadURL), "http://") || strings.HasPrefix(strings.ToLower(downloadURL), "https://")) && torrentURL == "":
			torrentURL = downloadURL
		}
	}
	infoHash := firstValue(row, "info_hash", "infohash")
	if infoHash == "" && magnet != "" {
		parsed, err := url.Parse(magnet)
		if err == nil {
			for _, exactTopic := range parsed.Query()["xt"] {
				if strings.HasPrefix(strings.ToLower(exactTopic), "urn:btih:") {
					infoHash = exactTopic[len("urn:btih:"):]
					break
				}
			}
		}
	}
	record := btindex.Record{
		Schema:      btindex.RecordSchema,
		SourceID:    stringValue(source["id"]),
		InfoHash:    infoHash,
		Magnet:      magnet,
		TorrentURL:  torrentURL,
		Title:       firstValue(row, "title"),
		PublishedAt: firstValue(row, "published_at", "date"),
		Fansub:      firstValue(row, "fansub"),
		Resolution:  firstValue(row, "resolution"),
	}
	if value := firstValue(row, "episode"); value != "" {
		episode, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return btindex.Record{}, fmt.Errorf("episode: %w", err)
		}
		record.Episode = &episode
	}
	if value := firstValue(row, "size_bytes", "size"); value != "" {
		size, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return btindex.Record{}, fmt.Errorf("size_bytes: %w", err)
		}
		record.SizeBytes = &size
	}
	if value := firstValue(row, "seeders"); value != "" {
		seeders, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return btindex.Record{}, fmt.Errorf("seeders: %w", err)
		}
		record.Seeders = &seeders
	}
	defaults := objectValue(source["defaults"])
	if record.Resolution == "" {
		record.Resolution = stringValue(defaults["resolution"])
	}
	record.SubtitleLanguages = stringSlice(defaults["subtitle_languages"])
	normalized, err := btindex.NormalizeRecord(record)
	if err != nil {
		return btindex.Record{}, err
	}
	return normalized, nil
}

func matchesQuery(source map[string]any, record btindex.Record, query Query) bool {
	matching := objectValue(source["matching"])
	if query.Episode > 0 && boolOr(matching["require_episode"], false) {
		if record.Episode == nil || *record.Episode != query.Episode {
			return false
		}
	}
	if query.Title != "" && boolOr(matching["require_subject"], false) {
		wanted := btindex.NormalizeTitle(query.Title)
		if wanted != "" && !strings.Contains(btindex.NormalizeTitle(record.Title), wanted) {
			return false
		}
	}
	return true
}

func SelfTestQuery(source map[string]any) (Query, error) {
	request := objectValue(objectValue(source["selftest"])["request"])
	if len(request) == 0 {
		return Query{}, errors.New("source has no selftest request")
	}
	query := Query{Page: 1}
	if value := stringValue(request["query"]); value != "" {
		query.Title = value
	} else if titles := stringSlice(request["titles"]); len(titles) > 0 {
		query.Title = titles[0]
	}
	query.Episode = floatValue(request["episode"])
	if query.Title == "" {
		return Query{}, errors.New("selftest has no query title")
	}
	return query, nil
}

func CheckSelfTest(source map[string]any, result Result) error {
	expect := objectValue(objectValue(source["selftest"])["expect"])
	if len(expect) == 0 {
		return errors.New("source has no selftest expectation")
	}
	minimum := intOr(expect["min_candidates"], 0)
	if len(result.Records) < minimum {
		return fmt.Errorf("selftest found %d candidates; expected at least %d", len(result.Records), minimum)
	}
	if transport := stringValue(expect["transport"]); transport != "" && transport != "torrent" {
		return fmt.Errorf("BT source cannot satisfy selftest transport %q", transport)
	}
	return nil
}

func WriteJSONL(path string, records []btindex.Record) error {
	normalizedRecords := make([]btindex.Record, 0, len(records))
	seen := make(map[string]bool, len(records))
	for index, record := range records {
		normalized, err := btindex.NormalizeRecord(record)
		if err != nil {
			return fmt.Errorf("record %d: %w", index+1, err)
		}
		key := normalized.Key()
		if seen[key] {
			return fmt.Errorf("duplicate sourceId/infoHash %s", key)
		}
		seen[key] = true
		normalizedRecords = append(normalizedRecords, normalized)
	}
	sort.Slice(normalizedRecords, func(i, j int) bool {
		if normalizedRecords[i].SourceID != normalizedRecords[j].SourceID {
			return normalizedRecords[i].SourceID < normalizedRecords[j].SourceID
		}
		return normalizedRecords[i].InfoHash < normalizedRecords[j].InfoHash
	})
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, normalized := range normalizedRecords {
		if err := encoder.Encode(normalized); err != nil {
			return err
		}
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".nagare-bt-records-*.jsonl")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	succeeded := false
	defer func() {
		temporary.Close()
		if !succeeded {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(output.Bytes()); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	succeeded = true
	return nil
}

func EncodeDiagnostics(diagnostics []Diagnostic) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, diagnostic := range diagnostics {
		if err := encoder.Encode(diagnostic); err != nil {
			return nil, err
		}
	}
	return output.Bytes(), nil
}

func renderTemplate(value string, variables map[string]string, escapeURL bool) (string, error) {
	var output strings.Builder
	remainder := value
	for {
		start := strings.Index(remainder, "{{")
		unexpectedClose := strings.Index(remainder, "}}")
		if start < 0 {
			if unexpectedClose >= 0 {
				return "", errors.New("malformed template")
			}
			output.WriteString(remainder)
			return output.String(), nil
		}
		if unexpectedClose >= 0 && unexpectedClose < start {
			return "", errors.New("malformed template")
		}
		output.WriteString(remainder[:start])
		endRelative := strings.Index(remainder[start+2:], "}}")
		if endRelative < 0 {
			return "", errors.New("malformed template")
		}
		end := start + 2 + endRelative
		name := strings.TrimSpace(remainder[start+2 : end])
		replacement, exists := variables[name]
		if !exists {
			return "", fmt.Errorf("unknown template variable %q", name)
		}
		if escapeURL {
			replacement = url.QueryEscape(replacement)
		}
		output.WriteString(replacement)
		remainder = remainder[end+2:]
	}
}

func renderJSONTemplate(value string, variables map[string]string) (string, error) {
	escaped := make(map[string]string, len(variables))
	for name, variable := range variables {
		encoded, err := json.Marshal(variable)
		if err != nil {
			return "", err
		}
		escaped[name] = string(encoded[1 : len(encoded)-1])
	}
	return renderTemplate(value, escaped, false)
}

func formatNumber(value float64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func redactedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	if parsed.RawQuery != "" {
		parsed.RawQuery = "[REDACTED]"
	}
	parsed.User = nil
	return parsed.String()
}

func firstValue(values map[string]string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(values[name]); value != "" {
			return value
		}
	}
	return ""
}

func objectValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func stringSlice(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func intOr(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	}
	return fallback
}

func floatValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case int:
		return float64(typed)
	}
	return 0
}

func boolOr(value any, fallback bool) bool {
	if result, ok := value.(bool); ok {
		return result
	}
	return fallback
}
