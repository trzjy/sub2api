package admin

// DeepSeek 邮箱验证码注册 handler 定向测试（任务卡 deepseek-email-register-t1
// §4-2/3/4/5/6/7/9）：注册成功链落库凭据五键齐、幂等重放上游仅调一次、幂等闭包内
// 显式去重 409 不落库、登录/CreateAccount 失败关闭 + F4 文案、代理显式解析不回退、
// 弱密码预校验零上游调用。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 测试桩
// ---------------------------------------------------------------------------

// registerAdminStub 覆盖 WebRegister 链所需的 AdminService 方法：GetProxy（F3 显式
// 代理解析）、ListAccountsForSchedulerScoreFilter（去重扫描）、CreateAccount（落库）。
type registerAdminStub struct {
	service.AdminService

	mu            sync.Mutex
	accounts      []service.Account
	proxies       map[int64]*service.Proxy
	proxyMisses   []int64
	createCalls   int
	lastCreate    *service.CreateAccountInput
	createErr     error
	createErrOnce bool
}

func newRegisterAdminStub(proxies map[int64]*service.Proxy) *registerAdminStub {
	return &registerAdminStub{proxies: proxies}
}

func (s *registerAdminStub) GetProxy(_ context.Context, id int64) (*service.Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proxyMisses = append(s.proxyMisses, id)
	if p, ok := s.proxies[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, errors.New("proxy not found")
}

func (s *registerAdminStub) ListAccountsForSchedulerScoreFilter(_ context.Context, _, _, _, _ string, _ int64, _ string) ([]service.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]service.Account, len(s.accounts))
	copy(out, s.accounts)
	return out, nil
}

func (s *registerAdminStub) CreateAccount(_ context.Context, input *service.CreateAccountInput) (*service.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	s.lastCreate = input
	if s.createErr != nil {
		err := s.createErr
		if s.createErrOnce {
			s.createErr = nil
			s.createErrOnce = false
		}
		return nil, err
	}
	account := service.Account{
		ID:          int64(len(s.accounts) + 1),
		Name:        input.Name,
		Platform:    input.Platform,
		Type:        input.Type,
		Status:      service.StatusActive,
		Credentials: input.Credentials,
		ProxyID:     input.ProxyID,
	}
	s.accounts = append(s.accounts, account)
	return &account, nil
}

// registerAutoLoginStub 同时实现 webPlatformAutoLoginService（LoginByEmail 等，供
// 既有字段断言）与 webDeepseekRegisterService（发码/注册）。
type registerAutoLoginStub struct {
	mu sync.Mutex

	sendCalls      int
	sendProxyURLs  []string
	sendErr        error
	registerCalls  int
	registerCalls_ int // 供幂等断言直接读取（registerCalls 同值）
	registerProxy  string
	registerErr    error

	loginCalls      int
	loginProxyURL   string
	loginDeviceID   string
	loginCookie     string
	loginErr        error
	seedExistingDed bool
}

func (s *registerAutoLoginStub) SendRegisterEmailCode(_ context.Context, _, _, _, _, proxyURL string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendCalls++
	s.sendProxyURLs = append(s.sendProxyURLs, proxyURL)
	if s.sendErr != nil {
		return 0, s.sendErr
	}
	return 60, nil
}

func (s *registerAutoLoginStub) RegisterByEmail(_ context.Context, _, _, _, _, deviceID, proxyURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registerCalls++
	s.registerCalls_ = s.registerCalls
	s.registerProxy = proxyURL
	s.registerErr = nil
	// 记录注册用 deviceID（登录应复用同一 device_id）。
	s.loginDeviceID = deviceID
	return nil
}

func (s *registerAutoLoginStub) LoginByEmail(_ context.Context, account *service.Account) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginCalls++
	s.loginDeviceID = account.GetCredential(service.CredKeyLoginDeviceID)
	if account.Proxy != nil {
		s.loginProxyURL = account.Proxy.URL()
	}
	if s.loginErr != nil {
		return "", s.loginErr
	}
	return s.loginCookie, nil
}

func (s *registerAutoLoginStub) RecoverAccount(_ context.Context, _ *service.Account) service.WebRecoverResult {
	return service.WebRecoverResult{Recovered: true}
}

func (s *registerAutoLoginStub) SendSmsCode(_ context.Context, _, _ string, _ service.WebSMSChallenge, _ *service.Account) (string, error) {
	return "", nil
}

