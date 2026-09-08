package pluginapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/sourceruntime"
)

type fakeRunner struct {
	source        sourceruntime.Source
	delay         time.Duration
	candidates    []sourceruntime.Candidate
	err           error
	waitForCancel bool
	started       chan struct{}
	cancelled     chan struct{}
	selfcheck     sourceruntime.SelfCheckResult
}

func (runner *fakeRunner) Source() sourceruntime.Source { return runner.source }

func (runner *fakeRunner) Candidates(ctx context.Context, _ sourceruntime.ResolveRequest, emit func(sourceruntime.Candidate) error) error {
	if runner.started != nil {
		close(runner.started)
	}
	if runner.waitForCancel {
		<-ctx.Done()
		if runner.cancelled != nil {
			close(runner.cancelled)
		}
		return ctx.Err()
	}
	if runner.delay > 0 {
		timer := time.NewTimer(runner.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, candidate := range runner.candidates {
		if err := emit(candidate); err != nil {
			return err
		}
	}
	return runner.err
}

func (runner *fakeRunner) SelfCheck(_ context.Context, _ string) sourceruntime.SelfCheckResult {
	return runner.selfcheck
}

func TestManifestSourcesHealthAndProtocolNegotiation(t *testing.T) {
	handler := testHandler(t, []sourceruntime.Runner{
		&fakeRunner{source: source("web-source", "web", true)},
		&fakeRunner{source: source("bt-source", "bt", false)},
	})

	manifestRequest := httptest.NewRequest(http.MethodGet, "/v1/manifest", nil)
	manifestRequest.Header.Set("X-Request-ID", "request-123")
	manifestResponse := httptest.NewRecorder()
	handler.ServeHTTP(manifestResponse, manifestRequest)
	if manifestResponse.Code != http.StatusOK || manifestResponse.Header().Get("X-Request-ID") != "request-123" {
		t.Fatalf("manifest response headers/status are wrong: status=%d headers=%v", manifestResponse.Code, manifestResponse.Header())
	}
	var manifest Manifest
	decodeResponse(t, manifestResponse, &manifest)
	if manifest.Version != "test" || len(manifest.ProtocolVersions) != 1 || manifest.ProtocolVersions[0] != 1 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}

	sourcesResponse := httptest.NewRecorder()
	handler.ServeHTTP(sourcesResponse, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))
	var sources struct {
		Sources []sourceruntime.Source `json:"sources"`
	}
	decodeResponse(t, sourcesResponse, &sources)
	if len(sources.Sources) != 2 || sources.Sources[0].ID != "bt-source" || sources.Sources[0].Status != "disabled" {
		t.Fatalf("sources are not stable or complete: %+v", sources.Sources)
	}

	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if healthResponse.Code != http.StatusOK || !strings.Contains(healthResponse.Body.String(), `"status":"ok"`) {
		t.Fatalf("unexpected health response: %d %s", healthResponse.Code, healthResponse.Body.String())
	}

	protocolRequest := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	protocolRequest.Header.Set("X-Nagare-Protocol-Version", "2")
	protocolResponse := httptest.NewRecorder()
	handler.ServeHTTP(protocolResponse, protocolRequest)
	if protocolResponse.Code != http.StatusUpgradeRequired || !strings.Contains(protocolResponse.Body.String(), "unsupported_protocol") {
		t.Fatalf("unsupported protocol was not rejected: %d %s", protocolResponse.Code, protocolResponse.Body.String())
	}
}

func TestSourcesApplyPersistedHealthStatus(t *testing.T) {
	handler := testHandler(t, []sourceruntime.Runner{
		&fakeRunner{source: source("example-http", "web", true)},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))
	var body struct {
		Sources []sourceruntime.Source `json:"sources"`
	}
	decodeResponse(t, response, &body)
	if len(body.Sources) != 1 || body.Sources[0].Status != "degraded" {
		t.Fatalf("persisted health status was not exposed: %+v", body.Sources)
	}
}

func TestCandidatesStreamsFastResultsErrorsAndDone(t *testing.T) {
	requestBody, err := os.ReadFile("../../fixtures/requests/frieren-episode-3.json")
	if err != nil {
		t.Fatal(err)
	}
	webCandidate := validCandidate("web-fast:400602:3:main", "web-fast", "hls")
	btCandidate := validCandidate("bt-slow:0123456789abcdef0123456789abcdef01234567", "bt-slow", "torrent")
	handler := testHandler(t, []sourceruntime.Runner{
		&fakeRunner{source: source("bt-slow", "bt", true), delay: 40 * time.Millisecond, candidates: []sourceruntime.Candidate{btCandidate}},
		&fakeRunner{source: source("web-fast", "web", true), delay: 5 * time.Millisecond, candidates: []sourceruntime.Candidate{webCandidate}},
		&fakeRunner{source: source("web-fail", "web", true), delay: 15 * time.Millisecond, err: sourceruntime.NewError("resolve_timeout", true, "source exceeded its configured deadline", context.DeadlineExceeded)},
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/candidates", bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/x-ndjson") || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected stream response: status=%d headers=%v", response.Code, response.Header())
	}
	events := decodeEvents(t, response.Body.Bytes())
	if len(events) != 4 {
		t.Fatalf("got %d events: %s", len(events), response.Body.String())
	}
	if events[0]["event"] != "candidate" || candidateSource(events[0]) != "web-fast" {
		t.Fatalf("fast WEB candidate was not streamed first: %v", events)
	}
	if events[1]["event"] != "source_error" || events[1]["category"] != "resolve_timeout" {
		t.Fatalf("source error was not streamed independently: %v", events[1])
	}
	if events[2]["event"] != "candidate" || candidateSource(events[2]) != "bt-slow" {
		t.Fatalf("BT candidate did not continue after WEB failure: %v", events[2])
	}
	done := events[3]
	if done["event"] != "done" || done["queried"] != float64(3) || done["succeeded"] != float64(2) || done["failed"] != float64(1) {
		t.Fatalf("invalid final counters: %v", done)
	}
}

