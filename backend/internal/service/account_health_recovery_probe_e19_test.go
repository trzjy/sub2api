//go:build unit

// E19 定向测试：恢复成功分支绑定提交确认（第四轮终审 #1）。
// 成功分支必须先写权威观测（ApplyModelRateLimitObservation）且返回 nil 才 clearCandidateBound；
// 提交失败沿既有探测错误路径处理（E7 同口径：按失败尝试计、不计成功、不清冻结上界）。
package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 提交失败：探测虽得 2xx，但权威观测写入失败——不得清除冻结上界、不得清除限流条目、
// 不得计为成功（按 Unclassified 失败尝试计）。
func TestE19SuccessCommitFailure_UnclassifiedAndBoundKept(t *testing.T) {
	now := time.Unix(20000, 0)
	repo := newD2ProbeObsRepo()
	rl := &RateLimitService{accountRepo: repo}
	const acct int64 = 91
	const scope = "deepseek:free"

	// 预置受限条目 + 已冻结上界（模拟该候选已进入调度）。
	repo.presetEntry(acct, scope, map[string]any{
		"rate_limit_reset_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
		"reason":              tokenHarborFreeTierReasonPrefix + ":x",
	})
	repo.commitErr = errors.New("simulated commit failure")

	svc := &AccountHealthRecoveryProbeService{
		nowFunc:      func() time.Time { return now },
		rateLimit:    rl,
		accountRepo:  repo,
		frozenBounds: make(map[string]int),
	}
	svc.freezeCandidateBound(acct, scope, 5)

	// 探测覆盖返回明确成功（2xx）。
	svc.probeTokenHarborOverride = func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
		return ProbeOutcomeSuccess, nil
	}

	svc.probeOneTokenHarbor(context.Background(), &Account{ID: acct}, tokenHarborCandidate{modelKey: scope})

	// 提交失败 → 冻结上界不得被清除（阈值不得被错误缩短）。
	if v, ok := svc.frozenCandidateBoundOK(acct, scope); !ok || v != 5 {
		t.Fatalf("提交失败不得清除冻结上界，got (%d, %v)", v, ok)
	}

	// 提交失败 → 限流条目不得被清除（状态未被权威写入）。
	_, ok := repo.entryFor(acct, scope)
	require.True(t, ok, "提交失败不得清除限流条目")

	// 提交失败 → 按失败尝试计，不计成功。
	stats, ok := svc.GetProbeObservationStats(acct)
	require.True(t, ok)
	require.Equal(t, int64(1), stats.Sends, "提交失败仍算一次实际发出")
	require.Zero(t, stats.Success, "提交失败不得计为 Success")
	require.Equal(t, int64(1), stats.Unclassified, "提交失败应按 Unclassified 计")
}

// 提交成功：先写权威观测返回 nil，才清除冻结上界；成功计数的时点绑定提交成功。
func TestE19SuccessCommitSucceeds_ClearsBound(t *testing.T) {
	now := time.Unix(21000, 0)
	repo := newD2ProbeObsRepo()
	rl := &RateLimitService{accountRepo: repo}
	const acct int64 = 92
	const scope = "deepseek:free"

	repo.presetEntry(acct, scope, map[string]any{
		"rate_limit_reset_at": now.Add(time.Hour).UTC().Format(time.RFC3339),
		"reason":              tokenHarborFreeTierReasonPrefix + ":x",
	})

	svc := &AccountHealthRecoveryProbeService{
		nowFunc:      func() time.Time { return now },
		rateLimit:    rl,
		accountRepo:  repo,
		frozenBounds: make(map[string]int),
	}
	svc.freezeCandidateBound(acct, scope, 5)

	svc.probeTokenHarborOverride = func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
		return ProbeOutcomeSuccess, nil
	}

	svc.probeOneTokenHarbor(context.Background(), &Account{ID: acct}, tokenHarborCandidate{modelKey: scope})

	// 提交成功 → 冻结上界清除。
	if _, ok := svc.frozenCandidateBoundOK(acct, scope); ok {
		t.Fatal("提交成功后应清除冻结上界")
	}
	// 提交成功 → 2xx 幂等清除限流条目。
	_, ok := repo.entryFor(acct, scope)
	require.False(t, ok, "2xx 成功应幂等清除限流条目")

	stats, ok := svc.GetProbeObservationStats(acct)
	require.True(t, ok)
	require.Equal(t, int64(1), stats.Sends)
	require.Equal(t, int64(1), stats.Success, "提交成功应计为 Success")
	require.Zero(t, stats.Unclassified)
}

// 账号级候选的提交失败同样不得清除账号级冻结上界、不计成功（避免维度只剩模型级被覆盖）。
func TestE19AccountLevelSuccessCommitFailure_KeepsAccountBound(t *testing.T) {
	now := time.Unix(22000, 0)
	repo := newD2ProbeObsRepo()
	rl := &RateLimitService{accountRepo: repo}
	const acct int64 = 93

	repo.commitErr = errors.New("simulated commit failure")

	svc := &AccountHealthRecoveryProbeService{
		nowFunc:      func() time.Time { return now },
		rateLimit:    rl,
		accountRepo:  repo,
		frozenBounds: make(map[string]int),
	}
	// 账号级冻结键为空 modelKey。
	svc.freezeCandidateBound(acct, "", 3)

	svc.probeTokenHarborOverride = func(_ context.Context, _ *Account, _ string) (ProbeOutcome, error) {
		return ProbeOutcomeSuccess, nil
	}

	svc.probeOneTokenHarbor(context.Background(), &Account{ID: acct}, tokenHarborCandidate{accountLevel: true})

	if v, ok := svc.frozenCandidateBoundOK(acct, ""); !ok || v != 3 {
		t.Fatalf("账号级提交失败不得清除冻结上界，got (%d, %v)", v, ok)
	}

	stats, ok := svc.GetProbeObservationStats(acct)
	require.True(t, ok)
	require.Zero(t, stats.Success, "账号级提交失败不得计为 Success")
	require.Equal(t, int64(1), stats.Unclassified)
}
