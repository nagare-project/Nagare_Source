package webresolver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNativeProbeUsesOnlyConfiguredReferer(t *testing.T) {
	for _, referer := range []string{"", "https://source.test/"} {
		t.Run(referer, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Referer") != referer {
					t.Errorf("inferred unexpected Referer: %q", r.Header.Get("Referer"))
				}
				w.Header().Set("Content-Type", "video/mp4")
				w.WriteHeader(206)
				w.Write([]byte("test video"))
			}))
			defer server.Close()
			policy := newPublicURLPolicy(nil, true)
			proxy, err := startOutboundProxy(policy, 1024)
			if err != nil {
				t.Fatal(err)
			}
			defer proxy.Close()
			browser := NewNativeWebView(NativeWebViewOptions{})
			result, err := browser.probeCandidate(context.Background(), proxy.URL(), BrowseRequest{Headers: map[string]string{"Referer": referer}, egressPolicy: policy}, nativeCandidate{URL: server.URL, Referer: "https://player.test/embed"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != 206 || result.Headers["Referer"] != referer {
				t.Fatalf("unexpected media probe: %+v", result)
			}
		})
	}
}

func TestNativeRejectsUnproxiedHTTPEntry(t *testing.T) {
	policy, _ := newURLPolicy([]string{"203.0.113.10"}, nil, false)
	browser := NewNativeWebView(NativeWebViewOptions{ExecutablePath: "must-not-run"})
	err := browser.Browse(context.Background(), BrowseRequest{URL: "http://203.0.113.10/watch", policy: policy, egressPolicy: policy}, func(NetworkEvent) { t.Fatal("HTTP entry should not launch") })
	assertCategory(t, err, CategoryBrowserBlocked)
}

func TestNativeWebViewAcceptsVerifiedExtensionlessVideo(t *testing.T) {
	helper := writeNativeHelper(t, `{"event":"candidate","url":"https://edge.akamaized.net/tokenized/asset","referer":"https://player.example.test/embed/3","userAgent":"Native UA","cookie":"session=short"}`)
	browser := NewNativeWebView(NativeWebViewOptions{ExecutablePath: helper, MaxSessions: 1})
	browser.probe = func(_ context.Context, _ string, _ BrowseRequest, candidate nativeCandidate) (nativeProbe, error) {
		return nativeProbe{
			URL: candidate.URL, Status: 206, MIMEType: "video/mp4",
			Headers: map[string]string{"User-Agent": candidate.UserAgent, "Referer": candidate.Referer, "Cookie": candidate.Cookie},
		}, nil
	}
	source := browserSource(1000)
	object(source["resolve"])["match"] = map[string]any{"include": []any{`akamaized\.net/`}}
	resolver := New(browser)
	resolver.resolver = staticResolver{"edge.akamaized.net": {{IP: net.ParseIP("198.51.100.20")}}}
	media, err := resolver.Resolve(context.Background(), source, map[string]string{
		"play_url": "https://203.0.113.10/watch/3",
	})
	if err != nil {
		t.Fatalf("%v: cause=%#v", err, errors.Unwrap(err))
	}
	if media.URL != "https://edge.akamaized.net/tokenized/asset" || media.Transport != "http" || media.MIMEType != "video/mp4" {
		t.Fatalf("extensionless native media was not accepted: %+v", media)
	}
	if media.Headers["User-Agent"] != "Native UA" || media.Headers["Referer"] != "https://player.example.test/embed/3" || media.Headers["Cookie"] != "session=short" {
		t.Fatalf("native playback headers were not preserved: %#v", media.Headers)
	}
}

func TestNativeWebViewPreservesStructuredFailure(t *testing.T) {
	helper := writeNativeHelper(t, `{"event":"error","category":"unsafe_redirect","message":"top-level navigation blocked"}`)
	browser := NewNativeWebView(NativeWebViewOptions{ExecutablePath: helper, MaxSessions: 1})
	_, err := New(browser).Resolve(context.Background(), browserSource(1000), map[string]string{
		"play_url": "https://203.0.113.10/watch/3",
	})
	assertCategory(t, err, CategoryUnsafeRedirect)
}

func writeNativeHelper(t *testing.T, event string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix helper fixture")
	}
	path := filepath.Join(t.TempDir(), "native-helper")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '" + event + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
