package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// openAICNFirstByteTimeout 是显式 CN 清单平台（kimi/deepseek/zhipu/minimax，
// 与派发单 1 的 isCNFamilyFastpathAccount 同一清单）出站请求的首包超时阈值。
// 用户裁定 1：60 秒（数据调研：60s 误杀 4%，30s 误杀 27%）。
//
// 计时边界（方案文档 P2）：起点 = 出站请求发出；终点 = 上游响应 body 首字节。
// 仅收到响应头不停止计时；首包到达后流式传输不受影响（不设整体读超时）。
const openAICNFirstByteTimeout = 60 * time.Second

// errOpenAICNFirstByteTimeout 标识网关内部的 CN 首包超时。它刻意与
// context.Canceled / context.DeadlineExceeded 区分：内部超时不得表现为
// 客户端取消语义（派发单 4 禁区：客户端取消不得触发冷却/换号，内部超时
// 必须触发冷却+换号）。消费链通过 isOpenAICNFirstByteTimeout 识别。
var errOpenAICNFirstByteTimeout = errors.New("cn upstream first byte timeout")

// openAICNFirstByteTimeoutFailoverBody 是超时触发 failover 后的客户端可见错误体。
var openAICNFirstByteTimeoutFailoverBody = []byte(`{"error":{"type":"upstream_error","message":"upstream first byte timeout"}}`)

// isOpenAICNFirstByteTimeout 报告 err 是否为网关内部 CN 首包超时（可穿过包装）。
func isOpenAICNFirstByteTimeout(err error) bool {
	return err != nil && errors.Is(err, errOpenAICNFirstByteTimeout)
}

// openAICNFirstByteWatchdog 裁决状态。客户端取消、首字节到达、内部超时三者
// 通过同一个原子状态 decided 竞争唯一胜者——先到者赢，后到者 CAS 失效。
const (
	// watchdogStateUndecided 是尚未裁决。
	watchdogStateUndecided int32 = 0
	// watchdogStateSettled 是非超时终态：首字节到达、显式释放或客户端取消。
	// 客户端取消是非超时终态，绝不置内部超时标志（绝不冷却/换号）。
	watchdogStateSettled int32 = 1
	// watchdogStateTimedOut 是内部超时终态：定时器到期且未先被其他方裁决。
	watchdogStateTimedOut int32 = 2
)

// openAICNFirstByteWatchdog 为单个出站尝试提供首包超时。
//
// 实现：独立 context 层（context.WithCancel + time.AfterFunc 定时器），到期取消
// 当前上游尝试。取消经由 request context 传播到 net/http Transport，从而中断
// 响应头等待与 body 读取；body 包装层把超时取消带来的读中断转译为
// errOpenAICNFirstByteTimeout（区别于客户端取消的 context.Canceled）。
//
// 竞态裁决（闸②终审整改）：客户端取消、首字节到达、内部超时三者以单一原子
// 状态 decided 裁决唯一胜者——先到者赢，后到者失效：
//   - firstByte/release 以 CAS(0→settled) 赢；赢家 disarm 定时器；
//   - watchParent（父 context 取消）以 CAS(0→settled) 赢，并取消当前上游尝试；
//   - expire（定时器到期）以 CAS(0→timedOut) 赢，并取消当前上游尝试。
// 三条路径都是纯 CAS，无任何"先检查再 CAS"的窗口（闸②终审整改项 3：旧实现
// 的 parentDone() 检查与 CAS(0,2) 之间存在窗口，父取消会被误判为内部超时）。
//   - 判定内部超时 = decided==timedOut（Fired）；其余终态均非超时。
type openAICNFirstByteWatchdog struct {
	parentCtx context.Context
	timer     *time.Timer
	cancel    context.CancelFunc
	// decided 是唯一胜者裁决状态（watchdogState* 常量）。
	decided atomic.Int32
	// firstByteSeen 独立记录"上游响应 body 首个字节已到达"，供流式消费者在
	// 首字节前抑制 keepalive 心跳（闸②终审整改项 1）。与 decided 正交：
	// release 也会置 settled，但不代表首字节已到达。
	firstByteSeen atomic.Bool
	// done 在裁决产生唯一胜者后关闭，释放 watchParent goroutine。
	done     chan struct{}
	doneOnce sync.Once
}

