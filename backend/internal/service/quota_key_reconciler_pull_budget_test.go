//go:build unit

package service

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReconciler_PullByBudget_PullsEveryEligibleKey 锁定 D4/D5 的新口径：
// 打开 pullByBudget 后，补池不再看池级总量水位——哪怕水位早已"达标"（level >= threshold），
// 只要还有有效码、还有今天没撞上游上限的拉号额度，就必须去拉。
//
// 为什么必须有这条用例：旧口径的水位是用 accounts.schedulable 算的，而额度耗尽的账号
// 在表里依然是 schedulable，于是水位永远达标、拉号永远不触发 —— 生产上就表现成
// "号池没有可用账号，但一个号也不拉"。这条用例是那个缺陷的回归守卫。
func TestReconciler_PullByBudget_PullsEveryEligibleKey(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	// 水位高于阈值（旧口径下会直接停拉）：新口径必须照样拉。
	mw.level = quotaKeyWaterLevelThreshold + 5
	mw.candidates = []int64{100, 101, 102}

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)
	r.SetPullByBudget(true)

	r.runOnce()

	require.Equal(t, []int64{100, 101, 102}, puller.calledIDs(),
		"pull-by-budget must pull every eligible key regardless of the pool water level")
	// 候选查询不再按缺口截断（传 0 = 不限量），否则等于又把水位偷偷带回来了。
	require.Equal(t, []int{0}, mw.requestedLimits(),
		"candidate listing must not be truncated by the pool deficit in pull-by-budget mode")
	require.Contains(t, ops.lastResultVal(), "reconciled 3 quota keys",
		"heartbeat must report the real number of successful pulls")
}

// TestReconciler_PullByBudget_DailyLimitCountsAsThrottle 验证新口径下"今日拉号额度已尽"
// 仍按可预期节流处理：心跳记成功（不是任务失败），LastResult 写明次日重置。
func TestReconciler_PullByBudget_DailyLimitCountsAsThrottle(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0
	mw.candidates = []int64{30, 40}

	puller := newFakePuller()
	limitErr := dailyLimitErr("当日刷新次数达到上限(5次)，将在每日8:00重置")
	puller.errOnIDs[30] = limitErr
	puller.errOnIDs[40] = limitErr

	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)
	r.SetPullByBudget(true)

	r.runOnce()

	require.True(t, ops.lastSuccessCalled(),
		"daily pull budget exhaustion is expected throttling, not a task failure")
	require.Empty(t, ops.lastErrorVal(), "daily limit must not be recorded as a task failure")
	require.Contains(t, ops.lastResultVal(), "daily refresh limit",
		"heartbeat must state the throttle and the next-day reset")
}

// TestReconciler_PullByBudget_MountFailedStillStopsRound 验证新口径保留了 F4 非幂等边界：
// 上游已发号但本地挂载失败时，本轮后续候选一律不再拉（shareai 拉号非幂等，重试会重复消耗额度）。
func TestReconciler_PullByBudget_MountFailedStillStopsRound(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0
	mw.candidates = []int64{500, 501, 502}

	puller := newFakePuller()
	puller.errOnIDs[501] = fmt.Errorf("mount account to groups: %w", ErrQuotaKeyMountFailed)

	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)
	r.SetPullByBudget(true)

	r.runOnce()

	require.Equal(t, []int64{500, 501}, puller.calledIDs(),
		"mount failure must stop the round: no pull may follow the failed key")
	require.False(t, ops.lastSuccessCalled(), "mount failure must be reported as a failed round")
}
