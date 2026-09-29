package service

import (
	"context"
	"sync/atomic"
	"time"
)

// upstreamFirstByteGuardDecided 是护栏 watchdog 的唯一胜出裁决状态。
// 响应头到达（headerArrived）与窗口耗尽（fire）通过同一个原子状态竞争唯一
// 胜者——先到者赢，后到者 CAS 失效，不存在"先检查再 CAS"的窗口。
const (
	// upstreamFirstByteGuardUndecided 尚未裁决。
	upstreamFirstByteGuardUndecided int32 = 0
	// upstreamFirstByteGuardSettled 非超时终态：响应头到达或显式释放。零取消。
	upstreamFirstByteGuardSettled int32 = 1
	// upstreamFirstByteGuardTimedOut 超时终态：窗口耗尽且未先被其他方裁决。
	upstreamFirstByteGuardTimedOut int32 = 2
)

// upstreamFirstByteGuard 是平台无关的首字节护栏（watchdog，非 context deadline）。
//
// 实现契约（CF524 D1 / 方案 v8.2 R6 P1-2 闭合）：
//   - 护栏 = 可取消 watchdog：attempt 开始时起定时器（窗口 = min(guard, 剩余预算)）；
//   - 响应头在窗口内到达 → 停 watchdog，护栏不产生任何取消（零取消）；
//   - 窗口耗尽 → watchdog 取消上游请求 context 并产生 failover 错误；
//   - 护栏 context 任何情况下不进入 body 读取路径：body 读取继承父 context
//     （客户端断开仍即时取消），由 BodyContext() 提供。
//
// 响应头 vs guard 超时线性化（R6 P1-3 闭合）：两事件纳入同一状态机，以不可变
// 截止时间线性化，通过单一原子状态 decided 竞争唯一胜者。
type upstreamFirstByteGuard struct {
	parentCtx context.Context
	timer     *time.Timer
	cancel    context.CancelFunc
	// decided 是唯一胜者裁决状态（upstreamFirstByteGuard* 常量）。
	decided atomic.Int32
	// cancelled 记录护栏是否取消了上游请求 context（仅超时 fire 路径置位，
	// 响应头到达路径永不置位）。
	cancelled atomic.Bool
}

// newUpstreamFirstByteGuard 建立首字节护栏，返回护栏与其持有的上游请求 context。
//
// parentCtx 是客户端/父请求 context（body 读取继承它）。返回的 reqCtx 用于发出
// 上游请求并等待响应头；窗口耗尽时 reqCtx 被取消以中断 header 等待。body 读取
// 必须使用 guard.BodyContext()（== parentCtx），绝不传入 reqCtx。
//
// window<=0 视为非法最小值（不允许禁用/零值路径，与配置 [30,90] 失败关闭一致），
// 退化为极小窗口——由调用方保证传入合法窗口。
func newUpstreamFirstByteGuard(parentCtx context.Context, window time.Duration) (*upstreamFirstByteGuard, context.Context) {
	if window <= 0 {
		window = time.Nanosecond
	}
	base := parentCtx
	if base == nil {
		base = context.Background()
	}
	reqCtx, cancel := context.WithCancel(base)
	g := &upstreamFirstByteGuard{
		parentCtx: base,
		cancel:    cancel,
	}
	g.timer = time.AfterFunc(window, g.fire)
	return g, reqCtx
}

// HeaderArrived 在响应头到达时调用：停 watchdog 且零取消（不取消 reqCtx，不置
// 超时标志）。与 fire 竞争唯一胜者——先到者赢，后到者 CAS 失败返回 false。
func (g *upstreamFirstByteGuard) HeaderArrived() bool {
	if !g.decided.CompareAndSwap(upstreamFirstByteGuardUndecided, upstreamFirstByteGuardSettled) {
		return false
	}
	if g.timer != nil {
		g.timer.Stop()
	}
	return true
}

// BodyContext 返回 body 读取应使用 context：父 context，永不返回护栏的可取消
// 请求 context（护栏 context 不进 body 读取路径）。
func (g *upstreamFirstByteGuard) BodyContext() context.Context {
	if g.parentCtx != nil {
		return g.parentCtx
	}
	return context.Background()
}

// fire 由定时器触发：以纯 CAS 竞争超时终态，唯一胜者才取消上游请求 context。
func (g *upstreamFirstByteGuard) fire() {
	if !g.decided.CompareAndSwap(upstreamFirstByteGuardUndecided, upstreamFirstByteGuardTimedOut) {
		return
	}
	g.cancelled.Store(true)
	if g.cancel != nil {
		g.cancel()
	}
}

// TimedOut 报告窗口是否已耗尽（唯一胜者裁决：decided == timedOut）。
func (g *upstreamFirstByteGuard) TimedOut() bool {
	return g != nil && g.decided.Load() == upstreamFirstByteGuardTimedOut
}

// Settled 报告响应头是否已到达（唯一胜者裁决：decided == settled）。
func (g *upstreamFirstByteGuard) Settled() bool {
	return g != nil && g.decided.Load() == upstreamFirstByteGuardSettled
}

