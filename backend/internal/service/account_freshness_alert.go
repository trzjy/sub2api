package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// 本文件实现 D4（工作项 3「陈旧告警生命周期」+ 验收 6）的告警生命周期管理。
// 设计约束（方案 account-channel-freshness v20 工作项 3 / 验收 6）：
//   - 复用既有 OpsAlertEvent 载体（ops_alert_models.go / ops_alerts.go），不新增邮件/通知链路。
//   - 账号+模型 / 账号级告警只对**当前异常候选**生效：基于「权威观测时间 observed_at」评估陈旧；
//     尝试时间 attempted_at 绝不解除告警。
//   - 恢复按同一原子状态变更关闭对应告警（关闭挂在写入口恢复路径内，非靠轮询收敛）：
//     恢复关闭窄面 ResolveFreshnessAlertOnRecovery（OpsService）由 probe 恢复路径经写入入口
//     注入的 resolver 闭包调用；Evaluate* 在状态回升/观测刷新时也原子关闭。
//   - 健康无流量账号（限流条目已清除 → 无 observed_at）不创建/关闭既有陈旧告警，无永久告警。
//   - 渠道告警只对 ChannelAlertEligible(degraded/failed/error) 生效；operational / 仅无数据
//     （无权威观测时间）渠道不建告警（TokenHarbor 零流量渠道无永久告警）；聚合状态回升时
//     原子关闭（ChannelAlertClosedOnRecovery）。
//   - 候选过期转非活动的账号+模型维（E42 后 precise_reset=true 条目到期被探测候选剔除，但此前
//     可能已因 observed_at 陈旧建了 firing 新鲜度告警，而探测调度对 0 active scope 账号直接跳过、
//     恢复关闭只挂探测成功路径）由本文件 CloseOrphanedFreshnessAlerts 孤儿清扫兜底关闭（有界小集合
//     反向核对条目解析态）；恢复关闭（ResolveFreshnessAlertOnRecovery）仍挂写入口，本清扫只兜孤儿维度。
//   - 孤儿清扫的逐条跳过（reader 读取错误 / reset_at 解析失败或缺失）须以结构化 Warn 暴露（account_id、
//     scope、确定性原因），并沿返回路径聚合成错误上报，使调用方 RunOnce 的 freshness_orphan_sweep_failed
//     Warn 触发；跳过条目仍保持 firing 不关闭（失败关闭语义不变，E46）。

// 陈旧告警维度标识（写入 OpsAlertEvent.Dimensions，用于同维度查/关）。
const (
	freshnessDimKind         = "kind"
	freshnessDimAccountModel = "account_model" // 账号+模型维
	freshnessDimAccountLevel = "account_level" // 账号级维（无模型键）
	freshnessDimChannel      = "channel"       // 渠道维
	freshnessDimAccountID    = "account_id"
	freshnessDimScope        = "scope"
	freshnessDimChannelID    = "channel_id"

	freshnessAlertSeverity = "warning"

	// 限流条目（GetModelRateLimitObservation 返回 map）的键名（与 ratelimit_service.go /
	// model_rate_limit.go 同源同义，仅读取，不改动哨兵常量）。
	freshEntryPreciseResetKey = "precise_reset"
	freshEntryResetAtKey      = "rate_limit_reset_at"
)

