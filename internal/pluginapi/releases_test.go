package pluginapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/btcrawler"
	"github.com/nagare-project/Nagare_Source/internal/repository"
	"github.com/nagare-project/Nagare_Source/internal/sourceruntime"
)

type fakeReleaseRunner struct {
	fakeRunner
	gate          chan struct{} // 非空时等测试放行，事件顺序不靠 sleep
	outcome       sourceruntime.ReleaseOutcome
	waitForCancel bool
	panics        bool
	mu            sync.Mutex
	titles        []string
}

func (runner *fakeReleaseRunner) Releases(ctx context.Context, titles []string) sourceruntime.ReleaseOutcome {
	runner.mu.Lock()
	runner.titles = append([]string(nil), titles...)
	runner.mu.Unlock()
	if runner.started != nil {
		close(runner.started)
	}
	if runner.waitForCancel {
		<-ctx.Done()
		if runner.cancelled != nil {
			close(runner.cancelled)
		}
		return sourceruntime.ReleaseOutcome{Err: ctx.Err()}
	}
	if runner.gate != nil {
		select {
		case <-runner.gate:
		case <-ctx.Done():
			return sourceruntime.ReleaseOutcome{Err: ctx.Err()}
		}
	}
	if runner.panics {
		panic("runtime bug")
	}
	return runner.outcome
}

func btReleaseRunner(id string, outcome sourceruntime.ReleaseOutcome) *fakeReleaseRunner {
	return &fakeReleaseRunner{fakeRunner: fakeRunner{source: source(id, "bt", true)}, outcome: outcome}
}

func gatedReleaseRunner(id string, outcome sourceruntime.ReleaseOutcome) *fakeReleaseRunner {
	runner := btReleaseRunner(id, outcome)
	runner.gate = make(chan struct{})
	return runner
}

// streamRecorder 在写出 source_result 时通知测试：测试据此放行下一个来源。
type streamRecorder struct {
	*httptest.ResponseRecorder
	results chan string
}

func (recorder *streamRecorder) Write(data []byte) (int, error) {
	written, err := recorder.ResponseRecorder.Write(data)
	if strings.Contains(string(data), `"event":"source_result"`) {
		recorder.results <- string(data)
	}
	return written, err
}

// logCollector 收集 Handler 的诊断日志；来源 goroutine 与写者可能同时写。
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (collector *logCollector) logf(format string, arguments ...any) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.lines = append(collector.lines, fmt.Sprintf(format, arguments...))
}

func (collector *logCollector) joined() string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return strings.Join(collector.lines, "\n")
}

func resultsBySource(events []map[string]any) map[string]map[string]any {
	results := map[string]map[string]any{}
	for _, event := range events {
		if event["event"] == "source_result" {
			results[event["sourceId"].(string)] = event
		}
	}
	return results
}

func testRelease(sourceID, digit, title string) sourceruntime.Release {
	hash := strings.Repeat(digit, 40)
	return sourceruntime.Release{
		ID: sourceID + ":" + hash, SourceID: sourceID, Title: title,
		Transport: sourceruntime.ReleaseTransport{Type: "torrent", Magnet: "magnet:?xt=urn:btih:" + hash, InfoHash: hash},
	}
}

const validReleaseRequest = `{"schema":"nagare-release-search/v1","subject":{"ids":{"anilist":"188525"},"titles":["描绘直至生命尽头","Kore Kaite Shine","これ描いて死ね","Draw This, Then Die!"]}}`

