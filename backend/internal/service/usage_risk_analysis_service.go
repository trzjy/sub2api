package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/google/uuid"
)

// 本文件实现异常调用分析的 job 编排与对外服务（派发单 U4b）。
//
// 设计约束（方案 §5 / 派发单禁区）：
//   - 纯编排：规则打分与证据装配全部委托 U4a（EvaluateRules / AssembleEvidence），本文件不重写规则逻辑。
//   - 不绕过 U3 repository 自建第二条 SQL 路径：所有 usage_logs / rollup / reports / candidates 查询
//     一律经 UsageRiskRepository 导出方法；本文件仅读取 users/groups 的 rpm_limit 配置
//     （非分析表，ent 正统访问）与 user_group_rate 的 override（U3 既有接口）。
//   - 不在 service 内做兜底打分或静默降级；限额读取失败按批次错误记录、下轮重试，不吞。
//   - 状态更新只经 UpdateStatus（列级隔离 + 原状态条件），重算路径绝不写 status 列。
//
// 关于 U3 接口未覆盖的事实的取舍（方案 §5）：
//   - 日级分组事实（DayFacts：请求数 / token / cost / occupied / 桶起点 / 分组 RPM 贴线分钟）由本编排层
//     通过 U3 的 AggregateHourly（小时桶）在单日内聚合并按 (user,group) 汇总得到——这正是方案“小时桶 → 日级指标”
//     的转换，复用既有方法，不新增 SQL 路径。
//   - R3a 限额选择语义严格复用 checkRPM（billing_cache_service.go:790）：override 替代分组限、override=0 免除、
//     用户级 rpm_limit 全局天花板。限额取值为分析时当前配置（users.rpm_limit / groups.rpm_limit / override），
//     经 ent 与 userGroupRateRepo 读取，evidence 记录限额快照与计算时间。
//   - R1 连续活跃天数取自 rollup 历史桶（方案 §4）：本层通过 U3 AggregateHourly 在回看窗口逐日扫描累计，
//     历史未覆盖时记 insufficient_history（0 分、不入榜），与 U4a 口径一致。

const (
	// usageRiskLeaderLockKey 保证多副本部署中每个周期只有一个实例执行分析（对齐 dashboard 惯例）。
	usageRiskLeaderLockKey = "usage:risk:analysis:leader"
	// usageRiskLeaderLockTTL 必须覆盖硬截止预算；取 5m 与 dashboard 一致。
	usageRiskLeaderLockTTL = 5 * time.Minute

	// usageRiskTotalBudget 是单轮运行的硬截止总预算，严格小于锁 TTL（5m），
	// 保证租期内不会有第二副本并发删除重建同一窗口（方案 §5）。
	usageRiskTotalBudget = 4 * time.Minute

	// usageRiskNearWindow 是“近窗”：每次运行事务化删除重建的小时桶范围（方案 §5：26h）。
	usageRiskNearWindow = 26 * time.Hour

	// usageRiskDefaultInterval 是每小时 job 的调度周期。
	usageRiskDefaultInterval = time.Hour

	// usageRiskReconBatchKeyLimit 是有界批次的键数上界（阶段二明细 ≤100/批；对账批次同口径）。
	usageRiskReconBatchKeyLimit = 100

	// usageRiskReserveReconBatches 是“对账预留预算先行”每轮至少完成的 older 有界批次数，
	// 保证对账单调推进不被候选批次饿死（方案 §5）。
	usageRiskReserveReconBatches = 1
)

// 报告四态状态机取值（方案 §6.2 冻结）。
const (
	statusUsageRiskOpen        = "open"
	statusUsageRiskAcknowledged = "acknowledged"
	statusUsageRiskResolved     = "resolved"
	statusUsageRiskDismissed    = "dismissed"
)

// UsageRiskRepository 是 U4b 编排直接消费的仓储接口，定义为本 service 包内的
// 消费者接口（见 usage_risk_repo_contract.go）。因 repository 包反向 import 本包，
// 不可直接引用 UsageRiskRepository，故在此本地定义并由 usageriskadapter 适配。

// UsageRiskAnalysisService 实现异常调用分析的全部编排与对外 API。
type UsageRiskAnalysisService struct {
	repo              UsageRiskRepository
	client            *ent.Client
	userGroupRateRepo UserGroupRateRepository
	timingWheel       *TimingWheelService
	lockCache         LeaderLockCache
	db                *sql.DB
	settingSvc        *SettingService
	auditLog          *AuditLogService
	cfg               *config.Config

	instanceID string
	running    int32

	interval time.Duration
	budget   time.Duration

	// finalizeTimeout 是每次收尾写（进度持久化 / CompleteRun）的独立超时。收尾写不共享
	// 一个贯穿全 run 的 ctx（R8-2 外审 #3）：每次写作新建 ctx，避免 run 运行超过单超时阈值后
	// 所有收尾写拿到已过期 ctx（run 遗留 running、台账失真）。构造默认 10s，测试可注入短超时。
	finalizeTimeout time.Duration

	// in-memory 版本缓存：用于检测策略变化触发全量重评；跨进程重启后丢失会退化为
	// “首次运行视为变化 → 全量重评”（幂等、正确，仅多一次重评）。
	// 时区变化检测已改用上一轮快照 prevRun.TZName（J：跨重启持久化，不再依赖本缓存）。
	lastPolicyVersion string

	// 配置加载失败（保留期绑定违反） → 失败关闭：job 不调度、Get* 暴露错误态。
	failedClosed bool
	failedErr    error

	// nowFn 用于测试注入时钟。
	nowFn func() time.Time
	// policyFn 用于测试注入策略读取；生产为 nil，退化为 settingSvc.LoadUsageRiskPolicy。
	// 对齐 nowFn 测试注入惯例：API 取 min_score 等路径经 currentPolicy，测试可绕过 settingSvc 依赖。
	policyFn func(ctx context.Context) (UsageRiskPolicy, error)
}

// 编译期断言：本服务实现 U4c 契约接口。
var _ UsageRiskService = (*UsageRiskAnalysisService)(nil)

// now 返回当前时间（测试可注入 nowFn）。
func (s *UsageRiskAnalysisService) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// finalizeWrite 为「一次收尾写」派生独立 ctx（脱离业务 ctx 的取消传播 + 独立超时）。
//
// N 语义（背景）：收尾进度/台账写必须即便业务 ctx 已取消（预算耗尽/上游错误）也能落账，
// 否则 run 永远滞留 running、台账失真。
// R8-2/F1 语义：每次写都新建 ctx。若在 run 开跑时一次性创建（贯穿全 run 的总预算内共用），
// run 超过该超时阈值后所有收尾写都会拿到已过期 ctx——生产 run 普遍远超 10s，恰好是墙钟盲区
// （既有测试全在 10s 墙钟内跑完故全绿）。故本方法每次调用新建：每写一份完整超时预算。
//
// 用法：fctx, fcancel := s.finalizeWrite(ctx); defer fcancel()；该次写用 fctx，
// 业务 SQL 仍用原 ctx。
func (s *UsageRiskAnalysisService) finalizeWrite(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.finalizeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second // 结构体直接构造（测试/零值）时退化为默认 10s，绝不 0 超时。
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// ensureProgressJSON 保证进度写库前 FailedBatches 为有效 JSON 数组：nil json.RawMessage 经
// pq 驱动发 SQL NULL，违反 failed_batches JSONB NOT NULL（外审⑥ #3——partial 收尾字面量曾因此
// 写库失败且被吞错，run 停留 running）；'null' 串虽合法但统一归一为 [] 消除口径分叉。
func ensureProgressJSON(p *UsageRiskRunProgress) {
	if len(p.FailedBatches) == 0 || string(p.FailedBatches) == "null" {
		p.FailedBatches = json.RawMessage("[]")
	}
}

// NewUsageRiskAnalysisService 构造异常调用分析服务（不启动调度，由 Provider 调用 Start）。
func NewUsageRiskAnalysisService(
	repo UsageRiskRepository,
	client *ent.Client,
	userGroupRateRepo UserGroupRateRepository,
	timingWheel *TimingWheelService,
	lockCache LeaderLockCache,
	db *sql.DB,
	settingSvc *SettingService,
	auditLog *AuditLogService,
	cfg *config.Config,
) *UsageRiskAnalysisService {
	interval := usageRiskDefaultInterval
	budget := usageRiskTotalBudget
	return &UsageRiskAnalysisService{
		repo:              repo,
		client:            client,
		userGroupRateRepo: userGroupRateRepo,
		timingWheel:       timingWheel,
		lockCache:         lockCache,
		db:                db,
		settingSvc:        settingSvc,
		auditLog:          auditLog,
		cfg:               cfg,
		instanceID:        uuid.NewString(),
		interval:          interval,
		budget:            budget,
		finalizeTimeout:   10 * time.Second,
		nowFn:             time.Now,
	}
}

// Start 启动定时分析作业；若配置加载（保留期绑定）失败则失败关闭（不调度、结构化日志错误态）。
func (s *UsageRiskAnalysisService) Start() {
	if s == nil || s.repo == nil || s.timingWheel == nil {
		return
	}
	// 启动重验：保留期绑定违反 → 分析任务失败关闭（方案 §5 / §6.4：拒绝启动、不静默运行）。
	if _, _, _, err := s.loadPolicy(context.Background()); err != nil {
		s.failedClosed = true
		s.failedErr = err
		slog.Error("[UsageRiskAnalysis] 启动保留期重验失败，分析 job 失败关闭（拒绝启动）",
			"error", err)
		return
	}
	s.timingWheel.ScheduleRecurring("usage:risk:analysis", s.interval, func() {
		s.runHourly()
	})
	slog.Info("[UsageRiskAnalysis] 分析作业已启动",
		"interval", s.interval.String(), "budget", s.budget.String())
}

// Stop 释放资源（与 DashboardAggregation 一致，无显式 stop 接口，仅复位 running 标记）。
func (s *UsageRiskAnalysisService) Stop() {
	if s == nil {
		return
	}
	atomic.StoreInt32(&s.running, 0)
}

// runHourly 是定时触发的入口：单实例互斥（running 标记，避免同进程重入）+ leader 锁多副本互斥。
func (s *UsageRiskAnalysisService) runHourly() {
	if !atomic.CompareAndSwapInt32(&s.running, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&s.running, 0)

	if s.failedClosed {
		slog.Error("[UsageRiskAnalysis] 分析 job 处于失败关闭态，跳过本轮", "error", s.failedErr)
		return
	}

	now := s.now()
	ctx, cancel := context.WithTimeout(context.Background(), s.budget)
	defer cancel()

	release, ok := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, usageRiskLeaderLockKey, s.instanceID, usageRiskLeaderLockTTL)
	if !ok {
		// 锁被对等实例持有：本周期跳过，不静默失败（下周期再争抢）。
		return
	}
	defer release()

	if err := s.runAnalysis(ctx, now); err != nil {
		slog.Error("[UsageRiskAnalysis] 分析运行失败", "error", err)
	}
}

