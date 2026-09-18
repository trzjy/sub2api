package admin

import (
	"context"
	"encoding/json"
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
// 的普通 API 账号，不得进入自动登录 / 测试 / 删除候选 / 改状态 / 导出。
//
// 复用的测试替身（walAdminStub / stubAutoLogin / stubSessionStore / doRequest /
// newTestAccountHandler / decodeEnvelope）定义在 web_auto_login_handler_test.go。
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

// TestAccessModeIsolation_BatchLogin：同平台下 API 账号必须作为失败项返回，且
// 绝不进入 RecoverAccount / RefreshToken。
func TestAccessModeIsolation_BatchLogin(t *testing.T) {
	apiZhipu := apiAccount(1, service.PlatformZhipu)
	webZhipu := webAccount(2, service.PlatformZhipu)
	adminSvc := newWALAdminStub(apiZhipu, webZhipu)
	auto := &isolationAutoLogin{}
	h := newIsolationHandler(adminSvc, auto)

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/batch-login", gin.H{
		"ids": []int64{1, 2},
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
	require.Len(t, data.Results, 2)
	require.Equal(t, 1, data.Summary.Success)
	require.Equal(t, 1, data.Summary.Failed)

	byID := map[int64]string{}
	for _, r := range data.Results {
		byID[r.ID] = r.Detail
	}
	// API 账号：明确失败并给出隔离原因（不静默丢弃）。
	require.Equal(t, "not a web access-mode account", byID[1])
	require.Equal(t, "", byID[2])

	// 证明未对 API 账号发起登录/刷新：只对 web 账号调用过。
	require.Empty(t, auto.recoverCalls)
	require.Equal(t, []int64{2}, auto.refreshCalls)
}

// TestAccessModeIsolation_BatchTest：API 账号不发起测试，作为失败项返回。
//
// accountTestService 是具体类型、无法桩替换；这里用零值 *AccountTestService 且
// 请求只含 API 账号——若隔离缺失，RunTestBackground 会被调用（零值服务报错或
// panic），断言即失败；隔离生效则根本不会触碰该服务。
func TestAccessModeIsolation_BatchTest(t *testing.T) {
	apiZhipu := apiAccount(7, service.PlatformZhipu)
	adminSvc := newWALAdminStub(apiZhipu)
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})
	h.accountTestService = &service.AccountTestService{}

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/batch-test", gin.H{
		"ids": []int64{7},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	env := decodeEnvelope(t, w)
	var data struct {
		Results []struct {
			ID      int64  `json:"id"`
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Len(t, data.Results, 1)
	require.Equal(t, int64(7), data.Results[0].ID)
	require.False(t, data.Results[0].Success)
	require.Equal(t, "not a web access-mode account", data.Results[0].Error)
}

// TestAccessModeIsolation_BatchDeleteBanned：同平台 API 账号即使错误文本命中
// 封禁词，也不得进入删除候选；web 账号正常进候选并可被删除。
func TestAccessModeIsolation_BatchDeleteBanned(t *testing.T) {
	apiZhipu := apiAccount(11, service.PlatformZhipu)
	apiZhipu.ErrorMessage = "账号已被封禁" // 普通 API 账号命中封禁词
	webZhipu := webAccount(12, service.PlatformZhipu)
	webZhipu.ErrorMessage = "账号已被封禁"

	adminSvc := newWALAdminStub(apiZhipu, webZhipu)
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})

	// 列表模式：候选只含 web 账号。
	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/batch-delete-banned", gin.H{"confirm": false})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeEnvelope(t, w)
	var listData struct {
		Candidates []struct {
			ID int64 `json:"id"`
		} `json:"candidates"`
		Deleted int `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &listData))
	require.Len(t, listData.Candidates, 1)
	require.Equal(t, int64(12), listData.Candidates[0].ID)
	require.Equal(t, 0, listData.Deleted)
	require.Empty(t, adminSvc.deleted)

	// 确认模式：只删除 web 账号，API 账号不受影响。
	w = doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/batch-delete-banned", gin.H{"confirm": true})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env = decodeEnvelope(t, w)
	var confirmData struct {
		Deleted int `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &confirmData))
	require.Equal(t, 1, confirmData.Deleted)
	require.Equal(t, []int64{12}, adminSvc.deleted)
}

// TestAccessModeIsolation_BatchStatus：API 账号不得被改状态，计入失败项。
func TestAccessModeIsolation_BatchStatus(t *testing.T) {
	apiDs := apiAccount(21, service.PlatformDeepseek)
	apiDs.Status = service.StatusActive
	webDs := webAccount(22, service.PlatformDeepseek)
	webDs.Status = service.StatusActive

	adminSvc := newWALAdminStub(apiDs, webDs)
	h := newTestAccountHandler(adminSvc, &stubAutoLogin{})

	w := doRequest(t, h, http.MethodPost, "/api/v1/admin/accounts/batch-status", gin.H{
		"ids":    []int64{21, 22},
		"status": service.StatusDisabled,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	env := decodeEnvelope(t, w)
	var data struct {
		Total   int `json:"total"`
		Success int `json:"success"`
		Failed  int `json:"failed"`
		Errors  []struct {
			AccountID int64  `json:"account_id"`
			Error     string `json:"error"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, 2, data.Total)
	require.Equal(t, 1, data.Success)
	require.Equal(t, 1, data.Failed)
	require.Len(t, data.Errors, 1)
	require.Equal(t, int64(21), data.Errors[0].AccountID)
	require.Equal(t, "not a web access-mode account", data.Errors[0].Error)

	// API 账号状态未被修改；web 账号已置 disabled。
	gotAPI, err := adminSvc.GetAccount(context.Background(), 21)
	require.NoError(t, err)
	require.Equal(t, service.StatusActive, gotAPI.Status)
	gotWeb, err := adminSvc.GetAccount(context.Background(), 22)
	require.NoError(t, err)
	require.Equal(t, service.StatusDisabled, gotWeb.Status)
}

// TestAccessModeIsolation_ExportWebAccounts：导出 JSON 不含同平台 API 账号。
func TestAccessModeIsolation_ExportWebAccounts(t *testing.T) {
	apiDs := apiAccount(31, service.PlatformDeepseek)
	webDs := webAccount(32, service.PlatformDeepseek)
	webDs.Credentials["login_email"] = "web@example.com"

	adminSvc := newWALAdminStub(apiDs, webDs)
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
	require.Equal(t, 1, data.Total)
	require.Equal(t, int64(32), data.Accounts[0].ID)
	require.Equal(t, "web@example.com", data.Accounts[0].EmailOrPhone)

	// 安全：导出体不含 API 账号。
	require.NotContains(t, w.Body.String(), "api-deepseek")
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
