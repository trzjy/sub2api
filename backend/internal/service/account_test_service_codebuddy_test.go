package service

// CodeBuddy 账号测试链回归测试（含 CodeBuddy 影子）。
//
// 覆盖：
//   - CodeBuddy 影子（自身 credentials 为空、母账号 platform=codebuddy）不再返回
//     "No API key available"，而是解析母账号后走 CodeBuddy 分支；
//   - credentialAccount 路由到 CodeBuddy 分支（不是 claude 兜底）；
//   - 站点 SSOT：cn（缺省）/ intl 出站域名正确；
//   - 非影子 CN apikey 账号行为不变（回归保护）；
//   - 母账号缺失/解析失败 → 优雅报错（不 panic、不发上游请求）；
//   - 账号级非法 base_url / 缺凭证 → 显式报错，错误文案不回显凭证值。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newCodeBuddyCountingService 在既有装配基础上再加一层出站计数器，用于验收
// "只发一次上游请求"（不得重试刷量）与非影子账号"不解析母账号"。
func newCodeBuddyCountingService(accounts map[int64]*Account, resp *http.Response) (*AccountTestService, *countingUpstream, *codebuddyDispatchRepo) {
	base := &webProbeUpstream{resp: resp}
	svc := newCodeBuddyProbeTestService(accounts, base, resp)
	repo := svc.accountRepo.(*codebuddyDispatchRepo)
	counting := &countingUpstream{HTTPUpstream: base}
	svc.httpUpstream = counting
	return svc, counting, repo
}

// codebuddyDispatchRepo 按 ID 返回账号（影子→母账号解析需要至少两个账号）。
// calls 统计 GetByID 次数，用于区分"非影子不解析（1 次）/ 影子解析母账号（2 次）"。
type codebuddyDispatchRepo struct {
	AccountRepository
	accounts map[int64]*Account
	calls    int
}

func (r *codebuddyDispatchRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.calls++
	if acc, ok := r.accounts[id]; ok && acc != nil {
		return acc, nil
	}
	return nil, fmt.Errorf("account %d not found", id)
}

// countingUpstream 统计出站请求次数：探活必须只发一次，不得重试刷量。
type countingUpstream struct {
	HTTPUpstream
	doCalls  int
	tlsCalls int
	tlsErr   error
}

func (u *countingUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	u.doCalls++
	return u.HTTPUpstream.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *countingUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.tlsCalls++
	if u.tlsErr != nil {
		return nil, u.tlsErr
	}
	return u.HTTPUpstream.DoWithTLS(req, proxyURL, accountID, accountConcurrency, profile)
}

func newCodeBuddyProbeTestService(accounts map[int64]*Account, upstream *webProbeUpstream, resp *http.Response) *AccountTestService {
	svc := newWebTestService(upstream, nil)
	// newWebTestService 会把 upstream.resp 置为传入值，故在其后重新注入响应。
	upstream.resp = resp
	svc.accountRepo = &codebuddyDispatchRepo{accounts: accounts}
	svc.cfg = &config.Config{
		Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled: false, AllowInsecureHTTP: false,
		}},
	}
	return svc
}

