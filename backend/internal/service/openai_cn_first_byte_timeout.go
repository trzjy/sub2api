package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// openAICNFirstByteTimeout 是显式 CN 清单平台（kimi/deepseek/zhipu/minimax，
// 与派发单 1 的 isCNFamilyFastpathAccount 同一清单）出站请求的整体首包超时阈值。
//
// 用户裁定 2（2026-10-26）：60 秒 -> 300 秒，「大请求本来就慢，就让用户慢慢等」。
// 原 60 秒的调研口径是全体请求（"60s 误杀 4%，30s 误杀 27%"），未单列大请求。
// 生产实测（2026-10-26 24h，usage_logs，9 个 deepseek/kimi 上游账号）：
// 1,164 个请求耗时超过 60 秒但成功（60-90s 780 条、90-120s 209 条、
// 120-180s 104 条、>180s 71 条），最长一条 576 秒；这些请求平均 input
// 2.6w~3.4w tokens、平均 body 39w~69w 字节 —— 即"本来就慢的大请求"被误杀。
// 300 秒与网关 HTTP 层 defaultResponseHeaderTimeout（300s）同级，
// 覆盖 180 秒内的全部误杀。
//
// 计时边界（方案文档 P2）：起点 = 出站请求发出；终点 = 上游响应 body 首字节。
// 仅收到响应头不停止计时；首包到达后流式传输不受影响（不设整体读超时）。
//
// 注意：本阈值与 HTTP 层 ResponseHeaderTimeout 不是同一层 ——
// OpenAI 兼容链路的 HTTP 层响应头超时为 0（不设限，http_upstream.go
// HTTPUpstreamProfileOpenAI 分支），本常量管的是「响应头已到、正文首字节未到」
// 这一段，与 HTTP 层无重叠。
const openAICNFirstByteTimeout = 300 * time.Second

// openAICNHeaderTimeout 是"等待上游响应头"这一段的独立阈值（30 秒）。
//
// 事故背景：zhipu 账号（id=132）被网关内部错误 cn upstream first byte timeout
// 反复打成临时停调。近 12h 全部 172 条该类错误的 upstream_status_code 与
// network_error_type 皆为空——上游连响应头都没有回来，请求石沉大海，而不是
// "上游在慢慢算"（同期成功请求 TTFT p50 2.84s / p95 8.4s；在途请求中位 0、
// 最大 3，远未到 concurrency=20；超时请求体仅 8~24KB）。强指纹是"账号空闲约
// 195s 后的第一个请求"（对照组请求间隔中位 14s，5 个账号一致）。
//
// 结论方向（待坐实）：请求被写进了一条本地仍认为 ESTABLISHED、但对端已静默
// 丢弃的半开连接，要等满 60s watchdog 才失败。因此把等待切成两段：
//   - 未收到响应头：按本常量快速失败——该场景等满 60s 也几乎不会有响应头
//     回来，短阈值只为尽早拿到一次"用新连接重试"的机会；
//   - 已收到响应头：仍按 openAICNFirstByteTimeout（60s 整体绝对截止）等
//     body 首字节，语义与切分前逐字一致，不缩短真实排队/生成窗口。
//
// 阈值 20s → 30s（2026-10-15）：20s 是按"响应头早于 body 到达"估的，将来开启
// 分段时按 30s 留余量。本常量当前**不生效**——分段整段由
// openAICNHeaderFastFailEnabled 关闭（见该常量注释里的实测证据）。
const openAICNHeaderTimeout = 30 * time.Second

