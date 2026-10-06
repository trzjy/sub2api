package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// smsUpstream 按请求路径脚本化返回短信登录响应的 mock upstream。
type smsUpstream struct {
	requests []*http.Request

	zhipuSendStatus   int
	zhipuSendBody     string
	zhipuVerifyStatus int
	zhipuVerifyBody   string
	zhipuSetCookies   []string

	kimiSendStatus   int
	kimiSendBody     string
	kimiVerifyStatus int
	kimiVerifyBody   string
}

func (m *smsUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	m.requests = append(m.requests, req)
	path := req.URL.Path
	body := "{}"
	status := http.StatusOK
	var cookies []string
	switch {
	case strings.HasSuffix(path, webZhipuSendSMSCodeEndpoint):
		status, body = m.zhipuSendStatus, m.zhipuSendBody
	case strings.HasSuffix(path, webZhipuPhoneLoginEndpoint):
		status, body, cookies = m.zhipuVerifyStatus, m.zhipuVerifyBody, m.zhipuSetCookies
	case strings.HasSuffix(path, webKimiSendSMSCodeEndpoint):
		status, body = m.kimiSendStatus, m.kimiSendBody
	case strings.HasSuffix(path, webKimiLoginWithSMSEndpoint):
		status, body = m.kimiVerifyStatus, m.kimiVerifyBody
	}
	h := http.Header{}
	for _, c := range cookies {
		h.Add("Set-Cookie", c)
	}
	return newMockResp(status, h, body), nil
}

func (m *smsUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return m.Do(req, "", 0, 0)
}

var _ HTTPUpstream = (*smsUpstream)(nil)

func newSmsTestService(up *smsUpstream) *WebPlatformAutoLoginService {
	cfg := &config.Config{Security: config.SecurityConfig{
		URLAllowlist: config.URLAllowlistConfig{Enabled: false},
	}}
	return NewWebPlatformAutoLoginService(nil, up, cfg)
}

func smsErrorCode(t *testing.T, err error) int64 {
	t.Helper()
	var le *webLoginHTTPError
	require.ErrorAs(t, err, &le)
	return le.Code
}

func smsErrorKind(t *testing.T, err error) string {
	t.Helper()
	var le *webLoginHTTPError
	require.ErrorAs(t, err, &le)
	return le.Kind
}

// readJSONBody 读取请求体并 JSON 解码（用于断言请求体字段）。
func readJSONBody(req *http.Request, out any) error {
	if req == nil || req.Body == nil {
		return fmt.Errorf("empty request body")
	}
	return json.NewDecoder(req.Body).Decode(out)
}

// zhipuChallenge 数美滑块已求解回传的合法 challenge（tI 签名由方法内部 webZhipuComputeSign 生成）。
func zhipuChallenge() WebSMSChallenge {
	return WebSMSChallenge{
		ZhipuCaptchaRid:     "rid-abc",
		ZhipuCaptchaMD5:     "md5-xyz",
		ZhipuPhoneCode:      "86",
		KimiCaptchaValidate: "yidun-v1",
	}
}

// ---------------------------------------------------------------------------
// zhipu 发码（2026-09-22 21:40 生产抓包终证：POST /chatglm/user-api/user/login_captcha，
// body {phone, phone_code, pic_captcha_id, tm, fr, distinct_id}，无 md5 键；
// 成功响应 {"status":0,...}；旧 13 号证据 send_sms 端点实测 405 已证伪）
// ---------------------------------------------------------------------------

func TestWebSMS_ZhipuSendSuccess(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{"status":0,"message":"短信验证码已发送","result":null,"rid":"x"}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.NoError(t, err)
	require.Len(t, up.requests, 1)

	// 请求契约（抓包终证）：路径 /chatglm 前缀；body 六键对齐，无 md5。
	req := up.requests[0]
	require.Equal(t, webZhipuSendSMSCodeEndpoint, req.URL.Path)
	var body map[string]string
	require.NoError(t, readJSONBody(req, &body))
	require.Equal(t, "13800000000", body["phone"])
	require.Equal(t, "rid-abc", body["pic_captcha_id"])
	// 2026-09-22 抓包契约：发码 body.phone_code="+86"（带加号）；challenge 输入
	// 仍是 "86"，证明组包处发生归一（2026-09-22 登录步抓包终证登录 body.phone_code
	// 同为 "+86"，发码/登录两路径共用同一归一函数）。
	require.Equal(t, "+86", body["phone_code"])
	require.Equal(t, "pc", body["tm"])
	require.Equal(t, "default", body["fr"])
	require.Equal(t, "", body["distinct_id"])
	_, present := body["md5"]
	require.False(t, present, "抓包 body 无 md5 键")
}

