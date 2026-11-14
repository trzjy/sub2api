//go:build unit

package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// c1p1-H（F3 技术债）：码级恢复后清除已过恢复点的陈旧停调。
//
// 旧实现 ClearStopsUnderRecoveredKey 的 WHERE 带了 TempUnschedulableUntilGT(time.Now())，
// 导致码级额度恢复后、until 已经过期的账号（停到 19:00、现在 19:15）永远清不掉 reason，
// 后台一直显示旧停调文本。本用例验证：清除【不】挑 until，且按前缀精确清、不误清账号级停调。
// ---------------------------------------------------------------------------

// TestClearStopsUnderRecoveredKey_ClearsExpiredAndFutureStops：码级 reason、until 分别落在
// 过去与未来的两个账号，调用后 until/reason 都应被清空，返回行数 = 2。
func TestClearStopsUnderRecoveredKey_ClearsExpiredAndFutureStops(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.QuotaKeyID(qk.ID)).Exec(context.Background())
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(context.Background())
	})

	now := time.Now()
	// 两个码级停调账号：until 一个在未来、一个在过去。
	pastUntil := now.Add(-1 * time.Hour)
	futureUntil := now.Add(1 * time.Hour)
	pastReason := fmt.Sprintf("%s until %s", cursorQuotaKeyExhaustedReasonPrefix, pastUntil.Format("01-02 15:04"))
	futureReason := fmt.Sprintf("%s until %s", cursorQuotaKeyExhaustedReasonPrefix, futureUntil.Format("01-02 15:04"))

	mustCreateKeyLevelStoppedAccount(t, client, qk.ID, futureUntil, futureReason)
	mustCreateKeyLevelStoppedAccount(t, client, qk.ID, pastUntil, pastReason)

	svc := &QuotaKeyService{client: client}
	n, err := svc.ClearStopsUnderRecoveredKey(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), n, "both key-level stops (future + expired) must be cleared")

	accs, qerr := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, qerr)
	require.Len(t, accs, 2)
	for _, a := range accs {
		require.Nil(t, a.TempUnschedulableUntil, "until must be cleared for key-level stopped account")
		require.Empty(t, a.TempUnschedulableReason, "reason must be cleared for key-level stopped account")
	}
}

// TestClearStopsUnderRecoveredKey_KeepsNonKeyLevelStop：带非码级 reason（账号级
// cursor account blocked by upstream）且 until 未来的账号，调用后必须原样保留（不被误清）。
func TestClearStopsUnderRecoveredKey_KeepsNonKeyLevelStop(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() {
		_, _ = client.Account.Delete().Where(account.QuotaKeyID(qk.ID)).Exec(context.Background())
		_ = client.QuotaKey.DeleteOneID(qk.ID).Exec(context.Background())
	})

	// 账号级停调（不同原因串前缀、且 until 未来）。
	keepUntil := time.Now().Add(1 * time.Hour)
	keepReason := "cursor account blocked by upstream until 11-10 20:00"
	mustCreateKeyLevelStoppedAccount(t, client, qk.ID, keepUntil, keepReason)

	svc := &QuotaKeyService{client: client}
	n, err := svc.ClearStopsUnderRecoveredKey(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), n, "account-level stop must NOT be cleared (prefix mismatch)")

	accs, qerr := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, qerr)
	require.Len(t, accs, 1)
	require.NotNil(t, accs[0].TempUnschedulableUntil, "account-level until must be preserved")
	require.NotNil(t, accs[0].TempUnschedulableReason, "account-level reason must be preserved")
	require.Equal(t, keepReason, *accs[0].TempUnschedulableReason, "account-level reason must be preserved")
}

// mustCreateKeyLevelStoppedAccount 落库一个带指定码级/账号级停调原因与到期时刻的账号。
func mustCreateKeyLevelStoppedAccount(t *testing.T, client *ent.Client, quotaKeyID int64, until time.Time, reason string) *ent.Account {
	t.Helper()
	acc, err := client.Account.Create().
		SetName(fmt.Sprintf("c1p1h-%d-%d", quotaKeyID, time.Now().UnixNano())).
		SetPlatform("cursor").
		SetType("oauth").
		SetCredentials(map[string]any{"k": "v"}).
		SetQuotaKeyID(quotaKeyID).
		SetTempUnschedulableUntil(until).
		SetTempUnschedulableReason(reason).
		Save(context.Background())
	require.NoError(t, err)
	return acc
}

