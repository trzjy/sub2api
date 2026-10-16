//go:build unit

// AC-C1 探测节奏对齐测试：三处门禁（clamp 下限 / Start 默认 / Start 内 >=15 独立判断）
// + 补探上限 3→50 + 补探异步化（r6 阻塞1）+ 账号级 in-flight 去重。
//
// 三道门禁须同步改，只改其一仍跑 60s（外审 round-2 实证）：
//   1. setting_openai_apikey_health.go 的 openAIAPIKeyHealthProbeIntervalMin 30→15
//      （经 normalize 把 IntervalSeconds clamp 到 [15,600]）；
//   2. account_health_recovery_probe_service.go 的 probeIntervalFromSettings 默认 15s；
//   3. 同函数内 IntervalSeconds >= 15 的独立判断（旧 >= 30）。
// 本文件所有用例均以 TestProbeInterval 前缀命名，确保落入白名单 -run 过滤。

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// --- 探测间隔三处门禁对齐 ---

// settings 配 15 → interval=15s：验证 Start 内 >=15 新门在边界值放行（旧 >=30 会拒绝 15）。
func TestProbeIntervalFromSettings_Configured15(t *testing.T) {
	ss, _ := newSettingService(t, &OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true,
		Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: true, IntervalSeconds: 15, MaxAttempts: 100},
	})
	got, err := ss.GetOpenAIAPIKeyHealthBreakerSettings(context.Background())
	require.NoError(t, err)
	p := &AccountHealthRecoveryProbeService{}
	require.Equal(t, 15*time.Second, p.probeIntervalFromSettings(got, nil),
		"settings 配 15 应被新门 >=15 放行，interval=15s")
}

// settings 配 10（低于下限）→ 被 clamp 到 15，interval 仍为 15s。
func TestProbeIntervalFromSettings_BelowMinClampedTo15(t *testing.T) {
	ss, _ := newSettingService(t, &OpenAIAPIKeyHealthBreakerSettings{
		Enabled: true,
		Probe:   &OpenAIAPIKeyHealthBreakerProbeSettings{Enabled: true, IntervalSeconds: 10, MaxAttempts: 100},
	})
	got, err := ss.GetOpenAIAPIKeyHealthBreakerSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 15, got.Probe.IntervalSeconds, "低于下限 10 应被 normalize clamp 到 15")
	p := &AccountHealthRecoveryProbeService{}
	require.Equal(t, 15*time.Second, p.probeIntervalFromSettings(got, nil),
		"clamp 后 interval 仍须为 15s")
}

// settings 未配置 Probe 段 → 默认 15s（不再空等旧 60s）。
// 注：normalize 会补全 Probe 段并把 IntervalSeconds clamp 到下限 15，故读取结果
// Probe 非 nil 但 IntervalSeconds 为下限 15；探测间隔仍回落默认 15s。
func TestProbeIntervalFromSettings_NoProbeDefaultsTo15(t *testing.T) {
	ss, _ := newSettingService(t, &OpenAIAPIKeyHealthBreakerSettings{Enabled: true})
	got, err := ss.GetOpenAIAPIKeyHealthBreakerSettings(context.Background())
	require.NoError(t, err)
	require.NotNil(t, got.Probe, "normalize 会补全 Probe 段")
	require.Equal(t, 15, got.Probe.IntervalSeconds, "未配置 Probe 时补全为下限 15")
	p := &AccountHealthRecoveryProbeService{}
	require.Equal(t, 15*time.Second, p.probeIntervalFromSettings(got, nil),
		"Probe 未显式配置仍须回落默认 15s")
}

// 设置读取失败 → 回落默认 15s（与旧 60s 默认语义解耦）。
func TestProbeIntervalFromSettings_ReadErrorDefaultsTo15(t *testing.T) {
	p := &AccountHealthRecoveryProbeService{}
	require.Equal(t, 15*time.Second, p.probeIntervalFromSettings(nil, errors.New("boom")),
		"读取失败应回落默认 15s")
}

// --- 补探上限 3→50 ---

// 常量本身须为 50（AC-C1：旧 3）。
func TestProbeIntervalRetryBurstDefaultIs50(t *testing.T) {
	require.Equal(t, 50, accountHealthProbeRetryBurst, "AC-C1：补探上限 3→50")
}

