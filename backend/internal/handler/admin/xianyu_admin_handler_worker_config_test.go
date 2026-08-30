package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// workerConfigControlStub 实现 service.XianyuControlRepository，仅覆盖 worker-configs 三例
// 触发到的 5 个方法；其余方法返回零值。encryptor 用 noop 桩。
type workerConfigControlStub struct {
	configs     []service.XianyuWorkerConfig
	nextID      int64
	created     *service.XianyuWorkerConfig
	updated     *service.XianyuWorkerConfig
	activeFound bool
}

func (s *workerConfigControlStub) ListWorkerConfigs(context.Context) ([]service.XianyuWorkerConfig, error) {
	return s.configs, nil
}

func (s *workerConfigControlStub) GetWorkerConfigByID(_ context.Context, id int64) (*service.XianyuWorkerConfig, error) {
	for i := range s.configs {
		if s.configs[i].ID == id {
			return &s.configs[i], nil
		}
	}
	return nil, service.ErrXianyuWorkerConfigNotFound
}

func (s *workerConfigControlStub) CreateWorkerConfig(_ context.Context, cfg service.XianyuWorkerConfig) (*service.XianyuWorkerConfig, error) {
	s.nextID++
	cfg.ID = s.nextID
	if cfg.HealthStatus == "" {
		cfg.HealthStatus = service.XianyuWorkerHealthUnknown
	}
	s.created = &cfg
	return &cfg, nil
}

func (s *workerConfigControlStub) UpdateWorkerConfig(_ context.Context, cfg service.XianyuWorkerConfig) (*service.XianyuWorkerConfig, error) {
	for i := range s.configs {
		if s.configs[i].ID == cfg.ID {
			s.configs[i] = cfg
			s.updated = &cfg
			return &cfg, nil
		}
	}
	return nil, service.ErrXianyuWorkerConfigNotFound
}

func (s *workerConfigControlStub) GetActiveWorkerConfig(context.Context) (*service.XianyuWorkerConfig, error) {
	if !s.activeFound {
		return nil, service.ErrXianyuWorkerConfigNotFound
	}
	return &s.configs[0], nil
}

// ── 未触发的接口方法：返回零值即可 ──────────────────────────────

func (s *workerConfigControlStub) ListAccounts(context.Context, int64) ([]service.XianyuAccount, error) { return nil, nil }
func (s *workerConfigControlStub) GetAccountByWorkerAndAccountID(context.Context, int64, string) (*service.XianyuAccount, error) {
	return nil, service.ErrXianyuAccountNotFound
}
func (s *workerConfigControlStub) UpsertAccount(context.Context, service.XianyuAccount) (*service.XianyuAccount, error) {
	return nil, nil
}
func (s *workerConfigControlStub) UpdateAccount(context.Context, service.XianyuAccount) (*service.XianyuAccount, error) {
	return nil, nil
}
func (s *workerConfigControlStub) ListItemPools(context.Context) ([]service.XianyuItemPool, error) { return nil, nil }
func (s *workerConfigControlStub) GetItemPoolByID(context.Context, int64) (*service.XianyuItemPool, error) {
	return nil, service.ErrXianyuItemPoolNotFound
}
func (s *workerConfigControlStub) GetItemPoolBySlug(context.Context, string) (*service.XianyuItemPool, error) {
	return nil, service.ErrXianyuItemPoolNotFound
}
func (s *workerConfigControlStub) CreateItemPool(context.Context, service.XianyuItemPool) (*service.XianyuItemPool, error) {
	return nil, nil
}
func (s *workerConfigControlStub) UpdateItemPool(context.Context, service.XianyuItemPool) (*service.XianyuItemPool, error) {
	return nil, nil
}
func (s *workerConfigControlStub) PoolStockCounts(context.Context, string) (int, int, int, error) { return 0, 0, 0, nil }
func (s *workerConfigControlStub) DeliveryStats(context.Context, time.Time) (int, int, error)     { return 0, 0, nil }
func (s *workerConfigControlStub) PendingDeliveryCount(context.Context) (int, error)              { return 0, nil }
func (s *workerConfigControlStub) ListProducts(context.Context) ([]service.XianyuProduct, error)  { return nil, nil }
func (s *workerConfigControlStub) ListProductsByAccount(context.Context, int64) ([]service.XianyuProduct, error) {
	return nil, nil
}
func (s *workerConfigControlStub) GetProductByIdentity(context.Context, int64, string, string, string) (*service.XianyuProduct, error) {
	return nil, service.ErrXianyuProductNotFound
}
func (s *workerConfigControlStub) UpsertProduct(context.Context, service.XianyuProduct) (*service.XianyuProduct, error) {
	return nil, nil
}
func (s *workerConfigControlStub) UpdateProduct(context.Context, service.XianyuProduct) (*service.XianyuProduct, error) {
	return nil, nil
}
func (s *workerConfigControlStub) UpdateProductBinding(context.Context, int64, string, string, *int64) error {
	return nil
}
func (s *workerConfigControlStub) ListBindingRules(context.Context) ([]service.XianyuBindingRule, error) { return nil, nil }
func (s *workerConfigControlStub) CreateBindingRule(context.Context, service.XianyuBindingRule) (*service.XianyuBindingRule, error) {
	return nil, nil
}
func (s *workerConfigControlStub) UpdateBindingRule(context.Context, service.XianyuBindingRule) (*service.XianyuBindingRule, error) {
	return nil, nil
}