// ---------------------------------------------------------------------------
// KNCY-D（2026-11-14 用户裁定）：码级停调改为「5 分钟重判」+ 统一耗尽判定。
//
// 旧实现把停调终点写死在 08:00 / 19:00（对 kn_cy 的刷新点无依据），且判定用
// (*Total - *Used) < cursorQuotaKeyRemainingFloor（对 kn_cy 的 usd_cent 口径恒不触发）。
// 本组用例验证：耗尽判定改走 QuotaKeyUsageExhausted(snapshot)；停调终点改为 now+5min
// （rolling-5m），下一轮快照刷新后再判定放行/续停。
// ---------------------------------------------------------------------------

// recordingKeyLevelStopSink 包裹真实 *QuotaKeyService（落库写入 reason/until），
// 同时记录 StopAccountsUnderExhaustedKey / ClearStopsUnderRecoveredKey 的调用，
// 便于断言「是否新增停调」「是否执行恢复清除」以及停调终点。
type recordingKeyLevelStopSink struct {
	inner       quotaKeyAccountStopSink
	stopCalls   int
	stopUntil   time.Time
	stopQuotaID int64
	clearCalls  int
}

func (s *recordingKeyLevelStopSink) StopAccountsUnderExhaustedKey(ctx context.Context, quotaKeyID int64, until time.Time) (int64, error) {
	s.stopCalls++
	s.stopQuotaID = quotaKeyID
	s.stopUntil = until
	return s.inner.StopAccountsUnderExhaustedKey(ctx, quotaKeyID, until)
}

func (s *recordingKeyLevelStopSink) ClearStopsUnderRecoveredKey(ctx context.Context, quotaKeyID int64) (int64, error) {
	s.clearCalls++
	return s.inner.ClearStopsUnderRecoveredKey(ctx, quotaKeyID)
}

func (s *recordingKeyLevelStopSink) HasKeyLevelStop(ctx context.Context, quotaKeyID int64) (bool, error) {
	return s.inner.HasKeyLevelStop(ctx, quotaKeyID)
}

// cleanupQuotaKey 清掉测试码及其名下账号，避免污染共享测试库。
func cleanupQuotaKey(t *testing.T, client *ent.Client, id int64) {
	t.Helper()
	_, _ = client.Account.Delete().Where(account.QuotaKeyID(id)).Exec(context.Background())
	_ = client.QuotaKey.DeleteOneID(id).Exec(context.Background())
}

// buildUsdCentSnapshot 拼一份新鲜（10s 内）、观测成功、指定今日剩余的 usd_cent 快照。
//
// 同时带 total/used（非 nil）以满足 sweepQuotaKeyLevelStops 不变的「字段完整」前置
// （Total == nil || Used == nil 则不下结论）。usd_cent 口径的耗尽判定走 TodayRemainingCent，
// 与 total/used 取值无关。
func buildUsdCentSnapshot(todayRemainingCent int) []byte {
	checkedAt := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	return []byte(fmt.Sprintf(`{"unit":"usd_cent","total":1000,"used":0,"today_remaining_cent":%d,"checked_at":%q}`, todayRemainingCent, checkedAt))
}

// TestCursorQuotaKeyRecoveryUntil_Rolling5m：停调终点不再依赖 08:00/19:00，恒为 now+5min、
// band "rolling-5m"（覆盖原 08:00 前/08:00–19:00/19:00 后三个分支）。
func TestCursorQuotaKeyRecoveryUntil_Rolling5m(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 11, 14, 3, 17, 0, 0, time.UTC),  // 原 before-08:00 分支
		time.Date(2026, 11, 14, 11, 0, 0, 0, time.UTC),  // 原 before-19:00 分支
		time.Date(2026, 11, 14, 21, 30, 0, 0, time.UTC), // 原 after-19:00 分支
	} {
		until, band := cursorQuotaKeyRecoveryUntil(now)
		require.Equal(t, now.Add(quotaKeyReconcileInterval), until, "until must be now+5m at %s", now)
		require.Equal(t, "rolling-5m", band, "band must be rolling-5m at %s", now)
	}
}

