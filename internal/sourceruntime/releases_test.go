package sourceruntime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/btcrawler"
)

// releaseFetcher 按请求里的 q 参数（即搜索标题）返回响应，可给某个标题加延迟或失败。
type releaseFetcher struct {
	mu       sync.Mutex
	bodies   map[string]string
	delays   map[string]time.Duration
	failures map[string]error
	requests []*url.URL
}

func (fetcher *releaseFetcher) Fetch(ctx context.Context, request btcrawler.Request) (btcrawler.Response, error) {
	parsed, err := url.Parse(request.URL)
	if err != nil {
		return btcrawler.Response{}, err
	}
	title := parsed.Query().Get("q")
	fetcher.mu.Lock()
	fetcher.requests = append(fetcher.requests, parsed)
	delay, failure := fetcher.delays[title], fetcher.failures[title]
	body, ok := fetcher.bodies[title]
	fetcher.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return btcrawler.Response{}, ctx.Err()
		}
	}
	if failure != nil {
		return btcrawler.Response{}, failure
	}
	if !ok {
		body = `{"items":[]}`
	}
	return btcrawler.Response{URL: request.URL, Body: []byte(body)}, nil
}

func (fetcher *releaseFetcher) requestCount() int {
	fetcher.mu.Lock()
	defer fetcher.mu.Unlock()
	return len(fetcher.requests)
}

