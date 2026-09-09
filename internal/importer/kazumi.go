package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

var kazumiTemplatePattern = regexp.MustCompile(`(^|[^A-Za-z0-9_])@([A-Za-z_][A-Za-z0-9_]*)`)

// ImportKazumi converts Kazumi XPath/API rules into Source Spec v1. Kazumi's
// imperative captcha helpers and browser scripts are deliberately not copied;
// affected sources are emitted disabled so an overlay or interactive runtime
// policy must explicitly opt them back in.
func ImportKazumi(data []byte, options Options) Result {
	var result Result
	if err := ValidateOptions(options); err != nil {
		result.add("error", "invalid_options", "", "", err.Error())
		return result
	}
	var root any
	if err := decodeJSON(data, &root); err != nil {
		result.add("error", "invalid_upstream", "", "", "decode Kazumi JSON: "+err.Error())
		return result
	}
	var entries []any
	switch typed := root.(type) {
	case map[string]any:
		entries = []any{typed}
	case []any:
		entries = typed
	default:
		result.add("error", "invalid_upstream", "", "", "Kazumi rule root must be an object or array")
		return result
	}
	if len(entries) == 0 {
		result.add("error", "invalid_upstream", "", "", "Kazumi rule array is empty")
		return result
	}

	seen := map[string]bool{}
	for index, value := range entries {
		entry, ok := value.(map[string]any)
		path := fmt.Sprintf("rules[%d]", index)
		if len(entries) == 1 {
			path = "rule"
		}
		if !ok {
			result.add("error", "invalid_source", path, path, "Kazumi rule must be an object")
			continue
		}
		canonical := canonicalJSON(entry)
		name := strings.TrimSpace(stringValue(entry["name"]))
		id, lossy := stableID(name, canonical)
		if seen[id] {
			id, _ = stableID(name+"-"+fmt.Sprint(index+1), canonical)
		}
		seen[id] = true
		if name == "" {
			result.add("error", "invalid_source", id, path+".name", "source name is required")
			continue
		}
		if lossy {
			result.add("warning", "id_normalized", id, path+".name", "source name required a digest-suffixed ASCII id")
		}
		source, diagnostics := importKazumiRule(id, entry, canonical, options)
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

func importKazumiRule(id string, entry map[string]any, canonical []byte, options Options) (map[string]any, []Diagnostic) {
	var diagnostics []Diagnostic
	api := intValue(entry["api"], 0)
	if api == 0 {
		api, _ = strconv.Atoi(stringValue(entry["api"]))
	}
	if api < 1 || api > 8 {
		return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "api", Message: fmt.Sprintf("Kazumi API level %q is unsupported; expected 1 through 8", stringValue(entry["api"]))}}
	}
	if ruleType := stringValue(entry["type"]); ruleType != "anime" {
		if ruleType == "1" {
			diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "legacy_type_normalized", Path: "type", Message: "legacy Kazumi anime type 1 was normalized to anime"})
		} else {
			return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "type", Message: fmt.Sprintf("Kazumi rule type %q is unsupported; expected anime", ruleType)}}
		}
	}
	version := strings.TrimSpace(stringValue(entry["version"]))
	if version == "" {
		return nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "version", Message: "source version is required"}}
	}
	rawBaseURL := strings.TrimSpace(stringValue(entry["baseURL"]))
	baseHost, err := hostFromTemplate(rawBaseURL)
	if err != nil {
		return nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "baseURL", Message: "invalid baseURL: " + err.Error()}}
	}
	homepage := baseURL(rawBaseURL)
	if homepage == "" {
		return nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "baseURL", Message: "baseURL must be an absolute HTTP(S) URL"}}
	}

	searchMode := kazumiMode(entry["searchMode"])
	chapterMode := kazumiMode(entry["chapterMode"])
	search, searchHosts, searchDiagnostics := kazumiSearch(entry, searchMode, rawBaseURL, baseHost)
	diagnostics = append(diagnostics, searchDiagnostics...)
	if search == nil || hasDiagnosticError(diagnostics) {
		return nil, diagnostics
	}
	episodes, episodeHosts, episodeDiagnostics := kazumiEpisodes(entry, chapterMode, rawBaseURL, baseHost)
	diagnostics = append(diagnostics, episodeDiagnostics...)
	if episodes == nil || hasDiagnosticError(diagnostics) {
		return nil, diagnostics
	}

	enabled := true
	if boolValue(entry["deprecated"], false) {
		enabled = false
		diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "deprecated_source", Path: "deprecated", Message: "deprecated Kazumi source was imported disabled"})
	}
	antiCrawler := objectValue(entry["antiCrawlerConfig"])
	if boolValue(antiCrawler["enabled"], boolValue(entry["antiCrawlerEnabled"], false)) {
		enabled = false
		diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "interactive_required", Path: "antiCrawlerConfig", Message: "Kazumi captcha automation is not portable or safe to import; source was disabled pending an interactive policy"})
	}
	if containsCategory(diagnostics, "sensitive_header_dropped") {
		enabled = false
	}
	if boolValue(entry["adBlocker"], false) {
		diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "adblock_behavior_dropped", Path: "adBlocker", Message: "Kazumi WebView ad blocking is runtime-specific and was not imported"})
	}
	if boolValue(entry["useLegacyParser"], false) {
		diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "legacy_parser_dropped", Path: "useLegacyParser", Message: "Kazumi legacy parser selection was replaced by the Source Spec browser sniffer"})
	}

	allowedHosts := uniqueSortedStrings(append([]string{baseHost}, append(searchHosts, episodeHosts...)...))
	resolve := map[string]any{
		"mode":          "browser_sniff",
		"transport":     "auto",
		"url":           "{{play_url}}",
		"match":         map[string]any{"include": []any{defaultVideoPattern}, "allow_verified_media": true},
		"allowed_hosts": stringsToAny(allowedHosts),
		"cookie_policy": "source",
		"timeout_ms":    15000,
	}
	requestHeaders := map[string]any{}
	if referer := strings.TrimSpace(stringValue(entry["referer"])); referer != "" {
		requestHeaders["Referer"] = referer
	}
	if userAgent := strings.TrimSpace(stringValue(entry["userAgent"])); userAgent != "" {
		requestHeaders["User-Agent"] = userAgent
	}
	if len(requestHeaders) > 0 {
		resolve["request_headers"] = requestHeaders
	}

	source := map[string]any{
		"schema":   "nagare-source/v1",
		"id":       id,
		"name":     stringValue(entry["name"]),
		"kind":     "web",
		"tier":     2,
		"enabled":  enabled,
		"homepage": homepage,
		"origin":   sourceOrigin(options, "kazumi", version, canonical),
		"search":   search,
		"episodes": episodes,
		"resolve":  resolve,
		"defaults": map[string]any{"resolution": "1080P", "subtitle_languages": []any{}},
		"matching": map[string]any{
			"require_episode":     true,
			"require_subject":     true,
			"distinguish_subject": true,
			"distinguish_channel": boolValue(entry["muliSources"], true),
		},
		"limits": defaultLimits(1, 20, 15000),
	}
	diagnostics = append(diagnostics,
		Diagnostic{Severity: "warning", Category: "overlay_required", Path: "baseURL", Message: "browser_sniff navigation allowlist contains only rule request hosts; review any required top-level player hosts with an overlay"},
		Diagnostic{Severity: "warning", Category: "selftest_missing", Path: "name", Message: "Kazumi rules do not carry a stable self-test title; add one with an overlay"},
	)

	supported := map[string]bool{
		"api": true, "type": true, "name": true, "version": true, "muliSources": true,
		"useWebview": true, "useNativePlayer": true, "usePost": true, "useLegacyParser": true,
		"adBlocker": true, "userAgent": true, "baseURL": true, "searchURL": true,
		"searchList": true, "searchName": true, "searchResult": true, "chapterRoads": true,
		"chapterResult": true, "referer": true, "deprecated": true, "antiCrawlerConfig": true,
		"antiCrawlerEnabled": true, "searchMode": true, "chapterMode": true,
		"searchApiConfig": true, "chapterApiConfig": true, "author": true, "lastUpdate": true,
	}
	var unknown []string
	for key := range entry {
		if !supported[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "unsupported_field", Path: key, Message: "Kazumi top-level field was not imported"})
	}
	return source, diagnostics
}