// loadPolicy 读取策略 + 时区名 + 版本哈希；任何校验失败（含保留期绑定违反）一律失败关闭。
// B1：若测试注入的 policyFn 非空，优先走 policyFn（归一化版本/时区），对齐 :105 注释与
// currentPolicy 先例；版本哈希仍按返回 policy + 时区名计算。生产为 nil，退化为 settingSvc。
func (s *UsageRiskAnalysisService) loadPolicy(ctx context.Context) (UsageRiskPolicy, string, string, error) {
	if s.policyFn != nil {
		policy, err := s.policyFn(ctx)
		if err != nil {
			return UsageRiskPolicy{}, "", "", fmt.Errorf("加载异常调用分析策略失败(policyFn): %w", err)
		}
		tzName := timezone.Name()
		version := UsageRiskPolicyVersion(policy, tzName)
		return policy, version, tzName, nil
	}
	if s.settingSvc == nil {
		return UsageRiskPolicy{}, "", "", errors.New("setting service 未注入，无法加载策略")
	}
	policy, err := s.settingSvc.LoadUsageRiskPolicy(ctx)
	if err != nil {
		return UsageRiskPolicy{}, "", "", fmt.Errorf("加载异常调用分析策略失败: %w", err)
	}
	tzName := timezone.Name()
	policy.TZName = tzName
	version := UsageRiskPolicyVersion(policy, tzName)
	return policy, version, tzName, nil
}

