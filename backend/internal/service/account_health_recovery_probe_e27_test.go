//go:build unit

// E27 定向测试：第五轮终审 #1/#2 两处 P1 域内 must_fix 的收敛验证。
//   - #1：账号级候选在一次 RunOnce 内恰好 1 次上游探测 + 1 次观测写入（第一相 probeAccount
//        已探测；第二相 dispatchAccountTokenHarbor 经 probedThisScan 跳过，不再发请求/写观测/
//        消耗预算），冻结上界仍按含账号级的候选总数冻结；仅出现在第二相列表的账号行为不变。
//   - #2：观测提交失败透传；成功 outcome 下提交失败 → 失败关闭（保持 parked，不清除状态、
//        不清除账号级冻结上界、不计成功），并产出 apikey_health_probe_commit_failed 事件；
//        非 success outcome 下提交失败 → Warn 后继续既有失败簿记（状态无迁移）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ===================== #1：账号级候选单次分派收敛 =====================

// #1 核心：第二相 dispatchAccountTokenHarbor 在 hasAccountLevel=true 时，账号级候选标记
// probedThisScan，跳过预算获取/上游分派/观测写入，但仍冻结并按含账号级总数计入候选。
func TestE27AccountLevelCandidate_SkipsProbeWhenProbedThisScan(t *testing.T) {
	now := time.Unix(31000, 0)
	repo := newD2ProbeObsRepo()
	rl := &RateLimitService{accountRepo: repo}
	const acct int64 = 271

	// 预置一次账号级观测（模拟第一相已写）；用于断言第二相不再写第二次。
	repo.presetEntry(acct, tokenHarborAccountLevelProbeScope, map[string]any{
		entryObservedAtKey: now.Add(-time.Hour).UTC().Format(time.RFC3339),
	})

	var probeCalls, accountLevelProbeCalls int64
	svc := &AccountHealthRecoveryProbeService{
		nowFunc:         func() time.Time { return now },
		budget:          newProbeBudgetManager(func() time.Time { return now }),
		frozenBounds:    make(map[string]int),
		rateLimit:       rl,
		accountRepo:     repo,
		startTime:       now.Add(-61 * time.Second), // 越过冷启动 60s 门禁
		probeTokenHarborOverride: func(_ context.Context, _ *Account, modelKey string) (ProbeOutcome, error) {
			atomic.AddInt64(&probeCalls, 1)
			if modelKey == "" {
				atomic.AddInt64(&accountLevelProbeCalls, 1)
			}
			// 非成功（免费档 429）：不清除冻结上界，便于验证候选总数含账号级的冻结值(2)。
			return ProbeOutcomeFreeTier429, nil
		},
	}

	acc := &Account{ID: acct}
	scopes := []string{"deepseek:free"}
	// hasAccountLevel=true：账号同时是账号级候选（第一相已探测）。
	svc.dispatchAccountTokenHarbor(context.Background(), acc, scopes, true)

	// 仅模型级候选发 1 次上游探测；账号级候选（probedThisScan）跳过，不再发请求。
	require.Equal(t, int64(1), atomic.LoadInt64(&probeCalls), "仅模型级候选应发 1 次上游探测")
	require.Equal(t, int64(0), atomic.LoadInt64(&accountLevelProbeCalls), "账号级候选已本相探测，第二相应跳过不再发上游请求")

	// 账号级观测不得被第二相重复写入（仍是预置的旧 observed_at）。
	entry, ok := repo.entryFor(acct, tokenHarborAccountLevelProbeScope)
	require.True(t, ok)
	require.Equal(t, now.Add(-time.Hour).UTC().Format(time.RFC3339), entry[entryObservedAtKey], "账号级观测不得被第二相重复写入")
	require.Equal(t, 0, repo.commitScopeCount[tokenHarborAccountLevelProbeScope], "账号级候选第二相不得再写观测")

	// 冻结上界仍按含账号级的候选总数（candidateTotal=2）冻结（阈值公平性不变）。
	require.Equal(t, 2, svc.frozenCandidateBound(acct, "deepseek:free"), "冻结值应含账号级候选总数(2)")
	require.Equal(t, 2, svc.frozenCandidateBound(acct, ""), "账号级维度冻结值应=候选总数(2)")
	// 模型级候选的正常提交（1 次）仍发生。
	require.Equal(t, 1, repo.commitScopeCount["deepseek:free"], "模型级候选仍应正常提交 1 次观测")
}

