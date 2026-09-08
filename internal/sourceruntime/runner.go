package sourceruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nagare-project/Nagare_Source/internal/btcrawler"
	"github.com/nagare-project/Nagare_Source/internal/btindex"
	"github.com/nagare-project/Nagare_Source/internal/webresolver"
)

type RunnerOptions struct {
	Fetcher        btcrawler.Fetcher
	FixtureFetcher btcrawler.Fetcher
	Browser        *webresolver.Runtime
}

type SpecRunner struct {
	source         map[string]any
	fetcher        btcrawler.Fetcher
	fixtureFetcher btcrawler.Fetcher
	browser        *webresolver.Runtime
	semaphore      chan struct{}
	rate           rateGate
}

type rateGate struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func NewSpecRunner(source map[string]any, options RunnerOptions) *SpecRunner {
	concurrency := integer(object(source["limits"])["concurrency"], 1)
	if concurrency < 1 {
		concurrency = 1
	}
	requestsPerMinute := integer(object(source["limits"])["requests_per_minute"], 60)
	interval := time.Duration(0)
	if requestsPerMinute > 0 {
		interval = time.Minute / time.Duration(requestsPerMinute)
	}
	if options.Fetcher == nil {
		options.Fetcher = btcrawler.HTTPFetcher{}
	}
	return &SpecRunner{
		source: source, fetcher: options.Fetcher, fixtureFetcher: options.FixtureFetcher,
		browser: options.Browser, semaphore: make(chan struct{}, concurrency), rate: rateGate{interval: interval},
	}
}

func (runner *SpecRunner) Source() Source {
	origin := object(runner.source["origin"])
	resolve := object(runner.source["resolve"])
	enabled := boolean(runner.source["enabled"], true)
	status := "healthy"
	if !enabled {
		status = "disabled"
	}
	capabilities := []string{text(runner.source["kind"]), text(resolve["mode"])}
	sort.Strings(capabilities)
	return Source{
		ID: text(runner.source["id"]), Name: text(runner.source["name"]), Kind: text(runner.source["kind"]),
		Tier: integer(runner.source["tier"], 0), Version: text(origin["version"]), Enabled: enabled,
		Status: status, Capabilities: capabilities,
	}
}

func (runner *SpecRunner) Candidates(ctx context.Context, request ResolveRequest, emit func(Candidate) error) error {
	if !runner.Source().Enabled {
		return sourceError("source_disabled", "source is disabled", false, nil)
	}
	timeout := time.Duration(integer(object(runner.source["limits"])["timeout_ms"], 15000)) * time.Millisecond
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	sourceContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case runner.semaphore <- struct{}{}:
		defer func() { <-runner.semaphore }()
	case <-sourceContext.Done():
		return contextError(sourceContext.Err(), "search")
	}
	limited := limitedFetcher{base: runner.fetcher, gate: &runner.rate}
	switch runner.Source().Kind {
	case "bt":
		return runner.btCandidates(sourceContext, request, emit, limited)
	case "web":
		return runner.webCandidates(sourceContext, request, emit, limited)
	default:
		return sourceError("unsupported_rule", "source kind is unsupported", false, nil)
	}
}

func (runner *SpecRunner) btCandidates(ctx context.Context, request ResolveRequest, emit func(Candidate) error, fetcher btcrawler.Fetcher) error {
	episode := requestedEpisode(request)
	titles := searchTitles(runner.source, request.Subject.Titles)
	if len(titles) == 0 {
		return sourceError("invalid_request", "request has no usable title", false, nil)
	}
	records := map[string]btindex.Record{}
	var lastError error
	for _, title := range titles {
		result, err := btcrawler.Crawl(ctx, runner.source, btcrawler.Query{Title: title, Episode: episode, Page: 1}, fetcher)
		if err != nil {
			lastError = err
			if ctx.Err() != nil {
				return contextError(ctx.Err(), "search")
			}
			continue
		}
		for _, record := range result.Records {
			records[record.SourceID+":"+record.InfoHash] = record
		}
		if len(records) > 0 {
			break
		}
	}
	if len(records) == 0 {
		if lastError != nil {
			return classifyNetworkError(lastError, "search")
		}
		return sourceError("episode_not_found", "source returned no matching BT release", true, nil)
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		candidate := runner.btCandidate(records[key], request)
		if err := emit(candidate); err != nil {
			return err
		}
	}
	return nil
}