// newOpenAICNFirstByteWatchdog 建立 watchdog 并返回其持有的 request context。
//
// deriveCtx 是出站 request 自身的 context（携带上游 transport profile：
// HTTPUpstreamProfileOpenAI 等连接池/协议/代理值），派生的 wctx 从它继承，
// 从而 request.WithContext 不会丢失 profile。
//
// clientCtx 是独立的客户端取消信号，只参与取消裁决，不整体装到 request 上
// （闸②终审整改项 2：旧实现从 clientCtx 派生 wctx，装到 request 上会覆盖并
// 丢弃 request.Context() 里的 transport profile）。
//
// timeout<=0 时使用 openAICNFirstByteTimeout。
func newOpenAICNFirstByteWatchdog(deriveCtx, clientCtx context.Context, timeout time.Duration) (*openAICNFirstByteWatchdog, context.Context) {
	if timeout <= 0 {
		timeout = openAICNFirstByteTimeout
	}
	base := deriveCtx
	if base == nil {
		base = context.Background()
	}
	if clientCtx == nil {
		clientCtx = base
	}
	// request.WithContext 会覆盖整个 ctx：从 request 自身 context 派生即可保留
	// 构建器写入的 HTTPUpstreamProfile。
	wctx, cancel := context.WithCancel(base)
	w := &openAICNFirstByteWatchdog{parentCtx: clientCtx, cancel: cancel, done: make(chan struct{})}
	w.timer = time.AfterFunc(timeout, w.expire)
	go w.watchParent()
	return w, wctx
}

// settle 在裁决为非超时终态时收尾：停定时器、释放 watchParent、首次调用时
// 返回 true。CAS 失败表示已有其他胜者，返回 false。
func (w *openAICNFirstByteWatchdog) settle() bool {
	if !w.decided.CompareAndSwap(watchdogStateUndecided, watchdogStateSettled) {
		return false
	}
	w.stop()
	return true
}

// fire 在裁决为内部超时终态时收尾：停定时器、释放 watchParent，并取消当前
// 上游尝试。CAS 失败表示已有其他胜者（首字节/释放/客户端取消），返回 false。
func (w *openAICNFirstByteWatchdog) fire() bool {
	if !w.decided.CompareAndSwap(watchdogStateUndecided, watchdogStateTimedOut) {
		return false
	}
	w.stop()
	if w.cancel != nil {
		w.cancel()
	}
	return true
}

// stop 停定时器并关闭 done（只关一次），解除 watchParent 的挂起。
func (w *openAICNFirstByteWatchdog) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.doneOnce.Do(func() {
		if w.done != nil {
			close(w.done)
		}
	})
}

// parentDone 报告父 context 是否已取消（客户端取消已发生）。
func (w *openAICNFirstByteWatchdog) parentDone() bool {
	return w != nil && w.parentCtx != nil && w.parentCtx.Err() != nil
}

// watchParent 等待客户端取消并以纯 CAS 竞争非超时终态。客户端断开时不得因
// 随后的定时器触发被误判为内部超时（否则会错误冷却+换号）。CAS 失败表示
// 首字节/释放/内部超时已先裁决，此时不覆盖既有裁决。
//
// 这里与 expire() 是同一条原子竞争路径：不再有"先检查 parentDone 再 CAS"的
// 时序窗口——取消本身即一个非超时终态候选（闸②终审整改项 3）。
func (w *openAICNFirstByteWatchdog) watchParent() {
	if w == nil || w.parentCtx == nil {
		return
	}
	select {
	case <-w.parentCtx.Done():
		// 客户端取消赢得非超时终态：取消上游尝试以尽快释放资源，但绝不置
		// 内部超时标志（不冷却不换号），且 body 读错误保持 context.Canceled。
		if w.settle() && w.cancel != nil {
			w.cancel()
		}
	case <-w.done:
	}
}

// expire 由定时器触发：以纯 CAS 竞争超时终态，唯一胜者才取消当前上游尝试。
// 客户端取消若先（或同时）经 watchParent 裁决，本 CAS 失败，误判窗口消失。
func (w *openAICNFirstByteWatchdog) expire() {
	w.fire()
}

// Fired 报告内部超时是否已触发（唯一胜者裁决：decided==timedOut）。
func (w *openAICNFirstByteWatchdog) Fired() bool {
	return w != nil && w.decided.Load() == watchdogStateTimedOut
}

// FirstByteSeen 报告上游响应 body 的首个字节是否已到达。与 decided 正交：
// 首字节到达后流式传输不再受超时约束（心跳可恢复既有节奏）。
func (w *openAICNFirstByteWatchdog) FirstByteSeen() bool {
	return w != nil && w.firstByteSeen.Load()
}

// firstByte 在响应 body 首个字节到达时调用，disarm 定时器。
// 与 expire/watchParent 竞争唯一胜者：首字节先到即赢，定时器随后失效。
func (w *openAICNFirstByteWatchdog) firstByte() {
	if w.settle() {
		w.firstByteSeen.Store(true)
	}
}

// release 终止定时器生命周期（Do 返回错误等不消费 body 的路径）。
func (w *openAICNFirstByteWatchdog) release() {
	w.settle()
}