// status==0 之外的历史白名单形态（success=true / code==0）保持成功
// （发码专用判定回退共享 zhipuBizSuccess：code==0 / ret==0 / success==true）。
func TestWebSMS_ZhipuSendSuccessLegacyShapes(t *testing.T) {
	for _, body := range []string{`{"success":true}`, `{"code":0}`} {
		up := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: body}
		svc := newSmsTestService(up)
		_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
		require.NoError(t, err, "body=%s", body)
	}
}

// 证据缺失 → 失败关闭：zhipu 发码缺数美滑块 rid，挑战值未随请求内回传即失败关闭。
func TestWebSMS_ZhipuSendFailClosedNoCaptcha(t *testing.T) {
	up := &smsUpstream{}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", WebSMSChallenge{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "挑战值")
	require.Equal(t, WebLoginKindWAF, smsErrorKind(t, err))
	require.Len(t, up.requests, 0) // 绝不发假参数请求，缺失即失败关闭
}

// WAF/PoW 以 HTTP 403 触发（smsHTTPStatusError 映射），直接失败关闭，无续跑路径。
func TestWebSMS_ZhipuSendWAFPoW(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusForbidden}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Equal(t, WebLoginCodePoW1, smsErrorCode(t, err))
	require.Equal(t, WebLoginKindWAF, smsErrorKind(t, err))
	require.Len(t, up.requests, 1)
}

// 限流以 HTTP 429 触发，映射为 Kind=Login 限流文案。
func TestWebSMS_ZhipuSendRateLimited(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusTooManyRequests, zhipuSendBody: `{}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Equal(t, WebLoginCodeHTTPTooMany, smsErrorCode(t, err))
	require.Equal(t, WebLoginKindLogin, smsErrorKind(t, err))
	require.Contains(t, err.Error(), "限流")
	require.Len(t, up.requests, 1)
}

func TestWebSMS_ZhipuSendHTTP429(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusTooManyRequests, zhipuSendBody: `{"code":0}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Equal(t, WebLoginCodeHTTPTooMany, smsErrorCode(t, err))
	require.Equal(t, WebLoginKindLogin, smsErrorKind(t, err))
	require.Contains(t, err.Error(), "限流")
	require.Len(t, up.requests, 1)
}

// 2xx + body 业务失败（success=false）→ 失败关闭透出文案。
func TestWebSMS_ZhipuSendBodyBizFailureFailsClosed(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{"success":false,"message":"blocked"}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "blocked")
	require.Len(t, up.requests, 1)
}

// 2xx + body 业务失败（code 非 0）→ 失败关闭透出文案。
func TestWebSMS_ZhipuSendBodyCodeFailureFailsClosed(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{"code":1001}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "code=1001")
	require.Len(t, up.requests, 1)
}

// 2xx + body 业务失败（status 非 0，2026-09-22 抓包响应使用 status 字段）→
// 发码专用判定失败关闭透出文案。
func TestWebSMS_ZhipuSendBodyStatusFailureFailsClosed(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{"status":500,"message":"upstream error"}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "status=500")
	require.Len(t, up.requests, 1)
}

// 已取证契约白名单：success=true 也算成功（与 code==0 同为已取证字段名）。
func TestWebSMS_ZhipuSendSuccessTrue(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{"success":true}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.NoError(t, err)
	require.Len(t, up.requests, 1)
}

// 2xx 但 body 无已取证成功标志（code/success 均缺失）→ 失败关闭（不再宽容放行）。
func TestWebSMS_ZhipuSendUnknownBodyFailsClosed(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{"ok":1,"message":"accepted"}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "缺少已取证成功标志")
	require.Len(t, up.requests, 1)
}

// 2xx 空 body → 失败关闭。
func TestWebSMS_ZhipuSendEmptyBodyFailsClosed(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "缺少已取证成功标志")
	require.Len(t, up.requests, 1)
}

// ---------------------------------------------------------------------------
// zhipu 登录（2026-09-22 22:59 登录步生产抓包终证：POST /chatglm/user-api/user/phone_login，
// body 七键 {phone, captcha, pic_captcha_id, phone_code:"+86", tm, fr, sensors_id}，
// 无任何签名键——三件套在请求头；成功响应 {"status":0,...,"result":{access_token,refresh_token}}，
// token 在 body result 内，非 Set-Cookie）
// ---------------------------------------------------------------------------