func (fetcher *releaseFetcher) queries() []string {
	fetcher.mu.Lock()
	defer fetcher.mu.Unlock()
	result := make([]string, 0, len(fetcher.requests))
	for _, request := range fetcher.requests {
		result = append(result, request.Query().Get("q")+"|page="+request.Query().Get("page"))
	}
	return result
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

// releaseSource 是一条 garden 形状的 JSON BT 规则：翻页、集号与作品名复核全开，
// 用来证明整部作品搜索不受这些设置影响。
func releaseSource() map[string]any {
	return map[string]any{
		"schema": "nagare-source/v1", "id": "bt-json", "name": "BT JSON", "kind": "bt", "tier": 3, "enabled": true,
		"origin": map[string]any{"version": "1"},
		"search": map[string]any{
			"response": "json",
			"request": map[string]any{
				"method": "GET", "url": "https://bt.example.test/search?q={{title}}&page={{page}}",
				"allowed_hosts": []any{"bt.example.test"},
			},
			"items": map[string]any{"type": "jsonpath", "expression": "$.items"},
			"fields": map[string]any{
				"title":   "$.title",
				"magnet":  "$.magnet",
				"seeders": map[string]any{"type": "jsonpath", "expression": "$.seeders", "required": false},
				"published_at": map[string]any{"type": "jsonpath", "expression": "$.date", "required": false, "transforms": []any{
					map[string]any{"type": "parse_datetime", "unit": "auto"},
				}},
				"episode": map[string]any{"type": "field", "expression": "title", "required": false, "transforms": []any{"parse_episode"}},
				"fansub":  map[string]any{"type": "field", "expression": "title", "required": false, "transforms": []any{"parse_fansub"}},
			},
		},
		"matching": map[string]any{"require_episode": true, "require_subject": true},
		"limits": map[string]any{
			"concurrency": 2, "requests_per_minute": 60000, "timeout_ms": 2000, "max_pages": 3,
			"max_response_bytes": 65536, "max_redirects": 2,
		},
	}
}

func releaseItem(hash, title string, seeders int, date string) string {
	item := fmt.Sprintf(`{"title":%q,"magnet":"magnet:?xt=urn:btih:%s"`, title, hash)
	if seeders >= 0 {
		item += fmt.Sprintf(`,"seeders":%d`, seeders)
	}
	if date != "" {
		item += fmt.Sprintf(`,"date":%q`, date)
	}
	return item + "}"
}

func releaseBody(items ...string) string {
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

func hash(digit string) string { return strings.Repeat(digit, 40) }

func newReleaseRunner(source map[string]any, fetcher *releaseFetcher, clock *fakeClock) *SpecRunner {
	options := RunnerOptions{Fetcher: fetcher}
	if clock != nil {
		options.Now = clock.Now
	}
	return NewSpecRunner(source, options)
}

func errorCategory(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Category
	}
	return ""
}

func TestReleasesSearchesEachTitleVerbatimOnFirstPageOnly(t *testing.T) {
	fetcher := &releaseFetcher{}
	runner := newReleaseRunner(releaseSource(), fetcher, nil)
	// searchTitles 会把它扩成「紧凑写法 / 原文 / 数字季」三个变体；整部作品搜索必须原样只搜一次。
	outcome := runner.Releases(context.Background(), []string{"幼女战记 第二季", "Youjo Senki II"})
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
	got := strings.Join(fetcher.queries(), ",")
	if fetcher.requestCount() != 2 || !strings.Contains(got, "幼女战记 第二季|page=1") || !strings.Contains(got, "Youjo Senki II|page=1") {
		t.Fatalf("want one page-1 request per verbatim title, got %s", got)
	}
}

func TestReleasesReturnsEveryEpisodeWithoutMatchingFilters(t *testing.T) {
	fetcher := &releaseFetcher{bodies: map[string]string{"Example Show": releaseBody(
		releaseItem(hash("1"), "[Group] 别名写法 [01-12][1080p]", -1, ""),
		releaseItem(hash("2"), "[Group] Example Show [01v2][1080p]", -1, ""),
		releaseItem(hash("3"), "[Group] Example Show - 07 [1080p]", -1, ""),
	)}}
	outcome := newReleaseRunner(releaseSource(), fetcher, nil).Releases(context.Background(), []string{"Example Show"})
	if outcome.Err != nil || len(outcome.Releases) != 3 {
		t.Fatalf("no release may be filtered: err=%v releases=%+v", outcome.Err, outcome.Releases)
	}
	episodes := map[string]string{}
	for _, release := range outcome.Releases {
		episodes[release.Title] = release.Episode
		if release.Transport.Type != "torrent" || release.ID != "bt-json:"+release.Transport.InfoHash {
			t.Fatalf("release identity is wrong: %+v", release)
		}
	}
	if episodes["[Group] 别名写法 [01-12][1080p]"] != "" || episodes["[Group] Example Show [01v2][1080p]"] != "1" || episodes["[Group] Example Show - 07 [1080p]"] != "7" {
		t.Fatalf("episode strings are wrong: %v", episodes)
	}
}

func TestReleasesDedupesPreferringSeedersAndSortsDeterministically(t *testing.T) {
	fetcher := &releaseFetcher{bodies: map[string]string{
		"A": releaseBody(
			releaseItem(hash("1"), "[G] Show - 01 (no seeders here)", -1, ""),
			releaseItem(hash("2"), "[G] Show - 02", 3, "2026-01-01T00:00:00Z"),
			releaseItem(hash("5"), "[G] Show - 05 first", 2, ""),
			releaseItem(hash("6"), "[G] Show - 06 b", -1, "2026-03-01T00:00:00Z"),
		),
		"B": releaseBody(
			releaseItem(hash("1"), "[G] Show - 01 (seeded copy)", 10, ""),
			releaseItem(hash("3"), "[G] Show - 03", -1, "2026-02-01T00:00:00Z"),
			releaseItem(hash("4"), "[G] Show - 04", -1, ""),
			releaseItem(hash("5"), "[G] Show - 05 second", 2, ""),
			releaseItem(hash("7"), "[G] Show - 06 a", -1, "2026-03-01T00:00:00Z"),
		),
	}}
	outcome := newReleaseRunner(releaseSource(), fetcher, nil).Releases(context.Background(), []string{"A", "B"})
	if outcome.Err != nil {
		t.Fatal(outcome.Err)
	}
	var titles []string
	for _, release := range outcome.Releases {
		titles = append(titles, release.Title)
	}
	want := []string{
		"[G] Show - 01 (seeded copy)", // 做种数 10，替换了没有做种数的同一发布
		"[G] Show - 02",               // 3
		"[G] Show - 05 first",         // 2；做种数相同保留第一次出现的
		"[G] Show - 06 a",             // 未知做种数：时间新的在前，同时间按标题
		"[G] Show - 06 b",
		"[G] Show - 03",
		"[G] Show - 04", // 时间也未知的排最后
	}
	if strings.Join(titles, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected order:\n%s\nwant:\n%s", strings.Join(titles, "\n"), strings.Join(want, "\n"))
	}
}

func TestReleasesKeepsPartialResultsWhenDeadlineHits(t *testing.T) {
	source := releaseSource()
	object(source["limits"])["timeout_ms"] = 150
	fetcher := &releaseFetcher{
		bodies: map[string]string{
			"fast": releaseBody(releaseItem(hash("a"), "[G] Show - 01", 1, "")),
			"slow": releaseBody(releaseItem(hash("b"), "[G] Show - 02", 1, "")),
		},
		delays: map[string]time.Duration{"slow": 5 * time.Second},
	}
	runner := newReleaseRunner(source, fetcher, nil)
	started := time.Now()
	outcome := runner.Releases(context.Background(), []string{"fast", "slow"})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("source deadline was not enforced: %s", elapsed)
	}
	if len(outcome.Releases) != 1 || outcome.Releases[0].Title != "[G] Show - 01" {
		t.Fatalf("collected releases were discarded on deadline: %+v", outcome.Releases)
	}
	if errorCategory(outcome.Err) != "search_timeout" || outcome.Cached {
		t.Fatalf("partial result must report search_timeout: err=%v cached=%v", outcome.Err, outcome.Cached)
	}
	before := fetcher.requestCount()
	runner.Releases(context.Background(), []string{"fast", "slow"})
	if fetcher.requestCount() == before {
		t.Fatal("a partial result was served from the cache")
	}
}

