package service

// 文件：internal/service/upstream_heartbeat.go
//
// CF524 D2 流式心跳保活组件（方案 A，单独交付，不做挂点接入；R3 P2-5 闭合）。
// 本卡只新建本组件与对应 _test.go；挂点 1-4 的接入、owner 安装、防线例外判定
// 一律归 G2。组件契约严格遵循 docs/cf524-upstream-hang-mitigation-plan.md v8.2 §2 D2。
//
// 职责边界（与方案冲突处以报告说明，不自行改语义）：
//   - 组件负责"停得干净 + 优先级唯一"；三分支顺序编排在 G2。
//   - delay 取值来自请求入口配置快照 RequestBudgetSnapshot.HeartbeatDelaySeconds
//     （v7：热加载不影响进行中请求）；计时起点 = 快照 EntryMonotonic（v6：覆盖
//     认证/排队前置期）。本组件不读 cfg，由 G2 经快照传入。
//   - 单一 writer 所有权：Stop/Resume 均为 stop-and-wait，返回后调用方才可写
//     ResponseWriter；心跳与真实转发/failover/错误通道不得并发持写。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// heartbeatKeepAliveInterval 是心跳帧之间的间隔（方案 D2：15s）。
	heartbeatKeepAliveInterval = 15 * time.Second
	// heartbeatSSEFrame 是 SSE 注释保活帧（单帧）。
	heartbeatSSEFrame = ": keep-alive\n\n"
)

// HeartbeatHeaderResult 是 OnUpstreamHeaderArrived 的优先级裁决结果，供 G2 编排
// 三分支顺序（方案 D2 R4 P1-1 / 用户授权晚到契约）。
type HeartbeatHeaderResult int

const (
	// HeartbeatHeaderRejectedHeartbeatNotStarted 上游响应头先于心跳启动到达
	// （优先级①：响应事件胜出）——心跳不启动，G2 保留原始状态码语义。
	HeartbeatHeaderRejectedHeartbeatNotStarted HeartbeatHeaderResult = iota
	// HeartbeatHeaderAfterCommit 心跳已提交 SSE 200（tick 先到，优先级①：tick
	// 胜出）——G2 走晚到契约（2xx→语义流；4xx/5xx→SSE error 帧）。
	HeartbeatHeaderAfterCommit
	// HeartbeatHeaderGuardTerminal guard 终态已先胜出——忽略本次响应头事件。
	HeartbeatHeaderGuardTerminal
)

// 心跳状态机（唯一线性化优先级，原子状态）。
const (
	hbIdle      int32 = iota // 已构造/等待 delay，尚未启动
	hbStarted                // SSE 200 已提交，keep-alive 循环运行中
	hbHeaderWon              // 优先级①：上游响应头事件胜出，心跳不启动
	hbGuardWon               // 优先级②：guard 终态胜出，心跳不启动（tick 一律丢弃）
)

// UpstreamHeartbeat 是流式心跳保活组件：在 clientStream 且上游响应头等待超过
// heartbeat_delay 时，写 SSE 200 响应头 + 周期 : keep-alive 注释帧，上游响应头
// 到达即停（stop-and-wait）。
//
// 组件为请求级 owner（不随 attempt 重建）；首轮 guard 超时后 G2 调用 Resume 即可
// 在下一 attempt 即时续写首帧（不重等 delay，交接空窗 < delay）。
type UpstreamHeartbeat struct {
	w       http.ResponseWriter
	flusher http.Flusher
	// flusherOK 记录 ResponseWriter 是否具备 Flush 能力；不具备时 Start/Resume
	// 失败关闭（拒绝启用心跳、上抛错误，不静默降级为无心跳）。
	flusherOK bool

	ctx context.Context

	// 不可变（请求级 owner）
	// initialDelay 是自 Start 起到首帧 SSE 200 的等待 = max(0, delay - (now-entry))，
	// 覆盖认证/排队前置期（v6/v7）。
	initialDelay time.Duration
	// keepAliveInterval 默认 15s；单测在同包内可临时调小以加速时间轴断言。
	keepAliveInterval time.Duration

	// 状态机（原子，保证唯一线性化优先级）
	state          atomic.Int32
	guardTerminal  atomic.Bool  // 优先级②：guard 终态后 tick 一律丢弃
	headerArrived  atomic.Bool  // 上游响应头后到（心跳已提交）：后续 tick 丢弃
	started        atomic.Bool  // SSE 200 已提交 ⇒ streamStarted（v6）
	committedBytes atomic.Int64 // 心跳已提交到 writer 的累计字节（供 G2 防线例外）

	// goroutine 控制
	launched atomic.Bool
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex // 保护 writer（单所有者 stop-and-wait）
}

