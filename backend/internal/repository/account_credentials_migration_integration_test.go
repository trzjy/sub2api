//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	"github.com/stretchr/testify/require"
)

// E3 存量迁移端到端：明文存量行经 MigrateLegacyCredentials 后——
//   - 敏感子键全部密文化（DB 直查无明文）；
//   - 指纹列回填，与 E2 写路径产出的形态完全一致；
//   - 幂等：第二遍扫描全部 Unchanged；
//   - 未配置密钥时拒绝执行。
func TestMigrateLegacyCredentialsEncryptsAndBackfillsIdempotently(t *testing.T) {
	withEncryptedCredentials(t, func() {
		ctx := context.Background()
		tx := testEntTx(t)
		db := sqlExecutor(tx)

		// 存量形态：明文敏感子键、NULL 指纹（绕过 repository 写路径直插）。
		legacyWithSecret, err := tx.Client().Account.Create().
			SetName("e3-legacy-secret").
			SetPlatform(service.PlatformOpenAI).
			SetType(service.AccountTypeAPIKey).
			SetStatus(service.StatusActive).
			SetCredentials(map[string]any{
				"api_key":   "sk-e3-legacy-secret",
				"base_url":  "https://relay.example.com/v1",
				"expires_at": "1893456000",
			}).
			SetSchedulable(true).
			Save(ctx)
		require.NoError(t, err)

		// 已加密行（模拟 E2 之后的形态）：再次迁移不得重复加密/改写。
		prepared, err := prepareCredentialsForStorage(map[string]any{"api_key": "sk-e3-already-encrypted"})
		require.NoError(t, err)
		alreadyEncrypted, err := tx.Client().Account.Create().
			SetName("e3-already-encrypted").
			SetPlatform(service.PlatformOpenAI).
			SetType(service.AccountTypeAPIKey).
			SetStatus(service.StatusActive).
			SetCredentials(prepared.storage).
			SetCredentialsMAC(prepared.mac).
			SetNillableCredentialsAPIKeyMAC(prepared.apiKeyMAC).
			SetSchedulable(true).
			Save(ctx)
		require.NoError(t, err)

		stats, err := MigrateLegacyCredentials(ctx, db, 2, false, nil)
		require.NoError(t, err)
		require.GreaterOrEqual(t, stats.Scanned, 2)
		require.Equal(t, 1, stats.EncryptedRows, "only the plaintext row needs encryption")
		require.Zero(t, stats.MACBackfillRows, "prepared row already has both MACs")

		// DB 直查：明文消失、密文落库、指纹回填；非敏感子键保持明文。
		row := tx.Client().Account.Query().Where(account.IDEQ(legacyWithSecret.ID)).OnlyX(ctx)
		raw, err := json.Marshal(row.Credentials)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "sk-e3-legacy-secret")
		require.Contains(t, string(raw), "enc:v1:")
		require.Contains(t, string(raw), `"base_url":"https://relay.example.com/v1"`)
		require.NotEmpty(t, row.CredentialsMAC)

		// 密文与 repository 常规写路径完全同构：GetByID 解密回明文。
		repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
		loaded, err := repo.GetByID(ctx, legacyWithSecret.ID)
		require.NoError(t, err)
		require.Equal(t, "sk-e3-legacy-secret", loaded.Credentials["api_key"])

		// 幂等：第二遍全部 Unchanged。
		stats2, err := MigrateLegacyCredentials(ctx, db, 2, false, nil)
		require.NoError(t, err)
		require.Equal(t, stats.Scanned, stats2.Scanned)
		require.Zero(t, stats2.EncryptedRows)
		require.Zero(t, stats2.MACBackfillRows)
		require.Equal(t, stats.Scanned, stats2.Unchanged)

		// 只缺指纹的行（凭证明文但无需加密的场景不存在敏感键）→ 仅回填。
		noSecret, err := tx.Client().Account.Create().
			SetName("e3-legacy-no-secret").
			SetPlatform(service.PlatformOpenAI).
			SetType(service.AccountTypeAPIKey).
			SetStatus(service.StatusActive).
			SetCredentials(map[string]any{"base_url": "https://x.example.com"}).
			SetSchedulable(true).
			Save(ctx)
		require.NoError(t, err)
		stats3, err := MigrateLegacyCredentials(ctx, db, 10, false, nil)
		require.NoError(t, err)
		require.Equal(t, 1, stats3.MACBackfillRows, "row without secrets only needs MAC backfill")
		row3 := tx.Client().Account.Query().Where(account.IDEQ(noSecret.ID)).OnlyX(ctx)
		require.NotEmpty(t, row3.CredentialsMAC)
		// Optional（非 Nillable）string 字段 NULL 扫描为零值空串。
		require.Empty(t, row3.CredentialsAPIKeyMAC)
		_ = alreadyEncrypted
	})
}

func TestMigrateLegacyCredentialsRefusesWithoutKey(t *testing.T) {
	// 未配置密钥（withEncryptedCredentials 未生效）必须拒绝执行。
	require.False(t, credcrypt.Enabled())
	_, err := MigrateLegacyCredentials(context.Background(), nil, 0, false, nil)
	require.Error(t, err)
}
