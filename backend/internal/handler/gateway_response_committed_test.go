//go:build unit

package handler

// 文件：internal/handler/gateway_response_committed_test.go
//
// 2026-11-11 根因修复的回归测试（并发满立即拒绝改造的连带回归）。
//
// 回归形态（已由生产日志与代码链路闭合证实）：
//   - 「客户端维度并发满立即拒绝」改造删掉了准入阶段的 SSE ping；
//   - Anthropic 路径的 streamStarted 因此恒为 false（唯一按指针传入的等待路径
//     AcquireAccountSlotWithWaitTimeout 已不再置真）；
//   - 而 CF524 上游心跳在请求入口安装（gateway_handler.go:264），15s 后就提交
//     SSE 200，早于账号维度等待超时（30s/45s）；
//   - 于是超时那一刻：响应早已提交 200，但错误出口看到 streamStarted=false，
//     走了 JSON 分支 → 真实 503 丢失（客户端只看到 200）+ JSON 错误体塞进 SSE 流。
//
// 本文件锁死的事实源契约（gateway_response_committed.go）：
//  1. gatewayResponseCommitted：已提交判定 = streamStarted || c.Writer.Written()
//     || service.IsResponseCommitted(c) 三信号取或；
//  2. gatewayResponseCommittedAsSSE：在 1 的基础上排除「已提交为 JSON 文档」的
//     形态（OpenAI 图片接口非流式空白保活）——那种响应后续错误仍须写 JSON 体；
//  3. gatewayStreamHasOnlyHeartbeats：心跳字节口径 = handler 层 key + service 层
//     CF524 心跳 CommittedBytes()（此前只算前者 → /v1/responses 漏补 response.failed）。
//
// 反证要求：把事实源退回到「只看 streamStarted」的基线形态后，本文件用例必须 FAIL。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCommittedCtx 造一个最小 gin 测试上下文。
func newCommittedCtx(path string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	return c, w
}

// startCommittedHeartbeat 造一个「已提交 SSE 200 且已写出 keep-alive 帧」的
// CF524 上游心跳，并挂进请求 context。
//
// 手法：HeartbeatDelaySeconds=0 让 Start() 立即提交 SSE 200 响应头（committedBytes
// 此时仍为 0——响应头不是 body 字节）；随后 Resume() 在「已提交」分支里**立刻写一帧**
// keep-alive（不重等 delay），committedBytes 随之变为 13。
//
// 为什么不用等 keep-alive 间隔（15s，service 包私有常量）：handler 包无法调小它，
// 而 Resume() 的语义正是「已提交后立即补一帧」，既快又贴近生产形态
// （生产上心跳首次写帧前会先提交 SSE 200 头，字节记账与本测试一致）。
func startCommittedHeartbeat(t *testing.T, c *gin.Context) *service.UpstreamHeartbeat {
	t.Helper()
	hb := service.NewUpstreamHeartbeat(c.Request.Context(), c.Writer,
		service.NewRequestBudgetSnapshot(250, 0, time.Now(), true), time.Now())
	ctx := service.WithUpstreamHeartbeat(c.Request.Context(), hb)
	c.Request = c.Request.WithContext(ctx)

	require.NoError(t, hb.Start())
	// 等首帧窗口：delay=0 时应当立刻提交 SSE 200 头。
	require.Eventually(t, func() bool {
		return hb.IsCommitted()
	}, 2*time.Second, 5*time.Millisecond, "前置条件：心跳必须已提交 SSE 200")

	// Resume 在「已提交」分支里立刻写一帧 keep-alive，并续跑 keep-alive 循环。
	require.NoError(t, hb.Resume())
	require.Eventually(t, func() bool {
		return hb.CommittedBytes() > 0
	}, 2*time.Second, 5*time.Millisecond, "前置条件：心跳必须已写出 keep-alive 帧")
	return hb
}

// ---------------------------------------------------------------------------
// 1. 事实源本身的三信号语义
// ---------------------------------------------------------------------------

