package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeAccountRepo 实现 service.AccountRepository 接口；检测服务仅用 GetByID，
// 其余方法均为测试桩（返回零值）。
type fakeAccountRepo struct {
	account *service.Account
}

func (r *fakeAccountRepo) Create(ctx context.Context, account *service.Account) error {
	return nil
}
func (r *fakeAccountRepo) GetByID(ctx context.Context, id int64) (*service.Account, error) {
	return r.account, nil
}
func (r *fakeAccountRepo) GetByIDs(ctx context.Context, ids []int64) ([]*service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ExistsByID(ctx context.Context, id int64) (bool, error) {
	return false, nil
}
func (r *fakeAccountRepo) GetByCRSAccountID(ctx context.Context, crsAccountID string) (*service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) FindByExtraField(ctx context.Context, key string, value any) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListCRSAccountIDs(ctx context.Context) (map[string]int64, error) {
	return nil, nil
}
func (r *fakeAccountRepo) Update(ctx context.Context, account *service.Account) error {
	return nil
}
func (r *fakeAccountRepo) Delete(ctx context.Context, id int64) error { return nil }
func (r *fakeAccountRepo) List(ctx context.Context, params pagination.PaginationParams) ([]service.Account, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (r *fakeAccountRepo) ListWithFilters(ctx context.Context, params pagination.PaginationParams, platform, accountType, status, search string, groupID int64, privacyMode string) ([]service.Account, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (r *fakeAccountRepo) ListAllWithFilters(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode string) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListByGroup(ctx context.Context, groupID int64) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListActive(ctx context.Context) ([]service.Account, error) { return nil, nil }
func (r *fakeAccountRepo) ListByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) UpdateLastUsed(ctx context.Context, id int64) error { return nil }
func (r *fakeAccountRepo) BatchUpdateLastUsed(ctx context.Context, updates map[int64]time.Time) error {
	return nil
}
func (r *fakeAccountRepo) SetError(ctx context.Context, id int64, errorMsg string) error { return nil }
func (r *fakeAccountRepo) ClearError(ctx context.Context, id int64) error                 { return nil }
func (r *fakeAccountRepo) SetSchedulable(ctx context.Context, id int64, schedulable bool) error {
	return nil
}
func (r *fakeAccountRepo) AutoPauseExpiredAccounts(ctx context.Context, now time.Time) (int64, error) {
	return 0, nil
}
func (r *fakeAccountRepo) BindGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	return nil
}
func (r *fakeAccountRepo) ListSchedulable(ctx context.Context) ([]service.Account, error) { return nil, nil }
func (r *fakeAccountRepo) ListSchedulableByGroupID(ctx context.Context, groupID int64) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListSchedulableByPlatforms(ctx context.Context, platforms []string) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, groupID int64, platforms []string) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListSchedulableUngroupedByPlatforms(ctx context.Context, platforms []string) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListModelAvailabilityCandidates(ctx context.Context, groupID *int64, platforms []string, includeGrouped bool) ([]service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	return nil
}
func (r *fakeAccountRepo) SetModelRateLimit(ctx context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	return nil
}
func (r *fakeAccountRepo) SetOverloaded(ctx context.Context, id int64, until time.Time) error { return nil }
func (r *fakeAccountRepo) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	return nil
}
func (r *fakeAccountRepo) ClearTempUnschedulable(ctx context.Context, id int64) error { return nil }
func (r *fakeAccountRepo) ListTempUnschedulableAccounts(ctx context.Context, now time.Time, limit int) ([]*service.Account, error) {
	return nil, nil
}
func (r *fakeAccountRepo) SetTempUnschedulableReason(ctx context.Context, id int64, reason string) error {
	return nil
}
func (r *fakeAccountRepo) ClearRateLimit(ctx context.Context, id int64) error { return nil }
func (r *fakeAccountRepo) ClearAntigravityQuotaScopes(ctx context.Context, id int64) error {
	return nil
}
func (r *fakeAccountRepo) ClearModelRateLimits(ctx context.Context, id int64) error { return nil }
func (r *fakeAccountRepo) UpdateSessionWindow(ctx context.Context, id int64, start, end *time.Time, status string) error {
	return nil
}
func (r *fakeAccountRepo) UpdateSessionWindowEnd(ctx context.Context, id int64, end time.Time) error {
	return nil
}
func (r *fakeAccountRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	return nil
}
func (r *fakeAccountRepo) BulkUpdate(ctx context.Context, ids []int64, updates service.AccountBulkUpdate) (int64, error) {
	return 0, nil
}
func (r *fakeAccountRepo) IncrementQuotaUsed(ctx context.Context, id int64, amount float64) error {
	return nil
}
func (r *fakeAccountRepo) ResetQuotaUsedAndClearRateLimitCooldown(ctx context.Context, id int64) error {
	return nil
}
func (r *fakeAccountRepo) RevertProxyFallback(ctx context.Context, accountID int64) error { return nil }
func (r *fakeAccountRepo) ListShadowsByParent(ctx context.Context, parentID int64) ([]*service.Account, error) {
	return nil, nil
}

