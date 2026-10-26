// 探针恢复判定定向单测（C1）：连续 2 次真实探测成功才解除停调 + 首探失败后短间隔补探
// + 探测次数无上限。
//
// 语义（C1）：
//   - 单次探测成功只把 TempUnschedState.ConsecutiveSuccesses 加一并回写，账号保持停调；
//   - 连续成功次数达到 probeRecoverySuccessThreshold（2）才走条件清除恢复；
//   - 任一失败/降级把连续成功计数归零；
//   - 补探没有轮数上限、也没有 settings.probe.max_attempts 上限：失败就一直按短间隔补探，
//     账号恢复后退出队列，循环天然收敛。
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
// （生产默认 accountHealthProbeRetryInterval=5s，单测不能真等）。probe 序列由
// 调用方注入；repo.accounts 与 repo.candidates 指向同一个 *Account 对象，
// 使状态回写对补探可见（与真实 DB 回读同语义）。
func newProbeRetryHarness(
	t *testing.T,
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
	p := newProbeService(t, true, rl, repo)
	// 补探节奏注入：默认常量为 5s，单测改为毫秒级。
	p.retryInterval = 10 * time.Millisecond
	p.SetProbeOverride(probe)
	return p, repo, rlRepo
}

// 用例 1：单次探测成功不足以恢复——只回写连续成功计数并保持停调；连续第二次成功才清除。
func TestProbeRecoveryNeedsConsecutiveSuccesses(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	const acct int64 = 101
	acc := parkedBreakerAccount(acct, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{acct: acc},
		candidates: []*Account{acc},
	}

	sink, stop := captureStructuredLog(t)
	defer stop()

	p := newProbeService(t, true, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })

	require.True(t, p.probeAccount(context.Background(), acc), "首次成功未达阈值，仍需补探")
	require.Zero(t, repo.clearTempIfReasonCalls, "未达连续成功阈值不得清除停调")
	require.Equal(t, 1, repo.bumpTempIfReasonCalls, "首次成功必须回写连续成功计数")
	require.True(t, sink.ContainsMessageAtLevel("openai.apikey_health_probe_success_pending", "info"),
		"首次成功必须产出 success_pending 事件（account_id/consecutive_successes/threshold）")

	st, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	require.True(t, ok)
	require.Equal(t, 1, st.ConsecutiveSuccesses)

	require.False(t, p.probeAccount(context.Background(), acc), "连续第二次成功应完成恢复")
	require.Equal(t, 1, repo.clearTempIfReasonCalls, "连续 2 次成功必须清除停调")
	require.True(t, sink.ContainsMessageAtLevel("openai.apikey_health_probe_recovered", "info"),
		"达标恢复必须产出 apikey_health_probe_recovered 事件")
}

// 用例 2：首探失败后按短间隔补探——sweep 异步发起补探（AC-C1），补探连续成功两次后
// 解除停调；sweep 自身不被补探阻塞。
func TestProbeRecoveryRetryBurstAfterFirstProbeFailure(t *testing.T) {
	var calls int32
	p, repo, _ := newProbeRetryHarness(t, func(_ context.Context, _ *Account) (bool, error) {
		// 首探失败，之后补探连续成功（第 2、3 次调用）。
		return atomic.AddInt32(&calls, 1) >= 2, nil
	})

	// sweep 必须立即返回（补探在独立 goroutine 异步执行，不阻塞本 sweep）。
	start := time.Now()
	p.RunOnce(context.Background())
	require.Less(t, time.Since(start), 50*time.Millisecond, "sweep 不得被异步补探阻塞")

	// 等待异步补探完成并完成恢复。
	p.wg.Wait()
	elapsed := time.Since(start)

	require.Equal(t, 1, repo.clearTempIfReasonCalls, "连续两次成功必须解除停调")
	require.Equal(t, int32(3), atomic.LoadInt32(&calls), "应为 1 次失败首探 + 2 次成功补探")
	require.GreaterOrEqual(t, elapsed, 10*time.Millisecond, "补探必须按注入间隔等待，不得零间隔空转")
	require.Less(t, elapsed, 30*time.Second, "补探须在秒级完成，不得等完整轮次或冷却到期")
}

// 用例 3：补探没有轮数上限——失败次数超过历史上限（50）仍继续补探，直到 ctx 取消。
func TestProbeRecoveryRetryHasNoRoundCeiling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	p, repo, _ := newProbeRetryHarness(t, func(_ context.Context, _ *Account) (bool, error) {
		// 累计 60 次探测后取消，验证补探越过历史 50 轮上限仍在进行。
		if atomic.AddInt32(&calls, 1) >= 60 {
			cancel()
		}
		return false, nil
	})

	p.RunOnce(ctx)
	p.wg.Wait()

	require.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(60),
		"补探不得在 50 轮后停止（C1 删除轮数上限，仅 ctx/Stop 可中断）")
	require.Zero(t, repo.clearTempIfReasonCalls, "全部失败不得解除停调（恢复仍须真实探测连续成功）")
}

// 用例 4：ProbeAttempts 不再构成放弃阈值——高计数账号仍继续探测并只做计数累计。
func TestProbeRecoveryIgnoresProbeAttempts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	p, repo, _ := newProbeRetryHarness(t, func(_ context.Context, _ *Account) (bool, error) {
		// 首探 + 1 次补探后取消 ctx，划定无上限补探的测试边界。
		if atomic.AddInt32(&calls, 1) >= 2 {
			cancel()
		}
		return false, nil
	})
	// 预置远超历史上限的 attempt 计数。
	high := parkedBreakerAccountWithState(7, 500, 0, "")
	repo.accounts[7] = high
	repo.candidates = []*Account{high}

	p.RunOnce(ctx)
	p.wg.Wait()

	require.Equal(t, int32(2), atomic.LoadInt32(&calls), "attempt 计数高也必须探测（无放弃阈值）")
	require.Zero(t, repo.clearTempIfReasonCalls)
	st, ok := parseHealthBreakerState(high.TempUnschedulableReason)
	require.True(t, ok)
	require.Equal(t, 502, st.ProbeAttempts, "ProbeAttempts 仅排障累计（500 + 首探 + 补探）")
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