// openAICNHeaderFastFailEnabled 是"无响应头快速失败 + 同账号新连接重试"这一分段
// 逻辑的开关**默认值：关闭**（止血，2026-10-15）。
//
// 关闭时（当前生产取值，与分段改造之前逐字一致）：
//   - 不注册 headerTimer：单次出站只有 openAICNFirstByteTimeout（60s）一个整体
//     截止计时器；
//   - TimedOutWaitingHeaders 恒为 false ⇒ 不发起 httpUpstreamFreshConn 新连接
//     重试，超时直接上抛 errOpenAICNFirstByteTimeout；
//   - 不产生任何额外延迟、额外出站与额外错误语义（无重试日志）；
//   - httptrace 采集的 connReused / connIdleTime 与日志字段 reused / idle_time
//     **照旧输出**——它们是坐实"空闲连接被静默丢弃"假设的唯一数据来源，与开关
//     是否打开无关。
//
// 为什么默认关闭（实测证据，改动以此为准）：分段设计假设"响应头先到、body 后到"，
// 于是"短阈值内无响应头"可区分死连接与慢请求。但绕过网关直连 open.bigmodel.cn
// 的 anthropic 端点（stream=true）实测：
//
//	small:           status=200 headers=0.83s first_body=0.83s
//	BIG(~24k token): status=200 headers=1.97s first_body=1.97s
//
// 即该端点 prefill/思考完成后才返回响应头，响应头与 body 首字节几乎同时到达，
// "无响应头"无法区分「半开/死连接」与「大请求 prefill 慢」。生产近 24h
// usage_logs 流式成功请求 TTFT：zhipu 2639 条中 233 条 >20s（8.8%，最大 388s）、
// kimi 3124 条中 484 条（15.5%）、deepseek 3611 条中 525 条（14.5%）。这些请求在
// 60s 阈值下是成功的，20s 分段会把已完成 prefill 的请求误杀并重放一遍。
//
// 打开时机：等 httptrace 的 reused / idle_time 在生产超时样本上坐实"失败集中在
// reused=true 且空闲较久的连接"后再开，且建议同时打开
// openAICNHeaderFastFailRequireReusedConn。
const openAICNHeaderFastFailEnabled = false

// openAICNHeaderFastFailRequireReusedConn 是快速失败的**可选附加门禁**（默认关闭，
// 且仅在 openAICNHeaderFastFailEnabled 打开时才有意义）。
//
// 打开后：只有 httptrace 显示本次出站确实复用了空闲连接（reused=true）时才允许
// "无响应头"快速失败与新连接重试；新建连接（reused=false）上的等待一律交给
// openAICNFirstByteTimeout 整体截止。用途是把快速失败收敛到"空闲连接被上游静默
// 丢弃"这一唯一有证据的假设上，避免误伤大请求 prefill（见
// openAICNHeaderFastFailEnabled 注释里的实测）。
const openAICNHeaderFastFailRequireReusedConn = false

// openAICNHeaderFastFail / openAICNHeaderFastFailRequireReused 是上面两个具名常量
// 的运行期镜像：初值即常量值（生产取值 = 常量，默认关闭）。只有测试（或将来接入
// gateway.* 配置项）会显式置位；生产路径不写这两个变量。
var (
	openAICNHeaderFastFail              atomic.Bool
	openAICNHeaderFastFailRequireReused atomic.Bool
)

func init() {
	openAICNHeaderFastFail.Store(openAICNHeaderFastFailEnabled)
	openAICNHeaderFastFailRequireReused.Store(openAICNHeaderFastFailRequireReusedConn)
}

// openAICNHeaderFastFailOn 报告分段快速失败总开关是否打开。
func openAICNHeaderFastFailOn() bool {
	return openAICNHeaderFastFail.Load()
}

// headerFastFailAllowed 报告本次"无响应头"快速失败与新连接重试是否被允许：
// 总开关必须打开；且打开 openAICNHeaderFastFailRequireReusedConn 门禁时，还要求
// 本次出站确实复用了空闲连接（httptrace GotConn 的 reused=true）。
func (w *openAICNFirstByteWatchdog) headerFastFailAllowed() bool {
	if !openAICNHeaderFastFailOn() {
		return false
	}
	if openAICNHeaderFastFailRequireReused.Load() {
		return w != nil && w.connReused.Load()
	}
	return true
}

