package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// QK-S2R2 P1-1 / P2: 掩码账号未命中规则的流式 failed/error 帧，其携带的
// error.message / response.error.message 须在通用写出前就地清洗；非掩码账号
// 逐字节不受影响；重叠标识（qwen 与 qwen3.8-flash）须因定序而一并洗净。

// --- 纯函数测试：maskStreamingFailedErrorMessageForClient ---

func TestMaskedAccountError_StreamFailedEventMaskedPayload(t *testing.T) {
	masked := newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})

	// response.failed 帧：response.error.message 被清洗，不残留标识/上游名。
	out := maskStreamingFailedErrorMessageForClient(masked,
		[]byte(`{"type":"response.failed","response":{"error":{"message":"qwen3.8-flash unavailable at tokenharbor"}}}`))
	require.NotNil(t, out)
	assert.NotContains(t, string(out), "qwen3.8-flash")
	assert.NotContains(t, string(out), "tokenharbor")
	assert.Contains(t, string(out), maskedUpstreamModelPlaceholder)
	assert.Contains(t, string(out), "response.failed")

	// 裸 error 帧：error.message 被清洗。
	out = maskStreamingFailedErrorMessageForClient(masked,
		[]byte(`{"type":"error","error":{"type":"upstream_error","message":"qwen3.8-flash unavailable at tokenharbor"}}`))
	require.NotNil(t, out)
	assert.NotContains(t, string(out), "qwen3.8-flash")
	assert.NotContains(t, string(out), "tokenharbor")

	// response.error.message 存在但为空（无消息可洗）时 fail-closed 落到固定文案。
	out = maskStreamingFailedErrorMessageForClient(masked,
		[]byte(`{"type":"response.failed","response":{"error":{"message":""}}}`))
	require.NotNil(t, out)
	assert.Contains(t, string(out), infraerrors.UpstreamRequestFailed)

	// 非掩码账号：不改写（返回 nil）。
	plain := newPlainTestAccount(2, map[string]any{"gpt-4": "qwen3.8-flash"})
	out = maskStreamingFailedErrorMessageForClient(plain,
		[]byte(`{"type":"response.failed","response":{"error":{"message":"qwen3.8-flash unavailable"}}}`))
	assert.Nil(t, out)

	// nil 账号：不改写。
	assert.Nil(t, maskStreamingFailedErrorMessageForClient(nil,
		[]byte(`{"type":"response.failed","response":{"error":{"message":"x"}}}`)))
}

// TestMaskedAccountError_StreamOverlapIdentifiers 验证 P2 定序：mapping 值同时
// 含 "qwen" 与 "qwen3.8-flash"，上游文案同时含两者时，长标识先替换、短标识不再
// 把长标识破坏成残渣，两者均不残留。
func TestMaskedAccountError_StreamOverlapIdentifiers(t *testing.T) {
	masked := newMaskedTestAccount(1, map[string]any{
		"gpt-4":   "qwen3.8-flash",
		"gpt-4.1": "qwen",
	})

	raw := `{"type":"response.failed","response":{"error":{"message":"both qwen3.8-flash and qwen are unavailable at tokenharbor"}}}`
	out := maskStreamingFailedErrorMessageForClient(masked, []byte(raw))
	require.NotNil(t, out)
	got := string(out)
	assert.NotContains(t, got, "qwen3.8-flash", "长标识残留")
	assert.NotContains(t, got, "qwen", "短标识残留（含 3.8-flash 片段应一并洗净）")
	assert.NotContains(t, got, "tokenharbor")
	assert.Contains(t, got, maskedUpstreamModelPlaceholder)
}

// --- 集成测试：经 handleStreamingResponse 真实写出路径 ---

func runMaskedStreamFailedFixture(t *testing.T, account *Account, failedEvent string) string {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	// 先产出可见输出，使后续 failed/error 帧走"通用写出"而非 failover 早返回。
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			"",
			"event: response.output_text.delta",
			`data: {"type":"response.output_text.delta","delta":"hi"}`,
			"",
			failedEvent,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-mask-leak"}},
	}

	_, _ = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Time{}, "gpt-4", "gpt-4")
	return rec.Body.String()
}

