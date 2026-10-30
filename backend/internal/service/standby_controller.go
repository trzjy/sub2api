package service

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// 主力渠道延迟/故障自适应备用切换 · 控制器（第 2a 片）。
//
// 职责：周期性遍历"覆盖的模型"（来自生产库只读查询的 codebuddy 账号
// model_mapping 键），对每个模型跑一次采集 + 唯一判定函数，把结果推给状态机，
// 迁移发生时写注册表并打结构化日志。
//
// 本文件不接线调度：不改选号、不改账号、不写库。调度行为零变化。

// StandbyDefaultPollInterval 是控制器默认轮询间隔（L12 覆盖范围的评估节奏）。
const StandbyDefaultPollInterval = 30 * time.Second

// StandbyEvaluator 是采集 + 判定的单入口（由 *StandbyHealthCollector 实现）。
// 抽成接口是为了单测注入可控判定。
type StandbyEvaluator interface {
	Evaluate(ctx context.Context, model, platform string, now time.Time) (StandbyHealthInput, StandbyVerdict)
}

// StandbyCoveredModelSource 提供"需要评估的模型"清单（只读）。
// 生产实现见 repository.NewStandbyCoveredRepository。
type StandbyCoveredModelSource interface {
	ListCoveredModels(ctx context.Context) ([]string, error)
}

// StandbyControllerOptions 是控制器可选项；零值即生产默认。
type StandbyControllerOptions struct {
	// Interval 轮询间隔，<=0 用 StandbyDefaultPollInterval。
	Interval time.Duration
	// Now 时钟，nil 用 time.Now（单测注入）。
	Now func() time.Time
	// Logger 日志器，nil 用 slog.Default()。
	Logger *slog.Logger
	// ForceOpenModels 是人工强制开启的模型键清单（第 3b 片）：命中者跳过采集与
	// 状态机，直接置 open，reason=force_open。
	ForceOpenModels []string
	// ForceClosedModels 是人工强制关闭的模型键清单：命中者跳过采集与状态机，
	// 直接置 closed，reason=force_closed。与 ForceOpenModels 同时命中时本表优先。
	ForceClosedModels []string
}

// 强制表的审计原因；与自动判定的 reason（如 ttft_p50_above_enter）一起出现在
// 注册表与 standby_transition 日志里，便于事后区分是人工还是自动置位。
const (
	StandbyForceReasonOpen   = "force_open"
	StandbyForceReasonClosed = "force_closed"
)

// StandbyController 周期性把判定结果落到 StandbyRegistry。
type StandbyController struct {
	evaluator StandbyEvaluator
	covered   StandbyCoveredModelSource
	registry  *StandbyRegistry
	interval  time.Duration
	now       func() time.Time
	logger    *slog.Logger

	mu sync.Mutex
	// machines 是每个模型独立的状态机：确认计时按模型各算各的。
	machines map[string]*StandbyStateMachine
	// warned 保证"模型名无法推导平台"每个进程只告警一次，避免 30s 刷屏。
	warned map[string]struct{}
	// forceOpen / forceClosed 是人工强制表（第 3b 片）的集合形式，构造时定型：
	// env 在一个进程生命周期内不变，运行期不需要重读。
	forceOpen   map[string]struct{}
	forceClosed map[string]struct{}

	stopCh   chan struct{}
	stopping bool
	wg       sync.WaitGroup
}

