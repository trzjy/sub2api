package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// ---------------------------------------------------------------------------
// 测试依赖桩
// ---------------------------------------------------------------------------

// autoLoginUpstream 按请求路径脚本化返回响应，覆盖 deepseek 登录 / zhipu / kimi 续期。
type autoLoginUpstream struct {
	mu                   sync.Mutex
	requests             []*http.Request
	loginBizCode         int64
	loginHTTPStatus      int
	deepseekVerifyStatus int
}

func newMockResp(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func (m *autoLoginUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	switch {
	case strings.HasSuffix(req.URL.Path, WebDeepseekLoginEndpoint):
		body := fmt.Sprintf(`{"code":0,"data":{"biz_code":%d}}`, m.loginBizCode)
		h := http.Header{}
		if m.loginBizCode == 0 {
			h.Add("Set-Cookie", "ds_session_id=abc123; Path=/; Domain=.deepseek.com")
			h.Add("Set-Cookie", "user=me; Path=/")
		}
		return newMockResp(m.loginHTTPStatus, h, body), nil
	case strings.HasSuffix(req.URL.Path, webDeepseekPoWChallengePath):
		status := m.deepseekVerifyStatus
		if status == 0 {
			status = http.StatusOK
		}
		return newMockResp(status, http.Header{}, `{"code":0,"data":{"biz_code":0,"biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"6de3393aba4cece63e3e6a761752722b05f2cfe531bc1b5c82e01985e93fddd2","salt":"salt123","signature":"sig","difficulty":43,"expire_at":1739764288699}}}}`), nil
	}
	return newMockResp(http.StatusOK, http.Header{}, "{}"), nil
}

func (m *autoLoginUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return m.Do(req, "", 0, 0)
}

var _ HTTPUpstream = (*autoLoginUpstream)(nil)

// autoLoginStore 是 AutoLoginAccountStore 的内存实现，记录每次凭据/状态更新。
type autoLoginStore struct {
	mu       sync.Mutex
	accounts []*Account
	creds    map[int64]map[string]any
	statuses map[int64]statusUpdate
}

type statusUpdate struct {
	status string
	errMsg string
}

func newAutoLoginStore(accounts ...*Account) *autoLoginStore {
	return &autoLoginStore{
		accounts: accounts,
		creds:    map[int64]map[string]any{},
		statuses: map[int64]statusUpdate{},
	}
}

func (m *autoLoginStore) ListAccounts(ctx context.Context) ([]*Account, error) {
	return m.accounts, nil
}

func (m *autoLoginStore) UpdateAccountCredentials(ctx context.Context, id int64, c map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]any, len(c))
	for k, v := range c {
		cp[k] = v
	}
	m.creds[id] = cp
	for _, a := range m.accounts {
		if a.ID == id {
			a.Credentials = cp
		}
	}
	return nil
}

func (m *autoLoginStore) UpdateAccountStatus(ctx context.Context, id int64, status, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[id] = statusUpdate{status, errMsg}
	for _, a := range m.accounts {
		if a.ID == id {
			a.Status = status
			a.ErrorMessage = errMsg
		}
	}
	return nil
}

func newTestAutoLoginService(store *autoLoginStore, up *autoLoginUpstream) *WebPlatformAutoLoginService {
	cfg := &config.Config{Security: config.SecurityConfig{
		URLAllowlist: config.URLAllowlistConfig{Enabled: false},
	}}
	return NewWebPlatformAutoLoginService(store, up, cfg)
}

// ---------------------------------------------------------------------------
// deepseek：无头密码登录
// ---------------------------------------------------------------------------

func TestWebPlatformAutoLogin_DeepseekLoginSuccess(t *testing.T) {
	acc := &Account{
		ID:       1,
		Platform: PlatformDeepseek,
		Status:   StatusError,
		Credentials: map[string]any{
			"access_mode":        AccountAccessModeWeb,
			CredKeyLoginEmail:    "u@x.com",
			CredKeyLoginPassword: "pw",
		},
	}
	store := newAutoLoginStore(acc)
	up := &autoLoginUpstream{loginBizCode: 0, loginHTTPStatus: http.StatusOK}
	svc := newTestAutoLoginService(store, up)

	// 单元：LoginByEmail 直接返回整串 Cookie 头。
	cookie, err := svc.LoginByEmail(context.Background(), acc)
	require.NoError(t, err)
	require.Equal(t, "ds_session_id=abc123; user=me", cookie)

	// 两级恢复：deepseek 成功 → cookie 写回、fail_count 清零、状态转 active。
	res := svc.RecoverAccount(context.Background(), acc)
	require.True(t, res.Recovered)
	require.Equal(t, StatusActive, acc.Status)
	require.Equal(t, "", acc.ErrorMessage)

	merged := store.creds[1]
	require.Equal(t, "ds_session_id=abc123; user=me", merged["cookie"])
	require.Equal(t, "0", fmt.Sprint(merged[CredKeyLoginFailCount]))
	require.NotEmpty(t, merged[CredKeyLoginLastAt])
	require.Equal(t, "false", merged[CredKeyLoginNonRetryable])
	require.NotEmpty(t, merged[CredKeyLoginDeviceID]) // 首次生成后复用
}

