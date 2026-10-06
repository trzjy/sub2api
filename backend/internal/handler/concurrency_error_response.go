package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const statusClientClosedRequest = 499

const (
	gatewayQueueFullCode        = "gateway_queue_full"
	gatewayConcurrencyLimitCode = "gateway_concurrency_limit"
)

// renderConcurrencyLimitMessage 渲染并发超限文案（仅 user/group 维度；account 维度不得调用）。
// 规则（契约，写进代码注释以稳定验收）：
//  1. account 或非 user/group → 返回中文账号槽并发文案（infraerrors.AccountSlotConcurrencyLimit），
//     绝不渲染可配模板；
//  2. user/group 且 tmpl 为空（trim 后）→ 渲染中文默认模板 ConcurrencyDefaultTemplate（{scope}/{limit} 照常）；
//  3. {scope} → "订阅"（group）/ "用户"（user）；
//  4. {limit} 且 limit>0 → 替换为数字；limit 拿不到（含不限 0）→ 整段回退不含占位符的中文同语义串，
//     绝不出现裸 {limit}；
//  5. 其他花括号原样保留。
func renderConcurrencyLimitMessage(tmpl string, kind string, limit int) string {
	// account 维度（或非 user/group）→ 中文账号槽并发文案，不渲染可配模板。
	if kind != "user" && kind != "group" {
		return infraerrors.AccountSlotConcurrencyLimit
	}
	if strings.TrimSpace(tmpl) == "" {
		// 未配置可配文案 → 渲染中文默认模板（{scope}/{limit} 占位符照常）。
		return renderConcurrencyDefault(kind, limit)
	}
	scopeWord := "用户"
	if kind == "group" {
		scopeWord = "订阅"
	}
	msg := strings.ReplaceAll(tmpl, "{scope}", scopeWord)
	if limit > 0 {
		return strings.ReplaceAll(msg, "{limit}", strconv.Itoa(limit))
	}
	// 拿不到上限值（含不限 0）→ 回退中文同语义串，不出裸 {limit}（已核实不可达，防御性）。
	return infraerrors.ConcurrencyLimitReachedNoLimit
}

// renderConcurrencyDefault 渲染未配置可配文案时的中文默认并发文案。
// limit>0 时填入 {scope}/{limit}；limit 拿不到（含不限 0，已核实不可达）时回退不含占位符的同语义串。
func renderConcurrencyDefault(kind string, limit int) string {
	scopeWord := "用户"
	if kind == "group" {
		scopeWord = "订阅"
	}
	if limit > 0 {
		msg := strings.ReplaceAll(infraerrors.ConcurrencyDefaultTemplate, "{scope}", scopeWord)
		return strings.ReplaceAll(msg, "{limit}", strconv.Itoa(limit))
	}
	return infraerrors.ConcurrencyLimitReachedNoLimit
}

func concurrencyErrorResponse(err error, slotType string, concurrencyLimitMessage string) (int, string, string, string) {
	var waitQueueFullErr *WaitQueueFullError
	if errors.As(err, &waitQueueFullErr) {
		return http.StatusTooManyRequests, "rate_limit_error", gatewayQueueFullCode,
			infraerrors.GatewayQueueFull
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
		// account 维度（或非 user/group、或 user/group 未配置可配文案）→ 同走中文语义，不残留英文。
		return http.StatusTooManyRequests, "rate_limit_error", gatewayConcurrencyLimitCode,
			renderConcurrencyLimitMessage(concurrencyLimitMessage, slotType, concurrencyErr.Limit)
	}

	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest, "api_error", "", "context canceled"
	}

	return http.StatusServiceUnavailable, "api_error", "", infraerrors.ConcurrencyFallback
}
