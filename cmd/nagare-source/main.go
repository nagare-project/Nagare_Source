package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nagare-project/Nagare_Source/internal/btcrawler"
	"github.com/nagare-project/Nagare_Source/internal/btindex"
	"github.com/nagare-project/Nagare_Source/internal/importer"
	"github.com/nagare-project/Nagare_Source/internal/repository"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "validate":
		err = validate(os.Args[2:])
	case "build":
		err = build(os.Args[2:])
	case "import":
		err = importSources(os.Args[2:])
	case "crawl-bt":
		err = crawlBT(os.Args[2:])
	case "serve":
		err = serve(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func crawlBT(arguments []string) error {
	flags := flag.NewFlagSet("crawl-bt", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root containing schema/")
	sourcePath := flags.String("source", "", "BT Source Spec YAML file or directory")
	output := flags.String("out", "", "normalized BT record JSONL output")
	title := flags.String("title", "", "search title or keyword")
	episode := flags.Float64("episode", 0, "optional episode number")
	page := flags.Int("page", 1, "search page number")
	responseFile := flags.String("response-file", "", "offline response fixture instead of a network request")
	responseURL := flags.String("response-url", "", "base URL for an offline response fixture")
	selftest := flags.Bool("selftest", false, "use and verify the source selftest definition")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *sourcePath == "" || *output == "" {
		return errors.New("--source and --out are required")
	}
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	validator, err := repository.NewValidator(absoluteRoot)
	if err != nil {
		return err
	}
	absoluteSource := *sourcePath
	if !filepath.IsAbs(absoluteSource) {
		absoluteSource = filepath.Join(absoluteRoot, absoluteSource)
	}
	sourcePaths, err := readBTSourcePaths(absoluteSource)
	if err != nil {
		return err
	}
	if *responseFile != "" && len(sourcePaths) != 1 {
		return errors.New("--response-file requires exactly one --source file")
	}
	var fixtureBody []byte
	if *responseFile != "" {
		fixturePath := *responseFile
		if !filepath.IsAbs(fixturePath) {
			fixturePath = filepath.Join(absoluteRoot, fixturePath)
		}
		fixtureBody, err = os.ReadFile(fixturePath)
		if err != nil {
			return err
		}
	}
	var records []btindex.Record
	var diagnostics []btcrawler.Diagnostic
	for _, currentPath := range sourcePaths {
		value, err := repository.LoadDocument(currentPath)
		if err != nil {
			return err
		}
		source, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: source root must be an object", currentPath)
		}
		if err := validator.ValidateSource(absoluteRoot, currentPath, source); err != nil {
			return err
		}
		query := btcrawler.Query{Title: *title, Episode: *episode, Page: *page}
		if *selftest {
			query, err = btcrawler.SelfTestQuery(source)
			if err != nil {
				return fmt.Errorf("%s: %w", source["id"], err)
			}
		}
		var fetcher btcrawler.Fetcher = btcrawler.HTTPFetcher{}
		if fixtureBody != nil {
			fetcher = btcrawler.StaticFetcher{Response: btcrawler.Response{URL: *responseURL, Body: fixtureBody}}
		}
		result, err := btcrawler.Crawl(context.Background(), source, query, fetcher)
		if err != nil {
			return fmt.Errorf("%s: %w", source["id"], err)
		}
		if *selftest {
			if err := btcrawler.CheckSelfTest(source, result); err != nil {
				return fmt.Errorf("%s: %w", source["id"], err)
			}
		}
		records = append(records, result.Records...)
		diagnostics = append(diagnostics, result.Diagnostics...)
	}
	encodedDiagnostics, err := btcrawler.EncodeDiagnostics(diagnostics)
	if err != nil {
		return err
	}
	if len(encodedDiagnostics) > 0 {
		if _, err := os.Stderr.Write(encodedDiagnostics); err != nil {
			return err
		}
	}
	absoluteOutput := *output
	if !filepath.IsAbs(absoluteOutput) {
		absoluteOutput = filepath.Join(absoluteRoot, absoluteOutput)
	}
	if err := btcrawler.WriteJSONL(absoluteOutput, records); err != nil {
		return err
	}
	fmt.Printf("crawled %d BT records from %d sources into %s\n", len(records), len(sourcePaths), absoluteOutput)
	return nil
}

func readBTSourcePaths(inputPath string) ([]string, error) {
	info, err := os.Stat(inputPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{inputPath}, nil
	}
	var paths []string
	err = filepath.WalkDir(inputPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
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
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s contains no YAML source files", inputPath)
	}
	return paths, nil
}

func importSources(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("import requires an ecosystem: animeko, kazumi, or nagare-v1")
	}
	ecosystem := arguments[0]
	flags := flag.NewFlagSet("import "+ecosystem, flag.ContinueOnError)
	root := flags.String("root", ".", "repository root containing schema/")
	input := flags.String("input", "", "upstream JSON or YAML file")
	upstream := flags.String("upstream", "", "canonical HTTP(S) URL for the upstream rule")
	license := flags.String("license", "", "SPDX license identifier for the upstream rule")
	output := flags.String("out", "imported", "output directory containing web/ and bt/")
	overlays := flags.String("overlays", "overlays", "overlay directory; empty disables overlays")
	diagnosticsPath := flags.String("diagnostics", "", "optional JSONL diagnostics output")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if *input == "" {
		return errors.New("--input is required")
	}
	if ecosystem != "animeko" && ecosystem != "kazumi" && ecosystem != "nagare-v1" {
		return fmt.Errorf("unsupported importer %q; expected animeko, kazumi, or nagare-v1", ecosystem)
	}
	inputs, err := readImportInputs(*input, ecosystem, *upstream)
	if err != nil {
		return err
	}
	var result importer.Result
	for _, inputDocument := range inputs {
		options := importer.Options{Upstream: inputDocument.Upstream, License: *license}
		var imported importer.Result
		switch ecosystem {
		case "animeko":
			imported = importer.ImportAnimeko(inputDocument.Data, options)
		case "kazumi":
			imported = importer.ImportKazumi(inputDocument.Data, options)
		case "nagare-v1":
			imported = importer.ImportNagareV1(inputDocument.Data, options)
		}
		for index := range imported.Diagnostics {
			if imported.Diagnostics[index].Path == "" {
				imported.Diagnostics[index].Path = inputDocument.Path
			} else {
				imported.Diagnostics[index].Path = inputDocument.Path + ":" + imported.Diagnostics[index].Path
			}
		}
		result.Sources = append(result.Sources, imported.Sources...)
		result.Diagnostics = append(result.Diagnostics, imported.Diagnostics...)
	}
	seenIDs := make(map[string]bool, len(result.Sources))
	for _, source := range result.Sources {
		id, _ := source["id"].(string)
		if seenIDs[id] {
			result.Diagnostics = append(result.Diagnostics, importer.Diagnostic{Severity: "error", Category: "duplicate_source", Source: id, Message: "multiple inputs generated the same source id"})
		}
		seenIDs[id] = true
	}

	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	overlayDir := *overlays
	if overlayDir != "" && !filepath.IsAbs(overlayDir) {
		overlayDir = filepath.Join(absoluteRoot, overlayDir)
	}
	var overlayDiagnostics []importer.Diagnostic
	result.Sources, overlayDiagnostics = importer.ApplyOverlays(result.Sources, overlayDir)
	result.Diagnostics = append(result.Diagnostics, overlayDiagnostics...)

	validator, err := repository.NewValidator(absoluteRoot)
	if err != nil {
		return err
	}
	absoluteOutput, err := filepath.Abs(*output)
	if err != nil {
		return err
	}
	for _, source := range result.Sources {
		id, _ := source["id"].(string)
		kind, _ := source["kind"].(string)
		virtualPath := filepath.Join(absoluteOutput, kind, id+".yaml")
		if err := validator.ValidateSource(absoluteOutput, virtualPath, source); err != nil {
			result.Diagnostics = append(result.Diagnostics, importer.Diagnostic{
				Severity: "error",
				Category: "invalid_generated_source",
				Source:   id,
				Path:     virtualPath,
				Message:  err.Error(),
			})
		}
	}
	encodedDiagnostics, err := importer.EncodeDiagnostics(result.Diagnostics)
	if err != nil {
		return err
	}
	if *diagnosticsPath != "" {
		if err := os.WriteFile(*diagnosticsPath, encodedDiagnostics, 0o644); err != nil {
			return err
		}
	} else if len(encodedDiagnostics) > 0 {
		if _, err := os.Stderr.Write(encodedDiagnostics); err != nil {
			return err
		}
	}
	if result.HasErrors() {
		return fmt.Errorf("import failed with diagnostics; no sources were written")
	}
	if err := importer.WriteSources(absoluteOutput, result.Sources); err != nil {
		return err
	}
	fmt.Printf("imported %d %s sources into %s\n", len(result.Sources), ecosystem, absoluteOutput)
	return nil
}

