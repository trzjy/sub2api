//go:build unit

package service

// S2a 卡：cursor_protocol_request.go 产生点标码（方案 §2.2.1 逐点分配表）。
// 每个码至少一个真实触发输入；断言 Code / Status 与表一致，且仍能 errors.Is 到哨兵。

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestTestCursorProtocolRequestRequestErrorCodes(t *testing.T) {
	userMsg := func(content string) *apicompat.ChatCompletionsRequest {
		return &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(content)}},
		}
	}
	imageMsg := func(url string) *apicompat.ChatCompletionsRequest {
		return userMsg(`[{"type":"image_url","image_url":{"url":` + cursorJSONString(url) + `}}]`)
	}
	const pngURI = "data:image/png;base64," + cursorTestPNGDataURI
	imagePart := `{"type":"image_url","image_url":{"url":` + cursorJSONString(pngURI) + `}}`

	cases := []struct {
		name   string
		req    *apicompat.ChatCompletionsRequest
		model  string
		code   string
		status int
	}{
		// CUR-EXEC-08（500）：内部调用方不变量破坏。
		{"nil request", nil, "grok-4.7",
			infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError},
		// CUR-EXEC-07（500）：管理员模型映射为空。
		{"empty upstream model", userMsg(`"hi"`), "   ",
			infraerrors.CursorExecModelMappingInvalidCode, http.StatusInternalServerError},
		// CUR-REQ-04（400）：消息结构 / 内容格式。
		{"no convertible messages", &apicompat.ChatCompletionsRequest{}, "grok-4.7",
			infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest},
		{"ends with assistant", &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`"prefill"`)},
		}}, "grok-4.7", infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest},
		{"message without role", &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
			{Content: json.RawMessage(`"hi"`)},
		}}, "grok-4.7", infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest},
		{"unsupported role", &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
			{Role: "wizard", Content: json.RawMessage(`"hi"`)},
		}}, "grok-4.7", infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest},
		{"content not a string", userMsg(`"unterminated`), "grok-4.7",
			infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest},
		{"content not a valid parts array", userMsg(`[1,2]`), "grok-4.7",
			infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest},
		{"non-text part without image", userMsg(`[{"type":"file","file":{"file_id":"f"}}]`), "grok-4.7",
			infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest},
		{"non-text part with image", userMsg(`[` + imagePart + `,{"type":"file","file":{"file_id":"f"}}]`), "grok-4.7",
			infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest},
		{"numeric content", userMsg(`42`), "grok-4.7",
			infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest},
		// CUR-REQ-05（400）：工具 input_schema 无效。
		{"invalid tool input_schema", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
			Tools: []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
				Name: "broken", Parameters: json.RawMessage(`{not json`)}}},
		}, "grok-4.7", infraerrors.CursorReqInvalidToolSchemaCode, http.StatusBadRequest},
		// CUR-REQ-03（400）：图片出现在非 user 消息。
		{"system message with image", &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
			{Role: "system", Content: json.RawMessage(`[` + imagePart + `]`)},
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		}}, "grok-4.7", infraerrors.CursorReqImageOutsideUserCode, http.StatusBadRequest},
		{"assistant message with image", &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`[` + imagePart + `]`)},
			{Role: "user", Content: json.RawMessage(`"again"`)},
		}}, "grok-4.7", infraerrors.CursorReqImageOutsideUserCode, http.StatusBadRequest},
		{"tool message with image", &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "tool", ToolCallID: "c1", Content: json.RawMessage(`[` + imagePart + `]`)},
			{Role: "user", Content: json.RawMessage(`"again"`)},
		}}, "grok-4.7", infraerrors.CursorReqImageOutsideUserCode, http.StatusBadRequest},
		// CUR-REQ-02（400）：图片格式。
		{"image_url part missing url", userMsg(`[{"type":"image_url"}]`), "grok-4.7",
			infraerrors.CursorReqInvalidImageCode, http.StatusBadRequest},
		{"remote image url", imageMsg("https://example.com/a.png"), "grok-4.7",
			infraerrors.CursorReqInvalidImageCode, http.StatusBadRequest},
		{"data URI without comma", imageMsg("data:image/png;base64"), "grok-4.7",
			infraerrors.CursorReqInvalidImageCode, http.StatusBadRequest},
		{"data URI not base64", imageMsg("data:image/png,abcd"), "grok-4.7",
			infraerrors.CursorReqInvalidImageCode, http.StatusBadRequest},
		{"data URI missing mime", imageMsg("data:;base64,AAAA"), "grok-4.7",
			infraerrors.CursorReqInvalidImageCode, http.StatusBadRequest},
		{"data URI non-image mime", imageMsg("data:text/html;base64,AAAA"), "grok-4.7",
			infraerrors.CursorReqInvalidImageCode, http.StatusBadRequest},
		{"data URI invalid base64", imageMsg("data:image/png;base64,@@@@"), "grok-4.7",
			infraerrors.CursorReqInvalidImageCode, http.StatusBadRequest},
		{"data URI empty payload", imageMsg("data:image/png;base64,"), "grok-4.7",
			infraerrors.CursorReqInvalidImageCode, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := cursorConvertRequest(c.req, c.model)
			assertCursorRequestErrorCode(t, err, c.code, c.status)
		})
	}

	// cursorHistoryBlobs 的 assistant 带图分支：经 cursorConvertMessages 会先被拦下，
	// 这里直接构造历史轮次触发该产生点。
	t.Run("history assistant turn with image", func(t *testing.T) {
		_, err := cursorHistoryBlobs([]cursorAgentTurn{{
			role:   "assistant",
			text:   "x",
			images: []cursorTurnImage{{data: []byte{1}, mimeType: "image/png"}},
		}})
		assertCursorRequestErrorCode(t, err,
			infraerrors.CursorReqImageOutsideUserCode, http.StatusBadRequest)
	})
}

func assertCursorRequestErrorCode(t *testing.T, err error, code string, status int) {
	t.Helper()
	require.Error(t, err)
	var reqErr *CursorRequestError
	require.True(t, errors.As(err, &reqErr), "error must be *CursorRequestError, got %T: %v", err, err)
	require.Equal(t, code, reqErr.Code)
	require.Equal(t, status, reqErr.Status)
	require.True(t, errors.Is(err, ErrCursorUnsupportedRequest), "must still unwrap to the sentinel")
}
