package service

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 本文件实现 C4（渠道监控聚合水位停滞告警）。设计约束（方案 §2.6）：
//   - 共享持久水位 = 聚合 leader 完整扫描成功后由 RecomputeRange 持久化的
//     channel_monitor_v2_watermarks.last_successful_at（DB 共享；仅 leader 写，本检测体只读）。
//     复用既有机制，不新建表/字段。
//   - 检测体独立于聚合 worker 与 channel_monitor_mode 门控：即使 worker 完全未装配 / mode 关，
//     只要本进程仍运行 ops 告警评估链（OpsAlertEvaluatorService 独立 loop），就能读到共享水位并告警。
//   - 判据（DB 时钟，now 取自注入时钟，默认 time.Now）：
//     冷启动：进程启动 30 分钟内水位缺失 → 不告警；宽限期满仍缺失 → 告警（覆盖 worker 永不装配死区）。
//     首次成功扫描后：水位年龄 > 2× 配置刷新周期（严格大于）→ 告警。
//     恢复（水位刷新）→ 自动 resolve。
//   - 幂等键 = kind（channel_monitor_stale）+ 固定维度；沿既有 ops_alert 唯一活跃机制
//     （ListAlertEvents + DimensionExact），防重复创建 / 重复 resolve。
//   - 通知送达（邮件）沿既有 ops_alert 通知链（maybeSendAlertEmail），不新造。

const (
	channelMonitorV2StaleKind = "channel_monitor_stale"

	channelMonitorV2StaleSeverity = "warning"

	// channelMonitorV2StaleColdStartGrace 冷启动宽限：进程启动后 30 分钟内水位缺失不告警。
	channelMonitorV2StaleColdStartGrace = 30 * time.Minute

	// channelMonitorV2StaleRefreshDefault 配置刷新周期缺省值（秒级 RefreshIntervalSeconds 缺省 300s）。
	channelMonitorV2StaleRefreshDefault = 300 * time.Second

	// channelMonitorV2StaleFactor 水位年龄超过配置刷新周期的该倍数（严格大于）即判定停滞。
	channelMonitorV2StaleFactor = 2
)

// channelMonitorV2StaleProcessStart 进程启动时刻（文件加载即 capture，用于冷启动宽限）。
var channelMonitorV2StaleProcessStart = time.Now()

// channelMonitorV2StaleSource 检测体读取共享水位与配置的最小接口（由 ChannelMonitorV2Repository 满足）。
type channelMonitorV2StaleSource interface {
	GetAggregationWatermark(ctx context.Context) (*ChannelMonitorV2AggregationWatermark, error)
	GetConfig(ctx context.Context) (*ChannelMonitorV2Config, error)
}

// channelMonitorV2StaleOpsRepo 检测体落盘告警事件的最小接口（由 OpsRepository 满足）。
type channelMonitorV2StaleOpsRepo interface {
	ListAlertEvents(ctx context.Context, filter *OpsAlertEventFilter) ([]*OpsAlertEvent, error)
	CreateAlertEvent(ctx context.Context, event *OpsAlertEvent) (*OpsAlertEvent, error)
	UpdateAlertEventStatus(ctx context.Context, eventID int64, status string, resolvedAt *time.Time) error
	UpdateAlertEventEmailSent(ctx context.Context, eventID int64, emailSent bool) error
}

// channelMonitorV2StaleNotifier 邮件送达回调（生产由 OpsAlertEvaluatorService 注入 maybeSendAlertEmail）。
type channelMonitorV2StaleNotifier func(ctx context.Context, runtimeCfg *OpsAlertRuntimeSettings, event *OpsAlertEvent) bool

// ChannelMonitorV2StaleAlertEvaluator 聚合水位停滞检测体。独立于聚合 worker / mode 门控，
// 仅依赖共享持久水位与 ops 告警事件载体（复用既有 OpsAlertEvent）。
type ChannelMonitorV2StaleAlertEvaluator struct {
	source  channelMonitorV2StaleSource
	opsRepo channelMonitorV2StaleOpsRepo

	nowFunc    func() time.Time
	graceStart time.Time
	notify     channelMonitorV2StaleNotifier
}

