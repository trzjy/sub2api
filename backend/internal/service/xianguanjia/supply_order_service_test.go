package xianguanjia

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// testMemGoodsSource 是单测用的内存 SupplyGoodsSource（按 goods_no 聚合）。
type testMemGoodsSource struct {
	byNo map[string]*SupplyGoods
}

func (m *testMemGoodsSource) ListGoods(ctx context.Context, kw string, off, lim int) ([]SupplyGoods, int, error) {
	var list []SupplyGoods
	for _, g := range m.byNo {
		list = append(list, *g)
	}
	return list, len(list), nil
}

func (m *testMemGoodsSource) GetGoods(ctx context.Context, goodsNo string) (*SupplyGoods, error) {
	if g, ok := m.byNo[goodsNo]; ok {
		return g, nil
	}
	return nil, nil
}

// newTestGoodsSource 返回 goods_no="42"、单价 price 分、状态 status 的内存货源。
func newTestGoodsSource(price int64, status int) *testMemGoodsSource {
	return &testMemGoodsSource{byNo: map[string]*SupplyGoods{
		"42": {GoodsNo: "42", GoodsName: "测试商品", GoodsType: 2, Price: price, Stock: 99, Status: status},
	}}
}

// newTestSupplyOrderService 构造内存版订单服务（含卡池与 card_pwd 解析器）。
func newTestSupplyOrderService(cardNos []string, pwds map[string]string, goods SupplyGoodsSource) (*SupplyOrderService, *supplyOrderStoreMem, *SupplyCardPoolMemory) {
	store := NewSupplyOrderStoreMemory()
	pool := NewSupplyCardPoolMemory(cardNos, pwds)
	svc := NewSupplyOrderService(store, pool, goods, NewSupplyCardPwdResolverMemory(pool))
	return svc, store, pool
}

func TestSupplyOrderCreateOrderSuccess(t *testing.T) {
	ctx := context.Background()
	svc, _, pool := newTestSupplyOrderService(
		[]string{"card-1", "card-2", "card-3"},
		map[string]string{"card-1": "pwd-1", "card-2": "pwd-2", "card-3": "pwd-3"},
		newTestGoodsSource(990, 1),
	)

	order, err := svc.CreateOrder(ctx, "MO-1001", "42", 2, 0)
	require.NoError(t, err)
	require.NotNil(t, order)
	require.Equal(t, "MO-1001", order.OrderNo)
	require.Equal(t, "1", order.OutOrderNo, "OutOrderNo 为我方订单 id 十进制串")
	require.Equal(t, 20, order.OrderStatus)
	require.Equal(t, int64(990*2), order.OrderAmount, "金额 = 单价(分) × 数量")
	require.Equal(t, "测试商品", order.GoodsName)
	require.Equal(t, 2, order.BuyQuantity)
	require.NotZero(t, order.OrderTime)
	require.Equal(t, order.EndTime, order.OrderTime, "创建同步成功终态 end_time = order_time")
	require.Len(t, order.CardItems, 2, "返回的 card_items 数量须等于下单数量")
	// 官方合规：card_no 留空、card_pwd 承载兑换码（resolver 覆盖时取覆盖值）。
	for _, it := range order.CardItems {
		require.Empty(t, it.CardNo, "官方合规：card_no 须为空")
		require.NotEmpty(t, it.CardPwd, "card_pwd 须承载兑换码")
	}
	require.Equal(t, 2, pool.CountByStatus("delivered"), "取卡后卡须为 delivered（已发货）")
	require.Equal(t, 1, pool.CountByStatus("unused"))
}

func TestSupplyOrderCreateOrderIdempotentNoReissue(t *testing.T) {
	ctx := context.Background()
	svc, _, pool := newTestSupplyOrderService(
		[]string{"card-1", "card-2", "card-3", "card-4"},
		nil,
		newTestGoodsSource(990, 1),
	)

	first, err := svc.CreateOrder(ctx, "MO-1002", "42", 2, 0)
	require.NoError(t, err)
	require.Len(t, first.CardItems, 2)
	deliveredAfterFirst := pool.CountByStatus("delivered")

	// 同订单号重发：必须返回同一批卡，且不新增取卡（资金红线）。
	second, err := svc.CreateOrder(ctx, "MO-1002", "42", 2, 0)
	require.NoError(t, err)
	require.Equal(t, first.CardItems, second.CardItems, "重复订单号必须返回同一批卡")
	require.Equal(t, deliveredAfterFirst, pool.CountByStatus("delivered"), "重复订单号不得二次取卡")
	require.Equal(t, 2, pool.CountByStatus("delivered"))
}

func TestSupplyOrderCreateOrderInsufficientStock(t *testing.T) {
	ctx := context.Background()
	svc, store, pool := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, 1))

	_, err := svc.CreateOrder(ctx, "MO-1003", "42", 3, 0)
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeStockInsufficient, apiErr.Code, "库存不足须返回 1102")
	require.Equal(t, 1, pool.CountByStatus("unused"), "取卡不足须放回，卡仍为 unused")
	require.Equal(t, 0, pool.CountByStatus("delivered"))

	row, err := store.GetByManagerOrderNo(ctx, "MO-1003")
	require.NoError(t, err)
	require.Nil(t, row, "库存不足不得残留订单行")
}

func TestSupplyOrderCreateOrderGoodsNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, 1))

	_, err := svc.CreateOrder(ctx, "MO-1003B", "9999", 1, 0)
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeGoodsNotFound, apiErr.Code, "商品不存在须返回 1100")
}

func TestSupplyOrderCreateOrderGoodsUnavailable(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, SupplyGoodsStatusOffSale))

	_, err := svc.CreateOrder(ctx, "MO-1003C", "42", 1, 0)
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeGoodsUnavailable, apiErr.Code, "商品不可用(status=2)须返回 1101")
}

func TestSupplyOrderCreateOrderMaxAmountExceeded(t *testing.T) {
	ctx := context.Background()
	svc, store, pool := newTestSupplyOrderService([]string{"card-1", "card-2", "card-3", "card-4", "card-5", "card-6"}, nil, newTestGoodsSource(990, 1))

	// OrderAmount = 990*2 = 1980 分；max_amount=1000 < 1980 → 1202，且不得取卡。
	_, err := svc.CreateOrder(ctx, "MO-MAX", "42", 2, 1000)
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeOrderAmountBelowCost, apiErr.Code, "max_amount 超额须返回 1202")
	require.Equal(t, 6, pool.CountByStatus("unused"), "max_amount 超额不得取卡（卡全 unused）")
	require.Equal(t, 0, pool.CountByStatus("delivered"))

	row, err := store.GetByManagerOrderNo(ctx, "MO-MAX")
	require.NoError(t, err)
	require.Nil(t, row, "max_amount 超额不得残留订单行")

	// max_amount 不传(0) 或 >= 金额：正常下单。
	ok, err := svc.CreateOrder(ctx, "MO-MAX-OK", "42", 2, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1980), ok.OrderAmount)
	ok2, err := svc.CreateOrder(ctx, "MO-MAX-OK2", "42", 2, 1980)
	require.NoError(t, err)
	require.NotNil(t, ok2)
}

func TestSupplyOrderGetOrder(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestSupplyOrderService([]string{"card-1", "card-2"}, nil, newTestGoodsSource(990, 1))

	created, err := svc.CreateOrder(ctx, "MO-1004", "42", 1, 0)
	require.NoError(t, err)

	got, err := svc.GetOrder(ctx, "MO-1004", "")
	require.NoError(t, err)
	require.Equal(t, created.CardItems, got.CardItems)
	require.Equal(t, 20, got.OrderStatus)
	require.Equal(t, int64(990), got.OrderAmount)
	require.Equal(t, "1", got.OutOrderNo)

	_, err = svc.GetOrder(ctx, "MO-UNKNOWN", "")
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeOrderNotFound, apiErr.Code, "订单不存在须返回 1200")
}

func TestSupplyOrderGetOrderByOutOrderNo(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, 1))

	created, err := svc.CreateOrder(ctx, "MO-OUT", "42", 1, 0)
	require.NoError(t, err)
	require.NotEmpty(t, created.OutOrderNo, "创建须返回 out_order_no")

	// 按 out_order_no 查单命中。
	got, err := svc.GetOrder(ctx, "", created.OutOrderNo)
	require.NoError(t, err)
	require.Equal(t, created.OrderNo, got.OrderNo)
	require.Equal(t, created.OutOrderNo, got.OutOrderNo)
	require.Equal(t, created.CardItems, got.CardItems)

	// order_no 优先：传入 order_no 时忽略 out_order_no。
	got2, err := svc.GetOrder(ctx, created.OrderNo, "999999")
	require.NoError(t, err)
	require.Equal(t, created.OrderNo, got2.OrderNo)

	// out_order_no 非法：参数错误（非 1200）。
	_, err = svc.GetOrder(ctx, "", "not-a-number")
	require.Error(t, err)
}

func TestSupplyOrderRefundNotifyVoidsCardsAndIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, _, pool := newTestSupplyOrderService([]string{"card-1", "card-2", "card-3"}, nil, newTestGoodsSource(990, 1))

	created, err := svc.CreateOrder(ctx, "MO-1005", "42", 2, 0)
	require.NoError(t, err)
	require.Equal(t, 2, pool.CountByStatus("delivered"))

	// 第一次退款：可撤（voided>0）→ agree；对外订单态归一 20（非内部 30）。
	refunded, agree, err := svc.RefundNotify(ctx, "MO-1005")
	require.NoError(t, err)
	require.True(t, agree)
	require.Equal(t, 20, refunded.OrderStatus, "内部退款态 30 对外须归一为 20")
	require.Equal(t, int64(990*2), refunded.OrderAmount)
	require.Equal(t, 0, pool.CountByStatus("delivered"), "退款后卡不得再为 delivered")
	require.Equal(t, 2, pool.CountByStatus("expired"), "退款须把卡 delivered→expired")
	for _, it := range created.CardItems {
		require.Equal(t, "expired", pool.StatusOf(it.CardPwd), "卡片应已作废")
	}

	// 退款后再查单：可见对外 20 与已作废卡。
	got, err := svc.GetOrder(ctx, "MO-1005", "")
	require.NoError(t, err)
	require.Equal(t, 20, got.OrderStatus)
	require.Len(t, got.CardItems, 2)

	// 重复退款通知：幂等，仍 agree（已退款态），不改变已作废状态。
	again, agree2, err := svc.RefundNotify(ctx, "MO-1005")
	require.NoError(t, err)
	require.True(t, agree2, "重复退款通知须幂等 agree")
	require.Equal(t, 20, again.OrderStatus)
	require.Equal(t, 2, pool.CountByStatus("expired"))
}

