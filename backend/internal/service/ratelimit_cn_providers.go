package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
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
// cn_quota_lifecycle_service.go 的 CNQuotaLifecycleService 为准；抽象成窄面便于测试
// 注入假实现）。生产由 app 装配层注入。
//
// 结构化信号透传（方案 §4 F2）：OnUpstreamQuotaExhaustedScoped 携带 {status, scope,
// platform} 结构化信号，使状态机按作用域矩阵执行（kira+402=account 权威冻结、其他=
// model/path 探针确认、unknown=不整号转换）。OnUpstreamQuotaExhausted 保留为兼容签名
// （派生默认作用域后转交 Scoped），供周期快照交接窄面（cn_provider_balance_check_
// service.go 的 cnQuotaLifecycleHandover）等仅持有自由文本的调用方使用。
type CNQuotaLifecycleEntry interface {
	OnUpstreamQuotaExhausted(ctx context.Context, account *Account, upstreamMsg string) error
	OnUpstreamQuotaExhaustedScoped(ctx context.Context, account *Account, upstreamMsg string, signal QuotaExhaustionSignal) error
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

// cnQuotaExhausted403ErrorType 是 Kimi Coding Plan 配额窗口耗尽的 403 错误类型。
// 与并发限制 403（见 isCNProviderConcurrencyLimit403）不同，这是 5h/weekly
// 窗口耗尽的限流信号：窗口到期后自动恢复，绝不能落入通用 403 升级计数
// （连续 3 次永久 SetError），必须按 429 口径冷却到真实窗口重置点。
const cnQuotaExhausted403ErrorType = "access_terminated_error"

// 注：cnQuotaExhaustedReasonPrefix（= "cn_quota_exhausted"）的声明归
// cn_quota_lifecycle_service.go（额度耗尽状态机的停调 reason 稳定前缀，本仓库 SSOT）；
// 本文件只引用该常量，不重复声明（避免同名常量双声明）。403 配额耗尽兜底与状态机
// 共用同一前缀，使停调可被状态机 sweep 统一识别。

// isCNProviderQuotaExhausted403 识别 CN 供应商配额窗口耗尽的 403。
// 先做廉价的文案子串匹配（usage limit / quota will reset），未命中再解析
// 响应体匹配结构化错误类型（Kimi 的 access_terminated_error），以兼容同族
// 供应商的文案变体。与并发限制文案互斥，两类信号不会误判。
// 仅限 Coding Plan 账号：滚动窗口快照（含重置时间）只有 Coding Plan 账号才有，
// 非 Coding Plan 账号的同类 403 语义不明（可能是需要升级套餐的硬上限），
// 应回退到通用 403 升级逻辑而非无限临时冷却。
func isCNProviderQuotaExhausted403(account *Account, responseBody []byte, upstreamMsg string) bool {
	if account == nil || !account.IsCNProvider() || !account.IsCodingPlan() {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(upstreamMsg))
	if strings.Contains(msg, "usage limit") || strings.Contains(msg, "quota will reset") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(responseBody, "error.type").String()), cnQuotaExhausted403ErrorType)
}

// handleCNProviderQuotaExhausted403 把 CN 供应商配额窗口耗尽的 403 按 429
// 同口径处理：有配额快照时冷却到最早的未来窗口重置点（SetRateLimited，
// 窗口过期后调度自动恢复）；快照缺失（额度探测尚未刷新）时兜底默认 403
// 冷却时长的临时停调，避免过度停调到数天后的窗口。无论哪条路径都不写
// 账号 error 状态。
func (s *RateLimitService) handleCNProviderQuotaExhausted403(
	ctx context.Context,
	account *Account,
	upstreamMsg string,
) {
	if s.cooldownCNProviderToQuotaSnapshotReset(ctx, account, cnQuotaExhaustedReasonPrefix, "cn_quota_exhausted_rate_limited") != nil {
		return
	}

	// upstreamMsg 由调用方（HandleUpstreamError）从同一响应体提取，此处直接复用；
	// 为空时仅存前缀，保证 reason 稳定可检索。
	reason := cnQuotaExhaustedReasonPrefix
	if msg := strings.TrimSpace(upstreamMsg); msg != "" {
		reason += ": " + msg
	}
	until := time.Now().Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
	s.setCNProviderTempUnschedulable(ctx, account, until, cnQuotaExhaustedReasonPrefix, reason, "cn_quota_exhausted_temp_unschedulable")
}