// NewUpstreamHeartbeat 构造心跳组件。
//
// ctx 为请求 context，客户端断开（ctx cancel）时心跳 goroutine 干净退出。
// w 必须支持 http.Flusher（gin.ResponseWriter 满足），否则 Start/Resume 失败关闭。
// snap 取 RequestBudgetSnapshot：HeartbeatDelaySeconds 为 delay，EntryMonotonic
// 为计时起点（v6/v7）；snap 为 nil 时退化为 delay=0（G2 不应传入 nil）。
// now 为当前单调时间（通常 time.Now()），用于按入口起点折算 delay 剩余。
func NewUpstreamHeartbeat(ctx context.Context, w http.ResponseWriter, snap *RequestBudgetSnapshot, now time.Time) *UpstreamHeartbeat {
	h := &UpstreamHeartbeat{
		w:                 w,
		ctx:               ctx,
		keepAliveInterval: heartbeatKeepAliveInterval,
		stopCh:            make(chan struct{}),
		doneCh:            make(chan struct{}),
	}
	if ctx == nil {
		h.ctx = context.Background()
	}
	if w != nil {
		if f, ok := w.(http.Flusher); ok {
			h.flusher = f
			h.flusherOK = true
		}
	}
	if snap == nil {
		snap = &RequestBudgetSnapshot{}
	}
	delay := time.Duration(snap.HeartbeatDelaySeconds) * time.Second
	elapsed := time.Duration(0)
	if !snap.EntryMonotonic.IsZero() && !now.IsZero() {
		if e := now.Sub(snap.EntryMonotonic); e > 0 {
			elapsed = e
		}
	}
	h.initialDelay = delay - elapsed
	if h.initialDelay < 0 {
		h.initialDelay = 0
	}
	return h
}

// Start 启动心跳：delay 到点写 SSE 200 响应头 + 15s 间隔 keep-alive 帧。
//
// 检测不到 http.Flusher 能力时失败关闭——返回错误、零写出、拒绝启用心跳
// （不静默降级为无心跳）。幂等：已启动则直接返回 nil。
func (h *UpstreamHeartbeat) Start() error {
	if !h.flusherOK {
		return fmt.Errorf("upstream heartbeat: ResponseWriter lacks http.Flusher capability; refusing to enable heartbeat")
	}
	if h.launched.Load() {
		return nil
	}
	h.launched.Store(true)
	go h.run()
	return nil
}

// run 是心跳主 goroutine：等待 initialDelay 后尝试启动（CAS 抢唯一胜者），成功则
// 进入 keep-alive 循环；stopCh/ctx 取消即干净退出。
func (h *UpstreamHeartbeat) run() {
	defer close(h.doneCh)
	if h.initialDelay <= 0 {
		if !h.tryStart() {
			return
		}
		h.keepAliveLoop()
		return
	}
	timer := time.NewTimer(h.initialDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		if !h.tryStart() {
			return
		}
		h.keepAliveLoop()
	case <-h.stopCh:
		timer.Stop()
	case <-h.ctx.Done():
		timer.Stop()
	}
}

// tryStart 以单一原子 CAS 竞争"启动"唯一胜者（优先级①/②的守门）：仅 hbIdle→
// hbStarted 成功才写 SSE 200。若响应事件/guard 终态已占住状态，CAS 失败、零写出。
func (h *UpstreamHeartbeat) tryStart() bool {
	if !h.state.CompareAndSwap(hbIdle, hbStarted) {
		return false
	}
	h.writeSSEHeader()
	h.started.Store(true)
	return true
}

