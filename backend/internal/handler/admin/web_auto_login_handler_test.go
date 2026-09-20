package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// walAdminStub 通过嵌入 service.AdminService 仅重写本任务 handler 用到的少数方法；
// 其余接口方法保持 nil 提升（不被调用）。
type walAdminStub struct {
	service.AdminService
	mu         sync.Mutex
	accounts   []*service.Account
	byID       map[int64]*service.Account
	deleted    []int64
	nextID     int64
	createErr  error
	creates    int
	updates    int
	lastCreate *service.CreateAccountInput
}

func newWALAdminStub(accounts ...*service.Account) *walAdminStub {
	byID := make(map[int64]*service.Account, len(accounts))
	var maxID int64
	for _, a := range accounts {
		if a != nil {
			byID[a.ID] = a
			if a.ID > maxID {
				maxID = a.ID
			}
		}
	}
	return &walAdminStub{accounts: accounts, byID: byID, nextID: maxID}
}

func (s *walAdminStub) ListAccounts(_ context.Context, _ int, _ int, platform, _, _, _ string, _ int64, _, _, _ string) ([]service.Account, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]service.Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		if a == nil {
			continue
		}
		if platform != "" && a.Platform != platform {
			continue
		}
		out = append(out, *a)
	}
	return out, int64(len(out)), nil
}

func (s *walAdminStub) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.byID[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, errors.New("account not found")
}

