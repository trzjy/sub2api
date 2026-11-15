package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
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
}

// reportEmptyContent 是「上游 200 但零正文」的唯一执法点：判定 → 记账 → 停调 →
// （能换号时）返回换号错误。
//
// 为什么记账和停调不跟换号绑在一起（P0/P1 拆开的原因）：很多情况下我们的网桥
// 已经先写出了 message_start / role 帧，响应头已提交，换号窗口关闭。这种场景以前
// 什么都不做——客户拿到空白回答，后台一条记录都没有，运维只能靠客户投诉发现。
// 现在即使换不了号，也会记一笔账并冷却该账号一分钟，让下一个请求绕开它。
//
// 返回 nil 表示「已记账/已冷却，但没有可用的换号动作」，调用方按原有流程继续收尾。
func (s *OpenAIGatewayService) reportEmptyContent(
	ctx context.Context,
	c *gin.Context,
	r emptyContentReport,
) error {
	if r.SawContent {
		return nil
	}
	if emptyContentClientBudgetFinishReason(r.FinishReason) {
		// 客户预算给小了：空正文是预期截断，不是账号故障，不记账也不停调。
		logger.L().Warn(r.LogPrefix+": empty content with client output budget exhausted, pass through",
			zap.String("request_id", r.UpstreamRequestID),
			zap.Int64("account_id", emptyContentAccountID(r.Account)),
			zap.String("upstream_model", r.UpstreamModel),
			zap.String("finish_reason", r.FinishReason),
			zap.String("stage", r.Stage),
		)
		return nil
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
		return nil
	}
	return newEmptyContentFailoverError()
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
