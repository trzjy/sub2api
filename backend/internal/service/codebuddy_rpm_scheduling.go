package service

import (
	"context"
	"log/slog"
)

// CodeBuddy 账号级 RPM 限流（B1）。
//
// 背景：CodeBuddy 走 OpenAI 兼容网关路径，Anthropic 侧的 RPM 机制
// （GatewayService.isAccountSchedulableForRPM）对它完全不生效。故在
// OpenAIGatewayService 选号链路内单独实现，且**仅对 CodeBuddy OAuth 账号生效**，
// 不影响 OpenAI/Grok/国产供应商等其它平台的既有调度行为。
//
// 语义与 Anthropic 侧对齐：
//   - 有效 RPM = 账号显式 base_rpm，未配置时取平台默认 codebuddy_default_rpm（默认 10）。
//   - 非粘性候选：仅绿区（currentRPM < base）可选。
//   - 粘性会话：允许黄区（base <= currentRPM < base+buffer），红区才清绑定换号。
//   - RPM 计数不可用（缓存未配置/查询失败）一律失败开放，避免误杀。

// codeBuddyRPMGated 判断账号是否纳入 CodeBuddy 平台默认 RPM 限流。
func codeBuddyRPMGated(account *Account) bool {
	return account != nil && account.Platform == PlatformCodeBuddy && account.Type == AccountTypeOAuth
}

// codeBuddyEffectiveRPM 返回账号实际生效的每分钟请求上限：
// 显式 base_rpm 优先；CodeBuddy 未配置时回落平台默认；其它情况 0（不限流）。
func (s *OpenAIGatewayService) codeBuddyEffectiveRPM(ctx context.Context, account *Account) int {
	if account == nil {
		return 0
	}
	if base := account.GetBaseRPM(); base > 0 {
		return base
	}
	if !codeBuddyRPMGated(account) {
		return 0
	}
	if s != nil && s.settingService != nil {
		return s.settingService.GetCodeBuddyDefaultRPM(ctx)
	}
	return DefaultCodeBuddyRPM
}

// prefetchCodeBuddyRPMCounts 批量预取 CodeBuddy 候选账号当前分钟的 RPM 计数，
// 避免在选号热路径逐账号查 Redis（N+1）。失败开放：无候选或查询失败返回 nil。
func (s *OpenAIGatewayService) prefetchCodeBuddyRPMCounts(ctx context.Context, accounts []Account) map[int64]int {
	if s == nil || s.rpmCache == nil {
		return nil
	}
	var ids []int64
	for i := range accounts {
		acc := &accounts[i]
		if !codeBuddyRPMGated(acc) {
			continue
		}
		if s.codeBuddyEffectiveRPM(ctx, acc) <= 0 {
			continue
		}
		ids = append(ids, acc.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	counts, err := s.rpmCache.GetRPMBatch(ctx, ids)
	if err != nil {
		return nil
	}
	return counts
}

// codeBuddyRPMSchedulable 判断 CodeBuddy 账号在 RPM 维度是否可调度。
// currentRPM < 0 表示计数不可用（失败开放）。非 CodeBuddy 或无限流配置恒 true。
func (s *OpenAIGatewayService) codeBuddyRPMSchedulable(ctx context.Context, account *Account, currentRPM int, isSticky bool) bool {
	if !codeBuddyRPMGated(account) {
		return true
	}
	baseRPM := s.codeBuddyEffectiveRPM(ctx, account)
	if baseRPM <= 0 || currentRPM < 0 {
		return true
	}
	switch account.CheckRPMSchedulabilityWithBase(currentRPM, baseRPM) {
	case WindowCostSchedulable:
		return true
	case WindowCostStickyOnly:
		return isSticky
	default:
		return false
	}
}

// incrementCodeBuddyRPM 成功触达上游后递增账号 RPM 计数。
func (s *OpenAIGatewayService) incrementCodeBuddyRPM(ctx context.Context, account *Account) {
	if s == nil || s.rpmCache == nil || !codeBuddyRPMGated(account) {
		return
	}
	if s.codeBuddyEffectiveRPM(ctx, account) <= 0 {
		return
	}
	if _, err := s.rpmCache.IncrementRPM(ctx, account.ID); err != nil {
		slog.Warn("codebuddy.rpm_increment_failed", "account_id", account.ID, "error", err)
	}
}

// codeBuddyRPMCountsContextKey 承载一次选号内预取的 CodeBuddy RPM 计数。
type codeBuddyRPMCountsContextKey struct{}

func withCodeBuddyRPMCounts(ctx context.Context, counts map[int64]int) context.Context {
	if len(counts) == 0 {
		return ctx
	}
	return context.WithValue(ctx, codeBuddyRPMCountsContextKey{}, counts)
}

func codeBuddyRPMCountFromContext(ctx context.Context, accountID int64) (int, bool) {
	counts, ok := ctx.Value(codeBuddyRPMCountsContextKey{}).(map[int64]int)
	if !ok {
		return 0, false
	}
	count, found := counts[accountID]
	return count, found
}
