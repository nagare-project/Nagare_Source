package sourceruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/antchfx/htmlquery"
	nethtml "golang.org/x/net/html"
)

type responseDocument struct {
	kind        string
	responseURL string
	raw         string
	json        any
	html        *nethtml.Node
}

type extractContext struct {
	document  *responseDocument
	item      any
	variables map[string]string
	position  int
}

func extractSearch(stage map[string]any, responseURL string, body []byte, variables map[string]string) ([]map[string]string, error) {
	document, err := parseResponseDocument(text(stage["response"]), responseURL, body)
	if err != nil {
		return nil, err
	}
	if baseURL := text(stage["base_url"]); baseURL != "" {
		document.responseURL = baseURL
	}
	return extractCollection(document, stage, documentRoot(document), variables)
}

func extractEpisodes(stage map[string]any, responseURL string, body []byte, variables map[string]string) ([]map[string]string, error) {
	document, err := parseResponseDocument(text(stage["response"]), responseURL, body)
	if err != nil {
		return nil, err
	}
	if baseURL := text(stage["base_url"]); baseURL != "" {
		document.responseURL = baseURL
	}
	variables = copyStrings(variables)
	if definitions := object(stage["variables"]); len(definitions) > 0 {
		values, err := extractFieldMap(definitions, extractContext{document: document, item: documentRoot(document), variables: variables, position: -1})
		if err != nil {
			return nil, fmt.Errorf("extract episode variables: %w", err)
		}
		for key, value := range values {
			variables[key] = value
		}
	}
	lines := object(stage["lines"])
	if len(lines) == 0 {
		return extractCollection(document, stage, documentRoot(document), variables)
	}
	lineItems, err := selectItems(lines["items"], extractContext{document: document, item: documentRoot(document), variables: variables, position: -1})
	if err != nil {
		return nil, fmt.Errorf("extract lines.items: %w", err)
	}
	var rows []map[string]string
	for lineIndex, lineItem := range lineItems {
		lineVariables := copyStrings(variables)
		lineVariables["line_index"] = strconv.Itoa(lineIndex)
		lineVariables["line_number"] = strconv.Itoa(lineIndex + 1)
		lineFields, err := extractFieldMap(object(lines["fields"]), extractContext{
			document: document, item: lineItem, variables: lineVariables, position: lineIndex,
		})
		if err != nil {
			continue
		}
		for key, value := range lineFields {
			lineVariables[key] = value
		}
		if lineVariables["line_key"] == "" {
			lineVariables["line_key"] = firstValue(lineFields, "line_key", "channel")
		}
		episodes := object(lines["episodes"])
		episodeRows, err := extractCollection(document, episodes, lineItem, lineVariables)
		if err != nil {
			return nil, fmt.Errorf("extract line %d episodes: %w", lineIndex+1, err)
		}
		for _, episode := range episodeRows {
			merged := copyStrings(lineFields)
			for key, value := range episode {
				merged[key] = value
			}
			merged["line_index"] = strconv.Itoa(lineIndex)
			merged["line_number"] = strconv.Itoa(lineIndex + 1)
			rows = append(rows, merged)
		}
	}
	return rows, nil
}

