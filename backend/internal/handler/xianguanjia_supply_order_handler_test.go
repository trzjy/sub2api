package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service/xianguanjia"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// testSupplyGoodsSource 是 handler 测试用的内存 SupplyGoodsSource。
type testSupplyGoodsSource struct {
	byNo map[string]*xianguanjia.SupplyGoods
}

func (m *testSupplyGoodsSource) ListGoods(ctx context.Context, kw string, off, lim int) ([]xianguanjia.SupplyGoods, int, error) {
	var list []xianguanjia.SupplyGoods
	for _, g := range m.byNo {
		list = append(list, *g)
	}
	return list, len(list), nil
}

func (m *testSupplyGoodsSource) GetGoods(ctx context.Context, goodsNo string) (*xianguanjia.SupplyGoods, error) {
	if g, ok := m.byNo[goodsNo]; ok {
		return g, nil
	}
	return nil, nil
}

func newTestSupplyGoodsSource() *testSupplyGoodsSource {
	return &testSupplyGoodsSource{byNo: map[string]*xianguanjia.SupplyGoods{
		"42": {GoodsNo: "42", GoodsName: "测试商品", GoodsType: 2, Price: 990, Stock: 99, Status: 1},
	}}
}

// newTestSupplyOrderHandler 构造基于内存实现的订单 handler 与路由。
func newTestSupplyOrderHandler(cardNos []string, goods xianguanjia.SupplyGoodsSource) (*gin.Engine, *xianguanjia.SupplyOrderService, *xianguanjia.SupplyCardPoolMemory) {
	gin.SetMode(gin.TestMode)
	store := xianguanjia.NewSupplyOrderStoreMemory()
	pool := xianguanjia.NewSupplyCardPoolMemory(cardNos, nil)
	svc := xianguanjia.NewSupplyOrderService(store, pool, goods, xianguanjia.NewSupplyCardPwdResolverMemory(pool))
	h := NewXianguanjiaSupplyOrderHandler(svc)
	r := gin.New()
	r.POST("/create", h.CreateOrder)
	r.POST("/get", h.GetOrder)
	r.POST("/refund", h.RefundNotify)
	return r, svc, pool
}

type supplyOrderTestEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type supplyOrderTestData struct {
	OrderNo     string `json:"order_no"`
	OutOrderNo  string `json:"out_order_no"`
	OrderStatus int    `json:"order_status"`
	OrderAmount int64  `json:"order_amount"`
	GoodsName   string `json:"goods_name"`
	BuyQuantity int    `json:"buy_quantity"`
	OrderTime   int64  `json:"order_time"`
	EndTime     int64  `json:"end_time"`
	CardItems   []struct {
		CardNo  string `json:"card_no"`
		CardPwd string `json:"card_pwd"`
	} `json:"card_items"`
	Result     string `json:"result"`
	Remark     string `json:"remark"`
	RefundData *struct {
		ApplyTime    int64 `json:"apply_time"`
		RefundStatus int   `json:"refund_status"`
		RefundAmount int64 `json:"refund_amount"`
		RefundTime   int64 `json:"refund_time"`
	} `json:"refund_data"`
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

// assertTopLevelKeys 精确断言 data 顶层键集合（顺序无关）。
func assertTopLevelKeys(t *testing.T, raw json.RawMessage, want ...string) {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	require.Equal(t, wantSorted, got, "data 顶层键须精确匹配（无多余键）")
}

func TestSupplyOrderHandlerCreateAndDuplicate(t *testing.T) {
	r, _, _ := newTestSupplyOrderHandler([]string{"card-1", "card-2", "card-3"}, newTestSupplyGoodsSource())

	env := doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H1", "goods_no": "42", "buy_quantity": 2, "notify_url": "https://x/notify",
	})
	require.Equal(t, xianguanjia.SupplyCodeOK, env.Code)
	var data supplyOrderTestData
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, "MO-H1", data.OrderNo)
	require.NotEmpty(t, data.OutOrderNo)
	require.Equal(t, 20, data.OrderStatus)
	require.Equal(t, int64(990*2), data.OrderAmount)
	require.Equal(t, "测试商品", data.GoodsName)
	require.Equal(t, 2, data.BuyQuantity)
	require.NotZero(t, data.OrderTime)
	require.Len(t, data.CardItems, 2)
	// 官方合规：card_no 空、card_pwd 承载兑换码。
	for _, it := range data.CardItems {
		require.Empty(t, it.CardNo)
		require.NotEmpty(t, it.CardPwd)
	}

	// 重复订单号：幂等返回同一批卡，code=0（不返回 1203，不重发卡）。
	env2 := doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H1", "goods_no": "42", "buy_quantity": 2,
	})
	require.Equal(t, xianguanjia.SupplyCodeOK, env2.Code)
	var data2 supplyOrderTestData
	require.NoError(t, json.Unmarshal(env2.Data, &data2))
	require.Equal(t, data.CardItems, data2.CardItems)
}

