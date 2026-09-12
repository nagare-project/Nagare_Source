package btcrawler

import (
	"bytes"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type parsedDocument struct {
	kind        string
	responseURL string
	raw         string
	json        any
	xml         *xmlNode
	namespaces  map[string]string
}

type xmlNode struct {
	Name     xml.Name
	Attrs    []xml.Attr
	Text     string
	Parent   *xmlNode
	Children []*xmlNode
}

type extractContext struct {
	document *parsedDocument
	item     any
}

func extractRows(source map[string]any, responseType string, response Response) ([]map[string]string, []Diagnostic, error) {
	search := objectValue(source["search"])
	document, err := parseResponse(responseType, response, objectValue(search["namespaces"]))
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s response: %w", responseType, err)
	}
	if baseURL := stringValue(search["base_url"]); baseURL != "" {
		document.responseURL = baseURL
	}
	if mode := stringValue(search["mode"]); mode == "zipped" {
		return nil, nil, errors.New("BT crawler does not yet support zipped search collections")
	}
	items, err := selectItems(search["items"], document)
	if err != nil {
		return nil, nil, fmt.Errorf("extract search.items: %w", err)
	}
	fields := objectValue(search["fields"])
	rows := make([]map[string]string, 0, len(items))
	diagnostics := make([]Diagnostic, 0)
	for index, item := range items {
		row, err := extractFieldMap(fields, extractContext{document: document, item: item})
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: "warning", Category: "item_extraction_failed", Source: stringValue(source["id"]),
				Item: index + 1, Message: err.Error(),
			})
			continue
		}
		rows = append(rows, row)
	}
	return rows, diagnostics, nil
}

func parseResponse(responseType string, response Response, namespaceDocument map[string]any) (*parsedDocument, error) {
	document := &parsedDocument{
		kind: responseType, responseURL: response.URL, raw: string(response.Body),
		namespaces: make(map[string]string, len(namespaceDocument)),
	}
	for prefix, value := range namespaceDocument {
		document.namespaces[prefix] = stringValue(value)
	}
	switch responseType {
	case "json":
		decoder := json.NewDecoder(bytes.NewReader(response.Body))
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
	case "xml", "rss":
		root, err := parseXML(response.Body)
		if err != nil {
			return nil, err
		}
		document.xml = root
	default:
		return nil, fmt.Errorf("BT response type %q is unsupported", responseType)
	}
	return document, nil
}

func parseXML(data []byte) (*xmlNode, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var root *xmlNode
	var current *xmlNode
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			node := &xmlNode{Name: typed.Name, Attrs: append([]xml.Attr(nil), typed.Attr...), Parent: current}
			if current != nil {
				current.Children = append(current.Children, node)
			} else if root == nil {
				root = node
			} else {
				return nil, errors.New("multiple XML root elements")
			}
			current = node
		case xml.EndElement:
			if current == nil || current.Name != typed.Name {
				return nil, errors.New("unbalanced XML elements")
			}
			current = current.Parent
		case xml.CharData:
			if current != nil {
				current.Text += string(typed)
			}
		}
	}
	if root == nil {
		return nil, errors.New("XML document has no root element")
	}
	return root, nil
}

