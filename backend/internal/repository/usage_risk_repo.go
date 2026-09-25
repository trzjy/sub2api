package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// usageRiskRepository 实现异常调用分析全部数据访问（派发单 U3）。
//
// 选型说明（对齐方案 §7 repository 行）：
//   - 全部走原生 SQL（聚合/GROUP BY/JSONB 过滤/列级 UPSERT 都要精确控制语句文本，
//     ent 在 GROUP BY 与列级隔离更新上样板代码过多且无法表达 ON CONFLICT 部分列语义）；
//   - 复用 sqlExecutor 接口（*sql.DB 或 *sql.Tx 均可执行），使清理方法可在接入方
//     的同一事务内执行（方案 §6.4 两清同事务）。
//
// 禁区遵守：本层只做数据读写，不打分、不判阈值、不选限额；限额/阈值/白名单全部
// 参数传入（rule 判断在 U4 service 层）。
type usageRiskRepository struct {
	client *dbent.Client
	sql    sqlExecutor
	// audit 用于在同事务内落账审计事件（U4b-R1：状态变更与审计同事务）。
	audit *auditLogRepository
}

// NewUsageRiskRepository 创建异常调用分析仓储。client 保留对齐既有 DI 形态，
// 聚合全部走原生 SQL（sqlDB）。
func NewUsageRiskRepository(client *dbent.Client, sqlDB *sql.DB) UsageRiskRepository {
	return newUsageRiskRepositoryWithSQL(client, sqlDB, sqlDB)
}

func newUsageRiskRepositoryWithSQL(client *dbent.Client, sqlq sqlExecutor, auditDB *sql.DB) *usageRiskRepository {
	return &usageRiskRepository{client: client, sql: sqlq, audit: &auditLogRepository{db: auditDB}}
}

// UsageRiskRepository 是异常调用分析仓储对外接口（U4b 编排依赖，本包定义避免循环依赖）。
type UsageRiskRepository interface {
	// ── 阶段一：聚合（只读 usage_logs，资格谓词内联） ──

	// AggregateHourly 按 (user_id, group_id, 墙钟小时) 聚合窗口内资格行，产出小时桶事实。
	AggregateHourly(ctx context.Context, windowStart, windowEnd time.Time,
		p AggregateParams) ([]UsageRiskHourAggregate, error)

	// AggregateGroupRPMMinutes 计算用户-分组作用域 RPM 贴线命中分钟数（R3a 分组作用域）。
	AggregateGroupRPMMinutes(ctx context.Context, windowStart, windowEnd time.Time,
		p AggregateParams, limits []UsageRiskRPMLimit, ratio float64) (map[UsageRiskUserGroupKey]int, error)

	// AggregateUserGlobalRPMMinutes 按 user+minute 独立现算用户全局作用域贴线分钟数
	// （该用户全部请求含按量行；每轮现算、不落表）。
	AggregateUserGlobalRPMMinutes(ctx context.Context, windowStart, windowEnd time.Time,
		limits []UsageRiskUserLimit, ratio float64) (map[int64]int, error)

	// ListCandidates 查询窗口内满足资格谓词 + min_daily_requests 的 (user,group) 完整超集
	// （无分数粗筛），按键序分页取批（每批 ≤ batchSize）。
	ListCandidates(ctx context.Context, windowStart, windowEnd time.Time, p AggregateParams,
		minDailyRequests int, offset, batchSize int) ([]UsageRiskCandidate, error)

	// RebuildHourlyWindow 在事务内对 [windowStart, windowEnd) 小时桶执行删除重建，
	// 返回受影响桶行数。
	RebuildHourlyWindow(ctx context.Context, windowStart, windowEnd time.Time,
		p AggregateParams) (int64, error)

	// ── 阶段二：明细（每批候选） ──

	// DistinctIPs 返回候选 (user) 当日资格行的 distinct IP 精确值（R4）。
	DistinctIPs(ctx context.Context, userID int64, dayStart, dayEnd time.Time,
		p AggregateParams) ([]string, error)

	// IPAssociatedUsers 按非空 IP 跨分组累计当日 distinct 订阅资格 user_id（R5）。
	// p 为资格谓词参数（与 DistinctIPs/AggregateHourly 同一 eligibilityClause，方案 §4 谓词 SSOT）。
	IPAssociatedUsers(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]service.IPClusterFact, error)

	// UADistribution 返回候选 (user) 当日资格行 UA → 请求数（内联同一资格谓词）。
	UADistribution(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]int, error)

	// KeyUsageDistribution 返回候选 (user) 当日资格行 api_key_id(字符串) → 请求数（跨分组聚合，R8；内联同一资格谓词）。
	KeyUsageDistribution(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]int, error)

	// ActiveHourHeatmap 返回候选 (user) 当日活跃小时热力（本地墙钟小时 → 请求数，跨全部分组）。
	// p 为资格谓词参数（内联同一 eligibilityClause，方案 §4 谓词 SSOT）。
	ActiveHourHeatmap(ctx context.Context, userID int64, dayStart, dayEnd time.Time,
		tzName string, p AggregateParams) (map[int]int, error)

	// R2PeerDayCosts 返回同分组 peer 群的当日日 cost 集合（同资格谓词 + 门槛，含被测用户）。
	R2PeerDayCosts(ctx context.Context, groupID int64, dayStart, dayEnd time.Time,
		p AggregateParams, minDailyRequests int) ([]UsageRiskPeerCost, error)

	// ── 报告写入（列级隔离，禁止整行覆盖） ──

	// UpsertReportDerived 对报告执行派生列 UPSERT：ON CONFLICT (user_id,group_id,report_date)
	// DO UPDATE 只更新 score/level/rule_hits/evidence/policy_version/invalidated_at。
	// invalidated_at 恢复 NULL 也走此路径（传入 nil 指针）。
	UpsertReportDerived(ctx context.Context, r UsageRiskReportRecord) error

	// UpdateReportStatus 状态接口：UPDATE SET status,status_updated_by,status_updated_at
	// WHERE report_id=$1 AND status=$2（原状态条件）。并发冲突返回 0 行由调用方判冲突。
	UpdateReportStatus(ctx context.Context, reportID int64, oldStatus, newStatus string,
		adminID int64, at time.Time) (bool, error)

	// UpdateStatusWithAudit 状态变更与审计事件同事务落账（方案 §5:91 / U4b-R1）。
	// 单事务内先条件 UPDATE（reportStatusUpdateSQL 原样语义），再 InsertTx 写入 entry；
	// 任一步失败整体回滚并返回错误。UPDATE 影响 0 行（并发冲突）时不写审计、直接回滚返回 (false, nil)。
	UpdateStatusWithAudit(ctx context.Context, reportID int64, oldStatus, newStatus string,
		adminID int64, at time.Time, entry *service.AuditLog) (bool, error)

	// ReconcileReportBatchUPSERTOnly 对账批次 UPSERT-only（单事务）：仅对 records 走派生列
	// UPSERT 路径，绝不触发集合外失效。幂等、可重复（崩溃续跑已提交批不重做）。
	ReconcileReportBatchUPSERTOnly(ctx context.Context, reportDate time.Time, records []UsageRiskReportRecord) error

	// InvalidateOutsideKeySet 失效 reportDate 当日、键集（userIDs,groupIDs）之外且当前有效
	// （invalidated_at IS NULL）的报告。空数组 = 失效该日全部有效旧行（等价现 nil 派生集语义）。
	// 单事务，只动有效性维度（status / 审计列不出现）。
	InvalidateOutsideKeySet(ctx context.Context, reportDate time.Time, userIDs, groupIDs []int64) error
	// InvalidateAllEffective 全量失效当前有效（invalidated_at IS NULL）报告，不推测日期范围。
	// 时区换日迁移专用：按当前配置推导日期循环会漏掉保留期曾缩短/清理曾失败遗留的范围外
	// 有效报告（外审⑨ 发现3）。只动有效性维度（status/审计列不出现）。
	InvalidateAllEffective(ctx context.Context) error

	// ── 台账 usage_risk_runs ──

	CreateRun(ctx context.Context, r UsageRiskRunRecord) (int64, error)
	UpdateRunProgress(ctx context.Context, runID int64, p UsageRiskRunProgress) error
	CompleteRun(ctx context.Context, runID int64, status string, p UsageRiskRunProgress) error
	ConvergeStaleRuns(ctx context.Context, staleBefore time.Time, failureStage string) error
	LatestRun(ctx context.Context) (*service.UsageRiskRunStatus, error)
	// LoadLatestReconCursor 只读补读上一轮持久化游标三元组 + 失败批次（U4b-R3 断点续跑）。
	// 仅 SELECT，不写、不改其他查询语义。
	LoadLatestReconCursor(ctx context.Context) (*service.UsageRiskReconCursor, error)

	// ── 查询端 ──

	// UserActiveBucketHours 只读回看窗口内某用户的 distinct 活跃桶（user_usage_metrics_rollup），
	// 供 computeConsecutiveActiveDays 直接统计连续活跃天数，删除逐日 AggregateHourly 全量重聚合（I）。
	UserActiveBucketHours(ctx context.Context, userID int64, lookbackStart, reportDay time.Time) ([]time.Time, error)

	ListReports(ctx context.Context, f service.UsageRiskListFilter, minScore int) ([]service.UsageRiskReportItem, int, error)
	GetReportDetail(ctx context.Context, reportID int64) (*service.UsageRiskReportDetail, error)
	RiskSummary(ctx context.Context, minScore, topN int) (*service.UsageRiskSummary, error)

	// ── 清理（可在外部事务执行） ──

	// CleanupReportAndRollupTx 在外部传入的 executor（*sql.DB 或 *sql.Tx）上执行两清。
	CleanupReportAndRollupTx(ctx context.Context, exec sqlExecutor, reportCutoff, rollupCutoff time.Time) (int64, int64, error)
	// CleanupReportAndRollupTxDB 是 CleanupReportAndRollupTx 的 *sql.Tx 暴露版本（由 usage_risk_cleanup_adapter.go 提供），
	// 供 dashboard 保留期协调器在唯一 DB 事务内与源日志清理同事务提交（service 包无法引用未导出的 sqlExecutor）。
	CleanupReportAndRollupTxDB(ctx context.Context, tx *sql.Tx, reportCutoff, rollupCutoff time.Time) (int64, int64, error)
	// CleanupReportAndRollup 便捷版本：自开事务执行两清。
	CleanupReportAndRollup(ctx context.Context, reportCutoff, rollupCutoff time.Time) (int64, int64, error)
}

