package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	"github.com/stretchr/testify/require"
)

// 测试用 32 字节 base64 密钥（openssl rand -base64 32 产物格式）。
const testCredKeyB64 = "qHgB4zsz/n318EWX04NTyvzktDbclTZNbIzCP2dMxKk="

// withTestCredCipher 在加密启用状态下执行 fn，结束后恢复 passthrough 模式。
// credcrypt 是进程级全局状态，仓库测试串行执行，退出前必须还原，
// 避免影响同包其他依赖明文形态的测试。
func withTestCredCipher(t *testing.T, fn func()) {
	t.Helper()
	enabled, err := credcrypt.Configure(testCredKeyB64, "")
	require.NoError(t, err)
	require.True(t, enabled)
	defer func() {
		_, err := credcrypt.Configure("", "")
		require.NoError(t, err)
	}()
	fn()
}

func TestPrepareCredentialsForStorage_PassthroughKeepsPlaintext(t *testing.T) {
	// passthrough（未配置密钥）：storage 与明文一致、无 enc:v1: 前缀，指纹照常计算。
	in := map[string]any{
		"access_token": "at-secret",
		"api_key":      "sk-plain",
		"base_url":     "https://example.com",
		"expires_at":   "1893456000",
		"empty_token":  "",
	}
	prepared, err := prepareCredentialsForStorage(in)
	require.NoError(t, err)

	require.Equal(t, "at-secret", prepared.storage["access_token"])
	require.Equal(t, "sk-plain", prepared.storage["api_key"])
	require.False(t, credcrypt.IsEncrypted(prepared.storage["access_token"].(string)))

	expectedMac, err := credentialsMACOf(in)
	require.NoError(t, err)
	require.Equal(t, expectedMac, prepared.mac)
	require.NotEmpty(t, prepared.mac)

	apiKeyMac, err := credentialsApiKeyMACOf(in)
	require.NoError(t, err)
	require.NotNil(t, apiKeyMac)
	require.Equal(t, *apiKeyMac, *prepared.apiKeyMAC)
}

func TestPrepareCredentialsForStorage_EncryptsSensitiveKeysOnly(t *testing.T) {
	withTestCredCipher(t, func() {
		in := map[string]any{
			"access_token": "at-secret",
			"refresh_token": "",
			"api_key":      "sk-live-123",
			"base_url":     "https://example.com",
			"expires_at":   "1893456000",
			"session_key":  12345, // 非字符串值不加密
		}
		prepared, err := prepareCredentialsForStorage(in)
		require.NoError(t, err)

		// 敏感字符串子键全部带前缀。
		for _, key := range []string{"access_token", "api_key"} {
			value, ok := prepared.storage[key].(string)
			require.True(t, ok, key)
			require.True(t, credcrypt.IsEncrypted(value), key)
			require.NotContains(t, value, "secret")
			require.NotContains(t, value, "sk-live-123")
		}
		// 空字符串不加密（保持既有"refresh_token 是否为空"类 SQL 守卫语义）。
		require.Equal(t, "", prepared.storage["refresh_token"])
		// 非敏感键与非字符串值原样保留。
		require.Equal(t, "https://example.com", prepared.storage["base_url"])
		require.Equal(t, "1893456000", prepared.storage["expires_at"])
		require.Equal(t, 12345, prepared.storage["session_key"])
		// 入参 map 不被修改。
		require.Equal(t, "at-secret", in["access_token"])
	})
}

