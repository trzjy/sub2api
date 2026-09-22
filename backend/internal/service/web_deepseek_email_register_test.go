package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// ---------------------------------------------------------------------------
// 测试依赖桩（deepseek 邮箱注册链，任务卡 T1 §4-1/4-8）
// ---------------------------------------------------------------------------

// regUpstreamStub 按请求路径脚本化返回响应，并记录每次请求的头与体（供 mock 断言
// PoW 头出现、shumei_verification 字段存在、代理透传）。
type regUpstreamStub struct {
	mu        sync.Mutex
	requests  []*http.Request
	bodies    map[string][]string // path suffix → 原始请求体（按调用序）
	powHeader string              // create_guest_challenge 响应内嵌的 challenge 串

	// 脚本化响应（按路径后缀）。
	guestStatus   int // 挑战端点 HTTP 状态（0=200）
	guestHasChall bool
	sendBizCode   int64
	sendHTTP      int
	sendWindow    int64
	registerBiz   int64
	registerHTTP  int
}

func newRegUpstreamStub() *regUpstreamStub {
	return &regUpstreamStub{
		bodies:        map[string][]string{},
		powHeader:     "6de3393aba4cece63e3e6a761752722b05f2cfe531bc1b5c82e01985e93fddd2",
		guestHasChall: true,
	}
}

func (m *regUpstreamStub) record(req *http.Request, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	m.bodies[req.URL.Path] = append(m.bodies[req.URL.Path], string(body))
}

func (m *regUpstreamStub) calls(pathSuffix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for p := range m.bodies {
		if strings.HasSuffix(p, pathSuffix) {
			n += len(m.bodies[p])
		}
	}
	return n
}

func (m *regUpstreamStub) lastBody(pathSuffix string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for p, bodies := range m.bodies {
		if strings.HasSuffix(p, pathSuffix) && len(bodies) > 0 {
			return bodies[len(bodies)-1]
		}
	}
	return ""
}

func (m *regUpstreamStub) lastRequest(pathSuffix string) *http.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.requests) - 1; i >= 0; i-- {
		if strings.HasSuffix(m.requests[i].URL.Path, pathSuffix) {
			return m.requests[i]
		}
	}
	return nil
}

func (m *regUpstreamStub) Do(req *http.Request, proxyURL string, _ int64, _ int) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	m.record(req, body)

	switch {
	case strings.HasSuffix(req.URL.Path, webDeepseekGuestPoWChallengePath):
		status := m.guestStatus
		if status == 0 {
			status = http.StatusOK
		}
		if !m.guestHasChall {
			return newMockResp(status, http.Header{}, `{"code":0,"data":{"biz_code":0,"biz_data":{}}}`), nil
		}
		respBody := fmt.Sprintf(`{"code":0,"data":{"biz_code":0,"biz_data":{"guest_challenge":{"algorithm":"DeepSeekHashV1","challenge":"%s","salt":"salt123","signature":"sig","difficulty":43,"expire_at":1739764288699,"target_path":%s}}}}`,
			m.powHeader, strconv.Quote(reqTargetPath(body)))
		return newMockResp(status, http.Header{}, respBody), nil
	case strings.HasSuffix(req.URL.Path, WebDeepseekEmailCodeSendPath):
		status := m.sendHTTP
		if status == 0 {
			status = http.StatusOK
		}
		return newMockResp(status, http.Header{}, fmt.Sprintf(`{"code":0,"data":{"biz_code":%d,"biz_data":{"send_window_secs":%d}}}`, m.sendBizCode, m.sendWindow)), nil
	case strings.HasSuffix(req.URL.Path, WebDeepseekRegisterPath):
		status := m.registerHTTP
		if status == 0 {
			status = http.StatusOK
		}
		return newMockResp(status, http.Header{}, fmt.Sprintf(`{"code":0,"data":{"biz_code":%d}}`, m.registerBiz)), nil
	}
	return newMockResp(http.StatusOK, http.Header{}, "{}"), nil
}

// reqTargetPath 从挑战请求体里取 target_path（求解头的 target_path 必须与其一致）。
func reqTargetPath(body []byte) string {
	var parsed struct {
		TargetPath string `json:"target_path"`
	}
	_ = json.Unmarshal(body, &parsed)
	return parsed.TargetPath
}

func (m *regUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return m.Do(req, proxyURL, 0, 0)
}

var _ HTTPUpstream = (*regUpstreamStub)(nil)

func newRegSvc(upstream HTTPUpstream) *WebPlatformAutoLoginService {
	return NewWebPlatformAutoLoginService(newAutoLoginStore(), upstream, nil)
}

// ---------------------------------------------------------------------------
// 发码 SendRegisterEmailCode
// ---------------------------------------------------------------------------

