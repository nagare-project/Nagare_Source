package importer

import (
	"fmt"
	"regexp"
	"strings"
)

const defaultVideoPattern = `(?i)(?:https?://[^\s"']+\.(?:m3u8|mp4|mkv)(?:[?#][^\s"']*)?|https?://[^\s"']*(?:akamaized|bilivideo\.com)[^\s"']*)`
const defaultNestedPattern = `(?i)https?://[^\s"']*(?:m3u8|vip|xigua\.php)[^\s"']*`

func ImportAnimeko(data []byte, options Options) Result {
	var result Result
	if err := ValidateOptions(options); err != nil {
		result.add("error", "invalid_options", "", "", err.Error())
		return result
	}
	var root any
	if err := decodeJSON(data, &root); err != nil {
		result.add("error", "invalid_upstream", "", "", "decode Animeko JSON: "+err.Error())
		return result
	}
	var entries []any
	switch typed := root.(type) {
	case map[string]any:
		if mediaSources, ok := typed["mediaSources"].([]any); ok {
			entries = mediaSources
		} else if _, ok := typed["factoryId"]; ok {
			entries = []any{typed}
		} else {
			result.add("error", "invalid_upstream", "", "", "expected mediaSources array or a factoryId object")
			return result
		}
	default:
		result.add("error", "invalid_upstream", "", "", "Animeko subscription root must be an object")
		return result
	}
	if len(entries) == 0 {
		result.add("error", "invalid_upstream", "", "mediaSources", "subscription contains no media sources")
		return result
	}

	seen := make(map[string]bool)
	for index, entryValue := range entries {
		entry, ok := entryValue.(map[string]any)
		path := fmt.Sprintf("mediaSources[%d]", index)
		if !ok {
			result.add("error", "invalid_source", path, path, "media source must be an object")
			continue
		}
		arguments := objectValue(entry["arguments"])
		name := strings.TrimSpace(stringValue(arguments["name"]))
		if name == "" {
			result.add("error", "invalid_source", path, path+".arguments.name", "source name is required")
			continue
		}
		canonical := canonicalJSON(entry)
		id, lossy := stableID(name, canonical)
		if seen[id] {
			id, _ = stableID(name+"-"+fmt.Sprint(index+1), canonical)
		}
		seen[id] = true
		if lossy {
			result.add("warning", "id_normalized", id, path+".arguments.name", "source name required a digest-suffixed ASCII id")
		}

		factoryID := stringValue(entry["factoryId"])
		version := intValue(entry["version"], 0)
		var source map[string]any
		var diagnostics []Diagnostic
		switch factoryID {
		case "rss":
			if version != 1 {
				result.add("error", "unsupported_rule", id, path+".version", fmt.Sprintf("rss version %d is unsupported; expected 1", version))
				continue
			}
			source, diagnostics = importAnimekoRSS(id, entry, arguments, canonical, options)
		case "web-selector":
			if version != 2 {
				result.add("error", "unsupported_rule", id, path+".version", fmt.Sprintf("web-selector version %d is unsupported; expected 2", version))
				continue
			}
			source, diagnostics = importAnimekoWeb(id, entry, arguments, canonical, options)
		default:
			result.add("error", "unsupported_factory", id, path+".factoryId", fmt.Sprintf("Animeko factory %q is unsupported", factoryID))
			continue
		}
		for _, diagnostic := range diagnostics {
			if diagnostic.Source == "" {
				diagnostic.Source = id
			}
			if diagnostic.Path != "" {
				diagnostic.Path = path + "." + diagnostic.Path
			}
			result.Diagnostics = append(result.Diagnostics, diagnostic)
		}
		if source != nil && !hasSourceError(result.Diagnostics, id) {
			result.Sources = append(result.Sources, source)
		}
	}
	return result
}

