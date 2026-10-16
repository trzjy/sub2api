//go:build unit

// AC-G2R4 第 5 轮两条 must_fix 的定向回归（gate② 终版 output_sha256 a3da31c9ee68802b）：
//   - Stop/Start 重入竞态修复：Stop 关闭路径锁内置 stopping=true 并取走 stopCh（置 nil），
//     锁外 close + wg.Wait；期间并发 Start 被 stopping 挡住立即返回，绝不执行 wg.Add，
//     从而 wg.Add 永不与 wg.Wait 交错；Stop 完整返回后 Start 才可用。
//   - 清除所有权（条件清除，AC-G2R5 代际令牌改为 until）：探测成功提交恢复时以探测开始/入队时
//     读到的 until（UNIX 秒）作不可变代际令牌，仓储层原子条件清除仅当 until 未变（rows>0）才
//     恢复；until 被他人改写为更晚值（新停调接管，rows==0）则不清除、记 Info 日志、账号保持新停调。
//   - 补探 spawn 收进锁段（must_fix2）：Stop 进行中（stopping=true）时 RunOnce 补探启动路径在
//     startMu 锁内发现 stopping → 不入队、清理本轮已标 in-flight 标记，杜绝锁外 wg.Add 与
//     Stop 的 wg.Wait 交错。
//
// 本文件仅含 acct 卡列新增的定向用例，命名以 TestHealthProbe 前缀命中验证白名单
// `-run "TestHealthProbe|TestCodeBuddy|TestProbeInterval"`。
package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// TestHealthProbeStopBlocksConcurrentStart 锁定 Stop/Start 重入竞态修复：Stop 在关闭路径上
// 锁内置 stopping=true 并取走 stopCh（置 nil），锁外 close + wg.Wait；期间任何并发 Start 在
// 锁内被 stopping 挡住立即返回（绝不执行 wg.Add），从而 wg.Add 永不与 wg.Wait 交错。Stop 完整
// 返回后（stopping 清零、stopCh 仍为 nil）Start 才重新可用、重建 stopCh 并启动循环。
func TestHealthProbeStopBlocksConcurrentStart(t *testing.T) {
	// 用 newProbeService 取得带真实 settingService 的服务（使后续真实 Start 可读配置、启动循环）。
	repo := &probeRepoMock{accounts: map[int64]*Account{}, candidates: nil}
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	p := newProbeService(t, true, 10, rl, repo)

	// 模拟「服务已启动」：持有非空 stopCh（与 Start 守卫同口径），不启动真实循环。
	p.startMu.Lock()
	p.stopCh = make(chan struct{})
	p.startMu.Unlock()

	// 注入一个在途 goroutine，使 Stop 的 wg.Wait 阻塞（模拟补探/主循环在途），
	// 制造确定的「Stop 等待期间」窗口。
	released := make(chan struct{})
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		<-released
	}()

	// 并发调用 Stop（将阻塞于 wg.Wait）。
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()

	// 等待 Stop 拿到关闭权（stopping=true 且 stopCh 已置 nil）再发起并发 Start。
	require.Eventually(t, func() bool {
		p.startMu.Lock()
		defer p.startMu.Unlock()
		return p.stopping && p.stopCh == nil
	}, time.Second, 5*time.Millisecond, "Stop 必须进入关闭路径（stopping=true 且 stopCh=nil）")

	// 并发 Start 必须被 stopping 立即挡住、不得阻塞。
	startReturned := make(chan struct{})
	go func() {
		// 即便 settingService 就绪，stopping 标志也应使其立即返回、不启动循环、不 wg.Add。
		p.Start(context.Background())
		close(startReturned)
	}()
	select {
	case <-startReturned:
		// 期望：Start 立即返回（被拒）。
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 等待期间并发 Start 被阻塞，未按预期被 stopping 立即拒绝")
	}

	// 此时 Stop 仍在等待在途 goroutine；确认 stopping 仍为 true、stopCh 仍 nil。
	p.startMu.Lock()
	require.True(t, p.stopping, "Stop 等待期间 stopping 必须保持 true")
	require.Nil(t, p.stopCh, "Stop 已取走 stopCh，必须为 nil")
	p.startMu.Unlock()

	// 放行在途 goroutine，Stop 的 wg.Wait 返回，stopping 清零。
	close(released)
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 未能在在途 goroutine 结束后返回")
	}

	// Stop 返回后：stopping 已清零、stopCh 仍为 nil → Start 重新可用、重建 stopCh 并启动循环。
	p.Start(context.Background())
	p.startMu.Lock()
	require.NotNil(t, p.stopCh, "Stop 返回后 Start 必须可用（重建 stopCh）")
	p.startMu.Unlock()

	// 清理：再次 Stop 关闭 stopCh，循环退出，避免 goroutine 泄漏。
	p.Stop()
}

