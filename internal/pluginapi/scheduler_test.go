package pluginapi

import (
	"context"
	"testing"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/sourceruntime"
)

type scheduledRunner struct {
	fakeRunner
	entered chan string
	release <-chan struct{}
}

func (r *scheduledRunner) Candidates(ctx context.Context, _ sourceruntime.ResolveRequest, _ func(sourceruntime.Candidate) error) error {
	r.entered <- r.source.ID
	if r.release == nil {
		return nil
	}
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestBrowserQueuePreservesBudgetAndDoesNotBlockBT(t *testing.T) {
	entered := make(chan string, 8)
	release := make(chan struct{})
	makeRunner := func(id string, browser bool, tier int, hold bool) sourceruntime.Runner {
		s := source(id, "web", true)
		s.Tier = tier
		if browser {
			s.Capabilities = append(s.Capabilities, "browser_sniff")
		} else {
			s.Kind = "bt"
		}
		r := &scheduledRunner{fakeRunner: fakeRunner{source: s}, entered: entered}
		if hold {
			r.release = release
		}
		return r
	}
	runners := []sourceruntime.Runner{makeRunner("last", true, 3, false), makeRunner("first", true, 0, true), makeRunner("second", true, 1, true), makeRunner("bt", false, 3, false)}
	h := testHandler(t, runners)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	messages := make(chan candidateMessage, 8)
	h.startSources(ctx, runners, sourceruntime.ResolveRequest{}, messages)
	seen := map[string]bool{}
	for range 3 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatal("sources did not start")
		}
	}
	if !seen["first"] || !seen["second"] || !seen["bt"] || seen["last"] {
		t.Fatalf("wrong initial scheduling: %v", seen)
	}
	select {
	case id := <-entered:
		t.Fatalf("queued source started early: %s", id)
	default:
	}
	close(release)
	select {
	case id := <-entered:
		if id != "last" {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("queued source did not start")
	}
	for range 4 {
		select {
		case <-messages:
		case <-time.After(time.Second):
			t.Fatal("missing completion")
		}
	}
}

func TestCancelDiscardsQueuedBrowserSources(t *testing.T) {
	h := testHandler(t, nil)
	h.browserSlots <- struct{}{}
	h.browserSlots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := make(chan struct{})
	s := source("queued", "web", true)
	s.Capabilities = append(s.Capabilities, "browser_sniff")
	h.startSources(ctx, []sourceruntime.Runner{&fakeRunner{source: s, started: started}}, sourceruntime.ResolveRequest{}, make(chan candidateMessage, 1))
	<-h.browserSlots
	select {
	case <-started:
		t.Fatal("cancelled request started queued work")
	case <-time.After(30 * time.Millisecond):
	}
}
