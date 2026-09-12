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

// codeBuddyRouteTransport 把 CodeBuddyOAuthService 发出的请求（固定指向
// copilot.tencent.com）重定向到本地 httptest server，便于断言上游交互。
// 它不修改任何生产代码：CodeBuddyUpstreamBaseURL 仍是 const。
type codeBuddyRouteTransport struct {
	serverURL string
	mu        sync.Mutex
	lastPath  string
	lastQuery string
}

func (t *codeBuddyRouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	// 仅捕获首个请求（PollToken 场景下为 token 请求，login/account 为第二次请求，
	// 不应覆盖）。
	if t.lastPath == "" {
		t.lastPath = req.URL.Path
		t.lastQuery = req.URL.RawQuery
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
	svc := NewCodeBuddyOAuthService()
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

	res, err := svc.GenerateAuthURL(t.Context())
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

	_, err := svc.PollToken(t.Context(), "st-123")
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

	info, err := svc.PollToken(t.Context(), "st-123")
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

	_, err := svc.PollToken(t.Context(), "st-123")
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

	info, err := svc.RefreshToken(t.Context(), "rt-old", "u-old", "e-old", "d-old")
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

	_, err := svc.RefreshToken(t.Context(), "rt-garbage", "u", "e", "d")
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

	_, err := svc.PollToken(t.Context(), rawState)
	require.NoError(t, err)

	// 服务端解码后的 state 必须与原始完全一致，且 path 正确（未被特殊字符污染）。
	require.Equal(t, "/v2/plugin/auth/token", transport.lastPath)
	q, err := url.ParseQuery(transport.lastQuery)
	require.NoError(t, err)
	require.Equal(t, rawState, q.Get("state"),
		"state 经 URL 编码后不得篡改 query（含 &/# 不得被当作分隔符）")
}
