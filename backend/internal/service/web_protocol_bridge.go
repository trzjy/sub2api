package service

// web_protocol_bridge.go 把网页逆向适配器（zhipu / deepseek / kimi 官方平台 + access_mode=web）产出的
// 通用 OpenAI chat.completion / chat.completion.chunk 包络，按入站协议回桥为对应形态
// （#2，Codex 审查回执）：
//
//   - /v1/chat/completions 入站 → chat.completion 直出（现状，不变）；
//   - /v1/responses 入站       → OpenAI Responses API 对象 / 事件流；
//   - /v1/messages 入站        → Anthropic Messages 响应 / 事件流。
//
// 转换全部复用既有 apicompat 回桥（ChatCompletionsResponseToResponses /
// ChatCompletionsChunkToResponsesEvents / FinalizeChatCompletionsResponsesStream /
// ChatCompletionsResponseToAnthropic / ChatCompletionsChunkToAnthropicEvents /
// FinalizeChatCompletionsAnthropicStream），不在三个 forward 内部散落任何协议逻辑。
// 三个 forward 只负责把上游数据聚合为标准的 chat.completion 包络，再由本文件统一回桥。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// webResponseMode 选择 forwardWeb* 出站响应的协议形态，与入站协议对齐。
type webResponseMode int

const (
	// webResponseModeChat 入站 /v1/chat/completions：chat.completion 直出（现状正确）。
	webResponseModeChat webResponseMode = iota
	// webResponseModeResponses 入站 /v1/responses：回桥为 Responses API 响应。
	webResponseModeResponses
	// webResponseModeAnthropic 入站 /v1/messages：回桥为 Anthropic Messages 响应。
	webResponseModeAnthropic
)

// webClientStreamState 跨流式分片持有回桥状态，以及出站协议形态与模型名。
// chat 模式无需状态（直出），respState / anthState 仅 Responses / Anthropic 模式使用。
type webClientStreamState struct {
	mode      webResponseMode
	model     string
	respState *apicompat.ChatCompletionsToResponsesStreamState
	anthState *apicompat.ChatCompletionsToAnthropicStreamState
}

// newWebClientStreamState 按出站协议创建流式状态；chat 模式状态机置空（直出）。
func newWebClientStreamState(mode webResponseMode, model string) *webClientStreamState {
	st := &webClientStreamState{mode: mode, model: model}
	switch mode {
	case webResponseModeResponses:
		st.respState = apicompat.NewChatCompletionsToResponsesStreamState(model)
	case webResponseModeAnthropic:
		st.anthState = apicompat.NewChatCompletionsToAnthropicStreamState(model)
	}
	return st
}

// writeWebStreamChunk 把一帧 chat.completion.chunk 包络按出站协议写回客户端。
// 调用方负责在流结束（含终帧）时再调用一次 finalizeWebStream。
func writeWebStreamChunk(c *gin.Context, st *webClientStreamState, envelope map[string]any) error {
	switch st.mode {
	case webResponseModeChat:
		return writeRawWebChunk(c, envelope)
	case webResponseModeResponses:
		chunk := webEnvelopeToChatChunk(envelope)
		if chunk == nil {
			return nil
		}
		return writeResponsesEvents(c, apicompat.ChatCompletionsChunkToResponsesEvents(chunk, st.respState))
	case webResponseModeAnthropic:
		chunk := webEnvelopeToChatChunk(envelope)
		if chunk == nil {
			return nil
		}
		return writeAnthropicEvents(c, apicompat.ChatCompletionsChunkToAnthropicEvents(chunk, st.anthState))
	}
	return nil
}