// status:0 + result 双 token 的成功正例：断言请求契约（路径 /chatglm 前缀、body
// 七键、无签名键、请求头含三件套）与结果提取（单一路径来自 body，Cookie 串按
// 既有同族键名组装）。
func TestWebSMS_ZhipuVerifySuccess(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"status":0,"message":"success","result":{"user_id":"u-1","access_token":"CT-1","refresh_token":"RFT-1"},"rid":"r-1"}`,
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.NoError(t, err)
	require.Equal(t, "CT-1", res.ChatGLMToken)
	require.Equal(t, "RFT-1", res.LoginRefreshToken)
	require.Contains(t, res.Cookie, "chatglm_token=CT-1")
	require.Contains(t, res.Cookie, "chatglm_refresh_token=RFT-1")
	require.Len(t, up.requests, 1)

	// 请求契约（2026-09-22 登录步抓包终证）：路径带 /chatglm 前缀，与发码同构。
	req := up.requests[0]
	require.Equal(t, webZhipuPhoneLoginEndpoint, req.URL.Path)

	// body 七键对齐；无任何签名键（timestamp/xNonce/sign 在请求头，不在 body）。
	var body map[string]string
	require.NoError(t, readJSONBody(req, &body))
	require.Len(t, body, 7)
	require.Equal(t, "13800000000", body["phone"])
	require.Equal(t, "123456", body["captcha"])
	require.Equal(t, "rid-abc", body["pic_captcha_id"])
	// phone_code 归一与发码同一函数：challenge 输入 "86"，body 为 "+86"。
	require.Equal(t, "+86", body["phone_code"])
	require.Equal(t, "pc", body["tm"])
	require.Equal(t, "default", body["fr"])
	require.Equal(t, "", body["sensors_id"])
	for _, key := range []string{"timestamp", "xNonce", "sign"} {
		_, present := body[key]
		require.False(t, present, "抓包 body 无 %s 键（签名三件套在请求头）", key)
	}

	// 请求头含签名三件套（与发码头同构，applyZhipuFingerprintHeaders 单一口径）。
	require.NotEmpty(t, req.Header.Get("x-sign"))
	require.NotEmpty(t, req.Header.Get("x-nonce"))
	require.NotEmpty(t, req.Header.Get("x-timestamp"))
}

// status:0 但 result 缺 token → 失败关闭（缺 access_token）。
func TestWebSMS_ZhipuVerifyStatusZeroMissingTokensFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"status":0,"message":"ok"}`,
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "缺少 result.access_token")
	require.Len(t, up.requests, 1)
}

// 强制 refresh token（收敛项 4，2026-09-22 登录步抓包终证契约）：status:0 但
// result 缺 refresh_token → 失败关闭，拒绝建号；不再存在"缺失允许空"的宽容分支。
func TestWebSMS_ZhipuVerifyMissingRefreshTokenFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"status":0,"message":"success","result":{"access_token":"CT-1"}}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "缺少 result.refresh_token")
	require.Contains(t, err.Error(), "拒绝建号")
	require.Equal(t, WebLoginKindLogin, smsErrorKind(t, err))
	require.Len(t, up.requests, 1)
}

// 防回归（旧白名单移除）：success=true 无 status → 登录失败关闭（2026-09-22
// 登录步抓包终证成功标志为顶层 status==0，旧 code/ret/success 白名单不再采信，
// 即使响应带双 Cookie 也绝不采纳）。
func TestWebSMS_ZhipuVerifySuccessTrueNoStatusFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"success":true}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-2; Path=/", "chatglm_refresh_token=RFT-2; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "缺少已取证成功标志")
	require.Len(t, up.requests, 1)
}

// 证据缺失 → 失败关闭：zhipu 登录缺数美滑块 rid，挑战值未随请求内回传即失败关闭。
func TestWebSMS_ZhipuVerifyFailClosedNoCaptchaRid(t *testing.T) {
	up := &smsUpstream{}
	svc := newSmsTestService(up)

	ch := zhipuChallenge()
	ch.ZhipuCaptchaRid = ""
	_, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", ch, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "挑战值")
	require.Equal(t, WebLoginKindWAF, smsErrorKind(t, err))
	require.Len(t, up.requests, 0)
}

// tI 签名三件套由方法内部 webZhipuComputeSign 生成并放入请求头（02-sign-algorithm.md
// 已取证；2026-09-22 登录步抓包终证三件套在请求头、body 无签名键），
// 不再要求调用方传入，故无「缺签名」失败关闭点。