// fakeCapRepo 记录 Upsert 调用，用于断言落库行为。
type fakeCapRepo struct {
	mu      sync.Mutex
	records []*model.AccountModelCapability
}

func (r *fakeCapRepo) Get(ctx context.Context, accountID int64, upstreamModel, protocol string) (*model.AccountModelCapability, error) {
	return nil, nil
}
func (r *fakeCapRepo) Upsert(ctx context.Context, cap *model.AccountModelCapability) (*model.AccountModelCapability, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, cap)
	return cap, nil
}
func (r *fakeCapRepo) ListByAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*model.AccountModelCapability, len(r.records))
	copy(out, r.records)
	return out, nil
}
func (r *fakeCapRepo) DeleteByAccount(ctx context.Context, accountID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = nil
	return nil
}
func (r *fakeCapRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

// fakeCapCache 缓存桩（不回源）。
type fakeCapCache struct{}

func (c *fakeCapCache) GetAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, bool) {
	return nil, false
}
func (c *fakeCapCache) SetAccount(ctx context.Context, accountID int64, caps []*model.AccountModelCapability) error {
	return nil
}
func (c *fakeCapCache) InvalidateAccount(ctx context.Context, accountID int64) error { return nil }
func (c *fakeCapCache) NotifyUpdate(ctx context.Context, accountID int64, suspectRedis bool) error {
	return nil
}
func (c *fakeCapCache) SubscribeUpdates(ctx context.Context, handler func(int64, bool)) {}

// fakeDetectHTTPDoer 替代真实上游 HTTP 请求，直接返回预设响应。
type fakeDetectHTTPDoer struct {
	statusCode int
	body       string
}

func (f *fakeDetectHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: f.statusCode,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func newVisionTestHandler(doer *fakeDetectHTTPDoer) (*VisionCapabilityHandler, *fakeCapRepo) {
	capRepo := &fakeCapRepo{}
	capSvc := service.NewAccountModelCapabilityService(capRepo, &fakeCapCache{})
	accRepo := &fakeAccountRepo{account: &service.Account{
		ID:          1,
		Platform:    service.PlatformDeepseek,
		Type:        service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
	}}
	detect := service.NewVisionDetectService(accRepo, capSvc)
	detect.SetHTTPDoer(doer)
	return &VisionCapabilityHandler{detect: detect, capability: capSvc}, capRepo
}

func decodeVisionDetectBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	data, ok := body["data"].(map[string]any)
	require.True(t, ok, "response should wrap payload under data field: %s", rec.Body.String())
	return data
}

// ---------------------------------------------------------------------------
// 检测端点测试
// ---------------------------------------------------------------------------