func (s *walAdminStub) GetAccountsByIDs(_ context.Context, ids []int64) ([]*service.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*service.Account, 0, len(ids))
	for _, id := range ids {
		if a, ok := s.byID[id]; ok {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *walAdminStub) UpdateAccount(_ context.Context, id int64, input *service.UpdateAccountInput) (*service.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates++
	a, ok := s.byID[id]
	if !ok {
		return nil, errors.New("account not found")
	}
	if input.Status != "" {
		a.Status = input.Status
	}
	if input.Credentials != nil {
		if a.Credentials == nil {
			a.Credentials = map[string]any{}
		}
		for k, v := range input.Credentials {
			a.Credentials[k] = v
		}
	}
	cp := *a
	return &cp, nil
}

func (s *walAdminStub) CreateAccount(_ context.Context, input *service.CreateAccountInput) (*service.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	s.lastCreate = input
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.nextID++
	creds := map[string]any{}
	for k, v := range input.Credentials {
		creds[k] = v
	}
	a := &service.Account{
		ID:          s.nextID,
		Name:        input.Name,
		Platform:    input.Platform,
		Type:        input.Type,
		Credentials: creds,
		Extra:       input.Extra,
		ProxyID:     input.ProxyID,
		Concurrency: input.Concurrency,
		Status:      service.StatusActive,
	}
	s.byID[a.ID] = a
	s.accounts = append(s.accounts, a)
	cp := *a
	return &cp, nil
}

func (s *walAdminStub) DeleteAccount(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *walAdminStub) ClearAccountError(_ context.Context, id int64) (*service.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.byID[id]; ok {
		a.ErrorMessage = ""
		cp := *a
		return &cp, nil
	}
	return nil, errors.New("account not found")
}

func (s *walAdminStub) SetAccountError(_ context.Context, id int64, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.byID[id]; ok {
		a.ErrorMessage = msg
	}
	return nil
}

// stubAutoLogin 是 webPlatformAutoLoginService 的可配置桩。
type stubAutoLogin struct {
	cookieForEmail string
	emailErr       error
	loginDeviceID  string
	loginCalls     int
	loginAccount   *service.Account
	recoverResults map[int64]service.WebRecoverResult
	refreshErrs    map[int64]error

	smsResult        *service.SMSLoginResult
	smsSendErr       error
	smsVerifyErr     error
	smsSendAccount   *service.Account
	smsVerifyAccount *service.Account
}

func (s *stubAutoLogin) LoginByEmail(_ context.Context, account *service.Account) (string, error) {
	s.loginCalls++
	s.loginAccount = account
	if s.loginDeviceID != "" {
		if account.Credentials == nil {
			account.Credentials = map[string]any{}
		}
		account.Credentials[service.CredKeyLoginDeviceID] = s.loginDeviceID
	}
	return s.cookieForEmail, s.emailErr
}

func (s *stubAutoLogin) RefreshToken(_ context.Context, account *service.Account) error {
	if e, ok := s.refreshErrs[account.ID]; ok {
		return e
	}
	return nil
}

func (s *stubAutoLogin) RecoverAccount(_ context.Context, account *service.Account) service.WebRecoverResult {
	if r, ok := s.recoverResults[account.ID]; ok {
		return r
	}
	return service.WebRecoverResult{Recovered: true}
}

func (s *stubAutoLogin) SendSmsCode(_ context.Context, _, _ string, _ service.WebSMSChallenge, account *service.Account) (string, error) {
	s.smsSendAccount = account
	if s.smsSendErr != nil {
		return "", s.smsSendErr
	}
	return "", nil
}

func (s *stubAutoLogin) VerifySmsCode(_ context.Context, _, _, _ string, _ service.WebSMSChallenge, account *service.Account) (*service.SMSLoginResult, error) {
	s.smsVerifyAccount = account
	if s.smsVerifyErr != nil {
		return nil, s.smsVerifyErr
	}
	if s.smsResult != nil {
		return s.smsResult, nil
	}
	return &service.SMSLoginResult{}, nil
}

type stubCaptchaHelper struct {
	mu            sync.Mutex
	result        service.LocalCaptchaHelperResult
	resultErr     error
	resultCalls   int
	resultStarted chan struct{}
	releaseResult chan struct{}

	// Start 入参捕获（验证 zhipu/kimi 平台的 phone_code 传递）。
	startPlatform  string
	startPhoneCode string
}

func (s *stubCaptchaHelper) Start(_ context.Context, platform, _, _, phoneCode string) (service.LocalCaptchaHelperSession, error) {
	s.mu.Lock()
	s.startPlatform = platform
	s.startPhoneCode = phoneCode
	s.mu.Unlock()
	return service.LocalCaptchaHelperSession{ID: "helper-session"}, nil
}

func (s *stubCaptchaHelper) StartArgs() (platform, phoneCode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startPlatform, s.startPhoneCode
}

func (s *stubCaptchaHelper) Status(context.Context, service.LocalCaptchaHelperSession, string, string, string) (string, error) {
	return "ok", nil
}

func (s *stubCaptchaHelper) Result(context.Context, service.LocalCaptchaHelperSession, string, string, string) (service.LocalCaptchaHelperResult, error) {
	s.mu.Lock()
	s.resultCalls++
	started := s.resultStarted
	release := s.releaseResult
	result, err := s.result, s.resultErr
	s.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return result, err
}

func (s *stubCaptchaHelper) ResultCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resultCalls
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestAccountHandler(adminSvc *walAdminStub, auto *stubAutoLogin) *AccountHandler {
	h := &AccountHandler{
		adminService:           adminSvc,
		webPlatformAutoLogin:   auto,
		webLoginChallengeStore: service.NewWebLoginChallengeSessionStore(),
		webLoginCaptchaHelper: &stubCaptchaHelper{result: service.LocalCaptchaHelperResult{
			Status: "succeeded", Data: map[string]string{"validate": "validate-1"},
		}},
	}
	return h
}

func seedSMSChallenge(t *testing.T, h *AccountHandler, platform, phone, stage string, challenge service.WebSMSChallenge, in service.WebLoginChallengeCreateInput) string {
	t.Helper()
	in.AdminID = 7
	in.Platform = platform
	in.Phone = phone
	in.Stage = stage
	sess, err := h.webLoginChallengeStore.Create(in)
	require.NoError(t, err)
	// 用 claim 模型把会话推进到 status=succeeded（BeginConsume → SetConsumeResult → FinishConsume）。
	claim, err := h.webLoginChallengeStore.BeginConsume(sess.ID, 7, platform, phone)
	require.NoError(t, err)
	require.NoError(t, h.webLoginChallengeStore.SetConsumeResult(sess.ID, claim.Token, challenge))
	require.NoError(t, h.webLoginChallengeStore.FinishConsume(sess.ID, claim.Token, true))
	return sess.ID
}

func seedPendingSMSChallenge(t *testing.T, h *AccountHandler, platform, phone, stage string, in service.WebLoginChallengeCreateInput) string {
	t.Helper()
	in.AdminID = 7
	in.Platform = platform
	in.Phone = phone
	in.Stage = stage
	sess, err := h.webLoginChallengeStore.Create(in)
	require.NoError(t, err)
	return sess.ID
}

func doRequest(t *testing.T, h *AccountHandler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	group := r.Group("/api/v1/admin/accounts")
	group.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
		c.Next()
	})
	group.POST("/web-login-password", h.WebLoginPassword)
	group.POST("/web-login-sms", h.WebLoginSMS)
	group.POST("/web-login-challenge/start", h.WebLoginChallengeStart)
	group.GET("/web-login-challenge/:session_id/status", h.WebLoginChallengeStatus)
	group.POST("/web-login-challenge/:session_id/consume", h.WebLoginChallengeConsume)

	var reqBody *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reqBody = bytes.NewReader(raw)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reqBody)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type respEnvelope struct {
	Code     int               `json:"code"`
	Message  string            `json:"message"`
	Reason   string            `json:"reason"`
	Metadata map[string]string `json:"metadata"`
	Data     json.RawMessage   `json:"data"`
}