func kazumiSearch(entry map[string]any, mode, rawBaseURL, baseHost string) (map[string]any, []string, []Diagnostic) {
	if mode == "api" {
		config := objectValue(entry["searchApiConfig"])
		if len(config) == 0 {
			return nil, nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "searchApiConfig", Message: "API search mode requires searchApiConfig"}}
		}
		request, host, diagnostics := kazumiAPIRequest(objectValue(config["request"]), map[string]string{"keyword": "title"}, "searchApiConfig.request")
		if request == nil {
			return nil, nil, diagnostics
		}
		listPath := stringValue(config["listPath"])
		namePath := stringValue(config["namePath"])
		sourcePath := stringValue(config["sourcePath"])
		for field, path := range map[string]string{"listPath": listPath, "namePath": namePath, "sourcePath": sourcePath} {
			if err := validateKazumiJSONPath(path); err != nil {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: "searchApiConfig." + field, Message: err.Error()})
			}
		}
		stage := map[string]any{
			"request":  request,
			"response": "json",
			"base_url": rawBaseURL,
			"items":    extractor("jsonpath", listPath, "", nil, ""),
			"fields": map[string]any{
				"title":       extractor("jsonpath", namePath, "", []any{"trim"}, ""),
				"subject_key": extractor("jsonpath", sourcePath, "", []any{"trim"}, ""),
				"subject_url": extractor("jsonpath", sourcePath, "", []any{"trim", "absolute_url"}, ""),
			},
		}
		return stage, []string{host}, diagnostics
	}

	rawSearchURL := strings.TrimSpace(stringValue(entry["searchURL"]))
	searchList := strings.TrimSpace(stringValue(entry["searchList"]))
	searchName := strings.TrimSpace(stringValue(entry["searchName"]))
	searchResult := strings.TrimSpace(stringValue(entry["searchResult"]))
	if rawSearchURL == "" || searchList == "" || searchName == "" {
		return nil, nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "searchURL", Message: "XPath search requires searchURL, searchList, searchName, and searchResult"}}
	}
	var inferredDiagnostics []Diagnostic
	if searchResult == "" {
		if !regexp.MustCompile(`(?:^|/)a(?:\[[^]]+\])?$`).MatchString(searchList) {
			return nil, nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "searchResult", Message: "empty searchResult can only be inferred when searchList selects anchors"}}
		}
		searchResult = "."
		inferredDiagnostics = append(inferredDiagnostics, Diagnostic{Severity: "warning", Category: "selector_inferred", Path: "searchResult", Message: "empty searchResult was inferred as the search item itself"})
	}
	request, host, diagnostics := kazumiXPathSearchRequest(rawSearchURL, boolValue(entry["usePost"], false))
	diagnostics = append(diagnostics, inferredDiagnostics...)
	if request == nil {
		return nil, nil, diagnostics
	}
	stage := map[string]any{
		"request":  request,
		"response": "html",
		"base_url": rawBaseURL,
		"items":    extractor("xpath", searchList, "", nil, ""),
		"fields": map[string]any{
			"title":       extractor("xpath", searchName, "", []any{"trim"}, ""),
			"subject_key": extractor("xpath", searchResult, "href", []any{"trim"}, ""),
			"subject_url": extractor("xpath", searchResult, "href", []any{"trim", "absolute_url"}, ""),
		},
	}
	return stage, uniqueSortedStrings([]string{baseHost, host}), diagnostics
}

