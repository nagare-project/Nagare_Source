package repository

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/btindex"
	"github.com/santhosh-tekuri/jsonschema/v5"
	"gopkg.in/yaml.v3"
)

const (
	SourceSchemaName     = "source-v1.schema.json"
	RequestSchemaName    = "resolve-request-v1.schema.json"
	CandidateSchemaName  = "candidate-v1.schema.json"
	IndexSchemaName      = "repository-index-v1.schema.json"
	HealthSchemaName     = "source-health-v1.schema.json"
	ApprovalsSchemaName  = "upstream-approvals-v1.schema.json"
	defaultGeneratedTime = "1970-01-01T00:00:00Z"
)

var templatePattern = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_]*)\s*\}\}`)
var unresolvedTemplatePattern = regexp.MustCompile(`\{\{|\}\}`)

var sourceErrorCategories = map[string]bool{
	"invalid_request":      true,
	"unsupported_protocol": true,
	"source_disabled":      true,
	"unsupported_factory":  true,
	"unsupported_rule":     true,
	"interactive_required": true,
	"search_timeout":       true,
	"search_failed":        true,
	"no_subject_match":     true,
	"episode_not_found":    true,
	"episode_ambiguous":    true,
	"resolve_timeout":      true,
	"resolve_failed":       true,
	"browser_blocked":      true,
	"unsafe_redirect":      true,
	"invalid_candidate":    true,
	"cancelled":            true,
}

// Validator owns the compiled JSON Schemas used by both the CLI and tests.
type Validator struct {
	schemas map[string]*jsonschema.Schema
}

type ValidationSummary struct {
	Sources       int
	Requests      int
	Candidates    int
	BTRecords     int
	HealthReports int
	Approvals     int
}

type SourceDocument struct {
	Path     string
	Document map[string]any
}

type UpstreamApproval struct {
	ID                string                 `json:"id"`
	Ecosystem         string                 `json:"ecosystem"`
	Repository        string                 `json:"repository"`
	Ref               string                 `json:"ref"`
	ReviewedRevision  string                 `json:"reviewedRevision"`
	InputPath         string                 `json:"inputPath"`
	OutputPath        string                 `json:"outputPath"`
	SourceURLTemplate string                 `json:"sourceUrlTemplate"`
	StableIDStrategy  string                 `json:"stableIdStrategy"`
	License           UpstreamLicense        `json:"license"`
	Redistribution    UpstreamRedistribution `json:"redistribution"`
}

type UpstreamLicense struct {
	SPDX         string `json:"spdx"`
	UpstreamPath string `json:"upstreamPath"`
	NoticePath   string `json:"noticePath"`
	NoticeSHA256 string `json:"noticeSha256"`
}

type UpstreamRedistribution struct {
	Rules             bool `json:"rules"`
	NormalizedSources bool `json:"normalizedSources"`
	Fixtures          bool `json:"fixtures"`
}

type Index struct {
	Schema               string       `json:"schema"`
	Version              string       `json:"version"`
	GeneratedAt          string       `json:"generatedAt"`
	SourceSchemaVersions []int        `json:"sourceSchemaVersions"`
	Artifacts            Artifacts    `json:"artifacts"`
	Sources              []IndexEntry `json:"sources"`
}

type Artifacts struct {
	BTIndex BTIndexArtifact `json:"btIndex"`
	Health  HealthArtifact  `json:"health"`
}

type BTIndexArtifact struct {
	Path          string `json:"path"`
	Format        string `json:"format"`
	SchemaVersion int    `json:"schemaVersion"`
	Records       int    `json:"records"`
	Digest        string `json:"digest"`
}

type HealthArtifact struct {
	Path          string `json:"path"`
	Format        string `json:"format"`
	SchemaVersion int    `json:"schemaVersion"`
	GeneratedAt   string `json:"generatedAt"`
	Digest        string `json:"digest"`
}

type IndexEntry struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Kind         string      `json:"kind"`
	Tier         int         `json:"tier"`
	Version      string      `json:"version"`
	Path         string      `json:"path"`
	Digest       string      `json:"digest"`
	Capabilities []string    `json:"capabilities"`
	Origin       IndexOrigin `json:"origin"`
}

type IndexOrigin struct {
	Ecosystem string `json:"ecosystem"`
	Upstream  string `json:"upstream"`
	License   string `json:"license"`
}

func NewValidator(root string) (*Validator, error) {
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	compiler.AssertFormat = true

	names := []string{SourceSchemaName, RequestSchemaName, CandidateSchemaName, IndexSchemaName, HealthSchemaName, ApprovalsSchemaName}
	for _, name := range names {
		path := filepath.Join(root, "schema", name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read schema %s: %w", name, err)
		}
		if err := compiler.AddResource(name, bytes.NewReader(data)); err != nil {
			return nil, fmt.Errorf("load schema %s: %w", name, err)
		}
	}

	compiled := make(map[string]*jsonschema.Schema, len(names))
	for _, name := range names {
		schema, err := compiler.Compile(name)
		if err != nil {
			return nil, fmt.Errorf("compile schema %s: %w", name, err)
		}
		compiled[name] = schema
	}
	return &Validator{schemas: compiled}, nil
}

func (v *Validator) Validate(schemaName string, document any) error {
	schema, ok := v.schemas[schemaName]
	if !ok {
		return fmt.Errorf("unknown schema %q", schemaName)
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("%s: %w", schemaName, err)
	}
	return nil
}

// ValidateSource validates one generated source, including the repository's
// semantic and security lints. The file need not exist, but its name and parent
// directory are checked against the source id and kind.
func (v *Validator) ValidateSource(root, path string, document map[string]any) error {
	if err := v.Validate(SourceSchemaName, document); err != nil {
		return err
	}
	return lintSource(root, path, document)
}

func LoadDocument(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var value any
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, &value); err != nil {
			return nil, fmt.Errorf("decode YAML: %w", err)
		}
		// Round-tripping through JSON normalizes YAML values to JSON-compatible data.
		normalized, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("normalize YAML: %w", err)
		}
		if err := json.Unmarshal(normalized, &value); err != nil {
			return nil, fmt.Errorf("normalize YAML as JSON: %w", err)
		}
	case ".json":
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
		return nil, fmt.Errorf("unsupported document extension %q", filepath.Ext(path))
	}
	return value, nil
}

func ValidateRepository(root string) (ValidationSummary, error) {
	validator, err := NewValidator(root)
	if err != nil {
		return ValidationSummary{}, err
	}

	sources, err := LoadSources(root, validator)
	if err != nil {
		return ValidationSummary{}, err
	}
	summary := ValidationSummary{Sources: len(sources)}

	requestPaths, err := documentPaths(filepath.Join(root, "fixtures", "requests"), ".json")
	if err != nil {
		return summary, err
	}
	for _, path := range requestPaths {
		if err := validateFile(validator, RequestSchemaName, path); err != nil {
			return summary, err
		}
		summary.Requests++
	}

	candidatePaths, err := documentPaths(filepath.Join(root, "fixtures", "candidates"), ".json")
	if err != nil {
		return summary, err
	}
	for _, path := range candidatePaths {
		if err := validateFile(validator, CandidateSchemaName, path); err != nil {
			return summary, err
		}
		summary.Candidates++
	}

	if err := validateNDJSONFixtures(root, validator); err != nil {
		return summary, err
	}
	btPaths, err := documentPaths(filepath.Join(root, "fixtures", "bt-index"), ".jsonl")
	if err != nil {
		return summary, err
	}
	for _, path := range btPaths {
		records, err := btindex.LoadJSONL(path)
		if err != nil {
			return summary, err
		}
		if err := validateBTRecordSources(sources, records); err != nil {
			return summary, fmt.Errorf("%s: %w", relative(root, path), err)
		}
		summary.BTRecords += len(records)
	}
	if err := validateHealthReport(root, validator, sources); err != nil {
		return summary, err
	}
	summary.HealthReports = 1
	approvals, err := validateUpstreamApprovals(root, validator)
	if err != nil {
		return summary, err
	}
	summary.Approvals = approvals
	return summary, nil
}

func validateUpstreamApprovals(root string, validator *Validator) (int, error) {
	path := filepath.Join(root, "upstreams", "approved.json")
	value, err := LoadDocument(path)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", relative(root, path), err)
	}
	if err := validator.Validate(ApprovalsSchemaName, value); err != nil {
		return 0, fmt.Errorf("%s: %w", relative(root, path), err)
	}
	document := objectValue(value)
	upstreams := arrayValue(document["upstreams"])
	previous := ""
	for _, item := range upstreams {
		upstream := objectValue(item)
		id := stringValue(upstream["id"])
		if previous != "" && id <= previous {
			return 0, fmt.Errorf("%s: upstream IDs must be unique and sorted", relative(root, path))
		}
		previous = id
		if err := validateApproval(root, upstream); err != nil {
			return 0, fmt.Errorf("%s: upstream %q: %w", relative(root, path), id, err)
		}
	}
	return len(upstreams), nil
}

func ApprovedUpstream(root, id string) (UpstreamApproval, error) {
	upstreams, err := ApprovedUpstreams(root)
	if err != nil {
		return UpstreamApproval{}, err
	}
	for _, upstream := range upstreams {
		if upstream.ID == id {
			return upstream, nil
		}
	}
	return UpstreamApproval{}, fmt.Errorf("upstream %q is not approved", id)
}

func ApprovedUpstreams(root string) ([]UpstreamApproval, error) {
	validator, err := NewValidator(root)
	if err != nil {
		return nil, err
	}
	if _, err := validateUpstreamApprovals(root, validator); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, "upstreams", "approved.json"))
	if err != nil {
		return nil, err
	}
	var registry struct {
		Upstreams []UpstreamApproval `json:"upstreams"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("decode approved upstream registry: %w", err)
	}
	return registry.Upstreams, nil
}

