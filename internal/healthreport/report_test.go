package healthreport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/sourceruntime"
)

type fakeRunner struct {
	source    sourceruntime.Source
	result    sourceruntime.SelfCheckResult
	panic     bool
	called    bool
	callMutex sync.Mutex
}

func (runner *fakeRunner) Source() sourceruntime.Source { return runner.source }

func (runner *fakeRunner) Candidates(context.Context, sourceruntime.ResolveRequest, func(sourceruntime.Candidate) error) error {
	return errors.New("not used")
}

func (runner *fakeRunner) SelfCheck(context.Context, string) sourceruntime.SelfCheckResult {
	runner.callMutex.Lock()
	runner.called = true
	runner.callMutex.Unlock()
	if runner.panic {
		panic("broken source")
	}
	return runner.result
}

func TestGenerateSortsSourcesAndSummarizesFailures(t *testing.T) {
	runners := []sourceruntime.Runner{
		&fakeRunner{source: source("web-z", true), result: sourceruntime.SelfCheckResult{Status: "degraded", Category: "search_failed", Message: "offline", DurationMS: 12}},
		&fakeRunner{source: source("bt-a", true), result: sourceruntime.SelfCheckResult{Status: "healthy", DurationMS: 3}},
		&fakeRunner{source: source("web-panic", true), panic: true},
		&fakeRunner{source: source("web-disabled", false)},
	}
	report, err := Generate(context.Background(), runners, Options{Mode: "fixture", GeneratedAt: "2026-09-07T00:00:00+10:00", Concurrency: 3})
	if err != nil {
		t.Fatal(err)
	}
	if report.GeneratedAt != "2026-09-06T14:00:00Z" || len(report.Sources) != 4 || report.Sources[0].ID != "bt-a" || report.Sources[3].ID != "web-z" {
		t.Fatalf("report is not normalized and sorted: %+v", report)
	}
	if report.Summary.Total != 4 || report.Summary.Healthy != 1 || report.Summary.Degraded != 1 || report.Summary.Unavailable != 1 || report.Summary.Disabled != 1 {
		t.Fatalf("summary is wrong: %+v", report.Summary)
	}
	if report.Sources[2].Category != "resolve_failed" || report.Sources[1].Status != "disabled" {
		t.Fatalf("panic or disabled source was not isolated: %+v", report.Sources)
	}
	disabled := runners[3].(*fakeRunner)
	disabled.callMutex.Lock()
	defer disabled.callMutex.Unlock()
	if disabled.called {
		t.Fatal("disabled source self-check was executed")
	}
}

func TestGenerateRejectsInvalidInputs(t *testing.T) {
	runner := &fakeRunner{source: source("duplicate", true)}
	if _, err := Generate(context.Background(), []sourceruntime.Runner{runner, runner}, Options{Mode: "fixture", GeneratedAt: time.Now().Format(time.RFC3339)}); err == nil {
		t.Fatal("duplicate source IDs were accepted")
	}
	if _, err := Generate(context.Background(), nil, Options{Mode: "invalid", GeneratedAt: time.Now().Format(time.RFC3339)}); err == nil {
		t.Fatal("invalid mode was accepted")
	}
}

func TestRenderHTMLEscapesSourceDataAndWriteAtomicReplacesOutput(t *testing.T) {
	report := Report{Schema: "nagare-source-health/v1", GeneratedAt: "2026-09-07T00:00:00Z", Mode: "fixture", Summary: Summary{Total: 1}, Sources: []SourceHealth{{
		ID: "safe-id", Name: `<script>alert(1)</script>`, Kind: "web", Version: "1", Status: "degraded", Category: "search_failed", Message: `<b>failed</b>`,
	}}}
	page, err := RenderHTML(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "<script>alert") || !strings.Contains(string(page), "&lt;script&gt;") || !strings.Contains(string(page), "health.json") {
		t.Fatalf("health page did not escape source data: %s", page)
	}
	path := filepath.Join(t.TempDir(), "nested", "health.json")
	if err := WriteAtomic(path, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new\n" {
		t.Fatalf("atomic output was not replaced: %q %v", data, err)
	}
}

func source(id string, enabled bool) sourceruntime.Source {
	return sourceruntime.Source{ID: id, Name: id, Kind: "web", Tier: 1, Version: "1", Enabled: enabled}
}
