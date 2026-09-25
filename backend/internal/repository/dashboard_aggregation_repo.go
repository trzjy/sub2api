package repository

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type dashboardAggregationRepository struct {
	sql   sqlExecutor
	clock func() time.Time
}

const usageLogsCleanupBatchSize = 10000
const usageBillingDedupCleanupBatchSize = 10000

// NewDashboardAggregationRepository 创建仪表盘预聚合仓储。
func NewDashboardAggregationRepository(sqlDB *sql.DB) service.DashboardAggregationRepository {
	if sqlDB == nil {
		return nil
	}
	if !isPostgresDriver(sqlDB) {
		log.Printf("[DashboardAggregation] 检测到非 PostgreSQL 驱动，已自动禁用预聚合")
		return nil
	}
	return newDashboardAggregationRepositoryWithSQL(sqlDB)
}

func newDashboardAggregationRepositoryWithSQL(sqlq sqlExecutor) *dashboardAggregationRepository {
	return &dashboardAggregationRepository{sql: sqlq, clock: time.Now}
}

func (r *dashboardAggregationRepository) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

func isPostgresDriver(db *sql.DB) bool {
	if db == nil {
		return false
	}
	_, ok := db.Driver().(*pq.Driver)
	return ok
}

func (r *dashboardAggregationRepository) AggregateRange(ctx context.Context, start, end time.Time) error {
	if r == nil || r.sql == nil {
		return nil
	}
	loc := timezone.Location()
	startLocal := start.In(loc)
	endLocal := end.In(loc)
	if !endLocal.After(startLocal) {
		return nil
	}

	hourStart := startLocal.Truncate(time.Hour)
	hourEnd := endLocal.Truncate(time.Hour)
	if endLocal.After(hourEnd) {
		hourEnd = hourEnd.Add(time.Hour)
	}

	dayStart := truncateToDay(startLocal)
	dayEnd := truncateToDay(endLocal)
	if endLocal.After(dayEnd) {
		dayEnd = dayEnd.Add(24 * time.Hour)
	}

	if db, ok := r.sql.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		txRepo := newDashboardAggregationRepositoryWithSQL(tx)
		if err := txRepo.aggregateRangeInTx(ctx, hourStart, hourEnd, dayStart, dayEnd); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return r.aggregateRangeInTx(ctx, hourStart, hourEnd, dayStart, dayEnd)
}