// finalizeWebStream 写出流的终止符：chat 模式为 data: [DONE]；Responses / Anthropic 模式
// 由对应回桥发出 terminal 事件（response.completed / message_stop），不再写 [DONE]。
func finalizeWebStream(c *gin.Context, st *webClientStreamState) error {
	switch st.mode {
	case webResponseModeChat:
		if _, err := c.Writer.WriteString("data: [DONE]\n\n"); err != nil {
			return err
		}
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	case webResponseModeResponses:
		return writeResponsesEvents(c, apicompat.FinalizeChatCompletionsResponsesStream(st.respState))
	case webResponseModeAnthropic:
		return writeAnthropicEvents(c, apicompat.FinalizeChatCompletionsAnthropicStream(st.anthState))
	}
	return nil
}

// writeWebCompletion 把非流式 chat.completion 包络按出站协议写回客户端（JSON 响应体）。
func writeWebCompletion(c *gin.Context, st *webClientStreamState, completion map[string]any) error {
	switch st.mode {
	case webResponseModeChat:
		data, err := json.Marshal(completion)
		if err != nil {
			return err
		}
		c.Header("Content-Type", "application/json")
		c.Data(http.StatusOK, "application/json", data)
		return nil
	case webResponseModeResponses:
		out := apicompat.ChatCompletionsResponseToResponses(webEnvelopeToChatResponse(completion), st.model, nil, nil, false, nil)
		data, err := json.Marshal(out)
		if err != nil {
			return err
		}
		c.Header("Content-Type", "application/json")
		c.Data(http.StatusOK, "application/json", data)
		return nil
	case webResponseModeAnthropic:
		out := apicompat.ChatCompletionsResponseToAnthropic(webEnvelopeToChatResponse(completion), st.model)
		data, err := json.Marshal(out)
		if err != nil {
			return err
		}
		c.Header("Content-Type", "application/json")
		c.Data(http.StatusOK, "application/json", data)
		return nil
	}
	return nil
}

// writeRawWebChunk 以标准 OpenAI chat.completion.chunk 形状（data: 前缀）写一帧 SSE。
func writeRawWebChunk(c *gin.Context, chunk map[string]any) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	frame := append([]byte("data: "), data...)
	frame = append(frame, '\n', '\n')
	if _, err := c.Writer.Write(frame); err != nil {
		return err
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// writeResponsesEvents 把若干 Responses 流事件写为 SSE 帧（event: <type>\ndata: <json>）。
func writeResponsesEvents(c *gin.Context, events []apicompat.ResponsesStreamEvent) error {
	for i := range events {
		sse, err := apicompat.ResponsesEventToSSE(events[i])
		if err != nil {
			return err
		}
		if _, err := c.Writer.WriteString(sse); err != nil {
			return err
		}
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// writeAnthropicEvents 把若干 Anthropic 流事件写为 SSE 帧（event: <type>\ndata: <json>）。
func writeAnthropicEvents(c *gin.Context, events []apicompat.AnthropicStreamEvent) error {
	for i := range events {
		sse, err := apicompat.ResponsesAnthropicEventToSSE(events[i])
		if err != nil {
			return err
		}
		if _, err := c.Writer.WriteString(sse); err != nil {
			return err
		}
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// webEnvelopeToChatChunk 把 chat.completion.chunk 包络经 JSON 往返还原为
// apicompat.ChatCompletionsChunk；usage 单独以兼容解析覆盖（见 webUsageFromEnvelope）。
func webEnvelopeToChatChunk(envelope map[string]any) *apicompat.ChatCompletionsChunk {
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil
	}
	var chunk apicompat.ChatCompletionsChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil
	}
	if u := webUsageFromEnvelope(envelope); u != nil {
		chunk.Usage = u
	}
	return &chunk
}

// webEnvelopeToChatResponse 把 chat.completion 包络经 JSON 往返还原为
// apicompat.ChatCompletionsResponse；usage 单独以兼容解析覆盖。
func webEnvelopeToChatResponse(completion map[string]any) *apicompat.ChatCompletionsResponse {
	data, err := json.Marshal(completion)
	if err != nil {
		return nil
	}
	var resp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil
	}
	if u := webUsageFromEnvelope(completion); u != nil {
		resp.Usage = u
	}
	return &resp
}

// webUsageFromEnvelope 兼容两种 usage 命名形态：上游 chat.completion 的
// prompt_tokens/completion_tokens，以及本仓库网页适配器本地 OpenAIUsage 的
// input_tokens/output_tokens（Anthropic/Responses 命名）。统一归一到 apicompat.ChatUsage
// （prompt_tokens/completion_tokens 命名），保证回桥后 Responses/Anthropic usage 正确。
func webUsageFromEnvelope(envelope map[string]any) *apicompat.ChatUsage {
	raw, ok := envelope["usage"]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var u struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
	}
	if err := json.Unmarshal(data, &u); err != nil {
		return nil
	}
	prompt := u.PromptTokens
	completion := u.CompletionTokens
	if prompt == 0 && completion == 0 {
		prompt = u.InputTokens
		completion = u.OutputTokens
	}
	if prompt == 0 && completion == 0 {
		return nil
	}
	total := u.TotalTokens
	if total == 0 {
		total = prompt + completion
	}
	return &apicompat.ChatUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	}
}