func (s *registerAutoLoginStub) VerifySmsCode(_ context.Context, _, _, _ string, _ service.WebSMSChallenge, _ *service.Account) (*service.SMSLoginResult, error) {
	return &service.SMSLoginResult{}, nil
}

// ---------------------------------------------------------------------------
// 路由装配
// ---------------------------------------------------------------------------

type registerTestHarness struct {
	router     *gin.Engine
	admin      *registerAdminStub
	auto       *registerAutoLoginStub
	sendCalls  func() int
	regCalls   func() int
	loginCalls func() int
}

func newRegisterTestHarness(t *testing.T, admin *registerAdminStub, auto *registerAutoLoginStub) *registerTestHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// 幂等协调器（闭包内执行 + 重放语义依赖它）。
	repo := newMemoryIdempotencyRepoStub()
	cfg := service.DefaultIdempotencyConfig()
	cfg.ProcessingTimeout = 2 * time.Second
	service.SetDefaultIdempotencyCoordinator(service.NewIdempotencyCoordinator(repo, cfg))
	t.Cleanup(func() { service.SetDefaultIdempotencyCoordinator(nil) })

	h := &AccountHandler{adminService: admin, webPlatformAutoLogin: auto}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7})
		c.Next()
	})
	group := router.Group("/api/v1/admin/accounts")
	group.POST("/web-register-email-code", h.WebRegisterEmailCode)
	group.POST("/web-register", h.WebRegister)
	return &registerTestHarness{
		router:     router,
		admin:      admin,
		auto:       auto,
		sendCalls:  func() int { auto.mu.Lock(); defer auto.mu.Unlock(); return auto.sendCalls },
		regCalls:   func() int { auto.mu.Lock(); defer auto.mu.Unlock(); return auto.registerCalls },
		loginCalls: func() int { auto.mu.Lock(); defer auto.mu.Unlock(); return auto.loginCalls },
	}
}

func (th *registerTestHarness) post(t *testing.T, path string, body any, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytesReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	rec := httptest.NewRecorder()
	th.router.ServeHTTP(rec, req)
	return rec
}

func bytesReader(b []byte) *bytesReaderImpl { return &bytesReaderImpl{data: b} }

type bytesReaderImpl struct {
	data []byte
	off  int
}

func (r *bytesReaderImpl) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, errEOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

var errEOF = errors.New("EOF")

func validDraft() map[string]any {
	proxyID := int64(0)
	_ = proxyID
	return map[string]any{
		"name":     "ds-register",
		"platform": service.PlatformDeepseek,
		"type":     service.AccountTypeAPIKey,
		"credentials": map[string]any{
			"base_url": "https://chat.deepseek.com",
		},
	}
}

func validRegisterBody() map[string]any {
	return map[string]any{
		"platform":                service.PlatformDeepseek,
		"email":                   "user@example.com",
		"email_verification_code": "123456",
		"password":                "Passw0rd",
		"account_draft":           validDraft(),
	}
}

// ---------------------------------------------------------------------------
// 测试
// ---------------------------------------------------------------------------