func TestWebDeepseekRegisterSendCodeSuccess(t *testing.T) {
	up := newRegUpstreamStub()
	up.sendBizCode = 0
	up.sendWindow = 60
	svc := newRegSvc(up)

	secs, err := svc.SendRegisterEmailCode(context.Background(), "user@example.com", "en", "device-1", "register", "")
	require.NoError(t, err)
	require.Equal(t, int64(60), secs)

	// PoW 挑战先取、发码后调（一次一换）。
	require.Equal(t, 1, up.calls(webDeepseekGuestPoWChallengePath))
	require.Equal(t, 1, up.calls(WebDeepseekEmailCodeSendPath))

	// 请求体断言：shumei_verification 字段必须存在（空串），scenario/locale/device_id 齐。
	sendBody := up.lastBody(WebDeepseekEmailCodeSendPath)
	var sendParsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(sendBody), &sendParsed))
	require.Contains(t, sendParsed, "shumei_verification")
	require.Equal(t, "", sendParsed["shumei_verification"])
	require.Equal(t, "user@example.com", sendParsed["email"])
	require.Equal(t, "en", sendParsed["locale"])
	require.Equal(t, "register", sendParsed["scenario"])
	require.Equal(t, "device-1", sendParsed["device_id"])

	// 头断言：PoW 头 + x-client-* 公共头族。
	sendReq := up.lastRequest(WebDeepseekEmailCodeSendPath)
	require.NotEmpty(t, sendReq.Header.Get("X-DS-Guest-PoW-Response"))
	require.Equal(t, "web", sendReq.Header.Get("x-client-platform"))
	require.Equal(t, "1.1", sendReq.Header.Get("x-client-version"))
	require.Equal(t, "en", sendReq.Header.Get("x-client-locale"))
	require.Equal(t, "device-1", sendReq.Header.Get("x-device-id"))
	require.Equal(t, "https://chat.deepseek.com", sendReq.Header.Get("Origin"))
	require.Equal(t, webDeepseekLoginUA, sendReq.Header.Get("User-Agent"))
}

func TestWebDeepseekRegisterSendCodeCaptchaBizCode(t *testing.T) {
	up := newRegUpstreamStub()
	up.sendBizCode = 2
	svc := newRegSvc(up)

	_, err := svc.SendRegisterEmailCode(context.Background(), "user@example.com", "en", "device-1", "register", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "人机验证")
}

func TestWebDeepseekRegisterSendCodeDomainNotSupported(t *testing.T) {
	up := newRegUpstreamStub()
	up.sendBizCode = 9
	svc := newRegSvc(up)

	_, err := svc.SendRegisterEmailCode(context.Background(), "user@example.com", "en", "device-1", "register", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "邮箱域名")
}

func TestWebDeepseekRegisterSendCodeMissingShumeiFieldShape(t *testing.T) {
	// mock 断言请求体含 shumei_verification 字段（方案 §2 取证 4：缺失上游 422）。
	up := newRegUpstreamStub()
	svc := newRegSvc(up)

	_, err := svc.SendRegisterEmailCode(context.Background(), "user@example.com", "en", "device-1", "register", "")
	require.NoError(t, err)
	sendBody := up.lastBody(WebDeepseekEmailCodeSendPath)
	require.Contains(t, sendBody, `"shumei_verification"`)
}

// ---------------------------------------------------------------------------
// 注册 RegisterByEmail
// ---------------------------------------------------------------------------

func TestWebDeepseekRegisterEmailSuccess(t *testing.T) {
	up := newRegUpstreamStub()
	up.registerBiz = 0
	svc := newRegSvc(up)

	err := svc.RegisterByEmail(context.Background(), "user@example.com", "123456", "Passw0rd", "HK", "device-1", "")
	require.NoError(t, err)
	require.Equal(t, 1, up.calls(webDeepseekGuestPoWChallengePath))
	require.Equal(t, 1, up.calls(WebDeepseekRegisterPath))

	regBody := up.lastBody(WebDeepseekRegisterPath)
	var regParsed struct {
		Locale  string `json:"locale"`
		Region  string `json:"region"`
		Payload struct {
			Email                 string `json:"email"`
			EmailVerificationCode string `json:"email_verification_code"`
			Password              string `json:"password"`
		} `json:"payload"`
		DeviceID string `json:"device_id"`
		OS       string `json:"os"`
	}
	require.NoError(t, json.Unmarshal([]byte(regBody), &regParsed))
	require.Equal(t, "en", regParsed.Locale)
	require.Equal(t, "HK", regParsed.Region)
	require.Equal(t, "user@example.com", regParsed.Payload.Email)
	require.Equal(t, "123456", regParsed.Payload.EmailVerificationCode)
	require.Equal(t, "Passw0rd", regParsed.Payload.Password)
	require.Equal(t, "device-1", regParsed.DeviceID)
	require.Equal(t, "web", regParsed.OS)

	regReq := up.lastRequest(WebDeepseekRegisterPath)
	require.NotEmpty(t, regReq.Header.Get("X-DS-Guest-PoW-Response"))
}