// ── web 三平台流式回程的读侧兜底（kimi / zhipu / deepseek 共用）────────────
//
// 背景：三条网页逆向流式回程此前都是「裸 for + 阻塞读」——读函数底层是
// io.ReadFull / bufio.Scanner，均无 deadline。上游开了流却不吐数据（挂死连接、
// 中间设备吞流）时读循环永久阻塞，且：
//   - 无 StreamDataIntervalTimeout 兜底 → goroutine 长期占用（Redis TTL 15/30 分钟）；
//   - 无下游 keepalive → Cloudflare Tunnel / 反代因空闲断开，客户端侧表现为无响应；
//   - 无独立读取 goroutine → ctx.Done() 无法唤醒阻塞中的读。
//
// 对照组（已三件套齐全）：anthropic 回程 gateway_upstream_response.go 的
// handleStreamingResponse —— 独立读 goroutine + done 通道 + interval ticker +
// keepalive timer。本文件把这套机制抽成三个平台共用的守卫，避免同一份超时
// 语义在三处各写一遍而漂移。
//
// 语义边界（务必保持与 anthropic 回程一致）：
//   - interval 超时判定的是「上游数据到达间隔」，不是下游写入耗时；
//   - keepalive 只在**尚未写出首帧**时发（见 keepalivePending 的 data != "" 口径），
//     写出首帧后上游持续来数，keepalive 没有意义且会污染 TTFT 观测；
//   - 客户端断开**不**立刻掐断读：断开后仍继续读上游把 usage 采完（计费需要），
//     由 interval 超时作为最终上界。这与 anthropic 回程 clientDisconnected 后
//     「continuing to drain upstream for billing」同构。
//   - interval 判定用**周期性 Ticker**（非一次性 Timer）：每个 tick 都按
//     lastReadAt 重新判定，即使某一拍因「距上次读不足 interval」被跳过，后续
//     拍仍会到达，挂死上游必然被兜住（与 anthropic 回程 NewTicker 同构）。

// errWebStreamSkippedFrame 表示本次读取没有产出可投递的帧（上游发了应跳过的
// 空帧 / keepalive 帧）。守卫视之为「继续读下一帧」，既不投递也不重置
// interval 计时——否则一个只发心跳的死上游会被误判为持续有数据，interval
// 超时永不触发。
var errWebStreamSkippedFrame = errors.New("web stream frame skipped")

// errWebStreamIdleTimeout 是上游流数据间隔超时的哨兵错误。调用方据此走各自的
// 收口路径（failover / 客户端 error 帧），故单独导出而非复用 anthropic 回程的
// errAnthropicNativeStreamIdle——两者语义相同但归属不同链路。
var errWebStreamIdleTimeout = errors.New("web stream data interval timeout")

// webStreamReadResult 是单次读取的结果：data 为一帧文本（kimi 为已剥壳的
// Connect envelope payload）；err 非空表示读取失败（含超时）；EOF 时由
// readFn 返回 io.EOF 之外的约定——见 webStreamGuard 的 events 关闭语义。
type webStreamReadResult struct {
	data []byte
	err  error
}

