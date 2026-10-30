//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 主力渠道备用切换第 2b-fix 片：跨平台平台门（codeBuddyTierPlatformMismatchAllowed）
// 的 L14 收口用例。
//
// 2b 只拆了 tier 池过滤，平台门仍要求 tierPool==masked 且账号伪装，导致备用开启后
// 跨平台承接照样返回 platform_mismatch。本片只在该函数内新增一条"备用集合成员跳过
// 档位/伪装要求"的分支，其余条件（聚合族、分组聚合开关、分组绑定、模型支持）不变，
// 非备用集合路径与 b03ae1079 基线逐条一致。

const standbyGateGroupID = int64(12801)

func standbyGateGroup() *Group {
	return &Group{ID: standbyGateGroupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}
}

// standbyGateAccount 构造已绑定分组的生产形状 codebuddy 账号：model_mapping 含
// standbyTestModel → hy3，masked=false 即真实线（mask_upstream_identity 缺省）。
func standbyGateAccount(id int64, masked bool) Account {
	account := standbyTestAccount(id, 1, masked)
	account.GroupIDs = []int64{standbyGateGroupID}
	account.AccountGroups = []AccountGroup{{AccountID: id, GroupID: standbyGateGroupID}}
	return account
}

func TestStandbyPlatformGateExemption(t *testing.T) {
	group := standbyGateGroup()
	allowed := func(account *Account, tierPool string) bool {
		return codeBuddyTierPlatformMismatchAllowed(account, PlatformKimi, standbyTestModel, standbyGateGroupID, group, tierPool)
	}

	masked := standbyGateAccount(12810, true)
	realLine := standbyGateAccount(12811, false)

	t.Run("open_upstream_pool_admits_standby", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		require.True(t, allowed(&masked, tierRoutingPoolUpstream),
			"43 组大档：备用开启后伪装 codebuddy 账号不再被平台门拒绝")
		require.True(t, allowed(&realLine, tierRoutingPoolUpstream),
			"真实线同样准入")
	})

	t.Run("open_empty_pool_admits_standby", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		require.True(t, allowed(&masked, ""), "49 组无档位门：pool 为空时备用账号同样准入")
		require.True(t, allowed(&realLine, ""))
	})

	t.Run("closed_upstream_pool_rejects", func(t *testing.T) {
		installStandbyRegistry(t)
		require.False(t, allowed(&masked, tierRoutingPoolUpstream), "closed：平台门语义不变")
		require.False(t, allowed(&realLine, tierRoutingPoolUpstream))
		require.False(t, allowed(&masked, ""), "closed：空 pool 同样拒绝")
	})

	t.Run("closed_masked_pool_unchanged", func(t *testing.T) {
		installStandbyRegistry(t)
		require.True(t, allowed(&masked, tierRoutingPoolMasked), "closed：masked 池内伪装账号既准入不变")
		require.False(t, allowed(&realLine, tierRoutingPoolMasked), "closed：非伪装账号不得进 masked 池")
	})

	t.Run("open_other_model_does_not_spill", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		other := standbyGateAccount(12812, true)
		other.Credentials = map[string]any{
			"model_mapping": map[string]any{standbyTestModel: "hy3", "kimi-k3": "hy3"},
		}
		require.True(t, allowed(&other, tierRoutingPoolUpstream), "本模型 open 时准入")
		require.False(t, codeBuddyTierPlatformMismatchAllowed(&other, PlatformKimi, "kimi-k3", standbyGateGroupID, group, tierRoutingPoolUpstream),
			"kimi-k3 自身未 open，open 状态不得外溢到另一模型")
	})

	t.Run("open_non_codebuddy_unchanged", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		foreign := standbyGateAccount(12813, true)
		foreign.Platform = PlatformKimi
		require.False(t, allowed(&foreign, tierRoutingPoolUpstream), "非 codebuddy 账号的结论不因 standby 改变")
		require.False(t, allowed(&foreign, ""))
	})

	t.Run("open_still_requires_group_binding", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		unbound := standbyGateAccount(12814, true)
		unbound.GroupIDs = nil
		unbound.AccountGroups = nil
		require.False(t, allowed(&unbound, tierRoutingPoolUpstream), "备用豁免不放宽分组绑定")
	})
}

// TestStandbyPlatformGateEndToEnd 覆盖真实阻塞点：请求平台与账号平台不一致时，
// 平台门之前的档位门已放开（2b），平台门必须同样放开，否则仍返回 platform_mismatch。
func TestStandbyPlatformGateEndToEnd(t *testing.T) {
	group := standbyGateGroup()
	baseCtx := context.WithValue(context.Background(), openAIGroupPrivacyRequirementContextKey{},
		openAIGroupPrivacyRequirement{groupID: standbyGateGroupID, group: group})

	reason := func(ctx context.Context, account *Account) string {
		return openAICompatibleAccountEligibilityFailureReasonBeforeProfit(
			ctx, account, PlatformKimi, standbyTestModel, false, OpenAIEndpointCapabilityChatCompletions)
	}

	masked := standbyGateAccount(12820, true)
	realLine := standbyGateAccount(12821, false)

	t.Run("closed_platform_mismatch", func(t *testing.T) {
		installStandbyRegistry(t)
		require.Equal(t, "platform_mismatch", reason(WithTierRouting(baseCtx, TierRoutingBig), &realLine),
			"closed：跨平台仍按现状拒绝")
		require.Equal(t, "tier_pool_mismatch", reason(WithTierRouting(baseCtx, TierRoutingBig), &masked),
			"closed：伪装账号先被档位门拦下")
	})

	t.Run("open_admitted", func(t *testing.T) {
		installStandbyRegistry(t, standbyTestModel)
		require.Empty(t, reason(WithTierRouting(baseCtx, TierRoutingBig), &realLine),
			"open：大档跨平台承接不再返回 platform_mismatch")
		require.Empty(t, reason(WithTierRouting(baseCtx, TierRoutingBig), &masked))
		require.Empty(t, reason(baseCtx, &masked), "open：无档位门分组同样准入")
	})
}