// codebuddyQuotaOKResponse 是 get-user-resource 的成功报文（仅 code=0 与最小 data，
// 探针只判定状态码与业务信封）。
func codebuddyQuotaOKResponse() *http.Response {
	body, err := json.Marshal(map[string]any{
		"code": 0,
		"data": map[string]any{"Response": map[string]any{"Data": map[string]any{
			"Accounts": []map[string]any{{"CapacitySize": 100, "CapacityUsed": 1}},
		}}},
	})
	if err != nil {
		panic(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

// codebuddyParentAccount 构造 CodeBuddy 母账号（OAuth，access_token 在位）。
func codebuddyProbeParentAccount(id int64, site string) *Account {
	credentials := map[string]any{"access_token": "PARENT_TOKEN_SECRET"}
	if site != "" {
		credentials["site"] = site
	}
	return &Account{ID: id, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth, Concurrency: 1, Credentials: credentials}
}

// codebuddyShadowAccount 构造 CodeBuddy 影子：platform 为目标分组平台、type=oauth、
// 自身 credentials 恒空（生产实证形态：id=112 name 前缀 8277）。
func codebuddyProbeShadowAccount(id, parentID int64) *Account {
	return &Account{
		ID:              id,
		Platform:        PlatformDeepseek,
		Type:            AccountTypeOAuth,
		Concurrency:     1,
		QuotaDimension:  QuotaDimensionCodeBuddy,
		ParentAccountID: int64Ptr(parentID),
		Credentials:     map[string]any{},
	}
}

// TestAccountTestService_CodeBuddyShadowUsesCodeBuddyProbe 影子账号自身凭证为空，
// 必须解析母账号后走 CodeBuddy 分支，而不是读自己的空 api_key。
func TestAccountTestService_CodeBuddyShadowUsesCodeBuddyProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(112, 109)
	// 影子的模型映射仍以影子自身为准（与 forwardCodeBuddy 同口径）。
	shadow.Credentials["model_mapping"] = map[string]any{"public-ds": "deepseek-v4.1-flash"}
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)

	upstream := &webProbeUpstream{resp: codebuddyQuotaOKResponse()}
	svc := newCodeBuddyProbeTestService(map[int64]*Account{112: shadow, 109: parent}, upstream, codebuddyQuotaOKResponse())
	ctx, recorder := newWebTestContext()

	require.NoError(t, svc.TestAccountConnection(ctx, 112, "public-ds", "", AccountTestModeDefault))

	require.NotNil(t, upstream.lastReq, "codebuddy shadow must issue a codebuddy upstream probe")
	require.Equal(t, codeBuddyBillingMeterPath, upstream.lastReq.URL.Path)
	require.Equal(t, "www.codebuddy.cn", upstream.lastReq.URL.Host, "cn 站点配额端点域名必须来自站点 SSOT")
	// 凭证取自母账号，影子自身 credentials 为空。
	require.Equal(t, "Bearer PARENT_TOKEN_SECRET", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "https://www.codebuddy.cn", upstream.lastReq.Header.Get("Origin"))

	out := recorder.Body.String()
	require.NotContains(t, out, "No API key available", "影子账号不得再落到空凭证分支")
	require.Contains(t, out, `"type":"test_complete"`)
	require.Contains(t, out, `"success":true`)
	// 模型映射仍取影子自身（未被母账号替换）。
	require.Equal(t, "deepseek-v4.1-flash", parseTestStartModel(out))
	// 错误/成功文案绝不回显凭证值。
	require.NotContains(t, out, "PARENT_TOKEN_SECRET")
}

// TestAccountTestService_CodeBuddyShadowIntlUsesIntlHost intl 站点（母账号 site=intl）
// 出站域名必须是 www.codebuddy.ai，不能回落 cn。
func TestAccountTestService_CodeBuddyShadowIntlUsesIntlHost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(113, 109)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteIntl)

	upstream := &webProbeUpstream{resp: codebuddyQuotaOKResponse()}
	svc := newCodeBuddyProbeTestService(map[int64]*Account{113: shadow, 109: parent}, upstream, codebuddyQuotaOKResponse())
	ctx, recorder := newWebTestContext()

	require.NoError(t, svc.TestAccountConnection(ctx, 113, "", "", AccountTestModeDefault))
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "www.codebuddy.ai", upstream.lastReq.URL.Host)
	require.Equal(t, "https://www.codebuddy.ai", upstream.lastReq.Header.Get("Origin"))
	require.Contains(t, recorder.Body.String(), `"success":true`)
}

// TestAccountTestService_CodeBuddyShadowDoesNotFallThroughToClaude 解析后的
// credentialAccount 必须命中 CodeBuddy 分支（配额端点），而不是 claude 兜底
// （api.anthropic.com /v1/messages）。
func TestAccountTestService_CodeBuddyShadowDoesNotFallThroughToClaude(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(112, 109)
	parent := codebuddyProbeParentAccount(109, "")

	upstream := &webProbeUpstream{resp: codebuddyQuotaOKResponse()}
	svc := newCodeBuddyProbeTestService(map[int64]*Account{112: shadow, 109: parent}, upstream, codebuddyQuotaOKResponse())
	ctx, _ := newWebTestContext()

	require.NoError(t, svc.TestAccountConnection(ctx, 112, "", "", AccountTestModeDefault))
	require.NotNil(t, upstream.lastReq)
	require.NotEqual(t, "api.anthropic.com", upstream.lastReq.URL.Host, "codebuddy 不得掉进 claude 兜底")
	require.NotEqual(t, "/v1/messages", upstream.lastReq.URL.Path)
	require.Equal(t, codeBuddyBillingMeterPath, upstream.lastReq.URL.Path)
}

