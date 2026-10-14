package pluginapi

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/nagare-project/Nagare_Source/internal/repository"
)

// maximumInspectMessageBytes 限制校验失败时回给宿主的说明长度；Schema 错误可能很长。
const maximumInspectMessageBytes = 600

type inspectSourceRequest struct {
	Format  string `json:"format"`
	Content string `json:"content"`
}

// postInspectSource 只读地校验一份来源文档并返回展示摘要，供宿主在用户确认安装前预览。
// 它不写盘、不发网络请求，也不改变当前加载的来源。
func (handler *Handler) postInspectSource(writer http.ResponseWriter, request *http.Request) {
	if !requireMethod(writer, request, http.MethodPost) {
		return
	}
	var input inspectSourceRequest
	if err := decodeStrictJSON(request, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if input.Format != "json" && input.Format != "yaml" {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "format must be json or yaml")
		return
	}
	if strings.TrimSpace(input.Content) == "" {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "content must not be empty")
		return
	}
	value, err := repository.DecodeDocument([]byte(input.Content), input.Format)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "invalid_source", truncateMessage(err.Error()))
		return
	}
	document, err := handler.validator.ValidateStandaloneSource(value)
	if err != nil {
		writeAPIError(writer, http.StatusUnprocessableEntity, "invalid_source", truncateMessage(err.Error()))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"source": repository.SummarizeSource(document)})
}

func truncateMessage(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if len(message) <= maximumInspectMessageBytes {
		return message
	}
	cut := maximumInspectMessageBytes
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + "…"
}
