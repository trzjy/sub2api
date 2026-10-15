// 探针恢复判定（PRC-02）定向单测：首次探测成功即解除停调 + 首探失败后短间隔补探。
//
// PRC-02 移除旧「连续两轮成功才清除」的二次确认（consecutive_successes 计数）：
// 恢复判据回到「一次真实探测成功」。旧语义下首探成功后必须再等一个完整轮次
// （默认 60s）才有第二次确认机会，第二轮若失败就只能等冷却到期（cooldown 分钟级），
// 恢复时长在 76~111s 与 300s 间抖动。新语义：
//   - 首探成功 → 立即解除停调（clearTempUnschedulable）；
//   - 首探失败 → 同一轮内按短间隔（默认 accountHealthProbeRetryInterval=5s）连补
//     最多 accountHealthProbeRetryBurst 次，受 settings.probe.max_attempts 双重约束，
//     避免探测风暴。
//
// 夹具复用 openai_apikey_health_breaker_test.go 的 probeRepoMock / probeRateLimitRepo /
// newProbeService / parkedBreakerAccount（该文件无构建标签），故本文件同样不加标签，
// 普通 `go test ./internal/service/` 即可覆盖。
package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// newProbeRetryHarness 构造一个探针 service + 内存 repo，并注入毫秒级补探节奏
//（生产默认 accountHealthProbeRetryInterval=5s，单测不能真等）。probe 序列由
// 调用方注入；repo.accounts 与 repo.candidates 指向同一个 *Account 对象，
// 使 bumpProbeAttempts 的回写对补探可见（与真实 DB 回读同语义）。
func newProbeRetryHarness(
	t *testing.T,
	maxAttempts int,
	probe func(ctx context.Context, acc *Account) (bool, error),
) (*AccountHealthRecoveryProbeService, *probeRepoMock, *probeRateLimitRepo) {
	t.Helper()
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	const acct int64 = 7
	acc := parkedBreakerAccount(acct, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{acct: acc},
		candidates: []*Account{acc},
	}
	p := newProbeService(t, true, maxAttempts, rl, repo)
	// 补探节奏注入：默认常量为 5s/3 次，单测改为毫秒级。
	p.retryInterval = 10 * time.Millisecond
	p.retryBurst = 3
	p.SetProbeOverride(probe)
	return p, repo, rlRepo
}

// 用例 1：首次（单次）探测成功即解除停调——不再需要第二轮确认，且不再回写
// consecutive_successes。
func TestProbeRecoveryFirstSuccessClearsImmediately(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	const acct int64 = 101
	acc := parkedBreakerAccount(acct, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{acct: acc},
		candidates: []*Account{acc},
	}
	originalReason := acc.TempUnschedulableReason

	sink, stop := captureStructuredLog(t)
	defer stop()

	p := newProbeService(t, true, 10, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })

	stillParked := p.probeAccount(context.Background(), acc, 10)

	require.False(t, stillParked, "首探成功即恢复，不应仍标记为待补探")
	require.Equal(t, 1, rlRepo.clearTempCalls, "首次探测成功必须立即解除停调（不再要求连续两轮成功）")
	require.Equal(t, 0, repo.setReasonCall, "不再回写 consecutive_successes 计数")
	require.Equal(t, originalReason, acc.TempUnschedulableReason, "成功恢复路径不得改写 reason")
	require.True(t, sink.ContainsMessageAtLevel("openai.apikey_health_probe_recovered", "info"),
		"首探成功必须产出 apikey_health_probe_recovered 事件")
}

// 用例 2：首探失败后按短间隔补探——同一轮 RunOnce 内即补探成功并解除停调，
// 不再空等一个完整轮次（默认 60s）或冷却到期。
func TestProbeRecoveryRetryBurstAfterFirstProbeFailure(t *testing.T) {
	var calls int32
	p, repo, rlRepo := newProbeRetryHarness(t, 10, func(_ context.Context, _ *Account) (bool, error) {
		// 首探失败，补探成功。
		return atomic.AddInt32(&calls, 1) >= 2, nil
	})

	start := time.Now()
	p.RunOnce(context.Background())
	elapsed := time.Since(start)

	require.Equal(t, 1, rlRepo.clearTempCalls, "首探失败后应在同一轮内补探成功并解除停调")
	require.Equal(t, int32(2), atomic.LoadInt32(&calls), "探测次数应为 1 次首探 + 1 次补探")
	require.GreaterOrEqual(t, elapsed, 10*time.Millisecond, "补探必须按注入间隔等待，不得零间隔空转")
	require.Less(t, elapsed, 30*time.Second, "补探须在秒级完成，不得等完整轮次或冷却到期")
	require.Equal(t, 1, repo.setReasonCall, "首探失败应簿记一次 ProbeAttempts")
}

// 用例 3：补探全部失败时不得解除停调，且补探次数受本轮上限（retryBurst）约束。
func TestProbeRecoveryRetryBurstRespectsBurstLimit(t *testing.T) {
	var calls int32
	p, _, rlRepo := newProbeRetryHarness(t, 100, func(_ context.Context, _ *Account) (bool, error) {
		atomic.AddInt32(&calls, 1)
		return false, nil // 始终失败
	})
	p.retryBurst = 2

	p.RunOnce(context.Background())

	require.Equal(t, int32(3), atomic.LoadInt32(&calls), "探测次数应为 1 次首探 + 2 次补探（本轮补探上限）")
	require.Zero(t, rlRepo.clearTempCalls, "补探全部失败不得解除停调（恢复仍须基于真实探测成功）")
}

// 用例 4：补探同样受 settings.probe.max_attempts 约束——达上限后 gave_up，不再补探。
func TestProbeRecoveryRetryBurstStopsAtMaxAttempts(t *testing.T) {
	var calls int32
	p, _, rlRepo := newProbeRetryHarness(t, 1, func(_ context.Context, _ *Account) (bool, error) {
		atomic.AddInt32(&calls, 1)
		return false, nil
	})
	p.retryBurst = 5

	sink, stop := captureStructuredLog(t)
	defer stop()

	p.RunOnce(context.Background())

	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "max_attempts=1 时首探即达上限，不得再补探")
	require.Zero(t, rlRepo.clearTempCalls, "达尝试上限不得解除停调")
	require.True(t, sink.ContainsMessageAtLevel("openai.apikey_health_probe_gave_up", "info"),
		"达到 max_attempts 应产出 apikey_health_probe_gave_up 事件")
}

// 用例 5a：探针硬超时应为 30s。
func TestProbeRequestHardTimeout(t *testing.T) {
	require.Equal(t, 30*time.Second, probeRequestHardTimeout, "探针硬超时应提升到 30s（覆盖上游首字节上界 26.7s）")
}

// 用例 5b：ComputeProbeUpperBound 随新超时值正确（n<=60 时 == 60s+30s+60s）。
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