func kazumiEpisodes(entry map[string]any, mode, rawBaseURL, baseHost string) (map[string]any, []string, []Diagnostic) {
	if mode == "api" {
		config := objectValue(entry["chapterApiConfig"])
		if len(config) == 0 {
			return nil, nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "chapterApiConfig", Message: "API chapter mode requires chapterApiConfig"}}
		}
		if format := stringValue(config["format"]); format == "delimited" {
			return nil, nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.format", Message: "Kazumi delimited chapter responses are not representable by Source Spec v1 collections"}}
		} else if format != "" && format != "nested" {
			return nil, nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.format", Message: fmt.Sprintf("chapter format %q is unsupported", format)}}
		}
		request, requestHost, diagnostics := kazumiAPIRequest(objectValue(config["request"]), map[string]string{"source": "subject_key"}, "chapterApiConfig.request")
		if request == nil {
			return nil, nil, diagnostics
		}

		variables := map[string]any{}
		variableNames := map[string]string{}
		rawVariables := objectValue(config["variables"])
		keys := make([]string, 0, len(rawVariables))
		for key := range rawVariables {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			path := stringValue(rawVariables[key])
			if err := validateKazumiJSONPath(path); err != nil {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.variables." + key, Message: err.Error()})
				continue
			}
			name := kazumiVariableName(key)
			if name == "" {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.variables." + key, Message: "variable name cannot be normalized to a Source Spec identifier"})
				continue
			}
			if _, duplicate := variables[name]; duplicate {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.variables." + key, Message: fmt.Sprintf("variable name collides after normalization as %q", name)})
				continue
			}
			if name != key {
				diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "variable_normalized", Path: "chapterApiConfig.variables." + key, Message: fmt.Sprintf("variable was normalized to %q", name)})
			}
			variableNames[key] = name
			variables[name] = extractor("jsonpath", path, "", []any{"trim"}, "document")
		}

		roadsPath := strings.TrimSpace(stringValue(config["roadsPath"]))
		if roadsPath == "" {
			roadsPath = "$"
		}
		episodesPath := strings.TrimSpace(stringValue(config["episodesPath"]))
		episodeNamePath := strings.TrimSpace(stringValue(config["episodeNamePath"]))
		if err := validateKazumiJSONPath(roadsPath); err != nil {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.roadsPath", Message: err.Error()})
		}
		if err := validateKazumiJSONPath(episodesPath); err != nil {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.episodesPath", Message: err.Error()})
		}
		if err := validateKazumiJSONPath(episodeNamePath); err != nil {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.episodeNamePath", Message: err.Error()})
		}

		roadNamePath := strings.TrimSpace(stringValue(config["roadNamePath"]))
		channelOptions := []any{}
		if roadNamePath != "" {
			if err := validateKazumiJSONPath(roadNamePath); err != nil {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.roadNamePath", Message: err.Error()})
			}
			channelOptions = append(channelOptions, extractor("jsonpath", roadNamePath, "", []any{"trim"}, ""))
		}
		channelOptions = append(channelOptions, extractor("template", "播放线路{{line_number}}", "", nil, ""))

		episodeURLPath := strings.TrimSpace(stringValue(config["episodeUrlPath"]))
		episodeFields := map[string]any{
			"title": extractor("jsonpath", episodeNamePath, "", []any{"trim"}, ""),
			"number": fallbackExtractor([]any{
				optionalExtractor(extractor("jsonpath", episodeNamePath, "", []any{"trim", "parse_episode"}, "")),
				extractor("template", "{{episode_number}}", "", []any{"parse_episode"}, ""),
			}, nil, true),
		}
		if episodeURLPath != "" {
			if err := validateKazumiJSONPath(episodeURLPath); err != nil {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.episodeUrlPath", Message: err.Error()})
			}
			episodeFields["episode_url"] = extractor("jsonpath", episodeURLPath, "", []any{"trim"}, "")
		}

		page := objectValue(config["episodePage"])
		var pageHost string
		if len(page) > 0 {
			mapping := map[string]string{
				"source": "subject_key", "roadIndex": "line_index", "roadNumber": "line_number",
				"episodeIndex": "episode_index", "episodeNumber": "episode_number", "episodeUrl": "episode_url",
			}
			for upstream, normalized := range variableNames {
				mapping[upstream] = normalized
			}
			expression, err := kazumiEpisodePage(page, mapping)
			if err != nil {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: "chapterApiConfig.episodePage", Message: err.Error()})
			} else {
				episodeFields["play_url"] = extractor("template", expression, "", nil, "")
				pageHost, err = hostFromTemplate(expression)
				if err != nil {
					diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "invalid_source", Path: "chapterApiConfig.episodePage.url", Message: err.Error()})
				}
			}
		} else if episodeURLPath != "" {
			episodeFields["play_url"] = extractor("jsonpath", episodeURLPath, "", []any{"trim", "absolute_url"}, "")
		} else {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "invalid_source", Path: "chapterApiConfig.episodeUrlPath", Message: "episodeUrlPath or episodePage is required"})
		}

		stage := map[string]any{
			"request":  request,
			"response": "json",
			"base_url": rawBaseURL,
			"lines": map[string]any{
				"items":  extractor("jsonpath", roadsPath, "", nil, ""),
				"fields": map[string]any{"channel": fallbackExtractor(channelOptions, nil, true)},
				"episodes": map[string]any{
					"items":  extractor("jsonpath", episodesPath, "", nil, ""),
					"fields": episodeFields,
				},
			},
		}
		if len(variables) > 0 {
			stage["variables"] = variables
		}
		return stage, uniqueSortedStrings([]string{baseHost, requestHost, pageHost}), diagnostics
	}

	chapterRoads := strings.TrimSpace(stringValue(entry["chapterRoads"]))
	chapterResult := strings.TrimSpace(stringValue(entry["chapterResult"]))
	if chapterRoads == "" || chapterResult == "" {
		return nil, nil, []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "chapterRoads", Message: "XPath chapter mode requires chapterRoads and chapterResult"}}
	}
	stage := map[string]any{
		"request":  map[string]any{"method": "GET", "url": "{{subject_url}}", "allowed_hosts": []any{baseHost}},
		"response": "html",
		"base_url": rawBaseURL,
		"lines": map[string]any{
			"items":  extractor("xpath", chapterRoads, "", nil, ""),
			"fields": map[string]any{"channel": extractor("template", "播放线路{{line_number}}", "", nil, "")},
			"episodes": map[string]any{
				"items": extractor("xpath", chapterResult, "", nil, ""),
				"fields": map[string]any{
					"title": extractor("xpath", ".", "", []any{"trim"}, ""),
					"number": fallbackExtractor([]any{
						optionalExtractor(extractor("xpath", ".", "", []any{"trim", "parse_episode"}, "")),
						extractor("template", "{{episode_number}}", "", []any{"parse_episode"}, ""),
					}, nil, true),
					"play_url": extractor("xpath", ".", "href", []any{"trim", "absolute_url"}, ""),
				},
			},
		},
	}
	return stage, []string{baseHost}, nil
}

