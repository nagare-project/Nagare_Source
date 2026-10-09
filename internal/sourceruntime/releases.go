package sourceruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/btcrawler"
	"github.com/nagare-project/Nagare_Source/internal/btindex"
)

// 整部作品的 BT 发布搜索（/v1/releases）：每个标题原样只搜第一页、不按集号筛，
// 一次拿到来源上这部作品的全部发布，选集交给客户端在本机完成（与 animego 网站的磁力搜索同一模型）。
const (
	// 选集窗口等不起来源的完整 timeout_ms（20 秒）：到点就把已经拿到的部分交出去。
	releaseSearchDeadline = 8 * time.Second
	releaseTitleLimit     = 4
	releaseTitleMaxRunes  = 512
)

// Release 是 release 事件里的一条 BT 发布，必须通过 schema/release-v1.schema.json。
type Release struct {
	ID          string           `json:"id"`
	SourceID    string           `json:"sourceId"`
	Title       string           `json:"title"`
	Transport   ReleaseTransport `json:"transport"`
	Fansub      string           `json:"fansub,omitempty"`
	SizeBytes   *int64           `json:"sizeBytes,omitempty"`
	Seeders     *int64           `json:"seeders,omitempty"`
	PublishedAt string           `json:"publishedAt,omitempty"`
	Episode     string           `json:"episode,omitempty"`
}

type ReleaseTransport struct {
	Type       string `json:"type"`
	Magnet     string `json:"magnet,omitempty"`
	InfoHash   string `json:"infoHash,omitempty"`
	TorrentURL string `json:"torrentUrl,omitempty"`
}

// ReleaseOutcome 是一个来源的一次整部作品搜索。Err 非空表示至少一个标题请求失败
// 或撞到来源 deadline；Releases 仍保留已经拿到的部分（部分结果）。
type ReleaseOutcome struct {
	Releases []Release
	Cached   bool
	Err      error
}

// ReleaseSearcher 由支持整部作品搜索的 Runner 实现（BT 来源）。
type ReleaseSearcher interface {
	Releases(ctx context.Context, titles []string) ReleaseOutcome
}

// NormalizeReleaseTitles 去首尾空白、丢空串、按大小写不敏感去重（保留第一次出现的写法），
// 最多保留 4 个。标题原样作为关键词，不做 searchTitles 的变体扩展。
func NormalizeReleaseTitles(titles []string) []string {
	result := make([]string, 0, min(len(titles), releaseTitleLimit))
	seen := map[string]bool{}
	for _, title := range titles {
		title = strings.TrimSpace(title)
		key := strings.ToLower(title)
		if title == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, title)
		if len(result) == releaseTitleLimit {
			break
		}
	}
	return result
}

func (runner *SpecRunner) Releases(ctx context.Context, titles []string) ReleaseOutcome {
	source := runner.Source()
	if !source.Enabled {
		return ReleaseOutcome{Err: sourceError("source_disabled", "source is disabled", false, nil)}
	}
	if source.Kind != "bt" {
		return ReleaseOutcome{Err: sourceError("unsupported_rule", "source kind does not support release search", false, nil)}
	}
	titles = NormalizeReleaseTitles(titles)
	if len(titles) == 0 {
		return ReleaseOutcome{Err: sourceError("invalid_request", "request has no usable title", false, nil)}
	}
	key := releaseCacheKey(source.ID, titles)
	if releases, ok := runner.releaseCache.get(key, runner.now()); ok {
		return ReleaseOutcome{Releases: releases, Cached: true}
	}
	// deadline 从进入来源算起，排队等并发槽位的时间也计入。
	searchContext, cancel := context.WithTimeout(ctx, runner.releaseDeadline())
	defer cancel()
	select {
	case runner.semaphore <- struct{}{}:
		defer func() { <-runner.semaphore }()
	case <-searchContext.Done():
		return ReleaseOutcome{Err: contextError(searchContext.Err(), "search")}
	}
	// 排队期间同一请求可能已经被前一个搜索填进缓存（重复打开选集窗口）。
	if releases, ok := runner.releaseCache.get(key, runner.now()); ok {
		return ReleaseOutcome{Releases: releases, Cached: true}
	}
	records, err := runner.crawlReleaseTitles(searchContext, titles)
	releases := releasesFromRecords(records)
	if ttl := runner.releaseCacheTTL(len(releases) == 0); err == nil && ttl > 0 {
		runner.releaseCache.put(key, releases, runner.now(), ttl)
	}
	return ReleaseOutcome{Releases: releases, Err: err}
}

