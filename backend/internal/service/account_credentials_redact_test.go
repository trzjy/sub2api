//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMergePreservingSensitiveCreds_PreservesSensitiveWhenIncomingMissing(t *testing.T) {
	existing := map[string]any{
		"refresh_token": "rt-old",
		"access_token":  "at-old",
		"api_key":       "sk-old",
		"base_url":      "https://old.example.com",
	}
	incoming := map[string]any{
		"base_url":      "https://new.example.com",
		"model_mapping": map[string]any{"foo": "bar"},
	}

	out := MergePreservingSensitiveCreds(existing, incoming)

	require.Equal(t, "rt-old", out["refresh_token"], "incoming 没传 refresh_token，应保留 existing")
	require.Equal(t, "at-old", out["access_token"])
	require.Equal(t, "sk-old", out["api_key"])
	require.Equal(t, "https://new.example.com", out["base_url"], "非敏感键由 incoming 决定")
	require.Equal(t, map[string]any{"foo": "bar"}, out["model_mapping"])
}

func TestMergePreservingSensitiveCreds_OverwritesWhenIncomingProvidesSensitive(t *testing.T) {
	existing := map[string]any{
		"refresh_token": "rt-old",
		"api_key":       "sk-old",
	}
	incoming := map[string]any{
		"refresh_token": "rt-new",
		// 显式没传 api_key —— 应保留
	}
	out := MergePreservingSensitiveCreds(existing, incoming)
	require.Equal(t, "rt-new", out["refresh_token"], "incoming 显式传入应覆盖")
	require.Equal(t, "sk-old", out["api_key"], "incoming 没传应保留")
}

func TestMergePreservingSensitiveCreds_DoesNotMutateInputs(t *testing.T) {
	existing := map[string]any{"refresh_token": "rt"}
	incoming := map[string]any{"base_url": "x"}

	_ = MergePreservingSensitiveCreds(existing, incoming)

	require.Equal(t, "rt", existing["refresh_token"])
	require.NotContains(t, existing, "base_url")
	require.Equal(t, "x", incoming["base_url"])
	require.NotContains(t, incoming, "refresh_token")
}

func TestMergePreservingSensitiveCreds_NilInputs(t *testing.T) {
	out := MergePreservingSensitiveCreds(nil, map[string]any{"base_url": "x"})
	require.Equal(t, "x", out["base_url"])
	require.NotContains(t, out, "refresh_token")

	out2 := MergePreservingSensitiveCreds(map[string]any{"refresh_token": "rt"}, nil)
	require.Equal(t, "rt", out2["refresh_token"])
}

func TestMergePreservingSensitiveCreds_NonSensitiveDeletionAllowed(t *testing.T) {
	existing := map[string]any{
		"refresh_token": "rt",
		"base_url":      "https://old",
		"project_id":    "p1",
	}
	incoming := map[string]any{
		"base_url": "https://new",
		// 不带 project_id —— 等同删除（非敏感键由 incoming 决定）
	}
	out := MergePreservingSensitiveCreds(existing, incoming)
	require.Equal(t, "rt", out["refresh_token"], "敏感键保留")
	require.Equal(t, "https://new", out["base_url"])
	require.NotContains(t, out, "project_id", "非敏感键 incoming 不传 = 删除")
}

func TestIsSensitiveCredentialKey(t *testing.T) {
	require.True(t, IsSensitiveCredentialKey("refresh_token"))
	require.True(t, IsSensitiveCredentialKey("api_key"))
	require.True(t, IsSensitiveCredentialKey("private_key"))
	require.False(t, IsSensitiveCredentialKey("base_url"))
	require.False(t, IsSensitiveCredentialKey(""))
	require.False(t, IsSensitiveCredentialKey("model_mapping"))
}

// web 自动续期凭证键不在全局 SensitiveCredentialKeys 内（login_email 需回显编辑框），
// 经 extraPreserveKeys 追加保留语义：incoming 没带 → 保留 existing，绝不误清空。
func TestMergePreservingSensitiveCreds_ExtraPreserveKeys(t *testing.T) {
	existing := map[string]any{
		"login_email":     "u@x.com",
		"login_password":  "pw-old",
		"login_phone":     "13800000000",
		"login_refresh_token": "rt-old",
		"cookie":          "COOKIE-OLD",
	}
	incoming := map[string]any{
		"cookie": "COOKIE-NEW",
		// 没带 login_* —— 应保留
	}
	out := MergePreservingSensitiveCreds(existing, incoming,
		"login_email", "login_password", "login_phone", "login_refresh_token")
	require.Equal(t, "u@x.com", out["login_email"])
	require.Equal(t, "pw-old", out["login_password"])
	require.Equal(t, "13800000000", out["login_phone"])
	require.Equal(t, "rt-old", out["login_refresh_token"])
	require.Equal(t, "COOKIE-NEW", out["cookie"], "全局敏感键 cookie 仍按原语义：incoming 提供则覆盖")
}

func TestMergePreservingSensitiveCreds_ExtraPreserveKeysOverwritesWhenIncomingProvides(t *testing.T) {
	existing := map[string]any{
		"login_email":    "old@x.com",
		"login_password": "pw-old",
	}
	incoming := map[string]any{
		"login_email":    "new@x.com",
		"login_password": "pw-new",
	}
	out := MergePreservingSensitiveCreds(existing, incoming,
		"login_email", "login_password", "login_phone", "login_refresh_token")
	require.Equal(t, "new@x.com", out["login_email"], "incoming 显式传入应覆盖")
	require.Equal(t, "pw-new", out["login_password"])
}

func TestMergePreservingSensitiveCreds_ExtraPreserveKeysNotIncludedByDefault(t *testing.T) {
	existing := map[string]any{"login_email": "u@x.com"}
	incoming := map[string]any{"cookie": "ck"}
	out := MergePreservingSensitiveCreds(existing, incoming)
	require.NotContains(t, out, "login_email", "不传 extraPreserveKeys 时不应保留 login_*（默认行为不变）")
}