// noopEncryptor 满足 service.SecretEncryptor，保存明文不加密（测试专用）。
type noopEncryptor struct{}

func (noopEncryptor) Encrypt(plaintext string) (string, error) { return plaintext, nil }
func (noopEncryptor) Decrypt(ciphertext string) (string, error) {
	return ciphertext, nil
}

// newWorkerConfigsTestHandler 构造真实 XianyuControlService + 精简 repo/encryptor 桩。
// 除 control/encryptor 外的依赖传 nil（worker-configs 路径不触碰）。
func newWorkerConfigsTestHandler(stub *workerConfigControlStub) *XianyuAdminHandler {
	svc := service.NewXianyuControlService(
		stub, nil, nil, nil, noopEncryptor{},
		nil, nil, nil, nil, nil,
	)
	return NewXianyuAdminHandler(svc)
}

func sampleWorkerConfig() service.XianyuWorkerConfig {
	now := time.Now()
	return service.XianyuWorkerConfig{
		ID:            1,
		BaseURL:       "http://xianyu-worker-backend:8089",
		Status:        service.XianyuWorkerStatusActive,
		HealthStatus:  service.XianyuWorkerHealthHealthy,
		LastCheckedAt: &now,
	}
}

// 用例 1：GET 列表返回 snake_case 字段（修复根因）。
func TestWorkerConfigsListReturnsSnakeCaseFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &workerConfigControlStub{configs: []service.XianyuWorkerConfig{sampleWorkerConfig()}}
	handler := newWorkerConfigsTestHandler(stub)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/admin/xianyu/worker-configs", nil)

	handler.WorkerConfigs(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	// snake_case 键必须存在
	for _, key := range []string{"id", "base_url", "status", "health_status", "last_checked_at"} {
		require.Contains(t, body, `"`+key+`"`, "响应应包含 snake_case 键 "+key)
	}
	// 必须不能输出 PascalCase 裸字段名（修复前是 ID/BaseURL/Status/HealthStatus）
	for _, key := range []string{"\"ID\"", "\"BaseURL\"", "\"HealthStatus\""} {
		require.NotContains(t, body, key, "响应不应包含 PascalCase 键 "+key)
	}
	// token 字段应存在但为空（不回显明文）
	require.Contains(t, body, "api_token_encrypted")

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 1)
	require.Equal(t, float64(1), resp.Data[0]["id"])
	require.Equal(t, "http://xianyu-worker-backend:8089", resp.Data[0]["base_url"])
	require.Equal(t, "healthy", resp.Data[0]["health_status"])
	require.Equal(t, "", resp.Data[0]["api_token_encrypted"])
}

// 用例 2：已有配置时以 id=0 保存 → 409 XIANYU_WORKER_CONFIG_EXISTS。
func TestSaveWorkerConfigReturns409WhenConfigExists(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &workerConfigControlStub{configs: []service.XianyuWorkerConfig{sampleWorkerConfig()}}
	handler := newWorkerConfigsTestHandler(stub)

	body := `{"id":0,"base_url":"http://xianyu-worker-backend:8089","api_token":"tok","status":"active"}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/admin/xianyu/worker-configs", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.SaveWorkerConfig(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	require.Equal(t, http.StatusConflict, resp.Code)
	require.Equal(t, "XIANYU_WORKER_CONFIG_EXISTS", resp.Reason)
}

// 用例 3：成功保存（新建）返回 snake_case 字段。
func TestSaveWorkerConfigReturnsSnakeCaseOnSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &workerConfigControlStub{configs: []service.XianyuWorkerConfig{}}
	handler := newWorkerConfigsTestHandler(stub)

	body := `{"id":0,"base_url":"http://xianyu-worker-backend:8089","api_token":"tok","status":"active"}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/admin/xianyu/worker-configs", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.SaveWorkerConfig(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	raw := recorder.Body.String()
	for _, key := range []string{"id", "base_url", "status", "health_status"} {
		require.Contains(t, raw, `"`+key+`"`, "保存回执应包含 snake_case 键 "+key)
	}
	require.NotContains(t, raw, `"ID"`)
	require.NotContains(t, raw, `"BaseURL"`)
	// 保存回执 token 字段应存在但为空（不回显明文）
	require.Contains(t, raw, "api_token_encrypted")

	var resp struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
	require.Equal(t, float64(1), resp.Data["id"])
	require.Equal(t, "http://xianyu-worker-backend:8089", resp.Data["base_url"])
	require.Equal(t, "unknown", resp.Data["health_status"])
	require.Equal(t, "", resp.Data["api_token_encrypted"])
}