func postReleases(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/releases", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestManifestAdvertisesReleaseSearch(t *testing.T) {
	response := httptest.NewRecorder()
	testHandler(t, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/manifest", nil))
	var manifest Manifest
	decodeResponse(t, response, &manifest)
	if !strings.Contains(strings.Join(manifest.Capabilities, ","), "bt_releases") {
		t.Fatalf("manifest does not advertise bt_releases: %v", manifest.Capabilities)
	}
}

func TestReleasesRejectsInvalidRequestsBeforeStreaming(t *testing.T) {
	titles := func(values ...string) string {
		return `{"schema":"nagare-release-search/v1","subject":{"titles":["` + strings.Join(values, `","`) + `"]}}`
	}
	cases := map[string]string{
		"missing titles":       `{"schema":"nagare-release-search/v1","subject":{"ids":{"anilist":"1"}}}`,
		"missing subject":      `{"schema":"nagare-release-search/v1"}`,
		"empty titles":         `{"schema":"nagare-release-search/v1","subject":{"titles":[]}}`,
		"five titles":          titles("a", "b", "c", "d", "e"),
		"empty title":          titles(""),
		"blank titles":         titles("  ", `\t`),
		"unknown field":        `{"schema":"nagare-release-search/v1","subject":{"titles":["a"]},"episode":{"number":"1"}}`,
		"unknown subject":      `{"schema":"nagare-release-search/v1","subject":{"titles":["a"],"season":2}}`,
		"wrong schema":         `{"schema":"nagare-resolve-request/v1","subject":{"titles":["a"]}}`,
		"invalid id key":       `{"schema":"nagare-release-search/v1","subject":{"ids":{"AniList":"1"},"titles":["a"]}}`,
		"two JSON values":      titles("a") + `{}`,
		"not an object":        `["a"]`,
		"empty id value":       `{"schema":"nagare-release-search/v1","subject":{"ids":{"anilist":""},"titles":["a"]}}`,
		"title is not string":  `{"schema":"nagare-release-search/v1","subject":{"titles":[1]}}`,
		"subject is not array": `{"schema":"nagare-release-search/v1","subject":[]}`,
	}
	runner := btReleaseRunner("bt-source", sourceruntime.ReleaseOutcome{})
	handler := testHandler(t, []sourceruntime.Runner{runner})
	for name, body := range cases {
		response := postReleases(t, handler, body)
		if response.Code != http.StatusBadRequest || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") || !strings.Contains(response.Body.String(), "invalid_request") {
			t.Errorf("%s: want 400 invalid_request, got status=%d type=%s body=%s", name, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
	}
	if runner.titles != nil {
		t.Fatalf("an invalid request reached a source: %v", runner.titles)
	}

	wrongType := httptest.NewRequest(http.MethodPost, "/v1/releases", strings.NewReader(validReleaseRequest))
	wrongType.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, wrongType)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("non-JSON content type accepted: %d", response.Code)
	}
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, httptest.NewRequest(http.MethodGet, "/v1/releases", nil))
	if getResponse.Code != http.StatusMethodNotAllowed || getResponse.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET was not rejected: %d %v", getResponse.Code, getResponse.Header())
	}
	oversized := postReleases(t, handler, `{"padding":"`+strings.Repeat("x", maximumRequestBytes)+`"}`)
	if oversized.Code != http.StatusBadRequest || !strings.Contains(oversized.Body.String(), "request body is too large") {
		t.Fatalf("oversized body was not rejected: %d %s", oversized.Code, oversized.Body.String())
	}
}

