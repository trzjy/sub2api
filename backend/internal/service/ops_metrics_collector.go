package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

const (
	opsMetricsCollectorJobName     = "ops_metrics_collector"
	opsMetricsCollectorMinInterval = 60 * time.Second
	opsMetricsCollectorMaxInterval = 1 * time.Hour

	opsMetricsCollectorTimeout = 10 * time.Second

	opsMetricsCollectorLeaderLockKey = "ops:metrics:collector:leader"
	opsMetricsCollectorLeaderLockTTL = 90 * time.Second

	opsMetricsCollectorHeartbeatTimeout = 2 * time.Second

	bytesPerMB = 1024 * 1024
)

// ── 「上游错误分子」的统一 SQL 口径（SSOT）────────────────────────────────────
//
// 同一指标（upstream_error_count_excl_429_529）在本仓共有 4 份 SQL 副本：
//
//  1. repository/ops_repo_dashboard.go  queryErrorCounts   （raw 仪表盘）
//  2. service/ops_metrics_collector.go  queryErrorCounts   （1min 预聚合）  ← 本文件
//  3. repository/ops_repo_preagg.go     error_agg          （hourly 表）
//  4. repository/ops_repo_trends.go     GetErrorTrend      （趋势图）
//
// 历史漂移：只有 #1 带 `status_code >= 400` 门控，#2/#3/#4 没有。门控缺失时
// ops_error_logs 里的 recovered 行会被算成上游错误——logOpsRecoveredUpstream
// 为「上游失败但最终成功」的请求记一条 provider 归属错误，其 status_code 是
// 2xx、upstream_status_code 非 2xx。生产实测 recovered 行占该分子 54%
// （preagg 2252 vs raw 1036，同一屏差 2.17 倍）。
//
// 这几个谓词因此必须逐字一致，故集中到本文件导出，4 处引用同一来源。改动任一
// 谓词时四处同步生效，不会再出现「只改了仪表盘、预聚合仍是旧口径」。
//
// 列名按各自 FROM 子句适配：#1/#2/#4 直接查 ops_error_logs，最终状态列名就是
// `status_code`；#3 的 error_base CTE 把它投影成 `client_status_code`，故用一个
// %[1]s 占位符渲染，其余谓词逐字相同。
const (
	// opsErrorFinalFailedGateTemplate 是「本次请求最终对客户端失败」的门控。
	// status_code 是最终客户端可见状态；upstream_status_code 只是最后一次上游
	// 尝试的状态，即使整体成功也会被写入。recovered 行正是「status_code=2xx 且
	// upstream_status_code 非 2xx」，故必须按最终状态门控。
	opsErrorFinalFailedGateTemplate = `COALESCE(%[1]s, 0) >= 400`

	// OpsUpstreamErrorExclFilterSQLTemplate 是「上游错误（剔除 429/529）」分子的
	// 谓词，也是本次门控整改的目标。%[1]s 处填入该处可用的「最终状态列」
	// （status_code / client_status_code）。
	//
	// 只覆盖 excl、不覆盖 429/529 两个分子，是刻意与 #1（dashboard，已修且不在
	// 本次可改范围）保持逐字一致：dashboard 的 upstream_429/upstream_529 至今
	// 未加最终状态门控。若此处单独给 429/529 加门控，preagg 会比 raw 少算，
	// 等于用新的漂移替换旧的漂移。429/529 的 recovered 污染是独立议题，
	// 需连同 dashboard 一并改。
	OpsUpstreamErrorExclFilterSQLTemplate = opsErrorFinalFailedGateTemplate +
		` AND error_owner = 'provider' AND NOT is_business_limited` +
		` AND COALESCE(upstream_status_code, %[1]s, 0) NOT IN (429, 529)`

	// OpsUpstream429FilterSQLTemplate / OpsUpstream529FilterSQLTemplate 是限流/过载
	// 两个分子的谓词，逐字复制 #1 的现状（无最终状态门控）。抽成常量是为了让
	// 「这里为什么没有门控」在代码里可见，而不是靠四处心照不宣。
	// %[1]s 处填入该处可用的「最终状态列」。
	OpsUpstream429FilterSQLTemplate = `error_owner = 'provider' AND NOT is_business_limited` +
		` AND COALESCE(upstream_status_code, %[1]s, 0) = 429`
	OpsUpstream529FilterSQLTemplate = `error_owner = 'provider' AND NOT is_business_limited` +
		` AND COALESCE(upstream_status_code, %[1]s, 0) = 529`
)