func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) respEnvelope {
	t.Helper()
	var env respEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	return env
}

// ---------------------------------------------------------------------------
// WebLoginPassword（deepseek 密码登录）
// ---------------------------------------------------------------------------

func TestWebLoginChallengeStartReturnsContextGapAndOpaqueSession(t *testing.T) {
	h := newTestAccountHandler(newWALAdminStub(&service.Account{ID: 1, Platform: service.PlatformKimi, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}}), &stubAutoLogin{})
	h.SetWebLoginCaptchaHelper(nil)
	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-challenge/start", map[string]any{
		"platform": "kimi", "phone": "13800138000", "stage": "send_code", "account_id": 1,
	})
	require.Equal(t, http.StatusNotImplemented, w.Code)
	env := decodeEnvelope(t, w)
	require.Equal(t, "challenge_session", env.Reason)
	require.Equal(t, "true", env.Metadata["context_gap"])
	require.Equal(t, "context_gap", env.Metadata["status"])
	sessionID := env.Metadata["session_id"]
	ok := sessionID != ""
	require.True(t, ok)
	require.NotEmpty(t, sessionID)
	require.NotContains(t, sessionID, "13800138000")

	status := doRequest(t, h, http.MethodGet, "/api/v1/admin/accounts/web-login-challenge/"+sessionID+"/status", nil)
	require.Equal(t, http.StatusOK, status.Code)

	consume := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-challenge/"+sessionID+"/consume", nil)
	require.Equal(t, http.StatusBadRequest, consume.Code)
}

func TestWebLoginChallengeStartPassesCountryCodeAsZhipuPhoneCode(t *testing.T) {
	// zhipu：helper.Start 必须收到国家码作为 phone_code（空值会导致 helper 返回
	// "GLM challenge requires phone_code" → context_gap 501，外呼从未发出）。
	adminSvc := newWALAdminStub(&service.Account{ID: 1, Platform: service.PlatformZhipu, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}})
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})
	helper := h.webLoginCaptchaHelper.(*stubCaptchaHelper)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-challenge/start", map[string]any{
		"platform": "zhipu", "phone": "13800138000", "stage": "send_code", "account_id": 1,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	platform, phoneCode := helper.StartArgs()
	require.Equal(t, service.PlatformZhipu, platform)
	require.Equal(t, "86", phoneCode)

	// 带国家码前缀的形态同样拆出 "86"。
	w = doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-challenge/start", map[string]any{
		"platform": "zhipu", "phone": "+86 13900139000", "stage": "send_code", "account_id": 1,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, phoneCode = helper.StartArgs()
	require.Equal(t, "86", phoneCode)
}

