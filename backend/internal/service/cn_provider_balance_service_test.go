package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
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

// 订阅有效期透传：subscription.expires_at 解析成功则落结果与快照；
// 非法时间串静默丢弃，不影响余额主链路。
func TestCNProviderBalanceService_RelayOverrideParsesExpiresAt(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		want            string
		wantSnapshotKey bool // 是否期望快照中包含 deepseek_balance_expires_at 键
	}{
		{
			name:            "subscription expires_at",
			body:            `{"remaining":-1,"unit":"USD","isValid":true,"planName":"DeepSeek订阅","subscription":{"expires_at":"2026-09-20T02:47:56.827014+08:00"}}`,
			want:            "2026-09-20T02:47:56.827014+08:00",
			wantSnapshotKey: true,
		},
		{
			name:            "top-level expires_at (quota mode)",
			body:            `{"remaining":12.5,"unit":"USD","expires_at":"2026-10-01T00:00:00Z"}`,
			want:            "2026-10-01T00:00:00Z",
			wantSnapshotKey: true,
		},
		{
			name:            "invalid expires_at dropped",
			body:            `{"remaining":12.5,"unit":"USD","expires_at":"not-a-time"}`,
			want:            "",
			wantSnapshotKey: false, // 不应写入空值覆盖旧快照
		},
		{
			name:            "missing expires_at",
			body:            `{"remaining":12.5,"unit":"USD"}`,
			want:            "",
			wantSnapshotKey: false, // 不应写入空值覆盖旧快照
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
				"enabled": true, "url": "https://inferaiapi.example.com/v1/usage", "bearer_auth": true,
			})}
			upstream := &cnBalanceResponseUpstream{statusCode: http.StatusOK, body: tt.body}
			svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

			result, err := svc.QueryBalance(context.Background(), repo.account.ID)

			require.NoError(t, err)
			require.True(t, result.Success)
			require.Equal(t, tt.want, result.ExpiresAt)
			require.Len(t, repo.extraWrites, 1)
			if tt.wantSnapshotKey {
				require.Equal(t, tt.want, repo.extraWrites[0]["deepseek_balance_expires_at"])
			} else {
				_, exists := repo.extraWrites[0]["deepseek_balance_expires_at"]
				require.False(t, exists, "expires_at should not be written to snapshot when upstream does not provide it")
			}
		})
	}
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

// 订阅用量透传：subscription.daily/monthly_usage_usd 落结果与快照。
func TestCNProviderBalanceService_RelayOverrideParsesSubscriptionUsage(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled": true, "url": "https://inferaiapi.example.com/v1/usage", "bearer_auth": true,
	})}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"remaining":-1,"unit":"USD","planName":"DeepSeek订阅","subscription":{"daily_usage_usd":1.83,"monthly_usage_usd":89.22,"expires_at":"2026-09-20T02:47:56+08:00"}}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.True(t, result.Success)
	require.True(t, result.Unlimited)
	require.NotNil(t, result.DailyUsage)
	require.InDelta(t, 1.83, *result.DailyUsage, 1e-9)
	require.NotNil(t, result.MonthlyUsage)
	require.InDelta(t, 89.22, *result.MonthlyUsage, 1e-9)
	require.Len(t, repo.extraWrites, 1)
	require.InDelta(t, 1.83, repo.extraWrites[0]["deepseek_balance_daily_used"], 1e-9)
	require.InDelta(t, 89.22, repo.extraWrites[0]["deepseek_balance_monthly_used"], 1e-9)
}

// 非订阅账号不写用量快照键（UpdateExtra 合并语义下避免写入 null 覆盖）。
func TestCNProviderBalanceService_RelayOverrideNonSubscriptionSkipsUsageKeys(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled": true, "url": "https://inferaiapi.example.com/v1/usage", "bearer_auth": true,
	})}
	upstream := &cnBalanceResponseUpstream{statusCode: http.StatusOK, body: `{"remaining":12.5,"unit":"USD"}`}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	_, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.Len(t, repo.extraWrites, 1)
	_, hasDaily := repo.extraWrites[0]["deepseek_balance_daily_used"]
	_, hasMonthly := repo.extraWrites[0]["deepseek_balance_monthly_used"]
	require.False(t, hasDaily)
	require.False(t, hasMonthly)
}

// 覆盖地址留空时按 base_url + /v1/usage 推导（中转账号只需打开探测开关）。
func TestCNProviderBalanceService_RelayOverrideEmptyURLDerivesFromBaseURL(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled": true, "url": "", "bearer_auth": true,
	})}
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"remaining":3.25,"unit":"USD"}`,
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "https://inferaiapi.example.com/v1/usage", upstream.lastURL)
	require.Equal(t, 3.25, result.Balance)
}

// 官方域名账号不触发默认推导：enabled 但留空地址时仍走官方余额端点。
func TestCNProviderBalanceService_EmptyURLOnOfficialHostKeepsOfficialEndpoint(t *testing.T) {
	account := newDeepSeekBalanceProbeAccount() // base_url=https://relay.example.com 视为自定义；
	account.Credentials["base_url"] = "https://api.deepseek.com"
	account.Credentials[BalanceProbeConfigCredentialKey] = map[string]any{"enabled": true, "url": ""}
	repo := &cnBalanceProbeRepo{account: account}
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"7.5"}]}`,
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "https://api.deepseek.com/user/balance", upstream.lastURL)
	require.Equal(t, 7.5, result.Balance)
}

