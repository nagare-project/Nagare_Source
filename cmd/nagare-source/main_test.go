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

	"github.com/nagare-project/Nagare_Source/internal/repository"
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
	handler, err := newPluginHandler(root, "test-version", "", "")
	if err != nil {
		t.Fatal(err)
	}
	manifest := httptest.NewRecorder()
	handler.ServeHTTP(manifest, httptest.NewRequest(http.MethodGet, "/v1/manifest", nil))
	if manifest.Code != http.StatusOK || !strings.Contains(manifest.Body.String(), `"bt_releases"`) {
		t.Fatalf("serve manifest must advertise /v1/releases: status=%d body=%s", manifest.Code, manifest.Body.String())
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
	if selfCheckResponse.Code != http.StatusOK || !strings.Contains(selfCheckResponse.Body.String(), `"status":"disabled"`) {
		t.Fatalf("disabled example must stay disabled in plugin: status=%d body=%s", selfCheckResponse.Code, selfCheckResponse.Body.String())
	}
}

func TestNewPluginHandlerOverlaysInstalledSources(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	value, err := repository.LoadDocument(filepath.Join(root, "sources", "bt", "example-rss.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document := value.(map[string]any)
	installedDir := t.TempDir()
	writeSource := func(name string, document map[string]any) {
		t.Helper()
		data, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(installedDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 同 id 覆盖仓库来源：用户安装的版本启用并改名。
	document["enabled"] = true
	document["name"] = "Installed Override"
	writeSource("example-rss.json", document)
	// 全新的 id。
	document["id"] = "user-added"
	document["name"] = "User Added"
	writeSource("user-added.json", document)
	// 坏文件只被跳过，不影响插件启动。
	if err := os.WriteFile(filepath.Join(installedDir, "broken.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}

	handler, err := newPluginHandler(root, "test-version", "", installedDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest := httptest.NewRecorder()
	handler.ServeHTTP(manifest, httptest.NewRequest(http.MethodGet, "/v1/manifest", nil))
	if !strings.Contains(manifest.Body.String(), `"installed_sources"`) {
		t.Fatalf("manifest must advertise installed_sources: %s", manifest.Body.String())
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))
	var body struct {
		Sources []struct {
			ID, Name, Status string
			Enabled          bool
		} `json:"sources"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for _, source := range body.Sources {
		byID[source.ID]++
		switch source.ID {
		case "example-rss":
			if source.Name != "Installed Override" || !source.Enabled || source.Status != "healthy" {
				t.Errorf("installed override was not applied: %+v", source)
			}
		case "user-added":
			if source.Name != "User Added" || !source.Enabled {
				t.Errorf("installed source was not loaded: %+v", source)
			}
		}
	}
	if byID["example-rss"] != 1 || byID["user-added"] != 1 || byID["example-http"] != 1 {
		t.Fatalf("unexpected source set: %v", byID)
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
	// 不钉死 healthy 的具体数字：每接一条 BT 来源它就会变，钉数字只会让每次加源都改测试。
	if !containsAll(string(jsonData), `"schema": "nagare-source-health/v1"`, `"id": "garden"`, `"id": "mikan"`, `"id": "example-http"`, `"id": "example-rss"`) {
		t.Fatalf("health JSON is incomplete: %s", jsonData)
	}
	var report struct {
		Summary struct{ Healthy int } `json:"summary"`
	}
	if err := json.Unmarshal(jsonData, &report); err != nil || report.Summary.Healthy < 1 {
		t.Fatalf("health JSON must report at least one healthy source (bundled BT sources use fixtures): err=%v report=%s", err, jsonData)
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

func TestMarketplaceCommandWritesStaticSite(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	healthPath := filepath.Join(directory, "health.json")
	if err := os.WriteFile(healthPath, []byte(`{"sources":[{"id":"nyaa","status":"healthy"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "site")
	if err := marketplaceCommand([]string{"--root", root, "--out", output, "--health", healthPath, "--generated-at", "2026-09-07T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	indexData, err := os.ReadFile(filepath.Join(output, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(string(indexData), `"schema": "nagare-source-marketplace/v1"`, `"id": "nyaa"`, `"path": "packs/bt.json"`, `"path": "packs/web.json"`, `"status": "healthy"`) ||
		strings.Contains(string(indexData), "example-rss") {
		t.Fatalf("marketplace index is unexpected: %s", indexData)
	}
	for _, name := range []string{"bt.json", "web.json"} {
		if _, err := os.Stat(filepath.Join(output, "packs", name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(output, "sources")); !os.IsNotExist(err) {
		t.Fatalf("marketplace must not publish per-source files: %v", err)
	}
	if err := marketplaceCommand([]string{"--root", root, "--out", output}); err == nil {
		t.Fatal("marketplace must refuse a non-empty output directory")
	}
	if err := marketplaceCommand([]string{"--root", root}); err == nil {
		t.Fatal("marketplace must require --out")
	}
}