func TestWebLoginChallengeStartKeepsEmptyPhoneCodeForKimi(t *testing.T) {
	// kimi：helper 不需要 phone_code，保持空串。
	adminSvc := newWALAdminStub(&service.Account{ID: 1, Platform: service.PlatformKimi, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}})
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})
	helper := h.webLoginCaptchaHelper.(*stubCaptchaHelper)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-challenge/start", map[string]any{
		"platform": "kimi", "phone": "13800138000", "stage": "send_code", "account_id": 1,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	platform, phoneCode := helper.StartArgs()
	require.Equal(t, service.PlatformKimi, platform)
	require.Equal(t, "", phoneCode)
}

func TestWebLoginPassword_DeepseekSuccess(t *testing.T) {
	acc := &service.Account{
		ID:          1,
		Name:        "ds-acc",
		Platform:    service.PlatformDeepseek,
		Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb, "login_email": "u@e.com", "login_password": "pw"},
		Status:      service.StatusError,
	}
	adminSvc := newWALAdminStub(acc)
	auto := &stubAutoLogin{cookieForEmail: "cookie=ds_session_id=abc", loginDeviceID: "device-existing"}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform":       service.PlatformDeepseek,
		"login_email":    "u@e.com",
		"login_password": "pw",
		"account_id":     1,
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var data map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &data))
	require.Equal(t, map[string]any{"success": true, "account_id": float64(1)}, data)

	// 凭据已回写 cookie 键 + 状态置 active + 清 ErrorMessage。
	updated, err := adminSvc.GetAccount(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, updated.Status)
	require.Equal(t, "cookie=ds_session_id=abc", updated.GetCredential("cookie"))
	require.Equal(t, "u@e.com", updated.GetCredential("login_email"))
	require.Equal(t, "device-existing", updated.GetCredential(service.CredKeyLoginDeviceID))
}

func TestWebLoginPassword_DeepseekCreatesFromDraft(t *testing.T) {
	adminSvc := newWALAdminStub()
	auto := &stubAutoLogin{cookieForEmail: "ds_session_id=created", loginDeviceID: "device-created"}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform":       service.PlatformDeepseek,
		"login_email":    "new@example.com",
		"login_password": "pw-new",
		"account_draft": gin.H{
			"name":        "deepseek-draft",
			"platform":    service.PlatformDeepseek,
			"type":        service.AccountTypeAPIKey,
			"credentials": gin.H{"access_mode": service.AccountAccessModeWeb, "base_url": "https://chat.deepseek.com", "custom_key": "custom-value"},
			"proxy_id":    77,
			"concurrency": 13,
		},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var data map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &data))
	require.Len(t, data, 2)
	require.Equal(t, true, data["success"])
	require.NotZero(t, data["account_id"])

	id := int64(data["account_id"].(float64))
	created, err := adminSvc.GetAccount(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "deepseek-draft", created.Name)
	require.Equal(t, "ds_session_id=created", created.GetCredential("cookie"))
	require.Equal(t, "new@example.com", created.GetCredential("login_email"))
	require.Equal(t, "pw-new", created.GetCredential("login_password"))
	require.Equal(t, "https://chat.deepseek.com", created.GetCredential("base_url"))
	require.Equal(t, "custom-value", created.GetCredential("custom_key"))
	require.Equal(t, "device-created", created.GetCredential(service.CredKeyLoginDeviceID))
	require.Equal(t, int64(77), *created.ProxyID)
	require.Equal(t, 13, created.Concurrency)
	require.Equal(t, int64(77), *auto.loginAccount.ProxyID)
	require.Equal(t, 13, auto.loginAccount.Concurrency)
	require.Equal(t, "custom-value", auto.loginAccount.GetCredential("custom_key"))
}