func TestVisionCapabilityDetectPersistsOnDefinitiveResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 200 但响应不含验证码 → unsupported（仍是确定性结果，应落库 supports_vision=false）
	doer := &fakeDetectHTTPDoer{statusCode: 200, body: `{"choices":[{"message":{"content":"hello world"}}]}`}
	h, capRepo := newVisionTestHandler(doer)

	r := gin.New()
	r.POST("/accounts/:id/vision-capability/detect", h.Detect)
	req := httptest.NewRequest(http.MethodPost, "/accounts/1/vision-capability/detect",
		bytes.NewBufferString(`{"model":"gpt-4","protocol":"chat_completions"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeVisionDetectBody(t, rec)
	require.Equal(t, "unsupported", body["status"])
	require.Equal(t, false, body["supports_vision"])

	require.Equal(t, 1, capRepo.count(), "确定性检测结果应落库")
	rec0 := capRepo.records[0]
	require.Equal(t, model.CapabilitySourceDetect, rec0.Source)
	require.False(t, rec0.SupportsVision)
}

func TestVisionCapabilityDetectFailedDoesNotPersist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	doer := &fakeDetectHTTPDoer{statusCode: 500, body: "upstream error"}
	h, capRepo := newVisionTestHandler(doer)

	r := gin.New()
	r.POST("/accounts/:id/vision-capability/detect", h.Detect)
	req := httptest.NewRequest(http.MethodPost, "/accounts/1/vision-capability/detect",
		bytes.NewBufferString(`{"model":"gpt-4","protocol":"chat_completions"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeVisionDetectBody(t, rec)
	require.Equal(t, "detect_failed", body["status"])
	require.Nil(t, body["supports_vision"])
	require.Equal(t, 0, capRepo.count(), "detect_failed 不应落库")
}

func TestVisionCapabilityDetectManualReviewDoesNotPersist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 200 + 策略性拒答 → manual_review，不落库。
	// 注意：manual_review 仅限明确政策/意愿类拒答（方案 §3.5，U 轮 P2-E 收窄）；
	// 能力性措辞（cannot recognize 等）走 unsupported 落 false，见分类矩阵测试。
	doer := &fakeDetectHTTPDoer{statusCode: 200, body: `{"choices":[{"message":{"content":"I'm sorry, I won't help with reading captchas, it's against my policy"}}]}`}
	h, capRepo := newVisionTestHandler(doer)

	r := gin.New()
	r.POST("/accounts/:id/vision-capability/detect", h.Detect)
	req := httptest.NewRequest(http.MethodPost, "/accounts/1/vision-capability/detect",
		bytes.NewBufferString(`{"model":"gpt-4","protocol":"chat_completions"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeVisionDetectBody(t, rec)
	require.Equal(t, "manual_review", body["status"])
	require.Nil(t, body["supports_vision"])
	require.Equal(t, 0, capRepo.count(), "manual_review 不应落库")
}

func TestVisionCapabilityManualOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	doer := &fakeDetectHTTPDoer{statusCode: 200, body: `{"choices":[{"message":{"content":"hello"}}]}`}
	h, capRepo := newVisionTestHandler(doer)

	r := gin.New()
	r.PUT("/accounts/:id/vision-capability", h.Override)
	req := httptest.NewRequest(http.MethodPut, "/accounts/1/vision-capability",
		bytes.NewBufferString(`{"model":"gpt-4","protocol":"chat_completions","supports_vision":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, capRepo.count(), "人工覆盖应落库")
	rec0 := capRepo.records[0]
	require.Equal(t, model.CapabilitySourceManual, rec0.Source, "覆盖写应为 manual 来源")
	require.True(t, rec0.SupportsVision)
	require.Equal(t, int64(1), rec0.AccountID)
	require.Equal(t, "gpt-4", rec0.UpstreamModel)
}

func TestVisionCapabilityDetectBatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	doer := &fakeDetectHTTPDoer{statusCode: 200, body: `{"choices":[{"message":{"content":"hello world"}}]}`}
	h, capRepo := newVisionTestHandler(doer)

	r := gin.New()
	r.POST("/accounts/vision-capability/detect", h.DetectBatch)
	body := `{"account_ids":[1,2],"items":[{"model":"gpt-4","protocol":"chat_completions"}]}`
	req := httptest.NewRequest(http.MethodPost, "/accounts/vision-capability/detect", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var wrapped struct {
		Data visionBatchResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &wrapped))
	require.Len(t, wrapped.Data.Results, 2)
	for _, item := range wrapped.Data.Results {
		require.Equal(t, "unsupported", item.Status)
		require.NotNil(t, item.SupportsVision)
		require.False(t, *item.SupportsVision)
	}
	require.Equal(t, 2, capRepo.count(), "批量每个账号各落库一条")
}

// ---------------------------------------------------------------------------
// 分组视觉分流同组校验（group create/update 链路接线）
// ---------------------------------------------------------------------------

type fakeVisionRoutingRepo struct {
	mu   sync.Mutex
	data map[int64]map[string][]int64
}

func (r *fakeVisionRoutingRepo) Get(ctx context.Context, groupID int64) (map[string][]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.data[groupID], nil
}
func (r *fakeVisionRoutingRepo) GetByGroupIDs(ctx context.Context, groupIDs []int64) (map[int64]map[string][]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[int64]map[string][]int64, len(groupIDs))
	for _, id := range groupIDs {
		if v, ok := r.data[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}
func (r *fakeVisionRoutingRepo) Set(ctx context.Context, groupID int64, routing map[string][]int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.data == nil {
		r.data = map[int64]map[string][]int64{}
	}
	r.data[groupID] = routing
	return nil
}

type fakeGroupAccountsReader struct{ allowed []int64 }

func (r *fakeGroupAccountsReader) GetAccountIDsByGroupIDs(ctx context.Context, groupIDs []int64) ([]int64, error) {
	return r.allowed, nil
}

// TestGroupVisionRoutingCrossGroupRejected 跨组账号在 group 创建链路被拒绝（4xx）。
func TestGroupVisionRoutingCrossGroupRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newStubAdminService()
	vs := service.NewVisionRoutingService(
		&fakeVisionRoutingRepo{},
		&fakeGroupAccountsReader{allowed: []int64{1, 2, 3}},
	)
	h := NewGroupHandlerWithConfig(svc, nil, nil, &config.Config{})
	h.SetVisionRoutingService(vs)

	r := gin.New()
	r.POST("/groups", h.Create)

	// 引用不属于本分组的账号 999 → 同组校验拒绝
	body := `{"name":"g","platform":"openai","vision_routing":{"gpt-4":[999]}}`
	req := httptest.NewRequest(http.MethodPost, "/groups", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.GreaterOrEqual(t, rec.Code, 400, "跨组账号应被拒绝")
	require.Less(t, rec.Code, 500, "跨组账号应返回 4xx 而非 5xx")
}

// TestGroupVisionRoutingAllowed 同组账号通过校验。
func TestGroupVisionRoutingAllowed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newStubAdminService()
	vs := service.NewVisionRoutingService(
		&fakeVisionRoutingRepo{},
		&fakeGroupAccountsReader{allowed: []int64{1, 2, 3}},
	)
	h := NewGroupHandlerWithConfig(svc, nil, nil, &config.Config{})
	h.SetVisionRoutingService(vs)

	r := gin.New()
	r.POST("/groups", h.Create)

	body := `{"name":"g","platform":"openai","vision_routing":{"gpt-4":[2]}}`
	req := httptest.NewRequest(http.MethodPost, "/groups", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "同组账号应通过校验")
}