func validateApproval(root string, upstream map[string]any) error {
	if _, err := safeRepositoryPath(root, stringValue(upstream["inputPath"])); err != nil {
		return fmt.Errorf("invalid input path: %w", err)
	}
	id := stringValue(upstream["id"])
	outputPath := stringValue(upstream["outputPath"])
	if _, err := safeRepositoryPath(root, outputPath); err != nil {
		return fmt.Errorf("invalid output path: %w", err)
	}
	if outputPath != "sources/upstreams/"+id {
		return errors.New("outputPath must be the upstream's dedicated sources/upstreams directory")
	}
	repositoryURL := stringValue(upstream["repository"])
	parsed, err := url.Parse(repositoryURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("repository must be a plain HTTPS URL")
	}
	ref := stringValue(upstream["ref"])
	if strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") {
		return errors.New("ref contains an unsafe path segment")
	}
	template := stringValue(upstream["sourceUrlTemplate"])
	wantPrefix := strings.TrimSuffix(repositoryURL, "/") + "/blob/{revision}/"
	if !strings.HasPrefix(template, wantPrefix) || strings.Count(template, "{revision}") != 1 || strings.Count(template, "{path}") != 1 || !strings.HasSuffix(template, "{path}") {
		return errors.New("sourceUrlTemplate must bind the approved repository, revision, and path")
	}
	license := objectValue(upstream["license"])
	if _, err := safeRepositoryPath(root, stringValue(license["upstreamPath"])); err != nil {
		return fmt.Errorf("invalid upstream license path: %w", err)
	}
	noticePath := stringValue(license["noticePath"])
	absoluteNotice, err := safeRepositoryPath(root, noticePath)
	if err != nil {
		return fmt.Errorf("invalid license notice path: %w", err)
	}
	info, err := os.Lstat(absoluteNotice)
	if err != nil {
		return fmt.Errorf("read license notice: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("license notice must be a regular file")
	}
	data, err := os.ReadFile(absoluteNotice)
	if err != nil {
		return fmt.Errorf("read license notice: %w", err)
	}
	digest := sha256.Sum256(data)
	actual := "sha256:" + hex.EncodeToString(digest[:])
	if actual != stringValue(license["noticeSha256"]) {
		return fmt.Errorf("license notice digest mismatch: got %s", actual)
	}
	return nil
}

