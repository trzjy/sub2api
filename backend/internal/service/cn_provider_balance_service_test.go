package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type cnBalanceResponseUpstream struct {
	statusCode int
	body       string
}

func (u *cnBalanceResponseUpstream) Do(
	_ *http.Request,
	_ string,
	_ int64,
	_ int,
) (*http.Response, error) {
	return &http.Response{
		StatusCode: u.statusCode,
		Body:       io.NopCloser(strings.NewReader(u.body)),
		Header:     make(http.Header),
	}, nil
}

func (u *cnBalanceResponseUpstream) DoWithTLS(
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

type cnBalanceProbeRepo struct {
	AccountRepository
	account     *Account
	extraWrites []map[string]any
}

func (r *cnBalanceProbeRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return r.account, nil
}

func (r *cnBalanceProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.extraWrites = append(r.extraWrites, updates)
	return nil
}

func newDeepSeekBalanceProbeAccount() *Account {
	return &Account{
		ID:       42,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"account_mode": AccountModePayG,
			"api_key":      "sk-test",
			"base_url":     "https://relay.example.com",
		},
	}
}

func TestCNProviderBalanceService_DeepSeekInvalidBalancePayloadDoesNotBecomeZero(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError string
	}{
		{
			name:      "missing balance infos",
			body:      `{"data":{"models":["deepseek-v4-flash"]}}`,
			wantError: "missing balance_infos",
		},
		{
			name:      "empty balance infos",
			body:      `{"is_available":true,"balance_infos":[]}`,
			wantError: "no valid balance entries",
		},
		{
			name:      "invalid balance value",
			body:      `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"not-a-number"}]}`,
			wantError: "no valid balance entries",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &cnBalanceProbeRepo{account: newDeepSeekBalanceProbeAccount()}
			upstream := &cnBalanceResponseUpstream{statusCode: http.StatusOK, body: tt.body}
			svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

			result, err := svc.QueryBalance(context.Background(), repo.account.ID)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.Success)
			require.Contains(t, result.Error, tt.wantError)
			require.Empty(t, result.Balances)
			require.Empty(t, repo.extraWrites, "invalid relay balance payload must not persist a synthetic zero balance")
		})
	}
}

func TestCNProviderBalanceService_DeepSeekValidZeroBalanceRemainsSuccessful(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newDeepSeekBalanceProbeAccount()}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"0"}]}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.False(t, result.Available)
	require.Equal(t, "CNY", result.Currency)
	require.Zero(t, result.Balance)
	require.Len(t, result.Balances, 1)
	require.Len(t, repo.extraWrites, 1, "a valid upstream zero balance must still be persisted")
}

// 记录请求 URL 的上游桩，用于断言覆盖地址是否被实际使用。
type cnBalanceCaptureUpstream struct {
	cnBalanceResponseUpstream
	lastURL  string
	lastAuth string
}

func (u *cnBalanceCaptureUpstream) Do(
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
) (*http.Response, error) {
	u.lastURL = req.URL.String()
	u.lastAuth = req.Header.Get("Authorization")
	return u.cnBalanceResponseUpstream.Do(req, proxyURL, accountID, accountConcurrency)
}

func newRelayBalanceProbeAccount(probe map[string]any) *Account {
	account := newDeepSeekBalanceProbeAccount()
	account.Credentials["api_protocol"] = "anthropic"
	account.Credentials["base_url"] = "https://inferaiapi.example.com"
	account.Credentials[BalanceProbeConfigCredentialKey] = probe
	return account
}

// 同程序中转账号：anthropic 协议下官方余额地址本应回退到 api.deepseek.com，
// 但账号配了余额探测地址时必须走覆盖地址并按 /v1/usage 解析。
func TestCNProviderBalanceService_RelayOverrideUsesConfiguredURL(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled":     true,
		"url":         "https://inferaiapi.example.com/v1/usage",
		"bearer_auth": true,
	})}
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"unrestricted","remaining":12.5,"unit":"USD","isValid":true,"planName":"DeepSeek订阅"}`,
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, "https://inferaiapi.example.com/v1/usage", upstream.lastURL)
	require.Equal(t, "Bearer sk-test", upstream.lastAuth)
	require.Equal(t, 12.5, result.Balance)
	require.Equal(t, "USD", result.Currency)
	require.False(t, result.Unlimited)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, 12.5, repo.extraWrites[0]["deepseek_balance"])
	require.Equal(t, false, repo.extraWrites[0]["deepseek_balance_unlimited"])
}

// 上游订阅制不限量（remaining<0）：成功但标记 Unlimited，不落数字余额明细，
// 展示用 plan_name 落快照。
func TestCNProviderBalanceService_RelayOverrideUnlimitedSubscription(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled":     true,
		"url":         "https://inferaiapi.example.com/v1/usage",
		"bearer_auth": true,
	})}
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"unrestricted","remaining":-1,"unit":"USD","isValid":true,"planName":"DeepSeek订阅"}`,
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, result.Unlimited)
	require.Equal(t, "DeepSeek订阅", result.PlanName)
	require.Empty(t, result.Balances)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, true, repo.extraWrites[0]["deepseek_balance_unlimited"])
	require.Equal(t, "DeepSeek订阅", repo.extraWrites[0]["deepseek_balance_plan_name"])
}

// 覆盖地址返回 401：保留 Authentication failed 文案，不抛错、不落快照。
func TestCNProviderBalanceService_RelayOverrideAuthFailure(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled":     true,
		"url":         "https://inferaiapi.example.com/v1/usage",
		"bearer_auth": true,
	})}
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusUnauthorized,
		body:       `{"code":"API_KEY_REQUIRED"}`,
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "Authentication failed (HTTP 401)")
	require.Empty(t, repo.extraWrites)
}

// 余额探测开关关闭时不走覆盖地址：anthropic 协议 deepseek 回退官方地址。
func TestCNProviderBalanceService_RelayOverrideDisabledFallsBackToOfficial(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled": false,
		"url":     "https://inferaiapi.example.com/v1/usage",
	})}
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"7.5"}]}`,
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, "https://api.deepseek.com/user/balance", upstream.lastURL)
	require.Equal(t, 7.5, result.Balance)
}
