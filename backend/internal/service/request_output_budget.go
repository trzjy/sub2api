package service

import (
	"fmt"

	"github.com/tidwall/gjson"
)

// 输出预算准入（Output Budget Admission）——出站前的 max_tokens 上限与
// context 长度预检。
//
// 生产事故（2026-10）：客户端 api_key=402 / group 57 / okhttp / deepseek-v4.1-flash
// 发送哨兵值 max_tokens=999999999（65/65 条该文案样本的 output 项全是该值），
// 上游按「prompt + 预留输出」判超 100 万 context → 400 → 网关包成 502，24h 603 条。
// 根因是客户端把 max_tokens 当「不限」用；请求体大小上限（256MB）挡不住一个数字字段。
//
// 本文件是唯一的判定实现（SSOT），由 server/middleware 的
// RequestOutputBudgetGuard 挂一次覆盖全部网关入口，避免各 handler 各写一遍 clamp。

const (
	// DefaultMaxOutputTokensCeiling 是 max_tokens 的全局硬上限。
	//
	// 取值依据（本仓库内的真实客户端/上游证据，非拍脑袋）：
	//   - Claude Code CLI 的真实默认值是 128000，见
	//     gateway_claude_oauth_body.go:274-277（缺 max_tokens 时补 128000 以对齐指纹）。
	//     这是本仓库里唯一有据可查的「合法大值」客户端，上限必须容得下它。
	//   - antigravity 平台 clamp 到 64000（antigravity_gateway_compat.go:33），
	//     Ollama Cloud 上游硬上限 65535（openai_gateway_ollama_cloud_max_tokens.go:23，
	//     实测上游报 "max_tokens (256000) exceeds model's maximum output tokens (65536)"）。
	//     二者都是平台侧更严的 provider 级上限，由各自既有 clamp 负责，本上限不与之争。
	//   - 事故值 999999999 比任何真实上限大 4 个数量级。
	// 取 2 的幂 131072（128K）：既完整容纳 Claude Code 的 128000，又把哨兵值
	// 挡在门外，且为上游偶发上调留一倍余量。
	DefaultMaxOutputTokensCeiling int64 = 131072

	// DefaultModelContextWindow 是 context 预检使用的默认模型上下文窗口。
	//
	// 取值依据：openai_codex_models_service.go:373 的
	// configuredCodexDeepSeekV4Context = 1_000_000 —— 与事故上游文案
	// 「按 prompt + 预留输出判超 100 万 context」完全一致，是本仓库对该事故模型
	// 声明的 context 上限。同文件另有 fallback 272_000 / grok 500_000 /
	// gpt-5.6 872_000 / astra 1_050_000，均为 Codex 清单专用口径，不宜当通用默认。
	// 这里只做「预留输出是否会独自撑爆窗口」的保守预检，默认取事故模型口径。
	DefaultModelContextWindow int64 = 1_000_000
)

// RequestOutputTokenFields 是各协议承载「最大输出 token」的字段的 gjson 路径。
// Chat Completions 用 max_tokens / max_completion_tokens，Responses 用
// max_output_tokens，Anthropic Messages 用 max_tokens，Gemini 原生用
// generationConfig.maxOutputTokens。四者取最大值判定，避免只盯一个字段导致
// 换字段名绕过。
//
// Gemini 的字段是**嵌套**路径而非顶层：原生 :generateContent 请求体形如
// {"contents":[...],"generationConfig":{"maxOutputTokens":N}}，实测顶层
// gjson 路径取不到（顶层写 "maxOutputTokens" 永远 exists=false）。gjson 原生
// 支持点号路径，故直接写全路径即可，无需改判定逻辑。
var RequestOutputTokenFields = []string{
	"max_tokens",
	"max_completion_tokens",
	"max_output_tokens",
	"generationConfig.maxOutputTokens",
}

// RequestOutputBudgetVerdict 描述一次输出预算预检的结论。
type RequestOutputBudgetVerdict struct {
	// Rejected 为 true 时必须拒绝出站。
	Rejected bool
	// Reason 面向用户（不含内部细节：上游名/分组/账号等一律不出现在这里）。
	Reason string
	// Field 触发拒绝的字段名，仅用于日志。
	Field string
	// Requested 客户端书写的值。
	Requested int64
	// Limit 生效的上限（clamp 后的值，或 context 窗口）。
	Limit int64
}

// RequestOutputBudgetLimits 是预检阈值。零值/负值表示该项检查关闭。
type RequestOutputBudgetLimits struct {
	MaxOutputTokens int64
	ContextWindow   int64
}