func kazumiXPathSearchRequest(raw string, usePost bool) (map[string]any, string, []Diagnostic) {
	host, err := hostFromTemplate(raw)
	if err != nil {
		return nil, "", []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "searchURL", Message: err.Error()}}
	}
	mapping := map[string]string{"keyword": "title"}
	if !usePost {
		converted, err := convertKazumiTemplate(raw, mapping)
		if err != nil {
			return nil, "", []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "searchURL", Message: err.Error()}}
		}
		return map[string]any{"method": "GET", "url": converted, "allowed_hosts": []any{host}}, host, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return nil, "", []Diagnostic{{Severity: "error", Category: "invalid_source", Path: "searchURL", Message: "invalid POST search URL"}}
	}
	form := map[string]any{}
	for key, values := range parsed.Query() {
		if len(values) == 0 {
			continue
		}
		converted, convertErr := convertKazumiTemplate(values[0], mapping)
		if convertErr != nil {
			return nil, "", []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: "searchURL", Message: convertErr.Error()}}
		}
		form[key] = converted
	}
	parsed.RawQuery = ""
	request := map[string]any{"method": "POST", "url": parsed.String(), "allowed_hosts": []any{host}}
	if len(form) > 0 {
		request["form"] = form
	}
	return request, host, nil
}

