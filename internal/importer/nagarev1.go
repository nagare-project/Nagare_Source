package importer

import (
	"fmt"
	"regexp"
	"strings"
)

func ImportNagareV1(data []byte, options Options) Result {
	var result Result
	if err := ValidateOptions(options); err != nil {
		result.add("error", "invalid_options", "", "", err.Error())
		return result
	}
	document, err := decodeYAML(data)
	if err != nil {
		result.add("error", "invalid_upstream", "", "", "decode Nagare schema 1 YAML: "+err.Error())
		return result
	}
	if intValue(document["schema"], 0) != 1 {
		result.add("error", "unsupported_rule", "", "schema", "only Nagare schema 1 is supported")
		return result
	}
	canonical := canonicalJSON(document)
	originalID := stringValue(document["id"])
	name := strings.TrimSpace(stringValue(document["name"]))
	if originalID == "" || name == "" {
		result.add("error", "invalid_source", originalID, "", "id and name are required")
		return result
	}
	id, lossy := stableID(originalID, canonical)
	if lossy || id != originalID {
		result.add("warning", "id_normalized", id, "id", fmt.Sprintf("legacy id %q was normalized to %q", originalID, id))
	}

	request := objectValue(document["request"])
	requestURL := strings.ReplaceAll(stringValue(request["url"]), "{{query}}", "{{title}}")
	if requestURL == "" {
		result.add("error", "invalid_source", id, "request.url", "request URL is required")
		return result
	}
	host, err := hostFromTemplate(requestURL)
	if err != nil {
		result.add("error", "invalid_source", id, "request.url", err.Error())
		return result
	}
	format := stringValue(document["format"])
	if format != "xml" && format != "json" {
		result.add("error", "unsupported_rule", id, "format", fmt.Sprintf("format %q is unsupported", format))
		return result
	}
	items, itemDiagnostics := nagarePathExtractor(document["items"], format, true)
	appendDiagnostics(&result, id, "items", itemDiagnostics)
	if items == nil {
		return result
	}

	legacyFields := objectValue(document["fields"])
	if len(legacyFields) == 0 {
		result.add("error", "invalid_source", id, "fields", "fields are required")
		return result
	}
	fields := make(map[string]any, len(legacyFields)+1)
	for oldName, definition := range legacyFields {
		newName := canonicalNagareField(oldName)
		converted, diagnostics := nagareFieldExtractor(definition, format)
		appendDiagnostics(&result, id, "fields."+oldName, diagnostics)
		if converted != nil {
			if newName == "published_at" && !hasTransform(converted, "parse_datetime") {
				appendTransform(converted, map[string]any{"type": "parse_datetime", "unit": "auto"})
			}
			fields[newName] = converted
		}
	}
	if hasSourceError(result.Diagnostics, id) {
		return result
	}
	if _, ok := fields["title"]; !ok {
		result.add("error", "invalid_source", id, "fields.title", "title field is required")
	}
	downloadField := "magnet"
	if _, ok := fields[downloadField]; !ok {
		result.add("error", "invalid_source", id, "fields.magnet", "magnet field is required")
	}
	if result.HasErrors() {
		return result
	}
	if _, ok := fields["episode"]; !ok {
		fields["episode"] = map[string]any{
			"type":       "field",
			"expression": "title",
			"transforms": []any{"parse_episode"},
			"required":   false,
		}
	}

	searchRequest := map[string]any{
		"method":        "GET",
		"url":           requestURL,
		"allowed_hosts": []any{host},
	}
	if headers := stringMap(request["headers"]); len(headers) > 0 {
		normalized := make(map[string]any, len(headers))
		for key, value := range headers {
			normalized[key] = value
		}
		searchRequest["headers"] = normalized
	}
	timeoutSeconds := intValue(request["timeout_seconds"], 20)
	if timeoutSeconds < 1 {
		timeoutSeconds = 20
	}
	if timeoutSeconds > 60 {
		timeoutSeconds = 60
		result.add("warning", "limit_clamped", id, "request.timeout_seconds", "timeout was clamped to Source Spec's 60 second request maximum")
	}
	searchRequest["timeout_ms"] = timeoutSeconds * 1000

	search := map[string]any{
		"request":  searchRequest,
		"response": format,
		"items":    items,
		"fields":   fields,
	}
	if namespaces := stringMap(document["namespaces"]); len(namespaces) > 0 {
		normalized := make(map[string]any, len(namespaces))
		for key, value := range namespaces {
			normalized[key] = value
		}
		search["namespaces"] = normalized
	}

	capabilities := objectValue(document["capabilities"])
	priority := intValue(capabilities["priority"], 0)
	hasSeeders := boolValue(capabilities["seeders"], false)
	source := map[string]any{
		"schema":   "nagare-source/v1",
		"id":       id,
		"name":     name,
		"kind":     "bt",
		"tier":     3,
		"enabled":  true,
		"origin":   sourceOrigin(options, "nagare-v1", "1", canonical),
		"search":   search,
		"resolve":  map[string]any{"mode": "direct", "transport": "torrent", "url": "{{magnet}}"},
		"defaults": map[string]any{"resolution": "1080P", "subtitle_languages": []any{}},
		"matching": map[string]any{"require_episode": true, "require_subject": true},
		"ranking":  map[string]any{"priority": priority, "has_seeders": hasSeeders},
		"limits":   defaultLimits(1, 30, timeoutSeconds*1000),
	}
	if homepage := stringValue(document["homepage"]); homepage != "" {
		source["homepage"] = homepage
	}
	if selftest := objectValue(document["selftest"]); stringValue(selftest["query"]) != "" {
		source["selftest"] = map[string]any{
			"request": map[string]any{"query": stringValue(selftest["query"])},
			"expect":  map[string]any{"min_candidates": 1, "transport": "torrent"},
		}
	} else {
		result.add("warning", "selftest_missing", id, "selftest", "legacy source has no self-test query")
	}

	supported := map[string]bool{
		"schema": true, "id": true, "name": true, "homepage": true, "request": true,
		"format": true, "namespaces": true, "items": true, "fields": true,
		"capabilities": true, "selftest": true,
	}
	for key := range document {
		if !supported[key] {
			result.add("warning", "unsupported_field", id, key, "legacy top-level field was not imported")
		}
	}
	result.Sources = append(result.Sources, source)
	return result
}

