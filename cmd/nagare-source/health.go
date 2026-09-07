package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/healthreport"
	"github.com/nagare-project/Nagare_Source/internal/repository"
)

func health(arguments []string) error {
	flags := flag.NewFlagSet("health", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root containing schema/ and sources/")
	mode := flags.String("mode", "fixture", "self-check mode: fixture or network")
	output := flags.String("out", "reports/health.json", "health report JSON output")
	htmlOutput := flags.String("html", "reports/health.html", "static health page output; empty disables HTML")
	generatedAt := flags.String("generated-at", "", "RFC 3339 report time; defaults to current UTC time")
	concurrency := flags.Int("concurrency", 4, "maximum concurrent source self-checks")
	chrome := flags.String("chrome", "", "optional Chrome/Chromium executable path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *generatedAt == "" {
		*generatedAt = time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	}
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	runners, err := loadSourceRunners(absoluteRoot, *chrome)
	if err != nil {
		return err
	}
	report, err := healthreport.Generate(context.Background(), runners, healthreport.Options{
		Mode: *mode, GeneratedAt: *generatedAt, Concurrency: *concurrency,
	})
	if err != nil {
		return err
	}
	jsonData, err := healthreport.EncodeJSON(report)
	if err != nil {
		return err
	}
	validator, err := repository.NewValidator(absoluteRoot)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	if err := validator.Validate(repository.HealthSchemaName, document); err != nil {
		return fmt.Errorf("generated health report is invalid: %w", err)
	}
	page, err := healthreport.RenderHTML(report)
	if err != nil {
		return err
	}
	jsonPath := rootedPath(absoluteRoot, *output)
	htmlPath := ""
	if *htmlOutput != "" {
		htmlPath = rootedPath(absoluteRoot, *htmlOutput)
		if filepath.Clean(jsonPath) == filepath.Clean(htmlPath) {
			return errors.New("--out and --html must use different files")
		}
	}
	if err := healthreport.WriteAtomic(jsonPath, jsonData); err != nil {
		return err
	}
	if htmlPath != "" {
		if err := healthreport.WriteAtomic(htmlPath, page); err != nil {
			return err
		}
	}
	fmt.Printf("checked %d sources: %d healthy, %d degraded, %d unavailable, %d interactive, %d disabled\n",
		report.Summary.Total, report.Summary.Healthy, report.Summary.Degraded, report.Summary.Unavailable,
		report.Summary.InteractiveRequired, report.Summary.Disabled)
	return nil
}

func rootedPath(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(root, path)
}