func TestCandidatesRejectsInvalidRequestBeforeStartingStream(t *testing.T) {
	handler := testHandler(t, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/candidates", strings.NewReader(`{"schema":"nagare-resolve-request/v1","unknown":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") || !strings.Contains(response.Body.String(), "invalid_request") {
		t.Fatalf("invalid request started a stream: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestCandidatesRejectsOversizedBodyAsJSONError(t *testing.T) {
	handler := testHandler(t, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/candidates", strings.NewReader(`{"padding":"`+strings.Repeat("x", maximumRequestBytes)+`"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") || !strings.Contains(response.Body.String(), "request body is too large") {
		t.Fatalf("oversized request did not produce a deterministic API error: status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestClientCancellationStopsSourceWork(t *testing.T) {
	cancelled := make(chan struct{})
	started := make(chan struct{})
	runner := &fakeRunner{source: source("wait-source", "web", true), waitForCancel: true, started: started, cancelled: cancelled}
	handler := testHandler(t, []sourceruntime.Runner{runner})
	requestBody, err := os.ReadFile("../../fixtures/requests/frieren-episode-3.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/candidates", bytes.NewReader(requestBody)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	finished := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(finished)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("source runner did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("source runner did not observe client cancellation")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("candidate handler did not return after cancellation")
	}
}

func TestSelfCheckUsesRequestedModeAndReportsDisabledSource(t *testing.T) {
	runner := &fakeRunner{
		source:    source("web-source", "web", true),
		selfcheck: sourceruntime.SelfCheckResult{SourceID: "web-source", Status: "healthy", DurationMS: 2},
	}
	handler := testHandler(t, []sourceruntime.Runner{
		runner,
		&fakeRunner{source: source("bt-source", "bt", false)},
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/selfcheck", strings.NewReader(`{"sourceIds":["web-source","bt-source"],"mode":"fixture"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("selfcheck failed: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Results []sourceruntime.SelfCheckResult `json:"results"`
	}
	decodeResponse(t, response, &body)
	if len(body.Results) != 2 || body.Results[0].SourceID != "bt-source" || body.Results[0].Status != "disabled" || body.Results[1].Status != "healthy" {
		t.Fatalf("unexpected selfcheck results: %+v", body.Results)
	}
}

func TestSelfCheckRejectsDuplicateSourceIDs(t *testing.T) {
	handler := testHandler(t, []sourceruntime.Runner{
		&fakeRunner{source: source("web-source", "web", true)},
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/selfcheck", strings.NewReader(`{"sourceIds":["web-source","web-source"],"mode":"fixture"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "duplicate source id") {
		t.Fatalf("duplicate selfcheck source was accepted: status=%d body=%s", response.Code, response.Body.String())
	}
}

func testHandler(t *testing.T, runners []sourceruntime.Runner) *Handler {
	t.Helper()
	handler, err := New(Options{Root: "../..", Manifest: Manifest{Version: "test"}, Runners: runners})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func source(id, kind string, enabled bool) sourceruntime.Source {
	status := "healthy"
	if !enabled {
		status = "disabled"
	}
	return sourceruntime.Source{ID: id, Name: id, Kind: kind, Tier: 1, Version: "1", Enabled: enabled, Status: status, Capabilities: []string{kind}}
}

func validCandidate(id, sourceID, transportType string) sourceruntime.Candidate {
	episode := 3.0
	transport := sourceruntime.Transport{Type: transportType}
	if transportType == "torrent" {
		transport.InfoHash = "0123456789abcdef0123456789abcdef01234567"
	} else {
		transport.URL = "https://cdn.example.invalid/e3.m3u8"
	}
	return sourceruntime.Candidate{
		Schema: "nagare-candidate/v1", ID: id, SourceID: sourceID, Tier: 1, MatchConfidence: 1,
		Match:     sourceruntime.Match{Basis: []string{"title_episode"}, EpisodeNumber: &episode},
		Transport: transport, Metadata: sourceruntime.Metadata{Episode: &episode},
	}
}

func decodeEvents(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func candidateSource(event map[string]any) string {
	candidate, _ := event["candidate"].(map[string]any)
	value, _ := candidate["sourceId"].(string)
	return value
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, output any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), output); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
