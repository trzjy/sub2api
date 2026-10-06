package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *usageLogRepository) getAllGroupUsageSummaryFromRollups(ctx context.Context, todayStart time.Time) (results []usagestats.GroupUsageSummary, err error) {
	todayStart = service.GroupUsageTodayStart(todayStart)
	yesterdayStart := service.GroupUsageYesterdayStart(todayStart)
	timezoneName := service.GroupUsageTimezoneName()
	todayDate := service.GroupUsageDate(todayStart)
	yesterdayDate := service.GroupUsageDate(yesterdayStart)

	const query = `
		WITH state_values AS (
			SELECT
				COUNT(*) = 1
					AND MAX(timezone_name) = $3
					AND MAX(closed_before) <= $4::date AS valid,
				MAX(closed_before) AS closed_before,
				MAX(retained_from) AS retained_from
			FROM usage_group_rollup_state
			WHERE id = 1
		),
		state AS (
			SELECT
				CASE WHEN valid THEN closed_before ELSE DATE '1970-01-01' END AS closed_before,
				CASE WHEN valid THEN retained_from ELSE TIMESTAMPTZ '1970-01-01 00:00:00+00' END AS retained_from,
				CASE
					WHEN valid THEN closed_before::timestamp AT TIME ZONE $3::text
					ELSE TIMESTAMPTZ '1970-01-01 00:00:00+00'
				END AS tail_start,
				valid
			FROM state_values
		),
		historical AS (
			SELECT
				rollup.group_id,
				COALESCE(SUM(rollup.actual_cost), 0) AS actual_cost,
				COALESCE(SUM(rollup.actual_cost) FILTER (
					WHERE rollup.bucket_date = $5::date
				), 0) AS yesterday_cost
			FROM usage_group_daily_rollups rollup
			CROSS JOIN state
			WHERE state.valid
				AND rollup.bucket_date >= (state.retained_from AT TIME ZONE $3::text)::date
				AND rollup.bucket_date < state.closed_before
			GROUP BY rollup.group_id
		),
		tail AS (
			SELECT
				ul.group_id,
				COALESCE(SUM(ul.actual_cost), 0) AS actual_cost,
				COALESCE(SUM(ul.actual_cost) FILTER (WHERE ul.created_at >= $1), 0) AS today_cost,
				COALESCE(SUM(ul.actual_cost) FILTER (
					WHERE ul.created_at >= $2
						AND ul.created_at < $1
				), 0) AS yesterday_cost
			FROM usage_logs ul
			CROSS JOIN state
			WHERE ul.created_at >= state.tail_start
			GROUP BY ul.group_id
		)
		SELECT
			g.id AS group_id,
			COALESCE(historical.actual_cost, 0) + COALESCE(tail.actual_cost, 0) AS total_cost,
			COALESCE(tail.today_cost, 0) AS today_cost,
			COALESCE(historical.yesterday_cost, 0) + COALESCE(tail.yesterday_cost, 0) AS yesterday_cost
		FROM groups g
		LEFT JOIN historical ON historical.group_id = g.id
		LEFT JOIN tail ON tail.group_id = g.id
		ORDER BY g.id
	`

	rows, err := r.sql.QueryContext(
		ctx,
		query,
		todayStart,
		yesterdayStart,
		timezoneName,
		todayDate,
		yesterdayDate,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
			results = nil
		}
	}()

	results = make([]usagestats.GroupUsageSummary, 0)
	for rows.Next() {
		var row usagestats.GroupUsageSummary
		if err := rows.Scan(&row.GroupID, &row.TotalCost, &row.TodayCost, &row.YesterdayCost); err != nil {
			return nil, err
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// SyncGroupUsageRollups 将服务端配置时区今日以前的用量发布为分组日桶。
// 公共入口签名与既有行为不变：exec 为 *sql.DB 时自行开启独立事务；其余 executor
// （如事务路径复用的 *sql.Tx）直接委托到 syncGroupUsageRollupsOnExecutor。
func (r *dashboardAggregationRepository) SyncGroupUsageRollups(ctx context.Context, todayStart time.Time) error {
	if r == nil || r.sql == nil {
		return nil
	}
	todayStart = service.GroupUsageTodayStart(todayStart)
	if db, ok := r.sql.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := r.syncGroupUsageRollupsOnExecutor(ctx, tx, todayStart); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return r.syncGroupUsageRollupsOnExecutor(ctx, r.sql, todayStart)
}

// syncGroupUsageRollupsOnExecutor 是 SyncGroupUsageRollups 的 SQL 主体，执行器参数化：
// exec 为 *sql.Tx 时与调用方事务同连接、同可见性、同原子性（清理保留期场景下复用同一事务，
// 避免基于未提交删除之外的旧日志同步出错误汇总，也避免汇总状态行锁等待/超时）；
// exec 为 *sql.DB 时退回独立事务语义。失败由调用方整体回滚，不得降级跳过。
func (r *dashboardAggregationRepository) syncGroupUsageRollupsOnExecutor(ctx context.Context, exec sqlExecutor, todayStart time.Time) error {
	todayStart = service.GroupUsageTodayStart(todayStart)
	var closedBefore string
	var previousRetainedFrom time.Time
	var stateTimezoneName string
	if err := scanSingleRow(ctx, exec, `
		SELECT closed_before::text, retained_from, timezone_name
		FROM usage_group_rollup_state
		WHERE id = 1
		FOR UPDATE
	`, nil, &closedBefore, &previousRetainedFrom, &stateTimezoneName); err != nil {
		return fmt.Errorf("读取分组用量汇总水位: %w", err)
	}

	todayDate := service.GroupUsageDate(todayStart)
	timezoneName := service.GroupUsageTimezoneName()
	timezoneChanged := stateTimezoneName != timezoneName
	var closedTime time.Time
	if !timezoneChanged {
		var err error
		closedTime, err = service.ParseGroupUsageDate(closedBefore)
		if err != nil {
			return fmt.Errorf("解析分组用量汇总水位 %q: %w", closedBefore, err)
		}
		todayDateTime, err := service.ParseGroupUsageDate(todayDate)
		if err != nil {
			return err
		}
		if closedTime.After(todayDateTime) {
			return fmt.Errorf("分组用量汇总水位位于未来: %s", closedBefore)
		}
		if closedBefore == todayDate {
			return nil
		}
	}

	var earliest sql.NullTime
	if err := scanSingleRow(ctx, exec, "SELECT MIN(created_at) FROM usage_logs", nil, &earliest); err != nil {
		return fmt.Errorf("读取最早用量记录: %w", err)
	}
	retainedFrom := todayStart
	if earliest.Valid {
		retainedFrom = earliest.Time.UTC()
	}
	retainedDate := service.GroupUsageDate(retainedFrom)
	retainedDateTime, err := service.ParseGroupUsageDate(retainedDate)
	if err != nil {
		return err
	}
	rebuildStartDate := retainedDate
	if !timezoneChanged && closedTime.After(retainedDateTime) {
		rebuildStartDate = closedBefore
	}
	rebuildStart, err := service.ParseGroupUsageDate(rebuildStartDate)
	if err != nil {
		return err
	}

	if _, err := exec.ExecContext(ctx, `
		DELETE FROM usage_group_daily_rollups
		WHERE bucket_date < $1::date
			OR (bucket_date >= $2::date AND bucket_date < $3::date)
			OR bucket_date >= $3::date
	`, retainedDate, rebuildStartDate, todayDate); err != nil {
		return fmt.Errorf("清理分组用量日桶: %w", err)
	}

	if _, err := exec.ExecContext(ctx, `
		INSERT INTO usage_group_daily_rollups (bucket_date, group_id, actual_cost, computed_at)
		SELECT
			(created_at AT TIME ZONE $3::text)::date AS bucket_date,
			group_id,
			COALESCE(SUM(actual_cost), 0) AS actual_cost,
			NOW()
		FROM usage_logs
		WHERE group_id IS NOT NULL
			AND created_at >= $1
			AND created_at < $2
		GROUP BY 1, 2
		ON CONFLICT (bucket_date, group_id)
		DO UPDATE SET
			actual_cost = EXCLUDED.actual_cost,
			computed_at = EXCLUDED.computed_at
	`, rebuildStart.UTC(), todayStart.UTC(), timezoneName); err != nil {
		return fmt.Errorf("重建分组用量日桶: %w", err)
	}

	if _, err := exec.ExecContext(ctx, `
		UPDATE usage_group_rollup_state
		SET closed_before = $1::date,
			retained_from = $2,
			timezone_name = $3,
			updated_at = NOW()
		WHERE id = 1
	`, todayDate, retainedFrom, timezoneName); err != nil {
		return fmt.Errorf("更新分组用量汇总水位: %w", err)
	}
	return nil
}

// syncGroupUsageRollupsInTx 是同步分组用量汇总的既有时事务收尾入口（RecomputeRange 等复用），
// 委托到执行器参数化实现，保持调用点零改动。
func (r *dashboardAggregationRepository) syncGroupUsageRollupsInTx(ctx context.Context, todayStart time.Time) error {
	return r.syncGroupUsageRollupsOnExecutor(ctx, r.sql, todayStart)
}

func lockGroupUsageRollupState(ctx context.Context, tx *sql.Tx) error {
	var id int16
	if err := tx.QueryRowContext(ctx, `
		SELECT id
		FROM usage_group_rollup_state
		WHERE id = 1
		FOR UPDATE
	`).Scan(&id); err != nil {
		return fmt.Errorf("锁定分组用量汇总水位: %w", err)
	}
	return nil
}

func invalidateGroupUsageRollupsAt(ctx context.Context, tx *sql.Tx, affectedAt time.Time) error {
	timezoneName := service.GroupUsageTimezoneName()
	_, err := tx.ExecContext(ctx, `
		UPDATE usage_group_rollup_state
		SET closed_before = LEAST(
			closed_before,
			($1::timestamptz AT TIME ZONE $2::text)::date
		),
			updated_at = NOW()
		WHERE id = 1
	`, affectedAt.UTC(), timezoneName)
	return err
}