// ───────────────────────── DTO（repository 包内，供 U4b 编排消费） ─────────────────────────

// AggregateParams 是聚合查询的资格谓词参数（不参与规则判断）。
type AggregateParams struct {
	// UnlimitedGroupsOnly 仅分析不限量订阅分组（组 daily/weekly/monthly_limit_usd 全 NULL）。
	UnlimitedGroupsOnly bool
	// UAWhitelist 已知编程客户端 UA 子串白名单（R6 计数用）。
	UAWhitelist []string
}

// UsageRiskUserGroupKey 是 (user_id, group_id) 复合键。
type UsageRiskUserGroupKey struct {
	UserID  int64
	GroupID int64
}

// UsageRiskHourAggregate 是小时桶聚合事实（§6.1 字段）。
type UsageRiskHourAggregate struct {
	UserID               int64
	GroupID              int64
	BucketHour           time.Time
	RequestCount         int
	InputTokensSum       int64
	OutputTokensSum      int64
	CacheReadTokensSum   int64
	CostUSDSum           float64
	OccupiedMsSum        int64
	NonWhitelistedUACount int
}

// UsageRiskRPMLimit 是用户-分组作用域限额输入（适用分组上限，调用方按 checkRPM 语义算好）。
type UsageRiskRPMLimit struct {
	UserID   int64
	GroupID  int64
	GroupLimit int // 适用分组上限；0 = 免除分组检查（override=0）→ 不参与贴线
}

// UsageRiskUserLimit 是用户全局作用域限额输入（users.rpm_limit 全局天花板）。
type UsageRiskUserLimit struct {
	UserID    int64
	UserLimit int
}

// UsageRiskCandidate 是候选 (user, group) 及其门槛内请求数。
type UsageRiskCandidate struct {
	UserID       int64
	GroupID      int64
	RequestCount int
}

// UsageRiskPeerCost 是 peer 群单成员当日日 cost。
type UsageRiskPeerCost struct {
	UserID  int64
	CostUSD float64
}

// UsageRiskReportKey 是对账批次键。
type UsageRiskReportKey struct {
	UserID     int64
	GroupID    int64
	ReportDate time.Time
}

// UsageRiskReportRecord 是报告派生列写入记录。
type UsageRiskReportRecord struct {
	UserID        int64
	GroupID       int64
	ReportDate    time.Time
	PolicyVersion int64
	Score         int
	Level         string
	RuleHits      json.RawMessage
	Evidence      json.RawMessage
	InvalidatedAt *time.Time // nil = 有效（invalidated_at 恢复 NULL）
}

// UsageRiskRunRecord 是新建 running run 的输入。
type UsageRiskRunRecord struct {
	RunAt           time.Time
	WindowStart     time.Time
	WindowEnd       time.Time
	PolicyVersion   int64
	PolicySnapshot  json.RawMessage
	CandidatesTotal int
}

// UsageRiskRunProgress 是 run 进度/收敛更新字段。
type UsageRiskRunProgress struct {
	BatchesDone         int
	BatchesFailed       int
	FailedBatches       json.RawMessage
	BudgetExhausted     bool
	ReconCursorDate     *time.Time
	ReconBatchOffset    *int
	ReconBatchesDone    int
	HistoryCovered      bool
	CandidatesTotal     int  // 本轮累计候选数（L137 台账契约）
	ConsecutivePartials int  // 连续 partial 计数（completed 重置 0，否则上一轮+1；M）
	R1ReevalPending     bool  // 覆盖翻转后待重评 R1 标记（E）
	FinishedAt          *time.Time
	FailureStage        *string
}

// ───────────────────────── 辅助 ─────────────────────────

// setLocalStatementTimeout 在事务/独占连接上按 ctx 剩余预算设置 statement_timeout。
// 仅可在事务内使用（SET LOCAL 随事务结束自动恢复，不污染连接池）。
// 无 deadline 时不设置（由调用方 ctx 控制取消）。
func setLocalStatementTimeout(ctx context.Context, exec sqlExecutor) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return ctx.Err()
	}
	ms := remaining.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	if _, err := exec.ExecContext(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(ms, 10)); err != nil {
		return err
	}
	return nil
}

