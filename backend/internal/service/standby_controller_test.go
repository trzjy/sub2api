//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type standbyEvalCall struct {
	model    string
	platform string
}

type fakeStandbyEvaluator struct {
	mu       sync.Mutex
	verdicts map[string]StandbyVerdict
	inputs   map[string]StandbyHealthInput
	calls    []standbyEvalCall
}

func newFakeStandbyEvaluator() *fakeStandbyEvaluator {
	return &fakeStandbyEvaluator{
		verdicts: make(map[string]StandbyVerdict),
		inputs:   make(map[string]StandbyHealthInput),
	}
}

func (f *fakeStandbyEvaluator) Evaluate(_ context.Context, model, platform string, _ time.Time) (StandbyHealthInput, StandbyVerdict) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, standbyEvalCall{model: model, platform: platform})
	in, ok := f.inputs[model]
	if !ok {
		in = StandbyHealthInput{Model: model}
	}
	verdict, ok := f.verdicts[model]
	if !ok {
		verdict = StandbyVerdictInconclusive
	}
	return in, verdict
}

func (f *fakeStandbyEvaluator) setVerdict(model string, verdict StandbyVerdict) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verdicts[model] = verdict
}

func (f *fakeStandbyEvaluator) callsSnapshot() []standbyEvalCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]standbyEvalCall, len(f.calls))
	copy(out, f.calls)
	return out
}

type fakeStandbyCovered struct {
	models []string
	err    error

	mu    sync.Mutex
	calls int
}

func (f *fakeStandbyCovered) ListCoveredModels(context.Context) ([]string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.models, nil
}

type fakeStandbyClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeStandbyClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeStandbyClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newStandbyTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newStandbyTestController(t *testing.T, eval StandbyEvaluator, covered StandbyCoveredModelSource, clock *fakeStandbyClock) (*StandbyController, *StandbyRegistry) {
	t.Helper()
	registry := NewStandbyRegistry()
	ctrl := NewStandbyController(eval, covered, registry, StandbyControllerOptions{
		Interval: time.Minute,
		Now:      clock.Now,
		Logger:   newStandbyTestLogger(),
	})
	return ctrl, registry
}

// degradedTTFTInput 返回一个 TTFT 明显越进入线的输入（基线与样本都足够）。
func degradedTTFTInput(model string) StandbyHealthInput {
	baseline := 10000.0
	current := 25000.0
	return StandbyHealthInput{
		Model:              model,
		TTFTBaselineP50Ms:  &baseline,
		TTFTBaselineDays:   StandbyBaselineMinDays + 2,
		TTFTCurrentP50Ms:   &current,
		TTFTCurrentSamples: StandbyTTFTMinSamples + 10,
	}
}

// healthyTTFTInput 返回一个远低于退出线的输入。
func healthyTTFTInput(model string) StandbyHealthInput {
	baseline := 10000.0
	current := 8000.0
	return StandbyHealthInput{
		Model:              model,
		TTFTBaselineP50Ms:  &baseline,
		TTFTBaselineDays:   StandbyBaselineMinDays + 2,
		TTFTCurrentP50Ms:   &current,
		TTFTCurrentSamples: StandbyTTFTMinSamples + 10,
	}
}

func TestStandbyController_DegradedOpensAfterConfirm(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 3, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	eval.setVerdict("deepseek-v4-pro", StandbyVerdictDegraded)
	eval.inputs["deepseek-v4-pro"] = degradedTTFTInput("deepseek-v4-pro")
	covered := &fakeStandbyCovered{models: []string{"deepseek-v4-pro"}}
	ctrl, registry := newStandbyTestController(t, eval, covered, clock)

	ctrl.RunOnce(context.Background())
	require.False(t, registry.IsOpen("deepseek-v4-pro"))

	clock.Advance(StandbyConfirmDuration - time.Second)
	ctrl.RunOnce(context.Background())
	require.False(t, registry.IsOpen("deepseek-v4-pro"), "未满 5 分钟不得开启")

	clock.Advance(time.Second)
	ctrl.RunOnce(context.Background())
	require.True(t, registry.IsOpen("deepseek-v4-pro"), "连续退化满 5 分钟应开启")

	status, ok := registry.Status("deepseek-v4-pro")
	require.True(t, ok)
	require.Equal(t, "deepseek", status.Platform)
	require.Equal(t, StandbyVerdictDegraded, status.Verdict)
	require.Equal(t, clock.Now(), status.OpenedAt)
	require.Equal(t, 25000.0, status.CurrentP50Ms)
	require.Equal(t, 10000.0, status.BaselineP50Ms)
	require.Equal(t, "ttft_p50_above_enter", status.Reason)
}

