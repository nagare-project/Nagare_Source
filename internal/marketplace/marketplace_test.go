package marketplace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nagare-project/Nagare_Source/internal/repository"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureRoot 构造一个最小仓库：schema、一条启用的 nyaa、一条禁用的示例，以及一条描述里带 HTML 的来源。
func fixtureRoot(t *testing.T) string {
	t.Helper()
	repo := repositoryRoot(t)
	root := t.TempDir()
	entries, err := os.ReadDir(filepath.Join(repo, "schema"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		copyFile(t, filepath.Join(repo, "schema", entry.Name()), filepath.Join(root, "schema", entry.Name()))
	}
	copyFile(t, filepath.Join(repo, "sources", "bt", "nyaa.yaml"), filepath.Join(root, "sources", "bt", "nyaa.yaml"))
	copyFile(t, filepath.Join(repo, "sources", "bt", "example-rss.yaml"), filepath.Join(root, "sources", "bt", "example-rss.yaml"))

	data, err := os.ReadFile(filepath.Join(repo, "sources", "bt", "nyaa.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := repository.DecodeDocument(data, "yaml")
	if err != nil {
		t.Fatal(err)
	}
	document := value.(map[string]any)
	document["id"] = "nyaa-mirror"
	document["name"] = "Mirror <b>bold</b>"
	document["description"] = `<img src=x onerror="alert(1)">`
	document["tier"] = 5
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sources", "bt", "nyaa-mirror.yaml"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func decodePack(t *testing.T, data []byte) (Pack, []json.RawMessage) {
	t.Helper()
	var pack Pack
	if err := json.Unmarshal(data, &pack); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Sources []json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return pack, raw.Sources
}

func TestBuildPublishesPackWithMatchingDigests(t *testing.T) {
	root := fixtureRoot(t)
	site, err := Build(root, Options{GeneratedAt: "2026-01-02T03:04:05Z", Health: map[string]string{"nyaa": "healthy"}})
	if err != nil {
		t.Fatal(err)
	}
	if site.Index.Schema != Schema || site.Index.GeneratedAt != "2026-01-02T03:04:05Z" {
		t.Fatalf("unexpected index header: %+v", site.Index)
	}
	// 夹具里只有 BT 来源，因此只发布 BT 来源包；没有成员的在线播放来源包不出现。
	if len(site.Index.Packs) != 1 || len(site.Documents) != 1 {
		t.Fatalf("unexpected packs: %+v", site.Index.Packs)
	}
	entry := site.Index.Packs[0]
	if entry.ID != "nagare-source-bt" || entry.Kind != "bt" || entry.Name != "BT 磁力" || entry.Path != "packs/bt.json" {
		t.Fatalf("unexpected pack entry: %+v", entry)
	}
	ids := []string{}
	for _, member := range entry.Sources {
		ids = append(ids, member.ID)
	}
	// 禁用的示例不进市场；包内按 tier 降序。
	if strings.Join(ids, ",") != "nyaa-mirror,nyaa" || site.Index.SourceCount() != 2 {
		t.Fatalf("unexpected pack members: %v", ids)
	}
	if entry.Sources[1].Status != "healthy" || entry.Sources[0].Status != "" {
		t.Fatalf("health statuses not attached: %+v", entry.Sources)
	}

	data, ok := site.Documents[entry.Path]
	if !ok {
		t.Fatalf("missing pack document at %q", entry.Path)
	}
	digest := sha256.Sum256(data)
	if entry.SHA256 != hex.EncodeToString(digest[:]) || entry.Bytes != len(data) || len(data) > MaxPackBytes {
		t.Fatal("digest/size do not match served bytes")
	}
	pack, members := decodePack(t, data)
	if pack.Schema != PackSchema || pack.ID != entry.ID || pack.Name != entry.Name || pack.Version != entry.Version || len(pack.Version) != 12 {
		t.Fatalf("pack header mismatch: %+v", pack)
	}
	if strings.Contains(string(data), "healthy") || strings.Contains(string(data), "2026-01-02") {
		t.Fatal("health and generation time must stay out of the pack so its digest is stable")
	}
	validator, err := repository.NewValidator(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("unexpected member count: %d", len(members))
	}
	// 安装端会把每个成员的原始字节单独保存成 <id>.json，所以每个成员都必须能独立通过校验。
	for index, raw := range members {
		value, err := repository.DecodeDocument(raw, "json")
		if err != nil {
			t.Fatal(err)
		}
		validated, err := validator.ValidateStandaloneSource(value)
		if err != nil {
			t.Fatalf("member %d is not installable: %v", index, err)
		}
		if summary := repository.SummarizeSource(validated); summary.ID != ids[index] || len(summary.AllowedHosts) == 0 {
			t.Fatalf("member %d summary mismatch: %+v", index, summary)
		}
	}

	var decoded Index
	if err := json.Unmarshal(site.IndexJSON, &decoded); err != nil || len(decoded.Packs) != 1 || len(decoded.Packs[0].Sources) != 2 {
		t.Fatalf("index.json does not round-trip: err=%v", err)
	}
}

func TestBuildPackDigestIsStableAcrossRuns(t *testing.T) {
	root := fixtureRoot(t)
	first, err := Build(root, Options{GeneratedAt: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(root, Options{GeneratedAt: "2026-02-01T00:00:00Z", Health: map[string]string{"nyaa": "degraded"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Index.Packs[0].SHA256 != second.Index.Packs[0].SHA256 {
		t.Fatal("pack digest must only depend on member content")
	}

	path := filepath.Join(root, "sources", "bt", "example-rss.yaml")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	unchanged, err := Build(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Index.Packs[0].Version != first.Index.Packs[0].Version {
		t.Fatal("removing a disabled source must not change the pack")
	}
	if err := os.Remove(filepath.Join(root, "sources", "bt", "nyaa-mirror.yaml")); err != nil {
		t.Fatal(err)
	}
	changed, err := Build(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Index.Packs[0].Version == first.Index.Packs[0].Version || len(changed.Index.Packs[0].Sources) != 1 {
		t.Fatal("changing pack members must change the version")
	}
}

func TestBuildEscapesUntrustedTextInPage(t *testing.T) {
	site, err := Build(fixtureRoot(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	page := string(site.HTML)
	if strings.Contains(page, "<img src=x") || strings.Contains(page, "<b>bold</b>") {
		t.Fatal("source text must be HTML-escaped on the marketplace page")
	}
	for _, want := range []string{"Nagare 来源市场", "&lt;img src=x", "/sources/install?url=", `data-path="packs/bt.json"`, "data-sha256=", "127.0.0.1:", "nyaa.si", "安装BT 磁力", "2 个来源"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page is missing %q", want)
		}
	}
	if strings.Count(page, `class="install"`) != 1 {
		t.Fatal("each pack must have exactly one install button")
	}
}

func TestBuildRepositorySourcesFormTwoInstallablePacks(t *testing.T) {
	site, err := Build(repositoryRoot(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(site.Index.Packs) != 2 || site.Index.Packs[0].Kind != "bt" || site.Index.Packs[1].Kind != "web" {
		t.Fatalf("expected BT and online playback packs, got %+v", site.Index.Packs)
	}
	seen := map[string]bool{}
	for _, pack := range site.Index.Packs {
		if len(pack.Sources) == 0 || pack.Bytes > MaxPackBytes {
			t.Fatalf("%s: unexpected pack size or members", pack.ID)
		}
		for _, member := range pack.Sources {
			if !member.Enabled || strings.HasPrefix(member.ID, "example-") || member.Kind != pack.Kind || seen[member.ID] {
				t.Fatalf("%s: unexpected member %s", pack.ID, member.ID)
			}
			seen[member.ID] = true
		}
	}
}

func TestWriteCreatesFilesAndRefusesNonEmptyDirectory(t *testing.T) {
	site, err := Build(fixtureRoot(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "marketplace")
	if err := site.Write(output); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.html", "index.json", "packs/bt.json"} {
		data, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if name == "packs/bt.json" && string(data) != string(site.Documents[name]) {
			t.Fatal("written pack differs from digested bytes")
		}
	}
	if err := site.Write(output); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected non-empty directory error, got %v", err)
	}
}

func TestLoadHealthReadsStatuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	if err := os.WriteFile(path, []byte(`{"schema":"nagare-source-health/v1","sources":[{"id":"nyaa","status":"healthy"},{"id":"x","status":"degraded"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	statuses, err := LoadHealth(path)
	if err != nil {
		t.Fatal(err)
	}
	if statuses["nyaa"] != "healthy" || statuses["x"] != "degraded" || len(statuses) != 2 {
		t.Fatalf("unexpected statuses: %v", statuses)
	}
	if err := os.WriteFile(path, []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHealth(path); err == nil {
		t.Fatal("expected decode error")
	}
}