// OpsUpstreamErrorFilterSQL 按给定「最终状态列」渲染上游错误（剔除 429/529）分子谓词。
// 4 处副本统一走这个入口，避免各处手写占位符替换时漏改或漏门控。
func OpsUpstreamErrorFilterSQL(finalStatusCol string) string {
	return fmt.Sprintf(OpsUpstreamErrorExclFilterSQLTemplate, finalStatusCol)
}

// OpsUpstreamErrorFilterFinalStatusGate 渲染「最终对客户端失败」门控，供本包的
// error_total / business_limited / error_sla 三个分子，以及 repository 包的
// ops_repo_trends.go / ops_repo_preagg.go 复用（跨包，故导出）。
func OpsUpstreamErrorFilterFinalStatusGate(finalStatusCol string) string {
	return fmt.Sprintf(opsErrorFinalFailedGateTemplate, finalStatusCol)
}

// OpsUpstream429FilterFor 是 429 分子的按列渲染入口。
func OpsUpstream429FilterFor(finalStatusCol string) string {
	return fmt.Sprintf(OpsUpstream429FilterSQLTemplate, finalStatusCol)
}

// OpsUpstream529FilterFor 是 529 分子的按列渲染入口。
func OpsUpstream529FilterFor(finalStatusCol string) string {
	return fmt.Sprintf(OpsUpstream529FilterSQLTemplate, finalStatusCol)
}

var opsMetricsCollectorAdvisoryLockID = hashAdvisoryLockID(opsMetricsCollectorLeaderLockKey)

type opsSchedulableAccountLoadRepository interface {
	ListSchedulableAccountLoads(ctx context.Context) ([]AccountWithConcurrency, error)
}

type OpsMetricsCollector struct {
	opsRepo     OpsRepository
	settingRepo SettingRepository
	cfg         *config.Config

	accountRepo        AccountRepository
	concurrencyService *ConcurrencyService

	db          *sql.DB
	redisClient *redis.Client
	instanceID  string

	lastCgroupCPUUsageNanos uint64
	lastCgroupCPUSampleAt   time.Time

	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once

	skipLogMu sync.Mutex
	skipLogAt time.Time
}

func NewOpsMetricsCollector(
	opsRepo OpsRepository,
	settingRepo SettingRepository,
	accountRepo AccountRepository,
	concurrencyService *ConcurrencyService,
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
) *OpsMetricsCollector {
	return &OpsMetricsCollector{
		opsRepo:            opsRepo,
		settingRepo:        settingRepo,
		cfg:                cfg,
		accountRepo:        accountRepo,
		concurrencyService: concurrencyService,
		db:                 db,
		redisClient:        redisClient,
		instanceID:         uuid.NewString(),
	}
}

func (c *OpsMetricsCollector) Start() {
	if c == nil {
		return
	}
	c.startOnce.Do(func() {
		if c.stopCh == nil {
			c.stopCh = make(chan struct{})
		}
		go c.run()
	})
}

func (c *OpsMetricsCollector) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		if c.stopCh != nil {
			close(c.stopCh)
		}
	})
}

func (c *OpsMetricsCollector) run() {
	// First run immediately so the dashboard has data soon after startup.
	c.collectOnce()

	for {
		interval := c.getInterval()
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
			c.collectOnce()
		case <-c.stopCh:
			timer.Stop()
			return
		}
	}
}

func (c *OpsMetricsCollector) getInterval() time.Duration {
	interval := opsMetricsCollectorMinInterval

	if c.settingRepo == nil {
		return interval
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	raw, err := c.settingRepo.GetValue(ctx, SettingKeyOpsMetricsIntervalSeconds)
	if err != nil {
		return interval
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return interval
	}

	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return interval
	}
	if seconds < int(opsMetricsCollectorMinInterval.Seconds()) {
		seconds = int(opsMetricsCollectorMinInterval.Seconds())
	}
	if seconds > int(opsMetricsCollectorMaxInterval.Seconds()) {
		seconds = int(opsMetricsCollectorMaxInterval.Seconds())
	}
	return time.Duration(seconds) * time.Second
}

