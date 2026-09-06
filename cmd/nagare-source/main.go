package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

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

func importSources(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("import requires an ecosystem: animeko or nagare-v1")
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
	data, err := os.ReadFile(*input)
	if err != nil {
		return err
	}
	options := importer.Options{Upstream: *upstream, License: *license}
	var result importer.Result
	switch ecosystem {
	case "animeko":
		result = importer.ImportAnimeko(data, options)
	case "nagare-v1":
		result = importer.ImportNagareV1(data, options)
	default:
		return fmt.Errorf("unsupported importer %q; expected animeko or nagare-v1", ecosystem)
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
	fmt.Printf("validated %d sources, %d resolve requests, and %d candidates\n", summary.Sources, summary.Requests, summary.Candidates)
	return nil
}

func build(arguments []string) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root")
	output := flags.String("out", "dist", "generated output directory")
	version := flags.String("version", "dev", "repository release version")
	generatedAt := flags.String("generated-at", "", "RFC 3339 build time (defaults to SOURCE_DATE_EPOCH or the Git commit time)")
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
	index, err := repository.Build(absoluteRoot, absoluteOutput, *version, *generatedAt)
	if err != nil {
		return err
	}
	fmt.Printf("built %d sources into %s (version %s)\n", len(index.Sources), absoluteOutput, index.Version)
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `Nagare Source repository tool

Usage:
  nagare-source validate [--root PATH]
  nagare-source build [--root PATH] [--out PATH] [--version VERSION] [--generated-at RFC3339]
  nagare-source import animeko --input FILE --upstream URL --license SPDX [--out PATH]
  nagare-source import nagare-v1 --input FILE --upstream URL --license SPDX [--out PATH]`)
}
