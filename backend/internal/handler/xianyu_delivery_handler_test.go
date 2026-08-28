package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		InternalToken: "test-secret", SystemUserID: 1, ItemPools: map[string]string{"account:item": "standard"},
	}}
	return NewXianyuDeliveryHandler(service.NewXianyuDeliveryService(xianyuHandlerRepoStub{}, cfg, newXianyuSettingsStub(true)), cfg)
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
