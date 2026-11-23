//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// sensenova 接入的解耦断言（2026-10-31）。
//
// 响应侧身份净化（模型回写 / 错误体净化 / 指纹剥离）由账号级开关
// response_identity_mask 独立控制，与老的身份掩码开关解耦：只开响应侧净化
// 开关即触发净化，未开任何开关的账号不触发净化，老开关
// mask_upstream_identity 的净化行为保持不变。

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

// TestResponseIdentityMaskKeepsLegacyMaskBehavior 断言既有掩码账号行为不变：
// 老开关 mask_upstream_identity=true 的账号在响应侧仍是掩码账号。
func TestResponseIdentityMaskKeepsLegacyMaskBehavior(t *testing.T) {
	legacy := svDecoupledTestAccount(9103, map[string]any{"mask_upstream_identity": true})

	require.True(t, IsIdentityMaskedAccount(legacy), "老开关账号仍是掩码账号")
	require.True(t, IsResponseIdentityMaskedAccount(legacy), "老开关账号仍触发响应侧净化")
}