func (runner *SpecRunner) btCandidate(record btindex.Record, request ResolveRequest) Candidate {
	episode := requestedEpisode(request)
	confidence := 0.85
	if record.Episode != nil && episode > 0 && *record.Episode == episode {
		confidence = 0.95
	}
	metadata := Metadata{
		Resolution: record.Resolution, SubtitleLanguages: append([]string(nil), record.SubtitleLanguages...),
		Fansub: record.Fansub, SizeBytes: record.SizeBytes, Seeders: record.Seeders, PublishedAt: record.PublishedAt,
	}
	if record.Episode != nil {
		metadata.Episode = pointer(*record.Episode)
	}
	return Candidate{
		Schema: "nagare-candidate/v1", ID: record.SourceID + ":" + record.InfoHash,
		SourceID: record.SourceID, Tier: runner.Source().Tier, MatchConfidence: confidence,
		Match:     Match{Basis: []string{"title_episode"}, SubjectTitle: firstTitle(request.Subject.Titles), EpisodeNumber: pointer(episode), Evidence: "normalized title alias and release episode matched"},
		Transport: Transport{Type: "torrent", Magnet: record.Magnet, InfoHash: record.InfoHash, TorrentURL: record.TorrentURL},
		Metadata:  metadata,
	}
}

func (runner *SpecRunner) webCandidates(ctx context.Context, request ResolveRequest, emit func(Candidate) error, fetcher btcrawler.Fetcher) error {
	initial := requestVariables(runner.source, request)
	searchStage := object(runner.source["search"])
	var subjects []map[string]string
	var lastSearchError error
	for _, title := range searchTitles(runner.source, request.Subject.Titles) {
		variables := copyStrings(initial)
		variables["title"] = title
		variables["keyword"] = title
		response, err := fetchStage(ctx, searchStage, runner.source, variables, fetcher)
		if err != nil {
			lastSearchError = err
			if ctx.Err() != nil {
				return contextError(ctx.Err(), "search")
			}
			continue
		}
		rows, err := extractSearch(searchStage, response.URL, response.Body, variables)
		if err != nil {
			lastSearchError = err
			continue
		}
		subjects = append(subjects, rows...)
		if hasExactTitle(rows, request.Subject.Titles) {
			break
		}
	}
	subject, confidence, basis := chooseSubject(subjects, request.Subject)
	if subject == nil {
		if lastSearchError != nil && len(subjects) == 0 {
			return classifyNetworkError(lastSearchError, "search")
		}
		return sourceError("no_subject_match", "source returned no reliable subject match", false, nil)
	}
	variables := copyStrings(initial)
	for key, value := range subject {
		variables[key] = value
	}
	episodeStage := object(runner.source["episodes"])
	response, err := fetchStage(ctx, episodeStage, runner.source, variables, fetcher)
	if err != nil {
		return classifyNetworkError(err, "search")
	}
	episodes, err := extractEpisodes(episodeStage, response.URL, response.Body, variables)
	if err != nil {
		return sourceError("search_failed", "source episode response could not be extracted", true, err)
	}
	wanted := requestedEpisode(request)
	matches := matchingEpisodes(episodes, wanted)
	if len(matches) == 0 {
		return sourceError("episode_not_found", "requested episode was not found", false, nil)
	}
	seenIDs := map[string]int{}
	var lastResolveError error
	emitted := 0
	for index, episode := range matches {
		candidateVariables := copyStrings(variables)
		for key, value := range episode {
			candidateVariables[key] = value
		}
		candidate, err := runner.resolveWebCandidate(ctx, request, subject, episode, candidateVariables, confidence, basis, index, fetcher)
		if err != nil {
			lastResolveError = err
			continue
		}
		seenIDs[candidate.ID]++
		if seenIDs[candidate.ID] > 1 {
			candidate.ID += ":" + strconv.Itoa(seenIDs[candidate.ID])
		}
		if err := emit(candidate); err != nil {
			return err
		}
		emitted++
	}
	if emitted == 0 {
		if lastResolveError != nil {
			return lastResolveError
		}
		return sourceError("resolve_failed", "source produced no readable media candidate", true, nil)
	}
	return nil
}

