package handler

import (
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

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
	require.Equal(t, gatewayConcurrencyLimitCode, code)
	require.Equal(t, infraerrors.AccountSlotConcurrencyLimit, message,
		"终态文案必须与既有「槽位等不到」出口逐字节一致")
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
	})

	t.Run("not_acquired_immediate_success", func(t *testing.T) {
		cache := &helperConcurrencyCacheStub{accountSeq: []bool{true}}
		helper := newHelper(cache)
		c, _ := newHelperTestContext(http.MethodPost, "/v1/messages")

		release, slotFull := helper.AcquireAccountSlotForSelection(c, newSelection(), false, false, nil)
		require.False(t, slotFull)
		require.NotNil(t, release)
		require.Equal(t, 1, cache.accountAcquireCalls, "未持槽应只做一次立即抢槽")
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
}