func TestStandbyController_HealthyClosesAfterConfirm(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	eval.setVerdict("kimi-k3", StandbyVerdictDegraded)
	eval.inputs["kimi-k3"] = degradedTTFTInput("kimi-k3")
	covered := &fakeStandbyCovered{models: []string{"kimi-k3"}}
	ctrl, registry := newStandbyTestController(t, eval, covered, clock)

	ctrl.RunOnce(context.Background())
	clock.Advance(StandbyConfirmDuration)
	ctrl.RunOnce(context.Background())
	require.True(t, registry.IsOpen("kimi-k3"))

	eval.setVerdict("kimi-k3", StandbyVerdictHealthy)
	eval.inputs["kimi-k3"] = healthyTTFTInput("kimi-k3")

	clock.Advance(time.Minute)
	ctrl.RunOnce(context.Background())

	clock.Advance(StandbyConfirmDuration - time.Second)
	ctrl.RunOnce(context.Background())
	require.True(t, registry.IsOpen("kimi-k3"), "恢复未满 5 分钟不得关闭")

	clock.Advance(time.Second)
	ctrl.RunOnce(context.Background())
	require.False(t, registry.IsOpen("kimi-k3"), "连续健康满 5 分钟应关闭")

	status, ok := registry.Status("kimi-k3")
	require.True(t, ok)
	require.Equal(t, StandbyVerdictHealthy, status.Verdict)
	require.Equal(t, "signals_recovered", status.Reason)
	require.True(t, status.OpenedAt.IsZero())
}

func TestStandbyController_VerdictChangeResetsTimer(t *testing.T) {
	start := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	clock := &fakeStandbyClock{now: start}
	eval := newFakeStandbyEvaluator()
	eval.setVerdict("glm-5.3", StandbyVerdictDegraded)
	eval.inputs["glm-5.3"] = degradedTTFTInput("glm-5.3")
	covered := &fakeStandbyCovered{models: []string{"glm-5.3"}}
	ctrl, registry := newStandbyTestController(t, eval, covered, clock)

	// t0：degraded，开始计时。
	ctrl.RunOnce(context.Background())

	// t0+3m：中途变 Inconclusive，计时重置。
	clock.Advance(3 * time.Minute)
	eval.setVerdict("glm-5.3", StandbyVerdictInconclusive)
	ctrl.RunOnce(context.Background())

	// t0+7m：重新 degraded，此时才重新计时。
	clock.Advance(4 * time.Minute)
	eval.setVerdict("glm-5.3", StandbyVerdictDegraded)
	ctrl.RunOnce(context.Background())

	// t0+11m：若按首次 degraded 计已 11 分钟，但按重置后仅 4 分钟 → 仍 closed。
	clock.Advance(4 * time.Minute)
	ctrl.RunOnce(context.Background())
	require.False(t, registry.IsOpen("glm-5.3"), "中途 verdict 变化必须重置计时")

	// t0+12m：重置后满 5 分钟 → open。
	clock.Advance(time.Minute)
	ctrl.RunOnce(context.Background())
	require.True(t, registry.IsOpen("glm-5.3"))
}

// 覆盖组合解析：模型名 → 平台；无法判定的模型与通配符键跳过。
func TestStandbyController_CoveredModelPlatformDerivation(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 6, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	covered := &fakeStandbyCovered{models: []string{
		"deepseek-v4-pro",
		"kimi-k3",
		"glm-5.3",
		" glm-5.3-flash ",
		"claude-sonnet-4-6",
		"kimi-*",
		"",
	}}
	ctrl, _ := newStandbyTestController(t, eval, covered, clock)

	ctrl.RunOnce(context.Background())

	want := []standbyEvalCall{
		{model: "deepseek-v4-pro", platform: "deepseek"},
		{model: "kimi-k3", platform: "kimi"},
		{model: "glm-5.3", platform: "zhipu"},
		{model: "glm-5.3-flash", platform: "zhipu"},
	}
	require.Equal(t, want, eval.callsSnapshot())

	for _, model := range []string{"claude-sonnet-4-6", "kimi-*", ""} {
		_, ok := ctrl.Registry().Status(model)
		require.Falsef(t, ok, "无法判定的模型不得进入注册表: %q", model)
	}
}

