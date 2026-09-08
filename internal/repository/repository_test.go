package repository

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryFixturesValidate(t *testing.T) {
	root := repositoryRoot(t)
	summary, err := ValidateRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Sources < 2 || summary.Requests != 1 || summary.Candidates != 2 || summary.BTRecords != 1 || summary.HealthReports != 1 || summary.Approvals != 1 {
		t.Fatalf("unexpected validation summary: %+v", summary)
	}
}

func TestApprovedUpstreamsVerifyLicenseNoticeAndRepositoryBinding(t *testing.T) {
	root := repositoryRoot(t)
	validator, err := NewValidator(root)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := validateUpstreamApprovals(root, validator); err != nil || count != 1 {
		t.Fatalf("committed upstream approval is invalid: count=%d err=%v", count, err)
	}

	value, err := LoadDocument(filepath.Join(root, "upstreams", "approved.json"))
	if err != nil {
		t.Fatal(err)
	}
	upstream := value.(map[string]any)["upstreams"].([]any)[0].(map[string]any)
	template := upstream["sourceUrlTemplate"]
	upstream["sourceUrlTemplate"] = "https://attacker.example/blob/{revision}/{path}"
	if err := validateApproval(root, upstream); err == nil || !strings.Contains(err.Error(), "approved repository") {
		t.Fatalf("mismatched source template returned unexpected error: %v", err)
	}
	upstream["sourceUrlTemplate"] = template
	license := upstream["license"].(map[string]any)
	digest := license["noticeSha256"]
	license["noticeSha256"] = "sha256:" + strings.Repeat("0", 64)
	if err := validateApproval(root, upstream); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered license digest returned unexpected error: %v", err)
	}
	license["noticeSha256"] = digest
	inputPath := upstream["inputPath"]
	upstream["inputPath"] = "../outside"
	if err := validateApproval(root, upstream); err == nil || !strings.Contains(err.Error(), "invalid input path") {
		t.Fatalf("escaping input path returned unexpected error: %v", err)
	}
	upstream["inputPath"] = inputPath
	approval, err := ApprovedUpstream(root, "kazumi-rules")
	if err != nil || approval.Ecosystem != "kazumi" || approval.OutputPath != "sources/upstreams/kazumi-rules" {
		t.Fatalf("approved upstream lookup failed: approval=%+v err=%v", approval, err)
	}
	if _, err := ApprovedUpstream(root, "missing"); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("missing approval returned unexpected error: %v", err)
	}
}