// runAnalysis 是单轮分析主体：收敛超期 running 台账 → 近窗重建 → 候选/对账分批打分 → 台账收尾。
//
// 游标增量对账（派发单 U4b-R3 / 方案 §5）：常态轮从上一轮持久化游标（日期+键区间+批内偏移）续跑
// older 窗口，不再从 retentionStart 全量重放（避免保留窗口超出预算时的活锁）；游标单调推进
// （日期只进不退）。fullReeval（版本/时区变化）重置游标到 retentionStart 全量重评；时区变更先走
// 换日迁移。history_covered 翻转后常态轮跳过 older 重放（稳态效率），只处理近窗。失败批次下轮优先重试。
func (s *UsageRiskAnalysisService) runAnalysis(ctx context.Context, now time.Time) error {
	policy, version, tzName, err := s.loadPolicy(ctx)
	if err != nil {
		return err
	}

	// C：全局开关关闭 → 零副作用退出（不建 run、不重建桶、不产报告；调度保持运转，重新开启后自动恢复）。
	if !policy.Enabled {
		slog.Info("[UsageRiskAnalysis] 全局开关关闭，本轮跳过（无 DB 副作用）", "tz", tzName)
		s.lastPolicyVersion = "" // B：关闭分支标记版本失效——重新启用（即使同阈值同版本）必触发 fullReeval，关闭超 26h 的历史不再漏算
		return nil
	}

	// N：收尾写用独立 ctx（仅本 run 自身进度/收尾写使用，业务 SQL 仍用原 ctx）。
	//    业务 SQL 全部仍用原 ctx；只有本 run 自身的进度/收尾写用新 ctx——即使业务 ctx 被取消，
	//    收尾进度与台账也能独立落账（避免“预算耗尽→收尾失败→run 永远 running”）。
	//    R8-2/F1：每次写作新建 ctx（finalizeWrite），不再 run 开跑时单次创建（整体预算内共用）
	//    ——生产 run 远超单次 10s 阈值后，单次创建的 ctx 已过期导致后续进度写与收尾全部失效（外审 #3）。

	// 1) 收敛超期限遗留 running 行 → partial（写入 finished_at + failure_stage，防崩溃后永久 running）。
	staleBefore := now.Add(-s.budget)
	if err := s.repo.ConvergeStaleRuns(ctx, staleBefore, "stale_timeout"); err != nil {
		slog.Warn("[UsageRiskAnalysis] 收敛超期 run 失败", "error", err)
	}

	// B/J：先读上一轮（此时最近行是上一轮的），用于覆盖判定/时区判定/连续 partial 计数/重评 pending。
	//     必须在 CreateRun 之前读取，否则会读到本轮刚插入的 running 行（R3 活锁/读己误判根因）。
	prevRun, lerr := s.repo.LatestRun(ctx)
	if lerr != nil {
		return fmt.Errorf("读取上一轮 run 失败（失败关闭）: %w", lerr)
	}
	prevHistoryCovered := prevRun != nil && prevRun.HistoryCovered
	prevR1ReevalPending := prevRun != nil && prevRun.R1ReevalPending
	prevConsecutivePartials := 0
	if prevRun != nil {
		prevConsecutivePartials = prevRun.ConsecutivePartials
	}

	// B/J：读上一轮持久化游标三元组 + 失败批次（与 LatestRun 配合构成断点续跑完整进度）。
	cursor, cerr := s.repo.LoadLatestReconCursor(ctx)
	if cerr != nil {
		return fmt.Errorf("读取对账游标失败（失败关闭，不静默全量重放）: %w", cerr)
	}

	// 外审⑨ 发现2：失败批次游标解析失败时失败关闭。跨版本部署/回滚/人工恢复可产生合法 JSONB
	// 但结构不符的形态；静默当空列表会让本轮推进 history_covered 并以 [] 覆盖原状态，
	// 永久丢失未重试批次。保留原游标待修复，本轮不建 run、不更新任何状态（与时区迁移
	// 失败关闭同一模式，见 :330 注释）。
	var failedBatches []reconFailedBatch
	if cursor != nil && len(cursor.FailedBatches) > 0 {
		if uerr := json.Unmarshal(cursor.FailedBatches, &failedBatches); uerr != nil {
			return fmt.Errorf("解析失败批次游标失败（保留原游标本轮失败关闭，不降级为空列表）: %w", uerr)
		}
	}

	// 2) 策略/时区变化检测 → 驱动保留窗口全量重评；时区变更走换日迁移。
	//    J：时区变更判定来自上一轮快照 prevRun.TZName（跨重启持久化），不再依赖进程内 lastTZ；
	//    版本变化仍用进程内 lastPolicyVersion 检测（重启首轮退化为全量重评，幂等正确）。
	fullReeval := prevRun == nil // 冷启动：全窗口重评
	if prevRun != nil && (s.lastPolicyVersion != version || (prevRun.TZName != "" && prevRun.TZName != tzName)) {
		fullReeval = true
	}
	tzChanged := prevRun != nil && prevRun.TZName != "" && prevRun.TZName != tzName
	// J：时区变更判定来自上一轮快照 prevRun.TZName（跨重启持久化）；版本变化检测用上一轮的
	// lastPolicyVersion（比较在本行之前完成）。版本缓存的**推进**移至 CreateRun 成功之后
	// （R10-2/#4）：若在此处先推进而 CreateRun 临时失败，下一轮版本比较相等 → fullReeval
	// 不触发 → 新策略要求的保留窗口重评永久丢失。

	// 时区变更：换日迁移失败 → 本轮失败关闭（不 CreateRun、不更新任何状态），下轮重试。
	if tzChanged {
		if err := s.handleTimezoneChange(ctx, now, policy, tzName); err != nil {
			return fmt.Errorf("时区变更换日迁移失败（本轮失败关闭）: %w", err)
		}
	}

	windowEnd := now
	windowStart := now.Add(-usageRiskNearWindow)

	// 3) 创建 running run（持久化策略快照 JSONB）。
	snapshot, _ := json.Marshal(policy)
	snapshotJSON := append(json.RawMessage(nil), snapshot...)
	// A/F1：位宽保护——策略版本哈希为 16 进制 64 位无符号；≥2^63 时 ParseInt 会越界 err，
	// 必须 ParseUint + 位保真 int64(u)；解析失败视为配置/快照损坏，本轮失败关闭（不静默 0）。
	versionU, verr := parsePolicyVersionHex(version)
	if verr != nil {
		return fmt.Errorf("解析策略版本失败（失败关闭，不静默 0）: %w", verr)
	}
	runID, err := s.repo.CreateRun(ctx, UsageRiskRunRecord{
		RunAt:          now,
		WindowStart:    windowStart,
		WindowEnd:      windowEnd,
		PolicyVersion:  int64(versionU),
		PolicySnapshot: snapshotJSON,
		CandidatesTotal: 0,
	})
	if err != nil {
		return fmt.Errorf("创建 run 失败: %w", err)
	}

	// R10-2/#4：仅在本轮 run 成功持久化后推进版本缓存——失败路径不推进，下轮仍按版本
	// 变化触发 fullReeval（tz 变更走 prevRun.TZName 持久化，不受本缓存影响）。
	s.lastPolicyVersion = version

	progress := UsageRiskRunProgress{}

	failureStage := ""

	// 4) 近窗事务化删除重建小时桶。
	if _, err := s.repo.RebuildHourlyWindow(ctx, windowStart, windowEnd, aggParams(policy)); err != nil {
		failureStage = "aggregate"
		fctx, fcancel := s.finalizeWrite(ctx)
		// R10-2/#3：partial 收尾不再吞错（原 `_ =` 使 NOT NULL 违反静默、run 停留 running）；
		// FailedBatches 归一为 []（nil 经 pq 发 SQL NULL）。收尾失败仅告警，不覆盖主错误。
		partialP := UsageRiskRunProgress{
			FailureStage: ptrStr(failureStage),
			FinishedAt:   ptrTime(s.now()),
			BudgetExhausted: true,
			ConsecutivePartials: consecutivePartialOf("partial", prevConsecutivePartials),
		}
		ensureProgressJSON(&partialP)
		if perr := s.repo.CompleteRun(fctx, runID, "partial", partialP); perr != nil {
			slog.Warn("[UsageRiskAnalysis] partial 收尾落账失败", "runID", runID, "error", perr)
		}
		fcancel()
		return fmt.Errorf("近窗桶重建失败: %w", err)
	}

	reconStart := now.AddDate(0, 0, -policy.RetentionDays)
	retentionStart := timezone.StartOfDay(reconStart)
	nearStart := timezone.StartOfDay(windowStart)
	reportNowDay := timezone.StartOfDay(windowEnd).AddDate(0, 0, 1) // 含当天

	versionIntRun := int64(versionU)

	// 常态轮续跑起点：从持久化游标日期继续（单调推进，日期只进不退）；无游标/全量重评回退 retentionStart。
	olderStart := retentionStart
	var cursorOffset int
	if cursor != nil {
		if cursor.CursorDate != nil {
			cd := timezone.StartOfDay(*cursor.CursorDate)
			if !cd.Before(retentionStart) {
				olderStart = cd
			}
		}
		if cursor.BatchOffset != nil {
			cursorOffset = *cursor.BatchOffset
		}
	}
	if fullReeval {
		// fullReeval：游标重置到 retentionStart 全量重放；失败批次被整窗重评覆盖，清空。
		olderStart = retentionStart
		cursorOffset = 0
		failedBatches = nil
	}

	// E：覆盖翻转后待重评 R1（下一轮做一次 older 全窗口重评，R1 补算）。
	//    thisRoundReevalR1 为真时，强制 older 全窗口重评且以 historyCovered=true 传入每日（补算 R1）。
	// R8-2/F2（外审 #4）：本轮不再重置 olderStart/cursorOffset——pending 重评的「回卷」由
	//    翻转轮统一持久化时写入（见收尾 `if r1PendingNew && !prevR1ReevalPending` 回卷到
	//    retentionStart）。此处每轮重置会让「重放窗口 > 单轮预算」的场景每轮从头重放前缀、
	//    pending 永不清除（方案 L86 全窗口补算语义死循环失效）。
	//    首个重放轮从翻转轮回卷后的持久化游标（retentionStart）起跑；中断后的后续 pending 轮
	//    从持久化断点续跑。
	thisRoundReevalR1 := prevR1ReevalPending && !fullReeval

	// 传给 reconcileDay 的 historyCovered：稳态保持覆盖(true) 或本轮回填(true) 时 R1 才参与计算。
	// 外审⑨ 发现1：fullReeval 轮（版本/时区/重新启用变化）历史桶按新语义逐日重建中，近窗 R1
	// 若继承旧覆盖状态会在半重建桶上算出错误分数——本轮 R1 一律不参与（宁可缺一轮 R1，不在
	// 坏数据上算错），由收尾置 r1_reeval_pending，下一轮基于完整新桶权威重评（复用既有 pending
	// 回卷重放链条 :719）。
	hc := (prevHistoryCovered || thisRoundReevalR1) && !fullReeval

	budgetExhausted := false
	reconBatchesDone := 0
	// olderCursor 跟踪“下一待处理 older 日”，供游标持久化与覆盖完成判定（单调推进）。
	olderCursor := olderStart

	// 5) 失败批次下轮优先重试：先于游标推进重放；成功后从列表移除（失败保留下轮再试）。
	//    即使在稳态（已覆盖）也须执行——失败 older 日必须被重试，不能因跳过 older 而遗漏。
	//    R8-2/F3：attempted 记录本轮已由 retry 处理过的批所在日（fb.Day 键），近窗循环据此跳过，
	//    避免同日双跑（成功已计/失败已保留于列表；该日下轮仍会进近窗循环）。
	attempted := map[time.Time]bool{}
	if !fullReeval && len(failedBatches) > 0 {
		remaining := failedBatches[:0]
		for _, fb := range failedBatches {
			// R10-2/#5：近窗日（活数据，日志与候选排序每轮在变）禁跨轮偏移——每轮强制从 0
			// 全量重算（方案 §5"近窗每轮从偏移 0 全量重算"）；仅冻结 older 日可消费跨轮偏移
			// （R9-2 断点续跑语义）。否则 [0,offset) 新增候选被漏且 attempted 阻止本轮全量重算。
			isNear := !fb.Day.Before(nearStart)
			startOffset := fb.Offset
			if isNear {
				startOffset = 0
			}
			b, nextOff, _, cands, rerr := s.reconcileDay(ctx, runID, timezone.StartOfDay(fb.Day), startOffset, policy, tzName, versionIntRun, hc, &progress)
			if rerr != nil {
				slog.Warn("[UsageRiskAnalysis] 失败批次重试失败（保留下轮再试）", "day", fb.Day, "offset", nextOff, "error", rerr)
				// R9-2：保留 reconcileDay 返回的最新批内偏移（断点续跑）。前置失败（回填/聚合/收集候选
				// 等未进批循环）时 nextOffset=startOffset（=fb.Offset），语义退化为原行为；批级失败
				// （:788）或日内预算耗尽（:805）时 nextOffset=已提交批后位置，下轮从断点续跑不回卷。
				// 按 (Day, Offset) 与 remaining 已有项查重（同 R8-2b/F3 近窗去重），防逐轮增长。
				keepOff := nextOff
				if isNear {
					keepOff = 0 // 近窗活数据无断点语义，失败保留也从 0（每轮全量重算）。
				}
				nfb := reconFailedBatch{Day: fb.Day, Offset: keepOff}
				dup := false
				for _, e := range remaining {
					if e.Day.Equal(fb.Day) && e.Offset == nfb.Offset {
						dup = true
						break
					}
				}
				if !dup {
					remaining = append(remaining, nfb)
				}
				progress.BatchesFailed++
			} else {
				reconBatchesDone += b
				// S3：失败批次重试成功时该日候选数量即入累计（失败日下轮重试时计）。
				progress.CandidatesTotal += cands
			}
			attempted[timezone.StartOfDay(fb.Day)] = true
		}
		failedBatches = remaining
		if fbJSON, merr := json.Marshal(failedBatches); merr == nil {
			progress.FailedBatches = fbJSON
		}
		fctx, fcancel := s.finalizeWrite(ctx)
		ensureProgressJSON(&progress)
		if err := s.repo.UpdateRunProgress(fctx, runID, progress); err != nil {
			slog.Warn("[UsageRiskAnalysis] 更新失败批次重放进度失败", "error", err)
		}
		fcancel()
	}

	// 6) 对账预算次序（S2）：预留 older 批 → 近窗（26h 每轮必做）→ 余量续 older。
	//    稳态跳过 = prevHistoryCovered && !prevR1ReevalPending && !fullReeval（E）。
	//    非稳态：先从 olderStart 处理至少 usageRiskReserveReconBatches 个有界批
	//    （日粒度下"完成一个日"即满足，一日 ≥1 批），保证对账单调推进不被近窗/候选饿死；
	//    预留段之后无论 older 是否走完都进入近窗段（每轮必做），最后余量从 olderCursor 续跑。
	steadySkip := prevHistoryCovered && !prevR1ReevalPending && !fullReeval

	// processOlderDay 处理一个 older 日（共用段间实现，保证游标单调推进与每日进度持久化）。
	// dayCompleted 返回 true 表示该日完整走完；candCount 为该日成功处理的候选总数。
	processOlderDay := func(day time.Time) (dayCompleted bool, candCount int) {
		if ctx.Err() != nil {
			budgetExhausted = true
			return false, 0
		}
		startOffset := 0
		if day.Equal(olderCursor) && cursorOffset > 0 {
			startOffset = cursorOffset
			// 一次性消费：批内偏移仅属于持久化游标日；该日处理（或跳过）后即清空，
			// 避免后续分段首日误沿用旧偏移（进入新一日批内偏移重置为 0 的语义）。
			cursorOffset = 0
		}
		batches, nextOffset, complete, cands, derr := s.reconcileDay(ctx, runID, day, startOffset, policy, tzName, versionIntRun, hc, &progress)
		if derr != nil {
			slog.Warn("[UsageRiskAnalysis] 对账日失败（记录下轮重试）", "day", day, "error", derr)
			progress.BatchesFailed++
			// 失败批次：offset=失败时下一批起点（已 UPSERT 批不重做），主循环越过该日，
			// 重试由 failedBatches 路径承担（不双跑同位置）。
			fb := reconFailedBatch{Day: day, Offset: nextOffset}
			failedBatches = append(failedBatches, fb)
			return false, 0
		}
		reconBatchesDone += batches
		// S3：成功处理一日即累计该日候选总数（失败日不计，下轮重试时计）。
		progress.CandidatesTotal += cands
		dayCompleted = complete
		return dayCompleted, cands
	}

	if !steadySkip {
		// 预留段：处理 older 直到本轮新增完成批次数 ≥ usageRiskReserveReconBatches，
		// 或 older 窗口走完 / 预算耗尽。预留段的日失败计入 failedBatches（retry 机制下轮承担）。
		reservedThisRound := 0
		for day := olderStart; day.Before(nearStart) && !budgetExhausted && reservedThisRound < usageRiskReserveReconBatches; day = day.AddDate(0, 0, 1) {
			prevDone := reconBatchesDone
			dayCompleted, _ := processOlderDay(day)
			// 预算耗尽（processOlderDay 内 ctx.Err 置位）：该日未处理，不得推进游标（下轮从本日继续）。
			// 不得漏算：预算耗尽即终止预留段，进入近窗段（近窗每轮必做）。
			if budgetExhausted {
				break
			}
			// 本轮新增完成批次数（仅统计成功批；失败日不计入配额，避免失败拖累预留保证）。
			if dayCompleted {
				reservedThisRound += reconBatchesDone - prevDone
			}
			// 游标单调推进：记录下一待处理日（日期只进不退）；进入新一日批内偏移重置为 0
			// （偏移仅用于同日崩溃续跑标记，由 failedBatches 路径兜底，见 reconcileDay 注释）。
			olderCursor = day.AddDate(0, 0, 1)
			progress.ReconCursorDate = ptrTime(olderCursor)
			progress.ReconBatchOffset = usageRiskIntPtr(0)
			progress.ReconBatchesDone = reconBatchesDone
			if fbJSON, merr := json.Marshal(failedBatches); merr == nil {
				progress.FailedBatches = fbJSON
			}
			fctx, fcancel := s.finalizeWrite(ctx)
			ensureProgressJSON(&progress)
			if perr := s.repo.UpdateRunProgress(fctx, runID, progress); perr != nil {
				slog.Warn("[UsageRiskAnalysis] 对账进度落账失败", "runID", runID, "error", perr)
			}
			fcancel()
		}
	} else {
		// 稳态：older 已覆盖，游标标记越过 nearStart，跳过 older 重放。
		olderCursor = nearStart
	}

	// 7) 近窗报告（候选全集分批阶段二明细 + 规则打分 + 派生列 UPSERT）。
	//    近窗覆盖 [nearStart, reportNowDay)，按日对账（派生键集 UPSERT + 集合外失效，天然幂等）。
	//    S2：近窗 26h 每轮必做——不再因 older 段耗尽预算而整轮跳过；ctx 已取消则按
	//    既有失败关闭语义（循环体首行检查）记 partial。
	for day := nearStart; day.Before(reportNowDay); day = day.AddDate(0, 0, 1) {
		if ctx.Err() != nil {
			budgetExhausted = true
			break
		}
		// R8-2/F3：本轮已由 retry-first 块处理过的近窗日不再跑（成功已计/失败已保留于
		// failedBatches——下轮仍会经 retry 路径重试）。仅跳过本轮已尝试日，非降级近窗必做语义。
		if attempted[day] {
			continue
		}
		batches, _, _, cands, derr := s.reconcileDay(ctx, runID, day, 0, policy, tzName, versionIntRun, hc, &progress)
		if derr != nil {
			slog.Warn("[UsageRiskAnalysis] 近窗日失败（记录下轮重试）", "day", day, "error", derr)
			progress.BatchesFailed++
			// R8-2/F3：按 (Day, Offset) 去重——retry-first 已保留的同键失败不重复 append，防逐轮增长。
			// R10-2/#5：近窗日失败保留偏移记 0（活数据每轮从 0 全量重算，无跨轮断点语义）。
			nfb := reconFailedBatch{Day: day, Offset: 0}
			dup := false
			for _, e := range failedBatches {
				if e.Day.Equal(day) && e.Offset == nfb.Offset {
					dup = true
					break
				}
			}
			if !dup {
				failedBatches = append(failedBatches, nfb)
			}
		} else {
			reconBatchesDone += batches
			// S3：成功处理一日即累计候选总数。
			progress.CandidatesTotal += cands
		}
		// 近窗日未完成时游标由失败批次路径承担（不推进近窗游标）
		progress.ReconBatchesDone = reconBatchesDone
		if fbJSON, merr := json.Marshal(failedBatches); merr == nil {
			progress.FailedBatches = fbJSON
		}
		fctx, fcancel := s.finalizeWrite(ctx)
		ensureProgressJSON(&progress)
		if perr := s.repo.UpdateRunProgress(fctx, runID, progress); perr != nil {
			slog.Warn("[UsageRiskAnalysis] 对账进度落账失败", "runID", runID, "error", perr)
		}
		fcancel()
	}

	// 8) 续 older 段：剩余预算从 olderCursor 续跑到 nearStart 或 budgetExhausted。
	//    近窗段之后才有机会处理剩余 older；游标从 olderCursor 单调续跑（不回退）。
	if !steadySkip {
		for day := olderCursor; day.Before(nearStart) && !budgetExhausted; day = day.AddDate(0, 0, 1) {
			if ctx.Err() != nil {
				// 预算耗尽：当日未处理，不得推进游标（下轮从本日继续，避免漏算）。
				budgetExhausted = true
				break
			}
			processOlderDay(day)
			if budgetExhausted {
				// processOlderDay 内 ctx.Err 置位：当日未处理（或处理中断），不得推进游标。
				break
			}
			olderCursor = day.AddDate(0, 0, 1)
			progress.ReconCursorDate = ptrTime(olderCursor)
			progress.ReconBatchOffset = usageRiskIntPtr(0)
			progress.ReconBatchesDone = reconBatchesDone
			if fbJSON, merr := json.Marshal(failedBatches); merr == nil {
				progress.FailedBatches = fbJSON
			}
			fctx, fcancel := s.finalizeWrite(ctx)
			ensureProgressJSON(&progress)
			if perr := s.repo.UpdateRunProgress(fctx, runID, progress); perr != nil {
				slog.Warn("[UsageRiskAnalysis] 对账进度落账失败", "runID", runID, "error", perr)
			}
			fcancel()
		}
	}

	// 8) history_covered：older 窗口（保留期起点 → 近窗起点）全部走完且无预算耗尽时翻转。
	//    保留期 ≤ 近窗（olderDays==0）时无需回填，直接视为覆盖完成。已覆盖（prevHistoryCovered）维持。
	//    覆盖以“游标推进到 nearStart”为判据（非本轮回填批次数，跨轮续跑后仍能正确翻转）。
	//    E：翻转判据新增“失败批次为空”——失败批次下轮优先重试，覆盖完成前不得翻转。
	olderFullyDone := !olderCursor.Before(nearStart)
	historyCovered := prevHistoryCovered && !fullReeval
	if !historyCovered {
		historyCovered = !budgetExhausted && olderFullyDone && len(failedBatches) == 0
	}

	// 9) 台账收尾：completed（全量覆盖且无失败）或 partial（预算耗尽/有失败批次）。
	status := "completed"
	if budgetExhausted || progress.BatchesFailed > 0 || failureStage != "" {
		status = "partial"
	}
	if fullReeval {
		// 全量重评轮：若预算耗尽仍 partial（history_covered 可能未翻转）。
		if budgetExhausted {
			status = "partial"
		}
	}

	// E：r1_reeval_pending 语义闭环：
	//    - 覆盖由 false→true 的那轮置 true（下轮补算 R1）；
	//    - pending 重评轮仅在"重放完成"（older 走完 + 无失败批次 + 未耗尽预算）时清除，
	//      否则保持 pending（下轮从游标续跑重放），不因预算耗尽而丢失 R1 补算语义；
	//    - 全量重评轮已覆盖 R1 置 false；其余保持上一轮值。
	var r1PendingNew bool
	switch {
	case !prevHistoryCovered && historyCovered:
		r1PendingNew = true // 覆盖翻转 → 置 pending
	case thisRoundReevalR1:
		// S1：条件清除——仅当本轮 older 重放真正走完且无失败、无预算耗尽时置 false。
		if olderFullyDone && len(failedBatches) == 0 && !budgetExhausted {
			r1PendingNew = false
		} else {
			r1PendingNew = prevR1ReevalPending // 保持 pending，下轮从游标续跑
		}
	case fullReeval:
		// 外审⑨ 发现1：fullReeval 轮 R1 未参与（hc 已排除 fullReeval）——桶重建完成后置
		// pending，下一轮以完整新桶全窗重评 R1（:719 新置 pending 同轮回卷 retentionStart，
		// 下轮全窗重放）。冷启动翻转轮走上一 case（!prevHistoryCovered && historyCovered），
		// 不受本 case 影响。
		r1PendingNew = true
	default:
		r1PendingNew = prevR1ReevalPending // 兜底保持
	}

	// M：连续 partial 计数——completed 重置 0，否则上一轮值+1。
	consecutivePartials := consecutivePartialOf(status, prevConsecutivePartials)

	progress.HistoryCovered = historyCovered
	progress.BudgetExhausted = budgetExhausted
	progress.ReconCursorDate = ptrTime(olderCursor)
	progress.ReconBatchOffset = usageRiskIntPtr(0)
	progress.R1ReevalPending = r1PendingNew
	progress.ConsecutivePartials = consecutivePartials
	if fbJSON, merr := json.Marshal(failedBatches); merr == nil {
		progress.FailedBatches = fbJSON
	}
	if failureStage != "" {
		progress.FailureStage = ptrStr(failureStage)
	} else if budgetExhausted {
		progress.FailureStage = ptrStr("budget_exhausted")
	}
	progress.FinishedAt = ptrTime(s.now())
	// R8-2/F2：翻转置 pending 轮——统一持久化（ReconCursorDate 已按 olderCursor 写完后）
	// 追加回卷游标到 retentionStart。覆盖 false→true 的翻转轮（r1PendingNew=true 且上一轮
	// 非 pending）持久化回卷点；后续 pending 轮读取持久化游标=回卷点或断点续跑，不再每轮
	// 重置（修复外审 #4：每轮重置 → 重放窗口大于单轮预算时永久重复前缀）。
	// fullReeval case 的 r1PendingNew=false 不受影响。位置必须在统一持久化之后：
	// 否则被上面的 olderCursor 覆盖（白修）。
	if r1PendingNew && !prevR1ReevalPending {
		progress.ReconCursorDate = ptrTime(retentionStart)
		progress.ReconBatchOffset = usageRiskIntPtr(0)
	}
	fctx, fcancel := s.finalizeWrite(ctx)
	ensureProgressJSON(&progress)
	if err := s.repo.CompleteRun(fctx, runID, status, progress); err != nil {
		fcancel()
		return fmt.Errorf("收尾 run 失败: %w", err)
	}
	fcancel()
	return nil
}