func TestSupplyOrderHandlerInsufficientStock(t *testing.T) {
	r, _, _ := newTestSupplyOrderHandler([]string{"card-1"}, newTestSupplyGoodsSource())
	env := doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H2", "goods_no": "42", "buy_quantity": 5,
	})
	require.Equal(t, xianguanjia.SupplyCodeStockInsufficient, env.Code, "库存不足须返回 1102")
}

func TestSupplyOrderHandlerGetOrder(t *testing.T) {
	r, _, _ := newTestSupplyOrderHandler([]string{"card-1", "card-2"}, newTestSupplyGoodsSource())
	require.Equal(t, xianguanjia.SupplyCodeOK, doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H3", "goods_no": "42", "buy_quantity": 1,
	}).Code)

	env := doSupplyOrderRequest(t, r, "/get", map[string]any{"order_no": "MO-H3"})
	require.Equal(t, xianguanjia.SupplyCodeOK, env.Code)
	var data supplyOrderTestData
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Len(t, data.CardItems, 1)
	require.Equal(t, 20, data.OrderStatus)
	require.Equal(t, int64(990), data.OrderAmount)

	envMiss := doSupplyOrderRequest(t, r, "/get", map[string]any{"order_no": "MO-MISS"})
	require.Equal(t, xianguanjia.SupplyCodeOrderNotFound, envMiss.Code, "订单不存在须返回 1200")
}

// TestSupplyOrderHandlerGetOrderByOutOrderNo 验证以 out_order_no 查单命中。
func TestSupplyOrderHandlerGetOrderByOutOrderNo(t *testing.T) {
	r, _, _ := newTestSupplyOrderHandler([]string{"card-1", "card-2"}, newTestSupplyGoodsSource())
	create := doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H3B", "goods_no": "42", "buy_quantity": 1,
	})
	require.Equal(t, xianguanjia.SupplyCodeOK, create.Code)
	var cd supplyOrderTestData
	require.NoError(t, json.Unmarshal(create.Data, &cd))
	require.NotEmpty(t, cd.OutOrderNo)

	env := doSupplyOrderRequest(t, r, "/get", map[string]any{"out_order_no": cd.OutOrderNo})
	require.Equal(t, xianguanjia.SupplyCodeOK, env.Code)
	var data supplyOrderTestData
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, "MO-H3B", data.OrderNo)
	require.Equal(t, cd.OutOrderNo, data.OutOrderNo)
}

// TestSupplyOrderHandlerOrderNoPriorToOutOrderNo 验证 order_no 优先于 out_order_no。
func TestSupplyOrderHandlerOrderNoPriorToOutOrderNo(t *testing.T) {
	r, _, _ := newTestSupplyOrderHandler([]string{"card-1", "card-2"}, newTestSupplyGoodsSource())
	c1 := doSupplyOrderRequest(t, r, "/create", map[string]any{"order_no": "MO-P1", "goods_no": "42", "buy_quantity": 1})
	require.Equal(t, xianguanjia.SupplyCodeOK, c1.Code)
	var d1 supplyOrderTestData
	require.NoError(t, json.Unmarshal(c1.Data, &d1))
	c2 := doSupplyOrderRequest(t, r, "/create", map[string]any{"order_no": "MO-P2", "goods_no": "42", "buy_quantity": 1})
	require.Equal(t, xianguanjia.SupplyCodeOK, c2.Code)
	var d2 supplyOrderTestData
	require.NoError(t, json.Unmarshal(c2.Data, &d2))

	// 同时传 order_no=MO-P1 与 out_order_no=d2：应命中 MO-P1。
	env := doSupplyOrderRequest(t, r, "/get", map[string]any{"order_no": "MO-P1", "out_order_no": d2.OutOrderNo})
	require.Equal(t, xianguanjia.SupplyCodeOK, env.Code)
	var got supplyOrderTestData
	require.NoError(t, json.Unmarshal(env.Data, &got))
	require.Equal(t, "MO-P1", got.OrderNo)
	require.Equal(t, d1.OutOrderNo, got.OutOrderNo)
}

