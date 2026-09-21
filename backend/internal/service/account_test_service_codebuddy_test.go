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
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// codebuddyDispatchRepo 按 ID 返回账号（影子→母账号解析需要至少两个账号）。
type codebuddyDispatchRepo struct {
	AccountRepository
	accounts map[int64]*Account
}

func (r *codebuddyDispatchRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if acc, ok := r.accounts[id]; ok && acc != nil {
		return acc, nil
	}
	return nil, fmt.Errorf("account %d not found", id)
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