// consecutivePartialOf 计算连续 partial 计数：completed 重置 0，否则上一轮值+1（M）。
func consecutivePartialOf(status string, prev int) int {
	if status == "completed" {
		return 0
	}
	return prev + 1
}

// reconFailedBatch 记录一次对账日失败的位置（派发单 U4b-R3 失败批次下轮优先重试）。
// Day 为该失败日（StartOfDay），Offset 为本日批内起始偏移。
// R8-2/F4：键区间死链已删（方案修订为「日期+批内偏移」二元游标，键区间维度删除）。
type reconFailedBatch struct {
	Day    time.Time `json:"day"`
	Offset int       `json:"offset"`
}

// reconcileDay 处理单日（两段式对账，派发单 U4b-R4 目标语义）：
//  1. 候选全集分批（≤100/批）做阶段二明细+打分，每批完成即立即以
//     ReconcileReportBatchUPSERTOnly(day, batchRecords) 提交（UPSERT-only，绝不触发集合外失效——
//     拆雷点：部分键集永不触发失效）；
//  2. 当日全部批走完（或当日无候选）后，以"该日完整候选键集"调用 InvalidateOutsideKeySet
//     （失效只可能以该日完整派生键集触发——核心不变量）。
//
// 批内偏移续跑（派发单 f）：startOffset 支持从传入偏移起步（跳过已持久化批次）；nextOffset 返回
// 本轮到达的偏移（已 UPSERT 批之后首个未处理批次起点），供游标精确持久化（崩溃后同日内不重复批）。
// 每批 UPSERT 成功后即调用 UpdateRunProgress 持久化（当日, 下一批起点, 当前批键区间）。
//
// 预算耗尽（ctx 取消）：已 UPSERT 批已提交，不触发当日失效（避免派生集不全误失效），下轮从 nextOffset
// 续跑（失败批次路径带偏移，主循环不得同轮重复处理）。dayComplete 返回 true 表示该日已完整走完且已失效。
//
// 返回值（S3）：最后一项 cands 为该日完整候选键总数（ListCandidates 各批累计 = collectDayCandidates
// 聚合结果），供调用方跨日累计 progress.CandidatesTotal。失败/预算耗尽当日返回 cands=0
// （当日未完成，下轮续跑成功时由调用方按全量键数计）。
func (s *UsageRiskAnalysisService) reconcileDay(ctx context.Context, runID int64, day time.Time, startOffset int, policy UsageRiskPolicy, tzName string, policyVersion int64, historyCovered bool, progress *UsageRiskRunProgress) (batches, nextOffset int, dayComplete bool, cands int, err error) {
	dayStart := timezone.StartOfDay(day)
	dayEnd := dayStart.AddDate(0, 0, 1)
	reportDate := dayStart

	// I/F11：游标顺带回填该日小时桶（新表上线为空 / 时区变更全窗口重建都走此路径；R1 历史桶数据源）。
	if _, err := s.repo.RebuildHourlyWindow(ctx, dayStart, dayEnd, aggParams(policy)); err != nil {
		return 0, startOffset, false, 0, fmt.Errorf("回填日小时桶失败: %w", err)
	}

	// 日内聚合桶（小时桶 → 日级指标）一次性取回并构建 (user,group) 汇总，复用 U3 AggregateHourly。
	hourly, err := s.repo.AggregateHourly(ctx, dayStart, dayEnd, aggParams(policy))
	if err != nil {
		return 0, startOffset, false, 0, fmt.Errorf("聚合日桶失败: %w", err)
	}
	dayMap := buildDayAggMap(hourly)
	if ctx.Err() != nil {
		return 0, startOffset, false, 0, ctx.Err()
	}

	// 候选全集（资格谓词 + 门槛，无分数粗筛）。分页遍历聚合到切片。
	candList, err := s.collectDayCandidates(ctx, dayStart, dayEnd, policy)
	if err != nil {
		return 0, startOffset, false, 0, fmt.Errorf("收集候选失败: %w", err)
	}

	// 空候选日：仍以空完整键集调用失效（失效该日所有旧有效报告，保持对账一致性）。
	if len(candList) == 0 {
		if err := s.repo.InvalidateOutsideKeySet(ctx, reportDate, nil, nil); err != nil {
			return 0, startOffset, false, 0, fmt.Errorf("空日失效失败: %w", err)
		}
		return 1, 0, true, 0, nil
	}

	// 候选全集派生的完整键集（供日完成失效使用；重 derive 便宜，无需明细）。
	dayUserIDs := make([]int64, len(candList))
	dayGroupIDs := make([]int64, len(candList))
	for i, c := range candList {
		dayUserIDs[i] = c.UserID
		dayGroupIDs[i] = c.GroupID
	}

	rpmGroupInputs, rpmUserInputs, err := s.buildRPMLimitInputs(ctx, candList)
	if err != nil {
		return 0, startOffset, false, 0, fmt.Errorf("准备 RPM 限额输入失败: %w", err)
	}
	groupHitMinutes, err := s.repo.AggregateGroupRPMMinutes(ctx, dayStart, dayEnd, aggParams(policy), rpmGroupInputs, policy.R3ARatio)
	if err != nil {
		return 0, startOffset, false, 0, fmt.Errorf("分组 RPM 贴线分钟聚合失败: %w", err)
	}
	globalHitMinutes, err := s.repo.AggregateUserGlobalRPMMinutes(ctx, dayStart, dayEnd, rpmUserInputs, policy.R3ARatio)
	if err != nil {
		return 0, startOffset, false, 0, fmt.Errorf("用户全局 RPM 贴线分钟聚合失败: %w", err)
	}
	if ctx.Err() != nil {
		return 0, startOffset, false, 0, ctx.Err()
	}

	// 逐候选（有界批次 ≤100）做阶段二明细+打分，并立即 UPSERT-only 提交（崩溃续跑不重做已提交批）。
	nextOffset = startOffset
	for start := startOffset; start < len(candList); start += usageRiskReconBatchKeyLimit {
		if ctx.Err() != nil {
			break
		}
		end := start + usageRiskReconBatchKeyLimit
		if end > len(candList) {
			end = len(candList)
		}
		batch := candList[start:end]
		batchRecords := make([]UsageRiskReportRecord, 0, len(batch))
		for _, c := range batch {
			rec, berr := s.buildReportRecord(ctx, c, dayStart, dayEnd, policy, tzName, dayMap, groupHitMinutes, globalHitMinutes, policyVersion, historyCovered)
			if berr != nil {
				// D：候选报告构建失败 → 立即以该批为失败单元返回（批未提交、offset=该批起点 start，
				// 下轮优先重试）；本日不触发失效（部分键集永不失效的不变量保持）。
				return batches, start, false, 0, fmt.Errorf("候选报告构建失败(user=%d,group=%d): %w", c.UserID, c.GroupID, berr)
			}
			batchRecords = append(batchRecords, rec)
		}
		// 立即 UPSERT-only 提交本批（绝不触发集合外失效）。
		if len(batchRecords) > 0 {
			if uerr := s.repo.ReconcileReportBatchUPSERTOnly(ctx, reportDate, batchRecords); uerr != nil {
				return batches, nextOffset, false, 0, fmt.Errorf("批 UPSERT 失败: %w", uerr)
			}
		}
		batches++
		nextOffset = end // 已提交批之后首个未处理批次起点
		progress.BatchesDone++

		// 游标真持久化：批 UPSERT 成功后持久化（当日, 下一批起点）。
		progress.ReconCursorDate = ptrTime(dayStart)
		progress.ReconBatchOffset = usageRiskIntPtr(nextOffset)
		ensureProgressJSON(progress)
		if perr := s.repo.UpdateRunProgress(ctx, runID, *progress); perr != nil {
			slog.Warn("[UsageRiskAnalysis] 批游标持久化失败", "day", dayStart, "error", perr)
		}
	}

	// 预算在日内耗尽：不触发当日失效（避免派生集不全误失效），下轮从 nextOffset 续跑。
	if ctx.Err() != nil {
		return batches, nextOffset, false, 0, ctx.Err()
	}

	// 日完成：以完整候选键集触发失效（核心不变量——失效只可能以"该日完整派生键集"触发）。
	if err := s.repo.InvalidateOutsideKeySet(ctx, reportDate, dayUserIDs, dayGroupIDs); err != nil {
		return batches, nextOffset, false, 0, fmt.Errorf("当日失效失败: %w", err)
	}
	return batches, len(candList), true, len(candList), nil
}

