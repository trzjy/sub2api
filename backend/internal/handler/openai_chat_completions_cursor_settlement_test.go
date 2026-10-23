//go:build unit

package handler

// 本文件覆盖 §25.17.5 A06 在 openai_chat_completions.go 的落地（H 微卡）：
//  1. captureOpenAIChatCursorSettlement 值捕获不可变计费参数快照（不闭包 gin.Context）；
//  2. cursorChatFinalToOpenAIResult 终态 ForwardResult → OpenAI 网关结果形状的字段映射；
//  3. 注册的 CursorSettlementFunc 经 handover settle 驱动后收到终态且**只排一次**提交
//     （重复终结不二次提交）。
//
// 提交观测走 mandatory usage worker 池的真实入队计数（pool.Stats().SubmittedTasks），
// 断言"对 mandatory usage worker 的提交恰好一次"。
// handover 自身的幂等/装配错误/缺 usage 语义由 cursor_handover_handle_test.go 覆盖。
//
// 观察点说明（§25.17.5 A06）：晚绑定链已随 A06 删除——生产代码不再调用
// CursorSettlementOwner.AdoptSettlementTask（openai_gateway_handler.go 的
// submitMandatoryUsageRecordTask 无 adopt 分支），故旧 spy owner 的 adoptCount 恒为 0、
// 无法再观测提交。这里与本包既有 A06 用例 openai_gateway_cursor_settlement_test.go
// 同口径，直接以 worker 池入队计数为证据，断言强度不变（恰好一次）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newCursorSettlementTestGinContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.RemoteAddr = "203.0.113.7:45678"
	return c, rec
}

