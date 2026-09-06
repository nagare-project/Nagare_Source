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
	if summary.Sources != 2 || summary.Requests != 1 || summary.Candidates != 2 {
		t.Fatalf("unexpected validation summary: %+v", summary)
	}
}

func TestBuildIsDeterministicAndDigestMatches(t *testing.T) {
	root := repositoryRoot(t)
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	generatedAt := "2026-01-02T03:04:05Z"

	index, err := Build(root, first, "0.1.0-test", generatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(root, second, "0.1.0-test", generatedAt); err != nil {
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
	if len(index.Sources) != 2 || index.Sources[0].ID != "example-http" || index.Sources[1].ID != "example-rss" {
		t.Fatalf("sources are not sorted by id: %+v", index.Sources)
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