// collectDayCandidates 取回单日全部候选（资格谓词 + 门槛），分页聚合到切片。
func (s *UsageRiskAnalysisService) collectDayCandidates(ctx context.Context, dayStart, dayEnd time.Time, policy UsageRiskPolicy) ([]UsageRiskCandidate, error) {
	var out []UsageRiskCandidate
	offset := 0
	for {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		batch, err := s.repo.ListCandidates(ctx, dayStart, dayEnd, aggParams(policy), policy.MinDailyRequests, offset, usageRiskReconBatchKeyLimit)
		if err != nil {
			return out, err
		}
		out = append(out, batch...)
		if len(batch) < usageRiskReconBatchKeyLimit {
			break
		}
		offset += len(batch)
	}
	return out, nil
}

// buildRPMLimitInputs 为单日候选准备 R3a 双作用域限额输入：
//   - 分组作用域：按 checkRPM 语义算适用分组限（override 替代分组限、override=0 免除）。
//   - 用户全局作用域：user.rpm_limit 全局天花板。
func (s *UsageRiskAnalysisService) buildRPMLimitInputs(ctx context.Context, cands []UsageRiskCandidate) ([]UsageRiskRPMLimit, []UsageRiskUserLimit, error) {
	userLimits := make(map[int64]int)
	var groupInputs []UsageRiskRPMLimit
	seenGroup := make(map[UsageRiskUserGroupKey]struct{})
	for _, c := range cands {
		if _, ok := seenGroup[UsageRiskUserGroupKey{UserID: c.UserID, GroupID: c.GroupID}]; !ok {
			seenGroup[UsageRiskUserGroupKey{UserID: c.UserID, GroupID: c.GroupID}] = struct{}{}
			groupLimit, err := s.applicableGroupLimit(ctx, c.UserID, c.GroupID)
			if err != nil {
				return nil, nil, err
			}
			groupInputs = append(groupInputs, UsageRiskRPMLimit{
				UserID:     c.UserID,
				GroupID:    c.GroupID,
				GroupLimit: groupLimit,
			})
		}
		if _, ok := userLimits[c.UserID]; !ok {
			ul, err := s.userRPMLimit(ctx, c.UserID)
			if err != nil {
				return nil, nil, err
			}
			userLimits[c.UserID] = ul
		}
	}
	userInputs := make([]UsageRiskUserLimit, 0, len(userLimits))
	for uid, lim := range userLimits {
		userInputs = append(userInputs, UsageRiskUserLimit{UserID: uid, UserLimit: lim})
	}
	return groupInputs, userInputs, nil
}

