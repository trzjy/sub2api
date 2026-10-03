package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newTestSupplyOrderHandler 构造基于内存实现的订单 handler 与路由。
func newTestSupplyOrderHandler(cardNos []string) (*gin.Engine, *xianguanjia.SupplyOrderService) {
	gin.SetMode(gin.TestMode)
	store := xianguanjia.NewSupplyOrderStoreMemory()
	pool := xianguanjia.NewSupplyCardPoolMemory(cardNos, nil)
	svc := xianguanjia.NewSupplyOrderService(store, pool, xianguanjia.NewSupplyCardPwdResolverMemory(pool))
	h := NewXianguanjiaSupplyOrderHandler(svc)
	r := gin.New()
	r.POST("/create", h.CreateOrder)
	r.POST("/get", h.GetOrder)
	r.POST("/refund", h.RefundNotify)
	return r, svc
}

type supplyOrderTestEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type supplyOrderTestData struct {
	OrderStatus int  `json:"order_status"`
	Refunded    bool `json:"refunded"`
	CardItems   []struct {
		CardNo  string `json:"card_no"`
		CardPwd string `json:"card_pwd"`
	} `json:"card_items"`
	Result string `json:"result"`
}

func doSupplyOrderRequest(t *testing.T, r *gin.Engine, path string, body any) supplyOrderTestEnvelope {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		buf.Write(raw)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "官方信封固定 HTTP 200")
	var env supplyOrderTestEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	return env
}

func TestSupplyOrderHandlerCreateAndDuplicate(t *testing.T) {
	r, _ := newTestSupplyOrderHandler([]string{"card-1", "card-2", "card-3"})

	env := doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H1", "goods_no": "42", "quantity": 2, "notify_url": "https://x/notify",
	})
	require.Equal(t, xianguanjia.SupplyCodeOK, env.Code)
	var data supplyOrderTestData
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, 20, data.OrderStatus)
	require.Len(t, data.CardItems, 2)

	// 重复订单号：幂等返回同一批卡，code=0（不返回 1203，不重发卡）。
	env2 := doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H1", "goods_no": "42", "quantity": 2,
	})
	require.Equal(t, xianguanjia.SupplyCodeOK, env2.Code)
	var data2 supplyOrderTestData
	require.NoError(t, json.Unmarshal(env2.Data, &data2))
	require.Equal(t, data.CardItems, data2.CardItems)
}

func TestSupplyOrderHandlerInsufficientStock(t *testing.T) {
	r, _ := newTestSupplyOrderHandler([]string{"card-1"})
	env := doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H2", "goods_no": "42", "quantity": 5,
	})
	require.Equal(t, xianguanjia.SupplyCodeStockInsufficient, env.Code, "库存不足须返回 1102")
}

func TestSupplyOrderHandlerGetOrder(t *testing.T) {
	r, _ := newTestSupplyOrderHandler([]string{"card-1", "card-2"})
	require.Equal(t, xianguanjia.SupplyCodeOK, doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H3", "goods_no": "42", "quantity": 1,
	}).Code)

	env := doSupplyOrderRequest(t, r, "/get", map[string]any{"order_no": "MO-H3"})
	require.Equal(t, xianguanjia.SupplyCodeOK, env.Code)
	var data supplyOrderTestData
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Len(t, data.CardItems, 1)
	require.Equal(t, 20, data.OrderStatus)

	envMiss := doSupplyOrderRequest(t, r, "/get", map[string]any{"order_no": "MO-MISS"})
	require.Equal(t, xianguanjia.SupplyCodeOrderNotFound, envMiss.Code, "订单不存在须返回 1200")
}

func TestSupplyOrderHandlerRefundNotify(t *testing.T) {
	r, _ := newTestSupplyOrderHandler([]string{"card-1", "card-2"})
	require.Equal(t, xianguanjia.SupplyCodeOK, doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H4", "goods_no": "42", "quantity": 2,
	}).Code)

	env := doSupplyOrderRequest(t, r, "/refund", map[string]any{"order_no": "MO-H4"})
	require.Equal(t, xianguanjia.SupplyCodeOK, env.Code)
	var data supplyOrderTestData
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, "success", data.Result)

	// 退款后再查单：可见退款态。
	envGet := doSupplyOrderRequest(t, r, "/get", map[string]any{"order_no": "MO-H4"})
	require.Equal(t, xianguanjia.SupplyCodeOK, envGet.Code)
	var got supplyOrderTestData
	require.NoError(t, json.Unmarshal(envGet.Data, &got))
	require.True(t, got.Refunded)
	require.Equal(t, 30, got.OrderStatus)
}

func TestSupplyOrderHandlerMissingOrderNo(t *testing.T) {
	r, _ := newTestSupplyOrderHandler([]string{"card-1"})
	env := doSupplyOrderRequest(t, r, "/create", map[string]any{"goods_no": "42", "quantity": 1})
	require.Equal(t, xianguanjia.SupplyCodeOrderTimeout, env.Code, "缺参数创建须返回 1209")
}
