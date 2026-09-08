package importer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImporterFixtures(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		path      string
		importer  func([]byte, Options) Result
		wantCount int
	}{
		{
			name:      "animeko subscription",
			path:      filepath.Join(root, "fixtures", "importers", "animeko", "subscription.json"),
			importer:  ImportAnimeko,
			wantCount: 2,
		},
		{
			name:      "nagare schema 1",
			path:      filepath.Join(root, "fixtures", "importers", "nagare-v1", "source.yaml"),
			importer:  ImportNagareV1,
			wantCount: 1,
		},
		{
			name:      "kazumi xpath",
			path:      filepath.Join(root, "fixtures", "importers", "kazumi", "xpath.json"),
			importer:  ImportKazumi,
			wantCount: 1,
		},
		{
			name:      "kazumi api",
			path:      filepath.Join(root, "fixtures", "importers", "kazumi", "api.json"),
			importer:  ImportKazumi,
			wantCount: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatal(err)
			}
			result := test.importer(data, testOptions())
			if result.HasErrors() {
				t.Fatalf("fixture import returned errors: %+v", result.Diagnostics)
			}
			if len(result.Sources) != test.wantCount {
				t.Fatalf("got %d sources, want %d", len(result.Sources), test.wantCount)
			}
			for _, source := range result.Sources {
				assertValidSource(t, source)
			}
		})
	}
}