// 上游订阅到期：/v1/usage 不再返回 remaining，报错需指出订阅名而非笼统的
// 解析失败；不覆盖上次快照（UpdateExtra 不调用），前端继续展示历史值+红到期。
func TestCNProviderBalanceService_RelayOverrideExpiredSubscriptionKeepsSnapshot(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled": true, "url": "https://inferaiapi.example.com/v1/usage", "bearer_auth": true,
	})}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"unrestricted","isValid":true,"planName":"kimi订阅 ","unit":"USD"}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "kimi订阅")
	require.Contains(t, result.Error, "expired")
	require.Empty(t, repo.extraWrites, "expired subscription must not overwrite last good snapshot")
}

// 上游订阅失效分支状态字段化：上游 200 + body 含 planName、无 remaining 时，
// SubscriptionExpired=true、Success=false，Error 含 "expired" 且含上游分组名
// （不再含 "missing remaining"），且**不落库 extra**（保留上次好快照）。
func TestCNProviderBalanceService_RelayOverrideExpiredSubscriptionStatus(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled": true, "url": "https://inferaiapi.example.com/v1/usage", "bearer_auth": true,
	})}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"unrestricted","isValid":true,"planName":"kimi订阅 ","unit":"USD"}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.True(t, result.SubscriptionExpired)
	require.Contains(t, result.Error, "expired")
	require.Contains(t, result.Error, "kimi订阅")
	require.NotContains(t, result.Error, "missing remaining")
	require.Empty(t, repo.extraWrites, "expired subscription must not overwrite last good snapshot")
}

// 成功路径（上游返回 remaining）：SubscriptionExpired 必须恒为 false（不误伤正常余额口径）。
func TestCNProviderBalanceService_RelayOverrideSuccessHasEmptyErrorCode(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled": true, "url": "https://inferaiapi.example.com/v1/usage", "bearer_auth": true,
	})}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"remaining":12.5,"unit":"USD","isValid":true,"planName":"DeepSeek订阅"}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.False(t, result.SubscriptionExpired)
}

// one-api 形状（body 含 data.unlimited_quota=true）：SubscriptionExpired 必须恒为 false。
func TestCNProviderBalanceService_RelayOverrideOneApiUnlimitedHasEmptyErrorCode(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled":     true,
		"url":         "https://inferaiapi.example.com/api/usage/token",
		"bearer_auth": true,
	})}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"code":true,"data":{"unlimited_quota":true,"total_available":1,"total_used":0,"expires_at":0,"name":"长期 代理春风","object":"token_usage"}}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.False(t, result.SubscriptionExpired)
}

// 无 planName 的 missing remaining 分支：SubscriptionExpired 必须恒为 false
// （仍走原文回退，Error 文本保持 "Invalid balance response: missing remaining"）。
func TestCNProviderBalanceService_RelayOverrideMissingRemainingNoPlanEmptyErrorCode(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled": true, "url": "https://inferaiapi.example.com/v1/usage", "bearer_auth": true,
	})}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"unrestricted","isValid":true,"unit":"USD"}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.False(t, result.SubscriptionExpired)
	require.Contains(t, result.Error, "Invalid balance response: missing remaining")
	require.Empty(t, repo.extraWrites)
}

// one-api/New API 系中转 token 配额形状（2026-10-08 星思云站生产实测）：
// data.unlimited_quota=true 视为订阅制不限量，Unlimited 落库、PlanName 落令牌名，
// 同时 total_available=剩余次数（1）落数字快照供前端『剩余 N』展示。
func TestCNProviderBalanceService_RelayOverrideOneApiUnlimitedToken(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled":     true,
		"url":         "https://inferaiapi.example.com/api/usage/token",
		"bearer_auth": true,
	})}
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"code":true,"data":{"unlimited_quota":true,"total_available":1,"total_used":0,"expires_at":0,"name":"长期 代理春风","object":"token_usage"}}`,
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, result.Unlimited)
	require.Equal(t, "长期 代理春风", result.PlanName)
	require.Equal(t, 1.0, result.Balance)
	require.Len(t, result.Balances, 1)
	require.Equal(t, 1.0, result.Balances[0].Balance)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, true, repo.extraWrites[0]["deepseek_balance_unlimited"])
	require.Equal(t, "长期 代理春风", repo.extraWrites[0]["deepseek_balance_plan_name"])
	require.Equal(t, 1.0, repo.extraWrites[0]["deepseek_balance"])
}

// one-api 形状 unlimited_quota=true 但缺 total_available：unlimited 成功路径
// 不受次数缺失影响，Balance 保持 0，不落错误（次数是附加信息）。
func TestCNProviderBalanceService_RelayOverrideOneApiUnlimitedNoTotalAvailable(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled":     true,
		"url":         "https://inferaiapi.example.com/api/usage/token",
		"bearer_auth": true,
	})}
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"code":true,"data":{"unlimited_quota":true,"total_used":0,"expires_at":0,"name":"长期 代理春风","object":"token_usage"}}`,
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, result.Unlimited)
	require.Equal(t, "长期 代理春风", result.PlanName)
	require.Equal(t, 0.0, result.Balance)
	require.Empty(t, result.Balances)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, true, repo.extraWrites[0]["deepseek_balance_unlimited"])
	require.Equal(t, "长期 代理春风", repo.extraWrites[0]["deepseek_balance_plan_name"])
}