// draft.credentials 缺失 access_mode 时，服务端强制兜底写入 web，
// LoginByEmail 收到的临时账号必须是 web access mode。
func TestWebLoginPassword_DraftWithoutAccessModeDefaultsToWeb(t *testing.T) {
	adminSvc := newWALAdminStub()
	auto := &stubAutoLogin{cookieForEmail: "ds_session_id=created"}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform":       service.PlatformDeepseek,
		"login_email":    "new@example.com",
		"login_password": "pw-new",
		"account_draft": gin.H{
			"name":        "deepseek-draft",
			"platform":    service.PlatformDeepseek,
			"type":        service.AccountTypeAPIKey,
			"credentials": gin.H{"base_url": "https://chat.deepseek.com"},
		},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	require.NotNil(t, auto.loginAccount)
	require.True(t, auto.loginAccount.IsWebAccessMode())
	require.Equal(t, service.AccountAccessModeWeb, auto.loginAccount.GetCredential("access_mode"))
}

func TestWebLoginPassword_RejectsExistingAccountPlatformMismatch(t *testing.T) {
	acc := &service.Account{
		ID:          7,
		Platform:    service.PlatformKimi,
		Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb},
	}
	adminSvc := newWALAdminStub(acc)
	auto := &stubAutoLogin{cookieForEmail: "must-not-write"}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform":       service.PlatformDeepseek,
		"login_email":    "u@example.com",
		"login_password": "pw",
		"account_id":     7,
	})

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Equal(t, 0, auto.loginCalls)
	require.Equal(t, 0, adminSvc.updates)
	require.Equal(t, 0, adminSvc.creates)
}

func TestWebLoginPassword_RejectsDraftPlatformMismatch(t *testing.T) {
	adminSvc := newWALAdminStub()
	auto := &stubAutoLogin{cookieForEmail: "must-not-write"}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform":       service.PlatformDeepseek,
		"login_email":    "u@example.com",
		"login_password": "pw",
		"account_draft": gin.H{
			"name":        "mismatched-draft",
			"platform":    service.PlatformKimi,
			"type":        service.AccountTypeAPIKey,
			"credentials": gin.H{"access_mode": service.AccountAccessModeWeb},
		},
	})

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Equal(t, 0, auto.loginCalls)
	require.Equal(t, 0, adminSvc.updates)
	require.Equal(t, 0, adminSvc.creates)
}

func TestWebLoginPassword_RequiresDraftForNewAccount(t *testing.T) {
	adminSvc := newWALAdminStub()
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{cookieForEmail: "cookie"})
	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform":       service.PlatformDeepseek,
		"login_email":    "new@example.com",
		"login_password": "pw-new",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Empty(t, adminSvc.accounts)
}

func TestWebLoginPassword_RejectsMismatchedExistingPlatformBeforeLogin(t *testing.T) {
	adminSvc := newWALAdminStub(&service.Account{
		ID: 1, Platform: service.PlatformZhipu,
		Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb},
	})
	auto := &stubAutoLogin{cookieForEmail: "must-not-be-used"}
	h := newTestAccountHandler(adminSvc, auto)
	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform": service.PlatformDeepseek, "account_id": 1,
		"login_email": "u@example.com", "login_password": "pw",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Empty(t, auto.loginAccount)
	require.Zero(t, adminSvc.creates)
	require.Zero(t, adminSvc.updates)
}

