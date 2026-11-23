//go:build unit

package service

import (
	"context"
	"errors"
	"math/rand"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// selectorAccountRepoStub 只实现 SelectCompositeCrossChainAccount 需要的 GetByID；
// 其余 AccountRepository 方法由嵌入的 nil 接口补全（本测试不会触达）。
type selectorAccountRepoStub struct {
	AccountRepository
	byID map[int64]*Account
	err  map[int64]error
}

func (r *selectorAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if err := r.err[id]; err != nil {
		return nil, err
	}
	return r.byID[id], nil
}

// selectorVetoSnapshotCache 按账号 ID 返回刷新对象，用于覆盖抢槽后终检的
// vetoed 分支（不同账号在等待期间倍率变化不同）。
type selectorVetoSnapshotCache struct {
	SchedulerCache
	byID map[int64]*Account
}

func (c *selectorVetoSnapshotCache) GetAccount(_ context.Context, id int64) (*Account, error) {
	return c.byID[id], nil
}

func TestSelectCompositeSupplyByPriority(t *testing.T) {
	acc := func(id int64, priority int) *Account {
		return &Account{ID: id, Priority: priority}
	}

	t.Run("nilAndEmpty", func(t *testing.T) {
		require.Nil(t, selectCompositeSupplyByPriority(nil, nil, nil))
		require.Nil(t, selectCompositeSupplyByPriority([]*Account{}, nil, nil))
		require.Nil(t, selectCompositeSupplyByPriority([]*Account{nil, nil}, nil, nil))
	})

	t.Run("excludedAndNilEntries", func(t *testing.T) {
		low := acc(1, 0)
		high := acc(2, 1)
		// low 优先级数值更小但被 excluded，应选中 high。
		got := selectCompositeSupplyByPriority(
			[]*Account{nil, low, high}, map[int64]struct{}{1: {}}, nil)
		require.Same(t, high, got)
	})

	t.Run("minPriorityTier", func(t *testing.T) {
		low1 := acc(1, 5)
		low2 := acc(2, 5)
		high := acc(3, 9)
		// rnd==nil 取该档第一个；不得越到高 priority 数值档。
		got := selectCompositeSupplyByPriority([]*Account{high, low1, low2}, nil, nil)
		require.Same(t, low1, got)
	})

	t.Run("minPriorityTierExcluded", func(t *testing.T) {
		lo := acc(1, 0)
		hi := acc(2, 3)
		got := selectCompositeSupplyByPriority(
			[]*Account{lo, hi}, map[int64]struct{}{1: {}}, nil)
		require.Same(t, hi, got)
	})

	t.Run("sameTierRandomWithFixedSeed", func(t *testing.T) {
		candidates := []*Account{acc(1, 2), acc(2, 2), acc(3, 2)}
		rnd := rand.New(rand.NewSource(42))
		seen := make(map[int64]bool)
		for i := 0; i < 200; i++ {
			got := selectCompositeSupplyByPriority(candidates, nil, rnd)
			require.NotNil(t, got)
			require.Equal(t, 2, got.Priority, "同档随机不得越出最小 priority 档")
			seen[got.ID] = true
		}
		require.Greater(t, len(seen), 1, "同档内应出现随机分布，而不是固定第一个")
		require.LessOrEqual(t, len(seen), 3, "不得选出档外账号")
	})

	// ============ 方法级集成：复用既有 fake/资格门，轻量构造 ============
	const (
		groupID int64 = 77
		model         = "test-model"
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
	servableLow := newAcc(1, 5, PlatformOpenAI)     // 可服务，priority 数值较大
	servableHigh := newAcc(2, 1, PlatformOpenAI)    // 可服务，priority 数值最小
	nonServable := newAcc(3, 0, PlatformAnthropic)  // images 端点不可服务，priority 数值最小
	repo := &selectorAccountRepoStub{byID: map[int64]*Account{
		servableLow.ID:   servableLow,
		servableHigh.ID:  servableHigh,
		nonServable.ID:   nonServable,
	}}
	svc := &GatewayService{accountRepo: repo}
	ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{
		ID:       groupID,
		Hydrated: true,
		Platform: PlatformOpenAI,
		Status:   StatusActive,
	})
	supply := []CompositeOwnershipClaimant{
		{AccountID: servableLow.ID, Platform: PlatformOpenAI},
		{AccountID: servableHigh.ID, Platform: PlatformOpenAI},
		{AccountID: nonServable.ID, Platform: PlatformAnthropic},
	}

	t.Run("integrationOnlyServableAndHighestPriority", func(t *testing.T) {
		got, release, err := svc.SelectCompositeCrossChainAccount(
			ctx, groupID, model, CompositeEndpointImages, supply, nil)
		require.NoError(t, err)
		// priority 数值最小的 3 号不可服务被过滤；可服务的 2 号胜出。
		require.Same(t, servableHigh, got)
		require.NotNil(t, release, "成功选号必须返回非空 release")
		require.NotPanics(t, release, "release 必须可安全调用")
	})

	t.Run("integrationExcludedTakesEffect", func(t *testing.T) {
		got, release, err := svc.SelectCompositeCrossChainAccount(
			ctx, groupID, model, CompositeEndpointImages, supply,
			map[int64]struct{}{servableHigh.ID: {}})
		require.NoError(t, err)
		require.Same(t, servableLow, got)
		require.NotNil(t, release)
		require.NotPanics(t, release)
	})

	t.Run("integrationReturnsNonNilReleaseWithoutSlotBlocking", func(t *testing.T) {
		// concurrencyService 为 nil 时 tryAcquireAccountSlot 返回 Acquired=true，
		// 不应触发槽满换号；release 非空且可安全调用。
		got, release, err := svc.SelectCompositeCrossChainAccount(
			ctx, groupID, model, CompositeEndpointImages,
			[]CompositeOwnershipClaimant{
				{AccountID: servableHigh.ID, Platform: PlatformOpenAI},
				{AccountID: servableLow.ID, Platform: PlatformOpenAI},
			}, nil)
		require.NoError(t, err)
		require.Same(t, servableHigh, got, "无槽满时不应换号")
		require.NotNil(t, release)
		require.NotPanics(t, release)
	})

	t.Run("integrationNoCandidate", func(t *testing.T) {
		got, release, err := svc.SelectCompositeCrossChainAccount(
			ctx, groupID, model, CompositeEndpointImages,
			[]CompositeOwnershipClaimant{{AccountID: nonServable.ID}}, nil)
		require.Nil(t, got)
		require.Nil(t, release)
		require.ErrorIs(t, err, ErrNoAvailableAccounts)
	})

	t.Run("integrationReadErrorFailsClosed", func(t *testing.T) {
		failing := &GatewayService{accountRepo: &selectorAccountRepoStub{
			err: map[int64]error{9: errors.New("db read failed")},
		}}
		got, release, err := failing.SelectCompositeCrossChainAccount(
			ctx, groupID, model, CompositeEndpointImages,
			[]CompositeOwnershipClaimant{{AccountID: 9}}, nil)
		require.Nil(t, got)
		require.Nil(t, release)
		require.Error(t, err)
		require.False(t, errors.Is(err, ErrNoAvailableAccounts), "读取错误必须是明确错误，不能降级为选不到号")
	})

	t.Run("integrationInvalidArgs", func(t *testing.T) {
		_, release, err := svc.SelectCompositeCrossChainAccount(
			ctx, 0, model, CompositeEndpointImages, supply, nil)
		require.Error(t, err)
		require.Nil(t, release)
		_, release, err = (&GatewayService{}).SelectCompositeCrossChainAccount(
			ctx, groupID, model, CompositeEndpointImages, supply, nil)
		require.Error(t, err)
		require.Nil(t, release)
	})

	t.Run("integrationProfitTerminalVetoReleasesAndSwapsAccount", func(t *testing.T) {
		// 两个可服务账号候选阶段倍率均合格；抢槽后按最新快照终检：
		// A 倍率越线被否（释放槽、换号），B 刷新后仍合格并返回刷新对象。
		newProfitAcc := func(id int64, priority int, rate float64) *Account {
			acc := gatewayProfitTestAccount(id, PlatformOpenAI, rate, groupID)
			acc.Priority = priority
			return &acc
		}
		a := newProfitAcc(11, 1, 0.2) // priority 数值最小，先被选中；刷新后越线
		b := newProfitAcc(12, 2, 0.2) // 刷新后仍合格

		aFresh := *a
		aRate := 0.8
		aFresh.RateMultiplier = &aRate
		bFresh := *b
		bRate := 0.2
		bFresh.RateMultiplier = &bRate

		svcVeto := &GatewayService{
			accountRepo: &selectorAccountRepoStub{byID: map[int64]*Account{
				a.ID: a,
				b.ID: b,
			}},
			schedulerSnapshot: NewSchedulerSnapshotService(
				&selectorVetoSnapshotCache{byID: map[int64]*Account{
					a.ID: &aFresh,
					b.ID: &bFresh,
				}},
				nil,
				gatewayProfitAccountRepo{},
				nil,
				nil,
			),
		}
		gateCtx := context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{
			groupID:   groupID,
			platform:  PlatformOpenAI,
			threshold: 0.5,
		})
		supplyVeto := []CompositeOwnershipClaimant{
			{AccountID: a.ID, Platform: PlatformOpenAI},
			{AccountID: b.ID, Platform: PlatformOpenAI},
		}

		got, release, err := svcVeto.SelectCompositeCrossChainAccount(
			gateCtx, groupID, model, CompositeEndpointImages, supplyVeto, nil)
		require.NoError(t, err)
		require.NotNil(t, release)
		require.NotSame(t, a, got, "A 被终检否决后必须换号")
		require.Same(t, &bFresh, got, "通过时必须返回刷新后的账号对象")
		require.NotPanics(t, release)
	})
}