// one-api token 配额形状：data.unlimited_quota=false 时按数字配额落余额，
// total_available 正常落库，unlimited=false。
func TestCNProviderBalanceService_RelayOverrideOneApiQuotaToken(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled":     true,
		"url":         "https://inferaiapi.example.com/api/usage/token",
		"bearer_auth": true,
	})}
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"code":true,"data":{"unlimited_quota":false,"total_available":42.5,"total_used":0,"name":"代理春风","object":"token_usage"}}`,
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.False(t, result.Unlimited)
	require.Equal(t, 42.5, result.Balance)
	require.Len(t, result.Balances, 1)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, false, repo.extraWrites[0]["deepseek_balance_unlimited"])
	require.Equal(t, "代理春风", repo.extraWrites[0]["deepseek_balance_plan_name"])
}

// cnBalanceRouteUpstream 按 URL 路径分发响应的桩（账户级探测一次查询打两个端点）。
type cnBalanceRouteUpstream struct {
	routes   map[string]cnBalanceResponseUpstream
	lastURLs []string
	lastAuth string
}

func (u *cnBalanceRouteUpstream) Do(
	req *http.Request,
	_ string,
	_ int64,
	_ int,
) (*http.Response, error) {
	u.lastURLs = append(u.lastURLs, req.URL.String())
	u.lastAuth = req.Header.Get("Authorization")
	route, ok := u.routes[req.URL.Path]
	if !ok {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Invalid URL"}}`)),
			Header:     make(http.Header),
		}, nil
	}
	return route.Do(req, "", 0, 0)
}

func (u *cnBalanceRouteUpstream) DoWithTLS(
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func newRelaySubscriptionProbeAccount(probe map[string]any) *Account {
	account := newRelayBalanceProbeAccount(map[string]any{
		"enabled": true,
		"url":     "https://inferaiapi.example.com/api/usage/token",
	})
	// 访问令牌存顶层敏感键（balance_probe 内不嵌令牌）。
	account.Credentials[BalanceProbeAccessTokenKey] = "at-test"
	account.Credentials[BalanceProbeConfigCredentialKey] = probe
	return account
}

const cnRelaySubscriptionBody = `{"success":true,"data":{"all_subscriptions":[{"subscription":{"id":1000004135,"user_id":41,"plan_id":4,"amount_total":250000000,"amount_used":157730000,"status":"active","source":"legacy","end_time":253402214400},"legacy_package_id":35,"legacy_countpack":true,"model_pools":null}],"billing_preference":"subscription_first"}}`

// 账户级次数包探测（New API 系）：access_token 分支打 /api/subscription/self +
// /api/user/self，剩余次数 = (total-used)/count_unit，PlanName 取上游分组，
// 快照覆盖令牌级残留展示键（expires_at 清空、daily/monthly_used 归零）。
func TestCNProviderBalanceService_RelaySubscriptionCountPack(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelaySubscriptionProbeAccount(map[string]any{
		"enabled":    true,
		"url":        "https://inferaiapi.example.com/api/usage/token",
		"count_unit": float64(5000),
	})}
	upstream := &cnBalanceRouteUpstream{routes: map[string]cnBalanceResponseUpstream{
		"/api/subscription/self": {statusCode: http.StatusOK, body: cnRelaySubscriptionBody},
		"/api/user/self":         {statusCode: http.StatusOK, body: `{"success":true,"data":{"id":41,"group":"开发者 Pro · 代理组","quota":1}}`},
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, result.Unlimited)
	require.Equal(t, "开发者 Pro · 代理组", result.PlanName)
	require.Equal(t, 18454.0, result.Balance)
	require.Len(t, result.Balances, 1)
	require.Equal(t, 18454.0, result.Balances[0].Balance)
	// 两个端点同源派生、Bearer 访问令牌鉴权。
	require.Equal(t, []string{
		"https://inferaiapi.example.com/api/subscription/self",
		"https://inferaiapi.example.com/api/user/self",
	}, upstream.lastURLs)
	require.Equal(t, "Bearer at-test", upstream.lastAuth)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, 18454.0, repo.extraWrites[0]["deepseek_balance"])
	require.Equal(t, true, repo.extraWrites[0]["deepseek_balance_unlimited"])
	require.Equal(t, "开发者 Pro · 代理组", repo.extraWrites[0]["deepseek_balance_plan_name"])
	require.Equal(t, "", repo.extraWrites[0]["deepseek_balance_expires_at"])
	require.Equal(t, float64(0), repo.extraWrites[0]["deepseek_balance_monthly_used"])
	require.Equal(t, float64(0), repo.extraWrites[0]["deepseek_balance_daily_used"])
}

// 配置了 access_token 但缺 count_unit：失败关闭（配置错误），不打上游、不落快照。
func TestCNProviderBalanceService_RelaySubscriptionMissingCountUnit(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelaySubscriptionProbeAccount(map[string]any{
		"enabled": true,
		"url":     "https://inferaiapi.example.com/api/usage/token",
	})}
	upstream := &cnBalanceRouteUpstream{routes: map[string]cnBalanceResponseUpstream{}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	_, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.Error(t, err)
	require.Contains(t, err.Error(), "count_unit")
	require.Empty(t, upstream.lastURLs)
	require.Empty(t, repo.extraWrites)
}

