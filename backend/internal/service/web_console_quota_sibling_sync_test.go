//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 测试替身：为既有 wcqRepoStub 补 ListActive（不改既有测试文件）
// ---------------------------------------------------------------------------

// wcqRepoStub 的 ListActive 补齐方法定义落在本新文件（不改既有测试文件）：
// 默认无活跃账号，避免既有探测用例走到内嵌接口的 panic 兜底。
func (r *wcqRepoStub) ListActive(_ context.Context) ([]Account, error) {
	return nil, nil
}

// wcqSiblingRepoStub 在 wcqRepoStub 之上补 ListActive 并记录扫描次数。
type wcqSiblingRepoStub struct {
	*wcqRepoStub

	active    []Account
	listCalls int
}

func (r *wcqSiblingRepoStub) ListActive(_ context.Context) ([]Account, error) {
	r.listCalls++
	return r.active, nil
}

// wcSiblingFakeHelper 是 LocalCaptchaHelperWebAuth 的最小内存替身（固定返回成功结果）。
type wcSiblingFakeHelper struct {
	storageState string
	accountLabel string
}

func (h *wcSiblingFakeHelper) WebAuthStart(_ context.Context, _ WebAuthStartRequest) (string, error) {
	return "sess-sibling", nil
}

func (h *wcSiblingFakeHelper) WebAuthStatus(_ context.Context, _ string) (string, string, error) {
	return webConsoleAuthStatusSucceeded, "", nil
}

func (h *wcSiblingFakeHelper) WebAuthResult(_ context.Context, _, _, _ string) (string, string, error) {
	return h.storageState, h.accountLabel, nil
}

// ---------------------------------------------------------------------------
// 构造辅助
// ---------------------------------------------------------------------------

// wcqSiblingAccount 构造同控制台账号场景下的 API Key 账号。
func wcqSiblingAccount(id int64, baseURL, username string) Account {
	return Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url":              baseURL,
			"api_key":               "sk-sibling-" + username,
			"console_username":      username,
			"console_password":      "pw-should-never-leak",
			"console_storage_state": `{"cookies":[]}`,
			"model_mapping":         map[string]any{"claude-3-5-sonnet": "deepseek-v3"},
		},
	}
}

const (
	wcqSiblingCompshareBaseURL = "https://cp.compshare.cn/api/v1"
	wcqSiblingSensenovaBaseURL = "https://token.sensenova.cn/v1"
	wcqSiblingNewStorageState  = `{"cookies":[{"name":"U_JWT_TOKEN","value":"new"}],"origins":[]}`
	wcqSiblingSameStorageState = `{"cookies":[]}`
)

// ---------------------------------------------------------------------------
// 用例 1：大小写不敏感命中 + 上游隔离
// ---------------------------------------------------------------------------

func TestSiblingSync_CaseInsensitiveAndProviderIsolation(t *testing.T) {
	origin := wcqSiblingAccount(1, wcqSiblingCompshareBaseURL, "A")
	active := []Account{
		wcqSiblingAccount(1, wcqSiblingCompshareBaseURL, "A"), // 自身
		wcqSiblingAccount(2, wcqSiblingCompshareBaseURL, "a"), // 大小写不同 → 命中
		wcqSiblingAccount(3, wcqSiblingCompshareBaseURL, "B"), // 用户名不同 → 跳过
		wcqSiblingAccount(4, wcqSiblingSensenovaBaseURL, "A"), // 上游不同 → 跳过
		wcqSiblingAccount(5, wcqSiblingCompshareBaseURL, ""),  // 用户名为空 → 跳过
	}
	repo := &wcqSiblingRepoStub{wcqRepoStub: &wcqRepoStub{}, active: active}

	syncWebConsoleStorageStateToSiblings(context.Background(), repo, &origin, "A", wcqSiblingNewStorageState)

	require.Equal(t, 1, repo.patchCount(), "只有大小写不同的同上游同账号应被补写")
	call := repo.patchCalls[0]
	require.Equal(t, int64(2), call.accountID)
	require.Len(t, call.patch, 1, "只允许写 console_storage_state 一个键")
	require.Equal(t, wcqSiblingNewStorageState, call.patch[webConsoleQuotaCredentialKeyStorageState])
	require.Equal(t, "a", call.expected, "expectedUsername 必须取兄弟账号自己的库内值")

	for _, forbidden := range []string{
		"api_key", "base_url", "model_mapping", "console_password", "console_username",
	} {
		require.NotContains(t, call.patch, forbidden)
	}
}

// ---------------------------------------------------------------------------
// 用例 2：origin 用户名为空不同步
// ---------------------------------------------------------------------------

func TestSiblingSync_EmptyUsernameDoesNotSync(t *testing.T) {
	origin := wcqSiblingAccount(1, wcqSiblingCompshareBaseURL, "")
	repo := &wcqSiblingRepoStub{
		wcqRepoStub: &wcqRepoStub{},
		active:      []Account{wcqSiblingAccount(2, wcqSiblingCompshareBaseURL, "")},
	}

	syncWebConsoleStorageStateToSiblings(context.Background(), repo, &origin, "", wcqSiblingNewStorageState)

	require.Equal(t, 0, repo.listCalls, "空用户名必须按 account_id 隔离，连扫描都不做")
	require.Equal(t, 0, repo.patchCount())
}