func (c *OpsMetricsCollector) collectOnce() {
	if c == nil {
		return
	}
	if c.cfg != nil && !c.cfg.Ops.Enabled {
		return
	}
	if c.opsRepo == nil {
		return
	}
	if c.db == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), opsMetricsCollectorTimeout)
	defer cancel()

	if !c.isMonitoringEnabled(ctx) {
		return
	}

	release, ok := c.tryAcquireLeaderLock(ctx)
	if !ok {
		return
	}
	if release != nil {
		defer release()
	}

	startedAt := time.Now().UTC()
	err := c.collectAndPersist(ctx)
	finishedAt := time.Now().UTC()

	durationMs := finishedAt.Sub(startedAt).Milliseconds()
	dur := durationMs
	runAt := startedAt

	if err != nil {
		msg := truncateString(err.Error(), 2048)
		errAt := finishedAt
		hbCtx, hbCancel := context.WithTimeout(context.Background(), opsMetricsCollectorHeartbeatTimeout)
		defer hbCancel()
		_ = c.opsRepo.UpsertJobHeartbeat(hbCtx, &OpsUpsertJobHeartbeatInput{
			JobName:        opsMetricsCollectorJobName,
			LastRunAt:      &runAt,
			LastErrorAt:    &errAt,
			LastError:      &msg,
			LastDurationMs: &dur,
		})
		log.Printf("[OpsMetricsCollector] collect failed: %v", err)
		return
	}

	successAt := finishedAt
	hbCtx, hbCancel := context.WithTimeout(context.Background(), opsMetricsCollectorHeartbeatTimeout)
	defer hbCancel()
	_ = c.opsRepo.UpsertJobHeartbeat(hbCtx, &OpsUpsertJobHeartbeatInput{
		JobName:        opsMetricsCollectorJobName,
		LastRunAt:      &runAt,
		LastSuccessAt:  &successAt,
		LastDurationMs: &dur,
	})
}

func (c *OpsMetricsCollector) isMonitoringEnabled(ctx context.Context) bool {
	if c == nil {
		return false
	}
	if c.cfg != nil && !c.cfg.Ops.Enabled {
		return false
	}
	if c.settingRepo == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}

	value, err := c.settingRepo.GetValue(ctx, SettingKeyOpsMonitoringEnabled)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return true
		}
		// Fail-open: collector should not become a hard dependency.
		return true
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "false", "0", "off", "disabled":
		return false
	default:
		return true
	}
}

func (c *OpsMetricsCollector) collectAndPersist(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// Align to stable minute boundaries to avoid partial buckets and to maximize cache hits.
	now := time.Now().UTC()
	windowEnd := now.Truncate(time.Minute)
	windowStart := windowEnd.Add(-1 * time.Minute)

	sys, err := c.collectSystemStats(ctx)
	if err != nil {
		// Continue; system stats are best-effort.
		log.Printf("[OpsMetricsCollector] system stats error: %v", err)
	}

	dbOK := c.checkDB(ctx)
	redisOK := c.checkRedis(ctx)
	active, idle := c.dbPoolStats()
	redisTotal, redisIdle, redisStatsOK := c.redisPoolStats()

	successCount, tokenConsumed, err := c.queryUsageCounts(ctx, windowStart, windowEnd)
	if err != nil {
		return fmt.Errorf("query usage counts: %w", err)
	}

	duration, ttft, err := c.queryUsageLatency(ctx, windowStart, windowEnd)
	if err != nil {
		return fmt.Errorf("query usage latency: %w", err)
	}

	errorTotal, businessLimited, errorSLA, upstreamExcl, upstream429, upstream529, err := c.queryErrorCounts(ctx, windowStart, windowEnd)
	if err != nil {
		return fmt.Errorf("query error counts: %w", err)
	}

	accountSwitchCount, err := c.queryAccountSwitchCount(ctx, windowStart, windowEnd)
	if err != nil {
		return fmt.Errorf("query account switch counts: %w", err)
	}

	windowSeconds := windowEnd.Sub(windowStart).Seconds()
	if windowSeconds <= 0 {
		windowSeconds = 60
	}
	requestTotal := successCount + errorTotal
	qps := float64(requestTotal) / windowSeconds
	tps := float64(tokenConsumed) / windowSeconds

	goroutines := runtime.NumGoroutine()
	concurrencyQueueDepth := c.collectConcurrencyQueueDepth(ctx)

	input := &OpsInsertSystemMetricsInput{
		CreatedAt:     windowEnd,
		WindowMinutes: 1,

		SuccessCount:         successCount,
		ErrorCountTotal:      errorTotal,
		BusinessLimitedCount: businessLimited,
		ErrorCountSLA:        errorSLA,

		UpstreamErrorCountExcl429529: upstreamExcl,
		Upstream429Count:             upstream429,
		Upstream529Count:             upstream529,

		TokenConsumed:      tokenConsumed,
		AccountSwitchCount: accountSwitchCount,
		QPS:                float64Ptr(roundTo1DP(qps)),
		TPS:                float64Ptr(roundTo1DP(tps)),

		DurationP50Ms: duration.p50,
		DurationP90Ms: duration.p90,
		DurationP95Ms: duration.p95,
		DurationP99Ms: duration.p99,
		DurationAvgMs: duration.avg,
		DurationMaxMs: duration.max,

		TTFTP50Ms: ttft.p50,
		TTFTP90Ms: ttft.p90,
		TTFTP95Ms: ttft.p95,
		TTFTP99Ms: ttft.p99,
		TTFTAvgMs: ttft.avg,
		TTFTMaxMs: ttft.max,

		CPUUsagePercent:    sys.cpuUsagePercent,
		MemoryUsedMB:       sys.memoryUsedMB,
		MemoryTotalMB:      sys.memoryTotalMB,
		MemoryUsagePercent: sys.memoryUsagePercent,
		DiskUsagePercent:   sys.diskUsagePercent,

		DBOK:    boolPtr(dbOK),
		RedisOK: boolPtr(redisOK),

		RedisConnTotal: func() *int {
			if !redisStatsOK {
				return nil
			}
			return intPtr(redisTotal)
		}(),
		RedisConnIdle: func() *int {
			if !redisStatsOK {
				return nil
			}
			return intPtr(redisIdle)
		}(),

		DBConnActive:          intPtr(active),
		DBConnIdle:            intPtr(idle),
		GoroutineCount:        intPtr(goroutines),
		ConcurrencyQueueDepth: concurrencyQueueDepth,
	}

	return c.opsRepo.InsertSystemMetrics(ctx, input)
}