func (runner *SpecRunner) resolveWebCandidate(ctx context.Context, request ResolveRequest, subject, episode map[string]string, variables map[string]string, confidence float64, basis []string, index int, fetcher btcrawler.Fetcher) (Candidate, error) {
	resolve := object(runner.source["resolve"])
	var transport Transport
	switch text(resolve["mode"]) {
	case "direct":
		rawURL, err := render(text(resolve["url"]), func(name string) (string, error) {
			value, ok := variables[name]
			if !ok {
				return "", fmt.Errorf("unknown resolve variable %q", name)
			}
			return value, nil
		})
		if err != nil {
			return Candidate{}, sourceError("resolve_failed", "direct resolve template is invalid", false, err)
		}
		response, err := verifyDirect(ctx, runner.source, resolve, rawURL, fetcher)
		if err != nil {
			return Candidate{}, classifyNetworkError(err, "resolve")
		}
		typeName := text(resolve["transport"])
		isHLS := strings.HasPrefix(strings.TrimSpace(string(response.Body)), "#EXTM3U")
		if typeName == "auto" {
			if strings.HasSuffix(strings.ToLower(strings.Split(response.URL, "?")[0]), ".m3u8") || isHLS {
				typeName = "hls"
			} else {
				typeName = "http"
			}
		}
		if typeName == "hls" && !isHLS {
			return Candidate{}, sourceError("resolve_failed", "direct HLS response is not a readable manifest", true, nil)
		}
		if typeName == "http" && isHLS {
			return Candidate{}, sourceError("resolve_failed", "direct response conflicts with configured HTTP transport", false, nil)
		}
		transport = Transport{Type: typeName, URL: response.URL, Headers: stringMap(resolve["request_headers"])}
	case "browser_sniff":
		if runner.browser == nil {
			return Candidate{}, sourceError("browser_blocked", "browser runtime is unavailable", true, nil)
		}
		media, err := runner.browser.Resolve(ctx, runner.source, variables)
		if err != nil {
			var typed *webresolver.Error
			if errors.As(err, &typed) {
				return Candidate{}, sourceError(typed.Category, typed.Message, typed.Retryable, typed.Cause)
			}
			return Candidate{}, sourceError("browser_blocked", "browser resolver failed", true, err)
		}
		transport = Transport{Type: media.Transport, URL: media.URL, Headers: media.Headers}
	default:
		return Candidate{}, sourceError("unsupported_rule", "resolve mode is unsupported", false, nil)
	}
	episodeNumber := number(episode["number"])
	channel := firstValue(episode, "channel", "line_key")
	channelTier := integer(object(runner.source["defaults"])["channel_tier"], 0)
	if value, ok := object(runner.source["channel_tiers"])[channel]; ok {
		channelTier = integer(value, channelTier)
	}
	identifier := subjectIdentifier(request, subject)
	channelID := channel
	if channelID == "" {
		channelID = strconv.Itoa(index + 1)
	}
	return Candidate{
		Schema: "nagare-candidate/v1", ID: fmt.Sprintf("%s:%s:%s:%s", runner.Source().ID, identifier, request.Episode.Number, channelID),
		SourceID: runner.Source().ID, Tier: runner.Source().Tier, MatchConfidence: confidence,
		Match:     Match{Basis: basis, SubjectTitle: firstValue(subject, "title"), EpisodeNumber: pointer(episodeNumber), Evidence: "subject title and parsed episode number matched"},
		Transport: transport,
		Metadata: Metadata{
			Resolution:        text(object(runner.source["defaults"])["resolution"]),
			SubtitleLanguages: stringsValue(object(runner.source["defaults"])["subtitle_languages"]),
			Channel:           channel, ChannelTier: pointer(channelTier), Episode: pointer(episodeNumber),
		},
	}, nil
}