func kazumiAPIRequest(config map[string]any, mapping map[string]string, path string) (map[string]any, string, []Diagnostic) {
	var diagnostics []Diagnostic
	method := strings.ToUpper(strings.TrimSpace(stringValue(config["method"])))
	if method == "" {
		method = "GET"
	}
	if method != "GET" && method != "POST" {
		return nil, "", []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: path + ".method", Message: fmt.Sprintf("request method %q is unsupported", method)}}
	}
	rawURL := strings.TrimSpace(stringValue(config["url"]))
	convertedURL, err := convertKazumiTemplate(rawURL, mapping)
	if err != nil {
		return nil, "", []Diagnostic{{Severity: "error", Category: "unsupported_rule", Path: path + ".url", Message: err.Error()}}
	}
	host, err := hostFromTemplate(convertedURL)
	if err != nil {
		return nil, "", []Diagnostic{{Severity: "error", Category: "invalid_source", Path: path + ".url", Message: err.Error()}}
	}
	request := map[string]any{"method": method, "url": convertedURL, "allowed_hosts": []any{host}}

	if rawQuery := objectValue(config["query"]); len(rawQuery) > 0 {
		query := map[string]any{}
		for _, key := range sortedMapKeys(rawQuery) {
			converted, convertErr := convertKazumiScalar(rawQuery[key], mapping)
			if convertErr != nil {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: path + ".query." + key, Message: convertErr.Error()})
				continue
			}
			query[key] = converted
		}
		if len(query) > 0 {
			request["query"] = query
		}
	}

	headers := map[string]any{}
	if rawHeaders := objectValue(config["headers"]); len(rawHeaders) > 0 {
		for _, key := range sortedMapKeys(rawHeaders) {
			if kazumiSensitiveHeader(key) {
				diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Category: "sensitive_header_dropped", Path: path + ".headers." + key, Message: "fixed credential-bearing header was removed and the source was disabled"})
				continue
			}
			text := fmt.Sprint(rawHeaders[key])
			converted, convertErr := convertKazumiTemplate(text, mapping)
			if convertErr != nil {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: path + ".headers." + key, Message: convertErr.Error()})
				continue
			}
			headers[key] = converted
		}
	}

	if method == "POST" {
		switch bodyType := stringValue(config["bodyType"]); bodyType {
		case "", "none":
		case "json":
			converted, convertErr := convertKazumiValue(config["body"], mapping)
			if convertErr != nil {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: path + ".body", Message: convertErr.Error()})
			} else {
				encoded, marshalErr := json.Marshal(converted)
				if marshalErr != nil {
					diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "invalid_source", Path: path + ".body", Message: marshalErr.Error()})
				} else {
					request["body"] = string(encoded)
					if !hasHeader(headers, "Content-Type") {
						headers["Content-Type"] = "application/json"
					}
				}
			}
		case "form":
			rawForm := objectValue(config["body"])
			if len(rawForm) == 0 {
				diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: path + ".body", Message: "form body must be an object of scalar values"})
				break
			}
			form := map[string]any{}
			for _, key := range sortedMapKeys(rawForm) {
				converted, convertErr := convertKazumiScalar(rawForm[key], mapping)
				if convertErr != nil {
					diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: path + ".body." + key, Message: convertErr.Error()})
					continue
				}
				form[key] = converted
			}
			request["form"] = form
		default:
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "unsupported_rule", Path: path + ".bodyType", Message: fmt.Sprintf("body type %q is unsupported", bodyType)})
		}
	}
	if len(headers) > 0 {
		request["headers"] = headers
	}
	return request, host, diagnostics
}

