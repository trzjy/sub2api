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
	return NewXianyuDeliveryHandler(service.NewXianyuDeliveryService(xianyuHandlerRepoStub{}, &xianyuHandlerControlStub{}, nil, cfg, newXianyuSettingsStub(true), nil), cfg)
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
	payload := service.XianyuDeliveryClaimRequest{OrderID: "order", ItemID: "item", OrderQuantity: "1", CookieID: "account", BuyerID: "buyer"}
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
		OrderID: "order", ItemID: "item", OrderQuantity: "1", CookieID: "account", BuyerID: "buyer",
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
