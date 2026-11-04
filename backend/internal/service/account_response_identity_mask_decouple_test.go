//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// sensenova 接入的解耦断言（2026-10-31）。
//
// 用户裁定「我只让你改伪装，没让你改分层路由」→ 响应侧身份净化必须能在
// **不改变账号分层准入结果**的前提下独立开启。
//
// 本文件对同一账号同时断言两件事：
//   (1) 响应侧净化门为真（模型回写 / 错误体净化 / 指纹剥离 生效）；
//   (2) 分层路由侧判定与「该账号完全没开任何伪装开关」时逐字节相同
//       （池准入 openAIAccountEligibleForTierPool 与门控 tierRoutingGateOpenForAccounts）。
//
// 任何一处耦合都会让 (2) 失败。

// svDecoupledTestAccount 造一个只开响应侧净化开关的账号（不开 mask_upstream_identity）。
func svDecoupledTestAccount(id int64, extra map[string]any) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"model_mapping": map[string]any{"deepseek-v4.1-flash": "sensenova-6.8-flash-lite"}},
		Extra:       extra,
	}
}

// TestResponseIdentityMaskDecouplesTierRouting 是本次改动的核心命题断言：
// 开启响应侧净化开关后，响应侧净化生效，而分层路由的池准入与门控判定
// 与「未开任何伪装开关」的账号完全一致。
func TestResponseIdentityMaskDecouplesTierRouting(t *testing.T) {
	const model = "deepseek-v4.1-flash"

	// 基准账号：什么伪装开关都没开。
	plain := svDecoupledTestAccount(9101, map[string]any{})
	// 目标账号：只开响应侧净化开关。
	decoupled := svDecoupledTestAccount(9102, map[string]any{responseIdentityMaskKey: true})

	t.Run("响应侧净化门生效", func(t *testing.T) {
		require.False(t, IsIdentityMaskedAccount(plain), "基准账号不应是分层掩码账号")
		require.True(t, IsResponseIdentityMaskedAccount(decoupled),
			"只开响应侧净化开关即应触发响应侧净化")
		require.False(t, IsResponseIdentityMaskedAccount(plain),
			"未开任何开关的账号不应触发响应侧净化")
	})

	t.Run("分层门控判定与基准账号一致", func(t *testing.T) {
		// 门控只认 mask_upstream_identity：新开关账号不得开门，也不得改变他人门控。
		require.False(t, tierRoutingGateOpenForAccounts([]Account{*decoupled}, model),
			"只开响应侧净化开关不得使分层门开启")
		require.False(t, tierRoutingGateOpenForAccounts([]Account{*plain}, model),
			"基准账号门控同样关闭")
	})

	t.Run("分层池准入判定与基准账号一致", func(t *testing.T) {
		// 大档首轮落伪装线大档池（masked_large，要求声明 masked_input_band）：两个账号都
		// 不是大档伪装号，必须一致落选；它们是大档整池无可用、跨档之后星思池的供给。
		bigCtx := WithTierRouting(context.Background(), TierRoutingBig, tierRoutingBigEstimate)
		require.Equal(t, openAIAccountEligibleForTierPool(bigCtx, plain),
			openAIAccountEligibleForTierPool(bigCtx, decoupled),
			"响应侧净化开关不得改变大档首轮池准入判定")
		require.False(t, openAIAccountEligibleForTierPool(bigCtx, decoupled),
			"未声明区间的普通账号不进大档伪装池")

		// 估算超过伪装线大档上限 → 直回星思池，此时两者都必须一致入选（档位供给不能丢）。
		upstreamCtx := WithTierRouting(context.Background(), TierRoutingBig, TierRoutingMaskedInputCeilingTokens+1)
		require.Equal(t, openAIAccountEligibleForTierPool(upstreamCtx, plain),
			openAIAccountEligibleForTierPool(upstreamCtx, decoupled),
			"响应侧净化开关不得改变星思池准入判定")
		require.True(t, openAIAccountEligibleForTierPool(upstreamCtx, decoupled),
			"超过伪装线大档上限时该账号必须留在星思池（保住档位供给）")

		// 小档（masked 池）：新开关账号与基准账号都必须落选（都不是伪装线账号）。
		smallCtx := WithTierRouting(context.Background(), TierRoutingSmall, tierRoutingSmallEstimate)
		require.Equal(t, openAIAccountEligibleForTierPool(smallCtx, plain),
			openAIAccountEligibleForTierPool(smallCtx, decoupled),
			"响应侧净化开关不得改变小档池准入判定")
		require.False(t, openAIAccountEligibleForTierPool(smallCtx, decoupled),
			"该账号不应被划入伪装线小档池")
	})
}

// TestResponseIdentityMaskKeepsLegacyMaskBehavior 断言既有掩码账号行为不变：
// 老开关 mask_upstream_identity=true 的账号在响应侧与分层侧都仍是掩码账号。
func TestResponseIdentityMaskKeepsLegacyMaskBehavior(t *testing.T) {
	const model = "deepseek-v4.1-flash"
	legacy := svDecoupledTestAccount(9103, map[string]any{"mask_upstream_identity": true})

	require.True(t, IsIdentityMaskedAccount(legacy), "老开关账号仍是分层掩码账号")
	require.True(t, IsResponseIdentityMaskedAccount(legacy), "老开关账号仍触发响应侧净化")
	require.True(t, tierRoutingGateOpenForAccounts([]Account{*legacy}, model),
		"老开关账号的分层门控行为不变")

	bigCtx := WithTierRouting(context.Background(), TierRoutingBig, tierRoutingBigEstimate)
	require.False(t, openAIAccountEligibleForTierPool(bigCtx, legacy),
		"老开关账号仍只进小档伪装池（行为不变）")

	smallCtx := WithTierRouting(context.Background(), TierRoutingSmall, tierRoutingSmallEstimate)
	require.True(t, openAIAccountEligibleForTierPool(smallCtx, legacy),
		"老开关账号仍进小档伪装池（行为不变）")
}