// requireNoSMSCredentials 断言登录失败时未产出任何登录凭据：结果为 nil（无值），
// 或（防御性）Cookie/AccessToken/RefreshToken/ChatGLMToken/LoginRefreshToken 全为空串。
func requireNoSMSCredentials(t *testing.T, res *SMSLoginResult) {
	t.Helper()
	require.True(t,
		res == nil || (res.Cookie == "" && res.AccessToken == "" && res.RefreshToken == "" &&
			res.ChatGLMToken == "" && res.LoginRefreshToken == ""),
		"登录失败时不得返回任何凭据，got %+v", res)
}

// 短信码错误 → body 业务失败（code 非 0 且非空）→ 失败关闭：VerifySmsCode
// 返回错误且不产出任何凭据，疑似成功 Set-Cookie 一律不采纳。
func TestWebSMS_ZhipuVerifyWrongCode(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"code":403,"msg":"wrong code"}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "000000", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "403")
	require.Equal(t, WebLoginKindLogin, smsErrorKind(t, err))
	require.Len(t, up.requests, 1)
}

// HTTP 200 + body 业务失败（code=0 但 data 包裹被宽容放行，此处 code 非 0）→ 失败关闭。
func TestWebSMS_ZhipuVerifyHTTP200BizErrorFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"success":false,"message":"biz error"}`,
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "biz error")
	require.Len(t, up.requests, 1)
}

// HTTP 200 + 无 status 无失败标志（未知形状）→ 失败关闭（正向成功条件缺失：
// 顶层 status==0 才算成功，即使响应带 Set-Cookie 也不采纳）。
func TestWebSMS_ZhipuVerifyHTTP200UnknownBodyFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{}`,
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "GLM 登录响应缺少已取证成功标志")
	require.Len(t, up.requests, 1)
}

// 审查核心场景：2xx + 字符串数字错误码（"code":"403"）+ 错误响应带双 Cookie
// → 不得判成功（zhipuBizFailure 识别字符串数字，先于成功判定拦截；即使带
// Set-Cookie 也绝不采纳）。
func TestWebSMS_ZhipuVerifyStringCodeWithCookiesFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"code":"403","msg":"wrong code"}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/", "chatglm_refresh_token=RFT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "000000", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "code=403")
	require.Equal(t, WebLoginKindLogin, smsErrorKind(t, err))
	require.Len(t, up.requests, 1)
}

// 嵌套错误体（data.code 非 0）+ 响应带双 Cookie → 失败关闭，Cookie 不采纳。
func TestWebSMS_ZhipuVerifyNestedDataCodeWithCookiesFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"data":{"code":40002}}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/", "chatglm_refresh_token=RFT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "data.code=40002")
	require.Len(t, up.requests, 1)
}

// 旧白名单形态（success=true + Set-Cookie 双 Cookie）→ 失败关闭（成功标志为
// status==0 + result 双 token，旧形态不被采信，Cookie 不采纳）。
func TestWebSMS_ZhipuVerifyNoBizFlagWithCookiesFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"message":"accepted"}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/", "chatglm_refresh_token=RFT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "GLM 登录响应缺少已取证成功标志")
	require.Len(t, up.requests, 1)
}

// 防回归（2026-09-22 登录步抓包终证收敛）：status:0 但 result 缺 token → 失败关闭，
// 即使响应带同族 Set-Cookie 也绝不采纳（结果提取单一路径来自 body）。
func TestWebSMS_ZhipuVerifyStatusZeroWithCookiesFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"status":0,"message":"ok"}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/", "chatglm_refresh_token=RFT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "缺少 result.access_token")
	require.Len(t, up.requests, 1)
}

// 嵌套字符串数字（data.status:"500"）也算业务失败（类型容错覆盖嵌套位置）。
func TestWebSMS_ZhipuVerifyNestedDataStatusStringFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"data":{"status":"500"},"msg":"upstream error"}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/", "chatglm_refresh_token=RFT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "data.status=500")
	require.Len(t, up.requests, 1)
}

// 旧成功正例（success==true + 双 Cookie）已废弃：2026-09-22 登录步抓包终证成功
// 标志为顶层 status==0 + result 双 token，success=true 不再判成功（见
// TestWebSMS_ZhipuVerifySuccessTrueNoStatusFailsClosed）。

