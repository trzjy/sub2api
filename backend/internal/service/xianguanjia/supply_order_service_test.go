package xianguanjia

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

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

// OffSale 模拟商品下架（status=2），用于 #1 幂等回读前置回归。
func (m *testMemGoodsSource) OffSale(goodsNo string) {
	if g, ok := m.byNo[goodsNo]; ok {
		g.Status = SupplyGoodsStatusOffSale
	}
}

// ChangePrice 模拟改价，用于 #1/#2 金额快照回归。
func (m *testMemGoodsSource) ChangePrice(goodsNo string, price int64) {
	if g, ok := m.byNo[goodsNo]; ok {
		g.Price = price
	}
}

// Delete 模拟商品删除（查单/退款不应再读商品源），用于 #2 回归。
func (m *testMemGoodsSource) Delete(goodsNo string) {
	delete(m.byNo, goodsNo)
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

// TestSupplyOrderCreateOrderIdempotentAfterGoodsChange 回归 #1：商品下架/改价/更小
// max_amount 后同订单号重试，必须命中幂等回读前置、返回原订单原卡（完全不读商品源）。
func TestSupplyOrderCreateOrderIdempotentAfterGoodsChange(t *testing.T) {
	ctx := context.Background()
	goods := newTestGoodsSource(990, 1)
	svc, _, pool := newTestSupplyOrderService(
		[]string{"card-1", "card-2", "card-3"}, nil, goods)

	first, err := svc.CreateOrder(ctx, "MO-RETRY", "42", 2, 0)
	require.NoError(t, err)
	require.Len(t, first.CardItems, 2)
	deliveredAfterFirst := pool.CountByStatus("delivered")

	// 模拟商品下架 + 改价 + 更小 max_amount，重试应绕过商品/金额校验直接返回原订单。
	goods.OffSale("42")
	goods.ChangePrice("42", 1)

	// maxAmount=1 远小于快照金额 1980：旧逻辑会返回 1202；新逻辑（幂等前置）应跳过校验。
	second, err := svc.CreateOrder(ctx, "MO-RETRY", "42", 2, 1)
	require.NoError(t, err, "幂等回读前置：命中既有订单应直接返回，绕过商品/金额校验")
	require.Equal(t, first.CardItems, second.CardItems, "重试必须返回同一批卡")
	require.Equal(t, first.OrderAmount, second.OrderAmount, "金额须为下单快照，不受改价影响")
	require.Equal(t, first.GoodsName, second.GoodsName, "商品名须为快照")
	require.Equal(t, deliveredAfterFirst, pool.CountByStatus("delivered"), "重试不得二次取卡")
	require.Equal(t, 2, pool.CountByStatus("delivered"))

	// 商品删除场景下重试同样返回原订单原卡（绝不读商品源）。
	goods.Delete("42")
	deleted, err := svc.CreateOrder(ctx, "MO-RETRY", "42", 2, 0)
	require.NoError(t, err)
	require.Equal(t, first.CardItems, deleted.CardItems, "商品删除后重试仍须返回原卡")
	require.Equal(t, first.OrderAmount, deleted.OrderAmount)
}

// TestSupplyOrderGetOrderAmountIsSnapshot 回归 #2：下单后改价/删商品，查单金额须为
// 下单快照，不随商品源漂移（viewExisting 只读订单行快照，零 goods 源调用）。
func TestSupplyOrderGetOrderAmountIsSnapshot(t *testing.T) {
	ctx := context.Background()
	goods := newTestGoodsSource(990, 1)
	svc, _, _ := newTestSupplyOrderService([]string{"card-1", "card-2"}, nil, goods)

	created, err := svc.CreateOrder(ctx, "MO-SNAP", "42", 2, 0)
	require.NoError(t, err)
	require.Equal(t, int64(990*2), created.OrderAmount)

	// 改价 + 删除商品：查单金额/商品名须仍为下单快照。
	goods.ChangePrice("42", 1)
	goods.Delete("42")

	got, err := svc.GetOrder(ctx, "MO-SNAP", "")
	require.NoError(t, err)
	require.Equal(t, int64(990*2), got.OrderAmount, "查单金额须为下单快照，不漂移")
	require.Equal(t, "测试商品", got.GoodsName, "查单商品名须为快照")
}

// failMarkStore 是注入用的测试 store：可令 MarkRefundedTx 失败，验证退款原子化回滚。
type failMarkStore struct {
	*supplyOrderStoreMem
	failMark bool
}

func (s *failMarkStore) MarkRefundedTx(ctx context.Context, tx *sql.Tx, managerOrderNo string, at time.Time) (bool, error) {
	if s.failMark {
		return false, errors.New("injected mark failure")
	}
	return s.supplyOrderStoreMem.MarkRefundedTx(ctx, tx, managerOrderNo, at)
}

// TestSupplyOrderRefundRollbackOnMarkFailure 回归 #3：作废成功但置态失败时，整体回滚
// （卡保持 delivered、订单保持原态）；修复后重试 → agree 且卡已作废、金额=快照。
func TestSupplyOrderRefundRollbackOnMarkFailure(t *testing.T) {
	ctx := context.Background()
	pool := NewSupplyCardPoolMemory([]string{"card-1", "card-2"}, nil)
	baseStore := NewSupplyOrderStoreMemory()
	store := &failMarkStore{supplyOrderStoreMem: baseStore}
	svc := NewSupplyOrderService(store, pool, newTestGoodsSource(990, 1), NewSupplyCardPwdResolverMemory(pool))

	_, err := svc.CreateOrder(ctx, "MO-RB", "42", 2, 0)
	require.NoError(t, err)
	require.Equal(t, 2, pool.CountByStatus("delivered"))

	// 注入：作废成功但置态失败 → 整体回滚。
	store.failMark = true
	_, agree, err := svc.RefundNotify(ctx, "MO-RB")
	require.Error(t, err, "置态失败应返回错误，交由闲管家重试")
	require.False(t, agree)
	require.Equal(t, 2, pool.CountByStatus("delivered"), "回滚后卡须保持 delivered，不得为 expired")
	require.Equal(t, 0, pool.CountByStatus("expired"))
	row, err := store.GetByManagerOrderNo(ctx, "MO-RB")
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NotEqual(t, supplyOrderStatusRefunded, row.Status, "订单须保持原态")

	// 修复 store 再试 → agree 且卡已作废、金额=快照。
	store.failMark = false
	refunded, agree, err := svc.RefundNotify(ctx, "MO-RB")
	require.NoError(t, err)
	require.True(t, agree)
	require.Equal(t, 2, pool.CountByStatus("expired"), "重试后卡须作废")
	require.Equal(t, int64(990*2), refunded.OrderAmount, "退款金额须为快照")
}

// TestSupplyOrderRefundIdempotentAlreadyRefunded 回归 #3：已退款订单重复退款须幂等
// agree，且不再二次作废、refund_amount 取快照。
func TestSupplyOrderRefundIdempotentAlreadyRefunded(t *testing.T) {
	ctx := context.Background()
	svc, _, pool := newTestSupplyOrderService([]string{"card-1", "card-2"}, nil, newTestGoodsSource(990, 1))

	_, err := svc.CreateOrder(ctx, "MO-IDEM", "42", 2, 0)
	require.NoError(t, err)

	first, agree, err := svc.RefundNotify(ctx, "MO-IDEM")
	require.NoError(t, err)
	require.True(t, agree)
	require.Equal(t, int64(990*2), first.OrderAmount)

	// 重复退款：已退款 → 幂等 agree，不再作废（expired 计数不变），refund_amount=快照。
	again, agree2, err := svc.RefundNotify(ctx, "MO-IDEM")
	require.NoError(t, err)
	require.True(t, agree2)
	require.Equal(t, 2, pool.CountByStatus("expired"), "重复退款不得二次作废")
	require.Equal(t, int64(990*2), again.OrderAmount, "幂等退款金额须为快照")
}

// TestSupplyOrderRefundNotifyRefuseWhenPartialCardsUsed 验证修 1（P1）：多卡订单部分卡
// 已用/已过期时，voided(1..n-1) != len(cardNos) → 整体回滚 + refuse，不得按全额 agree。
func TestSupplyOrderRefundNotifyRefuseWhenPartialCardsUsed(t *testing.T) {
	ctx := context.Background()
	svc, store, pool := newTestSupplyOrderService([]string{"card-1", "card-2", "card-3"}, nil, newTestGoodsSource(990, 1))

	_, err := svc.CreateOrder(ctx, "MO-PARTIAL", "42", 3, 0)
	require.NoError(t, err)
	require.Equal(t, 3, pool.CountByStatus("delivered"))

	// 预置 1 卡已用（used）：本轮仅能作废 2 张，不得按全额 agree 置退款。
	pool.SetCardStatusForTest("card-1", "used")

	refunded, agree, err := svc.RefundNotify(ctx, "MO-PARTIAL")
	require.NoError(t, err)
	require.False(t, agree, "部分卡已用：不得按全额 agree → refuse")
	require.NotNil(t, refunded)
	require.Equal(t, 20, refunded.OrderStatus, "refuse 对外仍归一为 20（成功态）")

	// 已用卡保持 used；其余卡整体回滚为 delivered（无 expired 残留）。
	require.Equal(t, "used", pool.StatusOf("card-1"), "已用卡须保持 used")
	require.Equal(t, "delivered", pool.StatusOf("card-2"), "未用卡须回滚为 delivered")
	require.Equal(t, "delivered", pool.StatusOf("card-3"), "未用卡须回滚为 delivered")
	require.Equal(t, 0, pool.CountByStatus("expired"), "部分作废须整体回滚，无 expired 残留")

	// 订单非退款态。
	row, err := store.GetByManagerOrderNo(ctx, "MO-PARTIAL")
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NotEqual(t, supplyOrderStatusRefunded, row.Status, "订单须保持原态（非退款）")

	// 再次下单同订单号幂等不受影响（订单未被污染，仍返回原单原卡）。
	retry, err := svc.CreateOrder(ctx, "MO-PARTIAL", "42", 3, 0)
	require.NoError(t, err, "退款 refuse 不得破坏订单，重下单须幂等返回原单")
	require.Len(t, retry.CardItems, 3, "幂等重发须返回同一批卡")
}

// TestSupplyOrderRefundUndoVoidOnlyRestoresThisRound 验证修 3（P3）：UndoVoidTx 仅复位
// 本轮 VoidCardsForOrderTx 实际作废集合，不得误复活本轮之前已 expired 的卡。
//
// 场景：本订单卡 card-1 在本轮退款前已 expired（如上一轮部分作废留下的），其余卡正常。
// 本轮退款因部分卡已用/过期（voided=2 != 3）走 refuse 分支并整体回滚：UndoVoidTx 须只
// 把本轮已作废的 card-2/card-3 复位 delivered，card-1（本轮前已 expired，不在本集合内）
// 与无关过期卡 card-x 须保持 expired，不得误复活。
func TestSupplyOrderRefundUndoVoidOnlyRestoresThisRound(t *testing.T) {
	ctx := context.Background()
	pool := NewSupplyCardPoolMemory([]string{"card-1", "card-2", "card-3", "card-x"}, nil)
	baseStore := NewSupplyOrderStoreMemory()
	store := &failMarkStore{supplyOrderStoreMem: baseStore}
	svc := NewSupplyOrderService(store, pool, newTestGoodsSource(990, 1), NewSupplyCardPwdResolverMemory(pool))

	// 先下单：取卡 card-1/card-2/card-3（全部 delivered）。
	_, err := svc.CreateOrder(ctx, "MO-UNDO", "42", 3, 0)
	require.NoError(t, err)
	require.Equal(t, 3, pool.CountByStatus("delivered"), "下单后本订单 3 卡为 delivered")

	// 与本订单无关的过期卡（不在本订单 cardNos 中），回滚后须保持 expired。
	pool.SetCardStatusForTest("card-x", "expired")
	// 本订单的一张卡在本轮退款前已被作废（如上一轮部分作废留下的 expired），
	// 本轮回滚时不得误复活它。
	pool.SetCardStatusForTest("card-1", "expired")
	require.Equal(t, 2, pool.CountByStatus("delivered"), "card-1 预置 expired 后仅 2 张 delivered")

	// 本轮退款：card-2/card-3 正常作废（voided=2），但 card-1 已 expired 使
	// voided(2) != len(3) → refuse 分支整体回滚（UndoVoidTx 复位本轮集合）。
	_, agree, err := svc.RefundNotify(ctx, "MO-UNDO")
	require.NoError(t, err, "部分卡已用/过期走 refuse，不返回错误")
	require.False(t, agree, "部分作废须 refuse")
	// 本轮实际作废的卡（card-2/card-3）复位 delivered。
	require.Equal(t, "delivered", pool.StatusOf("card-2"))
	require.Equal(t, "delivered", pool.StatusOf("card-3"))
	// 本轮之前已 expired 的本订单卡（card-1）须保持 expired，不得被 UndoVoidTx 误复活。
	require.Equal(t, "expired", pool.StatusOf("card-1"), "本轮前已 expired 的本订单卡不得被误复活")
	// 无关 expired 卡仍 expired。
	require.Equal(t, "expired", pool.StatusOf("card-x"), "无关过期卡不得被误复活")
	require.Equal(t, 2, pool.CountByStatus("expired"), "仅 card-1/card-x 为 expired")
	// 订单非退款态。
	row, err := store.GetByManagerOrderNo(ctx, "MO-UNDO")
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NotEqual(t, supplyOrderStatusRefunded, row.Status, "订单须保持原态（非退款）")

	// 修复（card-1 复位 delivered 后再整体退款）→ agree，本轮卡全作废。
	pool.SetCardStatusForTest("card-1", "delivered")
	_, agree, err = svc.RefundNotify(ctx, "MO-UNDO")
	require.NoError(t, err)
	require.True(t, agree)
	require.Equal(t, "expired", pool.StatusOf("card-1"))
	require.Equal(t, "expired", pool.StatusOf("card-2"))
	require.Equal(t, "expired", pool.StatusOf("card-3"))
	require.Equal(t, "expired", pool.StatusOf("card-x"), "无关过期卡仍不受影响")
}