// §4-2 注册成功链：register → login（Set-Cookie）→ 去重未命中 → CreateAccount 落库，
// 凭据五键齐（cookie/login_email/login_password/login_device_id/access_mode=web）。
func TestWebRegisterSuccessCreatesAccountWithFiveCredentialKeys(t *testing.T) {
	admin := newRegisterAdminStub(nil)
	auto := &registerAutoLoginStub{loginCookie: "ds_session_id=fresh-session"}
	th := newRegisterTestHarness(t, admin, auto)

	rec := th.post(t, "/api/v1/admin/accounts/web-register", validRegisterBody(), "reg-key-1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, th.regCalls())
	require.Equal(t, 1, th.loginCalls())

	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	var data map[string]any
	require.NoError(t, json.Unmarshal(envelope.Data, &data))
	require.Equal(t, true, data["success"])
	require.NotZero(t, data["account_id"])

	admin.mu.Lock()
	defer admin.mu.Unlock()
	require.Equal(t, 1, admin.createCalls)
	creds := admin.lastCreate.Credentials
	require.Equal(t, "ds_session_id=fresh-session", creds["cookie"])
	require.Equal(t, "user@example.com", creds[service.CredKeyLoginEmail])
	require.Equal(t, "Passw0rd", creds[service.CredKeyLoginPassword])
	require.NotEmpty(t, creds[service.CredKeyLoginDeviceID])
	require.Equal(t, service.AccountAccessModeWeb, creds["access_mode"])
	require.True(t, admin.lastCreate.FromWebLogin)
	require.Equal(t, service.PlatformDeepseek, admin.lastCreate.Platform)
}

// §4-3 幂等（外审 F1）：同 Idempotency-Key 二次请求 → 上游 register 仅被调一次，
// 响应为重放。
func TestWebRegisterIdempotentReplayCallsUpstreamOnce(t *testing.T) {
	admin := newRegisterAdminStub(nil)
	auto := &registerAutoLoginStub{loginCookie: "ds_session_id=replay"}
	th := newRegisterTestHarness(t, admin, auto)

	rec1 := th.post(t, "/api/v1/admin/accounts/web-register", validRegisterBody(), "reg-replay-1")
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	require.Equal(t, 1, th.regCalls())
	require.Equal(t, 1, admin.createCalls)

	rec2 := th.post(t, "/api/v1/admin/accounts/web-register", validRegisterBody(), "reg-replay-1")
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	require.Equal(t, "true", rec2.Header().Get("X-Idempotency-Replayed"))
	require.Equal(t, 1, th.regCalls(), "重放不得再次请求上游 register")
	require.Equal(t, 1, admin.createCalls, "重放不得再次落库")
}

// §4-4 去重（外审 F2）：mock 既有账号持相同 Cookie → 409 web_credential_duplicate
// 且不落库（去重在幂等闭包内显式执行）。
func TestWebRegisterDuplicateLoginSessionReturns409WithoutCreate(t *testing.T) {
	admin := newRegisterAdminStub(nil)
	admin.accounts = []service.Account{{
		Name: "既有账号",
		Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"access_mode": service.AccountAccessModeWeb,
			"cookie":      "ds_session_id=duplicated-session",
		},
	}}
	auto := &registerAutoLoginStub{loginCookie: "ds_session_id=duplicated-session"}
	th := newRegisterTestHarness(t, admin, auto)

	rec := th.post(t, "/api/v1/admin/accounts/web-register", validRegisterBody(), "reg-dup-1")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "web_credential_duplicate")
	require.Contains(t, rec.Body.String(), "既有账号")
	require.Equal(t, 1, th.regCalls(), "去重发生在注册成功之后，上游 register 已被调用一次")
	require.Equal(t, 0, admin.createCalls, "去重命中不得落库")
}

// §4-5 登录失败关闭：login 失败 → 不落库 + F4 文案。
func TestWebRegisterLoginFailClosedReturnsPostSuccessMessage(t *testing.T) {
	admin := newRegisterAdminStub(nil)
	auto := &registerAutoLoginStub{loginErr: errors.New("deepseek web 登录返回 HTTP 500")}
	th := newRegisterTestHarness(t, admin, auto)

	rec := th.post(t, "/api/v1/admin/accounts/web-register", validRegisterBody(), "reg-login-fail")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "账号已在 DeepSeek 注册成功")
	require.Contains(t, rec.Body.String(), "密码登录")
	require.Contains(t, rec.Body.String(), "HTTP 500")
	require.Equal(t, 0, admin.createCalls, "登录失败不得落库")
}

// §4-6 CreateAccount 失败（凭据校验失败注入）→ 不落库 + F4 文案。
func TestWebRegisterCreateAccountFailClosedReturnsPostSuccessMessage(t *testing.T) {
	admin := newRegisterAdminStub(nil)
	admin.createErr = errors.New("凭据校验失败：group 不存在")
	admin.createErrOnce = true
	auto := &registerAutoLoginStub{loginCookie: "ds_session_id=ok"}
	th := newRegisterTestHarness(t, admin, auto)

	rec := th.post(t, "/api/v1/admin/accounts/web-register", validRegisterBody(), "reg-create-fail")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "账号已在 DeepSeek 注册成功")
	require.Contains(t, rec.Body.String(), "凭据校验失败")
	require.Equal(t, 0, len(admin.accounts), "CreateAccount 失败不得落库")
}

