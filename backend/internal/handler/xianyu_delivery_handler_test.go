package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type xianyuHandlerRepoStub struct{}

func (xianyuHandlerRepoStub) Claim(context.Context, service.XianyuDeliveryClaim, int64) (string, error) {
	return "ABCD-1234", nil
}

func newXianyuHandlerForTest() *XianyuDeliveryHandler {
	cfg := &config.Config{XianyuDelivery: config.XianyuDeliveryConfig{
		InternalToken: "test-secret", SystemUserID: 1,
	}}
	return NewXianyuDeliveryHandler(service.NewXianyuDeliveryService(xianyuHandlerRepoStub{}, &xianyuHandlerControlStub{}, nil, nil, cfg, newXianyuSettingsStub(true), nil), cfg)
}

// xianyuHandlerControlStub 提供 Claim 所需的最小控制面解析。
type xianyuHandlerControlStub struct{}

func (xianyuHandlerControlStub) ListWorkerConfigs(context.Context) ([]service.XianyuWorkerConfig, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) CreateWorkerConfig(context.Context, service.XianyuWorkerConfig) (*service.XianyuWorkerConfig, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) UpdateWorkerConfig(context.Context, service.XianyuWorkerConfig) (*service.XianyuWorkerConfig, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) GetActiveWorkerConfig(context.Context) (*service.XianyuWorkerConfig, error) {
	return &service.XianyuWorkerConfig{ID: 1, Status: service.XianyuWorkerStatusActive}, nil
}
func (xianyuHandlerControlStub) GetWorkerConfigByID(context.Context, int64) (*service.XianyuWorkerConfig, error) {
	return &service.XianyuWorkerConfig{ID: 1, Status: service.XianyuWorkerStatusActive}, nil
}
func (xianyuHandlerControlStub) ListAccounts(context.Context, int64) ([]service.XianyuAccount, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) GetAccountByWorkerAndAccountID(context.Context, int64, string) (*service.XianyuAccount, error) {
	return &service.XianyuAccount{ID: 11, Status: service.XianyuAccountStatusEnabled}, nil
}
func (xianyuHandlerControlStub) UpsertAccount(context.Context, service.XianyuAccount) (*service.XianyuAccount, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) UpdateAccount(context.Context, service.XianyuAccount) (*service.XianyuAccount, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) ListItemPools(context.Context) ([]service.XianyuItemPool, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) GetItemPoolByID(context.Context, int64) (*service.XianyuItemPool, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) GetItemPoolBySlug(context.Context, string) (*service.XianyuItemPool, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) CreateItemPool(context.Context, service.XianyuItemPool) (*service.XianyuItemPool, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) UpdateItemPool(context.Context, service.XianyuItemPool) (*service.XianyuItemPool, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) PoolStockCounts(context.Context, string) (int, int, int, error) {
	return 0, 0, 0, nil
}
func (xianyuHandlerControlStub) DeliveryStats(context.Context, time.Time) (int, int, error) {
	return 0, 0, nil
}
func (xianyuHandlerControlStub) PendingDeliveryCount(context.Context) (int, error) {
	return 0, nil
}
func (xianyuHandlerControlStub) ListProducts(context.Context) ([]service.XianyuProduct, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) ListProductsByAccount(context.Context, int64) ([]service.XianyuProduct, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) GetProductByIdentity(context.Context, int64, string, string, string) (*service.XianyuProduct, error) {
	poolID := int64(1)
	return &service.XianyuProduct{ID: 21, ItemID: "item", BindingStatus: service.XianyuBindingStatusMapped, PoolID: &poolID, Status: service.XianyuProductStatusActive}, nil
}
func (xianyuHandlerControlStub) UpsertProduct(context.Context, service.XianyuProduct) (*service.XianyuProduct, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) UpdateProduct(context.Context, service.XianyuProduct) (*service.XianyuProduct, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) UpdateProductBinding(context.Context, int64, string, string, *int64) error {
	return nil
}
func (xianyuHandlerControlStub) ListBindingRules(context.Context) ([]service.XianyuBindingRule, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) CreateBindingRule(context.Context, service.XianyuBindingRule) (*service.XianyuBindingRule, error) {
	return nil, nil
}
func (xianyuHandlerControlStub) UpdateBindingRule(context.Context, service.XianyuBindingRule) (*service.XianyuBindingRule, error) {
	return nil, nil
}

type newXianyuSettingsStubType struct {
	enabled bool
}

func newXianyuSettingsStub(enabled bool) *newXianyuSettingsStubType {
	return &newXianyuSettingsStubType{enabled: enabled}
}

func (s *newXianyuSettingsStubType) GetXianyuDeliveryRuntime(context.Context) service.XianyuDeliveryRuntime {
	return service.XianyuDeliveryRuntime{Enabled: s.enabled}
}

func TestXianyuDeliveryHandlerRejectsMissingOrWrongToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/claim", newXianyuHandlerForTest().Claim)
	body := bytes.NewBufferString(`{"order_id":"order","item_id":"item","order_quantity":"1","cookie_id":"account","buyer_id":"buyer"}`)
	req := httptest.NewRequest(http.MethodPost, "/claim", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusUnauthorized, resp.Code)
}

func TestXianyuDeliveryHandlerReturnsStandardResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/claim", newXianyuHandlerForTest().Claim)
	payload := service.XianyuDeliveryClaimRequest{OrderID: "order", ItemID: "item", OrderQuantity: "1", CookieID: "account", BuyerID: "buyer", ChatID: "chat"}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/claim", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", "test-secret")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	require.JSONEq(t, `{"code":0,"message":"success","data":{"content":"ABCD-1234"}}`, resp.Body.String())
}

func TestXianyuDeliveryHandlerRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/claim", newXianyuHandlerForTest().Claim)
	payload := service.XianyuDeliveryClaimRequest{
		OrderID: "order", ItemID: "item", OrderQuantity: "1", CookieID: "account", BuyerID: "buyer", ChatID: "chat",
		ItemDetail: strings.Repeat("x", 17*1024),
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/claim", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", "test-secret")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusBadRequest, resp.Code)
}

// xianyuHandlerStateStub 实现 XianyuDeliveryStateUpdater，供 DeliveryResult 测试。
type xianyuHandlerStateStub struct {
	recorded *service.XianyuDeliveryStatusResult
}

func (s *xianyuHandlerStateStub) RecordDeliveryResult(_ context.Context, result service.XianyuDeliveryStatusResult) error {
	s.recorded = &result
	return nil
}
func (s *xianyuHandlerStateStub) GetDeliveryClaim(context.Context, string) (*service.XianyuOrderClaim, error) {
	return nil, nil
}
func (s *xianyuHandlerStateStub) ResendOriginalCode(context.Context, string) (string, int, error) {
	return "", 0, nil
}

func TestXianyuDeliveryHandlerRecordsResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	state := &xianyuHandlerStateStub{}
	cfg := &config.Config{XianyuDelivery: config.XianyuDeliveryConfig{
		InternalToken: "test-secret", SystemUserID: 1,
	}}
	delivery := service.NewXianyuDeliveryService(&xianyuHandlerRepoStub{}, &xianyuHandlerControlStub{}, state, nil, cfg, newXianyuSettingsStub(true), nil)
	h := NewXianyuDeliveryHandler(delivery, cfg)

	r := gin.New()
	r.POST("/delivery-results", h.DeliveryResult)

	body := bytes.NewBufferString(`{"order_no":"order-1","success":true,"confirmed":true}`)
	req := httptest.NewRequest(http.MethodPost, "/delivery-results", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", "test-secret")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	require.NotNil(t, state.recorded)
	require.Equal(t, "order-1", state.recorded.OrderNo)
	require.True(t, state.recorded.Success)
	require.True(t, state.recorded.Confirmed)
}

func TestXianyuDeliveryHandlerResultRequiresTokenAndOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newXianyuHandlerForTest()
	r := gin.New()
	r.POST("/delivery-results", h.DeliveryResult)

	// 缺 token -> 401
	body := bytes.NewBufferString(`{"order_no":"order-1","success":true}`)
	req := httptest.NewRequest(http.MethodPost, "/delivery-results", body)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusUnauthorized, resp.Code)

	// 有 token 但缺 order_no -> 400
	req = httptest.NewRequest(http.MethodPost, "/delivery-results", bytes.NewBufferString(`{"success":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", "test-secret")
	resp = httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusBadRequest, resp.Code)
}

func TestXianyuDeliveryHandlerForwardsQuantitySent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	state := &xianyuHandlerStateStub{}
	cfg := &config.Config{XianyuDelivery: config.XianyuDeliveryConfig{
		InternalToken: "test-secret", SystemUserID: 1,
	}}
	delivery := service.NewXianyuDeliveryService(&xianyuHandlerRepoStub{}, &xianyuHandlerControlStub{}, state, nil, cfg, newXianyuSettingsStub(true), nil)
	h := NewXianyuDeliveryHandler(delivery, cfg)

	r := gin.New()
	r.POST("/delivery-results", h.DeliveryResult)

	// 多数量订单只成功 2 份：quantity_sent=2（不是 quantity=3）。
	body := bytes.NewBufferString(`{"order_no":"order-1","success":true,"confirmed":true,"quantity_sent":2}`)
	req := httptest.NewRequest(http.MethodPost, "/delivery-results", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", "test-secret")
	resp := httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	require.NotNil(t, state.recorded)
	require.Equal(t, 2, state.recorded.QuantitySent)

	// 缺 quantity_sent：按 0 处理，不报错（兼容旧 Worker）。
	body = bytes.NewBufferString(`{"order_no":"order-2","success":true,"confirmed":true}`)
	req = httptest.NewRequest(http.MethodPost, "/delivery-results", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", "test-secret")
	resp = httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	require.NotNil(t, state.recorded)
	require.Equal(t, 0, state.recorded.QuantitySent)

	// quantity_sent=0（显式 0）：按 0 处理。
	body = bytes.NewBufferString(`{"order_no":"order-3","success":true,"confirmed":true,"quantity_sent":0}`)
	req = httptest.NewRequest(http.MethodPost, "/delivery-results", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", "test-secret")
	resp = httptest.NewRecorder()
	r.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	require.Equal(t, 0, state.recorded.QuantitySent)
}
