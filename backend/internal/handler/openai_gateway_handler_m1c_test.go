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
// 注意：G1-c 判定已改为按 Reason 语义标记，真实首字节超时错误（service 层 M1-a/M1-b 产出）
// 都会设置 Reason == service.GatewayFirstByteTimeoutReason，故此处构造也须带上该 Reason，
// 否则不会被识别为首字节超时、落到通用映射。
func m1cFirstByteTimeoutFailover() *service.UpstreamFailoverError {
	body := []byte(fmt.Sprintf(
		`{"error":{"type":"%s","message":"%s"}}`,
		service.UpstreamFirstByteTimeoutType,
		service.UpstreamFirstByteTimeoutMessage,
	))
	return &service.UpstreamFailoverError{
		StatusCode:   http.StatusGatewayTimeout,
		ResponseBody: body,
		Reason:       service.GatewayFirstByteTimeoutReason,
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

// TestM1cFirstByteTimeout_CommittedButFlagFalse_StillStreamFrame 覆盖首轮遗漏的
// 真实组合（方案文档 §3 F3）：心跳已提交 SSE 200（响应已提交，c.Writer.Written() 为真），
// 但传入的 streamStarted 仍为 false——这正是 chat/completions 路径上首轮失效的现场。
// 断言此时耗尽写出的是流内 SSE error 帧（含 data: 前缀、含可区分 type），而非纯 JSON 体，
// 且文案不含类取消触发词、引用流内帧专属 SSOT。
func TestM1cFirstByteTimeout_CommittedButFlagFalse_StillStreamFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name string
		// commit 在调用前让响应"已提交"：心跳组件提交 SSE 200 的两种等价形态。
		commit func(w *httptest.ResponseRecorder, c *gin.Context)
	}{
		{
			name: "响应已 WriteHeader(200)",
			commit: func(w *httptest.ResponseRecorder, c *gin.Context) {
				// gin 的 c.Writer.Written() 在 WriteHeader 后不一定置位（取决于实现），
				// 真实心跳通过 writeSSEHeader 写字节使 Written() 为真；此处用 WriteHeaderNow
				// 显式提交响应头，等价于"响应头已 200 提交"。
				c.Writer.WriteHeaderNow()
			},
		},
		{
			name: "响应已写过字节",
			commit: func(w *httptest.ResponseRecorder, c *gin.Context) {
				_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

			// 关键：心跳已提交响应，但上游并发槽位并未将 streamStarted 置 true。
			tc.commit(w, c)

			h := &OpenAIGatewayHandler{}
			h.handleFailoverExhausted(c, m1cFirstByteTimeoutFailover(), false)

			body := w.Body.String()

			// 必须是流内 SSE error 帧，而非纯 JSON 错误体（c.JSON 形态会以 {"error" 开头，
			// 且无 event:/data: 行）。已提交的响应可能已在流中写过 keep-alive 注释等字节，
			// 故用 Contains 而非 HasPrefix 判定帧存在。
			assert.Contains(t, body, "event: error", "响应已提交时须写流内 SSE error 帧，实际响应: %q", body)
			assert.Contains(t, body, "data: ", "流内帧须带 data: 前缀")
			// 反例：不得是一行纯 JSON 错误体（这正是首轮失效的产物，会以 {"error" 开头）。
			assert.False(t, strings.HasPrefix(strings.TrimSpace(body), "{\"error\""),
				"已提交响应不得再写纯 JSON 错误体，应写流内 SSE 帧，实际: %q", body)

			parsed := extractSSEErrorFrame(t, body)
			errorObj, ok := parsed["error"].(map[string]any)
			require.True(t, ok, "错误帧应含 error 对象，实际: %v", parsed)

			assert.Equal(t, service.UpstreamFirstByteTimeoutType, errorObj["type"],
				"已提交响应下错误帧 type 必须是可区分的首字节超时类型")
			assert.Equal(t, service.UpstreamFirstByteTimeoutType, errorObj["code"],
				"已提交响应下错误帧 code 必须与 type 一致")
			msg, _ := errorObj["message"].(string)
			require.NotEmpty(t, msg)
			assertNoCancellationLikeWords(t, msg)
			// 已提交响应应使用流内错误帧专属文案 SSOT，而非 JSON 文案。
			assert.Equal(t, errors.UpstreamFirstByteTimeoutStreamFrame, msg,
				"已提交响应时应引用流内错误帧专属 SSOT 文案")
		})
	}
}

// m1cCNFirstByteTimeoutFailover 构造 CN 家族路径的首字节超时错误（客户走的正是一条）：
// StatusCode=504，错误体 type=upstream_error（CN 家族产出，与通用护栏不同），
// 但 Reason == service.GatewayFirstByteTimeoutReason（G1-a/G1-b 在造此错误时设置该语义标记）。
// 这是历史漏网现场：旧判定靠字符串匹配错误体，因 type=upstream_error 永不命中；
// 新判定靠 Reason 语义标记，应能命中并写回可区分 type 的流内帧。
func m1cCNFirstByteTimeoutFailover() *service.UpstreamFailoverError {
	body := []byte(`{"error":{"type":"upstream_error","message":"上游首字节超时，请稍后重试"}}`)
	return &service.UpstreamFailoverError{
		StatusCode:   http.StatusGatewayTimeout,
		ResponseBody: body,
		Reason:       service.GatewayFirstByteTimeoutReason,
	}
}

