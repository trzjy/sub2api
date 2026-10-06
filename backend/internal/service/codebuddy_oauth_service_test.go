package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// codeBuddyRouteTransport 把 CodeBuddyOAuthService 发出的请求重定向到本地 httptest
// server，便于断言上游交互。它不修改任何生产代码：站点域名由 codebuddy_site.go 的
// 站点表固定，本 transport 只改写到本地，同时记录原始 host 供站点选择断言。
type codeBuddyRouteTransport struct {
	serverURL  string
	mu         sync.Mutex
	lastPath   string
	lastQuery  string
	lastHost   string
	lastOrigin string
}

func (t *codeBuddyRouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	// 仅捕获首个请求（PollToken 场景下为 token 请求，login/account 为第二次请求，
	// 不应覆盖）。
	if t.lastPath == "" {
		t.lastPath = req.URL.Path
		t.lastQuery = req.URL.RawQuery
		t.lastHost = req.URL.Host
		t.lastOrigin = req.Header.Get("Origin")
	}
	t.mu.Unlock()

	var body io.Reader
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(raw))
	}
	target := t.serverURL + req.URL.Path
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	forward, err := http.NewRequest(req.Method, target, body)
	if err != nil {
		return nil, err
	}
	for k, vv := range req.Header {
		for _, v := range vv {
			forward.Header.Add(k, v)
		}
	}
	return http.DefaultClient.Do(forward)
}

func newCodeBuddyTestService(handler http.Handler) (*CodeBuddyOAuthService, *codeBuddyRouteTransport, func()) {
	srv := httptest.NewServer(handler)
	transport := &codeBuddyRouteTransport{serverURL: srv.URL}
	svc := NewCodeBuddyOAuthService(nil, nil)
	svc.httpClient = &http.Client{Transport: transport}
	return svc, transport, srv.Close
}

func TestCodeBuddyOAuthService_GenerateAuthURL(t *testing.T) {
	svc, _, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/plugin/auth/state", r.URL.Path)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"state":"st-123","authUrl":"https://www.codebuddy.cn/auth?state=st-123"}}`))
	}))
	defer closeFn()

	res, err := svc.GenerateAuthURL(t.Context(), CodeBuddySiteCN, nil)
	require.NoError(t, err)
	require.Equal(t, "st-123", res.State)
	require.Equal(t, "https://www.codebuddy.cn/auth?state=st-123", res.AuthURL)
}

func TestCodeBuddyOAuthService_PollToken_Pending(t *testing.T) {
	svc, _, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":11111,"msg":"login ing"}`))
	}))
	defer closeFn()

	_, err := svc.PollToken(t.Context(), "st-123", CodeBuddySiteCN, nil)
	require.ErrorIs(t, err, ErrCodeBuddyLoginPending)
}

func TestCodeBuddyOAuthService_PollToken_Success(t *testing.T) {
	svc, transport, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"accessToken":"at-1","refreshToken":"rt-1","expiresIn":3600,"domain":"d-1"}}`))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"uid":"u-1","enterpriseId":"e-1","nickname":"nick-1"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer closeFn()

	info, err := svc.PollToken(t.Context(), "st-123", CodeBuddySiteCN, nil)
	require.NoError(t, err)
	require.Equal(t, "at-1", info.AccessToken)
	require.Equal(t, "rt-1", info.RefreshToken)
	require.Equal(t, "u-1", info.UID)
	require.Equal(t, "e-1", info.EnterpriseID)
	require.Equal(t, "nick-1", info.Nickname)
	require.Equal(t, "d-1", info.Domain)
	require.Greater(t, info.ExpiresAt, int64(0))
	require.Equal(t, "/v2/plugin/auth/token", transport.lastPath)
}

func TestCodeBuddyOAuthService_PollToken_5xxNotPending(t *testing.T) {
	svc, _, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"upstream down"}`))
	}))
	defer closeFn()

	_, err := svc.PollToken(t.Context(), "st-123", CodeBuddySiteCN, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCodeBuddyLoginPending, "5xx 必须如实上报，不能误判为登录未完成")
}