// #1 回归：仅出现在第二相 TokenHarbor 列表（非熔断）的账号，hasAccountLevel=false，
// 不产生账号级候选、不写账号级观测，行为不变。
func TestE27AccountOnlyInTokenHarborList_Unchanged(t *testing.T) {
	now := time.Unix(33000, 0)
	repo := newD2ProbeObsRepo()
	rl := &RateLimitService{accountRepo: repo}
	const acct int64 = 276

	thAcc := &Account{
		ID: acct,
		Extra: map[string]any{
			modelRateLimitsKey: map[string]any{
				"deepseek:free": map[string]any{
					"reason":              tokenHarborFreeTierReasonPrefix + ":x",
					"rate_limit_reset_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
				},
			},
		},
	}
	repo.listTokenHarbor = []*Account{thAcc}

	svc := &AccountHealthRecoveryProbeService{
		nowFunc:         func() time.Time { return now },
		budget:          newProbeBudgetManager(func() time.Time { return now }),
		frozenBounds:    make(map[string]int),
		rateLimit:       rl,
		accountRepo:     repo,
		startTime:       now.Add(-61 * time.Second),
		probeTokenHarborOverride: func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
			return ProbeOutcomeSuccess, nil
		},
	}

	// 空 accountLevelAccountIDs → 仅第二相列表的账号，hasAccountLevel=false。
	svc.runTokenHarborProbePhase(context.Background(), map[int64]struct{}{})

	// 仅模型级候选 1 次提交；不产生账号级候选提交（行为不变）。
	require.Equal(t, 0, repo.commitScopeCount[tokenHarborAccountLevelProbeScope], "仅 TokenHarbor 列表的账号不应产生账号级候选提交")
	require.Equal(t, 1, repo.commitScopeCount["deepseek:free"], "模型级候选应 1 次提交")
}

// #1 端到端：RunOnce 内同一账号同时是熔断候选（第一相 probeAccount 探测）与 TokenHarbor
// 候选（第二相），账号级候选恰好 1 次上游探测 + 1 次观测写入。
func TestE27RunOnce_AccountLevelProbedOnceAcrossPhases(t *testing.T) {
	now := time.Unix(32000, 0)
	repo := newD2ProbeObsRepo()
	rl := &RateLimitService{accountRepo: repo}
	const acct int64 = 275

	breakerUntil := now.Add(time.Hour)
	breakerAcc := &Account{
		ID: acct,
		TempUnschedulableReason: func() string {
			b, _ := json.Marshal(TempUnschedState{MatchedKeyword: openAIAPIKeyHealthBreakerReason, ProbeAttempts: 0})
			return string(b)
		}(),
		TempUnschedulableUntil: &breakerUntil,
	}
	// 同账号也在 TokenHarbor 免费档模型级列表。
	thAcc := &Account{
		ID: acct,
		Extra: map[string]any{
			modelRateLimitsKey: map[string]any{
				"deepseek:free": map[string]any{
					"reason":              tokenHarborFreeTierReasonPrefix + ":x",
					"rate_limit_reset_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
				},
			},
		},
	}

	settings := OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true,
		Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: true, IntervalSeconds: 30, MaxAttempts: 3},
	}
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	ss := NewSettingService(&fakeSettingRepo{vals: map[string]string{
		SettingKeyOpenAIAPIKeyHealthBreakerSettings: string(raw),
	}}, nil)

	repo.listTempUnsched = []*Account{breakerAcc}
	repo.listTokenHarbor = []*Account{thAcc}
	repo.accountsByID[acct] = breakerAcc

	var breakerProbes, thProbes int64
	svc := &AccountHealthRecoveryProbeService{
		nowFunc:         func() time.Time { return now },
		startTime:       now.Add(-61 * time.Second),
		budget:          newProbeBudgetManager(func() time.Time { return now }),
		frozenBounds:    make(map[string]int),
		rateLimit:       rl,
		accountRepo:     repo,
		settingService:  ss,
		probeOverride: func(_ context.Context, _ *Account) (bool, error) {
			atomic.AddInt64(&breakerProbes, 1)
			return true, nil
		},
		probeTokenHarborOverride: func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
			atomic.AddInt64(&thProbes, 1)
			// 非成功（免费档 429）：不清除冻结上界，便于验证候选总数含账号级的冻结值(2)。
			return ProbeOutcomeFreeTier429, nil
		},
	}

	svc.RunOnce(context.Background())

	// 第一相 probeAccount 恰好 1 次上游探测。
	require.Equal(t, int64(1), atomic.LoadInt64(&breakerProbes), "第一相 probeAccount 应恰好 1 次上游探测")
	// 第二相仅模型级候选发 1 次上游探测；账号级候选（probedThisScan）应跳过。
	require.Equal(t, int64(1), atomic.LoadInt64(&thProbes), "第二相仅模型级候选 1 次上游探测，账号级候选应被跳过")

	// 账号级观测恰好写入 1 次（仅第一相 recordAccountLevelObservation；第二相跳过）。
	require.Equal(t, 1, repo.commitScopeCount[tokenHarborAccountLevelProbeScope], "账号级观测应恰好写入 1 次（第二相跳过）")

	// 冻结上界仍按含账号级的候选总数（candidateTotal=2）冻结。
	require.Equal(t, 2, svc.frozenCandidateBound(acct, "deepseek:free"), "冻结值应含账号级候选总数(2)")
	require.Equal(t, 2, svc.frozenCandidateBound(acct, ""), "账号级维度冻结值应=候选总数(2)")
}