func kazumiEpisodePage(page map[string]any, mapping map[string]string) (string, error) {
	rawURL := strings.TrimSpace(stringValue(page["url"]))
	if rawURL == "" {
		return "", errors.New("episode page URL is required")
	}
	converted, err := convertKazumiTemplate(rawURL, mapping)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(converted)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return "", errors.New("episode page URL must be absolute HTTP(S)")
	}
	fragment := ""
	withoutFragment := converted
	if index := strings.IndexByte(withoutFragment, '#'); index >= 0 {
		fragment = withoutFragment[index:]
		withoutFragment = withoutFragment[:index]
	}
	pageURL := withoutFragment
	parts := []string{}
	if index := strings.IndexByte(withoutFragment, '?'); index >= 0 {
		pageURL = withoutFragment[:index]
		if existing := withoutFragment[index+1:]; existing != "" {
			parts = append(parts, existing)
		}
	}
	query := objectValue(page["query"])
	for _, key := range sortedMapKeys(query) {
		value, convertErr := convertKazumiScalar(query[key], mapping)
		if convertErr != nil {
			return "", fmt.Errorf("query %s: %w", key, convertErr)
		}
		parts = append(parts, url.QueryEscape(key)+"="+escapeKazumiTemplateQuery(fmt.Sprint(value)))
	}
	if len(parts) > 0 {
		pageURL += "?" + strings.Join(parts, "&")
	}
	return pageURL + fragment, nil
}

