//go:build unit

package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestAttachCursorEstimatedInputTokensSetsEstimateForCursorPlatform 覆盖 KC-19B 接线：
// cursor 平台（kn-cy 等 OpenAI 兼容反代）请求用既有本地估算 SSOT 装好估算 input，
// 同一 tierRoutingContextKey 通道可读回（RecordUsage 即经此读取）。空 class → 不装分档门，
// 与既有分层路由零冲突。
func TestAttachCursorEstimatedInputTokensSetsEstimateForCursorPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hello world this is a test"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	reqLog := zap.NewNop()

	attachCursorEstimatedInputTokens(c, reqLog, service.PlatformCursor, "gpt-6.1-sol", service.TierRoutingProtocolChatCompletions, body)

	got := service.TierRoutingEstimatedInputTokens(c.Request.Context())
	require.Greater(t, got, 0, "cursor 平台请求必须把本地估算装进 ctx")
}

// TestAttachCursorEstimatedInputTokensNoopForNonCursorPlatform 锁死「非 cursor 零变化」：
// OpenAI 平台请求不装估算（ctx 读到 0），既有分层路由/计费不受影响。
func TestAttachCursorEstimatedInputTokensNoopForNonCursorPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	reqLog := zap.NewNop()

	attachCursorEstimatedInputTokens(c, reqLog, service.PlatformOpenAI, "gpt-4o", service.TierRoutingProtocolChatCompletions, body)

	require.Equal(t, 0, service.TierRoutingEstimatedInputTokens(c.Request.Context()),
		"非 cursor 平台不得装估算值")
}

// TestAttachCursorEstimatedInputTokensFailOpenOnBadBody 锁死失败开放：估算失败
// （非法请求体）仅告警、不装门、不 panic，请求照常继续原路径。
func TestAttachCursorEstimatedInputTokensFailOpenOnBadBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := []byte("not-a-json-body")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	reqLog := zap.NewNop()

	require.NotPanics(t, func() {
		attachCursorEstimatedInputTokens(c, reqLog, service.PlatformCursor, "gpt-6.1-sol", service.TierRoutingProtocolChatCompletions, body)
	})
	require.Equal(t, 0, service.TierRoutingEstimatedInputTokens(c.Request.Context()),
		"估算失败必须失败开放：不装门")
}

// TestRecordUsageEstimatedInputTokensFilledFromContext 覆盖 KC-19B 透传契约：handler 在
// 异步提交「之前」用同样的 service.TierRoutingEstimatedInputTokens 从请求 ctx 取值并填入
// OpenAIRecordUsageInput.EstimatedInputTokens（异步闭包不再从 ctx 取，避免 ctx 已失效）。
// 此处复刻生产 submitChatUsage 的取数-填充顺序，断言 cursor 平台估算被透传、非 cursor 为 0。
func TestRecordUsageEstimatedInputTokensFilledFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// cursor 平台：估算被透传。
	cursorRecorder := httptest.NewRecorder()
	cc, _ := gin.CreateTestContext(cursorRecorder)
	cursorBody := []byte(`{"model":"claude-fable-5-1","messages":[{"role":"user","content":"count these tokens please"}]}`)
	cc.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(cursorBody))
	attachCursorEstimatedInputTokens(cc, zap.NewNop(), service.PlatformCursor, "claude-fable-5-1", service.TierRoutingProtocolMessages, cursorBody)

	estimatedCursor := service.TierRoutingEstimatedInputTokens(cc.Request.Context())
	require.Greater(t, estimatedCursor, 0)
	input := &service.OpenAIRecordUsageInput{EstimatedInputTokens: estimatedCursor}
	require.Equal(t, estimatedCursor, input.EstimatedInputTokens, "cursor 估算必须透传进 RecordUsage 输入")

	// 非 cursor 平台：取数为 0，填入 0（消费端按账号类型决定是否采用）。
	otherRecorder := httptest.NewRecorder()
	oc, _ := gin.CreateTestContext(otherRecorder)
	otherBody := []byte(`{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`)
	oc.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(otherBody))
	attachCursorEstimatedInputTokens(oc, zap.NewNop(), service.PlatformOpenAI, "gemini-4-argon", service.TierRoutingProtocolMessages, otherBody)

	estimatedOther := service.TierRoutingEstimatedInputTokens(oc.Request.Context())
	require.Equal(t, 0, estimatedOther)
	otherInput := &service.OpenAIRecordUsageInput{EstimatedInputTokens: estimatedOther}
	require.Equal(t, 0, otherInput.EstimatedInputTokens, "非 cursor 平台填入 0（回落上游 actualInputTokens）")
}
