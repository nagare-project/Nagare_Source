package importer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

type Options struct {
	Upstream string
	License  string
}

type Diagnostic struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	Source   string `json:"source,omitempty"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

type Result struct {
	Sources     []map[string]any
	Diagnostics []Diagnostic
}

func (r Result) HasErrors() bool {
	for _, diagnostic := range r.Diagnostics {
		if diagnostic.Severity == "error" {
			return true
		}
	}
	return false
}

func (r *Result) add(severity, category, source, path, message string) {
	r.Diagnostics = append(r.Diagnostics, Diagnostic{
		Severity: severity,
		Category: category,
		Source:   source,
		Path:     path,
		Message:  message,
	})
}

func ValidateOptions(options Options) error {
	if strings.TrimSpace(options.Upstream) == "" {
		return errors.New("upstream URL is required")
	}
	parsed, err := url.Parse(options.Upstream)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return fmt.Errorf("upstream must be an absolute HTTP(S) URL")
	}
	if strings.TrimSpace(options.License) == "" {
		return errors.New("license identifier is required")
	}
	return nil
}

func WriteSources(outputDir string, sources []map[string]any) error {
	ordered := append([]map[string]any(nil), sources...)
	sort.Slice(ordered, func(i, j int) bool { return stringValue(ordered[i]["id"]) < stringValue(ordered[j]["id"]) })
	seen := make(map[string]bool, len(ordered))
	for _, source := range ordered {
		id := stringValue(source["id"])
		kind := stringValue(source["kind"])
		if id == "" || (kind != "web" && kind != "bt") {
			return fmt.Errorf("cannot write source with id %q and kind %q", id, kind)
		}
		if seen[id] {
			return fmt.Errorf("duplicate imported source id %q", id)
		}
		seen[id] = true
		directory := filepath.Join(outputDir, kind)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return err
		}
		data, err := yaml.Marshal(source)
		if err != nil {
			return fmt.Errorf("encode source %s: %w", id, err)
		}
		temporary, err := os.CreateTemp(directory, "."+id+"-*.tmp")
		if err != nil {
			return err
		}
		temporaryPath := temporary.Name()
		cleanup := func() {
			temporary.Close()
			os.Remove(temporaryPath)
		}
		if _, err := temporary.Write(data); err != nil {
			cleanup()
			return err
		}
		if err := temporary.Chmod(0o644); err != nil {
			cleanup()
			return err
		}
		if err := temporary.Close(); err != nil {
			os.Remove(temporaryPath)
			return err
		}
		if err := os.Rename(temporaryPath, filepath.Join(directory, id+".yaml")); err != nil {
			os.Remove(temporaryPath)
			return err
		}
	}
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

func decodeJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON root values")
		}
		return fmt.Errorf("invalid trailing JSON content: %w", err)
	}
	return nil
}

func decodeYAML(data []byte) (map[string]any, error) {
	var value any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	return document, nil
}

func stableID(name string, canonical []byte) (string, bool) {
	var output strings.Builder
	lastDash := false
	lossy := false
	for _, character := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			output.WriteRune(character)
			lastDash = false
		case character == '-' || character == '_' || character == '.' || unicode.IsSpace(character):
			if output.Len() > 0 && !lastDash {
				output.WriteByte('-')
				lastDash = true
			}
		default:
			lossy = true
			if output.Len() > 0 && !lastDash {
				output.WriteByte('-')
				lastDash = true
			}
		}
	}
	id := strings.Trim(output.String(), "-")
	digest := sha256.Sum256(canonical)
	suffix := hex.EncodeToString(digest[:4])
	if id == "" {
		id = "source-" + suffix
		lossy = true
	} else if lossy {
		if len(id) > 53 {
			id = strings.Trim(id[:53], "-")
		}
		id += "-" + suffix
	}
	if len(id) == 1 {
		id += "-" + suffix
		lossy = true
	}
	if len(id) > 63 {
		id = strings.Trim(id[:54], "-") + "-" + suffix
		lossy = true
	}
	return id, lossy
}

func sourceOrigin(options Options, ecosystem, version string, canonical []byte) map[string]any {
	digest := sha256.Sum256(canonical)
	return map[string]any{
		"ecosystem": ecosystem,
		"upstream":  options.Upstream,
		"version":   version,
		"license":   options.License,
		"digest":    "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func hostFromTemplate(raw string) (string, error) {
	replacer := regexp.MustCompile(`\{\{?[^{}]+\}?\}`)
	parsed, err := url.Parse(replacer.ReplaceAllString(raw, "template"))
	if err != nil {
		return "", err
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", errors.New("URL has no host")
	}
	return host, nil
}

func baseURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host + "/"
}

func canonicalJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func objectValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func arrayValue(value any) []any {
	result, _ := value.([]any)
	return result
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func intValue(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := strconv.Atoi(typed.String())
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func boolValue(value any, fallback bool) bool {
	result, ok := value.(bool)
	if !ok {
		return fallback
	}
	return result
}

func stringMap(value any) map[string]string {
	object := objectValue(value)
	result := make(map[string]string, len(object))
	for key, item := range object {
		if text, ok := item.(string); ok {
			result[key] = text
		}
	}
	return result
}