// TestAccountTestService_NativeCodeBuddyAccountUsesCodeBuddyProbe 原生 CodeBuddy 账号
// （非影子）同样走 CodeBuddy 分支：改造前无任何 codebuddy 出口，会掉进 claude 兜底。
func TestAccountTestService_NativeCodeBuddyAccountUsesCodeBuddyProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := codebuddyProbeParentAccount(109, "")

	upstream := &webProbeUpstream{resp: codebuddyQuotaOKResponse()}
	svc := newCodeBuddyProbeTestService(map[int64]*Account{109: account}, upstream, codebuddyQuotaOKResponse())
	ctx, recorder := newWebTestContext()

	require.NoError(t, svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault))
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, codeBuddyBillingMeterPath, upstream.lastReq.URL.Path)
	require.Equal(t, "Bearer PARENT_TOKEN_SECRET", upstream.lastReq.Header.Get("Authorization"))
	require.Contains(t, recorder.Body.String(), `"success":true`)
}

// TestAccountTestService_CodeBuddyProbeRejectedByUpstream 401/403 → 上游拒绝，
// 错误文案只含站点/端点/状态码，不回显凭证。
func TestAccountTestService_CodeBuddyProbeRejectedByUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := codebuddyProbeParentAccount(109, "")

	upstream := &webProbeUpstream{}
	svc := newCodeBuddyProbeTestService(map[int64]*Account{109: account}, upstream, &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"code":1,"msg":"offline user session"}`)),
	})
	ctx, recorder := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "rejected by upstream")
	require.Contains(t, err.Error(), "HTTP 401")
	require.NotContains(t, err.Error(), "PARENT_TOKEN_SECRET", "错误文案不得回显凭证")
	require.Contains(t, recorder.Body.String(), codeBuddyBillingMeterPath)
}

// TestAccountTestService_CodeBuddyShadowMissingParentFailsClosed 母账号缺失/解析失败
// 必须优雅报错（不 panic、不发任何上游请求）。
func TestAccountTestService_CodeBuddyShadowMissingParentFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(112, 404) // 母账号不存在

	upstream := &webProbeUpstream{resp: codebuddyQuotaOKResponse()}
	svc := newCodeBuddyProbeTestService(map[int64]*Account{112: shadow}, upstream, codebuddyQuotaOKResponse())
	ctx, recorder := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 112, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Failed to resolve account credentials")
	require.Nil(t, upstream.lastReq, "解析失败不得发出任何上游请求")
	require.Contains(t, recorder.Body.String(), "Failed to resolve account credentials")
}

// TestAccountTestService_CodeBuddyShadowParentNotSupportedFailsClosed 母账号不是
// 支持的 OAuth/APIKey 母账号时同样 fail-closed。
func TestAccountTestService_CodeBuddyShadowParentNotSupportedFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(112, 109)
	// 母账号是普通 deepseek apikey 账号 → 不是受支持的母账号类型。
	parent := dispatchAPIAccount(PlatformDeepseek, 109)

	upstream := &webProbeUpstream{resp: codebuddyQuotaOKResponse()}
	svc := newCodeBuddyProbeTestService(map[int64]*Account{112: shadow, 109: parent}, upstream, codebuddyQuotaOKResponse())
	ctx, _ := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 112, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Failed to resolve account credentials")
	require.Nil(t, upstream.lastReq)
}

// TestAccountTestService_CodeBuddyInvalidBaseURLFailsClosed 账号级 base_url 非法
// （明文 http）时显式报错，不静默回落站点默认。
func TestAccountTestService_CodeBuddyInvalidBaseURLFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := codebuddyProbeParentAccount(109, "")
	account.Credentials["base_url"] = "http://insecure.example.com"

	upstream := &webProbeUpstream{resp: codebuddyQuotaOKResponse()}
	svc := newCodeBuddyProbeTestService(map[int64]*Account{109: account}, upstream, codebuddyQuotaOKResponse())
	ctx, _ := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Invalid base URL")
	require.Nil(t, upstream.lastReq, "非法 base_url 不得发出上游请求")
}

// TestAccountTestService_CodeBuddyMissingTokenFailsClosed 母账号无凭证时报错且不发请求。
func TestAccountTestService_CodeBuddyMissingTokenFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := codebuddyProbeParentAccount(109, "")
	delete(account.Credentials, "access_token")

	upstream := &webProbeUpstream{resp: codebuddyQuotaOKResponse()}
	svc := newCodeBuddyProbeTestService(map[int64]*Account{109: account}, upstream, codebuddyQuotaOKResponse())
	ctx, _ := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing access_token credential")
	require.Nil(t, upstream.lastReq)
}

// TestAccountTestService_NonShadowCNAPIKeyAccountUnchanged 回归保护：非影子 CN apikey
// 账号仍走 /v1/chat/completions 探活，行为与改造前一致。
func TestAccountTestService_NonShadowCNAPIKeyAccountUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 与 dispatchAPIAccount 同形态，仅 base_url 用 https（本文件 cfg 不开放明文 http）。
	account := &Account{
		ID: 200, Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{
			"access_mode":  AccountAccessModeAPI,
			"api_key":      "sk-test-dispatch",
			"api_protocol": APIProtocolChatCompletions,
			"base_url":     "https://api.example",
		},
	}

	upstream := &webProbeUpstream{}
	svc := newCodeBuddyProbeTestService(map[int64]*Account{200: account}, upstream, chatCompletionsProbeResponse())
	ctx, recorder := newWebTestContext()

	require.NoError(t, svc.TestAccountConnection(ctx, 200, "test-model", "", AccountTestModeDefault))
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "/v1/chat/completions", upstream.lastReq.URL.Path)
	require.Equal(t, "Bearer sk-test-dispatch", upstream.lastReq.Header.Get("Authorization"))
	require.Contains(t, recorder.Body.String(), "已通过 /v1/chat/completions 验证")
	require.NotContains(t, recorder.Body.String(), "No API key available")
}

// --- 以下为独立验收补充用例（2026-09-22 验收会话追加） ---
//
// 目的：把实现代理的"存在性断言"升级为"行为断言"——出站次数、方法/路径/请求体/关键头、
// 业务信封判定、以及影子解析的三条 fail-closed 支线（母账号缺失 / 二级影子 / 母账号
// 平台不受支持 / 母账号是 web 模式 CN 账号）与非影子不解析回归。

// TestAccountTestService_CodeBuddyShadowProbeSendsExactlyOneRequest 探活只能发一次
// 上游请求（不重试刷量），且方法/URL/请求体/关键头必须与宣称一致。
func TestAccountTestService_CodeBuddyShadowProbeSendsExactlyOneRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(112, 109)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)

	svc, counting, repo := newCodeBuddyCountingService(
		map[int64]*Account{112: shadow, 109: parent}, codebuddyQuotaOKResponse())
	ctx, recorder := newWebTestContext()

	require.NoError(t, svc.TestAccountConnection(ctx, 112, "", "", AccountTestModeDefault))

	require.Equal(t, 1, counting.tlsCalls, "codebuddy 探活必须只发一次上游请求")
	require.Equal(t, 0, counting.doCalls, "不得额外走无指纹通道重试")
	require.Equal(t, 2, repo.calls, "影子账号须解析母账号（自身 + 母账号两次读取）")
	require.Contains(t, recorder.Body.String(), `"success":true`)
	require.NotContains(t, recorder.Body.String(), "No API key available")
}

// TestAccountTestService_CodeBuddyProbeRequestShape 校验出站请求形状：POST、站点
// SSOT 域名、零消耗配额端点、body {}、Bearer 母账号 token、身份头占位。
func TestAccountTestService_CodeBuddyProbeRequestShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(112, 109)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)
	parent.Credentials["uid"] = "u-1"
	parent.Credentials["enterprise_id"] = "e-1"

	svc, counting, _ := newCodeBuddyCountingService(
		map[int64]*Account{112: shadow, 109: parent}, codebuddyQuotaOKResponse())
	ctx, recorder := newWebTestContext()

	require.NoError(t, svc.TestAccountConnection(ctx, 112, "", "", AccountTestModeDefault))
	require.Equal(t, 1, counting.tlsCalls)

	// 通过内层桩取回实际请求。
	inner := counting.HTTPUpstream.(*webProbeUpstream)
	req := inner.lastReq
	require.NotNil(t, req)
	require.Equal(t, http.MethodPost, req.Method)
	require.Equal(t, "https://www.codebuddy.cn/v2/billing/meter/get-user-resource", req.URL.String())

	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, "{}", string(body), "探活请求体必须是零消耗的 {}")
	require.Equal(t, "Bearer PARENT_TOKEN_SECRET", req.Header.Get("Authorization"))
	require.Equal(t, "https://www.codebuddy.cn", req.Header.Get("Origin"))
	require.Equal(t, "https://www.codebuddy.cn/", req.Header.Get("Referer"))
	require.Equal(t, CodeBuddyClientUA, req.Header.Get("User-Agent"))
	require.Equal(t, "SaaS", req.Header.Get("X-Product"))
	require.Equal(t, "u-1", req.Header.Get("X-User-Id"))
	require.Equal(t, "e-1", req.Header.Get("X-Enterprise-Id"))
	require.Equal(t, "1", req.Header.Get("X-No-Domain"))
	require.Contains(t, recorder.Body.String(), `"success":true`)
}

// TestAccountTestService_CodeBuddyProbeDoesNotRetryOnTransportError 传输层失败
// 只发一次，不重试，且错误文案不回显凭证。
func TestAccountTestService_CodeBuddyProbeDoesNotRetryOnTransportError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(112, 109)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)

	svc, counting, _ := newCodeBuddyCountingService(
		map[int64]*Account{112: shadow, 109: parent}, codebuddyQuotaOKResponse())
	counting.tlsErr = errors.New("dial tcp www.codebuddy.cn:443: i/o timeout")
	ctx, recorder := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 112, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Equal(t, 1, counting.tlsCalls, "传输失败不得重试")
	require.Contains(t, err.Error(), "probe request failed")
	require.NotContains(t, err.Error(), "PARENT_TOKEN_SECRET")
	require.NotContains(t, recorder.Body.String(), "PARENT_TOKEN_SECRET")
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

// TestAccountTestService_CodeBuddyProbeStatusAndEnvelopeHandling 非 2xx 与业务信封
// code != 0 都判定为失败，且每次只发一次请求。
func TestAccountTestService_CodeBuddyProbeStatusAndEnvelopeHandling(t *testing.T) {
	cases := []struct {
		name     string
		resp     *http.Response
		errParts []string
	}{
		{
			name: "http500",
			resp: &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"code":1}`)),
			},
			errParts: []string{"probe failed", "HTTP 500"},
		},
		{
			name: "403",
			resp: &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"code":1}`)),
			},
			errParts: []string{"rejected by upstream", "HTTP 403"},
		},
		{
			name: "envelope-code",
			resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"code":1001,"msg":"offline user session"}`)),
			},
			errParts: []string{"rejected by upstream", "code=1001"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			account := codebuddyProbeParentAccount(109, CodeBuddySiteIntl)
			svc, counting, _ := newCodeBuddyCountingService(map[int64]*Account{109: account}, tc.resp)
			ctx, _ := newWebTestContext()

			err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
			require.Error(t, err)
			for _, part := range tc.errParts {
				require.Contains(t, err.Error(), part)
			}
			require.Equal(t, 1, counting.tlsCalls)
			require.NotContains(t, err.Error(), "PARENT_TOKEN_SECRET")
		})
	}
}