func importAnimekoRSS(id string, entry, arguments map[string]any, canonical []byte, options Options) (map[string]any, []Diagnostic) {
	searchConfig := objectValue(arguments["searchConfig"])
	searchURL := convertAnimekoTemplate(stringValue(searchConfig["searchUrl"]))
	if searchURL == "" {
		return nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "arguments.searchConfig.searchUrl", Message: "searchUrl is required"}}
	}
	host, err := hostFromTemplate(searchURL)
	if err != nil {
		return nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "arguments.searchConfig.searchUrl", Message: err.Error()}}
	}
	tier, tierDiagnostic := normalizedTier(arguments["tier"], 2)
	diagnostics := append([]Diagnostic(nil), tierDiagnostic...)
	name := stringValue(arguments["name"])
	source := animekoBase(id, name, "bt", tier, entry, arguments, canonical, options)
	source["homepage"] = baseURL(searchURL)
	source["search"] = map[string]any{
		"request": map[string]any{
			"method":        "GET",
			"url":           searchURL,
			"allowed_hosts": []any{host},
		},
		"response": "rss",
		"items":    extractor("xpath", "/rss/channel/item", "", nil, ""),
		"fields": map[string]any{
			"title": extractor("xpath", "./title/text()", "", []any{"trim"}, ""),
			"download_url": fallbackExtractor([]any{
				extractor("xpath", "./enclosure/@url", "", []any{"trim"}, ""),
				extractor("xpath", "./link/text()", "", []any{"trim"}, ""),
			}, nil, true),
			"size_bytes": optionalExtractor(extractor("xpath", "./enclosure/@length", "", []any{
				map[string]any{"type": "parse_size", "unit": "B"},
			}, "")),
			"published_at": optionalExtractor(extractor("xpath", "./pubDate/text()", "", []any{
				map[string]any{"type": "parse_datetime", "unit": "auto"},
			}, "")),
			"fansub":  optionalExtractor(extractor("field", "title", "", []any{"parse_fansub"}, "")),
			"episode": optionalExtractor(extractor("field", "title", "", []any{"parse_episode"}, "")),
			"info_hash": optionalExtractor(extractor("field", "download_url", "", []any{
				map[string]any{"type": "regex", "pattern": `(?i)btih:([a-f0-9]{40}|[a-z2-7]{32})`, "group": 1},
				"normalize_infohash",
			}, "")),
		},
	}
	source["resolve"] = map[string]any{
		"mode":      "direct",
		"transport": "torrent",
		"url":       "{{download_url}}",
	}
	source["defaults"] = map[string]any{
		"resolution":         "1080P",
		"subtitle_languages": []any{},
	}
	source["matching"] = map[string]any{
		"require_episode": boolValue(searchConfig["filterByEpisodeSort"], true),
		"require_subject": boolValue(searchConfig["filterBySubjectName"], true),
	}
	source["ranking"] = map[string]any{"has_seeders": false, "priority": 0}
	source["limits"] = defaultLimits(1, 12, 15000)
	diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "selftest_missing", Path: "arguments", Message: "Animeko subscriptions do not carry a stable self-test query; add one with an overlay"})
	return source, diagnostics
}