// webStreamGuard 是 web 三平台流式回程的读侧守卫：一个独立 goroutine 负责
// 阻塞读，上层用 select 同时观察「读到数据 / 间隔超时 / 下游 keepalive /
// 上层 ctx 取消」四路信号。
type webStreamGuard struct {
	events chan webStreamReadResult
	// done 关闭后读 goroutine 停止投递并退出；必须由调用方 defer close，
	// 否则调用方提前返回（如 failover）时读 goroutine 会向满 channel 阻塞泄漏。
	done chan struct{}

	// lastReadAt 是最后一次成功读到上游字节的时刻（UnixNano，原子访问）。
	// 与 anthropic 回程同口径：interval 判定基于它而非 ticker 累积，避免
	// 下游写入阻塞被误判成上游断流。
	lastReadAt atomic.Int64

	// intervalTicker 是**周期性**触发器（不是一次性 Timer）。每次 tick 都重新
	// 用 lastReadAt 判定 streamIdle，语义与 anthropic 回程的 time.NewTicker
	// 完全一致。曾经的 time.NewTimer 是最严重缺陷：它只 fire 一次，若那一次
	// 因「距上次读不足 interval」被 continue 掉，intervalCh 就永久枯竭——
	// 上游随后挂死（连接保持、不吐字节、不返 EOF）时读 goroutine 永久阻塞，
	// 正是本守卫要修的场景。
	intervalTicker *time.Ticker
	intervalCh     <-chan time.Time
	keepaliveTimer *time.Timer
	keepaliveCh    <-chan time.Time
	// keepalive 保留装配时的间隔值，供 resetKeepalive 重置计时器使用。
	keepalive time.Duration
}

// webStreamGuardParams 是守卫的构造参数。
type webStreamGuardParams struct {
	// readFn 执行一次阻塞读，返回一帧数据。约定：
	//   - (data, nil)：投递 data 给上层继续处理；
	//   - (nil, io.EOF)：上游正常收流，守卫关闭 events 通道，上层跳出读循环；
	//   - (nil, 其他 err)：读取失败，守卫投递该错误后关闭 events 通道。
	// readFn 必须把 io.EOF 原样返回（可用 errors.Is 判定），不得吞掉——否则
	// 上层无法区分「流结束」与「读到一帧空数据」，会出现读到 EOF 后空转。
	readFn func() ([]byte, error)
	// interval 是上游数据间隔上限，<=0 表示禁用（保持旧的无限等待行为）。
	interval time.Duration
	// keepalive 是下游保活间隔，<=0 表示禁用。
	keepalive time.Duration
}

// newWebStreamGuard 启动读 goroutine 并返回守卫。调用方必须 defer stop()。
func newWebStreamGuard(p webStreamGuardParams) *webStreamGuard {
	g := &webStreamGuard{
		events: make(chan webStreamReadResult, 16),
		done:   make(chan struct{}),
	}
	g.lastReadAt.Store(time.Now().UnixNano())
	if p.interval > 0 {
		g.intervalTicker = time.NewTicker(p.interval)
		g.intervalCh = g.intervalTicker.C
	}
	if p.keepalive > 0 {
		g.keepalive = p.keepalive
		g.keepaliveTimer = time.NewTimer(p.keepalive)
		g.keepaliveCh = g.keepaliveTimer.C
	}

	go func() {
		defer close(g.events)
		for {
			data, err := p.readFn()
			switch {
			case errors.Is(err, io.EOF):
				// 上游正常收流：关闭 events 即为「流结束」信号。
				return
			case errors.Is(err, errWebStreamSkippedFrame):
				// 上游显式要求跳过本帧：继续读下一帧，且不重置 interval 计时。
				continue
			case err != nil:
				select {
				case g.events <- webStreamReadResult{err: err}:
				case <-g.done:
				}
				return
			}
			if len(data) == 0 {
				// 零长帧（空 SSE 行 / Connect keepalive）：不投递、**不更新
				// lastReadAt**。若在此更新，一个只发心跳的死上游会被误判为持续
				// 有数据，interval 超时永不触发——正是本次要修的挂死场景。
				//
				// 与 Ticker 的关系：零长帧不更新 lastReadAt，而每次 tick 都重新
				// 用 lastReadAt 判定 streamIdle，故心跳流会在「最后一次实质字节
				// + interval」之后的第一个 tick 被判定 idle 并终止；不需要（也不
				// 应该）在零长帧上 Reset 计时器——那正是把心跳流误判为活流的口子。
				continue
			}
			g.lastReadAt.Store(time.Now().UnixNano())
			select {
			case g.events <- webStreamReadResult{data: data}:
			case <-g.done:
				return
			}
		}
	}()
	return g
}