// applicableGroupLimit 复用 checkRPM 第一层分组限额选择语义（billing_cache_service.go:790）。
func (s *UsageRiskAnalysisService) applicableGroupLimit(ctx context.Context, userID, groupID int64) (int, error) {
	var override *int
	if s.userGroupRateRepo != nil {
		ov, err := s.userGroupRateRepo.GetRPMOverrideByUserAndGroup(ctx, userID, groupID)
		if err != nil {
			return 0, fmt.Errorf("读取 rpm override 失败: %w", err)
		}
		override = ov
	}
	if override != nil {
		if *override == 0 {
			return 0, nil // 免除分组检查
		}
		return *override, nil // override 替代分组限
	}
	gl, err := s.groupRPMLimit(ctx, groupID)
	if err != nil {
		return 0, err
	}
	return gl, nil
}

// groupRPMLimit 读取分组当前 rpm_limit（ent 正统访问，非分析表）。
func (s *UsageRiskAnalysisService) groupRPMLimit(ctx context.Context, groupID int64) (int, error) {
	if s.client == nil {
		return 0, nil
	}
	g, err := s.client.Group.Get(ctx, groupID)
	if err != nil {
		return 0, fmt.Errorf("读取分组限额失败: %w", err)
	}
	return g.RpmLimit, nil
}

// userRPMLimit 读取用户全局 rpm_limit（ent 正统访问，非分析表）。
func (s *UsageRiskAnalysisService) userRPMLimit(ctx context.Context, userID int64) (int, error) {
	if s.client == nil {
		return 0, nil
	}
	u, err := s.client.User.Get(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("读取用户限额失败: %w", err)
	}
	return u.RpmLimit, nil
}