func TestWebLoginPassword_RejectsMismatchedDraftPlatformBeforeLogin(t *testing.T) {
	adminSvc := newWALAdminStub()
	auto := &stubAutoLogin{cookieForEmail: "must-not-be-used"}
	h := newTestAccountHandler(adminSvc, auto)
	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform":    service.PlatformDeepseek,
		"login_email": "u@example.com", "login_password": "pw",
		"account_draft": gin.H{
			"name": "wrong-platform", "platform": service.PlatformZhipu,
			"type":        service.AccountTypeAPIKey,
			"credentials": gin.H{"access_mode": service.AccountAccessModeWeb},
		},
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Empty(t, auto.loginAccount)
	require.Zero(t, adminSvc.creates)
	require.Zero(t, adminSvc.updates)
}

func TestWebLoginPassword_ZhipuKimiRejected(t *testing.T) {
	// zhipu/kimi 官方网页端无密码登录（手机号短信码），后端拒绝密码登录请求。
	for _, platform := range []string{service.PlatformZhipu, service.PlatformKimi} {
		t.Run(platform, func(t *testing.T) {
			adminSvc := newWALAdminStub()
			auto := &stubAutoLogin{}
			h := newTestAccountHandler(adminSvc, auto)

			w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
				"platform":       platform,
				"login_phone":    "13800000000",
				"login_password": "pw",
			})

			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			env := decodeEnvelope(t, w)
			require.NotEqual(t, 0, env.Code)
		})
	}
}

func TestWebLoginPassword_UnsupportedPlatform(t *testing.T) {
	adminSvc := newWALAdminStub()
	auto := &stubAutoLogin{}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform": "openai",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// ---------------------------------------------------------------------------
// WebLoginSMS（zhipu / kimi 手机号+短信码）
// ---------------------------------------------------------------------------

func TestWebLoginChallengeConsumeKeepsChallengeForSMS(t *testing.T) {
	adminSvc := newWALAdminStub()
	auto := &stubAutoLogin{smsResult: &service.SMSLoginResult{AccessToken: "AT-1", RefreshToken: "RT-1", LoginRefreshToken: "RT-1"}}
	h := newTestAccountHandler(adminSvc, auto)
	challengeSessionID := seedPendingSMSChallenge(t, h, service.PlatformKimi, "19900000000", "send_code", service.WebLoginChallengeCreateInput{AccountDraft: &CreateAccountRequest{Name: "kimi-draft", Platform: service.PlatformKimi, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}}})
	sess, err := h.webLoginChallengeStore.Status(challengeSessionID, 7)
	require.NoError(t, err)
	require.NoError(t, h.webLoginChallengeStore.SetHelperSessionID(sess.ID, 7, "helper-session"))

	consume := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-challenge/"+challengeSessionID+"/consume", gin.H{
		"platform": service.PlatformKimi, "phone": "19900000000", "stage": "send_code",
	})
	require.Equal(t, http.StatusOK, consume.Code, consume.Body.String())

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action": "send_code", "platform": service.PlatformKimi, "phone": "19900000000", "challenge_session_id": challengeSessionID,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action": "login", "platform": service.PlatformKimi, "phone": "19900000000", "sms_code": "123456", "challenge_session_id": challengeSessionID,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action": "login", "platform": service.PlatformKimi, "phone": "19900000000", "sms_code": "123456", "challenge_session_id": challengeSessionID,
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "already consumed")
}

func TestWebLoginSMS_SendCodeSuccess(t *testing.T) {
	adminSvc := newWALAdminStub()
	auto := &stubAutoLogin{}
	h := newTestAccountHandler(adminSvc, auto)
	challengeSessionID := seedSMSChallenge(t, h, service.PlatformKimi, "19900000000", "send_code", service.WebSMSChallenge{KimiCaptchaValidate: "validate-1"}, service.WebLoginChallengeCreateInput{AccountDraft: &CreateAccountRequest{Name: "kimi-draft", Platform: service.PlatformKimi, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}}})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":               "send_code",
		"platform":             service.PlatformKimi,
		"phone":                "19900000000",
		"challenge_session_id": challengeSessionID,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeEnvelope(t, w)
	var data map[string]any
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, true, data["success"])
	require.NotContains(t, data, "session_token") // 发码成功不再回传 session_token（E0 取证）
}

