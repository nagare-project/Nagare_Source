package repository

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadStandaloneFixture(t *testing.T) (*Validator, map[string]any, []byte) {
	t.Helper()
	root := repositoryRoot(t)
	validator, err := NewValidator(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "sources", "bt", "nyaa.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := DecodeDocument(data, "yaml")
	if err != nil {
		t.Fatal(err)
	}
	return validator, value.(map[string]any), data
}

func TestStandaloneSourceValidatesAndSummarizes(t *testing.T) {
	validator, document, _ := loadStandaloneFixture(t)
	validated, err := validator.ValidateStandaloneSource(document)
	if err != nil {
		t.Fatal(err)
	}
	summary := SummarizeSource(validated)
	if summary.ID != "nyaa" || summary.Kind != "bt" || summary.Name != "Nyaa" || summary.Tier != 3 || !summary.Enabled {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary.Version != "1" || summary.Ecosystem != "community" || summary.License != "AGPL-3.0" {
		t.Fatalf("unexpected origin summary: %+v", summary)
	}
	if len(summary.AllowedHosts) != 1 || summary.AllowedHosts[0] != "nyaa.si" {
		t.Fatalf("unexpected allowed hosts: %v", summary.AllowedHosts)
	}

	// 同一文档以 JSON 形式（宿主下载后原样保存的格式）也必须通过。
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	value, err := DecodeDocument(encoded, "json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.ValidateStandaloneSource(value); err != nil {
		t.Fatalf("JSON form rejected: %v", err)
	}
}

func TestStandaloneSourceRejectsSchemaAndLintViolations(t *testing.T) {
	validator, _, _ := loadStandaloneFixture(t)

	cases := map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"missing search": {
			mutate: func(document map[string]any) { delete(document, "search") },
			want:   "search",
		},
		"bad id": {
			mutate: func(document map[string]any) { document["id"] = "../escape" },
			want:   "id",
		},
		"private request host": {
			mutate: func(document map[string]any) {
				request := document["search"].(map[string]any)["request"].(map[string]any)
				request["url"] = "http://127.0.0.1/?q={{title}}"
				request["allowed_hosts"] = []any{"127.0.0.1"}
			},
			want: "search.request.url",
		},
		"committed cookie": {
			mutate: func(document map[string]any) {
				request := document["search"].(map[string]any)["request"].(map[string]any)
				request["headers"] = map[string]any{"Cookie": "session=secret"}
			},
			want: "forbidden",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, document, _ := loadStandaloneFixture(t)
			testCase.mutate(document)
			_, err := validator.ValidateStandaloneSource(document)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("expected error containing %q, got %v", testCase.want, err)
			}
		})
	}

	if _, err := validator.ValidateStandaloneSource([]any{"not", "an", "object"}); err == nil {
		t.Fatal("non-object root unexpectedly passed")
	}
	if _, err := DecodeDocument([]byte(`{"a":1}{"b":2}`), "json"); err == nil || !strings.Contains(err.Error(), "multiple root") {
		t.Fatalf("multiple JSON roots returned unexpected error: %v", err)
	}
	if _, err := DecodeDocument([]byte(`a: 1`), "toml"); err == nil {
		t.Fatal("unknown format unexpectedly decoded")
	}
}

func TestLoadInstalledSourcesSkipsBadFilesIndependently(t *testing.T) {
	validator, document, yamlData := loadStandaloneFixture(t)
	dir := t.TempDir()

	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 合法：YAML 原文。
	write("nyaa.yaml", yamlData)
	// 合法：改 id 后以 JSON 保存。
	renamed := cloneObject(t, document)
	renamed["id"] = "nyaa-mirror"
	renamedJSON, _ := json.Marshal(renamed)
	write("nyaa-mirror.json", renamedJSON)
	// 同 id 第二份（按文件名排序在后）被跳过。
	write("nyaa.yml", yamlData)
	// 文件名与 id 不符。
	write("wrong-name.json", renamedJSON)
	// 解析失败。
	write("broken.json", []byte(`{"id":`))
	// 校验失败。
	invalid := cloneObject(t, document)
	invalid["id"] = "invalid"
	delete(invalid, "resolve")
	invalidJSON, _ := json.Marshal(invalid)
	write("invalid.json", invalidJSON)
	// 超过大小上限。
	write("huge.json", append([]byte(`{"pad":"`), append(make([]byte, MaxStandaloneSourceBytes), []byte(`"}`)...)...))
	// 忽略：隐藏文件、其他扩展名、子目录。
	write(".nyaa.json", renamedJSON)
	write("installed.txt", []byte("ignored"))
	if err := os.Mkdir(filepath.Join(dir, "nested.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	sources, problems := LoadInstalledSources(dir, validator)
	ids := []string{}
	for _, source := range sources {
		ids = append(ids, stringValue(source.Document["id"]))
	}
	if strings.Join(ids, ",") != "nyaa-mirror,nyaa" {
		t.Fatalf("unexpected loaded sources: %v", ids)
	}
	reported := map[string]string{}
	for _, problem := range problems {
		reported[filepath.Base(problem.Path)] = problem.Error()
	}
	expectations := map[string]string{
		"nyaa.yml":        "duplicate",
		"wrong-name.json": "filename must match",
		"broken.json":     "decode JSON",
		"invalid.json":    "resolve",
		"huge.json":       "exceeds 1 MiB",
	}
	if len(reported) != len(expectations) {
		t.Fatalf("unexpected problems: %v", reported)
	}
	for name, want := range expectations {
		if !strings.Contains(reported[name], want) {
			t.Errorf("%s: expected %q in %q", name, want, reported[name])
		}
	}
}

func TestLoadInstalledSourcesMissingDirectoryIsEmpty(t *testing.T) {
	validator, _, _ := loadStandaloneFixture(t)
	sources, problems := LoadInstalledSources(filepath.Join(t.TempDir(), "absent"), validator)
	if sources != nil || problems != nil {
		t.Fatalf("missing directory returned sources=%v problems=%v", sources, problems)
	}
}
