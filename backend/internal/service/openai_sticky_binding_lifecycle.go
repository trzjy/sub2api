package service

import (
	"context"
	"os"
	"strings"
	"sync"
)

// stickyBindingMissReason 归类「粘性会话本次没能命中绑定账号」的全部原因。
//
// 收敛动机（2026-11-15 定位）：同一份事实——「绑定的账号这一轮为什么没被选中」——
// 原先在 openai_gateway_scheduling.go 与 openai_account_scheduler.go 里各写一份，
// 合计十几处 if 各自决定「删绑定 / 保留 / 改写」，且两处只对「并发等待队列满」和
// 「粘性逃逸」两种临时情形做了保留，其余临时情形（限流、档位漂移、RPM 红区、
// 免费额度、team 限流、模型配额、渠道限制）一律删绑定或改写绑定 ——
// 于是会话被永久搬到缓存冷的新账号上，再也回不到原账号。
//
// 生产实测（近 3 天，同 api_key 相邻请求 ≤120 秒）：
//   - 会话 ID 完全相同的换号率 14.8%，走内容指纹兜底的 14.6%（识别没问题）
//   - 换号时原账号仍在接单 75.8%（账号没坏）
//   - 换号时原账号并发槽 ≤1（空闲）53.2%（不是并发满）
//   - 换号 47.6% 跨 priority 档，且生产 PRIORITY_TIER_STRICT_ENABLED=1
//
// 因此新增本文件作为唯一事实源：所有调用点只回答「原因是什么」，
// 由 stickyBindingDroppedOnMiss 统一回答「这条原因该不该销毁绑定」。
type stickyBindingMissReason string

const (
	// —— 永久原因：绑定必须销毁，否则会话会永久卡在一个再也用不了的账号上 ——

	// stickyMissAccountGone 账号被删除、禁用或查不到（DB 重查返回 nil）。
	stickyMissAccountGone stickyBindingMissReason = "account_gone"
	// stickyMissWebAccessMode Web 接入模式账号不得经粘性路由（B3 规则）。
	stickyMissWebAccessMode stickyBindingMissReason = "web_access_mode"
	// stickyMissIllegalAccessMode access_mode 显式非法，fail-closed。
	stickyMissIllegalAccessMode stickyBindingMissReason = "illegal_access_mode"
	// stickyMissPlatformMismatch 账号平台与请求平台不一致。
	stickyMissPlatformMismatch stickyBindingMissReason = "platform_mismatch"
	// stickyMissGroupMismatch 账号已不属于当前调度分组（分组归属变更）。
	stickyMissGroupMismatch stickyBindingMissReason = "group_mismatch"
	// stickyMissModelUnsupported 账号不再支持请求的模型（模型映射/能力变更）。
	stickyMissModelUnsupported stickyBindingMissReason = "model_unsupported"
	// stickyMissTransportUnsupported 账号不再支持请求所需的传输方式。
	stickyMissTransportUnsupported stickyBindingMissReason = "transport_unsupported"

	// —— 临时原因：绑定必须保留，本次借道但不得改写绑定 ——

	// stickyMissRateLimited 该模型仍有未到期的限流窗口。
	stickyMissRateLimited stickyBindingMissReason = "rate_limited"
	// stickyMissUnschedulable 账号临时不可调度（额度窗口/熔断/维护），不是删除。
	stickyMissUnschedulable stickyBindingMissReason = "unschedulable"
	// stickyMissConcurrencyFull 并发槽已满（含等待队列满）。
	stickyMissConcurrencyFull stickyBindingMissReason = "concurrency_full"
	// stickyMissPriorityTier 粘住的账号不在当前最小 priority 档（档位随候选池每轮重算）。
	stickyMissPriorityTier stickyBindingMissReason = "priority_tier"
	// stickyMissRPMRed CodeBuddy RPM 进入红区（黄区仍允许粘性）。
	stickyMissRPMRed stickyBindingMissReason = "rpm_red"
	// stickyMissFreeQuotaOver 免费额度用尽（按窗口恢复）。
	stickyMissFreeQuotaOver stickyBindingMissReason = "free_quota_over"
	// stickyMissTeamRateLimited 同 team 429 冷却窗口。
	stickyMissTeamRateLimited stickyBindingMissReason = "team_rate_limited"
	// stickyMissModelQuotaBlocked 模型级配额阻塞窗口。
	stickyMissModelQuotaBlocked stickyBindingMissReason = "model_quota_blocked"
	// stickyMissStickyEscape 粘性逃逸（错误率/首字节劣化触发的主动逃离）。
	stickyMissStickyEscape stickyBindingMissReason = "sticky_escape"
	// stickyMissChannelRestricted 上游渠道对本模型的临时限制。
	stickyMissChannelRestricted stickyBindingMissReason = "channel_restricted"
	// stickyMissSizeScopeExcluded 本次请求因「只接大请求」等尺寸规则被排除。
	stickyMissSizeScopeExcluded stickyBindingMissReason = "size_scope_excluded"
	// stickyMissRuntimeBlocked 运行时临时阻塞（不是账号被停用）。
	stickyMissRuntimeBlocked stickyBindingMissReason = "runtime_blocked"
	// stickyMissShadowParentUnhealthy 影账号母体临时不健康。
	stickyMissShadowParentUnhealthy stickyBindingMissReason = "shadow_parent_unhealthy"
	// stickyMissUnknown 未归类原因：保守按临时处理，保留绑定。
	stickyMissUnknown stickyBindingMissReason = "unknown"
)