func TestWebLoginSMS_SendCodeFailClosed(t *testing.T) {
	adminSvc := newWALAdminStub()
	auto := &stubAutoLogin{smsSendErr: errors.New("unexpected sms send")}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":   "send_code",
		"platform": service.PlatformZhipu,
		"phone":    "13800000000",
	})
	// 失败关闭：缺失 opaque challenge session，且不落任何账号。
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Empty(t, adminSvc.accounts)
	require.Contains(t, w.Body.String(), "ChallengeSessionID")
	require.NotContains(t, w.Body.String(), "回填挑战值")
}

func TestWebLoginSMS_LoginPersistsZhipuCredentials(t *testing.T) {
	acc := &service.Account{
		ID:          2,
		Name:        "zp-acc",
		Platform:    service.PlatformZhipu,
		Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb},
		Status:      service.StatusError,
	}
	adminSvc := newWALAdminStub(acc)
	auto := &stubAutoLogin{smsResult: &service.SMSLoginResult{
		Cookie:            "chatglm_token=CT-1; chatglm_refresh_token=RFT-1",
		ChatGLMToken:      "CT-1",
		LoginRefreshToken: "RFT-1",
	}}
	h := newTestAccountHandler(adminSvc, auto)
	accountID := int64(2)
	challengeSessionID := seedSMSChallenge(t, h, service.PlatformZhipu, "13800000000", "login", service.WebSMSChallenge{
		ZhipuCaptchaRid: "rid-1", ZhipuCaptchaMD5: "md5-1", ZhipuPhoneCode: "86",
	}, service.WebLoginChallengeCreateInput{AccountID: &accountID})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":               "login",
		"platform":             service.PlatformZhipu,
		"phone":                "13800000000",
		"account_id":           2,
		"sms_code":             "123456",
		"challenge_session_id": challengeSessionID,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var data map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &data))
	require.Equal(t, map[string]any{"success": true, "account_id": float64(2)}, data)

	updated, err := adminSvc.GetAccount(context.Background(), 2)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, updated.Status)
	require.Equal(t, "chatglm_token=CT-1; chatglm_refresh_token=RFT-1", updated.GetCredential("cookie"))
	require.Equal(t, "RFT-1", updated.GetCredential(service.CredKeyLoginRefreshToken))
	require.Equal(t, "RFT-1", updated.GetCredential("refresh_token"))
	require.Equal(t, "13800000000", updated.GetCredential("login_phone"))
	require.Equal(t, service.AccountAccessModeWeb, updated.GetCredential("access_mode"))
}

func TestWebLoginSMS_LoginCreatesKimiAccount(t *testing.T) {
	adminSvc := newWALAdminStub()
	auto := &stubAutoLogin{smsResult: &service.SMSLoginResult{
		AccessToken:       "AT-1",
		RefreshToken:      "RT-1",
		LoginRefreshToken: "RT-1",
	}}
	h := newTestAccountHandler(adminSvc, auto)
	proxyID := int64(77)
	draft := &CreateAccountRequest{Name: "kimi-draft", Platform: service.PlatformKimi, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}, ProxyID: &proxyID, Concurrency: 13}
	challengeSessionID := seedSMSChallenge(t, h, service.PlatformKimi, "19900000000", "login", service.WebSMSChallenge{KimiCaptchaValidate: "validate-1"}, service.WebLoginChallengeCreateInput{AccountDraft: draft, ProxyID: &proxyID})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":               "login",
		"platform":             service.PlatformKimi,
		"phone":                "19900000000",
		"sms_code":             "123456",
		"challenge_session_id": challengeSessionID,
		"account_draft": gin.H{
			"name":        "untrusted-client-draft",
			"platform":    service.PlatformKimi,
			"type":        service.AccountTypeAPIKey,
			"credentials": gin.H{"access_mode": service.AccountAccessModeWeb},
		},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var data map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &data))
	require.Equal(t, 2, len(data))
	require.Equal(t, true, data["success"])
	require.NotZero(t, data["account_id"])

	id := int64(data["account_id"].(float64))
	created, err := adminSvc.GetAccount(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, service.PlatformKimi, created.Platform)
	require.Equal(t, service.AccountTypeAPIKey, created.Type)
	require.Equal(t, "AT-1", created.GetCredential("access_token"))
	require.Equal(t, "RT-1", created.GetCredential(service.CredKeyLoginRefreshToken))
	require.Equal(t, "RT-1", created.GetCredential("refresh_token"))
	require.Equal(t, service.AccountAccessModeWeb, created.GetCredential("access_mode"))
	require.Empty(t, created.GetCredential("cookie")) // kimi 不写 cookie 键
	require.NotNil(t, auto.smsVerifyAccount)
	require.NotNil(t, auto.smsVerifyAccount.ProxyID)
	require.Equal(t, int64(77), *auto.smsVerifyAccount.ProxyID)
	require.Equal(t, 13, auto.smsVerifyAccount.Concurrency)
}

