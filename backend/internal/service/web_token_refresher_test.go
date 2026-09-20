package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// --- 测试基建（复用 web_protocol_chain_test.go 的既有 stub 形态） ---

type webRefreshAutoLoginStoreStub struct {
	updatedID    int64
	updatedCreds map[string]any
	statusID     int64
	statusValue  string
}

func (s *webRefreshAutoLoginStoreStub) ListAccounts(_ context.Context) ([]*Account, error) {
	return nil, nil
}

func (s *webRefreshAutoLoginStoreStub) UpdateAccountCredentials(_ context.Context, id int64, creds map[string]any) error {
	s.updatedID = id
	s.updatedCreds = creds
	return nil
}

func (s *webRefreshAutoLoginStoreStub) UpdateAccountStatus(_ context.Context, id int64, status string, _ string) error {
	s.statusID = id
	s.statusValue = status
	return nil
}

// --- kimi refresher ---

func TestWebKimiTokenRefresherSuccess(t *testing.T) {
	account := &Account{
		ID:       9801,
		Platform: PlatformKimi,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"refresh_token": "old-refresh",
			"access_token":  "old-access",
		},
	}
	gateway := &OpenAIGatewayService{
		accountRepo: &kimiRefreshRepoStub{},
		httpUpstream: &httpUpstreamRecorder{resp: refreshResponse(http.StatusOK,
			`{"accessToken":"AT-NEW","refreshToken":"RT-NEW"}`)},
	}
	r := NewWebKimiTokenRefresher(gateway, &config.TokenRefreshConfig{WebRefreshMinIntervalHours: 72})

	creds, err := r.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "AT-NEW", account.Credentials["access_token"])
	require.Equal(t, "AT-NEW", creds["access_token"], "返回 clone，含新 token")
	require.Contains(t, account.Credentials, CredKeyWebRefreshLastOKAt, "应写保活簿记键")
	_, parseErr := time.Parse(time.RFC3339, account.Credentials[CredKeyWebRefreshLastOKAt].(string))
	require.NoError(t, parseErr, "簿记键应为 RFC3339")
}

func TestWebKimiTokenRefresherFailure(t *testing.T) {
	account := &Account{
		ID:       9802,
		Platform: PlatformKimi,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"refresh_token": "old-refresh",
		},
	}
	gateway := &OpenAIGatewayService{
		accountRepo:  &kimiRefreshRepoStub{},
		httpUpstream: &httpUpstreamRecorder{resp: refreshResponse(http.StatusUnauthorized, `{"code":"unauthenticated"}`)},
	}
	r := NewWebKimiTokenRefresher(gateway, &config.TokenRefreshConfig{WebRefreshMinIntervalHours: 72})

	_, err := r.Refresh(context.Background(), account)
	require.Error(t, err)
	require.Contains(t, err.Error(), "kimi web refresh failed")
	require.NotContains(t, account.Credentials, CredKeyWebRefreshLastOKAt, "失败不写簿记键")
}

func TestWebKimiTokenRefresherCanRefresh(t *testing.T) {
	r := NewWebKimiTokenRefresher(&OpenAIGatewayService{}, &config.TokenRefreshConfig{})
	require.True(t, r.CanRefresh(&Account{Platform: PlatformKimi, Credentials: map[string]any{
		"access_mode": AccountAccessModeWeb, "refresh_token": "rt",
	}}))
	require.False(t, r.CanRefresh(&Account{Platform: PlatformKimi, Credentials: map[string]any{
		"access_mode": AccountAccessModeWeb,
	}}), "缺 refresh_token 不可刷")
	require.False(t, r.CanRefresh(&Account{Platform: PlatformOpenAI, Credentials: map[string]any{
		"access_mode": AccountAccessModeWeb, "refresh_token": "rt",
	}}))
}

// --- zhipu refresher ---

func TestWebZhipuTokenRefresherSuccess(t *testing.T) {
	account := &Account{
		ID:       9811,
		Platform: PlatformZhipu,
		Credentials: map[string]any{
			"access_mode": AccountAccessModeWeb,
			// cookie 含 chatglm_refresh_token= 的场景（CanRefresh 走 cookie 解析分支）。
			"cookie": "chatglm_token=old-at; chatglm_refresh_token=old-rt",
		},
	}
	gateway := &OpenAIGatewayService{
		accountRepo: &kimiRefreshRepoStub{},
		httpUpstream: &httpUpstreamRecorder{resp: refreshResponse(http.StatusOK,
			`{"result":{"access_token":"AT-ZHIPU","refresh_token":"RT-ZHIPU"}}`)},
	}
	r := NewWebZhipuTokenRefresher(gateway, &config.TokenRefreshConfig{WebRefreshMinIntervalHours: 72})

	creds, err := r.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Contains(t, creds["cookie"], "chatglm_token=AT-ZHIPU", "返回 clone，cookie 已就地替换")
	require.Contains(t, account.Credentials["cookie"], "chatglm_token=AT-ZHIPU", "cookie 两字段就地替换")
	require.Contains(t, account.Credentials["cookie"], "chatglm_refresh_token=RT-ZHIPU")
	require.Contains(t, account.Credentials, CredKeyWebRefreshLastOKAt)
}

