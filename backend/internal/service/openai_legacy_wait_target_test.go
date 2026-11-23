//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSelectLoadAwareWaitTargetPrefersStickyOriginal 覆盖方案包 A #2（口径文件第四节偏差 #2）：
// legacy 负载感知路径 Layer 3 兜底等待目标必须优先会话粘性原号，且该提升只作用于等待
// 目标本身——被排除的原号不得提升，无绑定时保持既有排序首项。
func TestSelectLoadAwareWaitTargetPrefersStickyOriginal(t *testing.T) {
	const (
		highID     = int64(1)
		originalID = int64(2)
		session    = "sess"
	)

	ctx := context.Background()
	groupID := int64(94011)
	// openAISessionCacheKey 统一加 "openai:" 前缀，绑定键必须匹配该口径。
	stickyKey := "openai:" + session

	newService := func(t *testing.T, bindings map[string]int64) *OpenAIGatewayService {
		t.Helper()
		// 档位门固定关闭：否则 Priority=5 的粘性原号会在候选构造阶段被裁掉，
		// Layer 3 根本看不到它，无法覆盖原号提升逻辑。
		setPriorityTierStrictForTest(t, false)

		high := ffKiraOpenAICompactAccount(highID, nil)
		high.Priority = 1
		original := ffKiraOpenAICompactAccount(originalID, nil)
		original.Priority = 5

		cache := schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{highID: false, originalID: false},
		}
		svc := ffNewCompactLoadAwareService(cache, high, original)
		// schedulingConfig 在 cfg 非 nil 时直接返回 cfg.Gateway.Scheduling，
		// 夹具只开启了 LoadBatchEnabled，等待计划字段需显式给正值。
		svc.cfg.Gateway.Scheduling.FallbackWaitTimeout = 30 * time.Second
		svc.cfg.Gateway.Scheduling.FallbackMaxWaiting = 100
		svc.cache = &schedulerTestGatewayCache{sessionBindings: bindings}
		return svc
	}

	selectWait := func(t *testing.T, svc *OpenAIGatewayService, sessionHash string, excludedIDs map[int64]struct{}) *AccountSelectionResult {
		t.Helper()
		selection, err := svc.selectAccountWithLoadAwareness(ctx, &groupID, PlatformOpenAI, sessionHash, "", excludedIDs, false, "", false)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.NotNil(t, selection.WaitPlan, "抢槽全失败应走唯一等待出口 Layer 3")
		require.False(t, selection.Acquired)
		require.Greater(t, selection.WaitPlan.Timeout, time.Duration(0))
		require.Greater(t, selection.WaitPlan.MaxWaiting, 0)
		return selection
	}

	t.Run("sticky original becomes wait target", func(t *testing.T) {
		svc := newService(t, map[string]int64{stickyKey: originalID})
		selection := selectWait(t, svc, session, nil)
		require.Equal(t, originalID, selection.Account.ID)
		require.Equal(t, originalID, selection.WaitPlan.AccountID)
	})

	t.Run("no binding keeps sorted first high as wait target", func(t *testing.T) {
		svc := newService(t, nil)
		selection := selectWait(t, svc, "", nil)
		// 证明 high(Priority 1) 原生排在 original(Priority 5) 之前，
		// 上一子测试的原号优先来自 Layer 3 提升而非构造巧合。
		require.Equal(t, highID, selection.Account.ID)
		require.Equal(t, highID, selection.WaitPlan.AccountID)
	})

	t.Run("excluded sticky original falls back to high", func(t *testing.T) {
		svc := newService(t, map[string]int64{stickyKey: originalID})
		selection := selectWait(t, svc, session, map[int64]struct{}{originalID: {}})
		require.Equal(t, highID, selection.Account.ID)
		require.Equal(t, highID, selection.WaitPlan.AccountID)
	})
}
