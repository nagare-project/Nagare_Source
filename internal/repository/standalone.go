package repository

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxStandaloneSourceBytes 是单个独立安装来源文档的大小上限。
const MaxStandaloneSourceBytes = 1 << 20

// SourceSummary 是来源文档里给安装确认页展示的只读摘要。
type SourceSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Tier         int      `json:"tier"`
	Version      string   `json:"version"`
	Enabled      bool     `json:"enabled"`
	Description  string   `json:"description,omitempty"`
	Homepage     string   `json:"homepage,omitempty"`
	Ecosystem    string   `json:"ecosystem"`
	Upstream     string   `json:"upstream"`
	License      string   `json:"license"`
	AllowedHosts []string `json:"allowedHosts"`
}

// InstalledSourceError 记录独立安装目录里被跳过的文件；一个坏文件不影响其他来源。
type InstalledSourceError struct {
	Path string
	Err  error
}

func (e InstalledSourceError) Error() string {
	return fmt.Sprintf("%s: %v", filepath.Base(e.Path), e.Err)
}

func (e InstalledSourceError) Unwrap() error { return e.Err }

// DecodeDocument 解析内存中的 JSON 或 YAML 文档，结果与 LoadDocument 读文件一致。
func DecodeDocument(data []byte, format string) (any, error) {
	var value any
	switch format {
	case "yaml":
		if err := yaml.Unmarshal(data, &value); err != nil {
			return nil, fmt.Errorf("decode YAML: %w", err)
		}
		// 经 JSON 往返一次，把 YAML 值规整成 JSON 兼容的数据。
		normalized, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("normalize YAML: %w", err)
		}
		if err := json.Unmarshal(normalized, &value); err != nil {
			return nil, fmt.Errorf("normalize YAML as JSON: %w", err)
		}
	case "json":
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode JSON: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return nil, errors.New("decode JSON: multiple root values")
			}
			return nil, fmt.Errorf("decode JSON trailing content: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported document format %q", format)
	}
	return value, nil
}

func documentFormat(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return "yaml", true
	case ".json":
		return "json", true
	default:
		return "", false
	}
}

// ValidateStandaloneSource 校验不属于仓库 sources/ 目录的单个来源文档（用户从地址安装的来源）。
// 它与仓库来源走同一套 JSON Schema 和安全 lint（公网主机、allowed_hosts、禁止提交凭据等），
// 只是不要求文件位于 sources/<kind>/<id>.yaml。
func (v *Validator) ValidateStandaloneSource(value any) (map[string]any, error) {
	if err := v.Validate(SourceSchemaName, value); err != nil {
		return nil, err
	}
	document, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("source root must be an object")
	}
	id := stringValue(document["id"])
	kind := stringValue(document["kind"])
	// lintSource 用路径校验 id/kind；这里给一个与文档自身一致的虚拟路径。
	if err := lintSource("standalone", filepath.Join("standalone", kind, id+".json"), document); err != nil {
		return nil, err
	}
	return document, nil
}

// SummarizeSource 从已校验的来源文档提取展示摘要。
func SummarizeSource(document map[string]any) SourceSummary {
	origin := objectValue(document["origin"])
	enabled := true
	if value, ok := document["enabled"].(bool); ok {
		enabled = value
	}
	return SourceSummary{
		ID:           stringValue(document["id"]),
		Name:         stringValue(document["name"]),
		Kind:         stringValue(document["kind"]),
		Tier:         intValue(document["tier"]),
		Version:      stringValue(origin["version"]),
		Enabled:      enabled,
		Description:  stringValue(document["description"]),
		Homepage:     stringValue(document["homepage"]),
		Ecosystem:    stringValue(origin["ecosystem"]),
		Upstream:     stringValue(origin["upstream"]),
		License:      stringValue(origin["license"]),
		AllowedHosts: SourceAllowedHosts(document),
	}
}

// SourceAllowedHosts 汇总来源里所有请求声明的 allowed_hosts，去重并排序。
func SourceAllowedHosts(document map[string]any) []string {
	seen := map[string]bool{}
	_ = walkDocument(document, "", func(_ string, value any) error {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		for _, host := range arrayValue(object["allowed_hosts"]) {
			if text := strings.ToLower(strings.TrimSpace(stringValue(host))); text != "" {
				seen[text] = true
			}
		}
		return nil
	})
	hosts := make([]string, 0, len(seen))
	for host := range seen {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

// LoadInstalledSources 读取独立安装目录（不递归）中的来源文档。
//
// 与 LoadSources 不同，单个文件损坏只会被跳过并在第二个返回值里报告，
// 不让一个用户装坏的来源拖垮整个插件。目录不存在视为没有安装任何来源。
// 文件名必须等于来源 id；同一 id 出现多次时只保留按文件名排序的第一个。
func LoadInstalledSources(dir string, validator *Validator) ([]SourceDocument, []InstalledSourceError) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []InstalledSourceError{{Path: dir, Err: err}}
	}
	var (
		sources  []SourceDocument
		problems []InstalledSourceError
		seen     = map[string]bool{}
	)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !entry.Type().IsRegular() {
			continue
		}
		format, ok := documentFormat(name)
		if !ok {
			continue
		}
		path := filepath.Join(dir, name)
		document, err := loadInstalledSource(path, format, validator)
		if err != nil {
			problems = append(problems, InstalledSourceError{Path: path, Err: err})
			continue
		}
		id := stringValue(document["id"])
		if base := strings.TrimSuffix(name, filepath.Ext(name)); base != id {
			problems = append(problems, InstalledSourceError{Path: path, Err: fmt.Errorf("filename must match source id %q", id)})
			continue
		}
		if seen[id] {
			problems = append(problems, InstalledSourceError{Path: path, Err: fmt.Errorf("duplicate installed source id %q", id)})
			continue
		}
		seen[id] = true
		sources = append(sources, SourceDocument{Path: path, Document: document})
	}
	return sources, problems
}

func loadInstalledSource(path, format string, validator *Validator) (map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxStandaloneSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxStandaloneSourceBytes {
		return nil, errors.New("source document exceeds 1 MiB")
	}
	value, err := DecodeDocument(data, format)
	if err != nil {
		return nil, err
	}
	return validator.ValidateStandaloneSource(value)
}
