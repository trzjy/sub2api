package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProbeModelCandidatesSensenovaAccount 验证商汤 8877 账号（生产事故账号 1002）的
// 候选来自账号自己维护的模型名单，且绝不包含 gpt-5.4 之类硬编码模型。
func TestProbeModelCandidatesSensenovaAccount(t *testing.T) {
	t.Parallel()
	acc := &Account{
		ID:       1002,
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"deepseek-flash": "sensenova-6.8-flash-lite",
				"deepseek-v4-pro": "sensenova-6.8-flash-lite",
			},
		},
	}
	candidates := probeModelCandidates(acc)
	require.Len(t, candidates, 1, "两个请求侧模型指向同一上游真名，去重后只应有一个候选")
	require.Equal(t, "sensenova-6.8-flash-lite", candidates[0].Upstream)

	// 绝不能出现 gpt-5.4：商汤上游没有这个模型，打过去恒 404。
	for _, c := range candidates {
		require.NotEqual(t, "gpt-5.4", c.Upstream)
		require.NotEqual(t, "gpt-5.4", c.InboundKey)
	}
}

// TestProbeModelCandidatesKeepsMultipleModels 验证账号提供多个模型时**全部保留**，
// 由各消费方按自己语义消费（取首个 / 逐候选回退），不在事实源层就砍成一个。
func TestProbeModelCandidatesKeepsMultipleModels(t *testing.T) {
	t.Parallel()
	acc := &Account{
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"client-b": "zeta-model",
				"client-a": "alpha-model",
				"client-c": "beta-model",
			},
		},
	}
	candidates := probeModelCandidates(acc)
	require.Len(t, candidates, 3, "多个模型必须全部保留，不能在事实源层丢掉")

	// 按上游真名字典序，可复现。
	require.Equal(t, "alpha-model", candidates[0].Upstream)
	require.Equal(t, "beta-model", candidates[1].Upstream)
	require.Equal(t, "zeta-model", candidates[2].Upstream)

	// 取首个的调用方拿字典序首个。
	require.Equal(t, "alpha-model", pickProbeModelUpstream(candidates))
	// 逐候选回退的调用方拿到完整列表。
	require.Equal(t, []string{"alpha-model", "beta-model", "zeta-model"}, probeModelUpstreams(acc))
}

// TestProbeModelCandidatesKeepsInboundKey 验证候选同时保留入站键（cursor 分支需要键
// 命中映射，出站时再解析成上游名）。
func TestProbeModelCandidatesKeepsInboundKey(t *testing.T) {
	t.Parallel()
	acc := &Account{
		Platform: PlatformCursor,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"glm-5.3": "glm-5p3"},
		},
	}
	candidates := probeModelCandidates(acc)
	require.Len(t, candidates, 1)
	require.Equal(t, "glm-5.3", candidates[0].InboundKey)
	require.Equal(t, "glm-5p3", candidates[0].Upstream)
}

// TestProbeModelCandidatesNoNameShapeFiltering 验证不按模型名形态过滤：管理员可以自定义
// 命名（非 "gpt-" 前缀）也必须可用；按名字形态丢弃会把"明明有可用模型"变成"无模型可探"
// ⇒ 账号永远回不了池。
func TestProbeModelCandidatesNoNameShapeFiltering(t *testing.T) {
	t.Parallel()
	acc := &Account{
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"codex-auto-review": "codex-auto-review"},
		},
	}
	candidates := probeModelCandidates(acc)
	require.Len(t, candidates, 1)
	require.Equal(t, "codex-auto-review", candidates[0].Upstream)
}

// TestProbeModelCandidatesExcludesUnprobeable 验证排除物理上不可探的候选：
// 空值、通配符、图片/媒体模型。
func TestProbeModelCandidatesExcludesUnprobeable(t *testing.T) {
	t.Parallel()
	acc := &Account{
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"wildcard":  "gpt-*",
				"blank":     "  ",
				"image":     "gpt-image-2",
				"real-model": "real-model",
			},
		},
	}
	candidates := probeModelCandidates(acc)
	require.Len(t, candidates, 1)
	require.Equal(t, "real-model", candidates[0].Upstream)
}

// TestProbeModelCandidatesEmptyMapping 验证映射为空时返回空候选（不回落任何硬编码模型）。
func TestProbeModelCandidatesEmptyMapping(t *testing.T) {
	t.Parallel()
	require.Empty(t, probeModelCandidates(&Account{}))
	require.Empty(t, probeModelCandidates(nil))
	require.Empty(t, pickProbeModelUpstream(nil))
	require.Empty(t, pickProbeModelInboundKey(nil))
}