func extractCollection(document *responseDocument, collection map[string]any, parent any, variables map[string]string) ([]map[string]string, error) {
	fields := object(collection["fields"])
	if text(collection["mode"]) == "zipped" {
		return extractZipped(fields, extractContext{document: document, item: parent, variables: variables, position: -1})
	}
	items, err := selectItems(collection["items"], extractContext{document: document, item: parent, variables: variables, position: -1})
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]string, 0, len(items))
	for index, item := range items {
		itemVariables := copyStrings(variables)
		itemVariables["episode_index"] = strconv.Itoa(index)
		itemVariables["episode_number"] = strconv.Itoa(index + 1)
		row, err := extractFieldMap(fields, extractContext{document: document, item: item, variables: itemVariables, position: index})
		if err != nil {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func extractZipped(fields map[string]any, context extractContext) ([]map[string]string, error) {
	columns := make(map[string][]string, len(fields))
	requiredLength := -1
	optionalLength := 0
	for _, name := range sortedKeys(fields) {
		values, err := evaluateExtractor(fields[name], context, func(field string) (string, error) {
			return "", fmt.Errorf("zipped field %q cannot depend on field %q", name, field)
		})
		if err != nil {
			if extractorRequired(fields[name]) {
				return nil, fmt.Errorf("field %s: %w", name, err)
			}
			continue
		}
		columns[name] = values
		if extractorRequired(fields[name]) {
			if requiredLength < 0 || len(values) < requiredLength {
				requiredLength = len(values)
			}
		}
		if len(values) > optionalLength {
			optionalLength = len(values)
		}
	}
	length := optionalLength
	if requiredLength >= 0 {
		length = requiredLength
	}
	rows := make([]map[string]string, 0, length)
	for index := 0; index < length; index++ {
		row := map[string]string{}
		for name, values := range columns {
			if index < len(values) {
				row[name] = values[index]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseResponseDocument(kind, responseURL string, body []byte) (*responseDocument, error) {
	document := &responseDocument{kind: kind, responseURL: responseURL, raw: string(body)}
	switch kind {
	case "json":
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&document.json); err != nil {
			return nil, err
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return nil, errors.New("multiple JSON root values")
			}
			return nil, err
		}
	case "html", "xml", "rss":
		root, err := htmlquery.Parse(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		document.html = root
	case "text":
	default:
		return nil, fmt.Errorf("response type %q is unsupported", kind)
	}
	return document, nil
}

func documentRoot(document *responseDocument) any {
	if document.kind == "json" {
		return document.json
	}
	if document.html != nil {
		return document.html
	}
	return document.raw
}

func selectItems(definition any, context extractContext) ([]any, error) {
	values, err := selectValues(definition, context)
	if err != nil {
		return nil, err
	}
	if len(values) == 1 {
		if array, ok := values[0].([]any); ok {
			return array, nil
		}
	}
	return values, nil
}

func extractFieldMap(definitions map[string]any, context extractContext) (map[string]string, error) {
	values := make(map[string]string, len(definitions))
	states := make(map[string]int, len(definitions))
	var resolve func(string) (string, error)
	resolve = func(name string) (string, error) {
		if states[name] == 2 {
			return values[name], nil
		}
		if states[name] == 1 {
			return "", fmt.Errorf("field dependency cycle at %q", name)
		}
		definition, exists := definitions[name]
		if !exists {
			if value, ok := context.variables[name]; ok {
				return value, nil
			}
			return "", fmt.Errorf("unknown field or variable %q", name)
		}
		states[name] = 1
		extracted, err := evaluateExtractor(definition, context, resolve)
		if context.position >= 0 && extractorScope(definition) == "document" {
			if len(extracted) > context.position {
				extracted = extracted[context.position : context.position+1]
			} else {
				extracted = nil
			}
		}
		if err != nil && !extractorRequired(definition) {
			extracted = nil
			err = nil
		}
		if err != nil {
			return "", fmt.Errorf("field %s: %w", name, err)
		}
		value := firstNonEmpty(extracted)
		if value == "" && extractorRequired(definition) {
			return "", fmt.Errorf("field %s: required value is empty", name)
		}
		values[name] = value
		states[name] = 2
		return value, nil
	}
	for _, name := range sortedKeys(definitions) {
		if _, err := resolve(name); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func evaluateExtractor(definition any, context extractContext, resolve func(string) (string, error)) ([]string, error) {
	objectDefinition, isObject := definition.(map[string]any)
	if !isObject {
		objectDefinition = map[string]any{"expression": definition}
	}
	var values []string
	if alternatives, ok := objectDefinition["any"].([]any); ok {
		for _, alternative := range alternatives {
			candidate, err := evaluateExtractor(alternative, context, resolve)
			if err == nil && firstNonEmpty(candidate) != "" {
				values = candidate
				break
			}
		}
	} else {
		typeName, expression, err := extractorDefinition(objectDefinition, context.document.kind)
		if err != nil {
			return nil, err
		}
		switch typeName {
		case "field":
			value, err := resolve(expression)
			if err != nil {
				return nil, err
			}
			values = []string{value}
		case "template":
			value, err := render(expression, func(name string) (string, error) {
				if value, ok := context.variables[name]; ok {
					return value, nil
				}
				return resolve(name)
			})
			if err != nil {
				return nil, err
			}
			values = []string{value}
		default:
			selected, err := selectValues(objectDefinition, context)
			if err != nil {
				return nil, err
			}
			attribute := text(objectDefinition["attribute"])
			for _, selectedValue := range selected {
				values = append(values, selectedText(selectedValue, attribute, context.document))
			}
		}
	}
	transforms := array(objectDefinition["transforms"])
	if len(values) == 0 && hasDefaultTransform(transforms) {
		values = []string{""}
	}
	for transformIndex, transform := range transforms {
		for valueIndex, value := range values {
			transformed, err := applyTransform(value, transform, context.document.responseURL, resolve)
			if err != nil {
				return nil, fmt.Errorf("transform %d: %w", transformIndex, err)
			}
			values[valueIndex] = transformed
		}
	}
	return values, nil
}

func selectValues(definition any, context extractContext) ([]any, error) {
	typeName, expression, err := extractorDefinition(definition, context.document.kind)
	if err != nil {
		return nil, err
	}
	objectDefinition, _ := definition.(map[string]any)
	item := context.item
	if text(objectDefinition["scope"]) == "document" {
		item = documentRoot(context.document)
	}
	switch typeName {
	case "template":
		value, err := render(expression, func(name string) (string, error) {
			if value, ok := context.variables[name]; ok {
				return value, nil
			}
			return "", fmt.Errorf("unknown template variable %q", name)
		})
		if err != nil {
			return nil, err
		}
		return []any{value}, nil
	case "field":
		return nil, errors.New("field extractors must be evaluated through their collection")
	case "jsonpath":
		return selectJSON(item, expression)
	case "css":
		node, ok := item.(*nethtml.Node)
		if !ok {
			return nil, errors.New("CSS context is not an HTML node")
		}
		if expression == ":scope" {
			return []any{node}, nil
		}
		matcher, err := cascadia.Compile(expression)
		if err != nil {
			return nil, err
		}
		selection := goquery.NewDocumentFromNode(node).FindMatcher(matcher)
		result := make([]any, 0, selection.Length())
		selection.Each(func(_ int, item *goquery.Selection) {
			for _, selected := range item.Nodes {
				result = append(result, selected)
			}
		})
		return result, nil
	case "xpath":
		node, ok := item.(*nethtml.Node)
		if !ok {
			return nil, errors.New("XPath context is not an HTML node")
		}
		if expression == "." || expression == "./" {
			return []any{node}, nil
		}
		nodes, err := htmlquery.QueryAll(node, expression)
		if err != nil {
			return nil, err
		}
		result := make([]any, len(nodes))
		for index, selected := range nodes {
			result[index] = selected
		}
		return result, nil
	case "regex":
		pattern, err := regexp.Compile(expression)
		if err != nil {
			return nil, err
		}
		input := selectedText(item, "", context.document)
		group := integer(objectDefinition["group"], 0)
		var result []any
		for _, match := range pattern.FindAllStringSubmatch(input, -1) {
			if group < 0 || group >= len(match) {
				return nil, fmt.Errorf("regex group %d does not exist", group)
			}
			result = append(result, match[group])
		}
		return result, nil
	default:
		return nil, fmt.Errorf("extractor type %q is unsupported", typeName)
	}
}

func extractorDefinition(definition any, responseType string) (string, string, error) {
	if expression, ok := definition.(string); ok {
		return defaultExtractorType(responseType), expression, nil
	}
	value, ok := definition.(map[string]any)
	if !ok {
		return "", "", errors.New("extractor must be a string or object")
	}
	typeName := text(value["type"])
	if typeName == "" {
		typeName = defaultExtractorType(responseType)
	}
	expression := text(value["expression"])
	if expression == "" && value["any"] == nil {
		return "", "", errors.New("extractor expression is empty")
	}
	return typeName, expression, nil
}

func defaultExtractorType(responseType string) string {
	switch responseType {
	case "json":
		return "jsonpath"
	case "html":
		return "css"
	case "text":
		return "regex"
	default:
		return "xpath"
	}
}

func extractorRequired(definition any) bool {
	value, ok := definition.(map[string]any)
	if !ok {
		return true
	}
	return boolean(value["required"], true)
}

func extractorScope(definition any) string {
	value, _ := definition.(map[string]any)
	return text(value["scope"])
}

func selectedText(value any, attribute string, document *responseDocument) string {
	switch typed := value.(type) {
	case *nethtml.Node:
		if attribute != "" {
			return htmlquery.SelectAttr(typed, attribute)
		}
		return htmlquery.InnerText(typed)
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return ""
	default:
		encoded, _ := json.Marshal(typed)
		if string(encoded) == "null" && document != nil {
			return document.raw
		}
		return string(encoded)
	}
}

func selectJSON(root any, expression string) ([]any, error) {
	if expression == "$" || expression == "@" || expression == "." {
		return []any{root}, nil
	}
	if !strings.HasPrefix(expression, "$") && !strings.HasPrefix(expression, "@") {
		return nil, errors.New("JSONPath must start with $ or @")
	}
	remainder := expression[1:]
	current := []any{root}
	for len(remainder) > 0 {
		switch remainder[0] {
		case '.':
			remainder = remainder[1:]
			end := strings.IndexAny(remainder, ".[")
			if end < 0 {
				end = len(remainder)
			}
			key := remainder[:end]
			if key == "" {
				return nil, errors.New("empty JSONPath property")
			}
			current = jsonProperty(current, key)
			remainder = remainder[end:]
		case '[':
			end := strings.IndexByte(remainder, ']')
			if end < 0 {
				return nil, errors.New("unterminated JSONPath bracket")
			}
			token := strings.TrimSpace(remainder[1:end])
			switch {
			case token == "*":
				current = jsonWildcard(current)
			case (strings.HasPrefix(token, "'") && strings.HasSuffix(token, "'")) || (strings.HasPrefix(token, `"`) && strings.HasSuffix(token, `"`)):
				current = jsonProperty(current, token[1:len(token)-1])
			default:
				index, err := strconv.Atoi(token)
				if err != nil || index < 0 {
					return nil, fmt.Errorf("unsupported JSONPath bracket %q", token)
				}
				current = jsonIndex(current, index)
			}
			remainder = remainder[end+1:]
		default:
			return nil, fmt.Errorf("unsupported JSONPath syntax near %q", remainder)
		}
	}
	return current, nil
}

func jsonProperty(values []any, key string) []any {
	var result []any
	for _, value := range values {
		if objectValue, ok := value.(map[string]any); ok {
			if child, exists := objectValue[key]; exists && child != nil {
				result = append(result, child)
			}
		}
	}
	return result
}

func jsonWildcard(values []any) []any {
	var result []any
	for _, value := range values {
		switch typed := value.(type) {
		case []any:
			result = append(result, typed...)
		case map[string]any:
			for _, key := range sortedKeys(typed) {
				result = append(result, typed[key])
			}
		}
	}
	return result
}

func jsonIndex(values []any, index int) []any {
	var result []any
	for _, value := range values {
		if items, ok := value.([]any); ok && index < len(items) {
			result = append(result, items[index])
		}
	}
	return result
}

func applyTransform(value string, definition any, responseURL string, resolve func(string) (string, error)) (string, error) {
	typeName, options, err := transformDefinition(definition)
	if err != nil {
		return "", err
	}
	switch typeName {
	case "trim":
		return strings.TrimSpace(value), nil
	case "lowercase":
		return strings.ToLower(value), nil
	case "uppercase":
		return strings.ToUpper(value), nil
	case "html_decode":
		return stdhtml.UnescapeString(value), nil
	case "url_decode":
		return url.QueryUnescape(value)
	case "absolute_url":
		base, err := url.Parse(responseURL)
		if err != nil {
			return "", err
		}
		reference, err := url.Parse(strings.TrimSpace(value))
		if err != nil {
			return "", err
		}
		return base.ResolveReference(reference).String(), nil
	case "parse_episode":
		return parseEpisode(value)
	case "parse_size":
		return parseSize(value, text(options["unit"]))
	case "parse_datetime":
		return parseDateTime(value, text(options["unit"]))
	case "normalize_infohash":
		return strings.ToLower(strings.TrimSpace(value)), nil
	case "parse_fansub":
		return parseFansub(value), nil
	case "regex":
		pattern, err := regexp.Compile(text(options["pattern"]))
		if err != nil {
			return "", err
		}
		match := pattern.FindStringSubmatch(value)
		if match == nil {
			return "", errors.New("regex did not match")
		}
		group := integer(options["group"], 0)
		if group < 0 || group >= len(match) {
			return "", fmt.Errorf("regex group %d does not exist", group)
		}
		return match[group], nil
	case "replace":
		pattern, err := regexp.Compile(text(options["pattern"]))
		if err != nil {
			return "", err
		}
		return pattern.ReplaceAllString(value, text(options["replacement"])), nil
	case "prepend":
		return fmt.Sprint(options["value"]) + value, nil
	case "append":
		return value + fmt.Sprint(options["value"]), nil
	case "default":
		if strings.TrimSpace(value) == "" {
			return fmt.Sprint(options["value"]), nil
		}
		return value, nil
	case "magnet":
		hash := strings.ToLower(strings.TrimSpace(value))
		values := make(url.Values)
		values.Set("xt", "urn:btih:"+hash)
		if field := text(options["display_name_from"]); field != "" {
			displayName, err := resolve(field)
			if err != nil {
				return "", err
			}
			values.Set("dn", displayName)
		}
		for _, tracker := range stringsValue(options["trackers"]) {
			values.Add("tr", tracker)
		}
		return "magnet:?" + values.Encode(), nil
	default:
		return "", fmt.Errorf("transform %q is unsupported", typeName)
	}
}

func transformDefinition(definition any) (string, map[string]any, error) {
	if name, ok := definition.(string); ok {
		return name, map[string]any{"type": name}, nil
	}
	value, ok := definition.(map[string]any)
	if !ok {
		return "", nil, errors.New("transform must be a string or object")
	}
	typeName := text(value["type"])
	if typeName == "" {
		return "", nil, errors.New("transform type is empty")
	}
	return typeName, value, nil
}

func parseEpisode(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if number, err := strconv.ParseFloat(trimmed, 64); err == nil && number > 0 {
		return strconv.FormatFloat(number, 'f', -1, 64), nil
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])EP?\s*([0-9]{1,4}(?:\.[0-9]+)?)`),
		regexp.MustCompile(`第\s*([0-9]{1,4}(?:\.[0-9]+)?)\s*[话話集]`),
		regexp.MustCompile(`\s-\s*([0-9]{1,4}(?:\.[0-9]+)?)`),
		regexp.MustCompile(`\[\s*([0-9]{1,4}(?:\.[0-9]+)?)\s*\]`),
	}
	for _, pattern := range patterns {
		if match := pattern.FindStringSubmatch(trimmed); len(match) > 1 {
			return match[1], nil
		}
	}
	return "", errors.New("episode could not be parsed")
}

func parseSize(value, unit string) (string, error) {
	trimmed := strings.ReplaceAll(strings.TrimSpace(value), ",", "")
	match := regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*(B|KB|KIB|MB|MIB|GB|GIB|TB|TIB)?$`).FindStringSubmatch(trimmed)
	if match == nil {
		return "", errors.New("size could not be parsed")
	}
	number, _ := strconv.ParseFloat(match[1], 64)
	suffix := strings.ToUpper(match[2])
	if suffix == "" && unit != "" && unit != "auto" {
		suffix = strings.ToUpper(unit)
	}
	multiplier := map[string]float64{"": 1, "B": 1, "KB": 1000, "KIB": 1024, "MB": 1e6, "MIB": 1 << 20, "GB": 1e9, "GIB": 1 << 30, "TB": 1e12, "TIB": 1 << 40}[suffix]
	if multiplier == 0 {
		return "", fmt.Errorf("unsupported size unit %q", suffix)
	}
	return strconv.FormatInt(int64(number*multiplier), 10), nil
}

func parseDateTime(value, unit string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if unit == "unix_seconds" || unit == "unix_milliseconds" {
		integerValue, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return "", err
		}
		if unit == "unix_milliseconds" {
			return time.UnixMilli(integerValue).UTC().Format(time.RFC3339), nil
		}
		return time.Unix(integerValue, 0).UTC().Format(time.RFC3339), nil
	}
	for _, layout := range []string{time.RFC3339, time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC().Format(time.RFC3339), nil
		}
	}
	return "", errors.New("datetime could not be parsed")
}

func parseFansub(value string) string {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "[") {
		if end := strings.Index(trimmed, "]"); end > 1 {
			return strings.TrimSpace(trimmed[1:end])
		}
	}
	return trimmed
}

func render(template string, resolve func(string) (string, error)) (string, error) {
	var output strings.Builder
	remainder := template
	for {
		start := strings.Index(remainder, "{{")
		unexpectedClose := strings.Index(remainder, "}}")
		if start < 0 {
			if unexpectedClose >= 0 {
				return "", errors.New("malformed template")
			}
			output.WriteString(remainder)
			return output.String(), nil
		}
		if unexpectedClose >= 0 && unexpectedClose < start {
			return "", errors.New("malformed template")
		}
		output.WriteString(remainder[:start])
		endOffset := strings.Index(remainder[start+2:], "}}")
		if endOffset < 0 {
			return "", errors.New("malformed template")
		}
		end := start + 2 + endOffset
		name := strings.TrimSpace(remainder[start+2 : end])
		value, err := resolve(name)
		if err != nil {
			return "", err
		}
		output.WriteString(value)
		remainder = remainder[end+2:]
	}
}

func firstNonEmpty(values []string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func hasDefaultTransform(transforms []any) bool {
	for _, transform := range transforms {
		if name, options, err := transformDefinition(transform); err == nil && name == "default" && options != nil {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