// NewChannelMonitorV2StaleAlertEvaluator 构造检测体。notify 可为 nil（不发送邮件，安全 no-op）。
func NewChannelMonitorV2StaleAlertEvaluator(source channelMonitorV2StaleSource, opsRepo channelMonitorV2StaleOpsRepo) *ChannelMonitorV2StaleAlertEvaluator {
	return &ChannelMonitorV2StaleAlertEvaluator{
		source:     source,
		opsRepo:    opsRepo,
		nowFunc:    time.Now,
		graceStart: channelMonitorV2StaleProcessStart,
	}
}

// SetClock 注入确定性时钟（测试用）。
func (e *ChannelMonitorV2StaleAlertEvaluator) SetClock(f func() time.Time) {
	if e != nil && f != nil {
		e.nowFunc = f
	}
}

// SetGraceStart 覆盖冷启动宽限起点（测试用）。
func (e *ChannelMonitorV2StaleAlertEvaluator) SetGraceStart(t time.Time) {
	if e != nil {
		e.graceStart = t
	}
}

// SetNotifier 注入邮件送达回调（生产由 OpsAlertEvaluatorService 注入）。
func (e *ChannelMonitorV2StaleAlertEvaluator) SetNotifier(fn channelMonitorV2StaleNotifier) {
	if e != nil {
		e.notify = fn
	}
}

func (e *ChannelMonitorV2StaleAlertEvaluator) now() time.Time {
	if e.nowFunc != nil {
		return e.nowFunc()
	}
	return time.Now()
}

func channelMonitorV2StaleDims() map[string]any {
	return map[string]any{"kind": channelMonitorV2StaleKind}
}

func channelMonitorV2StaleAlertRule() *OpsAlertRule {
	return &OpsAlertRule{
		Name:        "Channel Monitor Aggregation Stale",
		Severity:    channelMonitorV2StaleSeverity,
		NotifyEmail: true,
	}
}

// refreshInterval 读取配置刷新周期；缺省回退到 channelMonitorV2StaleRefreshDefault。
func (e *ChannelMonitorV2StaleAlertEvaluator) refreshInterval(ctx context.Context) time.Duration {
	if e.source != nil {
		if cfg, err := e.source.GetConfig(ctx); err == nil && cfg != nil && cfg.RefreshIntervalSeconds > 0 {
			return time.Duration(cfg.RefreshIntervalSeconds) * time.Second
		}
	}
	return channelMonitorV2StaleRefreshDefault
}

// isStale 按判据判定停滞：
//   - 水位缺失（LastSuccessfulAt 零值）：冷启动宽限内不告警，宽限期满（进程启动 > 30min）仍缺失 → 停滞。
//   - 水位存在：年龄 > 2× 刷新周期（严格大于）→ 停滞。
func (e *ChannelMonitorV2StaleAlertEvaluator) isStale(now time.Time, wm *ChannelMonitorV2AggregationWatermark, threshold time.Duration) bool {
	if wm == nil || wm.LastSuccessfulAt.IsZero() {
		return now.After(e.graceStart.Add(channelMonitorV2StaleColdStartGrace))
	}
	return now.Sub(wm.LastSuccessfulAt) > threshold
}

func (e *ChannelMonitorV2StaleAlertEvaluator) describeStale(wm *ChannelMonitorV2AggregationWatermark, refresh, threshold time.Duration, now time.Time) string {
	if wm == nil || wm.LastSuccessfulAt.IsZero() {
		return fmt.Sprintf("渠道监控聚合水位自进程启动已缺失超过 %s（配置刷新周期 %s，停滞阈值 %s）",
			channelMonitorV2StaleColdStartGrace, refresh, threshold)
	}
	age := now.Sub(wm.LastSuccessfulAt)
	return fmt.Sprintf("渠道监控聚合水位停滞：上次成功扫描 %s 前（年龄 %s > 停滞阈值 %s，配置刷新周期 %s）",
		wm.LastSuccessfulAt.Format(time.RFC3339), age, threshold, refresh)
}

// getActiveAlert 返回该维度当前 firing 的停滞告警（无则 nil）。沿用既有 ops_alert 唯一活跃机制：
// ListAlertEvents + DimensionExact 精确匹配，不做固定上限内存过滤。
func (e *ChannelMonitorV2StaleAlertEvaluator) getActiveAlert(ctx context.Context, dims map[string]any) (*OpsAlertEvent, error) {
	events, err := e.opsRepo.ListAlertEvents(ctx, &OpsAlertEventFilter{
		Status:         OpsAlertStatusFiring,
		DimensionExact: dims,
	})
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		if ev != nil {
			return ev, nil
		}
	}
	return nil, nil
}

