package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// waitCountingCacheStub 在 helperConcurrencyCacheStub 之上记录账号等待计数的增减调用，
// 用于验证统一抢槽入口的排队上限语义。仅扩展计数字段/方法，不改生产接口。
type waitCountingCacheStub struct {
	*helperConcurrencyCacheStub

	canWait    bool
	waitErr    error
	increments int
	decrements int
}

func (s *waitCountingCacheStub) IncrementAccountWaitCount(_ context.Context, _ int64, _ int) (bool, error) {
	s.increments++
	return s.canWait, s.waitErr
}

func (s *waitCountingCacheStub) DecrementAccountWaitCount(_ context.Context, _ int64) error {
	s.decrements++
	return nil
}

func TestRecordConcurrencySlotFailure(t *testing.T) {
	failed := map[int64]struct{}{}
	attempts := 0

	// 外审第三轮 must_fix #1 整改后的语义：**候选耗尽不再由尝试次数判定**，
	// 而由「选号返回无候选」判定。因此调用方必须一直收到「继续换号」——
	// 否则池里 4 个以上账号时，前 3 个满、第 4 个空闲也会被误报成耗尽。
	for i := 1; i <= 60; i++ {
		require.True(t, recordConcurrencySlotFailure(nil, failed, int64(i), &attempts),
			"第 %d 次槽位失败必须继续换号（耗尽由选号无候选判定，不以次数代替）", i)
		require.Contains(t, failed, int64(i), "满载账号要记入本请求排除集合")
	}

	// 防御：nil 集合 / nil 计数不得 panic，直接判定不再换号。
	require.False(t, recordConcurrencySlotFailure(nil, nil, 1, &attempts))
	require.False(t, recordConcurrencySlotFailure(nil, failed, 9, nil))
}

func TestConcurrencySlotExhaustedResponse(t *testing.T) {
	status, errType, code, message := concurrencySlotExhaustedResponse()

	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "api_error", errType)
	require.Equal(t, "", code)
	require.Equal(t, infraerrors.NoAvailableAccounts, message,
		"终态文案必须与账号维度「等不到」出口逐字节一致")
}

// TestConcurrencySlotExhaustedResponse_MatchesAccountSlotError 锁定「换号耗尽」与
// 「直接等不到」两条路径的终态输出一致 —— 锁定项：账号维度等待语义不变。
func TestConcurrencySlotExhaustedResponse_MatchesAccountSlotError(t *testing.T) {
	directStatus, directType, directCode, directMessage := concurrencyErrorResponse(
		&ConcurrencyError{SlotType: "account", Limit: 3}, "account", "")
	exhaustedStatus, exhaustedType, exhaustedCode, exhaustedMessage := concurrencySlotExhaustedResponse()

	require.Equal(t, directStatus, exhaustedStatus)
	require.Equal(t, directType, exhaustedType)
	require.Equal(t, directCode, exhaustedCode)
	require.Equal(t, directMessage, exhaustedMessage)
}

