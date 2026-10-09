package sourceruntime

import (
	"container/list"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// 有结果的整部作品搜索缓存一小时：同一部番反复打开选集窗口时不必再打一遍站点。
	releaseCacheTTL = time.Hour
	// 零结果只缓存五分钟：新番刚播出时字幕组随时会发布，不能把「暂时没有」记太久。
	releaseCacheEmptyTTL = 5 * time.Minute
	// 每个来源最多记这么多部作品，超出按最近最少使用淘汰。
	releaseCacheLimit = 256
)

// releaseCache 是单个来源的整部作品搜索缓存。只存完整成功的结果：
// 有请求失败或撞到 deadline 的部分结果不进缓存，下次照常重搜。
type releaseCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // 队首是最近使用的
	limit   int
}

type releaseCacheEntry struct {
	key      string
	releases []Release
	expires  time.Time
}

func newReleaseCache(limit int) *releaseCache {
	if limit < 1 {
		limit = 1
	}
	return &releaseCache{entries: map[string]*list.Element{}, order: list.New(), limit: limit}
}

// releaseCacheKey = 来源 id + 小写后排序的标题集合：标题顺序与大小写不同的同一请求共用一条。
// 每个标题加引号转义，标题里本身带分隔符也不会和别的标题组合撞键。
func releaseCacheKey(sourceID string, titles []string) string {
	quoted := make([]string, len(titles))
	for index, title := range titles {
		quoted[index] = strconv.Quote(strings.ToLower(title))
	}
	sort.Strings(quoted)
	return sourceID + "\x00" + strings.Join(quoted, "\x00")
}

func (cache *releaseCache) get(key string, now time.Time) ([]Release, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	element, ok := cache.entries[key]
	if !ok {
		return nil, false
	}
	entry := element.Value.(releaseCacheEntry)
	if !now.Before(entry.expires) {
		cache.order.Remove(element)
		delete(cache.entries, key)
		return nil, false
	}
	cache.order.MoveToFront(element)
	return cloneReleases(entry.releases), true
}

// put 存一条有效期 ttl 的结果，顺手清掉已过期的条目。
func (cache *releaseCache) put(key string, releases []Release, now time.Time, ttl time.Duration) {
	entry := releaseCacheEntry{key: key, releases: cloneReleases(releases), expires: now.Add(ttl)}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.sweep(now)
	if element, ok := cache.entries[key]; ok {
		element.Value = entry
		cache.order.MoveToFront(element)
		return
	}
	cache.entries[key] = cache.order.PushFront(entry)
	for cache.order.Len() > cache.limit {
		oldest := cache.order.Back()
		cache.order.Remove(oldest)
		delete(cache.entries, oldest.Value.(releaseCacheEntry).key)
	}
}

// sweep 删除 now 之前已过期的条目；调用方持锁。
func (cache *releaseCache) sweep(now time.Time) {
	for key, element := range cache.entries {
		if !now.Before(element.Value.(releaseCacheEntry).expires) {
			cache.order.Remove(element)
			delete(cache.entries, key)
		}
	}
}

// cloneReleases 连指针字段一起复制：缓存里的结果与调用方拿到的互不影响。
func cloneReleases(releases []Release) []Release {
	result := make([]Release, len(releases))
	for index, release := range releases {
		if release.SizeBytes != nil {
			release.SizeBytes = pointer(*release.SizeBytes)
		}
		if release.Seeders != nil {
			release.Seeders = pointer(*release.Seeders)
		}
		result[index] = release
	}
	return result
}
