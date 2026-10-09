package pluginapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/repository"
	"github.com/nagare-project/Nagare_Source/internal/sourceruntime"
)

// /v1/releases：按作品标题一次搜遍全部已启用 BT 来源，返回每个来源的全部发布（不按集号筛），
// 选集交给客户端在本机完成。与 /v1/candidates 并存：后者仍服务在线来源与单集 BT。

type releaseSearchRequest struct {
	Schema  string               `json:"schema"`
	Subject releaseSearchSubject `json:"subject"`
}

type releaseSearchSubject struct {
	IDs    map[string]string `json:"ids,omitempty"`
	Titles []string          `json:"titles"`
}

type releaseEvent struct {
	Event   string                `json:"event"`
	Release sourceruntime.Release `json:"release"`
}

// sourceResultEvent 每个被查询的来源恰好一条。ok / zero 带 partial 与 cached；
// failed 带 category / message / retryable。
type sourceResultEvent struct {
	Event      string `json:"event"`
	SourceID   string `json:"sourceId"`
	State      string `json:"state"`
	Count      int    `json:"count"`
	Partial    *bool  `json:"partial,omitempty"`
	Cached     *bool  `json:"cached,omitempty"`
	Category   string `json:"category,omitempty"`
	Message    string `json:"message,omitempty"`
	Retryable  *bool  `json:"retryable,omitempty"`
	DurationMS int64  `json:"durationMs"`
}

type releaseMessage struct {
	index    int
	outcome  sourceruntime.ReleaseOutcome
	duration int64
}

func (handler *Handler) postReleases(writer http.ResponseWriter, request *http.Request) {
	if !requireMethod(writer, request, http.MethodPost) {
		return
	}
	var input releaseSearchRequest
	if err := handler.decodeSchemaRequest(request, repository.ReleaseSearchRequestSchemaName, "release-search-request-v1", &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	titles := sourceruntime.NormalizeReleaseTitles(input.Subject.Titles)
	if len(titles) == 0 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "subject.titles has no usable title")
		return
	}
	flusher := beginNDJSON(writer)
	handler.streamReleases(writer, flusher, request.Context(), titles, time.Now())
}

// releaseRunners 是本次要查询的来源：全部已启用的 bt 来源。
func (handler *Handler) releaseRunners() []sourceruntime.Runner {
	var active []sourceruntime.Runner
	for _, runner := range handler.runners {
		if source := runner.Source(); source.Enabled && source.Kind == "bt" {
			active = append(active, runner)
		}
	}
	return active
}

// streamReleases 每个来源一个 goroutine；谁先结束谁先写，写入只发生在这一个循环里。
func (handler *Handler) streamReleases(writer io.Writer, flusher http.Flusher, ctx context.Context, titles []string, started time.Time) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	active := handler.releaseRunners()
	messages := make(chan releaseMessage, len(active))
	for index, runner := range active {
		go handler.searchReleases(ctx, index, runner, titles, messages)
	}
	write := newEventWriter(writer, flusher, cancel).write
	succeeded := 0
	for completed := 0; completed < len(active); completed++ {
		select {
		case <-ctx.Done():
			return
		case message := <-messages:
			state, ok := handler.writeSourceReleases(write, active[message.index].Source().ID, message)
			if !ok {
				return
			}
			if state != "failed" {
				succeeded++
			}
		}
	}
	_ = write(map[string]any{
		"event": "done", "queried": len(active), "succeeded": succeeded,
		"failed": len(active) - succeeded, "durationMs": durationMilliseconds(started, time.Now()),
	})
}

func (handler *Handler) searchReleases(ctx context.Context, index int, runner sourceruntime.Runner, titles []string, messages chan<- releaseMessage) {
	began := time.Now()
	outcome := handler.releaseOutcome(ctx, runner, titles)
	select {
	case messages <- releaseMessage{index: index, outcome: outcome, duration: durationMilliseconds(began, time.Now())}:
	case <-ctx.Done():
	}
}

func (handler *Handler) releaseOutcome(ctx context.Context, runner sourceruntime.Runner, titles []string) (outcome sourceruntime.ReleaseOutcome) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// panic 值可能带远端内容，日志只记来源 id。
			handler.logf("release search: source %s panicked", runner.Source().ID)
			outcome = sourceruntime.ReleaseOutcome{Err: sourceruntime.NewError("search_failed", true, "source runtime panicked", nil)}
		}
	}()
	searcher, ok := runner.(sourceruntime.ReleaseSearcher)
	if !ok {
		return sourceruntime.ReleaseOutcome{Err: sourceruntime.NewError("unsupported_rule", false, "source does not support release search", nil)}
	}
	return searcher.Releases(ctx, titles)
}

// writeSourceReleases 写出一个来源的全部发布再写它的 source_result；
// 不合 release-v1 的条目跳过并记日志，不连坐整个来源。
func (handler *Handler) writeSourceReleases(write func(any) bool, sourceID string, message releaseMessage) (string, bool) {
	written, skipped := 0, 0
	seen := map[string]bool{}
	for _, release := range message.outcome.Releases {
		if release.SourceID != sourceID || seen[release.ID] || handler.validateRelease(release) != nil {
			skipped++
			continue
		}
		seen[release.ID] = true
		if !write(releaseEvent{Event: "release", Release: release}) {
			return "", false
		}
		written++
	}
	if skipped > 0 {
		handler.logf("release search: source %s skipped %d invalid releases", sourceID, skipped)
	}
	event := sourceResult(sourceID, written, skipped, message)
	if !write(event) {
		return "", false
	}
	return event.State, true
}

func sourceResult(sourceID string, written, skipped int, message releaseMessage) sourceResultEvent {
	event := sourceResultEvent{Event: "source_result", SourceID: sourceID, Count: written, DurationMS: message.duration}
	err := message.outcome.Err
	switch {
	case written > 0:
		event.State = "ok"
		event.Partial, event.Cached = pointer(err != nil), pointer(message.outcome.Cached)
	case err == nil && skipped == 0:
		event.State = "zero"
		event.Partial, event.Cached = pointer(false), pointer(message.outcome.Cached)
	default:
		if err == nil {
			err = sourceruntime.NewError("invalid_candidate", false, "source emitted only invalid releases", nil)
		}
		category, text, retryable := classifyError(searchError(err))
		event.State, event.Category, event.Message, event.Retryable = "failed", category, text, pointer(retryable)
	}
	return event
}

// searchError 给未分类的错误补上搜索阶段的分类：整部作品搜索没有 resolve 阶段，
// 不能沿用 classifyError 的 resolve_failed 兜底。
func searchError(err error) error {
	var typed *sourceruntime.Error
	switch {
	case errors.As(err, &typed), errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return sourceruntime.NewError("search_timeout", true, "source exceeded its configured deadline", err)
	default:
		return sourceruntime.NewError("search_failed", true, "source search failed", err)
	}
}

func (handler *Handler) validateRelease(release sourceruntime.Release) error {
	data, err := json.Marshal(release)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return handler.validator.Validate(repository.ReleaseSchemaName, value)
}

func (handler *Handler) logf(format string, arguments ...any) {
	if handler.logger != nil {
		handler.logger(format, arguments...)
	}
}

func pointer[T any](value T) *T { return &value }
