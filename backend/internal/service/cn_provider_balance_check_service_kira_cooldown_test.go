package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---- F7（2026-10-10 用户裁定）：Kira 探针失败冷却 ----
//
// 冷却键 kira_probe_cooldown_until 的唯一消费者 = 周期收集入口 shouldSkipKiraCollect
// 的调用方；lifecycle due 确认探针 / F3 恢复探针 / 手动探针（管理端查询）一律不得
// 消费此键（豁免硬边界）。以下测试覆盖：冷却门放行/拦截、失败写键、成功不写、
// 部分成功照常提交、冷却到期后年龄门正常接管，以及负例证明豁免未过宽。

// kiraCooldownTestAccount 构造带 kiraai.vn 凭据的 Kira 测试账号（基线 active）。
func kiraCooldownTestAccount(id int64) *Account {
	a := newKiraTestAccount(PlatformKimi)
	a.ID = id
	a.Status = StatusActive
	a.Schedulable = true
	return a
}

// kiraOKUpstream 返回用量端点 200 + 真实 summary 夹具（采集成功）。
func kiraOKUpstream() *kiraRecordingUpstream {
	return &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.URL.Path == "/api/user/usage" {
			return http.StatusOK, kiraUsageSummaryFixture
		}
		return http.StatusNotFound, `{}`
	}}
}

// kiraFailUpstream 返回用量端点 500（采集失败）。
func kiraFailUpstream() *kiraRecordingUpstream {
	return &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
		if r.URL.Path == "/api/user/usage" {
			return http.StatusInternalServerError, `{"error":"boom"}`
		}
		return http.StatusNotFound, `{}`
	}}
}

// 测试 1（正例）：冷却有效期内，周期收集入口必须跳过 Kira 账号（无热重试）。
func TestCNBalanceCheckKiraCooldownGateSkipsPeriodicCollect(t *testing.T) {
	now := time.Now().UTC()
	kira := kiraCooldownTestAccount(901)
	kira.Extra = map[string]any{
		kiraUsageSnapshotExtraKey:      map[string]any{"fetched_at": now.Add(-70 * time.Minute).Format(time.RFC3339)},
		kiraProbeCooldownUntilExtraKey: float64(now.Add(10 * time.Minute).Unix()),
	}
	repo := &cnRunOnceExtraRepo{
		byPlatform: map[string][]Account{PlatformKimi: {*kira}},
		byID:       map[int64]*Account{kira.ID: kira},
	}
	upstream := kiraOKUpstream()
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, nil)
	svc := &CNProviderBalanceCheckService{accountRepo: repo, balanceService: balanceSvc, cfg: thKiraGateConfig()}
	svc.runOnce()

	require.Empty(t, upstream.requests, "冷却有效期内 Kira 周期收集必须跳过（不得热重试）")
	// 直接断言门控命中。
	require.True(t, svc.shouldSkipKiraCollect(now, kira), "冷却未过期必须跳过本轮收集")
}

// 测试 2（冷却到期→年龄门接管）：冷却过期 + 快照陈旧 → 放行收集；冷却过期 + 快照新鲜
// → 仍被年龄门跳过（冷却不强制特权尝试）。
func TestCNBalanceCheckKiraCooldownExpiredFallsThroughToAgeGate(t *testing.T) {
	now := time.Now().UTC()

	// A：冷却过期 + 快照陈旧（70 分钟）→ 年龄门放行，正常收集。
	stale := kiraCooldownTestAccount(911)
	stale.Extra = map[string]any{
		kiraUsageSnapshotExtraKey:      map[string]any{"fetched_at": now.Add(-70 * time.Minute).Format(time.RFC3339)},
		kiraProbeCooldownUntilExtraKey: float64(now.Add(-10 * time.Minute).Unix()),
	}
	repoA := &cnRunOnceExtraRepo{byPlatform: map[string][]Account{PlatformKimi: {*stale}}, byID: map[int64]*Account{stale.ID: stale}}
	upA := kiraOKUpstream()
	svcA := &CNProviderBalanceCheckService{accountRepo: repoA, balanceService: NewCNProviderBalanceService(repoA, nil, upA, nil), cfg: thKiraGateConfig()}
	svcA.runOnce()
	require.Len(t, upA.requests, 1, "冷却过期 + 快照陈旧必须放行收集（年龄门正常接管）")

	// B：冷却过期 + 快照新鲜（5 分钟）→ 年龄门仍跳过（不强制特权重试）。
	fresh := kiraCooldownTestAccount(912)
	fresh.Extra = map[string]any{
		kiraUsageSnapshotExtraKey:      map[string]any{"fetched_at": now.Add(-5 * time.Minute).Format(time.RFC3339)},
		kiraProbeCooldownUntilExtraKey: float64(now.Add(-10 * time.Minute).Unix()),
	}
	repoB := &cnRunOnceExtraRepo{byPlatform: map[string][]Account{PlatformKimi: {*fresh}}, byID: map[int64]*Account{fresh.ID: fresh}}
	upB := kiraOKUpstream()
	svcB := &CNProviderBalanceCheckService{accountRepo: repoB, balanceService: NewCNProviderBalanceService(repoB, nil, upB, nil), cfg: thKiraGateConfig()}
	svcB.runOnce()
	require.Empty(t, upB.requests, "冷却过期 + 快照新鲜仍被年龄门跳过（不强制特权尝试）")
}

