//go:build unit

// 卡 A（#1）定向用例：429/额度耗尽类补探减速 + 首成功后加速。
//
// 验收四场景（派发单 §4 + 方案 §3 卡 A）：
//   - 429（StatusCode=429）停调账号：补探间隔取 5min；
//   - 429 账号首次探测成功后：下一次间隔回落到 5s（首成功后加速）；
//   - 非 429（如 500/401）账号：保持 5s；
//   - isQuotaExhaustedTrip：429 返回 true，500/401/0 返回 false。
//
// 间隔断言通过结构化日志的 interval 字段读取（生产把该字段改为"本账号本次实际采用间隔"）。
// 为免真实等待 5min/5s，探测 override 在补探首轮后立即取消 ctx，使 sleepBeforeProbeRetry
// 被 ctx 取消强退，但本轮已产出的 interval 日志不变。
//
// 本文件用例命名均以 TestProbeQuota / TestProbeIsQuota 开头，命中白名单
// -run "HealthRecoveryProbe|ProbeRetry|Breaker|Quota|ProbeInterval|ProbeConsecutive"。
package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// parkedBreakerAccountWithStatusCode 构造被健康熔断停调、并带指定 StatusCode 的账号
// （isQuotaExhaustedTrip 据此区分 429 慢类与其它状态码）。
func parkedBreakerAccountWithStatusCode(id int64, statusCode int) *Account {
	until := time.Now().Add(5 * time.Minute)
	state := TempUnschedState{
		UntilUnix:      until.Unix(),
		MatchedKeyword: openAIAPIKeyHealthBreakerReason,
		Tier:           HealthBreakerTierTrip,
		StatusCode:     statusCode,
	}
	reason, _ := json.Marshal(state)
	return &Account{
		ID:                      id,
		Platform:                domain.PlatformOpenAI,
		Type:                    AccountTypeAPIKey,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: string(reason),
	}
}

// probeRetryLogIntervals 抽取结构化日志中 openai.apikey_health_probe_retry 事件携带的
// interval 字段（time.Duration），供断言每个账号本轮实际采用的补探间隔。
func probeRetryLogIntervals(t *testing.T, sink *inMemoryLogSink) []time.Duration {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var out []time.Duration
	for _, ev := range sink.events {
		if ev == nil || ev.Fields == nil {
			continue
		}
		if !strings.Contains(ev.Message, "openai.apikey_health_probe_retry") {
			continue
		}
		if v, ok := ev.Fields["interval"]; ok {
			if d, ok := v.(time.Duration); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

// TestProbeIsQuotaExhaustedTrip 直接单测分类函数：429 慢类返回 true；500/401/无 reason/0 均 false。
func TestProbeIsQuotaExhaustedTrip(t *testing.T) {
	p := &AccountHealthRecoveryProbeService{}

	// 429 → 慢类 true
	a429 := parkedBreakerAccountWithStatusCode(1, http.StatusTooManyRequests)
	require.True(t, p.isQuotaExhaustedTrip(a429), "429 应判为额度耗尽慢类")

	// 500 → 非慢类 false
	a500 := parkedBreakerAccountWithStatusCode(2, http.StatusInternalServerError)
	require.False(t, p.isQuotaExhaustedTrip(a500), "500 属 5xx 现状类，不归慢类")

	// 401 → 非慢类 false（401 冷却 / oauth401 / pool401 均保持秒级）
	a401 := parkedBreakerAccountWithStatusCode(3, http.StatusUnauthorized)
	require.False(t, p.isQuotaExhaustedTrip(a401), "401 不属慢类")

	// StatusCode=0（未知/非 429）→ false
	a0 := parkedBreakerAccountWithStatusCode(4, 0)
	require.False(t, p.isQuotaExhaustedTrip(a0), "StatusCode=0 不属慢类")

	// 空 reason（无法解析）→ false
	empty := &Account{ID: 5, TempUnschedulableReason: ""}
	require.False(t, p.isQuotaExhaustedTrip(empty), "空 reason 返回 false")
}

// TestProbeQuotaRetryIntervalFor429Account 锁定：429 停调账号补探间隔取 5min。
func TestProbeQuotaRetryIntervalFor429Account(t *testing.T) {
	const acct int64 = 7
	acc := parkedBreakerAccountWithStatusCode(acct, http.StatusTooManyRequests)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{acct: acc},
		candidates: []*Account{acc},
	}
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	p := newProbeService(t, true, rl, repo)
	// 不注入 retryInterval：慢类间隔恒为 accountHealthProbeQuotaRetryInterval=5min。

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int32
	// 首探（n=1）失败；补探（n=2）失败并取消 ctx 使循环在 sleep 前强退——本轮已记录 5min。
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		n := atomic.AddInt32(&calls, 1)
		if n >= 2 {
			cancel()
		}
		return false, nil
	})

	sink, stop := captureStructuredLog(t)
	defer stop()

	p.RunOnce(ctx)
	p.wg.Wait()

	intervals := probeRetryLogIntervals(t, sink)
	require.NotEmpty(t, intervals, "补探必须产出 interval 日志")
	require.Equal(t, 5*time.Minute, intervals[0], "429 账号补探间隔必须取 5min")
}