// streamIdle 判定上游是否已超过 interval 未送来任何字节。
func (g *webStreamGuard) streamIdle(interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	return time.Since(time.Unix(0, g.lastReadAt.Load())) >= interval
}

// resetKeepalive 在向下游成功写出数据后重置保活计时。
func (g *webStreamGuard) resetKeepalive() {
	if g.keepaliveTimer == nil {
		return
	}
	if !g.keepaliveTimer.Stop() {
		select {
		case <-g.keepaliveTimer.C:
		default:
		}
	}
	g.keepaliveTimer.Reset(g.keepalive)
}

// stop 终止读 goroutine 并释放计时器。
//
// 注意（与 anthropicNativeLinePump 同约束）：若读 goroutine 正阻塞在
// readFn 内的 socket 读上，仅 close(done) 不足以让它立刻退出——必须由调用方
// 关闭 resp.Body（各 forward 既有 defer resp.Body.Close() 已覆盖）才能解除阻塞。
func (g *webStreamGuard) stop() {
	close(g.done)
	if g.intervalTicker != nil {
		g.intervalTicker.Stop()
	}
	if g.keepaliveTimer != nil {
		g.keepaliveTimer.Stop()
	}
}

// webStreamGuardParamsFromConfig 从网关配置装配守卫参数：三平台与 anthropic
// 回程共用同一组阈值（gateway.stream_data_interval_timeout 默认 180s、
// gateway.stream_keepalive_interval 默认 10s），避免同仓两套超时语义。
func (s *OpenAIGatewayService) webStreamGuardParams(readFn func() ([]byte, error)) webStreamGuardParams {
	p := webStreamGuardParams{readFn: readFn}
	if s.cfg != nil {
		if s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
			p.interval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
		}
		if s.cfg.Gateway.StreamKeepaliveInterval > 0 {
			p.keepalive = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
		}
	}
	return p
}

// webStreamKeepalivePending 判定此刻是否应当发下游 keepalive。
//
// dataWritten 为「是否已向客户端写出过实质内容」。与 anthropic 回程
// gateway_upstream_response.go 的 `data != ""` 判定同构：只有**尚未写出首帧**
// 时上游才可能长时间静默（首帧前的思考/检索阶段），此时保活才有必要；
// 已出首帧后保活帧会污染 TTFT 观测且无收益。
func webStreamKeepalivePending(dataWritten bool, keepaliveInterval time.Duration, lastDataAt time.Time) bool {
	if dataWritten || keepaliveInterval <= 0 {
		return false
	}
	return time.Since(lastDataAt) >= keepaliveInterval
}