// TestSweepQuotaKeyLevelStops_UsdCentExhausted_StopsFor5m：
// unit=usd_cent、today_remaining_cent=48（< 50 下限）→ 判定耗尽 → 新增停调，
// 停调终点 = now+5min，写入 reason 前缀 "cursor quota key exhausted"。
func TestSweepQuotaKeyLevelStops_UsdCentExhausted_StopsFor5m(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() { cleanupQuotaKey(t, client, qk.ID) })

	// 一个尚未被停的账号（until/reason 均为 nil），应被码级停调命中。
	mustCreateAccounts(t, client, qk.ID, 1)

	qk.UsageSnapshot = buildUsdCentSnapshot(48)
	repo := newFakeQuotaKeyRepo()
	repo.byID[qk.ID] = qk

	svc := &QuotaKeyService{client: client}
	sink := &recordingKeyLevelStopSink{inner: svc}
	r := &QuotaKeyReconciler{repo: repo}
	r.SetAccountStopSink(sink)

	r.sweepQuotaKeyLevelStops(ctx)

	require.Equal(t, 1, sink.stopCalls, "exhausted key must trigger a stop")
	require.Equal(t, 0, sink.clearCalls, "no recovery clear when still exhausted")
	require.Equal(t, qk.ID, sink.stopQuotaID)

	// 停调终点 = now + quotaKeyReconcileInterval（5min），按 Unix 秒比对以忽略时区。
	expectedUntil := time.Now().Add(quotaKeyReconcileInterval)
	require.InDelta(t, expectedUntil.Unix(), sink.stopUntil.Unix(), 3,
		"until must equal now+5min, got diff %ds", expectedUntil.Unix()-sink.stopUntil.Unix())

	// reason 前缀 + until 已落库。
	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	require.NotNil(t, accs[0].TempUnschedulableReason, "stop reason must be written")
	require.True(t, strings.HasPrefix(*accs[0].TempUnschedulableReason, cursorQuotaKeyExhaustedReasonPrefix),
		"reason must start with %q, got %q", cursorQuotaKeyExhaustedReasonPrefix, *accs[0].TempUnschedulableReason)
	require.NotNil(t, accs[0].TempUnschedulableUntil, "stop until must be written")
}

// TestSweepQuotaKeyLevelStops_UsdCentRecovered_ClearsStops：
// unit=usd_cent、today_remaining_cent=50（>= 50 下限）→ 判定未耗尽 → 不新增停调，
// 且对名下既有的码级停调执行恢复清除。
func TestSweepQuotaKeyLevelStops_UsdCentRecovered_ClearsStops(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() { cleanupQuotaKey(t, client, qk.ID) })

	// 先放一个被码级停调的账号（将来到期），用于验证"恢复清除"。
	futureUntil := time.Now().Add(1 * time.Hour)
	futureReason := fmt.Sprintf("%s until %s", cursorQuotaKeyExhaustedReasonPrefix, futureUntil.Format("01-02 15:04"))
	mustCreateKeyLevelStoppedAccount(t, client, qk.ID, futureUntil, futureReason)

	qk.UsageSnapshot = buildUsdCentSnapshot(50)
	repo := newFakeQuotaKeyRepo()
	repo.byID[qk.ID] = qk

	svc := &QuotaKeyService{client: client}
	sink := &recordingKeyLevelStopSink{inner: svc}
	r := &QuotaKeyReconciler{repo: repo}
	r.SetAccountStopSink(sink)

	r.sweepQuotaKeyLevelStops(ctx)

	require.Equal(t, 0, sink.stopCalls, "recovered key must NOT add a new stop")
	require.Equal(t, 1, sink.clearCalls, "recovered key must trigger recovery clear")

	// 码级停调被按前缀精确清除。
	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	require.Nil(t, accs[0].TempUnschedulableUntil, "key-level stop must be cleared")
	require.Empty(t, accs[0].TempUnschedulableReason, "key-level stop reason must be cleared")
}

// ---------------------------------------------------------------------------
// KNCY-J（外审 must_fix #5，2026-11-14）：刷新失败时续停，避免停调 5 分钟后自然过期。
//
// 旧实现：观测失败 / 字段不全 / 快照过期时直接 continue，既不续停也不放行；5 分钟一到
// TempUnschedulableUntil 自然过期，账号回池；额度仍耗尽就陷入「派单→403→再停 5 分钟」循环。
// 新实现：上述情形不再放行，若该码名下已有码级停调则续停 5 分钟（失败关闭）；没有码级停调
// 记录时保持原状（首次观测失败不凭空停号）。
// ---------------------------------------------------------------------------

// buildErrorSnapshot 拼一份观测失败快照：error_code 非空（解析后 ok=true）、total/used 非 nil、
// checked_at 新鲜。用于断言「观测失败 → 续停门禁」。
func buildErrorSnapshot(errorCode string) []byte {
	checkedAt := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	return []byte(fmt.Sprintf(`{"unit":"usd_cent","total":1000,"used":0,"today_remaining_cent":50,"error_code":%q,"checked_at":%q}`, errorCode, checkedAt))
}

