package service

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// 国产供应商（kimi/zhipu/deepseek）的响应式冷却辅助。
//
// 与 openai/anthropic 不同：
//   - 余额不足是「可恢复」状态（充值/检测恢复后自动重新调度），不能走 handleAuthError
//     永久置 status=error。停调语义已移交额度耗尽状态机（CNQuotaLifecycleService，
//     方案 th-kira-quota-lifecycle §4.2，派发单 D-QL-002）：确认探针属实则停调至
//     官方恢复时间，探测不确定则失败关闭。旧的 2×BalanceCheckIntervalMinutes
//     滚动冷却已退役。
//   - Coding Plan 滚动窗口耗尽（429）的冷却终点应是真实的窗口重置时间（已由
//     CNProviderQuotaService 落入 account.Extra 快照），而非默认的秒级兜底。

// CNQuotaLifecycleEntry 额度耗尽状态机的响应式入口窄面（接口以
// cn_quota_lifecycle_service.go 的 CNQuotaLifecycleService.OnUpstreamQuotaExhausted
// 为准；抽象成窄面便于测试注入假实现）。生产由 app 装配层注入。
type CNQuotaLifecycleEntry interface {
	OnUpstreamQuotaExhausted(ctx context.Context, account *Account, upstreamMsg string) error
}

// SetCNQuotaLifecycle 注入额度耗尽状态机（可选依赖）。未注入时 402/429 响应式
// 入口只写信号标记、不停调（滚动冷却已退役，无兜底回退）。
func (s *RateLimitService) SetCNQuotaLifecycle(lc CNQuotaLifecycleEntry) {
	if s != nil {
		s.cnQuotaLifecycle = lc
	}
}

// cnBalanceExtraSuffixLow 标记账号响应过「余额不足」，供余额检测任务区分
// 「确属余额不足」与「尚未探测」。
const cnBalanceExtraSuffixLow = "balance_low"

// cnBalanceLowReasonPrefix 是历史余额不足临时停调 reason 的稳定前缀。
// 前缀保留仅供存量停调识别清除与 402 响应式语义；2026-10-08 用户裁定清除阈值停调
// 后，本服务不再新写入以该前缀开头的停调（仅用于识别并清除既有残留）。
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

// handleCNProviderInsufficientBalance 是 402 余额不足信号的响应式入口：
//   - 保留 402 信号识别语义与 cn_balance_low 标记写入（方案 §7：标记保留为响应式
//     信号键，供余额检测任务区分「确属余额不足」与「尚未探测」）；
//   - 停调语义移交额度耗尽状态机 OnUpstreamQuotaExhausted：确认探针属实则停调至
//     官方恢复时间（temp_unschedulable 到期=状态机给定值），探测不确定则失败关闭。
//     旧 2×BalanceCheckIntervalMinutes 滚动冷却已删除（方案 §4.2/§7）。
//   - 非 TH/Kira 账号不在状态机管辖内（OnUpstreamQuotaExhausted 内部 no-op），
//     本入口不再做任何停调。
func (s *RateLimitService) handleCNProviderInsufficientBalance(
	ctx context.Context,
	account *Account,
	upstreamMsg string,
) {
	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		cnExtraKey(account.Platform, cnBalanceExtraSuffixLow): true,
	}); err != nil {
		slog.Warn("cn_balance_low_mark_failed", "account_id", account.ID, "error", err)
	}

	if s.cnQuotaLifecycle == nil {
		slog.Warn("cn_quota_lifecycle_not_wired",
			"account_id", account.ID,
			"platform", account.Platform,
			"msg", "quota lifecycle not injected; rolling cooldown retired, no park applied",
		)
		return
	}
	if err := s.cnQuotaLifecycle.OnUpstreamQuotaExhausted(ctx, account, upstreamMsg); err != nil {
		// 停调失败（如仓库层错误）不回退滚动冷却：下轮真实流量会再次携带 402 信号
		// 进入状态机重试；余额检测周期任务也可再次触发本入口。
		slog.Warn("cn_quota_lifecycle_entry_failed",
			"account_id", account.ID,
			"platform", account.Platform,
			"error", err,
		)
	}
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

// cnNonQuota429Cooldown 非配额型 429 的固定短冷却：中转上游按分钟重置限额，
// 61 秒确保落入下一分钟窗口；忽略上游 Retry-After（其语义对本场景不可靠）。
const cnNonQuota429Cooldown = 61 * time.Second

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
	responseBody []byte,
) bool {
	if !account.IsCNProvider() {
		return false
	}
	// 1) 余额不足文案：可恢复停调，语义移交额度耗尽状态机（含智谱 payg 这类
	// 无余额端点的场景；状态机不管辖的账号只落信号标记，不再滚动停调）。
	if cnProviderResponseIndicatesInsufficientBalance(responseBody) {
		s.handleCNProviderInsufficientBalance(ctx, account, extractUpstreamErrorMessage(responseBody))
		return true
	}
	// 2) TH/Kira 账号：429 归一为额度耗尽（L2 统一口径，账号级），同口径接入
	// 额度耗尽状态机——确认探针属实则停调至官方恢复时间，不确定则失败关闭。
	// 判定与状态机同源（base_url 是唯一事实源），不能用 platform。
	if cnQuotaLifecycleProviderOf(account) != "" {
		if s.cnQuotaLifecycle == nil {
			slog.Warn("cn_quota_lifecycle_not_wired",
				"account_id", account.ID,
				"platform", account.Platform,
				"msg", "reactive 429 for lifecycle-managed account dropped; lifecycle not injected",
			)
			return true
		}
		if err := s.cnQuotaLifecycle.OnUpstreamQuotaExhausted(ctx, account, extractUpstreamErrorMessage(responseBody)); err != nil {
			slog.Warn("cn_quota_lifecycle_entry_failed",
				"account_id", account.ID,
				"platform", account.Platform,
				"error", err,
			)
		}
		return true
	}
	// 3) 额度判定直接读官方用量快照（与前端「用量窗口」同一数据源），不看响应
	// 文案——文案措辞不可靠（火山把瞬时过载也写成 429），官方用量百分比才是权威：
	//    - 任一窗口触顶（≥99%）→ 窗口耗尽，冷却到该窗口重置点
	//    - 否则（余量充足或快照缺失）→ 固定 61 秒短冷却（中转上游按分钟重置，
	//      忽略 Retry-After）；真实耗尽由周期额度探测的阈值停调接管
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