// writeSSEHeader 写 SSE 200 响应头并显式 Flush（Flush 契约硬性：不 Flush 则 CF
// 收不到帧、心跳失效）。在 mu 下执行，保证单一 writer 所有权。
func (h *UpstreamHeartbeat) writeSSEHeader() {
	h.mu.Lock()
	defer h.mu.Unlock()
	before := h.sizeNow()
	h.w.Header().Set("Content-Type", "text/event-stream")
	h.w.Header().Set("Cache-Control", "no-cache")
	h.w.Header().Set("Connection", "keep-alive")
	h.w.Header().Set("X-Accel-Buffering", "no")
	h.w.WriteHeader(http.StatusOK)
	after := h.sizeNow()
	h.committedBytes.Add(after - before)
	h.flusher.Flush()
}

// keepAliveLoop 是 keep-alive 循环本体：周期写 keep-alive 注释帧；guard 终态/
// 响应头后到/stop/ctx 取消时退出。本函数不关闭 doneCh（由 run 或 Resume 派发的
// goroutine 负责），以避免重复 close。
func (h *UpstreamHeartbeat) keepAliveLoop() {
	ticker := time.NewTicker(h.keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if h.shouldDropTick() {
				continue
			}
			h.writeFrame()
		case <-h.stopCh:
			return
		case <-h.ctx.Done():
			return
		}
	}
}

// shouldDropTick 是优先级②/响应后到的落闸：guard 终态或响应头已后到后，tick 一律
// 丢弃、零写出。
func (h *UpstreamHeartbeat) shouldDropTick() bool {
	return h.guardTerminal.Load() || h.headerArrived.Load()
}

// writeFrame 写一帧 : keep-alive 注释帧并显式 Flush（Flush 契约硬性）。在 mu 下
// 执行，保证单一 writer 所有权。committedBytes 以 gin.Size() 增量记账，与 G2 防御
// 线 c.Writer.Size() 口径自洽（gin.Size() 仅计 body 字节，不含响应头）。
func (h *UpstreamHeartbeat) writeFrame() {
	h.mu.Lock()
	defer h.mu.Unlock()
	before := h.sizeNow()
	n, err := io.WriteString(h.w, heartbeatSSEFrame)
	if err != nil {
		return
	}
	after := h.sizeNow()
	h.committedBytes.Add(after - before)
	_ = n
	h.flusher.Flush()
}

// sizeNow 返回当前 writer 已写 body 字节数（gin.ResponseWriter.Size()）。非 gin
// writer 时返回 0（该路径仅用于单测构造，生产必为 gin writer）。
//
// G5d 哨兵归一：gin noWritten 哨兵为 -1，首次 Write/WriteString/Flush 触发
// WriteHeaderNow() 把 size 从 -1 翻转为 0。noWritten 翻转发生在 Flush/首写的
// WriteHeaderNow 内、在调用方 before/after 快照之外，若原样透传 -1 会把 +1 记入
// CommittedBytes（如无 header 先行首帧 before=-1、after=N → delta=N+1），与 handler
// 侧归一口径错位。统一按 <0 → 0 归一，保证 CommittedBytes 恒为纯帧字节、与 handler
// 归一口径对称。
func (h *UpstreamHeartbeat) sizeNow() int64 {
	if gw, ok := h.w.(gin.ResponseWriter); ok {
		if size := gw.Size(); size > 0 {
			return int64(size)
		}
		return 0
	}
	return 0
}

// OnUpstreamHeaderArrived 记录上游响应头事件，解析优先级①。返回值告知 G2 走哪条
// 分支（组件只负责"优先级唯一"，顺序编排在 G2）：
//   - HeartbeatHeaderRejectedHeartbeatNotStarted：响应先到，心跳不启动，G2 保留
//     原始状态码语义（临界 4xx/5xx 仍透传原始状态码）；
//   - HeartbeatHeaderAfterCommit：心跳已提交（tick 先到），G2 走晚到契约
//     （2xx→语义流；4xx/5xx→SSE error 帧）；组件停心跳循环让出 writer；
//   - HeartbeatHeaderGuardTerminal：guard 终态已先胜出，忽略本次响应头事件。
func (h *UpstreamHeartbeat) OnUpstreamHeaderArrived() HeartbeatHeaderResult {
	// 优先级①：上游响应头事件 vs 心跳启动。Idle 时 CAS 占住即"响应事件胜出"。
	if h.state.CompareAndSwap(hbIdle, hbHeaderWon) {
		h.signalStop()
		return HeartbeatHeaderRejectedHeartbeatNotStarted
	}
	if h.state.Load() == hbStarted {
		// tick 先到，心跳已提交：标记响应后到（后续 tick 丢弃）+ 停循环让出 writer。
		h.headerArrived.Store(true)
		h.signalStop()
		return HeartbeatHeaderAfterCommit
	}
	// hbHeaderWon（已拒）/ hbGuardWon（guard 已胜）→ 忽略。
	return HeartbeatHeaderGuardTerminal
}

