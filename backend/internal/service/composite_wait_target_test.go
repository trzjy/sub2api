//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// compositeWaitTargetCacheStub 仅实现 GetSessionAccountID，返回固定账号 ID；
// 其余 GatewayCache 方法由嵌入的 nil 接口补全（本测试不会触达）。
type compositeWaitTargetCacheStub struct {
	GatewayCache
	id int64
}

func (c *compositeWaitTargetCacheStub) GetSessionAccountID(_ context.Context, _ int64, _ string) (int64, error) {
	if c.id <= 0 {
		return 0, ErrStickySessionNotFound
	}
	return c.id, nil
}

func TestSelectCompositeCrossChainWaitTarget(t *testing.T) {
	const (
		groupID     int64 = 77
		model             = "test-model"
		sessionHash       = "composite-wait-target-session"
	)
	newAcc := func(id int64, priority int, platform string) *Account {
		return &Account{
			ID:          id,
			Platform:    platform,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Priority:    priority,
			GroupIDs:    []int64{groupID},
		}
	}
	high := newAcc(1, 1, PlatformOpenAI)           // 可服务，priority 数值最小
	low := newAcc(2, 5, PlatformOpenAI)            // 可服务，priority 数值较大
	nonServable := newAcc(3, 0, PlatformAnthropic) // images 端点不可服务

	repo := &selectorAccountRepoStub{byID: map[int64]*Account{
		high.ID:        high,
		low.ID:         low,
		nonServable.ID: nonServable,
	}}
	ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{
		ID:       groupID,
		Hydrated: true,
		Platform: PlatformOpenAI,
		Status:   StatusActive,
	})
	supply := []CompositeOwnershipClaimant{
		{AccountID: high.ID, Platform: PlatformOpenAI},
		{AccountID: low.ID, Platform: PlatformOpenAI},
	}

	t.Run("noSessionBindingPicksMinPriorityHigh", func(t *testing.T) {
		// 缓存存在但无会话绑定：回退到最小 priority 档。
		svc := &GatewayService{
			accountRepo: repo,
			cache:       &compositeWaitTargetCacheStub{},
		}
		selection, err := svc.SelectCompositeCrossChainWaitTarget(
			ctx, groupID, model, CompositeEndpointImages, supply, nil, sessionHash)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Same(t, high, selection.Account)
		require.False(t, selection.Acquired)

		require.NotNil(t, selection.WaitPlan)
		require.Equal(t, high.ID, selection.WaitPlan.AccountID)
		require.Positive(t, selection.WaitPlan.Timeout)
		require.Positive(t, selection.WaitPlan.MaxWaiting)
	})

	t.Run("sessionBindingPicksOriginalLow", func(t *testing.T) {
		// 会话绑定到 low：即使 high 优先级更小，仍优先等待原号。
		svc := &GatewayService{
			accountRepo: repo,
			cache:       &compositeWaitTargetCacheStub{id: low.ID},
		}
		selection, err := svc.SelectCompositeCrossChainWaitTarget(
			ctx, groupID, model, CompositeEndpointImages, supply, nil, sessionHash)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Same(t, low, selection.Account)
		require.NotNil(t, selection.WaitPlan)
		require.Equal(t, low.ID, selection.WaitPlan.AccountID)
	})

	t.Run("excludedHighPicksLow", func(t *testing.T) {
		svc := &GatewayService{accountRepo: repo}
		selection, err := svc.SelectCompositeCrossChainWaitTarget(
			ctx, groupID, model, CompositeEndpointImages, supply,
			map[int64]struct{}{high.ID: {}}, sessionHash)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Same(t, low, selection.Account)
		require.NotNil(t, selection.WaitPlan)
		require.Equal(t, low.ID, selection.WaitPlan.AccountID)
	})

	t.Run("onlyNonServableSupplyErrors", func(t *testing.T) {
		svc := &GatewayService{accountRepo: repo}
		selection, err := svc.SelectCompositeCrossChainWaitTarget(
			ctx, groupID, model, CompositeEndpointImages,
			[]CompositeOwnershipClaimant{{AccountID: nonServable.ID, Platform: PlatformAnthropic}},
			nil, sessionHash)
		require.Nil(t, selection)
		require.ErrorIs(t, err, ErrNoAvailableAccounts)
	})

	t.Run("doesNotAcquireSlot", func(t *testing.T) {
		// 本方法只给等待目标：不抢槽、不返回 release。
		svc := &GatewayService{accountRepo: repo}
		selection, err := svc.SelectCompositeCrossChainWaitTarget(
			ctx, groupID, model, CompositeEndpointImages, supply, nil, sessionHash)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.WaitPlan)
		require.False(t, selection.Acquired)
		require.Nil(t, selection.ReleaseFunc)
	})
}