// NewStandbyController 构造控制器。registry 为 nil 时新建一个（仍只在本进程内）。
func NewStandbyController(
	evaluator StandbyEvaluator,
	covered StandbyCoveredModelSource,
	registry *StandbyRegistry,
	opts StandbyControllerOptions,
) *StandbyController {
	if opts.Interval <= 0 {
		opts.Interval = StandbyDefaultPollInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if registry == nil {
		registry = NewStandbyRegistry()
	}
	return &StandbyController{
		evaluator:   evaluator,
		covered:     covered,
		registry:    registry,
		interval:    opts.Interval,
		now:         opts.Now,
		logger:      opts.Logger,
		machines:    make(map[string]*StandbyStateMachine, 8),
		warned:      make(map[string]struct{}, 8),
		forceOpen:   standbyForceSet(opts.ForceOpenModels),
		forceClosed: standbyForceSet(opts.ForceClosedModels),
	}
}

// standbyForceSet 把配置里的模型键清单转成集合：去空白、丢空串。
// 不做通配符展开——强制表按模型键精确匹配，与注册表 key 同口径。
func standbyForceSet(models []string) map[string]struct{} {
	if len(models) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		set[model] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// NewProcessStandbyController 构造控制器，并把它的注册表挂成进程级单例（第 3 片装配）。
//
// 调度层只读 DefaultStandbyRegistry()（见 standbyRoutingOpen），控制器必须写同一实例，
// 否则选号层永远读到空表、备用开启也不会前置任何账号。setDefaultStandbyRegistry 只在
// 服务包内可见，故由本构造函数收口；注册表只存活在本进程内，重启即空表 = 全部 closed。
func NewProcessStandbyController(
	evaluator StandbyEvaluator,
	covered StandbyCoveredModelSource,
	opts StandbyControllerOptions,
) *StandbyController {
	registry := NewStandbyRegistry()
	setDefaultStandbyRegistry(registry)
	return NewStandbyController(evaluator, covered, registry, opts)
}

// Registry 返回控制器写入的注册表，供调度层只读消费。
func (c *StandbyController) Registry() *StandbyRegistry {
	if c == nil {
		return nil
	}
	return c.registry
}

// Start 启动轮询循环（先立即跑一轮再按 interval 轮询）。重复调用无副作用。
func (c *StandbyController) Start(ctx context.Context) {
	if c == nil || c.evaluator == nil || c.covered == nil {
		return
	}
	c.mu.Lock()
	if c.stopCh != nil || c.stopping {
		c.mu.Unlock()
		return
	}
	stopCh := make(chan struct{})
	c.stopCh = stopCh
	c.wg.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.wg.Done()
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		c.RunOnce(ctx)
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.RunOnce(ctx)
			}
		}
	}()
}

// Stop 终止轮询循环；可安全重复调用。
func (c *StandbyController) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.stopCh == nil || c.stopping {
		c.mu.Unlock()
		return
	}
	c.stopping = true
	stopCh := c.stopCh
	c.stopCh = nil
	c.mu.Unlock()

	close(stopCh)
	c.wg.Wait()

	c.mu.Lock()
	c.stopping = false
	c.mu.Unlock()
}

// RunOnce 执行一轮完整评估。
//
// 覆盖清单查询失败即自动判定整轮作废（不迁移任何模型）；但人工强制表（第 3b 片）
// 不依赖任何信号与数据库，仍照常置位——止血开关不能因为查库失败而失效。
// 单模型采集失败由采集器判 Inconclusive，同样不迁移。
func (c *StandbyController) RunOnce(ctx context.Context) {
	if c == nil || c.evaluator == nil || c.covered == nil {
		return
	}
	models, err := c.covered.ListCoveredModels(ctx)
	if err != nil {
		c.logger.Warn("standby_covered_models_failed", "error", err)
		models = nil
	}
	now := c.now()
	// 强制表里的模型即使不在覆盖清单内也要生效（运维可以直接点名一个模型止血），
	// 因此本轮待处理集合 = 覆盖清单 ∪ 两张强制表。
	seen := make(map[string]struct{}, len(models)+len(c.forceOpen)+len(c.forceClosed))
	for _, model := range c.roundModels(models) {
		model = strings.TrimSpace(model)
		if _, dup := seen[model]; dup {
			continue
		}
		seen[model] = struct{}{}
		c.evaluateModel(ctx, model, now)
	}
}

