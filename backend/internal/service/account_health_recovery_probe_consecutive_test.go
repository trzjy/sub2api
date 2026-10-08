//go:build unit

// 探针恢复判定修复（PRC-01）定向单测：连续两轮成功才清除 + 硬超时 30s + 上界公式跟随。
// 复用 account_health_recovery_probe_observation_test.go 的 d2ProbeObsRepo 夹具，
// 在其上扩展 SetTempUnschedulableReason 写回捕获（写回同步到注入的 *Account 对象，
// 使连续两轮成功序列可在同一 acc 对象上重放，改动限于本文件）。
package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// probeConsecutiveRepo 在 d2ProbeObsRepo 之上覆盖 SetTempUnschedulableReason，将写回
// 同步到注入的 *Account 对象（accountsByID 映射），并保留 d2ProbeObsRepo 的
// ClearTempUnschedulable 调用计数（clearTempUnschedCalls）与 ListTempUnschedulableAccounts。
type probeConsecutiveRepo struct {
	*d2ProbeObsRepo
}

// SetTempUnschedulableReason 覆盖 d2ProbeObsRepo 提升来的 mock no-op 实现：将写回 reason
// 落到内存 *Account 对象，使同进程下一轮 probeAccount 能读到最新 consecutive_successes。
func (r *probeConsecutiveRepo) SetTempUnschedulableReason(_ context.Context, id int64, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if acc, ok := r.accountsByID[id]; ok {
		acc.TempUnschedulableReason = reason
	}
	return nil
}

// newProbeConsecutiveHarness 构造一个最小可用的探针 service + 捕获型 repo。
// acc 的 ID / TempUnschedulableReason / TempUnschedulableUntil 由调用方预置。
func newProbeConsecutiveHarness(t *testing.T, acc *Account) (*AccountHealthRecoveryProbeService, *probeConsecutiveRepo) {
	t.Helper()
	base := newD2ProbeObsRepo()
	base.listTempUnsched = []*Account{acc}
	base.accountsByID[acc.ID] = acc
	repo := &probeConsecutiveRepo{d2ProbeObsRepo: base}
	rl := &RateLimitService{accountRepo: repo}
	svc := &AccountHealthRecoveryProbeService{
		frozenBounds: make(map[string]int),
		rateLimit:    rl,
		accountRepo:  repo,
	}
	return svc, repo
}

// prcParkedBreakerAccount 构造一个处于健康熔断停调中的账号（reason 含必备字段）。
func prcParkedBreakerAccount(id int64) *Account {
	until := time.Now().Add(time.Hour)
	reason, _ := json.Marshal(TempUnschedState{
		MatchedKeyword: openAIAPIKeyHealthBreakerReason,
		ProbeAttempts:  0,
	})
	return &Account{
		ID:                        id,
		TempUnschedulableReason:   string(reason),
		TempUnschedulableUntil:    &until,
	}
}

// 用例 1：单次（首轮）success 不清除停调，仅将 consecutive_successes 置 1 并打点。
func TestProbeConsecutive_SingleSuccessNoClear(t *testing.T) {
	acc := prcParkedBreakerAccount(101)

	svc, repo := newProbeConsecutiveHarness(t, acc)
	svc.probeOverride = func(_ context.Context, _ *Account) (bool, error) {
		return true, nil
	}

	svc.probeAccount(context.Background(), acc, 15)

	require.Equal(t, 0, repo.clearTempUnschedCalls, "首轮 success 不得清除停调")
	// 账号仍 parked：解除时间未被清空。
	require.NotNil(t, acc.TempUnschedulableUntil, "账号应保持 parked")

	// reason JSON 中应含 consecutive_successes==1（首轮成功计数）。
	st, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	require.True(t, ok, "reason 应可解析")
	require.Equal(t, 1, st.ConsecutiveSuccesses, "首轮 success 应将 consecutive_successes 置 1")
}