// 测试 3（失败写键）：Kira 快照采集或余额采集任一失败 → 写 kira_probe_cooldown_until。
func TestCNProviderBalanceCheckKiraCooldownWrittenOnFailure(t *testing.T) {
	account := kiraCooldownTestAccount(921)
	repo := &cnRunOnceExtraRepo{}
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		quotaService: NewCNProviderQuotaService(repo, nil, kiraFailUpstream(), nil),
		balanceService: NewCNProviderBalanceService(repo, nil, kiraFailUpstream(), nil),
		cfg:          thKiraGateConfig(),
	}
	svc.refreshKiraAccount(context.Background(), account)

	require.Len(t, repo.extraWrites, 1, "两探测均失败必须写一次冷却键")
	raw, ok := repo.extraWrites[0][kiraProbeCooldownUntilExtraKey]
	require.True(t, ok, "失败必须写 kira_probe_cooldown_until")
	// 键值为 now + kira_probe_interval_minutes（与成功间隔同值=60 分钟）。
	written, ok := kiraProbeCooldownUntilFromExtra(&Account{Extra: map[string]any{kiraProbeCooldownUntilExtraKey: raw}})
	require.True(t, ok, "写入的冷却键值必须可解析为合法截止")
	require.True(t, written.After(time.Now()), "冷却截止必须指向未来（now + 间隔）")
	// 与成功间隔同值：60 分钟（thKiraGateConfig），容差 2 分钟吸收测试耗时。
	delta := written.Sub(time.Now())
	require.InDelta(t, (60 * time.Minute).Seconds(), delta.Seconds(), 120,
		"冷却时长必须等于 kira_probe_interval_minutes（与成功间隔同值）")
}

// 测试 4（成功不写）：两探测均成功 → 不得写冷却键（成功侧快照照常落库）。
func TestCNProviderBalanceCheckKiraCooldownNotWrittenOnSuccess(t *testing.T) {
	account := kiraCooldownTestAccount(922)
	repo := &cnRunOnceExtraRepo{byID: map[int64]*Account{account.ID: account}}
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		quotaService: NewCNProviderQuotaService(repo, nil, kiraOKUpstream(), nil),
		balanceService: NewCNProviderBalanceService(repo, nil, kiraOKUpstream(), nil),
		cfg:          thKiraGateConfig(),
	}
	svc.refreshKiraAccount(context.Background(), account)

	for _, w := range repo.extraWrites {
		_, ok := w[kiraProbeCooldownUntilExtraKey]
		require.False(t, ok, "成功不得写 kira_probe_cooldown_until 冷却键")
	}
	// 成功侧快照照常落库。
	snapFound := false
	for _, w := range repo.extraWrites {
		if _, ok := w[kiraUsageSnapshotExtraKey]; ok {
			snapFound = true
		}
	}
	require.True(t, snapFound, "成功侧 kira_usage_snapshot 必须照常落库")
}