// FreshnessAlertStore 陈旧告警持久化抽象（生产由 OpsService 实现，复用 OpsAlertEvent）。
// 仅取 OpsService 的子集能力，便于测试用 fake 实现。
type FreshnessAlertStore interface {
	CreateAlertEvent(ctx context.Context, event *OpsAlertEvent) (*OpsAlertEvent, error)
	GetActiveFreshnessAlert(ctx context.Context, dims map[string]any) (*OpsAlertEvent, error)
	UpdateAlertEventStatus(ctx context.Context, eventID int64, status string, resolvedAt *time.Time) error
	// ListActiveFreshnessAlerts 列出当前 firing 的账号+模型维新鲜度告警（孤儿清扫使用）。
	// 仅返回 kind==account_model 的 firing 告警（其余维度不受本清扫影响）。
	ListActiveFreshnessAlerts(ctx context.Context) ([]*OpsAlertEvent, error)
	// ResolveFreshnessAlertOnRecovery 不受监控开关门禁约束的恢复关闭窄面（E47）：孤儿清扫关闭
	// 孤独 firing 告警时复用之（与 OpsService.ResolveFreshnessAlertOnRecovery 同源，不经
	// RequireMonitoringEnabled），保证监控开关关闭但主动探测仍在运行时，列表可列出孤儿告警、
	// 关闭阶段不因门禁返回 ErrOpsDisabled 而失败（与本清扫「不因监控开关回滚」语义一致）。
	// 无活动告警返回 nil（幂等 no-op）；语义等同 syncFreshness 非 stale 关闭分支，只是不撞门禁。
	ResolveFreshnessAlertOnRecovery(ctx context.Context, dims map[string]any) error
}

// FreshnessObservationReader 读取账号侧观测条目（D2 写入口读面 GetModelRateLimitObservation）。
type FreshnessObservationReader interface {
	GetModelRateLimitObservation(ctx context.Context, accountID int64, scope string) (map[string]any, error)
}

// FreshnessUpperBoundSource 读取某 (account, modelKey) 冻结上界（probe 调度器，D2/C）。
type FreshnessUpperBoundSource interface {
	AccountFreshnessUpperBound(accountID int64, modelKey string) time.Duration
}

// FreshnessChannelDeriver 推导渠道状态（D3 DeriveChannelStatus，返回含 AlertEligible 的推导结果）。
type FreshnessChannelDeriver interface {
	DeriveChannelStatus(ctx context.Context, monitorID int64) (ChannelStatusDerivation, error)
}

// FreshnessAlertService 管理三维状态新鲜度陈旧告警的创建与原子关闭。
type FreshnessAlertService struct {
	store   FreshnessAlertStore
	reader  FreshnessObservationReader
	bounds  FreshnessUpperBoundSource
	deriver FreshnessChannelDeriver
	nowFunc func() time.Time
}

// NewFreshnessAlertService 构造陈旧告警服务。nowFunc 可注入（测试用），默认 time.Now。
func NewFreshnessAlertService(
	store FreshnessAlertStore,
	reader FreshnessObservationReader,
	bounds FreshnessUpperBoundSource,
	deriver FreshnessChannelDeriver,
) *FreshnessAlertService {
	return &FreshnessAlertService{
		store:   store,
		reader:  reader,
		bounds:  bounds,
		deriver: deriver,
		nowFunc: time.Now,
	}
}

// SetFreshnessClock 注入确定性时钟（测试用）。
func (s *FreshnessAlertService) SetFreshnessClock(nowFunc func() time.Time) {
	if s != nil && nowFunc != nil {
		s.nowFunc = nowFunc
	}
}

func (s *FreshnessAlertService) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// --- 账号+模型维 ---

// EvaluateAccountModelFreshness 评估某 (account, scope) 的陈旧状态并同步告警。
// 阈值 = max(账号配置基线, 该候选冻结上界)；陈旧只看 observed_at（尝试时间不解除）。
func (s *FreshnessAlertService) EvaluateAccountModelFreshness(ctx context.Context, accountID int64, scope string) error {
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: accountID, freshnessDimScope: scope}
	threshold := EffectiveFreshnessThreshold(accountModelFreshnessBaseline, s.bounds.AccountFreshnessUpperBound(accountID, scope))
	observedAt, present, err := s.accountObservedAt(ctx, accountID, scope)
	if err != nil {
		// 权威读取失败：跳过本轮告警状态变更（保持现状），不因瞬时 DB 故障错误关闭 firing 告警。
		return err
	}
	stale := present && s.now().Sub(observedAt) > threshold
	return s.syncFreshness(ctx, dims, stale, "账号模型状态陈旧", accountModelAlertDesc(accountID, scope))
}

