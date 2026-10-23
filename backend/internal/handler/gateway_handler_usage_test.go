package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsageUnrestrictedIncludesWeeklyWindowStart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/usage", nil)

	weeklyWindowStart := time.Date(2026, time.July, 13, 0, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	c.Set(string(middleware.ContextKeySubscription), &service.UserSubscription{
		WeeklyWindowStart: &weeklyWindowStart,
	})

	handler := &GatewayHandler{}
	handler.usageUnrestricted(
		c,
		context.Background(),
		&service.APIKey{Group: &service.Group{
			Name:             "Weekly plan",
			SubscriptionType: service.SubscriptionTypeSubscription,
		}},
		middleware.AuthSubject{},
		nil,
		nil,
		nil,
	)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Subscription struct {
			WeeklyWindowStart *time.Time `json:"weekly_window_start"`
		} `json:"subscription"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.NotNil(t, response.Subscription.WeeklyWindowStart)
	require.True(t, weeklyWindowStart.Equal(*response.Subscription.WeeklyWindowStart))
}

// TestGatewayHandlerCursorSettlementFuncTerminalOnce 覆盖 §25.17.5 A06：gateway_handler 的
// cursorSettlementFunc 经手转物注册后，收到**终态真实 *ForwardResult** 时把原 RecordUsage
// 任务提交给原 mandatory usage worker 且**至多一次**（重复终结不得重复提交）。
func TestGatewayHandlerCursorSettlementFuncTerminalOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)

	apiKey := &service.APIKey{ID: 7, User: &service.User{ID: 9}, GroupID: int64Ptr(3)}
	account := &service.Account{ID: 11, Platform: service.PlatformCursor}
	subscription := &service.UserSubscription{ID: 21}

	var submitted atomic.Int32
	submit := func(context.Context, service.UsageRecordTask) { submitted.Add(1) }

	h := &GatewayHandler{}
	handler := h.cursorSettlementFunc(cursorSettlementSnapshot{
		apiKey:             apiKey,
		user:               apiKey.User,
		account:            account,
		subscription:       subscription,
		pricingAt:          time.Now(),
		quotaPlatform:      service.QuotaPlatform(context.Background(), apiKey),
		inboundEndpoint:    "/v1/messages",
		upstreamEndpoint:   "/v1/messages",
		userAgent:          "cursor-test",
		ipAddress:          "127.0.0.1",
		sessionID:          "sess-1",
		requestPayloadHash: "hash-1",
		forceCacheBilling:  false,
		channelMapping:     service.ChannelMappingResult{},
		requestedModel:     "claude-sonnet",
		thinking:           true,
		userID:             9,
		groupID:            apiKey.GroupID,
		reqModel:           "claude-sonnet",
	}, submit)

	handover := newCursorResourceHandover(nil, nil)
	handover.RegisterCursorSettlement(handler)
	require.NotNil(t, handover.SettlementFunc(), "注册后 finalizer 侧访问器必须能取出同一钩子")

	final := &service.ForwardResult{
		Model:         "cursor-final",
		UpstreamModel: "cursor-final-upstream",
		Usage:         service.ClaudeUsage{InputTokens: 11, OutputTokens: 22, CacheReadInputTokens: 3, CacheCreationInputTokens: 4},
	}
	ev := service.CursorSettlementEvent{
		SessionID:       "sess-1",
		BillingEventID:  "cursor_session:sess-1",
		Result:          final,
		UsageComplete:   true,
		InflightRelease: func() {},
	}
	// 终态恰好到达一次 → 原 mandatory usage worker 恰好提交一次。
	handover.Settlement()(context.Background(), ev)
	require.EqualValues(t, 1, submitted.Load(), "回调收到终态且只排一次")
	require.True(t, handover.Settled())
	require.NoError(t, handover.SettlementError(), "完整 usage + 已注册 = 装配正常")

	// 重复终结（Stop 与结果回传竞争）不得重复提交。
	handover.Settlement()(context.Background(), ev)
	require.EqualValues(t, 1, submitted.Load(), "重复终结不得重复排账")
}

// TestGatewayHandlerCursorSettlementFuncHandlesNilTerminal 覆盖 §25.17.5 A06：
// 闭包对 nil 终态直接返回（settle 侧已保证真实结果才调用，防御性透传 nil 不 panic）。
func TestGatewayHandlerCursorSettlementFuncHandlesNilTerminal(t *testing.T) {
	var submitted atomic.Int32
	h := &GatewayHandler{}
	handler := h.cursorSettlementFunc(cursorSettlementSnapshot{
		apiKey:   &service.APIKey{ID: 7, User: &service.User{ID: 9}},
		reqModel: "claude-sonnet",
		userID:   9,
	}, func(context.Context, service.UsageRecordTask) { submitted.Add(1) })
	handler(context.Background(), nil)
	require.Zero(t, submitted.Load(), "nil 终态不得提交账务任务")
}

func int64Ptr(v int64) *int64 { return &v }
