//go:build unit

package service

// F3 403 content-policy 恢复链单测（派发单 C1-b② / 方案 §3.3 + §7.1）。
//
// 覆盖：
//   - ParseHTTP403PausedUntil：多格式解析、region-403 无 until 落入 not-found、
//     大小写不敏感、paused/until 同现约束、upstreamMsg + 响应体 message 合并。
//   - sweep 六分支：2xx 清除（CAS）、仍 403 带 until 续等、仍 403 无 until 冷却轮、
//     401/402/429 原子转交、瞬时 5xx/传输 受控快速重探达上限转既有 sweep、
//     未列举响应失败关闭。
//   - stale 收敛（易主删键）、CAS 冲突（在途新状态丢弃）。
//   - 存量 403 error 收编幂等（候选查询排除已持 F3 键账号）。
//   - 三振写点：达阈值进入 F3（带 until 解析 / 无 until 冷却轮）、repo 不支持回退 handleAuthError。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ---------- F3 探针控制器（override） ----------

type f3ProbeController struct {
	mu     sync.Mutex
	cls    f3ProbeResponseClass
	status int
	body   []byte
	err    error
	calls  int
}

func (c *f3ProbeController) set(cls f3ProbeResponseClass, status int, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cls = cls
	c.status = status
	c.body = body
	c.err = nil
}

func (c *f3ProbeController) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cls = f3ProbeTransient
	c.status = 0
	c.body = nil
	c.err = err
}

func (c *f3ProbeController) probe(_ context.Context, _ *Account) (f3ProbeResponseClass, int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.cls, c.status, c.body, c.err
}

// ---------- F3 计数清除 fake ----------

type f3CounterStub struct {
	mu       sync.Mutex
	resetIDs []int64
}

func (c *f3CounterStub) IncrementOpenAI403Count(_ context.Context, _ int64, _ int) (int64, error) {
	return 1, nil
}

func (c *f3CounterStub) ResetOpenAI403Count(_ context.Context, accountID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIDs = append(c.resetIDs, accountID)
	return nil
}

// ---------- 测试服务组装 ----------

func newF3TestService(repo *quotaLifecycleFakeRepo, alerts *fakeQuotaAlertStore, ctrl *f3ProbeController) *CNQuotaLifecycleService {
	svc := NewCNQuotaLifecycleService(repo, &recordingHTTPUpstream{}, &config.Config{}, alerts)
	svc.SetQuotaLifecycleClock(quotaLifecycleClock())
	svc.SetQuotaF3ProbeOverride(ctrl.probe)
	return svc
}

func seedF3Record(t *testing.T, repo *quotaLifecycleFakeRepo, a *Account, until time.Time) int64 {
	t.Helper()
	gen, err := repo.AllocHTTP403Generation(context.Background(), a.ID)
	require.NoError(t, err)
	require.NoError(t, repo.Mark403PausedWithRecovery(context.Background(), a.ID, until, gen, "seed", "Access forbidden (403): seed", nil))
	return gen
}

func newF3Account(id int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Extra:    map[string]any{},
	}
}

// ---------- 解析器 ----------