// roundModels 返回本轮待评估的模型键：覆盖清单在前（保持原顺序），强制表在后。
func (c *StandbyController) roundModels(covered []string) []string {
	models := make([]string, 0, len(covered)+len(c.forceOpen)+len(c.forceClosed))
	models = append(models, covered...)
	for model := range c.forceOpen {
		models = append(models, model)
	}
	for model := range c.forceClosed {
		models = append(models, model)
	}
	return models
}

// evaluateModel 评估单个模型并推进其状态机。
func (c *StandbyController) evaluateModel(ctx context.Context, model string, now time.Time) {
	model = strings.TrimSpace(model)
	// 通配符键（如 "kimi-*"）不是具体模型，无法作为信号键。
	if model == "" || strings.ContainsAny(model, "*?") {
		return
	}
	platform, ok := StandbyPlatformForModel(model)
	if !ok {
		c.warnUnknownModel(model)
		return
	}

	// 人工强制表优先于采集与状态机：命中即定态，本轮不采集、不推进状态机。
	if phase, reason, ok := c.forcedPhase(model); ok {
		c.applyForced(model, platform, phase, reason, now)
		return
	}

	in, verdict := c.evaluator.Evaluate(ctx, model, platform, now)
	transition := c.machineFor(model).Observe(verdict, now)

	status := StandbyStatus{
		Platform:      platform,
		Open:          transition.To == StandbyPhaseOpen,
		Verdict:       verdict,
		Reason:        standbyStatusReason(in, verdict),
		BaselineP50Ms: derefFloat(in.TTFTBaselineP50Ms),
		CurrentP50Ms:  derefFloat(in.TTFTCurrentP50Ms),
		EvaluatedAt:   now,
	}
	if transition.Changed {
		// 迁移瞬间没有待确认 verdict，Since 记迁移时刻。
		status.Since = now
	} else if !transition.Since.IsZero() {
		status.Since = transition.Since
	}
	if status.Open {
		if transition.Changed {
			status.OpenedAt = now
		} else if prev, ok := c.registry.Status(model); ok {
			status.OpenedAt = prev.OpenedAt
		}
	}

	c.registry.set(model, status)

	if transition.Changed {
		c.logger.Info("standby_transition",
			"model", model,
			"platform", platform,
			"from", transition.From.String(),
			"to", transition.To.String(),
			"verdict", verdict.String(),
			"reason", status.Reason,
			"baseline_p50_ms", status.BaselineP50Ms,
			"current_p50_ms", status.CurrentP50Ms,
		)
	}
}

// forcedPhase 报告该模型是否被人工强制表命中，并给出目标相位与审计原因。
// 两张表同时命中时 force_closed 优先（止血优先），并每进程告警一次。
func (c *StandbyController) forcedPhase(model string) (StandbyPhase, string, bool) {
	_, closed := c.forceClosed[model]
	_, open := c.forceOpen[model]
	switch {
	case closed && open:
		c.warnForceConflict(model)
		return StandbyPhaseClosed, StandbyForceReasonClosed, true
	case closed:
		return StandbyPhaseClosed, StandbyForceReasonClosed, true
	case open:
		return StandbyPhaseOpen, StandbyForceReasonOpen, true
	default:
		return StandbyPhaseClosed, "", false
	}
}

// applyForced 把人工强制结果写进注册表；只有相位/原因变化时才打迁移日志，
// 避免每轮（默认 30s）刷屏。强制路径不采集，Verdict 固定 Inconclusive（判定未发生）。
func (c *StandbyController) applyForced(model, platform string, phase StandbyPhase, reason string, now time.Time) {
	prev, hasPrev := c.registry.Status(model)
	status := StandbyStatus{
		Platform:    platform,
		Open:        phase == StandbyPhaseOpen,
		Verdict:     StandbyVerdictInconclusive,
		Reason:      reason,
		EvaluatedAt: now,
	}
	changed := !hasPrev || prev.Open != status.Open || prev.Reason != reason
	switch {
	case !changed:
		status.Since = prev.Since
		status.OpenedAt = prev.OpenedAt
	case status.Open:
		// 刚被强制开启：Since 与 OpenedAt 都记本次置位时刻。
		status.Since = now
		status.OpenedAt = now
	default:
		status.Since = now
	}

	c.registry.set(model, status)

	if !changed {
		return
	}
	from := StandbyPhaseClosed
	if hasPrev && prev.Open {
		from = StandbyPhaseOpen
	}
	c.logger.Info("standby_transition",
		"model", model,
		"platform", platform,
		"from", from.String(),
		"to", phase.String(),
		"verdict", status.Verdict.String(),
		"reason", reason,
		"forced", true,
	)
}