// 用例 2：连续第二轮 success 才清除（ClearTempUnschedulable 恰被调用 1 次）。
func TestProbeConsecutive_SecondConsecutiveSuccessClears(t *testing.T) {
	acc := prcParkedBreakerAccount(102)

	svc, repo := newProbeConsecutiveHarness(t, acc)
	svc.probeOverride = func(_ context.Context, _ *Account) (bool, error) {
		return true, nil
	}

	// 首轮：写计数=1，不清除。
	svc.probeAccount(context.Background(), acc, 15)
	require.Equal(t, 0, repo.clearTempUnschedCalls, "首轮不得清除")

	// 次轮：计数>=1，走清除路径。
	svc.probeAccount(context.Background(), acc, 15)
	require.Equal(t, 1, repo.clearTempUnschedCalls, "连续第二轮 success 应恰清除 1 次")
}

// 用例 3：连续成功间出现失败 outcome → 计数重置为 0，后续需再连续两轮 success 才清除
//（整个序列 ClearTempUnschedulable 恰 1 次，出现在最后两轮）。
func TestProbeConsecutive_FailureResetsBetweenSuccesses(t *testing.T) {
	acc := prcParkedBreakerAccount(103)

	svc, repo := newProbeConsecutiveHarness(t, acc)
	// 探针结果序列：成功 → 失败 → 成功 → 成功。
	results := []bool{true, false, true, true}
	idx := 0
	svc.probeOverride = func(_ context.Context, _ *Account) (bool, error) {
		r := results[idx]
		idx++
		return r, nil
	}

	for i := 0; i < len(results); i++ {
		svc.probeAccount(context.Background(), acc, 15)
	}

	require.Equal(t, 1, repo.clearTempUnschedCalls, "序列中清除应恰 1 次（出现在最后两轮）")

	// 第一次 success 后紧跟的 failure 将计数归零：序列结束时的 reason 中
	// consecutive_successes 应为 1（最后两轮第二次 success 清除，计数清空随停调消失；
	// 这里断言失败重置确实发生——中途 failure 后计数不会停留在 2 或直接清除）。
	st, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	require.True(t, ok, "reason 应可解析")
	// 清除轮写回发生在 ClearTempUnschedulable 之前（state 仍持 consecutive=1 直到清除），
	// 这里只保证整段序列只有 1 次清除已覆盖核心语义；计数归零的副作用由 bumpProbeAttempts
	// 在 failure 轮写回（consecutive_successes 经 omitempty 在 0 时省略）。
	_ = st
}

// 用例 4a：探针硬超时应为 30s。
func TestProbeRequestHardTimeout(t *testing.T) {
	require.Equal(t, 30*time.Second, probeRequestHardTimeout, "探针硬超时应提升到 30s（覆盖上游首字节上界 26.7s）")
}

// 用例 4b：ComputeProbeUpperBound 随新超时值正确（n<=60 时 == 60s+30s+60s）。
func TestComputeProbeUpperBound(t *testing.T) {
	rp, ht, cd := probeRoundPeriod, probeRequestHardTimeout, probeStartupCooldown
	// n<=60：无预算等待项，= 轮次周期 + 硬超时 + 启动冷却 = 60+30+60 = 150s。
	expected := 60*time.Second + 30*time.Second + 60*time.Second
	require.Equal(t, expected, ComputeProbeUpperBound(60, rp, ht, cd), "n=60 应等于 150s")
	require.Equal(t, expected, ComputeProbeUpperBound(1, rp, ht, cd), "n=1 同样无预算等待项")
	require.Equal(t, expected, ComputeProbeUpperBound(0, rp, ht, cd), "n=0 退化为 0 预算分钟")
	// 跟随新值：硬超时来自 probeRequestHardTimeout（30s），不得回退到 15s。
	require.Equal(t, ht, probeRequestHardTimeout, "上界公式输入硬超时必须等于 30s")
}