// openAICNFirstByteTimeoutBody 包装上游响应 body：首字节到达 disarm 定时器；
// 定时器先到期时把底层读中断转译为 errOpenAICNFirstByteTimeout。
type openAICNFirstByteTimeoutBody struct {
	io.ReadCloser
	w *openAICNFirstByteWatchdog
}

// FirstByteSeen 报告上游响应 body 的首个字节是否已到达（转发给 watchdog）。
// 流式消费者在首字节前据此抑制 keepalive 心跳，避免提前提交响应头破坏
// 60s 首包超时窗口内的透明换号（闸②终审整改项 1）。
func (b *openAICNFirstByteTimeoutBody) FirstByteSeen() bool {
	return b != nil && b.w != nil && b.w.FirstByteSeen()
}

// cnFirstByteTimeoutBodyCarrier 由包装了（可能含 watchdog body 的）内层 body
// 的包装层实现，使 watchdog 标记可穿透任意层包装被识别。
type cnFirstByteTimeoutBodyCarrier interface {
	cnFirstByteTimeoutBody() *openAICNFirstByteTimeoutBody
}

// cnFirstByteTimeoutBodyOf 返回 resp.Body 若其被首包超时 watchdog 包装
// （CN 清单平台出站路径），否则返回 nil。非 CN 路径返回 nil，心跳行为不变。
//
// 载体接口优先 + 直接断言兜底：Forward 消费链会在 watchdog 包装外再套
// openAIRequestContextReadCloser / responsesClientToolStreamBody，直接类型断言
// 会失效；实现载体接口的包装层可先把内层（可能含 watchdog body 的）body 交给
// 本函数识别，使标记穿透任意层包装仍可达（闸②终审整改项 2）。
func cnFirstByteTimeoutBodyOf(body io.ReadCloser) *openAICNFirstByteTimeoutBody {
	if c, ok := body.(cnFirstByteTimeoutBodyCarrier); ok {
		if wb := c.cnFirstByteTimeoutBody(); wb != nil {
			return wb
		}
	}
	wb, _ := body.(*openAICNFirstByteTimeoutBody)
	return wb
}

// cnFirstByteTimeoutBody 实现载体接口：直接返回自身，使外层包装可穿透获得
// watchdog 标记（被 openAIRequestContextReadCloser 等包住时）。
func (b *openAICNFirstByteTimeoutBody) cnFirstByteTimeoutBody() *openAICNFirstByteTimeoutBody {
	return b
}

func (b *openAICNFirstByteTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		// 首字节已到达：即使定时器与首包在边界竞态（同刻触发），也以首包
		// 为准（"首包到达后不误杀"），并 disarm 定时器。
		b.w.firstByte()
		return n, err
	}
	if err != nil && b.w.Fired() {
		// 内部超时已触发（底层 Read 因 watchdog 取消当前上游尝试而中断）：
		// 转译为网关内部超时错误，消费链据此触发冷却+换号。此分支在
		// release/Close 之前先行返回，超时错误转换语义不受释放路径影响。
		// 客户端取消的读中断（decided==settled）不会落入此分支，保持
		// context.Canceled 原契约（闸②终审整改项 3）。
		return 0, errOpenAICNFirstByteTimeout
	}
	if err != nil {
		// 非超时终态（含无数据 EOF、客户端取消的 context.Canceled）：disarm
		// 定时器，防止 timer/context 在流结束后仍挂满 60s。Fired 已在上方
		// 先行拦截，不会误伤超时语义。客户端取消不得被转译为内部超时。
		b.w.release()
	}
	return 0, err
}

// Close 显式终止定时器生命周期：提前关闭（未消费完 body）即释放，防
// timer/context 挂 60s。真正超时（decided==2）后的 Close 对 release 为
// no-op，超时错误转换语义不变。
func (b *openAICNFirstByteTimeoutBody) Close() error {
	b.w.release()
	return b.ReadCloser.Close()
}

// openAICNFirstByteTimeoutFor 返回服务实例的 CN 首包超时阈值（测试可注入短值）。
func (s *OpenAIGatewayService) openAICNFirstByteTimeoutFor() time.Duration {
	if s == nil || s.cnFirstByteTimeout <= 0 {
		return openAICNFirstByteTimeout
	}
	return s.cnFirstByteTimeout
}