func nagareFieldExtractor(value any, format string) (any, []Diagnostic) {
	if text, ok := value.(string); ok {
		return nagarePathExtractor(text, format, false)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Message: "field extractor must be a string or object"}}
	}
	var result map[string]any
	if path, exists := object["path"]; exists {
		extractorValue, diagnostics := nagarePathExtractor(path, format, false)
		if len(diagnostics) > 0 {
			return nil, diagnostics
		}
		result = objectValue(extractorValue)
	} else if alternatives := arrayValue(object["any"]); len(alternatives) > 0 {
		converted := make([]any, 0, len(alternatives))
		var diagnostics []Diagnostic
		for index, alternative := range alternatives {
			item, itemDiagnostics := nagareFieldExtractor(alternative, format)
			for _, diagnostic := range itemDiagnostics {
				diagnostic.Path = fmt.Sprintf("any[%d]", index)
				diagnostics = append(diagnostics, diagnostic)
			}
			if item != nil {
				converted = append(converted, item)
			}
		}
		if len(diagnostics) > 0 {
			return nil, diagnostics
		}
		result = map[string]any{"any": converted}
	} else {
		return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Message: "field object requires path or any"}}
	}
	if rawTransforms := arrayValue(object["transforms"]); len(rawTransforms) > 0 {
		transforms := make([]any, 0, len(rawTransforms))
		for index, rawTransform := range rawTransforms {
			transform, diagnostics := nagareTransform(rawTransform)
			if len(diagnostics) > 0 {
				for _, diagnostic := range diagnostics {
					diagnostic.Path = fmt.Sprintf("transforms[%d]", index)
				}
				return nil, diagnostics
			}
			transforms = append(transforms, transform)
		}
		result["transforms"] = transforms
	}
	return result, nil
}