// 防回归（2026-09-22 整改轮 must_fix）：{"status":500,"success":true} 冲突响应
// + 双 Cookie → 顶层 status 非 0 先被共享 zhipuBizFailure 拦截，登录失败关闭，
// Cookie 绝不采纳。
func TestWebSMS_ZhipuVerifyStatusNonZeroWithSuccessTrueFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"status":500,"success":true}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/", "chatglm_refresh_token=RFT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "status=500")
	require.Len(t, up.requests, 1)
}

// 非正向状态：HTTP 302 带 Cookie 也不得继续判定（仅 2xx 属正向成功条件）。
func TestWebSMS_ZhipuVerifyNon2xxStatusFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusFound,
		zhipuVerifyBody:   `{"code":0}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/", "chatglm_refresh_token=RFT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Len(t, up.requests, 1)
}

// 结果提取单一路径来自 body：status:0 + result 双 token 且响应不带任何
// Set-Cookie → 仍判成功（不依赖 Set-Cookie；旧"缺 Cookie 即失败"分支随抓包
// 终证契约移除）。
func TestWebSMS_ZhipuVerifySuccessWithoutSetCookie(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"status":0,"message":"success","result":{"user_id":"u-1","access_token":"CT-3","refresh_token":"RFT-3"},"rid":"r-3"}`,
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.NoError(t, err)
	require.Equal(t, "CT-3", res.ChatGLMToken)
	require.Equal(t, "RFT-3", res.LoginRefreshToken)
	require.Contains(t, res.Cookie, "chatglm_token=CT-3")
	require.Contains(t, res.Cookie, "chatglm_refresh_token=RFT-3")
	require.Len(t, up.requests, 1)
}

// 旧 Cookie 提取路径（combineSetCookies/extractCookieValue）已随 2026-09-22 登录步
// 抓包终证移除：token 在 body result 内，结果提取不依赖 Set-Cookie；
// 「缺 refresh token → 失败关闭拒绝建号」用例由上方
// TestWebSMS_ZhipuVerifyMissingRefreshTokenFailsClosed 承载（status:0 + result
// 缺 refresh_token，且响应携带 chatglm_token Set-Cookie 也不采纳）。

// ---------------------------------------------------------------------------
// Lane G（任务卡 2026-09-22）：helper 同会话发码结果消费 / 会话上下文复用 /
// 非 2xx 最小脱敏诊断。测试值全用假数据（零凭据红线）。
// ---------------------------------------------------------------------------

// helperInt 辅助：构造 *int（goes into WebSMSChallenge 扩展字段）。
func helperInt(v int) *int { return &v }

// helper challenge：带 helper 同会话发码成功结果 + 会话上下文（假数据）。
func zhipuHelperChallenge(sendStatus, bodyStatus int) WebSMSChallenge {
	ch := zhipuChallenge()
	ch.ZhipuHelperSendStatus = helperInt(sendStatus)
	ch.ZhipuHelperSendBodyStatus = helperInt(bodyStatus)
	ch.ZhipuHelperSendMessage = "fake-helper-message"
	ch.ZhipuSessionCookie = "chatglm_token=fake-guest; other=fake"
	ch.ZhipuDeviceID = "fake-device-id"
	return ch
}

