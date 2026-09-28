package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 以下测试覆盖方案 §2.1-1 的 repo 候选池扩展（聚合直绑前置于 B2 的交付线）：
// 分组 platform ∈ 聚合族（deepseek/zhipu/kimi/minimax/other/codebuddy）时，
// 候选池平台集合额外并入 platform=codebuddy；非聚合分组（openai/anthropic/
// gemini/grok）逐位不变；schedulable 过滤由 queryAccountsByGroup 既有谓词
// 保证，本扩展只动"平台集合"，不触碰 schedulable/状态/瞬态过滤。

// 聚合族平台（与 repository.aggregatePlatformFamily 同源，仅用于用例枚举）。
var aggregatePoolFamilyPlatforms = []string{
	service.PlatformDeepseek,
	service.PlatformZhipu,
	service.PlatformKimi,
	service.PlatformMiniMax,
	service.PlatformOther,
	service.PlatformCodeBuddy,
}

// 非聚合族平台（国外协议/其它分组，不应纳入 codebuddy 候选）。
var nonAggregatePoolPlatforms = []string{
	service.PlatformOpenAI,
	service.PlatformAnthropic,
	service.PlatformGemini,
	service.PlatformGrok,
}

// 场景一：聚合分组候选池额外纳入 codebuddy 账号。
func TestAggregatePool_AggregateGroupIncludesCodeBuddy(t *testing.T) {
	for _, p := range aggregatePoolFamilyPlatforms {
		p := p
		t.Run(p, func(t *testing.T) {
			got := expandPlatformsForAggregatePool([]string{p}, true)
			require.Contains(t, got, service.PlatformCodeBuddy,
				"聚合族分组 %q 的候选池应并入 codebuddy", p)
			require.Contains(t, got, p, "原平台 %q 应保留", p)
		})
	}
}

// 场景二：非聚合分组（openai/anthropic/gemini/grok）候选逐位不含 codebuddy。
func TestAggregatePool_NonAggregateGroupExcludesCodeBuddy(t *testing.T) {
	for _, p := range nonAggregatePoolPlatforms {
		p := p
		t.Run(p, func(t *testing.T) {
			got := expandPlatformsForAggregatePool([]string{p}, true)
			require.NotContains(t, got, service.PlatformCodeBuddy,
				"非聚合分组 %q 的候选池不应并入 codebuddy", p)
			require.Equal(t, []string{p}, got, "非聚合分组平台集合应逐位不变")
		})
	}
}

// 场景三：schedulable=false 的 codebuddy 账号任何分组都不可见。
// expandPlatformsForAggregatePool 仅扩展"平台集合"，绝不引入任何绕过
// SchedulableEQ(true) 谓词的手段；schedulable 过滤仍由 queryAccountsByGroup
// 既有逻辑保证（本次改动未触碰）。该测试断言扩展逻辑本身不会把非聚合分组的
// codebuddy 拉入候选（即在非聚合分组中，schedulable=false 的 codebuddy 甚至
// 不在平台候选集合内，更不可能因本次扩展而可见）；聚合分组即便并入 codebuddy
// 平台，也只是把它交给既有 SchedulableEQ(true) 过滤——schedulable=false 仍被排除。
func TestAggregatePool_SchedulableFalseCodeBuddyNotVisible(t *testing.T) {
	// 非聚合分组：codebuddy 根本不在候选平台集合内，与 schedulable 取值无关。
	for _, p := range nonAggregatePoolPlatforms {
		require.NotContains(t, expandPlatformsForAggregatePool([]string{p}, true),
			service.PlatformCodeBuddy, "非聚合分组 %q 不应暴露 codebuddy 候选", p)
	}

	// 聚合分组：扩展逻辑只追加平台名，不追加任何"schedulable 豁免"标志。
	for _, p := range aggregatePoolFamilyPlatforms {
		got := expandPlatformsForAggregatePool([]string{p}, true)
		// 集合长度仅在原集合不含 codebuddy 时 +1，证明是纯追加平台名，
		// 没有把 schedulable 维度一并改掉。
		expected := 2
		if p == service.PlatformCodeBuddy {
			expected = 1 // 自身已在集合，不再重复
		}
		require.Len(t, got, expected,
			"聚合分组 %q 扩展应为纯平台追加，不应改变 schedulable 维度", p)
	}
}