func TestParseHTTP403PausedUntil(t *testing.T) {
	t.Run("rfc3339", func(t *testing.T) {
		_, ok := ParseHTTP403PausedUntil("your account is paused until 2026-10-07T08:00:00Z", nil)
		require.True(t, ok)
	})
	t.Run("space_separated", func(t *testing.T) {
		_, ok := ParseHTTP403PausedUntil("paused until 2026-10-07 08:00:00", nil)
		require.True(t, ok)
	})
	t.Run("rfc1123", func(t *testing.T) {
		_, ok := ParseHTTP403PausedUntil("account paused until Mon, 07 Oct 2026 08:00:00 GMT", nil)
		require.True(t, ok)
	})
	t.Run("rfc1123z", func(t *testing.T) {
		_, ok := ParseHTTP403PausedUntil("paused until Mon, 07 Oct 2026 08:00:00 +0000", nil)
		require.True(t, ok)
	})
	t.Run("date_only_end_of_day", func(t *testing.T) {
		got, ok := ParseHTTP403PausedUntil("paused until 2026-10-07", nil)
		require.True(t, ok)
		require.Equal(t, time.Date(2026, 10, 7, 23, 59, 59, 0, time.UTC), got)
	})
	t.Run("region_403_no_until_not_found", func(t *testing.T) {
		_, ok := ParseHTTP403PausedUntil("API access from your region is not available", nil)
		require.False(t, ok, "known production region-403 sample must fall through to cooldown loop")
	})
	t.Run("case_insensitive", func(t *testing.T) {
		_, ok := ParseHTTP403PausedUntil("PAUSED UNTIL 2026-10-07T08:00:00Z", nil)
		require.True(t, ok)
	})
	t.Run("requires_both_paused_and_until", func(t *testing.T) {
		_, ok := ParseHTTP403PausedUntil("until 2026-10-07T08:00:00Z", nil)
		require.False(t, ok, "missing 'paused' keyword must not parse")
		_, ok = ParseHTTP403PausedUntil("account is paused but no date given", nil)
		require.False(t, ok, "missing 'until' keyword must not parse")
	})
	t.Run("merges_body_message", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{"message": "your account is paused until 2026-10-07T09:30:00Z"})
		_, ok := ParseHTTP403PausedUntil("Access forbidden (403):", body)
		require.True(t, ok)
	})
}

// ---------- sweep 六分支 ----------

func TestF3SweepRecoversOn2xx(t *testing.T) {
	account := newF3Account(401)
	repo := newQuotaLifecycleFakeRepo(account)
	seedF3Record(t, repo, account, quotaLifecycleBase)
	alerts := &fakeQuotaAlertStore{}
	counter := &f3CounterStub{}
	ctrl := &f3ProbeController{}
	ctrl.set(f3ProbeRecovered2xx, 200, nil)
	svc := newF3TestService(repo, alerts, ctrl)
	svc.SetQuota403CounterCache(counter)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))

	require.Equal(t, 1, ctrl.calls)
	require.Equal(t, StatusActive, account.Status, "2xx must clear error")
	require.Empty(t, account.ErrorMessage)
	require.False(t, repo.f3Records[401] != nil && repo.f3Records[401].Until != "", "F3 key must be cleared on recovery")
	_, hasKey := account.Extra[http403RecoveryExtraKey]
	require.False(t, hasKey)
	require.Equal(t, []int64{401}, counter.resetIDs, "2xx recovery must reset 3-strike counter")
}

func TestF3SweepStill403WithUntil(t *testing.T) {
	account := newF3Account(402)
	repo := newQuotaLifecycleFakeRepo(account)
	seedF3Record(t, repo, account, quotaLifecycleBase)
	alerts := &fakeQuotaAlertStore{}
	ctrl := &f3ProbeController{}
	body, _ := json.Marshal(map[string]any{"message": "paused until 2026-10-07T08:00:00Z"})
	ctrl.set(f3ProbeStill403, 403, body)
	svc := newF3TestService(repo, alerts, ctrl)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))

	require.Equal(t, 1, ctrl.calls)
	require.True(t, repo.f3Records[402] != nil, "F3 key must persist on still-403")
	got, err := time.Parse(time.RFC3339, repo.f3Records[402].Until)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC), got, "still-403 with until must refresh until from body")
	require.Equal(t, StatusError, account.Status)
}

func TestF3SweepStill403NoUntilCooldown(t *testing.T) {
	account := newF3Account(403)
	repo := newQuotaLifecycleFakeRepo(account)
	seedF3Record(t, repo, account, quotaLifecycleBase)
	alerts := &fakeQuotaAlertStore{}
	ctrl := &f3ProbeController{}
	ctrl.set(f3ProbeStill403, 403, []byte(`{"message":"API access from your region is not available"}`))
	svc := newF3TestService(repo, alerts, ctrl)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))

	require.True(t, repo.f3Records[403] != nil)
	got, err := time.Parse(time.RFC3339, repo.f3Records[403].Until)
	require.NoError(t, err)
	require.Equal(t, quotaLifecycleBase.Add(time.Duration(openAI403CooldownMinutesDefault)*time.Minute), got, "still-403 no-until must enter cooldown loop")
}