// openAICNFreshConnRetryLimit 是"未收到响应头"快速失败后，对同一账号用**新建
// TCP 连接**重试的上限（每次请求最多 1 次；不是换账号、不是换分组）。
// 超过此次数即按既有超时语义上抛，杜绝无限重试与探测风暴。
const openAICNFreshConnRetryLimit = 1

// errOpenAICNFirstByteTimeout 标识网关内部的 CN 首包超时。它刻意与
// context.Canceled / context.DeadlineExceeded 区分：内部超时不得表现为
// 客户端取消语义（派发单 4 禁区：客户端取消不得触发冷却/换号，内部超时
// 必须触发冷却+换号）。消费链通过 isOpenAICNFirstByteTimeout 识别。
var errOpenAICNFirstByteTimeout = errors.New("cn upstream first byte timeout")

// httpUpstreamFreshConn 是 HTTPUpstream 的**可选**能力：强制本次出站使用新建的
// TCP 连接，不复用连接池中的空闲连接。
//
// 为何不能只发 Connection: close：Go 的 http.Transport 在取连接时（queueForIdleConn）
// 只看 DisableKeepAlives / IdleConnTimeout / isBroken，请求头上的 Connection: close
// 只决定"响应读完之后是否关闭"，并不阻止本次复用池里已有的空闲连接。要让重试真正
// 落在一条新连接上，只能用一个 DisableKeepAlives 的一次性 Transport。
//
// 未实现本接口的 HTTPUpstream（含测试桩）不参与同账号新连接重试，走既有超时语义。
type httpUpstreamFreshConn interface {
	DoFreshConn(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error)
}

// openAICNFirstByteTimeoutFailoverBody 是超时触发 failover 后的客户端可见错误体。
var openAICNFirstByteTimeoutFailoverBody = []byte(`{"error":{"type":"upstream_error","message":"` + infraerrors.UpstreamFirstByteTimeout + `"}`)

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
//
// 三条路径都是纯 CAS，无任何"先检查再 CAS"的窗口（闸②终审整改项 3：旧实现
// 的 parentDone() 检查与 CAS(0,2) 之间存在窗口，父取消会被误判为内部超时）。
//   - 判定内部超时 = decided==timedOut（Fired）；其余终态均非超时。
//
// 两段计时（本次整改）：headerTimer 管"等待响应头"段（openAICNHeaderTimeout），
// timer 管整体绝对截止（openAICNFirstByteTimeout）。响应头到达即 stop headerTimer，
// 之后只剩整体截止——与整改前单一 60s 计时器的 body 阶段语义完全一致。两个计时器
// 共用同一 defeat fire()/settle() 的唯一胜者裁决，且不可 Reset（避免重挂竞态）。
type openAICNFirstByteWatchdog struct {
	parentCtx context.Context
	// timer 是整体首包超时绝对截止计时器（起点 = 出站请求发出）。
	timer *time.Timer
	// headerTimer 是"等待响应头"段计时器；响应头到达后即停，不再裁决。
	headerTimer *time.Timer
	cancel      context.CancelFunc
	// decided 是唯一胜者裁决状态（watchdogState* 常量）。
	decided atomic.Int32
	// headerReceived 记录当前尝试是否已收到上游响应头。true 表示已进入
	// "等待 body 首字节"段，此后再触发的超时属首包超时而非无头超时。
	headerReceived atomic.Bool
	// firstByteSeen 独立记录"上游响应 body 首个字节已到达"，供流式消费者在
	// 首字节前抑制 keepalive 心跳（闸②终审整改项 1）。与 decided 正交：
	// release 也会置 settled，但不代表首字节已到达。
	firstByteSeen atomic.Bool
	// connReused / connIdleTime 由 httptrace.GotConn 采集本次尝试实际用到的
	// 连接：是否复用、复用前空闲多久。用于坐实"空闲连接被上游静默丢弃"假设。
	connReused   atomic.Bool
	connIdleTime atomic.Int64
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
// headerTimeout<=0 时取 min(openAICNHeaderTimeout, timeout)：注入短整体阈值的
// 场景（测试/特殊配置）自动退化为"单段"，行为与整改前完全一致。
//
// 分段 headerTimer 只在 openAICNHeaderFastFailEnabled 打开时注册；关闭（当前默认）
// 时本次出站只有整体截止一个计时器，单段语义。
// 注意：httptrace 观测不受开关影响，任何情况下都挂。
//
// 返回的 context 已挂 httptrace.ClientTrace（GotConn 采集连接复用信息）。
func newOpenAICNFirstByteWatchdog(deriveCtx, clientCtx context.Context, timeout time.Duration, headerTimeout ...time.Duration) (*openAICNFirstByteWatchdog, context.Context) {
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
	hp := openAICNHeaderTimeout
	if len(headerTimeout) > 0 && headerTimeout[0] > 0 {
		hp = headerTimeout[0]
	}
	if timeout < hp {
		hp = timeout
	}
	// request.WithContext 会覆盖整个 ctx：从 request 自身 context 派生即可保留
	// 构建器写入的 HTTPUpstreamProfile。
	wctx, cancel := context.WithCancel(base)
	w := &openAICNFirstByteWatchdog{parentCtx: clientCtx, cancel: cancel, done: make(chan struct{})}
	wctx = httptrace.WithClientTrace(wctx, w.gotConnTrace())
	w.timer = time.AfterFunc(timeout, w.expire)
	if openAICNHeaderFastFailOn() {
		w.headerTimer = time.AfterFunc(hp, w.expireHeader)
	}
	go w.watchParent()
	return w, wctx
}

// gotConnTrace 返回采集连接复用状态的 ClientTrace。只有 GotConn 被设置，
// 其余钩子保持 nil，net/http 不会产生额外系统调用。
func (w *openAICNFirstByteWatchdog) gotConnTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			w.connReused.Store(info.Reused)
			w.connIdleTime.Store(int64(info.IdleTime))
		},
	}
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