// TestSupplyOrderHandlerRefundAgree 验证退款申请 agree 分支响应 JSON 形状。
func TestSupplyOrderHandlerRefundAgree(t *testing.T) {
	r, _, _ := newTestSupplyOrderHandler([]string{"card-1", "card-2"}, newTestSupplyGoodsSource())
	require.Equal(t, xianguanjia.SupplyCodeOK, doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H4", "goods_no": "42", "buy_quantity": 2,
	}).Code)

	env := doSupplyOrderRequest(t, r, "/refund", map[string]any{"order_no": "MO-H4", "apply_time": 1700000000})
	require.Equal(t, xianguanjia.SupplyCodeOK, env.Code)
	var data supplyOrderTestData
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, "agree", data.Result)
	require.Empty(t, data.Remark, "agree 不应带 remark")
	require.NotNil(t, data.RefundData, "agree 须带 refund_data")
	require.Equal(t, int64(1700000000), data.RefundData.ApplyTime)
	require.Equal(t, 20, data.RefundData.RefundStatus)
	require.Equal(t, int64(990*2), data.RefundData.RefundAmount)
	require.NotZero(t, data.RefundData.RefundTime)
	// data 顶层仅允许 result/refund_data（agree 不带 remark）。
	assertTopLevelKeys(t, env.Data, "result", "refund_data")

	// 退款后再查单：对外订单态归一 20。
	envGet := doSupplyOrderRequest(t, r, "/get", map[string]any{"order_no": "MO-H4"})
	require.Equal(t, xianguanjia.SupplyCodeOK, envGet.Code)
	var got supplyOrderTestData
	require.NoError(t, json.Unmarshal(envGet.Data, &got))
	require.Equal(t, 20, got.OrderStatus)
}

// TestSupplyOrderHandlerRefundRefuse 验证退款申请 refuse 分支响应 JSON 形状。
func TestSupplyOrderHandlerRefundRefuse(t *testing.T) {
	r, _, pool := newTestSupplyOrderHandler([]string{"card-1", "card-2"}, newTestSupplyGoodsSource())
	require.Equal(t, xianguanjia.SupplyCodeOK, doSupplyOrderRequest(t, r, "/create", map[string]any{
		"order_no": "MO-H4R", "goods_no": "42", "buy_quantity": 2,
	}).Code)

	// 模拟卡已使用/已过期 → 本次作废 0 张 → refuse。
	pool.SetCardStatusForTest("card-1", "expired")
	pool.SetCardStatusForTest("card-2", "expired")

	env := doSupplyOrderRequest(t, r, "/refund", map[string]any{"order_no": "MO-H4R", "apply_time": 1700000000})
	require.Equal(t, xianguanjia.SupplyCodeOK, env.Code)
	var data supplyOrderTestData
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, "refuse", data.Result)
	require.Equal(t, "卡密已使用或已过期，无法撤单", data.Remark)
	require.Nil(t, data.RefundData, "refuse 不应带 refund_data")
	// data 顶层仅允许 result/remark（refuse 不带 refund_data）。
	assertTopLevelKeys(t, env.Data, "result", "remark")
}

// TestSupplyOrderHandlerMissingBuyQuantity 验证缺 buy_quantity → 1201。
func TestSupplyOrderHandlerMissingBuyQuantity(t *testing.T) {
	r, _, _ := newTestSupplyOrderHandler([]string{"card-1"}, newTestSupplyGoodsSource())

	env := doSupplyOrderRequest(t, r, "/create", map[string]any{"order_no": "MO-H5", "goods_no": "42"})
	require.Equal(t, xianguanjia.SupplyCodeOrderParamError, env.Code, "缺 buy_quantity 须返回 1201")

	env2 := doSupplyOrderRequest(t, r, "/create", map[string]any{"goods_no": "42", "buy_quantity": 1})
	require.Equal(t, xianguanjia.SupplyCodeOrderParamError, env2.Code, "缺 order_no 须返回 1201")

	env3 := doSupplyOrderRequest(t, r, "/create", map[string]any{"order_no": "MO-H5", "buy_quantity": 1})
	require.Equal(t, xianguanjia.SupplyCodeOrderParamError, env3.Code, "缺 goods_no 须返回 1201")
}