func (c *OpsMetricsCollector) collectConcurrencyQueueDepth(parentCtx context.Context) *int {
	if c == nil || c.accountRepo == nil || c.concurrencyService == nil {
		return nil
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	// Best-effort: never let concurrency sampling break the metrics collector.
	ctx, cancel := context.WithTimeout(parentCtx, 2*time.Second)
	defer cancel()

	accountLoads, err := c.listSchedulableAccountLoads(ctx)
	if err != nil {
		return nil
	}
	if len(accountLoads) == 0 {
		zero := 0
		return &zero
	}

	loadMap, err := c.concurrencyService.GetAccountsLoadBatch(ctx, accountLoads)
	if err != nil {
		return nil
	}

	var total int64
	for _, info := range loadMap {
		if info == nil || info.WaitingCount <= 0 {
			continue
		}
		total += int64(info.WaitingCount)
	}
	if total < 0 {
		total = 0
	}

	maxInt := int64(^uint(0) >> 1)
	if total > maxInt {
		total = maxInt
	}
	v := int(total)
	return &v
}

func (c *OpsMetricsCollector) listSchedulableAccountLoads(ctx context.Context) ([]AccountWithConcurrency, error) {
	if repo, ok := c.accountRepo.(opsSchedulableAccountLoadRepository); ok {
		return repo.ListSchedulableAccountLoads(ctx)
	}

	accounts, err := c.accountRepo.ListSchedulable(ctx)
	if err != nil {
		return nil, err
	}
	loads := make([]AccountWithConcurrency, 0, len(accounts))
	for _, account := range accounts {
		if account.ID <= 0 {
			continue
		}
		loads = append(loads, AccountWithConcurrency{
			ID:             account.ID,
			MaxConcurrency: account.EffectiveLoadFactor(),
		})
	}
	return loads, nil
}

type opsCollectedPercentiles struct {
	p50 *int
	p90 *int
	p95 *int
	p99 *int
	avg *float64
	max *int
}

func (c *OpsMetricsCollector) queryUsageCounts(ctx context.Context, start, end time.Time) (successCount int64, tokenConsumed int64, err error) {
	q := `
SELECT
  COALESCE(COUNT(*), 0) AS success_count,
  COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens), 0) AS token_consumed
FROM usage_logs
WHERE created_at >= $1 AND created_at < $2`

	var tokens sql.NullInt64
	if err := c.db.QueryRowContext(ctx, q, start, end).Scan(&successCount, &tokens); err != nil {
		return 0, 0, err
	}
	if tokens.Valid {
		tokenConsumed = tokens.Int64
	}
	return successCount, tokenConsumed, nil
}

func (c *OpsMetricsCollector) queryUsageLatency(ctx context.Context, start, end time.Time) (duration opsCollectedPercentiles, ttft opsCollectedPercentiles, err error) {
	{
		q := `
SELECT
  percentile_cont(0.50) WITHIN GROUP (ORDER BY duration_ms) AS p50,
  percentile_cont(0.90) WITHIN GROUP (ORDER BY duration_ms) AS p90,
  percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) AS p95,
  percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms) AS p99,
  AVG(duration_ms) AS avg_ms,
  MAX(duration_ms) AS max_ms
FROM usage_logs
WHERE created_at >= $1 AND created_at < $2
  AND duration_ms IS NOT NULL`

		var p50, p90, p95, p99 sql.NullFloat64
		var avg sql.NullFloat64
		var max sql.NullInt64
		if err := c.db.QueryRowContext(ctx, q, start, end).Scan(&p50, &p90, &p95, &p99, &avg, &max); err != nil {
			return opsCollectedPercentiles{}, opsCollectedPercentiles{}, err
		}
		duration.p50 = floatToIntPtr(p50)
		duration.p90 = floatToIntPtr(p90)
		duration.p95 = floatToIntPtr(p95)
		duration.p99 = floatToIntPtr(p99)
		if avg.Valid {
			v := roundTo1DP(avg.Float64)
			duration.avg = &v
		}
		if max.Valid {
			v := int(max.Int64)
			duration.max = &v
		}
	}

	{
		q := `
SELECT
  percentile_cont(0.50) WITHIN GROUP (ORDER BY first_token_ms) AS p50,
  percentile_cont(0.90) WITHIN GROUP (ORDER BY first_token_ms) AS p90,
  percentile_cont(0.95) WITHIN GROUP (ORDER BY first_token_ms) AS p95,
  percentile_cont(0.99) WITHIN GROUP (ORDER BY first_token_ms) AS p99,
  AVG(first_token_ms) AS avg_ms,
  MAX(first_token_ms) AS max_ms
FROM usage_logs
WHERE created_at >= $1 AND created_at < $2
  AND first_token_ms IS NOT NULL`

		var p50, p90, p95, p99 sql.NullFloat64
		var avg sql.NullFloat64
		var max sql.NullInt64
		if err := c.db.QueryRowContext(ctx, q, start, end).Scan(&p50, &p90, &p95, &p99, &avg, &max); err != nil {
			return opsCollectedPercentiles{}, opsCollectedPercentiles{}, err
		}
		ttft.p50 = floatToIntPtr(p50)
		ttft.p90 = floatToIntPtr(p90)
		ttft.p95 = floatToIntPtr(p95)
		ttft.p99 = floatToIntPtr(p99)
		if avg.Valid {
			v := roundTo1DP(avg.Float64)
			ttft.avg = &v
		}
		if max.Valid {
			v := int(max.Int64)
			ttft.max = &v
		}
	}

	return duration, ttft, nil
}

func (c *OpsMetricsCollector) queryErrorCounts(ctx context.Context, start, end time.Time) (
	errorTotal int64,
	businessLimited int64,
	errorSLA int64,
	upstreamExcl429529 int64,
	upstream429 int64,
	upstream529 int64,
	err error,
) {
	// 上游三分子走共享谓词（见文件头 OpsUpstreamError*FilterSQLTemplate）：必须带
	// `status_code >= 400` 最终状态门控，否则 recovered 行（上游失败但最终成功）
	// 会被算成上游错误，生产实测占该分子 54%。此处与 raw 仪表盘
	// （repository/ops_repo_dashboard.go queryErrorCounts）逐字同源。
	q := `
SELECT
  COALESCE(COUNT(*) FILTER (WHERE ` + OpsUpstreamErrorFilterFinalStatusGate("status_code") + `), 0) AS error_total,
  COALESCE(COUNT(*) FILTER (WHERE ` + OpsUpstreamErrorFilterFinalStatusGate("status_code") + ` AND is_business_limited), 0) AS business_limited,
  COALESCE(COUNT(*) FILTER (WHERE ` + OpsUpstreamErrorFilterFinalStatusGate("status_code") + ` AND NOT is_business_limited), 0) AS error_sla,
  COALESCE(COUNT(*) FILTER (WHERE ` + OpsUpstreamErrorFilterSQL("status_code") + `), 0) AS upstream_excl,
  COALESCE(COUNT(*) FILTER (WHERE ` + OpsUpstream429FilterFor("status_code") + `), 0) AS upstream_429,
  COALESCE(COUNT(*) FILTER (WHERE ` + OpsUpstream529FilterFor("status_code") + `), 0) AS upstream_529
FROM ops_error_logs
WHERE created_at >= $1 AND created_at < $2
  AND is_count_tokens = FALSE`

	if err := c.db.QueryRowContext(ctx, q, start, end).Scan(
		&errorTotal,
		&businessLimited,
		&errorSLA,
		&upstreamExcl429529,
		&upstream429,
		&upstream529,
	); err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}
	return errorTotal, businessLimited, errorSLA, upstreamExcl429529, upstream429, upstream529, nil
}