func (r *dashboardAggregationRepository) aggregateRangeInTx(ctx context.Context, hourStart, hourEnd, dayStart, dayEnd time.Time) error {
	// 以桶边界聚合，允许覆盖 end 所在桶的剩余区间。
	if err := r.insertHourlyActiveUsers(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.insertDailyActiveUsers(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.upsertHourlyAggregates(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.upsertDailyAggregates(ctx, dayStart, dayEnd); err != nil {
		return err
	}
	return nil
}

func (r *dashboardAggregationRepository) RecomputeRange(ctx context.Context, start, end time.Time) error {
	if r == nil || r.sql == nil {
		return nil
	}
	loc := timezone.Location()
	startLocal := start.In(loc)
	endLocal := end.In(loc)
	if !endLocal.After(startLocal) {
		return nil
	}

	hourStart := startLocal.Truncate(time.Hour)
	hourEnd := endLocal.Truncate(time.Hour)
	if endLocal.After(hourEnd) {
		hourEnd = hourEnd.Add(time.Hour)
	}

	dayStart := truncateToDay(startLocal)
	dayEnd := truncateToDay(endLocal)
	if endLocal.After(dayEnd) {
		dayEnd = dayEnd.Add(24 * time.Hour)
	}

	// 尽量使用事务保证范围内的一致性（允许在非 *sql.DB 的情况下退化为非事务执行）。
	if db, ok := r.sql.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := lockGroupUsageRollupState(ctx, tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := invalidateGroupUsageRollupsAt(ctx, tx, start); err != nil {
			_ = tx.Rollback()
			return err
		}
		txRepo := newDashboardAggregationRepositoryWithSQL(tx)
		if err := txRepo.recomputeRangeInTx(ctx, hourStart, hourEnd, dayStart, dayEnd); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := txRepo.syncGroupUsageRollupsInTx(ctx, service.GroupUsageTodayStart(r.now())); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}
	return r.recomputeRangeInTx(ctx, hourStart, hourEnd, dayStart, dayEnd)
}

func (r *dashboardAggregationRepository) recomputeRangeInTx(ctx context.Context, hourStart, hourEnd, dayStart, dayEnd time.Time) error {
	// 先清空范围内桶，再重建（避免仅增量插入导致活跃用户等指标无法回退）。
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_hourly WHERE bucket_start >= $1 AND bucket_start < $2", hourStart, hourEnd); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_hourly_users WHERE bucket_start >= $1 AND bucket_start < $2", hourStart, hourEnd); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_daily WHERE bucket_date >= $1::date AND bucket_date < $2::date", dayStart, dayEnd); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_daily_users WHERE bucket_date >= $1::date AND bucket_date < $2::date", dayStart, dayEnd); err != nil {
		return err
	}

	if err := r.insertHourlyActiveUsers(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.insertDailyActiveUsers(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.upsertHourlyAggregates(ctx, hourStart, hourEnd); err != nil {
		return err
	}
	if err := r.upsertDailyAggregates(ctx, dayStart, dayEnd); err != nil {
		return err
	}
	return nil
}

func (r *dashboardAggregationRepository) GetAggregationWatermark(ctx context.Context) (time.Time, error) {
	var ts time.Time
	query := "SELECT last_aggregated_at FROM usage_dashboard_aggregation_watermark WHERE id = 1"
	if err := scanSingleRow(ctx, r.sql, query, nil, &ts); err != nil {
		if err == sql.ErrNoRows {
			return time.Unix(0, 0).UTC(), nil
		}
		return time.Time{}, err
	}
	return ts.UTC(), nil
}

func (r *dashboardAggregationRepository) UpdateAggregationWatermark(ctx context.Context, aggregatedAt time.Time) error {
	query := `
		INSERT INTO usage_dashboard_aggregation_watermark (id, last_aggregated_at, updated_at)
		VALUES (1, $1, NOW())
		ON CONFLICT (id)
		DO UPDATE SET last_aggregated_at = EXCLUDED.last_aggregated_at, updated_at = EXCLUDED.updated_at
	`
	_, err := r.sql.ExecContext(ctx, query, aggregatedAt.UTC())
	return err
}

func (r *dashboardAggregationRepository) CleanupAggregates(ctx context.Context, hourlyCutoff, dailyCutoff time.Time) error {
	hourlyCutoffUTC := hourlyCutoff.UTC()
	dailyCutoffUTC := dailyCutoff.UTC()
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_hourly WHERE bucket_start < $1", hourlyCutoffUTC); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_hourly_users WHERE bucket_start < $1", hourlyCutoffUTC); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_daily WHERE bucket_date < $1::date", dailyCutoffUTC); err != nil {
		return err
	}
	if _, err := r.sql.ExecContext(ctx, "DELETE FROM usage_dashboard_daily_users WHERE bucket_date < $1::date", dailyCutoffUTC); err != nil {
		return err
	}
	return nil
}

// CleanupUsageLogs 是既有保留清理入口（截止点计算与行为语义不变）。
// 删除段走 r.sql（独立的 DB 执行），不在任何外部事务内。
func (r *dashboardAggregationRepository) CleanupUsageLogs(ctx context.Context, cutoff time.Time) error {
	return r.cleanupUsageLogsOnExecutor(ctx, r.sql, cutoff)
}

// CleanupUsageLogsTx 是 CleanupUsageLogs 的事务参数变体：删除段在调用方传入的
// *sql.Tx 上执行，使源日志清理能与风险表清理（U3）纳入同一 DB 事务（方案 §6.4/§5）。
// 截止点计算与既有行为语义不变，仅事务边界被调用方接管。
func (r *dashboardAggregationRepository) CleanupUsageLogsTx(ctx context.Context, tx *sql.Tx, cutoff time.Time) error {
	return r.cleanupUsageLogsOnExecutor(ctx, tx, cutoff)
}

// cleanupUsageLogsOnExecutor 是 CleanupUsageLogs / CleanupUsageLogsTx 的统一实现。
// exec 为 *sql.DB 时退回既有独立执行语义；exec 为 *sql.Tx 时删除段在外部事务内执行，
// 失败由调用方回滚（保留期协调场景）。截止点计算与既有行为语义不变。
func (r *dashboardAggregationRepository) cleanupUsageLogsOnExecutor(ctx context.Context, exec sqlExecutor, cutoff time.Time) error {
	// 事务路径（exec 为 *sql.Tx）的探测/枚举/删除/汇总必须全部在同一事务连接上——
	// 绕池既破坏「同连接」不变量（探测结果对事务未提交数据不可见虽无影响，但破坏原子性语义），
	// 又在外借池连接时与事务互锁（MaxOpenConns=1 时事务占住连接后等第二条直至超时死锁）。
	isPartitioned, err := r.isUsageLogsPartitioned(ctx, exec)
	if err != nil {
		return err
	}
	if isPartitioned {
		if err := r.dropUsageLogsPartitionsOnExecutor(ctx, exec, cutoff); err != nil {
			return err
		}
	} else if err := r.cleanupUsageLogsBatchesOnExecutor(ctx, exec, cutoff); err != nil {
		return err
	}
	// 收尾的分组汇总同步复用同一执行器。事务路径（exec 为 *sql.Tx）直接在同一事务内同步，
	// 与删除段同连接、同可见性、同原子性（事务内删除未提交时池连接看不到，且汇总状态行锁
	// 被同事务占用）；同步失败随事务回滚，不得降级为跳过或改走池连接。
	// 非事务路径（exec 为 *sql.DB 或通用执行器）退回既有独立事务语义
	// （公共 SyncGroupUsageRollups 自行开启事务），保持既有行为不变（R5-5 已验收）。
	if _, ok := exec.(*sql.Tx); ok {
		return r.syncGroupUsageRollupsOnExecutor(ctx, exec, service.GroupUsageTodayStart(r.now()))
	}
	return r.SyncGroupUsageRollups(ctx, service.GroupUsageTodayStart(r.now()))
}

func (r *dashboardAggregationRepository) cleanupUsageLogsBatches(ctx context.Context, cutoff time.Time) error {
	db, transactional := r.sql.(*sql.DB)
	for {
		if transactional {
			affected, err := cleanupUsageLogsBatchWithRollupInvalidation(ctx, db, cutoff)
			if err != nil {
				return err
			}
			if affected < usageLogsCleanupBatchSize {
				return nil
			}
			continue
		}

		res, err := r.sql.ExecContext(ctx, `
			WITH victims AS (
				SELECT ctid
				FROM usage_logs
				WHERE created_at < $1
				ORDER BY created_at ASC, id ASC
				LIMIT $2
			)
			DELETE FROM usage_logs
			WHERE ctid IN (SELECT ctid FROM victims)
		`, cutoff.UTC(), usageLogsCleanupBatchSize)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected < usageLogsCleanupBatchSize {
			return nil
		}
	}
}

// cleanupUsageLogsBatchesOnExecutor 在调用方提供的 executor 上执行源日志分批删除。
// 当 exec 为 *sql.Tx 时复用 cleanupUsageLogsBatchesInTx（删除段在外部事务内，并就地做
// 分组用量汇总失效，与既有 DB 路径语义一致）；其余 executor 退回既有 cleanupUsageLogsBatches。
func (r *dashboardAggregationRepository) cleanupUsageLogsBatchesOnExecutor(ctx context.Context, exec sqlExecutor, cutoff time.Time) error {
	if tx, ok := exec.(*sql.Tx); ok {
		return r.cleanupUsageLogsBatchesInTx(ctx, tx, cutoff)
	}
	return r.cleanupUsageLogsBatches(ctx, cutoff)
}

// cleanupUsageLogsBatchesInTx 在外部 *sql.Tx 内分批删除过期 usage_logs，并在每批内
// 锁定并失效分组用量汇总（与 cleanupUsageLogsBatchWithRollupInvalidation 的语义一致，
// 但复用调用方的同一事务，保证与风险表清理原子提交/回滚）。
func (r *dashboardAggregationRepository) cleanupUsageLogsBatchesInTx(ctx context.Context, tx *sql.Tx, cutoff time.Time) error {
	for {
		if err := lockGroupUsageRollupState(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			WITH victims AS (
				SELECT ctid
				FROM usage_logs
				WHERE created_at < $1
				ORDER BY created_at ASC, id ASC
				LIMIT $2
			)
			DELETE FROM usage_logs
			WHERE ctid IN (SELECT ctid FROM victims)
			RETURNING created_at
		`, cutoff.UTC(), usageLogsCleanupBatchSize)
		if err != nil {
			return err
		}
		var affected int64
		var earliestDeletedAt time.Time
		for rows.Next() {
			var deletedAt time.Time
			if err := rows.Scan(&deletedAt); err != nil {
				_ = rows.Close()
				return err
			}
			affected++
			if earliestDeletedAt.IsZero() || deletedAt.Before(earliestDeletedAt) {
				earliestDeletedAt = deletedAt
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if affected > 0 {
			if err := invalidateGroupUsageRollupsAt(ctx, tx, earliestDeletedAt); err != nil {
				return err
			}
		}
		if affected < usageLogsCleanupBatchSize {
			return nil
		}
	}
}

func cleanupUsageLogsBatchWithRollupInvalidation(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	rollback := func(err error) (int64, error) {
		_ = tx.Rollback()
		return 0, err
	}

	if err := lockGroupUsageRollupState(ctx, tx); err != nil {
		return rollback(err)
	}
	rows, err := tx.QueryContext(ctx, `
		WITH victims AS (
			SELECT ctid
			FROM usage_logs
			WHERE created_at < $1
			ORDER BY created_at ASC, id ASC
			LIMIT $2
		)
		DELETE FROM usage_logs
		WHERE ctid IN (SELECT ctid FROM victims)
		RETURNING created_at
	`, cutoff.UTC(), usageLogsCleanupBatchSize)
	if err != nil {
		return rollback(err)
	}

	var affected int64
	var earliestDeletedAt time.Time
	for rows.Next() {
		var deletedAt time.Time
		if err := rows.Scan(&deletedAt); err != nil {
			_ = rows.Close()
			return rollback(err)
		}
		affected++
		if earliestDeletedAt.IsZero() || deletedAt.Before(earliestDeletedAt) {
			earliestDeletedAt = deletedAt
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return rollback(err)
	}
	if err := rows.Close(); err != nil {
		return rollback(err)
	}
	if affected > 0 {
		if err := invalidateGroupUsageRollupsAt(ctx, tx, earliestDeletedAt); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return affected, nil
}

func (r *dashboardAggregationRepository) CleanupUsageBillingDedup(ctx context.Context, cutoff time.Time) error {
	for {
		res, err := r.sql.ExecContext(ctx, `
			WITH victims AS (
				SELECT ctid, request_id, api_key_id, request_fingerprint, created_at
				FROM usage_billing_dedup
				WHERE created_at < $1
				LIMIT $2
			), archived AS (
				INSERT INTO usage_billing_dedup_archive (request_id, api_key_id, request_fingerprint, created_at)
				SELECT request_id, api_key_id, request_fingerprint, created_at
				FROM victims
				ON CONFLICT (request_id, api_key_id) DO NOTHING
			)
			DELETE FROM usage_billing_dedup
			WHERE ctid IN (SELECT ctid FROM victims)
		`, cutoff.UTC(), usageBillingDedupCleanupBatchSize)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected < usageBillingDedupCleanupBatchSize {
			return nil
		}
	}
}

func (r *dashboardAggregationRepository) EnsureUsageLogsPartitions(ctx context.Context, now time.Time) error {
	isPartitioned, err := r.isUsageLogsPartitioned(ctx, r.sql)
	if err != nil || !isPartitioned {
		return err
	}
	monthStart := truncateToMonthUTC(now)
	prevMonth := monthStart.AddDate(0, -1, 0)
	nextMonth := monthStart.AddDate(0, 1, 0)

	for _, m := range []time.Time{prevMonth, monthStart, nextMonth} {
		if err := r.createUsageLogsPartition(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (r *dashboardAggregationRepository) insertHourlyActiveUsers(ctx context.Context, start, end time.Time) error {
	tzName := timezone.Name()
	query := `
		INSERT INTO usage_dashboard_hourly_users (bucket_start, user_id)
		SELECT DISTINCT
			date_trunc('hour', created_at AT TIME ZONE $3) AT TIME ZONE $3 AS bucket_start,
			user_id
		FROM usage_logs
		WHERE created_at >= $1 AND created_at < $2
		ON CONFLICT DO NOTHING
	`
	_, err := r.sql.ExecContext(ctx, query, start, end, tzName)
	return err
}

func (r *dashboardAggregationRepository) insertDailyActiveUsers(ctx context.Context, start, end time.Time) error {
	tzName := timezone.Name()
	query := `
		INSERT INTO usage_dashboard_daily_users (bucket_date, user_id)
		SELECT DISTINCT
			(bucket_start AT TIME ZONE $3)::date AS bucket_date,
			user_id
		FROM usage_dashboard_hourly_users
		WHERE bucket_start >= $1 AND bucket_start < $2
		ON CONFLICT DO NOTHING
	`
	_, err := r.sql.ExecContext(ctx, query, start, end, tzName)
	return err
}

func (r *dashboardAggregationRepository) upsertHourlyAggregates(ctx context.Context, start, end time.Time) error {
	tzName := timezone.Name()
	query := `
		WITH hourly AS (
			SELECT
				date_trunc('hour', created_at AT TIME ZONE $3) AT TIME ZONE $3 AS bucket_start,
				COUNT(*) AS total_requests,
				COALESCE(SUM(input_tokens), 0) AS input_tokens,
				COALESCE(SUM(output_tokens), 0) AS output_tokens,
				COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
				COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
				COALESCE(SUM(total_cost), 0) AS total_cost,
				COALESCE(SUM(actual_cost), 0) AS actual_cost,
				COALESCE(SUM(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)), 0) AS account_cost,
				COALESCE(SUM(COALESCE(duration_ms, 0)), 0) AS total_duration_ms
			FROM usage_logs
			WHERE created_at >= $1 AND created_at < $2
			GROUP BY 1
		),
		user_counts AS (
			SELECT bucket_start, COUNT(*) AS active_users
			FROM usage_dashboard_hourly_users
			WHERE bucket_start >= $1 AND bucket_start < $2
			GROUP BY bucket_start
		)
		INSERT INTO usage_dashboard_hourly (
			bucket_start,
			total_requests,
			input_tokens,
			output_tokens,
			cache_creation_tokens,
			cache_read_tokens,
			total_cost,
			actual_cost,
			account_cost,
			total_duration_ms,
			active_users,
			computed_at
		)
		SELECT
			hourly.bucket_start,
			hourly.total_requests,
			hourly.input_tokens,
			hourly.output_tokens,
			hourly.cache_creation_tokens,
			hourly.cache_read_tokens,
			hourly.total_cost,
			hourly.actual_cost,
			hourly.account_cost,
			hourly.total_duration_ms,
			COALESCE(user_counts.active_users, 0) AS active_users,
			NOW()
		FROM hourly
		LEFT JOIN user_counts ON user_counts.bucket_start = hourly.bucket_start
		ON CONFLICT (bucket_start)
		DO UPDATE SET
			total_requests = EXCLUDED.total_requests,
			input_tokens = EXCLUDED.input_tokens,
			output_tokens = EXCLUDED.output_tokens,
			cache_creation_tokens = EXCLUDED.cache_creation_tokens,
			cache_read_tokens = EXCLUDED.cache_read_tokens,
			total_cost = EXCLUDED.total_cost,
			actual_cost = EXCLUDED.actual_cost,
			account_cost = EXCLUDED.account_cost,
			total_duration_ms = EXCLUDED.total_duration_ms,
			active_users = EXCLUDED.active_users,
			computed_at = EXCLUDED.computed_at
	`
	_, err := r.sql.ExecContext(ctx, query, start, end, tzName)
	return err
}

func (r *dashboardAggregationRepository) upsertDailyAggregates(ctx context.Context, start, end time.Time) error {
	tzName := timezone.Name()
	query := `
		WITH daily AS (
			SELECT
				(bucket_start AT TIME ZONE $5)::date AS bucket_date,
				COALESCE(SUM(total_requests), 0) AS total_requests,
				COALESCE(SUM(input_tokens), 0) AS input_tokens,
				COALESCE(SUM(output_tokens), 0) AS output_tokens,
				COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
				COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
				COALESCE(SUM(total_cost), 0) AS total_cost,
				COALESCE(SUM(actual_cost), 0) AS actual_cost,
				COALESCE(SUM(account_cost), 0) AS account_cost,
				COALESCE(SUM(total_duration_ms), 0) AS total_duration_ms
			FROM usage_dashboard_hourly
			WHERE bucket_start >= $1 AND bucket_start < $2
			GROUP BY (bucket_start AT TIME ZONE $5)::date
		),
		user_counts AS (
			SELECT bucket_date, COUNT(*) AS active_users
			FROM usage_dashboard_daily_users
			WHERE bucket_date >= $3::date AND bucket_date < $4::date
			GROUP BY bucket_date
		)
		INSERT INTO usage_dashboard_daily (
			bucket_date,
			total_requests,
			input_tokens,
			output_tokens,
			cache_creation_tokens,
			cache_read_tokens,
			total_cost,
			actual_cost,
			account_cost,
			total_duration_ms,
			active_users,
			computed_at
		)
		SELECT
			daily.bucket_date,
			daily.total_requests,
			daily.input_tokens,
			daily.output_tokens,
			daily.cache_creation_tokens,
			daily.cache_read_tokens,
			daily.total_cost,
			daily.actual_cost,
			daily.account_cost,
			daily.total_duration_ms,
			COALESCE(user_counts.active_users, 0) AS active_users,
			NOW()
		FROM daily
		LEFT JOIN user_counts ON user_counts.bucket_date = daily.bucket_date
		ON CONFLICT (bucket_date)
		DO UPDATE SET
			total_requests = EXCLUDED.total_requests,
			input_tokens = EXCLUDED.input_tokens,
			output_tokens = EXCLUDED.output_tokens,
			cache_creation_tokens = EXCLUDED.cache_creation_tokens,
			cache_read_tokens = EXCLUDED.cache_read_tokens,
			total_cost = EXCLUDED.total_cost,
			actual_cost = EXCLUDED.actual_cost,
			account_cost = EXCLUDED.account_cost,
			total_duration_ms = EXCLUDED.total_duration_ms,
			active_users = EXCLUDED.active_users,
			computed_at = EXCLUDED.computed_at
	`
	_, err := r.sql.ExecContext(ctx, query, start, end, start, end, tzName)
	return err
}

// isUsageLogsPartitioned 探测 usage_logs 是否分区。exec 由调用方传入：
// 事务路径必须与删除段同连接（同 *sql.Tx），非事务路径传 r.sql 保持既有语义。
func (r *dashboardAggregationRepository) isUsageLogsPartitioned(ctx context.Context, exec sqlExecutor) (bool, error) {
	query := `
		SELECT EXISTS(
			SELECT 1
			FROM pg_partitioned_table pt
			JOIN pg_class c ON c.oid = pt.partrelid
			WHERE c.relname = 'usage_logs'
		)
	`
	var partitioned bool
	if err := scanSingleRow(ctx, exec, query, nil, &partitioned); err != nil {
		return false, err
	}
	return partitioned, nil
}

// usageLogsPartition 描述一个待清理的 usage_logs 子分区。
type usageLogsPartition struct {
	name  string
	month time.Time
}

// listUsageLogsPartitions 列出所有早于 cutoff 月界的 usage_logs 子分区（只读探测/枚举，
// 执行器由调用方传入；事务路径必须与删除段同连接）。
func (r *dashboardAggregationRepository) listUsageLogsPartitions(ctx context.Context, exec sqlExecutor, cutoff time.Time) ([]usageLogsPartition, error) {
	rows, err := exec.QueryContext(ctx, `
		SELECT c.relname
		FROM pg_inherits
		JOIN pg_class c ON c.oid = pg_inherits.inhrelid
		JOIN pg_class p ON p.oid = pg_inherits.inhparent
		WHERE p.relname = 'usage_logs'
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cutoffMonth := truncateToMonthUTC(cutoff)
	partitions := make([]usageLogsPartition, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(name, "usage_logs_") {
			continue
		}
		suffix := strings.TrimPrefix(name, "usage_logs_")
		month, err := time.Parse("200601", suffix)
		if err != nil {
			continue
		}
		month = month.UTC()
		if month.Before(cutoffMonth) {
			partitions = append(partitions, usageLogsPartition{name: name, month: month})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(partitions, func(i, j int) bool {
		return partitions[i].month.Before(partitions[j].month)
	})
	return partitions, nil
}

// dropUsageLogsPartitionsOnExecutor 在调用方提供的 executor 上清理过期子分区。
// exec 为 *sql.Tx 时，每分区的「锁分组汇总态 + 失效分组汇总 + DROP」在同一外部事务内执行
// （PostgreSQL 的 DDL 是事务性的，DROP TABLE 随外层事务提交/回滚，不同于 MySQL 的隐式提交）；
// exec 为 *sql.DB 时退回既有 dropUsageLogsPartitionWithRollupInvalidation（各自独立事务）。
func (r *dashboardAggregationRepository) dropUsageLogsPartitionsOnExecutor(ctx context.Context, exec sqlExecutor, cutoff time.Time) error {
	partitions, err := r.listUsageLogsPartitions(ctx, exec, cutoff)
	if err != nil {
		return err
	}
	if tx, ok := exec.(*sql.Tx); ok {
		for _, partition := range partitions {
			if err := lockGroupUsageRollupState(ctx, tx); err != nil {
				return err
			}
			if err := invalidateGroupUsageRollupsAt(ctx, tx, partition.month); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", pq.QuoteIdentifier(partition.name))); err != nil {
				return err
			}
		}
		return nil
	}
	if db, ok := exec.(*sql.DB); ok {
		for _, partition := range partitions {
			if err := dropUsageLogsPartitionWithRollupInvalidation(ctx, db, partition.name, partition.month); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func dropUsageLogsPartitionWithRollupInvalidation(ctx context.Context, db *sql.DB, name string, monthStart time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}

	if err := lockGroupUsageRollupState(ctx, tx); err != nil {
		return rollback(err)
	}
	if err := invalidateGroupUsageRollupsAt(ctx, tx, monthStart); err != nil {
		return rollback(err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", pq.QuoteIdentifier(name))); err != nil {
		return rollback(err)
	}
	return tx.Commit()
}

func (r *dashboardAggregationRepository) createUsageLogsPartition(ctx context.Context, month time.Time) error {
	monthStart := truncateToMonthUTC(month)
	nextMonth := monthStart.AddDate(0, 1, 0)
	name := fmt.Sprintf("usage_logs_%s", monthStart.Format("200601"))
	query := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF usage_logs FOR VALUES FROM (%s) TO (%s)",
		pq.QuoteIdentifier(name),
		pq.QuoteLiteral(monthStart.Format("2006-01-02")),
		pq.QuoteLiteral(nextMonth.Format("2006-01-02")),
	)
	_, err := r.sql.ExecContext(ctx, query)
	return err
}

func truncateToDay(t time.Time) time.Time {
	return timezone.StartOfDay(t)
}

func truncateToMonthUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