// TestGatewayResponseCommitted_ThreeSignals 锁死「已提交」的三信号取或语义。
func TestGatewayResponseCommitted_ThreeSignals(t *testing.T) {
	t.Run("三信号全 false → 未提交", func(t *testing.T) {
		c, _ := newCommittedCtx(EndpointMessages)
		assert.False(t, gatewayResponseCommitted(c, false),
			"响应未写出、未标记提交、streamStarted 为 false 时不得判为已提交")
	})

	t.Run("streamStarted 为 true → 已提交", func(t *testing.T) {
		c, _ := newCommittedCtx(EndpointMessages)
		assert.True(t, gatewayResponseCommitted(c, true),
			"调用方已知流已开始时必须判为已提交")
	})

	t.Run("writer 已写出 → 已提交", func(t *testing.T) {
		c, _ := newCommittedCtx(EndpointMessages)
		_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
		require.True(t, c.Writer.Written(), "前置条件：writer 必须已写出")
		assert.True(t, gatewayResponseCommitted(c, false),
			"CF524 心跳提交 SSE 200 后，即使 streamStarted 为 false 也必须判为已提交")
	})

	t.Run("service 层已标记提交 → 已提交", func(t *testing.T) {
		c, _ := newCommittedCtx(EndpointMessages)
		service.MarkResponseCommitted(c)
		assert.True(t, gatewayResponseCommitted(c, false),
			"service 层已向客户端提交响应时必须判为已提交")
	})
}

// TestGatewayResponseCommittedAsSSE_ExcludesJSONDocument 锁死「已提交为 JSON 文档」
// 不算 SSE 流：OpenAI 图片接口的非流式空白保活提交的是 application/json，
// 此时错误必须仍写成 JSON 体，塞 SSE 帧会破坏 JSON。
func TestGatewayResponseCommittedAsSSE_ExcludesJSONDocument(t *testing.T) {
	t.Run("无图片保活 → 已提交即 SSE", func(t *testing.T) {
		c, _ := newCommittedCtx(EndpointMessages)
		_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
		assert.True(t, gatewayResponseCommittedAsSSE(c, false),
			"普通 SSE 流已提交后必须判为 SSE 形态")
	})

	t.Run("图片 JSON 保活 → 不算 SSE", func(t *testing.T) {
		c, _ := newCommittedCtx("/v1/images/generations")
		stop := service.StartOpenAIImagesJSONKeepalive(c, 5*time.Millisecond)
		defer stop()
		// 等第一拍：提交 200 + 写出空白 padding。
		require.Eventually(t, func() bool {
			return c.Writer.Written()
		}, 2*time.Second, 5*time.Millisecond, "前置条件：图片保活必须已提交响应")

		assert.True(t, gatewayResponseCommitted(c, false),
			"前置条件：响应确实已提交")
		assert.False(t, gatewayResponseCommittedAsSSE(c, false),
			"图片 JSON 保活提交的是 application/json，不得判为 SSE 形态")
	})
}

// ---------------------------------------------------------------------------
// 2. Anthropic 路径的回归：账号维度等待超时 + 心跳已提交 200
// ---------------------------------------------------------------------------

// TestAnthropicConcurrencyError_HeartbeatCommitted_WritesStreamFrame 钉死本次回归
// 的核心形态：CF524 心跳已提交 SSE 200、streamStarted 仍为 false 时，账号维度并发
// 超时必须写成流内 SSE 错误帧，绝不能走 JSON 分支。
//
// 反证：把 gateway_handler.go 的 handleStreamingAwareErrorWithCode 退回
// 「只看 streamStarted」的基线形态后，本用例必须 FAIL（响应体会变成 JSON 错误体，
// 且真实 503 丢失——状态码已固化为 200）。
func TestAnthropicConcurrencyError_HeartbeatCommitted_WritesStreamFrame(t *testing.T) {
	c, w := newCommittedCtx(EndpointMessages)
	c.Header("Content-Type", "text/event-stream")

	// 心跳已提交 SSE 200（写 keep-alive 注释帧，与生产心跳 writeFrame 同型）。
	_, _ = c.Writer.WriteString(": keep-alive\n\n")
	require.True(t, c.Writer.Written(), "前置条件：心跳必须已提交响应")

	// 关键：streamStarted 仍为 false（准入 ping 已删，账号维度等待路径不再置真）。
	h := &GatewayHandler{}
	h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error",
		"upstream account concurrency limit", false)

	body := w.Body.String()

	// 必须写流内 SSE 错误帧（含 data: 前缀）。
	require.Contains(t, body, "data:",
		"心跳已提交 200 后，错误必须写成流内 SSE 帧而非 JSON 体，实际: %q", body)
	require.Contains(t, body, `"type":"error"`,
		"流内帧必须是 Anthropic 形状的 error 事件，实际: %q", body)

	// 反例：不得是纯 JSON 错误体。
	require.False(t, strings.HasPrefix(strings.TrimSpace(body), "{\"error\""),
		"不得把 JSON 错误体塞进已开的 SSE 流，实际: %q", body)
}

