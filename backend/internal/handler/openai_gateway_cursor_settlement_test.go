package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestCursorOpenAIGatewaySettlementRegisteredSubmitsOnce 覆盖 §25.17.5 A06 的 handler 接线：
// registerCursorSettlementForAttempt 在首次 Execute 与资源转交之前注册唯一结算钩子，会话终结时
// 收到**终态真实 *ForwardResult** 后经原 mandatory usage worker 提交且**至多排一次**；
// 重复终结不重复提交；缺完整 usage 的终态不提交（零账务任务），预留引用仍只归还一次。
func TestCursorOpenAIGatewaySettlementRegisteredSubmitsOnce(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}

	apiKey := &service.APIKey{
		ID:     780040,
		UserID: 780041,
		Status: service.StatusActive,
		User:   &service.User{ID: 780041, Balance: 100},
	}
	account := &service.Account{
		ID:          780043,
		Platform:    service.PlatformCursor,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
	}

	handover := newCursorResourceHandover(nil, nil)
	h.registerCursorSettlementForAttempt(
		handover,
		apiKey,
		account,
		nil, // subscription（订阅为空走计量路径，与既有 RecordUsage 输入一致）
		service.ChannelMappingResult{},
		"gpt-5.1-codex",         // 请求侧模型（客户可见名）
		"cursor-cli/1.0",        // User-Agent
		"203.0.113.1",           // 客户端 IP
		"/v1/responses",         // inbound endpoint
		"/v1/chat/completions",  // upstream endpoint（值快照，不回读旧 gin.Context）
		"hash-of-original-body", // 原请求 hash
		service.PlatformCursor,  // quotaPlatform
		"cursor-session-x",      // 请求侧 sessionID
		time.Now(),              // pricingAt（请求级定价时刻）
		false,                   // cyberBlocked
		true,                    // nativeCompactionV2（Responses 链装配）
	)
	require.NotNil(t, handover.SettlementFunc(), "Cursor 分支必须在终态前注册唯一结算钩子")

	var inflightReleases atomic.Int32
	final := &service.ForwardResult{
		Model:  "gpt-5.1-codex",
		Stream: true,
		// f1–f4 计费字段逐字段存在（含 cache_write=0 的真实完整用量）。
		Usage: service.ClaudeUsage{InputTokens: 11, OutputTokens: 5, CacheReadInputTokens: 3, CacheCreationInputTokens: 0},
	}
	ev := service.CursorSettlementEvent{
		SessionID:       "cursor-session-x",
		BillingEventID:  "cursor_session:cursor-session-x",
		Result:          final,
		UsageComplete:   true,
		InflightRelease: func() { inflightReleases.Add(1) },
	}

	// 终态完整：回调收到真实终态并只排一次。
	handover.Settlement()(context.Background(), ev)
	require.True(t, handover.Settled(), "终态完整必须完成唯一结算")
	require.NoError(t, handover.SettlementError(), "注册齐全且终态完整不得按装配错误处理")

	require.Eventually(t, func() bool { return pool.Stats().SubmittedTasks >= 1 }, 2*time.Second, time.Millisecond)
	require.EqualValues(t, 1, pool.Stats().SubmittedTasks,
		"会话终态驱动的结算至多排一次账务任务（首次/最终 HTTP 返回不得再为 Cursor 另排账）")

	// 重复终结（Stop 与结果回传竞争）不得重复提交、不得重复归还。
	handover.Settlement()(context.Background(), ev)
	require.EqualValues(t, 1, pool.Stats().SubmittedTasks, "重复终结不得再排账务任务")
	require.EqualValues(t, 1, inflightReleases.Load(), "预留引用只归还一次")

	// 另一只手：缺完整 usage 的终态不提交零 usage 假账，也不按装配错误处理；预留仍归还。
	var shortReleases atomic.Int32
	shortHandover := newCursorResourceHandover(nil, nil)
	h.registerCursorSettlementForAttempt(
		shortHandover,
		apiKey,
		account,
		nil,
		service.ChannelMappingResult{},
		"gpt-5.1-codex",
		"cursor-cli/1.0",
		"203.0.113.1",
		"/v1/responses",
		"/v1/chat/completions",
		"hash-of-original-body",
		service.PlatformCursor,
		"cursor-session-y",
		time.Now(),
		false,
		false,
	)
	shortHandover.Settlement()(context.Background(), service.CursorSettlementEvent{
		SessionID:       "cursor-session-y",
		BillingEventID:  "cursor_session:cursor-session-y",
		Result:          &service.ForwardResult{Model: "gpt-5.1-codex"},
		UsageComplete:   false,
		InflightRelease: func() { shortReleases.Add(1) },
	})
	require.False(t, shortHandover.Settled())
	require.NoError(t, shortHandover.SettlementError(), "缺完整 usage 是已接受的契约路径，不是装配错误")
	require.EqualValues(t, 1, shortReleases.Load(), "未结算也必须归还预留引用")
	require.EqualValues(t, 1, pool.Stats().SubmittedTasks, "缺完整 usage 绝不排账务任务")
}
