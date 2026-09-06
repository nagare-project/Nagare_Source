package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
