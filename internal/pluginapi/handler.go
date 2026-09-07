// Package pluginapi implements the loopback HTTP/JSON and NDJSON surface used
// by Nagare to consume Source Spec runtimes.
package pluginapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nagare-project/Nagare_Source/internal/repository"
	"github.com/nagare-project/Nagare_Source/internal/sourceruntime"
)

const maximumRequestBytes = 1024 * 1024

type Manifest struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Version              string   `json:"version"`
	ProtocolVersions     []int    `json:"protocolVersions"`
	SourceSchemaVersions []int    `json:"sourceSchemaVersions"`
	Capabilities         []string `json:"capabilities"`
}

type Options struct {
	Root     string
	Manifest Manifest
	Runners  []sourceruntime.Runner
}

type Handler struct {
	manifest  Manifest
	runners   []sourceruntime.Runner
	validator *repository.Validator
	statuses  map[string]string
	started   time.Time
}

func New(options Options) (*Handler, error) {
	validator, err := repository.NewValidator(options.Root)
	if err != nil {
		return nil, err
	}
	statuses, err := loadSourceStatuses(options.Root, validator)
	if err != nil {
		return nil, err
	}
	manifest := options.Manifest
	if manifest.ID == "" {
		manifest.ID = "org.nagare.source.community"
	}
	if manifest.Name == "" {
		manifest.Name = "Nagare Community Sources"
	}
	if manifest.Version == "" {
		manifest.Version = "dev"
	}
	if len(manifest.ProtocolVersions) == 0 {
		manifest.ProtocolVersions = []int{1}
	}
	if len(manifest.SourceSchemaVersions) == 0 {
		manifest.SourceSchemaVersions = []int{1}
	}
	if len(manifest.Capabilities) == 0 {
		manifest.Capabilities = []string{"web", "browser_sniff", "bt", "ndjson"}
	}
	runners := append([]sourceruntime.Runner(nil), options.Runners...)
	sort.Slice(runners, func(i, j int) bool { return runners[i].Source().ID < runners[j].Source().ID })
	for index, runner := range runners {
		if runner == nil || runner.Source().ID == "" {
			return nil, errors.New("plugin runner has no source id")
		}
		if index > 0 && runner.Source().ID == runners[index-1].Source().ID {
			return nil, fmt.Errorf("duplicate plugin source id %q", runner.Source().ID)
		}
	}
	return &Handler{manifest: manifest, runners: runners, validator: validator, statuses: statuses, started: time.Now()}, nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if requestID := request.Header.Get("X-Request-ID"); requestID != "" {
		writer.Header().Set("X-Request-ID", requestID)
	}
	if !handler.protocolSupported(request.Header.Get("X-Nagare-Protocol-Version")) {
		writeAPIError(writer, http.StatusUpgradeRequired, "unsupported_protocol", "plugin does not support the requested protocol version")
		return
	}
	switch request.URL.Path {
	case "/v1/manifest":
		handler.getManifest(writer, request)
	case "/v1/sources":
		handler.getSources(writer, request)
	case "/v1/candidates":
		handler.postCandidates(writer, request)
	case "/v1/selfcheck":
		handler.postSelfCheck(writer, request)
	case "/v1/health":
		handler.getHealth(writer, request)
	default:
		writeAPIError(writer, http.StatusNotFound, "invalid_request", "endpoint does not exist")
	}
}

func (handler *Handler) protocolSupported(value string) bool {
	if strings.TrimSpace(value) == "" {
		return true
	}
	requested, err := strconv.Atoi(value)
	if err != nil {
		return false
	}
	for _, supported := range handler.manifest.ProtocolVersions {
		if supported == requested {
			return true
		}
	}
	return false
}

func (handler *Handler) getManifest(writer http.ResponseWriter, request *http.Request) {
	if !requireMethod(writer, request, http.MethodGet) {
		return
	}
	writeJSON(writer, http.StatusOK, handler.manifest)
}

func (handler *Handler) getSources(writer http.ResponseWriter, request *http.Request) {
	if !requireMethod(writer, request, http.MethodGet) {
		return
	}
	sources := make([]sourceruntime.Source, 0, len(handler.runners))
	for _, runner := range handler.runners {
		source := runner.Source()
		if source.Enabled {
			if status := handler.statuses[source.ID]; status != "" {
				source.Status = status
			}
		} else {
			source.Status = "disabled"
		}
		sources = append(sources, source)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"sources": sources})
}