func TestReleasesDeadlineIncludesWaitingForConcurrencySlot(t *testing.T) {
	source := releaseSource()
	limits := object(source["limits"])
	limits["concurrency"], limits["timeout_ms"] = 1, 100
	runner := newReleaseRunner(source, &releaseFetcher{}, nil)
	runner.semaphore <- struct{}{} // 另一个搜索占着唯一的槽位
	defer func() { <-runner.semaphore }()
	started := time.Now()
	outcome := runner.Releases(context.Background(), []string{"Show"})
	if errorCategory(outcome.Err) != "search_timeout" || time.Since(started) > time.Second {
		t.Fatalf("queued search must time out within its deadline: err=%v after %s", outcome.Err, time.Since(started))
	}
}

func TestReleasesCacheHonoursTTLAndTitleSet(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	fetcher := &releaseFetcher{bodies: map[string]string{"Show": releaseBody(releaseItem(hash("1"), "[G] Show - 01", 1, ""))}}
	runner := newReleaseRunner(releaseSource(), fetcher, clock)
	first := runner.Releases(context.Background(), []string{"Show", "Other"})
	if first.Err != nil || first.Cached || len(first.Releases) != 1 {
		t.Fatalf("unexpected first search: %+v", first)
	}
	requests := fetcher.requestCount()
	clock.Advance(59 * time.Minute)
	// 标题顺序与大小写不同仍是同一个请求。
	second := runner.Releases(context.Background(), []string{"other", "SHOW"})
	if !second.Cached || len(second.Releases) != 1 || fetcher.requestCount() != requests {
		t.Fatalf("search within an hour must be served from the cache: %+v", second)
	}
	clock.Advance(2 * time.Minute)
	third := runner.Releases(context.Background(), []string{"Show", "Other"})
	if third.Cached || fetcher.requestCount() == requests {
		t.Fatalf("cache entry outlived its one-hour TTL: %+v", third)
	}
}

func TestReleasesCachesZeroResultsForFiveMinutes(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	fetcher := &releaseFetcher{}
	runner := newReleaseRunner(releaseSource(), fetcher, clock)
	if outcome := runner.Releases(context.Background(), []string{"Nothing"}); outcome.Err != nil || len(outcome.Releases) != 0 {
		t.Fatalf("unexpected zero search: %+v", outcome)
	}
	clock.Advance(4 * time.Minute)
	if outcome := runner.Releases(context.Background(), []string{"Nothing"}); !outcome.Cached {
		t.Fatal("zero result was not cached for five minutes")
	}
	clock.Advance(2 * time.Minute)
	if outcome := runner.Releases(context.Background(), []string{"Nothing"}); outcome.Cached || fetcher.requestCount() != 2 {
		t.Fatalf("zero result outlived its five-minute TTL: %+v requests=%d", outcome, fetcher.requestCount())
	}
}