// releaseCacheTTL 有结果 1 小时、零结果 5 分钟；规则声明了 cache.ttl_ms 时以它为上限，0 表示不缓存。
func (runner *SpecRunner) releaseCacheTTL(empty bool) time.Duration {
	ttl := releaseCacheTTL
	if empty {
		ttl = releaseCacheEmptyTTL
	}
	if value, declared := object(runner.source["cache"])["ttl_ms"]; declared {
		if limit := time.Duration(integer(value, 0)) * time.Millisecond; limit < ttl {
			ttl = limit
		}
	}
	return ttl
}

func (runner *SpecRunner) releaseDeadline() time.Duration {
	timeout := time.Duration(integer(object(runner.source["limits"])["timeout_ms"], 0)) * time.Millisecond
	if timeout <= 0 || timeout > releaseSearchDeadline {
		return releaseSearchDeadline
	}
	return timeout
}

type titleOutcome struct {
	index   int
	records []btindex.Record
	err     error
}

// crawlReleaseTitles 每个标题一个请求（第 1 页），全部并发、由来源的 rateGate 隔开。
// deadline 到了不再等：已经返回的标题照常合并，没返回的记为超时。
func (runner *SpecRunner) crawlReleaseTitles(ctx context.Context, titles []string) ([]btindex.Record, error) {
	fetcher := limitedFetcher{base: runner.fetcher, gate: &runner.rate}
	outcomes := make(chan titleOutcome, len(titles))
	for index, title := range titles {
		go func(index int, title string) {
			outcomes <- runner.crawlReleaseTitle(ctx, index, title, fetcher)
		}(index, title)
	}
	collected := make([]*titleOutcome, len(titles))
	for received := 0; received < len(titles); received++ {
		select {
		case outcome := <-outcomes:
			collected[outcome.index] = &outcome
		case <-ctx.Done():
			drainTitleOutcomes(outcomes, collected)
			return mergeTitleOutcomes(ctx, collected)
		}
	}
	return mergeTitleOutcomes(ctx, collected)
}

// crawlReleaseTitle 抓一个标题。有条目却一条记录都抽不出来，说明规则已经对不上站点：
// 报失败而不是零结果（零 ≠ 死），也就不会被当成零结果缓存。解析的是远端内容，panic 在这里兜住，
// 否则这个 goroutine 会带走整个插件进程。
func (runner *SpecRunner) crawlReleaseTitle(ctx context.Context, index int, title string, fetcher btcrawler.Fetcher) (outcome titleOutcome) {
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome = titleOutcome{index: index, err: sourceError("search_failed", "source runtime panicked", true, nil)}
		}
	}()
	query := btcrawler.Query{Title: title, Page: 1, Unfiltered: true}
	result, err := btcrawler.Crawl(ctx, runner.source, query, fetcher)
	if err == nil && len(result.Records) == 0 && rejectedItems(result.Diagnostics) > 0 {
		err = sourceError("search_failed", "source response could not be extracted", true, nil)
	}
	return titleOutcome{index: index, records: result.Records, err: err}
}

func rejectedItems(diagnostics []btcrawler.Diagnostic) int {
	count := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Category == "item_extraction_failed" || diagnostic.Category == "item_rejected" {
			count++
		}
	}
	return count
}

// deadline 与最后几个结果同时就绪时 select 随机挑一个：把缓冲里已经到的收完，结果不随调度抖动。
func drainTitleOutcomes(outcomes <-chan titleOutcome, collected []*titleOutcome) {
	for {
		select {
		case outcome := <-outcomes:
			collected[outcome.index] = &outcome
		default:
			return
		}
	}
}