// TestHealthProbeConditionalClearSkipsWhenUntilTakenOver 锁定条件清除所有权（AC-G2R5 代际令牌
// 改为 until）：探测成功但探测期间 until 已被他人改写为更晚值（他因新停调接管）→ 仓储层代际
// 令牌失配、rows==0、返回 false，账号保持新停调（不清）、并记 Info 日志
// 「probe skipped clear: temp-unsched generation taken over」。
func TestHealthProbeConditionalClearSkipsWhenUntilTakenOver(t *testing.T) {
	const acct int64 = 7
	// 账号当前 until（探测开始时读到的代际令牌）。
	acc := parkedBreakerAccount(acct, 0)
	originalUntil := acc.TempUnschedulableUntil.Unix()
	// 模拟 DB 中该账号的 until 已被他人改写为更晚值（新停调接管）。
	repo := &probeRepoMock{
		accounts:     map[int64]*Account{acct: acc},
		candidates:   []*Account{acc},
		storedUntils: map[int64]int64{acct: originalUntil + 100},
	}
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	p := newProbeService(t, true, 10, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })

	sink, stop := captureStructuredLog(t)
	defer stop()

	stillParked := p.probeAccount(context.Background(), acc, 10)

	// 条件清除被调用一次（尝试），但因代际失配未清除（命中失配、零写入）。
	require.Equal(t, 1, repo.clearTempIfReasonCalls, "条件清除必须被调用（尝试）")
	require.True(t, stillParked, "until 被改写为更晚值（他因接管）时账号必须保持停调（不得清除）")
	require.True(t, sink.ContainsMessageAtLevel("probe skipped clear: temp-unsched generation taken over", "info"),
		"代际失配必须产出 temp-unsched generation taken over 的 Info 日志")
}

// TestHealthProbeConditionalClearRecoversWhenUntilUnchanged 正向锁定：探测成功且 until 未变
// （代际令牌命中）→ 条件清除 rows>0、返回 true，账号恢复（stillParked=false）。
func TestHealthProbeConditionalClearRecoversWhenUntilUnchanged(t *testing.T) {
	const acct int64 = 7
	acc := parkedBreakerAccount(acct, 0)
	// 不设置 storedUntils：缺省取账号当前 until，代际令牌命中。
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{acct: acc},
		candidates: []*Account{acc},
	}
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	p := newProbeService(t, true, 10, rl, repo)
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) { return true, nil })

	stillParked := p.probeAccount(context.Background(), acc, 10)

	require.Equal(t, 1, repo.clearTempIfReasonCalls, "条件清除必须被调用并命中")
	require.False(t, stillParked, "until 未变时探测成功必须恢复（清除停调）")
}

// 用例 A（AC-G2R5 第 6 轮 must_fix1 回归）：首探失败 → bumpProbeAttempts 改写 reason（写入新
// attempt 计数）→ 补探成功 → 仍按入队时读到的 until 代际令牌清除成功。证明 G2R4 的 reason
// 代际缺陷（bump 持续改写 reason 导致补探令牌失配清不掉）已被 until 代际令牌根治：同一次停调内
// bump 只改 reason 不改 until，补探令牌恒命中。
func TestG2R5_GenerationTokenSurvivesReasonBump(t *testing.T) {
	const acct int64 = 7
	acc := parkedBreakerAccount(acct, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{acct: acc},
		candidates: []*Account{acc},
	}
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	p := newProbeService(t, true, 10, rl, repo)
	setFastProbeRetry(p) // 补探节奏注入毫秒级，单测不真实等待

	// 探测：首探失败、补探（第 2 次调用）成功——验证 until 代际令牌跨 reason 改写仍命中。
	var attempts int32
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		n := atomic.AddInt32(&attempts, 1)
		return n > 1, nil // 首探失败，补探成功
	})

	// 经由 RunOnce 完整路径：首探（失败、bump reason）→ 异步补探（成功、按 until 清除）。
	p.RunOnce(context.Background())
	p.wg.Wait() // 等待异步补探完成，使清除断言确定

	require.Equal(t, int32(2), atomic.LoadInt32(&attempts), "应发生一次首探 + 一次补探")
	// bumpProbeAttempts 在首探失败后改写 reason（写入 ProbeAttempts=1），证明 reason 确实变了。
	st, ok := parseHealthBreakerState(acc.TempUnschedulableReason)
	require.True(t, ok, "bump 后 reason 仍应是合法 breaker 状态")
	require.Equal(t, 1, st.ProbeAttempts, "首探失败后 bumpProbeAttempts 必须改写 reason（写入 attempt 计数）")
	// 尽管 reason 被改写，补探仍按入队时读到的 until 代际令牌命中清除。
	require.Equal(t, 1, repo.clearTempIfReasonCalls, "补探成功必须按 until 代际令牌清除停调（G2R4 缺陷已根治）")
	require.False(t, p.isRetryInFlight(acct), "补探结束后 in-flight 标记必须被清理")
}