func TestReleasesDoesNotCacheFailures(t *testing.T) {
	fetcher := &releaseFetcher{
		bodies:   map[string]string{"ok": releaseBody(releaseItem(hash("1"), "[G] Show - 01", 1, ""))},
		failures: map[string]error{"broken": errors.New("connection reset by peer")},
	}
	runner := newReleaseRunner(releaseSource(), fetcher, nil)
	outcome := runner.Releases(context.Background(), []string{"ok", "broken"})
	if len(outcome.Releases) != 1 || errorCategory(outcome.Err) != "search_failed" {
		t.Fatalf("one failed title must keep the other's releases and report search_failed: %+v", outcome)
	}
	again := runner.Releases(context.Background(), []string{"ok", "broken"})
	if again.Cached || fetcher.requestCount() != 4 {
		t.Fatalf("a result with failed requests was cached: %+v requests=%d", again, fetcher.requestCount())
	}
}

func TestReleasesStopsWhenCallerCancels(t *testing.T) {
	fetcher := &releaseFetcher{delays: map[string]time.Duration{"Show": 5 * time.Second}}
	runner := newReleaseRunner(releaseSource(), fetcher, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	started := time.Now()
	outcome := runner.Releases(ctx, []string{"Show"})
	if errorCategory(outcome.Err) != "cancelled" || time.Since(started) > time.Second {
		t.Fatalf("cancellation did not stop the search: err=%v after %s", outcome.Err, time.Since(started))
	}
}

func TestReleasesRejectsDisabledAndWebSources(t *testing.T) {
	disabled := releaseSource()
	disabled["enabled"] = false
	if outcome := newReleaseRunner(disabled, &releaseFetcher{}, nil).Releases(context.Background(), []string{"Show"}); errorCategory(outcome.Err) != "source_disabled" {
		t.Fatalf("disabled source searched: %+v", outcome)
	}
	if outcome := NewSpecRunner(jsonWebSource(), RunnerOptions{Fetcher: &releaseFetcher{}}).Releases(context.Background(), []string{"Show"}); errorCategory(outcome.Err) != "unsupported_rule" {
		t.Fatalf("web source accepted a release search: %+v", outcome)
	}
}

func TestNormalizeReleaseTitles(t *testing.T) {
	got := NormalizeReleaseTitles([]string{"  Frieren ", "", "frieren", "葬送的芙莉莲", " ", "Sousou no Frieren", "FRIEREN", "Frieren: Beyond", "extra"})
	want := []string{"Frieren", "葬送的芙莉莲", "Sousou no Frieren", "Frieren: Beyond"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReleaseDeadlineIsCappedAtEightSeconds(t *testing.T) {
	source := releaseSource()
	limits := object(source["limits"])
	for _, tc := range []struct {
		timeout any
		want    time.Duration
	}{{20000, 8 * time.Second}, {3000, 3 * time.Second}, {nil, 8 * time.Second}} {
		limits["timeout_ms"] = tc.timeout
		if got := NewSpecRunner(source, RunnerOptions{}).releaseDeadline(); got != tc.want {
			t.Errorf("timeout_ms=%v: got %s want %s", tc.timeout, got, tc.want)
		}
	}
}

func TestReleaseTitleIsTruncatedOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("葬", 600)
	got := truncateRunes(long, releaseTitleMaxRunes)
	if len([]rune(got)) != 512 || !strings.HasPrefix(long, got) {
		t.Fatalf("title was not truncated to 512 runes: %d", len([]rune(got)))
	}
	if truncateRunes("short", releaseTitleMaxRunes) != "short" {
		t.Fatal("short title was changed")
	}
}

func TestReleaseCacheEvictsLeastRecentlyUsed(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	cache := newReleaseCache(2)
	release := []Release{{ID: "x:1"}}
	cache.put("a", release, now, time.Hour)
	cache.put("b", release, now, time.Hour)
	if _, ok := cache.get("a", now); !ok {
		t.Fatal("a missing")
	}
	cache.put("c", release, now, time.Hour) // b 最久没用，被淘汰
	if _, ok := cache.get("b", now); ok {
		t.Fatal("least recently used entry was not evicted")
	}
	if _, ok := cache.get("a", now); !ok {
		t.Fatal("recently used entry was evicted")
	}
}

// 有条目却一条都抽不出来＝规则对不上站点：报失败、不缓存，不能伪装成「没有资源」。
func TestReleasesReportsRuleBreakageInsteadOfZero(t *testing.T) {
	fetcher := &releaseFetcher{bodies: map[string]string{"Show": `{"items":[{"name":"renamed field"},{"name":"another"}]}`}}
	runner := newReleaseRunner(releaseSource(), fetcher, nil)
	outcome := runner.Releases(context.Background(), []string{"Show"})
	if errorCategory(outcome.Err) != "search_failed" || len(outcome.Releases) != 0 {
		t.Fatalf("all-rejected response must fail: %+v", outcome)
	}
	if again := runner.Releases(context.Background(), []string{"Show"}); again.Cached || fetcher.requestCount() != 2 {
		t.Fatalf("broken-rule result was cached: %+v", again)
	}
}

type panickingFetcher struct{}

func (panickingFetcher) Fetch(context.Context, btcrawler.Request) (btcrawler.Response, error) {
	panic("malformed upstream response")
}

func TestReleasesRecoversPanicInTitleWorker(t *testing.T) {
	outcome := NewSpecRunner(releaseSource(), RunnerOptions{Fetcher: panickingFetcher{}}).Releases(context.Background(), []string{"Show"})
	if errorCategory(outcome.Err) != "search_failed" {
		t.Fatalf("panicking title worker was not converted into a failure: %+v", outcome)
	}
}

// 重复打开选集窗口：第二个请求排队等槽位，拿到槽位时第一个已经填好缓存，不该再打一遍站点。
func TestReleasesRechecksCacheAfterWaitingForSlot(t *testing.T) {
	source := releaseSource()
	object(source["limits"])["concurrency"] = 1
	fetcher := &releaseFetcher{
		bodies: map[string]string{"Show": releaseBody(releaseItem(hash("1"), "[G] Show - 01", 1, ""))},
		delays: map[string]time.Duration{"Show": 100 * time.Millisecond},
	}
	runner := newReleaseRunner(source, fetcher, nil)
	first := make(chan ReleaseOutcome, 1)
	go func() { first <- runner.Releases(context.Background(), []string{"Show"}) }()
	for deadline := time.Now().Add(time.Second); fetcher.requestCount() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("first search never started")
		}
		time.Sleep(time.Millisecond)
	}
	second := runner.Releases(context.Background(), []string{"show"})
	if (<-first).Cached || !second.Cached || fetcher.requestCount() != 1 {
		t.Fatalf("queued duplicate search re-crawled: second=%+v requests=%d", second, fetcher.requestCount())
	}
}