func importAnimekoWeb(id string, entry, arguments map[string]any, canonical []byte, options Options) (map[string]any, []Diagnostic) {
	config := objectValue(arguments["searchConfig"])
	searchURL := convertAnimekoTemplate(stringValue(config["searchUrl"]))
	if searchURL == "" {
		return nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "arguments.searchConfig.searchUrl", Message: "searchUrl is required"}}
	}
	host, err := hostFromTemplate(searchURL)
	if err != nil {
		return nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "arguments.searchConfig.searchUrl", Message: err.Error()}}
	}
	tier, diagnostics := normalizedTier(arguments["tier"], 2)
	name := stringValue(arguments["name"])
	source := animekoBase(id, name, "web", tier, entry, arguments, canonical, options)
	rawBaseURL := stringValue(config["rawBaseUrl"])
	if rawBaseURL == "" {
		rawBaseURL = baseURL(searchURL)
	}
	source["homepage"] = rawBaseURL

	search, subjectDiagnostics := animekoSubjectSearch(config, searchURL, rawBaseURL, host)
	diagnostics = append(diagnostics, subjectDiagnostics...)
	if search == nil {
		return nil, diagnostics
	}
	source["search"] = search

	episodes, episodeDiagnostics := animekoEpisodes(config, host)
	diagnostics = append(diagnostics, episodeDiagnostics...)
	if episodes == nil {
		return nil, diagnostics
	}
	source["episodes"] = episodes

	matchVideo := objectValue(config["matchVideo"])
	videoPattern := stringValue(matchVideo["matchVideoUrl"])
	if videoPattern == "" {
		videoPattern = `(^http(s)?:\/\/(?!.*http(s)?:\/\/).+((\.mp4)|(\.mkv)|(m3u8)).*(\?.+)?)|(akamaized)|(bilivideo.com)`
	}
	videoPattern, videoCapture, rewritten := compatibleRegex(videoPattern, defaultVideoPattern)
	if rewritten {
		diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "regex_rewritten", Path: "arguments.searchConfig.matchVideo.matchVideoUrl", Message: "Kotlin regex was not RE2-compatible; replaced with a conservative media URL matcher"})
	}
	match := map[string]any{"include": []any{videoPattern}}
	if videoCapture != "" {
		match["capture_group"] = videoCapture
	}
	if boolValue(matchVideo["enableNestedUrl"], true) {
		nested := stringValue(matchVideo["matchNestedUrl"])
		if nested == "" {
			nested = `^.+(m3u8|vip|xigua\.php).+\?`
		}
		nested, _, nestedRewritten := compatibleRegex(nested, defaultNestedPattern)
		if nestedRewritten {
			diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "regex_rewritten", Path: "arguments.searchConfig.matchVideo.matchNestedUrl", Message: "nested URL regex was not RE2-compatible and was conservatively rewritten"})
		}
		match["nested_include"] = []any{nested}
	}
	resolve := map[string]any{
		"mode":          "browser_sniff",
		"transport":     "auto",
		"url":           "{{play_url}}",
		"match":         match,
		"allowed_hosts": []any{host},
		"timeout_ms":    15000,
	}
	cookies := parseCookies(stringValue(matchVideo["cookies"]))
	if len(cookies) > 0 {
		resolve["cookie_policy"] = "source"
		resolve["cookies"] = cookies
	}
	headersConfig := objectValue(matchVideo["addHeadersToVideo"])
	headers := map[string]any{}
	if referer := stringValue(headersConfig["referer"]); referer != "" {
		headers["Referer"] = referer
	}
	if userAgent := stringValue(headersConfig["userAgent"]); userAgent != "" {
		headers["User-Agent"] = userAgent
	}
	if len(headers) > 0 {
		resolve["request_headers"] = headers
	}
	source["resolve"] = resolve

	resolution := stringValue(config["defaultResolution"])
	if !validResolution(resolution) {
		if resolution != "" {
			diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "default_normalized", Path: "arguments.searchConfig.defaultResolution", Message: "unsupported resolution defaulted to 1080P"})
		}
		resolution = "1080P"
	}
	language, recognizedLanguage := subtitleLanguage(stringValue(config["defaultSubtitleLanguage"]))
	if !recognizedLanguage {
		diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "default_normalized", Path: "arguments.searchConfig.defaultSubtitleLanguage", Message: "unknown subtitle language was omitted"})
	}
	languages := []any{}
	if language != "" {
		languages = append(languages, language)
	}
	source["defaults"] = map[string]any{"resolution": resolution, "subtitle_languages": languages}

	channelTiers := objectValue(arguments["channelTiers"])
	if len(channelTiers) > 0 {
		normalized := make(map[string]any, len(channelTiers))
		for channel, value := range channelTiers {
			tier := intValue(value, 0)
			if tier < 0 {
				tier = 0
			}
			if tier > 99 {
				tier = 99
			}
			normalized[channel] = tier
		}
		source["channel_tiers"] = normalized
	}
	selectMedia := objectValue(config["selectMedia"])
	titleLimit := intValue(config["searchUseSubjectNamesCount"], 1)
	if titleLimit < 1 {
		titleLimit = 1
	}
	if titleLimit > 100 {
		titleLimit = 100
	}
	source["matching"] = map[string]any{
		"search_first_word_only": boolValue(config["searchUseOnlyFirstWord"], true),
		"search_remove_special":  boolValue(config["searchRemoveSpecial"], true),
		"search_title_limit":     titleLimit,
		"require_episode":        boolValue(config["filterByEpisodeSort"], true),
		"require_subject":        boolValue(config["filterBySubjectName"], true),
		"distinguish_subject":    boolValue(selectMedia["distinguishSubjectName"], true),
		"distinguish_channel":    boolValue(selectMedia["distinguishChannelName"], true),
	}
	interval := intValue(config["requestInterval"], 3000)
	requestsPerMinute := 600
	if interval > 0 {
		requestsPerMinute = 60000 / interval
		if requestsPerMinute < 1 {
			requestsPerMinute = 1
		}
		if requestsPerMinute > 600 {
			requestsPerMinute = 600
		}
	}
	source["limits"] = defaultLimits(1, requestsPerMinute, 15000)
	cacheTTL := intValue(config["searchCacheTtl"], 7200000)
	if cacheTTL >= 0 {
		if cacheTTL > 2592000000 {
			cacheTTL = 2592000000
			diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "limit_clamped", Path: "arguments.searchConfig.searchCacheTtl", Message: "cache TTL was clamped to 30 days"})
		}
		source["cache"] = map[string]any{"ttl_ms": cacheTTL}
	}
	if players := arrayValue(config["onlySupportsPlayers"]); len(players) > 0 {
		diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "player_restriction_dropped", Path: "arguments.searchConfig.onlySupportsPlayers", Message: "Animeko player IDs are not portable; transport validation replaces this restriction"})
	}
	diagnostics = append(diagnostics,
		Diagnostic{Severity: "warning", Category: "overlay_required", Path: "arguments.searchConfig.matchVideo", Message: "browser_sniff allowlist contains only the site host; review and add required CDN hosts with an overlay"},
		Diagnostic{Severity: "warning", Category: "selftest_missing", Path: "arguments", Message: "Animeko subscriptions do not carry a stable self-test title; add one with an overlay"},
	)
	return source, diagnostics
}

