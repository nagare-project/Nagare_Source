package webresolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type scriptedBrowser struct {
	mu      sync.Mutex
	scripts [][]NetworkEvent
	errors  []error
	calls   int
}

func (browser *scriptedBrowser) Browse(ctx context.Context, _ BrowseRequest, emit func(NetworkEvent)) error {
	browser.mu.Lock()
	index := browser.calls
	browser.calls++
	var script []NetworkEvent
	if index < len(browser.scripts) {
		script = browser.scripts[index]
	}
	var result error
	if index < len(browser.errors) {
		result = browser.errors[index]
	}
	browser.mu.Unlock()
	for _, event := range script {
		emit(event)
	}
	if result != nil {
		return result
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestResolveFollowsNestedPageAndAcceptsReadableHLS(t *testing.T) {
	browser := &scriptedBrowser{scripts: [][]NetworkEvent{
		{{Kind: EventRequest, RequestID: "nested", URL: "https://203.0.113.10/embed/player?token=nested-secret", ResourceType: "Fetch"}},
		{
			{Kind: EventRequest, RequestID: "media", URL: "https://203.0.113.10/media/episode.m3u8?token=media-secret", Headers: map[string]string{"Cookie": "session=ephemeral"}},
			{Kind: EventResponse, RequestID: "media", URL: "https://203.0.113.10/media/episode.m3u8?token=media-secret", Status: 200, MIMEType: "application/vnd.apple.mpegurl"},
		},
	}}
	var logs strings.Builder
	runtime := New(browser)
	runtime.Logger = LoggerFunc(func(format string, arguments ...any) {
		fmt.Fprintf(&logs, format, arguments...)
		logs.WriteByte('\n')
	})
	media, err := runtime.Resolve(context.Background(), browserSource(500), map[string]string{
		"play_url": "https://203.0.113.10/watch/3?page_token=page-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if media.Transport != "hls" || media.Status != 200 || !strings.Contains(media.URL, "media-secret") {
		t.Fatalf("unexpected media: %+v", media)
	}
	if media.Headers["Cookie"] != "session=ephemeral" {
		t.Fatalf("ephemeral request headers were not returned: %#v", media.Headers)
	}
	if browser.calls != 2 {
		t.Fatalf("browser calls = %d, want nested navigation plus media page", browser.calls)
	}
	for _, secret := range []string{"page-secret", "nested-secret", "media-secret", "ephemeral"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs leaked %q: %s", secret, logs.String())
		}
	}
}

func TestResolveKeepsBrowserSessionForNestedNavigation(t *testing.T) {
	navigated := ""
	browser := &scriptedBrowser{scripts: [][]NetworkEvent{{
		{
			Kind: EventRequest, RequestID: "nested", URL: "https://203.0.113.10/embed/player", ResourceType: "Fetch",
			Navigate: func(_ context.Context, target string) error {
				navigated = target
				return nil
			},
		},
		{Kind: EventResponse, RequestID: "media", URL: "https://203.0.113.10/media/episode.m3u8", Status: 200, MIMEType: "application/vnd.apple.mpegurl"},
	}}}
	media, err := New(browser).Resolve(context.Background(), browserSource(500), map[string]string{
		"play_url": "https://203.0.113.10/watch/3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if browser.calls != 1 || navigated != "https://203.0.113.10/embed/player" || media.Transport != "hls" {
		t.Fatalf("nested navigation did not reuse the browser session: calls=%d navigated=%q media=%+v", browser.calls, navigated, media)
	}
}

func TestResolveUsesNamedMediaCaptureAndCookieSnapshot(t *testing.T) {
	const captured = "https://203.0.113.10/media/captured.m3u8?token=short-lived"
	browser := &scriptedBrowser{scripts: [][]NetworkEvent{{{
		Kind: EventResponse, RequestID: "relay", URL: "https://203.0.113.10/relay?media=" + captured, Status: 200,
		MIMEType: "application/vnd.apple.mpegurl",
		SnapshotHeaders: func(_ context.Context, target string) (map[string]string, error) {
			if target != captured {
				t.Fatalf("snapshot target = %q, want captured media URL", target)
			}
			return map[string]string{"Cookie": "quality=1080; session=runtime-only"}, nil
		},
	}}}}
	source := browserSource(500)
	resolve := object(source["resolve"])
	resolve["match"] = map[string]any{
		"include":       []any{`media=(?P<video>https://[^&]+\.m3u8(?:\?[^&]+)?)`},
		"capture_group": "video",
	}
	resolve["cookies"] = []string{"quality=1080"}
	media, err := New(browser).Resolve(context.Background(), source, map[string]string{
		"play_url": "https://203.0.113.10/watch/3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if media.URL != captured || media.Headers["Cookie"] != "quality=1080; session=runtime-only" {
		t.Fatalf("captured media or cookie snapshot was lost: %+v", media)
	}
}

func TestResolveRejectsCrossBoundaryRedirect(t *testing.T) {
	browser := &scriptedBrowser{scripts: [][]NetworkEvent{{{
		Kind: EventRequest, RequestID: "redirect", URL: "http://127.0.0.1/admin?token=secret", ResourceType: "Document", Redirect: true,
	}}}}
	_, err := New(browser).Resolve(context.Background(), browserSource(500), map[string]string{
		"play_url": "https://203.0.113.10/watch/3",
	})
	assertCategory(t, err, CategoryUnsafeRedirect)
	if strings.Contains(err.Error(), "token") {
		t.Fatalf("error leaked redirect query: %v", err)
	}
}

func TestResolveTimeoutAndCancellationAreDistinct(t *testing.T) {
	_, err := New(&scriptedBrowser{}).Resolve(context.Background(), browserSource(25), map[string]string{
		"play_url": "https://203.0.113.10/watch/3",
	})
	assertCategory(t, err, CategoryResolveTimeout)

	context, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = New(&scriptedBrowser{}).Resolve(context, browserSource(500), map[string]string{
		"play_url": "https://203.0.113.10/watch/3",
	})
	assertCategory(t, err, CategoryCancelled)
}

func TestResolveRequiresCompleteTemplateAndSuccessfulResponse(t *testing.T) {
	_, err := New(&scriptedBrowser{}).Resolve(context.Background(), browserSource(100), map[string]string{})
	assertCategory(t, err, CategoryResolveFailed)

	browser := &scriptedBrowser{
		scripts: [][]NetworkEvent{{{Kind: EventResponse, RequestID: "bad", URL: "https://203.0.113.10/media/ad.m3u8", Status: 403}}},
		errors:  []error{errors.New("page finished")},
	}
	_, err = New(browser).Resolve(context.Background(), browserSource(100), map[string]string{
		"play_url": "https://203.0.113.10/watch/3",
	})
	assertCategory(t, err, CategoryBrowserBlocked)
}

func browserSource(timeout int) map[string]any {
	return map[string]any{
		"enabled": true,
		"resolve": map[string]any{
			"mode":          "browser_sniff",
			"transport":     "auto",
			"url":           "{{play_url}}",
			"allowed_hosts": []any{"203.0.113.10"},
			"cookie_policy": "source",
			"timeout_ms":    timeout,
			"match": map[string]any{
				"include":        []any{`\.m3u8(?:\?|$)`, `\.mp4(?:\?|$)`},
				"exclude":        []any{`/advertising/`},
				"nested_include": []any{`/embed/`},
			},
		},
		"limits": map[string]any{"timeout_ms": timeout, "max_redirects": 2, "max_response_bytes": 1024 * 1024},
	}
}

func assertCategory(t *testing.T, err error, expected string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", expected)
	}
	var typed *Error
	if !asError(err, &typed) || typed.Category != expected {
		t.Fatalf("got error %T %v, want category %s", err, err, expected)
	}
}

type staticResolver map[string][]net.IPAddr

func (resolver staticResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	values, ok := resolver[host]
	if !ok {
		return nil, fmt.Errorf("unknown host")
	}
	return values, nil
}

type blockingResolver struct{}

func (blockingResolver) LookupIPAddr(ctx context.Context, _ string) ([]net.IPAddr, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestResolveDeadlineCoversInitialDNSValidation(t *testing.T) {
	browser := &scriptedBrowser{}
	runtime := New(browser)
	runtime.resolver = blockingResolver{}
	source := browserSource(25)
	resolve := object(source["resolve"])
	resolve["allowed_hosts"] = []string{"resolver.example"}
	started := time.Now()
	_, err := runtime.Resolve(context.Background(), source, map[string]string{
		"play_url": "https://resolver.example/watch/3",
	})
	assertCategory(t, err, CategoryResolveTimeout)
	if browser.calls != 0 || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("initial DNS ignored the resolve deadline: browser_calls=%d elapsed=%s", browser.calls, time.Since(started))
	}
}

func TestURLPolicyPinsPublicAddressesAndRedactsQueries(t *testing.T) {
	resolver := staticResolver{
		"cdn.example.org":     {{IP: net.ParseIP("203.0.113.20")}},
		"private.example.org": {{IP: net.ParseIP("10.0.0.2")}},
	}
	policy, err := newURLPolicy([]string{"*.example.org"}, resolver, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, addresses, err := policy.validateURL(context.Background(), "https://cdn.example.org/video.m3u8?token=secret"); err != nil || len(addresses) != 1 {
		t.Fatalf("public URL did not validate: addresses=%v err=%v", addresses, err)
	}
	if _, _, err := policy.validateURL(context.Background(), "https://example.org/video.m3u8"); err == nil {
		t.Fatal("wildcard unexpectedly matched its root domain")
	}
	if _, _, err := policy.validateURL(context.Background(), "https://private.example.org/video.m3u8"); err == nil {
		t.Fatal("private DNS result unexpectedly passed")
	}
	redacted := RedactURL("https://user:password@cdn.example.org/video.m3u8?token=secret#fragment")
	if strings.Contains(redacted, "secret") || strings.Contains(redacted, "password") || strings.Contains(redacted, "fragment") {
		t.Fatalf("URL was not redacted: %s", redacted)
	}
}

func TestSharedAddressSpaceIsUnsafe(t *testing.T) {
	for _, address := range []string{"100.64.0.1", "100.127.255.254", "10.0.0.1", "169.254.1.1", "::1", "fc00::1"} {
		if !unsafeIP(net.ParseIP(address)) {
			t.Errorf("%s unexpectedly considered safe", address)
		}
	}
	if unsafeIP(net.ParseIP("203.0.113.1")) {
		t.Fatal("public test address unexpectedly considered unsafe")
	}
}

func TestWaitBrowserDoesNotDelayNormalCancellation(t *testing.T) {
	done := make(chan error, 1)
	done <- context.Canceled
	started := time.Now()
	waitBrowser(done)
	if time.Since(started) > time.Second {
		t.Fatal("waitBrowser delayed a completed session")
	}
}

func TestDetectTransportRejectsConfiguredTypeMismatch(t *testing.T) {
	if got := detectTransport("hls", "https://203.0.113.1/video.mp4", "video/mp4"); got != "" {
		t.Fatalf("MP4 unexpectedly accepted as configured HLS: %q", got)
	}
	if got := detectTransport("http", "https://203.0.113.1/video.m3u8", "application/vnd.apple.mpegurl"); got != "" {
		t.Fatalf("HLS unexpectedly accepted as configured HTTP: %q", got)
	}
	if got := detectTransport("auto", "https://203.0.113.1/play?id=3", "application/x-mpegURL"); got != "hls" {
		t.Fatalf("MIME-based HLS detection returned %q", got)
	}
}

func TestCompileMatchRulesRejectsMissingCaptureGroup(t *testing.T) {
	_, err := compileMatchRules(map[string]any{
		"include":       []string{`https://[^/]+/media\.m3u8`},
		"capture_group": "video",
	})
	if err == nil {
		t.Fatal("missing named capture group unexpectedly compiled")
	}
	_, err = compileMatchRules(map[string]any{
		"include":       []string{`https://([^/]+)/media\.m3u8`},
		"capture_group": 2,
	})
	if err == nil {
		t.Fatal("out-of-range numeric capture group unexpectedly compiled")
	}
}