func safeRepositoryPath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != filepath.FromSlash(name) {
		return "", errors.New("path must be a normalized repository-relative path")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absolutePath := filepath.Join(absoluteRoot, filepath.FromSlash(name))
	relativePath, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes the repository")
	}
	return absolutePath, nil
}

func validateHealthReport(root string, validator *Validator, repositorySources []SourceDocument) error {
	path := filepath.Join(root, "reports", "health.json")
	value, err := LoadDocument(path)
	if err != nil {
		return fmt.Errorf("%s: %w", relative(root, path), err)
	}
	if err := validator.Validate(HealthSchemaName, value); err != nil {
		return fmt.Errorf("%s: %w", relative(root, path), err)
	}
	document := objectValue(value)
	sources := arrayValue(document["sources"])
	summary := objectValue(document["summary"])
	if intValue(summary["total"]) != len(sources) {
		return fmt.Errorf("%s: summary.total does not match sources", relative(root, path))
	}
	counts := map[string]int{}
	expected := make(map[string]bool, len(repositorySources))
	for _, source := range repositorySources {
		expected[stringValue(source.Document["id"])] = true
	}
	previous := ""
	for _, value := range sources {
		source := objectValue(value)
		id := stringValue(source["id"])
		if previous != "" && id <= previous {
			return fmt.Errorf("%s: sources must have unique IDs in ascending order", relative(root, path))
		}
		if !expected[id] {
			return fmt.Errorf("%s: health report references unknown source %q", relative(root, path), id)
		}
		delete(expected, id)
		counts[stringValue(source["status"])]++
		previous = id
	}
	if len(expected) > 0 {
		return fmt.Errorf("%s: health report does not cover every repository source", relative(root, path))
	}
	for status, summaryName := range map[string]string{
		"healthy": "healthy", "degraded": "degraded", "unavailable": "unavailable",
		"interactive_required": "interactiveRequired", "disabled": "disabled",
	} {
		if intValue(summary[summaryName]) != counts[status] {
			return fmt.Errorf("%s: summary.%s does not match source statuses", relative(root, path), summaryName)
		}
	}
	return nil
}

