package service

// 文件：internal/service/cf524_observation.go
//
// CF524 G5 可观测事件与关联契约（方案 v8.2 §4「验收」第 8 条 / 派发单 G5 v2）。
//
// 职责边界：
//   - 只承载"事件发射 + 请求级关联状态"，不改任何护栏/心跳/换号语义；
//   - 事件全部由四个非 OpenAI 挂点（gateway_forward.go / gateway_anthropic_passthrough.go /
//     gateway_forward_as_responses.go / gateway_forward_as_chat_completions.go）在共享执行器
//     `cf524ExecuteUpstreamWithGuard` 及其调用方处发射；OpenAI 原生路径零触碰；
//   - `upstream_headers_received_ms` 判别器仅经既有请求级完成日志（http request completed）
//     落一个字段，禁止新建事件流（v8.1 收敛）。
//
// 事件 schema（固化）：
//   - gateway_first_byte_guard_triggered：path/platform/stream/attempt/outcome/elapsed_ms/request_id
//     （+ phase=attempt|terminal 区分"attempt 事件"与"终态关联事件"，同 request_id）；
//   - gateway_upstream_heartbeat_started：path/platform/heartbeat_delay_ms/request_id。
//
// outcome ∈ {failover_success, failover_exhausted, client_gone, subsequent_upstream_error}。

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

// CF524 guard 终态枚举（写入点见 cf524ObserveGuardTriggered / cf524ResolveGuardOutcome）。
const (
	// guardOutcomeFailoverSuccess：guard 成功交棒既有 failover 消费链；下一 attempt 正常收敛
	// （attempt 事件的 guard 时判定 = 交棒成功；终态事件的最终判定 = 请求最终成功）。
	guardOutcomeFailoverSuccess = "failover_success"
	// guardOutcomeFailoverExhausted：剩余预算 < 最小可行窗口，既有 FailoverExhausted 终结。
	guardOutcomeFailoverExhausted = "failover_exhausted"
	// guardOutcomeClientGone：客户端已断开，换号静默终止。
	guardOutcomeClientGone = "client_gone"
	// guardOutcomeSubsequentUpstreamError：guard 后下一 attempt 以非 failover 类错误终结
	// （写入点 = 下一 attempt 非 failover 错误处理处）。
	guardOutcomeSubsequentUpstreamError = "subsequent_upstream_error"
)

// guard 事件 phase：区分 attempt 事件与终态关联事件（同 request_id）。
const (
	guardEventPhaseAttempt  = "attempt"
	guardEventPhaseTerminal = "terminal"
)

// gin.Context 关联键（请求级，跨 handler failover 迭代共享；键名带 cf524 前缀防止冲突）。
const (
	cf524GuardTrackerKey    = "cf524_guard_observation_tracker"
	cf524HeadersReceivedKey = "cf524_upstream_headers_received_at"
)

// cf524GuardAttempt 是一次 guard 触发的不可变事件载荷（attempt 事件与终态事件复用）。
type cf524GuardAttempt struct {
	path      string
	platform  string
	stream    bool
	attempt   int
	elapsedMs int64
	requestID string
}

// cf524GuardTracker 是请求级 guard 观测状态：记录尚未解析终态的 guard 触发，
// 并保证心跳事件每请求只发一次。
type cf524GuardTracker struct {
	mu       sync.Mutex
	pending  []cf524GuardAttempt
	hbLogged bool
}

func cf524TrackerIfAny(c *gin.Context) *cf524GuardTracker {
	if c == nil {
		return nil
	}
	if v, ok := c.Get(cf524GuardTrackerKey); ok {
		if tr, ok := v.(*cf524GuardTracker); ok {
			return tr
		}
	}
	return nil
}

func cf524Tracker(c *gin.Context) *cf524GuardTracker {
	if tr := cf524TrackerIfAny(c); tr != nil {
		return tr
	}
	tr := &cf524GuardTracker{}
	c.Set(cf524GuardTrackerKey, tr)
	return tr
}

func (t *cf524GuardTracker) hasPending() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending) > 0
}

// resolve 为所有待解析的 guard 触发发射终态关联事件并清空 pending。
//
// 同 request_id 的每条 attempt 事件都对应一条终态关联事件：一次 resolve 会为
// pending 中每个未解析的 guard 触发各发一条终态事件（全部同一 outcome，因为整条
// 换号链在同一请求内共享同一最终结果）。
func (t *cf524GuardTracker) resolve(outcome string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	pending := t.pending
	t.pending = nil
	t.mu.Unlock()
	for _, ev := range pending {
		cf524EmitGuardEvent(ev, outcome, guardEventPhaseTerminal)
	}
}

// —— 事件载荷构造 ——

func cf524RequestID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	requestID, _ := c.Request.Context().Value(ctxkey.RequestID).(string)
	return requestID
}

func cf524Path(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return ""
	}
	return c.Request.URL.Path
}

// —— 事件发射（slog，结构化；经 logger.Init 设置的默认 handler 落到统一日志管线）——