// permanentStickyBindingMissReasons 是「绑定必须销毁」的原因全集。
// 只有这里列出的原因才允许删除绑定；新增原因默认走临时（保留绑定），
// 因为多留一次绑定只多一次几十毫秒的重试，误删绑定却会让整个会话永久迁移。
var permanentStickyBindingMissReasons = map[stickyBindingMissReason]struct{}{
	stickyMissAccountGone:          {},
	stickyMissWebAccessMode:        {},
	stickyMissIllegalAccessMode:    {},
	stickyMissPlatformMismatch:     {},
	stickyMissGroupMismatch:        {},
	stickyMissModelUnsupported:     {},
	stickyMissTransportUnsupported: {},
}

// stickyBindingDroppedOnMiss 是唯一判定入口：这条原因要不要销毁绑定。
//
// 「临时」的定义：账号本身仍然存在且未被删除/停用，只是当前这一刻接不了这一单。
// 这类情形必须保留绑定 —— 否则会话会永久迁移到新账号，缓存全废，而且再也回不来。
func stickyBindingDroppedOnMiss(reason stickyBindingMissReason) bool {
	if !stickyKeepBindingOnTransientEnabled() {
		// 回滚开关：恢复旧行为（任何未命中都销毁绑定）。
		return true
	}
	_, permanent := permanentStickyBindingMissReasons[reason]
	return permanent
}

// stickyKeepBindingOnTransientEnabled 回滚开关，默认开启。
// 设置 STICKY_KEEP_BINDING_ON_TRANSIENT=0/false 可恢复 2026-11-15 之前的旧行为。
func stickyKeepBindingOnTransientEnabled() bool {
	stickyKeepBindingOnce.Do(func() {
		raw := strings.TrimSpace(os.Getenv("STICKY_KEEP_BINDING_ON_TRANSIENT"))
		switch strings.ToLower(raw) {
		case "0", "false", "off", "no":
			stickyKeepBindingOn = false
		default:
			stickyKeepBindingOn = true
		}
	})
	return stickyKeepBindingOn
}

var (
	stickyKeepBindingOnce sync.Once
	stickyKeepBindingOn   = true
)

// resetStickyKeepBindingCacheForTest 只供测试重置进程级开关缓存。
// sync.Once 一旦求值就固定，测试需要在同一进程内验证开与关两种行为。
func resetStickyKeepBindingCacheForTest() {
	stickyKeepBindingOnce = sync.Once{}
}

// releaseStickyBindingOnMiss 在粘性未命中时按原因处理绑定。
// 永久原因 → 删除绑定；临时原因 → 保留绑定（本次借道，下一轮仍回原账号）。
// 返回 true 表示绑定确实被删除，调用方应同时把「绑定仍有效」标志置为 false，
// 否则会拦住后续本应发生的重新绑定。
func (s *OpenAIGatewayService) releaseStickyBindingOnMiss(ctx context.Context, groupID *int64, sessionHash string, reason stickyBindingMissReason) bool {
	if sessionHash == "" || s == nil {
		return false
	}
	if !stickyBindingDroppedOnMiss(reason) {
		return false
	}
	_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
	return true
}

// stickyBindingMustSurviveFallback 判断本次回退到别处时，是否必须保留原绑定。
//
// true 表示：原绑定账号仍然存在，这次只是临时接不了，所以「借道」结束后绑定应保持不变。
// 调用方据此决定是否跳过 setStickySessionAccountID 的改写。
func (s *OpenAIGatewayService) stickyBindingMustSurviveFallback(ctx context.Context, groupID *int64, sessionHash string) bool {
	if sessionHash == "" || s == nil || s.cache == nil {
		return false
	}
	if !stickyKeepBindingOnTransientEnabled() {
		return false
	}
	accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash)
	if err != nil || accountID <= 0 {
		// 没有绑定就无所谓保留。
		return false
	}
	// 只问「账号还在不在」，不能问「能不能调度」：
	// 带调度门槛的查询在账号临时限流/并发满时会返回 nil，那恰好是必须保留绑定的场景，
	// 用它会得到「允许改写」的反向结果（原实现踩过这个坑）。
	if s.accountRepo == nil {
		return true
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		// 账号真的没了 → 允许改写。
		return false
	}
	return true
}
