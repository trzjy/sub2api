//go:build unit

package handler

import (
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// slotOwnershipDecision 构造「账号模型归属命中 + messages 走 anthropic 链」的选号决策。
// Source=Account 时 CompositeSelectedAccountServes 要求账号出现在 SupplyAccounts 里，
// 之后再过链路可服务判定；这正是归属拒绝要核对的两道门。
func slotOwnershipDecision(claimants ...service.CompositeOwnershipClaimant) service.CompositeRouteDecision {
	return service.CompositeRouteDecision{
		Matched:        true,
		Source:         service.CompositeRouteSourceAccount,
		TargetPlatform: service.PlatformAnthropic,
		ServiceChain:   service.CompositeChainAnthropic,
		Endpoint:       service.CompositeEndpointMessages,
		SupplyAccounts: claimants,
	}
}

// slotOwnershipTestContext 建一个带 /v1/messages 请求且已盖上归属决策的 gin context。
func slotOwnershipTestContext(claimants ...service.CompositeOwnershipClaimant) *gin.Context {
	c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")
	c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(
		c.Request.Context(), slotOwnershipDecision(claimants...)))
	return c
}

// slotOwnershipTestHelper 用无状态 cache 构造一个 helper，归属核对本身不抢槽。
func slotOwnershipTestHelper() *ConcurrencyHelper {
	return NewConcurrencyHelper(
		service.NewConcurrencyService(&helperConcurrencyCacheStub{}),
		SSEPingFormatNone,
		time.Millisecond,
	)
}

// TestRejectCompositeIneligible_ReleasesAndDoesNotConsumeWait 锁定归属拒绝的两条硬契约：
//  1. 合格账号原样放行；不合格账号释放选号器已占的槽、记入排除集合并登记独立计数；
//  2. 归属拒绝**不**消耗本次请求唯一一次账号槽位等待（不得设置等待耗尽标记）。
func TestRejectCompositeIneligible_ReleasesAndDoesNotConsumeWait(t *testing.T) {
	t.Run("eligible_keeps_slot_and_does_not_reject", func(t *testing.T) {
		c := slotOwnershipTestContext(service.CompositeOwnershipClaimant{AccountID: 1})

		releaseCalls := 0
		failed := map[int64]struct{}{}
		rejectedCount := 0
		selection := &service.AccountSelectionResult{
			Account:     &service.Account{ID: 1, Platform: service.PlatformAnthropic},
			Acquired:    true,
			ReleaseFunc: func() { releaseCalls++ },
		}

		require.False(t, slotOwnershipTestHelper().RejectCompositeIneligible(c, selection, failed, &rejectedCount),
			"anthropic 账号可服务 messages，不应被归属拒绝")
		require.Equal(t, 0, releaseCalls, "合格账号的槽必须保留给后续转发")
		require.Empty(t, failed, "合格账号不得进入本请求排除集合")
		require.Equal(t, 0, rejectedCount, "合格账号不得登记归属拒绝计数")
		require.False(t, concurrencySlotWaitExhausted(c), "放行分支不得触碰等待耗尽标记")
	})

	t.Run("ineligible_releases_slot_and_does_not_consume_wait", func(t *testing.T) {
		c := slotOwnershipTestContext(service.CompositeOwnershipClaimant{AccountID: 1})

		releaseCalls := 0
		failed := map[int64]struct{}{}
		rejectedCount := 0
		selection := &service.AccountSelectionResult{
			Account:     &service.Account{ID: 1, Platform: service.PlatformTypeSafe},
			Acquired:    true,
			ReleaseFunc: func() { releaseCalls++ },
		}

		require.True(t, slotOwnershipTestHelper().RejectCompositeIneligible(c, selection, failed, &rejectedCount),
			"TypeSafe 不在 messages 的 anthropic/openai/gemini 任一链上，必须拒绝")
		require.Equal(t, 1, releaseCalls, "拒绝时必须释放选号器已占的槽，且只释放一次")
		require.Contains(t, failed, int64(1), "不合格账号要记入本请求排除集合")
		require.Len(t, failed, 1)
		require.Equal(t, 1, rejectedCount, "归属拒绝独立计数应递增一次")
		require.False(t, concurrencySlotWaitExhausted(c),
			"归属拒绝不得消耗唯一一次等待，否则后续合格账号无法排队")
	})
}

