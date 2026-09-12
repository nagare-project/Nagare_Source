package sourceruntime

import (
	"testing"

	"github.com/nagare-project/Nagare_Source/internal/repository"
)

func TestSingleRoadRootExtractsOneEpisodeList(t *testing.T) {
	stage := map[string]any{"response": "html", "lines": map[string]any{
		"items": map[string]any{"type": "xpath", "expression": "."},
		"episodes": map[string]any{
			"items":  map[string]any{"type": "xpath", "expression": "//a"},
			"fields": map[string]any{"title": map[string]any{"type": "xpath", "expression": "."}},
		},
	}}
	rows, err := extractEpisodes(stage, "https://example.org/", []byte(`<div><a>第1集</a><a>第5集</a></div>`), nil)
	if err != nil || len(rows) != 2 || rows[1]["title"] != "第5集" {
		t.Fatalf("root extraction: %v %v", rows, err)
	}
	value, err := repository.LoadDocument("../../sources/upstreams/kazumi-rules/web/aafun.yaml")
	if err != nil {
		t.Fatal(err)
	}
	source := value.(map[string]any)
	if text(object(object(object(source["episodes"])["lines"])["items"])["expression"]) != "." {
		t.Fatal("committed aafun rule uses invalid root")
	}
}

func TestExampleSourcesAreNeverEnabledByDefault(t *testing.T) {
	for _, path := range []string{"../../sources/web/example-http.yaml", "../../sources/bt/example-rss.yaml"} {
		value, err := repository.LoadDocument(path)
		if err != nil {
			t.Fatal(err)
		}
		if NewSpecRunner(value.(map[string]any), RunnerOptions{}).Source().Enabled {
			t.Fatalf("example enabled: %s", path)
		}
	}
}