// 测试 5（部分成功照常提交）：快照采集成功 + 余额采集失败 → 成功侧快照照常落库，
// 失败侧写冷却键。
func TestCNProviderBalanceCheckKiraCooldownPartialSuccessCommitsSnapshot(t *testing.T) {
	account := kiraCooldownTestAccount(923)
	repo := &cnRunOnceExtraRepo{byID: map[int64]*Account{account.ID: account}}
	svc := &CNProviderBalanceCheckService{
		accountRepo:  repo,
		quotaService: NewCNProviderQuotaService(repo, nil, kiraOKUpstream(), nil),   // 快照采集成功
		balanceService: NewCNProviderBalanceService(repo, nil, kiraFailUpstream(), nil), // 余额采集失败
		cfg:          thKiraGateConfig(),
	}
	svc.refreshKiraAccount(context.Background(), account)

	snapFound, cooldownFound := false, false
	for _, w := range repo.extraWrites {
		if _, ok := w[kiraUsageSnapshotExtraKey]; ok {
			snapFound = true
		}
		if _, ok := w[kiraProbeCooldownUntilExtraKey]; ok {
			cooldownFound = true
		}
	}
	require.True(t, snapFound, "部分成功（快照成功）必须照常落库 kira_usage_snapshot")
	require.True(t, cooldownFound, "余额采集失败必须写冷却键")
}

// 测试 6（负例/豁免）：手动探针（管理端查询，走 QueryBalanceForAccount/QueryKiraBalance）
// 即便冷却键未过期也必须执行探测——不得消费冷却键。
func TestCNProviderBalanceCheckKiraCooldownManualProbeIgnores(t *testing.T) {
	now := time.Now().UTC()
	account := kiraCooldownTestAccount(931)
	account.Extra = map[string]any{kiraProbeCooldownUntilExtraKey: float64(now.Add(10 * time.Minute).Unix())}
	repo := &cnRunOnceExtraRepo{}
	upstream := kiraOKUpstream()
	balanceSvc := NewCNProviderBalanceService(repo, nil, upstream, nil)
	// 手动探针（管理端查询按钮）直接探余额，不读冷却键。
	_, err := balanceSvc.QueryBalanceForAccount(context.Background(), account)
	require.NoError(t, err)
	require.NotEmpty(t, upstream.requests, "手动探针不得被冷却键拦截（仍执行探测，证明豁免）")
}

// 测试 7（负例/豁免）：lifecycle due 确认探针（probeKiraExhaustion，sweep 调用）
// 即便冷却键未过期也必须执行探测——不得消费冷却键。
//
// 夹具前提（对齐 G1/G1-R2 新语义）：确认探针结论由 9 格判定表给出
// （evaluateKiraConfirmGrid，cn_quota_lifecycle_service.go 注释表），VND 三态是
// 判定轴之一——VND unknown（缺键/过期）一律 Uncertain，且探针会返回
// "grid concluded uncertain" 错误（不是被冷却键拦截，是判定表既定的无权威证据
// 结论）。故本夹具必须补齐新鲜 VND>0 付费快照（真实 Kira 号在余额采集成功侧
// 恒有此键），使探针按「VND fresh>0 ⇒ Recovered」格收敛；否则
// require.NoError 会被判定表的 Uncertain 结论带偏，掩盖"探针是否执行"这一
// 真正的断言目标。
func TestCNProviderBalanceCheckKiraCooldownLifecycleConfirmationProbeIgnores(t *testing.T) {
	now := time.Now().UTC()
	account := kiraCooldownTestAccount(932)
	account.Extra = map[string]any{
		kiraProbeCooldownUntilExtraKey: float64(now.Add(10 * time.Minute).Unix()),
		// 新鲜 VND>0：9 格表的 Recovered 依据（付费池可服务 ⇒ 账号可服务）。
		cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance): 1.0,
		cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated): now.Format(time.RFC3339),
	}
	repo := &cnRunOnceExtraRepo{}
	upstream := kiraOKUpstream()
	// lifecycle due 确认探针走 fetchKiraUsageWithReauth，不读冷却键。
	lc := &CNQuotaLifecycleService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}
	outcome, err := lc.probeKiraExhaustion(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, quotaProbeRecovered, outcome, "新鲜 VND>0 ⇒ 判定表 Recovered（证明探针确实跑完查表）")
	require.NotEmpty(t, upstream.requests, "lifecycle 确认探针不得被冷却键拦截（仍执行探测，证明豁免）")
}