// EvaluateAccountLevelFreshness 评估账号级（无模型键）维度的陈旧状态并同步告警。
func (s *FreshnessAlertService) EvaluateAccountLevelFreshness(ctx context.Context, accountID int64) error {
	dims := map[string]any{freshnessDimKind: freshnessDimAccountLevel, freshnessDimAccountID: accountID}
	threshold := EffectiveFreshnessThreshold(accountLevelFreshnessBaseline, s.bounds.AccountFreshnessUpperBound(accountID, ""))
	observedAt, present, err := s.accountObservedAt(ctx, accountID, tokenHarborAccountLevelProbeScope)
	if err != nil {
		// 权威读取失败：跳过本轮告警状态变更（保持现状），不因瞬时 DB 故障错误关闭 firing 告警。
		return err
	}
	stale := present && s.now().Sub(observedAt) > threshold
	return s.syncFreshness(ctx, dims, stale, "账号级状态陈旧", accountLevelAlertDesc(accountID))
}

// --- 渠道维 ---

// EvaluateChannelFreshness 评估渠道陈旧状态并同步告警。
// 仅当 ChannelAlertEligible（degraded/failed/error）且有权威观测时间超阈值才告警；
// operational / 仅无数据（无权威观测时间）渠道不建告警；状态回升时原子关闭。
// 渠道阈值默认取渠道配置基线（空账号侧最坏上界）；worst-UB 增强可经 DeriveChannelStatus 注入。
func (s *FreshnessAlertService) EvaluateChannelFreshness(ctx context.Context, channelID int64) error {
	derived, err := s.deriver.DeriveChannelStatus(ctx, channelID)
	if err != nil {
		return err
	}
	dims := map[string]any{freshnessDimKind: freshnessDimChannel, freshnessDimChannelID: channelID}
	// 渠道维阈值同样走三维唯一公式 ComputeChannelFreshnessThreshold（不直接读基线常量）。
	// 账号侧最坏上界入参：D3 推导结果（ChannelStatusDerivation）不暴露账号侧上界/账号 ID，
	// 生产暂未接入 → 传 0，按方案「空集合 = 配置基线」语义取基线（不得退化为零值）。
	// 该未接入项登记于 D4-evidence.md；接入时只改此处唯一入参。
	threshold := ComputeChannelFreshnessThreshold(channelFreshnessBaselineDefault, 0)
	stale := derived.AlertEligible && !derived.ObservedAt.IsZero() && s.now().Sub(derived.ObservedAt) > threshold
	return s.syncFreshness(ctx, dims, stale, "渠道状态陈旧", channelAlertDesc(channelID, derived.Status))
}

// --- 原子关闭（恢复路径） ---

// GetActiveFreshnessAlert 返回该维度当前 firing 的陈旧告警（无则 nil）。
// 管理端透传与告警判读共用同一维度键与同一查询面，保证展示的告警状态与 Evaluate* 一致。
func (s *FreshnessAlertService) GetActiveFreshnessAlert(ctx context.Context, dims map[string]any) (*OpsAlertEvent, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	return s.store.GetActiveFreshnessAlert(ctx, dims)
}

// --- 孤儿告警清扫（候选过期转非活动维度兜底关闭） ---