func TestF3SweepHandoff(t *testing.T) {
	cases := []struct {
		name   string
		cls    f3ProbeResponseClass
		status int
	}{
		{"401", f3ProbeAuth401, 401},
		{"402", f3ProbeQuota402, 402},
		{"429", f3ProbeRate429, 429},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := newF3Account(410)
			repo := newQuotaLifecycleFakeRepo(account)
			seedF3Record(t, repo, account, quotaLifecycleBase)
			alerts := &fakeQuotaAlertStore{}
			ctrl := &f3ProbeController{}
			ctrl.set(tc.cls, tc.status, nil)
			svc := newF3TestService(repo, alerts, ctrl)

			require.NoError(t, svc.RunRecoverySweep(context.Background()))

			require.False(t, repo.f3Records[410] != nil && repo.f3Records[410].Until != "", "handoff must remove F3 key")
			_, hasKey := account.Extra[http403RecoveryExtraKey]
			require.False(t, hasKey)
			// 401 认证失败链本就 status=error，维持现状正确（不改）。
			if tc.name == "401" {
				require.Equal(t, StatusError, account.Status, "401 handoff must keep error state (SetError semantics)")
				require.NotEmpty(t, account.ErrorMessage, "401 handoff must keep error_message text")
				require.Nil(t, account.TempUnschedulableUntil, "401 handoff must not set temp_unschedulable")
				return
			}
			// 402/429：复刻既有链暂停语义 = status active + 清 error_message 残留 +
			// 过渡 temp park（F3 拥有的 temp_unschedulable_until）。
			require.Equal(t, StatusActive, account.Status, "402/429 handoff must be active (replicate SetTempUnschedulable semantics)")
			require.Empty(t, account.ErrorMessage, "402/429 handoff must clear error_message residual")
			require.NotNil(t, account.TempUnschedulableUntil, "402/429 handoff must set F3-owned temp_unschedulable")
		})
	}
}

// TestF3SweepResumesAfterRestart 验证方案锁定："进程重启后 paused 账号仍被 sweep 探测"。
// 种子经写路径（repo 原语 Mark403PausedWithRecovery，等效 writeF3ThreeStrikeEntry /
// legacy init）进入 F3 键；随后新构造第二个 service 实例（进程内状态清零 = 重启语义，
// 同一 repo），跑 RunRecoverySweep，断言探针被发起且 F3 键被清——证明候选资格完全由
// 持久键派生、不依赖进程内状态（tracked / f3Repo 断言本就无态，但新实例未经任何写入铺垫）。
func TestF3SweepResumesAfterRestart(t *testing.T) {
	account := newF3Account(412)
	repo := newQuotaLifecycleFakeRepo(account)
	// 写路径写入 F3 键（不经 seedF3Record 直写 helper）：账号持 status=error + F3 恢复键
	// + until（到期=基点，故下一轮 sweep 即候选）。
	gen, err := repo.AllocHTTP403Generation(context.Background(), account.ID)
	require.NoError(t, err)
	require.NoError(t, repo.Mark403PausedWithRecovery(context.Background(), account.ID, quotaLifecycleBase, gen, "seed", "Access forbidden (403): seed", nil))

	alerts := &fakeQuotaAlertStore{}
	ctrl := &f3ProbeController{}
	ctrl.set(f3ProbeRecovered2xx, 200, nil)
	// 模拟进程重启：全新 service 实例（进程内 tracked 为空、未做任何预热写入），同一 repo。
	svc := newF3TestService(repo, alerts, ctrl)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.Equal(t, 1, ctrl.calls, "restarted service must still discover candidate purely from persisted F3 key (in-process state cleared)")
	require.False(t, repo.f3Records[412] != nil && repo.f3Records[412].Until != "", "recovery 2xx must clear F3 key on restarted service")
}

