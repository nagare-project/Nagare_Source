package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritePluginReadyIsMachineReadable(t *testing.T) {
	var output bytes.Buffer
	if err := writePluginReady(&output, "127.0.0.1:43127"); err != nil {
		t.Fatal(err)
	}
	var event pluginReadyEvent
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Event != "ready" || event.Protocol != "nagare-plugin-launch/v1" || event.URL != "http://127.0.0.1:43127" {
		t.Fatalf("unexpected ready event: %+v", event)
	}
	if bytes.Count(output.Bytes(), []byte{'\n'}) != 1 {
		t.Fatalf("ready output must be exactly one JSON line: %q", output.String())
	}
}

func TestReadImportInputsRecursivelyAndDeterministically(t *testing.T) {
	directory := t.TempDir()
	paths := []string{
		filepath.Join(directory, "t1", "第二.json"),
		filepath.Join(directory, "t0", "first.json"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inputs, err := readImportInputs(directory, "animeko", "https://example.invalid/rules/{path}")
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 {
		t.Fatalf("got %d inputs, want 2", len(inputs))
	}
	if inputs[0].Upstream != "https://example.invalid/rules/t0/first.json" {
		t.Fatalf("inputs are not sorted or templated correctly: %+v", inputs)
	}
	if inputs[1].Upstream != "https://example.invalid/rules/t1/%E7%AC%AC%E4%BA%8C.json" {
		t.Fatalf("Unicode path was not URL-escaped: %s", inputs[1].Upstream)
	}
}

func TestServeOnlyAcceptsExplicitLoopbackAddresses(t *testing.T) {
	for _, address := range []string{"127.0.0.1:7788", "[::1]:0"} {
		if err := validateListenAddress(address); err != nil {
			t.Errorf("%s was rejected: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:7788", "localhost:7788", "192.0.2.1:7788", "missing-port"} {
		if err := validateListenAddress(address); err == nil {
			t.Errorf("unsafe listen address %s was accepted", address)
		}
	}
}

func TestNewPluginHandlerLoadsValidatedRepositorySources(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newPluginHandler(root, "test-version", "")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))
	if response.Code != http.StatusOK || !containsAll(response.Body.String(), "example-http", "example-rss") {
		t.Fatalf("serve handler did not expose repository sources: status=%d body=%s", response.Code, response.Body.String())
	}
	selfCheck := httptest.NewRequest(http.MethodPost, "/v1/selfcheck", strings.NewReader(`{"sourceIds":["example-rss"],"mode":"fixture"}`))
	selfCheck.Header.Set("Content-Type", "application/json")
	selfCheckResponse := httptest.NewRecorder()
	handler.ServeHTTP(selfCheckResponse, selfCheck)
	if selfCheckResponse.Code != http.StatusOK || !strings.Contains(selfCheckResponse.Body.String(), `"status":"healthy"`) {
		t.Fatalf("committed BT fixture was not connected to selfcheck: status=%d body=%s", selfCheckResponse.Code, selfCheckResponse.Body.String())
	}
}

func TestHealthCommandWritesValidatedJSONAndStaticPage(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	jsonPath := filepath.Join(directory, "health.json")
	htmlPath := filepath.Join(directory, "index.html")
	if err := health([]string{
		"--root", root, "--mode", "fixture", "--generated-at", "2026-09-07T00:00:00Z",
		"--out", jsonPath, "--html", htmlPath, "--concurrency", "2",
	}); err != nil {
		t.Fatal(err)
	}
	jsonData, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(string(jsonData), `"schema": "nagare-source-health/v1"`, `"healthy": 1`, `"degraded": 1`, `"id": "example-http"`, `"id": "example-rss"`) {
		t.Fatalf("health JSON is incomplete: %s", jsonData)
	}
	htmlData, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(string(htmlData), "Nagare Source Health", "example-http", "example-rss", "health.json") {
		t.Fatalf("health page is incomplete: %s", htmlData)
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

func TestReadBTSourcePathsRecursivelyAndDeterministically(t *testing.T) {
	directory := t.TempDir()
	paths := []string{
		filepath.Join(directory, "z", "second.yml"),
		filepath.Join(directory, "a", "first.yaml"),
		filepath.Join(directory, "ignored.json"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := readBTSourcePaths(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != 2 || actual[0] != paths[1] || actual[1] != paths[0] {
		t.Fatalf("unexpected BT source order: %v", actual)
	}
}

func TestReadKazumiInputsSkipsRepositoryIndex(t *testing.T) {
	directory := t.TempDir()
	for name, data := range map[string]string{
		"rule.json":  `{}`,
		"index.json": `[]`,
		"README.md":  `ignored`,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inputs, err := readImportInputs(directory, "kazumi", "https://example.invalid/{path}")
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || filepath.Base(inputs[0].Path) != "rule.json" {
		t.Fatalf("unexpected Kazumi inputs: %+v", inputs)
	}
}

func TestImportKazumiDirectoryEndToEnd(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "imported")
	diagnostics := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	err = importSources([]string{
		"kazumi",
		"--root", root,
		"--input", filepath.Join(root, "fixtures", "importers", "kazumi"),
		"--upstream", "https://rules.example.invalid/{path}",
		"--license", "MIT",
		"--out", output,
		"--overlays", "",
		"--diagnostics", diagnostics,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(output, "web", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("got %d generated Kazumi sources, want 2", len(paths))
	}
	if info, err := os.Stat(diagnostics); err != nil || info.Size() == 0 {
		t.Fatalf("diagnostics were not written: info=%v err=%v", info, err)
	}
}