// buildStaleSnapshot 拼一份字段完整但 checked_at 已超新鲜窗口（10 分钟前）的快照。
// 用于断言「快照过期 → 续停门禁」（不基于旧数据放号，但已有码级停调则续停）。
func buildStaleSnapshot(todayRemainingCent int) []byte {
	checkedAt := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	return []byte(fmt.Sprintf(`{"unit":"usd_cent","total":1000,"used":0,"today_remaining_cent":%d,"checked_at":%q}`, todayRemainingCent, checkedAt))
}

// TestSweepQuotaKeyLevelStops_FailedObservation_ExtendsExistingKeyLevelStop：
// 该码名下已有码级停调 + 本轮快照 error_code:"lookup_failed" → 调用续停，
// 停调终点 = now+5min，仓库 reason 仍带码级前缀（不重复计为首次停号）。
func TestSweepQuotaKeyLevelStops_FailedObservation_ExtendsExistingKeyLevelStop(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() { cleanupQuotaKey(t, client, qk.ID) })

	// 一个被码级停调的账号（将来到期），用于验证"失败续停"。
	futureUntil := time.Now().Add(1 * time.Hour)
	futureReason := fmt.Sprintf("%s until %s", cursorQuotaKeyExhaustedReasonPrefix, futureUntil.Format("01-02 15:04"))
	mustCreateKeyLevelStoppedAccount(t, client, qk.ID, futureUntil, futureReason)

	qk.UsageSnapshot = buildErrorSnapshot("lookup_failed")
	repo := newFakeQuotaKeyRepo()
	repo.byID[qk.ID] = qk

	svc := &QuotaKeyService{client: client}
	sink := &recordingKeyLevelStopSink{inner: svc}
	r := &QuotaKeyReconciler{repo: repo}
	r.SetAccountStopSink(sink)

	r.sweepQuotaKeyLevelStops(ctx)

	require.Equal(t, 1, sink.stopCalls, "failed observation with existing key-level stop must trigger extend")
	require.Equal(t, 0, sink.clearCalls, "no recovery clear on failed observation")
	require.Equal(t, qk.ID, sink.stopQuotaID)

	// 续停终点 = now + quotaKeyReconcileInterval（5min）。
	expectedUntil := time.Now().Add(quotaKeyReconcileInterval)
	require.InDelta(t, expectedUntil.Unix(), sink.stopUntil.Unix(), 3,
		"extend until must equal now+5min, got diff %ds", expectedUntil.Unix()-sink.stopUntil.Unix())

	// 仓库落库：仍带码级前缀，且被续停到新的 5 分钟点。
	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	require.NotNil(t, accs[0].TempUnschedulableReason, "stop reason must remain")
	require.True(t, strings.HasPrefix(*accs[0].TempUnschedulableReason, cursorQuotaKeyExhaustedReasonPrefix),
		"reason must keep %q prefix, got %q", cursorQuotaKeyExhaustedReasonPrefix, *accs[0].TempUnschedulableReason)
	require.NotNil(t, accs[0].TempUnschedulableUntil, "stop until must be written")
}

// TestSweepQuotaKeyLevelStops_FailedObservation_NoStop_WhenNoKeyLevelStop：
// 该码名下无停调记录 + 本轮观测失败 → 不调用停调（首次观测失败不凭空停号）。
func TestSweepQuotaKeyLevelStops_FailedObservation_NoStop_WhenNoKeyLevelStop(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() { cleanupQuotaKey(t, client, qk.ID) })

	// 一个尚未被任何理由停着的账号（until/reason 均为 nil）。
	mustCreateAccounts(t, client, qk.ID, 1)

	qk.UsageSnapshot = buildErrorSnapshot("lookup_failed")
	repo := newFakeQuotaKeyRepo()
	repo.byID[qk.ID] = qk

	svc := &QuotaKeyService{client: client}
	sink := &recordingKeyLevelStopSink{inner: svc}
	r := &QuotaKeyReconciler{repo: repo}
	r.SetAccountStopSink(sink)

	r.sweepQuotaKeyLevelStops(ctx)

	require.Equal(t, 0, sink.stopCalls, "failed observation with NO key-level stop must NOT add a stop")
	require.Equal(t, 0, sink.clearCalls, "no recovery clear on failed observation")

	// 仓库落库：账号仍未被停（until/reason 保持 nil）。
	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	require.Nil(t, accs[0].TempUnschedulableUntil, "account must NOT be stopped on first observation failure")
	require.Empty(t, accs[0].TempUnschedulableReason, "account must NOT gain a stop reason on first observation failure")
}