func LoadSources(root string, validator *Validator) ([]SourceDocument, error) {
	paths, err := sourcePaths(filepath.Join(root, "sources"))
	if err != nil {
		return nil, err
	}
	seen := make(map[string]string, len(paths))
	sources := make([]SourceDocument, 0, len(paths))
	for _, path := range paths {
		value, err := LoadDocument(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", relative(root, path), err)
		}
		if err := validator.Validate(SourceSchemaName, value); err != nil {
			return nil, fmt.Errorf("%s: %w", relative(root, path), err)
		}
		document, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: source root must be an object", relative(root, path))
		}
		if err := lintSource(root, path, document); err != nil {
			return nil, err
		}
		id := stringValue(document["id"])
		if previous, exists := seen[id]; exists {
			return nil, fmt.Errorf("duplicate source id %q in %s and %s", id, relative(root, previous), relative(root, path))
		}
		seen[id] = path
		sources = append(sources, SourceDocument{Path: path, Document: document})
	}
	return sources, nil
}

func Build(root, outputDir, version, generatedAt string) (Index, error) {
	return build(root, outputDir, version, generatedAt, nil)
}

// BuildWithBTRecords includes normalized crawler output in the release. The
// entire JSONL input is validated before the output directory is touched.
func BuildWithBTRecords(root, outputDir, version, generatedAt, recordsPath string) (Index, error) {
	records, err := btindex.LoadJSONL(recordsPath)
	if err != nil {
		return Index{}, err
	}
	return build(root, outputDir, version, generatedAt, records)
}