// send_status 存在且 status==0（helper 同会话发码成功）→ 成功，后端不再重复发码
// （upstream 零请求）；send_status/body_status 为 nil（Lane H 未上线）→ 沿用后端直发。
func TestWebSMS_ZhipuHelperSendStatusConsumed(t *testing.T) {
	up := &smsUpstream{}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuHelperChallenge(200, 0), nil)
	require.NoError(t, err)
	require.Len(t, up.requests, 0, "helper 同会话已发码，后端绝不重复发码")

	// Lane H 未上线兼容：字段缺失 → 后端直发路径照常执行。
	up2 := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{"status":0}`}
	svc2 := newSmsTestService(up2)
	_, err = svc2.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.NoError(t, err)
	require.Len(t, up2.requests, 1)
}

// send_status 非 2xx → 失败关闭，状态码透出，后端不重复发码。
func TestWebSMS_ZhipuHelperSendStatusNon2xxFailsClosed(t *testing.T) {
	up := &smsUpstream{}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuHelperChallenge(400, -1), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 400")
	require.Len(t, up.requests, 0)
}

// 2xx 但 body_status 缺失 → 失败关闭（无成功证据，不重复发码）。
func TestWebSMS_ZhipuHelperSendMissingBodyStatusFailsClosed(t *testing.T) {
	up := &smsUpstream{}
	svc := newSmsTestService(up)

	ch := zhipuHelperChallenge(200, 0)
	ch.ZhipuHelperSendBodyStatus = nil
	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", ch, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "缺少 status 判定字段")
	require.Len(t, up.requests, 0)
}

// body_status 非 0 → 失败关闭透出，不重复发码。
func TestWebSMS_ZhipuHelperSendBodyStatusNonZeroFailsClosed(t *testing.T) {
	up := &smsUpstream{}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuHelperChallenge(200, 1204), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "status=1204")
	require.Len(t, up.requests, 0)
}

// 会话上下文（cookies/device_id）一旦存在必须附加到出站请求头（Cookie 头 + x-device-id 头）。
// 发码路径：send_status 存在时后端不重复发码（无出站可断言）；send_status 缺失但
// cookies/device_id 存在（Lane H 部分上线）→ 后端直发且必须携带会话头。
func TestWebSMS_ZhipuSessionContextApplied(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{"status":0}`}
	svc := newSmsTestService(up)

	ch := zhipuChallenge()
	ch.ZhipuSessionCookie = "chatglm_token=fake-guest; other=fake"
	ch.ZhipuDeviceID = "fake-device-id"
	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", ch, nil)
	require.NoError(t, err)
	require.Len(t, up.requests, 1)
	require.Equal(t, "chatglm_token=fake-guest; other=fake", up.requests[0].Header.Get("Cookie"))
	require.Equal(t, "fake-device-id", up.requests[0].Header.Get("x-device-id"))

	// 字段全缺（Lane H 未上线）→ 后端直发且不带会话头（沿用现行为）。
	up2 := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{"status":0}`}
	svc2 := newSmsTestService(up2)
	_, err = svc2.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.NoError(t, err)
	require.Len(t, up2.requests, 1)
	require.Equal(t, "", up2.requests[0].Header.Get("Cookie"))
	require.Equal(t, "", up2.requests[0].Header.Get("x-device-id"))
}

// 登录步复用会话上下文：Cookie 头 + x-device-id 头附加到 phone_login 出站请求。
func TestWebSMS_ZhipuVerifyReusesSessionContext(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"status":0,"message":"success","result":{"access_token":"CT-1","refresh_token":"RFT-1"}}`,
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuHelperChallenge(200, 0), nil)
	require.NoError(t, err)
	require.NotEmpty(t, res.Cookie)
	require.Len(t, up.requests, 1)
	req := up.requests[0]
	require.Equal(t, "chatglm_token=fake-guest; other=fake", req.Header.Get("Cookie"))
	require.Equal(t, "fake-device-id", req.Header.Get("x-device-id"))
	// 既有签名三件套与指纹头不受影响。
	require.NotEmpty(t, req.Header.Get("x-sign"))
	require.NotEmpty(t, req.Header.Get("app-name"))
}

// 非 2xx 最小脱敏诊断：仅状态码 + json_keys + 上下文存在布尔，绝不落 Cookie/device_id 值
// （零凭据红线，smsJSONShape 复用既有零值形状器）。
func TestWebSMS_ZhipuNon2xxDiagnosticsZeroCredential(t *testing.T) {
	// 后端直发路径 400：捕获 slog 输出，断言诊断只含状态码 + json_keys 形状 + 上下文
	// 存在布尔，绝不落 Cookie/device_id/响应体值（零凭据红线直接证据）。
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	up := &smsUpstream{zhipuSendStatus: http.StatusBadRequest, zhipuSendBody: `{"message":"bad request fake"}`}
	svc := newSmsTestService(up)
	ch := zhipuChallenge()
	ch.ZhipuSessionCookie = "chatglm_token=fake-guest"
	ch.ZhipuDeviceID = "fake-device-id"
	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", ch, nil)
	require.Error(t, err)
	require.Len(t, up.requests, 1)

	logged := buf.String()
	require.Contains(t, logged, "status=400")
	require.Contains(t, logged, "json_keys=")
	require.Contains(t, logged, "has_session_cookie=true")
	require.Contains(t, logged, "has_session_device_id=true")
	// 红线：Cookie/device_id 值与响应体值绝不进日志。
	require.NotContains(t, logged, "fake-guest")
	require.NotContains(t, logged, "fake-device-id")
	require.NotContains(t, logged, "bad request fake")
}