// TestAcquireAccountSlotForSelection 覆盖统一抢槽入口 §4.3 第 0-4 步的五种情形。
func TestAcquireAccountSlotForSelection(t *testing.T) {
	newSelection := func() *service.AccountSelectionResult {
		return &service.AccountSelectionResult{
			Account: &service.Account{ID: 101, Concurrency: 3},
		}
	}
	newHelper := func(cache *helperConcurrencyCacheStub) *ConcurrencyHelper {
		return NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Millisecond)
	}

	t.Run("already_acquired_reuses_release_without_reacquire", func(t *testing.T) {
		cache := &helperConcurrencyCacheStub{accountSeq: []bool{true}}
		helper := newHelper(cache)
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")

		called := false
		wantRelease := func() { called = true }
		selection := newSelection()
		selection.Acquired = true
		selection.ReleaseFunc = wantRelease

		release, slotFull := helper.AcquireAccountSlotForSelection(c, selection, false, false, nil)
		require.False(t, slotFull)
		require.NotNil(t, release)
		release()
		require.True(t, called, "已持槽必须返回同一 ReleaseFunc")
		require.Equal(t, 0, cache.accountAcquireCalls, "已持槽不得再次抢槽")
		require.False(t, concurrencySlotWaitExhausted(c), "已持槽分支不得设置等待耗尽标记")
	})

	t.Run("not_acquired_immediate_success", func(t *testing.T) {
		cache := &helperConcurrencyCacheStub{accountSeq: []bool{true}}
		helper := newHelper(cache)
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")

		release, slotFull := helper.AcquireAccountSlotForSelection(c, newSelection(), false, false, nil)
		require.False(t, slotFull)
		require.NotNil(t, release)
		require.Equal(t, 1, cache.accountAcquireCalls, "未持槽应只做一次立即抢槽")
		require.False(t, concurrencySlotWaitExhausted(c), "立即抢槽成功分支不得设置等待耗尽标记")
		release()
	})

	t.Run("not_acquired_no_wait_plan_slot_full", func(t *testing.T) {
		cache := &helperConcurrencyCacheStub{accountSeq: []bool{false}}
		helper := newHelper(cache)
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")

		release, slotFull := helper.AcquireAccountSlotForSelection(c, newSelection(), false, false, nil)
		require.Nil(t, release)
		require.True(t, slotFull)
		require.Equal(t, 1, cache.accountAcquireCalls, "无等待计划时不得进入等待")
	})

	t.Run("not_acquired_wait_success", func(t *testing.T) {
		// 第一次立即抢槽失败，等待路径的立即抢槽成功（无需真实退避等待）。
		cache := &helperConcurrencyCacheStub{accountSeq: []bool{false, true}}
		helper := newHelper(cache)
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")

		selection := newSelection()
		selection.WaitPlan = &service.AccountWaitPlan{
			AccountID:      101,
			MaxConcurrency: 3,
			Timeout:        2 * time.Second,
		}
		streamStarted := false
		release, slotFull := helper.AcquireAccountSlotForSelection(c, selection, false, false, &streamStarted)
		require.False(t, slotFull)
		require.NotNil(t, release)
		require.Equal(t, 2, cache.accountAcquireCalls, "应先立即抢槽一次，再进入等待一次")
		require.False(t, streamStarted, "准入等待阶段不得把 streamStarted 置真")
		release()
	})

	t.Run("already_waited_only_immediate_then_slot_full", func(t *testing.T) {
		// 已等过时只做立即抢槽（accountSeq 仅一个 false）；若误入等待会多一次 acquire。
		cache := &helperConcurrencyCacheStub{accountSeq: []bool{false}}
		helper := newHelper(cache)
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")

		selection := newSelection()
		selection.WaitPlan = &service.AccountWaitPlan{
			AccountID:      101,
			MaxConcurrency: 3,
			Timeout:        2 * time.Second,
		}
		release, slotFull := helper.AcquireAccountSlotForSelection(c, selection, true, false, nil)
		require.Nil(t, release)
		require.True(t, slotFull)
		require.Equal(t, 1, cache.accountAcquireCalls, "已等过时只做一次立即抢槽，不得再等待")
	})

	t.Run("wait_queue_full_returns_slot_full_without_waiting", func(t *testing.T) {
		// 等待计数达到上限（canWait=false）→ 直接 slotFull，不进入等待，也不减数。
		base := &helperConcurrencyCacheStub{accountSeq: []bool{false}}
		cache := &waitCountingCacheStub{helperConcurrencyCacheStub: base, canWait: false}
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Millisecond)
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")

		selection := newSelection()
		selection.WaitPlan = &service.AccountWaitPlan{
			AccountID:      101,
			MaxConcurrency: 3,
			MaxWaiting:     2,
			Timeout:        2 * time.Second,
		}
		release, slotFull := helper.AcquireAccountSlotForSelection(c, selection, false, false, nil)
		require.Nil(t, release)
		require.True(t, slotFull, "排队上限达到时必须直接 slotFull")
		require.Equal(t, 1, cache.increments, "等待前应计数一次")
		require.Equal(t, 0, cache.decrements, "未成功计数不得减数")
		require.Equal(t, 1, base.accountAcquireCalls, "不可等待时不得进入等待路径（只做一次立即抢槽）")
		require.True(t, concurrencySlotWaitExhausted(c), "排队上限达到（等待机会用尽）也应设置标记")
	})

	t.Run("wait_success_decrements_wait_count_once", func(t *testing.T) {
		// 立即抢槽失败、计数成功、等待路径抢槽成功 → 计数必须被减回一次。
		base := &helperConcurrencyCacheStub{accountSeq: []bool{false, true}}
		cache := &waitCountingCacheStub{helperConcurrencyCacheStub: base, canWait: true}
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Millisecond)
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")

		selection := newSelection()
		selection.WaitPlan = &service.AccountWaitPlan{
			AccountID:      101,
			MaxConcurrency: 3,
			MaxWaiting:     2,
			Timeout:        2 * time.Second,
		}
		release, slotFull := helper.AcquireAccountSlotForSelection(c, selection, false, false, nil)
		require.False(t, slotFull)
		require.NotNil(t, release)
		release()
		require.Equal(t, 2, base.accountAcquireCalls, "先立即抢槽一次，再等待抢槽一次")
		require.Equal(t, 1, cache.increments, "等待前应计数一次")
		require.Equal(t, 1, cache.decrements, "等待结束后计数必须减回一次")
	})

	t.Run("wait_failure_marks_wait_exhausted", func(t *testing.T) {
		// 立即抢槽失败 + 等待超时 → 必须设置请求级等待耗尽标记，
		// 让同一请求的后续候选只做立即抢槽，不再重复完整等待。
		base := &helperConcurrencyCacheStub{accountSeq: []bool{false, false}}
		cache := &waitCountingCacheStub{helperConcurrencyCacheStub: base, canWait: true}
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Millisecond)
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")

		selection := newSelection()
		selection.WaitPlan = &service.AccountWaitPlan{
			AccountID:      101,
			MaxConcurrency: 3,
			MaxWaiting:     2,
			Timeout:        10 * time.Millisecond,
		}
		release, slotFull := helper.AcquireAccountSlotForSelection(c, selection, false, false, nil)
		require.Nil(t, release)
		require.True(t, slotFull, "等待超时必须返回 slotFull")
		require.True(t, concurrencySlotWaitExhausted(c), "等待失败后必须设置请求级等待耗尽标记")
		require.Equal(t, 2, base.accountAcquireCalls, "立即抢槽一次 + 等待入口立即抢槽一次")
		require.Equal(t, 1, cache.increments, "等待前应计数一次")
		require.Equal(t, 1, cache.decrements, "等待失败后计数必须减回一次")
	})

	t.Run("wait_exhausted_marker_prevents_second_wait", func(t *testing.T) {
		// 端到端：第一次候选等待失败设置标记；后续候选通过 concurrencySlotWaitExhausted(c)
		// 读到 true 作为 alreadyWaited，只做立即抢槽，不再进入等待。
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")

		firstCache := &helperConcurrencyCacheStub{accountSeq: []bool{false, false}}
		firstHelper := newHelper(firstCache)
		firstSelection := newSelection()
		firstSelection.WaitPlan = &service.AccountWaitPlan{
			AccountID:      101,
			MaxConcurrency: 3,
			Timeout:        10 * time.Millisecond,
		}
		_, firstSlotFull := firstHelper.AcquireAccountSlotForSelection(c, firstSelection, false, false, nil)
		require.True(t, firstSlotFull, "前置条件：第一个候选等待失败")
		require.True(t, concurrencySlotWaitExhausted(c), "前置条件：请求级等待耗尽标记已设置")

		// 第二个候选换用全新 helper/cache，仅通过请求上下文传递标记，
		// 模拟调用方把 concurrencySlotWaitExhausted(c) 作为 alreadyWaited 传入。
		secondCache := &helperConcurrencyCacheStub{accountSeq: []bool{false}}
		secondHelper := newHelper(secondCache)
		secondSelection := newSelection()
		secondSelection.WaitPlan = &service.AccountWaitPlan{
			AccountID:      102,
			MaxConcurrency: 3,
			Timeout:        10 * time.Millisecond,
		}
		release, slotFull := secondHelper.AcquireAccountSlotForSelection(
			c, secondSelection, concurrencySlotWaitExhausted(c), false, nil)
		require.Nil(t, release)
		require.True(t, slotFull)
		require.Equal(t, 1, secondCache.accountAcquireCalls, "已等过时只做一次立即抢槽，不得再次进入等待")
	})
}
