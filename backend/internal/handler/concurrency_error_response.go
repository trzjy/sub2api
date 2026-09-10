package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const statusClientClosedRequest = 499

const (
	gatewayQueueFullCode        = "gateway_queue_full"
	gatewayConcurrencyLimitCode = "gateway_concurrency_limit"
)

// renderConcurrencyLimitMessage 渲染并发超限文案（仅 user/group 维度；account 维度不得调用）。
// 规则（契约，写进代码注释以稳定验收）：
//  1. account 或非 user/group → 返回英文默认，不渲染本文案；
//  2. tmpl 为空（trim 后）→ 英文默认，与改动前逐字节一致；
//  3. {scope} → "订阅"（group）/ "用户"（user）；
//  4. {limit} 且 limit>0 → 替换为数字；limit 拿不到（含不限） → 整段回退英文，绝不出现裸 {limit}；
//  5. 其他花括号原样保留。
func renderConcurrencyLimitMessage(tmpl string, kind string, limit int) string {
	defaultMsg := fmt.Sprintf("Concurrency limit exceeded for %s, please retry later", kind)
	if kind != "user" && kind != "group" {
		return defaultMsg
	}
	if strings.TrimSpace(tmpl) == "" {
		return defaultMsg
	}
	scopeWord := "用户"
	if kind == "group" {
		scopeWord = "订阅"
	}
	msg := strings.ReplaceAll(tmpl, "{scope}", scopeWord)
	if limit > 0 {
		return strings.ReplaceAll(msg, "{limit}", strconv.Itoa(limit))
	}
	// 拿不到上限值（含不限 0）→ 回退英文，不出裸 {limit}
	return defaultMsg
}

func concurrencyErrorResponse(err error, slotType string, concurrencyLimitMessage string) (int, string, string, string) {
	var waitQueueFullErr *WaitQueueFullError
	if errors.As(err, &waitQueueFullErr) {
		return http.StatusTooManyRequests, "rate_limit_error", gatewayQueueFullCode,
			"Too many pending requests, please retry later"
	}

	var concurrencyErr *ConcurrencyError
	if errors.As(err, &concurrencyErr) {
		if concurrencyErr.SlotType != "" {
			slotType = concurrencyErr.SlotType
		}
		// account 维度保持英文，绝不套用本文案；仅 user/group 渲染可配文案。
		if (slotType == "user" || slotType == "group") && strings.TrimSpace(concurrencyLimitMessage) != "" {
			return http.StatusTooManyRequests, "rate_limit_error", gatewayConcurrencyLimitCode,
				renderConcurrencyLimitMessage(concurrencyLimitMessage, slotType, concurrencyErr.Limit)
		}
		return http.StatusTooManyRequests, "rate_limit_error", gatewayConcurrencyLimitCode,
			fmt.Sprintf("Concurrency limit exceeded for %s, please retry later", slotType)
	}

	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest, "api_error", "", "context canceled"
	}

	return http.StatusServiceUnavailable, "api_error", "", "Service temporarily unavailable, please retry later"
}