// mergeTitleOutcomes 按标题顺序合并、按 Record.Key() 去重；报告的错误取标题顺序上的第一个失败。
func mergeTitleOutcomes(ctx context.Context, collected []*titleOutcome) ([]btindex.Record, error) {
	var failure error
	groups := make([][]btindex.Record, 0, len(collected))
	for _, outcome := range collected {
		switch {
		case outcome == nil:
			if failure == nil {
				failure = contextError(ctx.Err(), "search")
			}
		case outcome.err != nil:
			if failure == nil {
				failure = classifyTitleError(outcome.err)
			}
		default:
			groups = append(groups, outcome.records)
		}
	}
	return mergeReleaseRecords(groups), failure
}

func classifyTitleError(err error) error {
	var typed *Error
	if errors.As(err, &typed) {
		return err
	}
	return classifyNetworkError(err, "search")
}

// mergeReleaseRecords 同一发布被多个标题搜到时：做种数已知且更高的优先，否则保留第一次出现的。
func mergeReleaseRecords(groups [][]btindex.Record) []btindex.Record {
	positions := map[string]int{}
	var merged []btindex.Record
	for _, records := range groups {
		for _, record := range records {
			key := record.Key()
			position, seen := positions[key]
			if !seen {
				positions[key] = len(merged)
				merged = append(merged, record)
				continue
			}
			if moreSeeders(record.Seeders, merged[position].Seeders) {
				merged[position] = record
			}
		}
	}
	return merged
}

func moreSeeders(candidate, current *int64) bool {
	return candidate != nil && (current == nil || *candidate > *current)
}

func releasesFromRecords(records []btindex.Record) []Release {
	releases := make([]Release, 0, len(records))
	for _, record := range records {
		releases = append(releases, releaseFromRecord(record))
	}
	sort.SliceStable(releases, func(i, j int) bool { return releaseLess(releases[i], releases[j]) })
	return releases
}

// btReleaseID 是 BT 候选与 /v1/releases 发布共用的 id：<sourceId>:<infohash>；
// 只有 .torrent 地址时用地址摘要（<sourceId>:url:<16 位十六进制>），不把地址本身塞进 id。
func btReleaseID(record btindex.Record) string {
	if record.InfoHash != "" {
		return record.SourceID + ":" + record.InfoHash
	}
	sum := sha256.Sum256([]byte(record.TorrentURL))
	return record.SourceID + ":url:" + hex.EncodeToString(sum[:8])
}

func releaseFromRecord(record btindex.Record) Release {
	release := Release{
		ID: btReleaseID(record), SourceID: record.SourceID, Title: truncateRunes(record.Title, releaseTitleMaxRunes),
		Transport: ReleaseTransport{Type: "torrent", Magnet: record.Magnet, InfoHash: record.InfoHash, TorrentURL: record.TorrentURL},
		Fansub:    record.Fansub, SizeBytes: record.SizeBytes, Seeders: record.Seeders, PublishedAt: record.PublishedAt,
	}
	if record.Episode != nil {
		release.Episode = strconv.FormatFloat(*record.Episode, 'f', -1, 64)
	}
	return release
}

// releaseLess：做种数降序（未知排后）→ 发布时间降序（未知排后）→ 标题升序 → id 升序。
// publishedAt 已由 NormalizeRecord 统一成 RFC 3339 UTC，字符串比较即时间比较。
func releaseLess(a, b Release) bool {
	if (a.Seeders == nil) != (b.Seeders == nil) {
		return a.Seeders != nil
	}
	if a.Seeders != nil && *a.Seeders != *b.Seeders {
		return *a.Seeders > *b.Seeders
	}
	if (a.PublishedAt == "") != (b.PublishedAt == "") {
		return a.PublishedAt != ""
	}
	if a.PublishedAt != b.PublishedAt {
		return a.PublishedAt > b.PublishedAt
	}
	if a.Title != b.Title {
		return a.Title < b.Title
	}
	return a.ID < b.ID
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