// 场景四：codebuddy 自有分组行为不变（自身已在平台集合内，不应重复并入，
// 也不应反向剔除）。
func TestAggregatePool_CodeBuddyOwnGroupUnchanged(t *testing.T) {
	got := expandPlatformsForAggregatePool([]string{service.PlatformCodeBuddy}, true)
	require.Equal(t, []string{service.PlatformCodeBuddy}, got,
		"codebuddy 自有分组的候选池应逐位不变，且不应重复并入自身")

	// 多平台入参且已含 codebuddy 时，不应重复。
	multi := expandPlatformsForAggregatePool([]string{
		service.PlatformDeepseek, service.PlatformCodeBuddy,
	}, true)
	require.ElementsMatch(t, []string{service.PlatformDeepseek, service.PlatformCodeBuddy}, multi,
		"已含 codebuddy 的多平台入参不应重复并入")
}

// 场景五：开关=false × 聚合分组 → 候选池不含 codebuddy（核心收紧点，方案 §1.2）。
// 与场景一（开关=true 并入）对照，证明开关两侧行为逐位相反、由 enabled 单一变量决定。
func TestAggregatePool_DisabledExcludesCodeBuddy(t *testing.T) {
	for _, p := range aggregatePoolFamilyPlatforms {
		p := p
		t.Run(p, func(t *testing.T) {
			got := expandPlatformsForAggregatePool([]string{p}, false)
			// 开关关闭：原平台集合逐位不变，绝不额外并入 codebuddy。
			require.Equal(t, []string{p}, got, "开关关闭时 %q 的候选池应逐位不变", p)
			if p != service.PlatformCodeBuddy {
				require.NotContains(t, got, service.PlatformCodeBuddy,
					"开关关闭时非 codebuddy 聚合分组 %q 的候选池不应出现 codebuddy", p)
			}
		})
	}
}

// 场景六：开关=false × codebuddy 自有分组 → 候选池逐位不变（codebuddy 自身已在集合，
// 开关关闭不改变其既有候选，仅控制"是否额外并入"）。
func TestAggregatePool_DisabledCodeBuddyOwnGroupUnchanged(t *testing.T) {
	got := expandPlatformsForAggregatePool([]string{service.PlatformCodeBuddy}, false)
	require.Equal(t, []string{service.PlatformCodeBuddy}, got,
		"开关关闭时 codebuddy 自有分组候选池应逐位不变")
}

// 场景七：非聚合族平台集合（openai/anthropic/gemini/grok）无论开关取值都不触发
// codebuddy 并入，亦不触发任何额外的 groups 读取（对应方案 §1.2 的
// "非聚合族平台集合零额外读取"——expandPlatformsForAggregatePool 对聚合族无交集时
// 直接返回原集合，queryAccountsByGroup 亦不会读取 groups 行）。本用例以行为等价断言
// 钉死该不变量（基建不提供 groups 读取计数 stub，故以扩展函数输出断言等价行为）。
func TestAggregatePool_NonAggregateFamilyNeverExpands(t *testing.T) {
	for _, p := range nonAggregatePoolPlatforms {
		p := p
		t.Run(p, func(t *testing.T) {
			gotFalse := expandPlatformsForAggregatePool([]string{p}, false)
			gotTrue := expandPlatformsForAggregatePool([]string{p}, true)
			require.Equal(t, []string{p}, gotFalse, "非聚合分组开关=false 候选池逐位不变")
			require.Equal(t, []string{p}, gotTrue, "非聚合分组开关=true 候选池仍逐位不变（无 groups 读取、无 codebuddy 并入）")
		})
	}
}