func TestReleasesNormalizesTitlesBeforeSearching(t *testing.T) {
	runner := btReleaseRunner("bt-source", sourceruntime.ReleaseOutcome{})
	response := postReleases(t, testHandler(t, []sourceruntime.Runner{runner}),
		`{"schema":"nagare-release-search/v1","subject":{"titles":[" Frieren ","frieren","葬送的芙莉莲"]}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	if strings.Join(runner.titles, "|") != "Frieren|葬送的芙莉莲" {
		t.Fatalf("titles were not trimmed and case-insensitively deduplicated: %q", runner.titles)
	}
}

func TestReleasesStreamsEachSourceAsItFinishes(t *testing.T) {
	timeout := sourceruntime.NewError("search_timeout", true, "source exceeded its configured deadline", context.DeadlineExceeded)
	failed := sourceruntime.NewError("search_failed", true, "source request failed", errors.New("reset"))
	runners := []*fakeReleaseRunner{
		gatedReleaseRunner("bt-zero", sourceruntime.ReleaseOutcome{}),
		gatedReleaseRunner("bt-fast", sourceruntime.ReleaseOutcome{Releases: []sourceruntime.Release{
			testRelease("bt-fast", "1", "[G] Show - 01"), testRelease("bt-fast", "2", "[G] Show - 02"),
		}}),
		gatedReleaseRunner("bt-fail", sourceruntime.ReleaseOutcome{Err: failed}),
		gatedReleaseRunner("bt-partial", sourceruntime.ReleaseOutcome{Releases: []sourceruntime.Release{testRelease("bt-partial", "3", "[G] Show - 03")}, Err: timeout}),
		gatedReleaseRunner("bt-cached", sourceruntime.ReleaseOutcome{Releases: []sourceruntime.Release{testRelease("bt-cached", "4", "[G] Show - 04")}, Cached: true}),
	}
	all := []sourceruntime.Runner{
		&fakeReleaseRunner{fakeRunner: fakeRunner{source: source("web-source", "web", true)}},
		&fakeReleaseRunner{fakeRunner: fakeRunner{source: source("bt-disabled", "bt", false)}},
	}
	for _, runner := range runners {
		all = append(all, runner)
	}
	handler := testHandler(t, all)
	request := httptest.NewRequest(http.MethodPost, "/v1/releases", strings.NewReader(validReleaseRequest))
	request.Header.Set("Content-Type", "application/json")
	response := &streamRecorder{ResponseRecorder: httptest.NewRecorder(), results: make(chan string, len(runners))}
	finished := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(finished)
	}()
	// 按与来源 id 无关的顺序逐个放行：每个来源的结果必须在下一个来源结束之前就写出。
	for _, index := range []int{3, 0, 4, 1, 2} {
		close(runners[index].gate)
		select {
		case line := <-response.results:
			if !strings.Contains(line, `"sourceId":"`+runners[index].source.ID+`"`) {
				t.Fatalf("released %s but got %s", runners[index].source.ID, line)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s finished but its source_result was not streamed", runners[index].source.ID)
		}
	}
	<-finished
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/x-ndjson") || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected stream response: status=%d headers=%v", response.Code, response.Header())
	}
	events := decodeEvents(t, response.Body.Bytes())
	var sequence []string
	for _, event := range events {
		switch event["event"] {
		case "release":
			release, _ := event["release"].(map[string]any)
			sequence = append(sequence, "release:"+release["sourceId"].(string))
		case "source_result":
			sequence = append(sequence, "result:"+event["sourceId"].(string))
		default:
			sequence = append(sequence, fmt.Sprint(event["event"]))
		}
	}
	want := []string{
		"release:bt-partial", "result:bt-partial",
		"result:bt-zero",
		"release:bt-cached", "result:bt-cached",
		"release:bt-fast", "release:bt-fast", "result:bt-fast",
		"result:bt-fail",
		"done",
	}
	if strings.Join(sequence, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected event order:\n got %v\nwant %v", sequence, want)
	}
	results := resultsBySource(events)
	assertResult(t, results["bt-zero"], map[string]any{"state": "zero", "count": float64(0), "partial": false, "cached": false})
	assertResult(t, results["bt-fast"], map[string]any{"state": "ok", "count": float64(2), "partial": false, "cached": false})
	assertResult(t, results["bt-partial"], map[string]any{"state": "ok", "count": float64(1), "partial": true, "cached": false})
	assertResult(t, results["bt-cached"], map[string]any{"state": "ok", "count": float64(1), "partial": false, "cached": true})
	assertResult(t, results["bt-fail"], map[string]any{"state": "failed", "count": float64(0), "category": "search_failed", "message": "source request failed", "retryable": true})
	if _, ok := results["bt-fail"]["partial"]; ok {
		t.Fatalf("failed source_result must not carry partial: %v", results["bt-fail"])
	}
	for id, result := range results {
		if _, ok := result["durationMs"].(float64); !ok {
			t.Fatalf("%s source_result has no durationMs: %v", id, result)
		}
	}
	done := events[len(events)-1]
	if done["queried"] != float64(5) || done["succeeded"] != float64(4) || done["failed"] != float64(1) {
		t.Fatalf("invalid final counters: %v", done)
	}
}

func assertResult(t *testing.T, event map[string]any, want map[string]any) {
	t.Helper()
	for key, value := range want {
		if event[key] != value {
			t.Fatalf("source_result %v: %s = %v, want %v", event["sourceId"], key, event[key], value)
		}
	}
}

func TestReleasesClassifiesUntypedErrorsAsSearchFailures(t *testing.T) {
	logs := &logCollector{}
	panicking := btReleaseRunner("bt-panic", sourceruntime.ReleaseOutcome{})
	panicking.panics = true
	handler, err := New(Options{Root: "../..", Manifest: Manifest{Version: "test"}, Logf: logs.logf, Runners: []sourceruntime.Runner{
		btReleaseRunner("bt-deadline", sourceruntime.ReleaseOutcome{Err: context.DeadlineExceeded}),
		btReleaseRunner("bt-other", sourceruntime.ReleaseOutcome{Err: errors.New("boom")}),
		panicking,
	}})
	if err != nil {
		t.Fatal(err)
	}
	events := decodeEvents(t, postReleases(t, handler, validReleaseRequest).Body.Bytes())
	results := resultsBySource(events)
	if len(events) != 4 || len(results) != 3 || events[3]["event"] != "done" || events[3]["failed"] != float64(3) {
		t.Fatalf("unexpected events: %v", events)
	}
	assertResult(t, results["bt-deadline"], map[string]any{"state": "failed", "category": "search_timeout", "retryable": true})
	assertResult(t, results["bt-other"], map[string]any{"state": "failed", "category": "search_failed", "retryable": true})
	assertResult(t, results["bt-panic"], map[string]any{"state": "failed", "category": "search_failed", "message": "source runtime panicked"})
	if !strings.Contains(logs.joined(), "source bt-panic panicked") {
		t.Fatalf("recovered panic was not logged: %q", logs.joined())
	}
}

func TestReleasesSkipsInvalidReleasesWithoutFailingTheSource(t *testing.T) {
	invalid := testRelease("bt-mixed", "5", "[G] Show - 05")
	invalid.Transport.InfoHash = "not-a-hash"
	foreign := testRelease("other-source", "6", "[G] Show - 06")
	logs := &logCollector{}
	handler, err := New(Options{Root: "../..", Manifest: Manifest{Version: "test"}, Logf: logs.logf, Runners: []sourceruntime.Runner{
		btReleaseRunner("bt-mixed", sourceruntime.ReleaseOutcome{Releases: []sourceruntime.Release{
			invalid, testRelease("bt-mixed", "1", "[G] Show - 01"), foreign, testRelease("bt-mixed", "1", "[G] duplicate id"),
		}}),
		btReleaseRunner("bt-broken", sourceruntime.ReleaseOutcome{Releases: []sourceruntime.Release{invalid}}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	events := decodeEvents(t, postReleases(t, handler, validReleaseRequest).Body.Bytes())
	results := resultsBySource(events)
	releases := 0
	for _, event := range events {
		if event["event"] == "release" {
			releases++
		}
	}
	if len(events) != 4 || releases != 1 || events[3]["succeeded"] != float64(1) || events[3]["failed"] != float64(1) {
		t.Fatalf("unexpected events: %v", events)
	}
	assertResult(t, results["bt-mixed"], map[string]any{"state": "ok", "count": float64(1)})
	assertResult(t, results["bt-broken"], map[string]any{"state": "failed", "category": "invalid_candidate", "retryable": false})
	if text := logs.joined(); !strings.Contains(text, "bt-mixed skipped 3") || !strings.Contains(text, "bt-broken skipped 1") {
		t.Fatalf("skipped releases were not logged: %q", text)
	}
}

func TestReleasesClientCancellationStopsSourceWork(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	runner := &fakeReleaseRunner{fakeRunner: fakeRunner{source: source("bt-wait", "bt", true), started: started, cancelled: cancelled}, waitForCancel: true}
	handler := testHandler(t, []sourceruntime.Runner{runner})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/releases", strings.NewReader(validReleaseRequest)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	finished := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(finished)
	}()
	for _, step := range []struct {
		name    string
		channel chan struct{}
		action  func()
	}{{"start", started, cancel}, {"cancel", cancelled, nil}, {"finish", finished, nil}} {
		select {
		case <-step.channel:
		case <-time.After(time.Second):
			t.Fatalf("release search did not %s", step.name)
		}
		if step.action != nil {
			step.action()
		}
	}
}

// 用仓库里真实的 BT 规则与离线 fixture 跑一遍：规则产出的每条发布都必须通过 release-v1。
func TestReleasesFromBundledBTRulesSatisfySchema(t *testing.T) {
	root := filepath.Join("..", "..")
	validator, err := repository.NewValidator(root)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := repository.LoadSources(root, validator)
	if err != nil {
		t.Fatal(err)
	}
	var runners []sourceruntime.Runner
	for _, document := range sources {
		id, _ := document.Document["id"].(string)
		if document.Document["kind"] != "bt" || document.Document["enabled"] == false {
			continue
		}
		fixture, err := os.ReadFile(fixturePath(t, root, id))
		if err != nil {
			t.Fatal(err)
		}
		runners = append(runners, sourceruntime.NewSpecRunner(document.Document, sourceruntime.RunnerOptions{
			Fetcher: btcrawler.StaticFetcher{Response: btcrawler.Response{Body: fixture}},
		}))
	}
	logs := &logCollector{}
	handler, err := New(Options{Root: root, Manifest: Manifest{Version: "test"}, Runners: runners, Logf: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	events := decodeEvents(t, postReleases(t, handler, validReleaseRequest).Body.Bytes())
	done := events[len(events)-1]
	if logs.joined() != "" || done["event"] != "done" || done["queried"] != float64(len(runners)) || done["failed"] != float64(0) {
		t.Fatalf("bundled rules produced invalid releases: logs=%q done=%v", logs.joined(), done)
	}
	for _, event := range events {
		if event["event"] == "source_result" && (event["state"] != "ok" || event["count"].(float64) < 1) {
			t.Fatalf("bundled rule returned no releases from its fixture: %v", event)
		}
	}
}

func fixturePath(t *testing.T, root, sourceID string) string {
	t.Helper()
	for _, extension := range []string{".xml", ".json"} {
		path := filepath.Join(root, "fixtures", "responses", sourceID+extension)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	t.Fatalf("no fixture for %s", sourceID)
	return ""
}
