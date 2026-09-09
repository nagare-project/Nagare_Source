package sourceruntime

import (
	"context"
	"errors"
	"github.com/nagare-project/Nagare_Source/internal/repository"
	"github.com/nagare-project/Nagare_Source/internal/webresolver"
	"net/url"
	"os"
	"testing"
	"time"
)

// This opt-in check contacts the published source; ordinary tests stay offline.
func TestNativeLiveSeasonFour(t *testing.T) {
	if os.Getenv("NAGARE_SOURCE_LIVE_TEST") != "1" {
		t.Skip("explicit live source test only")
	}
	helper := webresolver.FindNativeWebViewHelper("../..")
	if helper == "" {
		t.Fatal("build native helper first")
	}
	value, err := repository.LoadDocument("../../sources/upstreams/kazumi-rules/web/7sefun.yaml")
	if err != nil {
		t.Fatal(err)
	}
	source := value.(map[string]any)
	browser := webresolver.New(webresolver.NewNativeWebView(webresolver.NativeWebViewOptions{ExecutablePath: helper, Logger: webresolver.LoggerFunc(t.Logf)}))
	browser.Logger = webresolver.LoggerFunc(t.Logf)
	runner := NewSpecRunner(source, RunnerOptions{Browser: browser})
	started := time.Now()
	count := 0
	stop := errors.New("first candidate verified")
	err = runner.Candidates(context.Background(), ResolveRequest{
		Schema: "nagare-resolve-request/v1", Subject: Subject{Titles: []string{"关于我转生变成史莱姆这档事 第四季"}, Season: pointer(4), Year: pointer(2026)}, Episode: Episode{Number: "1"},
	}, func(candidate Candidate) error {
		if compactTitle(candidate.Match.SubjectTitle) != compactTitle("关于我转生变成史莱姆这档事第四季") {
			t.Fatalf("wrong installment: %s", candidate.Match.SubjectTitle)
		}
		if candidate.Match.EpisodeNumber == nil || *candidate.Match.EpisodeNumber != 1 {
			t.Fatal("wrong episode")
		}
		parsed, _ := url.Parse(candidate.Transport.URL)
		t.Logf("title=%s episode=1 transport=%s host=%s elapsed=%s", candidate.Match.SubjectTitle, candidate.Transport.Type, parsed.Hostname(), time.Since(started))
		count++
		return stop
	})
	if err != nil && !errors.Is(err, stop) {
		t.Fatalf("pipeline: %v (cause: %v)", err, errors.Unwrap(err))
	}
	if count == 0 {
		t.Fatal("no candidate")
	}
}