func build(root, outputDir, version, generatedAt string, btRecords []btindex.Record) (Index, error) {
	cleanRoot := filepath.Clean(root)
	cleanOutput := filepath.Clean(outputDir)
	volumeRoot := filepath.VolumeName(cleanOutput) + string(os.PathSeparator)
	unsafeOutput := cleanOutput == volumeRoot || filepath.Dir(cleanOutput) == volumeRoot || cleanOutput == cleanRoot
	if home, err := os.UserHomeDir(); err == nil && cleanOutput == filepath.Clean(home) {
		unsafeOutput = true
	}
	if relativeRoot, err := filepath.Rel(cleanOutput, cleanRoot); err == nil && relativeRoot != "." && relativeRoot != ".." && !strings.HasPrefix(relativeRoot, ".."+string(os.PathSeparator)) {
		// The output is an ancestor of the repository, so replacing it would erase inputs.
		unsafeOutput = true
	}
	if unsafeOutput {
		return Index{}, fmt.Errorf("refusing to replace unsafe output directory %q", outputDir)
	}
	validator, err := NewValidator(root)
	if err != nil {
		return Index{}, err
	}
	approvals, err := ApprovedUpstreams(root)
	if err != nil {
		return Index{}, err
	}
	sources, err := LoadSources(root, validator)
	if err != nil {
		return Index{}, err
	}
	if strings.TrimSpace(version) == "" {
		return Index{}, errors.New("release version must not be empty")
	}
	when, err := time.Parse(time.RFC3339, generatedAt)
	if err != nil {
		return Index{}, fmt.Errorf("generated-at must be RFC 3339: %w", err)
	}
	if err := validateBTRecordSources(sources, btRecords); err != nil {
		return Index{}, err
	}
	if err := validateHealthReport(root, validator, sources); err != nil {
		return Index{}, err
	}
	healthValue, err := LoadDocument(filepath.Join(root, "reports", "health.json"))
	if err != nil {
		return Index{}, err
	}
	healthData, err := json.MarshalIndent(healthValue, "", "  ")
	if err != nil {
		return Index{}, err
	}
	healthData = append(healthData, '\n')
	healthDocument := objectValue(healthValue)

	parent := filepath.Dir(outputDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Index{}, err
	}
	temporary, err := os.MkdirTemp(parent, ".nagare-dist-")
	if err != nil {
		return Index{}, err
	}
	defer os.RemoveAll(temporary)
	if err := os.MkdirAll(filepath.Join(temporary, "sources"), 0o755); err != nil {
		return Index{}, err
	}
	if err := copyLicenseNotices(root, temporary, approvals); err != nil {
		return Index{}, err
	}

	index := Index{
		Schema:               "nagare-repository-index/v1",
		Version:              version,
		GeneratedAt:          when.UTC().Format(time.RFC3339),
		SourceSchemaVersions: []int{1},
		Sources:              make([]IndexEntry, 0, len(sources)),
	}
	artifact, err := btindex.Build(btRecords, when, filepath.Join(temporary, "bt-index.sqlite.zst"))
	if err != nil {
		return Index{}, fmt.Errorf("build BT index: %w", err)
	}
	index.Artifacts.BTIndex = BTIndexArtifact{
		Path:          "bt-index.sqlite.zst",
		Format:        "sqlite3+zstd",
		SchemaVersion: btindex.SchemaVersion,
		Records:       artifact.Records,
		Digest:        artifact.Digest,
	}
	healthDigest := sha256.Sum256(healthData)
	index.Artifacts.Health = HealthArtifact{
		Path: "health.json", Format: "json", SchemaVersion: 1,
		GeneratedAt: stringValue(healthDocument["generatedAt"]), Digest: "sha256:" + hex.EncodeToString(healthDigest[:]),
	}
	if err := os.WriteFile(filepath.Join(temporary, index.Artifacts.Health.Path), healthData, 0o644); err != nil {
		return Index{}, err
	}
	for _, source := range sources {
		data, err := canonicalJSON(source.Document)
		if err != nil {
			return Index{}, fmt.Errorf("encode %s: %w", relative(root, source.Path), err)
		}
		digest := sha256.Sum256(data)
		id := stringValue(source.Document["id"])
		destination := filepath.Join(temporary, "sources", id+".json")
		if err := os.WriteFile(destination, prettyJSON(data), 0o644); err != nil {
			return Index{}, err
		}

		origin := objectValue(source.Document["origin"])
		resolve := objectValue(source.Document["resolve"])
		kind := stringValue(source.Document["kind"])
		capabilities := []string{kind, stringValue(resolve["mode"])}
		sort.Strings(capabilities)
		index.Sources = append(index.Sources, IndexEntry{
			ID:           id,
			Name:         stringValue(source.Document["name"]),
			Kind:         kind,
			Tier:         intValue(source.Document["tier"]),
			Version:      stringValue(origin["version"]),
			Path:         "sources/" + id + ".json",
			Digest:       "sha256:" + hex.EncodeToString(digest[:]),
			Capabilities: capabilities,
			Origin: IndexOrigin{
				Ecosystem: stringValue(origin["ecosystem"]),
				Upstream:  stringValue(origin["upstream"]),
				License:   stringValue(origin["license"]),
			},
		})
	}
	sort.Slice(index.Sources, func(i, j int) bool { return index.Sources[i].ID < index.Sources[j].ID })
	if err := validator.Validate(IndexSchemaName, toJSONValue(index)); err != nil {
		return Index{}, fmt.Errorf("generated index is invalid: %w", err)
	}
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return Index{}, err
	}
	indexData = append(indexData, '\n')
	if err := os.WriteFile(filepath.Join(temporary, "index.json"), indexData, 0o644); err != nil {
		return Index{}, err
	}

	if err := os.RemoveAll(outputDir); err != nil {
		return Index{}, fmt.Errorf("replace output directory: %w", err)
	}
	if err := os.Rename(temporary, outputDir); err != nil {
		return Index{}, fmt.Errorf("publish output directory: %w", err)
	}
	return index, nil
}

