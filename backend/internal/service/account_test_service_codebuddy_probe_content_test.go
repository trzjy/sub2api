package service

// CodeBuddy 探活 2xx 响应体内容判定回归测试（与 web 探活同源缺陷修复）。
//
// 背景：get-user-resource 配额端点的认证/业务错误随 HTTP 200 返回，上游异常时还可能
// 只回空 body。此前 testCodeBuddyAccountConnection 只看 HTTP 状态码 + `gjson.Int()` 判定
// 业务信封，两种形态被误判成 healthy：
//  1. 空 body：取不到 code 字段 → 跳过判定 → 直接判成功；
//  2. 字符串 code：gjson.Int() 对 "unauthenticated" 返回 0 → 同样跳过判定。
//
// 本文件锁定修复后的口径：2xx 必须「body 非空 + 无错误码（数字 != 0 / 字符串非空且
// 非 "0"）」才判健康；识别到认证语义按登录态失效口径报错。
//
// 安全红线：任何失败文案不得回显凭证值（token/api_key/base_url）或上游原文。

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newCodeBuddyContentProbeService 回放一条指定状态码/响应体的探活上游响应（不发起真实网络）。
func newCodeBuddyContentProbeService(statusCode int, body string) (*AccountTestService, *webProbeUpstream) {
	resp := &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	// codebuddyProbeParentAccount 的凭证值是 PARENT_TOKEN_SECRET，用作红线断言样本。
	parent := codebuddyProbeParentAccount(109, CodeBuddySiteCN)
	upstream := &webProbeUpstream{resp: resp}
	return newCodeBuddyProbeTestService(map[int64]*Account{109: parent}, upstream, resp), upstream
}

// --- HTTP 200 + 空 body → 失败（不得再把空返回判成成功） ---

func TestCodeBuddyProbe_EmptyBodyFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newCodeBuddyContentProbeService(http.StatusOK, "")
	ctx, rec := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty response", "空返回必须带空响应语义")
	require.Contains(t, err.Error(), codeBuddyBillingMeterPath, "失败文案须含站点/端点")

	body := rec.Body.String()
	require.Contains(t, body, "empty response")
	require.Contains(t, body, "test_start", "既有事件结构必须保留")
	require.NotContains(t, body, `"success":true`, "空 body 不得判成功")
	require.NotContains(t, body, "account is healthy")
	require.NotContains(t, body, "PARENT_TOKEN_SECRET", "失败文案不得回显 token 明文")
}

// 纯空白字符（不可见脏数据）同样算空响应，不得绕过 emptiness 判定。
func TestCodeBuddyProbe_WhitespaceOnlyBodyFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newCodeBuddyContentProbeService(http.StatusOK, "\r\n  \t\n")
	ctx, rec := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty response")
	require.NotContains(t, rec.Body.String(), `"success":true`)
	require.NotContains(t, rec.Body.String(), "PARENT_TOKEN_SECRET")
}

// --- HTTP 200 + 字符串 code → 失败（gjson.Int() 对字符串返回 0 的历史漏判） ---

func TestCodeBuddyProbe_StringCodeUnauthenticatedFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newCodeBuddyContentProbeService(http.StatusOK, `{"code":"unauthenticated","msg":"offline user session"}`)
	ctx, rec := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "rejected by upstream")
	require.Contains(t, err.Error(), "login state invalid", "认证类语义须按登录态失效口径报错")

	body := rec.Body.String()
	require.NotContains(t, body, `"success":true`)
	require.NotContains(t, body, "account is healthy")
	require.NotContains(t, body, "PARENT_TOKEN_SECRET", "失败文案不得回显 token 明文")
	require.NotContains(t, body, "unauthenticated}", "失败文案不得回显上游原文")
	require.NotContains(t, body, "offline user session", "失败文案不得回显上游原文")
}

// 字符串形态的数字 code（"1001"）同样拦截，且回显安全数值（全数字才回显）。
func TestCodeBuddyProbe_StringNumericCodeFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newCodeBuddyContentProbeService(http.StatusOK, `{"code":"1001"}`)
	ctx, rec := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "rejected by upstream")
	require.Contains(t, err.Error(), "code=1001")
	require.NotContains(t, rec.Body.String(), `"success":true`)
}

// 非数字字符串 code（"forbidden scope"）拦截但不回显原文。
func TestCodeBuddyProbe_ArbitraryStringCodeFailsClosedWithoutEcho(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newCodeBuddyContentProbeService(http.StatusOK, `{"code":"quota account frozen"}`)
	ctx, rec := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream business error")
	require.NotContains(t, err.Error(), "quota account frozen", "非数字 code 不得回显原文")
	require.NotContains(t, rec.Body.String(), `"success":true`)
}