func loadSourceStatuses(root string, validator *repository.Validator) (map[string]string, error) {
	value, err := repository.LoadDocument(filepath.Join(root, "reports", "health.json"))
	if err != nil {
		return nil, err
	}
	if err := validator.Validate(repository.HealthSchemaName, value); err != nil {
		return nil, err
	}
	document, _ := value.(map[string]any)
	items, _ := document["sources"].([]any)
	statuses := make(map[string]string, len(items))
	for _, item := range items {
		source, _ := item.(map[string]any)
		id, _ := source["id"].(string)
		status, _ := source["status"].(string)
		if id != "" && status != "" {
			statuses[id] = status
		}
	}
	return statuses, nil
}

func (handler *Handler) getHealth(writer http.ResponseWriter, request *http.Request) {
	if !requireMethod(writer, request, http.MethodGet) {
		return
	}
	seconds := int64(time.Since(handler.started).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok", "version": handler.manifest.Version,
		"protocolVersions": handler.manifest.ProtocolVersions, "uptimeSeconds": seconds,
	})
}

func (handler *Handler) postCandidates(writer http.ResponseWriter, request *http.Request) {
	if !requireMethod(writer, request, http.MethodPost) {
		return
	}
	var resolveRequest sourceruntime.ResolveRequest
	if err := handler.decodeResolveRequest(request, &resolveRequest); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if requestedEpisode(resolveRequest) <= 0 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "episode number must be greater than zero")
		return
	}
	writer.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	handler.streamCandidates(writer, flusher, request.Context(), resolveRequest, time.Now())
}

type candidateMessage struct {
	index     int
	candidate *sourceruntime.Candidate
	err       error
	done      bool
}

type sourceState struct {
	emitted      int
	errorEmitted bool
	done         bool
}