// 采集失败/超时（采集器判 Inconclusive）→ 不迁移。
func TestStandbyController_CollectFailureDoesNotMigrate(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 7, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	eval.setVerdict("deepseek-v4-pro", StandbyVerdictInconclusive)
	covered := &fakeStandbyCovered{models: []string{"deepseek-v4-pro"}}
	ctrl, registry := newStandbyTestController(t, eval, covered, clock)

	for i := 0; i < 20; i++ {
		ctrl.RunOnce(context.Background())
		clock.Advance(time.Minute)
	}

	require.False(t, registry.IsOpen("deepseek-v4-pro"))
	status, ok := registry.Status("deepseek-v4-pro")
	require.True(t, ok)
	require.Equal(t, StandbyVerdictInconclusive, status.Verdict)
	require.Equal(t, "no_signal_data", status.Reason)
}

// 覆盖清单查询失败 = 整轮作废：不调用采集器、不改任何状态。
func TestStandbyController_CoveredQueryFailureSkipsRound(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	eval.setVerdict("deepseek-v4-pro", StandbyVerdictDegraded)
	eval.inputs["deepseek-v4-pro"] = degradedTTFTInput("deepseek-v4-pro")
	covered := &fakeStandbyCovered{err: errors.New("db down")}
	ctrl, registry := newStandbyTestController(t, eval, covered, clock)

	ctrl.RunOnce(context.Background())
	clock.Advance(10 * time.Minute)
	ctrl.RunOnce(context.Background())

	require.Empty(t, eval.callsSnapshot())
	require.Empty(t, registry.Snapshot())
}