func (runner *SpecRunner) SelfCheck(ctx context.Context, mode string) (result SelfCheckResult) {
	started := time.Now()
	result = SelfCheckResult{SourceID: runner.Source().ID, Status: "unavailable"}
	defer func() { result.DurationMS = durationMilliseconds(started) }()
	selftest := object(runner.source["selftest"])
	requestDocument := object(selftest["request"])
	if len(requestDocument) == 0 {
		result.Status = "degraded"
		result.Category = "unsupported_rule"
		result.Message = "source has no self-test request"
		return result
	}
	optionsFetcher := runner.fetcher
	if mode == "fixture" {
		if runner.fixtureFetcher == nil {
			result.Status = "degraded"
			result.Category = "search_failed"
			result.Message = "source has no offline self-test fixture"
			return result
		}
		optionsFetcher = runner.fixtureFetcher
	}
	clone := NewSpecRunner(runner.source, RunnerOptions{Fetcher: optionsFetcher, FixtureFetcher: runner.fixtureFetcher, Browser: runner.browser})
	titles := stringsValue(requestDocument["titles"])
	if len(titles) == 0 && text(requestDocument["query"]) != "" {
		titles = []string{text(requestDocument["query"])}
	}
	episode := number(requestDocument["episode"])
	request := ResolveRequest{Schema: "nagare-resolve-request/v1", Subject: Subject{IDs: map[string]string{"selftest": "fixture"}, Titles: titles}, Episode: Episode{Number: strconv.FormatFloat(episode, 'f', -1, 64), Absolute: pointer(episode)}}
	count := 0
	expectedTransport := text(object(selftest["expect"])["transport"])
	transportMismatch := false
	err := clone.Candidates(ctx, request, func(candidate Candidate) error {
		count++
		if expectedTransport != "" && candidate.Transport.Type != expectedTransport {
			transportMismatch = true
		}
		return nil
	})
	if err != nil {
		var typed *Error
		if errors.As(err, &typed) {
			result.Category, result.Message = typed.Category, typed.Message
		} else {
			result.Category, result.Message = "search_failed", "source self-test failed"
		}
		return result
	}
	minimum := integer(object(selftest["expect"])["min_candidates"], 1)
	if count < minimum {
		result.Status = "degraded"
		result.Category = "invalid_candidate"
		result.Message = "source self-test returned too few candidates"
		return result
	}
	if transportMismatch {
		result.Status = "degraded"
		result.Category = "invalid_candidate"
		result.Message = "source self-test returned an unexpected transport"
		return result
	}
	result.Status = "healthy"
	return result
}

type limitedFetcher struct {
	base btcrawler.Fetcher
	gate *rateGate
}

func (fetcher limitedFetcher) Fetch(ctx context.Context, request btcrawler.Request) (btcrawler.Response, error) {
	if err := fetcher.gate.Wait(ctx); err != nil {
		return btcrawler.Response{}, err
	}
	return fetcher.base.Fetch(ctx, request)
}

