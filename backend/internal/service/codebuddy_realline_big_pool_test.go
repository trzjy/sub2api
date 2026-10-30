//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// codeBuddyReallineSchedAccount 构造聚合分组下的 CodeBuddy 候选：真实线不带 mask，
// 伪装线带 mask_upstream_identity=true；两者 model_mapping 与上游目录一致。
func codeBuddyReallineSchedAccount(id, groupID int64, masked bool) Account {
	account := Account{
		ID: id, Platform: PlatformCodeBuddy, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 2, Priority: 1,
		GroupIDs:      []int64{groupID},
		AccountGroups: []AccountGroup{{AccountID: id, GroupID: groupID}},
	}
	account.Extra = map[string]any{}
	if masked {
		account.Extra["mask_upstream_identity"] = true
	}
	account.Credentials = map[string]any{
		"model_mapping": map[string]any{"deepseek-v4.1-flash-1017": "deepseek-v4.1-flash"},
	}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Models: map[string]UpstreamModelMetadata{"deepseek-v4.1-flash": {ID: "deepseek-v4.1-flash"}},
	})
	return account
}

// TestCodeBuddyRealLineAdmittedForBigAndUngatedPools 钉死 2026-10-30 用户裁定（B 方案）：
// 真实线 CodeBuddy 账号（mask_upstream_identity=false）必须能在
//
//	① 未装分档门（pool=""，分组内无伪装号，如 group 49）与
//	② 大档（upstream 池，如 group 43 有 247 开门）跨平台承接聚合分组请求。
//
// 伪装号仍只在小档（masked 池）被准入；大档由 openAIAccountEligibleForTierPool 先行剔除，
// 与入站平台准入解耦（回归护栏：mask 不再是跨平台准入的前置条件）。
func TestCodeBuddyRealLineAdmittedForBigAndUngatedPools(t *testing.T) {
	const groupID = int64(6001)
	group := &Group{ID: groupID, Platform: PlatformDeepseek, AggregateCodeBuddyEnabled: true}
	ctx := context.WithValue(context.Background(), openAIGroupPrivacyRequirementContextKey{},
		openAIGroupPrivacyRequirement{groupID: groupID, group: group})

	realLine := codeBuddyReallineSchedAccount(6002, groupID, false)
	masked := codeBuddyReallineSchedAccount(6003, groupID, true)

	const model = "deepseek-v4.1-flash-1017"
	reason := func(ctx context.Context, account *Account) string {
		return openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
			ctx, account, PlatformDeepseek, model, false, OpenAIEndpointCapabilityChatCompletions)
	}

	require.Empty(t, reason(ctx, &realLine),
		"真实线号在未装分档门（pool=\"\"）时必须跨平台准入")
	require.Empty(t, reason(WithTierRouting(ctx, TierRoutingBig), &realLine),
		"真实线号必须能进大档（upstream 池）")
	require.Equal(t, "tier_pool_mismatch", reason(WithTierRouting(ctx, TierRoutingSmall), &realLine),
		"真实线号不得进伪装小档池")

	require.Equal(t, "tier_pool_mismatch", reason(WithTierRouting(ctx, TierRoutingBig), &masked),
		"伪装号不得进大档")
	require.Empty(t, reason(WithTierRouting(ctx, TierRoutingSmall), &masked),
		"伪装号在小档仍须准入（既有行为不变）")
}

// TestCodeBuddyTierPlatformMismatchKeepsMaskedSmallOnly 锁定平台准入的 mask 语义：
// 真实线号（mask=false）在大档 / 未装门时准入；伪装号（mask=true）仍只允许 masked 池，
// 且分组聚合开关等其余准入条件不得被一并放开。
func TestCodeBuddyTierPlatformMismatchKeepsMaskedSmallOnly(t *testing.T) {
	const groupID = int64(6002)
	group := &Group{ID: groupID, Platform: PlatformDeepseek, AggregateCodeBuddyEnabled: true}
	realLine := codeBuddyReallineSchedAccount(6010, groupID, false)
	masked := codeBuddyReallineSchedAccount(6011, groupID, true)

	require.True(t, codeBuddyTierPlatformMismatchAllowed(&realLine, PlatformDeepseek, "deepseek-v4.1-flash-1017", groupID, group, ""))
	require.True(t, codeBuddyTierPlatformMismatchAllowed(&realLine, PlatformDeepseek, "deepseek-v4.1-flash-1017", groupID, group, tierRoutingPoolUpstream))
	require.True(t, codeBuddyTierPlatformMismatchAllowed(&masked, PlatformDeepseek, "deepseek-v4.1-flash-1017", groupID, group, tierRoutingPoolMasked))
	require.False(t, codeBuddyTierPlatformMismatchAllowed(&masked, PlatformDeepseek, "deepseek-v4.1-flash-1017", groupID, group, ""),
		"伪装号在未装门时必须 fail closed")
	require.False(t, codeBuddyTierPlatformMismatchAllowed(&masked, PlatformDeepseek, "deepseek-v4.1-flash-1017", groupID, group, tierRoutingPoolUpstream),
		"伪装号不得进大档")

	// 分组聚合开关关闭时仍拒绝（准入的其余条件不得被一并放开）。
	disabled := &Group{ID: groupID, Platform: PlatformDeepseek, AggregateCodeBuddyEnabled: false}
	require.False(t, codeBuddyTierPlatformMismatchAllowed(&realLine, PlatformDeepseek, "deepseek-v4.1-flash-1017", groupID, disabled, ""))
}