func TestBuildIsDeterministicAndDigestMatches(t *testing.T) {
	root := repositoryRoot(t)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	generatedAt := "2026-01-02T03:04:05Z"
	recordsPath := filepath.Join(root, "fixtures", "bt-index", "releases.jsonl")

	index, err := BuildWithBTRecords(root, first, "0.1.0-test", generatedAt, recordsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildWithBTRecords(root, second, "0.1.0-test", generatedAt, recordsPath); err != nil {
		t.Fatal(err)
	}
	firstIndex, err := os.ReadFile(filepath.Join(first, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	secondIndex, err := os.ReadFile(filepath.Join(second, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstIndex, secondIndex) {
		t.Fatal("identical inputs produced different indexes")
	}
	firstBTIndex, err := os.ReadFile(filepath.Join(first, "bt-index.sqlite.zst"))
	if err != nil {
		t.Fatal(err)
	}
	secondBTIndex, err := os.ReadFile(filepath.Join(second, "bt-index.sqlite.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBTIndex, secondBTIndex) {
		t.Fatal("identical inputs produced different BT indexes")
	}
	if index.Artifacts.BTIndex.Records != 1 || index.Artifacts.BTIndex.Digest != sha256Digest(firstBTIndex) {
		t.Fatalf("unexpected BT artifact metadata: %+v", index.Artifacts.BTIndex)
	}
	firstHealth, err := os.ReadFile(filepath.Join(first, "health.json"))
	if err != nil {
		t.Fatal(err)
	}
	secondHealth, err := os.ReadFile(filepath.Join(second, "health.json"))
	if err != nil {
		t.Fatal(err)
	}
	var committedHealth struct {
		GeneratedAt string `json:"generatedAt"`
	}
	if err := json.Unmarshal(firstHealth, &committedHealth); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstHealth, secondHealth) || index.Artifacts.Health.Digest != sha256Digest(firstHealth) || index.Artifacts.Health.GeneratedAt != committedHealth.GeneratedAt {
		t.Fatalf("health artifact is not deterministic or indexed: %+v", index.Artifacts.Health)
	}
	if len(index.Sources) < 2 {
		t.Fatalf("release omitted repository sources: %+v", index.Sources)
	}
	foundExamples := map[string]bool{}
	for position, source := range index.Sources {
		if position > 0 && index.Sources[position-1].ID >= source.ID {
			t.Fatalf("sources are not sorted by id: %s before %s", index.Sources[position-1].ID, source.ID)
		}
		if source.ID == "example-http" || source.ID == "example-rss" {
			foundExamples[source.ID] = true
		}
	}
	if !foundExamples["example-http"] || !foundExamples["example-rss"] {
		t.Fatalf("release omitted example sources: %+v", foundExamples)
	}
	noticeName := "KazumiRules-MIT.txt"
	builtNotice, err := os.ReadFile(filepath.Join(first, "licenses", noticeName))
	if err != nil {
		t.Fatal(err)
	}
	committedNotice, err := os.ReadFile(filepath.Join(root, "third_party", "licenses", noticeName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(builtNotice, committedNotice) {
		t.Fatal("release bundle changed the approved upstream license notice")
	}
	for _, entry := range index.Sources {
		data, err := os.ReadFile(filepath.Join(first, filepath.FromSlash(entry.Path)))
		if err != nil {
			t.Fatal(err)
		}
		var document any
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		canonical, err := canonicalJSON(document)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256Digest(canonical)
		if entry.Digest != digest {
			t.Fatalf("digest mismatch for %s: got %s, want %s", entry.ID, entry.Digest, digest)
		}
	}
}

func TestSchemasRejectUnknownCandidateProperties(t *testing.T) {
	validator, err := NewValidator(repositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	document, err := LoadDocument(filepath.Join(repositoryRoot(t), "fixtures", "candidates", "http.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate := document.(map[string]any)
	candidate["temporarySecret"] = "must not pass"
	if err := validator.Validate(CandidateSchemaName, candidate); err == nil {
		t.Fatal("candidate with an unknown property unexpectedly passed")
	}
}

func TestSourceSchemaAcceptsLinesAndRejectsEpisodesForBT(t *testing.T) {
	root := repositoryRoot(t)
	validator, err := NewValidator(root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := LoadDocument(filepath.Join(root, "sources", "web", "example-http.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	source := cloneObject(t, value)
	source["episodes"] = map[string]any{
		"request": map[string]any{
			"method":        "GET",
			"url":           "https://media.example.invalid/show/{{subject_key}}",
			"allowed_hosts": []any{"media.example.invalid"},
		},
		"response": "json",
		"lines": map[string]any{
			"items":  "$.lines[*]",
			"fields": map[string]any{"channel": "$.name"},
			"episodes": map[string]any{
				"items":  "$.episodes[*]",
				"fields": map[string]any{"number": "$.number", "play_url": "$.url"},
			},
		},
	}
	if err := validator.Validate(SourceSchemaName, source); err != nil {
		t.Fatalf("multi-line web source should pass: %v", err)
	}
	source["kind"] = "bt"
	resolve := source["resolve"].(map[string]any)
	resolve["transport"] = "magnet"
	if err := validator.Validate(SourceSchemaName, source); err == nil {
		t.Fatal("BT source with an episodes stage unexpectedly passed")
	}
}

func TestContributionSourceTemplatesPassSchemaAndSecurityLint(t *testing.T) {
	root := repositoryRoot(t)
	validator, err := NewValidator(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		kind string
		id   string
	}{
		{name: "web.yaml", kind: "web", id: "template-web"},
		{name: "bt.yaml", kind: "bt", id: "template-bt"},
	} {
		value, err := LoadDocument(filepath.Join(root, "templates", test.name))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		document, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s: template root is not an object", test.name)
		}
		path := filepath.Join(root, "sources", test.kind, test.id+".yaml")
		if err := validator.ValidateSource(root, path, document); err != nil {
			t.Errorf("%s is not a valid copyable source: %v", test.name, err)
		}
	}
}

func TestSourceSchemaAcceptsEpisodeVariablesAndTemplateExtractor(t *testing.T) {
	root := repositoryRoot(t)
	validator, err := NewValidator(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sources", "web", "example-http.yaml")
	value, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	source := cloneObject(t, value)
	episodes := source["episodes"].(map[string]any)
	episodes["variables"] = map[string]any{
		"slug": map[string]any{"type": "jsonpath", "expression": "$.slug", "scope": "document"},
	}
	fields := episodes["fields"].(map[string]any)
	fields["play_url"] = map[string]any{
		"type":       "template",
		"expression": "https://media.example.invalid/{{slug}}?episode={{episode_index}}",
	}
	if err := validator.Validate(SourceSchemaName, source); err != nil {
		t.Fatalf("episode variables and template extractor should pass: %v", err)
	}
	if err := lintSource(root, path, source); err != nil {
		t.Fatalf("documented template variables should pass lint: %v", err)
	}
}

func TestLintRejectsPrivateNetworkAndUnknownVariables(t *testing.T) {
	if _, err := lintPublicURL("http://127.0.0.1/video.m3u8"); err == nil {
		t.Fatal("loopback URL unexpectedly passed")
	}
	if _, err := lintPublicURL("http://169.254.169.254/latest/meta-data"); err == nil {
		t.Fatal("link-local URL unexpectedly passed")
	}
	variables := map[string]bool{"title": true}
	if err := lintTemplate("https://example.invalid/{{secret}}", variables); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown variable returned unexpected error: %v", err)
	}
	source := map[string]any{
		"resolve": map[string]any{
			"url":             "{{play_url}}",
			"request_headers": map[string]any{"Cookie": "session=secret"},
		},
	}
	if err := lintRequests(source); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("fixed Cookie returned unexpected error: %v", err)
	}
}

func TestBuildRejectsRepositoryRootAsOutput(t *testing.T) {
	root := repositoryRoot(t)
	if _, err := Build(root, root, "test", "2026-01-02T03:04:05Z"); err == nil {
		t.Fatal("repository root unexpectedly accepted as generated output")
	}
}

func TestBuildEmitsEmptyBTIndex(t *testing.T) {
	root := repositoryRoot(t)
	output := filepath.Join(t.TempDir(), "dist")
	index, err := Build(root, output, "test", "2026-01-02T03:04:05Z")
	if err != nil {
		t.Fatal(err)
	}
	if index.Artifacts.BTIndex.Records != 0 {
		t.Fatalf("empty build reported %d BT records", index.Artifacts.BTIndex.Records)
	}
	if info, err := os.Stat(filepath.Join(output, index.Artifacts.BTIndex.Path)); err != nil || info.Size() == 0 {
		t.Fatalf("empty BT index was not emitted: info=%v err=%v", info, err)
	}
}

func TestBuildRejectsNonBTRecordWithoutReplacingOutput(t *testing.T) {
	root := repositoryRoot(t)
	directory := t.TempDir()
	recordsPath := filepath.Join(directory, "records.jsonl")
	record := `{"schema":"nagare-bt-record/v1","sourceId":"example-http","infoHash":"0123456789abcdef0123456789abcdef01234567","magnet":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567","title":"Example"}`
	if err := os.WriteFile(recordsPath, []byte(record+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "dist")
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(output, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("existing release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildWithBTRecords(root, output, "test", "2026-01-02T03:04:05Z", recordsPath); err == nil || !strings.Contains(err.Error(), "non-BT") {
		t.Fatalf("non-BT source returned unexpected error: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "existing release\n" {
		t.Fatalf("failed build replaced existing output: data=%q err=%v", data, err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func sha256Digest(data []byte) string {
	sum := sha256Bytes(data)
	return "sha256:" + sum
}

func cloneObject(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}