func TestWebDeepseekRegisterEmailBizCodeMapping(t *testing.T) {
	cases := []struct {
		code int64
		want string
	}{
		{1, "EMAIL_EXISTS"},
		{4, "INVALID_PASSWORD"},
		{5, "EMAIL_VERIFY_TOO_MANY_ATTEMPTS"},
		{6, "REGISTER_FROM_MAINLAND"},
		{7, "EMAIL_EXPIRED"},
		{8, "EMAIL_PASSCODE_FAILED"},
		{9, "EMAIL_DOMAIN_NOT_SUPPORTED"},
	}
	for _, tc := range cases {
		t.Run(strconv.FormatInt(tc.code, 10), func(t *testing.T) {
			up := newRegUpstreamStub()
			up.registerBiz = tc.code
			svc := newRegSvc(up)
			err := svc.RegisterByEmail(context.Background(), "user@example.com", "123456", "Passw0rd", "HK", "device-1", "")
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
			if tc.code == 6 {
				require.Contains(t, err.Error(), "非大陆")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PoW（任务卡 §4-8）
// ---------------------------------------------------------------------------

func TestWebDeepseekRegisterPoWHeaderPresentOnSendAndRegister(t *testing.T) {
	up := newRegUpstreamStub()
	svc := newRegSvc(up)

	_, err := svc.SendRegisterEmailCode(context.Background(), "user@example.com", "en", "device-1", "register", "")
	require.NoError(t, err)
	err = svc.RegisterByEmail(context.Background(), "user@example.com", "123456", "Passw0rd", "HK", "device-1", "")
	require.NoError(t, err)

	// 两次业务请求都带求解头；挑战端点被调两次（发码一次、注册一次，一次一换）。
	require.Equal(t, 2, up.calls(webDeepseekGuestPoWChallengePath))
	for _, path := range []string{WebDeepseekEmailCodeSendPath, WebDeepseekRegisterPath} {
		req := up.lastRequest(path)
		require.NotEmpty(t, req.Header.Get("X-DS-Guest-PoW-Response"), path)
	}
}

func TestWebDeepseekRegisterPoWChallengeMissingFailClosed(t *testing.T) {
	up := newRegUpstreamStub()
	up.guestHasChall = false
	svc := newRegSvc(up)

	_, err := svc.SendRegisterEmailCode(context.Background(), "user@example.com", "en", "device-1", "register", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "挑战缺失")
	// 失败关闭：发码上游未被调用。
	require.Equal(t, 0, up.calls(WebDeepseekEmailCodeSendPath))

	err = svc.RegisterByEmail(context.Background(), "user@example.com", "123456", "Passw0rd", "HK", "device-1", "")
	require.Error(t, err)
	require.Equal(t, 0, up.calls(WebDeepseekRegisterPath))
}

func TestWebDeepseekRegisterPoWChallengeWAFStatusFailClosed(t *testing.T) {
	up := newRegUpstreamStub()
	up.guestStatus = http.StatusForbidden
	svc := newRegSvc(up)

	_, err := svc.SendRegisterEmailCode(context.Background(), "user@example.com", "en", "device-1", "register", "")
	require.Error(t, err)
	kind, _, ok := WebLoginErrorKind(err)
	require.True(t, ok)
	require.Equal(t, WebLoginKindWAF, kind)
}

// ---------------------------------------------------------------------------
// 代理与白名单（外审 F3 service 侧口径：显式 proxyURL 参数透传）
// ---------------------------------------------------------------------------

type regProxyRecorder struct {
	HTTPUpstream
	proxyURLs []string
	mu        sync.Mutex
}

func (r *regProxyRecorder) Do(req *http.Request, proxyURL string, id int64, conc int) (*http.Response, error) {
	r.mu.Lock()
	r.proxyURLs = append(r.proxyURLs, proxyURL)
	r.mu.Unlock()
	return r.HTTPUpstream.Do(req, proxyURL, id, conc)
}

func TestWebDeepseekRegisterProxyURLPassedThrough(t *testing.T) {
	up := newRegUpstreamStub()
	rec := &regProxyRecorder{HTTPUpstream: up}
	svc := newRegSvc(rec)

	_, err := svc.SendRegisterEmailCode(context.Background(), "user@example.com", "en", "device-1", "register", "http://proxy:8080")
	require.NoError(t, err)
	err = svc.RegisterByEmail(context.Background(), "user@example.com", "123456", "Passw0rd", "HK", "device-1", "http://proxy:8080")
	require.NoError(t, err)

	require.NotEmpty(t, rec.proxyURLs)
	for _, u := range rec.proxyURLs {
		require.Equal(t, "http://proxy:8080", u)
	}
}