func copyLicenseNotices(root, output string, approvals []UpstreamApproval) error {
	seen := map[string]bool{}
	for _, approval := range approvals {
		if !approval.Redistribution.NormalizedSources {
			continue
		}
		name := filepath.Base(approval.License.NoticePath)
		if seen[name] {
			return fmt.Errorf("duplicate third-party license notice filename %q", name)
		}
		seen[name] = true
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(approval.License.NoticePath)))
		if err != nil {
			return fmt.Errorf("read third-party license notice for %s: %w", approval.ID, err)
		}
		directory := filepath.Join(output, "licenses")
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func validateBTRecordSources(sources []SourceDocument, records []btindex.Record) error {
	btSources := make(map[string]bool)
	for _, source := range sources {
		if stringValue(source.Document["kind"]) == "bt" {
			btSources[stringValue(source.Document["id"])] = true
		}
	}
	for _, record := range records {
		if !btSources[record.SourceID] {
			return fmt.Errorf("BT record references missing or non-BT source %q", record.SourceID)
		}
	}
	return nil
}

func DefaultGeneratedAt(root string) string {
	if epoch := strings.TrimSpace(os.Getenv("SOURCE_DATE_EPOCH")); epoch != "" {
		seconds, err := strconv.ParseInt(epoch, 10, 64)
		if err == nil {
			return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
		}
	}
	command := exec.Command("git", "log", "-1", "--format=%cI")
	command.Dir = root
	if output, err := command.Output(); err == nil {
		if parsed, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(string(output))); parseErr == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return defaultGeneratedTime
}

