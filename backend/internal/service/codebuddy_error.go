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
	// CodeBuddyErrKindRequestBody 请求体/模型问题（400 + 11101/11102/11128 / Unmarshal），不罚账号。
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

// ClassifyCodeBuddyError 按 §2.6 严格表序（不可调换）对上游错误分类：
//
//	1 余额耗尽 → 2 session 死亡 → 3 模型级限流(429+6004) → 4 账号级软限流
//	→ 5 上游故障(404/5xx) → 6 内容审核 → 7 请求体/模型问题(11101/11102/11128)。
//
// 关键顺序约束（验收要求，已按方案负责人裁决改回严格表序）：
//   - ModelLimit（429+6004）必须先于 AccountSoftLimit（行 4 之前）：6004 的 body 通常也含
//     限流文案，先判 AccountSoftLimit 会把模型级限流误判为账号级冷却。
//   - SessionDead 必须先于 AccountSoftLimit：12153 与 rate-limit 文案混排时宁可判死，
//     不留死号在池里反复被选。
//   - AccountSoftLimit（软限流文案/裸 429）优先于 UpstreamFault 的状态码判定，以覆盖
//     「5xx + 限流文案」场景（参照实现 issue #28 实战结论）。
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

	// 4. 账号级软限流（429 或 rate-limit 文案，覆盖 200+code 11140 等非 429 限流语义）。
	//    软限流文案优先于状态码判定，覆盖「5xx + 限流文案」场景（参照实现 issue #28 实战结论）。
	if statusCode == http.StatusTooManyRequests ||
		codeBuddyBodyContains(bodyLower,
			"rate limit",
			"rate-limiting",
			"请求过于频繁",
			"too many") {
		return CodeBuddyErrKindAccountSoftLimit
	}

	// 5. 上游偶发/故障（404 / 5xx）。位于行 4 账号级软限流之后判定：若 5xx 响应体含限流
	//    文案，已在行 4 被归类为账号级软限流冷却，故此处仅处理「纯故障」5xx（无软限流文案），
	//    按故障重试而不罚账号（覆盖「5xx + 限流文案」→ 软限流的实战语义，参照 issue #28）。
	if statusCode == http.StatusNotFound ||
		(statusCode >= 500 && statusCode <= 599) {
		return CodeBuddyErrKindUpstreamFault
	}

	// 6. 内容审核拦截（400 + 审核关键词）。不罚账号，走 sanitize 降级重试。
	if statusCode == http.StatusBadRequest &&
		codeBuddyBodyContains(bodyLower,
			"blocked by security policy",
			"unapproved channel",
			"illegal api invocation") {
		return CodeBuddyErrKindContentAudit
	}

	// 7. 请求体/模型问题（400 + 11101 / 11102 / 11128 / 11133 / Unmarshal）。不罚账号，仍轮转。
	//    Phase 0 校准新增（D7，见 docs/evidence/codebuddy-intl）：
	//      - 11102 `model [X] service info not found`：模型无效/无权限；
	//      - 11128 `first message is not system prompt`：首条消息必须为 system；
	//      - 11133 `the request parameters were rejected by the model provider`：
	//        请求参数被模型提供方拒绝（上游 extError.code=400002）。与 11101 同族，
	//        属于客户端请求体/参数问题，不罚账号（实测：codex-tui /responses 请求
	//        携带 metadata、tools 等参数时上游返回该码，同账号其它请求正常）。
	if statusCode == http.StatusBadRequest &&
		(codeBuddyBodyHasCode(body, 11101) ||
			codeBuddyBodyHasCode(body, 11102) ||
			codeBuddyBodyHasCode(body, 11128) ||
			codeBuddyBodyHasCode(body, 11133) ||
			strings.Contains(bodyLower, "unmarshal chat params failed")) {
		return CodeBuddyErrKindRequestBody
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

// handleCodeBuddyModelLimit 将 429+6004 模型级限流按模型维度冷却：仅排除该模型，
// 其它模型仍正常调度（满足方案 §2.6 行 3 语义 + PR3 task 1.5 裁定）。
//
// 实现：写入现成 model_rate_limits[model] 结构（与 antigravity/anthropic fable 同源），
// 调度器的 IsSchedulableForModel 据此仅对该模型返回不可调度，而账号整体保持 active。
// 同时保留可读的 codebuddy_model_limit 标记做可观测。不再做账号级 SetTempUnschedulable
// （PR2 遗留做法会误杀整账号，违背「仅该模型跳过」）。
func (s *RateLimitService) handleCodeBuddyModelLimit(ctx context.Context, account *Account, model string, until time.Time) {
	if model == "" {
		// 缺少模型信息无法按模型排除，退化为账号级短冷却，避免死号被反复选。
		s.handle429(ctx, account, nil, nil)
		return
	}
	reason := "CodeBuddy 模型级限流 (模型=" + model + ") 冷却至 " + until.UTC().Format(time.RFC3339)
	if err := s.accountRepo.SetModelRateLimit(ctx, account.ID, model, until, reason); err != nil {
		slog.Warn("codebuddy_model_limit_set_failed", "account_id", account.ID, "model", model, "error", err)
		return
	}
	// 保留可读标记做可观测（非调度依据）。
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		"codebuddy_model_limit": model + "@" + until.UTC().Format(time.RFC3339),
	}); err != nil {
		slog.Warn("codebuddy_model_limit_mark_failed", "account_id", account.ID, "error", err)
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