// buildReportRecord 组装单候选的日级/用户级事实 → 调 U4a EvaluateRules + AssembleEvidence → 报告记录。
// 日级分组事实来自日内小时桶汇总（dayMap）；用户级事实来自 U3 阶段二明细方法。
func (s *UsageRiskAnalysisService) buildReportRecord(
	ctx context.Context,
	c UsageRiskCandidate,
	dayStart, dayEnd time.Time,
	policy UsageRiskPolicy,
	tzName string,
	dayMap map[UsageRiskUserGroupKey]*dayAgg,
	groupHitMinutes map[UsageRiskUserGroupKey]int,
	globalHitMinutes map[int64]int,
	policyVersion int64,
	historyCovered bool,
) (UsageRiskReportRecord, error) {
	key := UsageRiskUserGroupKey{UserID: c.UserID, GroupID: c.GroupID}
	da := dayMap[key]

	dayFacts := DayFacts{}
	userActiveInstants := map[time.Time]struct{}{}
	if da != nil {
		dayFacts.RequestCount = da.RequestCount
		dayFacts.InputTokens = da.InputTokensSum
		dayFacts.OutputTokens = da.OutputTokensSum
		dayFacts.CacheReadTokens = da.CacheReadTokensSum
		dayFacts.CostUSD = da.CostUSDSum
		dayFacts.OccupiedMs = da.OccupiedMsSum
		dayFacts.RPMLineMinutesGroup = groupHitMinutes[key]
		dayFacts.BucketInstants = append([]time.Time(nil), da.BucketInstants...)
	}

	// 用户级事实（阶段二明细）。
	userFacts := UserFacts{
		IPClusters:     map[string]IPClusterFact{},
		UADistribution: map[string]int{},
		KeyCounts:      map[string]int{},
	}
	ips, err := s.repo.DistinctIPs(ctx, c.UserID, dayStart, dayEnd, aggParams(policy))
	if err != nil {
		return UsageRiskReportRecord{}, fmt.Errorf("DistinctIPs: %w", err)
	}
	userFacts.DistinctIPCount = len(ips)

	clusters, err := s.repo.IPAssociatedUsers(ctx, c.UserID, dayStart, dayEnd, aggParams(policy))
	if err != nil {
		return UsageRiskReportRecord{}, fmt.Errorf("IPAssociatedUsers: %w", err)
	}
	for ip, f := range clusters {
		userFacts.IPClusters[ip] = f
	}

	ua, err := s.repo.UADistribution(ctx, c.UserID, dayStart, dayEnd, aggParams(policy))
	if err != nil {
		return UsageRiskReportRecord{}, fmt.Errorf("UADistribution: %w", err)
	}
	userFacts.UADistribution = ua

	keys, err := s.repo.KeyUsageDistribution(ctx, c.UserID, dayStart, dayEnd, aggParams(policy))
	if err != nil {
		return UsageRiskReportRecord{}, fmt.Errorf("KeyUsageDistribution: %w", err)
	}
	userFacts.KeyCounts = keys

	heatmap, err := s.repo.ActiveHourHeatmap(ctx, c.UserID, dayStart, dayEnd, tzName, aggParams(policy))
	if err != nil {
		return UsageRiskReportRecord{}, fmt.Errorf("ActiveHourHeatmap: %w", err)
	}
	userFacts.ActiveHourHeatmap = heatmap
	// F：用户级活跃桶 = 用户当日全部合格分组的桶起点（跨分组 distinct）。
	//    dayMap 已含当日全部 (user,group) 小时桶汇总，故按 UserID 聚合即可得到用户级活跃桶，
	//    用于 R1 活跃小时（= 跨分组 distinct 桶数），而非仅本候选分组。
	for k, v := range dayMap {
		if k.UserID == c.UserID {
			for _, b := range v.BucketInstants {
				userActiveInstants[b] = struct{}{}
			}
		}
	}
	userFacts.ActiveBucketInstants = make([]time.Time, 0, len(userActiveInstants))
	for b := range userActiveInstants {
		userFacts.ActiveBucketInstants = append(userFacts.ActiveBucketInstants, b)
	}
	userFacts.GlobalRPMHitMinutes = globalHitMinutes[c.UserID]

	// R2 peer 群当日日 cost（同分组、同资格谓词 + 门槛，含被测用户）。
	peers, err := s.repo.R2PeerDayCosts(ctx, c.GroupID, dayStart, dayEnd, aggParams(policy), policy.MinDailyRequests)
	if err != nil {
		return UsageRiskReportRecord{}, fmt.Errorf("R2PeerDayCosts: %w", err)
	}
	peerCost := make([]float64, 0, len(peers))
	for _, p := range peers {
		peerCost = append(peerCost, p.CostUSD)
	}

	// R3a 限额上下文（当前有效配置；覆盖/分组限由 applicableGroupLimit 已算好，传入 groupHitMinutes 的适用限）。
	groupLimit, err := s.applicableGroupLimit(ctx, c.UserID, c.GroupID)
	if err != nil {
		return UsageRiskReportRecord{}, err
	}
	userLimit, err := s.userRPMLimit(ctx, c.UserID)
	if err != nil {
		return UsageRiskReportRecord{}, err
	}
	override, err := s.rpmOverride(ctx, c.UserID, c.GroupID)
	if err != nil {
		return UsageRiskReportRecord{}, err
	}

	// R1 连续活跃天数（取自 rollup 历史桶）；历史未覆盖时记 insufficient_history（由 R1HistoryCovered 控制）。
	consecutiveDays := 0
	dayElapsed := dayElapsedHours(dayStart)
	if historyCovered {
		consecutiveDays, err = s.computeConsecutiveActiveDays(ctx, c.UserID, dayStart, policy)
		if err != nil {
			return UsageRiskReportRecord{}, err
		}
	}

	rc := RuleContext{
		DayStart:           dayStart,
		DayEnd:             dayEnd,
		ElapsedHours:       dayElapsed,
		R1HistoryCovered:   historyCovered,
		R1ConsecutiveDays:  consecutiveDays,
		R2PeerCost:         peerCost,
		R2Filter:           fmt.Sprintf("group_id=%d AND subscription_id IS NOT NULL AND request_count>=%d", c.GroupID, policy.MinDailyRequests),
		R2Algorithm:        "linear-interpolation-p95",
		UserRPMLimit:       userLimit,
		GroupRPMLimit:      groupLimit,
		Override:           override,
		LimitSnapshotTime:   s.now(),
		TZName:             tzName,
	}

	outcome := EvaluateRules(policy, dayFacts, userFacts, rc)
	evidence := AssembleEvidence(dayFacts, userFacts, rc)

	ruleHitsJSON, _ := json.Marshal(outcome.RuleHits)
	evidenceJSON, _ := json.Marshal(evidence)

	rec := UsageRiskReportRecord{
		UserID:        c.UserID,
		GroupID:       c.GroupID,
		ReportDate:    dayStart,
		PolicyVersion: policyVersion,
		Score:         outcome.Score,
		Level:         outcome.Level,
		RuleHits:      ruleHitsJSON,
		Evidence:      evidenceJSON,
		InvalidatedAt: nil,
	}
	return rec, nil
}

// rpmOverride 读取 (user,group) override（nil 表示无 override）。
func (s *UsageRiskAnalysisService) rpmOverride(ctx context.Context, userID, groupID int64) (*int, error) {
	if s.userGroupRateRepo == nil {
		return nil, nil
	}
	return s.userGroupRateRepo.GetRPMOverrideByUserAndGroup(ctx, userID, groupID)
}

// isHistoryCovered 已内联至 runAnalysis / reconcileDay，本方法移除。

// computeConsecutiveActiveDays 在回看窗口内统计连续活跃天数（活跃小时 ≥ R1ActiveHours 的天数）。
// I：直接读 rollup 的 distinct 活跃桶（user_usage_metrics_rollup），删除逐日 AggregateHourly 全量重聚合
// （性能炸弹）；按日分组统计 distinct 桶数，从最近一天向前连续计数，遇中断即停止。
func (s *UsageRiskAnalysisService) computeConsecutiveActiveDays(ctx context.Context, userID int64, reportDay time.Time, policy UsageRiskPolicy) (int, error) {
	// 回看长度取 R1ConsecutiveDays + 1，避免无限回溯；历史桶已回填时才调用本函数。
	lookback := policy.R1ConsecutiveDays + 1
	lookbackStart := timezone.StartOfDay(reportDay).AddDate(0, 0, -lookback)
	reportDayStart := timezone.StartOfDay(reportDay)

	buckets, err := s.repo.UserActiveBucketHours(ctx, userID, lookbackStart, reportDayStart)
	if err != nil {
		return 0, fmt.Errorf("读取用户活跃桶失败: %w", err)
	}
	// 按日分组统计 distinct 桶数。
	dayActive := make(map[string]int)
	for _, b := range buckets {
		d := timezone.StartOfDay(b).Format("2006-01-02")
		dayActive[d]++
	}

	consecutive := 0
	for i := 1; i <= lookback; i++ {
		if ctx.Err() != nil {
			return consecutive, ctx.Err()
		}
		d := timezone.StartOfDay(reportDay).AddDate(0, 0, -i).Format("2006-01-02")
		if active := dayActive[d]; active >= policy.R1ActiveHours {
			consecutive++
		} else {
			break // 连续中断
		}
	}
	return consecutive, nil
}

// handleTimezoneChange 时区变更换日迁移：旧 report_date 行失效（保留人工状态 + 审计），
// 新日界新行由后续对账自然生成；桶由近窗重建 + 全窗口对账重建（方案 §5）。
// 实现：将当前所有有效报告置为 invalidated（仅动有效性维度，status/审计原样保留），
// 后续 reconcileDay 对每日起点重新 UPSERT 新日界行。
func (s *UsageRiskAnalysisService) handleTimezoneChange(ctx context.Context, now time.Time, policy UsageRiskPolicy, tzName string) error {
	// 失效全部当前有效报告（集合外失效语义：以空派生集对该日调用对账）。
	// 由于跨日界，旧日界行无法直接归属新日界，统一失效后由下轮按新日界重算成报。
	// 为保留人工状态与审计，仅置 invalidated_at，不动 status 列。
	if err := s.invalidateAllEffective(ctx); err != nil {
		return err
	}
	slog.Info("[UsageRiskAnalysis] 时区变更换日迁移：旧日界报告已失效，待按新日界重算",
		"new_tz", tzName)
	return nil
}

// invalidateAllEffective 将当前所有有效报告置为 invalidated_at=now（仅动有效性维度，保留人工状态）。
// 单一 SQL 全量，不按日期范围推测——覆盖保留期曾缩短/清理曾失败遗留的范围外有效报告。
func (s *UsageRiskAnalysisService) invalidateAllEffective(ctx context.Context) error {
	// 外审⑨ 发现3：原实现按当前 RetentionDays 推导日期逐日失效，保留期曾缩短/清理曾失败时
	// 范围外旧报告残留并与新日界并存；改为仓储单一 SQL 全量失效当前有效报告，不推测范围。
	if err := s.repo.InvalidateAllEffective(ctx); err != nil {
		return fmt.Errorf("换日迁移全量失效有效报告失败: %w", err)
	}
	return nil
}

// ───────────────────────── 对外 API（U4c 契约实现） ─────────────────────────

