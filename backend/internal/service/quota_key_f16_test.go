//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// R13-2（外审 R13 must_fix #2）：自动链路未知失败如实计入心跳。
// 旧行为：pull 自动链路对未知上游错误 casSetExhausted 后 return nil → reconciler.pullKey
// 把 nil 当成功 pulled.Add(1) → 心跳记 "reconciled N quota keys"，失败被记成成功。
// 新行为：自动链路透传含 ErrUpstreamKeyBlocked 哨兵的包裹错误（落库 exhausted=true 不变）；
// reconciler.pullKey 识别该哨兵 → 记失败级日志、不加 pulled、返回 false（不停轮）。
// ---------------------------------------------------------------------------

// TestCURF16_PullAccountsForModel_BlockedSentinel：自动链路遇未知 code=1 → 返回
// ErrUpstreamKeyBlocked 包裹错误（errors.Is 真）、DB exhausted=true 落位不变（落库行为零变化）。
func TestCURF16_PullAccountsForModel_BlockedSentinel(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		err: NewUpstreamUnknownError("upstream code=1 unknown result"),
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	// 自动链路（manual=false）：返回含 ErrUpstreamKeyBlocked 哨兵的包裹错误（R13-2）。
	err := svc.PullAccountsForModel(ctx, qk.ID, "cursor-f16-unknown")
	require.Error(t, err, "automatic chain must surface the blocked sentinel (not silent success)")
	require.True(t, errors.Is(err, ErrUpstreamKeyBlocked),
		"automatic unknown path must wrap ErrUpstreamKeyBlocked; got=%v", err)
	require.Contains(t, err.Error(), "upstream code=1 unknown result",
		"underlying reason summary must be carried in the wrapped error")

	// DB 落位不变：casSetExhausted 仍置 exhausted=true（与旧行为一致），且保留原文。
	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.True(t, got.Exhausted, "automatic unknown path must still drive exhausted=true (casSetExhausted)")
	require.NotNil(t, got.LastRefreshError)
	require.Contains(t, *got.LastRefreshError, "upstream code=1 unknown result",
		"last_refresh_error must preserve the original msg text")
}

// TestCURF16_RefreshAccounts_ManualStillReturnsUpstreamPullError：手动链路回归——
// 仍返回 *UpstreamPullError 原文（F4 语义不变），不得被 R13-2 的哨兵改动影响。
func TestCURF16_RefreshAccounts_ManualStillReturnsUpstreamPullError(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		err: NewUpstreamUnknownError("manual upstream unknown text"),
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.RefreshAccounts(ctx, qk.ID)
	require.Error(t, err, "manual refresh must NOT report success on unknown upstream result")
	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe), "manual refresh must still return *UpstreamPullError")
	require.Equal(t, UpstreamUnknown, upe.Kind)
	require.Contains(t, upe.Message(), "manual upstream unknown text",
		"manual refresh message must still carry the original text verbatim")
	// 手动链路不得携带 R13-2 的 Blocked 哨兵。
	require.False(t, errors.Is(err, ErrUpstreamKeyBlocked),
		"manual chain must NOT be wrapped in ErrUpstreamKeyBlocked")
}

// TestCURF16_Reconciler_BlockedSentinel_NotCountedAndContinues：reconciler 消费端——
// 替身 puller 返回 ErrUpstreamKeyBlocked → 该 key 失败记入日志、pulled 不增、返回 false
// （不停轮，本批其余候选继续被尝试）。
func TestCURF16_Reconciler_BlockedSentinel_NotCountedAndContinues(t *testing.T) {
	readLog, restoreLog := captureLog(t)
	defer restoreLog()

	mw := newFakeModelWater()
	mw.models = []string{"m1", "m2"}
	mw.level = 0 // 池级总量 0（总量口径：不按模型分别计数）
	// 池级候选（总量口径下候选集不再按模型分桶），全部返回 Blocked 哨兵。
	mw.candidates = []int64{100, 101, 200, 201}

	puller := newFakePuller()
	// 替身 puller 返回与真实 pull 自动链路同形的包裹错误（含 ErrUpstreamKeyBlocked 哨兵），
	// 以便 reconciler 以 errors.Is 识别。
	blocked := fmt.Errorf("%w: key rejected", ErrUpstreamKeyBlocked)
	puller.errOnIDs[100] = blocked
	puller.errOnIDs[101] = blocked
	puller.errOnIDs[200] = blocked
	puller.errOnIDs[201] = blocked
	ops := newReconOpsRepo()
	r := newTestReconciler(puller, ops, mw)

	r.runOnce()

	// 不停轮：所有候选（含另一模型 m2）都被尝试，而非命中后中断。
	ids := puller.calledIDs()
	require.ElementsMatch(t, []int64{100, 101, 200, 201}, ids,
		"blocked sentinel must NOT stop the round; all candidates must be attempted")

	// 失败级日志：每个 key 都记 "upstream pull blocked"，且含 key id、不落凭据。
	logs := readLog()
	for _, k := range []int64{100, 101, 200, 201} {
		require.Contains(t, logs, fmt.Sprintf("quota key %d upstream pull blocked", k),
			"failure-level log must include the quota key id")
	}
	require.False(t, strings.Contains(logs, "credential") || strings.Contains(logs, "secret"),
		"failure log must not leak credentials")

	// pulled 不增（真实成功数为 0）。R15-1：blocked>0 的轮次心跳记【失败】——
	// LastError 非空、LastSuccessAt 不更新，运维侧可直接看到本轮失败而非"成功空跑"。
	require.Equal(t, 1, ops.hbCalls(), "exactly one heartbeat per round")
	require.False(t, ops.lastSuccessCalled(), "blocked round must NOT be recorded as success heartbeat")
	require.NotNil(t, ops.lastErrorVal(), "blocked round must record LastError")
	require.Contains(t, ops.lastErrorVal(), "upstream pull(s) blocked",
		"heartbeat error must reflect blocked pull count")
}