// ---------------------------------------------------------------------------
// 用例 3：storageState 未变化 → 本账号照旧写，兄弟零 patch、零扫描
// ---------------------------------------------------------------------------

func TestSiblingSync_UnchangedStorageStateSkipsSiblingScan(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	body := `{"status":"ok","provider":"compshare","account_label":"alice",` +
		`"updated_storage_state":"` + `{\"cookies\":[]}` + `","data":` + data + `}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)

	account := wcqCompshareAccount(30)
	account.Credentials[webConsoleQuotaCredentialKeyStorageState] = wcqSiblingSameStorageState
	repo := &wcqSiblingRepoStub{
		wcqRepoStub: &wcqRepoStub{account: account},
		active:      []Account{wcqSiblingAccount(31, wcqSiblingCompshareBaseURL, account.GetCredential(webConsoleQuotaCredentialKeyUsername))},
	}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	_, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)

	require.Equal(t, 1, repo.patchCount(), "本账号既有回写行为必须保持不变")
	require.Equal(t, int64(30), repo.patchCalls[0].accountID)
	require.Equal(t, 0, repo.listCalls, "状态未变化时不得做 ListActive 全表扫描")
	require.Len(t, repo.patchCalls, 1, "兄弟账号零 patch")
}

// ---------------------------------------------------------------------------
// 用例 3b：探测路径状态变化 → 兄弟同步（覆盖 persistUpdatedStorageState 链路）
// ---------------------------------------------------------------------------

func TestSiblingSync_ProbePathSyncsChangedState(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	body := `{"status":"ok","provider":"compshare","account_label":"alice",` +
		`"updated_storage_state":{"cookies":[{"name":"U_JWT_TOKEN","value":"new"}],"origins":[]},` +
		`"data":` + data + `}`
	worker := wcqNewFakeWorker(t, http.StatusOK, body)

	account := wcqCompshareAccount(32)
	repo := &wcqSiblingRepoStub{
		wcqRepoStub: &wcqRepoStub{account: account},
		active: []Account{
			*account,
			wcqSiblingAccount(33, wcqSiblingCompshareBaseURL, "alice@example.com"),
		},
	}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	_, err := svc.ProbeLoadedAccount(context.Background(), account)
	require.NoError(t, err)

	require.Equal(t, 1, repo.listCalls)
	require.Equal(t, 2, repo.patchCount())
	require.Equal(t, int64(32), repo.patchCalls[0].accountID)
	require.Equal(t, int64(33), repo.patchCalls[1].accountID)
	require.Len(t, repo.patchCalls[1].patch, 1)
	require.Contains(t, repo.patchCalls[1].patch[webConsoleQuotaCredentialKeyStorageState], "U_JWT_TOKEN")
	require.Equal(t, "alice@example.com", repo.patchCalls[1].expected)
}

// ---------------------------------------------------------------------------
// 用例 4：交互登录路径同步
// ---------------------------------------------------------------------------

func TestSiblingSync_InteractiveLoginSyncsState(t *testing.T) {
	account := wcqCompshareAccount(40)
	repo := &wcqSiblingRepoStub{
		wcqRepoStub: &wcqRepoStub{account: account},
		active: []Account{
			*account,
			wcqSiblingAccount(41, wcqSiblingCompshareBaseURL, "alice@example.com"),
		},
	}
	helper := &wcSiblingFakeHelper{
		storageState: wcqSiblingNewStorageState,
		accountLabel: "alice@example.com",
	}
	authSvc := NewWebConsoleAuthService(nil, helper, repo)

	session := &WebConsoleAuthSession{
		ID:               "sess-sibling",
		AccountID:        40,
		Provider:         providerCompshare,
		Binding:          "acct-40-binding",
		ExpectedUsername: "Alice@Example.com",
		ExpiresAt:        time.Now().Add(time.Minute).UTC(),
	}

	status, err := authSvc.completeInteractiveLogin(context.Background(), session)
	require.NoError(t, err)
	require.Equal(t, webConsoleAuthStatusSucceeded, status.Status)

	require.Equal(t, 1, repo.listCalls)
	require.Equal(t, 2, repo.patchCount())
	require.Equal(t, int64(40), repo.patchCalls[0].accountID)
	require.Equal(t, int64(41), repo.patchCalls[1].accountID)
	require.Len(t, repo.patchCalls[1].patch, 1)
	require.Equal(t, wcqSiblingNewStorageState, repo.patchCalls[1].patch[webConsoleQuotaCredentialKeyStorageState])
	require.Equal(t, "alice@example.com", repo.patchCalls[1].expected)

	// 会话结果一次性消费。
	require.Empty(t, authSvc.sessions)

	// 不越界写无关键。
	for _, forbidden := range []string{
		"api_key", "base_url", "model_mapping", "console_password", "console_username",
	} {
		require.NotContains(t, repo.patchCalls[1].patch, forbidden)
	}
}
