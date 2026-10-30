package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 主力渠道延迟/故障自适应备用切换 · 感知层只读查询（第 1 片）。
//
// 只读、不新增表、不新增埋点；全部走既有索引：
//
//	usage_logs (model, created_at)      —— idx_usage_logs_model_created_at
//	ops_error_logs (created_at)         —— idx_ops_error_logs_created_at
//	accounts (platform)                 —— idx_accounts_platform
//
// 调用方（service.StandbyHealthCollector）负责限时：本文件所有方法都直接吃 ctx，
// ctx 超时即以错误返回，由采集器整轮作废判 Inconclusive。
type standbyHealthRepository struct {
	db *sql.DB
}

// NewStandbyHealthRepository 返回感知层数据源；本片不接线，由后续执行层按需注入。
func NewStandbyHealthRepository(db *sql.DB) service.StandbyHealthSource {
	return &standbyHealthRepository{db: db}
}

// standbyTTFTBaselineSQL 取与当前 hour（loc 时区）同小时段的逐日 TTFT p50，再取这些
// 日 p50 的中位数：中位数抗单日尖峰，同小时桶抗峰谷噪声（L2）。
const standbyTTFTBaselineSQL = `
WITH daily AS (
  SELECT (ul.created_at AT TIME ZONE $4)::date AS local_day,
         percentile_cont(0.50) WITHIN GROUP (ORDER BY ul.first_token_ms) AS day_p50
  FROM usage_logs ul
  WHERE ul.model = $1
    AND ul.created_at >= $2
    AND ul.created_at < $3
    AND ul.first_token_ms IS NOT NULL
    AND EXTRACT(HOUR FROM ul.created_at AT TIME ZONE $4) = $5
  GROUP BY 1
)
SELECT percentile_cont(0.50) WITHIN GROUP (ORDER BY day_p50) AS baseline_p50,
       COUNT(*) AS day_count
FROM daily
`

func (r *standbyHealthRepository) TTFTBaselineP50(ctx context.Context, model string, from, to time.Time, loc *time.Location, hour int) (*service.StandbyBaselineSample, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil standby health repository")
	}
	if loc == nil {
		loc = time.UTC
	}
	var p50 sql.NullFloat64
	var days int
	if err := r.db.QueryRowContext(ctx, standbyTTFTBaselineSQL, model, from.UTC(), to.UTC(), loc.String(), hour).Scan(&p50, &days); err != nil {
		return nil, err
	}
	if days < service.StandbyBaselineMinDays || !p50.Valid {
		return nil, nil
	}
	return &service.StandbyBaselineSample{P50Ms: p50.Float64, Days: days}, nil
}

// standbyTTFTWindowSQL 取近窗口 TTFT p50 与样本数。first_token_ms 只在流式成功请求上
// 落值，因此样本数即"有 TTFT 的请求数"。
const standbyTTFTWindowSQL = `
SELECT percentile_cont(0.50) WITHIN GROUP (ORDER BY first_token_ms) AS p50,
       COUNT(first_token_ms) AS samples
FROM usage_logs
WHERE model = $1
  AND created_at >= $2
  AND created_at < $3
  AND first_token_ms IS NOT NULL
`

func (r *standbyHealthRepository) TTFTWindowSample(ctx context.Context, model string, from, to time.Time) (*service.StandbyTTFTSample, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil standby health repository")
	}
	var p50 sql.NullFloat64
	var samples int
	if err := r.db.QueryRowContext(ctx, standbyTTFTWindowSQL, model, from.UTC(), to.UTC()).Scan(&p50, &samples); err != nil {
		return nil, err
	}
	if samples == 0 || !p50.Valid {
		return nil, nil
	}
	return &service.StandbyTTFTSample{P50Ms: p50.Float64, Samples: samples}, nil
}

// standbyThrottleStatsSQL 统计上游 429/529 错误行与窗口请求数。
//
// 分母刻意取「成功用量行 + 429/529 错误行」：recovered 行（上游先失败、重试后成功）
// 会同时产生 error 行与 usage 行，用"全部 error 行 + usage 行"会双计，用
// "usage 行 + 429/529 行"则分子是分母的子集，比例单调且不会是 0 分母。
// count_tokens 是探测性请求，不计入（与 ops 预聚合口径一致）。
const standbyThrottleStatsSQL = `
SELECT
  (SELECT COUNT(*)
     FROM ops_error_logs el
    WHERE el.model = $1
      AND el.created_at >= $2
      AND el.created_at < $3
      AND el.is_count_tokens = FALSE
      AND COALESCE(el.upstream_status_code, 0) IN (429, 529)) AS throttle_count,
  (SELECT COUNT(*)
     FROM usage_logs ul
    WHERE ul.model = $1
      AND ul.created_at >= $2
      AND ul.created_at < $3) AS usage_count
`

func (r *standbyHealthRepository) UpstreamThrottleStats(ctx context.Context, model string, from, to time.Time) (*service.StandbyThrottleStats, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil standby health repository")
	}
	var throttle, usage int
	if err := r.db.QueryRowContext(ctx, standbyThrottleStatsSQL, model, from.UTC(), to.UTC()).Scan(&throttle, &usage); err != nil {
		return nil, err
	}
	return &service.StandbyThrottleStats{ThrottleCount: throttle, RequestCount: usage + throttle}, nil
}

// standbyAccountsSQL 只取 IsSchedulable() 判定所需字段 + extra（配额维度），
// 判定本身仍在 service 侧复用 Account.IsSchedulable，避免在 SQL 里复制一套资格逻辑。
const standbyAccountsSQL = `
SELECT id, platform, type, status, schedulable, expires_at, auto_pause_on_expired,
       rate_limit_reset_at, overload_until, temp_unschedulable_until,
       COALESCE(extra, '{}'::jsonb) AS extra
FROM accounts
WHERE platform = $1
  AND deleted_at IS NULL
`

func (r *standbyHealthRepository) ListAccountsForStandby(ctx context.Context, platform string) ([]service.Account, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil standby health repository")
	}
	rows, err := r.db.QueryContext(ctx, standbyAccountsSQL, platform)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	accounts := make([]service.Account, 0, 16)
	for rows.Next() {
		var (
			acc         service.Account
			expiresAt   sql.NullTime
			rateLimitAt sql.NullTime
			overload    sql.NullTime
			tempUnsched sql.NullTime
			extraRaw    []byte
		)
		if err := rows.Scan(
			&acc.ID, &acc.Platform, &acc.Type, &acc.Status, &acc.Schedulable,
			&expiresAt, &acc.AutoPauseOnExpired,
			&rateLimitAt, &overload, &tempUnsched, &extraRaw,
		); err != nil {
			return nil, err
		}
		if expiresAt.Valid {
			t := expiresAt.Time
			acc.ExpiresAt = &t
		}
		if rateLimitAt.Valid {
			t := rateLimitAt.Time
			acc.RateLimitResetAt = &t
		}
		if overload.Valid {
			t := overload.Time
			acc.OverloadUntil = &t
		}
		if tempUnsched.Valid {
			t := tempUnsched.Time
			acc.TempUnschedulableUntil = &t
		}
		if len(extraRaw) > 0 {
			var extra map[string]any
			if err := json.Unmarshal(extraRaw, &extra); err != nil {
				return nil, fmt.Errorf("decode account extra (id=%d): %w", acc.ID, err)
			}
			acc.Extra = extra
		}
		accounts = append(accounts, acc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return accounts, nil
}
