package webresolver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestChromeBrowserSniffsEphemeralHLS(t *testing.T) {
	if os.Getenv("NAGARE_BROWSER_TEST") != "1" {
		t.Skip("set NAGARE_BROWSER_TEST=1 to run the real Chrome isolation test")
	}
	executable := FindChrome()
	if executable == "" {
		t.Skip("Chrome or Chromium is not installed")
	}
	const pageSecret = "page-secret-value"
	const mediaSecret = "media-secret-value"
	var mu sync.Mutex
	mediaCookie := ""
	mediaReferer := ""
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/watch":
			http.SetCookie(writer, &http.Cookie{Name: "session", Value: "runtime-only", Path: "/"})
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(writer, `<!doctype html><script>fetch('/embed/player?token=nested-secret')</script>`)
		case "/embed/player":
			http.SetCookie(writer, &http.Cookie{Name: "nested", Value: "same-session", Path: "/"})
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(writer, `<!doctype html><script>fetch('/media/episode.m3u8?token=`+mediaSecret+`')</script>`)
		case "/media/episode.m3u8":
			mu.Lock()
			mediaCookie = request.Header.Get("Cookie")
			mediaReferer = request.Header.Get("Referer")
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(writer, "#EXTM3U\n#EXT-X-ENDLIST\n")
		default:
			http.NotFound(writer, request)
		}
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	temporaryRoot := t.TempDir()
	runtime := New(NewChrome(ChromeOptions{ExecutablePath: executable, TemporaryRoot: temporaryRoot}))
	runtime.allowPrivate = true
	var logs strings.Builder
	runtime.Logger = LoggerFunc(func(format string, arguments ...any) {
		fmt.Fprintf(&logs, format, arguments...)
		logs.WriteByte('\n')
	})
	source := map[string]any{
		"enabled": true,
		"resolve": map[string]any{
			"mode":          "browser_sniff",
			"transport":     "auto",
			"url":           "{{play_url}}",
			"allowed_hosts": []any{parsed.Hostname()},
			"cookie_policy": "source",
			"cookies":       []any{"quality=1080"},
			"request_headers": map[string]any{
				"Referer": server.URL + "/source",
			},
			"timeout_ms": 15000,
			"match": map[string]any{
				"include":        []any{`\.m3u8(?:\?|$)`},
				"nested_include": []any{`/embed/`},
			},
		},
		"limits": map[string]any{"timeout_ms": 15000, "max_redirects": 2, "max_response_bytes": 1024 * 1024},
	}
	media, err := runtime.Resolve(context.Background(), source, map[string]string{
		"play_url": server.URL + "/watch?token=" + pageSecret,
	})
	if err != nil {
		var typed *Error
		if errors.As(err, &typed) {
			t.Fatalf("%v (cause: %v)", err, typed.Cause)
		}
		t.Fatal(err)
	}
	if media.Transport != "hls" || !strings.Contains(media.URL, mediaSecret) {
		t.Fatalf("unexpected browser result: %+v", media)
	}
	returnedCookie := ""
	for key, value := range media.Headers {
		if strings.EqualFold(key, "Cookie") {
			returnedCookie = value
			break
		}
	}
	if !strings.Contains(returnedCookie, "quality=1080") || !strings.Contains(returnedCookie, "session=runtime-only") || !strings.Contains(returnedCookie, "nested=same-session") {
		t.Fatalf("playback headers do not contain source/session cookies: %q", returnedCookie)
	}
	mu.Lock()
	observedCookie := mediaCookie
	observedReferer := mediaReferer
	mu.Unlock()
	if !strings.Contains(observedCookie, "quality=1080") || !strings.Contains(observedCookie, "session=runtime-only") || !strings.Contains(observedCookie, "nested=same-session") {
		t.Fatalf("source/session cookies were not isolated and forwarded: %q", observedCookie)
	}
	if observedReferer != server.URL+"/source" {
		t.Fatalf("resolve Referer was not forwarded: %q", observedReferer)
	}
	for _, secret := range []string{pageSecret, mediaSecret, "nested-secret", "runtime-only", "same-session"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("browser logs leaked %q: %s", secret, logs.String())
		}
	}
	entries, err := os.ReadDir(temporaryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("ephemeral browser profile was not removed: %v", entries)
	}
}