type importInput struct {
	Path     string
	Upstream string
	Data     []byte
}

func readImportInputs(inputPath, ecosystem, upstream string) ([]importInput, error) {
	info, err := os.Stat(inputPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		data, err := os.ReadFile(inputPath)
		if err != nil {
			return nil, err
		}
		return []importInput{{Path: inputPath, Upstream: upstream, Data: data}}, nil
	}
	var paths []string
	err = filepath.WalkDir(inputPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		extension := strings.ToLower(filepath.Ext(path))
		if ((ecosystem == "animeko" || ecosystem == "kazumi") && extension == ".json") || (ecosystem == "nagare-v1" && (extension == ".yaml" || extension == ".yml")) {
			if ecosystem == "kazumi" && strings.EqualFold(entry.Name(), "index.json") {
				return nil
			}
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s contains no supported %s input files", inputPath, ecosystem)
	}
	inputs := make([]importInput, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(inputPath, path)
		if err != nil {
			return nil, err
		}
		relative = escapeURLPath(filepath.ToSlash(relative))
		inputUpstream := upstream
		if strings.Contains(inputUpstream, "{path}") {
			inputUpstream = strings.ReplaceAll(inputUpstream, "{path}", relative)
		} else {
			inputUpstream = strings.TrimRight(inputUpstream, "/") + "/" + relative
		}
		inputs = append(inputs, importInput{Path: path, Upstream: inputUpstream, Data: data})
	}
	return inputs, nil
}

func escapeURLPath(value string) string {
	parts := strings.Split(value, "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func validate(arguments []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	absolute, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	summary, err := repository.ValidateRepository(absolute)
	if err != nil {
		return err
	}
	fmt.Printf("validated %d sources, %d resolve requests, %d candidates, and %d BT record entries\n", summary.Sources, summary.Requests, summary.Candidates, summary.BTRecords)
	return nil
}

func build(arguments []string) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root")
	output := flags.String("out", "dist", "generated output directory")
	version := flags.String("version", "dev", "repository release version")
	generatedAt := flags.String("generated-at", "", "RFC 3339 build time (defaults to SOURCE_DATE_EPOCH or the Git commit time)")
	btRecords := flags.String("bt-records", "", "optional normalized BT record JSONL file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	absoluteOutput := *output
	if !filepath.IsAbs(absoluteOutput) {
		absoluteOutput = filepath.Join(absoluteRoot, absoluteOutput)
	}
	if *generatedAt == "" {
		*generatedAt = repository.DefaultGeneratedAt(absoluteRoot)
	}
	var index repository.Index
	if *btRecords == "" {
		index, err = repository.Build(absoluteRoot, absoluteOutput, *version, *generatedAt)
	} else {
		recordsPath := *btRecords
		if !filepath.IsAbs(recordsPath) {
			recordsPath = filepath.Join(absoluteRoot, recordsPath)
		}
		index, err = repository.BuildWithBTRecords(absoluteRoot, absoluteOutput, *version, *generatedAt, recordsPath)
	}
	if err != nil {
		return err
	}
	fmt.Printf("built %d sources and %d BT record entries into %s (version %s)\n", len(index.Sources), index.Artifacts.BTIndex.Records, absoluteOutput, index.Version)
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `Nagare Source repository tool

Usage:
  nagare-source validate [--root PATH]
  nagare-source crawl-bt --source FILE --out FILE [--title TITLE] [--episode NUMBER] [--selftest]
  nagare-source serve [--root PATH] [--listen 127.0.0.1:7788] [--version VERSION] [--chrome PATH]
  nagare-source build [--root PATH] [--out PATH] [--version VERSION] [--generated-at RFC3339] [--bt-records FILE]
  nagare-source import animeko --input FILE --upstream URL --license SPDX [--out PATH]
  nagare-source import kazumi --input FILE --upstream URL --license SPDX [--out PATH]
  nagare-source import nagare-v1 --input FILE --upstream URL --license SPDX [--out PATH]`)
}
