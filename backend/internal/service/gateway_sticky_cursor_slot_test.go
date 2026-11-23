//go:build unit

package service

// 本文件固定「粘性账号槽满 → 先分流、同档全满才等一次」的统一规则（方案 §3.1 / §4.1）。
//
// 旧实现按平台分轨：Cursor 槽满即换号，其他平台（如 Anthropic）原地返回粘性等待计划。
// 2026-11-23 用户裁定取消平台分轨：粘性抢槽失败不再产生等待计划，控制流继续到本层路由
// 候选或 Layer 2 整池选择；只有当前优先级档内、经既有初筛后的候选全部实际抢槽失败，
// 才产生唯一一次等待计划（目标在该档候选内，超时用 fallback_wait_timeout）。
// 旧的平台分轨谓词已随之删除，Cursor 与其他平台走同一规则。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// anthropicAccountForSlotTest 构造两个同档 Anthropic 账号：粘性号 1 与备选号 2。
// 必须同档（Priority 相同），否则档位门会把备选号排除，无法验证「同档空闲号分流」。
func anthropicAccountForSlotTest(id int64) Account {
	return Account{
		ID:          id,
		Platform:    PlatformAnthropic,
		Priority:    1,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 5,
	}
}

func newAnthropicSlotTestService(sessionKey string, stickyAccountID int64, acquireResults map[int64]bool, waitCounts map[int64]int) (*GatewayService, []Account) {
	accounts := []Account{anthropicAccountForSlotTest(1), anthropicAccountForSlotTest(2)}
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

// TestAnthropicStickySlotFullKeepsWaitPlan 固定非 Cursor 平台的新规则（原用例名保留）。
// 原语义是「槽满仍在等待队列容量内 → 返回粘性等待计划」；新规则下粘性账号槽满不再
// 原地等待，同档内有空闲号时直接选中空闲号，WaitPlan 必须为 nil。
func TestAnthropicStickySlotFullKeepsWaitPlan(t *testing.T) {
	t.Run("same_tier_idle_account_selected_without_wait_plan", func(t *testing.T) {
		svc, _ := newAnthropicSlotTestService("sticky-anthropic", 1,
			map[int64]bool{1: false, 2: true}, // 粘性号 1 槽满，2 号同档且有空位
			map[int64]int{1: 0, 2: 0})

		result, err := svc.SelectAccountWithLoadAwareness(
			context.Background(), nil, "sticky-anthropic", "claude-3-5-sonnet-20241022", nil, "", int64(0))

		require.NoError(t, err)
		require.NotNil(t, result)
		require.Nil(t, result.WaitPlan, "同档有空闲号时不得返回等待计划")
		require.Equal(t, int64(2), result.Account.ID, "必须直接分流到同档空闲号")
	})

	t.Run("all_same_tier_candidates_full_single_wait_plan", func(t *testing.T) {
		svc, _ := newAnthropicSlotTestService("sticky-anthropic-full", 1,
			map[int64]bool{1: false, 2: false}, // 同档两个候选抢槽全部失败
			map[int64]int{1: 0, 2: 0})

		result, err := svc.SelectAccountWithLoadAwareness(
			context.Background(), nil, "sticky-anthropic-full", "claude-3-5-sonnet-20241022", nil, "", int64(0))

		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.WaitPlan, "同档候选全部抢槽失败后必须产生唯一一次等待计划")
		require.Contains(t, []int64{1, 2}, result.WaitPlan.AccountID, "等待目标必须在本档候选内")
		require.Equal(t, 5, result.WaitPlan.MaxConcurrency)
	})

	t.Run("full_group_prefers_sticky_original_over_fallback_first", func(t *testing.T) {
		svc, accounts := newAnthropicSlotTestService("sticky-anthropic-priority", 1,
			map[int64]bool{1: false, 2: false}, // 同档 A(1)、B(2) 都满
			map[int64]int{1: 0, 2: 0})

		// 兜底排序按 LastUsedAt 升序：让 B(2) 排在 A(1) 前面；两者相差 1 小时
		// 避开同组 shuffle。粘性绑定为 A，期望等待目标仍是 A 而非排序首项 B。
		newer := time.Now()
		older := newer.Add(-time.Hour)
		accounts[0].LastUsedAt = &newer // A(1) 最近使用 → 排序靠后
		accounts[1].LastUsedAt = &older // B(2) 更早使用 → 排序靠前

		result, err := svc.SelectAccountWithLoadAwareness(
			context.Background(), nil, "sticky-anthropic-priority", "claude-3-5-sonnet-20241022", nil, "", int64(0))

		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.WaitPlan, "同档候选全部抢槽失败后必须产生唯一一次等待计划")
		require.Equal(t, int64(1), result.WaitPlan.AccountID,
			"粘性原号仍在候选内时，等待目标必须是原号 A，而非兜底排序首项 B")
	})
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

// TestCursorStickySlotFullSwitchesAccountNoRouting 固定统一规则在 Cursor 上的表现：
// 无模型路由配置（Layer 1.5 无路由粘性分支）时，粘性账号槽满同样直接换号，不返回等待计划。
// 原「Cursor 专属换号」语义已并入全局规则，不再依赖平台谓词。
func TestCursorStickySlotFullSwitchesAccountNoRouting(t *testing.T) {
	svc, _ := newCursorSlotTestService("sticky-cursor", 1,
		map[int64]bool{1: false, 2: true}, // 粘性号 1 槽满，2 号有空位
		map[int64]int{1: 0, 2: 0})

	ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformCursor)
	result, err := svc.SelectAccountWithLoadAwareness(
		ctx, nil, "sticky-cursor", "claude-opus-5-5", nil, "", int64(0))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.WaitPlan, "同档有空闲号时不得返回等待计划")
	require.Equal(t, int64(2), result.Account.ID, "必须换到有空槽的账号")
}

// TestCursorAllSlotsFullReportsNoAvailableAccounts 固定统一规则下「全满才等一次」的终点：
// 整池候选抢槽全部失败时，返回且只返回一个等待计划，目标在本档候选内。
// 用例名沿用白名单注册名；旧语义（全满直接报无可用账号）已随平台分轨一并废弃。
func TestCursorAllSlotsFullReportsNoAvailableAccounts(t *testing.T) {
	svc, _ := newCursorSlotTestService("sticky-cursor-full", 1,
		map[int64]bool{1: false, 2: false}, // 同档两个候选都抢不到槽
		map[int64]int{1: 0, 2: 0})

	ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, PlatformCursor)
	result, err := svc.SelectAccountWithLoadAwareness(
		ctx, nil, "sticky-cursor-full", "claude-opus-5-5", nil, "", int64(0))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.WaitPlan, "同档候选全部抢槽失败后必须产生唯一一次等待计划")
	require.Contains(t, []int64{1, 2}, result.WaitPlan.AccountID, "等待目标必须在本档候选内")
}
