package service

import (
	"context"
)

// stickyBindingMissReason 归类「粘性会话本次没能命中绑定账号」的全部原因。
//
// 会话记忆规则（2026-11-23 裁定，取代 2026-11-15「临时原因保留绑定」设计）：
// 绑定只在上游服务成功后写入；本次服务账号与既有绑定不同即覆盖。
// 未命中时的临时原因不再阻止后续覆盖：转发成功后由调用方把绑定改写到新账号。
// 本文件保留原因分类，仅为「永久失效要删绑定」提供唯一事实源。
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

	// —— 临时原因：不删绑定；是否改写由转发成功后的覆盖写入决定 ——

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
	// stickyMissUnknown 未归类原因：保守按临时处理，不删绑定。
	stickyMissUnknown stickyBindingMissReason = "unknown"
)

// permanentStickyBindingMissReasons 是「绑定必须销毁」的原因全集。
// 只有这里列出的原因才允许删除绑定；其余原因一律不删绑定，
// 会话记忆的覆盖统一由「转发成功后写入」完成。
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
// 永久原因 → 销毁；临时原因 → 不销毁（但不再阻止转发成功后的覆盖写入）。
func stickyBindingDroppedOnMiss(reason stickyBindingMissReason) bool {
	_, permanent := permanentStickyBindingMissReasons[reason]
	return permanent
}

// releaseStickyBindingOnMiss 在粘性未命中时按原因处理绑定。
// 永久原因 → 删除绑定；临时原因 → 不删绑定（后续由转发成功写入决定覆盖）。
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