// TestSweepQuotaKeyLevelStops_StaleSnapshot_ExtendsExistingKeyLevelStop：
// 快照过期（checked_at 超过 5 分钟）+ 已有码级停调 → 续停 5 分钟（不基于旧数据放号，但续停已停的号）。
func TestSweepQuotaKeyLevelStops_StaleSnapshot_ExtendsExistingKeyLevelStop(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() { cleanupQuotaKey(t, client, qk.ID) })

	// 一个被码级停调的账号（将来到期），用于验证"过期续停"。
	futureUntil := time.Now().Add(1 * time.Hour)
	futureReason := fmt.Sprintf("%s until %s", cursorQuotaKeyExhaustedReasonPrefix, futureUntil.Format("01-02 15:04"))
	mustCreateKeyLevelStoppedAccount(t, client, qk.ID, futureUntil, futureReason)

	qk.UsageSnapshot = buildStaleSnapshot(50)
	repo := newFakeQuotaKeyRepo()
	repo.byID[qk.ID] = qk

	svc := &QuotaKeyService{client: client}
	sink := &recordingKeyLevelStopSink{inner: svc}
	r := &QuotaKeyReconciler{repo: repo}
	r.SetAccountStopSink(sink)

	r.sweepQuotaKeyLevelStops(ctx)

	require.Equal(t, 1, sink.stopCalls, "stale snapshot with existing key-level stop must trigger extend")
	require.Equal(t, 0, sink.clearCalls, "no recovery clear on stale snapshot")
	require.Equal(t, qk.ID, sink.stopQuotaID)

	// 续停终点 = now + quotaKeyReconcileInterval（5min）。
	expectedUntil := time.Now().Add(quotaKeyReconcileInterval)
	require.InDelta(t, expectedUntil.Unix(), sink.stopUntil.Unix(), 3,
		"extend until must equal now+5min, got diff %ds", expectedUntil.Unix()-sink.stopUntil.Unix())
}

// TestSweepQuotaKeyLevelStops_FreshSuccessExhausted_NotCountedAsExtend：
// 新鲜成功且 today<50 → 走原「耗尽停调」路径（直接 StopAccountsUnderExhaustedKey），
// 不重复计为续停（续停只在观测失败/过期且已有码级停调时触发，此处停调仅 1 次）。
func TestSweepQuotaKeyLevelStops_FreshSuccessExhausted_NotCountedAsExtend(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() { cleanupQuotaKey(t, client, qk.ID) })

	// 名下已有一个码级停调的账号（验证"新鲜成功 + 已停"走原路径、只停一次）。
	futureUntil := time.Now().Add(1 * time.Hour)
	futureReason := fmt.Sprintf("%s until %s", cursorQuotaKeyExhaustedReasonPrefix, futureUntil.Format("01-02 15:04"))
	mustCreateKeyLevelStoppedAccount(t, client, qk.ID, futureUntil, futureReason)

	qk.UsageSnapshot = buildUsdCentSnapshot(48) // 新鲜、成功、today_remaining<50 → 耗尽
	repo := newFakeQuotaKeyRepo()
	repo.byID[qk.ID] = qk

	svc := &QuotaKeyService{client: client}
	sink := &recordingKeyLevelStopSink{inner: svc}
	r := &QuotaKeyReconciler{repo: repo}
	r.SetAccountStopSink(sink)

	r.sweepQuotaKeyLevelStops(ctx)

	// 只停一次（直接耗尽路径），不重复计为续停；且不走恢复清除。
	require.Equal(t, 1, sink.stopCalls, "fresh success exhausted must stop exactly once (not double-counted as extend)")
	require.Equal(t, 0, sink.clearCalls, "no recovery clear when still exhausted")

	// 停调终点 = now + quotaKeyReconcileInterval（5min），reason 带码级前缀。
	expectedUntil := time.Now().Add(quotaKeyReconcileInterval)
	require.InDelta(t, expectedUntil.Unix(), sink.stopUntil.Unix(), 3,
		"until must equal now+5min, got diff %ds", expectedUntil.Unix()-sink.stopUntil.Unix())

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	require.NotNil(t, accs[0].TempUnschedulableReason, "stop reason must be written")
	require.True(t, strings.HasPrefix(*accs[0].TempUnschedulableReason, cursorQuotaKeyExhaustedReasonPrefix),
		"reason must start with %q, got %q", cursorQuotaKeyExhaustedReasonPrefix, *accs[0].TempUnschedulableReason)
}