func cf524EmitGuardEvent(ev cf524GuardAttempt, outcome, phase string) {
	slog.Info("gateway_first_byte_guard_triggered",
		slog.String("path", ev.path),
		slog.String("platform", ev.platform),
		slog.Bool("stream", ev.stream),
		slog.Int("attempt", ev.attempt),
		slog.String("outcome", outcome),
		slog.Int64("elapsed_ms", ev.elapsedMs),
		slog.String("request_id", ev.requestID),
		slog.String("phase", phase),
	)
}

func cf524EmitHeartbeatStarted(path, platform string, delaySeconds int, requestID string) {
	slog.Info("gateway_upstream_heartbeat_started",
		slog.String("path", path),
		slog.String("platform", platform),
		slog.Int("heartbeat_delay_ms", delaySeconds*1000),
		slog.String("request_id", requestID),
	)
}

// —— 挂点调用入口 ——

// cf524GuardObservation 是四个非 OpenAI 挂点传给共享执行器
// `cf524ExecuteUpstreamWithGuard` 的观测上下文（单点埋点载体）。nil 表示不埋点
// （无埋点需求的调用方/测试可传 nil），全部方法对 nil 安全。
//
// c/ctx 均取自挂点函数参数：ctx 必须是**未被护栏包裹**的请求级原始 context——
// 护栏超时会 cancel 自己的 reqCtx，若用 reqCtx 判 ctx.Err() 会把每次护栏超时
// 误判为 client_gone。
type cf524GuardObservation struct {
	c         *gin.Context
	ctx       context.Context
	platform  string
	stream    bool
	attempt   int
	startedAt time.Time
	snapshot  *RequestBudgetSnapshot
}

// observeGuardTriggered 在护栏窗口耗尽时发 attempt 事件（及必要的即时终态）。
func (o *cf524GuardObservation) observeGuardTriggered() {
	if o == nil || o.c == nil {
		return
	}
	remaining := time.Duration(0)
	if o.snapshot != nil {
		remaining = o.snapshot.RemainingBudget(time.Now())
	}
	var elapsedMs int64
	if !o.startedAt.IsZero() {
		elapsedMs = time.Since(o.startedAt).Milliseconds()
	}
	cf524ObserveGuardTriggered(o.c, o.ctx, o.platform, o.stream, o.attempt, elapsedMs, remaining)
}

// noteHeartbeatIfCommitted 在心跳已提交（SSE 200 已写出）时发 started 事件（幂等）。
func (o *cf524GuardObservation) noteHeartbeatIfCommitted(hb *UpstreamHeartbeat) {
	if o == nil || o.c == nil || hb == nil || !hb.IsCommitted() {
		return
	}
	cf524NoteHeartbeatStarted(o.c, o.platform, cf524HeartbeatDelaySeconds(o.snapshot, o.snapshot != nil))
}

// markUpstreamHeadersReceived 记录上游响应头到达时刻（判别器字段来源，first-wins）。
func (o *cf524GuardObservation) markUpstreamHeadersReceived() {
	if o == nil || o.c == nil {
		return
	}
	MarkUpstreamHeadersReceived(o.c, time.Now())
}

// cf524ObserveGuardTriggered 在护栏触发时由挂点调用（attempt 事件，单点语义在共享执行器
// 及其调用方）。outcome 的 guard 时判定：
//   - 客户端 ctx 已取消 → client_gone（终态，立即发终态关联事件）；
//   - 剩余预算 < 最小可行窗口 → failover_exhausted（终态，立即发终态关联事件）；
//   - 其余 → failover_success（guard 成功交棒既有 failover 消费链；终态延后到下一 attempt
//     结果确定时由 cf524ResolveGuardOutcome 关联）。
func cf524ObserveGuardTriggered(c *gin.Context, ctx context.Context, platform string, stream bool, attempt int, elapsedMs int64, remaining time.Duration) {
	if c == nil {
		return
	}
	ev := cf524GuardAttempt{
		path:      cf524Path(c),
		platform:  platform,
		stream:    stream,
		attempt:   attempt,
		elapsedMs: elapsedMs,
		requestID: cf524RequestID(c),
	}
	outcome := guardOutcomeFailoverSuccess
	terminal := false
	switch {
	case ctx != nil && ctx.Err() != nil:
		outcome = guardOutcomeClientGone
		terminal = true
	case remaining < upstreamFirstByteMinViableWindow:
		outcome = guardOutcomeFailoverExhausted
		terminal = true
	}
	cf524EmitGuardEvent(ev, outcome, guardEventPhaseAttempt)
	if terminal {
		cf524EmitGuardEvent(ev, outcome, guardEventPhaseTerminal)
		// 本次 guard 触发以终态收场 ⇒ 整条换号链就此终结：此前 attempt 遗留的
		// pending 共享同一终态（避免关联事件缺失）。
		if tr := cf524TrackerIfAny(c); tr != nil {
			tr.resolve(outcome)
		}
		return
	}
	tr := cf524Tracker(c)
	tr.mu.Lock()
	tr.pending = append(tr.pending, ev)
	tr.mu.Unlock()
}