func TestWebPlatformAutoLogin_DeepseekLoginVerificationFailNoWrite(t *testing.T) {
	acc := &Account{
		ID:       11,
		Platform: PlatformDeepseek,
		Status:   StatusError,
		Credentials: map[string]any{
			"access_mode":        AccountAccessModeWeb,
			CredKeyLoginEmail:    "u@x.com",
			CredKeyLoginPassword: "pw",
			"cookie":             "OLDCOOKIE",
		},
	}
	store := newAutoLoginStore(acc)
	up := &autoLoginUpstream{loginBizCode: 0, loginHTTPStatus: http.StatusOK, deepseekVerifyStatus: http.StatusForbidden}
	svc := newTestAutoLoginService(store, up)

	cookie, err := svc.LoginByEmail(context.Background(), acc)
	require.Error(t, err)
	require.Empty(t, cookie)
	require.Empty(t, store.creds[11], "verification failure must not persist credentials")
	require.Equal(t, "OLDCOOKIE", acc.GetCredential("cookie"), "verification failure must not mutate account credentials")
}

func TestWebPlatformAutoLogin_DeepseekCode2(t *testing.T) {
	acc := &Account{
		ID:       2,
		Platform: PlatformDeepseek,
		Status:   StatusError,
		Credentials: map[string]any{
			"access_mode":         AccountAccessModeWeb,
			CredKeyLoginEmail:     "u@x.com",
			CredKeyLoginPassword:  "bad",
			CredKeyLoginFailCount: 1,
		},
	}
	store := newAutoLoginStore(acc)
	up := &autoLoginUpstream{loginBizCode: 2, loginHTTPStatus: http.StatusOK}
	svc := newTestAutoLoginService(store, up)

	res := svc.RecoverAccount(context.Background(), acc)
	require.False(t, res.Recovered)
	require.Contains(t, res.Detail, "密码")
	require.Equal(t, StatusError, acc.Status)
	require.Equal(t, "2", fmt.Sprint(store.creds[2][CredKeyLoginFailCount])) // 1+1
	require.Equal(t, "true", store.creds[2][CredKeyLoginNonRetryable])       // 不可重试
	require.Contains(t, store.creds[2][CredKeyLoginLastError], "密码")
}

func TestWebPlatformAutoLogin_DeepseekCode10Banned(t *testing.T) {
	acc := &Account{
		ID:       3,
		Platform: PlatformDeepseek,
		Status:   StatusError,
		Credentials: map[string]any{
			"access_mode":        AccountAccessModeWeb,
			CredKeyLoginEmail:    "u@x.com",
			CredKeyLoginPassword: "pw",
		},
	}
	store := newAutoLoginStore(acc)
	up := &autoLoginUpstream{loginBizCode: 10, loginHTTPStatus: http.StatusOK}
	svc := newTestAutoLoginService(store, up)

	res := svc.RecoverAccount(context.Background(), acc)
	require.False(t, res.Recovered)
	require.Contains(t, res.Detail, "封禁")
	require.Equal(t, "true", store.creds[3][CredKeyLoginNonRetryable])
	require.Equal(t, "1", fmt.Sprint(store.creds[3][CredKeyLoginFailCount]))
}

func TestWebPlatformAutoLogin_ZhipuKimiRecoverDelegatesToSemiAuto(t *testing.T) {
	// 裁定（2026-09-21）：auto_login 侧 refresh 实现已删除（单口径收敛到转发链
	// refreshWebKimiAccessToken / refreshWebZhipuAccessToken），RecoverAccount 对
	// zhipu/kimi 不再自动续期，直接要求半自动短信登录。
	for _, platform := range []string{PlatformZhipu, PlatformKimi} {
		acc := &Account{
			ID:          60,
			Platform:    platform,
			Status:      StatusError,
			Credentials: map[string]any{"access_mode": AccountAccessModeWeb},
		}
		store := newAutoLoginStore(acc)
		svc := newTestAutoLoginService(store, &autoLoginUpstream{})

		res := svc.RecoverAccount(context.Background(), acc)
		require.False(t, res.Recovered, platform)
		require.True(t, res.NeedsSemiAuto, platform)
	}
}

