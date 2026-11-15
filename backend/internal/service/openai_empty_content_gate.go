package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// upstreamEmptyContentErrorCode 是「上游回了 200，但整条流里没有一个客户端能渲染
// 的字节」的统一错误码。落在 ops 事件的 Kind 上（错误详情面板可见），同时以稳定
// 前缀出现在错误原文里（列表搜索可见）。
//
// 为什么必须只留一个码：这类故障此前分散在四条出站链路上各自表述——Anthropic 原生
// 直通记「renderable content」、聊天补全链路干脆不记、商汤链路只换号不记账。同一件
// 事有三种可见度，运维想统计「今天有多少单客户拿到空白回答」时无法回答。
const upstreamEmptyContentErrorCode = "upstream_empty_content"

// upstreamEmptyContentReason 给换号错误打标记，供 handler 侧归因。
const upstreamEmptyContentReason GatewayFailureReason = "upstream_empty_content"

// upstreamEmptyContentMessagePrefix 是错误原文的稳定前缀。列表搜索按它命中，
// 因此不要在中途改字——要加信息只用后面的括号补充。
const upstreamEmptyContentMessagePrefix = "upstream returned 200 without any renderable content"

// openAIEmptyContentGate 判断一条 Chat Completions 流是否「零可渲染内容」。
//
// 判据与 anthropic 原生链路同源：正文 / 工具调用 / 拒答算内容，思考
// （reasoning_content）不算——上下文预算被思考吃光时上游会正常收尾但正文为空，
// 客户看到的就是一个空白回答。区别只在协议形态，结论必须一致，否则同一个故障在
// 不同出站协议下会得到不同的处置。
type openAIEmptyContentGate struct {
	sawContent   bool
	sawError     bool
	finishReason string
}

// ObserveChunkPayload 观察一条**未解析**的 CC data 载荷（raw 直转链路专用：那条链路
// 刻意不反序列化每一帧，靠 gjson 按需取字段）。
//
// 判据必须与 chatChunkCarriesPayloadContent 逐条对齐，交叉一致性由
// TestOpenAIEmptyContentGatePayloadMatchesChunkJudge 用同一批载荷双向验证。
func (g *openAIEmptyContentGate) ObserveChunkPayload(payload []byte) {
	if g == nil || len(payload) == 0 {
		return
	}
	if g.sawError || strings.TrimSpace(string(payload)) == "[DONE]" {
		return
	}
	if gjson.GetBytes(payload, "error").Exists() {
		// 上游明确报了错：故障根因已经由错误帧承担，不能再记成「空回答」，
		// 否则同一次故障在后台会有两条互相矛盾的解释。
		g.sawError = true
		return
	}
	if !g.sawContent {
		for _, choice := range gjson.GetBytes(payload, "choices").Array() {
			if c := choice.Get("delta.content"); c.Exists() && c.String() != "" {
				g.sawContent = true
				break
			}
			if r := choice.Get("delta.refusal"); r.Exists() && r.String() != "" {
				g.sawContent = true
				break
			}
			if tc := choice.Get("delta.tool_calls"); tc.Exists() && tc.IsArray() && len(tc.Array()) > 0 {
				g.sawContent = true
				break
			}
		}
	}
	if g.finishReason == "" {
		for _, choice := range gjson.GetBytes(payload, "choices").Array() {
			if reason := strings.TrimSpace(choice.Get("finish_reason").String()); reason != "" {
				g.finishReason = reason
				break
			}
		}
	}
}

// ObserveChatChunk 观察一个已解析的 CC chunk（两条回退链路专用，那里本来就要
// 反序列化做协议转换，复用同一份解析结果）。
func (g *openAIEmptyContentGate) ObserveChatChunk(chunk *apicompat.ChatCompletionsChunk) {
	if g == nil || chunk == nil {
		return
	}
	if !g.sawContent && chatChunkCarriesPayloadContent(*chunk) {
		g.sawContent = true
	}
	if g.finishReason == "" {
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
				g.finishReason = strings.TrimSpace(*choice.FinishReason)
				break
			}
		}
	}
}

// Empty 表示客户端一个可渲染字节都没收到，且上游没有报过错。
func (g *openAIEmptyContentGate) Empty() bool {
	return g != nil && !g.sawContent && !g.sawError
}

