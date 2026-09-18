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
}

func newWALAdminStub(accounts ...*service.Account) *walAdminStub {
	byID := make(map[int64]*service.Account, len(accounts))
	for _, a := range accounts {
		if a != nil {
			byID[a.ID] = a
		}
	}
	return &walAdminStub{accounts: accounts, byID: byID}
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
	started        bool
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

func (s *stubAutoLogin) Start(_ context.Context) { s.started = true }
func (s *stubAutoLogin) Stop()                   {}

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
	group.POST("/batch-login", h.BatchLogin)
	group.POST("/batch-test", h.BatchTest)
	group.POST("/batch-delete-banned", h.BatchDeleteBanned)
	group.POST("/batch-status", h.BatchStatus)
	group.GET("/export", h.ExportWebAccounts)

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
// Tests
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
	// zhipu/kimi 官方网页端无密码登录（微信扫码/短信码），后端拒绝密码登录请求。
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

func TestBatchLogin_PerAccountStructure(t *testing.T) {
	dsOK := &service.Account{ID: 1, Platform: service.PlatformDeepseek, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}}
	dsFail := &service.Account{ID: 2, Platform: service.PlatformDeepseek, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}}
	zpFail := &service.Account{ID: 3, Platform: service.PlatformZhipu, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}}
	adminSvc := newWALAdminStub(dsOK, dsFail, zpFail)
	auto := &stubAutoLogin{
		recoverResults: map[int64]service.WebRecoverResult{
			1: {Recovered: true, NeedsSemiAuto: false, Detail: ""},
			2: {Recovered: false, NeedsSemiAuto: false, Detail: "boom"},
		},
		refreshErrs: map[int64]error{3: errors.New("refresh failed")},
	}
	h := newTestAccountHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/batch-login", gin.H{
		"ids": []int64{1, 2, 3},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeEnvelope(t, w)
	var data struct {
		Results []struct {
			ID        int64  `json:"id"`
			Recovered bool   `json:"recovered"`
			NeedsSMS  bool   `json:"needs_sms"`
			Detail    string `json:"detail"`
		} `json:"results"`
		Summary struct {
			Success int `json:"success"`
			Failed  int `json:"failed"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Len(t, data.Results, 3)
	require.Equal(t, 1, data.Summary.Success)
	require.Equal(t, 2, data.Summary.Failed)

	byID := map[int64]struct {
		Recovered bool
		NeedsSMS  bool
	}{}
	for _, r := range data.Results {
		byID[r.ID] = struct {
			Recovered bool
			NeedsSMS  bool
		}{r.Recovered, r.NeedsSMS}
	}
	require.True(t, byID[1].Recovered)
	require.False(t, byID[1].NeedsSMS)
	require.False(t, byID[2].Recovered)
	require.False(t, byID[2].NeedsSMS)
	require.False(t, byID[3].Recovered)
	require.True(t, byID[3].NeedsSMS)
}

func TestBatchDeleteBanned_ListMode(t *testing.T) {
	banned := &service.Account{ID: 1, Name: "b", Platform: service.PlatformDeepseek, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}, Status: service.StatusError, ErrorMessage: "账号已被封禁"}
	normal := &service.Account{ID: 2, Name: "n", Platform: service.PlatformDeepseek, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}}
	other := &service.Account{ID: 3, Name: "o", Platform: service.PlatformOpenAI}
	adminSvc := newWALAdminStub(banned, normal, other)
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/batch-delete-banned", gin.H{"confirm": false})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeEnvelope(t, w)
	var data struct {
		Candidates []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"candidates"`
		Deleted int `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Len(t, data.Candidates, 1)
	require.Equal(t, int64(1), data.Candidates[0].ID)
	require.Equal(t, 0, data.Deleted)
	require.Empty(t, adminSvc.deleted)
}

func TestBatchDeleteBanned_ConfirmDeletes(t *testing.T) {
	banned := &service.Account{ID: 1, Name: "b", Platform: service.PlatformDeepseek, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}, Status: service.StatusError, ErrorMessage: "account banned"}
	normal := &service.Account{ID: 2, Name: "n", Platform: service.PlatformZhipu, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}}
	adminSvc := newWALAdminStub(banned, normal)
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/batch-delete-banned", gin.H{"confirm": true})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeEnvelope(t, w)
	var data struct {
		Deleted int `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, 1, data.Deleted)
	require.Equal(t, []int64{1}, adminSvc.deleted)
}

func TestBatchStatus_ValueValidation(t *testing.T) {
	acc := &service.Account{ID: 1, Platform: service.PlatformDeepseek, Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb}}
	adminSvc := newWALAdminStub(acc)
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})

	// 非法状态值 → 400。
	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/batch-status", gin.H{
		"ids":    []int64{1},
		"status": "weird",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// 合法状态值 → 成功。
	w = doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/batch-status", gin.H{
		"ids":    []int64{1},
		"status": service.StatusDisabled,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	updated, err := adminSvc.GetAccount(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, service.StatusDisabled, updated.Status)
}

func TestExportWebAccounts_NoCredentialsLeak(t *testing.T) {
	acc := &service.Account{
		ID:          1,
		Name:        "ds-acc",
		Platform:    service.PlatformDeepseek,
		Status:      service.StatusActive,
		Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb, "login_email": "u@e.com", "cookie": "cookie=secret", "login_password": "pw"},
	}
	adminSvc := newWALAdminStub(acc)
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})

	w := doRequest(t, h, http.MethodGet, "/api/v1/admin/accounts/export?platform="+service.PlatformDeepseek, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeEnvelope(t, w)
	var data struct {
		Accounts []ExportedWebAccount `json:"accounts"`
		Total    int                  `json:"total"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Len(t, data.Accounts, 1)
	require.Equal(t, "u@e.com", data.Accounts[0].EmailOrPhone)
	require.Equal(t, service.StatusActive, data.Accounts[0].Status)

	// 安全红线：响应体绝不出现凭据值（cookie / password）。
	body := w.Body.String()
	require.NotContains(t, body, "cookie=secret")
	require.NotContains(t, body, "pw")
	require.NotContains(t, body, "\"cookie\"")
	require.NotContains(t, body, "\"login_password\"")
}