func TestSupplyOrderRefundNotifyRefuseWhenCardsUsed(t *testing.T) {
	ctx := context.Background()
	svc, _, pool := newTestSupplyOrderService([]string{"card-1", "card-2"}, nil, newTestGoodsSource(990, 1))

	_, err := svc.CreateOrder(ctx, "MO-1005R", "42", 2, 0)
	require.NoError(t, err)

	// 模拟卡已使用/已过期：直接置池卡为 expired，使本次作废 0 张且未退款 → refuse。
	pool.SetCardStatusForTest("card-1", "expired")
	pool.SetCardStatusForTest("card-2", "expired")

	refunded, agree, err := svc.RefundNotify(ctx, "MO-1005R")
	require.NoError(t, err)
	require.False(t, agree, "卡已使用/已过期无法撤单 → refuse")
	require.NotNil(t, refunded)
	// refuse 不置退款态：对外仍 20（成功态，卡已被用）。
	require.Equal(t, 20, refunded.OrderStatus)

	// 查单回读仍非退款态（未置 refunded）。
	got, err := svc.GetOrder(ctx, "MO-1005R", "")
	require.NoError(t, err)
	require.Equal(t, 20, got.OrderStatus)
}

func TestSupplyOrderRefundNotifyUnknownOrder(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, 1))

	_, _, err := svc.RefundNotify(ctx, "MO-NOPE")
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeOrderNotFound, apiErr.Code)
}

func TestSupplyOrderCreateOrderInvalidParams(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, 1))

	_, err := svc.CreateOrder(ctx, "", "42", 1, 0)
	require.Error(t, err)
	_, err = svc.CreateOrder(ctx, "MO-X", "42", 0, 0)
	require.Error(t, err)
}

// TestSupplyOrderCardPwdDefaultMapping 验证默认官方合规映射：card_no=""、
// card_pwd=兑换码（redeem code 本身）；resolver 为 nil 时同样生效。
func TestSupplyOrderCardPwdDefaultMapping(t *testing.T) {
	ctx := context.Background()
	store := NewSupplyOrderStoreMemory()
	pool := NewSupplyCardPoolMemory([]string{"c1", "c2"}, nil)
	svc := NewSupplyOrderService(store, pool, newTestGoodsSource(500, 1), nil) // pwdResolve=nil

	order, err := svc.CreateOrder(ctx, "MO-PWD", "42", 2, 0)
	require.NoError(t, err)
	require.Len(t, order.CardItems, 2)
	for _, it := range order.CardItems {
		require.Empty(t, it.CardNo, "官方合规：card_no 须留空")
		require.NotEmpty(t, it.CardPwd, "card_pwd 须承载兑换码")
	}
	// 默认映射下 card_pwd 即 redeem code 本身。
	require.ElementsMatch(t, []string{"c1", "c2"}, []string{order.CardItems[0].CardPwd, order.CardItems[1].CardPwd})
}

// TestSupplyOrderConcurrentSameOrderNoOnce 验证并发同订单号仅发一次卡。
// 池中卡数远多于下单量，保证「若发生重复发卡则必然多消耗卡」可被观测。
func TestSupplyOrderConcurrentSameOrderNoOnce(t *testing.T) {
	ctx := context.Background()
	svc, _, pool := newTestSupplyOrderService(
		[]string{"card-1", "card-2", "card-3", "card-4", "card-5"},
		nil,
		newTestGoodsSource(990, 1),
	)

	const goroutines = 16
	var wg sync.WaitGroup
	results := make([]*SupplyOrder, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			order, err := svc.CreateOrder(ctx, "MO-CONCURRENT", "42", 1, 0)
			results[idx] = order
			errs[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	// 无论并发如何交错，池中只能有 1 张卡被发（delivered），绝不重复发卡。
	require.Equal(t, 1, pool.CountByStatus("delivered"),
		"并发同订单号必须只发一次卡")

	// 所有成功返回必须给出同一批卡（不得出现第二套卡）。
	var first []SupplyOrderCardItem
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			continue
		}
		require.NotNil(t, results[i])
		require.Len(t, results[i].CardItems, 1)
		if first == nil {
			first = results[i].CardItems
			continue
		}
		require.Equal(t, first, results[i].CardItems, "并发调用须返回同一批卡")
	}
	require.NotNil(t, first, "至少应有一个并发调用成功")
}