// failoverOpenAICNFirstByteTimeout 把一次网关内部 CN 首包超时映射为既有 failover
// 循环可消费的错误，并调用派发单 1 已开放的冷却入口（handleOpenAIAccountUpstreamError
// 链）。返回的 UpstreamFailoverError.NextAccountAction = NextAccountRetry。
//
// upstreamModel 必填：真实消费路径的 canonical/upstream model 必须贯穿进冷却链，
// 空模型键会被 model 瞬态状态拒绝（openAIAccountModelTransientKey 对空 model
// 返回 !ok），导致冷却假闭环——超时账号不会被该 model 的调度门排除（闸②整改）。
//
// 调用点负责"响应头已下发后不做透明切换"判定：仅在响应头尚未提交给客户端时
// 由调用方使用返回的 failover 错误；已提交时按既有流中断语义处理（调用方忽略
// failover 错误继续既有流中断路径）。
func (s *OpenAIGatewayService) failoverOpenAICNFirstByteTimeout(ctx context.Context, account *Account, upstreamModel string) *UpstreamFailoverError {
	if s != nil && account != nil {
		model := strings.TrimSpace(upstreamModel)
		// 冷却入口（派发单 1 已对 CN 家族开放）：504 落入
		// shouldCooldownOpenAITransientUpstreamError 集合，触发既有瞬态冷却。
		s.handleOpenAIAccountUpstreamError(ctx, account, http.StatusGatewayTimeout, http.Header{}, openAICNFirstByteTimeoutFailoverBody, model)
	}
	return &UpstreamFailoverError{
		StatusCode:       http.StatusGatewayTimeout,
		ResponseBody:     openAICNFirstByteTimeoutFailoverBody,
		ResponseHeaders:  http.Header{},
		NextAccountAction: NextAccountRetry,
	}
}

// openAICNFirstByteTimeoutStreamFailover 处理流式读取错误中的 CN 首包超时。
// 返回非 nil 时调用方直接返回该 failover 错误；返回 nil 表示不属超时（或
// 响应头已提交、不做透明切换），调用方继续既有流中断语义。
//
// upstreamModel 必填，贯穿进冷却链（见 failoverOpenAICNFirstByteTimeout）。
//
// "响应头已下发后不做透明切换"判定基于 clientOutputStarted（首个 SSE 事件写出
// 前 writeStreamHeaders 才提交响应头）。超时发生且响应头未提交 → 冷却+换号；
// 响应头已提交 → 返回 nil，调用方按既有流中断处理。
func (s *OpenAIGatewayService) openAICNFirstByteTimeoutStreamFailover(c *gin.Context, account *Account, clientOutputStarted bool, err error, upstreamModel string) *UpstreamFailoverError {
	if !isOpenAICNFirstByteTimeout(err) {
		return nil
	}
	if clientOutputStarted {
		return nil
	}
	return s.failoverOpenAICNFirstByteTimeout(context.Background(), account, upstreamModel)
}

// abortCCNativeStreamAfterHeaders 处理 native anthropic 流式消费链中响应头已提交
// 后才到的 CN 首包超时：向客户端下发可识别的流内错误事件终止流（CC 格式），并
// 返回非 failover 的流中断错误——绝不落回正常 finalize 冒充成功空流。
//
// 调用前提：err 已由调用方确认为 isOpenAICNFirstByteTimeout 且响应头已提交
// （clientOutputStarted 由调用方判定，此处不再重复）。
func (s *OpenAIGatewayService) abortCCNativeStreamAfterHeaders(
	c *gin.Context,
	resultWithUsage func() *OpenAIForwardResult,
	err error,
) (*OpenAIForwardResult, error) {
	if c != nil && c.Writer != nil {
		if _, werr := fmt.Fprint(c.Writer, buildChatStreamErrorSSE("upstream_error", "upstream first byte timeout after stream started")); werr == nil {
			if _, werr := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); werr == nil {
				if fl, ok := c.Writer.(http.Flusher); ok {
					fl.Flush()
				}
			}
		}
	}
	return resultWithUsage(), fmt.Errorf("stream read error after headers committed: %w", err)
}

// abortResponsesNativeStreamAfterHeaders 处理 native anthropic 流式消费链中响应头
// 已提交后才到的 CN 首包超时：以 response.failed 终止事件回传（Responses 格式的
// 流内错误信封），并返回非 failover 的流中断错误——绝不落回正常 finalize 冒充成功。
func (s *OpenAIGatewayService) abortResponsesNativeStreamAfterHeaders(
	c *gin.Context,
	resultWithUsage func() *OpenAIForwardResult,
	err error,
) (*OpenAIForwardResult, error) {
	if c != nil && c.Writer != nil {
		sse := buildOpenAIResponseFailedSSE("", "", []byte("{}"), "upstream first byte timeout after stream started")
		if _, werr := fmt.Fprint(c.Writer, sse); werr == nil {
			if fl, ok := c.Writer.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}
	return resultWithUsage(), fmt.Errorf("stream read error after headers committed: %w", err)
}