func (gate *rateGate) Wait(ctx context.Context) error {
	gate.mu.Lock()
	now := time.Now()
	when := gate.next
	if when.Before(now) {
		when = now
	}
	gate.next = when.Add(gate.interval)
	gate.mu.Unlock()
	if delay := time.Until(when); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func fetchStage(ctx context.Context, stage, source map[string]any, variables map[string]string, fetcher btcrawler.Fetcher) (btcrawler.Response, error) {
	request, err := stageRequest(stage, source, variables)
	if err != nil {
		return btcrawler.Response{}, err
	}
	return fetcher.Fetch(ctx, request)
}

func stageRequest(stage, source map[string]any, variables map[string]string) (btcrawler.Request, error) {
	document := object(stage["request"])
	rawURL, err := renderRequestURL(text(document["url"]), variables)
	if err != nil {
		return btcrawler.Request{}, err
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return btcrawler.Request{}, err
	}
	query := parsed.Query()
	for _, key := range sortedKeys(object(document["query"])) {
		value, err := renderValue(fmt.Sprint(object(document["query"])[key]), variables)
		if err != nil {
			return btcrawler.Request{}, err
		}
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
	headers := http.Header{}
	for _, key := range sortedKeys(object(document["headers"])) {
		value, err := renderValue(fmt.Sprint(object(document["headers"])[key]), variables)
		if err != nil {
			return btcrawler.Request{}, err
		}
		headers.Set(key, value)
	}
	var body []byte
	if value := text(document["body"]); value != "" {
		var rendered string
		var err error
		if strings.Contains(strings.ToLower(headers.Get("Content-Type")), "application/json") {
			rendered, err = render(value, func(name string) (string, error) {
				variable, ok := variables[name]
				if !ok {
					return "", fmt.Errorf("unknown template variable %q", name)
				}
				encoded, marshalErr := json.Marshal(variable)
				if marshalErr != nil {
					return "", marshalErr
				}
				return string(encoded[1 : len(encoded)-1]), nil
			})
		} else {
			rendered, err = renderValue(value, variables)
		}
		if err != nil {
			return btcrawler.Request{}, err
		}
		if strings.Contains(strings.ToLower(headers.Get("Content-Type")), "application/json") && !json.Valid([]byte(rendered)) {
			return btcrawler.Request{}, errors.New("rendered request body is not valid JSON")
		}
		body = []byte(rendered)
	}
	if form := object(document["form"]); len(form) > 0 {
		values := url.Values{}
		for _, key := range sortedKeys(form) {
			value, err := renderValue(fmt.Sprint(form[key]), variables)
			if err != nil {
				return btcrawler.Request{}, err
			}
			values.Set(key, value)
		}
		body = []byte(values.Encode())
		headers.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	limits := object(source["limits"])
	return btcrawler.Request{
		Method: text(document["method"]), URL: parsed.String(), Headers: headers, Body: body,
		AllowedHosts: stringsValue(document["allowed_hosts"]),
		TimeoutMS:    integer(document["timeout_ms"], integer(limits["timeout_ms"], 15000)),
		MaxBytes:     int64(integer(document["max_bytes"], integer(limits["max_response_bytes"], 5*1024*1024))),
		MaxRedirects: integer(document["max_redirects"], integer(limits["max_redirects"], 3)),
		ResponseType: text(stage["response"]),
	}, nil
}

func verifyDirect(ctx context.Context, source, resolve map[string]any, rawURL string, fetcher btcrawler.Fetcher) (btcrawler.Response, error) {
	limits := object(source["limits"])
	maximum := integer(limits["max_response_bytes"], 64*1024)
	if maximum > 64*1024 {
		maximum = 64 * 1024
	}
	request := btcrawler.Request{
		Method: http.MethodGet, URL: rawURL, Headers: http.Header{},
		AllowedHosts: stringsValue(resolve["allowed_hosts"]), TimeoutMS: integer(resolve["timeout_ms"], integer(limits["timeout_ms"], 15000)),
		MaxBytes: int64(maximum), MaxRedirects: integer(limits["max_redirects"], 3),
	}
	if text(resolve["transport"]) != "hls" {
		request.Headers.Set("Range", "bytes=0-65535")
	}
	for key, value := range stringMap(resolve["request_headers"]) {
		request.Headers.Set(key, value)
	}
	response, err := fetcher.Fetch(ctx, request)
	if err != nil {
		return btcrawler.Response{}, err
	}
	return response, nil
}

func renderRequestURL(template string, variables map[string]string) (string, error) {
	trimmed := strings.TrimSpace(template)
	if strings.HasPrefix(trimmed, "{{") && strings.HasSuffix(trimmed, "}}") && strings.Count(trimmed, "{{") == 1 {
		name := strings.TrimSpace(trimmed[2 : len(trimmed)-2])
		value, ok := variables[name]
		if !ok {
			return "", fmt.Errorf("unknown URL variable %q", name)
		}
		return value, nil
	}
	return render(template, func(name string) (string, error) {
		value, ok := variables[name]
		if !ok {
			return "", fmt.Errorf("unknown URL variable %q", name)
		}
		return url.QueryEscape(value), nil
	})
}

func renderValue(template string, variables map[string]string) (string, error) {
	return render(template, func(name string) (string, error) {
		value, ok := variables[name]
		if !ok {
			return "", fmt.Errorf("unknown template variable %q", name)
		}
		return value, nil
	})
}

func requestVariables(source map[string]any, request ResolveRequest) map[string]string {
	episode := requestedEpisode(request)
	variables := map[string]string{
		"source": text(source["id"]), "episode": request.Episode.Number, "episode_number": request.Episode.Number,
		"page": "1", "title": firstTitle(request.Subject.Titles), "keyword": firstTitle(request.Subject.Titles),
	}
	if episode > 0 {
		variables["episode"] = strconv.FormatFloat(episode, 'f', -1, 64)
		variables["episode_number"] = variables["episode"]
	}
	return variables
}

func chooseSubject(rows []map[string]string, subject Subject) (map[string]string, float64, []string) {
	aliases := make([]string, 0, len(subject.Titles))
	for _, title := range subject.Titles {
		if normalized := btindex.NormalizeTitle(title); normalized != "" {
			aliases = append(aliases, normalized)
		}
	}
	var best map[string]string
	bestScore := 0.0
	basis := []string{"title_episode"}
	for _, row := range rows {
		key := firstValue(row, "subject_key", "subject_id")
		for _, identifier := range subject.IDs {
			if key != "" && key == identifier && bestScore < 1 {
				best, bestScore, basis = row, 1, []string{"subject_id", "title_episode"}
			}
		}
		normalized := btindex.NormalizeTitle(firstValue(row, "title", "name"))
		if normalized == "" {
			continue
		}
		for _, alias := range aliases {
			score := 0.0
			switch {
			case normalized == alias:
				score = 1
			case strings.Contains(normalized, alias) || strings.Contains(alias, normalized):
				score = 0.9
			}
			if score > bestScore {
				best, bestScore, basis = row, score, []string{"title_episode"}
			}
		}
	}
	if bestScore < 0.8 {
		return nil, 0, nil
	}
	return best, bestScore, basis
}

func matchingEpisodes(rows []map[string]string, wanted float64) []map[string]string {
	var result []map[string]string
	for _, row := range rows {
		if episode := number(row["number"]); episode > 0 && episode == wanted {
			result = append(result, row)
		}
	}
	return result
}

func hasExactTitle(rows []map[string]string, titles []string) bool {
	for _, row := range rows {
		value := btindex.NormalizeTitle(firstValue(row, "title", "name"))
		for _, title := range titles {
			if value != "" && value == btindex.NormalizeTitle(title) {
				return true
			}
		}
	}
	return false
}

func searchTitles(source map[string]any, titles []string) []string {
	matching := object(source["matching"])
	limit := integer(matching["search_title_limit"], len(titles))
	if limit < 1 || limit > len(titles) {
		limit = len(titles)
	}
	result := make([]string, 0, limit)
	seen := map[string]bool{}
	for _, title := range titles {
		title = strings.TrimSpace(title)
		if boolean(matching["search_remove_special"], false) {
			title = strings.Map(func(character rune) rune {
				if unicode.IsLetter(character) || unicode.IsNumber(character) || unicode.IsSpace(character) {
					return character
				}
				return -1
			}, title)
		}
		if boolean(matching["search_first_word_only"], false) {
			if words := strings.Fields(title); len(words) > 0 {
				title = words[0]
			}
		}
		if title != "" && !seen[title] {
			seen[title] = true
			result = append(result, title)
			if len(result) == limit {
				break
			}
		}
	}
	return result
}

func requestedEpisode(request ResolveRequest) float64 {
	if request.Episode.Absolute != nil && *request.Episode.Absolute > 0 {
		return *request.Episode.Absolute
	}
	value, _ := strconv.ParseFloat(request.Episode.Number, 64)
	return value
}

func subjectIdentifier(request ResolveRequest, subject map[string]string) string {
	keys := make([]string, 0, len(request.Subject.IDs))
	for key := range request.Subject.IDs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		return request.Subject.IDs[keys[0]]
	}
	if value := firstValue(subject, "subject_key", "subject_id"); value != "" {
		return value
	}
	return btindex.NormalizeTitle(firstValue(subject, "title", "name"))
}

func firstTitle(titles []string) string {
	if len(titles) == 0 {
		return ""
	}
	return titles[0]
}

func stringMap(value any) map[string]string {
	if values, ok := value.(map[string]string); ok {
		return copyStrings(values)
	}
	values := object(value)
	result := make(map[string]string, len(values))
	for key, value := range values {
		if value, ok := value.(string); ok {
			result[key] = value
		}
	}
	return result
}

func contextError(err error, stage string) error {
	if errors.Is(err, context.Canceled) {
		return sourceError("cancelled", "source work was cancelled", true, err)
	}
	category := stage + "_timeout"
	return sourceError(category, "source exceeded its configured deadline", true, err)
}

func classifyNetworkError(err error, stage string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return contextError(err, stage)
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "outside allowed_hosts") || strings.Contains(message, "non-public") || strings.Contains(message, "redirect") || strings.Contains(message, "credentials") {
		return sourceError("unsafe_redirect", "source request crossed its network boundary", false, err)
	}
	return sourceError(stage+"_failed", "source request failed", true, err)
}