// 无 active 订阅：失败关闭，不落快照、不继续打 user/self。
func TestCNProviderBalanceService_RelaySubscriptionNoActive(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelaySubscriptionProbeAccount(map[string]any{
		"enabled":    true,
		"url":        "https://inferaiapi.example.com/api/usage/token",
		"count_unit": float64(5000),
	})}
	upstream := &cnBalanceRouteUpstream{routes: map[string]cnBalanceResponseUpstream{
		"/api/subscription/self": {statusCode: http.StatusOK, body: `{"success":true,"data":{"all_subscriptions":[{"subscription":{"status":"expired","amount_total":1,"amount_used":1}}]}}`},
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "no active subscription")
	require.Len(t, upstream.lastURLs, 1)
	require.Empty(t, repo.extraWrites)
}

// user/self 失败：整体失败关闭（PlanName 是快照必产出），不落快照。
func TestCNProviderBalanceService_RelaySubscriptionUserSelfFailure(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelaySubscriptionProbeAccount(map[string]any{
		"enabled":    true,
		"url":        "https://inferaiapi.example.com/api/usage/token",
		"count_unit": float64(5000),
	})}
	upstream := &cnBalanceRouteUpstream{routes: map[string]cnBalanceResponseUpstream{
		"/api/subscription/self": {statusCode: http.StatusOK, body: cnRelaySubscriptionBody},
		"/api/user/self":         {statusCode: http.StatusUnauthorized, body: `{"error":{"message":"unauthorized"}}`},
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "user/self")
	require.Empty(t, repo.extraWrites)
}

// 外审 must_fix 回归①：配置了 count_unit 但缺访问令牌 —— 对称校验失败关闭，
// 不得落入旧令牌级路径把残留值写入快照。
func TestCNProviderBalanceService_RelaySubscriptionCountUnitWithoutToken(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
		"enabled":    true,
		"url":        "https://inferaiapi.example.com/api/usage/token",
		"count_unit": float64(5000),
	})}
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"code":true,"data":{"unlimited_quota":true,"total_available":1,"name":"长期 代理春风"}}`,
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	_, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.Error(t, err)
	require.Contains(t, err.Error(), "configured together")
	require.Empty(t, repo.extraWrites, "must not persist token-level residual via legacy path")
}

// 外审第 2 轮 must_fix 回归③：count_unit 键存在但无效（0 或非数值）且未配置
// 访问令牌 —— 不得绕过对称校验落入旧令牌级路径，失败关闭。
func TestCNProviderBalanceService_RelaySubscriptionInvalidCountUnit(t *testing.T) {
	for name, unit := range map[string]any{"zero": float64(0), "negative": float64(-1), "non_numeric": "5000"} {
		t.Run(name, func(t *testing.T) {
			repo := &cnBalanceProbeRepo{account: newRelayBalanceProbeAccount(map[string]any{
				"enabled":    true,
				"url":        "https://inferaiapi.example.com/api/usage/token",
				"count_unit": unit,
			})}
			upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
				statusCode: http.StatusOK,
				body:       `{"code":true,"data":{"unlimited_quota":true,"total_available":1,"name":"长期 代理春风"}}`,
			}}
			svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

			_, err := svc.QueryBalance(context.Background(), repo.account.ID)

			require.Error(t, err)
			require.Contains(t, err.Error(), "configured together")
			require.Empty(t, upstream.lastURL, "must not fall through to legacy token-level probe")
			require.Empty(t, repo.extraWrites)
		})
	}
}

// 外审第 3 轮 must_fix 回归④：账户 header overrides 含 authorization 时不得
// 顶替账户级探测专用 Bearer 凭据（balance_probe_access_token 唯一鉴权）。
func TestCNProviderBalanceService_RelaySubscriptionTokenSurvivesHeaderOverride(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelaySubscriptionProbeAccount(map[string]any{
		"enabled":    true,
		"url":        "https://inferaiapi.example.com/api/usage/token",
		"count_unit": float64(5000),
	})}
	repo.account.Credentials["header_override_enabled"] = true
	repo.account.Credentials["header_overrides"] = map[string]any{
		"authorization": "Bearer rogue-override",
	}
	upstream := &cnBalanceRouteUpstream{routes: map[string]cnBalanceResponseUpstream{
		"/api/subscription/self": {statusCode: http.StatusOK, body: cnRelaySubscriptionBody},
		"/api/user/self":         {statusCode: http.StatusOK, body: `{"success":true,"data":{"id":41,"group":"开发者 Pro · 代理组","quota":1}}`},
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, "Bearer at-test", upstream.lastAuth, "probe must use balance_probe_access_token, not header override")
}

// 外审 must_fix 回归②：user/self 失败时 StatusCode 反映实际失败请求。
func TestCNProviderBalanceService_RelaySubscriptionUserSelfStatusCode(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newRelaySubscriptionProbeAccount(map[string]any{
		"enabled":    true,
		"url":        "https://inferaiapi.example.com/api/usage/token",
		"count_unit": float64(5000),
	})}
	upstream := &cnBalanceRouteUpstream{routes: map[string]cnBalanceResponseUpstream{
		"/api/subscription/self": {statusCode: http.StatusOK, body: cnRelaySubscriptionBody},
		"/api/user/self":         {statusCode: http.StatusUnauthorized, body: `{"error":{"message":"unauthorized"}}`},
	}}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Equal(t, http.StatusUnauthorized, result.StatusCode)
	require.Contains(t, result.Error, "user/self")
	require.Empty(t, repo.extraWrites)
}

// ---- BAL-GATE-D1 §3.2 选路合同测试 ----

// newBalanceProbePlatformAccount 构造带余额探测配置（可选）的通用账号，
// api_key 默认 sk-test（非空），平台/凭据由调用方定制。
func newBalanceProbePlatformAccount(platform string, creds map[string]any) *Account {
	base := map[string]any{
		"account_mode": AccountModePayG,
		"api_key":      "sk-test",
	}
	for k, v := range creds {
		base[k] = v
	}
	return &Account{
		ID:         201,
		Platform:   platform,
		Type:       AccountTypeAPIKey,
		Status:     StatusActive,
		Credentials: base,
	}
}

// A2 选路：openai 平台 + probe enabled + 显式 URL → 探测成功并写 openai_balance 前缀快照键。
// openai 非 CN 平台，验证 RelayProbeTargetURL 对 Kira/TH 之外的账号选路，且
// apiKey 经 GetCredential("api_key")（GetCNAPIKey 对非 CN 恒空不可用）。
func TestCNProviderBalanceService_RelayProbeExplicitURLSucceeds(t *testing.T) {
	account := newBalanceProbePlatformAccount(PlatformOpenAI, map[string]any{
		BalanceProbeConfigCredentialKey: map[string]any{
			"enabled": true, "url": "https://relay.example.com/v1/usage", "bearer_auth": true,
		},
	})
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"remaining":12.5,"unit":"USD","isValid":true,"planName":"OpenAI订阅"}`,
	}}
	repo := &cnBalanceProbeRepo{account: account}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, "https://relay.example.com/v1/usage", upstream.lastURL)
	require.Equal(t, 12.5, result.Balance)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, 12.5, repo.extraWrites[0][cnExtraKey(PlatformOpenAI, cnBalanceExtraSuffixBalance)])
}