func selectItems(extractor any, document *parsedDocument) ([]any, error) {
	typeName, expression, err := extractorDefinition(extractor, document.kind)
	if err != nil {
		return nil, err
	}
	switch typeName {
	case "jsonpath":
		values, err := selectJSON(document.json, expression)
		if err != nil {
			return nil, err
		}
		if len(values) == 1 {
			if array, ok := values[0].([]any); ok {
				return array, nil
			}
		}
		return values, nil
	case "xpath":
		values, err := selectXML(document.xml, document.xml, expression, document.namespaces)
		if err != nil {
			return nil, err
		}
		result := make([]any, 0, len(values))
		for _, value := range values {
			node, ok := value.(*xmlNode)
			if !ok {
				return nil, errors.New("items XPath must select elements")
			}
			result = append(result, node)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("items extractor type %q is unsupported", typeName)
	}
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
			return "", fmt.Errorf("unknown field %q", name)
		}
		states[name] = 1
		extracted, err := evaluateExtractor(definition, context, resolve)
		required := extractorRequired(definition)
		if err != nil && !required {
			extracted = nil
			err = nil
		}
		if err != nil {
			return "", fmt.Errorf("field %s: %w", name, err)
		}
		value := firstNonEmpty(extracted)
		if value == "" && required {
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
	object, isObject := definition.(map[string]any)
	if !isObject {
		object = map[string]any{"expression": definition}
	}
	var values []string
	if alternatives, ok := object["any"].([]any); ok {
		for _, alternative := range alternatives {
			candidate, err := evaluateExtractor(alternative, context, resolve)
			if err != nil {
				continue
			}
			if firstNonEmpty(candidate) != "" {
				values = candidate
				break
			}
		}
	} else {
		typeName, expression, err := extractorDefinition(object, context.document.kind)
		if err != nil {
			return nil, err
		}
		scope := context.item
		if stringValue(object["scope"]) == "document" {
			if context.document.kind == "json" {
				scope = context.document.json
			} else {
				scope = context.document.xml
			}
		}
		switch typeName {
		case "field":
			value, err := resolve(expression)
			if err != nil {
				return nil, err
			}
			values = []string{value}
		case "jsonpath":
			selected, err := selectJSON(scope, expression)
			if err != nil {
				return nil, err
			}
			for _, value := range selected {
				values = append(values, scalarString(value))
			}
		case "xpath":
			node, ok := scope.(*xmlNode)
			if !ok {
				return nil, errors.New("XPath context is not an XML element")
			}
			selected, err := selectXML(context.document.xml, node, expression, context.document.namespaces)
			if err != nil {
				return nil, err
			}
			for _, value := range selected {
				switch typed := value.(type) {
				case *xmlNode:
					values = append(values, nodeText(typed))
				case string:
					values = append(values, typed)
				}
			}
		case "regex":
			input := contextText(scope, context.document)
			pattern, err := regexp.Compile(expression)
			if err != nil {
				return nil, err
			}
			group := intOr(object["group"], 0)
			for _, match := range pattern.FindAllStringSubmatch(input, -1) {
				if group >= len(match) {
					return nil, fmt.Errorf("regex group %d does not exist", group)
				}
				values = append(values, match[group])
			}
		default:
			return nil, fmt.Errorf("extractor type %q is unsupported", typeName)
		}
	}
	transforms := arrayValue(object["transforms"])
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

func extractorDefinition(definition any, responseType string) (string, string, error) {
	if expression, ok := definition.(string); ok {
		return defaultExtractorType(responseType), expression, nil
	}
	object, ok := definition.(map[string]any)
	if !ok {
		return "", "", errors.New("extractor must be a string or object")
	}
	typeName := stringValue(object["type"])
	if typeName == "" {
		typeName = defaultExtractorType(responseType)
	}
	expression := stringValue(object["expression"])
	if expression == "" && object["any"] == nil {
		return "", "", errors.New("extractor expression is empty")
	}
	return typeName, expression, nil
}

func defaultExtractorType(responseType string) string {
	if responseType == "json" {
		return "jsonpath"
	}
	return "xpath"
}

func extractorRequired(definition any) bool {
	object, ok := definition.(map[string]any)
	if !ok {
		return true
	}
	return boolOr(object["required"], true)
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
		if remainder[0] == '.' {
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
			continue
		}
		if remainder[0] == '[' {
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
			continue
		}
		return nil, fmt.Errorf("unsupported JSONPath syntax near %q", remainder)
	}
	return current, nil
}

func jsonProperty(values []any, key string) []any {
	var result []any
	for _, value := range values {
		if object, ok := value.(map[string]any); ok {
			if child, exists := object[key]; exists && child != nil {
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
			keys := sortedKeys(typed)
			for _, key := range keys {
				result = append(result, typed[key])
			}
		}
	}
	return result
}

func jsonIndex(values []any, index int) []any {
	var result []any
	for _, value := range values {
		if array, ok := value.([]any); ok && index < len(array) {
			result = append(result, array[index])
		}
	}
	return result
}

func selectXML(root, context *xmlNode, expression string, namespaces map[string]string) ([]any, error) {
	expression = strings.TrimSpace(expression)
	if expression == "." || expression == "./" {
		return []any{context}, nil
	}
	descendant := strings.HasPrefix(expression, "//")
	absolute := strings.HasPrefix(expression, "/") && !descendant
	if descendant {
		expression = strings.TrimPrefix(expression, "//")
	} else if absolute {
		expression = strings.TrimPrefix(expression, "/")
	} else {
		expression = strings.TrimPrefix(expression, "./")
	}
	segments := strings.Split(expression, "/")
	if len(segments) == 0 || segments[0] == "" {
		return nil, errors.New("empty XPath")
	}
	current := []*xmlNode{context}
	if absolute {
		current = []*xmlNode{root}
		matches, err := xmlNameMatches(root.Name, segments[0], namespaces)
		if err != nil {
			return nil, err
		}
		if matches {
			segments = segments[1:]
		}
	}
	if descendant {
		matches, err := xmlDescendants(root, segments[0], namespaces)
		if err != nil {
			return nil, err
		}
		current = matches
		segments = segments[1:]
	}
	for index, segment := range segments {
		if segment == "" || segment == "." {
			continue
		}
		if segment == "text()" {
			if index != len(segments)-1 {
				return nil, errors.New("text() must terminate XPath")
			}
			result := make([]any, 0, len(current))
			for _, node := range current {
				result = append(result, nodeText(node))
			}
			return result, nil
		}
		if strings.HasPrefix(segment, "@") {
			if index != len(segments)-1 {
				return nil, errors.New("attribute must terminate XPath")
			}
			name := strings.TrimPrefix(segment, "@")
			var result []any
			for _, node := range current {
				for _, attribute := range node.Attrs {
					matches, err := xmlNameMatches(attribute.Name, name, namespaces)
					if err != nil {
						return nil, err
					}
					if matches {
						result = append(result, attribute.Value)
					}
				}
			}
			return result, nil
		}
		var next []*xmlNode
		for _, node := range current {
			for _, child := range node.Children {
				matches, err := xmlNameMatches(child.Name, segment, namespaces)
				if err != nil {
					return nil, err
				}
				if matches {
					next = append(next, child)
				}
			}
		}
		current = next
	}
	result := make([]any, len(current))
	for index, node := range current {
		result[index] = node
	}
	return result, nil
}

func xmlDescendants(node *xmlNode, name string, namespaces map[string]string) ([]*xmlNode, error) {
	var result []*xmlNode
	var walk func(*xmlNode) error
	walk = func(current *xmlNode) error {
		matches, err := xmlNameMatches(current.Name, name, namespaces)
		if err != nil {
			return err
		}
		if matches {
			result = append(result, current)
		}
		for _, child := range current.Children {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(node); err != nil {
		return nil, err
	}
	return result, nil
}

func xmlNameMatches(name xml.Name, selector string, namespaces map[string]string) (bool, error) {
	prefix, local, hasPrefix := strings.Cut(selector, ":")
	if !hasPrefix {
		return name.Local == selector, nil
	}
	namespace, exists := namespaces[prefix]
	if !exists {
		return false, fmt.Errorf("XPath uses undeclared namespace prefix %q", prefix)
	}
	return name.Local == local && name.Space == namespace, nil
}

func nodeText(node *xmlNode) string {
	var builder strings.Builder
	var walk func(*xmlNode)
	walk = func(current *xmlNode) {
		builder.WriteString(current.Text)
		for _, child := range current.Children {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

func contextText(scope any, document *parsedDocument) string {
	switch typed := scope.(type) {
	case *xmlNode:
		return nodeText(typed)
	case string:
		return typed
	case nil:
		return document.raw
	default:
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	}
}

func scalarString(value any) string {
	switch typed := value.(type) {
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
		return string(encoded)
	}
}

func applyTransform(value string, definition any, responseURL string, resolve func(string) (string, error)) (string, error) {
	typeName, object, err := transformDefinition(definition)
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
		return html.UnescapeString(value), nil
	case "url_decode":
		decoded, err := url.QueryUnescape(value)
		return decoded, err
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
		return parseSize(value, stringValue(object["unit"]))
	case "parse_datetime":
		return parseDateTime(value, stringValue(object["unit"]))
	case "normalize_infohash":
		return normalizeInfoHash(value)
	case "parse_fansub":
		return parseFansub(value), nil
	case "regex":
		pattern, err := regexp.Compile(stringValue(object["pattern"]))
		if err != nil {
			return "", err
		}
		match := pattern.FindStringSubmatch(value)
		if match == nil {
			return "", errors.New("regex did not match")
		}
		group := intOr(object["group"], 0)
		if group >= len(match) {
			return "", fmt.Errorf("regex group %d does not exist", group)
		}
		return match[group], nil
	case "replace":
		pattern, err := regexp.Compile(stringValue(object["pattern"]))
		if err != nil {
			return "", err
		}
		return pattern.ReplaceAllString(value, stringValue(object["replacement"])), nil
	case "prepend":
		return fmt.Sprint(object["value"]) + value, nil
	case "append":
		return value + fmt.Sprint(object["value"]), nil
	case "default":
		if strings.TrimSpace(value) == "" {
			return fmt.Sprint(object["value"]), nil
		}
		return value, nil
	case "magnet":
		hash, err := normalizeInfoHash(value)
		if err != nil {
			return "", err
		}
		values := make(url.Values)
		values.Set("xt", "urn:btih:"+hash)
		if field := stringValue(object["display_name_from"]); field != "" {
			displayName, err := resolve(field)
			if err != nil {
				return "", err
			}
			if displayName != "" {
				values.Set("dn", displayName)
			}
		}
		for _, tracker := range stringSlice(object["trackers"]) {
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
	object, ok := definition.(map[string]any)
	if !ok {
		return "", nil, errors.New("transform must be a string or object")
	}
	typeName := stringValue(object["type"])
	if typeName == "" {
		return "", nil, errors.New("transform type is empty")
	}
	return typeName, object, nil
}

func parseEpisode(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if number, err := strconv.ParseFloat(trimmed, 64); err == nil && number > 0 {
		return strconv.FormatFloat(number, 'f', -1, 64), nil
	}
	patterns := []*regexp.Regexp{
		// S02E05 / s2e5：Nix-Raws 等 WEB-DL 组的写法，E 前面紧挨着季号数字，
		// 下面那条「E 前必须是非字母数字」的规则接不住它。
		regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])S[0-9]{1,2}E([0-9]{1,4}(?:\.[0-9]+)?)`),
		regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])EP?\s*([0-9]{1,4}(?:\.[0-9]+)?)`),
		regexp.MustCompile(`第\s*([0-9]{1,4}(?:\.[0-9]+)?)\s*[话話集]`),
		regexp.MustCompile(`\s-\s*([0-9]{1,4}(?:\.[0-9]+)?)`),
		regexp.MustCompile(`\[\s*([0-9]{1,4}(?:\.[0-9]+)?)\s*\]`),
	}
	for _, pattern := range patterns {
		match := pattern.FindStringSubmatch(trimmed)
		if len(match) > 1 {
			number, err := strconv.ParseFloat(match[1], 64)
			if err == nil && number > 0 {
				return strconv.FormatFloat(number, 'f', -1, 64), nil
			}
		}
	}
	return "", errors.New("episode could not be parsed")
}

func parseSize(value, unit string) (string, error) {
	trimmed := strings.ReplaceAll(strings.TrimSpace(value), ",", "")
	pattern := regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*(B|KB|KIB|MB|MIB|GB|GIB|TB|TIB)?$`)
	match := pattern.FindStringSubmatch(trimmed)
	if match == nil {
		return "", errors.New("size could not be parsed")
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return "", err
	}
	suffix := strings.ToUpper(match[2])
	if suffix == "" && unit != "" && unit != "auto" {
		suffix = strings.ToUpper(unit)
	}
	multipliers := map[string]float64{
		"": 1, "B": 1, "KB": 1024, "KIB": 1024,
		"MB": 1024 * 1024, "MIB": 1024 * 1024,
		"GB": 1024 * 1024 * 1024, "GIB": 1024 * 1024 * 1024,
		"TB": 1024 * 1024 * 1024 * 1024, "TIB": 1024 * 1024 * 1024 * 1024,
	}
	multiplier, exists := multipliers[suffix]
	if !exists {
		return "", fmt.Errorf("size unit %q is unsupported", suffix)
	}
	bytesValue := number * multiplier
	if bytesValue < 0 || bytesValue > math.MaxInt64 {
		return "", errors.New("size is out of range")
	}
	return strconv.FormatInt(int64(math.Round(bytesValue)), 10), nil
}

func parseDateTime(value, unit string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if unit == "unix_seconds" || unit == "unix_milliseconds" {
		number, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return "", err
		}
		when := time.Unix(number, 0)
		if unit == "unix_milliseconds" {
			when = time.UnixMilli(number)
		}
		return when.UTC().Format(time.RFC3339), nil
	}
	formats := []string{
		time.RFC3339Nano, time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822,
		"2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05",
	}
	for _, format := range formats {
		when, err := time.Parse(format, trimmed)
		if err == nil {
			return when.UTC().Format(time.RFC3339), nil
		}
	}
	return "", errors.New("datetime could not be parsed")
}

func normalizeInfoHash(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) == 40 {
		decoded, err := hex.DecodeString(value)
		if err == nil && len(decoded) == 20 {
			return strings.ToLower(value), nil
		}
	}
	if len(value) == 32 {
		decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(value))
		if err == nil && len(decoded) == 20 {
			return hex.EncodeToString(decoded), nil
		}
	}
	return "", errors.New("infohash is invalid")
}

func parseFansub(value string) string {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "[") {
		return ""
	}
	end := strings.Index(trimmed, "]")
	if end <= 1 {
		return ""
	}
	group := strings.TrimSpace(trimmed[1:end])
	upper := strings.ToUpper(group)
	if strings.Contains(upper, "1080") || strings.Contains(upper, "720") || strings.Contains(upper, "2160") {
		return ""
	}
	return group
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
		if name, _, _ := transformDefinition(transform); name == "default" {
			return true
		}
	}
	return false
}

func arrayValue(value any) []any {
	result, _ := value.([]any)
	return result
}