func (handler *Handler) streamCandidates(writer io.Writer, flusher http.Flusher, ctx context.Context, request sourceruntime.ResolveRequest, started time.Time) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var active []sourceruntime.Runner
	for _, runner := range handler.runners {
		if runner.Source().Enabled {
			active = append(active, runner)
		}
	}
	messages := make(chan candidateMessage, len(active)*2+1)
	for index, runner := range active {
		go runSource(ctx, index, runner, request, messages)
	}
	states := make([]sourceState, len(active))
	candidateIDs := map[string]bool{}
	completed := 0
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	write := func(value any) bool {
		if err := encoder.Encode(value); err != nil {
			cancel()
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	for completed < len(active) {
		select {
		case <-ctx.Done():
			return
		case message := <-messages:
			state := &states[message.index]
			sourceID := active[message.index].Source().ID
			if message.candidate != nil && !state.errorEmitted {
				candidate := *message.candidate
				if candidate.SourceID != sourceID {
					state.errorEmitted = true
					if !write(sourceErrorEvent(sourceID, sourceruntime.NewError("invalid_candidate", false, "candidate sourceId does not match its runner", nil))) {
						return
					}
				} else if err := handler.validateCandidate(candidate); err != nil {
					state.errorEmitted = true
					if !write(sourceErrorEvent(sourceID, sourceruntime.NewError("invalid_candidate", false, "source emitted an invalid candidate", err))) {
						return
					}
				} else if candidateIDs[candidate.ID] {
					state.errorEmitted = true
					if !write(sourceErrorEvent(sourceID, sourceruntime.NewError("invalid_candidate", false, "source emitted a duplicate candidate id", nil))) {
						return
					}
				} else {
					candidateIDs[candidate.ID] = true
					state.emitted++
					if !write(map[string]any{"event": "candidate", "candidate": candidate}) {
						return
					}
				}
			}
			if message.done && !state.done {
				state.done = true
				completed++
				if message.err != nil && !state.errorEmitted {
					state.errorEmitted = true
					if !write(sourceErrorEvent(sourceID, message.err)) {
						return
					}
				}
			}
		}
	}
	succeeded := 0
	for _, state := range states {
		if state.emitted > 0 {
			succeeded++
		}
	}
	_ = write(map[string]any{
		"event": "done", "queried": len(active), "succeeded": succeeded,
		"failed": len(active) - succeeded, "durationMs": durationMilliseconds(started, time.Now()),
	})
}

func runSource(ctx context.Context, index int, runner sourceruntime.Runner, request sourceruntime.ResolveRequest, messages chan<- candidateMessage) {
	defer func() {
		if recovered := recover(); recovered != nil {
			select {
			case messages <- candidateMessage{index: index, err: sourceruntime.NewError("resolve_failed", true, "source runtime panicked", nil), done: true}:
			case <-ctx.Done():
			}
		}
	}()
	err := runner.Candidates(ctx, request, func(candidate sourceruntime.Candidate) error {
		select {
		case messages <- candidateMessage{index: index, candidate: &candidate}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	select {
	case messages <- candidateMessage{index: index, err: err, done: true}:
	case <-ctx.Done():
	}
}

func (handler *Handler) validateCandidate(candidate sourceruntime.Candidate) error {
	data, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return handler.validator.Validate(repository.CandidateSchemaName, value)
}

type selfCheckRequest struct {
	SourceIDs []string `json:"sourceIds"`
	Mode      string   `json:"mode"`
}

func (handler *Handler) postSelfCheck(writer http.ResponseWriter, request *http.Request) {
	if !requireMethod(writer, request, http.MethodPost) {
		return
	}
	var input selfCheckRequest
	if err := decodeStrictJSON(request, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if input.Mode != "fixture" && input.Mode != "network" {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "selfcheck mode must be fixture or network")
		return
	}
	selected := map[string]sourceruntime.Runner{}
	for _, runner := range handler.runners {
		selected[runner.Source().ID] = runner
	}
	if len(input.SourceIDs) == 0 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "selfcheck sourceIds must not be empty")
		return
	}
	ids := append([]string(nil), input.SourceIDs...)
	sort.Strings(ids)
	for index := 1; index < len(ids); index++ {
		if ids[index] == ids[index-1] {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", fmt.Sprintf("duplicate source id %q", ids[index]))
			return
		}
	}
	results := make([]sourceruntime.SelfCheckResult, 0, len(ids))
	for _, sourceID := range ids {
		runner, ok := selected[sourceID]
		if !ok {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request", fmt.Sprintf("unknown source id %q", sourceID))
			return
		}
		if !runner.Source().Enabled {
			results = append(results, sourceruntime.SelfCheckResult{SourceID: sourceID, Status: "disabled", Category: "source_disabled", Message: "source is disabled"})
			continue
		}
		results = append(results, runner.SelfCheck(request.Context(), input.Mode))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"results": results})
}

func (handler *Handler) decodeResolveRequest(request *http.Request, output *sourceruntime.ResolveRequest) error {
	value, data, err := decodeJSONValue(request)
	if err != nil {
		return err
	}
	if err := handler.validator.Validate(repository.RequestSchemaName, value); err != nil {
		return errors.New("request does not satisfy resolve-request-v1")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(output)
}

func decodeStrictJSON(request *http.Request, output any) error {
	_, data, err := decodeJSONValue(request)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return errors.New("request body contains unsupported fields or values")
	}
	return nil
}

func decodeJSONValue(request *http.Request) (any, []byte, error) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, nil, errors.New("Content-Type must be application/json")
	}
	body := io.LimitReader(request.Body, maximumRequestBytes+1)
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, errors.New("request body is unreadable")
	}
	if len(data) > maximumRequestBytes {
		return nil, nil, errors.New("request body is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, errors.New("request body must be one JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("request body must contain exactly one JSON value")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, nil, errors.New("request body must be a JSON object")
	}
	return value, data, nil
}

func sourceErrorEvent(sourceID string, err error) map[string]any {
	category, message, retryable := "resolve_failed", "source runtime failed", true
	var typed *sourceruntime.Error
	if errors.As(err, &typed) {
		category, message, retryable = typed.Category, typed.Message, typed.Retryable
	} else if errors.Is(err, context.Canceled) {
		category, message = "cancelled", "source work was cancelled"
	}
	if strings.TrimSpace(message) == "" {
		message = "source runtime failed"
	}
	return map[string]any{"event": "source_error", "sourceId": sourceID, "category": category, "message": message, "retryable": retryable}
}

func requestedEpisode(request sourceruntime.ResolveRequest) float64 {
	if request.Episode.Absolute != nil {
		return *request.Episode.Absolute
	}
	value, _ := strconv.ParseFloat(request.Episode.Number, 64)
	return value
}

func durationMilliseconds(started, ended time.Time) int64 {
	value := ended.Sub(started).Milliseconds()
	if value < 0 {
		return 0
	}
	return value
}

func requireMethod(writer http.ResponseWriter, request *http.Request, method string) bool {
	if request.Method == method {
		return true
	}
	writer.Header().Set("Allow", method)
	writeAPIError(writer, http.StatusMethodNotAllowed, "invalid_request", "HTTP method is not allowed")
	return false
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeAPIError(writer http.ResponseWriter, status int, category, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]any{"category": category, "message": message}})
}
