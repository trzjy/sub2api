package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// 国产供应商（kimi/zhipu/deepseek）的响应式冷却辅助。
//
// 与 openai/anthropic 不同：
//   - 余额不足是「可恢复」状态（充值/检测恢复后自动重新调度），不能走 handleAuthError
//     永久置 status=error。这里改为 SetTempUnschedulable，由 CN 余额检测周期任务
//     （cn_provider_balance_check_service.go）在余额恢复后 ClearTempUnschedulable。
//   - Coding Plan 滚动窗口耗尽（429）的冷却终点应是真实的窗口重置时间（已由
//     CNProviderQuotaService 落入 account.Extra 快照），而非默认的秒级兜底。

// cnBalanceExtraSuffixLow 标记账号响应过「余额不足」，供余额检测任务区分
// 「确属余额不足」与「尚未探测」。
const cnBalanceExtraSuffixLow = "balance_low"

// cnBalanceLowReasonPrefix 是余额不足临时停调 reason 的稳定前缀。
// 周期余额检测任务据此识别「是我们停调的」并在余额恢复后安全清除——不会误清
// 其他子系统（阈值/限流/401）写入的临时停调。
const cnBalanceLowReasonPrefix = "cn_balance_low"

const kimiConcurrentRequestLimitMessage = "You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."

const cnConcurrencyLimitReasonPrefix = "cn_concurrency_limit"

func isCNProviderConcurrencyLimit403(account *Account, upstreamMsg string) bool {
	return account != nil && account.Platform == PlatformKimi &&
		strings.TrimSpace(upstreamMsg) == kimiConcurrentRequestLimitMessage
}