// A2 选路：zhipu 平台 + probe enabled（留空 URL，按 base_url 推导）→ 同 openai 行为。
// 验证推导分支只对非官方 base_url 生效（zhipu 无官方余额端点，cnIsOfficialBalanceHost 恒 false）。
func TestCNProviderBalanceService_RelayProbeDerivesURLFromBaseURL(t *testing.T) {
	account := newBalanceProbePlatformAccount(PlatformZhipu, map[string]any{
		"base_url": "https://zhipu-relay.example.com",
		BalanceProbeConfigCredentialKey: map[string]any{
			"enabled": true, "url": "", "bearer_auth": true,
		},
	})
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"remaining":5.0,"unit":"USD"}`,
	}}
	repo := &cnBalanceProbeRepo{account: account}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, "https://zhipu-relay.example.com/v1/usage", upstream.lastURL)
	require.Equal(t, 5.0, result.Balance)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, 5.0, repo.extraWrites[0][cnExtraKey(PlatformZhipu, cnBalanceExtraSuffixBalance)])
}

// 无 probe 且非 kimi/deepseek → 既有错误码 CN_BALANCE_NO_ENDPOINT、零出站。
func TestCNProviderBalanceService_RelayProbeDisabledNonEndpointPlatform(t *testing.T) {
	account := newBalanceProbePlatformAccount(PlatformZhipu, map[string]any{})
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{statusCode: http.StatusOK, body: `{}`}}
	repo := &cnBalanceProbeRepo{account: account}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	_, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.Error(t, err)
	require.Contains(t, err.Error(), "CN_BALANCE_NO_ENDPOINT")
	require.Empty(t, upstream.lastURL, "无 probe 非端点平台必须零出站")
}

// coding 模式 + probe enabled + 官方 base_url → relay 不可选路，落到
// validatePayGAccount 的 CN_BALANCE_CODING_PLAN（零出站）。
func TestCNProviderBalanceService_RelayProbeCodingOfficialBaseURLEmitsCodingPlan(t *testing.T) {
	account := newBalanceProbePlatformAccount(PlatformKimi, map[string]any{
		"account_mode": AccountModeCoding,
		"base_url":     "https://api.moonshot.cn",
		BalanceProbeConfigCredentialKey: map[string]any{"enabled": true, "url": ""},
	})
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{statusCode: http.StatusOK, body: `{}`}}
	repo := &cnBalanceProbeRepo{account: account}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	_, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.Error(t, err)
	require.Contains(t, err.Error(), "CN_BALANCE_CODING_PLAN")
	require.Empty(t, upstream.lastURL, "官方 base_url 不可选路 → 零出站")
}

// A1 ② Kira 回归：
//  (1) 合法 Kira coding 账号探测成功（coding 模式不阻断 Kira 分支）；
//  (2) 非 CN platform（other）命中 Kira URL → 原错误码 CN_BALANCE_INVALID_PLATFORM 且零出站。
func TestCNProviderBalanceService_KiraProbeRegression(t *testing.T) {
	t.Run("kira coding account succeeds", func(t *testing.T) {
		upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
			if r.Method == http.MethodGet && r.URL.Path == "/api/user/usage" {
				return http.StatusOK, kiraUsageSummaryFixture
			}
			return http.StatusNotFound, `{}`
		}}
		account := newKiraTestAccount(PlatformKimi)
		account.Credentials["account_mode"] = AccountModeCoding
		repo := &kiraRepo{account: account}
		svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

		result, err := svc.QueryBalanceForAccount(context.Background(), account)

		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.Success)
		require.Equal(t, "kiraai.vn", upstream.requests[0].URL.Hostname())
		require.Len(t, repo.extraWrites, 2)
	})
	t.Run("non cn platform with kira url rejected zero outbound", func(t *testing.T) {
		upstream := &kiraRecordingUpstream{handler: func(r *http.Request, _ string) (int, string) {
			return http.StatusNotFound, `{}`
		}}
		account := &Account{
			ID:       78,
			Platform: PlatformOther,
			Type:     AccountTypeAPIKey,
			Status:   StatusActive,
			Credentials: map[string]any{
				"account_mode": AccountModePayG,
				"api_key":      "sk-other",
				"base_url":     "https://kiraai.vn/api/v1",
			},
		}
		repo := &kiraRepo{account: account}
		svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

		_, err := svc.QueryBalanceForAccount(context.Background(), account)

		require.Error(t, err)
		require.Contains(t, err.Error(), "CN_BALANCE_INVALID_PLATFORM")
		require.Empty(t, upstream.requests, "非 CN platform 命中 Kira URL 必须零出站")
	})
}

// A2 + 出站策略：allowlist 开启时，other 平台 + probe enabled + URL 可解析但不在
// allowlist → CN_BALANCE_URL_REJECTED、HTTP 出站零次、不回落官方端点、不写快照。
func TestCNProviderBalanceService_RelayProbeAllowlistRejected(t *testing.T) {
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
		Enabled:       true,
		UpstreamHosts: []string{"api.deepseek.com"},
	}}}
	account := newBalanceProbePlatformAccount(PlatformOther, map[string]any{
		"api_key": "sk-other",
		BalanceProbeConfigCredentialKey: map[string]any{
			"enabled": true, "url": "https://relay.example.com/v1/usage", "bearer_auth": true,
		},
	})
	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{statusCode: http.StatusOK, body: `{}`}}
	repo := &cnBalanceProbeRepo{account: account}
	svc := NewCNProviderBalanceService(repo, nil, upstream, cfg)

	_, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.Error(t, err)
	require.Contains(t, err.Error(), "CN_BALANCE_URL_REJECTED")
	require.Empty(t, upstream.lastURL, "allowlist 拒绝必须零出站")
	require.Empty(t, repo.extraWrites, "allowlist 拒绝不写快照、不回落官方端点")
}

// A3 鉴权分支：
//  (1) 令牌级 BearerAuth 缺 api_key → CN_BALANCE_NO_APIKEY 零出站；
//  (2) 账户级 access_token+count_unit 无 api_key 仍成功（账户级分支不要求 api_key）。
func TestCNProviderBalanceService_RelayProbeAuthBranches(t *testing.T) {
	t.Run("token level missing api_key rejected zero outbound", func(t *testing.T) {
		account := newBalanceProbePlatformAccount(PlatformOpenAI, map[string]any{
			"api_key": "",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true, "url": "https://relay.example.com/v1/usage", "bearer_auth": true,
			},
		})
		upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{statusCode: http.StatusOK, body: `{}`}}
		repo := &cnBalanceProbeRepo{account: account}
		svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

		_, err := svc.QueryBalance(context.Background(), repo.account.ID)

		require.Error(t, err)
		require.Contains(t, err.Error(), "CN_BALANCE_NO_APIKEY")
		require.Empty(t, upstream.lastURL, "令牌级缺 key 必须零出站")
	})
	t.Run("account level access token without api_key succeeds", func(t *testing.T) {
		account := newBalanceProbePlatformAccount(PlatformOpenAI, map[string]any{
			"api_key":                 "",
			BalanceProbeAccessTokenKey: "at-test",
			BalanceProbeConfigCredentialKey: map[string]any{
				"enabled": true, "url": "https://relay.example.com/api/usage/token", "count_unit": float64(5000),
			},
		})
		upstream := &cnBalanceRouteUpstream{routes: map[string]cnBalanceResponseUpstream{
			"/api/subscription/self": {statusCode: http.StatusOK, body: cnRelaySubscriptionBody},
			"/api/user/self":         {statusCode: http.StatusOK, body: `{"success":true,"data":{"id":41,"group":"开发者 Pro · 代理组","quota":1}}`},
		}}
		repo := &cnBalanceProbeRepo{account: account}
		svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

		result, err := svc.QueryBalance(context.Background(), repo.account.ID)

		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.Success)
		require.Equal(t, 18454.0, result.Balance)
		require.Len(t, repo.extraWrites, 1, "账户级分支无 api_key 仍成功落快照")
	})
}

// ---- 前后端契约：data.value.kind / quota_limit / quota_used + extra[<platform>_balance_kind] ----

// newKimiRelayProbeAccount 构造 kimi 平台的中转余额探测账号（czc.zh.kg 站，
// base_url 带 /v1 后缀），快照键前缀为 kimi_。
func newKimiRelayProbeAccount(probe map[string]any) *Account {
	return newBalanceProbePlatformAccount(PlatformKimi, map[string]any{
		"base_url":                      "https://czc.zh.kg/v1",
		BalanceProbeConfigCredentialKey: probe,
	})
}

// mode=unrestricted + remaining<0：订阅制不限量，Kind=subscription、Unlimited=true，
// 快照恒写 kimi_balance_kind（非次卡不写 quota_limit/quota_used）。
func TestQueryRelayBalanceModeUnrestrictedIsSubscription(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newKimiRelayProbeAccount(map[string]any{
		"enabled": true, "url": "https://czc.zh.kg/v1/usage", "bearer_auth": true,
	})}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"unrestricted","remaining":-1,"unit":"USD","isValid":true,"planName":"订阅不限量kimi-k3","subscription":{"expires_at":"2026-10-12T19:40:06+08:00","monthly_usage_usd":120.05}}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, CNBalanceKindSubscription, result.Kind)
	require.True(t, result.Unlimited)
	require.NotEmpty(t, result.PlanName)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, CNBalanceKindSubscription, repo.extraWrites[0][cnExtraKey(PlatformKimi, cnBalanceExtraSuffixKind)])
	require.Equal(t, CNBalanceKindSubscription, repo.extraWrites[0]["kimi_balance_kind"])
	_, hasQuotaLimit := repo.extraWrites[0][cnExtraKey(PlatformKimi, cnBalanceExtraSuffixQuotaLimit)]
	_, hasQuotaUsed := repo.extraWrites[0][cnExtraKey(PlatformKimi, cnBalanceExtraSuffixQuotaUsed)]
	require.False(t, hasQuotaLimit, "非次卡不得写 quota_limit（合并语义下会留旧值）")
	require.False(t, hasQuotaUsed, "非次卡不得写 quota_used（合并语义下会留旧值）")
}

// mode=quota_limited：次卡（次数包）。单位是「次」，上游 unit 误标 USD 不可信
// （quota.used=295 与 usage.today.requests=295 逐位相等），故 Currency 置空、
// Balances 置空，只保留次数口径的 Balance/QuotaLimit/QuotaUsed。
func TestQueryRelayBalanceModeQuotaLimitedIsCountPack(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newKimiRelayProbeAccount(map[string]any{
		"enabled": true, "url": "https://czc.zh.kg/v1/usage", "bearer_auth": true,
	})}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"mode":"quota_limited","remaining":705,"unit":"USD","isValid":true,"quota":{"limit":1000,"used":295,"remaining":705,"unit":"USD"},"expires_at":"2026-10-12T19:33:00+08:00"}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, CNBalanceKindCountPack, result.Kind)
	require.Equal(t, 705.0, result.Balance)
	require.Equal(t, 1000.0, result.QuotaLimit)
	require.Equal(t, 295.0, result.QuotaUsed)
	require.Equal(t, "", result.Currency, "次卡单位不是金额，上游 unit=USD 不可信")
	require.Empty(t, result.Balances)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, CNBalanceKindCountPack, repo.extraWrites[0]["kimi_balance_kind"])
	require.Equal(t, 1000.0, repo.extraWrites[0][cnExtraKey(PlatformKimi, cnBalanceExtraSuffixQuotaLimit)])
	require.Equal(t, 295.0, repo.extraWrites[0][cnExtraKey(PlatformKimi, cnBalanceExtraSuffixQuotaUsed)])
}

// 无 mode 键（旧 /v1/usage 形状）回退数字余额口径：Kind=balance、Currency 保留
// 上游 unit，快照写 kimi_balance_kind=balance 且不带次卡额度键。
func TestQueryRelayBalanceNoModeFallsBackToBalance(t *testing.T) {
	repo := &cnBalanceProbeRepo{account: newKimiRelayProbeAccount(map[string]any{
		"enabled": true, "url": "https://czc.zh.kg/v1/usage", "bearer_auth": true,
	})}
	upstream := &cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"remaining":5.5,"unit":"USD","isValid":true}`,
	}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, CNBalanceKindBalance, result.Kind)
	require.Equal(t, 5.5, result.Balance)
	require.Equal(t, "USD", result.Currency)
	require.Len(t, result.Balances, 1)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, CNBalanceKindBalance, repo.extraWrites[0]["kimi_balance_kind"])
	_, hasQuotaLimit := repo.extraWrites[0][cnExtraKey(PlatformKimi, cnBalanceExtraSuffixQuotaLimit)]
	require.False(t, hasQuotaLimit, "balance 种类不写次卡额度键")
}

