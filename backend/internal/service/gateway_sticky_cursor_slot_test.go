//go:build unit

package service

// 本文件固定「Cursor 粘性账号槽满即换号」的行为契约（2026-11-04 故障修复）。
//
// 背景：Cursor 会话的用户槽/账号槽跟着会话走（§25.4），客户端在工具批交付后按 esc 中断
// 时无人收尾，槽要等 waiting_tools 空闲释放（§2.5，120 秒）或 Redis 槽 TTL（30 分钟）
// 才归还。旧调度行为是「等待队列没满就给 120 秒等待计划」，于是整个请求链每 2 分钟撞
// 一次 429，而同组其它空闲账号一个都不选。
//
// 修复后：Cursor 平台粘性账号槽满 → 视为粘性未命中，立即走负载感知选号换账号；
// 非 Cursor 平台保持既有等待语义（上游 prompt cache / 会话亲和收益更确定）。
//
// Cursor 账号在调度层还要通过 quota-key 来源门（cursorSupplySourceAvailabilityVetoReason），
// 端到端构造需要一整套来源桩；这里用谓词单测 + 非 Cursor 平台的端到端用例覆盖分支两侧，
// 避免为了测试反向放宽生产准入。

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// TestStickySlotFullSwitchPredicate 覆盖换号谓词：只有 Cursor 供给直接换号。
func TestStickySlotFullSwitchPredicate(t *testing.T) {
	cases := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "cursor 必须换号", account: &Account{ID: 1, Platform: PlatformCursor}, want: true},
		{name: "anthropic 保持等待", account: &Account{ID: 2, Platform: PlatformAnthropic}, want: false},
		{name: "antigravity 保持等待", account: &Account{ID: 3, Platform: PlatformAntigravity}, want: false},
		{name: "gemini 保持等待", account: &Account{ID: 4, Platform: PlatformGemini}, want: false},
		{name: "nil 账号不换号（防御）", account: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, shouldSwitchAccountOnStickySlotFull(tc.account))
		})
	}
}

// TestAnthropicStickySlotFullKeepsWaitPlan 保护非 Cursor 平台的既有语义：
// 槽满仍在等待队列容量内 → 返回粘性等待计划（不换号）。
func TestAnthropicStickySlotFullKeepsWaitPlan(t *testing.T) {
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{
		sessionBindings: map[string]int64{"sticky-anthropic": 1},
	}

	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3

	concurrencyCache := &mockConcurrencyCache{
		acquireResults: map[int64]bool{1: false},
		waitCounts:     map[int64]int{1: 0},
	}

	svc := &GatewayService{
		accountRepo:        repo,
		cache:              cache,
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	result, err := svc.SelectAccountWithLoadAwareness(
		context.Background(), nil, "sticky-anthropic", "claude-3-5-sonnet-20241022", nil, "", int64(0))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.WaitPlan, "非 Cursor 平台保持粘性等待语义")
	require.Equal(t, int64(1), result.Account.ID)
}

// cursorAccountForSlotTest 构造一个能过调度准入的 Cursor 账号：
// - type=apikey 走 OpenAI 兼容来源门豁免（见 cursorSupplySourceAvailabilityVetoReason）；
// - credentials.model_mapping 非空，否则 Cursor 平台按「无内置模型目录」直接判不可服务。
func cursorAccountForSlotTest(id int64) Account {
	return Account{
		ID:          id,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Priority:    1,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 3,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"claude-opus-5-5": "claude-opus-5-5"},
		},
	}
}

func newCursorSlotTestService(sessionKey string, stickyAccountID int64, acquireResults map[int64]bool, waitCounts map[int64]int) (*GatewayService, []Account) {
	accounts := []Account{cursorAccountForSlotTest(1), cursorAccountForSlotTest(2)}
	repo := &mockAccountRepoForPlatform{
		accounts:     accounts,
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	cache := &mockGatewayCacheForPlatform{
		sessionBindings: map[string]int64{sessionKey: stickyAccountID},
	}
	cfg := testConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 3
	concurrencyCache := &mockConcurrencyCache{
		acquireResults: acquireResults,
		waitCounts:     waitCounts,
	}
	svc := &GatewayService{
		accountRepo:        repo,
		cache:              cache,
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}
	return svc, accounts
}

// TestCursorStickySlotFullSwitchesAccountNoRouting 固定 2026-11-15 的修复：
// 分组没有模型路由配置（走 Layer 1.5 无路由粘性分支）时，Cursor 粘性账号槽满
// 必须立刻换到同组其它空闲账号，不得返回等待计划。
//
// 生产症状：账号 986 上游不回话、3 个并发槽被占死，后续请求原地排队 120 秒
// （而排队上限 120s < 上游失败耗时 125s，必然等空），同组 81 个空闲账号一个都没选。
func TestCursorStickySlotFullSwitchesAccountNoRouting(t *testing.T) {
	svc, _ := newCursorSlotTestService("sticky-cursor", 1,
		map[int64]bool{1: false, 2: true}, // 粘性号 1 槽满，2 号有空位
		map[int64]int{1: 0})

	ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformCursor)
	result, err := svc.SelectAccountWithLoadAwareness(
		ctx, nil, "sticky-cursor", "claude-opus-5-5", nil, "", int64(0))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.WaitPlan, "Cursor 槽满必须换号，不得返回等待计划")
	require.Equal(t, int64(2), result.Account.ID, "必须换到有空槽的账号")
}

// TestCursorAllSlotsFullReportsNoAvailableAccounts 固定「池子忙不过来要如实报错」：
// 整池 Cursor 账号都抢不到槽时，不得返回等待计划让客户端白等，而应返回
// 号池无可用账号（SessionSlotsFull），由 handler 转成可重试的 503 文案。
func TestCursorAllSlotsFullReportsNoAvailableAccounts(t *testing.T) {
	svc, _ := newCursorSlotTestService("sticky-cursor-full", 1,
		map[int64]bool{1: false, 2: false}, // 两个号都满
		map[int64]int{1: 0})

	ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformCursor)
	result, err := svc.SelectAccountWithLoadAwareness(
		ctx, nil, "sticky-cursor-full", "claude-opus-5-5", nil, "", int64(0))

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoAvailableAccounts),
		"整池槽满必须返回号池无可用账号，而不是让客户端等一个必然等空的等待计划，实际错误：%v", err)

	var noAvail *NoAvailableAccountsError
	require.True(t, errors.As(err, &noAvail), "错误必须携带可区分的结构体，供 handler 生成 503 文案")
	require.Greater(t, noAvail.SessionSlotsFull, 0, "必须标记为「账号可用但槽位占满」")
	require.Nil(t, result)
}