// RequestOutputBudgetLimitsOrDefault 在配置缺失时回落到包级默认值，
// 保证「配置没写」等价于「用文档化的默认值」，而不是「不做检查」。
func RequestOutputBudgetLimitsOrDefault(maxOutputTokens, contextWindow int64) RequestOutputBudgetLimits {
	if maxOutputTokens <= 0 {
		maxOutputTokens = DefaultMaxOutputTokensCeiling
	}
	if contextWindow <= 0 {
		contextWindow = DefaultModelContextWindow
	}
	return RequestOutputBudgetLimits{MaxOutputTokens: maxOutputTokens, ContextWindow: contextWindow}
}

// CheckRequestOutputBudget 在请求出站前做两件事：
//
//  1. max_tokens 硬上限：超过 limits.MaxOutputTokens 直接拒绝。事故的哨兵值
//     （999999999）在这一步就被拦下，不需要付出 token 估算的代价。
//  2. context 长度预检：input 估算 + max_tokens 超过 limits.ContextWindow 时拒绝。
//     第 2 步需要 tokenize 整个请求体（CPU 敏感），因此先做字节上界短路：
//     一个 token 至少占 1 字节，故 len(body) 是 input token 数的上界；上界都
//     装不下时必然不超窗，直接放行，不调用 tokenizer。
//
// 估算失败（解析不了/找不到 codec/协议未知）时**不拒绝**：本函数是准入护栏而非路由决策，
// 估算器失败不应把本来合法的请求挡在门外；而第 1 步的 max_tokens 上限已经独立
// 兜住了事故形态。protocol 为空串（Gemini 原生链无估算器分支）同样只跳到这一步
// 的 fail-open，不影响第 1 步。
func CheckRequestOutputBudget(model string, protocol TierRoutingRequestProtocol, body []byte, limits RequestOutputBudgetLimits) RequestOutputBudgetVerdict {
	field, requested, ok := MaxRequestedOutputTokens(body)
	if !ok {
		// 客户端没写 max_tokens：不改变其行为（上游按各自默认处理）。
		return RequestOutputBudgetVerdict{}
	}

	if limits.MaxOutputTokens > 0 && requested > limits.MaxOutputTokens {
		return RequestOutputBudgetVerdict{
			Rejected:  true,
			Field:     field,
			Requested: requested,
			Limit:     limits.MaxOutputTokens,
			Reason: fmt.Sprintf(
				"%s is %d, which exceeds the maximum supported value %d. Set %s to %d or lower.",
				field, requested, limits.MaxOutputTokens, field, limits.MaxOutputTokens,
			),
		}
	}

	if limits.ContextWindow <= 0 {
		return RequestOutputBudgetVerdict{}
	}

	// 字节上界短路：len(body) 是 input token 数的上界（1 token >= 1 byte）。
	// 上界 + 预留输出都放得进窗口，则真实 input 必然也放得进。
	if int64(len(body))+requested <= limits.ContextWindow {
		return RequestOutputBudgetVerdict{}
	}

	estimated, err := EstimateTierRoutingInputTokens(model, protocol, body)
	if err != nil {
		return RequestOutputBudgetVerdict{}
	}
	if int64(estimated)+requested > limits.ContextWindow {
		return RequestOutputBudgetVerdict{
			Rejected:  true,
			Field:     field,
			Requested: requested,
			Limit:     limits.ContextWindow,
			Reason: fmt.Sprintf(
				"This request is too long for the model: about %d input tokens plus %s=%d exceeds the %d token context limit. Shorten the input or lower %s.",
				estimated, field, requested, limits.ContextWindow, field,
			),
		}
	}
	return RequestOutputBudgetVerdict{}
}

// MaxRequestedOutputTokens 返回请求体中生效的最大输出 token 申请值。
// 三种协议字段任一存在即取其最大值（客户端换字段名不能绕过上限）；
// 多个字段同时存在时取最大的那个，与「谁生效取谁」的上游行为一致地保守。
func MaxRequestedOutputTokens(body []byte) (field string, value int64, ok bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return "", 0, false
	}
	for _, name := range RequestOutputTokenFields {
		result := gjson.GetBytes(body, name)
		if !result.Exists() || result.Type != gjson.Number {
			continue
		}
		n := result.Int()
		if n < 0 {
			// 负数不是「申请更多输出」，交给上游按非法参数处理，本层不改行为。
			continue
		}
		if !ok || n > value {
			field, value, ok = name, n, true
		}
	}
	return field, value, ok
}