func nagarePathExtractor(value any, format string, items bool) (any, []Diagnostic) {
	path := stringValue(value)
	if path == "" {
		return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Message: "path must be a non-empty string"}}
	}
	if strings.HasPrefix(path, "$") && path != "$" {
		return map[string]any{"type": "field", "expression": strings.TrimPrefix(path, "$")}, nil
	}
	switch format {
	case "json":
		if path == "$" {
			return map[string]any{"type": "jsonpath", "expression": "$"}, nil
		}
		if !strings.HasPrefix(path, "$") {
			path = "$." + path
		}
		return map[string]any{"type": "jsonpath", "expression": path}, nil
	case "xml":
		path = convertXMLPath(path, items)
		return map[string]any{"type": "xpath", "expression": path}, nil
	default:
		return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Message: "unknown response format"}}
	}
}

func convertXMLPath(path string, items bool) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		if strings.Contains(segment, "@") && !strings.HasPrefix(segment, "@") {
			parts := strings.SplitN(segment, "@", 2)
			segments[index] = parts[0] + "/@" + parts[1]
		}
	}
	path = strings.Join(segments, "/")
	if items {
		return "/" + strings.TrimPrefix(path, "/")
	}
	path = "./" + strings.TrimPrefix(path, "./")
	if !strings.Contains(path, "/@") && !strings.HasSuffix(path, "text()") {
		path += "/text()"
	}
	return path
}

func nagareTransform(value any) (any, []Diagnostic) {
	if name, ok := value.(string); ok {
		switch name {
		case "trim":
			return "trim", nil
		case "lower":
			return "lowercase", nil
		case "format_bytes":
			return map[string]any{"type": "parse_size", "unit": "B"}, nil
		case "format_kb":
			return map[string]any{"type": "parse_size", "unit": "KB"}, nil
		case "unix_rfc3339":
			return map[string]any{"type": "parse_datetime", "unit": "unix_seconds"}, nil
		case "parse_fansub":
			return "parse_fansub", nil
		default:
			return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Message: fmt.Sprintf("transform %q is unsupported", name)}}
		}
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) != 1 {
		return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Message: "transform object must have exactly one operation"}}
	}
	if pattern, exists := object["regex"]; exists {
		text := stringValue(pattern)
		if _, err := regexp.Compile(text); err != nil {
			return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Message: "regex is not RE2-compatible: " + err.Error()}}
		}
		return map[string]any{"type": "regex", "pattern": text}, nil
	}
	if magnetValue, exists := object["magnet"]; exists {
		config := objectValue(magnetValue)
		transform := map[string]any{"type": "magnet"}
		if displayName := strings.TrimPrefix(stringValue(config["dn"]), "$"); displayName != "" {
			transform["display_name_from"] = displayName
		}
		if trackers := arrayValue(config["trackers"]); len(trackers) > 0 {
			transform["trackers"] = trackers
		}
		return transform, nil
	}
	return nil, []Diagnostic{{Severity: "error", Category: "unsupported_rule", Message: "unknown transform operation"}}
}

func canonicalNagareField(name string) string {
	switch name {
	case "size":
		return "size_bytes"
	case "date":
		return "published_at"
	case "infohash":
		return "info_hash"
	default:
		return strings.ReplaceAll(strings.ToLower(name), "-", "_")
	}
}

func appendDiagnostics(result *Result, source, prefix string, diagnostics []Diagnostic) {
	for _, diagnostic := range diagnostics {
		diagnostic.Source = source
		if diagnostic.Path == "" {
			diagnostic.Path = prefix
		} else {
			diagnostic.Path = prefix + "." + diagnostic.Path
		}
		result.Diagnostics = append(result.Diagnostics, diagnostic)
	}
}

func appendTransform(extractorValue any, transform any) {
	object := objectValue(extractorValue)
	transforms := arrayValue(object["transforms"])
	object["transforms"] = append(transforms, transform)
}

func hasTransform(extractorValue any, transformType string) bool {
	for _, transform := range arrayValue(objectValue(extractorValue)["transforms"]) {
		if stringValue(objectValue(transform)["type"]) == transformType {
			return true
		}
	}
	return false
}