// FinishReason 返回上游给的停止原因（可能为空：部分兼容上游不发）。
func (g *openAIEmptyContentGate) FinishReason() string {
	if g == nil {
		return ""
	}
	return g.finishReason
}

// emptyContentClientBudgetFinishReason 判定「空正文是不是客户自己把输出预算给小了」。
//
// 这类空回答是预期截断，不是账号故障：冷却该账号只会让健康账号被无谓摘掉，
// 记一笔上游错误也会把责任算到供应商头上。OpenAI 系用 length，部分国产兼容实现
// 直接抄 Anthropic 的 max_tokens，个别上游用 max_output_tokens，三个都认。
func emptyContentClientBudgetFinishReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_output_tokens":
		return true
	}
	return false
}

// emptyContentOutputBudget 从「请求形状」里取客户自己设的输出上限。形状是出站前
// 采过一次的事实（不计正文、不含任何敏感值），三种协议字段名都认。
func emptyContentOutputBudget(shape map[string]any) (string, int64, bool) {
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		value, ok := shape[key]
		if !ok {
			continue
		}
		var n int64
		switch v := value.(type) {
		case int64:
			n = v
		case int:
			n = int64(v)
		case float64:
			n = int64(v)
		default:
			continue
		}
		if n > 0 {
			return key, n, true
		}
	}
	return "", 0, false
}

// emptyContentBudgetNoticeText 拼给客户看的提示。数值取不到时走无数字版本——
// 绝不把 `max_tokens=0` 这种占位符回显给客户。
func emptyContentBudgetNoticeText(shape map[string]any) string {
	key, value, ok := emptyContentOutputBudget(shape)
	if !ok {
		return infraerrors.OutputBudgetTooSmallNoticeNoValue
	}
	return fmt.Sprintf(infraerrors.OutputBudgetTooSmallNotice, key, value, key)
}

// emptyContentNoticeFrames 把一段提示文字按**入站协议**转成完整合法的 SSE 帧序列。
//
// 不手写 JSON：三种协议都复用仓库里已有的 CC→协议转换器（同一份事实、同一套状态机），
// 由它们生成 message_start / response.created / content_block_start 等前置帧。
// 这样客户收到的是一段正常回答，不是错误帧，也不是半截流——HTTP 200、协议不变、
// 客户端照常渲染，只是内容变成了一句「为什么这次是空的、该怎么改」。
func emptyContentNoticeFrames(c *gin.Context, model, text string) []string {
	path := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	id := "chatcmpl-notice"
	if c != nil {
		if v := c.GetString("request_id"); v != "" {
			id = v
		}
	}
	chunk := apicompat.ChatCompletionsChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []apicompat.ChatChunkChoice{{
			Index: 0,
			Delta: apicompat.ChatDelta{Role: "assistant", Content: &text},
		}},
	}
	frames := make([]string, 0, 8)

	switch {
	case strings.Contains(path, "/messages"):
		state := apicompat.NewChatCompletionsToAnthropicStreamState(model)
		events := apicompat.ChatCompletionsChunkToAnthropicEvents(&chunk, state)
		events = append(events, apicompat.FinalizeChatCompletionsAnthropicStream(state)...)
		for _, event := range events {
			if sse, err := apicompat.ResponsesAnthropicEventToSSE(event); err == nil {
				frames = append(frames, sse)
			}
		}
	case strings.Contains(path, "/responses"):
		state := apicompat.NewChatCompletionsToResponsesStreamState(model)
		events := apicompat.ChatCompletionsChunkToResponsesEvents(&chunk, state)
		events = append(events, apicompat.FinalizeChatCompletionsResponsesStream(state)...)
		for _, event := range events {
			if sse, err := apicompat.ResponsesEventToSSE(event); err == nil {
				frames = append(frames, sse)
			}
		}
		frames = append(frames, "data: [DONE]\n\n")
	default: // /v1/chat/completions 及其它 OpenAI 兼容入口
		if sse, err := apicompat.ChatChunkToSSE(chunk); err == nil {
			frames = append(frames, sse)
		}
		stop := "stop"
		finish := apicompat.ChatCompletionsChunk{
			ID: id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: model,
			Choices: []apicompat.ChatChunkChoice{{Index: 0, FinishReason: &stop}},
		}
		if sse, err := apicompat.ChatChunkToSSE(finish); err == nil {
			frames = append(frames, sse)
		}
		frames = append(frames, "data: [DONE]\n\n")
	}
	return frames
}