// TestAccountTestService_CodeBuddyShadowParentIsShadowFailsClosed 二级影子
// （母账号自身也是影子）必须 fail-closed：不 panic、不发上游请求。
func TestAccountTestService_CodeBuddyShadowParentIsShadowFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(112, 109)
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)
	parent.ParentAccountID = int64Ptr(108) // 母账号自身还是影子

	svc, counting, _ := newCodeBuddyCountingService(
		map[int64]*Account{112: shadow, 109: parent, 108: codebuddyProbeParentAccount(108, "")},
		codebuddyQuotaOKResponse())
	ctx, recorder := newWebTestContext()

	var err error
	require.NotPanics(t, func() {
		err = svc.TestAccountConnection(ctx, 112, "", "", AccountTestModeDefault)
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Failed to resolve account credentials")
	require.Equal(t, 0, counting.tlsCalls, "二级影子不得发出任何上游请求")
	require.Contains(t, recorder.Body.String(), "Failed to resolve account credentials")
}

// TestAccountTestService_CodeBuddyShadowParentOpenAIAPIKeyFailsClosed 母账号是
// openai 但非 OAuth（apikey）时不受支持，fail-closed。
func TestAccountTestService_CodeBuddyShadowParentOpenAIAPIKeyFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(112, 109)
	parent := &Account{
		ID: 109, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-parent-openai"},
	}

	svc, counting, _ := newCodeBuddyCountingService(
		map[int64]*Account{112: shadow, 109: parent}, codebuddyQuotaOKResponse())
	ctx, _ := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 112, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Failed to resolve account credentials")
	require.Equal(t, 0, counting.tlsCalls)
	require.NotContains(t, err.Error(), "sk-parent-openai", "错误文案不得回显母账号密钥")
}