// TestF3SweepHandoffVsProbeRace 验证方案锁定："转交与新恢复探针并发 → 原子性保持"。
// 确定性构造：探针 override 回调内先执行一次 TransitionHTTP403RecoveryTo（模拟并发路径
// 完成转交、generation/revision 已消费、F3 键移除），再返回 2xx；sweep 随后的 Clear CAS
// 必须失配 → cas_conflict 事件、结果丢弃，终态 = 转交目标态、无二次写入、无 403 残留。
// 行为侧断言：fake repo 内状态与转交目标一致、account.Status 仍为转交目标（Clear 未重置
// 为 active）= Clear 返回 false 路径被走到。
func TestF3SweepHandoffVsProbeRace(t *testing.T) {
	account := newF3Account(413)
	repo := newQuotaLifecycleFakeRepo(account)
	gen, err := repo.AllocHTTP403Generation(context.Background(), account.ID)
	require.NoError(t, err)
	require.NoError(t, repo.Mark403PausedWithRecovery(context.Background(), account.ID, quotaLifecycleBase, gen, "seed", "Access forbidden (403): seed", nil))

	alerts := &fakeQuotaAlertStore{}

	var calls int
	// 探针 override：先模拟并发 401 转交（generation/revision 已消费）、再返回 2xx。
	probe := func(ctx context.Context, a *Account) (f3ProbeResponseClass, int, []byte, error) {
		calls++
		g := repo.f3Records[a.ID].Generation
		target := HTTP403TransitionTarget{
			Status:       StatusError,
			ErrorMessage: "concurrent 401 handoff (generation consumed)",
			Schedulable:  false,
		}
		_, _ = repo.TransitionHTTP403RecoveryTo(ctx, a.ID, g, target)
		return f3ProbeRecovered2xx, 200, nil, nil
	}
	svc := NewCNQuotaLifecycleService(repo, &recordingHTTPUpstream{}, &config.Config{}, alerts)
	svc.SetQuotaLifecycleClock(quotaLifecycleClock())
	svc.SetQuotaF3ProbeOverride(probe)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))

	// 终态 = 转交目标态（Clear CAS 失配，未二次写入、未清 error_message）。
	require.False(t, repo.f3Records[413] != nil && repo.f3Records[413].Until != "", "F3 key removed by handoff; Clear CAS must mismatch (no re-seed)")
	require.Equal(t, StatusError, account.Status, "handoff target state preserved (Clear did not reset to active ⇒ Clear returned false path taken)")
	require.Equal(t, "concurrent 401 handoff (generation consumed)", account.ErrorMessage, "no second write from CAS-mismatched Clear")
	require.Equal(t, 1, calls, "probe invoked exactly once")
}

func TestF3SweepTransientExhaustedKept(t *testing.T) {
	account := newF3Account(404)
	repo := newQuotaLifecycleFakeRepo(account)
	seedF3Record(t, repo, account, quotaLifecycleBase)
	alerts := &fakeQuotaAlertStore{}
	ctrl := &f3ProbeController{}
	ctrl.setErr(errors.New("transport timeout")) // 每次都瞬时 → 1 次 + 2 次快速重探 = 3
	svc := newF3TestService(repo, alerts, ctrl)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))

	require.Equal(t, f3FastRetryMax+1, ctrl.calls, "transient must do 1 probe + f3FastRetryMax fast retries")
	require.True(t, repo.f3Records[404] != nil, "exhausted transient must keep F3 record for next sweep")
	require.Equal(t, StatusError, account.Status)
}

func TestF3SweepUnclassifiedFailClosed(t *testing.T) {
	account := newF3Account(405)
	repo := newQuotaLifecycleFakeRepo(account)
	seedF3Record(t, repo, account, quotaLifecycleBase)
	alerts := &fakeQuotaAlertStore{}
	ctrl := &f3ProbeController{}
	ctrl.set(f3ProbeUnclassified, 418, nil)
	svc := newF3TestService(repo, alerts, ctrl)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))

	require.True(t, repo.f3Records[405] != nil, "unclassified must remain frozen for next sweep (fail-closed)")
	require.Equal(t, StatusError, account.Status)
	// 失败关闭须触发告警保活。
	require.NotEmpty(t, alerts.firingEvents(quotaAlertDimsFor(405)))
}