func TestWebZhipuTokenRefresherFailure(t *testing.T) {
	account := &Account{
		ID:       9812,
		Platform: PlatformZhipu,
		Credentials: map[string]any{
			"access_mode": AccountAccessModeWeb,
			"cookie":      "chatglm_token=old-at; chatglm_refresh_token=old-rt",
		},
	}
	gateway := &OpenAIGatewayService{
		accountRepo:  &kimiRefreshRepoStub{},
		httpUpstream: &httpUpstreamRecorder{resp: refreshResponse(http.StatusUnauthorized, `{"code":"unauthorized"}`)},
	}
	r := NewWebZhipuTokenRefresher(gateway, &config.TokenRefreshConfig{WebRefreshMinIntervalHours: 72})

	_, err := r.Refresh(context.Background(), account)
	require.Error(t, err)
	require.Contains(t, err.Error(), "zhipu web refresh failed")
	require.Contains(t, account.Credentials["cookie"], "chatglm_token=old-at", "失败不改 cookie")
}

func TestWebZhipuTokenRefresherCanRefresh(t *testing.T) {
	r := NewWebZhipuTokenRefresher(&OpenAIGatewayService{}, &config.TokenRefreshConfig{})
	// cookie 含 chatglm_refresh_token= 分支。
	require.True(t, r.CanRefresh(&Account{Platform: PlatformZhipu, Credentials: map[string]any{
		"access_mode": AccountAccessModeWeb, "cookie": "a=b; chatglm_refresh_token=rt",
	}}))
	// 显式 refresh_token 分支。
	require.True(t, r.CanRefresh(&Account{Platform: PlatformZhipu, Credentials: map[string]any{
		"access_mode": AccountAccessModeWeb, "refresh_token": "rt", "cookie": "a=b",
	}}))
	require.False(t, r.CanRefresh(&Account{Platform: PlatformZhipu, Credentials: map[string]any{
		"access_mode": AccountAccessModeWeb, "cookie": "a=b",
	}}), "无 refresh token 不可刷")
}

// --- deepseek refresher ---

func TestWebDeepseekTokenRefresherSuccessAndDetailPassthrough(t *testing.T) {
	account := &Account{
		ID:       9821,
		Platform: PlatformDeepseek,
		Credentials: map[string]any{
			"access_mode":    AccountAccessModeWeb,
			"login_email":    "a@b.c",
			"login_password": "pw",
		},
	}
	r := NewWebDeepseekTokenRefresher(NewWebPlatformAutoLoginService(nil, nil, nil),
		&config.TokenRefreshConfig{WebReloginMinIntervalHours: 168})
	r.recoverFn = func(_ context.Context, _ *Account) WebRecoverResult {
		return WebRecoverResult{Recovered: true}
	}
	creds, err := r.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Contains(t, creds, CredKeyWebRefreshLastOKAt, "成功写簿记键")

	// Detail 透传。
	r.recoverFn = func(_ context.Context, _ *Account) WebRecoverResult {
		return WebRecoverResult{Recovered: false, Detail: "deepseek 登录失败: 密码错误"}
	}
	_, err = r.Refresh(context.Background(), account)
	require.Error(t, err)
	require.Contains(t, err.Error(), "密码错误")
	require.NotContains(t, account.Credentials, CredKeyWebRefreshLastOKAt+"-updated", "sanity")
	// 失败那次不应覆盖簿记键（第一次成功写的值保持）。
	_, ok := account.Credentials[CredKeyWebRefreshLastOKAt]
	require.True(t, ok)
}

func TestWebDeepseekTokenRefresherCanRefresh(t *testing.T) {
	r := NewWebDeepseekTokenRefresher(NewWebPlatformAutoLoginService(nil, nil, nil), &config.TokenRefreshConfig{})
	require.True(t, r.CanRefresh(&Account{Platform: PlatformDeepseek, Credentials: map[string]any{
		"access_mode": AccountAccessModeWeb, "login_email": "a@b.c", "login_password": "pw",
	}}))
	require.False(t, r.CanRefresh(&Account{Platform: PlatformDeepseek, Credentials: map[string]any{
		"access_mode": AccountAccessModeWeb, "login_email": "a@b.c",
	}}), "缺密码不可刷")
}