// warnForceConflict 保证同一模型的"两张强制表同时命中"每进程只告警一次。
func (c *StandbyController) warnForceConflict(model string) {
	key := "force_conflict:" + model
	c.mu.Lock()
	_, seen := c.warned[key]
	if !seen {
		c.warned[key] = struct{}{}
	}
	c.mu.Unlock()
	if seen {
		return
	}
	c.logger.Warn("standby_force_conflict",
		"model", model,
		"chosen", StandbyForceReasonClosed,
		"ignored", StandbyForceReasonOpen,
	)
}

// machineFor 返回（必要时创建）该模型的状态机。确认时长复用第 1 片常量。
func (c *StandbyController) machineFor(model string) *StandbyStateMachine {
	c.mu.Lock()
	defer c.mu.Unlock()
	machine, ok := c.machines[model]
	if !ok {
		machine = NewStandbyStateMachine(StandbyConfirmDuration)
		c.machines[model] = machine
	}
	return machine
}

func (c *StandbyController) warnUnknownModel(model string) {
	c.mu.Lock()
	_, seen := c.warned[model]
	if !seen {
		c.warned[model] = struct{}{}
	}
	c.mu.Unlock()
	if seen {
		return
	}
	c.logger.Warn("standby_model_platform_unknown", "model", model)
}

// StandbyPlatformForModel 把模型名映射到调度期目标平台（L3 的模型→平台推导）。
// 只认主力渠道的三个供给平台；composite 分组下的模型同样按模型名归属到具体平台。
func StandbyPlatformForModel(model string) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(name, "deepseek"):
		return domain.PlatformDeepseek, true
	case strings.HasPrefix(name, "kimi"):
		return domain.PlatformKimi, true
	case strings.HasPrefix(name, "glm"):
		return domain.PlatformZhipu, true
	default:
		return "", false
	}
}

// standbyStatusReason 复用判定层的单信号三态，给出审计原因（不复制阈值逻辑）。
func standbyStatusReason(in StandbyHealthInput, verdict StandbyVerdict) string {
	switch verdict {
	case StandbyVerdictDegraded:
		reasons := make([]string, 0, 3)
		if standbyTTFTSignal(in) == standbySignalDegraded {
			reasons = append(reasons, "ttft_p50_above_enter")
		}
		if standbyUpstreamThrottleSignal(in) == standbySignalDegraded {
			reasons = append(reasons, "upstream_throttle_rate_above_enter")
		}
		if standbySchedulableSignal(in) == standbySignalDegraded {
			reasons = append(reasons, "schedulable_ratio_below_enter")
		}
		return strings.Join(reasons, ",")
	case StandbyVerdictHealthy:
		return "signals_recovered"
	default:
		if standbyAllSignalsUnavailable(in) {
			return "no_signal_data"
		}
		return "inconclusive"
	}
}

// standbyAllSignalsUnavailable 报告三个信号是否全部不可用（含采集整轮失败）。
func standbyAllSignalsUnavailable(in StandbyHealthInput) bool {
	return standbyTTFTSignal(in) == standbySignalUnavailable &&
		standbyUpstreamThrottleSignal(in) == standbySignalUnavailable &&
		standbySchedulableSignal(in) == standbySignalUnavailable
}

func derefFloat(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