// ---------- stale 收敛 / CAS 冲突 ----------

func TestF3SweepStaleConvergenceRemovesKey(t *testing.T) {
	account := newF3Account(406)
	repo := newQuotaLifecycleFakeRepo(account)
	seedF3Record(t, repo, account, quotaLifecycleBase)
	// 模拟他链（如 401）已夺权：全局 sched_state_revision 推进，但 F3 键内 state_revision
	// 未同步；且错误文案已非 legacy 403 前缀（故 stale 删键后不会被存量初始化重新收编）。
	account.ErrorMessage = "token revoked (401): account authentication permanently revoked"
	account.Extra[schedStateRevisionExtraKey] = int64(999)
	alerts := &fakeQuotaAlertStore{}
	ctrl := &f3ProbeController{}
	svc := newF3TestService(repo, alerts, ctrl)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))

	require.False(t, repo.f3Records[406] != nil && repo.f3Records[406].Until != "", "stale F3 key must be removed atomically")
	require.Equal(t, StatusError, account.Status, "stale removal must NOT touch status/error (owned by other chain)")
}

func TestF3SweepCASConflictDiscardsResult(t *testing.T) {
	account := newF3Account(407)
	repo := newQuotaLifecycleFakeRepo(account)
	seedF3Record(t, repo, account, quotaLifecycleBase)
	// 在途新状态写入：推进全局 revision，使 CAS 三条件失配。
	repo.f3GlobalRev[407]++
	alerts := &fakeQuotaAlertStore{}
	ctrl := &f3ProbeController{}
	ctrl.set(f3ProbeRecovered2xx, 200, nil)
	svc := newF3TestService(repo, alerts, ctrl)
	counter := &f3CounterStub{}
	svc.SetQuota403CounterCache(counter)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))

	require.True(t, repo.f3Records[407] != nil, "CAS conflict must discard 2xx clear (keep F3 key)")
	require.Equal(t, StatusError, account.Status)
	require.Empty(t, counter.resetIDs, "CAS conflict must not reset counter")
}

// ---------- 存量收编幂等 ----------

func TestF3LegacyInitIdempotent(t *testing.T) {
	account := newF3Account(408)
	account.Status = StatusError
	account.ErrorMessage = "Access forbidden (403): legacy region blocked"
	repo := newQuotaLifecycleFakeRepo(account)
	alerts := &fakeQuotaAlertStore{}
	ctrl := &f3ProbeController{}
	svc := newF3TestService(repo, alerts, ctrl)

	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.True(t, repo.f3Records[408] != nil, "legacy 403 error must be enrolled into F3")
	firstGen := repo.f3GenCounter[408]

	// 第二轮：候选查询应排除已持 F3 键账号，不再重新收编。
	require.NoError(t, svc.RunRecoverySweep(context.Background()))
	require.Equal(t, firstGen, repo.f3GenCounter[408], "legacy init must be idempotent (no re-seed)")
}

// ---------- 三振写点（ratelimit） ----------

// f3RateLimitRepoStub 在既有 RateLimitService 测试 stub 之上补 F3 窄面实现，
// F3 内存态委托给 quotaLifecycleFakeRepo（复用已验证 CAS 语义），AccountRepository
// 接口由嵌入的 *rateLimitAccountRepoStub 提供。
type f3RateLimitRepoStub struct {
	*rateLimitAccountRepoStub
	f3 *quotaLifecycleFakeRepo
}

func newF3RateLimitRepoStub(account *Account) *f3RateLimitRepoStub {
	f3 := newQuotaLifecycleFakeRepo(account)
	return &f3RateLimitRepoStub{rateLimitAccountRepoStub: &rateLimitAccountRepoStub{}, f3: f3}
}