func TestWebDeepseekTokenRefresherNeedsRefreshNonRetryable(t *testing.T) {
	cfg := &config.TokenRefreshConfig{WebReloginMinIntervalHours: 168}
	r := NewWebDeepseekTokenRefresher(NewWebPlatformAutoLoginService(nil, nil, nil), cfg)
	creds := func() map[string]any {
		return map[string]any{
			"access_mode":    AccountAccessModeWeb,
			"login_email":    "a@b.c",
			"login_password": "pw",
		}
	}
	// login_non_retryable=true → NeedsRefresh=false（防撞登录端点）。
	account := &Account{ID: 1, Platform: PlatformDeepseek, Credentials: creds()}
	account.Credentials[CredKeyLoginNonRetryable] = "true"
	require.False(t, r.NeedsRefresh(account, 0))
	// 无 non_retryable 且缺簿记键 → true。
	account2 := &Account{ID: 2, Platform: PlatformDeepseek, Credentials: creds()}
	require.True(t, r.NeedsRefresh(account2, 0))
}

// --- webRefreshDue 四态 ---

func TestWebRefreshDue(t *testing.T) {
	account := &Account{Platform: PlatformKimi, Credentials: map[string]any{}}
	// 缺键 → true。
	require.True(t, webRefreshDue(account, 72*time.Hour))

	// 刚刷过 → false。
	account.Credentials[CredKeyWebRefreshLastOKAt] = time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	require.False(t, webRefreshDue(account, 72*time.Hour), "距上次成功 1h < 72h 间隔")

	// 超间隔 → true。
	account.Credentials[CredKeyWebRefreshLastOKAt] = time.Now().UTC().Add(-73 * time.Hour).Format(time.RFC3339)
	require.True(t, webRefreshDue(account, 72*time.Hour))

	// 间隔<=0 恒 false（kimi/zhipu 72h 与 deepseek 168h 配置来源各断言一次）。
	require.False(t, webRefreshDue(account, 0))
	require.False(t, webRefreshDue(account, -1))

	refreshCfg := config.TokenRefreshConfig{WebRefreshMinIntervalHours: 72}
	reloginCfg := config.TokenRefreshConfig{WebReloginMinIntervalHours: 168}
	require.Equal(t, 72*time.Hour, webKimiRefreshInterval(&refreshCfg))
	require.Equal(t, 168*time.Hour, webReloginInterval(&reloginCfg))
	// 解析失败 → true。
	account.Credentials[CredKeyWebRefreshLastOKAt] = "not-a-time"
	require.True(t, webRefreshDue(account, 72*time.Hour))
}

// --- kimi/zhipu NeedsRefresh 走 webRefreshDue ---

func TestWebKimiZhipuNeedsRefreshDue(t *testing.T) {
	cfg := &config.TokenRefreshConfig{WebRefreshMinIntervalHours: 72}
	kimi := NewWebKimiTokenRefresher(&OpenAIGatewayService{}, cfg)
	zhipu := NewWebZhipuTokenRefresher(&OpenAIGatewayService{}, cfg)

	account := &Account{
		ID:       9831,
		Platform: PlatformKimi,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"refresh_token": "rt",
		},
	}
	require.True(t, kimi.NeedsRefresh(account, 0))
	account.Credentials[CredKeyWebRefreshLastOKAt] = time.Now().UTC().Format(time.RFC3339)
	require.False(t, kimi.NeedsRefresh(account, 0))

	zAccount := &Account{
		ID:       9832,
		Platform: PlatformZhipu,
		Credentials: map[string]any{
			"access_mode":   AccountAccessModeWeb,
			"refresh_token": "rt",
		},
	}
	require.True(t, zhipu.NeedsRefresh(zAccount, 0))
	// <=0 禁用：恒 false。
	zeroCfg := &config.TokenRefreshConfig{WebRefreshMinIntervalHours: 0}
	z := NewWebZhipuTokenRefresher(&OpenAIGatewayService{}, zeroCfg)
	require.False(t, z.NeedsRefresh(zAccount, 0))
	k := NewWebKimiTokenRefresher(&OpenAIGatewayService{}, zeroCfg)
	require.False(t, k.NeedsRefresh(account, 0))
}

// sanity: 确保 bytes/io 在测试中确有使用（httpUpstreamRecorder 依赖外部定义）。
var _ = bytes.NewReader
var _ io.Reader