func TestStandbyController_StartStop(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	covered := &fakeStandbyCovered{models: []string{"kimi-k3"}}
	registry := NewStandbyRegistry()
	ctrl := NewStandbyController(eval, covered, registry, StandbyControllerOptions{
		Interval: 5 * time.Millisecond,
		Now:      clock.Now,
		Logger:   newStandbyTestLogger(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	ctrl.Start(ctx) // 重复启动无副作用

	deadline := time.Now().Add(2 * time.Second)
	for len(eval.callsSnapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.NotEmpty(t, eval.callsSnapshot())

	ctrl.Stop()
	ctrl.Stop() // 重复停止安全
}

// 第 3b 片：人工强制表。构造带强制表的控制器（日志可注入）。
func newStandbyForcedController(
	t *testing.T,
	eval StandbyEvaluator,
	covered StandbyCoveredModelSource,
	clock *fakeStandbyClock,
	forceOpen, forceClosed []string,
	logger *slog.Logger,
) (*StandbyController, *StandbyRegistry) {
	t.Helper()
	if logger == nil {
		logger = newStandbyTestLogger()
	}
	registry := NewStandbyRegistry()
	ctrl := NewStandbyController(eval, covered, registry, StandbyControllerOptions{
		Interval:          time.Minute,
		Now:               clock.Now,
		Logger:            logger,
		ForceOpenModels:   forceOpen,
		ForceClosedModels: forceClosed,
	})
	return ctrl, registry
}

// force_open 在健康信号下仍保持 open（健康信号在自动判定里永远不会开启）。
func TestStandbyController_ForceOpenIgnoresHealthySignals(t *testing.T) {
	start := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	clock := &fakeStandbyClock{now: start}
	eval := newFakeStandbyEvaluator()
	eval.setVerdict("deepseek-v4-pro", StandbyVerdictHealthy)
	eval.inputs["deepseek-v4-pro"] = healthyTTFTInput("deepseek-v4-pro")
	covered := &fakeStandbyCovered{models: []string{"deepseek-v4-pro"}}
	ctrl, registry := newStandbyForcedController(t, eval, covered, clock, []string{"deepseek-v4-pro"}, nil, nil)

	for i := 0; i < 10; i++ {
		ctrl.RunOnce(context.Background())
		clock.Advance(10 * time.Minute)
	}

	require.True(t, registry.IsOpen("deepseek-v4-pro"), "force_open 必须无视健康信号保持开启")
	status, ok := registry.Status("deepseek-v4-pro")
	require.True(t, ok)
	require.Equal(t, StandbyForceReasonOpen, status.Reason)
	require.Equal(t, "deepseek", status.Platform)
	require.Equal(t, start, status.OpenedAt, "强制开启时刻只在首次置位时记录")
	require.Empty(t, eval.callsSnapshot(), "命中强制表不得采集")
}

// force_closed 在退化信号下仍保持 closed。
func TestStandbyController_ForceClosedIgnoresDegradedSignals(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 11, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	eval.setVerdict("kimi-k3", StandbyVerdictDegraded)
	eval.inputs["kimi-k3"] = degradedTTFTInput("kimi-k3")
	covered := &fakeStandbyCovered{models: []string{"kimi-k3"}}
	ctrl, registry := newStandbyForcedController(t, eval, covered, clock, nil, []string{"kimi-k3"}, nil)

	for i := 0; i < 10; i++ {
		ctrl.RunOnce(context.Background())
		clock.Advance(10 * time.Minute)
	}

	require.False(t, registry.IsOpen("kimi-k3"), "force_closed 必须无视退化信号保持关闭")
	status, ok := registry.Status("kimi-k3")
	require.True(t, ok)
	require.Equal(t, StandbyForceReasonClosed, status.Reason)
	require.True(t, status.OpenedAt.IsZero())
	require.Empty(t, eval.callsSnapshot(), "命中强制表不得采集")
}

// 两张表同时命中同一模型：force_closed 优先。
func TestStandbyController_ForceClosedWinsOverForceOpen(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	covered := &fakeStandbyCovered{models: []string{"glm-5.3"}}
	ctrl, registry := newStandbyForcedController(t, eval, covered, clock,
		[]string{"glm-5.3"}, []string{"glm-5.3"}, nil)

	ctrl.RunOnce(context.Background())

	require.False(t, registry.IsOpen("glm-5.3"))
	status, ok := registry.Status("glm-5.3")
	require.True(t, ok)
	require.Equal(t, StandbyForceReasonClosed, status.Reason)
}

// 未命中强制表的模型仍走自动判定（回归护栏）。
func TestStandbyController_UnforcedModelStillAutoEvaluates(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	eval.setVerdict("kimi-k3", StandbyVerdictDegraded)
	eval.inputs["kimi-k3"] = degradedTTFTInput("kimi-k3")
	covered := &fakeStandbyCovered{models: []string{"kimi-k3", "deepseek-v4-pro"}}
	ctrl, registry := newStandbyForcedController(t, eval, covered, clock, []string{"deepseek-v4-pro"}, nil, nil)

	ctrl.RunOnce(context.Background())
	require.True(t, registry.IsOpen("deepseek-v4-pro"))
	require.False(t, registry.IsOpen("kimi-k3"), "自动判定仍需 5 分钟确认")

	clock.Advance(StandbyConfirmDuration)
	ctrl.RunOnce(context.Background())
	require.True(t, registry.IsOpen("kimi-k3"), "未命中强制表的模型必须继续自动开启")

	calls := eval.callsSnapshot()
	require.Len(t, calls, 2)
	for _, call := range calls {
		require.Equal(t, "kimi-k3", call.model, "只有未强制的模型才会被采集")
	}
}

// 强制表里的模型即使不在覆盖清单里也生效（运维可直接点名止血）。
func TestStandbyController_ForcedModelOutsideCoveredList(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 14, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	covered := &fakeStandbyCovered{models: []string{"kimi-k3"}}
	ctrl, registry := newStandbyForcedController(t, eval, covered, clock,
		[]string{"glm-5.3"}, []string{"deepseek-v4-pro"}, nil)

	ctrl.RunOnce(context.Background())

	require.True(t, registry.IsOpen("glm-5.3"))
	require.False(t, registry.IsOpen("deepseek-v4-pro"))
	status, ok := registry.Status("deepseek-v4-pro")
	require.True(t, ok)
	require.Equal(t, "deepseek", status.Platform)
	require.Equal(t, StandbyForceReasonClosed, status.Reason)
}

// 迁移日志只在状态变化时写：连续多轮强制态不应刷屏。
func TestStandbyController_ForceTransitionLoggedOnce(t *testing.T) {
	clock := &fakeStandbyClock{now: time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)}
	eval := newFakeStandbyEvaluator()
	covered := &fakeStandbyCovered{models: []string{"deepseek-v4-pro"}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctrl, _ := newStandbyForcedController(t, eval, covered, clock,
		[]string{"deepseek-v4-pro"}, nil, logger)

	for i := 0; i < 5; i++ {
		ctrl.RunOnce(context.Background())
		clock.Advance(time.Minute)
	}

	require.Equal(t, 1, strings.Count(buf.String(), "standby_transition"), "只有首次置位才写迁移日志")
}
