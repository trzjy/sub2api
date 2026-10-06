package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeCodeBuddySite(t *testing.T) {
	cases := map[string]string{
		"":                CodeBuddySiteCN,
		"  ":              CodeBuddySiteCN,
		CodeBuddySiteCN:   CodeBuddySiteCN,
		CodeBuddySiteIntl: CodeBuddySiteIntl,
		"INTL":            CodeBuddySiteIntl,
		" Intl ":          CodeBuddySiteIntl,
		"sg":              CodeBuddySiteCN, // 未知值回落 cn
		"codebuddy.ai":    CodeBuddySiteCN,
	}
	for in, want := range cases {
		require.Equal(t, want, NormalizeCodeBuddySite(in), "input=%q", in)
	}
}

func TestAccountCodeBuddySite(t *testing.T) {
	var nilAccount *Account
	require.Equal(t, CodeBuddySiteCN, nilAccount.CodeBuddySite(), "nil 账号回落 cn")

	// 存量账号：无 site 键 → cn（零迁移、零行为变化）。
	legacy := &Account{Credentials: map[string]any{"access_token": "at"}}
	require.Equal(t, CodeBuddySiteCN, legacy.CodeBuddySite())

	intl := &Account{Credentials: map[string]any{"access_token": "at", "site": "intl"}}
	require.Equal(t, CodeBuddySiteIntl, intl.CodeBuddySite())

	unknown := &Account{Credentials: map[string]any{"site": "eu"}}
	require.Equal(t, CodeBuddySiteCN, unknown.CodeBuddySite())
}

func TestCodeBuddySiteEndpointsTable(t *testing.T) {
	cn := codeBuddyEndpointsFor("")
	require.Equal(t, "https://copilot.tencent.com", cn.UpstreamBase)
	require.Equal(t, "https://www.codebuddy.cn", cn.OriginReferer)
	require.Equal(t, "https://www.codebuddy.cn", cn.BillingBase)
	require.Equal(t, "https://copilot.tencent.com", cn.ModelsBase)

	intl := codeBuddyEndpointsFor(CodeBuddySiteIntl)
	require.Equal(t, "https://www.codebuddy.ai", intl.UpstreamBase)
	require.Equal(t, "https://www.codebuddy.ai", intl.OriginReferer)
	require.Equal(t, "https://www.codebuddy.ai", intl.BillingBase)
	require.Equal(t, "https://www.codebuddy.ai", intl.ModelsBase)
}

// TestBuildCodeBuddyChatRequest_SiteSelection 回归"无 site=cn 行为不变"，并验证 intl
// 出站打到 codebuddy.ai、Origin/Referer 随站点变化。
func TestBuildCodeBuddyChatRequest_SiteSelection(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"messages":[]}`)

	t.Run("no site key keeps CN behavior", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		account := &Account{Platform: PlatformCodeBuddy, Credentials: map[string]any{"access_token": "at"}}
		req, err := svc.buildCodeBuddyChatRequest(context.Background(), nil, account, body, "tok", "m")
		require.NoError(t, err)
		require.Equal(t, "copilot.tencent.com", req.URL.Host)
		require.Equal(t, "https://www.codebuddy.cn", req.Header.Get("Origin"))
		require.Equal(t, "https://www.codebuddy.cn/", req.Header.Get("Referer"))
		require.Equal(t, CodeBuddyClientUA, req.Header.Get("User-Agent"))
	})

	t.Run("intl site selects codebuddy.ai", func(t *testing.T) {
		svc := &OpenAIGatewayService{}
		account := &Account{Platform: PlatformCodeBuddy, Credentials: map[string]any{
			"access_token": "at", "site": "intl",
		}}
		req, err := svc.buildCodeBuddyChatRequest(context.Background(), nil, account, body, "tok", "m")
		require.NoError(t, err)
		require.Equal(t, "www.codebuddy.ai", req.URL.Host)
		require.Equal(t, "https://www.codebuddy.ai", req.Header.Get("Origin"))
		require.Equal(t, "https://www.codebuddy.ai/", req.Header.Get("Referer"))
	})
}

// TestCodeBuddyQuotaBillingBaseBySite 验证计费端点按站点选 base（CN → codebuddy.cn，
// intl → codebuddy.ai），并保留站点 Origin。
func TestCodeBuddyQuotaBillingBaseBySite(t *testing.T) {
	cases := []struct {
		name       string
		creds      map[string]any
		wantHost   string
		wantOrigin string
	}{
		{name: "legacy no site -> cn", creds: map[string]any{"access_token": "at", "uid": "u"}, wantHost: "www.codebuddy.cn", wantOrigin: "https://www.codebuddy.cn"},
		{name: "intl", creds: map[string]any{"access_token": "at", "uid": "u", "site": "intl"}, wantHost: "www.codebuddy.ai", wantOrigin: "https://www.codebuddy.ai"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":0,"msg":"OK","data":{}}`)),
			}}
			svc := &CodeBuddyQuotaService{httpUpstream: upstream}
			account := &Account{ID: 1, Platform: PlatformCodeBuddy, Credentials: tc.creds}

			_, _, err := svc.doBillingRequest(context.Background(), account, http.MethodPost, codeBuddyBillingMeterPath, []byte("{}"))
			require.NoError(t, err)
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, tc.wantHost, upstream.lastReq.URL.Host, "billing base 必须按站点选表")
			require.Equal(t, tc.wantOrigin, upstream.lastReq.Header.Get("Origin"))
		})
	}
}

// TestCodeBuddyQuotaFetchModels_IntlUnavailable 钉住 Phase 0 D4：intl 站点 models 端点
// 实测 500，FetchModels 直接降级返回错误、不外呼上游（避免热路径 500 风暴）。
func TestCodeBuddyQuotaFetchModels_IntlUnavailable(t *testing.T) {
	upstream := &httpUpstreamRecorder{}
	account := &Account{ID: 1, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "at", "site": "intl"}}
	repo := &codeBuddyQuotaRepoStub{getByID: account}
	svc := &CodeBuddyQuotaService{accountRepo: repo, httpUpstream: upstream}

	models, err := svc.FetchModels(context.Background(), account.ID)
	require.Error(t, err)
	require.Nil(t, models)
	require.Empty(t, upstream.requests, "intl 不得请求不可用的 models 端点")

	// SupportedEffortsForModel 必须吞掉该降级错误并返回空，不阻塞转发热路径。
	require.Nil(t, svc.SupportedEffortsForModel(context.Background(), account, "deepseek-v3"))
}
