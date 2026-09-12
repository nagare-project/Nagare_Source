package webresolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

var sourceTemplatePattern = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_]*)\s*\}\}`)

type matchRules struct {
	include            []*regexp.Regexp
	exclude            []*regexp.Regexp
	nested             []*regexp.Regexp
	captureIndex       int
	captureName        string
	allowVerifiedMedia bool
}

func (runtime *Runtime) Resolve(ctx context.Context, source map[string]any, variables map[string]string) (Media, error) {
	started := time.Now()
	if runtime == nil || runtime.Browser == nil {
		return Media{}, wrapError(CategoryResolveFailed, false, "browser runtime is unavailable", nil)
	}
	if enabled, ok := source["enabled"].(bool); ok && !enabled {
		return Media{}, wrapError("source_disabled", false, "source is disabled", nil)
	}
	resolve := object(source["resolve"])
	if text(resolve["mode"]) != "browser_sniff" {
		return Media{}, wrapError(CategoryResolveFailed, false, "source does not use browser_sniff", nil)
	}
	timeout := durationMilliseconds(resolve["timeout_ms"], 15000)
	if limits := object(source["limits"]); len(limits) > 0 {
		limit := durationMilliseconds(limits["timeout_ms"], int(timeout/time.Millisecond))
		if limit < timeout {
			timeout = limit
		}
	}
	deadlineContext, cancelDeadline := context.WithTimeout(ctx, timeout)
	defer cancelDeadline()

	entryURL, err := renderTemplate(text(resolve["url"]), variables)
	if err != nil {
		return Media{}, wrapError(CategoryResolveFailed, false, "resolve URL template is invalid", err)
	}
	allowedHosts := stringSlice(resolve["allowed_hosts"])
	policy, err := newURLPolicy(allowedHosts, runtime.resolver, runtime.allowPrivate)
	if err != nil {
		return Media{}, wrapError(CategoryBrowserBlocked, false, "browser network policy is invalid", err)
	}
	if _, _, err := policy.validateURL(deadlineContext, entryURL); err != nil {
		if deadlineContext.Err() != nil {
			return Media{}, contextResolveError(deadlineContext.Err())
		}
		return Media{}, wrapError(CategoryUnsafeRedirect, false, "initial browser URL violates network policy", err)
	}
	egressPolicy := newPublicURLPolicy(runtime.resolver, runtime.allowPrivate)
	rules, err := compileMatchRules(object(resolve["match"]))
	if err != nil {
		return Media{}, wrapError(CategoryResolveFailed, false, "browser match rules are invalid", err)
	}
	transport := text(resolve["transport"])
	if transport != "auto" && transport != "hls" && transport != "http" {
		return Media{}, wrapError(CategoryResolveFailed, false, "browser transport is invalid", nil)
	}

	requestHeaders := stringObject(resolve["request_headers"])
	cookiePolicy := text(resolve["cookie_policy"])
	if cookiePolicy == "" {
		cookiePolicy = "none"
	}
	if cookiePolicy != "none" && cookiePolicy != "source" {
		return Media{}, wrapError(CategoryResolveFailed, false, "browser cookie policy is invalid", nil)
	}
	browseRequest := BrowseRequest{
		Headers:      requestHeaders,
		Cookies:      stringSlice(resolve["cookies"]),
		CookiePolicy: cookiePolicy,
		AllowedHosts: allowedHosts,
		MaxRedirects: integer(object(source["limits"])["max_redirects"], 3),
		MaxBytes:     int64(integer(object(source["limits"])["max_response_bytes"], 5*1024*1024)),
		policy:       policy,
		egressPolicy: egressPolicy,
	}

	currentURL := entryURL
	visited := map[string]bool{}
	for depth := 0; depth < 4; depth++ {
		if visited[currentURL] {
			return Media{}, wrapError(CategoryResolveFailed, false, "nested browser URL loop detected", nil)
		}
		visited[currentURL] = true
		browseRequest.URL = currentURL
		runtime.logf("browser session started: %s", RedactURL(currentURL))
		media, nestedURL, browseErr := runtime.browseOnce(deadlineContext, browseRequest, rules, transport, started)
		if browseErr != nil {
			return Media{}, browseErr
		}
		if media.URL != "" {
			runtime.logf("browser media response accepted: host=%s transport=%s", mediaHost(media.URL), media.Transport)
			return media, nil
		}
		if nestedURL == "" {
			return Media{}, wrapError(CategoryResolveFailed, true, "browser completed without a readable media response", nil)
		}
		currentURL = nestedURL
	}
	return Media{}, wrapError(CategoryResolveFailed, false, "nested browser navigation limit exceeded", nil)
}

func (runtime *Runtime) browseOnce(ctx context.Context, request BrowseRequest, rules matchRules, transport string, started time.Time) (Media, string, error) {
	sessionContext, cancelSession := context.WithCancel(ctx)
	events := make(chan NetworkEvent, 1024)
	done := make(chan error, 1)
	go func() {
		done <- runtime.Browser.Browse(sessionContext, request, func(event NetworkEvent) {
			select {
			case events <- event:
			case <-sessionContext.Done():
			default:
			}
		})
	}()
	defer cancelSession()

	requestHeaders := map[string]map[string]string{}
	requestURLs := map[string]string{}
	redirects := 0
	nestedNavigations := 0
	currentPageURL := request.URL
	nestedVisited := map[string]bool{request.URL: true}
	// 浏览器退出（done）和它退出前发出的最后几个事件（events 缓冲里）会同时就绪，
	// select 随机挑一个：先挑到 done 就把已经拿到的媒体响应当成「没找到」丢了（CI 上
	// 实测过这种失败）。所以 done 到了先不返回，把缓冲里的事件排干再下结论。
	finished := false
	var finishedErr error
	// 浏览器已退出时 done 早被收走，再等只会白等一秒
	wait := func() {
		if !finished {
			waitBrowser(done)
		}
	}
	browserStopped := func(err error) (Media, string, error) {
		if err == nil {
			return Media{}, "", wrapError(CategoryResolveFailed, true, "browser stopped before a media response was found", nil)
		}
		if errors.Is(err, context.Canceled) && ctx.Err() == nil {
			return Media{}, "", wrapError(CategoryResolveFailed, true, "browser session ended unexpectedly", err)
		}
		var typed *Error
		if asError(err, &typed) {
			return Media{}, "", typed
		}
		return Media{}, "", browserFailure("navigation or network error", err)
	}
	for {
		var event NetworkEvent
		if finished {
			select {
			case event = <-events:
			default:
				return browserStopped(finishedErr)
			}
		} else {
			select {
			case <-ctx.Done():
				cancelSession()
				wait()
				return Media{}, "", contextResolveError(ctx.Err())
			case err := <-done:
				finished, finishedErr = true, err
				continue
			case event = <-events:
			}
		}
		{
			if event.Kind == EventRequest && event.RequestID != "" && event.URL != "" {
				requestURLs[event.RequestID] = event.URL
			}
			if event.Kind == EventFailed {
				runtime.logf("browser request failed: host=%s resource=%s error=%s", mediaHost(requestURLs[event.RequestID]), event.ResourceType, event.ErrorText)
				continue
			}
			if event.RequestID != "" && len(event.Headers) > 0 {
				requestHeaders[event.RequestID] = mergeHeaders(requestHeaders[event.RequestID], event.Headers)
			}
			if event.Redirect {
				redirects++
				if redirects > request.MaxRedirects {
					cancelSession()
					wait()
					return Media{}, "", wrapError(CategoryUnsafeRedirect, false, "browser redirect limit exceeded", nil)
				}
			}
			if event.URL == "" {
				continue
			}
			_, _, boundaryErr := request.policy.validateURL(ctx, event.URL)
			withinSourceBoundary := boundaryErr == nil
			if event.Kind == EventResponse && event.Status >= 200 && event.Status < 300 {
				runtime.logf("browser response observed: host=%s resource=%s mime=%s source_boundary=%t", mediaHost(event.URL), event.ResourceType, event.MIMEType, withinSourceBoundary)
			}
			if !withinSourceBoundary && event.ResourceType == "Document" && event.TopLevel {
				cancelSession()
				wait()
				return Media{}, "", wrapError(CategoryUnsafeRedirect, false, "browser navigation crossed its network boundary", boundaryErr)
			}
			if event.Kind == EventRequest && rules.matchesNested(event.URL) && !rules.matchesMedia(event.URL) {
				if !withinSourceBoundary {
					continue
				}
				if event.ResourceType == "Document" {
					if event.URL != currentPageURL {
						nestedNavigations++
						if nestedNavigations >= 4 {
							cancelSession()
							wait()
							return Media{}, "", wrapError(CategoryResolveFailed, false, "nested browser navigation limit exceeded", nil)
						}
						currentPageURL = event.URL
						nestedVisited[event.URL] = true
						runtime.logf("browser session navigated: %s", RedactURL(event.URL))
					}
					continue
				}
				if nestedVisited[event.URL] {
					cancelSession()
					wait()
					return Media{}, "", wrapError(CategoryResolveFailed, false, "nested browser URL loop detected", nil)
				}
				nestedNavigations++
				if nestedNavigations >= 4 {
					cancelSession()
					wait()
					return Media{}, "", wrapError(CategoryResolveFailed, false, "nested browser navigation limit exceeded", nil)
				}
				nestedVisited[event.URL] = true
				if event.Navigate != nil {
					currentPageURL = event.URL
					runtime.logf("browser session navigating: %s", RedactURL(event.URL))
					if err := event.Navigate(ctx, event.URL); err != nil {
						if ctx.Err() != nil {
							cancelSession()
							wait()
							return Media{}, "", contextResolveError(ctx.Err())
						}
						cancelSession()
						wait()
						return Media{}, "", browserFailure("nested navigation failed", err)
					}
					continue
				}
				cancelSession()
				wait()
				return Media{}, event.URL, nil
			}
			mediaURL, matched := rules.mediaURL(event.URL)
			if !matched && event.VerifiedMedia && rules.allowVerifiedMedia {
				matched = true
				for _, pattern := range rules.exclude {
					if pattern.MatchString(event.URL) {
						matched = false
					}
				}
				mediaURL = event.URL
			}
			if event.Kind != EventResponse || event.Status < 200 || event.Status >= 300 || !matched {
				continue
			}
			mediaPolicy := request.egressPolicy
			if mediaPolicy == nil {
				mediaPolicy = request.policy
			}
			if _, _, err := mediaPolicy.validateURL(ctx, mediaURL); err != nil {
				cancelSession()
				wait()
				return Media{}, "", wrapError(CategoryUnsafeRedirect, false, "matched media URL crossed its network boundary", err)
			}
			actualTransport := detectTransport(transport, mediaURL, event.MIMEType)
			if actualTransport == "" {
				continue
			}
			headers := mergeHeaders(nil, request.Headers)
			if mediaHost(mediaURL) == mediaHost(request.URL) {
				headers = mergeHeaders(headers, sourceCookieHeaders(request))
			}
			headers = mergeHeaders(headers, requestHeaders[event.RequestID])
			if request.CookiePolicy == "source" && event.SnapshotHeaders != nil {
				if snapshot, snapshotErr := event.SnapshotHeaders(ctx, mediaURL); snapshotErr == nil {
					headers = mergeHeaders(headers, snapshot)
				} else {
					runtime.logf("browser playback header snapshot unavailable")
				}
			}
			headers = playbackHeaders(headers, request.CookiePolicy)
			media := Media{URL: mediaURL, Transport: actualTransport, Headers: headers, MIMEType: event.MIMEType, Status: event.Status, Duration: time.Since(started)}
			cancelSession()
			wait()
			return media, "", nil
		}
	}
}

func compileMatchRules(match map[string]any) (matchRules, error) {
	compile := func(values []string) ([]*regexp.Regexp, error) {
		result := make([]*regexp.Regexp, 0, len(values))
		for _, value := range values {
			pattern, err := regexp.Compile(value)
			if err != nil {
				return nil, err
			}
			result = append(result, pattern)
		}
		return result, nil
	}
	include, err := compile(stringSlice(match["include"]))
	if err != nil || len(include) == 0 {
		if err == nil {
			err = errors.New("at least one include pattern is required")
		}
		return matchRules{}, err
	}
	exclude, err := compile(stringSlice(match["exclude"]))
	if err != nil {
		return matchRules{}, err
	}
	nested, err := compile(stringSlice(match["nested_include"]))
	if err != nil {
		return matchRules{}, err
	}
	rules := matchRules{include: include, exclude: exclude, nested: nested, captureIndex: -1}
	rules.allowVerifiedMedia, _ = match["allow_verified_media"].(bool)
	switch capture := match["capture_group"].(type) {
	case nil:
	case string:
		if capture == "" {
			return matchRules{}, errors.New("capture group name is empty")
		}
		rules.captureName = capture
	case int:
		rules.captureIndex = capture
	case int64:
		rules.captureIndex = int(capture)
	case float64:
		if capture < 0 || capture != float64(int(capture)) {
			return matchRules{}, errors.New("capture group index is invalid")
		}
		rules.captureIndex = int(capture)
	case json.Number:
		parsed, parseErr := capture.Int64()
		if parseErr != nil {
			return matchRules{}, errors.New("capture group index is invalid")
		}
		rules.captureIndex = int(parsed)
	default:
		return matchRules{}, errors.New("capture group must be a name or index")
	}
	if rules.captureIndex < -1 {
		return matchRules{}, errors.New("capture group index cannot be negative")
	}
	if rules.captureName != "" {
		for _, pattern := range rules.include {
			if pattern.SubexpIndex(rules.captureName) >= 0 {
				return rules, nil
			}
		}
		return matchRules{}, fmt.Errorf("capture group %q is not present in include patterns", rules.captureName)
	}
	if rules.captureIndex >= 0 {
		for _, pattern := range rules.include {
			if rules.captureIndex <= pattern.NumSubexp() {
				return rules, nil
			}
		}
		return matchRules{}, fmt.Errorf("capture group %d is not present in include patterns", rules.captureIndex)
	}
	return rules, nil
}

func (rules matchRules) matchesMedia(raw string) bool {
	_, matched := rules.mediaURL(raw)
	return matched
}

func (rules matchRules) mediaURL(raw string) (string, bool) {
	for _, pattern := range rules.exclude {
		if pattern.MatchString(raw) {
			return "", false
		}
	}
	for _, pattern := range rules.include {
		match := pattern.FindStringSubmatch(raw)
		if match == nil {
			continue
		}
		candidate := raw
		if rules.captureName != "" {
			index := pattern.SubexpIndex(rules.captureName)
			if index < 0 || index >= len(match) || match[index] == "" {
				continue
			}
			candidate = match[index]
		} else if rules.captureIndex >= 0 {
			if rules.captureIndex >= len(match) || match[rules.captureIndex] == "" {
				continue
			}
			candidate = match[rules.captureIndex]
		}
		for _, excluded := range rules.exclude {
			if excluded.MatchString(candidate) {
				return "", false
			}
		}
		return candidate, true
	}
	return "", false
}

func (rules matchRules) matchesNested(raw string) bool {
	for _, pattern := range rules.nested {
		if pattern.MatchString(raw) {
			return true
		}
	}
	return false
}

func detectTransport(configured, raw, mime string) string {
	parsed, _ := url.Parse(raw)
	path := strings.ToLower(parsed.Path)
	mime = strings.ToLower(mime)
	if strings.Contains(mime, "text/html") || strings.Contains(mime, "application/json") {
		return ""
	}
	isHLS := strings.HasSuffix(path, ".m3u8") || strings.Contains(mime, "mpegurl")
	isHTTPMedia := strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/") ||
		strings.Contains(mime, "octet-stream") || strings.HasSuffix(path, ".mp4") ||
		strings.HasSuffix(path, ".mkv") || strings.HasSuffix(path, ".webm") || strings.HasSuffix(path, ".m4v")
	switch configured {
	case "hls":
		if !isHLS {
			return ""
		}
		return "hls"
	case "http":
		if isHLS || !isHTTPMedia {
			return ""
		}
		return "http"
	}
	if isHLS {
		return "hls"
	}
	if isHTTPMedia {
		return "http"
	}
	return ""
}

func renderTemplate(template string, variables map[string]string) (string, error) {
	if template == "" {
		return "", errors.New("empty template")
	}
	missing := map[string]bool{}
	result := sourceTemplatePattern.ReplaceAllStringFunc(template, func(match string) string {
		name := sourceTemplatePattern.FindStringSubmatch(match)[1]
		value, ok := variables[name]
		if !ok {
			missing[name] = true
			return match
		}
		return value
	})
	if strings.Contains(result, "{{") || strings.Contains(result, "}}") {
		var names []string
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) > 0 {
			return "", fmt.Errorf("missing variables: %s", strings.Join(names, ", "))
		}
		return "", errors.New("malformed template")
	}
	return result, nil
}

func playbackHeaders(headers map[string]string, cookiePolicy string) map[string]string {
	allowed := map[string]bool{"authorization": true, "cookie": cookiePolicy == "source", "origin": true, "referer": true, "user-agent": true}
	result := map[string]string{}
	for key, value := range headers {
		if allowed[strings.ToLower(key)] && strings.TrimSpace(value) != "" {
			result[key] = value
		}
	}
	return result
}

func sourceCookieHeaders(request BrowseRequest) map[string]string {
	if request.CookiePolicy != "source" || len(request.Cookies) == 0 {
		return nil
	}
	values := make([]string, 0, len(request.Cookies))
	for _, cookie := range request.Cookies {
		name, value, ok := strings.Cut(cookie, "=")
		if ok && strings.TrimSpace(name) != "" {
			values = append(values, strings.TrimSpace(name)+"="+value)
		}
	}
	if len(values) == 0 {
		return nil
	}
	return map[string]string{"Cookie": strings.Join(values, "; ")}
}

func mergeHeaders(base, overlay map[string]string) map[string]string {
	result := make(map[string]string, len(base)+len(overlay))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range overlay {
		for existing := range result {
			if strings.EqualFold(existing, key) && existing != key {
				delete(result, existing)
			}
		}
		result[key] = value
	}
	return result
}

func waitBrowser(done <-chan error) {
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func contextResolveError(cause error) error {
	if errors.Is(cause, context.DeadlineExceeded) {
		return wrapError(CategoryResolveTimeout, true, "browser resolve deadline exceeded", cause)
	}
	return wrapError(CategoryCancelled, true, "browser resolve was cancelled", cause)
}

func mediaHost(raw string) string {
	parsed, _ := url.Parse(raw)
	return parsed.Hostname()
}

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func text(value any) string {
	result, _ := value.(string)
	return result
}

func integer(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed)
		}
	}
	return fallback
}

func durationMilliseconds(value any, fallback int) time.Duration {
	milliseconds := integer(value, fallback)
	if milliseconds < 1 {
		milliseconds = fallback
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func stringSlice(value any) []string {
	if items, ok := value.([]string); ok {
		return append([]string(nil), items...)
	}
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item, ok := item.(string); ok {
			result = append(result, item)
		}
	}
	return result
}

func stringObject(value any) map[string]string {
	if items, ok := value.(map[string]string); ok {
		result := make(map[string]string, len(items))
		for key, value := range items {
			result[key] = value
		}
		return result
	}
	items := object(value)
	result := make(map[string]string, len(items))
	for key, value := range items {
		if value, ok := value.(string); ok {
			result[key] = value
		}
	}
	return result
}