// TestOpenAIChatCursorSettlementSnapshotCapturesValues 覆盖 §25.17.5 A06：
// 快照在首次 Execute 前值捕获不可变计费参数（user/API key/组/订阅/账号、pricingAt、
// quotaPlatform、原请求 hash、UA/IP、审计参数），随后修改请求上下文不影响快照。
func TestOpenAIChatCursorSettlementSnapshotCapturesValues(t *testing.T) {
	c, _ := newCursorSettlementTestGinContext(t)
	c.Request.Header.Set("User-Agent", "cursor-test-agent")
	c.Request.Header.Set("X-Session-Id", "client-session-abc")

	ctx := c.Request.Context()
	ctx = context.WithValue(ctx, ctxkey.RequestedPublicModel, "public-model-cursor")
	ctx = service.WithRequestedReasoningEffort(ctx, "low")
	c.Request = c.Request.WithContext(ctx)

	groupID := int64(9)
	apiKey := &service.APIKey{ID: 42, User: &service.User{ID: 7, Email: "u@example.com"}, GroupID: &groupID, Group: &service.Group{Platform: service.PlatformCursor}}
	account := &service.Account{ID: 11, Platform: service.PlatformCursor}
	subscription := &service.UserSubscription{ID: 3}
	pricingAt := time.Now().Add(-time.Minute)
	channelMapping := service.ChannelMappingResult{ChannelID: 77, Mapped: false}
	body := []byte(`{"model":"cursor-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	snap := captureOpenAIChatCursorSettlement(c, apiKey, account, subscription, channelMapping, "cursor-model", pricingAt, body)
	require.NotNil(t, snap)
	require.Same(t, apiKey, snap.apiKey, "API key 快照必须值捕获")
	require.Same(t, apiKey.User, snap.user, "user 快照必须值捕获")
	require.Same(t, account, snap.account, "账号快照必须值捕获")
	require.Same(t, subscription, snap.subscription, "订阅快照必须值捕获")
	require.Equal(t, pricingAt, snap.pricingAt, "pricingAt 必须按请求捕获时刻冻结")
	require.Equal(t, service.QuotaPlatform(c.Request.Context(), apiKey), snap.quotaPlatform)
	require.Equal(t, service.HashUsageRequestPayload(body), snap.requestPayloadHash)
	require.Equal(t, "cursor-test-agent", snap.userAgent)
	require.Equal(t, "203.0.113.7", snap.clientIP)
	require.Equal(t, EndpointChatCompletions, snap.inboundEndpoint)
	require.Equal(t, "client-session-abc", snap.sessionID)
	require.Equal(t, "public-model-cursor", snap.requestedPublicModel)
	require.NotNil(t, snap.requestedReasoningEffort)
	require.Equal(t, "low", *snap.requestedReasoningEffort)
	require.Equal(t, "cursor-model", snap.reqModel)
	require.False(t, snap.cyberBlocked)
	require.NotEmpty(t, snap.upstreamEndpoint, "实际出站信息必须在快照时捕获（终态 ForwardResult 无端点字段）")

	// 值捕获证明：快照后改动请求上下文，快照必须不受影响（不闭包 gin.Context/原正文）。
	c.Request.Header.Set("User-Agent", "mutated-agent")
	c.Set(ctxKeyActualUpstreamEndpoint, "https://mutated.example")
	require.Equal(t, "cursor-test-agent", snap.userAgent, "快照不闭包 gin.Context：header 变更不得影响 UA")
	require.Equal(t, pricingAt, snap.pricingAt)
	require.Equal(t, service.HashUsageRequestPayload(body), snap.requestPayloadHash)
}

// TestOpenAIChatCursorSettlementFinalToOpenAIResult 覆盖 §25.17.5 A06 字段映射：
// 终态 ForwardResult 的真实 usage/模型/时长/出站头逐字段搬进 RecordUsage 输入形状，
// 零折算、不补默认值。
func TestOpenAIChatCursorSettlementFinalToOpenAIResult(t *testing.T) {
	firstTokenMs := 100
	final := &service.ForwardResult{
		RequestID:       "cursor-req-1",
		UpstreamHeaders: http.Header{"X-Upstream": []string{"h"}},
		Usage: service.ClaudeUsage{
			InputTokens:              10,
			OutputTokens:             20,
			CacheCreationInputTokens: 2,
			CacheReadInputTokens:     3,
			ImageOutputTokens:        4,
		},
		Model:            "upstream-cursor-model",
		UpstreamModel:    "mapped-model",
		Stream:           false,
		Duration:         5 * time.Second,
		FirstTokenMs:     &firstTokenMs,
		ClientDisconnect: true,
		SearchCount:      2,
		AudioUsage:       &service.AudioUsage{Mode: "tts", DurationOrUnits: 1.5},
	}

	res := cursorChatFinalToOpenAIResult(final)
	require.NotNil(t, res)
	require.Equal(t, "cursor-req-1", res.ResponseID)
	require.Equal(t, 10, res.Usage.InputTokens, "input 真实值逐字段映射")
	require.Equal(t, 20, res.Usage.OutputTokens)
	require.Equal(t, 2, res.Usage.CacheCreationInputTokens)
	require.Equal(t, 3, res.Usage.CacheReadInputTokens)
	require.Equal(t, 4, res.Usage.ImageOutputTokens)
	require.Equal(t, "upstream-cursor-model", res.Model)
	require.Equal(t, "mapped-model", res.UpstreamModel)
	require.False(t, res.Stream)
	require.Equal(t, 5*time.Second, res.Duration)
	require.NotNil(t, res.FirstTokenMs)
	require.Equal(t, 100, *res.FirstTokenMs)
	require.True(t, res.ClientDisconnect)
	require.Equal(t, 2, res.SearchCount)
	require.NotNil(t, res.AudioUsage)
	require.Equal(t, "tts", res.AudioUsage.Mode)
	require.NotNil(t, res.ResponseHeaders, "上游响应头克隆到 ResponseHeaders")
	require.Equal(t, "h", res.ResponseHeaders.Get("X-Upstream"))

	require.Nil(t, cursorChatFinalToOpenAIResult(nil), "nil 终态返回 nil 结果")
}

// TestOpenAIChatCursorSettlementRegisteredCallbackReceivesTerminalOnce 覆盖
// §25.17.5 A06"回调收到终态且只排一次"：注册的 CursorSettlementFunc 经 handover
// settle 驱动，对 mandatory usage worker 提交恰好一次；重复终结（Stop 与结果回传
// 竞争）不二次提交。
func TestOpenAIChatCursorSettlementRegisteredCallbackReceivesTerminalOnce(t *testing.T) {
	c, _ := newCursorSettlementTestGinContext(t)
	groupID := int64(9)
	apiKey := &service.APIKey{ID: 42, User: &service.User{ID: 7}, GroupID: &groupID, Group: &service.Group{Platform: service.PlatformCursor}}
	account := &service.Account{ID: 11, Platform: service.PlatformCursor}
	snap := captureOpenAIChatCursorSettlement(c, apiKey, account, nil, service.ChannelMappingResult{}, "cursor-model", time.Now(), nil)
	require.NotNil(t, snap)

	// 结算提交经 mandatory usage worker 池出队观测（§25.17.5 A06 无 adopt 分支）。
	pool := newUsageRecordTestPool(t)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}

	handover := newCursorResourceHandover(nil, nil)
	handover.RegisterCursorSettlement(func(ctx context.Context, final *service.ForwardResult) {
		submitOpenAIChatCursorSettlement(h, snap, ctx, final)
	})
	require.NotNil(t, handover.SettlementFunc(), "注册后 finalizer 侧访问器必须取出同一个钩子")

	settleCtx := context.Background()

	eventID := "cursor_session:handler-callback-once"
	final := &service.ForwardResult{
		Model: "cursor-final",
		Usage: service.ClaudeUsage{InputTokens: 5, OutputTokens: 7, CacheReadInputTokens: 1, CacheCreationInputTokens: 0},
	}
	ev := service.CursorSettlementEvent{
		SessionID:      "unit-session",
		BillingEventID: eventID,
		Result:         final,
		UsageComplete:  true,
	}

	handover.Settlement()(settleCtx, ev)
	require.Eventually(t, func() bool { return pool.Stats().SubmittedTasks >= 1 }, 2*time.Second, time.Millisecond,
		"会话终结须对 mandatory usage worker 提交恰好一次")
	require.EqualValues(t, 1, pool.Stats().SubmittedTasks, "会话终结须对 mandatory usage worker 提交恰好一次")
	require.NoError(t, handover.SettlementError())
	require.True(t, handover.Settled())

	// 重复终结（Stop 与结果回传竞争）：不得二次提交。
	handover.Settlement()(settleCtx, ev)
	require.EqualValues(t, 1, pool.Stats().SubmittedTasks, "重复终结不得重复结算提交")

	// 无效装配防御：nil 快照/终态不 panic 且不提交。
	require.NotPanics(t, func() {
		submitOpenAIChatCursorSettlement(h, nil, settleCtx, final)
		submitOpenAIChatCursorSettlement(h, snap, settleCtx, nil)
		submitOpenAIChatCursorSettlement(nil, snap, settleCtx, final)
	})
	require.EqualValues(t, 1, pool.Stats().SubmittedTasks, "nil 装配不得产生额外提交")
}