// CloseOrphanedFreshnessAlerts 以 firing 的账号+模型维新鲜度告警为界（有界小集合）反向核对限流
// 条目解析态，关闭那些「候选已退出/过期转非活动」却再无任何评估/关闭路径的孤儿告警。
//
// 背景（E45，渠道与账号状态新鲜度终审第十一轮 P1 must_fix）：E42 后 precise_reset=true 条目到期
// 被 ActiveTokenHarborFreeTierScopes 从探测候选剔除；该条目此前可能已因 observed_at 陈旧创建 firing
// 新鲜度告警，而 runTokenHarborProbePhase 只对仍在候选列表中的 scope 评估、repo 过滤器对 0 active
// scope 账号直接跳过——过期条目的 (account,scope) 维度再无评估/关闭路径，告警永久 firing。恢复关闭
// （ResolveFreshnessAlertOnRecovery）只挂探测成功路径，条目过期不探测、不关闭。
//
// 本清扫仅兜底孤儿维度，仅复用既有机制（reader 读条目、store 列表查询、未门禁恢复关闭窄面
// ResolveFreshnessAlertOnRecovery），不新增邮件/通知链路、不改候选筛选语义、不引入轮询收敛恢复关闭
// （恢复关闭仍挂写入口）。对每个 firing 的账号+模型维告警，按 (account,scope) 读条目：
//   - 条目不存在 → 候选已退出（清除/过期）→ 关闭（经 resolveFreshnessAlertOnRecovery 未门禁窄面，幂等：
//     并发已 resolved 时 no-op；监控开关关闭也照常关闭，不撞 ErrOpsDisabled，E47）。
//   - 条目存在且 precise_reset===true 且 rate_limit_reset_at 可解析且已过期 → 候选过期转非活动 → 关闭（同窄面）。
//   - 条目存在且仍活动（precise_reset=false 哨兵 / precise=true 未到期 / 缺失 precise_reset）→ 保留。
//   - reader 读取错误 → 跳过该条（保持 firing，失败关闭：不因瞬时读取故障错误关闭），并以
//     freshness_orphan_sweep_skip 结构化 Warn 暴露。
//   - rate_limit_reset_at 解析失败 / 缺失 → 跳过该条（数据不明不关闭，失败关闭），同样以
//     freshness_orphan_sweep_skip 结构化 Warn 暴露。
//
// 可观测性（E46）：上述跳过事实累计，遍历完成后若存在任何跳过条目，返回包含跳过摘要的聚合错误，
// 使 RunOnce 既有 freshness_orphan_sweep_failed Warn 触发（关闭成功但存在跳过 = 部分完成，如实暴露）。
// 跳过条目始终不被关闭（失败关闭语义不变）。
//
// store 列表错误返回错误；reader 为 nil 与既 Evaluate* 同口径保护（直接返回 nil，不阻断）。
func (s *FreshnessAlertService) CloseOrphanedFreshnessAlerts(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	alerts, err := s.store.ListActiveFreshnessAlerts(ctx)
	if err != nil {
		return err
	}
	// reader 为 nil：与 Evaluate* 读面同口径，无观测读面则跳过本轮清扫，保持 firing（不错误关闭）。
	if s.reader == nil {
		return nil
	}
	// skipCount / firstSkip* 累计逐条跳过事实，遍历后聚合成聚合错误上报。
	var skipCount int
	var firstSkipAccount int64
	var firstSkipScope string
	for _, alert := range alerts {
		if alert == nil {
			continue
		}
		// ListActiveFreshnessAlerts 已过滤为账号+模型维（kind=account_model）；
		// 此处仅处理该维，其余维度不受本清扫影响。
		accountID, ok := dimInt64(alert.Dimensions[freshnessDimAccountID])
		if !ok {
			continue
		}
		scope, ok := alert.Dimensions[freshnessDimScope].(string)
		if !ok || scope == "" {
			continue
		}

		entry, rerr := s.reader.GetModelRateLimitObservation(ctx, accountID, scope)
		if rerr != nil {
			// 读取错误 → 跳过（保持 firing，失败关闭），结构化暴露。
			recordSkip(&skipCount, &firstSkipAccount, &firstSkipScope, accountID, scope)
			logger.L().Warn("freshness_orphan_sweep_skip",
				zap.Int64("account_id", accountID),
				zap.String("scope", scope),
				zap.Error(rerr))
			continue
		}

		// 条目不存在 → 候选已退出（清除/过期）→ 关闭。
		if entry == nil {
			if cerr := s.closeAccountModelAlert(ctx, accountID, scope); cerr != nil {
				return cerr
			}
			continue
		}

		// 条目存在：仅当「precise_reset===true 且 reset_at 可解析且已过期」才视作候选过期转非活动而关闭。
		precise, _ := entry[freshEntryPreciseResetKey].(bool)
		if !precise {
			// precise_reset=false 哨兵 / 缺失 precise_reset → 仍活动（E42：哨兵不吃到期剔除），保留。
			continue
		}
		resetAt, present, perr := parseResetAt(entry[freshEntryResetAtKey])
		if perr != nil || !present {
			// reset_at 解析失败 / 缺失 → 数据不明，不关闭（失败关闭），结构化暴露。
			reason := fmt.Errorf("reset_at_unparseable")
			if perr != nil {
				reason = perr
			} else if !present {
				reason = fmt.Errorf("reset_at_missing")
			}
			recordSkip(&skipCount, &firstSkipAccount, &firstSkipScope, accountID, scope)
			logger.L().Warn("freshness_orphan_sweep_skip",
				zap.Int64("account_id", accountID),
				zap.String("scope", scope),
				zap.Error(reason))
			continue
		}
		if resetAt.After(s.now()) {
			// precise=true 未到期 → 仍活动，保留。
			continue
		}
		// precise=true 且已过期 → 候选过期转非活动 → 关闭。
		if cerr := s.closeAccountModelAlert(ctx, accountID, scope); cerr != nil {
			return cerr
		}
	}
	if skipCount > 0 {
		return fmt.Errorf("freshness orphan sweep skipped %d entries (first: account=%d scope=%s)",
			skipCount, firstSkipAccount, firstSkipScope)
	}
	return nil
}