// 默认补探上限 50：单账号首探失败后续补 50 轮（受 max_attempts=100 双重约束未触发），
// 共 1 首探 + 50 补探 = 51 次探测 / 51 次 attempt 簿记。
func TestProbeIntervalRetryBurstRunsUpTo50(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(7, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: cand},
		candidates: []*Account{cand},
	}
	p := newProbeService(t, true, 100, rl, repo) // 不覆盖 retryBurst → 默认 50
	p.retryInterval = time.Millisecond
	var calls int32
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		atomic.AddInt32(&calls, 1)
		return false, nil
	})

	p.RunOnce(context.Background())
	p.wg.Wait() // 等待异步补探完成（AC-C1）

	require.Equal(t, int32(51), atomic.LoadInt32(&calls), "默认补探上限 50：1 首探 + 50 补探")
	require.Equal(t, 51, repo.bumpTempIfReasonCalls, "每轮补探各簿记一次 attempt")
}

// --- 补探异步化（r6 阻塞1 验收）：补探占用 >15s 时 sweep 不被阻塞 ---

// 补探（模拟）阻塞超过 15s 时，sweep（RunOnce）必须立即返回、不被补探 goroutine 阻塞；
// 补探在独立 goroutine 中异步执行（纳入 wg，Stop 等待语义不变）。
func TestProbeIntervalRetryBurstAsyncDoesNotBlockSweep(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(7, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: cand},
		candidates: []*Account{cand},
	}
	p := newProbeService(t, true, 100, rl, repo)
	p.retryInterval = time.Millisecond
	var calls int32
	// 首探（第 1 次调用）快速失败；补探（后续调用）阻塞直到 ctx 取消，模拟 >15s 的补探占用。
	p.SetProbeOverride(func(ctx context.Context, _ *Account) (bool, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return false, nil
		}
		<-ctx.Done()
		return false, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	p.RunOnce(ctx)
	elapsed := time.Since(start)
	require.Less(t, elapsed, 500*time.Millisecond,
		"sweep 不得被异步补探阻塞（r6 阻塞1 验收：补探占用 >15s 时 sweep 仍按 15s 节奏触发）")

	// 确认补探确实在异步执行（已进入补探 goroutine）。
	require.Eventually(t, func() bool { return atomic.LoadInt32(&calls) >= 2 },
		2*time.Second, 10*time.Millisecond, "补探应在独立 goroutine 异步执行")

	cancel() // 释放被阻塞的补探
	p.wg.Wait()
}

// --- 账号级 in-flight 去重（防止异步化后同账号补探重叠） ---

// 去重原语：同账号在途补探时不得重复入队；清除后可重新入队。
func TestProbeIntervalRetryInFlightDedupPrimitive(t *testing.T) {
	p := &AccountHealthRecoveryProbeService{}
	require.True(t, p.markRetryInFlight(7), "首次入队应成功")
	require.False(t, p.markRetryInFlight(7), "同账号在途补探时不得重复入队")
	p.clearRetryInFlight(7)
	require.True(t, p.markRetryInFlight(7), "清除 in-flight 后该账号可重新入队")
}

// 集成：某账号已有补探在途时，新一轮 sweep 跳过该账号的本轮补探（不重复入队），
// 因此该账号的 in-flight 标记不会被新 sweep 发起的补探清除。
func TestProbeIntervalRetryInFlightSkipsEnqueue(t *testing.T) {
	rlRepo := &probeRateLimitRepo{}
	rl := NewRateLimitService(rlRepo, nil, &config.Config{}, nil, nil)
	cand := parkedBreakerAccount(7, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{7: cand},
		candidates: []*Account{cand},
	}
	p := newProbeService(t, true, 100, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return false, nil }) // 失败快返回
	p.retryInterval = time.Millisecond
	p.retryBurst = 1

	// 模拟该账号已有补探在途（上一轮 sweep 发起、尚未完成）。
	require.True(t, p.markRetryInFlight(7))

	p.RunOnce(context.Background())
	p.wg.Wait()

	// 给任何被错误发起的补探时间运行并清除 in-flight（若去重失效才会发生）。
	time.Sleep(30 * time.Millisecond)

	// 若去重生效，RunOnce 不会为 7 重新入队补探，7 仍保持 in-flight（未被清除）。
	require.False(t, p.markRetryInFlight(7),
		"在途补探的同账号不得被新一轮 sweep 重复入队（去重生效）")
	p.clearRetryInFlight(7)
}