// webStreamTeeBuffer 是带并发保护的 bytes.Buffer：web 深搜流式回程用
// io.TeeReader 把原始响应体留存一份（流结束无内容帧时用于整包业务错误判定），
// 但 TeeReader 的写入发生在守卫的读 goroutine 里，与主 goroutine 读
// Bytes() 存在数据竞争。此包装用互斥量消除该竞争。
type webStreamTeeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *webStreamTeeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Bytes 返回当前已留存的原始响应体快照（加锁拷贝）。
func (b *webStreamTeeBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// recordWebStreamReadError 把「流式回程读失败」登记进 ops 上游上下文，
// 对齐原生 OpenAI 路径 recordOpenAIRawStreamTruncation 的做法（双设）。
//
// 为什么必须登记：ops 侧 classifyOpsPhase 对 upstream_error 无条件归类为
// upstream，phase/owner 本就正确；真正丢失的是 attempt 级遥测——
// upstream_status_code / upstream_errors / upstream_error_message 三字段全空，
// 排障时看不到本次尝试用的 platform / account / proxy / request-id，只能
// 从外层错误文案反推。
//
// 注意 error_source 的既有行为：classifyOpsErrorSource 对 phase=="upstream"
// 无条件返回 upstream_http，而传输层读失败（connection reset / unexpected EOF）
// 本不属于 HTTP 错误语义。这里不改动该分类（跨文件、影响面大且会变更既有
// 统计口径），只在 Kind 上如实标注 stream_read_error 以便区分。
//
// 注意 failover 行为不变：本函数只做登记，不触碰任何 failover 判定，
// 调用方仍按原有分支决定是否换号。
func recordWebStreamReadError(c *gin.Context, account *Account, upstreamModel string, cause error) {
	if c == nil {
		return
	}
	message := "upstream stream read failed"
	if cause != nil {
		message += ": " + sanitizeStreamError(cause)
	}
	platform := ""
	var accountID int64
	accountName := ""
	var proxyID *int64
	proxyName := ""
	if account != nil {
		platform = account.Platform
		accountID = account.ID
		accountName = account.Name
		proxyID = opsUpstreamProxyID(account)
		proxyName = opsUpstreamProxyName(account)
	}

	// 传输层失败没有 HTTP 状态码可言，用 0 表示「非 HTTP 层失败」，与
	// OpsUpstreamErrorEvent.UpstreamStatusCode 的 omitempty 语义一致——
	// 写 502 会让 ops 把传输层断流误读成上游返回了 502。
	setOpsUpstreamError(c, 0, message, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:     proxyID,
		ProxyName:   proxyName,
		Platform:    platform,
		AccountID:   accountID,
		AccountName: accountName,
		Kind:        "stream_read_error",
		Scope:       "upstream_stream",
		Reason:      upstreamModel,
		Message:     message,
	})
}

// writeWebStreamKeepalive 向下游写一帧 SSE 注释保活（`: \n\n`）。
//
// 用 SSE 注释而非 data 帧的原因：注释行按 SSE 规范被所有客户端解析器忽略，
// 不进入任何 SDK 的消息序列，因此不会污染 TTFT 观测、也不会被误当成
// assistant 内容——与 antigravity_gateway_streaming 的 keepalive 帧同形。
//
// 不走 writeWebStreamChunk：保活不是协议帧，不应进入 chat.completion.chunk
// → Responses / Anthropic 的回桥转换（那会把注释变成业务事件）。
//
// 保活字节必须登记（recordOpenAIStreamKeepaliveBytes），否则会被 handler 的
// failover 判定当成语义输出而拒绝透明换号——详见函数体内注释。
func writeWebStreamKeepalive(c *gin.Context) error {
	n, err := c.Writer.WriteString(": \n\n")
	// 把保活字节登记进既有的「非语义输出」扣减口径（recordOpenAIStreamKeepaliveBytes
	// 的累加器即 OpenAICompactKeepaliveAdjustedWrittenSize 读取的那个 key）。
	// 不登记的后果是 CN 首包分段快失败整改被击穿：keepalive 虽只写 SSE 注释，
	// 但已 Flush 提交 HTTP 头，handler 的 openAIForwardMayFailover 按
	// 「Forward 前后 Writer.Size 是否变化」判定已写出语义内容 → 拒绝换号 →
	// 客户端收到 200 + 若干 ": " 注释 + 流内 error，失去透明换号。
	recordOpenAIStreamKeepaliveBytes(c, n)
	if err != nil {
		return err
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}