// kiraCooldownF3RepoStub 最小 http403RecoveryRepo 假件。注：既有 F3 假件
//（quotaLifecycleFakeRepo / f3RateLimitRepoStub）全在 //go:build unit 域内，本文件与
// 同域 CN 收集测试一样不打标签（否则 go vet 默认编译缺符号），故本地自建窄面假件。
type kiraCooldownF3RepoStub struct {
	updateUntilCalls int
}

func (s *kiraCooldownF3RepoStub) AllocHTTP403Generation(context.Context, int64) (int64, error) {
	return 1, nil
}

func (s *kiraCooldownF3RepoStub) Mark403PausedWithRecovery(context.Context, int64, time.Time, int64, string, string, *time.Time) error {
	return nil
}

func (s *kiraCooldownF3RepoStub) ClearHTTP403RecoveryIfOwned(context.Context, int64, int64) (bool, error) {
	return true, nil
}

func (s *kiraCooldownF3RepoStub) TransitionHTTP403RecoveryTo(context.Context, int64, int64, HTTP403TransitionTarget) (bool, error) {
	return true, nil
}

func (s *kiraCooldownF3RepoStub) ListHTTP403RecoveryDueAccounts(context.Context, time.Time, int) ([]*Account, error) {
	return nil, nil
}

func (s *kiraCooldownF3RepoStub) UpdateHTTP403RecoveryUntil(context.Context, int64, int64, time.Time, string) (bool, error) {
	s.updateUntilCalls++
	return true, nil
}

func (s *kiraCooldownF3RepoStub) RemoveHTTP403RecoveryRecord(context.Context, int64, int64) (bool, error) {
	return true, nil
}

func (s *kiraCooldownF3RepoStub) ListLegacyHTTP403ErrorAccounts(context.Context, int) ([]*Account, error) {
	return nil, nil
}

// 测试 8（负例/豁免）：F3 恢复探针链（sweepF3RecoveryAccount → runHTTP403RecoveryProbe）
// 即便冷却键未过期也必须执行探测——不得消费冷却键。
func TestCNProviderBalanceCheckKiraCooldownF3RecoveryProbeIgnores(t *testing.T) {
	now := time.Now().UTC()
	account := kiraCooldownTestAccount(933)
	// 冷却键未过期（未来 10 分钟）。
	account.Extra = map[string]any{kiraProbeCooldownUntilExtraKey: float64(now.Add(10 * time.Minute).Unix())}
	// F3 恢复记录：state_revision 与账号全局 revision 一致（0），避免走 stale 提前 return
	// 分支掩盖"探针是否执行"这一断言目标。
	rec := &http403RecoveryRecord{
		Until:         now.Add(-time.Hour).UTC().Format(time.RFC3339),
		Generation:    1,
		Reason:        "test-f3-cooldown-exempt",
		Owner:         http403RecoveryOwnerF3,
		StateRevision: http403GlobalStateRevision(account.Extra),
	}
	require.Equal(t, http403GlobalStateRevision(account.Extra), rec.StateRevision,
		"前置：revision 必须匹配（否则走 stale 分支，测试目标被掩盖）")

	f3Repo := &kiraCooldownF3RepoStub{}
	probes := 0
	lc := &CNQuotaLifecycleService{
		accountRepo:  &cnRunOnceExtraRepo{},
		f3Repo:       f3Repo,
		httpUpstream: kiraOKUpstream(),
		cfg:          &config.Config{},
	}
	lc.SetQuotaF3ProbeOverride(func(_ context.Context, _ *Account) (f3ProbeResponseClass, int, []byte, error) {
		probes++
		return f3ProbeStill403, http.StatusForbidden, []byte(`{"message":"still paused"}`), nil
	})

	require.NoError(t, lc.sweepF3RecoveryAccount(context.Background(), account, rec, now))
	require.GreaterOrEqual(t, probes, 1,
		"F3 恢复探针不得被 Kira 冷却键拦截（冷却有效期内仍执行探测，证明豁免未过宽）")
}