// ===================== #2：观测提交失败传播 =====================

// #2 核心：探测成功但观测提交失败 → 失败关闭（E19 口径），账号保持 parked，
// 不清除 temp-unschedulable、不清除账号级冻结上界、不计成功，并产出 commit_failed 事件。
func TestE27ProbeAccount_CommitFailureKeepsParked(t *testing.T) {
	now := time.Unix(34000, 0)
	repo := newD2ProbeObsRepo()
	rl := &RateLimitService{accountRepo: repo}
	const acct int64 = 277
	repo.commitErr = errors.New("simulated commit failure")
	repo.accountsByID[acct] = &Account{ID: acct}

	until := now.Add(time.Hour)
	acc := &Account{
		ID: acct,
		TempUnschedulableReason: func() string {
			b, _ := json.Marshal(TempUnschedState{MatchedKeyword: openAIAPIKeyHealthBreakerReason, ProbeAttempts: 0})
			return string(b)
		}(),
		TempUnschedulableUntil: &until,
	}

	sink, stop := captureStructuredLog(t)
	defer stop()

	svc := &AccountHealthRecoveryProbeService{
		nowFunc:         func() time.Time { return now },
		rateLimit:       rl,
		accountRepo:     repo,
		frozenBounds:    make(map[string]int),
		probeOverride: func(_ context.Context, _ *Account) (bool, error) {
			return true, nil // 探测成功
		},
	}
	// 预置账号级冻结上界，断言提交失败时不被清除。
	svc.freezeCandidateBound(acct, "", 3)

	svc.probeAccount(context.Background(), acc, 3)

	// 恢复写入（ClearTempUnschedulable）未被调用 → 账号仍 parked。
	require.Equal(t, 0, repo.clearTempUnschedCalls, "提交失败不得清除 temp-unschedulable（保持 parked）")
	// 账号级冻结上界未被清除。
	if v, ok := svc.frozenCandidateBoundOK(acct, ""); !ok || v != 3 {
		t.Fatalf("提交失败不得清除账号级冻结上界，got (%d, %v)", v, ok)
	}

	// 产出 commit_failed 事件（与 E19 同事件名）。
	require.True(t, sink.ContainsMessageAtLevel("openai.apikey_health_probe_commit_failed", "warn"),
		"成功 outcome + 提交失败应产出 apikey_health_probe_commit_failed 事件")
}

// #2 补充：非 success outcome（探测失败）且观测提交失败 → Warn 后继续既有失败簿记，
// 状态无迁移（账号仍 parked，不清除恢复）。
func TestE27ProbeAccount_NonSuccessCommitFailureContinuesBookkeeping(t *testing.T) {
	now := time.Unix(35000, 0)
	repo := newD2ProbeObsRepo()
	rl := &RateLimitService{accountRepo: repo}
	const acct int64 = 278
	repo.commitErr = errors.New("simulated commit failure")
	repo.accountsByID[acct] = &Account{ID: acct}

	until := now.Add(time.Hour)
	acc := &Account{
		ID: acct,
		TempUnschedulableReason: func() string {
			b, _ := json.Marshal(TempUnschedState{MatchedKeyword: openAIAPIKeyHealthBreakerReason, ProbeAttempts: 0})
			return string(b)
		}(),
		TempUnschedulableUntil: &until,
	}

	sink, stop := captureStructuredLog(t)
	defer stop()

	svc := &AccountHealthRecoveryProbeService{
		nowFunc:         func() time.Time { return now },
		rateLimit:       rl,
		accountRepo:     repo,
		frozenBounds:    make(map[string]int),
		probeOverride: func(_ context.Context, _ *Account) (bool, error) {
			return false, nil // 探测失败（非降级）
		},
	}
	svc.freezeCandidateBound(acct, "", 3)

	svc.probeAccount(context.Background(), acc, 3)

	// 失败路径不得清除状态（恢复写入未调用、账号级冻结上界仍在）。
	require.Equal(t, 0, repo.clearTempUnschedCalls, "非成功路径不得清除 temp-unschedulable")
	if v, ok := svc.frozenCandidateBoundOK(acct, ""); !ok || v != 3 {
		t.Fatalf("非成功路径不得清除账号级冻结上界，got (%d, %v)", v, ok)
	}
	// 提交失败仍产出 commit_failed 事件（Warn 后继续失败簿记）。
	require.True(t, sink.ContainsMessageAtLevel("openai.apikey_health_probe_commit_failed", "warn"),
		"非 success outcome + 提交失败应产出 apikey_health_probe_commit_failed 事件（Warn 后仍继续失败簿记）")
}