// stop 停两个计时器并关闭 done（只关一次），解除 watchParent 的挂起。
func (w *openAICNFirstByteWatchdog) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
	if w.headerTimer != nil {
		w.headerTimer.Stop()
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

// expire 由整体截止计时器触发：以纯 CAS 竞争超时终态，唯一胜者才取消当前上游尝试。
// 客户端取消若先（或同时）经 watchParent 裁决，本 CAS 失败，误判窗口消失。
func (w *openAICNFirstByteWatchdog) expire() {
	w.fire()
}

// expireHeader 由"等待响应头"段计时器触发（该计时器仅在快速失败开关打开时注册）。
// 只有在响应头尚未到达时才有资格裁决：响应头已到达（headerReceived）意味着进入了
// 排队/生成环节，必须交给整体截止计时器，不得被短阈值误杀。附加门禁
// openAICNHeaderFastFailRequireReusedConn 打开时，新建连接上的等待同样不裁决。
// 真正的胜负仍由 fire() 的 CAS 单一裁决决定。
func (w *openAICNFirstByteWatchdog) expireHeader() {
	if w.headerReceived.Load() {
		return
	}
	if !w.headerFastFailAllowed() {
		return
	}
	w.fire()
}

// HeaderReceived 报告当前尝试是否已收到上游响应头。
func (w *openAICNFirstByteWatchdog) HeaderReceived() bool {
	return w != nil && w.headerReceived.Load()
}

// TimedOutWaitingHeaders 报告本次是否属于"无响应头"分支的快速失败——
// 只有这一类需要（且允许）走同账号新连接重试。
//
// 快速失败开关关闭（当前默认）时恒为 false：分段改造之前没有新连接重试路径，
// 关闭开关必须逐字回到"Do 一次、超时直接上抛"的单段语义。
func (w *openAICNFirstByteWatchdog) TimedOutWaitingHeaders() bool {
	return w != nil && w.Fired() && !w.headerReceived.Load() && w.headerFastFailAllowed()
}

// markHeadersReceived 在 Do 成功返回（上游响应头已到达）时调用：标记进入
// 首字节等待段并停掉短阈值计时器，之后只剩整体 60s 绝对截止。
func (w *openAICNFirstByteWatchdog) markHeadersReceived() {
	w.headerReceived.Store(true)
	if w.headerTimer != nil {
		w.headerTimer.Stop()
	}
}

// connTraceFields 返回 httptrace.GotConn 采集到的连接观测字段，用于超时日志定位
// "空闲连接被上游静默丢弃"（reused=true 且 idle_time 较大即为嫌疑连接）。
func (w *openAICNFirstByteWatchdog) connTraceFields() []zap.Field {
	if w == nil {
		return nil
	}
	return []zap.Field{
		zap.Bool("reused", w.connReused.Load()),
		zap.Duration("idle_time", time.Duration(w.connIdleTime.Load())),
	}
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

// retryOpenAICNUpstreamFreshConn 在"未收到响应头"的快速失败之后，对**同一账号**
// 用**新建立的 TCP 连接**重试一次（不是换账号、不是换分组）。
//
// 返回 (resp, watchdog, retried)。retried=true 表示本次确实发起了重试（无论成败）；
// resp 非 nil 表示重试拿到了响应头，调用方须用返回的 watchdog 继续包 body 以继承
// 首字节阶段的 60s 阈值。retried=false 表示不具备重试条件，调用方按既有超时语义
// 返回 errOpenAICNFirstByteTimeout。
//
// 重试前置条件（调用方保证）：上一次尝试 Fired() 且 TimedOutWaitingHeaders()。
// 重试条件（本函数保证）：
//  0. 分段快速失败开关已打开（openAICNHeaderFastFailEnabled）；关闭时本函数是
//     纯粹的 no-op，出站次数与分段改造前完全一致（只有首次 Do）。
//  1. 上游实现了 httpUpstreamFreshConn（能给出 DisableKeepAlives 的一次性 Transport）；
//  2. 请求体可重放（Body 为 nil，或 GetBody 可用）——不可重放时绝不强发第二次；
//  3. 每请求最多 openAICNFreshConnRetryLimit 次（当前为 1），由调用方单次调用保证。
func (s *OpenAIGatewayService) retryOpenAICNUpstreamFreshConn(
	clientCtx context.Context,
	request *http.Request,
	proxyURL string,
	account *Account,
	previous *openAICNFirstByteWatchdog,
) (*http.Response, *openAICNFirstByteWatchdog, bool) {
	if s == nil || account == nil || request == nil {
		return nil, nil, false
	}
	// 开关关闭（当前默认）：不发起任何新连接重试，调用方按既有超时语义上抛。
	// 这条守卫让"关闭 = 单段语义"在重试函数内部也成立，不依赖调用方判断。
	if !openAICNHeaderFastFailOn() {
		return nil, nil, false
	}
	fields := []zap.Field{
		zap.Int64("account_id", account.ID),
		zap.String("platform", account.Platform),
	}
	if previous != nil {
		fields = append(fields, previous.connTraceFields()...)
	}

	upstream, ok := s.httpUpstream.(httpUpstreamFreshConn)
	if !ok {
		logOpenAICNFreshConnRetry(fields, false, "upstream_unsupported", nil)
		return nil, nil, false
	}
	// 请求体已被上一次尝试消费；没有 GetBody 就无法还原，只能放弃重试。
	if request.Body != nil && request.GetBody == nil {
		logOpenAICNFreshConnRetry(fields, false, "body_not_replayable", nil)
		return nil, nil, false
	}
	var replay io.ReadCloser
	if request.Body != nil {
		var err error
		if replay, err = request.GetBody(); err != nil {
			logOpenAICNFreshConnRetry(fields, false, "body_rewind_failed", err)
			return nil, nil, false
		}
	}

	// wctx 同样从 request.Context() 派生以继承 HTTPUpstreamProfile；clientCtx 只作
	// 取消裁决信号。重试用全新的 watchdog（阈值同原始值），与上一次完全隔离。
	watchdog, wctx := newOpenAICNFirstByteWatchdog(request.Context(), clientCtx, s.openAICNFirstByteTimeoutFor())
	retryReq := request.Clone(request.Context())
	retryReq.Body = replay
	retryReq = retryReq.WithContext(wctx)

	resp, err := upstream.DoFreshConn(retryReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		watchdog.release()
		switch {
		case watchdog.Fired() && !watchdog.HeaderReceived():
			logOpenAICNFreshConnRetry(fields, true, "retry_timeout_no_headers", err)
		case watchdog.Fired():
			logOpenAICNFreshConnRetry(fields, true, "retry_first_byte_timeout", err)
		case watchdog.parentDone():
			logOpenAICNFreshConnRetry(fields, true, "retry_client_gone", err)
		default:
			logOpenAICNFreshConnRetry(fields, true, "retry_transport_error", err)
		}
		return nil, watchdog, true
	}
	watchdog.markHeadersReceived()
	logOpenAICNFreshConnRetry(fields, true, "retry_ok", nil)
	return resp, watchdog, true
}

// logOpenAICNFreshConnRetry 输出同账号新连接重试的可观测事件。
// outcome 取值：upstream_unsupported / body_not_replayable / body_rewind_failed /
// retry_ok / retry_timeout_no_headers / retry_first_byte_timeout / retry_client_gone /
// retry_transport_error。
func logOpenAICNFreshConnRetry(fields []zap.Field, retried bool, outcome string, err error) {
	all := make([]zap.Field, 0, len(fields)+3)
	all = append(all, fields...)
	all = append(all,
		zap.Bool("retried", retried),
		zap.String("outcome", outcome),
		zap.Int("limit", openAICNFreshConnRetryLimit),
	)
	if err != nil {
		all = append(all, zap.Error(err))
	}
	logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
		"openai.cn_upstream_fresh_conn_retry", all...)
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
		StatusCode:        http.StatusGatewayTimeout,
		ResponseBody:      openAICNFirstByteTimeoutFailoverBody,
		ResponseHeaders:   http.Header{},
		NextAccountAction: NextAccountRetry,
		// 请求级：首包超时标记的是「本次请求在上游侧等太久」（大请求 prefill 慢、
		// 上游当时排队或连接慢），不是账号本身坏了。生产实测 2026-10-26：
		// 同类失败占账号级熔断触发的 93.4%（502 已于同日排除出名单，熔断触发里
		// 只剩 504 与 429）；且这类超时 828 次里 495 次换个号即恢复 —— 换号有用
		// 正说明不是账号故障。若计入熔断会摘掉健康账号（含唯一能承载百万级
		// token 的 137 星思云站）→ account_select_failed(pool=N, excluded=N)
		// → 客户端 503「该分组暂无可用账号」。
		// 仍走上方 handleOpenAIAccountUpstreamError(504) 的既有瞬态冷却链：
		// 该链负责让账号歇一会儿，与熔断的调度层摘除是两条独立路径。
		Scope: GatewayFailureScopeRequest,
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
		if _, werr := fmt.Fprint(c.Writer, buildChatStreamErrorSSE("upstream_error", infraerrors.UpstreamFirstByteTimeoutAfterStart)); werr == nil {
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
		sse := buildOpenAIResponseFailedSSE("", "", []byte("{}"), infraerrors.UpstreamFirstByteTimeoutAfterStart)
		if _, werr := fmt.Fprint(c.Writer, sse); werr == nil {
			if fl, ok := c.Writer.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}
	return resultWithUsage(), fmt.Errorf("stream read error after headers committed: %w", err)
}