// TestAnthropicConcurrencyError_NotCommitted_WritesJSON 覆盖反向：响应确实未提交时
// 必须走 JSON 分支，并带真实状态码（503 不得丢失）。
func TestAnthropicConcurrencyError_NotCommitted_WritesJSON(t *testing.T) {
	c, w := newCommittedCtx(EndpointMessages)
	require.False(t, c.Writer.Written(), "前置条件：响应未提交")

	h := &GatewayHandler{}
	h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error",
		"upstream account concurrency limit", false)

	// 真实状态码必须保留——这正是回归中丢失的东西。
	require.Equal(t, http.StatusServiceUnavailable, w.Code,
		"响应未提交时必须写出真实状态码（回归形态是 503 被固化成 200）")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &parsed),
		"未提交时应写 JSON 错误体，实际: %q", w.Body.String())
	require.NotContains(t, w.Body.String(), "data:",
		"未提交时不得写 SSE 帧")
}

// TestAnthropicFailoverExhausted_HeartbeatCommitted_StreamFrameMessage 覆盖首字节
// 超时耗尽分支的文案选择：心跳已提交 200 时必须引用流内帧专属 SSOT 文案
// （而非 JSON 文案）——两者都引用同一个 type，客户端据此区分。
func TestAnthropicFailoverExhausted_HeartbeatCommitted_StreamFrameMessage(t *testing.T) {
	c, w := newCommittedCtx(EndpointMessages)
	c.Header("Content-Type", "text/event-stream")
	_, _ = c.Writer.WriteString(": keep-alive\n\n")

	h := &GatewayHandler{}
	h.handleFailoverExhausted(c, m1cFirstByteTimeoutFailover(), "anthropic", false)

	body := w.Body.String()
	require.Contains(t, body, "data:",
		"心跳已提交 200 后，耗尽必须写流内帧，实际: %q", body)

	idx := strings.Index(body, "data: ")
	require.GreaterOrEqual(t, idx, 0, "响应体应包含 data: 行，实际: %q", body)
	raw := strings.TrimSpace(body[idx+len("data: "):])
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &parsed),
		"流内帧 JSON 应可解析，原始: %q", raw)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "错误帧应含 error 对象，实际: %v", parsed)

	msg, _ := errorObj["message"].(string)
	assert.Equal(t, errors.UpstreamFirstByteTimeoutStreamFrame, msg,
		"已提交响应的耗尽文案必须是流内帧专属 SSOT")
}

// ---------------------------------------------------------------------------
// 3. /v1/responses 必须补 response.failed 终止事件
// ---------------------------------------------------------------------------

// TestResponsesFailoverExhausted_HeartbeatCommitted_EmitsResponseFailed 钉死次生缺陷：
// CF524 心跳已提交 200 并写出 keep-alive 帧后换号耗尽，必须补 response.failed 终止
// 事件——否则 Codex CLI 报 "stream closed before response.completed"。
//
// 关键点：心跳字节记在 service 层 owner（CommittedBytes），不记在 handler 层
// gatewayStreamHeartbeatBytesKey。旧口径只看 handler 层 → 漏判「只有心跳」→ 漏补终止事件。
func TestResponsesFailoverExhausted_HeartbeatCommitted_EmitsResponseFailed(t *testing.T) {
	c, w := newCommittedCtx(EndpointResponses)
	c.Header("Content-Type", "text/event-stream")

	// 复刻生产形态：CF524 心跳在 service 层 owner 记账并写出 keep-alive 帧。
	// 这里直接用 service 层 owner 写入，使字节记在 CommittedBytes 而非 handler 层 key。
	hb := startCommittedHeartbeat(t, c)
	defer hb.Stop()

	require.True(t, c.Writer.Written(), "前置条件：心跳必须已提交响应")

	h := &GatewayHandler{}
	h.handleResponsesFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusBadGateway,
	}, false)

	body := w.Body.String()
	require.Contains(t, body, "event: response.failed\n",
		"心跳已提交 200 且已写 keep-alive 帧时，必须补 response.failed 终止事件，实际: %q", body)
	require.Contains(t, body, `"type":"response.failed"`,
		"终止事件必须带可区分的 type，实际: %q", body)
}

