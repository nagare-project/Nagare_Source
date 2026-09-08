package sourceruntime

import (
	"encoding/json"
	"strconv"
	"strings"
)

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func array(value any) []any {
	result, _ := value.([]any)
	return result
}

func text(value any) string {
	result, _ := value.(string)
	return result
}

func integer(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed)
		}
	}
	return fallback
}

func number(value any) float64 {
	switch typed := value.(type) {
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case float64:
		return typed
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case string:
		parsed, _ := strconv.ParseFloat(typed, 64)
		return parsed
	}
	return 0
}

func boolean(value any, fallback bool) bool {
	result, ok := value.(bool)
	if !ok {
		return fallback
	}
	return result
}

func stringsValue(value any) []string {
	if values, ok := value.([]string); ok {
		return append([]string(nil), values...)
	}
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value, ok := value.(string); ok {
			result = append(result, value)
		}
	}
	return result
}

func copyStrings(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func firstValue(values map[string]string, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(values[name]); value != "" {
			return value
		}
	}
	return ""
}