func TestWebSMS_KimiSendSuccess(t *testing.T) {
	up := &smsUpstream{kimiSendStatus: http.StatusOK, kimiSendBody: `{}`}
	svc := newSmsTestService(up)

	ch := WebSMSChallenge{KimiCaptchaValidate: "yidun-v1"}
	session, err := svc.SendSmsCode(context.Background(), PlatformKimi, "19900000000", ch, nil)
	require.NoError(t, err)
	// E0 取证：SendVerifyCodeResponse 为 proto 空消息，发码成功返回空 session（无 smsToken）。
	require.Equal(t, "", session)
	require.Equal(t, webKimiSendSMSCodeEndpoint, up.requests[0].URL.Path)
	// host 必须为 auth.kimi.com（oauth 命名空间），而非 www.kimi.com。
	require.Equal(t, webKimiAuthBaseURL+webKimiSendSMSCodeEndpoint, up.requests[0].URL.String())
}

// 证据缺失 → 失败关闭：kimi 发码缺易盾 validate，挑战值未随请求内回传即失败关闭。
func TestWebSMS_KimiSendFailClosedNoValidate(t *testing.T) {
	up := &smsUpstream{}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformKimi, "19900000000", WebSMSChallenge{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "挑战值")
	require.Equal(t, WebLoginKindWAF, smsErrorKind(t, err))
	require.Len(t, up.requests, 0) // 绝不发假参数请求
}

// E0 取证：发码成功 = HTTP 2xx，响应体为空（SendVerifyCodeResponse proto 空消息），
// 返回空 session（不探测 smsToken/verifyCode 等虚构字段）。
func TestWebSMS_KimiSendSuccessEmpty(t *testing.T) {
	up := &smsUpstream{kimiSendStatus: http.StatusOK, kimiSendBody: `{}`}
	svc := newSmsTestService(up)

	ch := WebSMSChallenge{KimiCaptchaValidate: "yidun-v1"}
	session, err := svc.SendSmsCode(context.Background(), PlatformKimi, "19900000000", ch, nil)
	require.NoError(t, err)
	require.Equal(t, "", session)
	require.Len(t, up.requests, 1)
}

// ---------------------------------------------------------------------------
// kimi 登录（证据：AuthService.loginWithSMS，请求体不带 captcha）
// ---------------------------------------------------------------------------

func TestWebSMS_KimiVerifySuccess(t *testing.T) {
	up := &smsUpstream{
		kimiVerifyStatus: http.StatusOK,
		// 线上实测：LoginWithSMSResponse 为 Connect unary 顶层 JSON（camelCase），
		// 无 data 包裹。字段包括 accessToken/refreshToken/userId 等。
		kimiVerifyBody: `{"accessToken":"AT-9","refreshToken":"RT-9","userId":"kimi-user-1"}`,
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformKimi, "19900000000", "123456", WebSMSChallenge{}, nil)
	require.NoError(t, err)
	require.Equal(t, "AT-9", res.AccessToken)
	require.Equal(t, "RT-9", res.RefreshToken)
	require.Equal(t, "RT-9", res.LoginRefreshToken)
	require.Equal(t, webKimiLoginWithSMSEndpoint, up.requests[0].URL.Path)
	require.Equal(t, webKimiAuthBaseURL+webKimiLoginWithSMSEndpoint, up.requests[0].URL.String())
}

// E0 取证：kimi 登录响应按 LoginWithSMSResponse 单一路径提取，不探测业务码。
// 真实 WAF/PoW 以 HTTP 403 触发（smsHTTPStatusError 映射 PoW1），直接失败关闭，无续跑路径。
func TestWebSMS_KimiVerifyPoW(t *testing.T) {
	up := &smsUpstream{kimiVerifyStatus: http.StatusForbidden}
	svc := newSmsTestService(up)

	_, err := svc.VerifySmsCode(context.Background(), PlatformKimi, "19900000000", "123456", WebSMSChallenge{}, nil)
	require.Error(t, err)
	require.Equal(t, WebLoginCodePoW1, smsErrorCode(t, err))
	require.Equal(t, WebLoginKindWAF, smsErrorKind(t, err))
	// 真实 WAF/PoW（HTTP 403）失败关闭，无续跑路径：仅一次原始提交请求。
	require.Len(t, up.requests, 1)
}