// 用例 A（续）：期间他因写入更晚 until（新停调接管）→ 补探成功的代际令牌失配、清除零写入、
// 新停调保留（与 TestHealthProbeConditionalClearSkipsWhenUntilTakenOver 同口径，但走完整
// RunOnce 补探路径）。
func TestG2R5_GenerationMismatchPreservesNewPause(t *testing.T) {
	const acct int64 = 7
	acc := parkedBreakerAccount(acct, 0)
	originalUntil := acc.TempUnschedulableUntil.Unix()
	// 他因接管：DB 中 until 已被改写为更晚值。
	repo := &probeRepoMock{
		accounts:     map[int64]*Account{acct: acc},
		candidates:   []*Account{acc},
		storedUntils: map[int64]int64{acct: originalUntil + 100},
	}
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	p := newProbeService(t, true, 10, rl, repo)
	setFastProbeRetry(p)

	var attempts int32
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		n := atomic.AddInt32(&attempts, 1)
		return n > 1, nil // 首探失败，补探成功——但 until 代际失配 → 终止补探
	})

	p.RunOnce(context.Background())
	p.wg.Wait()

	// 代际守卫（AC-G2R6 must_fix）：首探失败后 attempt 回写 BumpTempUnschedulable
	// ReasonIfGeneration 用入队令牌比对，发现 until 已被接管（storedUntils 更晚）→ 失配
	// 返回 false，生产据此终止本轮补探（账号已属新停调，旧代探针不应继续操作），故仅首探
	// 一次、条件清除永不被调用——新停调得以保留（GenerationMismatchPreservesNewPause）。
	require.Equal(t, int32(1), atomic.LoadInt32(&attempts), "代际失配（他因接管）：首探失败后回写守卫失配，终止本轮补探，仅首探一次")
	require.Equal(t, 1, repo.bumpTempIfReasonCalls, "attempt 回写必须被尝试一次（代际守卫 BumpTempUnschedulableReasonIfGeneration）")
	require.Equal(t, 0, repo.clearTempIfReasonCalls, "代际失配终止补探，条件清除永不被调用")
	require.False(t, p.isRetryInFlight(acct), "补探终止后 in-flight 标记必须被清理")
}

// 用例 B（AC-G2R5 第 6 轮 must_fix2 回归）：Stop 进行中（stopping=true）时 RunOnce 的补探启动
// 路径在 startMu 锁内发现 stopping=true → 不入队补探、清理本轮已标 in-flight 的账号标记后返回，
// 杜绝「锁外 wg.Add 与 Stop 的 wg.Wait 交错」。
func TestG2R5_StopInProgressRetryNotEnqueued(t *testing.T) {
	const acct int64 = 7
	acc := parkedBreakerAccount(acct, 0)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{acct: acc},
		candidates: []*Account{acc},
	}
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	p := newProbeService(t, true, 10, rl, repo)
	setFastProbeRetry(p)

	// 首探恒定失败：确保 sweep 标记 in-flight 并入队补探候选。
	var attempts int32
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		atomic.AddInt32(&attempts, 1)
		return false, nil
	})

	// 模拟 Stop 进行中：在 RunOnce 之前于锁内置 stopping=true（与 Stop 关闭路径同口径），
	// 但不真正启动 Stop 的 wg.Wait 流程。
	p.startMu.Lock()
	p.stopping = true
	p.startMu.Unlock()

	p.RunOnce(context.Background())
	// 锁内 stopping=true → 补探 goroutine 未启动（无 wg.Add），故 wg.Wait 立即返回。
	p.wg.Wait()

	require.Equal(t, int32(1), atomic.LoadInt32(&attempts),
		"Stop 进行中补探不得入队：仅首探一次，无补探运行")
	require.False(t, p.isRetryInFlight(acct),
		"Stop 进行中必须清理本轮已标 in-flight 标记，避免后续 sweep 永久跳过该账号")
	// 清理 stopping 标志，避免影响服务其余状态（本用例为孤立构造）。
	p.startMu.Lock()
	p.stopping = false
	p.startMu.Unlock()
}