func (c *OpsMetricsCollector) queryAccountSwitchCount(ctx context.Context, start, end time.Time) (int64, error) {
	q := `
SELECT
  COALESCE(SUM(CASE
    WHEN split_part(ev->>'kind', ':', 1) IN ('failover', 'retry_exhausted_failover', 'failover_on_400') THEN 1
    ELSE 0
  END), 0) AS switch_count
FROM ops_error_logs o
CROSS JOIN LATERAL jsonb_array_elements(
  COALESCE(NULLIF(o.upstream_errors, 'null'::jsonb), '[]'::jsonb)
) AS ev
WHERE o.created_at >= $1 AND o.created_at < $2
  AND o.is_count_tokens = FALSE`

	var count int64
	if err := c.db.QueryRowContext(ctx, q, start, end).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

type opsCollectedSystemStats struct {
	cpuUsagePercent    *float64
	memoryUsedMB       *int64
	memoryTotalMB      *int64
	memoryUsagePercent *float64
	diskUsagePercent   *float64
}

func (c *OpsMetricsCollector) collectSystemStats(ctx context.Context) (*opsCollectedSystemStats, error) {
	out := &opsCollectedSystemStats{}
	if ctx == nil {
		ctx = context.Background()
	}

	sampleAt := time.Now().UTC()

	// CPU: prefer cgroup (container) metrics, falling back to host metrics when
	// cgroup CPU accounting is unavailable.
	if cpuPct := c.tryCgroupCPUPercent(sampleAt); cpuPct != nil {
		out.cpuUsagePercent = cpuPct
	}
	if out.cpuUsagePercent == nil {
		if cpuPercents, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(cpuPercents) > 0 {
			v := roundTo1DP(cpuPercents[0])
			out.cpuUsagePercent = &v
		}
	}

	// Memory: prefer cgroup (container) metrics, but only when the cgroup exposes
	// BOTH a current usage and a concrete limit (memory.max != "max"). When the
	// limit is missing or the cgroup data is otherwise incomplete, fall back
	// ENTIRELY to host metrics. Never mix a container "used" with a host "total":
	// doing so reports a misleadingly tiny percentage (e.g. 60MB / 23GB ~= 0.3%).
	cgroupUsed, cgroupTotal, cgroupOK := readCgroupMemoryBytes()
	var host *mem.VirtualMemoryStat
	if !cgroupOK || cgroupTotal == 0 {
		if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
			host = vm
		}
	}
	out.memoryUsedMB, out.memoryTotalMB, out.memoryUsagePercent = resolveMemoryStats(cgroupUsed, cgroupTotal, cgroupOK, host)

	// Disk: sample the root partition usage. Inside a container this is the
	// overlay filesystem backed by the host root partition, which is the same
	// disk that filled up during the 2026-09-26 incident.
	out.diskUsagePercent = collectDiskUsagePercent(ctx)

	return out, nil
}