func TestWebPlatformAutoLogin_DeepseekFailNoSemiAuto(t *testing.T) {
	acc := &Account{
		ID:       7,
		Platform: PlatformDeepseek,
		Status:   StatusError,
		Credentials: map[string]any{
			"access_mode":        AccountAccessModeWeb,
			CredKeyLoginEmail:    "u@x.com",
			CredKeyLoginPassword: "bad",
		},
	}
	store := newAutoLoginStore(acc)
	up := &autoLoginUpstream{loginBizCode: 2}
	svc := newTestAutoLoginService(store, up)

	res := svc.RecoverAccount(context.Background(), acc)
	require.False(t, res.Recovered)
	require.False(t, res.NeedsSemiAuto) // deepseek 全自动，绝不需半自动
}

// ---------------------------------------------------------------------------
// 凭据合并：保留既有键
// ---------------------------------------------------------------------------

func TestWebPlatformAutoLogin_CredentialMergeKeepsExisting(t *testing.T) {
	acc := &Account{
		ID:       9,
		Platform: PlatformDeepseek,
		Status:   StatusError,
		Credentials: map[string]any{
			"access_mode":        AccountAccessModeWeb,
			CredKeyLoginEmail:    "u@x.com",
			CredKeyLoginPassword: "pw",
			"cookie":             "OLDCOOKIE",
			"model_mapping":      `{"a":"b"}`,
			"access_token":       "AT-OLD",
		},
	}
	store := newAutoLoginStore(acc)
	up := &autoLoginUpstream{loginBizCode: 0, loginHTTPStatus: http.StatusOK}
	svc := newTestAutoLoginService(store, up)

	res := svc.RecoverAccount(context.Background(), acc)
	require.True(t, res.Recovered)
	merged := store.creds[9]
	require.Equal(t, "ds_session_id=abc123; user=me", merged["cookie"]) // 登录成功覆盖为新 Cookie
	require.Equal(t, `{"a":"b"}`, merged["model_mapping"])              // 既有键保留
	require.Equal(t, "AT-OLD", merged["access_token"])                  // 既有键保留
	require.Equal(t, "u@x.com", merged[CredKeyLoginEmail])              // 既有 login_* 保留
}

func TestWebPlatformAutoLogin_FailureKeepsCookie(t *testing.T) {
	acc := &Account{
		ID:       10,
		Platform: PlatformDeepseek,
		Status:   StatusError,
		Credentials: map[string]any{
			"access_mode":        AccountAccessModeWeb,
			CredKeyLoginEmail:    "u@x.com",
			CredKeyLoginPassword: "bad",
			"cookie":             "KEEPME",
		},
	}
	store := newAutoLoginStore(acc)
	up := &autoLoginUpstream{loginBizCode: 2}
	svc := newTestAutoLoginService(store, up)

	svc.RecoverAccount(context.Background(), acc)
	require.Equal(t, "KEEPME", store.creds[10]["cookie"]) // 失败不得覆盖既有 cookie
}

// ---------------------------------------------------------------------------
// 错误细化表（三平台共用）
// ---------------------------------------------------------------------------

func TestWebPlatformErrorDetail_Table(t *testing.T) {
	cases := []struct {
		platform  string
		code      int64
		kind      string
		wantTitle string
	}{
		{PlatformDeepseek, WebLoginCodeBadCredential, WebLoginKindLogin, "登录失败：邮箱或密码错误"},
		{PlatformDeepseek, WebLoginCodeBanned, WebLoginKindLogin, "账号已被封禁"},
		{PlatformDeepseek, WebLoginCodeAuthExpired, WebLoginKindLogin, "登录态已失效"},
		{PlatformDeepseek, WebLoginCodeRateLimited, WebLoginKindLogin, "请求过于频繁"},
		{PlatformDeepseek, WebLoginCodeHTTPTooMany, WebLoginKindLogin, "请求过于频繁"},
		{PlatformDeepseek, WebLoginCodePoW1, WebLoginKindLogin, "需要人机验证"},
		{PlatformDeepseek, WebLoginCodePoW2, WebLoginKindLogin, "需要人机验证"},
		{PlatformZhipu, WebLoginCodeAuthExpired2, WebLoginKindRefresh, "登录态已失效"},
	}
	for _, c := range cases {
		title, hint := WebPlatformErrorDetail(c.platform, c.code, c.kind)
		require.Equal(t, c.wantTitle, title, "platform=%s code=%d", c.platform, c.code)
		require.NotEmpty(t, hint)
		// 安全红线：文案绝不泄露凭据。
		require.NotContains(t, title, "Cookie")
		require.NotContains(t, title, "password")
		require.NotContains(t, hint, "ds_session_id")
		require.NotContains(t, hint, "chatglm_token")
		require.NotContains(t, hint, "accessToken")
	}
	// WAF 经 kind 触发。
	title, hint := WebPlatformErrorDetail(PlatformDeepseek, 0, WebLoginKindWAF)
	require.Contains(t, title, "WAF")
	require.NotEmpty(t, hint)
	// 成功码 → 空文案。
	title, _ = WebPlatformErrorDetail(PlatformDeepseek, WebLoginCodeSuccess, WebLoginKindLogin)
	require.Equal(t, "", title)
}