// recordSkip 累计一条跳过事实（保持 firing 不关闭，仅暴露）。首次跳过记录 account/scope 用于聚合错误摘要。
func recordSkip(count *int, firstAccount *int64, firstScope *string, accountID int64, scope string) {
	if count == nil {
		return
	}
	if *count == 0 {
		if firstAccount != nil {
			*firstAccount = accountID
		}
		if firstScope != nil {
			*firstScope = scope
		}
	}
	*count++
}

// closeAccountModelAlert 经未门禁窄面 ResolveFreshnessAlertOnRecovery 幂等关闭指定 (account,scope)
// 维度 firing 告警（与 OpsService 恢复关闭同源，不经 RequireMonitoringEnabled；并发已 resolved 时
// no-op）。孤儿清扫语义「不因监控开关回滚」由此保证：监控关闭时主动探测仍运行，列表可列出孤儿告警，
// 关闭阶段不因门禁失败（E47）。Evaluate* 路径仍走 syncFreshness（既有门禁行为，不在本单范围）。
func (s *FreshnessAlertService) closeAccountModelAlert(ctx context.Context, accountID int64, scope string) error {
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: accountID, freshnessDimScope: scope}
	return s.store.ResolveFreshnessAlertOnRecovery(ctx, dims)
}

// dimInt64 从告警维度值中提取 int64（兼容 JSON 反序列化后 float64 与内存中 int64 两种表示）。
func dimInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

// parseResetAt 解析 rate_limit_reset_at 字符串（同 ActiveTokenHarborFreeTierScopes 口径：
// RFC3339Nano / RFC3339）。返回 (时间, 是否可解析为有效时刻, 解析错误)。空/缺省 → (零值, false, nil)；
// 类型非法或格式非法 → (零值, false, err)（调用方据此失败关闭，不错误关闭）。
func parseResetAt(raw any) (time.Time, bool, error) {
	if raw == nil {
		return time.Time{}, false, nil
	}
	s, ok := raw.(string)
	if !ok {
		return time.Time{}, false, fmt.Errorf("invalid rate_limit_reset_at: wrong type %T (want string)", raw)
	}
	if strings.TrimSpace(s) == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("invalid rate_limit_reset_at %q: unparseable", s)
}

// --- 内部 ---

