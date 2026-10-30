//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type standbyFakeSource struct {
	baseline *StandbyBaselineSample
	window   *StandbyTTFTSample
	throttle *StandbyThrottleStats
	accounts []Account

	err   error
	block bool

	baselineArgs struct {
		from, to time.Time
		hour     int
		loc      *time.Location
	}
	windowArgs struct{ from, to time.Time }
}

func (f *standbyFakeSource) TTFTBaselineP50(ctx context.Context, model string, from, to time.Time, loc *time.Location, hour int) (*StandbyBaselineSample, error) {
	f.baselineArgs.from = from
	f.baselineArgs.to = to
	f.baselineArgs.hour = hour
	f.baselineArgs.loc = loc
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.baseline, f.err
}

func (f *standbyFakeSource) TTFTWindowSample(ctx context.Context, model string, from, to time.Time) (*StandbyTTFTSample, error) {
	f.windowArgs.from = from
	f.windowArgs.to = to
	return f.window, f.err
}

func (f *standbyFakeSource) UpstreamThrottleStats(ctx context.Context, model string, from, to time.Time) (*StandbyThrottleStats, error) {
	return f.throttle, f.err
}

func (f *standbyFakeSource) ListAccountsForStandby(ctx context.Context, platform string) ([]Account, error) {
	return f.accounts, f.err
}

func standbyAccount(schedulable bool) Account {
	return Account{Status: StatusActive, Schedulable: schedulable}
}

func TestStandbyHealthCollector_CollectsAllSignals(t *testing.T) {
	src := &standbyFakeSource{
		baseline: &StandbyBaselineSample{P50Ms: 10000, Days: 14},
		window:   &StandbyTTFTSample{P50Ms: 11000, Samples: 50},
		throttle: &StandbyThrottleStats{ThrottleCount: 1, RequestCount: 80},
		accounts: []Account{standbyAccount(true), standbyAccount(true), standbyAccount(true), standbyAccount(true)},
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	c := NewStandbyHealthCollector(src, 0, loc)

	now := time.Date(2026, 10, 30, 2, 30, 0, 0, time.UTC) // 北京 10:30
	in, verdict := c.Evaluate(context.Background(), "deepseek-v4-pro", "deepseek", now)

	require.Equal(t, StandbyVerdictHealthy, verdict)
	require.Equal(t, 10, src.baselineArgs.hour)
	require.Equal(t, loc, src.baselineArgs.loc)
	require.Equal(t, now.Add(-StandbyWindow), src.windowArgs.from)
	require.Equal(t, now, src.windowArgs.to)
	require.Equal(t, now.Add(-time.Duration(StandbyBaselineDays)*24*time.Hour), src.baselineArgs.from)
	require.Equal(t, now.Add(-StandbyBaselineExcludeWindow), src.baselineArgs.to)
	require.Equal(t, 1.0, *in.SchedulableRatio)
	require.Equal(t, 80, in.UpstreamRequestCount)
}

// 采集超时 → 整轮作废 → Inconclusive（不得用半截数据判定）。
func TestStandbyHealthCollector_TimeoutYieldsInconclusive(t *testing.T) {
	src := &standbyFakeSource{
		block:    true,
		baseline: &StandbyBaselineSample{P50Ms: 10000, Days: 14},
		window:   &StandbyTTFTSample{P50Ms: 100000, Samples: 50},
		throttle: &StandbyThrottleStats{ThrottleCount: 40, RequestCount: 50},
	}
	c := NewStandbyHealthCollector(src, 20*time.Millisecond, time.UTC)

	_, verdict := c.Evaluate(context.Background(), "deepseek-v4-pro", "deepseek", standbyT0)
	require.Equal(t, StandbyVerdictInconclusive, verdict)
}

// 单信号查询失败只让该信号缺失，其余信号仍可判定。
func TestStandbyHealthCollector_PartialFailureKeepsOtherSignals(t *testing.T) {
	src := &standbyFakeSource{
		err:      errors.New("db down"),
		accounts: []Account{standbyAccount(true), standbyAccount(true)},
	}
	c := NewStandbyHealthCollector(src, time.Second, time.UTC)
	in, verdict := c.Evaluate(context.Background(), "kimi-k3", "kimi", standbyT0)
	require.Nil(t, in.TTFTBaselineP50Ms)
	require.Nil(t, in.SchedulableRatio, "账号查询同样失败时该信号缺失")
	require.Equal(t, StandbyVerdictInconclusive, verdict)

	src2 := &standbyFakeSource{
		err:      nil,
		accounts: []Account{standbyAccount(true), standbyAccount(false), standbyAccount(false), standbyAccount(false)},
	}
	// 只有账号信号可用且骤降 → Degraded（L16 同一口径）
	c2 := NewStandbyHealthCollector(src2, time.Second, time.UTC)
	_, verdict2 := c2.Evaluate(context.Background(), "kimi-k3", "kimi", standbyT0)
	require.Equal(t, StandbyVerdictDegraded, verdict2)
}

func TestStandbyHealthCollector_NoAccountsMeansSignalUnavailable(t *testing.T) {
	ratio, ok := standbySchedulableAccountRatio(nil)
	require.False(t, ok)
	require.Zero(t, ratio)

	ratio, ok = standbySchedulableAccountRatio([]Account{standbyAccount(true), standbyAccount(false)})
	require.True(t, ok)
	require.Equal(t, 0.5, ratio)
}