func animekoBase(id, name, kind string, tier int, entry, arguments map[string]any, canonical []byte, options Options) map[string]any {
	source := map[string]any{
		"schema":  "nagare-source/v1",
		"id":      id,
		"name":    name,
		"kind":    kind,
		"tier":    tier,
		"enabled": true,
		"origin":  sourceOrigin(options, "animeko", stringValue(entry["version"]), canonical),
	}
	if description := stringValue(arguments["description"]); description != "" {
		source["description"] = description
	}
	if icon := stringValue(arguments["iconUrl"]); strings.HasPrefix(icon, "http://") || strings.HasPrefix(icon, "https://") {
		source["icon"] = icon
	}
	return source
}

func animekoSubjectSearch(config map[string]any, searchURL, rawBaseURL, host string) (map[string]any, []Diagnostic) {
	stage := map[string]any{
		"request":  map[string]any{"method": "GET", "url": searchURL, "allowed_hosts": []any{host}},
		"base_url": rawBaseURL,
	}
	formatID := stringValue(config["subjectFormatId"])
	if formatID == "" {
		formatID = "a"
	}
	switch formatID {
	case "a":
		format := objectValue(config["selectorSubjectFormatA"])
		selector := stringValue(format["selectLists"])
		if selector == "" {
			selector = "div.video-info-header > a"
		}
		stage["response"] = "html"
		stage["items"] = extractor("css", selector, "", nil, "")
		stage["fields"] = map[string]any{
			"title": fallbackExtractor([]any{
				extractor("css", ":scope", "title", []any{"trim"}, ""),
				extractor("css", ":scope", "", []any{"trim"}, ""),
			}, nil, true),
			"subject_url": extractor("css", ":scope", "href", []any{"absolute_url"}, ""),
		}
	case "indexed":
		format := objectValue(config["selectorSubjectFormatIndexed"])
		names := stringValue(format["selectNames"])
		links := stringValue(format["selectLinks"])
		if names == "" || links == "" {
			return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "arguments.searchConfig.selectorSubjectFormatIndexed", Message: "indexed subject format requires selectNames and selectLinks"}}
		}
		stage["response"] = "html"
		stage["mode"] = "zipped"
		stage["fields"] = map[string]any{
			"title":       extractor("css", names, "", []any{"trim"}, "document"),
			"subject_url": extractor("css", links, "href", []any{"absolute_url"}, "document"),
		}
	case "json-path-indexed":
		format := objectValue(config["selectorSubjectFormatJsonPathIndexed"])
		names := stringValue(format["selectNames"])
		links := stringValue(format["selectLinks"])
		if names == "" || links == "" {
			return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "arguments.searchConfig.selectorSubjectFormatJsonPathIndexed", Message: "JSONPath subject format requires selectNames and selectLinks"}}
		}
		stage["response"] = "json"
		stage["mode"] = "zipped"
		stage["fields"] = map[string]any{
			"title":       extractor("jsonpath", names, "", []any{"trim"}, "document"),
			"subject_url": extractor("jsonpath", links, "", []any{"absolute_url"}, "document"),
		}
	default:
		return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "arguments.searchConfig.subjectFormatId", Message: fmt.Sprintf("subject format %q is unsupported", formatID)}}
	}
	return stage, nil
}

