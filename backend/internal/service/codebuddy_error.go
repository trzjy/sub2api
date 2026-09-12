package service

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// CodeBuddyErrKind 是 §2.6 错误分类的结果。分类顺序不可调换（见 ClassifyCodeBuddyError）。
type CodeBuddyErrKind int

const (
	// CodeBuddyErrKindNone 未匹配到已知错误模式（交由通用错误处理）。
	CodeBuddyErrKindNone CodeBuddyErrKind = iota
	// CodeBuddyErrKindBalanceExhausted 余额/积分耗尽（402 或业务文案）。
	CodeBuddyErrKindBalanceExhausted
	// CodeBuddyErrKindSessionDead 会话失效（12153 / Offline user session not found），需重新 OAuth。
	CodeBuddyErrKindSessionDead
	// CodeBuddyErrKindModelLimit 模型级限流（429 + code 6004），按模型冷却，不罚账号。
	CodeBuddyErrKindModelLimit
	// CodeBuddyErrKindUpstreamFault 上游偶发/故障（404 / 5xx）。
	CodeBuddyErrKindUpstreamFault
	// CodeBuddyErrKindContentAudit 内容审核拦截（400 + 审核关键词），不罚账号，sanitize 降级重试。
	CodeBuddyErrKindContentAudit
	// CodeBuddyErrKindRequestBody 请求体问题（400 + 11101 / Unmarshal），不罚账号。
	CodeBuddyErrKindRequestBody
	// CodeBuddyErrKindAccountSoftLimit 账号级软限流（429 或 rate-limit 文案）。
	CodeBuddyErrKindAccountSoftLimit
)

// String 便于日志与测试断言。
func (k CodeBuddyErrKind) String() string {
	switch k {
	case CodeBuddyErrKindBalanceExhausted:
		return "balance_exhausted"
	case CodeBuddyErrKindSessionDead:
		return "session_dead"
	case CodeBuddyErrKindModelLimit:
		return "model_limit"
	case CodeBuddyErrKindUpstreamFault:
		return "upstream_fault"
	case CodeBuddyErrKindContentAudit:
		return "content_audit"
	case CodeBuddyErrKindRequestBody:
		return "request_body"
	case CodeBuddyErrKindAccountSoftLimit:
		return "account_soft_limit"
	default:
		return "none"
	}
}

// ClassifyCodeBuddyError 按 §2.6 表格顺序（不可调换）对上游错误分类。
//
// 关键顺序约束（验收要求）：
//   - ModelLimit（429+6004）必须先于 AccountSoftLimit：6004 的 body 通常也含限流文案，
//     先判 AccountSoftLimit 会把模型级限流误判为账号级冷却。
//   - SessionDead 必须先于 AccountSoftLimit：12153 与 rate-limit 文案混排时宁可判死，
//     不留死号在池里反复被选。
//   - ContentAudit / RequestBody（不罚账号的误报信号）先于 AccountSoftLimit 这种会罚账号的判定。
func ClassifyCodeBuddyError(statusCode int, body []byte) CodeBuddyErrKind {
	bodyLower := strings.ToLower(string(body))

	// 1. 余额/积分耗尽（最高优先级：最不可自愈，最先判）。
	if statusCode == http.StatusPaymentRequired ||
		codeBuddyBodyIndicatesInsufficientBalance(bodyLower) {
		return CodeBuddyErrKindBalanceExhausted
	}

	// 2. 会话失效（12153 / Offline user session not found）。
	if strings.Contains(bodyLower, "12153") ||
		strings.Contains(bodyLower, "offline user session not found") {
		return CodeBuddyErrKindSessionDead
	}

	// 3. 模型级限流（429 + code 6004）。必须先于行 7（账号级软限流）。
	if statusCode == http.StatusTooManyRequests && codeBuddyBodyHasCode(body, 6004) {
		return CodeBuddyErrKindModelLimit
	}

	// 4. 上游偶发/故障（404 / 5xx）。5xx 即便含限流文案也应视为故障重试，而非账号冷却。
	if statusCode == http.StatusNotFound ||
		(statusCode >= 500 && statusCode <= 599) {
		return CodeBuddyErrKindUpstreamFault
	}

	// 5. 内容审核拦截（400 + 审核关键词）。不罚账号，走 sanitize 降级重试。
	if statusCode == http.StatusBadRequest &&
		codeBuddyBodyContains(bodyLower,
			"blocked by security policy",
			"unapproved channel",
			"illegal api invocation") {
		return CodeBuddyErrKindContentAudit
	}

	// 6. 请求体问题（400 + 11101 / Unmarshal）。不罚账号，仍轮转。
	if statusCode == http.StatusBadRequest &&
		(codeBuddyBodyHasCode(body, 11101) ||
			strings.Contains(bodyLower, "unmarshal chat params failed")) {
		return CodeBuddyErrKindRequestBody
	}

	// 7. 账号级软限流（429 或 rate-limit 文案，覆盖 200+code 11140 等非 429 限流语义）。
	if statusCode == http.StatusTooManyRequests ||
		codeBuddyBodyContains(bodyLower,
			"rate limit",
			"rate-limiting",
			"请求过于频繁",
			"too many") {
		return CodeBuddyErrKindAccountSoftLimit
	}

	return CodeBuddyErrKindNone
}

// codeBuddyBodyIndicatesInsufficientBalance 通过响应体文案识别余额/积分不足。
func codeBuddyBodyIndicatesInsufficientBalance(bodyLower string) bool {
	return strings.Contains(bodyLower, "积分不足") ||
		strings.Contains(bodyLower, "余额不足") ||
		strings.Contains(bodyLower, "insufficient credit") ||
		strings.Contains(bodyLower, "insufficient balance") ||
		strings.Contains(bodyLower, "balance is not enough") ||
		strings.Contains(bodyLower, "no enough balance")
}