// Cancelled 报告护栏是否取消了上游请求 context（仅超时 fire 置位，响应头到达
// 路径为 false）。测试据此断言"头先到→护栏未取消上游 context"。
func (g *upstreamFirstByteGuard) Cancelled() bool {
	return g != nil && g.cancelled.Load()
}

// Stop 显式终止护栏生命周期（Do 返回错误等不消费 header 的路径）。等价于
// HeaderArrived 的零取消收尾：停定时器、标记 settled、不取消 context。
func (g *upstreamFirstByteGuard) Stop() bool {
	return g.HeaderArrived()
}

// —— 请求级预算快照类型与 helper（CF524 D1，v8.2）——
//
// 不可变截止时间 + 配置快照（R4 P1-3 + R6 P2-4 闭合）：请求入口一次捕获
// {guard 值, heartbeat delay 值, 绝对截止时间, 入口单调起点} 为不可变快照传入
// 所有 attempt 与心跳 owner。每轮尝试仅从快照截止时间取剩余窗口，禁止循环内
// 重建完整 guard 窗口；换号不重置。创建入口在 G2 的 handler 层安装点，本卡只
// 交付类型、纯函数与 helper。

// RequestBudgetSnapshot 是请求级不可变预算快照。
type RequestBudgetSnapshot struct {
	// GuardSeconds 是护栏窗口秒数（cfg.Gateway.UpstreamFirstByteGuardSeconds）。
	GuardSeconds int
	// HeartbeatDelaySeconds 是流式心跳延迟秒数
	// （cfg.Gateway.UpstreamHeartbeatDelaySeconds；0=禁用）。
	HeartbeatDelaySeconds int
	// AbsoluteDeadline 是请求级绝对截止时间（入口一次计算，热加载不可变）。
	AbsoluteDeadline time.Time
	// EntryMonotonic 是请求入口单调起点（time.Now()，携带单调读数）。
	EntryMonotonic time.Time
}

// NewRequestBudgetSnapshot 从配置值与入口时间构建不可变快照。clientStream 在
// 入口已知，用于一次性确定总预算与绝对截止。纯函数，不安装到 context（安装归 G2）。
func NewRequestBudgetSnapshot(guardSeconds, heartbeatDelaySeconds int, entry time.Time, clientStream bool) *RequestBudgetSnapshot {
	if entry.IsZero() {
		entry = time.Now()
	}
	s := &RequestBudgetSnapshot{
		GuardSeconds:          guardSeconds,
		HeartbeatDelaySeconds: heartbeatDelaySeconds,
		EntryMonotonic:        entry,
	}
	s.AbsoluteDeadline = entry.Add(s.TotalBudget(clientStream))
	return s
}

// TotalBudget 返回请求级总预算纯函数（R6 P2-6 / R5 P1-3）：
//   - 有心跳流式（clientStream && heartbeat>0）= 2×guard + 10s 写回余量；
//   - 其余（非流式，或 heartbeat=0 的流式——禁用心跳无墙保护，预算墙内收敛）
//     = guard + 25s。
func (s RequestBudgetSnapshot) TotalBudget(clientStream bool) time.Duration {
	guard := time.Duration(s.GuardSeconds) * time.Second
	if clientStream && s.HeartbeatDelaySeconds > 0 {
		return 2*guard + 10*time.Second
	}
	return guard + 25*time.Second
}

// RemainingBudget 返回不可变时间轴上从 now 到 AbsoluteDeadline 的剩余预算（≥0）。
func (s RequestBudgetSnapshot) RemainingBudget(now time.Time) time.Duration {
	rem := s.AbsoluteDeadline.Sub(now)
	if rem < 0 {
		return 0
	}
	return rem
}

// AttemptWindow 返回每轮 attempt 的护栏窗口 = min(guard, 剩余预算)。
// 换号不重置：每次都从不可变 AbsoluteDeadline 取剩余，禁止重建完整 guard 窗口。
func (s RequestBudgetSnapshot) AttemptWindow(now time.Time) time.Duration {
	guard := time.Duration(s.GuardSeconds) * time.Second
	rem := s.RemainingBudget(now)
	if guard < rem {
		return guard
	}
	return rem
}

type requestBudgetSnapshotKey struct{}

// WithRequestBudgetSnapshot 将预算快照写入 context（G2 安装点调用）。
func WithRequestBudgetSnapshot(ctx context.Context, s *RequestBudgetSnapshot) context.Context {
	return context.WithValue(ctx, requestBudgetSnapshotKey{}, s)
}

// RequestBudgetSnapshotFromContext 从 context 读回预算快照。无快照时返回
// (nil, false)——按方案定义不兜底（调用方据此走既有无护栏旧路径）。
func RequestBudgetSnapshotFromContext(ctx context.Context) (*RequestBudgetSnapshot, bool) {
	if ctx == nil {
		return nil, false
	}
	s, ok := ctx.Value(requestBudgetSnapshotKey{}).(*RequestBudgetSnapshot)
	if !ok || s == nil {
		return nil, false
	}
	return s, true
}
