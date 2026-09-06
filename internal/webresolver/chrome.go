package webresolver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

type ChromeOptions struct {
	ExecutablePath string
	TemporaryRoot  string
}

type ChromeBrowser struct {
	options ChromeOptions
}

func NewChrome(options ChromeOptions) *ChromeBrowser {
	return &ChromeBrowser{options: options}
}

func (browser *ChromeBrowser) Browse(ctx context.Context, request BrowseRequest, emit func(NetworkEvent)) error {
	if request.policy == nil {
		return errors.New("browser request is missing its network policy")
	}
	proxy, err := startOutboundProxy(request.policy, request.MaxBytes)
	if err != nil {
		return fmt.Errorf("start browser proxy: %w", err)
	}
	defer proxy.Close()
	profile, err := os.MkdirTemp(browser.options.TemporaryRoot, "nagare-browser-")
	if err != nil {
		return fmt.Errorf("create browser profile: %w", err)
	}
	defer os.RemoveAll(profile)

	allocatorOptions := append([]chromedp.ExecAllocatorOption(nil), chromedp.DefaultExecAllocatorOptions[:]...)
	if browser.options.ExecutablePath != "" {
		allocatorOptions = append(allocatorOptions, chromedp.ExecPath(browser.options.ExecutablePath))
	}
	allocatorOptions = append(allocatorOptions,
		chromedp.UserDataDir(profile),
		chromedp.ProxyServer(proxy.URL()),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
		chromedp.Flag("incognito", true),
		chromedp.Flag("disk-cache-size", "1"),
		chromedp.Flag("media-cache-size", "1"),
		chromedp.Flag("disable-application-cache", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-domain-reliability", true),
		chromedp.Flag("disable-quic", true),
		chromedp.Flag("force-webrtc-ip-handling-policy", "disable_non_proxied_udp"),
		chromedp.Flag("webrtc-ip-handling-policy", "disable_non_proxied_udp"),
	)
	allocatorContext, cancelAllocator := chromedp.NewExecAllocator(ctx, allocatorOptions...)
	defer cancelAllocator()
	// CDP errors can contain full request URLs. The resolver emits its own
	// sanitized diagnostics, so the browser library must not write them to the
	// process logger directly.
	discardLog := func(string, ...any) {}
	browserContext, cancelBrowser := chromedp.NewContext(allocatorContext,
		chromedp.WithLogf(discardLog),
		chromedp.WithErrorf(discardLog),
	)
	defer cancelBrowser()

	chromedp.ListenTarget(browserContext, func(value any) {
		switch event := value.(type) {
		case *network.EventRequestWillBeSent:
			if event.Request == nil {
				return
			}
			networkEvent := NetworkEvent{
				Kind:         EventRequest,
				RequestID:    string(event.RequestID),
				URL:          event.Request.URL,
				Method:       event.Request.Method,
				ResourceType: string(event.Type),
				Headers:      networkHeaders(event.Request.Headers),
				Redirect:     event.RedirectResponse != nil,
			}
			networkEvent.Navigate = func(callContext context.Context, targetURL string) error {
				if err := callContext.Err(); err != nil {
					return err
				}
				return chromedp.Run(browserContext, chromedp.Navigate(targetURL))
			}
			emit(networkEvent)
		case *network.EventRequestWillBeSentExtraInfo:
			emit(NetworkEvent{Kind: EventRequest, RequestID: string(event.RequestID), Headers: networkHeaders(event.Headers)})
		case *network.EventResponseReceived:
			if event.Response == nil {
				return
			}
			responseURL := event.Response.URL
			networkEvent := NetworkEvent{
				Kind:         EventResponse,
				RequestID:    string(event.RequestID),
				URL:          responseURL,
				ResourceType: string(event.Type),
				Status:       int(event.Response.Status),
				MIMEType:     event.Response.MimeType,
			}
			if request.CookiePolicy == "source" {
				networkEvent.SnapshotHeaders = func(callContext context.Context, playbackURL string) (map[string]string, error) {
					return snapshotCookieHeader(callContext, browserContext, playbackURL)
				}
			}
			emit(networkEvent)
		case *network.EventLoadingFailed:
			emit(NetworkEvent{Kind: EventFailed, RequestID: string(event.RequestID), ErrorText: sanitizeBrowserError(event.ErrorText)})
		}
	})

	actions := chromedp.Tasks{
		network.Enable(),
		network.SetCacheDisabled(true),
		network.ClearBrowserCache(),
		network.ClearBrowserCookies(),
	}
	if len(request.Headers) > 0 {
		headers := network.Headers{}
		for key, value := range request.Headers {
			headers[key] = value
		}
		actions = append(actions, network.SetExtraHTTPHeaders(headers))
	}
	if request.CookiePolicy == "source" {
		for _, value := range request.Cookies {
			name, cookieValue, ok := strings.Cut(value, "=")
			if !ok || strings.TrimSpace(name) == "" {
				continue
			}
			actions = append(actions, network.SetCookie(strings.TrimSpace(name), cookieValue).WithURL(request.URL))
		}
	}
	actions = append(actions, chromedp.Navigate(request.URL))
	if err := chromedp.Run(browserContext, actions); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if violation := proxy.Violation(); violation != nil {
			return wrapError(CategoryUnsafeRedirect, false, "browser proxy blocked an unsafe destination", violation)
		}
		return fmt.Errorf("browser navigation failed: %s", sanitizeBrowserError(err.Error()))
	}
	<-ctx.Done()
	return ctx.Err()
}

func snapshotCookieHeader(callContext, browserContext context.Context, rawURL string) (map[string]string, error) {
	if err := callContext.Err(); err != nil {
		return nil, err
	}
	queryContext, cancel := context.WithTimeout(browserContext, time.Second)
	defer cancel()
	var cookies []*network.Cookie
	err := chromedp.Run(queryContext, chromedp.ActionFunc(func(actionContext context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs([]string{rawURL}).Do(actionContext)
		return err
	}))
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie != nil {
			values = append(values, cookie.Name+"="+cookie.Value)
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	return map[string]string{"Cookie": strings.Join(values, "; ")}, nil
}

func networkHeaders(values network.Headers) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		if text, ok := value.(string); ok {
			result[key] = text
		} else if value != nil {
			result[key] = fmt.Sprint(value)
		}
	}
	return result
}

func sanitizeBrowserError(value string) string {
	if strings.Contains(value, "http://") || strings.Contains(value, "https://") {
		return "network request failed"
	}
	if len(value) > 300 {
		return value[:300]
	}
	return value
}

func FindChrome() string {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		for _, root := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if root != "" {
				candidates = append(candidates, filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}
