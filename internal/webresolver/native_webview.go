package webresolver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type NativeWebViewOptions struct {
	ExecutablePath string
	MaxSessions    int
	Logger         Logger
}

type NativeWebViewBrowser struct {
	options NativeWebViewOptions
	slots   chan struct{}
	probe   func(context.Context, string, BrowseRequest, nativeCandidate) (nativeProbe, error)
	serial  atomic.Uint64
}

type nativeInput struct {
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers,omitempty"`
	Cookies      []string          `json:"cookies,omitempty"`
	AllowedHosts []string          `json:"allowedHosts"`
	MaxRedirects int               `json:"maxRedirects"`
	TimeoutMS    int64             `json:"timeoutMs"`
	Proxy        nativeProxy       `json:"proxy"`
}

type nativeProxy struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type nativeEvent struct {
	Event     string `json:"event"`
	Category  string `json:"category,omitempty"`
	Message   string `json:"message,omitempty"`
	URL       string `json:"url,omitempty"`
	Referer   string `json:"referer,omitempty"`
	UserAgent string `json:"userAgent,omitempty"`
	Cookie    string `json:"cookie,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
}

type nativeCandidate struct {
	URL       string
	Referer   string
	UserAgent string
	Cookie    string
}

type nativeProbe struct {
	URL      string
	Status   int
	MIMEType string
	Headers  map[string]string
}

func NewNativeWebView(options NativeWebViewOptions) *NativeWebViewBrowser {
	if options.MaxSessions <= 0 {
		options.MaxSessions = 2
	}
	browser := &NativeWebViewBrowser{options: options, slots: make(chan struct{}, options.MaxSessions)}
	browser.probe = browser.probeCandidate
	return browser
}

// FindNativeWebViewHelper locates the helper beside a source checkout or a
// packaged plugin executable. The helper is used only on macOS 14 or newer.
func FindNativeWebViewHelper(root string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	var candidates []string
	if configured := strings.TrimSpace(os.Getenv("NAGARE_SOURCE_WK_HELPER")); configured != "" {
		candidates = append(candidates, configured)
	}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "nagare-source-wk"))
	}
	candidates = append(candidates,
		filepath.Join(root, "bin", "nagare-source-wk"),
		filepath.Join(root, "native", "macos", "nagare-source-wk"),
	)
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

func (browser *NativeWebViewBrowser) Browse(ctx context.Context, request BrowseRequest, emit func(NetworkEvent)) error {
	if browser == nil || browser.options.ExecutablePath == "" {
		return wrapError(CategoryBrowserBlocked, false, "native WebView helper is unavailable", nil)
	}
	if request.policy == nil || request.egressPolicy == nil {
		return errors.New("browser request is missing its network policy")
	}
	entry, err := url.Parse(request.URL)
	if err != nil || entry.Scheme != "https" {
		return wrapError(CategoryBrowserBlocked, false, "experimental native WebView requires an HTTPS playback page", nil)
	}
	select {
	case browser.slots <- struct{}{}:
		defer func() { <-browser.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}

	proxy, err := startOutboundProxy(request.egressPolicy, request.MaxBytes)
	if err != nil {
		return fmt.Errorf("start native WebView proxy: %w", err)
	}
	defer proxy.Close()
	defer func() {
		if browser.options.Logger != nil {
			browser.options.Logger.Printf("native proxy requests: %d", proxy.requests.Load())
		}
	}()
	proxyURL, err := url.Parse(proxy.URL())
	if err != nil {
		return fmt.Errorf("parse native WebView proxy address: %w", err)
	}
	port, err := strconv.Atoi(proxyURL.Port())
	if err != nil {
		return fmt.Errorf("parse native WebView proxy port: %w", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(15 * time.Second)
	}
	input := nativeInput{
		URL: request.URL, Headers: request.Headers, Cookies: request.Cookies,
		AllowedHosts: request.AllowedHosts, MaxRedirects: request.MaxRedirects,
		TimeoutMS: maxInt64(100, time.Until(deadline).Milliseconds()),
		Proxy:     nativeProxy{Host: proxyURL.Hostname(), Port: port},
	}
	if request.CookiePolicy != "source" {
		input.Cookies = nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode native WebView input: %w", err)
	}

	command := exec.CommandContext(ctx, browser.options.ExecutablePath)
	command.Stdin = bytes.NewReader(encoded)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return browserFailure("native WebView output is unavailable", err)
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return browserFailure("native WebView could not start", err)
	}
	defer command.Process.Kill()

	var reportedError error
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var event nativeEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			reportedError = browserFailure("native WebView returned invalid output", err)
			_ = command.Process.Kill()
			break
		}
		switch event.Event {
		case "candidate":
			if browser.options.Logger != nil {
				browser.options.Logger.Printf("native media hint: host=%s evidence=%s", mediaHost(event.URL), event.Evidence)
			}
			candidate := nativeCandidate{URL: event.URL, Referer: event.Referer, UserAgent: event.UserAgent, Cookie: event.Cookie}
			probe, probeErr := browser.probe(ctx, proxy.URL(), request, candidate)
			if probeErr != nil {
				if browser.options.Logger != nil {
					browser.options.Logger.Printf("native media probe failed: %s", sanitizeBrowserError(probeErr.Error()))
				}
				continue
			}
			if browser.options.Logger != nil {
				browser.options.Logger.Printf("native media probe: status=%d mime=%s", probe.Status, probe.MIMEType)
			}
			requestID := fmt.Sprintf("native-%d", browser.serial.Add(1))
			emit(NetworkEvent{Kind: EventRequest, RequestID: requestID, URL: probe.URL, Method: http.MethodGet, ResourceType: "Media", Headers: probe.Headers})
			responseEvent := NetworkEvent{
				Kind: EventResponse, RequestID: requestID, URL: probe.URL, ResourceType: "Media",
				Status: probe.Status, MIMEType: probe.MIMEType,
				VerifiedMedia: strings.HasPrefix(probe.MIMEType, "video/") || strings.Contains(probe.MIMEType, "mpegurl"),
			}
			emit(responseEvent)
		case "error":
			category := event.Category
			if category == "" {
				category = CategoryBrowserBlocked
			}
			reportedError = wrapError(category, category != CategoryUnsafeRedirect, event.Message, nil)
		}
	}
	if scanner.Err() != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if violation := proxy.Violation(); violation != nil {
		return wrapError(CategoryUnsafeRedirect, false, "native WebView proxy blocked an unsafe destination", violation)
	}
	if reportedError != nil {
		return reportedError
	}
	if err := scanner.Err(); err != nil {
		return browserFailure("native WebView output failed", err)
	}
	if waitErr != nil {
		return browserFailure("native WebView process failed", waitErr)
	}
	return wrapError(CategoryResolveFailed, true, "native WebView stopped before media was found", nil)
}

func (browser *NativeWebViewBrowser) probeCandidate(ctx context.Context, proxyAddress string, browse BrowseRequest, candidate nativeCandidate) (nativeProbe, error) {
	parsedProxy, err := url.Parse(proxyAddress)
	if err != nil {
		return nativeProbe{}, err
	}
	probeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if browse.egressPolicy == nil {
		return nativeProbe{}, errors.New("media probe policy is unavailable")
	}
	if _, _, err := browse.egressPolicy.validateURL(probeContext, candidate.URL); err != nil {
		return nativeProbe{}, err
	}
	request, err := http.NewRequestWithContext(probeContext, http.MethodGet, candidate.URL, nil)
	if err != nil {
		return nativeProbe{}, err
	}
	for name, value := range playbackHeaders(browse.Headers, browse.CookiePolicy) {
		request.Header.Set(name, value)
	}
	if request.Header.Get("User-Agent") == "" && candidate.UserAgent != "" {
		request.Header.Set("User-Agent", candidate.UserAgent)
	}
	// A frame URL is not evidence of the browser's actual Referer policy.
	// Preserve only a Referer explicitly provided by the source rule.
	if browse.CookiePolicy == "source" && candidate.Cookie != "" {
		request.Header.Set("Cookie", candidate.Cookie)
	}
	request.Header.Set("Range", "bytes=0-4095")
	transport := &http.Transport{Proxy: http.ProxyURL(parsedProxy), DisableCompression: true, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) > browse.MaxRedirects {
			return errors.New("media probe redirect limit exceeded")
		}
		if next.URL.Host != via[len(via)-1].URL.Host {
			next.Header.Del("Cookie")
			next.Header.Del("Authorization")
		}
		_, _, err := browse.egressPolicy.validateURL(probeContext, next.URL.String())
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return nativeProbe{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nativeProbe{}, fmt.Errorf("media probe returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return nativeProbe{}, err
	}
	if len(body) == 0 {
		return nativeProbe{}, errors.New("media probe returned an empty body")
	}
	mimeType := response.Header.Get("Content-Type")
	if separator := strings.IndexByte(mimeType, ';'); separator >= 0 {
		mimeType = strings.TrimSpace(mimeType[:separator])
	}
	if strings.HasPrefix(strings.TrimSpace(string(body)), "#EXTM3U") {
		mimeType = "application/vnd.apple.mpegurl"
	} else if strings.Contains(strings.ToLower(mimeType), "mpegurl") {
		return nativeProbe{}, errors.New("media probe returned an invalid HLS manifest")
	}
	headers := map[string]string{}
	for _, name := range []string{"User-Agent", "Referer", "Origin", "Cookie", "Authorization"} {
		if value := response.Request.Header.Get(name); value != "" {
			headers[name] = value
		}
	}
	return nativeProbe{URL: response.Request.URL.String(), Status: response.StatusCode, MIMEType: mimeType, Headers: headers}, nil
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