func TestReleasesHonoursDeclaredCacheTTL(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	disabled := releaseSource()
	disabled["cache"] = map[string]any{"ttl_ms": 0}
	fetcher := &releaseFetcher{}
	runner := newReleaseRunner(disabled, fetcher, clock)
	runner.Releases(context.Background(), []string{"Show"})
	if runner.Releases(context.Background(), []string{"Show"}).Cached {
		t.Fatal("cache.ttl_ms: 0 must disable the release cache")
	}
	short := releaseSource()
	short["cache"] = map[string]any{"ttl_ms": 60000}
	runner = newReleaseRunner(short, &releaseFetcher{}, clock)
	runner.Releases(context.Background(), []string{"Show"})
	clock.Advance(59 * time.Second)
	if !runner.Releases(context.Background(), []string{"Show"}).Cached {
		t.Fatal("declared one-minute TTL was not used")
	}
	clock.Advance(2 * time.Second)
	if runner.Releases(context.Background(), []string{"Show"}).Cached {
		t.Fatal("declared TTL did not cap the default five-minute zero-result TTL")
	}
}

func TestReleaseCacheReturnsIsolatedCopies(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	cache := newReleaseCache(4)
	cache.put("key", []Release{{ID: "x:1", Seeders: pointer(int64(5)), SizeBytes: pointer(int64(7))}}, now, time.Hour)
	got, _ := cache.get("key", now)
	*got[0].Seeders, *got[0].SizeBytes = 99, 99
	again, _ := cache.get("key", now)
	if *again[0].Seeders != 5 || *again[0].SizeBytes != 7 {
		t.Fatalf("caller mutation leaked into the cache: %+v", again[0])
	}
	if releaseCacheKey("s", []string{"a\x00b"}) == releaseCacheKey("s", []string{"a", "b"}) {
		t.Fatal("titles containing the separator collide with a different title set")
	}
}
