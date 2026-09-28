package service

import (
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// TestAggregateBindingFamilyMatchesPlan 钉死聚合族平台集合与方案 §1.1 一致
//（deepseek/zhipu/kimi/minimax/other/codebuddy），并与 repository.aggregatePlatformFamily
// 逐字一致（repository 侧由 account_repo_aggregate_pool_test.go 钉同一集合）。
// 两处字面一致 + 测试钉住，禁止产生第二套语义漂移空间。
func TestAggregateBindingFamilyMatchesPlan(t *testing.T) {
	expected := map[string]struct{}{
		PlatformDeepseek:  {},
		PlatformZhipu:     {},
		PlatformKimi:      {},
		PlatformMiniMax:   {},
		PlatformOther:     {},
		PlatformCodeBuddy: {},
	}
	require.Equal(t, expected, aggregateBindingFamily,
		"聚合族平台集合必须与方案 §1.1 / repository.aggregatePlatformFamily 逐字一致")
}

// TestValidateAggregateBinding 钉死收口：仅聚合族平台可置 enabled=true；
// 非聚合族平台（openai/anthropic/gemini/grok/composite）置 true 必须返回
// 400 AGGREGATE_BINDING_NOT_SUPPORTED；enabled=false 任何平台均允许。
func TestValidateAggregateBinding(t *testing.T) {
	// 聚合族平台：enabled=true / false 都通过。
	for p := range aggregateBindingFamily {
		require.NoError(t, validateAggregateBinding(p, true), "%s 是聚合族，应允许开启聚合直绑", p)
		require.NoError(t, validateAggregateBinding(p, false), "%s 关闭开关应始终允许", p)
	}

	// 非聚合族平台：enabled=true 必须被拒；enabled=false 通过。
	nonAggregate := []string{
		PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformGrok, PlatformComposite,
	}
	for _, p := range nonAggregate {
		err := validateAggregateBinding(p, true)
		require.Error(t, err, "%s 非聚合族，置 true 应被拒", p)
		require.Equal(t, "AGGREGATE_BINDING_NOT_SUPPORTED", infraerrors.Reason(err),
			"%s 应返回 AGGREGATE_BINDING_NOT_SUPPORTED", p)
		require.NoError(t, validateAggregateBinding(p, false), "%s 关闭开关应始终允许", p)
	}
}
