package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// dispatchAccountRepo 只实现公开探活所需的 GetByID，其余方法不会被这些测试调用。
type dispatchAccountRepo struct {
	AccountRepository
	account *Account
}

func (r *dispatchAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func newDispatchTestService(account *Account, upstream *webProbeUpstream) *AccountTestService {
	service := newWebTestService(upstream, nil)
	service.accountRepo = &dispatchAccountRepo{account: account}
	service.cfg = &config.Config{
		Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled: false, AllowInsecureHTTP: true,
		}},
	}
	return service
}

func chatCompletionsProbeResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n",
		)),
	}
}

func dispatchWebAccount(platform string, id int64) *Account {
	credentials := map[string]any{"access_mode": AccountAccessModeWeb}
	if platform == PlatformKimi {
		credentials["access_token"] = "test-access-token"
	} else {
		credentials["cookie"] = "test-cookie=1"
	}
	return &Account{ID: id, Platform: platform, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: credentials}
}

func dispatchAPIAccount(platform string, id int64) *Account {
	return &Account{
		ID: id, Platform: platform, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{
			"access_mode":  AccountAccessModeAPI,
			"api_key":      "sk-test-dispatch",
			"api_protocol": APIProtocolChatCompletions,
			"base_url":     "http://api.example",
		},
	}
}

func TestAccountTestService_WebAccountsUseWebProbeThroughPublicEntryPoint(t *testing.T) {
	tests := []struct {
		platform string
		path     string
		authName string
	}{
		{platform: PlatformKimi, path: webKimiChatPath, authName: "Authorization"},
		{platform: PlatformZhipu, path: webZhipuStreamPath, authName: "Cookie"},
		{platform: PlatformDeepseek, path: webDeepseekChatCompletionPath, authName: "Cookie"},
	}

	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			account := dispatchWebAccount(tt.platform, 100)
			upstream := &webProbeUpstream{}
			if tt.platform == PlatformDeepseek {
				upstream.respSeq = []*http.Response{
					webDeepseekSolvablePowChallengeResponse(),
					webDeepseekSessionCreateResponse(),
					okWebDeepseekProbeResponse(),
				}
			} else {
				upstream.resp = webProbeOKResponse(tt.platform)
			}
			svc := newDispatchTestService(account, upstream)
			if tt.platform != PlatformDeepseek {
				upstream.resp = webProbeOKResponse(tt.platform)
			}
			ctx, recorder := newWebTestContext()

			require.NoError(t, svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault))
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, tt.path, upstream.lastReq.URL.Path)
			require.NotEmpty(t, upstream.lastReq.Header.Get(tt.authName))
			require.NotContains(t, recorder.Body.String(), "No API key available")
			require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
		})
	}
}

func TestAccountTestService_APIAccountsKeepCNAPIProbeThroughPublicEntryPoint(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformZhipu, PlatformDeepseek} {
		t.Run(platform, func(t *testing.T) {
			account := dispatchAPIAccount(platform, 200)
			upstream := &webProbeUpstream{resp: chatCompletionsProbeResponse()}
			svc := newDispatchTestService(account, upstream)
			upstream.resp = chatCompletionsProbeResponse()
			ctx, recorder := newWebTestContext()

			require.NoError(t, svc.TestAccountConnection(ctx, account.ID, "test-model", "", AccountTestModeDefault))
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, "/v1/chat/completions", upstream.lastReq.URL.Path)
			require.Equal(t, "Bearer sk-test-dispatch", upstream.lastReq.Header.Get("Authorization"))
			require.NotContains(t, recorder.Body.String(), "Web login session is healthy.")
			require.Contains(t, recorder.Body.String(), "已通过 /v1/chat/completions 验证")
		})
	}
}

func TestAccountTestService_WebAccountsMissingLoginCredentialFailBeforeUpstream(t *testing.T) {
	cases := []struct {
		platform string
		errText  string
	}{
		{platform: PlatformKimi, errText: "kimi web account is missing access_token credential"},
		{platform: PlatformZhipu, errText: "zhipu web account is missing login cookie credential"},
		{platform: PlatformDeepseek, errText: "deepseek web account is missing login cookie credential"},
	}

	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			account := dispatchWebAccount(tc.platform, 300)
			if tc.platform == PlatformKimi {
				delete(account.Credentials, "access_token")
			} else {
				delete(account.Credentials, "cookie")
			}
			upstream := &webProbeUpstream{resp: okSSEResponse()}
			svc := newDispatchTestService(account, upstream)
			ctx, recorder := newWebTestContext()

			// 缺凭据走 sendErrorAndEnd：SSE 发 error 事件（HTTP 200）且函数返回该错误。
			// 关键断言：未触达上游 + 错误文案是平台缺凭据语义，不是 API key 文案。
			err := svc.TestAccountConnection(ctx, account.ID, "", "", AccountTestModeDefault)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.errText)
			require.NotContains(t, err.Error(), "No API key available")
			require.Nil(t, upstream.lastReq)
			require.Contains(t, recorder.Body.String(), tc.errText)
			require.NotContains(t, recorder.Body.String(), "No API key available")
		})
	}
}

func TestAccountTestService_RunTestBackgroundKimiWebUsesWebProbe(t *testing.T) {
	account := dispatchWebAccount(PlatformKimi, 400)
	upstream := &webProbeUpstream{resp: okWebKimiProbeResponse()}
	svc := newDispatchTestService(account, upstream)
	upstream.resp = okWebKimiProbeResponse()

	result, err := svc.RunTestBackground(context.Background(), account.ID, "")

	require.NoError(t, err)
	require.Equal(t, "success", result.Status)
	require.Empty(t, result.ErrorMessage)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, webKimiChatPath, upstream.lastReq.URL.Path)
}