// TestAccountTestService_CodeBuddyShadowWebCNParentFailsClosedAndSkipsWebProbe
// 影子母账号是 web 模式 CN 账号（zhipu + access_mode=web）时：resolveCredentialAccount
// 直接判为不受支持的母账号并 fail-closed，因此绝不会带着母账号进入
// testWebAccountConnection —— 即"影子自身 model_mapping 在 web 分支丢失"的场景
// 在本分发链上不可达（不构成回归）。
func TestAccountTestService_CodeBuddyShadowWebCNParentFailsClosedAndSkipsWebProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shadow := codebuddyProbeShadowAccount(112, 109)
	parent := dispatchWebAccount(PlatformZhipu, 109)

	svc, counting, _ := newCodeBuddyCountingService(
		map[int64]*Account{112: shadow, 109: parent}, okSSEResponse())
	ctx, recorder := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 112, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Failed to resolve account credentials")
	require.Equal(t, 0, counting.tlsCalls, "不得带着母账号走 web 探活")
	require.NotContains(t, recorder.Body.String(), "Web login session is healthy.")
	require.NotContains(t, err.Error(), "test-cookie=1", "错误文案不得回显母账号 cookie")
}

// TestAccountTestService_NonShadowAccountsAreNeverResolved 回归保护：非影子账号
// 不得去解析母账号（GetByID 只应发生一次，即取账号本身），行为与改造前完全一致。
func TestAccountTestService_NonShadowAccountsAreNeverResolved(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// CN apikey 账号（kimi/zhipu/deepseek）：改造前后都走 /v1/chat/completions。
	for _, platform := range []string{PlatformKimi, PlatformZhipu, PlatformDeepseek} {
		t.Run(platform, func(t *testing.T) {
			account := &Account{
				ID: 200, Platform: platform, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{
					"access_mode":  AccountAccessModeAPI,
					"api_key":      "sk-test-dispatch",
					"api_protocol": APIProtocolChatCompletions,
					"base_url":     "https://api.example",
				},
			}
			svc, counting, repo := newCodeBuddyCountingService(
				map[int64]*Account{200: account}, chatCompletionsProbeResponse())
			ctx, recorder := newWebTestContext()

			require.NoError(t, svc.TestAccountConnection(ctx, 200, "test-model", "", AccountTestModeDefault))
			require.Equal(t, 1, repo.calls, "非影子账号不得解析母账号")
			require.Equal(t, 1, counting.tlsCalls)
			inner := counting.HTTPUpstream.(*webProbeUpstream)
			require.NotNil(t, inner.lastReq)
			require.Equal(t, "/v1/chat/completions", inner.lastReq.URL.Path)
			require.Equal(t, "Bearer sk-test-dispatch", inner.lastReq.Header.Get("Authorization"))
			require.Contains(t, recorder.Body.String(), "已通过 /v1/chat/completions 验证")
		})
	}
}

// TestAccountTestService_NonShadowOpenAIOAuthStillUsesCodexProbe 非影子 openai
// oauth 账号仍走 ChatGPT Codex 探活，且不再触发母账号解析。
func TestAccountTestService_NonShadowOpenAIOAuthStillUsesCodexProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID: 300, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "OPENAI_ACCESS_TOKEN", "account_id": "acc-1"},
	}
	svc, counting, repo := newCodeBuddyCountingService(
		map[int64]*Account{300: account}, okSSEResponse())
	ctx, _ := newWebTestContext()

	_ = svc.TestAccountConnection(ctx, 300, "", "", AccountTestModeDefault)
	require.Equal(t, 1, repo.calls, "非影子账号不得解析母账号")
	require.Equal(t, 1, counting.tlsCalls)
	inner := counting.HTTPUpstream.(*webProbeUpstream)
	require.NotNil(t, inner.lastReq)
	require.Equal(t, "chatgpt.com", inner.lastReq.Host)
	require.Equal(t, "/backend-api/codex/responses", inner.lastReq.URL.Path)
	require.Equal(t, "Bearer OPENAI_ACCESS_TOKEN", inner.lastReq.Header.Get("Authorization"))
}