// codeBuddyBodyContains 命中任一子串即返回 true。
func codeBuddyBodyContains(bodyLower string, phrases ...string) bool {
	for _, p := range phrases {
		if p == "" {
			continue
		}
		if strings.Contains(bodyLower, p) {
			return true
		}
	}
	return false
}

// codeBuddyBodyHasCode 判断响应体 JSON 的顶层 "code" 字段是否等于给定整数。
// CodeBuddy 业务信封为 {code:int, msg, data}，非 JSON 或缺失时返回 false。
func codeBuddyBodyHasCode(body []byte, code int) bool {
	c := gjson.GetBytes(body, "code")
	if !c.Exists() {
		return false
	}
	switch c.Type {
	case gjson.Number:
		n, err := strconv.Atoi(strings.TrimSpace(c.Raw))
		if err != nil {
			return false
		}
		return n == code
	case gjson.String:
		n, err := strconv.Atoi(strings.TrimSpace(c.String()))
		if err != nil {
			return false
		}
		return n == code
	default:
		return false
	}
}

// codeBuddyResetTimeLayout 是 §2.6 行 3 中重置提示的时间格式（如「将在 2026-09-13 12:00:00 重置」）。
const codeBuddyResetTimeLayout = "2006-01-02 15:04:05"

// codeBuddyResetTimeLocation 是上游重置时间的时区（固定 UTC+8，与容器时区无关）。
var codeBuddyResetTimeLocation = time.FixedZone("UTC+8", 8*3600)

// handleCodeBuddyInsufficientBalance 将 CodeBuddy 余额/积分耗尽标记为可恢复的临时停调
// （对齐 CN 供应商：写入余额低位标记 + SetTempUnschedulable，由周期性配额探测恢复）。
func (s *RateLimitService) handleCodeBuddyInsufficientBalance(ctx context.Context, account *Account, upstreamMsg string) {
	msg := "CodeBuddy 余额/积分耗尽: " + upstreamMsg
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		"codebuddy_balance_low": true,
	}); err != nil {
		slog.Warn("codebuddy_balance_low_mark_failed", "account_id", account.ID, "error", err)
	}
	until := time.Now().Add(time.Duration(codeBuddyBalanceCooldownMinutes) * time.Minute)
	s.notifyAccountSchedulingBlocked(account, until, "codebuddy_insufficient_balance")
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, msg); err != nil {
		slog.Warn("codebuddy_balance_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("codebuddy_insufficient_balance", "account_id", account.ID, "until", until.UTC())
}

// handleCodeBuddySessionDead 将 12153 / Offline user session not found 视为会话失效：
// 账号置 error（需重新 OAuth），并通知调度阻塞。
func (s *RateLimitService) handleCodeBuddySessionDead(ctx context.Context, account *Account, upstreamMsg string) {
	msg := "CodeBuddy 会话失效 (需重新 OAuth): " + upstreamMsg
	until := time.Now().Add(24 * time.Hour)
	s.notifyAccountSchedulingBlocked(account, until, "codebuddy_session_dead")
	// handleAuthError 将账号置永久 error（status=error），由后台刷新/重新 OAuth 恢复。
	s.handleAuthError(ctx, account, msg)
}

// handleCodeBuddyModelLimit 将 429+6004 模型级限流按模型维度冷却：临时停调至上游重置时刻。
//
// 说明：当前以「账号级临时停调 + 模型标记」实现「不罚账号」（status 保持 active，可恢复），
// 而非永久 error/disable。真正的按模型排除调度（仅该模型跳过、其它模型仍可用）需要调度器
// 消费 account.Extra["codebuddy_model_limit"]，属后续增强；此处先保证不把账号整体误杀。
func (s *RateLimitService) handleCodeBuddyModelLimit(ctx context.Context, account *Account, model string, until time.Time) {
	msg := "CodeBuddy 模型级限流 (模型=" + model + ") 冷却至 " + until.UTC().Format(time.RFC3339)
	if model != "" {
		if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
			"codebuddy_model_limit": model + "@" + until.UTC().Format(time.RFC3339),
		}); err != nil {
			slog.Warn("codebuddy_model_limit_mark_failed", "account_id", account.ID, "error", err)
		}
	}
	s.notifyAccountSchedulingBlocked(account, until, "codebuddy_model_limit")
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, msg); err != nil {
		slog.Warn("codebuddy_model_limit_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("codebuddy_model_limit", "account_id", account.ID, "model", model, "until", until.UTC())
}

// parseCodeBuddyResetTime 从 body 中解析「将在 YYYY-MM-DD HH:mm:ss 重置」并固定按 UTC+8 解释。
// 返回 (时间, true) 表示解析成功。
func parseCodeBuddyResetTime(body []byte) (time.Time, bool) {
	s := string(body)
	idx := strings.Index(s, "将在")
	if idx < 0 {
		return time.Time{}, false
	}
	rest := s[idx:]
	// 截取「将在」之后、到「 重置」结束的时间串。
	end := strings.Index(rest, " 重置")
	if end < 0 {
		// 兼容无空格写法「重置」。
		end = strings.Index(rest, "重置")
	}
	if end < 0 {
		return time.Time{}, false
	}
	timeStr := strings.TrimSpace(rest[len("将在"):end])
	if timeStr == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(codeBuddyResetTimeLayout, timeStr, codeBuddyResetTimeLocation)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