// beginTxWithTimeout 开启事务并设置事务级 statement_timeout（≤ ctx 剩余预算）。
func (r *usageRiskRepository) beginTxWithTimeout(ctx context.Context) (*sql.Tx, error) {
	db, ok := r.sql.(*sql.DB)
	if !ok {
		return nil, errors.New("usage risk: cannot begin tx on non-*sql.DB executor")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := setLocalStatementTimeout(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// eligibilityClause 构建资格谓词 WHERE 片段（只读 usage_logs 的内联过滤）。
// 别名固定 ul；unlimited_groups_only 时 join 分组限额三列为 NULL 的子查询。
func eligibilityClause(p AggregateParams) string {
	if !p.UnlimitedGroupsOnly {
		return "ul.subscription_id IS NOT NULL AND ul.group_id IS NOT NULL"
	}
	return `ul.subscription_id IS NOT NULL AND ul.group_id IS NOT NULL
	AND ul.group_id IN (SELECT id FROM groups
		WHERE daily_limit_usd IS NULL AND weekly_limit_usd IS NULL AND monthly_limit_usd IS NULL
		AND deleted_at IS NULL)`
}

// uaWhitelistTerms 返回白名单子串原文数组（字面子串语义，与规则引擎
// isWhitelistedUA/containsSubstr 同口径），滤空串，空列表返回 nil。
// 外审⑧ #5：LIKE 模式包装（%sub%）会把配置中的 %/_ 解释为通配符，与规则引擎
// 字面匹配对同一日志集产出不同事实；空哨兵 [''] 又使空串 UA 对模式 '' 的 LIKE
// 为 true 被误当白名单命中——两路径事实分叉，一律废除。
func uaWhitelistTerms(whitelist []string) []string {
	out := make([]string, 0, len(whitelist))
	for _, w := range whitelist {
		if w == "" {
			continue
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ───────────────────────── 阶段一：聚合 ─────────────────────────

// aggregateHourlySQL 是小时桶聚合 SELECT 主体（阶段一聚合 + 删除重建共用）。
// 参数序：$1 windowStart, $2 windowEnd, $3 tzName, $4 UA 白名单子串数组（字面子串）。
// 资格谓词由资格子句常量拼接，无参数。
func aggregateHourlySQL() string {
	return fmt.Sprintf(`
WITH hourly AS (
    SELECT
        ul.user_id,
        ul.group_id,
        (date_trunc('hour', ul.created_at AT TIME ZONE $3) - ((ul.created_at AT TIME ZONE $3) - (ul.created_at AT TIME ZONE 'UTC'))) AT TIME ZONE 'UTC' AS bucket_hour,
        COUNT(*)::int                                  AS request_count,
        COALESCE(SUM(ul.input_tokens), 0)::bigint      AS input_tokens_sum,
        COALESCE(SUM(ul.output_tokens), 0)::bigint     AS output_tokens_sum,
        COALESCE(SUM(ul.cache_read_tokens), 0)::bigint AS cache_read_tokens_sum,
        COALESCE(SUM(ul.actual_cost), 0)::numeric(20,10) AS cost_usd_sum,
        COALESCE(SUM(ul.duration_ms), 0)::bigint       AS occupied_ms_sum,
        COUNT(*) FILTER (
            WHERE ul.user_agent IS NULL OR NOT COALESCE((
                SELECT bool_or(strpos(ul.user_agent, t) > 0) FROM unnest($4::text[]) t
            ), false)
        )::int AS non_whitelisted_ua_count
    FROM usage_logs ul
    WHERE ul.created_at >= $1 AND ul.created_at < $2
      AND %s
    GROUP BY ul.user_id, ul.group_id, bucket_hour
)
SELECT user_id, group_id, bucket_hour, request_count,
       input_tokens_sum, output_tokens_sum, cache_read_tokens_sum,
       cost_usd_sum, occupied_ms_sum, non_whitelisted_ua_count
FROM hourly
ORDER BY user_id, group_id, bucket_hour
`, "ul.subscription_id IS NOT NULL AND ul.group_id IS NOT NULL")
}

// AggregateHourly 聚合窗口内资格行为小时桶事实（对账/回填复用）。
func (r *usageRiskRepository) AggregateHourly(ctx context.Context, windowStart, windowEnd time.Time,
	p AggregateParams) ([]UsageRiskHourAggregate, error) {
	q := strings.Replace(aggregateHourlySQL(),
		"ul.subscription_id IS NOT NULL AND ul.group_id IS NOT NULL",
		eligibilityClause(p), 1)
	// 白名单子串字面直传（含 %/_ 原样）；空白名单 → nil → pq.Array(NULL) →
	// unnest 零行 → bool_or NULL → COALESCE false → 全部计入（与规则侧空
	// 白名单==false 一致），无空哨兵。
	terms := uaWhitelistTerms(p.UAWhitelist)
	rows, err := r.sql.QueryContext(ctx, q, windowStart, windowEnd, timezoneNameRepo(), pq.Array(terms))
	if err != nil {
		return nil, fmt.Errorf("aggregate hourly: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]UsageRiskHourAggregate, 0)
	for rows.Next() {
		var a UsageRiskHourAggregate
		if err := rows.Scan(&a.UserID, &a.GroupID, &a.BucketHour, &a.RequestCount,
			&a.InputTokensSum, &a.OutputTokensSum, &a.CacheReadTokensSum,
			&a.CostUSDSum, &a.OccupiedMsSum, &a.NonWhitelistedUACount); err != nil {
			return nil, fmt.Errorf("scan hourly aggregate: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RebuildHourlyWindow 事务内删除重建 [windowStart, windowEnd) 小时桶。
// 返回删除重建的桶行数（即受影响行数）。
// 桶对齐（方案 §5）：windowStart 按应用时区墙钟下取整到整点桶边界，删除与聚合统一
// 使用对齐后边界——非整点起点会使首段日志归入早于起点的 trunc 桶，而 DELETE
// bucket_hour >= windowStart 不覆盖该桶，重插同键 (user_id,group_id,bucket_hour)
// 唯一约束冲突（周期任务第二轮必炸）；对齐后桶完整重建。
func (r *usageRiskRepository) RebuildHourlyWindow(ctx context.Context, windowStart, windowEnd time.Time,
	p AggregateParams) (int64, error) {
	q := strings.Replace(aggregateHourlySQL(),
		"ul.subscription_id IS NOT NULL AND ul.group_id IS NOT NULL",
		eligibilityClause(p), 1)

	// 窗口起点按应用时区墙钟下取整到整点桶边界；对齐后变量贯穿 DELETE 与 INSERT（聚合 $1）。
	alignedStart := alignWindowStartToHour(timezone.Location(), windowStart)

	tx, err := r.beginTxWithTimeout(ctx)
	if err != nil {
		return 0, err
	}
	rollback := func(err error) (int64, error) {
		_ = tx.Rollback()
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM user_usage_metrics_rollup WHERE bucket_hour >= $1 AND bucket_hour < $2`,
		alignedStart, windowEnd); err != nil {
		return rollback(fmt.Errorf("delete hourly window: %w", err))
	}

	insertQ := fmt.Sprintf(`INSERT INTO user_usage_metrics_rollup (
		user_id, group_id, bucket_hour,
		request_count, input_tokens_sum, output_tokens_sum, cache_read_tokens_sum,
		cost_usd_sum, occupied_ms_sum, non_whitelisted_ua_count
	)
%s`, q)
	// q 以 SELECT ... FROM hourly 结尾；去掉 ORDER BY 以避免无意义排序。
	insertQ = strings.TrimSuffix(insertQ, "\nORDER BY user_id, group_id, bucket_hour")

	terms := uaWhitelistTerms(p.UAWhitelist)
	res, err := tx.ExecContext(ctx, insertQ, alignedStart, windowEnd, timezoneNameRepo(), pq.Array(terms))
	if err != nil {
		return rollback(fmt.Errorf("insert hourly window: %w", err))
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return rollback(fmt.Errorf("rows affected (insert hourly): %w", err))
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit hourly rebuild: %w", err)
	}
	return affected, nil
}

// alignWindowStartToHour 将 ts 按 loc 墙钟下取整到整点桶边界（instant-safe，外审⑦ #1）。
// DST 回拨歧义墙钟（如 Berlin 回拨日 02:37 两现）下 time.Date 的 occurrence 选择
// not guaranteed（Go 实测取第二个即未来时刻，会破坏"下取整"语义，DELETE/INSERT
// 遗漏窗口开头段）——改用墙钟分钟秒纳秒回退：IANA offset 跳变均在整点边界发生，
// 小时内无跳变，回退结果即 ts 同 occurrence 的墙钟整点；无 DST 时区与 time.Date
// 构造完全等价。loc 参数化供 DST 锁定测试直测（生产唯一调用点传应用时区）。
func alignWindowStartToHour(loc *time.Location, ts time.Time) time.Time {
	w := ts.In(loc)
	return ts.Add(-(time.Duration(w.Minute())*time.Minute +
		time.Duration(w.Second())*time.Second +
		time.Duration(w.Nanosecond())*time.Nanosecond))
}

// timezoneNameRepo 返回应用时区名。
func timezoneNameRepo() string {
	return timezone.Name()
}

// UserActiveBucketHours 只读回看窗口内某用户的 distinct 活跃桶（user_usage_metrics_rollup），
// 供 computeConsecutiveActiveDays 直接统计连续活跃天数，替代逐日 AggregateHourly 全量重聚合（I）。
// 仅 SELECT，不写、不改其他语义；bucket_hour 落在 [lookbackStart, reportDay) 半开区间。
func (r *usageRiskRepository) UserActiveBucketHours(ctx context.Context, userID int64, lookbackStart, reportDay time.Time) ([]time.Time, error) {
	const q = `
SELECT DISTINCT bucket_hour
FROM user_usage_metrics_rollup
WHERE user_id = $1 AND bucket_hour >= $2 AND bucket_hour < $3
ORDER BY 1
`
	rows, err := r.sql.QueryContext(ctx, q, userID, lookbackStart, reportDay)
	if err != nil {
		return nil, fmt.Errorf("user active bucket hours: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]time.Time, 0, 16)
	for rows.Next() {
		var bh time.Time
		if err := rows.Scan(&bh); err != nil {
			return nil, fmt.Errorf("scan active bucket hour: %w", err)
		}
		out = append(out, bh)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ───────────────────────── 分钟聚合 ─────────────────────────

// AggregateGroupRPMMinutes 计算用户-分组作用域 RPM 贴线命中分钟数（R3a 分组作用域）。
// 仅统计资格行；每个 (user,group) 的适用分组上限由调用方算好传入（GroupLimit>0 才参与贴线）。
func (r *usageRiskRepository) AggregateGroupRPMMinutes(ctx context.Context, windowStart, windowEnd time.Time,
	p AggregateParams, limits []UsageRiskRPMLimit, ratio float64) (map[UsageRiskUserGroupKey]int, error) {
	out := make(map[UsageRiskUserGroupKey]int)
	if len(limits) == 0 {
		return out, nil
	}
	userIDs := make([]int64, len(limits))
	groupIDs := make([]int64, len(limits))
	groupLimits := make([]int, len(limits))
	for i, l := range limits {
		userIDs[i] = l.UserID
		groupIDs[i] = l.GroupID
		groupLimits[i] = l.GroupLimit
	}

	const qTemplate = `
WITH limits AS (
    SELECT user_id, group_id, group_limit
    FROM unnest($3::bigint[], $4::bigint[], $5::int[]) AS t(user_id, group_id, group_limit)
),
minutes AS (
    SELECT ul.user_id, ul.group_id, l.group_limit,
           date_trunc('minute', ul.created_at) AS bucket_minute,
           COUNT(*) AS cnt
    FROM usage_logs ul
    JOIN limits l ON l.user_id = ul.user_id AND l.group_id = ul.group_id
    WHERE ul.created_at >= $1 AND ul.created_at < $2
      AND %s
    GROUP BY ul.user_id, ul.group_id, l.group_limit, bucket_minute
)
SELECT user_id, group_id, COUNT(*) FILTER (WHERE cnt::float8 >= group_limit * $6::float8)::int
FROM minutes
WHERE group_limit > 0
GROUP BY user_id, group_id
`
	q := fmt.Sprintf(qTemplate, eligibilityClause(p))
	rows, err := r.sql.QueryContext(ctx, q, windowStart, windowEnd, pq.Array(userIDs), pq.Array(groupIDs), pq.Array(groupLimits), ratio)
	if err != nil {
		return nil, fmt.Errorf("aggregate group rpm minutes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var key UsageRiskUserGroupKey
		var n int
		if err := rows.Scan(&key.UserID, &key.GroupID, &n); err != nil {
			return nil, fmt.Errorf("scan group rpm minutes: %w", err)
		}
		out[key] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AggregateUserGlobalRPMMinutes 按 user+minute 独立现算用户全局作用域贴线分钟数。
// 该用户全部请求（含按量行），与网关用户全局限流作用域一致；每轮现算、不落表。
func (r *usageRiskRepository) AggregateUserGlobalRPMMinutes(ctx context.Context, windowStart, windowEnd time.Time,
	limits []UsageRiskUserLimit, ratio float64) (map[int64]int, error) {
	out := make(map[int64]int)
	if len(limits) == 0 {
		return out, nil
	}
	userIDs := make([]int64, len(limits))
	userLimits := make([]int, len(limits))
	for i, l := range limits {
		userIDs[i] = l.UserID
		userLimits[i] = l.UserLimit
	}

	const q = `
WITH limits AS (
    SELECT user_id, user_limit
    FROM unnest($3::bigint[], $4::int[]) AS t(user_id, user_limit)
),
minutes AS (
    SELECT ul.user_id, l.user_limit, date_trunc('minute', ul.created_at) AS bucket_minute, COUNT(*) AS cnt
    FROM usage_logs ul
    JOIN limits l ON l.user_id = ul.user_id
    WHERE ul.created_at >= $1 AND ul.created_at < $2
    GROUP BY ul.user_id, l.user_limit, bucket_minute
)
SELECT user_id, COUNT(*) FILTER (WHERE cnt::float8 >= user_limit * $5::float8)::int
FROM minutes
WHERE user_limit > 0
GROUP BY user_id
`
	rows, err := r.sql.QueryContext(ctx, q, windowStart, windowEnd, pq.Array(userIDs), pq.Array(userLimits), ratio)
	if err != nil {
		return nil, fmt.Errorf("aggregate user global rpm minutes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var userID int64
		var n int
		if err := rows.Scan(&userID, &n); err != nil {
			return nil, fmt.Errorf("scan user global rpm minutes: %w", err)
		}
		out[userID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ───────────────────────── 候选集 ─────────────────────────

// ListCandidates 查询窗口内满足资格谓词 + min_daily_requests 的 (user,group) 完整超集。
// 无分数粗筛；按 (user_id, group_id) 键序分页取批。
// 方案 §6.2：用户级规则按"用户当日资格行全量"过门槛——跨分组各不足门槛但用户合计
// 达标的用户，其全部资格分组键必须进入候选（否则 R1/R4/R5/R6/R8 用户级规则漏报）。
// 故候选集 = 分组达标键 ∪ 用户全量达标用户的全部资格分组键（UNION 按整行去重；
// 同键两分支计数相同，request_count 保持该键的分组计数语义，不因用户级达标改变）。
func (r *usageRiskRepository) ListCandidates(ctx context.Context, windowStart, windowEnd time.Time, p AggregateParams,
	minDailyRequests int, offset, batchSize int) ([]UsageRiskCandidate, error) {
	q := fmt.Sprintf(`
SELECT user_id, group_id, request_count FROM (
    SELECT ul.user_id, ul.group_id, COUNT(*)::int AS request_count
    FROM usage_logs ul
    WHERE ul.created_at >= $1 AND ul.created_at < $2
      AND %s
    GROUP BY ul.user_id, ul.group_id
    HAVING COUNT(*) >= $3
    UNION
    SELECT ul.user_id, ul.group_id, COUNT(*)::int AS request_count
    FROM usage_logs ul
    WHERE ul.created_at >= $1 AND ul.created_at < $2
      AND %s
      AND ul.user_id IN (
          SELECT ul.user_id FROM usage_logs ul
          WHERE ul.created_at >= $1 AND ul.created_at < $2
            AND %s
          GROUP BY ul.user_id
          HAVING COUNT(*) >= $3)
    GROUP BY ul.user_id, ul.group_id
) c
ORDER BY user_id, group_id
LIMIT $5 OFFSET $4
`, eligibilityClause(p), eligibilityClause(p), eligibilityClause(p))
	rows, err := r.sql.QueryContext(ctx, q, windowStart, windowEnd, minDailyRequests, offset, batchSize)
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]UsageRiskCandidate, 0)
	for rows.Next() {
		var c UsageRiskCandidate
		if err := rows.Scan(&c.UserID, &c.GroupID, &c.RequestCount); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ───────────────────────── 阶段二：明细 ─────────────────────────

// DistinctIPs 返回候选 (user) 当日资格行的 distinct IP 精确值（R4）。
func (r *usageRiskRepository) DistinctIPs(ctx context.Context, userID int64, dayStart, dayEnd time.Time,
	p AggregateParams) ([]string, error) {
	q := fmt.Sprintf(`
SELECT DISTINCT ul.ip_address
FROM usage_logs ul
WHERE ul.user_id = $1 AND ul.created_at >= $2 AND ul.created_at < $3
  AND %s
  AND ul.ip_address IS NOT NULL AND ul.ip_address <> ''
ORDER BY ul.ip_address
`, eligibilityClause(p))
	rows, err := r.sql.QueryContext(ctx, q, userID, dayStart, dayEnd)
	if err != nil {
		return nil, fmt.Errorf("distinct IPs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0)
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, fmt.Errorf("scan distinct ip: %w", err)
		}
		out = append(out, ip)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// IPAssociatedUsers 按非空 IP 跨分组累计当日 distinct 订阅资格 user_id（R5）。
// 以被测用户当日使用的非空 IP 为起点，返回每个 IP 的全局聚簇事实（跨全部分组/全部用户）。
// p 为资格谓词参数（与 DistinctIPs/AggregateHourly 同一 eligibilityClause，方案 §4 谓词 SSOT）。
func (r *usageRiskRepository) IPAssociatedUsers(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]service.IPClusterFact, error) {
	out := make(map[string]service.IPClusterFact)
	if userID <= 0 {
		return out, nil
	}
	const qTemplate = `
WITH my_ips AS (
    SELECT DISTINCT ul.ip_address
    FROM usage_logs ul
    WHERE ul.user_id = $1 AND ul.created_at >= $2 AND ul.created_at < $3
      AND %s
      AND ul.ip_address IS NOT NULL AND ul.ip_address <> ''
)
SELECT ul.ip_address,
       COUNT(DISTINCT ul.user_id)::int,
       COUNT(*)::int
FROM usage_logs ul
JOIN my_ips mi ON mi.ip_address = ul.ip_address
WHERE ul.created_at >= $2 AND ul.created_at < $3
  AND %s
  AND ul.ip_address IS NOT NULL AND ul.ip_address <> ''
GROUP BY ul.ip_address
`
	q := fmt.Sprintf(qTemplate, eligibilityClause(p), eligibilityClause(p))
	rows, err := r.sql.QueryContext(ctx, q, userID, dayStart, dayEnd)
	if err != nil {
		return nil, fmt.Errorf("IP associated users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var ip string
		var f service.IPClusterFact
		if err := rows.Scan(&ip, &f.DistinctUserCount, &f.TotalRequests); err != nil {
			return nil, fmt.Errorf("scan ip cluster: %w", err)
		}
		out[ip] = f
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UADistribution 返回候选 (user) 当日资格行 UA → 请求数（内联同一资格谓词）。
func (r *usageRiskRepository) UADistribution(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]int, error) {
	out := make(map[string]int)
	if userID <= 0 {
		return out, nil
	}
	const qTemplate = `
SELECT COALESCE(ul.user_agent, ''), COUNT(*)::int
FROM usage_logs ul
WHERE ul.user_id = $1 AND ul.created_at >= $2 AND ul.created_at < $3
  AND %s
GROUP BY COALESCE(ul.user_agent, '')
`
	q := fmt.Sprintf(qTemplate, eligibilityClause(p))
	rows, err := r.sql.QueryContext(ctx, q, userID, dayStart, dayEnd)
	if err != nil {
		return nil, fmt.Errorf("UA distribution: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var ua string
		var n int
		if err := rows.Scan(&ua, &n); err != nil {
			return nil, fmt.Errorf("scan ua distribution: %w", err)
		}
		out[ua] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// KeyUsageDistribution 返回候选 (user) 当日资格行 api_key_id(字符串) → 请求数（跨分组聚合，R8；内联同一资格谓词）。
func (r *usageRiskRepository) KeyUsageDistribution(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]int, error) {
	out := make(map[string]int)
	if userID <= 0 {
		return out, nil
	}
	const qTemplate = `
SELECT ul.api_key_id::text, COUNT(*)::int
FROM usage_logs ul
WHERE ul.user_id = $1 AND ul.created_at >= $2 AND ul.created_at < $3
  AND %s
GROUP BY ul.api_key_id
`
	q := fmt.Sprintf(qTemplate, eligibilityClause(p))
	rows, err := r.sql.QueryContext(ctx, q, userID, dayStart, dayEnd)
	if err != nil {
		return nil, fmt.Errorf("key usage distribution: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, fmt.Errorf("scan key usage distribution: %w", err)
		}
		out[key] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ActiveHourHeatmap 返回候选 (user) 当日活跃小时热力（本地墙钟小时 → 请求数，跨全部分组）。
// DST 特殊日：本地小时索引可重复，由调用方聚合；返回 map[本地小时]请求数。
// p 为资格谓词参数（内联同一 eligibilityClause，方案 §4 谓词 SSOT）。
func (r *usageRiskRepository) ActiveHourHeatmap(ctx context.Context, userID int64, dayStart, dayEnd time.Time,
	tzName string, p AggregateParams) (map[int]int, error) {
	out := make(map[int]int)
	if userID <= 0 {
		return out, nil
	}
	const qTemplate = `
SELECT EXTRACT(HOUR FROM ul.created_at AT TIME ZONE $4)::int, COUNT(*)::int
FROM usage_logs ul
WHERE ul.user_id = $1 AND ul.created_at >= $2 AND ul.created_at < $3
  AND %s
GROUP BY 1
`
	q := fmt.Sprintf(qTemplate, eligibilityClause(p))
	rows, err := r.sql.QueryContext(ctx, q, userID, dayStart, dayEnd, tzName)
	if err != nil {
		return nil, fmt.Errorf("active hour heatmap: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var h, n int
		if err := rows.Scan(&h, &n); err != nil {
			return nil, fmt.Errorf("scan heatmap: %w", err)
		}
		out[h] += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// R2PeerDayCosts 返回同分组 peer 群的当日日 cost 集合（同资格谓词 + 门槛，含被测用户）。
func (r *usageRiskRepository) R2PeerDayCosts(ctx context.Context, groupID int64, dayStart, dayEnd time.Time,
	p AggregateParams, minDailyRequests int) ([]UsageRiskPeerCost, error) {
	q := fmt.Sprintf(`
SELECT ul.user_id, SUM(ul.actual_cost)::numeric(20,10)
FROM usage_logs ul
WHERE ul.group_id = $1 AND ul.created_at >= $2 AND ul.created_at < $3
  AND %s
GROUP BY ul.user_id
HAVING COUNT(*) >= $4
ORDER BY ul.user_id
`, eligibilityClause(p))
	rows, err := r.sql.QueryContext(ctx, q, groupID, dayStart, dayEnd, minDailyRequests)
	if err != nil {
		return nil, fmt.Errorf("R2 peer day costs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]UsageRiskPeerCost, 0)
	for rows.Next() {
		var c UsageRiskPeerCost
		if err := rows.Scan(&c.UserID, &c.CostUSD); err != nil {
			return nil, fmt.Errorf("scan peer cost: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ───────────────────────── 报告写入（列级隔离） ─────────────────────────

// reportDerivedUpsertSQL 是报告派生列 UPSERT 语句。
// 关键约束（方案 §6.2 / §10 并发隔离）：DO UPDATE SET 子句只含派生列
// score/level/rule_hits/evidence/policy_version/invalidated_at；
// status/status_updated_by/status_updated_at 绝不出现在 SET 子句。
// 该文本被单测断言（列级白名单）。
const reportDerivedUpsertSQL = `
INSERT INTO usage_risk_reports (
    user_id, group_id, report_date,
    policy_version, score, level, rule_hits, evidence, invalidated_at
)
VALUES ($1, $2, $3::date, $4, $5, $6, $7, $8, $9)
ON CONFLICT (user_id, group_id, report_date) DO UPDATE SET
    policy_version  = EXCLUDED.policy_version,
    score           = EXCLUDED.score,
    level           = EXCLUDED.level,
    rule_hits       = EXCLUDED.rule_hits,
    evidence        = EXCLUDED.evidence,
    invalidated_at  = EXCLUDED.invalidated_at
`

// UpsertReportDerived 对报告执行派生列 UPSERT。invalidated_at 恢复 NULL 走本路径。
func (r *usageRiskRepository) UpsertReportDerived(ctx context.Context, rec UsageRiskReportRecord) error {
	if _, err := r.sql.ExecContext(ctx, reportDerivedUpsertSQL,
		rec.UserID, rec.GroupID, rec.ReportDate, rec.PolicyVersion, rec.Score, rec.Level,
		rec.RuleHits, rec.Evidence, rec.InvalidatedAt); err != nil {
		return fmt.Errorf("upsert report derived: %w", err)
	}
	return nil
}

// reportStatusUpdateSQL 是状态接口 UPDATE 语句：只 SET 状态审计列，且带原状态条件。
// WHERE report_id=$1 AND status=$2 AND invalidated_at IS NULL；并发冲突返回 0 行由调用方判冲突。
// R10-1/#6：已失效（invalidated_at 非空）报告不参与状态机——对账并发失效后人工状态
// 变更收敛为 0 行受影响（调用方按既有 not-found/false 语义消费），与公开契约对齐。
// 该文本被单测断言（只含状态审计列 + 原状态条件 + 失效过滤）。
const reportStatusUpdateSQL = `
UPDATE usage_risk_reports
SET status = $3,
    status_updated_by = $4,
    status_updated_at = $5
WHERE report_id = $1 AND status = $2 AND invalidated_at IS NULL
`

// UpdateReportStatus 状态接口：原状态条件，冲突返回 (false, nil)。
func (r *usageRiskRepository) UpdateReportStatus(ctx context.Context, reportID int64, oldStatus, newStatus string,
	adminID int64, at time.Time) (bool, error) {
	res, err := r.sql.ExecContext(ctx, reportStatusUpdateSQL, reportID, oldStatus, newStatus, adminID, at)
	if err != nil {
		return false, fmt.Errorf("update report status: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected (update status): %w", err)
	}
	return affected > 0, nil
}

// UpdateStatusWithAudit 状态变更与审计事件同事务落账（方案 §5:91 / U4b-R1）。
// 单事务内：① 条件 UPDATE（reportStatusUpdateSQL 原样语义）；② InsertTx 写入 entry。
// 任一步失败整体回滚并返回错误；UPDATE 影响 0 行（并发冲突）时不写审计、直接回滚返回 (false, nil)。
func (r *usageRiskRepository) UpdateStatusWithAudit(ctx context.Context, reportID int64, oldStatus, newStatus string,
	adminID int64, at time.Time, entry *service.AuditLog) (bool, error) {
	tx, err := r.beginTxWithTimeout(ctx)
	if err != nil {
		return false, err
	}
	rollback := func(err error) (bool, error) {
		_ = tx.Rollback()
		return false, err
	}

	res, err := tx.ExecContext(ctx, reportStatusUpdateSQL, reportID, oldStatus, newStatus, adminID, at)
	if err != nil {
		return rollback(fmt.Errorf("update report status: %w", err))
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return rollback(fmt.Errorf("rows affected (update status): %w", err))
	}
	if affected == 0 {
		// 并发冲突：无行被改，不写审计，回滚（无副作用）并返回 false。
		_ = tx.Rollback()
		return false, nil
	}
	if err := r.audit.InsertTx(ctx, tx, entry); err != nil {
		return rollback(fmt.Errorf("insert status audit: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit status+audit: %w", err)
	}
	return true, nil
}

// reportInvalidateOutsideSQL 对账失效 SQL：把派生键集之外且未失效的报告写 invalidated_at。
// 由 InvalidateOutsideKeySet 独立暴露（单事务）。只动有效性维度；
// status 与审计列不出现（人工状态与审计全程保留）。
const reportInvalidateOutsideSQL = `
UPDATE usage_risk_reports r
SET invalidated_at = NOW()
WHERE r.invalidated_at IS NULL
  AND r.report_date = $1::date
  AND NOT EXISTS (
      SELECT 1
      FROM unnest($2::bigint[], $3::bigint[]) AS k(user_id, group_id)
      WHERE k.user_id = r.user_id AND k.group_id = r.group_id
  )
`

// ReconcileReportBatchUPSERTOnly 对账批次 UPSERT-only（单事务原子）：仅对 records 走
// reportDerivedUpsertSQL 派生列路径，绝不触发集合外失效。幂等、可重复——已提交的批在崩溃续跑
// 时不会被重复 UPSERT 之外的多余操作影响（阶段二明细是否重做由调用方游标控制）。
// 调用方负责在当日全部批走完后另行调用 InvalidateOutsideKeySet 完成失效（核心不变量）。
func (r *usageRiskRepository) ReconcileReportBatchUPSERTOnly(ctx context.Context, reportDate time.Time,
	records []UsageRiskReportRecord) error {
	tx, err := r.beginTxWithTimeout(ctx)
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}

	for _, rec := range records {
		if _, err := tx.ExecContext(ctx, reportDerivedUpsertSQL,
			rec.UserID, rec.GroupID, rec.ReportDate, rec.PolicyVersion, rec.Score, rec.Level,
			rec.RuleHits, rec.Evidence, rec.InvalidatedAt); err != nil {
			return rollback(fmt.Errorf("reconcile upsert-only derived: %w", err))
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reconcile upsert-only: %w", err)
	}
	return nil
}

// InvalidateOutsideKeySet 失效 reportDate 当日、键集（userIDs,groupIDs）之外且当前有效
// （invalidated_at IS NULL）的报告。单事务，只动有效性维度（status / 审计列不出现）。
//
// 核心不变量（派发单 U4b-R4）：失效只可能以"该日完整派生键集"触发——调用方必须传入当日全部候选
// 键集；传入空数组（nil 或长度 0）表示"该日无有效候选键集"，等价于失效该日全部有效旧行
// （与旧 ReconcileReportBatch(nil) 语义一致）。本方法绝不接受"部分键集"语义——调用方保证。
func (r *usageRiskRepository) InvalidateOutsideKeySet(ctx context.Context, reportDate time.Time,
	userIDs, groupIDs []int64) error {
	// 归一化 nil → 空切数组，保证 $2/$3 为 '{}'::bigint[]（unnest 零行 → NOT EXISTS 恒真 → 失效全部）。
	if userIDs == nil {
		userIDs = []int64{}
	}
	if groupIDs == nil {
		groupIDs = []int64{}
	}
	tx, err := r.beginTxWithTimeout(ctx)
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}

	if _, err := tx.ExecContext(ctx, reportInvalidateOutsideSQL,
		reportDate, pq.Array(userIDs), pq.Array(groupIDs)); err != nil {
		return rollback(fmt.Errorf("invalidate outside key set: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit invalidate outside key set: %w", err)
	}
	return nil
}

// InvalidateAllEffective 全量失效当前有效（invalidated_at IS NULL）的报告，单事务，只动
// 有效性维度（status/审计列不出现）。不按日期范围推测——覆盖保留期曾缩短/清理曾失败遗留的
// 范围外有效报告（外审⑨ 发现3；调用方为时区换日迁移）。
func (r *usageRiskRepository) InvalidateAllEffective(ctx context.Context) error {
	const q = `UPDATE usage_risk_reports SET invalidated_at = now() WHERE invalidated_at IS NULL`
	tx, err := r.beginTxWithTimeout(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, q); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("invalidate all effective: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit invalidate all effective: %w", err)
	}
	return nil
}

// ───────────────────────── 台账 usage_risk_runs ─────────────────────────

// CreateRun 创建 running 行并返回 run_id。
func (r *usageRiskRepository) CreateRun(ctx context.Context, rec UsageRiskRunRecord) (int64, error) {
	const q = `
INSERT INTO usage_risk_runs (
    run_at, window_start, window_end,
    policy_version, policy_snapshot, candidates_total,
    status
)
VALUES ($1, $2, $3, $4, $5, $6, 'running')
RETURNING run_id
`
	var runID int64
	if err := scanSingleRow(ctx, r.sql, q, []any{
		rec.RunAt, rec.WindowStart, rec.WindowEnd,
		rec.PolicyVersion, rec.PolicySnapshot, rec.CandidatesTotal,
	}, &runID); err != nil {
		return 0, fmt.Errorf("create run: %w", err)
	}
	return runID, nil
}

// UpdateRunProgress 更新进度字段（批次计数/游标三字段/偏移/候选累计）。
func (r *usageRiskRepository) UpdateRunProgress(ctx context.Context, runID int64, p UsageRiskRunProgress) error {
	const q = `
UPDATE usage_risk_runs
SET batches_done = $1,
    batches_failed = $2,
    failed_batches = $3,
    budget_exhausted = $4,
    candidates_total = $5,
    recon_batches_done = $6,
    recon_cursor_date = $7,
    recon_batch_offset = $8,
    history_covered = $9
WHERE run_id = $10
`
	if _, err := r.sql.ExecContext(ctx, q,
		p.BatchesDone, p.BatchesFailed, p.FailedBatches, p.BudgetExhausted,
		p.CandidatesTotal,
		p.ReconBatchesDone, p.ReconCursorDate, p.ReconBatchOffset,
		p.HistoryCovered, runID); err != nil {
		return fmt.Errorf("update run progress: %w", err)
	}
	return nil
}

// CompleteRun 完成/部分完成收敛：写 status + finished_at + failure_stage（+ 可选进度）。
func (r *usageRiskRepository) CompleteRun(ctx context.Context, runID int64, status string, p UsageRiskRunProgress) error {
	const q = `
UPDATE usage_risk_runs
SET status = $1,
    finished_at = $2,
    failure_stage = $3,
    batches_done = $4,
    batches_failed = $5,
    failed_batches = $6,
    budget_exhausted = $7,
    candidates_total = $8,
    recon_batches_done = $9,
    recon_cursor_date = $10,
    recon_batch_offset = $11,
    history_covered = $12,
    consecutive_partials = $13,
    r1_reeval_pending = $14
WHERE run_id = $15
`
	if _, err := r.sql.ExecContext(ctx, q,
		status, p.FinishedAt, p.FailureStage,
		p.BatchesDone, p.BatchesFailed, p.FailedBatches, p.BudgetExhausted,
		p.CandidatesTotal,
		p.ReconBatchesDone, p.ReconCursorDate, p.ReconBatchOffset,
		p.HistoryCovered, p.ConsecutivePartials, p.R1ReevalPending, runID); err != nil {
		return fmt.Errorf("complete run: %w", err)
	}
	return nil
}

// staleRunConvergeSQL 取锁后原子收敛超期限遗留 running 行为 partial。
// 防崩溃后永久 running：WHERE status='running' AND run_at < $1（超预算期限）。
// consecutive_partials 同口径递增：紧邻上一轮（run_at 更早）的值 +1（上一轮为 completed 则归 1）。
const staleRunConvergeSQL = `
UPDATE usage_risk_runs
SET status = 'partial',
    finished_at = NOW(),
    failure_stage = $2,
    consecutive_partials = COALESCE((SELECT consecutive_partials
                                     FROM usage_risk_runs r2
                                     WHERE r2.run_at < usage_risk_runs.run_at
                                     ORDER BY r2.run_at DESC LIMIT 1), 0) + 1
WHERE status = 'running' AND run_at < $1
`

// ConvergeStaleRuns 把超期限（run_at < staleBefore）的遗留 running 行收敛为 partial，
// 并写入 finished_at 与 failure_stage。返回收敛行数。
func (r *usageRiskRepository) ConvergeStaleRuns(ctx context.Context, staleBefore time.Time, failureStage string) error {
	if _, err := r.sql.ExecContext(ctx, staleRunConvergeSQL, staleBefore, failureStage); err != nil {
		return fmt.Errorf("converge stale runs: %w", err)
	}
	return nil
}

// LatestRun 返回最近一次 run（按 run_at DESC 取第一条）；无记录返回 (nil, nil)。
func (r *usageRiskRepository) LatestRun(ctx context.Context) (*service.UsageRiskRunStatus, error) {
	const q = `
SELECT status, window_end, consecutive_partials, batches_failed, history_covered,
       recon_cursor_date, window_start,
       policy_snapshot->>'tz' AS tz_name, r1_reeval_pending
FROM usage_risk_runs
ORDER BY run_at DESC
LIMIT 1
`
	rows, err := r.sql.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("latest run: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var st service.UsageRiskRunStatus
	var windowEnd, windowStart sql.NullTime
	var reconDate sql.NullTime
	var tzName sql.NullString
	var r1ReevalPending bool
	if err := rows.Scan(&st.Status, &windowEnd, &st.ConsecutivePartials, &st.FailedBatches,
		&st.HistoryCovered, &reconDate, &windowStart, &tzName, &r1ReevalPending); err != nil {
		return nil, fmt.Errorf("scan latest run: %w", err)
	}
	if tzName.Valid {
		st.TZName = tzName.String
	}
	st.R1ReevalPending = r1ReevalPending
	if windowEnd.Valid {
		t := windowEnd.Time
		st.WindowEnd = &t
	}
	// 对账进度描述：recon_cursor（对账游标日期） / 窗口终点（window_start=now-26h，
	// 即对账（重评）窗口最近端）；方案口径"重评进度 = 游标位置/窗口终点（终点=最近端）"。
	var reconEnd, reconCursor string
	if windowStart.Valid {
		reconEnd = windowStart.Time.Format("2006-01-02")
	}
	if reconDate.Valid {
		reconCursor = reconDate.Time.Format("2006-01-02")
	}
	if reconCursor == "" {
		reconCursor = "-"
	}
	if reconEnd == "" {
		reconEnd = "-"
	}
	st.ReconProgress = reconCursor + "/" + reconEnd
	return &st, nil
}

// LoadLatestReconCursor 只读补读最近一次 run（run_at DESC）的持久化游标三元组与失败批次，
// 供 U4b-R3 断点续跑。无记录返回 (nil, nil)；仅 SELECT，不改其他查询语义。
func (r *usageRiskRepository) LoadLatestReconCursor(ctx context.Context) (*service.UsageRiskReconCursor, error) {
	const q = `
SELECT recon_cursor_date, recon_batch_offset, failed_batches, history_covered
FROM usage_risk_runs
ORDER BY run_at DESC
LIMIT 1
`
	rows, err := r.sql.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("load latest recon cursor: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var (
		reconDate     sql.NullTime
		offset        sql.NullInt64
		failedBatches []byte
		historyCovered bool
	)
	if err := rows.Scan(&reconDate, &offset, &failedBatches, &historyCovered); err != nil {
		return nil, fmt.Errorf("scan latest recon cursor: %w", err)
	}
	cursor := &service.UsageRiskReconCursor{HistoryCovered: historyCovered}
	if reconDate.Valid {
		d := reconDate.Time
		cursor.CursorDate = &d
	}
	if offset.Valid {
		o := int(offset.Int64)
		cursor.BatchOffset = &o
	}
	if len(failedBatches) > 0 {
		cursor.FailedBatches = failedBatches
	}
	return cursor, nil
}

// ───────────────────────── 查询端 ─────────────────────────

// listReportsBaseSQL 是报告列表基础查询（不含分页/排序）。条件由 ListReports 动态拼接。
// 用户名/组名 join users/groups 只读 join，不落报告表。
const listReportsBaseSQL = `
SELECT r.report_id, r.user_id, COALESCE(u.username, ''),
       r.group_id, COALESCE(g.name, ''),
       r.report_date::text, r.score, r.level, r.rule_hits, r.status, r.invalidated_at
FROM usage_risk_reports r
LEFT JOIN users u ON u.id = r.user_id
LEFT JOIN groups g ON g.id = r.group_id
`

// ListReports 报告列表：过滤（日期/级别/用户/分组/规则 JSONB contains）、分页、score DESC。
// min_score 作为查询参数传入（禁止 SQL 内硬编码）；默认榜单过滤
// invalidated_at IS NULL AND status='open' AND score >= min_score，include_low 显式放开。
func (r *usageRiskRepository) ListReports(ctx context.Context, f service.UsageRiskListFilter, minScore int) ([]service.UsageRiskReportItem, int, error) {
	var conds []string
	args := make([]any, 0, 8)

	if fd := strings.TrimSpace(f.ReportDate); fd != "" {
		args = append(args, fd)
		conds = append(conds, "r.report_date = $"+strconv.Itoa(len(args))+"::date")
	}
	if lv := strings.TrimSpace(f.Level); lv != "" {
		args = append(args, lv)
		conds = append(conds, "r.level = $"+strconv.Itoa(len(args)))
	}
	if f.UserID > 0 {
		args = append(args, f.UserID)
		conds = append(conds, "r.user_id = $"+strconv.Itoa(len(args)))
	}
	if f.GroupID > 0 {
		args = append(args, f.GroupID)
		conds = append(conds, "r.group_id = $"+strconv.Itoa(len(args)))
	}
	if rule := strings.TrimSpace(f.Rule); rule != "" {
		// 规则 JSONB contains：rule_hits @> '[{"rule":"R1"}]'
		predBytes, err := json.Marshal([]struct {
			Rule string `json:"rule"`
		}{{Rule: rule}})
		if err != nil {
			return nil, 0, err
		}
		args = append(args, string(predBytes))
		conds = append(conds, "r.rule_hits @> $"+strconv.Itoa(len(args))+"::jsonb")
	}

	// 默认榜单过滤：有效 + 入榜线（删除 status='open'；open 仅为人工状态机初态，
	// 入榜过滤不应再绑定状态机可达性，方案状态机可达性由 UpdateStatus 路径保证）。
	// include_low 仅放开 score 过滤（保留对账失效过滤 invalidated_at IS NULL）。
	if !f.IncludeLow {
		conds = append(conds, "r.invalidated_at IS NULL")
		args = append(args, minScore)
		conds = append(conds, "r.score >= $"+strconv.Itoa(len(args)))
	} else {
		conds = append(conds, "r.invalidated_at IS NULL")
	}

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	countSQL := `SELECT COUNT(*) FROM usage_risk_reports r` + where
	var total int
	if err := scanSingleRow(ctx, r.sql, countSQL, args, &total); err != nil {
		return nil, 0, fmt.Errorf("count reports: %w", err)
	}

	page := f.Page
	if page <= 0 {
		page = 1
	}
	pageSize := f.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	args = append(args, pageSize, offset)
	query := listReportsBaseSQL + where + `
ORDER BY r.score DESC, r.report_id DESC
LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))

	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list reports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]service.UsageRiskReportItem, 0, pageSize)
	for rows.Next() {
		var item service.UsageRiskReportItem
		var ruleHitsRaw []byte
		var inv sql.NullTime
		if err := rows.Scan(&item.ReportID, &item.UserID, &item.Username,
			&item.GroupID, &item.GroupName, &item.ReportDate, &item.Score, &item.Level,
			&ruleHitsRaw, &item.Status, &inv); err != nil {
			return nil, 0, fmt.Errorf("scan report item: %w", err)
		}
		if inv.Valid {
			t := inv.Time
			item.InvalidatedAt = &t
		}
		if err := json.Unmarshal(ruleHitsRaw, &item.RuleHits); err != nil {
			item.RuleHits = []service.RuleHit{}
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// GetReportDetail 按 report_id 返回详情（含 evidence/policy_version）。
// 未找到返回 service.ErrUsageRiskReportNotFound；已失效（invalidated_at 非空）按
// not-found 返回（R10-1/#6，与公开契约对齐）。
func (r *usageRiskRepository) GetReportDetail(ctx context.Context, reportID int64) (*service.UsageRiskReportDetail, error) {
	const q = `
SELECT r.report_id, r.user_id, COALESCE(u.username, ''),
       r.group_id, COALESCE(g.name, ''),
       r.report_date::text, r.score, r.level, r.rule_hits, r.status, r.invalidated_at,
       r.evidence, r.policy_version
FROM usage_risk_reports r
LEFT JOIN users u ON u.id = r.user_id
LEFT JOIN groups g ON g.id = r.group_id
WHERE r.report_id = $1 AND r.invalidated_at IS NULL
`
	var d service.UsageRiskReportDetail
	var ruleHitsRaw, evidenceRaw []byte
	var inv sql.NullTime
	var pv int64
	if err := scanSingleRow(ctx, r.sql, q, []any{reportID},
		&d.ReportID, &d.UserID, &d.Username, &d.GroupID, &d.GroupName,
		&d.ReportDate, &d.Score, &d.Level, &ruleHitsRaw, &d.Status, &inv,
		&evidenceRaw, &pv); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrUsageRiskReportNotFound
		}
		return nil, fmt.Errorf("get report detail: %w", err)
	}
	if inv.Valid {
		t := inv.Time
		d.InvalidatedAt = &t
	}
	if err := json.Unmarshal(ruleHitsRaw, &d.RuleHits); err != nil {
		d.RuleHits = []service.RuleHit{}
	}
	d.Evidence = evidenceRaw
	d.PolicyVersion = fmt.Sprintf("%016x", uint64(pv))
	return &d, nil
}

// RiskSummary Dashboard 聚合：open 计数、级别分布、按 user_id 取 MAX(score) 的 Top N。
// 用户摘要不重复累加 scope=user 分（按 user 取有效分组报告行的 MAX(score)）。
// 入榜线 min_score 参数传入，禁止 SQL 内硬编码。
func (r *usageRiskRepository) RiskSummary(ctx context.Context, minScore, topN int) (*service.UsageRiskSummary, error) {
	if topN <= 0 {
		topN = 5
	}
	summary := &service.UsageRiskSummary{ByLevel: map[string]int{"low": 0, "medium": 0, "high": 0, "critical": 0}, Top: []service.UsageRiskTopUser{}}

	// open 计数 + 级别分布（仅有效 + open + 入榜线）
	const aggQ = `
SELECT r.level, COUNT(*)
FROM usage_risk_reports r
WHERE r.invalidated_at IS NULL AND r.status = 'open' AND r.score >= $1
GROUP BY r.level
`
	rows, err := r.sql.QueryContext(ctx, aggQ, minScore)
	if err != nil {
		return nil, fmt.Errorf("risk summary aggregate: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var level string
		var n int
		if err := rows.Scan(&level, &n); err != nil {
			return nil, fmt.Errorf("scan risk summary level: %w", err)
		}
		summary.ByLevel[level] = n
		summary.OpenTotal += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Top N 用户：按 user_id 跨分组取有效报告行 MAX(score)，再取级别与命中规则。
	const topQ = `
SELECT user_id, MAX(score) AS max_score
FROM usage_risk_reports
WHERE invalidated_at IS NULL AND status = 'open' AND score >= $1
GROUP BY user_id
ORDER BY max_score DESC, user_id
LIMIT $2
`
	topRows, err := r.sql.QueryContext(ctx, topQ, minScore, topN)
	if err != nil {
		return nil, fmt.Errorf("risk summary top: %w", err)
	}
	defer func() { _ = topRows.Close() }()

	type topEntry struct {
		userID  int64
		maxScore int
	}
	entries := make([]topEntry, 0, topN)
	for topRows.Next() {
		var e topEntry
		if err := topRows.Scan(&e.userID, &e.maxScore); err != nil {
			return nil, fmt.Errorf("scan risk summary top: %w", err)
		}
		entries = append(entries, e)
	}
	if err := topRows.Err(); err != nil {
		return nil, err
	}

	for _, e := range entries {
		u := service.UsageRiskTopUser{UserID: e.userID, MaxScore: e.maxScore, TopRules: []string{}}
		// 取最高分行：级别 + 命中规则（去重）
		var ruleHitsRaw []byte
		var level string
		var username string
		if err := scanSingleRow(ctx, r.sql, `
SELECT COALESCE(u.username, ''), r.level, r.rule_hits
FROM usage_risk_reports r
LEFT JOIN users u ON u.id = r.user_id
WHERE r.user_id = $1 AND r.invalidated_at IS NULL AND r.status = 'open'
ORDER BY r.score DESC, r.report_id DESC
LIMIT 1
`, []any{e.userID}, &username, &level, &ruleHitsRaw); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("scan top user row: %w", err)
		}
		u.Username = username
		u.Level = level
		var hits []service.RuleHit
		if err := json.Unmarshal(ruleHitsRaw, &hits); err == nil {
			seen := map[string]struct{}{}
			for _, h := range hits {
				if h.Rule == "" {
					continue
				}
				if _, ok := seen[h.Rule]; ok {
					continue
				}
				seen[h.Rule] = struct{}{}
				u.TopRules = append(u.TopRules, h.Rule)
			}
		}
		if len(u.TopRules) == 0 {
			u.TopRules = []string{}
		}
		summary.Top = append(summary.Top, u)
	}
	return summary, nil
}

// ───────────────────────── 清理（可在外部事务执行） ─────────────────────────

// CleanupReportAndRollupTx 在外部传入的 executor（*sql.DB 或 *sql.Tx）上执行两清。
// 报告按 report_date 截止、rollup 按 bucket_hour 截止，各自按年龄截止的删除幂等。
// 返回 (删除报告行数, 删除 rollup 行数)。
func (r *usageRiskRepository) CleanupReportAndRollupTx(ctx context.Context, exec sqlExecutor, reportCutoff, rollupCutoff time.Time) (int64, int64, error) {
	resReport, err := exec.ExecContext(ctx,
		`DELETE FROM usage_risk_reports WHERE report_date < $1::date`, reportCutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("cleanup reports: %w", err)
	}
	nReport, err := resReport.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("rows affected (cleanup reports): %w", err)
	}

	resRollup, err := exec.ExecContext(ctx,
		`DELETE FROM user_usage_metrics_rollup WHERE bucket_hour < $1`, rollupCutoff)
	if err != nil {
		return nReport, 0, fmt.Errorf("cleanup rollup: %w", err)
	}
	nRollup, err := resRollup.RowsAffected()
	if err != nil {
		return nReport, 0, fmt.Errorf("rows affected (cleanup rollup): %w", err)
	}
	return nReport, nRollup, nil
}

// CleanupReportAndRollup 便捷版本：自开事务执行两清（供接入方直接调用；接入方若需与
// 源日志清理同事务，请调用 CleanupReportAndRollupTx 传入自身事务）。
func (r *usageRiskRepository) CleanupReportAndRollup(ctx context.Context, reportCutoff, rollupCutoff time.Time) (int64, int64, error) {
	tx, err := r.beginTxWithTimeout(ctx)
	if err != nil {
		return 0, 0, err
	}
	nReport, nRollup, err := r.CleanupReportAndRollupTx(ctx, tx, reportCutoff, rollupCutoff)
	if err != nil {
		_ = tx.Rollback()
		return nReport, nRollup, err
	}
	if err := tx.Commit(); err != nil {
		return nReport, nRollup, fmt.Errorf("commit cleanup: %w", err)
	}
	return nReport, nRollup, nil
}