func (s *f3RateLimitRepoStub) AllocHTTP403Generation(ctx context.Context, id int64) (int64, error) {
	return s.f3.AllocHTTP403Generation(ctx, id)
}
func (s *f3RateLimitRepoStub) Mark403PausedWithRecovery(ctx context.Context, id int64, until time.Time, gen int64, reason, errorMsg string, tu *time.Time) error {
	return s.f3.Mark403PausedWithRecovery(ctx, id, until, gen, reason, errorMsg, tu)
}
func (s *f3RateLimitRepoStub) ClearHTTP403RecoveryIfOwned(ctx context.Context, id int64, gen int64) (bool, error) {
	return s.f3.ClearHTTP403RecoveryIfOwned(ctx, id, gen)
}
func (s *f3RateLimitRepoStub) TransitionHTTP403RecoveryTo(ctx context.Context, id int64, gen int64, target HTTP403TransitionTarget) (bool, error) {
	return s.f3.TransitionHTTP403RecoveryTo(ctx, id, gen, target)
}
func (s *f3RateLimitRepoStub) ListHTTP403RecoveryDueAccounts(ctx context.Context, now time.Time, limit int) ([]*Account, error) {
	return s.f3.ListHTTP403RecoveryDueAccounts(ctx, now, limit)
}
func (s *f3RateLimitRepoStub) UpdateHTTP403RecoveryUntil(ctx context.Context, id int64, gen int64, newUntil time.Time, reason string) (bool, error) {
	return s.f3.UpdateHTTP403RecoveryUntil(ctx, id, gen, newUntil, reason)
}
func (s *f3RateLimitRepoStub) RemoveHTTP403RecoveryRecord(ctx context.Context, id int64, gen int64) (bool, error) {
	return s.f3.RemoveHTTP403RecoveryRecord(ctx, id, gen)
}
func (s *f3RateLimitRepoStub) ListLegacyHTTP403ErrorAccounts(ctx context.Context, limit int) ([]*Account, error) {
	return s.f3.ListLegacyHTTP403ErrorAccounts(ctx, limit)
}

func TestRateLimitThreeStrikeEntersF3WithUntil(t *testing.T) {
	account := &Account{ID: 501, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := newF3RateLimitRepoStub(account)
	counter := &openAI403CounterCacheStub{counts: []int64{3}}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetOpenAI403CounterCache(counter)

	shouldDisable := service.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{},
		[]byte(`{"error":{"message":"your account is paused until 2026-10-07T08:00:00Z"}}`))

	require.True(t, shouldDisable)
	require.Equal(t, 0, repo.setErrorCalls, "F3 entry must NOT call legacy handleAuthError")
	require.True(t, repo.f3.f3Records[501] != nil, "three-strike must enter F3 recovery chain")
	got, err := time.Parse(time.RFC3339, repo.f3.f3Records[501].Until)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC), got)
}

func TestRateLimitThreeStrikeEntersF3NoUntilCooldown(t *testing.T) {
	account := &Account{ID: 502, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	repo := newF3RateLimitRepoStub(account)
	counter := &openAI403CounterCacheStub{counts: []int64{3}}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetOpenAI403CounterCache(counter)

	service.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{},
		[]byte(`{"error":{"message":"API access from your region is not available"}}`))

	require.True(t, repo.f3.f3Records[502] != nil, "region-403 (no until) must enter F3 cooldown loop")
	require.Equal(t, 0, repo.setErrorCalls)
}

func TestRateLimitThreeStrikeRepoUnsupportedFallsBack(t *testing.T) {
	// 既有的 rateLimitAccountRepoStub 未实现 http403RecoveryRepo → 断言失败静默降级，
	// 走 handleAuthError 永久禁用（不吞错）。
	repo := &rateLimitAccountRepoStub{}
	counter := &openAI403CounterCacheStub{counts: []int64{3}}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetOpenAI403CounterCache(counter)
	account := &Account{ID: 503, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	shouldDisable := service.HandleUpstreamError(context.Background(), account, http.StatusForbidden, http.Header{},
		[]byte(`{"error":{"message":"workspace forbidden by policy"}}`))

	require.True(t, shouldDisable)
	require.Equal(t, 1, repo.setErrorCalls, "unsupported repo must fall back to handleAuthError")
}
