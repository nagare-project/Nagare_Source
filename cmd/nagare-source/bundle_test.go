package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 捆绑包只能含 BT 规则：安装包随播放器分发，在线规则绝不能混进去。
func TestBundleContainsOnlyBTSourcesAndValidates(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "repo")
	if err := bundle([]string{"--root", root, "--out", out, "--generated-at", "2026-09-12T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "sources", "web")); !os.IsNotExist(err) {
		t.Fatalf("bundle must not carry web sources: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "sources", "upstreams")); !os.IsNotExist(err) {
		t.Fatalf("bundle must not carry imported upstream sources: %v", err)
	}
	for _, required := range []string{
		filepath.Join("sources", "bt", "garden.yaml"),
		filepath.Join("schema", "source-v1.schema.json"),
		filepath.Join("reports", "health.json"),
		filepath.Join("upstreams", "approved.json"),
		filepath.Join("fixtures", "responses", "garden.json"),
	} {
		if _, err := os.Stat(filepath.Join(out, required)); err != nil {
			t.Fatalf("bundle is missing %s: %v", required, err)
		}
	}
	health, err := os.ReadFile(filepath.Join(out, "reports", "health.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(health), `"id": "garden"`) || strings.Contains(string(health), `"id": "7sefun"`) {
		t.Fatalf("bundle health report must cover exactly the bundled sources: %s", health)
	}
	if err := bundle([]string{"--root", root, "--out", out}); err == nil {
		t.Fatal("bundling into a non-empty directory must fail")
	}
}
