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
	mu       sync.Mutex
	accounts []*service.Account
	byID     map[int64]*service.Account
	deleted  []int64
	nextID   int64
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
	recoverResults map[int64]service.WebRecoverResult
	refreshErrs    map[int64]error

	smsResult    *service.SMSLoginResult
	smsSendErr   error
	smsVerifyErr error
}

func (s *stubAutoLogin) LoginByEmail(_ context.Context, _ *service.Account) (string, error) {
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

func (s *stubAutoLogin) SendSmsCode(_ context.Context, _, _ string, _ service.WebSMSChallenge, _ *service.Account) (string, error) {
	if s.smsSendErr != nil {
		return "", s.smsSendErr
	}
	return "", nil
}

func (s *stubAutoLogin) VerifySmsCode(_ context.Context, _, _, _ string, _ service.WebSMSChallenge, _ *service.Account) (*service.SMSLoginResult, error) {
	if s.smsVerifyErr != nil {
		return nil, s.smsVerifyErr
	}
	if s.smsResult != nil {
		return s.smsResult, nil
	}
	return &service.SMSLoginResult{}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestAccountHandler(adminSvc *walAdminStub, auto *stubAutoLogin) *AccountHandler {
	h := &AccountHandler{
		adminService:         adminSvc,
		webPlatformAutoLogin: auto,
	}
	return h
}

func doRequest(t *testing.T, h *AccountHandler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	group := r.Group("/api/v1/admin/accounts")
	group.POST("/web-login-password", h.WebLoginPassword)
	group.POST("/web-login-sms", h.WebLoginSMS)

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
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
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

func TestWebLoginPassword_DeepseekSuccess(t *testing.T) {
	acc := &service.Account{
		ID:          1,
		Name:        "ds-acc",
		Platform:    service.PlatformDeepseek,
		Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb, "login_email": "u@e.com", "login_password": "pw"},
		Status:      service.StatusError,
	}
	adminSvc := newWALAdminStub(acc)
	auto := &stubAutoLogin{cookieForEmail: "cookie=ds_session_id=abc"}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform":       service.PlatformDeepseek,
		"login_email":    "u@e.com",
		"login_password": "pw",
		"account_id":     1,
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeEnvelope(t, w)
	require.Equal(t, 0, env.Code)
	var data map[string]any
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, true, data["success"])
	require.Equal(t, "cookie=ds_session_id=abc", data["cookie"])

	// 凭据已回写 cookie 键 + 状态置 active + 清 ErrorMessage。
	updated, err := adminSvc.GetAccount(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, updated.Status)
	require.Equal(t, "cookie=ds_session_id=abc", updated.GetCredential("cookie"))
	require.Equal(t, "u@e.com", updated.GetCredential("login_email"))
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

func TestWebLoginSMS_SendCodeSuccess(t *testing.T) {
	adminSvc := newWALAdminStub()
	auto := &stubAutoLogin{}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":   "send_code",
		"platform": service.PlatformKimi,
		"phone":    "19900000000",
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
	auto := &stubAutoLogin{smsSendErr: errors.New("发码需数美滑块 rid（pic_captcha_id），请在浏览器完成验证后回填挑战值再重试")}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":   "send_code",
		"platform": service.PlatformZhipu,
		"phone":    "13800000000",
	})
	// 失败关闭：非 2xx，且不落任何账号。
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Empty(t, adminSvc.accounts)
	require.Contains(t, w.Body.String(), "挑战值")
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

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":     "login",
		"platform":   service.PlatformZhipu,
		"phone":      "13800000000",
		"account_id": 2,
		"sms_code":   "123456",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeEnvelope(t, w)
	var data map[string]any
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, true, data["success"])
	require.EqualValues(t, 2, data["account_id"])
	require.Equal(t, "chatglm_token=CT-1; chatglm_refresh_token=RFT-1", data["cookie"])
	require.Equal(t, "RFT-1", data["login_refresh_token"])

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

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":   "login",
		"platform": service.PlatformKimi,
		"phone":    "19900000000",
		"sms_code": "123456",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeEnvelope(t, w)
	var data map[string]any
	require.NoError(t, json.Unmarshal(env.Data, &data))
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

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":     "login",
		"platform":   service.PlatformZhipu,
		"phone":      "13800000000",
		"account_id": 3,
		"sms_code":   "123456",
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

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":     "login",
		"platform":   service.PlatformZhipu,
		"phone":      "13800000000",
		"account_id": 4,
		"sms_code":   "123456",
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
