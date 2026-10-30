package service

import (
	"context"
	"errors"
	"time"
)

// StandbyCollectTimeout 是单轮感知采集的总超时。超时即整轮作废 → Inconclusive，
// 不允许用半截数据做判定。
const StandbyCollectTimeout = 3 * time.Second

// StandbyBaselineSample 是基线查询结果。
type StandbyBaselineSample struct {
	// P50Ms 是逐日同小时 p50 的中位数。
	P50Ms float64
	// Days 是参与聚合的天数。
	Days int
}

// StandbyTTFTSample 是当前窗口的延迟样本。
type StandbyTTFTSample struct {
	P50Ms   float64
	Samples int
}

// StandbyThrottleStats 是窗口内的上游限流统计。
type StandbyThrottleStats struct {
	// ThrottleCount 是 upstream_status_code ∈ {429, 529} 的错误行数。
	ThrottleCount int
	// RequestCount 是窗口请求数（成功用量行 + ThrottleCount）。
	RequestCount int
}

// StandbyHealthSource 是感知层的数据源端口（只读）。返回 nil 表示该信号无数据。
type StandbyHealthSource interface {
	// TTFTBaselineP50 取 [from, to) 内、与 hour（在 loc 时区）同小时段的逐日 TTFT p50 中位数。
	TTFTBaselineP50(ctx context.Context, model string, from, to time.Time, loc *time.Location, hour int) (*StandbyBaselineSample, error)
	// TTFTWindowSample 取 [from, to) 内的 TTFT p50 与样本数。
	TTFTWindowSample(ctx context.Context, model string, from, to time.Time) (*StandbyTTFTSample, error)
	// UpstreamThrottleStats 取 [from, to) 内的上游 429/529 统计。
	UpstreamThrottleStats(ctx context.Context, model string, from, to time.Time) (*StandbyThrottleStats, error)
	// ListAccountsForStandby 列出该平台下未删除的账号（调用方据此算可调度占比）。
	ListAccountsForStandby(ctx context.Context, platform string) ([]Account, error)
}

// StandbyHealthCollector 把数据源拼成一次判定输入，并暴露单入口 Evaluate。
// 它不缓存、不写库，也不做状态迁移。
type StandbyHealthCollector struct {
	source   StandbyHealthSource
	timeout  time.Duration
	location *time.Location
}

// NewStandbyHealthCollector 构造采集器。timeout <= 0 用 StandbyCollectTimeout；
// location 为 nil 时用 UTC。hour-of-day 分桶依赖该时区，生产必须传配置时区
// （time.Local 的 zone 名 "Local" 在 Postgres 里不可解析，故不作默认值）。
func NewStandbyHealthCollector(source StandbyHealthSource, timeout time.Duration, location *time.Location) *StandbyHealthCollector {
	if timeout <= 0 {
		timeout = StandbyCollectTimeout
	}
	if location == nil {
		location = time.UTC
	}
	return &StandbyHealthCollector{source: source, timeout: timeout, location: location}
}

// Collect 采集一轮信号。任一查询失败只让该信号缺失；整轮超时或全部查询失败才返回错误。
func (c *StandbyHealthCollector) Collect(ctx context.Context, model, platform string, now time.Time) (StandbyHealthInput, error) {
	in := StandbyHealthInput{Model: model}
	if c == nil || c.source == nil {
		return in, errors.New("standby: nil health source")
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	baselineFrom := now.Add(-time.Duration(StandbyBaselineDays) * 24 * time.Hour)
	baselineTo := now.Add(-StandbyBaselineExcludeWindow)
	windowFrom := now.Add(-StandbyWindow)
	hour := now.In(c.location).Hour()

	failed := 0
	var firstErr error
	fail := func(err error) {
		failed++
		if firstErr == nil {
			firstErr = err
		}
	}

	if sample, err := c.source.TTFTBaselineP50(ctx, model, baselineFrom, baselineTo, c.location, hour); err != nil {
		fail(err)
	} else if sample != nil {
		p50 := sample.P50Ms
		in.TTFTBaselineP50Ms = &p50
		in.TTFTBaselineDays = sample.Days
	}

	if sample, err := c.source.TTFTWindowSample(ctx, model, windowFrom, now); err != nil {
		fail(err)
	} else if sample != nil && sample.Samples > 0 {
		p50 := sample.P50Ms
		in.TTFTCurrentP50Ms = &p50
		in.TTFTCurrentSamples = sample.Samples
	}

	if stats, err := c.source.UpstreamThrottleStats(ctx, model, windowFrom, now); err != nil {
		fail(err)
	} else if stats != nil {
		in.UpstreamThrottleCount = stats.ThrottleCount
		in.UpstreamRequestCount = stats.RequestCount
	}

	if accounts, err := c.source.ListAccountsForStandby(ctx, platform); err != nil {
		fail(err)
	} else if ratio, ok := standbySchedulableAccountRatio(accounts); ok {
		in.SchedulableRatio = &ratio
	}

	// 整轮超时：即使部分信号已到手也整轮作废，避免用陈旧/半截数据判定。
	if err := ctx.Err(); err != nil {
		return in, err
	}
	if failed == 4 {
		return in, firstErr
	}
	return in, nil
}

// Evaluate 是感知 + 决策的单入口：采集失败（含超时）一律 Inconclusive。
func (c *StandbyHealthCollector) Evaluate(ctx context.Context, model, platform string, now time.Time) (StandbyHealthInput, StandbyVerdict) {
	in, err := c.Collect(ctx, model, platform, now)
	if err != nil {
		return in, StandbyVerdictInconclusive
	}
	return in, EvaluateMainChannelHealth(in)
}

// standbySchedulableAccountRatio 计算可调度账号占比；无账号时 ok=false（信号不可用）。
func standbySchedulableAccountRatio(accounts []Account) (float64, bool) {
	if len(accounts) == 0 {
		return 0, false
	}
	schedulable := 0
	for i := range accounts {
		if accounts[i].IsSchedulable() {
			schedulable++
		}
	}
	return float64(schedulable) / float64(len(accounts)), true
}