// 登录成功但未取得平台必需凭证（如 zhipu 无 cookie）→ 失败关闭，不落库。
func TestWebLoginSMS_LoginFailClosedNoCredential(t *testing.T) {
	acc := &service.Account{
		ID:          3,
		Name:        "zp-acc",
		Platform:    service.PlatformZhipu,
		Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb},
	}
	adminSvc := newWALAdminStub(acc)
	auto := &stubAutoLogin{smsResult: &service.SMSLoginResult{}} // 无 cookie / token
	h := newTestAccountHandler(adminSvc, auto)
	accountID := int64(3)
	challengeSessionID := seedSMSChallenge(t, h, service.PlatformZhipu, "13800000000", "login", service.WebSMSChallenge{
		ZhipuCaptchaRid: "rid-1", ZhipuCaptchaMD5: "md5-1", ZhipuPhoneCode: "86",
	}, service.WebLoginChallengeCreateInput{AccountID: &accountID})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":               "login",
		"platform":             service.PlatformZhipu,
		"phone":                "13800000000",
		"account_id":           3,
		"sms_code":             "123456",
		"challenge_session_id": challengeSessionID,
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	updated, err := adminSvc.GetAccount(context.Background(), 3)
	require.NoError(t, err)
	require.Empty(t, updated.GetCredential("cookie"))
	require.Empty(t, updated.GetCredential(service.CredKeyLoginRefreshToken))
}

func TestWebLoginSMS_RejectsNonWebAccount(t *testing.T) {
	api := &service.Account{ID: 4, Platform: service.PlatformZhipu, Credentials: map[string]any{"api_key": "sk-x"}}
	adminSvc := newWALAdminStub(api)
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})

	// 先建出一个 status=succeeded、绑定到 account_id=4 的 challenge session。
	accountID := int64(4)
	challengeSessionID := seedSMSChallenge(t, h, service.PlatformZhipu, "13800000000", "login", service.WebSMSChallenge{
		ZhipuCaptchaRid: "rid-1", ZhipuCaptchaMD5: "md5-1", ZhipuPhoneCode: "86",
	}, service.WebLoginChallengeCreateInput{AccountID: &accountID})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":               "login",
		"platform":             service.PlatformZhipu,
		"phone":                "13800000000",
		"account_id":           4,
		"sms_code":             "123456",
		"challenge_session_id": challengeSessionID,
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "not a web access-mode account")
}

func TestWebLoginSMS_RejectsUnsupportedPlatformAndAction(t *testing.T) {
	h := newTestAccountHandler(newWALAdminStub(), &stubAutoLogin{})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":   "send_code",
		"platform": service.PlatformDeepseek,
		"phone":    "13800000000",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":   "delete",
		"platform": service.PlatformZhipu,
		"phone":    "13800000000",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}