// collectDiskUsagePercent samples root-partition disk usage via gopsutil.
// Best-effort: any sampling error yields nil (not persisted).
func collectDiskUsagePercent(ctx context.Context) *float64 {
	if ctx == nil {
		ctx = context.Background()
	}
	usage, err := disk.UsageWithContext(ctx, "/")
	if err != nil || usage == nil {
		return nil
	}
	return resolveDiskUsagePercent(usage.Used, usage.Total)
}

// resolveDiskUsagePercent computes used/total*100 rounded to 1 decimal place.
// Returns nil when total is unknown (0) to avoid a division by zero.
func resolveDiskUsagePercent(used, total uint64) *float64 {
	if total == 0 {
		return nil
	}
	p := roundTo1DP(float64(used) / float64(total) * 100)
	return &p
}

// resolveMemoryStats picks a single, self-consistent (used, total, percent)
// memory trio from either cgroup (container) or host metrics — never a mix of
// the two.
//
// cgroup metrics are preferred, but only when the cgroup reports BOTH a current
// usage and a concrete limit (i.e. memory.max is a number, not "max", so
// cgroupTotal > 0). If the limit is absent or the cgroup data is otherwise
// incomplete, all three values fall back to the host reading. This avoids the
// classic pitfall of dividing a container "used" by a host "total", which
// wildly understates memory usage (e.g. 60MB / 23GB ~= 0.3%).
func resolveMemoryStats(cgroupUsed, cgroupTotal uint64, cgroupOK bool, host *mem.VirtualMemoryStat) (usedMB *int64, totalMB *int64, usagePercent *float64) {
	if cgroupOK && cgroupTotal > 0 {
		u := int64(cgroupUsed / bytesPerMB)
		t := int64(cgroupTotal / bytesPerMB)
		p := roundTo1DP(float64(cgroupUsed) / float64(cgroupTotal) * 100)
		return &u, &t, &p
	}

	if host == nil {
		return nil, nil, nil
	}

	u := int64(host.Used / bytesPerMB)
	usedMB = &u
	if host.Total > 0 {
		t := int64(host.Total / bytesPerMB)
		totalMB = &t
		p := roundTo1DP(float64(host.Used) / float64(host.Total) * 100)
		usagePercent = &p
	} else {
		// Degenerate: host reported no total. Preserve gopsutil's own percentage.
		p := roundTo1DP(host.UsedPercent)
		usagePercent = &p
	}
	return usedMB, totalMB, usagePercent
}

