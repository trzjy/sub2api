package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// D6F-A：下单现场生成卡密，卡池初始为空，生成器按需插入 delivered 卡。
func newTestSupplyOrderHandler(cardNos []string, goods xianguanjia.SupplyGoodsSource) (*gin.Engine, *xianguanjia.SupplyOrderService, *xianguanjia.SupplyCardPoolMemory) {
	gin.SetMode(gin.TestMode)
	store := xianguanjia.NewSupplyOrderStoreMemory()
	pool := xianguanjia.NewSupplyCardPoolMemory(cardNos, nil)
	gen := xianguanjia.NewSupplyCardGeneratorMemory(pool, func(groupID int64) (int, error) { return 30, nil })
	// 接线：内存生成器接进 store 事务生命周期（CommitTx 落盘前落库、RollbackTx 丢弃缓冲）。
	store.WireCardGeneratorLifecycle(gen)
	svc := xianguanjia.NewSupplyOrderService(store, gen, pool, goods, xianguanjia.NewSupplyCardPwdResolverMemory(pool), nil, nil)
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

// fakeSupplyOrderService 实现 handler 侧 XianguanjiaSupplyOrderService 窄接口，
// 用于驱动退款信封/错误映射测试，与真实 service 解耦——退款行为（作废/追回/幂等）
// 覆盖已移交 xianguanjia 包单测 + integration 门禁（新退款路径走 ent 客户端，
// handler 测试无法以真实 service 驱动；D6G-B 派发单 §3）。
type fakeSupplyOrderService struct {
	refundOrder *xianguanjia.SupplyOrder
	refundErr   error
}

func (f *fakeSupplyOrderService) CreateOrder(ctx context.Context, managerOrderNo, goodsNo string, buyQuantity int, maxAmount int64) (*xianguanjia.SupplyOrder, error) {
	return nil, errors.New("fake: CreateOrder unused in refund envelope tests")
}

func (f *fakeSupplyOrderService) GetOrder(ctx context.Context, orderNo, outOrderNo string) (*xianguanjia.SupplyOrder, error) {
	return nil, errors.New("fake: GetOrder unused in refund envelope tests")
}

func (f *fakeSupplyOrderService) RefundNotify(ctx context.Context, managerOrderNo string) (*xianguanjia.SupplyOrder, error) {
	if f.refundErr != nil {
		return nil, f.refundErr
	}
	return f.refundOrder, nil
}

// newRefundHandlerWithFake 用给定服务构造仅含 /refund 路由的 gin 引擎。
func newRefundHandlerWithFake(svc XianguanjiaSupplyOrderService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewXianguanjiaSupplyOrderHandler(svc)
	r := gin.New()
	r.POST("/refund", h.RefundNotify)
	return r
}

// TestSupplyOrderHandlerRefundAgree 验证退款申请恒 agree 信封全字段（D6G：refuse 分支已删）。
// refund_time 取订单行 refunded_at 列（视图层 EndTime 承载，handler 不独立决定时间来源，R3 #1）。
func TestSupplyOrderHandlerRefundAgree(t *testing.T) {
	svc := &fakeSupplyOrderService{refundOrder: &xianguanjia.SupplyOrder{
		OrderAmount: 990 * 2,
		EndTime:     1700000999,
	}}
	r := newRefundHandlerWithFake(svc)

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
	require.Equal(t, int64(1700000999), data.RefundData.RefundTime, "refund_time 须等于订单行 refunded_at（EndTime 承载）")
	// data 顶层仅允许 result/refund_data（agree 不带 remark）。
	assertTopLevelKeys(t, env.Data, "result", "refund_data")
}

// TestSupplyOrderHandlerRefundError 验证 RefundNotify 注入错误 → 1209 归一信封（R8），
// 响应中不得出现 "agree"（错误路径不容许误发同意信封）。
func TestSupplyOrderHandlerRefundError(t *testing.T) {
	svc := &fakeSupplyOrderService{refundErr: errors.New("refund atomically failed")}
	r := newRefundHandlerWithFake(svc)

	var buf bytes.Buffer
	raw, err := json.Marshal(map[string]any{"order_no": "MO-ERR", "apply_time": 1700000000})
	require.NoError(t, err)
	buf.Write(raw)
	req := httptest.NewRequest(http.MethodPost, "/refund", &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "官方信封固定 HTTP 200")
	require.Contains(t, w.Body.String(), `"code":1209`, "注入错误须归一为 1209")
	require.NotContains(t, w.Body.String(), "agree", "错误信封不得出现 agree（R8 建议）")
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

// --- 退款通知（/goofish/order/refund/notify）接口测试：独立两字段信封，与 apply 不混用 ---

// supplyOrderRefundNotifyTestEnvelope 是 notify 接口的响应信封（严格两字段）。
type supplyOrderRefundNotifyTestEnvelope struct {
	Result string `json:"result"`
	Msg    string `json:"msg"`
}

// newRefundNotifyHandlerWithFake 用给定服务构造仅含 /refund/notify 路由的 gin 引擎。
func newRefundNotifyHandlerWithFake(svc XianguanjiaSupplyOrderService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewXianguanjiaSupplyOrderHandler(svc)
	r := gin.New()
	r.POST("/refund/notify", h.RefundResultNotify)
	return r
}

// doRefundNotifyRequest 发 notify 请求并断言：HTTP 恒 200、响应精确两字段
// {"result","msg"}（无多余键）。
func doRefundNotifyRequest(t *testing.T, r *gin.Engine, body any) supplyOrderRefundNotifyTestEnvelope {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		buf.Write(raw)
	}
	req := httptest.NewRequest(http.MethodPost, "/refund/notify", &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "退款通知固定 HTTP 200, body=%s", w.Body.String())
	assertTopLevelKeys(t, json.RawMessage(w.Body.Bytes()), "result", "msg")
	var env supplyOrderRefundNotifyTestEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	return env
}

// TestSupplyOrderHandlerRefundNotifySuccess 验证成功 → 200 + 严格
// {"result":"success","msg":"接收成功"}（恰好 2 键）。
func TestSupplyOrderHandlerRefundNotifySuccess(t *testing.T) {
	svc := &fakeSupplyOrderService{refundOrder: &xianguanjia.SupplyOrder{OrderAmount: 990}}
	r := newRefundNotifyHandlerWithFake(svc)

	env := doRefundNotifyRequest(t, r, map[string]any{
		"order_no": "MO-N1", "order_type": 1, "out_order_no": "OO1",
		"biz_order_no": "BO1", "refund_type": 2, "refund_amount": 990,
		"refund_reason": "r", "refund_scene": "s", "refund_time": 1700000000,
	})
	require.Equal(t, "success", env.Result)
	require.Equal(t, "接收成功", env.Msg)
}

// TestSupplyOrderHandlerRefundNotifyIdempotentReplay 验证已退款幂等回放 → 同 success 信封。
func TestSupplyOrderHandlerRefundNotifyIdempotentReplay(t *testing.T) {
	svc := &fakeSupplyOrderService{refundOrder: &xianguanjia.SupplyOrder{OrderAmount: 990 * 3, EndTime: 1700000999}}
	r := newRefundNotifyHandlerWithFake(svc)

	env := doRefundNotifyRequest(t, r, map[string]any{"order_no": "MO-N2"})
	require.Equal(t, "success", env.Result)
	require.Equal(t, "接收成功", env.Msg)
}

// TestSupplyOrderHandlerRefundNotifyMissingOrderNo 验证缺 order_no → fail + 指定 msg。
func TestSupplyOrderHandlerRefundNotifyMissingOrderNo(t *testing.T) {
	svc := &fakeSupplyOrderService{refundOrder: &xianguanjia.SupplyOrder{}}
	r := newRefundNotifyHandlerWithFake(svc)

	env := doRefundNotifyRequest(t, r, map[string]any{"out_order_no": "OO3"})
	require.Equal(t, "fail", env.Result)
	require.Equal(t, "order_no is required", env.Msg)
}

// TestSupplyOrderHandlerRefundNotifyOrderNotFound 验证 svc 返回 ErrSupplyOrderNotFound
// → fail + "order not found"。
func TestSupplyOrderHandlerRefundNotifyOrderNotFound(t *testing.T) {
	svc := &fakeSupplyOrderService{refundErr: xianguanjia.ErrSupplyOrderNotFound}
	r := newRefundNotifyHandlerWithFake(svc)

	env := doRefundNotifyRequest(t, r, map[string]any{"order_no": "MO-N4"})
	require.Equal(t, "fail", env.Result)
	require.Equal(t, "order not found", env.Msg)
}

// TestSupplyOrderHandlerRefundNotifyOtherError 验证 svc 返回其他错误 → fail + "系统异常"，
// 且响应体不得含内部错误细节串（精确两字段）。
func TestSupplyOrderHandlerRefundNotifyOtherError(t *testing.T) {
	svc := &fakeSupplyOrderService{refundErr: errors.New("refund atomically failed")}
	r := newRefundNotifyHandlerWithFake(svc)

	var buf bytes.Buffer
	raw, err := json.Marshal(map[string]any{"order_no": "MO-N5"})
	require.NoError(t, err)
	buf.Write(raw)
	req := httptest.NewRequest(http.MethodPost, "/refund/notify", &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "退款通知固定 HTTP 200")
	require.Contains(t, w.Body.String(), `"result":"fail"`)
	require.Contains(t, w.Body.String(), `"msg":"系统异常"`)
	assertTopLevelKeys(t, json.RawMessage(w.Body.Bytes()), "result", "msg")
	require.NotContains(t, w.Body.String(), "atomically failed", "错误细节不得回显")
}

// TestSupplyOrderHandlerRefundNotifyNilService 验证 svc 未接线 → fail + "系统异常"。
func TestSupplyOrderHandlerRefundNotifyNilService(t *testing.T) {
	h := &XianguanjiaSupplyOrderHandler{} // svc 为 nil
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/refund/notify", h.RefundResultNotify)

	env := doRefundNotifyRequest(t, r, map[string]any{"order_no": "MO-N6"})
	require.Equal(t, "fail", env.Result)
	require.Equal(t, "系统异常", env.Msg)
}
