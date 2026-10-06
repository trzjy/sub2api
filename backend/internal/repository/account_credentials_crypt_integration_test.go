//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	"github.com/stretchr/testify/require"
)

const integrationCredKeyB64 = "qHgB4zsz/n318EWX04NTyvzktDbclTZNbIzCP2dMxKk="

// withEncryptedCredentials 在加密启用的进程状态下执行 fn，结束后恢复
// passthrough（Configure("", "") 即清除默认 Cipher）。credcrypt 是进程级
// 全局单例，集成测试串行执行，退出前必须还原，避免影响同包其他用例。
func withEncryptedCredentials(t *testing.T, fn func()) {
	t.Helper()
	enabled, err := credcrypt.Configure(integrationCredKeyB64, "")
	require.NoError(t, err)
	require.True(t, enabled)
	t.Cleanup(func() {
		_, _ = credcrypt.Configure("", "")
	})
	fn()
}

// 方案 A3 验收：E2 写路径加密后，DB 中直接 SELECT 看不到任何 token 明文；
// 读路径解密后 service 层拿到完整明文（双形态共存，legacy 明文行可读）。
func TestCredentialsEncryptedAtRestAndTransparentOnRead(t *testing.T) {
	withEncryptedCredentials(t, func() {
		ctx := context.Background()
		tx := testEntTx(t)
		repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

		plain := map[string]any{
			"access_token":  "at-integration-secret",
			"refresh_token": "rt-integration-secret",
			"api_key":       "sk-integration-secret",
			"expires_at":    "1893456000",
			"base_url":      "https://relay.example.com/v1",
		}
		createdAcc := &service.Account{
			Name:        "cred-crypt-e2e",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Credentials: plain,
			Extra:       map[string]any{},
			Concurrency: 1,
			Priority:    1,
			Schedulable: true,
		}
		require.NoError(t, repo.Create(ctx, createdAcc))

		// DB 层直查：任何敏感明文都不得出现；敏感子键带 enc:v1: 前缀；
		// 指纹列已维护；非敏感子键保持明文。
		row := tx.Client().Account.Query().Where(account.IDEQ(createdAcc.ID)).OnlyX(ctx)
		raw, err := json.Marshal(row.Credentials)
		require.NoError(t, err)
		stored := string(raw)
		for _, secret := range []string{"at-integration-secret", "rt-integration-secret", "sk-integration-secret"} {
			require.NotContains(t, stored, secret)
		}
		require.Contains(t, stored, "enc:v1:")
		require.Contains(t, stored, `"expires_at":"1893456000"`)
		require.Contains(t, stored, `"base_url":"https://relay.example.com/v1"`)
		require.NotEmpty(t, row.CredentialsMAC)
		require.NotEmpty(t, row.CredentialsAPIKeyMAC)

		// 读路径：service 层拿到完整明文，且指纹与明文一致。
		loaded, err := repo.GetByID(ctx, createdAcc.ID)
		require.NoError(t, err)
		require.Equal(t, plain, loaded.Credentials)

		// 整体更新走同一写路径，密文轮换（nonce 不同）后仍满足验收。
		plain["access_token"] = "at-rotated-secret"
		loaded.Credentials = plain
		require.NoError(t, repo.Update(ctx, loaded))
		reloaded, err := repo.GetByID(ctx, createdAcc.ID)
		require.NoError(t, err)
		require.Equal(t, "at-rotated-secret", reloaded.Credentials["access_token"])
		raw2, err := json.Marshal(func() any {
			r2 := tx.Client().Account.Query().Where(account.IDEQ(createdAcc.ID)).OnlyX(ctx)
			return r2.Credentials
		}())
		require.NoError(t, err)
		require.NotContains(t, string(raw2), "at-rotated-secret")
	})
}

// 双形态共存：存量明文行（无密文前缀、无指纹，模拟 E3 之前的 legacy 数据）
// 读路径原样返回；加密写入的新行不受影响。
func TestCredentialsDualFormLegacyPlaintextStillReadable(t *testing.T) {
	withEncryptedCredentials(t, func() {
		ctx := context.Background()
		tx := testEntTx(t)
		repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

		// 模拟存量行：明文 credentials、NULL 指纹（绕过 repository 写路径）。
		legacy, err := tx.Client().Account.Create().
			SetName("cred-crypt-legacy").
			SetPlatform(service.PlatformOpenAI).
			SetType(service.AccountTypeAPIKey).
			SetStatus(service.StatusActive).
			SetCredentials(map[string]any{"api_key": "sk-legacy-plain"}).
			SetSchedulable(true).
			Save(ctx)
		require.NoError(t, err)

		loaded, err := repo.GetByID(ctx, legacy.ID)
		require.NoError(t, err)
		require.Equal(t, "sk-legacy-plain", loaded.Credentials["api_key"])

		// 整体更新后落库形态变为加密（灰度推进）。
		loaded.Credentials["api_key"] = "sk-legacy-rotated"
		require.NoError(t, repo.Update(ctx, loaded))
		row := tx.Client().Account.Query().Where(account.IDEQ(legacy.ID)).OnlyX(ctx)
		require.Contains(t, row.Credentials["api_key"], "enc:v1:")
	})
}

