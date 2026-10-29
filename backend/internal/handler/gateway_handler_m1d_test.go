//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pkgerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// m1dFirstByteTimeoutBody 构造与 M1-a service.UpstreamFirstByteTimeoutBody 同构的
// failover ResponseBody：error.type 恒为 service.UpstreamFirstByteTimeoutType，
// StatusCode 504 为网关请求级总预算耗尽时合成的 504（方案文档 §1.2/§1.3）。
func m1dFirstByteTimeoutBody() []byte {
	return []byte(`{"error":{"type":"` + service.UpstreamFirstByteTimeoutType + `","message":"` + service.UpstreamFirstByteTimeoutMessage + `"}}`)
}

// m1dCancellationTriggerWords 是客户端 isCancelledLikeErrorMessage（方案文档 §1.4）
// 的完整触发词表：文案中出现任意一个（大小写不敏感），客户端就判为「类取消」并
// 显示「用户已取消」，必须确保写回内容一例不沾。
var m1dCancellationTriggerWords = []string{
	"abort",
	"aborted",
	"aborterror", // AbortError 小写化后
	"cancel",
	"cancelled",
	"canceled",
	"operation was aborted",
	"empty response output from model",
}

// assertNoCancellationTriggerWords 对写回的全部文本（错误体 JSON 与 SSE 错误帧）逐条
// 断言不包含任何客户端类取消触发词，大小写不敏感。
func assertNoCancellationTriggerWords(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		lower := strings.ToLower(text)
		for _, word := range m1dCancellationTriggerWords {
			assert.Falsef(t, strings.Contains(lower, word),
				"写回内容命中客户端类取消触发词 %q：%q", word, text)
		}
	}
}

// TestM1dFirstByteTimeoutJSONPath_WritesDistinctTypeAndSafeMessage 覆盖派发单 (a)/(b)/(d)：
// 流未开始路径，首字节护栏超时耗尽（StatusCode 504 + error.type=upstream_first_byte_timeout）
// 写回 JSON 错误体 —— 必须携带可区分类型、message 不含类取消触发词、且产出非空内容。
func TestM1dFirstByteTimeoutJSONPath_WritesDistinctTypeAndSafeMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:   http.StatusGatewayTimeout, // 504：请求级总预算耗尽，网关合成 504
		ResponseBody: m1dFirstByteTimeoutBody(),
	}
	h := &GatewayHandler{}
	h.handleFailoverExhausted(c, failoverErr, service.PlatformAnthropic, false)

	body := w.Body.String()

	// (d) 非空内容：不得静默 EOF。
	require.NotEmpty(t, body, "流未开始路径的 JSON 错误体不得为空（不得静默 EOF）")
	// 既有失败关闭语义：504 沿用 504→502 映射，不得改写状态码。
	require.Equal(t, http.StatusBadGateway, w.Code, "首字节超时耗尽应沿用既有 504→502 失败关闭映射")

	// (a) 可区分的错误类型标识。
	require.Contains(t, body, service.UpstreamFirstByteTimeoutType,
		"JSON 错误体必须携带可区分的 error.type=upstream_first_byte_timeout")

	// (b) message 不含类取消触发词：对整体错误体与提取出的 message 双重重断言。
	message := gjson.GetBytes(w.Body.Bytes(), "error.message").String()
	require.Equal(t, pkgerrors.UpstreamFirstByteTimeoutJSON, message,
		"流未开始路径的 message 必须取 M1 文案常量 UpstreamFirstByteTimeoutJSON")
	assertNoCancellationTriggerWords(t, body, message)

	// 第 4 条：错误体不写 content/delta 字段，不污染 assistant 内容。
	assert.NotContains(t, body, "content", "JSON 错误体不得携带 content 字段")
	assert.NotContains(t, body, "delta", "JSON 错误体不得携带 delta 字段")
}