// newEmptyContentFailoverError 构造「上游空回答」的换号错误：本次请求换下一个账号
// 重试，客户端无感。SafeToFailoverAfterWrite 的前提是调用方已确认客户端一个语义
// 字节都没收到。
//
// NextAccountAction/Scope 与商汤空流原实现保持一致（请求级、换号、不对账号追加
// 惩罚性判定——停调已经由 tempUnscheduleEmptyResponse 单独承担，两件事不混）。
func newEmptyContentFailoverError() *UpstreamFailoverError {
	return &UpstreamFailoverError{
		StatusCode:               http.StatusBadGateway,
		SafeToFailoverAfterWrite: true,
		NextAccountAction:        NextAccountRetry,
		Scope:                    GatewayFailureScopeRequest,
		Reason:                   upstreamEmptyContentReason,
	}
}

// emptyContentReport 是「上游空回答」的执法参数。字段都是调用方已知的现场，
// 判定与处置全部收在这一层，调用方只负责把看到的事实填进来。
type emptyContentReport struct {
	// Account 出问题的上游账号（用于记账与冷却）。
	Account *Account
	// Resp 上游响应（取状态码与上游 request id 入账）。
	Resp *http.Response
	// UpstreamModel 出站模型名，进日志。
	UpstreamModel string
	// UpstreamRequestID 上游追踪号，进日志。
	UpstreamRequestID string
	// LogPrefix 是容器日志前缀，用来区分是哪条出站链路命中（如
	// "openai chat_completions raw"）。同一个前缀也进入停调日志。
	LogPrefix string
	// Stage 命中时刻：complete（流正常收尾）/ eof / terminal_read_error。
	Stage string
	// SawContent 调用方自己判定出的「是否收到过可渲染内容」。协议不同，判据不同
	// （Anthropic SSE 用 anthropicSSELineCarriesPayloadContent，CC 用 Gate），
	// 但结论都落到这一个字段上。
	SawContent bool
	// FinishReason 上游停止原因，用于区分「账号故障」与「客户预算给小」。
	FinishReason string
	// CanFailover 是否还能换号：只有客户端一个语义字节都没收到时为真。
	CanFailover bool
	// WriteHeaders 是调用方自己的 SSE 响应头写入闭包。只有「响应头尚未提交」时才
	// 能用它把提示作为一段完整回答发给客户；复用调用方的闭包，避免在这里另造一套
	// 响应头（X-Accel-Buffering 等由各链路既有逻辑负责）。
	WriteHeaders func()
	// WriteBufferedNotice 是「非流式」链路的写出通道：把提示文字写进响应体，返回
	// 是否写成功。流式填 WriteHeaders，非流式填这个，二者只走一条。
	//
	// 判定、文案、日志全部留在 reportEmptyContent 这一处——调用方只描述「怎么把
	// 一句话放进响应」，不自己再判一次是不是预算不足。否则同一件事会在两处各有
	// 一套理解，改了一边忘另一边就会出现「日志说没提示、客户却收到了」。
	WriteBufferedNotice func(text string) bool
}