// m1cTrapEmptyReasonFailover 构造**陷阱**：StatusCode=504、Reason 为空（未作语义标记），
// 但 ResponseBody 字符串里恰好含有 `"upstream_first_byte_timeout"` 字样（被引号包裹）。
// 旧判定靠字符串匹配会误命中此特征串，把它当作首字节超时；
// 新判定靠 Reason 语义标记，因 Reason 为空必须**不**命中，落到通用 504 映射（upstream_error）。
// 此用例是本卡核心验收点：证明判定已从字符串匹配改为语义标记。
func m1cTrapEmptyReasonFailover() *service.UpstreamFailoverError {
	quoted := `"` + service.UpstreamFirstByteTimeoutType + `"`
	body := []byte(fmt.Sprintf(`{"error":{"type":"upstream_error","message":"上游首字节超时特征串 %s 命中"}}`, quoted))
	return &service.UpstreamFailoverError{
		StatusCode:   http.StatusGatewayTimeout,
		ResponseBody: body,
		// Reason 故意留空，模拟"错误体恰好带特征串但语义未标记"的现场。
		Reason: "",
	}
}

// TestM1cCNFirstByteTimeout_StreamingEmitsDistinguishableFrame 覆盖 CN 路径形态（客户现场）：
// StatusCode=504、ResponseBody type=upstream_error、但 Reason==gateway_first_byte_timeout。
// 断言写回的是流内 SSE error 帧（含 data: 前缀）且带 service.UpstreamFirstByteTimeoutType，
// 不是通用 upstream_error 文案。
func TestM1cCNFirstByteTimeout_StreamingEmitsDistinguishableFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, m1cCNFirstByteTimeoutFailover(), true)

	body := w.Body.String()

	// 必须是流内 SSE error 帧（含 data: 前缀），而非纯 JSON 错误体。
	assert.Contains(t, body, "event: error", "CN 路径须写流内 SSE error 帧，实际响应: %q", body)
	assert.Contains(t, body, "data: ", "流内帧须带 data: 前缀")

	parsed := extractSSEErrorFrame(t, body)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "错误帧应含 error 对象，实际: %v", parsed)

	// 关键：CN 路径（错误体 type=upstream_error）也必须被识别为首字节超时，写出可区分 type。
	assert.Equal(t, service.UpstreamFirstByteTimeoutType, errorObj["type"],
		"CN 路径（错误体 type=upstream_error）也必须写回可区分的首字节超时 type，而非通用 upstream_error")
	assert.Equal(t, service.UpstreamFirstByteTimeoutType, errorObj["code"],
		"CN 路径错误帧 code 必须与可区分 type 一致")
	// 反向断言：不得仍是通用 upstream_error 文案（漏网现场的产物）。
	assert.NotEqual(t, "upstream_error", errorObj["type"],
		"CN 路径不得落回通用 upstream_error type")

	msg, _ := errorObj["message"].(string)
	require.NotEmpty(t, msg, "错误帧必须携带 message")
	assertNoCancellationLikeWords(t, msg)
	assert.Equal(t, errors.UpstreamFirstByteTimeoutStreamFrame, msg,
		"流已开始时应引用流内错误帧专属 SSOT 文案")
}

// TestM1cTrapEmptyReason_NotTreatedAsFirstByteTimeout 覆盖核心验收陷阱（证明判定已从字符串
// 匹配改为语义标记）：StatusCode=504、Reason 为空，但 ResponseBody 恰好含
// "upstream_first_byte_timeout" 特征串。断言它**不得**被当作首字节超时——必须落回通用 504
// 映射（type==upstream_error），而非可区分的 service.UpstreamFirstByteTimeoutType。
func TestM1cTrapEmptyReason_NotTreatedAsFirstByteTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	h := &OpenAIGatewayHandler{}
	h.handleFailoverExhausted(c, m1cTrapEmptyReasonFailover(), true)

	body := w.Body.String()

	// 仍会走通用映射写出 SSE error 帧（event: error），只是 type 是通用的 upstream_error。
	assert.Contains(t, body, "event: error", "陷阱用例仍应写出 SSE error 帧，实际响应: %q", body)

	parsed := extractSSEErrorFrame(t, body)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "错误帧应含 error 对象，实际: %v", parsed)

	gotType, _ := errorObj["type"].(string)
	// 核心断言：因 Reason 为空，绝不命中首字节超时分支，落回通用 504 映射。
	assert.NotEqual(t, service.UpstreamFirstByteTimeoutType, gotType,
		"Reason 为空的特征串陷阱不得被当作首字节超时（证明判定已改语义标记）")
	assert.Equal(t, "upstream_error", gotType,
		"陷阱用例（Reason 空）应落回通用 504 映射 type=upstream_error")
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