// TestProbeQuotaRetryInterval429AcceleratesAfterFirstSuccess 锁定：429 账号首次探测成功后，
// 下一次间隔回落到 5s（首成功后加速确认第二次），而非继续 5min。
func TestProbeQuotaRetryInterval429AcceleratesAfterFirstSuccess(t *testing.T) {
	const acct int64 = 7
	acc := parkedBreakerAccountWithStatusCode(acct, http.StatusTooManyRequests)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{acct: acc},
		candidates: []*Account{acc},
	}
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	p := newProbeService(t, true, rl, repo)
	// 不注入 retryInterval：非慢类/加速路径恒为 accountHealthProbeRetryInterval=5s。

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int32
	// 首探（n=1）失败 → 补探（n=2）成功（ConsecutiveSuccesses=1，未达阈值、仍 parked）→ 取消 ctx。
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		n := atomic.AddInt32(&calls, 1)
		if n >= 2 {
			cancel()
		}
		return n > 1, nil
	})

	sink, stop := captureStructuredLog(t)
	defer stop()

	p.RunOnce(ctx)
	p.wg.Wait()

	intervals := probeRetryLogIntervals(t, sink)
	require.NotEmpty(t, intervals, "补探必须产出 interval 日志")
	require.Equal(t, 5*time.Second, intervals[0], "429 账号首成功后加速：下一间隔必须回落到 5s")
	require.NotEqual(t, 5*time.Minute, intervals[0], "首成功后不得继续 5min 慢类间隔")
}

// TestProbeQuotaRetryIntervalNon429Stays5s 锁定：非 429（500）账号补探保持 5s 秒级现状。
func TestProbeQuotaRetryIntervalNon429Stays5s(t *testing.T) {
	const acct int64 = 7
	acc := parkedBreakerAccountWithStatusCode(acct, http.StatusInternalServerError)
	repo := &probeRepoMock{
		accounts:   map[int64]*Account{acct: acc},
		candidates: []*Account{acc},
	}
	rl := NewRateLimitService(&probeRateLimitRepo{}, nil, &config.Config{}, nil, nil)
	p := newProbeService(t, true, rl, repo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int32
	p.SetProbeOverride(func(_ context.Context, _ *Account) (bool, error) {
		n := atomic.AddInt32(&calls, 1)
		if n >= 2 {
			cancel()
		}
		return false, nil
	})

	sink, stop := captureStructuredLog(t)
	defer stop()

	p.RunOnce(ctx)
	p.wg.Wait()

	intervals := probeRetryLogIntervals(t, sink)
	require.NotEmpty(t, intervals, "补探必须产出 interval 日志")
	require.Equal(t, 5*time.Second, intervals[0], "非 429 账号补探间隔必须保持 5s")
}