func TestCredentialsRoundtrip_EncryptedStorageDecryptsOnRead(t *testing.T) {
	in := map[string]any{
		"access_token":  "at-secret",
		"refresh_token": "rt-secret",
		"api_key":       "sk-live-123",
		"base_url":      "https://example.com",
	}
	withTestCredCipher(t, func() {
		prepared, err := prepareCredentialsForStorage(in)
		require.NoError(t, err)

		// 模拟 JSONB 落库再读出（json.Marshal/Unmarshal 即 ent 的实际路径）。
		raw, err := json.Marshal(prepared.storage)
		require.NoError(t, err)
		var stored map[string]any
		require.NoError(t, json.Unmarshal(raw, &stored))

		plain, err := decryptCredentialsMap(stored)
		require.NoError(t, err)
		require.Equal(t, in, plain)

		// 明文形态（legacy 行）读路径原样返回。
		legacyPlain, err := decryptCredentialsMap(map[string]any{"access_token": "legacy-plain"})
		require.NoError(t, err)
		require.Equal(t, "legacy-plain", legacyPlain["access_token"])
	})

	// 未配置密钥时读到密文：fail loud（ErrNotConfigured），不放行密文。
	_, err := decryptCredentialsMap(map[string]any{"access_token": credcrypt.Prefix + "AAAA"})
	require.ErrorIs(t, err, credcrypt.ErrNotConfigured)
}

func TestCredentialsMAC_StableWithinMode(t *testing.T) {
	in := map[string]any{
		"access_token": "at-secret",
		"api_key":      "sk-live-123",
		"base_url":     "https://example.com",
	}
	passthroughPrepared, err := prepareCredentialsForStorage(in)
	require.NoError(t, err)

	// 同一模式下指纹按明文确定（SQL 等值比较的前提）。
	again, err := credentialsMACOf(in)
	require.NoError(t, err)
	require.Equal(t, passthroughPrepared.mac, again)

	var encryptedMac string
	withTestCredCipher(t, func() {
		encryptedMac, err = credentialsMACOf(in)
		require.NoError(t, err)
		// 加密模式下同样确定，且与未配置密钥模式不同（MAC 密钥随主密钥派生）。
		againEncrypted, err := credentialsMACOf(in)
		require.NoError(t, err)
		require.Equal(t, encryptedMac, againEncrypted)
		require.NotEqual(t, passthroughPrepared.mac, encryptedMac)
	})

	// 明文不同则指纹不同。
	changed := map[string]any{"access_token": "at-other", "api_key": "sk-live-123", "base_url": "https://example.com"}
	changedMac, err := credentialsMACOf(changed)
	require.NoError(t, err)
	require.NotEqual(t, passthroughPrepared.mac, changedMac)
}

func TestCredentialsMACKey_ChangesWhenKeyConfigured(t *testing.T) {
	// MAC 密钥绑定主密钥派生：配置/取消配置主密钥会改变派生结果。
	// 后果（设计如此）：主密钥启用/轮换后存量行 MAC 失配，CAS 守卫按
	// 不匹配处理（安全侧失败），由 E3 迁移/下次整体写回填。
	before := append([]byte(nil), credcrypt.MACKey()...)
	withTestCredCipher(t, func() {
		after := credcrypt.MACKey()
		require.NotEqual(t, before, after)
	})
	require.Equal(t, before, credcrypt.MACKey())
}

func TestCredentialsApiKeyMAC_NilWhenAbsentOrEmpty(t *testing.T) {
	noKey, err := credentialsApiKeyMACOf(map[string]any{"base_url": "https://x"})
	require.NoError(t, err)
	require.Nil(t, noKey)

	emptyKey, err := credentialsApiKeyMACOf(map[string]any{"api_key": ""})
	require.NoError(t, err)
	require.Nil(t, emptyKey)

	withKey, err := credentialsApiKeyMACOf(map[string]any{"api_key": "sk-1"})
	require.NoError(t, err)
	require.NotNil(t, withKey)
	require.Len(t, *withKey, 64) // sha256 hex
}

func TestCredentialsMACOfJSONString_MatchesMapMAC(t *testing.T) {
	in := map[string]any{"access_token": "at", "_token_version": 3, "api_key": "sk"}
	want, err := credentialsMACOf(in)
	require.NoError(t, err)

	raw, err := json.Marshal(in)
	require.NoError(t, err)
	got, err := credentialsMACOfJSONString(string(raw))
	require.NoError(t, err)
	require.Equal(t, want, got)

	// 非法 JSON 报错。
	_, err = credentialsMACOfJSONString("{not-json")
	require.Error(t, err)
}
