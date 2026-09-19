package admin

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 本文件覆盖「按账号级 access_mode 隔离」的反例：平台名相同但 access_mode != web
// 的普通 API 账号，不得进入自动登录。
//
// 复用的测试替身（walAdminStub / stubAutoLogin / doRequest / newTestAccountHandler /
// decodeEnvelope）定义在 web_auto_login_handler_test.go。
// ---------------------------------------------------------------------------

// isolationAutoLogin 记录自动登录服务被实际调用的账号 id，用于证明非 web 账号
// 未发起登录/刷新。
type isolationAutoLogin struct {
	mu           sync.Mutex
	recoverCalls []int64
	refreshCalls []int64
}

var _ webPlatformAutoLoginService = (*isolationAutoLogin)(nil)

func (s *isolationAutoLogin) LoginByEmail(_ context.Context, _ *service.Account) (string, error) {
	return "", errors.New("LoginByEmail must not be called in this isolation test")
}

func (s *isolationAutoLogin) RefreshToken(_ context.Context, account *service.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCalls = append(s.refreshCalls, account.ID)
	return nil
}

func (s *isolationAutoLogin) RecoverAccount(_ context.Context, account *service.Account) service.WebRecoverResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoverCalls = append(s.recoverCalls, account.ID)
	return service.WebRecoverResult{Recovered: true}
}

func (s *isolationAutoLogin) SendSmsCode(_ context.Context, _, _ string, _ service.WebSMSChallenge, _ *service.Account) (string, error) {
	return "", errors.New("SendSmsCode must not be called in this isolation test")
}

func (s *isolationAutoLogin) VerifySmsCode(_ context.Context, _, _, _ string, _ service.WebSMSChallenge, _ *service.Account) (*service.SMSLoginResult, error) {
	return nil, errors.New("VerifySmsCode must not be called in this isolation test")
}

func (s *isolationAutoLogin) Start(_ context.Context) {}
func (s *isolationAutoLogin) Stop()                   {}

// newIsolationHandler 与 newTestAccountHandler 等价，但接受任意
// webPlatformAutoLoginService 实现（便于注入 isolationAutoLogin 记录调用）。
func newIsolationHandler(adminSvc *walAdminStub, auto webPlatformAutoLoginService) *AccountHandler {
	return &AccountHandler{
		adminService:         adminSvc,
		webPlatformAutoLogin: auto,
	}
}

// apiAccount 构造一个「官方平台 + 普通 API 凭据」的账号：platform 与 web 账号
// 相同，但 credentials 无 access_mode（GetAccessMode 回退 "api"）。
func apiAccount(id int64, platform string) *service.Account {
	return &service.Account{
		ID:          id,
		Name:        "api-" + platform,
		Platform:    platform,
		Credentials: map[string]any{"api_key": "sk-isolation-test"},
	}
}

// webAccount 构造一个「官方平台 + access_mode=web」的网页接入账号。
func webAccount(id int64, platform string) *service.Account {
	return &service.Account{
		ID:          id,
		Name:        "web-" + platform,
		Platform:    platform,
		Credentials: map[string]any{"access_mode": service.AccountAccessModeWeb},
	}
}

// TestAccessModeIsolation_WebLoginPasswordSingleAccount：单账号入口在取到完整账号
// 对象后按 access_mode 隔离，拒绝把普通 API 账号当 Web 账号登录。
func TestAccessModeIsolation_WebLoginPasswordSingleAccount(t *testing.T) {
	apiZhipu := apiAccount(41, service.PlatformZhipu)
	adminSvc := newWALAdminStub(apiZhipu)
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-password", gin.H{
		"platform":    service.PlatformZhipu,
		"login_phone": "13800000000",
		"account_id":  41,
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "not a web access-mode account")
}

// TestAccessModeIsolation_WebLoginSMSSingleAccount：短信登录单账号入口同样按
// access_mode 隔离，拒绝把普通 API 账号当 Web 账号登录。
func TestAccessModeIsolation_WebLoginSMSSingleAccount(t *testing.T) {
	apiZhipu := apiAccount(42, service.PlatformZhipu)
	adminSvc := newWALAdminStub(apiZhipu)
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/web-login-sms", gin.H{
		"action":     "login",
		"platform":   service.PlatformZhipu,
		"phone":      "13800000000",
		"account_id": 42,
		"sms_code":   "123456",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "not a web access-mode account")
}