func (s *RateLimitService) handleCNProviderConcurrencyLimit403(
	ctx context.Context,
	account *Account,
) {
	until := time.Now().Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
	s.setCNProviderTempUnschedulable(ctx, account, until,
		cnConcurrencyLimitReasonPrefix, cnConcurrencyLimitReasonPrefix+": "+kimiConcurrentRequestLimitMessage,
		"cn_provider_concurrency_limited")
}

// cnBalanceLowReason 构造余额不足临时停调的 reason（带稳定前缀）。
// 保留供上游余额类停调（cline/command code 等）与存量识别共用；本服务响应式 402
// 入口已不再用它构造停调（停调语义移交额度耗尽状态机）。
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
	// 结构化信号透传（方案 §4 F2 作用域矩阵）：402 余额不足信号经
	// ClassifyQuotaExhaustionScope 判定作用域后透传 Scoped 入口——kira+402=account
	// 权威冻结，非 Kira 402=model/path 探针确认；禁止从自由报文文本猜测作用域。
	signal := QuotaExhaustionSignal{
		Status:   402,
		Scope:    ClassifyQuotaExhaustionScope(account, 402, time.Now()),
		Platform: account.Platform,
		Msg:      upstreamMsg,
	}
	if err := s.cnQuotaLifecycle.OnUpstreamQuotaExhaustedScoped(ctx, account, upstreamMsg, signal); err != nil {
		// 停调失败（如仓库层错误）不回退滚动冷却：下轮真实流量会再次携带 402 信号
		// 进入状态机重试；余额检测周期任务也可再次触发本入口。
		slog.Warn("cn_quota_lifecycle_entry_failed",
			"account_id", account.ID,
			"platform", account.Platform,
			"error", err,
		)
	}
}

// cnBalanceCooldownDuration 返回余额不足临时停调的持续时长（= 2× 余额检测周期，
// 默认 20 分钟）。保留供上游余额类停调（cline 等，见 ratelimit_cline.go）共用；
// 国产供应商响应式 402 入口的停调语义已移交额度耗尽状态机，不再使用本冷却。
func (s *RateLimitService) cnBalanceCooldownDuration() time.Duration {
	if s == nil {
		return cnBalanceCheckCooldown(nil)
	}
	return cnBalanceCheckCooldown(s.cfg)
}

// cnBalanceCheckCooldown 返回余额类冷却时长（= 2× 余额检测周期，默认周期 10 分钟）。
func cnBalanceCheckCooldown(cfg *config.Config) time.Duration {
	minutes := 10
	if cfg != nil {
		if cfgMin := cfg.Gateway.CNProviders.BalanceCheckIntervalMinutes; cfgMin > 0 {
			minutes = cfgMin
		}
	}
	cooldown := time.Duration(minutes) * time.Minute * 2
	if cooldown < time.Minute {
		cooldown = 10 * time.Minute
	}
	return cooldown
}

// cnProviderQuotaSnapshotReset 读取账号快照中最早一个仍在未来的窗口重置时间
// （5h / weekly，OpenCode Go / Command Code 另含 monthly）。
// 429 多数由 5h 滚动窗口触发，取较早的重置点可避免把账号冷却到 weekly 重置
// （可达数天）的过度停调；如果确是 weekly 窗口耗尽，周期额度探测刷新快照后阈值
// 评估会再次停调到正确的时间点。无快照或均已过期返回 nil。
//
// 平台分派：OpenCode Go / Command Code 用 platform 直接取快照键；国产 coding 供应商
// 经 resolveCNQuotaProvider 解析（兼容 payg 火山号 platform 误存的场景）。其余账号
// 无滚动窗口快照，返回 nil。
func cnProviderQuotaSnapshotReset(account *Account, now time.Time) *time.Time {
	accountsnap := account.SnapshotExtra()
	if account == nil || len(accountsnap) == 0 {
		return nil
	}
	var provider string
	switch {
	case account.IsOpenCodeGo():
		provider = account.Platform
	case account.IsCommandCode():
		// 有充值积分时上游跳过窗口检查，429 与窗口无关。
		if !commandCodeWindowsBinding(accountsnap) {
			return nil
		}
		provider = account.Platform
	case account.IsCNProvider():
		// 快照键以 coding 供应商为前缀（kimi_/zhipu_/volcano_/kira_），与
		// cnQuotaExtraUpdates 的写入维度一致；不能用 platform（deepseek 火山号）取键。
		// 用 resolveCNQuotaProvider（兼容 payg 火山）而非 GetCodingPlanProvider，
		// 否则 payg 火山号的 429 冷却读不到 volcano_* 重置点、落入真实 5h 窗口。
		provider = resolveCNQuotaProvider(account)
	default:
		return nil
	}
	if provider == "" {
		return nil
	}
	suffixes := []string{cnExtraSuffix5hReset, cnExtraSuffixWeeklyReset}
	if cnQuotaHasMonthlyWindow(provider) {
		suffixes = append(suffixes, cnExtraSuffixMonthlyReset)
	}
	var earliest *time.Time
	for _, suffix := range suffixes {
		t := parseSchedulingResetAt(accountsnap[cnExtraKey(provider, suffix)])
		if t == nil || !t.After(now) {
			continue
		}
		if earliest == nil || t.Before(*earliest) {
			earliest = t
		}
	}
	return earliest
}

