package admin

// 火山订阅号同步端点（/models/sync-volcano-plan）的分支契约：非火山账号 400 守卫、
// 账号不存在 404、非法 id 400、无效请求体 400、未配置服务 500。preview/apply 与
// 401/400/错误分类的真实语义在 service 层火山编排测试全量覆盖（admin 包无法注入
// 服务内部官方文档夹具，故此处覆盖无需文档可达的确定性守卫分支）。

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// volcanoPlanAdminService 覆盖 GetAccount 返回受控账号（可嵌套 stub 兜底其余方法）。
type volcanoPlanAdminService struct {
	*stubAdminService
	account   service.Account
	getErr    error
}

func (s *volcanoPlanAdminService) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.account.ID == id {
		acc := s.account
		return &acc, nil
	}
	return nil, service.ErrAccountNotFound
}

func setupVolcanoPlanSyncRouter(adminSvc service.AdminService, testSvc *service.AccountTestService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, testSvc, nil, nil, nil, nil, nil)
	router.POST("/api/v1/admin/accounts/:id/models/sync-volcano-plan", handler.SyncVolcanoPlanModels)
	return router
}

func volcanoPlanAccount() service.Account {
	return service.Account{
		ID:       101,
		Platform: service.PlatformDeepseek,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "ark-key",
			"base_url":     "https://ark.cn-beijing.volces.com/api/coding",
			"api_protocol": "chat_completions",
		},
	}
}

func TestVolcanoPlanSync_NonVolcanoAccountRejected(t *testing.T) {
	svc := &volcanoPlanAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       9,
			Platform: service.PlatformDeepseek,
			Type:     service.AccountTypeAPIKey,
			Credentials: map[string]any{
				"base_url": "https://api.deepseek.com",
			},
		},
	}
	router := setupVolcanoPlanSyncRouter(svc, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/9/models/sync-volcano-plan",
		bytes.NewReader([]byte(`{"apply":false}`)))
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Volcengine", "应为火山专属误用提示")
}

func TestVolcanoPlanSync_AccountNotFound(t *testing.T) {
	svc := &volcanoPlanAdminService{
		stubAdminService: newStubAdminService(),
		getErr:           service.ErrAccountNotFound,
	}
	router := setupVolcanoPlanSyncRouter(svc, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/999/models/sync-volcano-plan",
		bytes.NewReader([]byte(`{"apply":false}`)))
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestVolcanoPlanSync_InvalidID(t *testing.T) {
	svc := &volcanoPlanAdminService{stubAdminService: newStubAdminService()}
	router := setupVolcanoPlanSyncRouter(svc, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/not_a_number/models/sync-volcano-plan",
		bytes.NewReader([]byte(`{"apply":false}`)))
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestVolcanoPlanSync_InvalidBody(t *testing.T) {
	svc := &volcanoPlanAdminService{
		stubAdminService: newStubAdminService(),
		account:          volcanoPlanAccount(),
	}
	router := setupVolcanoPlanSyncRouter(svc, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/101/models/sync-volcano-plan",
		bytes.NewReader([]byte(`not-json`)))
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestVolcanoPlanSync_ServiceNotConfigured(t *testing.T) {
	svc := &volcanoPlanAdminService{
		stubAdminService: newStubAdminService(),
		account:          volcanoPlanAccount(),
	}
	// account 是火山账号、accountTestService 为 nil → 未配置 500（不触文档）。
	router := setupVolcanoPlanSyncRouter(svc, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/101/models/sync-volcano-plan",
		bytes.NewReader([]byte(`{"apply":false}`)))
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}