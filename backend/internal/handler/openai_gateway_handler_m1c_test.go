//go:build unit

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// m1cForbiddenSubstrings 是客户端 isCancelledLikeErrorMessage 规则覆盖的触发词
// （方案文档 §1.4）。错误帧文案（大小写不敏感）不得包含其中任何一个，否则客户端会把
// 本次"流正常结束 + 零语义输出"误判为类取消，界面回落显示"用户已取消"。
var m1cForbiddenSubstrings = []string{
	"abort",
	"aborted",
	"aborterror",
	"cancel",
	"cancelled",
	"canceled",
	"operation was aborted",
	"empty response output from model",
}

// assertNoCancellationLikeWords 断言给定文本（大小写不敏感）不含任何类取消触发词。
func assertNoCancellationLikeWords(t *testing.T, s string) {
	t.Helper()
	lower := strings.ToLower(s)
	for _, w := range m1cForbiddenSubstrings {
		assert.NotContains(t, lower, w,
			"错误帧文案不得含类取消触发词 %q（否则客户端会误显「用户已取消」）", w)
	}
}

// m1cFirstByteTimeoutFailover 构造首字节护栏超时耗尽错误（与 service 层 M1-a 产出的
// 错误体同构：StatusCode=504，错误体 type=service.UpstreamFirstByteTimeoutType）。
func m1cFirstByteTimeoutFailover() *service.UpstreamFailoverError {
	body := []byte(fmt.Sprintf(
		`{"error":{"type":"%s","message":"%s"}}`,
		service.UpstreamFirstByteTimeoutType,
		service.UpstreamFirstByteTimeoutMessage,
	))
	return &service.UpstreamFailoverError{
		StatusCode:   http.StatusGatewayTimeout,
		ResponseBody: body,
	}
}

// extractSSEErrorFrame 从响应体里抽取第一个 event: error 帧的 JSON，解析为 map 返回。
func extractSSEErrorFrame(t *testing.T, body string) map[string]any {
	t.Helper()
	require.True(t, strings.Contains(body, "event: error\n"),
		"必须写出 event: error SSE 帧（不得静默 EOF），实际响应: %q", body)
	// 取最后一个 '{' 起的 JSON（SSE data 行）。
	idx := strings.Index(body, "{")
	require.GreaterOrEqual(t, idx, 0, "响应体应包含 JSON 错误体，实际: %q", body)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(body[idx:]), &parsed),
		"错误帧 JSON 应可解析，原始: %q", body[idx:])
	return parsed
}

// TestM1cFirstByteTimeout_StreamingEmitsDistinguishableFrame 覆盖验收点 (a)/(b)：
// 首字节护栏超时 + 客户端流式场景下，写回的流内错误帧携带可区分 type，且文案不含类取消词。
func TestM1cFirstByteTimeout_StreamingEmitsDistinguishableFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, m1cFirstByteTimeoutFailover(), true)

	body := w.Body.String()
	parsed := extractSSEErrorFrame(t, body)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "错误帧应含 error 对象，实际: %v", parsed)

	// (a) 可区分的错误类型标识。
	assert.Equal(t, service.UpstreamFirstByteTimeoutType, errorObj["type"],
		"错误帧 type 必须是可区分的首字节超时类型")
	// 同时校验 code 字段（handler 用 type 作为 code 透传），增强客户端可区分性。
	assert.Equal(t, service.UpstreamFirstByteTimeoutType, errorObj["code"],
		"错误帧 code 必须与 type 一致，确保客户端可区分")

	msg, _ := errorObj["message"].(string)
	require.NotEmpty(t, msg, "错误帧必须携带 message")
	// (b) 文案不含任何类取消触发词。
	assertNoCancellationLikeWords(t, msg)
	// 流已开始时应使用流内错误帧专属文案 SSOT。
	assert.Equal(t, errors.UpstreamFirstByteTimeoutStreamFrame, msg,
		"流已开始时应引用流内错误帧专属 SSOT 文案")
}

// TestM1cFirstByteTimeout_NonStreamingEmitsJSON 覆盖非流式（响应头尚未提交）场景：
// 同样使用可区分 type 与专属 JSON 文案，且不含类取消词。
func TestM1cFirstByteTimeout_NonStreamingEmitsJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, m1cFirstByteTimeoutFailover(), false)

	body := w.Body.String()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &parsed), "非流式应返回 JSON 错误体，实际: %q", body)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "JSON 错误体应含 error 对象，实际: %v", parsed)

	assert.Equal(t, service.UpstreamFirstByteTimeoutType, errorObj["type"],
		"非流式错误体 type 也必须是可区分的首字节超时类型")
	msg, _ := errorObj["message"].(string)
	require.NotEmpty(t, msg)
	assertNoCancellationLikeWords(t, msg)
	// 流未开始应使用 JSON 错误体专属文案 SSOT。
	assert.Equal(t, errors.UpstreamFirstByteTimeoutJSON, msg,
		"流未开始时应引用 JSON 错误体专属 SSOT 文案")
}

// TestM1cNonGuardErrorsUntouched 覆盖验收点 (c)：非护栏超时类错误（401/429）未被本次改动
// 污染为护栏超时 type，仍走原映射（upstream_error / rate_limit_error + 既有文案）。
func TestM1cNonGuardErrorsUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name        string
		statusCode  int
		wantType    string
		wantMessage string
	}{
		{
			name:        "401 鉴权失败走原映射",
			statusCode:  http.StatusUnauthorized,
			wantType:    "upstream_error",
			wantMessage: errors.UpstreamAuthFailed,
		},
		{
			name:        "429 限速走原映射",
			statusCode:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantMessage: errors.UpstreamRateLimited,
		},
		{
			name:        "500 上游错误走原映射",
			statusCode:  http.StatusInternalServerError,
			wantType:    "upstream_error",
			wantMessage: errors.UpstreamUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

			failoverErr := &service.UpstreamFailoverError{
				StatusCode:   tc.statusCode,
				ResponseBody: []byte(nil),
			}
			h := &OpenAIGatewayHandler{}
			h.handleFailoverExhausted(c, failoverErr, true)

			body := w.Body.String()
			parsed := extractSSEErrorFrame(t, body)
			errorObj, ok := parsed["error"].(map[string]any)
			require.True(t, ok, "错误帧应含 error 对象，实际: %v", parsed)

			gotType, _ := errorObj["type"].(string)
			assert.NotEqual(t, service.UpstreamFirstByteTimeoutType, gotType,
				"非护栏超时错误不得被污染为首字节超时 type")
			assert.Equal(t, tc.wantType, gotType, "非护栏超时错误应保持原映射 type")

			gotMsg, _ := errorObj["message"].(string)
			assert.Equal(t, tc.wantMessage, gotMsg, "非护栏超时错误应保持原映射文案")
		})
	}
}