func animekoEpisodes(config map[string]any, host string) (map[string]any, []Diagnostic) {
	stage := map[string]any{
		"request":  map[string]any{"method": "GET", "url": "{{subject_url}}", "allowed_hosts": []any{host}},
		"response": "html",
	}
	formatID := stringValue(config["channelFormatId"])
	if formatID == "" {
		formatID = "no-channel"
	}
	switch formatID {
	case "no-channel":
		format := objectValue(config["selectorChannelFormatNoChannel"])
		episodesSelector := stringValue(format["selectEpisodes"])
		if episodesSelector == "" {
			episodesSelector = "#glist-1 > div.module-blocklist.scroll-box.scroll-box-y > div > a"
		}
		linksSelector := stringValue(format["selectEpisodeLinks"])
		pattern := stringValue(format["matchEpisodeSortFromName"])
		if pattern == "" {
			pattern = `第\s*(?<ep>.+)\s*[话集]`
		}
		pattern, group, rewritten := episodeRegex(pattern, "ep")
		diagnostics := regexDiagnostic(rewritten, "arguments.searchConfig.selectorChannelFormatNoChannel.matchEpisodeSortFromName")
		fields := episodeFields(episodesSelector, linksSelector, pattern, group, linksSelector != "")
		if linksSelector == "" {
			stage["items"] = extractor("css", episodesSelector, "", nil, "")
		} else {
			stage["mode"] = "zipped"
		}
		stage["fields"] = fields
		return stage, diagnostics
	case "index-grouped":
		format := objectValue(config["selectorChannelFormatFlattened"])
		channelSelector := stringValue(format["selectChannelNames"])
		listSelector := stringValue(format["selectEpisodeLists"])
		episodesSelector := stringValue(format["selectEpisodesFromList"])
		if channelSelector == "" || listSelector == "" || episodesSelector == "" {
			return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "arguments.searchConfig.selectorChannelFormatFlattened", Message: "index-grouped format requires channel, list, and episode selectors"}}
		}
		channel := extractor("css", channelSelector, "", []any{"trim"}, "document")
		channelPattern := stringValue(format["matchChannelName"])
		var diagnostics []Diagnostic
		if channelPattern != "" {
			converted, group, rewritten := episodeRegex(channelPattern, "ch")
			channel["transforms"] = []any{"trim", map[string]any{"type": "regex", "pattern": converted, "group": group}}
			diagnostics = append(diagnostics, regexDiagnostic(rewritten, "arguments.searchConfig.selectorChannelFormatFlattened.matchChannelName")...)
		}
		episodePattern := stringValue(format["matchEpisodeSortFromName"])
		if episodePattern == "" {
			episodePattern = `第\s*(?<ep>.+)\s*[话集]`
		}
		episodePattern, episodeGroup, rewritten := episodeRegex(episodePattern, "ep")
		diagnostics = append(diagnostics, regexDiagnostic(rewritten, "arguments.searchConfig.selectorChannelFormatFlattened.matchEpisodeSortFromName")...)
		linksSelector := stringValue(format["selectEpisodeLinksFromList"])
		episodeCollection := map[string]any{"fields": episodeFields(episodesSelector, linksSelector, episodePattern, episodeGroup, linksSelector != "")}
		if linksSelector == "" {
			episodeCollection["items"] = extractor("css", episodesSelector, "", nil, "")
		} else {
			episodeCollection["mode"] = "zipped"
		}
		stage["lines"] = map[string]any{
			"items":    extractor("css", listSelector, "", nil, ""),
			"fields":   map[string]any{"channel": channel},
			"episodes": episodeCollection,
		}
		return stage, diagnostics
	default:
		return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "arguments.searchConfig.channelFormatId", Message: fmt.Sprintf("channel format %q is unsupported", formatID)}}
	}
}

func episodeFields(episodesSelector, linksSelector, pattern string, group int, zipped bool) map[string]any {
	scope := ""
	titleExpression := ":scope"
	if zipped {
		scope = "document"
		titleExpression = episodesSelector
	}
	playExpression := titleExpression
	if linksSelector != "" {
		playExpression = linksSelector
	}
	return map[string]any{
		"title": extractor("css", titleExpression, "", []any{"trim"}, scope),
		"number": extractor("css", titleExpression, "", []any{
			map[string]any{"type": "regex", "pattern": pattern, "group": group},
			"parse_episode",
		}, scope),
		"play_url": extractor("css", playExpression, "href", []any{"absolute_url"}, scope),
	}
}

