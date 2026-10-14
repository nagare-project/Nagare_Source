package pluginapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nagare-project/Nagare_Source/internal/repository"
	"github.com/nagare-project/Nagare_Source/internal/sourceruntime"
)

func postInspect(t *testing.T, handler *Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/sources/inspect", strings.NewReader(string(data)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestInspectSourceReturnsSummaryForValidDocument(t *testing.T) {
	handler := testHandler(t, nil)
	content, err := os.ReadFile("../../sources/bt/nyaa.yaml")
	if err != nil {
		t.Fatal(err)
	}
	response := postInspect(t, handler, map[string]string{"format": "yaml", "content": string(content)})
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Source repository.SourceSummary `json:"source"`
	}
	decodeResponse(t, response, &body)
	if body.Source.ID != "nyaa" || body.Source.Kind != "bt" || len(body.Source.AllowedHosts) != 1 {
		t.Fatalf("unexpected summary: %+v", body.Source)
	}
}

func TestInspectSourceRejectsInvalidInput(t *testing.T) {
	handler := testHandler(t, nil)
	cases := []struct {
		name   string
		body   any
		status int
		want   string
	}{
		{"unknown field", map[string]any{"format": "json", "content": "{}", "extra": true}, http.StatusBadRequest, "invalid_request"},
		{"bad format", map[string]any{"format": "toml", "content": "a = 1"}, http.StatusBadRequest, "format must be"},
		{"empty content", map[string]any{"format": "json", "content": "  "}, http.StatusBadRequest, "must not be empty"},
		{"undecodable", map[string]any{"format": "json", "content": "{"}, http.StatusUnprocessableEntity, "invalid_source"},
		{"schema violation", map[string]any{"format": "json", "content": `{"id":"x"}`}, http.StatusUnprocessableEntity, "invalid_source"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := postInspect(t, handler, testCase.body)
			if response.Code != testCase.status || !strings.Contains(response.Body.String(), testCase.want) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/sources/inspect", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET inspect returned %d", response.Code)
	}
}

func TestInspectErrorMessageIsBounded(t *testing.T) {
	message := truncateMessage(strings.Repeat("长", maximumInspectMessageBytes))
	if len(message) > maximumInspectMessageBytes+len("…") || !strings.HasSuffix(message, "…") {
		t.Fatalf("message was not truncated on a rune boundary: %d bytes", len(message))
	}
	if !json.Valid([]byte(`"` + message + `"`)) {
		t.Fatal("truncated message is not valid UTF-8 JSON")
	}
}

func TestInstalledSourcesIgnorePersistedRepositoryHealth(t *testing.T) {
	// 7sefun 在仓库健康报告中是 degraded；同 id 的用户安装版本不应继承这个状态。
	handler, err := New(Options{
		Root: "../..", Manifest: Manifest{Version: "test"},
		Runners:   []sourceruntime.Runner{&fakeRunner{source: source("7sefun", "web", true)}},
		Installed: map[string]bool{"7sefun": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))
	var body struct {
		Sources []sourceruntime.Source `json:"sources"`
	}
	decodeResponse(t, response, &body)
	if len(body.Sources) != 1 || body.Sources[0].Status != "healthy" {
		t.Fatalf("installed source inherited repository health: %+v", body.Sources)
	}
}