// TestRejectCompositeIneligible_ThenWaitStillHappens 端到端证明：先拒绝一个不合格账号，
// 后面对带 WaitPlan 的合格账号仍能进入等待（等待计数 +1、抢槽调用 2 次）。
// 若归属拒绝误设了等待耗尽标记，AcquireAccountSlotForSelection 只会立即抢槽一次。
func TestRejectCompositeIneligible_ThenWaitStillHappens(t *testing.T) {
	c := slotOwnershipTestContext(
		service.CompositeOwnershipClaimant{AccountID: 1, Platform: service.PlatformTypeSafe},
		service.CompositeOwnershipClaimant{AccountID: 2, Platform: service.PlatformAnthropic},
	)

	base := &helperConcurrencyCacheStub{accountSeq: []bool{false, false}}
	cache := &waitCountingCacheStub{helperConcurrencyCacheStub: base, canWait: true}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Millisecond)

	// 第一步：不合格账号（TypeSafe）被归属拒绝。
	failed := map[int64]struct{}{}
	rejectedCount := 0
	ineligible := &service.AccountSelectionResult{
		Account:     &service.Account{ID: 1, Platform: service.PlatformTypeSafe},
		Acquired:    true,
		ReleaseFunc: func() {},
	}
	require.True(t, helper.RejectCompositeIneligible(c, ineligible, failed, &rejectedCount))
	require.Contains(t, failed, int64(1))
	require.Equal(t, 1, rejectedCount)
	require.False(t, concurrencySlotWaitExhausted(c), "前置条件：拒绝后等待仍未耗尽")

	// 第二步：合格账号（anthropic）带等待计划抢槽；两次抢槽均失败以触发等待路径。
	eligible := &service.AccountSelectionResult{
		Account: &service.Account{ID: 2, Platform: service.PlatformAnthropic, Concurrency: 1},
		WaitPlan: &service.AccountWaitPlan{
			AccountID:      2,
			MaxConcurrency: 1,
			MaxWaiting:     2,
			Timeout:        10 * time.Millisecond,
		},
	}
	release, slotFull := helper.AcquireAccountSlotForSelection(
		c, eligible, concurrencySlotWaitExhausted(c), false, nil)
	require.Nil(t, release)
	require.True(t, slotFull, "两次抢槽都失败且等待超时，应返回 slotFull")

	require.Equal(t, 1, cache.increments, "等待计数增加一次，证明归属拒绝没有吃掉唯一一次等待")
	require.Equal(t, 1, cache.decrements, "等待结束后计数必须减回一次")
	require.Equal(t, 2, base.accountAcquireCalls, "立即抢槽一次 + 等待入口抢槽一次")
	require.True(t, concurrencySlotWaitExhausted(c), "等待确实发生过，耗尽标记应由等待路径设置")
}

func TestNoAvailableBySlotOrOwnership(t *testing.T) {
	tests := []struct {
		name              string
		slotFullCount     int
		ownershipRejected int
		want              bool
	}{
		{name: "none", slotFullCount: 0, ownershipRejected: 0, want: false},
		{name: "slot_only", slotFullCount: 1, ownershipRejected: 0, want: true},
		{name: "ownership_only", slotFullCount: 0, ownershipRejected: 1, want: true},
		{name: "both", slotFullCount: 1, ownershipRejected: 1, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, NoAvailableBySlotOrOwnership(tt.slotFullCount, tt.ownershipRejected))
		})
	}
}

func TestClassifySelectionFailureError_SessionSlotsFullIsAPIError(t *testing.T) {
	// fallback 故意用不同的 ErrType/Message，以证明本分支真实改写了分类结果，
	// 而不是恰好与兜底值相同。
	fallback := noAccountErrorClassification{
		Status:  http.StatusServiceUnavailable,
		ErrType: "service_unavailable",
		Message: "fallback should be replaced",
	}

	got := classifySelectionFailureError(
		&service.NoAvailableAccountsError{SessionSlotsFull: 1}, fallback)

	require.Equal(t, http.StatusServiceUnavailable, got.Status)
	require.Equal(t, "api_error", got.ErrType, "会话/并发槽占满必须归入统一 api_error 口径")
	require.Equal(t, infraerrors.NoAvailableAccounts, got.Message)
}

func TestConcurrencyErrorResponse_UserDimensionUnchanged(t *testing.T) {
	tests := []struct {
		name     string
		slotType string
		err      error
	}{
		{name: "user_limit", slotType: "user", err: &ConcurrencyError{SlotType: "user", Limit: 5}},
		{name: "user_timeout", slotType: "user", err: &ConcurrencyError{SlotType: "user", IsTimeout: true, Limit: 5}},
		{name: "group_limit", slotType: "group", err: &ConcurrencyError{SlotType: "group", Limit: 2}},
		{name: "group_timeout", slotType: "group", err: &ConcurrencyError{SlotType: "group", IsTimeout: true, Limit: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, errType, code, message := concurrencyErrorResponse(tt.err, tt.slotType, "")
			require.Equal(t, http.StatusTooManyRequests, status, "user/group 维度仍是 429")
			require.Equal(t, "rate_limit_error", errType)
			require.Equal(t, gatewayConcurrencyLimitCode, code)
			require.NotEmpty(t, message)
		})
	}
}