func lintSource(root, path string, document map[string]any) error {
	id := stringValue(document["id"])
	if base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)); base != id {
		return fmt.Errorf("%s: filename must match source id %q", relative(root, path), id)
	}
	expectedKind := filepath.Base(filepath.Dir(path))
	if kind := stringValue(document["kind"]); expectedKind != kind {
		return fmt.Errorf("%s: kind %q must match sources/%s directory", relative(root, path), kind, expectedKind)
	}

	variables := map[string]bool{
		"title": true, "keyword": true, "source": true, "episode": true,
		"episode_number": true, "episode_index": true, "line_key": true,
		"line_index": true, "line_number": true, "page": true,
	}
	collectFieldNames(document, variables)
	if err := walkDocument(document, "", func(location string, value any) error {
		text, ok := value.(string)
		if !ok {
			return nil
		}
		if strings.Contains(text, "{{") || strings.Contains(text, "}}") {
			if err := lintTemplate(text, variables); err != nil {
				return fmt.Errorf("%s: %s: %w", relative(root, path), location, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := lintRegexes(document); err != nil {
		return fmt.Errorf("%s: %w", relative(root, path), err)
	}
	if err := lintRequests(document); err != nil {
		return fmt.Errorf("%s: %w", relative(root, path), err)
	}
	return nil
}

func lintTemplate(text string, variables map[string]bool) error {
	matches := templatePattern.FindAllStringSubmatch(text, -1)
	stripped := templatePattern.ReplaceAllString(text, "")
	if len(matches) == 0 || unresolvedTemplatePattern.MatchString(stripped) {
		return errors.New("malformed template expression")
	}
	for _, match := range matches {
		if !variables[match[1]] {
			return fmt.Errorf("unknown template variable %q", match[1])
		}
	}
	return nil
}

func lintRegexes(document map[string]any) error {
	return walkDocument(document, "", func(location string, value any) error {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		if stringValue(object["type"]) == "regex" {
			pattern := stringValue(object["expression"])
			if pattern == "" {
				pattern = stringValue(object["pattern"])
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("%s: invalid RE2 expression: %w", location, err)
			}
		}
		if match, ok := object["match"].(map[string]any); ok {
			for _, key := range []string{"include", "exclude", "nested_include"} {
				for _, pattern := range arrayValue(match[key]) {
					if _, err := regexp.Compile(stringValue(pattern)); err != nil {
						return fmt.Errorf("%s.match.%s: invalid RE2 expression: %w", location, key, err)
					}
				}
			}
		}
		return nil
	})
}

func lintRequests(document map[string]any) error {
	return walkDocument(document, "", func(location string, value any) error {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		for _, headerKey := range []string{"headers", "request_headers"} {
			if headers, ok := object[headerKey].(map[string]any); ok {
				for key := range headers {
					if strings.EqualFold(key, "authorization") || strings.EqualFold(key, "cookie") {
						return fmt.Errorf("%s.%s: committed %s values are forbidden; use runtime credentials or cookie_policy", location, headerKey, key)
					}
				}
			}
		}
		rawURL, hasURL := object["url"].(string)
		if !hasURL || !strings.HasPrefix(rawURL, "http") {
			return nil
		}
		host, err := lintPublicURL(rawURL)
		if err != nil {
			return fmt.Errorf("%s.url: %w", location, err)
		}
		if allowed := arrayValue(object["allowed_hosts"]); len(allowed) > 0 && !hostAllowed(host, allowed) {
			return fmt.Errorf("%s.allowed_hosts: request host %q is not allowed", location, host)
		}
		return nil
	})
}

func lintPublicURL(raw string) (string, error) {
	replaced := templatePattern.ReplaceAllString(raw, "template")
	parsed, err := url.Parse(replaced)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("scheme %q is not allowed", parsed.Scheme)
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", errors.New("URL has no host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return "", fmt.Errorf("local host %q is forbidden", host)
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
		return "", fmt.Errorf("non-public IP %q is forbidden", host)
	}
	return host, nil
}

func hostAllowed(host string, allowed []any) bool {
	for _, item := range allowed {
		pattern := strings.ToLower(stringValue(item))
		if pattern == host {
			return true
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
				return true
			}
		}
	}
	return false
}

func collectFieldNames(value any, variables map[string]bool) {
	object, ok := value.(map[string]any)
	if !ok {
		if values, ok := value.([]any); ok {
			for _, child := range values {
				collectFieldNames(child, variables)
			}
		}
		return
	}
	for _, collection := range []string{"fields", "variables"} {
		if fields, ok := object[collection].(map[string]any); ok {
			for name := range fields {
				variables[name] = true
			}
		}
	}
	for _, child := range object {
		collectFieldNames(child, variables)
	}
}

func walkDocument(value any, location string, visit func(string, any) error) error {
	if err := visit(location, value); err != nil {
		return err
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			childLocation := key
			if location != "" {
				childLocation = location + "." + key
			}
			if err := walkDocument(typed[key], childLocation, visit); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := walkDocument(child, fmt.Sprintf("%s[%d]", location, index), visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateFile(validator *Validator, schemaName, path string) error {
	document, err := LoadDocument(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := validator.Validate(schemaName, document); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func validateNDJSONFixtures(root string, validator *Validator) error {
	paths, err := documentPaths(filepath.Join(root, "fixtures", "ndjson"), ".jsonl")
	if err != nil {
		return err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := bytes.Split(data, []byte("\n"))
		seenDone := false
		candidateIDs := make(map[string]bool)
		for index, line := range lines {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			if seenDone {
				return fmt.Errorf("%s:%d: events after done are forbidden", relative(root, path), index+1)
			}
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				return fmt.Errorf("%s:%d: each NDJSON line must be complete JSON: %w", relative(root, path), index+1, err)
			}
			switch stringValue(event["event"]) {
			case "candidate":
				if err := requireOnlyKeys(event, "event", "candidate"); err != nil {
					return fmt.Errorf("%s:%d: %w", relative(root, path), index+1, err)
				}
				if err := validator.Validate(CandidateSchemaName, event["candidate"]); err != nil {
					return fmt.Errorf("%s:%d: %w", relative(root, path), index+1, err)
				}
				candidate := objectValue(event["candidate"])
				id := stringValue(candidate["id"])
				if candidateIDs[id] {
					return fmt.Errorf("%s:%d: duplicate candidate id %q", relative(root, path), index+1, id)
				}
				candidateIDs[id] = true
			case "source_error":
				if err := requireOnlyKeys(event, "event", "sourceId", "category", "message", "retryable"); err != nil {
					return fmt.Errorf("%s:%d: %w", relative(root, path), index+1, err)
				}
				category := stringValue(event["category"])
				if stringValue(event["sourceId"]) == "" || category == "" || stringValue(event["message"]) == "" {
					return fmt.Errorf("%s:%d: source_error requires sourceId, category, and message", relative(root, path), index+1)
				}
				if _, ok := event["retryable"].(bool); !ok {
					return fmt.Errorf("%s:%d: source_error retryable must be boolean", relative(root, path), index+1)
				}
				if !sourceErrorCategories[category] {
					return fmt.Errorf("%s:%d: unknown source_error category %q", relative(root, path), index+1, category)
				}
			case "done":
				if err := requireOnlyKeys(event, "event", "queried", "succeeded", "failed", "durationMs"); err != nil {
					return fmt.Errorf("%s:%d: %w", relative(root, path), index+1, err)
				}
				queried, okQueried := nonNegativeInteger(event["queried"])
				succeeded, okSucceeded := nonNegativeInteger(event["succeeded"])
				failed, okFailed := nonNegativeInteger(event["failed"])
				_, okDuration := nonNegativeInteger(event["durationMs"])
				if !okQueried || !okSucceeded || !okFailed || !okDuration {
					return fmt.Errorf("%s:%d: done counters must be non-negative integers", relative(root, path), index+1)
				}
				if queried != succeeded+failed {
					return fmt.Errorf("%s:%d: done queried must equal succeeded + failed", relative(root, path), index+1)
				}
				seenDone = true
			default:
				return fmt.Errorf("%s:%d: unknown event %q", relative(root, path), index+1, event["event"])
			}
		}
		if !seenDone {
			return fmt.Errorf("%s: stream must end with a done event", relative(root, path))
		}
	}
	return nil
}

func requireOnlyKeys(object map[string]any, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		set[key] = true
	}
	for key := range object {
		if !set[key] {
			return fmt.Errorf("unknown event property %q", key)
		}
	}
	for _, key := range allowed {
		if _, ok := object[key]; !ok {
			return fmt.Errorf("event property %q is required", key)
		}
	}
	return nil
}

func nonNegativeInteger(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok || number < 0 || number != float64(int64(number)) {
		return 0, false
	}
	return int64(number), true
}

func sourcePaths(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		extension := strings.ToLower(filepath.Ext(path))
		if extension == ".yaml" || extension == ".yml" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func documentPaths(root, extension string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), extension) {
			paths = append(paths, filepath.Join(root, entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func canonicalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func prettyJSON(data []byte) []byte {
	var output bytes.Buffer
	if err := json.Indent(&output, data, "", "  "); err != nil {
		return append(data, '\n')
	}
	output.WriteByte('\n')
	return output.Bytes()
}

func toJSONValue(value any) any {
	data, _ := json.Marshal(value)
	var document any
	_ = json.Unmarshal(data, &document)
	return document
}

func relative(root, path string) string {
	value, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(value)
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
	result, _ := value.(string)
	return result
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	case json.Number:
		value, _ := typed.Int64()
		return int(value)
	default:
		return 0
	}
}
