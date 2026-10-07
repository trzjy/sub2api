//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// newKimiTierTestCtx 构造一个带 request_id 的 gin 测试上下文与可观测日志 core。
func newKimiTierTestCtx(t *testing.T, requestID string) (*gin.Context, *zap.Logger, *observer.ObservedLogs) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	base := context.WithValue(context.Background(), ctxkey.RequestID, requestID)
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(base)
	c.Request = req
	core, logs := observer.New(zapcore.InfoLevel)
	return c, zap.New(core), logs
}

// TestKimiTierApplyRoutingNonKimiPassthrough 验证非 kimi 平台请求零行为变化：
// 直接返回原 baseCtx，且不输出分层路由日志。
func TestKimiTierApplyRoutingNonKimiPassthrough(t *testing.T) {
	c, reqLog, logs := newKimiTierTestCtx(t, "req-nonkimi")
	baseCtx := context.Background()
	out, err := applyKimiTierRouting(c, baseCtx, reqLog, service.PlatformOpenAI, "gpt-5.1", service.KimiTierProtocolChatCompletions, []byte(`{"model":"gpt-5.1"}`))
	require.NoError(t, err)
	require.True(t, out == baseCtx, "non-kimi platform must pass through base ctx unchanged")
	require.Equal(t, 0, logs.Len(), "no kimi tier log for non-kimi platform")
}

// TestKimiTierApplyRoutingValidEmitsLog 验证 kimi 平台请求估算成功：装门返回新 ctx，
// 并输出一行结构化日志（request_id + 估算 input + 分档 small/big）。
func TestKimiTierApplyRoutingValidEmitsLog(t *testing.T) {
	c, reqLog, logs := newKimiTierTestCtx(t, "req-valid")
	body := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hello world"}]}`)
	out, err := applyKimiTierRouting(c, c.Request.Context(), reqLog, service.PlatformKimi, "kimi-k3", service.KimiTierProtocolChatCompletions, body)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotSame(t, c.Request.Context(), out, "kimi platform must return a tier-decorated ctx")

	entries := logs.FilterMessage("kimi_tier_routing_decision").All()
	require.Len(t, entries, 1, "exactly one structured kimi tier log line expected")
	// 按字段名断言结构化日志的 request_id + 估算 input + 分档。
	var gotRequestID, gotTier string
	var gotEstimated int
	for _, f := range entries[0].Context {
		switch f.Key {
		case "request_id":
			gotRequestID = f.String
		case "tier":
			gotTier = f.String
		case "estimated_input_tokens":
			gotEstimated = int(f.Integer)
		}
	}
	require.Equal(t, "req-valid", gotRequestID)
	require.Equal(t, "small", gotTier, "short prompt must classify as small")
	require.GreaterOrEqual(t, gotEstimated, 0)
}

// TestKimiTierApplyRoutingEstimateFailClosed 验证估算失败（非法请求体）返回错误，
// 不默认大档、不静默放行（派发单 B2，Done when #2 fail-closed）。
func TestKimiTierApplyRoutingEstimateFailClosed(t *testing.T) {
	c, reqLog, logs := newKimiTierTestCtx(t, "req-fail")
	out, err := applyKimiTierRouting(c, c.Request.Context(), reqLog, service.PlatformKimi, "kimi-k3", service.KimiTierProtocolChatCompletions, []byte("not-json"))
	require.Error(t, err)
	require.Nil(t, out, "estimate failure must not yield a tier ctx (no default-to-big)")
	require.Equal(t, 0, logs.Len(), "no tier log on estimate failure")
}