// §4-7 代理（外审 F3）：proxy_id 有值 → 显式查 Proxy 对象并用其 URL 出站（注册与
// 紧随的登录同 URL）；查不到 → 400 且不触上游；为空 → 默认出站（proxyURL=""）。
func TestWebRegisterProxyResolution(t *testing.T) {
	t.Run("proxy_found_uses_proxy_url", func(t *testing.T) {
		admin := newRegisterAdminStub(map[int64]*service.Proxy{
			9: {ID: 9, Protocol: "http", Host: "hk.proxy", Port: 8080},
		})
		auto := &registerAutoLoginStub{loginCookie: "ds_session_id=proxy"}
		th := newRegisterTestHarness(t, admin, auto)

		body := validRegisterBody()
		draft := body["account_draft"].(map[string]any)
		proxyID := int64(9)
		draft["proxy_id"] = proxyID

		rec := th.post(t, "/api/v1/admin/accounts/web-register", body, "reg-proxy-1")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		admin.mu.Lock()
		require.Equal(t, []int64{9}, admin.proxyMisses, "proxy_id 非空必须显式查 Proxy 对象")
		admin.mu.Unlock()
		require.Equal(t, "http://hk.proxy:8080", auto.registerProxy)
		require.Equal(t, "http://hk.proxy:8080", auto.loginProxyURL, "紧随的登录必须复用同一代理")

		admin.mu.Lock()
		require.NotNil(t, admin.lastCreate.ProxyID)
		require.Equal(t, int64(9), *admin.lastCreate.ProxyID)
		admin.mu.Unlock()
	})

	t.Run("proxy_missing_returns_400_without_upstream", func(t *testing.T) {
		admin := newRegisterAdminStub(nil)
		auto := &registerAutoLoginStub{}
		th := newRegisterTestHarness(t, admin, auto)

		body := validRegisterBody()
		draft := body["account_draft"].(map[string]any)
		proxyID := int64(42)
		draft["proxy_id"] = proxyID

		rec := th.post(t, "/api/v1/admin/accounts/web-register", body, "reg-proxy-404")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "代理不存在")
		require.Equal(t, 0, th.regCalls(), "代理解析失败不得触上游")
		require.Equal(t, 0, admin.createCalls)
	})

	t.Run("proxy_empty_uses_default_egress", func(t *testing.T) {
		admin := newRegisterAdminStub(nil)
		auto := &registerAutoLoginStub{loginCookie: "ds_session_id=default"}
		th := newRegisterTestHarness(t, admin, auto)

		rec := th.post(t, "/api/v1/admin/accounts/web-register", validRegisterBody(), "reg-proxy-empty")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		admin.mu.Lock()
		require.Empty(t, admin.proxyMisses, "proxy_id 为空不得发起 GetProxy 调用")
		admin.mu.Unlock()
		require.Equal(t, "", auto.registerProxy, "默认出站 proxyURL 必须为空串")
		require.Equal(t, "", auto.loginProxyURL)
	})
}

// §4-9 密码强度/格式预校验：弱密码 → 400 且上游零调用（预校验前移，方案 §1.2-4）。
func TestWebRegisterWeakPasswordRejectedBeforeUpstream(t *testing.T) {
	admin := newRegisterAdminStub(nil)
	auto := &registerAutoLoginStub{}
	th := newRegisterTestHarness(t, admin, auto)

	for _, pw := range []string{"short1", "alllettersonly", "12345678"} {
		body := validRegisterBody()
		body["password"] = pw
		rec := th.post(t, "/api/v1/admin/accounts/web-register", body, "reg-weak-"+pw)
		require.Equal(t, http.StatusBadRequest, rec.Code, "密码 %q 应被预校验拒绝", pw)
	}
	require.Equal(t, 0, th.regCalls(), "预校验失败不得触上游")
	require.Equal(t, 0, admin.createCalls)
}

// 发码端点：成功返回 send_window_secs + device_id；非 deepseek 平台 400。
func TestWebRegisterEmailCodeSendReturnsWindowAndDeviceID(t *testing.T) {
	admin := newRegisterAdminStub(nil)
	auto := &registerAutoLoginStub{}
	th := newRegisterTestHarness(t, admin, auto)

	rec := th.post(t, "/api/v1/admin/accounts/web-register-email-code", map[string]any{
		"platform": service.PlatformDeepseek,
		"email":    "user@example.com",
	}, "reg-send-1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	var data map[string]any
	require.NoError(t, json.Unmarshal(envelope.Data, &data))
	require.Equal(t, true, data["success"])
	require.Equal(t, float64(60), data["send_window_secs"])
	require.NotEmpty(t, data["device_id"])
	require.Equal(t, 1, th.sendCalls())

	// 非 deepseek 平台 → 400。
	rec = th.post(t, "/api/v1/admin/accounts/web-register-email-code", map[string]any{
		"platform": "zhipu", "email": "user@example.com",
	}, "reg-send-2")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, 1, th.sendCalls())
}

var _ = fmt.Sprintf // 保持 fmt 引用（代理错误文案断言使用）