func TestCodeBuddyOAuthService_RefreshToken_PreservesMissingFields(t *testing.T) {
	svc, _, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		// 响应缺 refreshToken 与 domain：应保留旧值。
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"accessToken":"at-new","expiresIn":3600}}`))
	}))
	defer closeFn()

	info, err := svc.RefreshToken(t.Context(), "rt-old", "u-old", "e-old", "d-old", CodeBuddySiteCN, nil)
	require.NoError(t, err)
	require.Equal(t, "at-new", info.AccessToken)
	require.Equal(t, "rt-old", info.RefreshToken, "上游缺省 refreshToken 时应保留旧值")
	require.Equal(t, "d-old", info.Domain, "上游缺省 domain 时应保留旧值")
	require.Equal(t, "u-old", info.UID)
	require.Equal(t, "e-old", info.EnterpriseID)
}

func TestCodeBuddyOAuthService_RefreshToken_InvalidRefreshToken(t *testing.T) {
	svc, _, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		// 业务成功但无 accessToken：视为 refresh token 失效。
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"expiresIn":3600}}`))
	}))
	defer closeFn()

	_, err := svc.RefreshToken(t.Context(), "rt-garbage", "u", "e", "d", CodeBuddySiteCN, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid_refresh_token",
		"refresh token 失效错误必须携带 invalid_refresh_token 关键词")
}

func TestCodeBuddyOAuthService_PollToken_StateURLSafeEncoding(t *testing.T) {
	// state 含 & 和 # 等特殊字符，构造 URL 时不得篡改 query（注入防护）。
	rawState := "a&b=c#frag?x=1"

	svc, transport, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"accessToken":"at-1","refreshToken":"rt-1","expiresIn":3600}}`))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"uid":"u-1"}}`))
		}
	}))
	defer closeFn()

	_, err := svc.PollToken(t.Context(), rawState, CodeBuddySiteCN, nil)
	require.NoError(t, err)

	// 服务端解码后的 state 必须与原始完全一致，且 path 正确（未被特殊字符污染）。
	require.Equal(t, "/v2/plugin/auth/token", transport.lastPath)
	q, err := url.ParseQuery(transport.lastQuery)
	require.NoError(t, err)
	require.Equal(t, rawState, q.Get("state"),
		"state 经 URL 编码后不得篡改 query（含 &/# 不得被当作分隔符）")
}