func TestMaskedAccountError_StreamFailedEventNoLeak(t *testing.T) {
	masked := newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
	body := runMaskedStreamFailedFixture(t, masked,
		`data: {"type":"response.failed","response":{"id":"resp_1","error":{"type":"upstream_error","message":"qwen3.8-flash unavailable at tokenharbor"}}}`)

	require.Contains(t, body, "response.failed")
	require.NotContains(t, body, "qwen3.8-flash", "掩码账号 failed 帧不得泄漏上游模型名")
	require.NotContains(t, body, "tokenharbor", "掩码账号 failed 帧不得泄漏 tokenharbor 痕迹")
	require.Contains(t, body, maskedUpstreamModelPlaceholder)
}

func TestMaskedAccountError_StreamBareErrorNoLeak(t *testing.T) {
	masked := newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
	body := runMaskedStreamFailedFixture(t, masked,
		`data: {"type":"error","error":{"type":"upstream_error","message":"qwen3.8-flash unavailable at tokenharbor"}}`)

	require.Contains(t, body, `"type":"error"`)
	require.NotContains(t, body, "qwen3.8-flash", "掩码账号裸 error 帧不得泄漏上游模型名")
	require.NotContains(t, body, "tokenharbor")
	require.Contains(t, body, maskedUpstreamModelPlaceholder)
}

func TestMaskedAccountError_StreamFailedEventPlainUnchanged(t *testing.T) {
	// 零影响对照：非掩码（真实 kimi）账号同事件，消息原样保留。
	plain := newPlainTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
	body := runMaskedStreamFailedFixture(t, plain,
		`data: {"type":"response.failed","response":{"id":"resp_1","error":{"type":"upstream_error","message":"qwen3.8-flash unavailable"}}}`)

	require.Contains(t, body, "response.failed")
	require.Contains(t, body, "qwen3.8-flash", "非掩码账号消息须逐字节不变")
	require.Contains(t, body, "qwen3.8-flash unavailable")
}

func TestMaskedAccountError_StreamOverlapIdentifiersNoLeak(t *testing.T) {
	masked := newMaskedTestAccount(1, map[string]any{
		"gpt-4":   "qwen3.8-flash",
		"gpt-4.1": "qwen",
	})
	body := runMaskedStreamFailedFixture(t, masked,
		`data: {"type":"response.failed","response":{"id":"resp_1","error":{"type":"upstream_error","message":"both qwen3.8-flash and qwen are unavailable at tokenharbor"}}}`)

	require.Contains(t, body, "response.failed")
	require.NotContains(t, body, "qwen3.8-flash", "长标识残留")
	require.NotContains(t, body, "qwen", "短标识残留（含 3.8-flash 片段应一并洗净）")
	require.NotContains(t, body, "tokenharbor")
}

// TestReplaceModelIn_IdentifiersOrderedLongestFirst 验证 P2 定序落地：
// maskedAccountUpstreamModelIdentifiers 返回按长度降序，等长保持稳定。
func TestReplaceModelIn_IdentifiersOrderedLongestFirst(t *testing.T) {
	acct := newMaskedTestAccount(1, map[string]any{
		"gpt-4":   "qwen",
		"gpt-4.1": "qwen3.8-flash",
	})
	ids := maskedAccountUpstreamModelIdentifiers(acct)
	require.GreaterOrEqual(t, len(ids), 2)
	// 长标识在前。
	longIdx, shortIdx := -1, -1
	for i, id := range ids {
		if id == "qwen3.8-flash" {
			longIdx = i
		}
		if id == "qwen" {
			shortIdx = i
		}
	}
	require.NotEqual(t, -1, longIdx, "qwen3.8-flash 应在列表内")
	require.NotEqual(t, -1, shortIdx, "qwen 应在列表内")
	require.Less(t, longIdx, shortIdx, "长标识须排在短标识之前（定序）")
}