func (s *RateLimitService) handleCNProviderConcurrencyLimit403(
	ctx context.Context,
	account *Account,
) {
	until := time.Now().Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
	reason := cnConcurrencyLimitReasonPrefix + ": " + kimiConcurrentRequestLimitMessage
	s.notifyAccountSchedulingBlocked(account, until, cnConcurrencyLimitReasonPrefix)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("cn_concurrency_limit_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("cn_provider_concurrency_limited",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}

// cnBalanceLowReason 构造余额不足临时停调的 reason（带稳定前缀）。
func cnBalanceLowReason(upstreamMsg string) string {
	if upstreamMsg = strings.TrimSpace(upstreamMsg); upstreamMsg != "" {
		return cnBalanceLowReasonPrefix + ": " + upstreamMsg
	}
	return cnBalanceLowReasonPrefix + ": 余额不足，账号临时停调"
}

// cnProviderResponseIndicatesInsufficientBalance 通过响应体文案识别余额不足
// （智谱 payg 无独立余额端点，仅能靠响应文案识别）。
func cnProviderResponseIndicatesInsufficientBalance(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "余额不足") ||
		strings.Contains(s, "insufficient balance") ||
		strings.Contains(s, "insufficient_credit") ||
		strings.Contains(s, "balance is not enough") ||
		strings.Contains(s, "no enough balance")
}

// handleCNProviderInsufficientBalance 把余额不足标记为可恢复的临时停调：
// 写入 balance_low 快照 + SetTempUnschedulable 一个余额检测周期，
// 由周期任务在余额恢复后清除。返回前已通知调度阻塞。
func (s *RateLimitService) handleCNProviderInsufficientBalance(
	ctx context.Context,
	account *Account,
	upstreamMsg string,
) {
	msg := cnBalanceLowReason(upstreamMsg)

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		cnExtraKey(account.Platform, cnBalanceExtraSuffixLow): true,
	}); err != nil {
		slog.Warn("cn_balance_low_mark_failed", "account_id", account.ID, "error", err)
	}

	until := time.Now().Add(s.cnBalanceCooldownDuration())
	s.notifyAccountSchedulingBlocked(account, until, "cn_insufficient_balance")
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, msg); err != nil {
		slog.Warn("cn_balance_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("cn_provider_insufficient_balance",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}

// cnBalanceCooldownDuration 返回余额不足临时停调的持续时长（= 2× 余额检测周期，
// 默认 20 分钟）。周期任务会在余额恢复后提前清除，故此处只需保证冷却覆盖到下一次
// 周期检测即可。
func (s *RateLimitService) cnBalanceCooldownDuration() time.Duration {
	minutes := 10
	if s != nil && s.cfg != nil {
		if cfgMin := s.cfg.Gateway.CNProviders.BalanceCheckIntervalMinutes; cfgMin > 0 {
			minutes = cfgMin
		}
	}
	cooldown := time.Duration(minutes) * time.Minute * 2
	if cooldown < time.Minute {
		cooldown = 10 * time.Minute
	}
	return cooldown
}

// cnProviderQuotaSnapshotReset 读取 Coding Plan 账号快照中最早一个仍在未来的窗口
// 重置时间（5h / weekly）。429 多数由 5h 滚动窗口触发，取较早的重置点可避免
// 把账号冷却到 weekly 重置（可达数天）的过度停调；如果确是 weekly 窗口耗尽，
// 周期额度探测刷新快照后阈值评估会再次停调到正确的时间点。
// 无快照或均已过期返回 nil。
func cnProviderQuotaSnapshotReset(account *Account, now time.Time) *time.Time {
	if account == nil || !account.IsCNProvider() || len(account.Extra) == 0 {
		return nil
	}
	// 快照键以 coding 供应商为前缀（kimi_/zhipu_/volcano_），与
	// cnQuotaExtraUpdates 的写入维度一致；不能用 platform（deepseek 火山号）取键。
	// 用 resolveCNQuotaProvider（兼容 payg 火山）而非 GetCodingPlanProvider，
	// 否则 payg 火山号的 429 冷却读不到 volcano_* 重置点、落入真实 5h 窗口。
	provider := resolveCNQuotaProvider(account)
	if provider == "" {
		return nil
	}
	var earliest *time.Time
	for _, suffix := range []string{cnExtraSuffix5hReset, cnExtraSuffixWeeklyReset} {
		t := parseSchedulingResetAt(account.Extra[cnExtraKey(provider, suffix)])
		if t == nil || !t.After(now) {
			continue
		}
		if earliest == nil || t.Before(*earliest) {
			earliest = t
		}
	}
	return earliest
}

// cnNonQuota429Cooldown 非配额型 429 的默认短冷却（官方语义「稍后重试」）。
const cnNonQuota429Cooldown = 90 * time.Second

// cnNonQuota429CooldownMax Retry-After 的接受上限，防止异常大值造成长禁闭。
const cnNonQuota429CooldownMax = 10 * time.Minute

// cnNonQuota429Reason 非配额型 429 临时停调 reason 的稳定前缀。
const cnNonQuota429Reason = "cn_429_short_retry: upstream 429 with quota headroom, retry shortly"

// cnQuotaSnapshotEarliestExhaustedReset 返回官方用量快照中「已触顶且仍在未来」的
// 最早窗口重置时间；无触顶窗口返回 nil。触顶判定 ≥99%（留 1% 容差防四舍五入）。
// 数据源与前端「用量窗口」一致（CNProviderQuotaService 周期探测落入 extra 的快照）。
func cnQuotaSnapshotEarliestExhaustedReset(extra map[string]any, provider string, now time.Time) *time.Time {
	if len(extra) == 0 {
		return nil
	}
	var earliest *time.Time
	for _, tier := range []struct{ used, reset string }{
		{cnExtraSuffix5hUsed, cnExtraSuffix5hReset},
		{cnExtraSuffixWeeklyUsed, cnExtraSuffixWeeklyReset},
		{cnExtraSuffixMonthlyUsed, cnExtraSuffixMonthlyReset},
	} {
		raw, ok := extra[cnExtraKey(provider, tier.used)]
		if !ok || schedulingPercentValue(raw) < 99 {
			continue
		}
		t := parseSchedulingResetAt(extra[cnExtraKey(provider, tier.reset)])
		if t == nil || !t.After(now) {
			continue
		}
		if earliest == nil || t.Before(*earliest) {
			earliest = t
		}
	}
	return earliest
}

// reconcileCNProviderRateLimits 对账账号级 429 限流（窗口耗尽路径写入）：
// 若官方配额快照显示没有任何窗口触顶，而限流重置点仍在 1 小时之外，判定为
// 「瞬时 429 被误禁闭到窗口重置」，提前解除。短冷却（<1h）自然到期，不参与。
// 快照缺失/探测失败视为无耗尽证据（宁可解除误禁闭——真实耗尽会由下次 429
// 或周期阈值停调重新接管）。
func reconcileCNProviderRateLimits(ctx context.Context, repo AccountRepository, platforms []string, now time.Time) int {
	if repo == nil {
		return 0
	}
	cleared := 0
	for _, platform := range platforms {
		accounts, err := repo.ListByPlatform(ctx, platform)
		if err != nil {
			slog.Warn("cn_rate_limit_reconcile_list_failed", "platform", platform, "error", err)
			continue
		}
		for i := range accounts {
			account := &accounts[i]
			if account.RateLimitedAt == nil || account.RateLimitResetAt == nil {
				continue
			}
			resetAt := *account.RateLimitResetAt
			if !resetAt.After(now) || resetAt.Sub(now) < time.Hour {
				continue
			}
			provider := resolveCNQuotaProvider(account)
			if provider == "" {
				continue
			}
			if cnQuotaSnapshotAnyWindowExhausted(account.Extra, provider) {
				continue
			}
			if err := repo.ClearRateLimit(ctx, account.ID); err != nil {
				slog.Warn("cn_rate_limit_reconcile_clear_failed", "account_id", account.ID, "error", err)
				continue
			}
			cleared++
			slog.Info("cn_rate_limit_reconcile_cleared",
				"account_id", account.ID,
				"platform", account.Platform,
				"was_reset_at", resetAt.UTC(),
				"reason", "quota snapshot shows no exhausted window; transient 429 mis-benched to window reset",
			)
		}
	}
	return cleared
}

// cnQuotaSnapshotAnyWindowExhausted 报告快照中是否有任一窗口用量达到耗尽水位
// （≥99%，留 1% 容差避免四舍五入误判）。
func cnQuotaSnapshotAnyWindowExhausted(extra map[string]any, provider string) bool {
	if len(extra) == 0 {
		return false
	}
	for _, suffix := range []string{cnExtraSuffix5hUsed, cnExtraSuffixWeeklyUsed, cnExtraSuffixMonthlyUsed} {
		raw, ok := extra[cnExtraKey(provider, suffix)]
		if !ok {
			continue
		}
		if schedulingPercentValue(raw) >= 99 {
			return true
		}
	}
	return false
}

// applyCNProviderReactive429 处理国产供应商的 429 响应。
// 返回 true 表示已处理（调用方应 return），false 表示未命中、继续走默认 429 逻辑。
func (s *RateLimitService) applyCNProviderReactive429(
	ctx context.Context,
	account *Account,
	headers http.Header,
	responseBody []byte,
) bool {
	if !account.IsCNProvider() {
		return false
	}
	// 1) 余额不足文案：可恢复临时停调（含智谱 payg 这类无余额端点的场景）。
	if cnProviderResponseIndicatesInsufficientBalance(responseBody) {
		s.handleCNProviderInsufficientBalance(ctx, account, extractUpstreamErrorMessage(responseBody))
		return true
	}
	// 2) 额度判定直接读官方用量快照（与前端「用量窗口」同一数据源），不看响应
	// 文案——文案措辞不可靠（火山把瞬时过载也写成 429），官方用量百分比才是权威：
	//    - 任一窗口触顶（≥99%）→ 窗口耗尽，冷却到该窗口重置点
	//    - 否则（余量充足或快照缺失）→ 短冷却稍后重试（Retry-After 优先，上限
	//      10 分钟、默认 90s）；真实耗尽由周期额度探测的阈值停调接管
	// 判定用 resolveCNQuotaProvider（兼容 payg 火山）而非 GetCodingPlanProvider，
	// 否则 payg 火山号的 429 读不到 volcano_* 快照。
	provider := resolveCNQuotaProvider(account)
	if provider != "" {
		if reset := cnQuotaSnapshotEarliestExhaustedReset(account.Extra, provider, time.Now()); reset != nil {
			s.notifyAccountSchedulingBlocked(account, *reset, "429")
			if err := s.accountRepo.SetRateLimited(ctx, account.ID, *reset); err != nil {
				slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
				return true
			}
			slog.Info("cn_provider_quota_window_rate_limited",
				"account_id", account.ID,
				"platform", account.Platform,
				"window_reset_at", *reset,
			)
			return true
		}
		until := time.Now().Add(cnNonQuota429Cooldown)
		if ra := parseRetryAfterResetTime(headers, time.Now()); ra != nil && ra.After(time.Now()) {
			until = *ra
			if max := time.Now().Add(cnNonQuota429CooldownMax); until.After(max) {
				until = max
			}
		}
		s.notifyAccountSchedulingBlocked(account, until, "429_short_retry")
		if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, cnNonQuota429Reason); err != nil {
			slog.Warn("cn_429_short_cooldown_set_failed", "account_id", account.ID, "error", err)
		}
		slog.Info("cn_provider_429_short_cooldown",
			"account_id", account.ID,
			"platform", account.Platform,
			"until", until.UTC(),
		)
		return true
	}
	return false
}