// accountObservedAt 读取账号侧某 scope 的权威观测时间（仅 observed_at；attempted_at 不计入）。
// 返回 (时间, 是否存在有效观测, 读取错误)。条目不存在 → (零值, false, nil) → 不陈旧
// （健康无流量无永久告警）；仓储读取错误 → (零值, false, err)，调用方须跳过本轮告警
// 状态变更（不创建/不关闭），只有权威读取成功才允许创建/关闭。
// 权威数据损坏（observed_at 存在但类型非法或格式非法）→ (零值, false, err)：不得降级为
// 「无观测」而错误关闭已有 firing 告警，与仓储读取错误同口径（失败关闭，第三轮终审 #5，
// 类型维度为第四轮终审 E21 补全）。
// 类型判别与解析口径与视图 BuildAccountFreshnessView 共用 observedAtFromEntry +
// parseObservedAt，展示/告警同一事实源。
func (s *FreshnessAlertService) accountObservedAt(ctx context.Context, accountID int64, scope string) (time.Time, bool, error) {
	entry, err := s.reader.GetModelRateLimitObservation(ctx, accountID, scope)
	if err != nil {
		return time.Time{}, false, err
	}
	if entry == nil {
		return time.Time{}, false, nil
	}
	// observed_at 类型判别与视图 BuildAccountFreshnessView 共用 observedAtFromEntry
	// （同一事实源）：键不存在 / null / 空串 / 仅空白 = 无观测；存在但类型非 string =
	// 权威数据损坏，失败关闭；合法 string 交 parseObservedAt 判格式。
	raw, terr := observedAtFromEntry(entry)
	if terr != nil {
		return time.Time{}, false, terr
	}
	observedAt, present, err := parseObservedAt(raw)
	if err != nil {
		return time.Time{}, false, err
	}
	// 保留既有「零值时间视作无观测」语义：RFC3339 零值（0001-01-01T00:00:00Z）非有效观测。
	if !present || observedAt.IsZero() {
		return time.Time{}, false, nil
	}
	return observedAt, true, nil
}

// syncFreshness 落盘陈旧告警的创建/关闭：stale 且无活动告警 → 创建 firing；
// 非 stale 且有活动告警 → 原子关闭（resolved）。store 错误沿调用链传播（失败关闭），不静默吞错：
// Get 错误返回 err；Create 错误返回 err；Update(close) 错误返回 err；「无活动告警」保持正常空结果
// （nil），不是错误。Evaluate* 调用方对返回错误仅 Warn 记录，不阻断探测主路径（既定口径）。
func (s *FreshnessAlertService) syncFreshness(ctx context.Context, dims map[string]any, stale bool, title, desc string) error {
	active, err := s.store.GetActiveFreshnessAlert(ctx, dims)
	if err != nil {
		return err
	}
	if stale {
		if active == nil {
			if _, cerr := s.store.CreateAlertEvent(ctx, &OpsAlertEvent{
				Status:      OpsAlertStatusFiring,
				Severity:    freshnessAlertSeverity,
				Title:       title,
				Description: desc,
				Dimensions:  dims,
				FiredAt:     s.now(),
			}); cerr != nil {
				return cerr
			}
		}
		return nil
	}
	// 非陈旧（恢复 / 观测刷新 / operational / 健康无流量）：原子关闭已有告警。
	if active != nil {
		resolvedAt := s.now()
		if rerr := s.store.UpdateAlertEventStatus(ctx, active.ID, OpsAlertStatusResolved, &resolvedAt); rerr != nil {
			return rerr
		}
	}
	return nil
}

func accountModelAlertDesc(accountID int64, scope string) string {
	return "账号 " + strconv.FormatInt(accountID, 10) + " 模型 " + scope + " 的权威状态观测超过有效阈值未刷新"
}

func accountLevelAlertDesc(accountID int64) string {
	return "账号 " + strconv.FormatInt(accountID, 10) + " 账号级（无模型键）状态观测超过有效阈值未刷新"
}

func channelAlertDesc(channelID int64, status string) string {
	return "渠道 " + strconv.FormatInt(channelID, 10) + " 聚合状态 " + status + " 且权威观测时间超过阈值"
}

// FreshnessAccountModelDims / FreshnessAccountLevelDims / FreshnessChannelDims 构造与
// 陈旧告警一致的维度键（管理端查询活跃告警时使用，保证同维度匹配）。
func FreshnessAccountModelDims(accountID int64, scope string) map[string]any {
	return map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: accountID, freshnessDimScope: scope}
}

func FreshnessAccountLevelDims(accountID int64) map[string]any {
	return map[string]any{freshnessDimKind: freshnessDimAccountLevel, freshnessDimAccountID: accountID}
}

func FreshnessChannelDims(channelID int64) map[string]any {
	return map[string]any{freshnessDimKind: freshnessDimChannel, freshnessDimChannelID: channelID}
}