// ListReports 委托 repository 查询；min_score 由策略统一提供（禁止硬编码）。
func (s *UsageRiskAnalysisService) ListReports(ctx context.Context, f UsageRiskListFilter) (*UsageRiskReportList, error) {
	policy, err := s.currentPolicy(ctx)
	if err != nil {
		return nil, err
	}
	items, total, err := s.repo.ListReports(ctx, f, policy.ListingMinScore)
	if err != nil {
		return nil, err
	}
	return &UsageRiskReportList{Items: items, Total: total, MinScore: policy.ListingMinScore}, nil
}

// GetReport 委托 repository 详情查询。
func (s *UsageRiskAnalysisService) GetReport(ctx context.Context, reportID int64) (*UsageRiskReportDetail, error) {
	return s.repo.GetReportDetail(ctx, reportID)
}

// UpdateStatus 四态状态机校验 + 列级原状态条件更新 + 审计事件，状态变更与审计同事务落账
// （方案 §5:91；路由中间件 SkipAudit 避免与事务内权威审计重复）。
func (s *UsageRiskAnalysisService) UpdateStatus(ctx context.Context, reportID int64, newStatus string, adminID int64) error {
	detail, err := s.repo.GetReportDetail(ctx, reportID)
	if err != nil {
		if errors.Is(err, ErrUsageRiskReportNotFound) {
			return ErrUsageRiskReportNotFound
		}
		return err
	}
	oldStatus := detail.Status
	if oldStatus == newStatus {
		return nil
	}
	if !isValidTransition(oldStatus, newStatus) {
		return ErrUsageRiskIllegalTransition
	}
	now := s.now()
	at := now
	// 审计与状态更新同事务（方案 §5:91）：构造 entry 交由 repo 在单事务内落账。
	entry := &AuditLog{
		ActorUserID: ptrInt64(adminID),
		Action:      "admin.usage_risk.status",
		Method:      "POST",
		Path:        "/api/v1/admin/usage-risk/reports/" + strconv.FormatInt(reportID, 10) + "/status",
		StatusCode:  200,
		Extra: map[string]any{
			"report_id":  reportID,
			"old_status": oldStatus,
			"new_status": newStatus,
		},
		CreatedAt: at,
	}
	ok, err := s.repo.UpdateStatusWithAudit(ctx, reportID, oldStatus, newStatus, adminID, at, entry)
	if err != nil {
		return err
	}
	if !ok {
		// 并发冲突或状态已被变更：返回非法转移（调用方重试）。
		return ErrUsageRiskIllegalTransition
	}
	return nil
}

// GetRunStatus 返回最近一次 run 新鲜度信息；失败关闭态经由 summary.Error 暴露。
func (s *UsageRiskAnalysisService) GetRunStatus(ctx context.Context) (*UsageRiskRunStatus, error) {
	// L：失败关闭态优先暴露（先于 LatestRun）：配置/快照损坏导致 job 失败关闭时，
	// 新鲜度条直接呈现 error 态，而非掩盖为 idle/completed。
	if s.failedClosed {
		return &UsageRiskRunStatus{Status: "error", Error: s.failedErr.Error()}, nil
	}
	st, err := s.repo.LatestRun(ctx)
	if err != nil {
		return nil, err
	}
	if st == nil {
		st = &UsageRiskRunStatus{Status: "idle"}
	}
	return st, nil
}

// GetRiskSummary 返回仪表盘风险摘要；min_score 由策略统一提供；配置失败暴露 Error。
func (s *UsageRiskAnalysisService) GetRiskSummary(ctx context.Context) (*UsageRiskSummary, error) {
	summary := &UsageRiskSummary{ByLevel: map[string]int{}, Top: []UsageRiskTopUser{}}
	policy, err := s.currentPolicy(ctx)
	if err != nil {
		summary.Error = err.Error()
		return summary, nil
	}
	got, err := s.repo.RiskSummary(ctx, policy.ListingMinScore, 5)
	if err != nil {
		summary.Error = err.Error()
		return summary, nil
	}
	if got == nil {
		return summary, nil
	}
	summary.OpenTotal = got.OpenTotal
	summary.ByLevel = got.ByLevel
	summary.Top = got.Top
	return summary, nil
}

// currentPolicy 统一读取当前策略（供 API 取 min_score 等），失败关闭态直接返回错误。
func (s *UsageRiskAnalysisService) currentPolicy(ctx context.Context) (UsageRiskPolicy, error) {
	if s.failedClosed {
		return UsageRiskPolicy{}, fmt.Errorf("分析 job 处于失败关闭态: %w", s.failedErr)
	}
	if s.policyFn != nil {
		return s.policyFn(ctx)
	}
	if s.settingSvc == nil {
		return UsageRiskPolicy{}, errors.New("setting service 未注入")
	}
	return s.settingSvc.LoadUsageRiskPolicy(ctx)
}

// ───────────────────────── 状态机 ─────────────────────────

// usageRiskTransitions 是 §6.2 冻结的四态状态机：
// open → acknowledged → resolved；open/acknowledged → dismissed（终态）。
var usageRiskTransitions = map[string][]string{
	statusUsageRiskOpen:       {statusUsageRiskAcknowledged, statusUsageRiskDismissed},
	statusUsageRiskAcknowledged: {statusUsageRiskResolved, statusUsageRiskDismissed},
	statusUsageRiskResolved:   {}, // 终态
	statusUsageRiskDismissed:  {}, // 终态
}

func isValidTransition(oldStatus, newStatus string) bool {
	for _, allowed := range usageRiskTransitions[oldStatus] {
		if allowed == newStatus {
			return true
		}
	}
	return false
}

// ───────────────────────── 辅助 ─────────────────────────

// aggParams 由策略构造聚合资格谓词参数。
func aggParams(p UsageRiskPolicy) AggregateParams {
	return AggregateParams{
		UnlimitedGroupsOnly: p.UnlimitedGroupsOnly,
		UAWhitelist:        p.UAWhitelist,
	}
}

// dayAgg 是单 (user,group) 一日的小时桶汇总（R3a 分组 RPM 贴线分钟数不在小时桶内，
// 由 AggregateGroupRPMMinutes 单独产出，经 groupHitMinutes[key] 注入 DayFacts）。
type dayAgg struct {
	RequestCount        int
	InputTokensSum      int64
	OutputTokensSum     int64
	CacheReadTokensSum  int64
	CostUSDSum          float64
	OccupiedMsSum       int64
	BucketInstants      []time.Time
}

// buildDayAggMap 将小时桶切片汇总为 (user,group) → 日级聚合（小时桶 → 日级指标）。
func buildDayAggMap(hourly []UsageRiskHourAggregate) map[UsageRiskUserGroupKey]*dayAgg {
	m := make(map[UsageRiskUserGroupKey]*dayAgg, len(hourly))
	for _, h := range hourly {
		k := UsageRiskUserGroupKey{UserID: h.UserID, GroupID: h.GroupID}
		da, ok := m[k]
		if !ok {
			da = &dayAgg{}
			m[k] = da
		}
		da.RequestCount += h.RequestCount
		da.InputTokensSum += h.InputTokensSum
		da.OutputTokensSum += h.OutputTokensSum
		da.CacheReadTokensSum += h.CacheReadTokensSum
		da.CostUSDSum += h.CostUSDSum
		da.OccupiedMsSum += h.OccupiedMsSum
		da.BucketInstants = append(da.BucketInstants, h.BucketHour)
	}
	return m
}

// dayElapsedHours 返回 [dayStart, dayStart+1day) 的实际 elapsed 小时（DST 兼容：23/23.5/24/24.5/25h）。
func dayElapsedHours(dayStart time.Time) float64 {
	next := dayStart.AddDate(0, 0, 1)
	return next.Sub(dayStart).Hours()
}

// daysBetween 返回 [a,b) 之间的整天数（a/b 均经 StartOfDay 对齐到午夜，故为精确整数天，无需 +1）。
// 下游用于 older 窗口批次数 olderDays：须与 “for day := retentionStart; day.Before(nearStart)” 实际迭代天数一致，
// 否则 historyCovered 的 reconBatchesDone >= olderDays 判定会永久不满足，导致 R1 连续活跃天数无法计算。
func daysBetween(a, b time.Time) int {
	if !b.After(a) {
		return 0
	}
	return int(b.Sub(a).Hours() / 24)
}

func ptrStr(s string) *string { return &s }
func ptrTime(t time.Time) *time.Time { return &t }
func ptrInt64(i int64) *int64 { return &i }
func usageRiskIntPtr(i int) *int { return &i }

// parsePolicyVersionHex 解析策略版本哈希（16 进制 64 位无符号）为 int64，位保真。
// 与旧 ParseInt 不同：≥2^63 时 ParseInt 会越界 err 并被吞掉静默成 0（生产首跑即炸），
// 此处用 ParseUint 接受全 64 位无符号空间，并以 int64(u) 保位（负数仅表示高位被置位，
// 写库读库仍为同一 64 位比特）。解析失败（非 16 进制/超长）一律返回错误，调用方失败关闭。
func parsePolicyVersionHex(s string) (int64, error) {
	u, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, err
	}
	return int64(u), nil
}
