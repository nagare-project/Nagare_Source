package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ApplyOverlays(sources []map[string]any, overlayDir string) ([]map[string]any, []Diagnostic) {
	if strings.TrimSpace(overlayDir) == "" {
		return sources, nil
	}
	result := make([]map[string]any, 0, len(sources))
	var diagnostics []Diagnostic
	for _, source := range sources {
		id := stringValue(source["id"])
		path := filepath.Join(overlayDir, id+".yaml")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			result = append(result, source)
			continue
		}
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "overlay_read_failed", Source: id, Path: path, Message: err.Error()})
			continue
		}
		overlay, err := decodeYAML(data)
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "invalid_overlay", Source: id, Path: path, Message: err.Error()})
			continue
		}
		if err := validateOverlay(id, source, overlay); err != nil {
			diagnostics = append(diagnostics, Diagnostic{Severity: "error", Category: "invalid_overlay", Source: id, Path: path, Message: err.Error()})
			continue
		}
		patch := objectValue(overlay["patch"])
		merged := deepCopyMap(source)
		deepMerge(merged, patch)
		result = append(result, merged)
		diagnostics = append(diagnostics, Diagnostic{Severity: "info", Category: "overlay_applied", Source: id, Path: path, Message: stringValue(overlay["reason"])})
	}
	return result, diagnostics
}

func validateOverlay(id string, source, overlay map[string]any) error {
	allowed := map[string]bool{"schema": true, "source_id": true, "origin_digest": true, "reason": true, "patch": true}
	for key := range overlay {
		if !allowed[key] {
			return fmt.Errorf("unknown overlay field %q", key)
		}
	}
	if stringValue(overlay["schema"]) != "nagare-source-overlay/v1" {
		return fmt.Errorf("schema must be nagare-source-overlay/v1")
	}
	if stringValue(overlay["source_id"]) != id {
		return fmt.Errorf("source_id must be %q", id)
	}
	if strings.TrimSpace(stringValue(overlay["reason"])) == "" {
		return fmt.Errorf("reason is required")
	}
	patch, ok := overlay["patch"].(map[string]any)
	if !ok || len(patch) == 0 {
		return fmt.Errorf("patch must be a non-empty object")
	}
	for _, immutable := range []string{"schema", "id", "kind", "origin"} {
		if _, exists := patch[immutable]; exists {
			return fmt.Errorf("patch cannot change immutable field %q", immutable)
		}
	}
	expectedDigest := stringValue(overlay["origin_digest"])
	if expectedDigest != "" {
		origin := objectValue(source["origin"])
		if actual := stringValue(origin["digest"]); actual != expectedDigest {
			return fmt.Errorf("origin_digest is stale: source is %s, overlay expects %s", actual, expectedDigest)
		}
	}
	return nil
}

func deepMerge(destination, patch map[string]any) {
	for key, patchValue := range patch {
		if patchValue == nil {
			delete(destination, key)
			continue
		}
		patchMap, patchIsMap := patchValue.(map[string]any)
		destinationMap, destinationIsMap := destination[key].(map[string]any)
		if patchIsMap && destinationIsMap {
			deepMerge(destinationMap, patchMap)
			continue
		}
		destination[key] = deepCopy(patchValue)
	}
}

func deepCopyMap(value map[string]any) map[string]any {
	return deepCopy(value).(map[string]any)
}

func deepCopy(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		copy := make(map[string]any, len(typed))
		for key, child := range typed {
			copy[key] = deepCopy(child)
		}
		return copy
	case []any:
		copy := make([]any, len(typed))
		for index, child := range typed {
			copy[index] = deepCopy(child)
		}
		return copy
	default:
		return typed
	}
}
