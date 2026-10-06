//go:build unit

// E27 定向测试（保留部分）：#2 观测提交失败传播。
// 原第 #1 组（账号级候选单次分派收敛）针对 D2 TokenHarbor 第二相位，已随
// th-kira-quota-lifecycle 方案 §7 旧链退役删除。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
		rateLimit:      rl,
		accountRepo:    repo,
		frozenBounds:   make(map[string]int),
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
		rateLimit:      rl,
		accountRepo:    repo,
		frozenBounds:   make(map[string]int),
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