// reportEmptyContent 是「上游 200 但零正文」的唯一执法点：判定 → 记账 → 停调 →
// （能换号时）返回换号错误。
//
// 为什么记账和停调不跟换号绑在一起（P0/P1 拆开的原因）：很多情况下我们的网桥
// 已经先写出了 message_start / role 帧，响应头已提交，换号窗口关闭。这种场景以前
// 什么都不做——客户拿到空白回答，后台一条记录都没有，运维只能靠客户投诉发现。
// 现在即使换不了号，也会记一笔账并冷却该账号一分钟，让下一个请求绕开它。
//
// 返回值 noticeWritten=true 表示「已经把提示作为一段正常回答完整写给客户了」，
// 调用方应直接按成功收尾，不要再 flush 上游那条空流（否则会重复 message_start
// 之类的前置帧）。返回 nil error 表示没做任何处置，调用方按原有流程继续收尾。
func (s *OpenAIGatewayService) reportEmptyContent(
	ctx context.Context,
	c *gin.Context,
	r emptyContentReport,
) (bool, error) {
	if r.SawContent {
		return false, nil
	}
	if emptyContentClientBudgetFinishReason(r.FinishReason) {
		// 客户预算给小了：空正文是预期截断，不是账号故障 —— 不记账、不停调、
		// 不报错、不改客户参数。但不能悄悄放行：客户看到空白回答只会怪站点。
		// 做法是把「为什么空、该怎么改」写成一段正常回答内容发回去（HTTP 200）。
		shape := OpsRequestShapeFrom(c)
		budgetKey, budgetValue, hasValue := emptyContentOutputBudget(shape)
		text := emptyContentBudgetNoticeText(shape)

		// 只有两条写出通道：非流式把提示塞进 JSON 响应体，流式补一段完整 SSE。
		// 都不可用（响应头已提交 / 客户端已收到字节）时保持原有放行语义，不再插
		// 前置帧，否则会和已经发出去的流打架。
		noticeChannel := "none"
		switch {
		case r.WriteBufferedNotice != nil:
			noticeChannel = "buffered"
		case r.CanFailover && r.WriteHeaders != nil && c != nil && c.Writer != nil:
			noticeChannel = "stream"
		}
		logger.L().Warn(r.LogPrefix+": empty content with client output budget exhausted, returning notice",
			zap.String("request_id", r.UpstreamRequestID),
			zap.Int64("account_id", emptyContentAccountID(r.Account)),
			zap.String("upstream_model", r.UpstreamModel),
			zap.String("finish_reason", r.FinishReason),
			zap.String("stage", r.Stage),
			zap.String("budget_key", budgetKey),
			zap.Int64("budget_value", budgetValue),
			zap.Bool("has_budget_value", hasValue),
			zap.String("notice_channel", noticeChannel),
			zap.Bool("can_write_notice", noticeChannel != "none"),
		)
		if noticeChannel == "buffered" {
			return r.WriteBufferedNotice(text), nil
		}
		if noticeChannel == "none" {
			return false, nil
		}
		frames := emptyContentNoticeFrames(c, r.UpstreamModel, text)
		if len(frames) == 0 {
			return false, nil
		}
		r.WriteHeaders()
		for _, frame := range frames {
			if _, err := io.WriteString(c.Writer, frame); err != nil {
				break
			}
		}
		c.Writer.Flush()
		return true, nil
	}

	message := upstreamEmptyContentMessage(r.Stage, r.FinishReason)
	logger.L().Warn(r.LogPrefix+": upstream returned empty content stream",
		zap.String("request_id", r.UpstreamRequestID),
		zap.Int64("account_id", emptyContentAccountID(r.Account)),
		zap.String("upstream_model", r.UpstreamModel),
		zap.String("finish_reason", r.FinishReason),
		zap.String("stage", r.Stage),
		zap.Bool("can_failover", r.CanFailover),
	)
	s.recordOpenAIMessagesStreamUpstreamError(c, r.Account, r.Resp, upstreamEmptyContentErrorCode, message)
	if s != nil && s.accountRepo != nil && r.Account != nil {
		tempUnscheduleEmptyResponse(ctx, s.accountRepo, r.Account.ID, "["+r.LogPrefix+"] empty_content")
	}
	if !r.CanFailover {
		return false, nil
	}
	return false, newEmptyContentFailoverError()
}

// upstreamEmptyContentMessage 拼接错误原文：稳定前缀 + 现场括号。括号里的内容只给
// 人看，不参与任何分类（分类靠 Kind 与 Reason，不用文本反解）。
func upstreamEmptyContentMessage(stage, finishReason string) string {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = "unknown"
	}
	finishReason = strings.TrimSpace(finishReason)
	if finishReason == "" {
		finishReason = "-"
	}
	return upstreamEmptyContentMessagePrefix + " (stage=" + stage + ", finish_reason=" + finishReason + ")"
}

func emptyContentAccountID(account *Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID
}

// emptyContentRequestContext 取请求上下文用于停调写库：调用方不一定持有 ctx
// （部分流式函数只有 gin.Context），而停调只需要一个能落库的上下文。
func emptyContentRequestContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}
