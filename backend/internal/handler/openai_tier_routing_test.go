//go:build unit

package handler

import (
	"context"
	"errors"
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

// newTierRoutingTestCtx 构造一个带 request_id 的 gin 测试上下文与可观测日志 core。
func newTierRoutingTestCtx(t *testing.T, requestID string) (*gin.Context, *zap.Logger, *observer.ObservedLogs) {
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

// gateFunc 是 applyTierRouting 注入的动态门控闭包（测试可编排开门/关门）。
type gateFunc func(model string) (bool, error)

func openGate(model string) (bool, error)   { return true, nil }
func closedGate(model string) (bool, error) { return false, nil }

// TestTierRoutingApplyRoutingNonTargetModelPassthrough 验证「同平台非目标模型」零装门零行为
// 变化（派发单 OC-T1，测试 #2）：即使平台命中（kimi），只要请求模型不在目标集合，直接返回
// 原 baseCtx，不输出分层路由日志，亦不依赖门控结果。
func TestTierRoutingApplyRoutingNonTargetModelPassthrough(t *testing.T) {
	for _, model := range []string{"kimi-k2", "glm-5.3", "deepseek-v4-pro", "gpt-5.1"} {
		c, reqLog, logs := newTierRoutingTestCtx(t, "req-nontarget-"+model)
		baseCtx := context.Background()
		out, err := applyTierRouting(c, baseCtx, reqLog, model, model,
			service.TierRoutingProtocolChatCompletions, []byte(`{"model":"`+model+`"}`), openGate)
		require.NoError(t, err)
		require.True(t, out == baseCtx, "non-target model must pass through base ctx unchanged: %s", model)
		require.Equal(t, 0, logs.Len(), "no tier log for non-target model: %s", model)
	}
}

// TestTierRoutingApplyRoutingGateClosedPassthrough 验证目标模型但动态门控关闭时零装门
// （派发单 OC-T1，动态门三态 ①②）：返回原 baseCtx，不输出分层路由日志。
func TestTierRoutingApplyRoutingGateClosedPassthrough(t *testing.T) {
	for _, model := range []string{"kimi-k3", "deepseek-v4.1-flash", "glm-5.3-flash"} {
		c, reqLog, logs := newTierRoutingTestCtx(t, "req-closed-"+model)
		body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello world"}]}`)
		out, err := applyTierRouting(c, c.Request.Context(), reqLog, model, model,
			service.TierRoutingProtocolChatCompletions, body, closedGate)
		require.NoError(t, err)
		require.True(t, baseCtxUnchanged(c, out), "gate closed must pass through ctx unchanged: %s", model)
		require.Equal(t, 0, logs.Len(), "no tier log when gate closed: %s", model)
	}
}

func baseCtxUnchanged(c *gin.Context, out context.Context) bool {
	return out == c.Request.Context()
}

// TestTierRoutingApplyRoutingGateErrorPassthrough 验证门控查询返回 error（基础设施异常）时
// 不装分档门、回落原路径（ctx 不变、无分层路由日志）；并按 FIX-5 要求以 Warn 级别（非 Debug）
// 记录（沿用既有 reqLog 体系，不新增机制）。
func TestTierRoutingApplyRoutingGateErrorPassthrough(t *testing.T) {
	errGate := func(string) (bool, error) { return false, errors.New("gate query boom") }
	for _, model := range []string{"kimi-k3", "deepseek-v4.1-flash", "glm-5.3-flash"} {
		c, reqLog, logs := newTierRoutingTestCtx(t, "req-gateerr-"+model)
		body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello world"}]}`)
		out, err := applyTierRouting(c, c.Request.Context(), reqLog, model, model,
			service.TierRoutingProtocolChatCompletions, body, errGate)
		require.NoError(t, err, "gate error must fall back to passthrough, not surface as failure: %s", model)
		require.True(t, baseCtxUnchanged(c, out), "gate error must pass through ctx unchanged: %s", model)
		require.Equal(t, 1, logs.Len(), "gate error must emit exactly one log line at Warn: %s", model)
		require.Equal(t, zapcore.WarnLevel, logs.All()[0].Level, "gate error log must be Warn level (FIX-5): %s", model)
	}
}

// TestTierRoutingApplyRoutingTargetModelGateOpen 验证目标模型且门开启时装门返回新 ctx，
// 并输出一行结构化日志（request_id + 估算 input + 分档 small/big）。=2000 判小的精确边界
// 由 TestTierRoutingClassForEstimate 在纯分档函数层确定性覆盖（派发单 OC-T1，测试 #2 + #1）。
func TestTierRoutingApplyRoutingTargetModelGateOpen(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantTier string
	}{
		{"small_below", "hello world", "small"},
		{"small_above_old_threshold", repeatWords(1500), "small"},
		{"big_above", repeatWords(2500), "big"},
	}
	for _, model := range []string{"kimi-k3", "deepseek-v4.1-flash", "glm-5.3-flash"} {
		for _, tc := range cases {
			c, reqLog, logs := newTierRoutingTestCtx(t, "req-valid-"+model+"-"+tc.name)
			body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"` + tc.content + `"}]}`)
			out, err := applyTierRouting(c, c.Request.Context(), reqLog, model, model,
				service.TierRoutingProtocolChatCompletions, body, openGate)
			require.NoError(t, err, "%s/%s", model, tc.name)
			require.NotNil(t, out)
			require.NotSame(t, c.Request.Context(), out, "target model must return a tier-decorated ctx: %s/%s", model, tc.name)

			entries := logs.FilterMessage("kimi_tier_routing_decision").All()
			require.Len(t, entries, 1, "exactly one structured tier log line expected: %s/%s", model, tc.name)
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
			require.Equal(t, "req-valid-"+model+"-"+tc.name, gotRequestID)
			require.Equal(t, tc.wantTier, gotTier, "tier misclassify: %s/%s", model, tc.name)
			require.GreaterOrEqual(t, gotEstimated, 0)
			if tc.name == "small_above_old_threshold" {
				require.Greater(t, gotEstimated, 1000)
				require.LessOrEqual(t, gotEstimated, service.TierRoutingInputThreshold)
			}
		}
	}
}

// TestTierRoutingApplyRoutingEstimateFailClosed 验证估算失败（非法请求体）返回错误，
// 不默认大档、不静默放行（派发单 OC-T1，测试 #3 fail-closed）。
func TestTierRoutingApplyRoutingEstimateFailClosed(t *testing.T) {
	c, reqLog, logs := newTierRoutingTestCtx(t, "req-fail")
	out, err := applyTierRouting(c, c.Request.Context(), reqLog, "kimi-k3", "kimi-k3",
		service.TierRoutingProtocolChatCompletions, []byte("not-json"), openGate)
	require.Error(t, err)
	require.Nil(t, out, "estimate failure must not yield a tier ctx (no default-to-big)")
	require.Equal(t, 0, logs.Len(), "no tier log on estimate failure")
}

// repeatWords 生成约 n 个词的文本（每个词约占 1 token），用于构造 2000 附近的边界。
func repeatWords(n int) string {
	const word = " word"
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*len(word))
	for i := 0; i < n; i++ {
		b = append(b, word...)
	}
	return string(b)
}

// TestTierRoutingApplyRoutingRoutingVsEstimateModelSplit 覆盖 FIX-C（routingModel≠estimateModel
// 回归，派发单 OC-T1 泛化 FIX-2）：当请求侧模型存在映射（映射名与请求侧名不同）时，动态门控
// 必须以请求侧模型（routingModel）做目标集合判定与 model_mapping 键查询，而 token 估算必须以
// 映射后模型（estimateModel）进行。用映射 {"glm-5.3-flash":"gpt-4"} 场景断言：
//   - ① gateOpen 收到的键 = 请求侧 glm-5.3-flash（而非映射名 gpt-4）；
//   - ② token 估算使用映射名 gpt-4（与请求侧 glm-5.3-flash 走不同 tokenizer，估算值可确定区分）；
//   - ③ 映射名 gpt-4 不在目标集合，但门仍开启（证明门用请求侧名而非映射名做目标判定，不误关门）。
func TestTierRoutingApplyRoutingRoutingVsEstimateModelSplit(t *testing.T) {
	const routingModel = "glm-5.3-flash"
	const estimateModel = "gpt-4" // 映射后上游名：Cl100k tokenizer，与 glm 的 O200k 不同；且不在目标集合
	c, reqLog, logs := newTierRoutingTestCtx(t, "req-split")
	// CJK 内容使 Cl100k 与 O200k 分词结果可确定区分（同英文文本两 codec 计数常相同）。
	body := []byte(`{"model":"` + routingModel + `","messages":[{"role":"user","content":"你好世界这是一段中文测试文本用于分词差异验证"}]}`)

	var gotGateModel string
	gate := func(m string) (bool, error) {
		gotGateModel = m
		return true, nil
	}

	out, err := applyTierRouting(c, c.Request.Context(), reqLog, routingModel, estimateModel,
		service.TierRoutingProtocolChatCompletions, body, gate)
	require.NoError(t, err)

	// ① 门控必须以请求侧名判定，不得误用映射名。
	require.Equal(t, routingModel, gotGateModel, "gateOpen 必须收到请求侧模型 glm-5.3-flash，而非映射名 gpt-4")

	// ③ 映射名不在目标集合，但门仍开启：证明门按请求侧名做目标判定（不误关门）。
	require.NotSame(t, c.Request.Context(), out, "门应开启（映射名不在目标集合也不误关门）")
	entries := logs.FilterMessage("kimi_tier_routing_decision").All()
	require.Len(t, entries, 1)
	var gotEstimated int
	for _, f := range entries[0].Context {
		if f.Key == "estimated_input_tokens" {
			gotEstimated = int(f.Integer)
		}
	}

	// ② token 估算必须使用映射名 estimateModel：两模型走不同 tokenizer，估算值可确定区分。
	expEstimate, errE := service.EstimateTierRoutingInputTokens(estimateModel, service.TierRoutingProtocolChatCompletions, body)
	require.NoError(t, errE)
	expRouting, errR := service.EstimateTierRoutingInputTokens(routingModel, service.TierRoutingProtocolChatCompletions, body)
	require.NoError(t, errR)
	require.NotEqual(t, expEstimate, expRouting, "映射名与请求侧名走不同 tokenizer，估算值应可确定区分")
	require.Equal(t, expEstimate, gotEstimated, "token 估算必须使用映射名 gpt-4（而非请求侧 glm-5.3-flash）")
}
