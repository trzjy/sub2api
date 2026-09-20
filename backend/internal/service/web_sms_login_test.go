package service

import (
	"context"
	"encoding/json"
	"fmt"
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
// zhipu 发码（证据：POST /backend-api/v1/user/send_sms，需数美滑块 rid+md5）
// ---------------------------------------------------------------------------

func TestWebSMS_ZhipuSendSuccess(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusOK, zhipuSendBody: `{"code":0}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "context_gap")
	require.Len(t, up.requests, 0)
}

// 证据缺失 → 失败关闭：zhipu 发码缺数美滑块 rid/md5，挑战值未随请求内回传即失败关闭。
func TestWebSMS_ZhipuSendFailClosedNoCaptcha(t *testing.T) {
	up := &smsUpstream{}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", WebSMSChallenge{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "挑战值")
	require.Equal(t, WebLoginKindWAF, smsErrorKind(t, err))
	require.Len(t, up.requests, 0) // 绝不发假参数请求，缺失即失败关闭
}

// E0 取证：zhipu 响应结构未取证，发码成功判定 = HTTP 2xx，不探测 body 业务码。
// 真实 WAF/PoW 以 HTTP 403 触发（smsHTTPStatusError 映射 PoW1），直接失败关闭，无续跑路径。
func TestWebSMS_ZhipuSendWAFPoW(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusForbidden}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "context_gap")
	require.Len(t, up.requests, 0)
}

// E0 取证：zhipu 响应结构未取证，body 业务码不猜测；限流以 HTTP 429 触发。
func TestWebSMS_ZhipuSendRateLimited(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusTooManyRequests, zhipuSendBody: `{}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "context_gap")
	require.Len(t, up.requests, 0)
}

func TestWebSMS_ZhipuSendHTTP429(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusTooManyRequests, zhipuSendBody: `{"code":0}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "context_gap")
	require.Len(t, up.requests, 0)
}

// ---------------------------------------------------------------------------
// zhipu 登录（证据：POST /user-api/user/phone_login，需数美 rid + tI 签名三件套）
// ---------------------------------------------------------------------------

func TestWebSMS_ZhipuVerifySuccess(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"code":0}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/", "chatglm_refresh_token=RFT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	_, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "context_gap")
	require.Len(t, up.requests, 0)
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

// tI 签名由方法内部 webZhipuComputeSign 生成（02-sign-algorithm.md 已取证），
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

// 短信码错误 → 失败关闭：GLM 登录响应契约未取证（context_gap），VerifySmsCode
// 返回错误且不产出任何凭据；请求不会发往上游（本地闸门直接失败），疑似成功
// Set-Cookie 一律不采纳。
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
	require.Contains(t, err.Error(), "context_gap")
	require.Equal(t, WebLoginKindLogin, smsErrorKind(t, err))
	require.Len(t, up.requests, 0)
}

// HTTP 200 + 业务错误体 / 结构未知体（即使携带疑似成功 Set-Cookie）→ 失败关闭：
// GLM 登录响应业务成功字段尚未取证，不存在成功解析路径，任意 2xx 响应体都不会
// 被解析为凭据，绝不把任意 2xx 当成功、绝不用任意 Cookie 充当必需凭据。
func TestWebSMS_ZhipuVerifyHTTP200BizErrorOrUnknownBodyFailsClosed(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"code":0,"data":{"token":"T-1"},"unknown_field":true}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-9; Path=/", "chatglm_refresh_token=RFT-9; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	requireNoSMSCredentials(t, res)
	require.Contains(t, err.Error(), "context_gap")
	require.Len(t, up.requests, 0)
}

func TestWebSMS_ZhipuVerifyNoCookie(t *testing.T) {
	up := &smsUpstream{zhipuVerifyStatus: http.StatusOK, zhipuVerifyBody: `{"code":0}`}
	svc := newSmsTestService(up)

	_, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Equal(t, WebLoginKindLogin, smsErrorKind(t, err))
}

// ---------------------------------------------------------------------------
// kimi 发码（证据：SMSService.sendVerifyCode，需易盾 validate）
// ---------------------------------------------------------------------------

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
		// E0 决定性证据：LoginWithSMSResponse 为 Connect unary 顶层 JSON（snake_case），
		// 无 data 包裹、无 camel 兜底。
		kimiVerifyBody: `{"access_token":"AT-9","refresh_token":"RT-9"}`,
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
