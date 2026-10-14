package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/marketplace"
)

// marketplaceCommand 生成来源市场静态站点（index.html、index.json、packs/bt.json、packs/web.json），
// 由 Pages 工作流发布。用户在页面上点“安装”，由本机 Nagare 下载、校验并确认安装。
func marketplaceCommand(arguments []string) error {
	flags := flag.NewFlagSet("marketplace", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root containing schema/ and sources/")
	output := flags.String("out", "", "output directory (must not exist or be empty)")
	healthPath := flags.String("health", "", "optional health report JSON whose statuses are shown on the page")
	generatedAt := flags.String("generated-at", "", "RFC 3339 generation time; defaults to current UTC time")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("--out is required")
	}
	if *generatedAt == "" {
		*generatedAt = time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	} else if _, err := time.Parse(time.RFC3339, *generatedAt); err != nil {
		return fmt.Errorf("--generated-at: %w", err)
	}
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	options := marketplace.Options{GeneratedAt: *generatedAt}
	if *healthPath != "" {
		statuses, err := marketplace.LoadHealth(rootedPath(absoluteRoot, *healthPath))
		if err != nil {
			return fmt.Errorf("load health report: %w", err)
		}
		options.Health = statuses
	}
	site, err := marketplace.Build(absoluteRoot, options)
	if err != nil {
		return err
	}
	outputDir := rootedPath(absoluteRoot, *output)
	if err := site.Write(outputDir); err != nil {
		return err
	}
	fmt.Printf("wrote marketplace with %d packs (%d sources) to %s\n", len(site.Index.Packs), site.Index.SourceCount(), outputDir)
	return nil
}
