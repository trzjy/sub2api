package service

import (
	"context"
	"encoding/json"
	"time"
)

// 本文件定义 U4b 编排层消费的仓储接口与 DTO（消费者定义接口，Go 惯例）。
//
// 重要（方案 §5 / 派发单禁区）：repository 包反向 import 本 service 包
// （account_credentials_crypt.go 引用 service.SensitiveCredentialKeys），
// 故本 service 包不得 import repository，否则形成 import cycle。
// 因此 U4b 在此定义最小依赖接口与 DTO，并由独立适配包（usageriskadapter）
// 将 *repository.UsageRiskRepository 适配到本接口（做 DTO 翻译），不改 U3 文件。
//
// 注：以下 DTO 字段严格对齐 repository.UsageRiskRepository 的导出结构
// （usage_risk_repo.go），仅作类型本地化，不做语义改动。

// AggregateParams 是聚合查询的资格谓词参数（不参与规则判断）。
type AggregateParams struct {
	UnlimitedGroupsOnly bool
	UAWhitelist        []string
}

// UsageRiskUserGroupKey 是 (user_id, group_id) 复合键。
type UsageRiskUserGroupKey struct {
	UserID  int64
	GroupID int64
}

// UsageRiskHourAggregate 是小时桶聚合事实。
type UsageRiskHourAggregate struct {
	UserID                int64
	GroupID               int64
	BucketHour            time.Time
	RequestCount          int
	InputTokensSum        int64
	OutputTokensSum       int64
	CacheReadTokensSum    int64
	CostUSDSum            float64
	OccupiedMsSum         int64
	NonWhitelistedUACount int
}

// UsageRiskRPMLimit 是用户-分组作用域限额输入（适用分组上限，调用方按 checkRPM 语义算好）。
type UsageRiskRPMLimit struct {
	UserID     int64
	GroupID    int64
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
	ConsecutivePartials int // 连续 partial 计数（completed 重置 0，否则上一轮+1；M）
	R1ReevalPending     bool // 覆盖翻转后待重评 R1 标记（E）
	CandidatesTotal     int // 本轮累计候选数（L137 台账契约；由进度路径跨日累计）
	FinishedAt          *time.Time
	FailureStage        *string
}

// UsageRiskReconCursor 是上一轮持久化的对账游标（日期+批内偏移二元）+ 失败批次（只读查询返回）。
// 用于常态轮从断点续跑（避免每轮从 retentionStart 全量重放导致的活锁）：
//   - CursorDate：下一待处理 older 日（StartOfDay）；无游标时为 nil（回退 retentionStart）。
//   - BatchOffset：批内偏移（同日内崩溃续跑位置标记）；本实现按日重置为 0（见 runAnalysis 注释）。
//   - FailedBatches：序列化后的失败批次列表（reconFailedBatch 数组），下轮优先重试。
//   - HistoryCovered：冗余携带（runAnalysis 仍以 LatestRun 的 HistoryCovered 为准）。
type UsageRiskReconCursor struct {
	CursorDate     *time.Time
	BatchOffset    *int
	FailedBatches  json.RawMessage
	HistoryCovered bool
}

// UsageRiskRepository 是 U4b 编排直接消费的仓储接口（消费者定义接口；具体实现见 usageriskadapter）。
//
// 方法集严格对齐 repository.UsageRiskRepository 中 U4b 实际调用的部分；U4b 不调用
// UpsertReportDerived（由 ReconcileReportBatchUPSERTOnly 内部 UPSERT）与清理类方法（保留期清理由
// 既有 retention 作业按方案 §6.4 在源日志清理同事务内调用，不走本接口）。
type UsageRiskRepository interface {
	// ── 阶段一：聚合 ──
	AggregateHourly(ctx context.Context, windowStart, windowEnd time.Time,
		p AggregateParams) ([]UsageRiskHourAggregate, error)
	AggregateGroupRPMMinutes(ctx context.Context, windowStart, windowEnd time.Time,
		p AggregateParams, limits []UsageRiskRPMLimit, ratio float64) (map[UsageRiskUserGroupKey]int, error)
	AggregateUserGlobalRPMMinutes(ctx context.Context, windowStart, windowEnd time.Time,
		limits []UsageRiskUserLimit, ratio float64) (map[int64]int, error)
	ListCandidates(ctx context.Context, windowStart, windowEnd time.Time, p AggregateParams,
		minDailyRequests int, offset, batchSize int) ([]UsageRiskCandidate, error)
	RebuildHourlyWindow(ctx context.Context, windowStart, windowEnd time.Time,
		p AggregateParams) (int64, error)

	// ── 阶段二：明细 ──
	DistinctIPs(ctx context.Context, userID int64, dayStart, dayEnd time.Time,
		p AggregateParams) ([]string, error)
	IPAssociatedUsers(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]IPClusterFact, error)
	UADistribution(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]int, error)
	KeyUsageDistribution(ctx context.Context, userID int64, dayStart, dayEnd time.Time, p AggregateParams) (map[string]int, error)
	ActiveHourHeatmap(ctx context.Context, userID int64, dayStart, dayEnd time.Time,
		tzName string, p AggregateParams) (map[int]int, error)
	R2PeerDayCosts(ctx context.Context, groupID int64, dayStart, dayEnd time.Time,
		p AggregateParams, minDailyRequests int) ([]UsageRiskPeerCost, error)

	// ── 报告写入（列级隔离） ──
	UpdateReportStatus(ctx context.Context, reportID int64, oldStatus, newStatus string,
		adminID int64, at time.Time) (bool, error)
	// UpdateStatusWithAudit 状态变更与审计事件同事务落账（U4b-R1，方案 §5:91）。
	UpdateStatusWithAudit(ctx context.Context, reportID int64, oldStatus, newStatus string,
		adminID int64, at time.Time, entry *AuditLog) (bool, error)
	// ReconcileReportBatchUPSERTOnly 对账批次 UPSERT-only（单事务，仅派生列 UPSERT，绝不失集外失效）。
	ReconcileReportBatchUPSERTOnly(ctx context.Context, reportDate time.Time, records []UsageRiskReportRecord) error
	// InvalidateOutsideKeySet 失效 reportDate 当日键集（userIDs,groupIDs）之外的有效报告；
	// 空数组 = 失效该日全部有效旧行（等价 nil 派生集语义）。单事务，只动有效性维度。
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
	LatestRun(ctx context.Context) (*UsageRiskRunStatus, error)
	// LoadLatestReconCursor 只读补读上一轮持久化游标三元组（日期+键区间+批内偏移）+ 失败批次；
	// 与 LatestRun（已含 HistoryCovered）配合构成断点续跑所需的完整进度。新增只读查询，不改既有 SQL 语义。
	LoadLatestReconCursor(ctx context.Context) (*UsageRiskReconCursor, error)

	// ── 查询端 ──
	// UserActiveBucketHours 只读回看窗口内某用户 distinct 活跃桶（rollup），替代逐日重聚合（I）。
	UserActiveBucketHours(ctx context.Context, userID int64, lookbackStart, reportDay time.Time) ([]time.Time, error)

	ListReports(ctx context.Context, f UsageRiskListFilter, minScore int) ([]UsageRiskReportItem, int, error)
	GetReportDetail(ctx context.Context, reportID int64) (*UsageRiskReportDetail, error)
	RiskSummary(ctx context.Context, minScore, topN int) (*UsageRiskSummary, error)
}
