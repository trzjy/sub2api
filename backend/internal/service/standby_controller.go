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
}

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
		evaluator: evaluator,
		covered:   covered,
		registry:  registry,
		interval:  opts.Interval,
		now:       opts.Now,
		logger:    opts.Logger,
		machines:  make(map[string]*StandbyStateMachine, 8),
		warned:    make(map[string]struct{}, 8),
	}
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

// RunOnce 执行一轮完整评估。覆盖清单查询失败即整轮作废（不迁移任何模型）；
// 单模型采集失败由采集器判 Inconclusive，同样不迁移。
func (c *StandbyController) RunOnce(ctx context.Context) {
	if c == nil || c.evaluator == nil || c.covered == nil {
		return
	}
	models, err := c.covered.ListCoveredModels(ctx)
	if err != nil {
		c.logger.Warn("standby_covered_models_failed", "error", err)
		return
	}
	now := c.now()
	for _, model := range models {
		c.evaluateModel(ctx, model, now)
	}
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
