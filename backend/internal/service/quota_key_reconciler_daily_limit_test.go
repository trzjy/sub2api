//go:build unit

package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// dailyLimitErr 构造与生产链路同形的错误：真实链路上 shareai 客户端先经
// classifyUpstreamError 把上游 msg 归类为 UpstreamDailyRefreshLimit（携带
// ErrUpstreamDailyRefreshLimit 哨兵），quota_key_service.pull 再把该归类错误
// 原样透传给 reconciler。测试替身必须复现这一形态，errors.Is 才能命中。
func dailyLimitErr(msg string) error {
	return newUpstreamDailyRefreshLimitError(msg)
}

// TestReconciler_DailyRefreshLimit_NotRecordedAsTaskFailure pins the 2026-10-27 fix
// at the reconciler level:
//
// shareai 上游每 key 每日只允许刷新 5 次（8:00 重置），quota_key_reconcile 周期 5 分钟
// 必然撞上该上限。该错误属「可预期节流」，心跳不得记为任务失败 —— 否则 ops 面板
// jobScore 长期被扣，并掩盖真正的任务故障（生产实证 2026-10-27：该任务
// last_success 距今 15.87 小时、last_error 非空，是 8 个心跳里唯一失败项）。
func TestReconciler_DailyRefreshLimit_NotRecordedAsTaskFailure(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0
	mw.candidates = []int64{30, 40}

	puller := newFakePuller()
	limitErr := dailyLimitErr("当日刷新次数达到上限(5次)，将在每日8:00重置，还需6小时48分钟")
	puller.errOnIDs[30] = limitErr
	puller.errOnIDs[40] = limitErr
	// 拉号后池水位涨到阈值（模拟真实入库），使本轮走「无缺口」收尾而非 shortfall 分支。
	puller.afterPull = func(int64, error) {
		mw.mu.Lock()
		mw.level = 10
		mw.mu.Unlock()
	}

	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)
	r.runOnce()

	// 心跳必须记成功（不记任务失败）。
	require.True(t, ops.lastSucc,
		"每日刷新上限属可预期节流，心跳不得记为任务失败")
	require.Empty(t, ops.lastError,
		"每日刷新上限不得写入任务失败错误")
	// LastResult 如实写明节流条数与「次日重置」。
	require.Contains(t, ops.lastResult, "daily refresh limit",
		"LastResult 须如实写明每日刷新上限节流")
}

// TestReconciler_RealPullFailure_StillRecordedAsTaskFailure pins the hard constraint:
// 真·拉号失败仍必须记任务失败，不得被「每日节流不算失败」的放宽波及。
func TestReconciler_RealPullFailure_StillRecordedAsTaskFailure(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0
	mw.candidates = []int64{30, 40}

	puller := newFakePuller()
	// 真失败：未知上游失败（携带 ErrUpstreamKeyBlocked 哨兵）。
	blockErr := fmt.Errorf("%w: 系统当前繁忙，请稍后再试", ErrUpstreamKeyBlocked)
	puller.errOnIDs[30] = blockErr
	puller.errOnIDs[40] = blockErr

	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)
	r.runOnce()

	require.False(t, ops.lastSucc,
		"真拉号失败仍须记任务失败")
	require.NotEmpty(t, ops.lastError,
		"真拉号失败须写入任务失败错误")
}