// --- HTTP 200 + 数字 code != 0 → 失败（既有行为保持） ---

func TestCodeBuddyProbe_NonZeroNumericCodeFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newCodeBuddyContentProbeService(http.StatusOK, `{"code":1001,"msg":"offline user session"}`)
	ctx, rec := newWebTestContext()

	err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
	require.Error(t, err)
	require.Contains(t, err.Error(), "rejected by upstream")
	require.Contains(t, err.Error(), "code=1001")
	require.NotContains(t, rec.Body.String(), `"success":true`)
	require.NotContains(t, rec.Body.String(), "offline user session", "失败文案不得回显上游原文")
}

// --- HTTP 200 + 非空 body + code=0 → 成功（健康路径不得被新判据误杀） ---

func TestCodeBuddyProbe_NonEmptyZeroCodeSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, upstream := newCodeBuddyContentProbeService(http.StatusOK,
		`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacitySize":100,"CapacityUsed":1}]}}}}`)
	ctx, rec := newWebTestContext()

	require.NoError(t, svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault))
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, codeBuddyBillingMeterPath, upstream.lastReq.URL.Path)

	body := rec.Body.String()
	require.Contains(t, body, "account is healthy")
	require.Contains(t, body, `"type":"test_complete"`)
	require.Contains(t, body, `"success":true`)
	require.NotContains(t, body, "PARENT_TOKEN_SECRET")
}

// 无 code 字段但有实际内容（部分站点成功报文形态）→ 成功，不得因缺 code 误判为空。
func TestCodeBuddyProbe_NonEmptyBodyWithoutCodeSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, _ := newCodeBuddyContentProbeService(http.StatusOK, `{"data":{"Response":{"Data":{}}}}`)
	ctx, rec := newWebTestContext()

	require.NoError(t, svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault))
	require.Contains(t, rec.Body.String(), `"success":true`)
	require.NotContains(t, rec.Body.String(), "PARENT_TOKEN_SECRET")
}

// --- 401 / 403：既有分支行为不变，且不受内容判定影响 ---

func TestCodeBuddyProbe_401And403KeepExistingBehavior(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		respBody   string
	}{
		{name: "401", statusCode: http.StatusUnauthorized, respBody: `{"code":1,"msg":"offline user session"}`},
		{name: "403", statusCode: http.StatusForbidden, respBody: `{"code":1,"msg":"forbidden"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			svc, upstream := newCodeBuddyContentProbeService(tc.statusCode, tc.respBody)
			ctx, rec := newWebTestContext()

			err := svc.TestAccountConnection(ctx, 109, "", "", AccountTestModeDefault)
			require.Error(t, err)
			require.Contains(t, err.Error(), "rejected by upstream")
			require.Contains(t, err.Error(), "HTTP "+tc.name)
			require.NotContains(t, err.Error(), "empty response", "401/403 不得被内容判据改写口径")
			require.NotContains(t, err.Error(), "PARENT_TOKEN_SECRET")

			body := rec.Body.String()
			require.NotContains(t, body, `"success":true`)
			require.NotContains(t, body, "offline user session", "失败文案不得回显上游原文")
			// 401/403 不重试：仍只有一次出站。
			require.NotNil(t, upstream.lastReq)
		})
	}
}

// --- evaluateCodeBuddyProbeBody 单元级矩阵：直接锁定判定边界 ---

func TestEvaluateCodeBuddyProbeBody(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		reason string
	}{
		{name: "empty", body: "", reason: "empty response"},
		{name: "whitespace-only", body: "  \n\t ", reason: "empty response"},
		{name: "string-code-unauthenticated", body: `{"code":"unauthenticated"}`, reason: "login state invalid"},
		{name: "auth-in-msg", body: `{"code":0,"msg":"UNAUTHENTICATED: token expired"}`, reason: "login state invalid"},
		{name: "string-code-zero", body: `{"code":"0"}`, reason: ""},
		{name: "string-code-empty", body: `{"code":""}`, reason: ""},
		{name: "string-code-numeric", body: `{"code":"1001"}`, reason: "code=1001"},
		{name: "numeric-code-zero", body: `{"code":0,"data":{}}`, reason: ""},
		{name: "no-code-but-content", body: `{"data":{"ok":true}}`, reason: ""},
		{name: "plain-text-content", body: "OK", reason: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateCodeBuddyProbeBody([]byte(tc.body))
			if tc.reason == "" {
				require.Empty(t, got, "健康形态不得返回失败原因")
				return
			}
			require.NotEmpty(t, got)
			require.Contains(t, got, tc.reason)
		})
	}
}