// C3 回归：base_url 带 /v1 后缀时不得拼成 /v1/v1/usage（该 404 导致 czc.zh.kg
// 的 kimi_balance* 快照从未落库）。两种输入都必须得到 .../v1/usage。
func TestRelayProbeTargetURLDedupV1(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"base_url with /v1 suffix", "https://czc.zh.kg/v1", "https://czc.zh.kg/v1/usage"},
		{"base_url without /v1 suffix", "https://czc.zh.kg", "https://czc.zh.kg/v1/usage"},
		{"base_url with trailing slash", "https://czc.zh.kg/", "https://czc.zh.kg/v1/usage"},
		{"base_url with /v1 and trailing slash", "https://czc.zh.kg/v1/", "https://czc.zh.kg/v1/usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := newBalanceProbePlatformAccount(PlatformKimi, map[string]any{
				"base_url": tt.baseURL,
				BalanceProbeConfigCredentialKey: map[string]any{
					"enabled": true, "url": "", "bearer_auth": true,
				},
			})

			got, ok := RelayProbeTargetURL(account)

			require.True(t, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

// zhipu 平台 relay 资格：balance_probe.enabled + 显式 URL → RelayProbeTargetURL
// 选路成功（不再落到 CN_BALANCE_NO_ENDPOINT），且探测成功落 zhipu_ 前缀快照。
func TestRelayProbeTargetURLZhipuExplicitURLEligible(t *testing.T) {
	account := newBalanceProbePlatformAccount(PlatformZhipu, map[string]any{
		"base_url": "https://czc.zh.kg/v1",
		BalanceProbeConfigCredentialKey: map[string]any{
			"enabled": true, "url": "https://czc.zh.kg/v1/usage", "bearer_auth": true,
		},
	})

	probeURL, ok := RelayProbeTargetURL(account)

	require.True(t, ok, "zhipu + enabled + 显式 URL 必须选路成功")
	require.Equal(t, "https://czc.zh.kg/v1/usage", probeURL)

	upstream := &cnBalanceCaptureUpstream{cnBalanceResponseUpstream: cnBalanceResponseUpstream{
		statusCode: http.StatusOK,
		body:       `{"remaining":9.5,"unit":"USD","isValid":true}`,
	}}
	repo := &cnBalanceProbeRepo{account: account}
	svc := NewCNProviderBalanceService(repo, nil, upstream, nil)

	result, err := svc.QueryBalance(context.Background(), repo.account.ID)

	require.NoError(t, err, "zhipu relay 账号不得再报 CN_BALANCE_NO_ENDPOINT")
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, "https://czc.zh.kg/v1/usage", upstream.lastURL)
	require.Equal(t, 9.5, result.Balance)
	require.Equal(t, CNBalanceKindBalance, result.Kind)
	require.Len(t, repo.extraWrites, 1)
	require.Equal(t, CNBalanceKindBalance, repo.extraWrites[0]["zhipu_balance_kind"])
}