func (c *OpsMetricsCollector) tryCgroupCPUPercent(now time.Time) *float64 {
	usageNanos, ok := readCgroupCPUUsageNanos()
	if !ok {
		return nil
	}

	// Initialize baseline sample.
	if c.lastCgroupCPUSampleAt.IsZero() {
		c.lastCgroupCPUUsageNanos = usageNanos
		c.lastCgroupCPUSampleAt = now
		return nil
	}

	elapsed := now.Sub(c.lastCgroupCPUSampleAt)
	if elapsed <= 0 {
		c.lastCgroupCPUUsageNanos = usageNanos
		c.lastCgroupCPUSampleAt = now
		return nil
	}

	prev := c.lastCgroupCPUUsageNanos
	c.lastCgroupCPUUsageNanos = usageNanos
	c.lastCgroupCPUSampleAt = now

	if usageNanos < prev {
		// Counter reset (container restarted).
		return nil
	}

	deltaUsageSec := float64(usageNanos-prev) / 1e9
	elapsedSec := elapsed.Seconds()
	if elapsedSec <= 0 {
		return nil
	}

	cores := readCgroupCPULimitCores()
	if cores <= 0 {
		// Can't reliably normalize; skip and fall back to gopsutil.
		return nil
	}

	pct := (deltaUsageSec / (elapsedSec * cores)) * 100
	if pct < 0 {
		pct = 0
	}
	// Clamp to avoid noise/jitter showing impossible values.
	if pct > 100 {
		pct = 100
	}
	v := roundTo1DP(pct)
	return &v
}

func readCgroupMemoryBytes() (usedBytes uint64, totalBytes uint64, ok bool) {
	// cgroup v2 (most common in modern containers)
	if used, ok1 := readUintFile("/sys/fs/cgroup/memory.current"); ok1 {
		usedBytes = used
		rawMax, err := os.ReadFile("/sys/fs/cgroup/memory.max")
		if err == nil {
			s := strings.TrimSpace(string(rawMax))
			if s != "" && s != "max" {
				if v, err := strconv.ParseUint(s, 10, 64); err == nil {
					totalBytes = v
				}
			}
		}
		return usedBytes, totalBytes, true
	}

	// cgroup v1 fallback
	if used, ok1 := readUintFile("/sys/fs/cgroup/memory/memory.usage_in_bytes"); ok1 {
		usedBytes = used
		if limit, ok2 := readUintFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok2 {
			// Some environments report a very large number when unlimited.
			if limit > 0 && limit < (1<<60) {
				totalBytes = limit
			}
		}
		return usedBytes, totalBytes, true
	}

	return 0, 0, false
}