func TestWebSMS_KimiSendUnknownResponseFieldFailsClosed(t *testing.T) {
	up := &smsUpstream{kimiSendStatus: http.StatusOK, kimiSendBody: `{"session_token":"unexpected"}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformKimi, "19900000000", WebSMSChallenge{KimiCaptchaValidate: "yidun-v1"}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "响应不符合")
	require.Len(t, up.requests, 1)
}

func TestWebSMS_KimiVerifyMalformedResponseFailsClosed(t *testing.T) {
	up := &smsUpstream{kimiVerifyStatus: http.StatusOK, kimiVerifyBody: `{not-json`}
	svc := newSmsTestService(up)

	_, err := svc.VerifySmsCode(context.Background(), PlatformKimi, "19900000000", "123456", WebSMSChallenge{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "解析失败")
}

// ---------------------------------------------------------------------------
// 参数校验
// ---------------------------------------------------------------------------

func TestWebSMS_MissingPhone(t *testing.T) {
	svc := newSmsTestService(&smsUpstream{})
	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "  ", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Equal(t, WebLoginKindLogin, smsErrorKind(t, err))
}

func TestWebSMS_UnsupportedPlatform(t *testing.T) {
	svc := newSmsTestService(&smsUpstream{})
	_, err := svc.SendSmsCode(context.Background(), "deepseek", "13800000000", WebSMSChallenge{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "暂不支持")
}

// smsJSONShape 只输出结构（键名/类型/长度），绝不输出字段值——诊断日志零凭据红线。
func TestSMSJSONShapeNoValueLeak(t *testing.T) {
	raw := []byte(`{"access_token":"SECRET-AT-VALUE","refresh_token":"SECRET-RT","new_user":false,"deactivating":{"reason":"SECRET-REASON"},"list":[1,2,3]}`)
	shape := smsJSONShape(raw)
	require.Contains(t, shape, "access_token=string(len=15)")
	require.NotContains(t, shape, "SECRET")
	require.NotContains(t, shape, "SECRET-AT-VALUE")
	require.Contains(t, shape, "new_user=scalar")
	require.Contains(t, shape, "deactivating=object{reason}")
	require.Contains(t, shape, "list=array(len=3)")
}

func TestSMSJSONShapeNonObject(t *testing.T) {
	require.Equal(t, "not-an-object", smsJSONShape([]byte(`[1,2]`)))
	require.Equal(t, "not-an-object", smsJSONShape([]byte(`"x"`)))
}

// smsConnectErrorShape 从 Connect 错误信封提取 code 与 details 形状，
// **绝不输出 details[].value（base64 proto 载荷）与 debug 的值**——诊断日志零凭据红线。
func TestSMSConnectErrorShapeExtractsCodeAndDetailType(t *testing.T) {
	// 形态取自生产实证：顶层只有 code + details；value 为不可泄露的 base64 载荷。
	raw := []byte(`{"code":"unauthenticated","details":[` +
		`{"type":"account.gateway.v1.SMSVerifyError","value":"Q0xBU1NJRklFRC1WQUxVRS0xMzgwMDAwMDAwMA==",` +
		`"debug":{"reason":"sms code expired","phone":"13800000000"}}]}`)
	code, details := smsConnectErrorShape(raw)
	require.Equal(t, "unauthenticated", code)
	require.Contains(t, details, "#0{")
	require.Contains(t, details, "type=account.gateway.v1.SMSVerifyError")
	require.Contains(t, details, "value_present=true")
	require.Contains(t, details, "debug_keys=phone,reason")

	// 红线：base64 载荷、debug 值、手机号一律不出现在输出里。
	require.NotContains(t, details, "Q0xBU1NJRklFRC1WQUxVRS0xMzgwMDAwMDAwMA==")
	require.NotContains(t, details, "13800000000")
	require.NotContains(t, details, "sms code expired")
}

// 非信封体 / 非法 JSON / details 非数组：优雅降级不 panic，也不泄露内容。
func TestSMSConnectErrorShapeDegrades(t *testing.T) {
	code, details := smsConnectErrorShape([]byte(`not json`))
	require.Equal(t, "", code)
	require.Equal(t, smsConnectUnparsed, details)

	code, details = smsConnectErrorShape([]byte(`[1,2]`))
	require.Equal(t, "", code)
	require.Equal(t, smsConnectUnparsed, details)

	code, details = smsConnectErrorShape([]byte(`{"code":"unauthenticated"}`))
	require.Equal(t, "unauthenticated", code)
	require.Equal(t, "", details)

	code, details = smsConnectErrorShape([]byte(`{"code":"invalid_argument","details":"oops"}`))
	require.Equal(t, "invalid_argument", code)
	require.Equal(t, smsConnectUnparsed, details)

	code, details = smsConnectErrorShape([]byte(`{"code":"x","details":[1,"two"]}`))
	require.Equal(t, "x", code)
	require.Contains(t, details, "#0{<unparsed>}")
	require.Contains(t, details, "#1{<unparsed>}")
}
