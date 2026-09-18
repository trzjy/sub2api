package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// 以下测试锁定账号创建链路上 upstream_billing_probe_enabled 契约：
// 网页逆向平台（官方 zhipu / deepseek / kimi + access_mode=web）与通用 other 上游
// 不在计费探测资格名单内（见 IsUpstreamBillingProbeIdentity / isUpstreamBillingProbeAccount）。管理端即便在
// 创建请求里显式打开 ProbeEnabled=true，也必须被 fail-closed 拒绝，而不是
// 静默写入无法服务的探测开关。资格平台（OpenAI/Anthropic/...）的创建探测契约
// 由同包内 TestCreateAccountAcceptsDedicatedUpstreamBillingProbeSetting 覆盖。

// TestCreateAccountWebPlatformProbeFlagContract 覆盖网页逆向平台的创建探测契约：
// 不带 ProbeEnabled 时正常落库、且不携带任何受管探测键；显式 ProbeEnabled=true
// 时固化为 ErrUpstreamBillingProbeAccountInvalid。
func TestCreateAccountWebPlatformProbeFlagContract(t *testing.T) {
	tests := []struct {
		name        string
		platform    string
		credentials map[string]any
	}{
		{
			// zhipu web：整串 Cookie 作为静态登录态凭证。
			name:        "zhipu web cookie",
			platform:    PlatformZhipu,
			credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=test-cookie"},
		},
		{
			// deepseek web：与 zhipu 同为整串 Cookie 准入口径。
			name:        "deepseek web cookie",
			platform:    PlatformDeepseek,
			credentials: map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=test-cookie"},
		},
		{
			// kimi web：凭证键是 access_token 而非 cookie，准入口径不同但同属资格外平台。
			name:        "kimi web access_token",
			platform:    PlatformKimi,
			credentials: map[string]any{"access_mode": AccountAccessModeWeb, "access_token": "test-token"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1) 不带 ProbeEnabled：创建成功，且绝不携带受管探测键。
			repo := &upstreamBillingProbeAccountRepo{}
			created, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
				Name:                 "web-probe-off",
				Platform:             tt.platform,
				Type:                 AccountTypeAPIKey,
				Credentials:          tt.credentials,
				SkipDefaultGroupBind: true,
			})
			require.NoError(t, err)
			require.NotContains(t, created.Extra, UpstreamBillingProbeEnabledExtraKey)
			require.NotContains(t, created.Extra, UpstreamBillingRateSyncEnabledExtraKey)
			require.NotContains(t, created.Extra, UpstreamBillingProbeExtraKey)
			require.Equal(t, tt.platform, created.Platform)
			require.Equal(t, AccountTypeAPIKey, created.Type)

			// 2) 显式 ProbeEnabled=true：资格外平台必须 fail-closed 拒绝，
			//    不允许把无法服务的探测开关写进新建账号。
			enabled := true
			_, err = (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
				Name:                 "web-probe-on",
				Platform:             tt.platform,
				Type:                 AccountTypeAPIKey,
				Credentials:          tt.credentials,
				ProbeEnabled:         &enabled,
				SkipDefaultGroupBind: true,
			})
			require.ErrorIs(t, err, ErrUpstreamBillingProbeAccountInvalid)
		})
	}
}

// TestCreateAccountOtherProbeFlagRejected 锁定通用 other 上游（任意第三方
// OpenAI 兼容 relay）同样不在探测资格内：显式 ProbeEnabled=true 必须被拒绝。
func TestCreateAccountOtherProbeFlagRejected(t *testing.T) {
	enabled := true
	repo := &upstreamBillingProbeAccountRepo{}
	_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "other-probe-on",
		Platform:             PlatformOther,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"base_url": "https://relay.example.com", "api_key": "sk-test"},
		ProbeEnabled:         &enabled,
		SkipDefaultGroupBind: true,
	})
	require.ErrorIs(t, err, ErrUpstreamBillingProbeAccountInvalid)
}

// TestCreateAccountWebCredentialValidationAndTypeAdmission 锁定网页逆向平台的
// 准入不变量（创建期即校验，不依赖后续编辑路径）：
// 空 Cookie 视为非法凭证；仅允许 apikey 类型，oauth 直接拒绝。
func TestCreateAccountWebCredentialValidationAndTypeAdmission(t *testing.T) {
	t.Run("zhipu web empty cookie is rejected", func(t *testing.T) {
		repo := &upstreamBillingProbeAccountRepo{}
		_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
			Name:                 "zhipu-web-empty-cookie",
			Platform:             PlatformZhipu,
			Type:                 AccountTypeAPIKey,
			Credentials:          map[string]any{"access_mode": AccountAccessModeWeb, "cookie": ""},
			SkipDefaultGroupBind: true,
		})
		require.Error(t, err)
	})

	t.Run("zhipu web only supports apikey", func(t *testing.T) {
		repo := &upstreamBillingProbeAccountRepo{}
		_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
			Name:                 "zhipu-web-oauth",
			Platform:             PlatformZhipu,
			Type:                 AccountTypeOAuth,
			Credentials:          map[string]any{"access_mode": AccountAccessModeWeb, "cookie": "sessionid=test-cookie"},
			SkipDefaultGroupBind: true,
		})
		require.Error(t, err)
	})
}