func convertKazumiValue(value any, mapping map[string]string) (any, error) {
	switch typed := value.(type) {
	case string:
		return convertKazumiTemplate(typed, mapping)
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			converted, err := convertKazumiValue(item, mapping)
			if err != nil {
				return nil, err
			}
			result[index] = converted
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for _, key := range sortedMapKeys(typed) {
			converted, err := convertKazumiValue(typed[key], mapping)
			if err != nil {
				return nil, err
			}
			result[key] = converted
		}
		return result, nil
	case nil, bool, json.Number:
		return typed, nil
	default:
		return nil, fmt.Errorf("unsupported request value type %T", value)
	}
}

func convertKazumiScalar(value any, mapping map[string]string) (any, error) {
	if _, ok := value.(map[string]any); ok {
		return nil, errors.New("request map values must be scalar")
	}
	if _, ok := value.([]any); ok {
		return nil, errors.New("request map values must be scalar")
	}
	return convertKazumiValue(value, mapping)
}

func convertKazumiTemplate(value string, mapping map[string]string) (string, error) {
	var unknown []string
	converted := kazumiTemplatePattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := kazumiTemplatePattern.FindStringSubmatch(match)
		name := parts[2]
		target, ok := mapping[name]
		if !ok {
			unknown = append(unknown, name)
			return match
		}
		return parts[1] + "{{" + target + "}}"
	})
	if len(unknown) > 0 {
		return "", fmt.Errorf("unknown Kazumi template variable @%s", strings.Join(uniqueSortedStrings(unknown), ", @"))
	}
	return converted, nil
}

func escapeKazumiTemplateQuery(value string) string {
	var output strings.Builder
	for len(value) > 0 {
		start := strings.Index(value, "{{")
		if start < 0 {
			output.WriteString(url.QueryEscape(value))
			break
		}
		output.WriteString(url.QueryEscape(value[:start]))
		endOffset := strings.Index(value[start+2:], "}}")
		if endOffset < 0 {
			output.WriteString(url.QueryEscape(value[start:]))
			break
		}
		end := start + 2 + endOffset + 2
		output.WriteString(value[start:end])
		value = value[end:]
	}
	return output.String()
}

func kazumiMode(value any) string {
	if stringValue(value) == "api" {
		return "api"
	}
	return "xpath"
}

func kazumiVariableName(value string) string {
	var output strings.Builder
	lastUnderscore := false
	for index, character := range strings.TrimSpace(value) {
		if unicode.IsUpper(character) {
			if index > 0 && output.Len() > 0 && !lastUnderscore {
				output.WriteByte('_')
			}
			character = unicode.ToLower(character)
		}
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9' && output.Len() > 0) {
			output.WriteRune(character)
			lastUnderscore = false
		} else if output.Len() > 0 && !lastUnderscore {
			output.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(output.String(), "_")
}

func validateKazumiJSONPath(expression string) error {
	if expression == "" || expression[0] != '$' {
		return fmt.Errorf("JSONPath must start with $")
	}
	for index := 1; index < len(expression); {
		switch expression[index] {
		case '.':
			index++
			start := index
			for index < len(expression) && isKazumiPathCharacter(expression[index]) {
				index++
			}
			if index == start {
				return fmt.Errorf("unsupported JSONPath %q", expression)
			}
		case '[':
			end := strings.IndexByte(expression[index+1:], ']')
			if end < 0 {
				return fmt.Errorf("JSONPath is missing ]: %q", expression)
			}
			end += index + 1
			content := strings.TrimSpace(expression[index+1 : end])
			quoted := len(content) >= 2 && ((content[0] == '\'' && content[len(content)-1] == '\'') || (content[0] == '"' && content[len(content)-1] == '"'))
			if content != "*" && !allDigits(content) && !quoted {
				return fmt.Errorf("unsupported JSONPath segment [%s]", content)
			}
			index = end + 1
		default:
			return fmt.Errorf("unsupported JSONPath %q", expression)
		}
	}
	return nil
}

func isKazumiPathCharacter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '_' || value == '$' || value == '-'
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func hasHeader(headers map[string]any, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

func kazumiSensitiveHeader(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch normalized {
	case "authorization", "proxy-authorization", "cookie", "apikey", "api-key", "x-api-key":
		return true
	default:
		return false
	}
}

func hasDiagnosticError(diagnostics []Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			return true
		}
	}
	return false
}

func containsCategory(diagnostics []Diagnostic, category string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Category == category {
			return true
		}
	}
	return false
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			seen[value] = true
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func stringsToAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}