// Evaluate 单次评估：创建/自动 resolve 渠道监控聚合水位停滞告警（幂等）。
// 仅在水位的 DB 读取或事件落盘失败时返回错误（失败不错误关闭，保持现状）。
func (e *ChannelMonitorV2StaleAlertEvaluator) Evaluate(ctx context.Context, runtimeCfg *OpsAlertRuntimeSettings) error {
	if e == nil || e.source == nil || e.opsRepo == nil {
		return nil
	}
	now := e.now()

	wm, err := e.source.GetAggregationWatermark(ctx)
	if err != nil {
		return err
	}

	refresh := e.refreshInterval(ctx)
	threshold := time.Duration(channelMonitorV2StaleFactor) * refresh
	stale := e.isStale(now, wm, threshold)

	dims := channelMonitorV2StaleDims()
	active, err := e.getActiveAlert(ctx, dims)
	if err != nil {
		return err
	}

	if stale {
		// 已有 firing 告警：幂等 no-op（不重复创建、不重复发信）。
		if active != nil {
			return nil
		}
		created, cerr := e.opsRepo.CreateAlertEvent(ctx, &OpsAlertEvent{
			Status:      OpsAlertStatusFiring,
			Severity:    channelMonitorV2StaleSeverity,
			Title:       "渠道监控聚合水位停滞",
			Description: e.describeStale(wm, refresh, threshold, now),
			Dimensions:  dims,
			FiredAt:     now,
			CreatedAt:   now,
		})
		if cerr != nil {
			return cerr
		}
		if created != nil && created.ID > 0 && e.notify != nil {
			if e.notify(ctx, runtimeCfg, created) {
				_ = e.opsRepo.UpdateAlertEventEmailSent(ctx, created.ID, true)
			}
		}
		return nil
	}

	// 非停滞（恢复 / 水位刷新 / 冷启动宽限内）：原子关闭已有告警。
	if active != nil {
		resolvedAt := now
		if rerr := e.opsRepo.UpdateAlertEventStatus(ctx, active.ID, OpsAlertStatusResolved, &resolvedAt); rerr != nil {
			return rerr
		}
	}
	return nil
}

// --- 接线：ops 告警评估链集成的共享水位源注册 ---

// channelMonitorV2StaleWatermarkSource 是 ops 评估链读取的共享水位源（由 NewChannelMonitorV2Aggregator 注册）。
var channelMonitorV2StaleWatermarkSource channelMonitorV2StaleSource

// SetChannelMonitorV2StaleSource 注册共享水位源。由聚合器构造时调用；即使聚合 worker 完全未
// 装配（Start 未调用），只要 ChannelMonitorV2Repository 存在，检测体仍能读取共享水位并告警。
func SetChannelMonitorV2StaleSource(s channelMonitorV2StaleSource) {
	channelMonitorV2StaleWatermarkSource = s
}

// evaluateChannelMonitorV2Stale 由 OpsAlertEvaluatorService.evaluateOnce 调用：独立于聚合 worker /
// mode 门控，运行既有的 ops 告警评估 loop 中。水源缺失（source 未注册）时安全 no-op。
func (s *OpsAlertEvaluatorService) evaluateChannelMonitorV2Stale(ctx context.Context, runtimeCfg *OpsAlertRuntimeSettings) {
	src := channelMonitorV2StaleWatermarkSource
	if src == nil {
		return
	}
	s.mu.Lock()
	if s.channelMonitorV2Stale == nil {
		e := NewChannelMonitorV2StaleAlertEvaluator(src, s.opsRepo)
		// 邮件沿既有 ops_alert 通知链（maybeSendAlertEmail），不新造。
		e.notify = func(c context.Context, rcfg *OpsAlertRuntimeSettings, ev *OpsAlertEvent) bool {
			return s.maybeSendAlertEmail(c, rcfg, channelMonitorV2StaleAlertRule(), ev)
		}
		s.channelMonitorV2Stale = e
	}
	s.mu.Unlock()

	if err := s.channelMonitorV2Stale.Evaluate(ctx, runtimeCfg); err != nil {
		logger.LegacyPrintf("service.ops_alert_evaluator", "[OpsAlertEvaluator] channel monitor v2 stale evaluation failed: %v", err)
	}
}
