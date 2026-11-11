//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// webConsoleQuotaTestAccount 构造满足既有校验（api 模式 api_key 非空）的
// DeepSeek APIKey 账号，并预置 Web 控制台凭据与受管快照。
func webConsoleQuotaTestAccount(id int64, baseURL string) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"base_url":                               baseURL,
			"api_key":                                "sk-test",
			webConsoleQuotaCredentialKeyUsername:     "a",
			webConsoleQuotaCredentialKeyStorageState: "S1",
		},
		Extra: map[string]any{
			webConsoleQuotaSnapshotExtraKey: map[string]any{"provider": "compshare"},
		},
	}
}

// TestUpdateAccountWithoutConsoleUsernameKeepsStoredCredentials 覆盖方案 §3.1-3：
// console_username 是身份标识而非认证秘密，未进 SensitiveCredentialKeys，必须在
// MergePreservingSensitiveCreds 的 extraPreserveKeys 里显式保留——编辑弹窗不回传时
// 不得被误清空（否则下一次保存会被误判为"换控制台账号"）。
// 入参模拟前端全量回写：携带原有 base_url 等非控制台键，唯独不回传控制台身份/登录态。
func TestUpdateAccountWithoutConsoleUsernameKeepsStoredCredentials(t *testing.T) {
	const accountID = int64(901)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: webConsoleQuotaTestAccount(accountID, "https://cp.compshare.cn/v1"),
	}}

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{
			"base_url": "https://cp.compshare.cn/v1",
			"notes":    "x",
		},
	})

	require.NoError(t, err)
	stored := repo.accounts[accountID].Credentials
	require.Equal(t, "a", stored[webConsoleQuotaCredentialKeyUsername], "未重传 console_username 必须保留库中值")
	require.Equal(t, "S1", stored[webConsoleQuotaCredentialKeyStorageState], "身份未变时 storageState 不得被清除")
	require.Contains(t, repo.accounts[accountID].Extra, webConsoleQuotaSnapshotExtraKey, "身份未变时快照不得被清除")
}

// TestUpdateAccountChangingConsoleUsernameClearsStorageStateAndSnapshot 覆盖方案 §3.1-4：
// 改换控制台账号后旧 storageState 必失效，且旧快照随同一次整行 Update 清除，
// 防止旧控制台会话/旧进度条被拿去展示新账号的量。
func TestUpdateAccountChangingConsoleUsernameClearsStorageStateAndSnapshot(t *testing.T) {
	const accountID = int64(902)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: webConsoleQuotaTestAccount(accountID, "https://cp.compshare.cn/v1"),
	}}

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{
			"base_url":                           "https://cp.compshare.cn/v1",
			webConsoleQuotaCredentialKeyUsername: "b",
		},
	})

	require.NoError(t, err)
	stored := repo.accounts[accountID].Credentials
	require.Equal(t, "b", stored[webConsoleQuotaCredentialKeyUsername])
	require.NotContains(t, stored, webConsoleQuotaCredentialKeyStorageState, "换控制台账号后旧 storageState 必须删除")
	require.NotContains(t, repo.accounts[accountID].Extra, webConsoleQuotaSnapshotExtraKey, "换控制台账号后旧快照必须删除")
}

// TestUpdateAccountBaseURLOutOfProviderClearsConsoleState 覆盖方案 §3.1-4 的另一半：
// base_url 跨出原上游后，旧控制台 storageState 不得再被拿去打新上游，旧快照同样清除。
func TestUpdateAccountBaseURLOutOfProviderClearsConsoleState(t *testing.T) {
	const accountID = int64(903)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: webConsoleQuotaTestAccount(accountID, "https://cp.compshare.cn/v1"),
	}}

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Credentials: map[string]any{"base_url": "https://api.deepseek.com"},
	})

	require.NoError(t, err)
	stored := repo.accounts[accountID].Credentials
	require.Equal(t, "https://api.deepseek.com", stored["base_url"])
	require.NotContains(t, stored, webConsoleQuotaCredentialKeyStorageState, "换上游后旧 storageState 必须删除")
	require.NotContains(t, repo.accounts[accountID].Extra, webConsoleQuotaSnapshotExtraKey, "换上游后旧快照必须删除")
}

// TestUpdateAccountWebConsoleSnapshotSurvivesFullExtraEdit 覆盖方案 §3.2-C-1：
// web_console_quota_snapshot 是服务端探针写入的受管快照键，编辑弹窗的全量 extra
// 整体替换不得用打开弹窗时的旧值覆盖回库（与 ollama/opencode 快照同型）。
func TestUpdateAccountWebConsoleSnapshotSurvivesFullExtraEdit(t *testing.T) {
	const accountID = int64(904)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: webConsoleQuotaTestAccount(accountID, "https://cp.compshare.cn/v1"),
	}}

	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{"custom": "value"},
	})

	require.NoError(t, err)
	require.Equal(t, "value", repo.accounts[accountID].Extra["custom"])
	require.Contains(t, repo.accounts[accountID].Extra, webConsoleQuotaSnapshotExtraKey, "受管快照键必须从库中原值带回")
}