// TestCodeBuddyOAuthService_SiteEndpoints 钉住站点 URL 表选择：无 site（存量账号）与
// 显式 cn 必须命中 CN 域，intl 命中 codebuddy.ai；且 Origin/Referer 随站点变化。
// 这是"无 site=cn 行为不变"的 OAuth 路径回归。
func TestCodeBuddyOAuthService_SiteEndpoints(t *testing.T) {
	cases := []struct {
		name       string
		site       string
		wantHost   string
		wantOrigin string
	}{
		{name: "no site key defaults to cn", site: "", wantHost: "copilot.tencent.com", wantOrigin: "https://www.codebuddy.cn"},
		{name: "explicit cn", site: CodeBuddySiteCN, wantHost: "copilot.tencent.com", wantOrigin: "https://www.codebuddy.cn"},
		{name: "unknown site falls back to cn", site: "sg", wantHost: "copilot.tencent.com", wantOrigin: "https://www.codebuddy.cn"},
		{name: "intl", site: CodeBuddySiteIntl, wantHost: "www.codebuddy.ai", wantOrigin: "https://www.codebuddy.ai"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, transport, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "application/json")
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"state":"st-1","authUrl":"https://x/login"}}`))
			}))
			defer closeFn()

			_, err := svc.GenerateAuthURL(t.Context(), tc.site, nil)
			require.NoError(t, err)
			require.Equal(t, tc.wantHost, transport.lastHost, "auth/state 出站 host 必须按站点选表")
			require.Equal(t, tc.wantOrigin, transport.lastOrigin, "Origin 必须按站点选表")
		})
	}
}

// TestCodeBuddyOAuthService_IntlPollAndRefreshEndpoints 验证 poll/refresh 同样按 site
// 选表：intl 的 token/login-account/refresh 全部打到 www.codebuddy.ai。
func TestCodeBuddyOAuthService_IntlPollAndRefreshEndpoints(t *testing.T) {
	t.Run("poll", func(t *testing.T) {
		svc, transport, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "application/json")
			switch r.URL.Path {
			case "/v2/plugin/auth/token":
				_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"accessToken":"at-1","refreshToken":"rt-1","expiresIn":3600}}`))
			case "/v2/plugin/login/account":
				_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"uid":"u-1"}}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer closeFn()

		_, err := svc.PollToken(t.Context(), "st-1", CodeBuddySiteIntl, nil)
		require.NoError(t, err)
		require.Equal(t, "www.codebuddy.ai", transport.lastHost)
		require.Equal(t, "https://www.codebuddy.ai", transport.lastOrigin)
	})

	t.Run("refresh", func(t *testing.T) {
		svc, transport, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"accessToken":"at-new","expiresIn":3600}}`))
		}))
		defer closeFn()

		_, err := svc.RefreshToken(t.Context(), "rt-old", "u", "e", "d", CodeBuddySiteIntl, nil)
		require.NoError(t, err)
		require.Equal(t, "www.codebuddy.ai", transport.lastHost)
		require.Equal(t, "https://www.codebuddy.ai", transport.lastOrigin)
	})
}

// TestCodeBuddyOAuthService_IntlRealCaptureShape 对齐 Phase 0 抓包的真实 intl 响应结构：
// token 含 refreshExpiresIn/scope/sessionState/tokenType 且 accessToken 为 JWT 串；
// login/account 的 uid 为 UUID 且无 enterpriseId；refresh 响应无 domain（保留旧值）。
// 详见 docs/evidence/codebuddy-intl/intl-calibration-report.md。
func TestCodeBuddyOAuthService_IntlRealCaptureShape(t *testing.T) {
	svc, _, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/token":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","requestId":"r-1","data":{
				"accessToken":"eyJ.header.sig","refreshToken":"eyJ.refresh.sig",
				"expiresIn":31535990,"refreshExpiresIn":31535990,
				"tokenType":"Bearer","sessionState":"sess-1","scope":"openid profile offline_access email",
				"domain":"www.codebuddy.ai"}}`))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","requestId":"r-2","data":{
				"uid":"78610417-1162-462f-ba3f-a7aaa06aa4eb","nickname":"user@example.com",
				"uin":"450701714658","type":"personal","pluginEnabled":true}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer closeFn()

	info, err := svc.PollToken(t.Context(), "st-1", CodeBuddySiteIntl, nil)
	require.NoError(t, err)
	require.Equal(t, "eyJ.header.sig", info.AccessToken)
	require.Equal(t, "eyJ.refresh.sig", info.RefreshToken)
	require.Equal(t, int64(31535990), info.ExpiresIn)
	require.Equal(t, "www.codebuddy.ai", info.Domain)
	require.Equal(t, "78610417-1162-462f-ba3f-a7aaa06aa4eb", info.UID)
	require.Equal(t, "user@example.com", info.Nickname)
	require.Empty(t, info.EnterpriseID, "intl 个人账号 login/account 无 enterpriseId")
}

// TestCodeBuddyOAuthService_RefreshAuthRefreshSourceBySite 钉住 Phase 0 D8 的站点差异：
// CN 保留 X-Auth-Refresh-Source=workbuddy（"无 site=cn 行为不变"），intl 不带该头
// （intl 实测非必需，收敛指纹面）。
func TestCodeBuddyOAuthService_RefreshAuthRefreshSourceBySite(t *testing.T) {
	cases := []struct {
		name    string
		site    string
		wantHdr string
	}{
		{name: "cn keeps workbuddy", site: CodeBuddySiteCN, wantHdr: "workbuddy"},
		{name: "no site key keeps cn behavior", site: "", wantHdr: "workbuddy"},
		{name: "intl omits", site: CodeBuddySiteIntl, wantHdr: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotHeader string
			svc, _, closeFn := newCodeBuddyTestService(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Get("X-Auth-Refresh-Source")
				w.Header().Set("content-type", "application/json")
				_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"accessToken":"at-new","expiresIn":3600}}`))
			}))
			defer closeFn()

			_, err := svc.RefreshToken(t.Context(), "rt-old", "u", "e", "d", tc.site, nil)
			require.NoError(t, err)
			require.Equal(t, tc.wantHdr, gotHeader)
		})
	}
}