// ---- 后端 JSON 契约：subscription_expired 的线上 JSON 键名 ↔ 前端读取键 ----

// TestCNProviderBalanceResult_SubscriptionExpiredJSONContract 锁死「线上 JSON 键名 ↔ 前端
// 读取键」的契约，并锁死「其它账号的 JSON 里不出现该键」这一最强保证：
//
//  1. 到期态：marshal 一个 SubscriptionExpired=true 的 DTO，线上 JSON 键名必须是逐字一致的
//     "subscription_expired"，且不得再出现上一批已删除的旧错误码
//     CN_BALANCE_UPSTREAM_SUBSCRIPTION_UNAVAILABLE（该码删除后若 JSON 仍残留，前端会静默
//     走不到到期状态分支）。
//  2. 非到期态（正常余额成功 Success=true，以及普通失败 Success=false 但
//     SubscriptionExpired=false）：JSON 必须不出现 subscription_expired 子串。这正是用户
//     「不要影响其他账号查询」的最强保证——其它账号的 JSON 里没有该键，前端不会误走到到期
//     状态分支而退回报错行。
//  3. 前端契约键名一致：把到期态 marshal 出的 JSON 解析回 map，断言键 subscription_expired
//     存在且值为 bool（锁定它不是字符串、不是数字）。
//
// 注意：本用例只新增，不改动既有 TestCNProviderBalanceService_* 用例的任何断言。
func TestCNProviderBalanceResult_SubscriptionExpiredJSONContract(t *testing.T) {
	// 1. 到期态：线上 JSON 键名必须逐字为 subscription_expired。
	expired := CNProviderBalanceResult{
		Provider:            "kimi",
		Success:             false,
		SubscriptionExpired: true,
		Error:               `Upstream subscription "kimi订阅" expired or unavailable`,
	}
	expiredJSON, err := json.Marshal(expired)
	require.NoError(t, err)
	require.Contains(t, string(expiredJSON), `"subscription_expired":true`,
		"到期态的线上 JSON 键名必须是逐字一致的 subscription_expired")
	// 旧错误码已随上一批删除，绝不可在 JSON 中残留。
	require.NotContains(t, string(expiredJSON), "CN_BALANCE_UPSTREAM_SUBSCRIPTION_UNAVAILABLE",
		"旧错误码已删除，JSON 中不得残留该旧码")

	// 3. 前端契约键名一致：解析回 map，锁定键存在且类型为 bool（非字符串、非数字）。
	var m map[string]any
	require.NoError(t, json.Unmarshal(expiredJSON, &m))
	val, exists := m["subscription_expired"]
	require.True(t, exists, "前端读取键 subscription_expired 必须存在")
	_, isBool := val.(bool)
	require.True(t, isBool, "subscription_expired 必须是 bool，不能是字符串/数字")

	// 2. 非到期态（正常余额成功）：JSON 不得出现 subscription_expired 键。
	//    这是「不影响其它账号查询」的核心保证：其它账号的 JSON 里不出现该键，前端不会
	//    误走到到期状态分支。
	normal := CNProviderBalanceResult{
		Provider: "deepseek",
		Success:  true,
		Balance:  7.5,
		Currency: "CNY",
	}
	normalJSON, err := json.Marshal(normal)
	require.NoError(t, err)
	require.NotContains(t, string(normalJSON), "subscription_expired",
		"正常余额成功的 JSON 不得出现 subscription_expired 键")

	// 2b. 非到期态（普通失败，非订阅到期）：JSON 也不得出现 subscription_expired 键。
	plainFail := CNProviderBalanceResult{
		Provider: "deepseek",
		Success:  false,
		Error:    "some probe failure unrelated to subscription",
	}
	plainFailJSON, err := json.Marshal(plainFail)
	require.NoError(t, err)
	require.NotContains(t, string(plainFailJSON), "subscription_expired",
		"普通失败（非订阅到期）的 JSON 不得出现 subscription_expired 键")
}
