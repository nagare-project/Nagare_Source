// Package healthreport runs Source Spec self-checks and produces stable JSON
// and static HTML artifacts for scheduled community health publishing.
package healthreport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/sourceruntime"
)

type Report struct {
	Schema      string         `json:"schema"`
	GeneratedAt string         `json:"generatedAt"`
	Mode        string         `json:"mode"`
	Summary     Summary        `json:"summary"`
	Sources     []SourceHealth `json:"sources"`
}

type Summary struct {
	Total               int `json:"total"`
	Healthy             int `json:"healthy"`
	Degraded            int `json:"degraded"`
	Unavailable         int `json:"unavailable"`
	InteractiveRequired int `json:"interactiveRequired"`
	Disabled            int `json:"disabled"`
}

type SourceHealth struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Tier       int    `json:"tier"`
	Version    string `json:"version"`
	Status     string `json:"status"`
	DurationMS int64  `json:"durationMs"`
	Category   string `json:"category,omitempty"`
	Message    string `json:"message,omitempty"`
}

type Options struct {
	Mode        string
	GeneratedAt string
	Concurrency int
}

func Generate(ctx context.Context, runners []sourceruntime.Runner, options Options) (Report, error) {
	if options.Mode != "fixture" && options.Mode != "network" {
		return Report{}, errors.New("health mode must be fixture or network")
	}
	generatedAt, err := time.Parse(time.RFC3339, options.GeneratedAt)
	if err != nil {
		return Report{}, fmt.Errorf("generated-at must be RFC 3339: %w", err)
	}
	ordered := append([]sourceruntime.Runner(nil), runners...)
	for _, runner := range ordered {
		if runner == nil || runner.Source().ID == "" {
			return Report{}, errors.New("health runner has no source id")
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Source().ID < ordered[j].Source().ID })
	for index := 1; index < len(ordered); index++ {
		if ordered[index].Source().ID == ordered[index-1].Source().ID {
			return Report{}, fmt.Errorf("duplicate health source id %q", ordered[index].Source().ID)
		}
	}

	report := Report{
		Schema: "nagare-source-health/v1", GeneratedAt: generatedAt.UTC().Format(time.RFC3339),
		Mode: options.Mode, Sources: make([]SourceHealth, len(ordered)),
	}
	if len(ordered) == 0 {
		return report, nil
	}
	concurrency := options.Concurrency
	if concurrency < 1 {
		concurrency = 4
	}
	if concurrency > len(ordered) {
		concurrency = len(ordered)
	}
	if concurrency > 32 {
		concurrency = 32
	}

	jobs := make(chan int, len(ordered))
	for index := range ordered {
		jobs <- index
	}
	close(jobs)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for range concurrency {
		go func() {
			defer workers.Done()
			for index := range jobs {
				report.Sources[index] = checkSource(ctx, ordered[index], options.Mode)
			}
		}()
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	for _, source := range report.Sources {
		report.Summary.Total++
		switch source.Status {
		case "healthy":
			report.Summary.Healthy++
		case "degraded":
			report.Summary.Degraded++
		case "interactive_required":
			report.Summary.InteractiveRequired++
		case "disabled":
			report.Summary.Disabled++
		default:
			report.Summary.Unavailable++
		}
	}
	return report, nil
}

func checkSource(ctx context.Context, runner sourceruntime.Runner, mode string) SourceHealth {
	source := runner.Source()
	result := SourceHealth{ID: source.ID, Name: source.Name, Kind: source.Kind, Tier: source.Tier, Version: source.Version}
	if !source.Enabled {
		result.Status = "disabled"
		result.Category = "source_disabled"
		result.Message = "source is disabled"
		return result
	}
	checked := safeSelfCheck(ctx, runner, mode)
	result.Status = checked.Status
	result.DurationMS = checked.DurationMS
	result.Category = checked.Category
	result.Message = checked.Message
	if result.DurationMS < 0 {
		result.DurationMS = 0
	}
	switch result.Status {
	case "healthy", "degraded", "unavailable", "interactive_required":
	default:
		result.Status = "unavailable"
		result.Category = "invalid_candidate"
		result.Message = "source self-check returned an invalid status"
	}
	if result.Status != "healthy" && strings.TrimSpace(result.Category) == "" {
		result.Category = "resolve_failed"
	}
	if result.Status != "healthy" && strings.TrimSpace(result.Message) == "" {
		result.Message = "source self-check failed"
	}
	return result
}

func safeSelfCheck(ctx context.Context, runner sourceruntime.Runner, mode string) (result sourceruntime.SelfCheckResult) {
	defer func() {
		if recover() != nil {
			result = sourceruntime.SelfCheckResult{
				SourceID: runner.Source().ID, Status: "unavailable", Category: "resolve_failed",
				Message: "source self-check panicked",
			}
		}
	}()
	return runner.SelfCheck(ctx, mode)
}

func EncodeJSON(report Report) ([]byte, error) {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func RenderHTML(report Report) ([]byte, error) {
	var output strings.Builder
	if err := healthPage.Execute(&output, report); err != nil {
		return nil, err
	}
	return []byte(output.String()), nil
}

func WriteAtomic(path string, data []byte) error {
	path = filepath.Clean(path)
	if path == "." || filepath.Base(path) == "." {
		return errors.New("health output path must name a file")
	}
	directory := filepath.Dir(path)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return fmt.Errorf("health output %q is a directory", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		backup, createErr := os.CreateTemp(directory, "."+filepath.Base(path)+"-backup-*")
		if createErr != nil {
			return err
		}
		backupPath := backup.Name()
		if closeErr := backup.Close(); closeErr != nil {
			return closeErr
		}
		if removeErr := os.Remove(backupPath); removeErr != nil {
			return removeErr
		}
		if backupErr := os.Rename(path, backupPath); backupErr != nil {
			return err
		}
		defer os.Remove(backupPath)
		if retryErr := os.Rename(temporaryPath, path); retryErr != nil {
			_ = os.Rename(backupPath, path)
			return retryErr
		}
	}
	return nil
}

var healthPage = template.Must(template.New("health").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <title>Nagare Source Health</title>
  <style>
    :root { font-family: Inter, ui-sans-serif, system-ui, sans-serif; color: #172033; background: #f5f7fb; }
    body { margin: 0; }
    main { width: min(1120px, calc(100% - 32px)); margin: 48px auto; }
    header { display: flex; align-items: end; justify-content: space-between; gap: 24px; margin-bottom: 24px; }
    h1 { margin: 0 0 8px; font-size: clamp(28px, 5vw, 44px); letter-spacing: -0.04em; }
    p { margin: 0; color: #61708a; }
    a { color: #315ee7; }
    .summary { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: 12px; margin-bottom: 24px; }
    .card, .table-wrap { border: 1px solid #dfe5ef; border-radius: 16px; background: #fff; box-shadow: 0 8px 30px rgba(38, 55, 86, .06); }
    .card { padding: 18px; }
    .card strong { display: block; margin-top: 6px; font-size: 28px; }
    .table-wrap { overflow-x: auto; }
    table { width: 100%; border-collapse: collapse; }
    th, td { padding: 14px 16px; border-bottom: 1px solid #edf0f5; text-align: left; vertical-align: top; }
    th { color: #61708a; font-size: 12px; letter-spacing: .05em; text-transform: uppercase; }
    tr:last-child td { border-bottom: 0; }
    .status { display: inline-block; padding: 4px 9px; border-radius: 999px; font-size: 12px; font-weight: 700; }
    .healthy { color: #096b45; background: #dff7ec; }
    .degraded { color: #855500; background: #fff0c2; }
    .unavailable, .interactive_required { color: #a12828; background: #ffe1e1; }
    .disabled { color: #596579; background: #e9edf3; }
    .message { max-width: 420px; color: #61708a; }
    footer { margin-top: 18px; font-size: 13px; }
    @media (prefers-color-scheme: dark) {
      :root { color: #edf3ff; background: #101521; }
      p, th, .message { color: #9eacc2; }
      .card, .table-wrap { border-color: #2c3547; background: #171e2c; box-shadow: none; }
      th, td { border-color: #283144; }
      a { color: #91adff; }
    }
  </style>
</head>
<body>
  <main>
    <header>
      <div><h1>Nagare Source Health</h1><p>{{.GeneratedAt}} · {{.Mode}} self-check</p></div>
      <a href="health.json">JSON report</a>
    </header>
    <section class="summary" aria-label="Health summary">
      <div class="card">Total<strong>{{.Summary.Total}}</strong></div>
      <div class="card">Healthy<strong>{{.Summary.Healthy}}</strong></div>
      <div class="card">Degraded<strong>{{.Summary.Degraded}}</strong></div>
      <div class="card">Unavailable<strong>{{.Summary.Unavailable}}</strong></div>
      <div class="card">Interactive<strong>{{.Summary.InteractiveRequired}}</strong></div>
      <div class="card">Disabled<strong>{{.Summary.Disabled}}</strong></div>
    </section>
    <div class="table-wrap">
      <table>
        <thead><tr><th>Source</th><th>Kind / tier</th><th>Status</th><th>Latency</th><th>Detail</th></tr></thead>
        <tbody>
          {{range .Sources}}<tr>
            <td><strong>{{.Name}}</strong><br><small>{{.ID}} · v{{.Version}}</small></td>
            <td>{{.Kind}} / T{{.Tier}}</td>
            <td><span class="status {{.Status}}">{{.Status}}</span></td>
            <td>{{.DurationMS}} ms</td>
            <td class="message">{{if .Category}}<strong>{{.Category}}</strong>{{end}}{{if .Message}}<br>{{.Message}}{{end}}</td>
          </tr>{{else}}<tr><td colspan="5">No sources were checked.</td></tr>{{end}}
        </tbody>
      </table>
    </div>
    <footer><p>Health affects ranking and maintenance decisions; it never hides an explicit per-request error.</p></footer>
  </main>
</body>
</html>
`))
