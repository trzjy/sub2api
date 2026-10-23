package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestResponsesCursorSettlementSnapshotBuildsRecordUsageInput 覆盖 §25.17.5 A06：
// 首次 Execute 前构造的不可变快照必须逐字段落到原 RecordUsage 输入，渠道字段用
// 快照的客户端请求模型 + 终态的真实 UpstreamModel 计算，不闭包任何请求期可变对象。
func TestResponsesCursorSettlementSnapshotBuildsRecordUsageInput(t *testing.T) {
	pricingAt := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	snapshot := responsesCursorSettlementSnapshot{
		log:              zap.NewNop(),
		apiKey:           &service.APIKey{ID: 7, User: &service.User{ID: 11}},
		user:             &service.User{ID: 11},
		account:          &service.Account{ID: 3, Platform: service.PlatformCursor},
		subscription:     &service.UserSubscription{},
		pricingAt:        pricingAt,
		quotaPlatform:    "cursor",
		requestHash:      "req-hash",
		userAgent:        "test-agent",
		clientIP:         "1.2.3.4",
		inboundEndpoint:  "/v1/responses",
		upstreamEndpoint: "/v1/responses",
		sessionID:        "session-1",
		requestedModel:   "gpt-5",
		channelMapping: service.ChannelMappingResult{
			ChannelID:          21,
			Mapped:             true,
			MappedModel:        "mapped-model",
			BillingModelSource: "channel_mapped",
		},
	}
	final := &service.ForwardResult{
		Model:         "cursor-final",
		UpstreamModel: "native-cursor",
		Usage:         service.ClaudeUsage{InputTokens: 5, OutputTokens: 9},
	}

	input := snapshot.recordUsageInput(final)
	require.Same(t, final, input.Result, "终态真实结果必须原样进入 RecordUsage 输入")
	require.Equal(t, "cursor", input.QuotaPlatform)
	require.Equal(t, int64(7), input.APIKey.ID)
	require.Equal(t, snapshot.user, input.User, "user 取自快照（= apiKey.User）")
	require.Equal(t, int64(3), input.Account.ID)
	require.Same(t, snapshot.subscription, input.Subscription)
	require.True(t, pricingAt.Equal(input.PricingAt))
	require.Equal(t, "/v1/responses", input.InboundEndpoint)
	require.Equal(t, "/v1/responses", input.UpstreamEndpoint)
	require.Equal(t, "test-agent", input.UserAgent)
	require.Equal(t, "1.2.3.4", input.IPAddress)
	require.Equal(t, "session-1", input.SessionID)
	require.Equal(t, "req-hash", input.RequestPayloadHash)
	require.Nil(t, input.APIKeyService, "APIKeyService 由结算闭包填充，不属于请求级快照")
	require.Equal(t, int64(21), input.ChannelID)
	require.Equal(t, "gpt-5", input.OriginalModel, "渠道字段使用快照的客户端请求模型")
	require.Equal(t, "mapped-model", input.ChannelMappedModel)
	require.Equal(t, "channel_mapped", input.BillingModelSource)
	require.Equal(t, "gpt-5→mapped-model→native-cursor", input.ModelMappingChain, "映射链基于终态真实 UpstreamModel")
}

// TestResponsesCursorSettlementNilTerminalIsSkipped 覆盖 §25.17.5 A06：
// 终态结果为空时不构造零输入、不提交账务，只记录诊断日志（helper 的完整性判定在
// 前，正常情况下不会出现 nil 终态，这里作为装配防御验证）。
func TestResponsesCursorSettlementNilTerminalIsSkipped(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	h := &GatewayHandler{}
	fn := h.buildResponsesCursorSettlement(responsesCursorSettlementSnapshot{
		log:            zap.New(core),
		apiKey:         &service.APIKey{ID: 1},
		user:           &service.User{ID: 2},
		account:        &service.Account{ID: 3, Platform: service.PlatformCursor},
		requestedModel: "gpt-5",
		channelMapping: service.ChannelMappingResult{},
	})

	require.NotPanics(t, func() { fn(context.Background(), nil) })
	require.Equal(t, 1, logs.FilterMessage("gateway.responses.cursor_settlement_nil_result").Len())
}

// TestResponsesCursorSettlementRegisteredFuncReceivesTerminalOnce 覆盖 §25.17.5 A06 在本
// handler 的装配验证：真实 handover 注册 buildResponsesCursorSettlement 闭包后，经
// settle 收到终态真实 *ForwardResult，闭包只提交一次；重复终结不重复结算。闭包内
// 提交路径使用既有 mandatory usage worker（submitMandatoryUsageRecordTask）。
func TestResponsesCursorSettlementRegisteredFuncReceivesTerminalOnce(t *testing.T) {
	var submits atomic.Int32
	h := &GatewayHandler{}
	snapshot := responsesCursorSettlementSnapshot{
		log:            zap.NewNop(),
		apiKey:         &service.APIKey{ID: 1, User: &service.User{ID: 2}},
		user:           &service.User{ID: 2},
		account:        &service.Account{ID: 3, Platform: service.PlatformCursor},
		requestedModel: "gpt-5",
		channelMapping: service.ChannelMappingResult{},
	}

	handover := newCursorResourceHandover(nil, nil)
	handover.RegisterCursorSettlement(func(ctx context.Context, final *service.ForwardResult) {
		// 复用真实 build 闭包：收到终态后构造原 RecordUsage 输入并调用原 mandatory
		// usage worker。gatewayService 未装配时 RecordUsage 会 panic，由
		// submitMandatoryUsageRecordTask 的同步回退路径 recover（证明其已进入提交链）。
		h.buildResponsesCursorSettlement(snapshot)(ctx, final)
		submits.Add(1)
	})

	final := &service.ForwardResult{
		Model:         "cursor-final",
		UpstreamModel: "native-cursor",
		Usage:         service.ClaudeUsage{InputTokens: 11, OutputTokens: 22},
	}
	ev := service.CursorSettlementEvent{
		SessionID:      "unit-test",
		BillingEventID: "cursor_session:unit-test",
		Result:         final,
		UsageComplete:  true,
	}
	handover.Settlement()(context.Background(), ev)
	require.EqualValues(t, 1, submits.Load(), "回调收到终态且只排一次")
	require.NoError(t, handover.SettlementError())
	require.True(t, handover.Settled())

	// 重复终结（Stop 与结果回传竞争）不得重复结算、不得再排账。
	handover.Settlement()(context.Background(), ev)
	require.EqualValues(t, 1, submits.Load(), "重复终结不得重复提交")
}
