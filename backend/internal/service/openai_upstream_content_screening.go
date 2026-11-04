package service

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// 上游「内容安全审核」拒绝的下游中文化（2026-11-04 用户裁定）。
//
// 背景：上游（商汤/DeepSeek 等）对长上下文或敏感内容会回 400，正文是英文话术：
//
//	400 {"error":{"code":"9","message":"safety check failed","type":"failed_precondition_error"}}
//	400 {"error":{"message":"Inappropriate input/output rejected for security reasons","type":"invalid_request_error"}}
//
// 生产实测（ops_error_logs，近 7 天）这两类合计 1643 次。其中 "safety check failed" 已被
// isOpenAIUpstreamSafetyRejection 纳入换号（多数换号后成功，用户无感）；但
// "Inappropriate input/output rejected ..." 没有被识别，会**把英文原文原样甩给用户**，
// 用户既看不懂也不知道该怎么办。
//
// 处置分两层，缺一不可：
//  1. 识别 → 纳入换号（同一条请求换账号常能过，这是账号/上游审核档位问题，不是客户端参数错）；
//  2. 换号耗尽 → 用中文终态文案，绝不透传英文。
const openAIContentScreeningClientMessage = "内容触发了上游安全审核，已被拒绝处理。请调整或缩短内容后重试。"

// OpenAIContentScreeningClientMessage 返回上游内容审核拒绝耗尽后的中文终态文案。
func OpenAIContentScreeningClientMessage() string {
	return openAIContentScreeningClientMessage
}

// openAIContentScreeningMarkers 是各家上游审核拒绝的固定话术（全部小写比较）。
//
// 只认**可枚举的固定话术**，不从自由文本里猜词：上游常把请求原文回显在错误里，
// 泛化匹配会把「用户问了一句为什么被拒绝」误判成审核拒绝，进而错误换号。
var openAIContentScreeningMarkers = []string{
	"safety check failed",
	"inappropriate input/output rejected",
	"rejected for security reasons",
	"content_policy_violation",
	"content policy violation",
	"blocked by content moderation",
}

// IsOpenAIContentScreeningRejection 判定上游 400 是否为内容安全审核拒绝。
//
// msg 是上游错误消息（已在调用方解析），body 是原始响应体（可为空）。
func IsOpenAIContentScreeningRejection(statusCode int, msg string, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	if containsContentScreeningMarker(msg) {
		return true
	}
	if len(body) == 0 {
		return false
	}
	// 结构化字段优先，不扫整包：避免把上游回显的请求内容误判成审核话术。
	for _, path := range []string{"error.message", "response.error.message", "message"} {
		if containsContentScreeningMarker(gjson.GetBytes(body, path).String()) {
			return true
		}
	}
	// 非 JSON 纯文本体才扫全文（与 isOpenAIUpstreamSafetyRejection 同口径）。
	if !gjson.ValidBytes(body) && containsContentScreeningMarker(string(body)) {
		return true
	}
	return false
}

// IsOpenAIContentScreeningRejectionBody 是只拿到响应体时的便捷入口（handler 侧透传分支用）。
func IsOpenAIContentScreeningRejectionBody(statusCode int, body []byte) bool {
	if len(body) == 0 {
		return false
	}
	return IsOpenAIContentScreeningRejection(statusCode, ExtractUpstreamErrorMessage(body), body)
}

func containsContentScreeningMarker(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, marker := range openAIContentScreeningMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