// TestResponsesFailoverExhausted_OnlyHeartbeatBytes_EmitsResponseFailed 覆盖另一形态：
// 响应只写过心跳字节（handler 层记账路径），同样必须补终止事件。
func TestResponsesFailoverExhausted_OnlyHeartbeatBytes_EmitsResponseFailed(t *testing.T) {
	c, w := newCommittedCtx(EndpointResponses)
	c.Header("Content-Type", "text/event-stream")

	// handler 层记账路径：准入 ping / 用户消息队列 ping。
	// 字节数取实际写入量（": keep-alive\n\n" 为 14 字节），不得写死——
	// 写死会与真实帧长错位，把「只有心跳」误判成「有语义输出」。
	written, err := c.Writer.WriteString(": keep-alive\n\n")
	require.NoError(t, err)
	recordGatewayStreamHeartbeat(c, written)

	h := &GatewayHandler{}
	h.handleResponsesFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusBadGateway,
	}, false)

	body := w.Body.String()
	require.Contains(t, body, "event: response.failed\n",
		"只有心跳字节时也必须补 response.failed 终止事件，实际: %q", body)
}

// ---------------------------------------------------------------------------
// 4. 心跳字节统一口径
// ---------------------------------------------------------------------------

// TestGatewayStreamHasOnlyHeartbeats_UnifiedAccounting 锁死心跳字节口径：
// handler 层 key 与 service 层 CF524 心跳字节两处记账必须相加（互不重叠）。
func TestGatewayStreamHasOnlyHeartbeats_UnifiedAccounting(t *testing.T) {
	t.Run("service 层心跳字节计入", func(t *testing.T) {
		c, w := newCommittedCtx(EndpointResponses)
		c.Header("Content-Type", "text/event-stream")

		hb := startCommittedHeartbeat(t, c)
		defer hb.Stop()

		assert.Positive(t, gatewayStreamHeartbeatBytes(c),
			"service 层 CF524 心跳字节必须计入统一口径")
		assert.True(t, gatewayStreamHasOnlyHeartbeats(c),
			"只写过心跳字节时必须判为「只有心跳」")
		assert.Equal(t, hb.CommittedBytes(), int64(gatewayStreamHeartbeatBytes(c)),
			"统一口径必须等于 service 层心跳字节（handler 层此时无记账）")
		_ = w
	})

	t.Run("两处记账相加", func(t *testing.T) {
		c, _ := newCommittedCtx(EndpointResponses)
		pingBytes, err := c.Writer.WriteString(": keep-alive\n\n")
		require.NoError(t, err)
		recordGatewayStreamHeartbeat(c, pingBytes)
		hb := startCommittedHeartbeat(t, c)
		defer hb.Stop()

		expected := pingBytes + int(hb.CommittedBytes())
		assert.Equal(t, expected, gatewayStreamHeartbeatBytes(c),
			"两处互不重叠的心跳记账必须相加")
	})

	t.Run("无语义输出时判为只有心跳", func(t *testing.T) {
		c, _ := newCommittedCtx(EndpointResponses)
		pingBytes, err := c.Writer.WriteString(": keep-alive\n\n")
		require.NoError(t, err)
		recordGatewayStreamHeartbeat(c, pingBytes)
		assert.True(t, gatewayStreamHasOnlyHeartbeats(c),
			"写出的字节全部来自心跳时必须判为「只有心跳」")
	})

	t.Run("有语义输出时不判为只有心跳", func(t *testing.T) {
		c, _ := newCommittedCtx(EndpointResponses)
		// 先写心跳（13→实际写入量），再写语义输出：语义输出不记进心跳 key。
		pingBytes, err := c.Writer.WriteString(": keep-alive\n\n")
		require.NoError(t, err)
		recordGatewayStreamHeartbeat(c, pingBytes)
		_, err = c.Writer.WriteString("data: {\"type\":\"response.completed\"}\n\n")
		require.NoError(t, err)
		assert.False(t, gatewayStreamHasOnlyHeartbeats(c),
			"已写出语义输出时不得判为「只有心跳」")
	})
}

// ---------------------------------------------------------------------------
// 5. 客户端断开后不追加错误帧（既有契约不被破坏）
// ---------------------------------------------------------------------------

// TestAnthropicError_CanceledClientAfterCommitted_AppendsNothing 锁死既有契约：
// 客户端已断开时不得向断开的连接追加错误帧。
func TestAnthropicError_CanceledClientAfterCommitted_AppendsNothing(t *testing.T) {
	c, w := newCommittedCtx(EndpointMessages)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = c.Request.WithContext(ctx)
	c.Header("Content-Type", "text/event-stream")
	_, _ = c.Writer.WriteString(":\n\n")
	cancel()

	h := &GatewayHandler{}
	wrote := h.ensureForwardErrorResponse(c, true)

	require.False(t, wrote, "客户端已断开时不得补写错误响应")
	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.Equal(t, ":\n\n", w.Body.String(),
		"客户端已断开时不得向流内追加字节")
}