// TestM1dFirstByteTimeoutStreamFrame_WritesSSEEventNonEmpty 覆盖派发单 (a)/(b)/(d)：
// 流已开始路径，首字节护栏超时耗尽必须写出流内 SSE 错误帧（不得静默 EOF）。
func TestM1dFirstByteTimeoutStreamFrame_WritesSSEEventNonEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:   http.StatusGatewayTimeout,
		ResponseBody: m1dFirstByteTimeoutBody(),
	}
	h := &GatewayHandler{}
	h.handleFailoverExhausted(c, failoverErr, service.PlatformAnthropic, true)

	body := w.Body.String()

	// (d) 非空内容：流已开始也必须写出流内错误帧，不得零字节结束。
	require.NotEmpty(t, body, "流已开始路径必须写出流内 SSE 错误帧（不得静默 EOF）")
	require.Contains(t, body, `"type":"error"`, "必须为 SSE 错误帧（data: {...error...}），不得静默结束流")

	// (a) 流内错误帧携带可区分错误类型标识。
	require.Contains(t, body, service.UpstreamFirstByteTimeoutType,
		"流内错误帧的 error.type 必须为 upstream_first_byte_timeout")
	// (c) 流已开始的错误帧文案必须用 M1 流内文案常量。
	require.Contains(t, body, pkgerrors.UpstreamFirstByteTimeoutStreamFrame,
		"流内错误帧的 message 必须取 M1 文案常量 UpstreamFirstByteTimeoutStreamFrame")

	// (b) message 不含类取消触发词。
	assertNoCancellationTriggerWords(t, body)

	// 第 4 条：错误帧不写 content/delta 字段，不污染 assistant 内容。
	assert.NotContains(t, body, "content", "流内错误帧不得携带 content 字段")
	assert.NotContains(t, body, "delta", "流内错误帧不得携带 delta 字段")
}

// TestM1dNonFirstByteTimeoutErrors_ExistingMappingUnchanged 覆盖派发单 (c)：
// 非护栏超时类错误（401/429 等）的既有映射未被改变 —— 不得被换用首字节超时类型，
// errType 与 message 必须仍是 mapUpstreamError 的既有映射结果。
func TestM1dNonFirstByteTimeoutErrors_ExistingMappingUnchanged(t *testing.T) {
	cases := []struct {
		name        string
		statusCode  int
		responseBody string
		wantStatus  int
		wantType    string
		wantMessage string
	}{
		{
			name:         "401_auth_failed",
			statusCode:   http.StatusUnauthorized,
			responseBody: `{"error":{"type":"authentication_error","message":"invalid api key"}}`,
			wantStatus:   http.StatusBadGateway,
			wantType:     "upstream_error",
			wantMessage:  pkgerrors.UpstreamAuthFailed,
		},
		{
			name:         "429_rate_limited",
			statusCode:   http.StatusTooManyRequests,
			responseBody: `{"error":{"type":"rate_limit_error","message":"rate limited"}}`,
			wantStatus:   http.StatusTooManyRequests,
			wantType:     "rate_limit_error",
			wantMessage:  pkgerrors.UpstreamRateLimited,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

			failoverErr := &service.UpstreamFailoverError{
				StatusCode:   tc.statusCode,
				ResponseBody: []byte(tc.responseBody),
			}
			h := &GatewayHandler{}
			h.handleFailoverExhausted(c, failoverErr, service.PlatformAnthropic, false)

			body := w.Body.String()
			require.NotEmpty(t, body, "非护栏超时错误路径不得产出空响应")
			require.Equal(t, tc.wantStatus, w.Code, "非护栏超时错误必须保持既有状态码映射")
			require.NotContains(t, body, service.UpstreamFirstByteTimeoutType,
				"非护栏超时类错误不得被换用首字节超时类型")
			require.Contains(t, body, tc.wantType, "非护栏超时错误必须保持既有 errType 映射")
			require.Contains(t, body, tc.wantMessage, "非护栏超时错误必须保持既有 message 映射")
		})
	}
}