// cf524ResolveGuardOutcome 在挂点返回时由 defer 调用：把此前 guard 触发的终态关联事件
// 按本次 attempt 的结果补发（同 request_id）。
//   - ctx 已取消 → client_gone；
//   - err == nil（请求最终成功）→ failover_success；
//   - err 为可继续换号的 *UpstreamFailoverError（ShouldRetryNextAccount=true）→ 保持
//     pending，交后续 attempt 解析；
//   - err 为已耗尽换号的 *UpstreamFailoverError（ShouldRetryNextAccount=false，如护栏
//     剩余预算 <5s 或上游终结性错误）→ failover_exhausted（终态，必须落一条关联事件）；
//   - 其余非 failover 错误 → subsequent_upstream_error。
func cf524ResolveGuardOutcome(c *gin.Context, ctx context.Context, err error) {
	tr := cf524TrackerIfAny(c)
	if tr == nil || !tr.hasPending() {
		return
	}
	if ctx != nil && ctx.Err() != nil {
		tr.resolve(guardOutcomeClientGone)
		return
	}
	if err == nil {
		tr.resolve(guardOutcomeFailoverSuccess)
		return
	}
	var fe *UpstreamFailoverError
	if errors.As(err, &fe) {
		if !fe.ShouldRetryNextAccount() {
			tr.resolve(guardOutcomeFailoverExhausted)
		}
		return
	}
	tr.resolve(guardOutcomeSubsequentUpstreamError)
}

// cf524NoteHeartbeatStarted 在挂点观察到心跳已提交（SSE 200 已写出）时调用，每请求只发一次。
func cf524NoteHeartbeatStarted(c *gin.Context, platform string, delaySeconds int) {
	if c == nil {
		return
	}
	tr := cf524Tracker(c)
	tr.mu.Lock()
	if tr.hbLogged {
		tr.mu.Unlock()
		return
	}
	tr.hbLogged = true
	tr.mu.Unlock()
	cf524EmitHeartbeatStarted(cf524Path(c), platform, delaySeconds, cf524RequestID(c))
}

// cf524HeartbeatDelaySeconds 从预算快照取心跳延迟秒数（无快照时 0）。
func cf524HeartbeatDelaySeconds(snapshot *RequestBudgetSnapshot, hasSnapshot bool) int {
	if !hasSnapshot || snapshot == nil {
		return 0
	}
	return snapshot.HeartbeatDelaySeconds
}

// MarkUpstreamHeadersReceived 由挂点在上游响应头到达时调用，记录首个响应头时刻
// （first-wins）。时间戳经既有请求级完成日志新增字段 upstream_headers_received_ms 输出，
// 不新建事件流。
func MarkUpstreamHeadersReceived(c *gin.Context, at time.Time) {
	if c == nil || at.IsZero() {
		return
	}
	if _, ok := UpstreamHeadersReceivedAt(c); ok {
		return
	}
	c.Set(cf524HeadersReceivedKey, at)
}

// UpstreamHeadersReceivedAt 读回首个上游响应头时刻；未收到时返回 (zero, false)。
func UpstreamHeadersReceivedAt(c *gin.Context) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	v, ok := c.Get(cf524HeadersReceivedKey)
	if !ok {
		return time.Time{}, false
	}
	at, ok := v.(time.Time)
	if !ok || at.IsZero() {
		return time.Time{}, false
	}
	return at, true
}

// ResolvePendingGuardObservations 在请求出口（既有请求级完成/访问日志处）由
// 中间件调用：把整条换号链结束时仍未解析的 guard 触发补发终态关联事件（同
// request_id）。语义：
//   - 客户端已断开（请求 ctx 已取消）→ client_gone；
//   - 其余（账号链路无候选而既有 FailoverExhausted 终结）→ failover_exhausted。
//
// 只读既有 tracker（cf524TrackerIfAny），不为无埋点请求创建任何状态：OpenAI 原生
// 路径不经过四个非 OpenAI 挂点、不创建 tracker，本函数对其实为 no-op，零新事件。
// 不新建事件流——终态事件仍是既有 gateway_first_byte_guard_triggered。
func ResolvePendingGuardObservations(c *gin.Context) {
	tr := cf524TrackerIfAny(c)
	if tr == nil || !tr.hasPending() {
		return
	}
	if c != nil && c.Request != nil && c.Request.Context().Err() != nil {
		tr.resolve(guardOutcomeClientGone)
		return
	}
	tr.resolve(guardOutcomeFailoverExhausted)
}

// UpstreamHeadersReceivedMs 以请求入口单调起点为基准把首个上游响应头时刻折算成
// 相对毫秒；供既有请求级完成/访问日志一行字段输出。未收到头时返回 (0, false)。
func UpstreamHeadersReceivedMs(c *gin.Context, entry time.Time) (int64, bool) {
	at, ok := UpstreamHeadersReceivedAt(c)
	if !ok {
		return 0, false
	}
	if entry.IsZero() {
		return at.UnixMilli(), true
	}
	if delta := at.Sub(entry); delta >= 0 {
		return delta.Milliseconds(), true
	}
	return 0, true
}