// OnGuardDecided 记录 guard 终态（换号/耗尽），解析优先级②：终态后 tick 一律丢弃、
// 零写出。若仍在等待启动则占住状态防止 tick 写出；若已启动则落闸并停循环。
func (h *UpstreamHeartbeat) OnGuardDecided() {
	h.guardTerminal.Store(true)
	h.state.CompareAndSwap(hbIdle, hbGuardWon)
	h.signalStop()
}

// Stop 是 stop-and-wait：停 tick、等心跳 goroutine 退出、返回后调用方才可写
// ResponseWriter（单一 writer 所有权）。未 Start 时安全 no-op。
func (h *UpstreamHeartbeat) Stop() {
	if !h.launched.Load() {
		return
	}
	h.signalStop()
	<-h.doneCh
}

// Resume 在换号交接时调用：stop-and-wait 当前轮后，下一 attempt 即时恢复心跳并
// 立刻 Flush 一帧（不重等 delay，交接空窗 < delay，防 ~125s 撞墙）。新 attempt 的
// guard 未终态，故重置 per-attempt 状态（guardTerminal/headerArrived 回 false）。
// 已提交 SSE 200 不重写（避免破坏已建立流），只续写 keep-alive 帧。
func (h *UpstreamHeartbeat) Resume() error {
	if !h.flusherOK {
		return fmt.Errorf("upstream heartbeat: cannot resume without http.Flusher capability")
	}
	h.Stop() // stop-and-wait 当前轮（等 goroutine 退出）
	// 重置 per-attempt 状态（新 attempt 的 guard 尚未终态）
	h.guardTerminal.Store(false)
	h.headerArrived.Store(false)
	h.state.Store(hbIdle)
	// 重建控制通道（旧通道已被 Stop 关闭且 goroutine 已退出）
	h.stopCh = make(chan struct{})
	h.doneCh = make(chan struct{})
	h.stopOnce = sync.Once{}
	h.launched.Store(true)
	if h.started.Load() {
		// 已提交 SSE 200：立即 Flush 一帧（不重等 delay），随后续跑 keep-alive 循环。
		// 派发独立 goroutine 并负责关闭 doneCh（keepAliveLoop 本身不关闭）。
		h.writeFrame()
		if h.shouldDropTick() {
			return nil
		}
		go func() {
			defer close(h.doneCh)
			h.keepAliveLoop()
		}()
	} else {
		// 异常形态（从未提交）：重新开始完整流程（run 负责关闭 doneCh）
		go h.run()
	}
	return nil
}

// IsCommitted 心跳已提交 SSE 200 ⇒ streamStarted（v6）。供 G2 在 failover/耗尽/
// 错误分支一律按 streamStarted 处理，防止把非流式 JSON/原始状态码写进已提交的
// SSE 200 造成格式损坏。
func (h *UpstreamHeartbeat) IsCommitted() bool { return h.started.Load() }

// IsStarted 同 IsCommitted（心跳 goroutine 已写出 SSE 200）。
func (h *UpstreamHeartbeat) IsStarted() bool { return h.started.Load() }

// CommittedBytes 心跳已提交到 writer 的累计字节数（仅 body 字节，不含响应头，
// 与 gin.Size() 口径一致）。供 G2 防线例外：heartbeatBaseSize = writerSizeBeforeForward
// + hb.CommittedBytes()，判定 Size() > max(before, heartbeatBaseSize) 才视为语义写出。
func (h *UpstreamHeartbeat) CommittedBytes() int64 { return h.committedBytes.Load() }

// signalStop 幂等关闭 stopCh（多次调用安全）。
func (h *UpstreamHeartbeat) signalStop() {
	h.stopOnce.Do(func() { close(h.stopCh) })
}