func readCgroupCPUUsageNanos() (usageNanos uint64, ok bool) {
	// cgroup v2: cpu.stat has usage_usec
	if raw, err := os.ReadFile("/sys/fs/cgroup/cpu.stat"); err == nil {
		lines := strings.Split(string(raw), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			if fields[0] != "usage_usec" {
				continue
			}
			v, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				continue
			}
			return v * 1000, true
		}
	}

	// cgroup v1: cpuacct.usage is in nanoseconds
	if v, ok := readUintFile("/sys/fs/cgroup/cpuacct/cpuacct.usage"); ok {
		return v, true
	}

	return 0, false
}

func readCgroupCPULimitCores() float64 {
	// cgroup v2: cpu.max => "<quota> <period>" or "max <period>"
	if raw, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) >= 2 && fields[0] != "max" {
			quota, err1 := strconv.ParseFloat(fields[0], 64)
			period, err2 := strconv.ParseFloat(fields[1], 64)
			if err1 == nil && err2 == nil && quota > 0 && period > 0 {
				return quota / period
			}
		}
	}

	// cgroup v1: cpu.cfs_quota_us / cpu.cfs_period_us
	quota, okQuota := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	period, okPeriod := readIntFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if okQuota && okPeriod && quota > 0 && period > 0 {
		return float64(quota) / float64(period)
	}

	return 0
}

func readUintFile(path string) (uint64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func readIntFile(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (c *OpsMetricsCollector) checkDB(ctx context.Context) bool {
	if c == nil || c.db == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var one int
	if err := c.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return false
	}
	return one == 1
}

func (c *OpsMetricsCollector) checkRedis(ctx context.Context) bool {
	if c == nil || c.redisClient == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return c.redisClient.Ping(ctx).Err() == nil
}

func (c *OpsMetricsCollector) redisPoolStats() (total int, idle int, ok bool) {
	if c == nil || c.redisClient == nil {
		return 0, 0, false
	}
	stats := c.redisClient.PoolStats()
	if stats == nil {
		return 0, 0, false
	}
	return int(stats.TotalConns), int(stats.IdleConns), true
}

func (c *OpsMetricsCollector) dbPoolStats() (active int, idle int) {
	if c == nil || c.db == nil {
		return 0, 0
	}
	stats := c.db.Stats()
	return stats.InUse, stats.Idle
}

var opsMetricsCollectorReleaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func (c *OpsMetricsCollector) tryAcquireLeaderLock(ctx context.Context) (func(), bool) {
	if c == nil || c.redisClient == nil {
		return nil, true
	}
	if ctx == nil {
		ctx = context.Background()
	}

	ok, err := c.redisClient.SetNX(ctx, opsMetricsCollectorLeaderLockKey, c.instanceID, opsMetricsCollectorLeaderLockTTL).Result()
	if err != nil {
		// Prefer fail-closed to avoid stampeding the database when Redis is flaky.
		// Fallback to a DB advisory lock when Redis is present but unavailable.
		release, ok := tryAcquireDBAdvisoryLock(ctx, c.db, opsMetricsCollectorAdvisoryLockID)
		if !ok {
			c.maybeLogSkip()
			return nil, false
		}
		return release, true
	}
	if !ok {
		c.maybeLogSkip()
		return nil, false
	}

	release := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = opsMetricsCollectorReleaseScript.Run(ctx, c.redisClient, []string{opsMetricsCollectorLeaderLockKey}, c.instanceID).Result()
	}
	return release, true
}

func (c *OpsMetricsCollector) maybeLogSkip() {
	c.skipLogMu.Lock()
	defer c.skipLogMu.Unlock()

	now := time.Now()
	if !c.skipLogAt.IsZero() && now.Sub(c.skipLogAt) < time.Minute {
		return
	}
	c.skipLogAt = now
	log.Printf("[OpsMetricsCollector] leader lock held by another instance; skipping")
}

func floatToIntPtr(v sql.NullFloat64) *int {
	if !v.Valid {
		return nil
	}
	n := int(math.Round(v.Float64))
	return &n
}

func roundTo1DP(v float64) float64 {
	return math.Round(v*10) / 10
}

func truncateString(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func boolPtr(v bool) *bool {
	out := v
	return &out
}

func intPtr(v int) *int {
	out := v
	return &out
}

func float64Ptr(v float64) *float64 {
	out := v
	return &out
}
