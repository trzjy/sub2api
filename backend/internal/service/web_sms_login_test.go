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

	session, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.NoError(t, err)
	require.Equal(t, "", session) // zhipu 协议无 session_token，登录时以 phone+code 直接验证
	require.Len(t, up.requests, 1)
	require.Equal(t, webZhipuSendSMSCodeEndpoint, up.requests[0].URL.Path)
	require.Equal(t, DefaultWebZhipuBaseURL+webZhipuSendSMSCodeEndpoint, up.requests[0].URL.String())
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
	require.Equal(t, WebLoginCodePoW1, smsErrorCode(t, err))
	require.Equal(t, WebLoginKindWAF, smsErrorKind(t, err))
	// 真实 WAF/PoW（HTTP 403）失败关闭，无续跑路径：仅一次原始发码请求。
	require.Len(t, up.requests, 1)
}

// E0 取证：zhipu 响应结构未取证，body 业务码不猜测；限流以 HTTP 429 触发。
func TestWebSMS_ZhipuSendRateLimited(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusTooManyRequests, zhipuSendBody: `{}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Equal(t, WebLoginCodeHTTPTooMany, smsErrorCode(t, err))
}

func TestWebSMS_ZhipuSendHTTP429(t *testing.T) {
	up := &smsUpstream{zhipuSendStatus: http.StatusTooManyRequests, zhipuSendBody: `{"code":0}`}
	svc := newSmsTestService(up)

	_, err := svc.SendSmsCode(context.Background(), PlatformZhipu, "13800000000", zhipuChallenge(), nil)
	require.Error(t, err)
	require.Equal(t, WebLoginCodeHTTPTooMany, smsErrorCode(t, err))
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

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "123456", zhipuChallenge(), nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "chatglm_token=CT-1; chatglm_refresh_token=RFT-1", res.Cookie)
	require.Equal(t, "CT-1", res.ChatGLMToken)
	require.Equal(t, "RFT-1", res.LoginRefreshToken)
	require.Len(t, up.requests, 1)
	require.Equal(t, webZhipuPhoneLoginEndpoint, up.requests[0].URL.Path)
	// tI 签名三件套由方法内部生成并填入请求体（签名算法已取证，黄金用例 TestWebZhipuComputeSign）。
	var reqBody map[string]any
	require.NoError(t, readJSONBody(up.requests[0], &reqBody))
	require.NotEmpty(t, reqBody["timestamp"])
	require.NotEmpty(t, reqBody["xNonce"])
	require.NotEmpty(t, reqBody["sign"])
	require.NotEmpty(t, reqBody["pic_captcha_id"])
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

// E0 取证：zhipu 响应结构未取证，body 业务码（如 code=403）不猜测；登录成功判定 =
// HTTP 2xx + Set-Cookie 含 chatglm_token。即使 body 带疑似错误码，只要带 Cookie 即成功。
func TestWebSMS_ZhipuVerifyWrongCode(t *testing.T) {
	up := &smsUpstream{
		zhipuVerifyStatus: http.StatusOK,
		zhipuVerifyBody:   `{"code":403,"msg":"wrong code"}`,
		zhipuSetCookies:   []string{"chatglm_token=CT-1; Path=/"},
	}
	svc := newSmsTestService(up)

	res, err := svc.VerifySmsCode(context.Background(), PlatformZhipu, "13800000000", "000000", zhipuChallenge(), nil)
	require.NoError(t, err) // body code 被忽略，Cookie 决定成功
	require.NotNil(t, res)
	require.Equal(t, "CT-1", res.ChatGLMToken)
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

func TestWebSMS_KimiVerifyMissingTokenField(t *testing.T) {
	up := &smsUpstream{kimiVerifyStatus: http.StatusOK, kimiVerifyBody: `{"access_token":"AT-1"}`}
	svc := newSmsTestService(up)

	_, err := svc.VerifySmsCode(context.Background(), PlatformKimi, "19900000000", "123456", WebSMSChallenge{}, nil)
	require.Error(t, err)
	require.Equal(t, WebLoginKindLogin, smsErrorKind(t, err))
	require.NotContains(t, err.Error(), "AT-1") // 不暴露凭据值
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
