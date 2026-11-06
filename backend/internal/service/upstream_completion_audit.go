package service

import (
	"context"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// upstreamCompletionAudit 审计「上游这次到底有没有真的产出可呈现内容」。
//
// 为什么必须由中转侧判定，而不是相信上游自称成功：
// 官方端点由官方保证「200 + 终态事件」就意味着真的给出了回答；我们对接的是各路第三方
// 供应商，实测存在「200 + 完整终态事件 + 零正文」的完成形态。2026-11-06 线上证据：
// 账号 137（星思云站Deepseek）三天内 163 次该形态，占其 /v1/messages 流量的 12.5%，
// 而同一分组内另外三个能接同一模型的账号为 0 次。客户端收到的是一段什么都没有的正常流，
// 既看不到回答，也不触发重试，只能认为「模型不稳定」——这是最坏的一种失败：
// 比报错更糟，因为客户无从知道发生了什么。
//
// 本类型是这条判据的唯一实现，各协议中继循环只负责把收到的 data 行喂进来，
// 禁止在任何中继循环里就地判断「内容为空」。
//
// 判定口径（宁可不判，也不误判）：
//   - 正文（text / tool_use / 图片）计入内容；
//   - 思考（thinking）不计入：思考不是客户要的答案，只有思考没有正文正是「空完成」的典型形态；
//   - 上游自报 output_tokens > 0 时一律不算空完成：上游说它产出了 token，我们却数不到正文，
//     属于协议差异而非故障，误判会把正常请求打成错误。
type upstreamCompletionAudit struct {
	visibleBytes int
	toolUses     int
	thinkingSeen bool
	stopReason   string
}

// observeAnthropicSSE 喂入一行 Anthropic SSE 的 data 负载（不含 "data:" 前缀）。
func (a *upstreamCompletionAudit) observeAnthropicSSE(data string) {
	if a == nil || data == "" || data == "[DONE]" {
		return
	}
	switch gjson.Get(data, "type").String() {
	case "message_start":
		if sr := gjson.Get(data, "message.stop_reason"); sr.Exists() && sr.String() != "" {
			a.stopReason = sr.String()
		}
	case "message_delta":
		if sr := gjson.Get(data, "delta.stop_reason"); sr.Exists() && sr.String() != "" {
			a.stopReason = sr.String()
		}
	case "content_block_start":
		switch gjson.Get(data, "content_block.type").String() {
		case "text":
			a.visibleBytes += len(gjson.Get(data, "content_block.text").String())
		case "tool_use":
			a.toolUses++
		case "image", "document":
			a.visibleBytes++
		case "thinking", "redacted_thinking":
			a.thinkingSeen = true
		}
	case "content_block_delta":
		switch gjson.Get(data, "delta.type").String() {
		case "text_delta":
			a.visibleBytes += len(gjson.Get(data, "delta.text").String())
		case "input_json_delta":
			// 工具参数就是工具调用的进度：模型已经在动手，不算空回复。
			a.visibleBytes += len(gjson.Get(data, "delta.partial_json").String())
		case "thinking_delta":
			a.thinkingSeen = true
		}
	}
}

// observeAnthropicBody 喂入一段非流式 Anthropic 响应体。
func (a *upstreamCompletionAudit) observeAnthropicBody(body []byte) {
	if a == nil || len(body) == 0 {
		return
	}
	root := gjson.ParseBytes(body)
	if sr := root.Get("stop_reason"); sr.Exists() && sr.String() != "" {
		a.stopReason = sr.String()
	}
	content := root.Get("content")
	if !content.IsArray() {
		return
	}
	for _, block := range content.Array() {
		switch block.Get("type").String() {
		case "text":
			a.visibleBytes += len(block.Get("text").String())
		case "tool_use":
			a.toolUses++
		case "image", "document":
			a.visibleBytes++
		case "thinking", "redacted_thinking":
			a.thinkingSeen = true
		}
	}
}

// hasVisibleContent 表示上游确实产出了客户能看到的东西。
func (a *upstreamCompletionAudit) hasVisibleContent() bool {
	return a != nil && (a.visibleBytes > 0 || a.toolUses > 0)
}

// isEmptyCompletion 判定「上游 200 完成，但什么都没产出」。
//
// 三道护栏，缺一不可：
//   - 没有任何可呈现内容；
//   - 上游自报 output_tokens 为 0（上游说它产出了 token 就不动，见类型注释）；
//   - stop_reason 不是 refusal（模型明确拒绝时，空正文本身是合法的回答形态）。
func (a *upstreamCompletionAudit) isEmptyCompletion(usage *ClaudeUsage) bool {
	if a == nil || a.hasVisibleContent() {
		return false
	}
	if a.stopReason == "refusal" {
		return false
	}
	if usage != nil && usage.OutputTokens > 0 {
		return false
	}
	return true
}

// upstreamEmptyCompletionError 是「上游空完成」的唯一错误形态。
// 调用方按上游故障处置：账号短期停调 + 对客可解释错误（流已提交则发 SSE 错误事件）。
type upstreamEmptyCompletionError struct {
	AccountID    int64
	AccountName  string
	Model        string
	StopReason   string
	ThinkingSeen bool
}

func (e *upstreamEmptyCompletionError) Error() string {
	if e == nil {
		return "upstream completed without any visible content"
	}
	reason := e.StopReason
	if reason == "" {
		reason = "unknown"
	}
	return "upstream completed without any visible content: account=" +
		strconv.FormatInt(e.AccountID, 10) + " model=" + e.Model + " stop_reason=" + reason
}

// noteEmptyUpstreamCompletion 是「空完成」的唯一处置点。
//
// 两件事必须同时做，少一件这个判据就没有意义：
//  1. 停调该账号：这是唯一能让客户「下一次就正常」的动作。空完成是账号级故障
//     （同分组其他账号同一模型 0 次），停调后客户端重试会被调度到健康账号，
//     而不是再撞一次同一堵墙；
//  2. 打日志：没有这条日志，「空回复」在后台完全不可见——线上正是因为它既不是
//     错误码、也不进 ops 错误表，才在 12.5% 的失败率下潜伏了三天。
func (s *GatewayService) noteEmptyUpstreamCompletion(ctx context.Context, account *Account, model string, usage *ClaudeUsage, audit *upstreamCompletionAudit) error {
	if account == nil {
		return &upstreamEmptyCompletionError{Model: model}
	}
	logger.LegacyPrintf("service.gateway",
		"[upstream] empty completion: account=%d(%s) model=%s output_tokens=%d thinking=%v stop_reason=%q —— 上游返回完整终态但没有正文，停调该账号让客户端重试换号",
		account.ID, account.Name, model, outputTokensOf(usage), audit != nil && audit.thinkingSeen, stopReasonOf(audit),
	)
	if s != nil && s.accountRepo != nil {
		tempUnscheduleEmptyResponse(ctx, s.accountRepo, account.ID, "[upstream]")
	}
	return &upstreamEmptyCompletionError{
		AccountID:    account.ID,
		AccountName:  account.Name,
		Model:        model,
		StopReason:   stopReasonOf(audit),
		ThinkingSeen: audit != nil && audit.thinkingSeen,
	}
}

func outputTokensOf(usage *ClaudeUsage) int {
	if usage == nil {
		return 0
	}
	return usage.OutputTokens
}

func stopReasonOf(audit *upstreamCompletionAudit) string {
	if audit == nil {
		return ""
	}
	return strings.TrimSpace(audit.stopReason)
}