// 加密形态下 Grok 凭证 CAS 守卫经指纹列生效：expected 与库中一致则命中，
// 不一致（并发旋转）则拒绝。
func TestGrokCredentialCASWorksOnEncryptedRows(t *testing.T) {
	withEncryptedCredentials(t, func() {
		ctx := context.Background()
		tx := testEntTx(t)
		repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

		expected := map[string]any{
			"access_token":   "at-grok-secret",
			"refresh_token":  "rt-grok-secret",
			"_token_version": int64(3),
		}
		created := &service.Account{
			Name:        "cred-crypt-grok",
			Platform:    service.PlatformGrok,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Credentials: expected,
			Extra:       map[string]any{},
			Concurrency: 1,
			Priority:    1,
			Schedulable: true,
		}
		require.NoError(t, repo.Create(ctx, created))

		// 一致 → 隔离成功。
		applied, err := repo.SetGrokOAuthErrorIfCredentialsUnchanged(
			ctx, created.ID,
			map[string]any{"access_token": "at-grok-secret", "refresh_token": "rt-grok-secret"},
			"missing refresh token",
		)
		// 该守卫额外要求 refresh_token 缺失；此处带 refresh_token，故 0 行命中，
		// 但必须走到指纹比较后"未命中"而非报错——用 refresh_token 缺失的 expected
		// 再验证一次真正的命中路径。
		require.NoError(t, err)
		require.False(t, applied)

		applied, err = repo.SetGrokOAuthErrorIfCredentialsUnchanged(
			ctx, created.ID,
			map[string]any{"access_token": "at-grok-secret", "_token_version": int64(0)},
			"missing refresh token",
		)
		require.NoError(t, err)
		// expected 文档与库中明文不一致（库里有 refresh_token），指纹不匹配 → 拒绝。
		require.False(t, applied)

		// 用严格一致的 expected（含 refresh_token）验证指纹命中的"未命中"分支
		// 与"命中"分支：把账号更新为无 refresh_token 的版本后再隔离。
		rotated := map[string]any{"access_token": "at-grok-rotated", "_token_version": int64(4)}
		require.NoError(t, repo.UpdateCredentials(ctx, created.ID, rotated))

		applied, err = repo.SetGrokOAuthErrorIfCredentialsUnchanged(
			ctx, created.ID,
			map[string]any{"access_token": "at-grok-rotated", "_token_version": int64(4)},
			"missing refresh token",
		)
		require.NoError(t, err)
		require.True(t, applied, "identical plaintext must match via credentials_mac")

		// 并发旋转后的 expected 不再匹配 → 拒绝（CAS 防覆盖语义在加密下保持）。
		created2 := &service.Account{
			Name:        "cred-crypt-grok-2",
			Platform:    service.PlatformGrok,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Credentials: map[string]any{"access_token": "at-grok-2", "_token_version": int64(1)},
			Extra:       map[string]any{},
			Concurrency: 1,
			Priority:    1,
			Schedulable: true,
		}
		require.NoError(t, repo.Create(ctx, created2))
		require.NoError(t, repo.UpdateCredentials(ctx, created2.ID,
			map[string]any{"access_token": "at-grok-2-rotated", "_token_version": int64(2)}))
		applied, err = repo.SetGrokOAuthErrorIfCredentialsUnchanged(
			ctx, created2.ID,
			map[string]any{"access_token": "at-grok-2", "_token_version": int64(1)},
			"missing refresh token",
		)
		require.NoError(t, err)
		require.False(t, applied, "stale expected credentials must not match after rotation")
	})
}

// UpdateCredentials 对"凭证未变化"的幂等更新不得误清 Ollama/探测托管 extra
// （指纹等值取代密文比较的关键回归）。
func TestUpdateCredentialsUnchangedCredentialsPreserveManagedExtraEncrypted(t *testing.T) {
	withEncryptedCredentials(t, func() {
		ctx := context.Background()
		tx := testEntTx(t)
		repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

		credentials := map[string]any{"api_key": "sk-shared", "base_url": "https://ollama.com"}
		created := &service.Account{
			Name:        "cred-crypt-ollama",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Credentials: credentials,
			Extra: map[string]any{
				service.UpstreamBillingProbeEnabledExtraKey: true,
				service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "ok"},
			},
			Concurrency: 1,
			Priority:    1,
			Schedulable: true,
		}
		require.NoError(t, repo.Create(ctx, created))

		require.NoError(t, repo.UpdateCredentials(ctx, created.ID, credentials))
		after, err := repo.GetByID(ctx, created.ID)
		require.NoError(t, err)
		require.Equal(t, true, after.Extra[service.UpstreamBillingProbeEnabledExtraKey],
			"unchanged credentials must not drop managed extra")
		require.Contains(t, after.Extra, service.UpstreamBillingProbeExtraKey)

		changed := map[string]any{"api_key": "sk-new", "base_url": "https://ollama.com"}
		require.NoError(t, repo.UpdateCredentials(ctx, created.ID, changed))
		after2, err := repo.GetByID(ctx, created.ID)
		require.NoError(t, err)
		require.NotContains(t, after2.Extra, service.UpstreamBillingProbeExtraKey,
			"changed api_key must drop stale probe snapshot")
	})
}

// ent 客户端直连场景（回避 repository 层）保留给 fixture 使用，此处引用避免 unused。
var _ = (*dbent.Client)(nil)
