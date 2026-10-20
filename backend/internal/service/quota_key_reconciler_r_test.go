//go:build unit

package service

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Lane R（派发单 D-CRT-01R / 方案 §18.4 第 10 条）：拉后重算失败立即停轮。
// 本文件只承载 R 的回归用例，生产改动在 quota_key_reconciler.go 的 runOnce；
// 替身沿用 quota_key_reconciler_test.go 的 newFakeModelWater / newFakePuller /
// newReconOpsRepo / newTestReconciler，不新建平行替身。
// ---------------------------------------------------------------------------

// TestQuotaKeyReconciler_RecomputeAfterPullFails_StopsRoundNoFurtherPull 是 R 的主回归：
// 拉号后重算 WaterLevel 失败 → 池级真相读不出、"还差几个号"未知，此时继续拉号等于在未知
// 总量下白白消耗上游（shareai 拉号非幂等）。故必须写失败心跳并立即 return，不再触碰下一 key。
//
// 构造：初始 WaterLevel（第 1 次调用）成功以驱动首拉；第 2 次起（拉号后重算）失败。
func TestQuotaKeyReconciler_RecomputeAfterPullFails_StopsRoundNoFurtherPull(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0 // 池级总量 0 → 缺口 10
	mw.candidates = []int64{100, 101, 102, 103}
	// 初始 WaterLevel（第 1 次调用）成功以驱动首拉；第 2 次起（拉号后重算）失败。
	mw.waterErrAfter = 1
	mw.waterErrAfterMsg = "recompute water level after pull failed"

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	// 拉后重算失败 → 立即停轮：命中的 key 之后无任何候选被拉（含 101/102/103）。
	ids := puller.calledIDs()
	require.Equal(t, []int64{100}, ids,
		"recompute-after-pull failure must stop the round; no later candidate may be pulled")
	// 失败心跳：写一次，LastError 非空，且非成功心跳。
	require.Equal(t, 1, ops.hbCalls(), "exactly one heartbeat on recompute failure")
	require.NotEmpty(t, ops.lastErrorVal(), "recompute failure must record a failure heartbeat (LastError)")
	require.False(t, ops.lastSuccessCalled(), "recompute failure must not be reported as success")
}

// TestQuotaKeyReconciler_InitialWaterLevelError_StopsAndRecordsError 是 §18.2 初始重算失败的
// 对照回归（与 R 行同形态）：函数内【初始】WaterLevel 错误分支同样是 log.Printf +
// recordHeartbeat(ctx, runAt, false, err, "") + return —— 两处处理保持一致，不发明第三种形态。
// 既有的 TestReconciler_ListModelsError_RecordsError 只覆盖枚举模型失败，本例补初始水位失败。
func TestQuotaKeyReconciler_InitialWaterLevelError_StopsAndRecordsError(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0
	mw.candidates = []int64{100, 101, 102}
	mw.waterErr = errors.New("initial water level failed")

	puller := newFakePuller()
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	require.Equal(t, 0, puller.callCount(), "initial water level error must stop before any pull")
	require.Equal(t, 1, ops.hbCalls(), "exactly one heartbeat on initial water level error")
	require.NotEmpty(t, ops.lastErrorVal(), "initial water level error must record a failure heartbeat")
	require.False(t, ops.lastSuccessCalled(), "initial water level error must not be reported as success")
}

// TestQuotaKeyReconciler_GeneralPullError_ContinuesToNextCandidate 验证 R 行不得扩大化：
// 一般 pull 错误（非哨兵 ErrQuotaKeyMountFailed、非 ErrUpstreamKeyBlocked）仍按既有分类处理——
// 记日志、继续下一个候选，不停轮。本例直接断言"拉号失败后重算正常时，后续候选仍被拉"且
// 未误走"拉后重算失败"停轮分支（该分支会写 LastError 并 return）。
func TestQuotaKeyReconciler_GeneralPullError_ContinuesToNextCandidate(t *testing.T) {
	mw := newFakeModelWater()
	mw.models = []string{"m1"}
	mw.level = 0
	mw.candidates = []int64{100, 101, 102}

	puller := newFakePuller()
	// 普通拉号错误（与哨兵无 errors.Is 关系、非 blocked）：既存语义——继续下一个候选。
	puller.errOnIDs[100] = errors.New("generic upstream error")
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	// 后续候选仍被拉（未被误停轮）。
	require.Equal(t, []int64{100, 101, 102}, puller.calledIDs(),
		"general pull error must not stop the round; later candidates must still be pulled")
	// 未误走"拉后重算失败"分支：该分支会写 LastError 并立即 return，此处不应出现失败心跳。
	require.Empty(t, ops.lastErrorVal(),
		"general pull error must not be misrouted to the recompute-failure stop-round path")
}