// cooldownCNProviderToQuotaSnapshotReset 把账号冷却到配额快照中最早的未来窗口
// 重置点（SetRateLimited，窗口过期后调度自动恢复），供 429（OpenCodeGo /
// Coding Plan）与配额耗尽 403 共用。仅在冷却成功持久化后返回非 nil；
// 无快照或写库失败时返回 nil，由调用方落入各自的兜底逻辑（避免写库失败
// 时账号完全失去持久冷却）。
func (s *RateLimitService) cooldownCNProviderToQuotaSnapshotReset(ctx context.Context, account *Account, reason, logEvent string) *time.Time {
	until := cnProviderQuotaSnapshotReset(account, time.Now())
	if until == nil {
		return nil
	}
	s.notifyAccountSchedulingBlocked(account, *until, reason)
	if err := s.accountRepo.SetRateLimited(ctx, account.ID, *until); err != nil {
		slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
		return nil
	}
	slog.Info(logEvent,
		"account_id", account.ID,
		"platform", account.Platform,
		"reset_at", until.UTC(),
	)
	return until
}

// setCNProviderTempUnschedulable 写临时停调（含调度层通知与日志），供 CN 403
// 各分支（并发限制、配额耗尽兜底）共用。告警 key 由 notifyReason 派生，保持
// 各分支既有日志 key 稳定（<prefix>_set_temp_unschedulable_failed）。
func (s *RateLimitService) setCNProviderTempUnschedulable(ctx context.Context, account *Account, until time.Time, notifyReason, storeReason, logEvent string) {
	s.notifyAccountSchedulingBlocked(account, until, notifyReason)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, storeReason); err != nil {
		slog.Warn(notifyReason+"_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info(logEvent,
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
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
			if cnQuotaSnapshotAnyWindowExhausted(account.SnapshotExtra(), provider) {
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
	accountsnap := account.SnapshotExtra()
	// 0) OpenCode Go：429 先冷却到配额快照最早的未来窗口重置点；无快照时解析响应体
	// 中的重置时间（秒级 unix）；两者皆无则不处理，交回默认 429 逻辑。
	if account.IsOpenCodeGo() {
		if s.cooldownCNProviderToQuotaSnapshotReset(ctx, account, "429", "opencode_go_rate_limited") != nil {
			return true
		}
		if resetAt := parseOpenAIRateLimitResetTime(responseBody); resetAt != nil {
			resetTime := time.Unix(*resetAt, 0)
			s.notifyAccountSchedulingBlocked(account, resetTime, "429")
			if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetTime); err != nil {
				slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
				return true
			}
			slog.Info("opencode_go_rate_limited",
				"account_id", account.ID,
				"platform", account.Platform,
				"reset_at", resetTime,
			)
			return true
		}
		return false
	}
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
		// 结构化信号透传（方案 §4 F2 作用域矩阵）：429 归一为额度耗尽，探针确认语义
		//（含 Kira 429 也走 model/path，不触发账号级权威冻结）；unknown=不整号转换。
		sigMsg := extractUpstreamErrorMessage(responseBody)
		signal := QuotaExhaustionSignal{
			Status:   429,
			Scope:    ClassifyQuotaExhaustionScope(account, 429, time.Now()),
			Platform: account.Platform,
			Msg:      sigMsg,
		}
		if err := s.cnQuotaLifecycle.OnUpstreamQuotaExhaustedScoped(ctx, account, sigMsg, signal); err != nil {
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
		if reset := cnQuotaSnapshotEarliestExhaustedReset(accountsnap, provider, time.Now()); reset != nil {
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