func extractor(kind, expression, attribute string, transforms []any, scope string) map[string]any {
	result := map[string]any{"type": kind, "expression": expression}
	if attribute != "" {
		result["attribute"] = attribute
	}
	if len(transforms) > 0 {
		result["transforms"] = transforms
	}
	if scope != "" {
		result["scope"] = scope
	}
	return result
}

func fallbackExtractor(options, transforms []any, required bool) map[string]any {
	result := map[string]any{"any": options, "required": required}
	if len(transforms) > 0 {
		result["transforms"] = transforms
	}
	return result
}

func optionalExtractor(value map[string]any) map[string]any {
	value["required"] = false
	return value
}

func convertAnimekoTemplate(value string) string {
	return strings.NewReplacer("{keyword}", "{{title}}", "{page}", "{{page}}").Replace(value)
}

func normalizedTier(value any, fallback int) (int, []Diagnostic) {
	tier := intValue(value, fallback)
	if tier < 0 {
		return 0, []Diagnostic{{Severity: "warning", Category: "tier_clamped", Path: "arguments.tier", Message: "negative tier was clamped to 0"}}
	}
	if tier > 9 {
		return 9, []Diagnostic{{Severity: "warning", Category: "tier_clamped", Path: "arguments.tier", Message: "tier above 9 was clamped to 9"}}
	}
	return tier, nil
}

func compatibleRegex(pattern, fallback string) (string, string, bool) {
	converted := regexp.MustCompile(`\(\?<([A-Za-z][A-Za-z0-9_]*)>`).ReplaceAllString(pattern, `(?P<$1>`)
	if _, err := regexp.Compile(converted); err == nil {
		capture := ""
		compiled := regexp.MustCompile(converted)
		for _, name := range compiled.SubexpNames() {
			if name == "v" {
				capture = "v"
				break
			}
		}
		return converted, capture, false
	}
	return fallback, "", true
}

func episodeRegex(pattern, captureName string) (string, int, bool) {
	converted := regexp.MustCompile(`\(\?<([A-Za-z][A-Za-z0-9_]*)>`).ReplaceAllString(pattern, `(?P<$1>`)
	compiled, err := regexp.Compile(converted)
	if err == nil {
		for index, name := range compiled.SubexpNames() {
			if name == captureName {
				return converted, index, false
			}
		}
		if compiled.NumSubexp() > 0 {
			return converted, 1, false
		}
	}
	return `(?i)(?:第\s*)?([0-9]+(?:\.[0-9]+)?)`, 1, true
}

func regexDiagnostic(rewritten bool, path string) []Diagnostic {
	if !rewritten {
		return nil
	}
	return []Diagnostic{{Severity: "warning", Category: "regex_rewritten", Path: path, Message: "Kotlin episode regex was not RE2-compatible; replaced with a numeric episode matcher"}}
}

func parseCookies(value string) []any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.FieldsFunc(value, func(character rune) bool { return character == '\n' || character == '\r' || character == ';' })
	seen := map[string]bool{}
	var result []any
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "=") || seen[part] {
			continue
		}
		seen[part] = true
		result = append(result, part)
	}
	return result
}

func subtitleLanguage(value string) (string, bool) {
	switch strings.ToUpper(value) {
	case "", "CHS":
		return "zh-Hans", true
	case "CHT":
		return "zh-Hant", true
	case "CHC":
		return "yue", true
	case "JPN":
		return "ja", true
	case "ENG":
		return "en", true
	default:
		return "", false
	}
}

func validResolution(value string) bool {
	valid := map[string]bool{"360P": true, "480P": true, "720P": true, "1080P": true, "1440P": true, "2160P": true}
	return valid[value]
}

func defaultLimits(concurrency, perMinute, timeout int) map[string]any {
	return map[string]any{
		"concurrency":         concurrency,
		"requests_per_minute": perMinute,
		"timeout_ms":          timeout,
		"max_response_bytes":  5242880,
		"max_redirects":       3,
	}
}

func hasSourceError(diagnostics []Diagnostic, source string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Source == source && diagnostic.Severity == "error" {
			return true
		}
	}
	return false
}
