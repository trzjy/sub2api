package xianguanjia

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
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

// defaultValidityResolver 是单测默认 validity_days 解析：固定返回 30 天（模拟 group 42
// 的订阅套餐有效天数）。无套餐分组场景由调用方注入返回 error 的 resolver 模拟。
func defaultValidityResolver(groupID int64) (int, error) {
	return 30, nil
}

// newTestSupplyOrderService 构造内存版订单服务（含生成器、卡池与 card_pwd 解析器）。
func newTestSupplyOrderService(cardNos []string, pwds map[string]string, goods SupplyGoodsSource) (*SupplyOrderService, *supplyOrderStoreMem, *supplyCardGeneratorMem, *SupplyCardPoolMemory) {
	return newTestSupplyOrderServiceWithResolver(cardNos, pwds, goods, defaultValidityResolver)
}

// newTestSupplyOrderServiceWithResolver 与上面相同，但可注入 validity 解析器（用于
// 「无套餐 fail-closed」等场景）。内存生成器遵守事务生命周期：构造后把 store 的
// commitHook/rollbackHook 接到 gen 的 CommitGenerated/RollbackGenerated（D6F-R 修复#2）。
func newTestSupplyOrderServiceWithResolver(cardNos []string, pwds map[string]string, goods SupplyGoodsSource, resolver func(int64) (int, error)) (*SupplyOrderService, *supplyOrderStoreMem, *supplyCardGeneratorMem, *SupplyCardPoolMemory) {
	store := NewSupplyOrderStoreMemory()
	pool := NewSupplyCardPoolMemory(cardNos, pwds)
	gen := NewSupplyCardGeneratorMemory(pool, resolver)
	// 接线：事务提交/回滚驱动生成器落库/丢弃，忠实模拟 DB 事务语义（单一惯用法）。
	store.WireCardGeneratorLifecycle(gen)
	svc := NewSupplyOrderService(store, gen, pool, goods, NewSupplyCardPwdResolverMemory(pool))
	return svc, store, gen, pool
}

func TestSupplyOrderCreateOrderSuccess(t *testing.T) {
	ctx := context.Background()
	svc, _, gen, pool := newTestSupplyOrderService(
		nil,
		nil,
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
	require.Equal(t, 2, pool.CountByStatus("delivered"), "生成后卡须为 delivered（已发货）")
	require.Equal(t, 0, pool.CountByStatus("unused"))

	// 生成器落库字段断言（type/status/group_id/validity_days/notes/code 格式）。
	recs := gen.GeneratedCards()
	require.Len(t, recs, 2, "生成器须记录 2 张卡")
	for _, r := range recs {
		require.Equal(t, domain.RedeemTypeSubscription, r.Type)
		require.Equal(t, domain.StatusDelivered, r.Status)
		require.Equal(t, int64(42), r.GroupID, "group_id 须为 goods_no 解析出的分组 ID")
		require.Equal(t, 30, r.ValidityDays, "validity_days 须来自 resolver")
		require.Equal(t, "xianguanjia supply order MO-1001", r.Note)
		require.Regexp(t, codeFormatRE, r.Code, "code 须为大写 XXXX-XXXX-XXXX-XXXX")
	}
}

// codeFormatRE 匹配父包 GenerateRandomRedeemCode 生成的码格式（大写 hex 四段）。
var codeFormatRE = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{8}-[0-9A-F]{8}-[0-9A-F]{8}$`)

func TestSupplyOrderCreateOrderIdempotentNoReissue(t *testing.T) {
	ctx := context.Background()
	svc, _, gen, pool := newTestSupplyOrderService(
		[]string{"card-1", "card-2", "card-3", "card-4"},
		nil,
		newTestGoodsSource(990, 1),
	)

	first, err := svc.CreateOrder(ctx, "MO-1002", "42", 2, 0)
	require.NoError(t, err)
	require.Len(t, first.CardItems, 2)
	deliveredAfterFirst := pool.CountByStatus("delivered")

	// 同订单号重发：必须返回同一批卡，且不新增生成（资金红线）。
	second, err := svc.CreateOrder(ctx, "MO-1002", "42", 2, 0)
	require.NoError(t, err)
	require.Equal(t, first.CardItems, second.CardItems, "重复订单号必须返回同一批卡")
	require.Equal(t, deliveredAfterFirst, pool.CountByStatus("delivered"), "重复订单号不得二次生成")
	require.Equal(t, 2, pool.CountByStatus("delivered"))
	require.Len(t, gen.GeneratedCards(), 2, "幂等重发不得二次生成卡")
}

func TestSupplyOrderCreateOrderGoodsNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, 1))

	_, err := svc.CreateOrder(ctx, "MO-1003B", "9999", 1, 0)
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeGoodsNotFound, apiErr.Code, "商品不存在须返回 1100")
}

func TestSupplyOrderCreateOrderGoodsUnavailable(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, SupplyGoodsStatusOffSale))

	_, err := svc.CreateOrder(ctx, "MO-1003C", "42", 1, 0)
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeGoodsUnavailable, apiErr.Code, "商品不可用(status=2)须返回 1101")
}

func TestSupplyOrderCreateOrderMaxAmountExceeded(t *testing.T) {
	ctx := context.Background()
	svc, store, _, pool := newTestSupplyOrderService(nil, nil, newTestGoodsSource(990, 1))

	// OrderAmount = 990*2 = 1980 分；max_amount=1000 < 1980 → 1202，且不得生成卡。
	_, err := svc.CreateOrder(ctx, "MO-MAX", "42", 2, 1000)
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeOrderAmountBelowCost, apiErr.Code, "max_amount 超额须返回 1202")
	require.Equal(t, 0, pool.CountByStatus("delivered"), "max_amount 超额不得生成卡")
	require.Equal(t, 0, pool.CountByStatus("unused"))

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
	svc, _, _, _ := newTestSupplyOrderService([]string{"card-1", "card-2"}, nil, newTestGoodsSource(990, 1))

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
	svc, _, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, 1))

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
	svc, _, gen, pool := newTestSupplyOrderService([]string{"card-1", "card-2", "card-3"}, nil, newTestGoodsSource(990, 1))

	created, err := svc.CreateOrder(ctx, "MO-1005", "42", 2, 0)
	require.NoError(t, err)
	require.Equal(t, 2, pool.CountByStatus("delivered"))
	require.Len(t, gen.GeneratedCards(), 2)

	// 第一次退款：可撤（voided>0）→ agree；对外订单态归一 20（非内部 30）。
	refunded, agree, err := svc.RefundNotify(ctx, "MO-1005")
	require.NoError(t, err)
	require.True(t, agree)
	require.Equal(t, 20, refunded.OrderStatus, "内部退款态 30 对外须归一为 20")
	require.Equal(t, int64(990*2), refunded.OrderAmount)
	require.Equal(t, 0, pool.CountByStatus("delivered"), "退款后卡不得再为 delivered")
	require.Equal(t, 2, pool.CountByStatus("expired"), "退款须把生成卡 delivered→expired")
	for _, it := range created.CardItems {
		require.Equal(t, "expired", pool.StatusOf(it.CardPwd), "生成卡应已作废")
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
	svc, _, _, pool := newTestSupplyOrderService([]string{"card-1", "card-2"}, nil, newTestGoodsSource(990, 1))

	created, err := svc.CreateOrder(ctx, "MO-1005R", "42", 2, 0)
	require.NoError(t, err)

	// 模拟生成卡已使用/已过期：直接置池卡为 expired，使本次作废 0 张且未退款 → refuse。
	require.Len(t, created.CardItems, 2)
	pool.SetCardStatusForTest(created.CardItems[0].CardPwd, "expired")
	pool.SetCardStatusForTest(created.CardItems[1].CardPwd, "expired")

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
	svc, _, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, 1))

	_, _, err := svc.RefundNotify(ctx, "MO-NOPE")
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeOrderNotFound, apiErr.Code)
}

func TestSupplyOrderCreateOrderInvalidParams(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil, newTestGoodsSource(990, 1))

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
	pool := NewSupplyCardPoolMemory(nil, nil)
	gen := NewSupplyCardGeneratorMemory(pool, defaultValidityResolver)
	// 接线事务生命周期钩子（单一惯用法）。
	store.WireCardGeneratorLifecycle(gen)
	svc := NewSupplyOrderService(store, gen, pool, newTestGoodsSource(500, 1), nil) // pwdResolve=nil

	order, err := svc.CreateOrder(ctx, "MO-PWD", "42", 2, 0)
	require.NoError(t, err)
	require.Len(t, order.CardItems, 2)
	for _, it := range order.CardItems {
		require.Empty(t, it.CardNo, "官方合规：card_no 须留空")
		require.NotEmpty(t, it.CardPwd, "card_pwd 须承载兑换码")
	}
	// 默认映射下 card_pwd 即 redeem code 本身。
	recs := gen.GeneratedCards()
	require.Len(t, recs, 2)
	for i, it := range order.CardItems {
		require.Equal(t, recs[i].Code, it.CardPwd)
	}
}

// TestSupplyOrderConcurrentSameOrderNoOnce 验证并发同订单号仅发一次卡。
// 内存生成器每次成功才落库，且内存 store 以 manager_order_no 唯一性仲裁，
// 保证「先到者生成、后到者命中幂等回读」，绝不重复发卡。
func TestSupplyOrderConcurrentSameOrderNoOnce(t *testing.T) {
	ctx := context.Background()
	svc, _, gen, pool := newTestSupplyOrderService(
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

	// 无论并发如何交错，内存池只能有 1 张卡被生成（delivered），绝不重复发卡。
	require.Equal(t, 1, pool.CountByStatus("delivered"),
		"并发同订单号必须只生成一次卡")
	require.Len(t, gen.GeneratedCards(), 1, "生成器仅落库 1 张卡")

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
	svc, _, gen, pool := newTestSupplyOrderService(
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
	require.Equal(t, deliveredAfterFirst, pool.CountByStatus("delivered"), "重试不得二次生成")
	require.Len(t, gen.GeneratedCards(), 2)

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
	svc, _, _, _ := newTestSupplyOrderService([]string{"card-1", "card-2"}, nil, goods)

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
	pool := NewSupplyCardPoolMemory(nil, nil)
	baseStore := NewSupplyOrderStoreMemory()
	store := &failMarkStore{supplyOrderStoreMem: baseStore}
	gen := NewSupplyCardGeneratorMemory(pool, defaultValidityResolver)
	// 接线事务生命周期钩子（单一惯用法）。
	store.WireCardGeneratorLifecycle(gen)
	svc := NewSupplyOrderService(store, gen, pool, newTestGoodsSource(990, 1), NewSupplyCardPwdResolverMemory(pool))

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
	svc, _, _, pool := newTestSupplyOrderService([]string{"card-1", "card-2"}, nil, newTestGoodsSource(990, 1))

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
	svc, store, _, pool := newTestSupplyOrderService([]string{"card-1", "card-2", "card-3"}, nil, newTestGoodsSource(990, 1))

	created, err := svc.CreateOrder(ctx, "MO-PARTIAL", "42", 3, 0)
	require.NoError(t, err)
	require.Equal(t, 3, pool.CountByStatus("delivered"))

	// 预置 1 张生成卡已用（used）：本轮仅能作废 2 张，不得按全额 agree 置退款。
	require.Len(t, created.CardItems, 3)
	pool.SetCardStatusForTest(created.CardItems[0].CardPwd, "used")

	refunded, agree, err := svc.RefundNotify(ctx, "MO-PARTIAL")
	require.NoError(t, err)
	require.False(t, agree, "部分卡已用：不得按全额 agree → refuse")
	require.NotNil(t, refunded)
	require.Equal(t, 20, refunded.OrderStatus, "refuse 对外仍归一为 20（成功态）")

	// 已用卡保持 used；其余卡整体回滚为 delivered（无 expired 残留）。
	require.Equal(t, "used", pool.StatusOf(created.CardItems[0].CardPwd), "已用卡须保持 used")
	require.Equal(t, "delivered", pool.StatusOf(created.CardItems[1].CardPwd), "未用卡须回滚为 delivered")
	require.Equal(t, "delivered", pool.StatusOf(created.CardItems[2].CardPwd), "未用卡须回滚为 delivered")
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
	pool := NewSupplyCardPoolMemory([]string{"card-x"}, nil) // 预置无关卡，用于验证 UndoVoidTx 不误复活
	baseStore := NewSupplyOrderStoreMemory()
	store := &failMarkStore{supplyOrderStoreMem: baseStore}
	gen := NewSupplyCardGeneratorMemory(pool, defaultValidityResolver)
	// 接线事务生命周期钩子（单一惯用法）。
	store.WireCardGeneratorLifecycle(gen)
	svc := NewSupplyOrderService(store, gen, pool, newTestGoodsSource(990, 1), NewSupplyCardPwdResolverMemory(pool))

	// 先下单：生成 3 张卡（全部 delivered）。
	created, err := svc.CreateOrder(ctx, "MO-UNDO", "42", 3, 0)
	require.NoError(t, err)
	require.Equal(t, 3, pool.CountByStatus("delivered"), "下单后本订单 3 卡为 delivered")
	require.Len(t, created.CardItems, 3)
	codes := []string{created.CardItems[0].CardPwd, created.CardItems[1].CardPwd, created.CardItems[2].CardPwd}

	// 与本订单无关的过期卡（不在本订单 cardNos 中），回滚后须保持 expired。
	pool.SetCardStatusForTest("card-x", "expired")
	// 本订单的一张卡在本轮退款前已被作废（如上一轮部分作废留下的 expired），
	// 本轮回滚时不得误复活它。
	pool.SetCardStatusForTest(codes[0], "expired")
	require.Equal(t, 2, pool.CountByStatus("delivered"), "card-1 预置 expired 后仅 2 张 delivered")

	// 本轮退款：codes[1]/codes[2] 正常作废（voided=2），但 codes[0] 已 expired 使
	// voided(2) != len(3) → refuse 分支整体回滚（UndoVoidTx 复位本轮集合）。
	_, agree, err := svc.RefundNotify(ctx, "MO-UNDO")
	require.NoError(t, err, "部分卡已用/过期走 refuse，不返回错误")
	require.False(t, agree, "部分作废须 refuse")
	// 本轮实际作废的卡（codes[1]/codes[2]）复位 delivered。
	require.Equal(t, "delivered", pool.StatusOf(codes[1]))
	require.Equal(t, "delivered", pool.StatusOf(codes[2]))
	// 本轮之前已 expired 的本订单卡（codes[0]）须保持 expired，不得被 UndoVoidTx 误复活。
	require.Equal(t, "expired", pool.StatusOf(codes[0]), "本轮前已 expired 的本订单卡不得被误复活")
	// 无关过期卡仍 expired。
	require.Equal(t, "expired", pool.StatusOf("card-x"), "无关过期卡不得被误复活")
	require.Equal(t, 2, pool.CountByStatus("expired"), "仅 card-1/card-x 为 expired")
	// 订单非退款态。
	row, err := store.GetByManagerOrderNo(ctx, "MO-UNDO")
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NotEqual(t, supplyOrderStatusRefunded, row.Status, "订单须保持原态（非退款）")

	// 修复（codes[0] 复位 delivered 后再整体退款）→ agree，本轮卡全作废。
	pool.SetCardStatusForTest(codes[0], "delivered")
	_, agree, err = svc.RefundNotify(ctx, "MO-UNDO")
	require.NoError(t, err)
	require.True(t, agree)
	require.Equal(t, "expired", pool.StatusOf(codes[0]))
	require.Equal(t, "expired", pool.StatusOf(codes[1]))
	require.Equal(t, "expired", pool.StatusOf(codes[2]))
	require.Equal(t, "expired", pool.StatusOf("card-x"), "无关过期卡仍不受影响")
}

// TestSupplyOrderGenerateCardsFields 断言现场生成卡的落库字段：
// type='subscription'、status='delivered'、group_id=分组、validity_days=换算天数、
// notes=留痕、code 格式正确；且订单 card_items 的 card_pwd 与生成码一致。
func TestSupplyOrderGenerateCardsFields(t *testing.T) {
	ctx := context.Background()
	svc, _, gen, _ := newTestSupplyOrderService(nil, nil, newTestGoodsSource(990, 1))

	order, err := svc.CreateOrder(ctx, "MO-GEN", "42", 3, 0)
	require.NoError(t, err)
	require.Len(t, order.CardItems, 3)

	recs := gen.GeneratedCards()
	require.Len(t, recs, 3)
	seen := map[string]bool{}
	for i, r := range recs {
		require.Equal(t, domain.RedeemTypeSubscription, r.Type, "type 须为 subscription")
		require.Equal(t, domain.StatusDelivered, r.Status, "status 须为 delivered")
		require.Equal(t, int64(42), r.GroupID, "group_id 须为 goods_no 解析的分组 ID")
		require.Equal(t, 30, r.ValidityDays, "validity_days 须来自父包换算（resolver）")
		require.Equal(t, "xianguanjia supply order MO-GEN", r.Note, "notes 须留痕订单号")
		require.Regexp(t, codeFormatRE, r.Code, "code 格式须为 XXXX-XXXX-XXXX-XXXX")
		require.False(t, seen[r.Code], "生成的码须唯一（无碰撞）")
		seen[r.Code] = true
		// 订单 card_pwd 须与生成码一致。
		require.Equal(t, r.Code, order.CardItems[i].CardPwd)
	}
}

// TestSupplyOrderCreateOrderDupConflictSameCards 验证：并发/重发的唯一键冲突路径
// （InsertCreating 命中 dup → 回滚生成 → 回读先到订单）返回同一批卡，且不二次生成。
type dupInsertStore struct {
	*supplyOrderStoreMem
	pre      *supplyOrderRow
	inserted int
}

func (s *dupInsertStore) GetByManagerOrderNo(ctx context.Context, managerOrderNo string) (*supplyOrderRow, error) {
	if s.inserted == 0 {
		// 首次回读（下单前幂等检查）：尚不存在。
		return nil, nil
	}
	// 冲突回滚后的回读：返回先到者的订单（携带先到批卡）。
	return cloneSupplyOrderRow(s.pre), nil
}

func (s *dupInsertStore) InsertCreating(ctx context.Context, tx *sql.Tx, row supplyOrderPlanned) (int64, error) {
	s.inserted++
	return 0, ErrSupplyOrderDup
}

func TestSupplyOrderCreateOrderDupConflictSameCards(t *testing.T) {
	ctx := context.Background()
	pool := NewSupplyCardPoolMemory(nil, nil)
	gen := NewSupplyCardGeneratorMemory(pool, defaultValidityResolver)
	store := &dupInsertStore{
		supplyOrderStoreMem: NewSupplyOrderStoreMemory(),
		pre: &supplyOrderRow{
			ID:             99,
			ManagerOrderNo: "MO-DUP",
			GoodsNo:        "42",
			Quantity:       2,
			Status:         supplyOrderStatusSuccess,
			CardNos:        []string{"preset-1", "preset-2"},
			CreatedAt:      time.Now(),
			GoodsName:      "测试商品",
			UnitPrice:      990,
			OrderAmount:    1980,
		},
	}
	// 接线事务生命周期钩子（单一惯用法）。本用例 InsertCreating 命中 dup，生成与
	// 提交均不发生，钩子仅为保持与正常路径一致的事务语义接线。
	store.WireCardGeneratorLifecycle(gen)
	svc := NewSupplyOrderService(store, gen, pool, newTestGoodsSource(990, 1), NewSupplyCardPwdResolverMemory(pool))

	order, err := svc.CreateOrder(ctx, "MO-DUP", "42", 2, 0)
	require.NoError(t, err, "Dup 冲突须回读先到订单而非报错")
	require.Equal(t, []string{"preset-1", "preset-2"}, []string{order.CardItems[0].CardPwd, order.CardItems[1].CardPwd},
		"Dup 冲突回读须返回先到同批卡")
	require.Empty(t, gen.GeneratedCards(), "dup 在生成前发生，生成器不得落库任何卡")
}

// TestSupplyOrderCreateOrderGenerateFailureRollback 验证：生成失败整体回滚——
// 不残留订单行、不残留已生成卡。
func TestSupplyOrderCreateOrderGenerateFailureRollback(t *testing.T) {
	ctx := context.Background()
	svc, store, gen, pool := newTestSupplyOrderService(nil, nil, newTestGoodsSource(990, 1))
	gen.failGen = true // 注入：生成第一张后整体失败

	_, err := svc.CreateOrder(ctx, "MO-FAIL", "42", 2, 0)
	require.Error(t, err, "生成失败须返回错误")

	row, err := store.GetByManagerOrderNo(ctx, "MO-FAIL")
	require.NoError(t, err)
	require.Nil(t, row, "生成失败整体回滚：不得残留订单行")
	require.Empty(t, gen.GeneratedCards(), "生成失败：不得落库任何卡")
	require.Equal(t, 0, pool.CountByStatus("delivered"), "生成失败：内存池不得登记卡")
}

// TestSupplyOrderCreateOrderNoPlanFailClosed 验证：无套餐分组（validity 解析失败）
// 整体 fail-closed——下单返回错误且不残留订单行、不生成卡。
func TestSupplyOrderCreateOrderNoPlanFailClosed(t *testing.T) {
	ctx := context.Background()
	// resolver 模拟「无套餐分组」：解析失败。
	failResolver := func(groupID int64) (int, error) {
		return 0, fmt.Errorf("no subscription plan for group %d", groupID)
	}
	svc, store, gen, pool := newTestSupplyOrderServiceWithResolver(nil, nil, newTestGoodsSource(990, 1), failResolver)

	_, err := svc.CreateOrder(ctx, "MO-NOPLAN", "42", 1, 0)
	require.Error(t, err, "无套餐分组须 fail-closed 返回错误")

	row, err := store.GetByManagerOrderNo(ctx, "MO-NOPLAN")
	require.NoError(t, err)
	require.Nil(t, row, "无套餐 fail-closed：不得残留订单行")
	require.Empty(t, gen.GeneratedCards(), "无套餐 fail-closed：不得生成卡")
	require.Equal(t, 0, pool.CountByStatus("delivered"))
}

// failSetCardsStore 是注入用的测试 store：SetCardsAndStatusTx 固定失败，验证
// 下单流程中「写入卡映射/最终状态」失败时整体回滚（订单不落盘、生成卡丢弃）。
type failSetCardsStore struct {
	*supplyOrderStoreMem
}

func (s *failSetCardsStore) SetCardsAndStatusTx(ctx context.Context, tx *sql.Tx, managerOrderNo string, cardNos []string, status int) error {
	return errors.New("injected set cards failure")
}

// TestSupplyOrderCreateOrderSetCardsFailureRollback 验证 修复#2：SetCardsAndStatusTx 失败时，
// 订单 pending 回滚且生成器未落库——GeneratedCards() 为空、内存池未登记、订单行不残留
// （模拟事务回滚丢弃生成卡）。
func TestSupplyOrderCreateOrderSetCardsFailureRollback(t *testing.T) {
	ctx := context.Background()
	pool := NewSupplyCardPoolMemory(nil, nil)
	baseStore := NewSupplyOrderStoreMemory()
	store := &failSetCardsStore{supplyOrderStoreMem: baseStore}
	gen := NewSupplyCardGeneratorMemory(pool, defaultValidityResolver)
	// 接线事务生命周期钩子（单一惯用法）：回滚时丢弃生成器 pending。
	store.WireCardGeneratorLifecycle(gen)
	svc := NewSupplyOrderService(store, gen, pool, newTestGoodsSource(990, 1), NewSupplyCardPwdResolverMemory(pool))

	_, err := svc.CreateOrder(ctx, "MO-SETFAIL", "42", 2, 0)
	require.Error(t, err, "SetCardsAndStatusTx 失败须返回错误")

	row, err := store.GetByManagerOrderNo(ctx, "MO-SETFAIL")
	require.NoError(t, err)
	require.Nil(t, row, "SetCards 失败整体回滚：不得残留订单行")
	require.Empty(t, gen.GeneratedCards(), "SetCards 失败：生成器不得落库任何卡（GeneratedCards 为空）")
	require.Equal(t, 0, pool.CountByStatus("delivered"), "SetCards 失败：内存池不得登记卡")
}

// TestSupplyOrderCreateOrderCommitFailureRollback 验证 修复#2：CommitTx 失败（commitHook 注入
// 错误，模拟 DB 提交失败）时，订单不落盘且生成器未落库——GeneratedCards() 为空、内存池未
// 登记、订单行不残留；RollbackTx 触发 rollbackHook 丢弃生成器 pending，事务语义一致。
// commitFailGenAdapter 是注入用的生成器替身：RollbackGenerated 继承生成器本体，
// CommitGenerated 被覆盖为始终返回注入错误，模拟 DB 提交失败（落盘中止）。用于把
// 提交失败路径收口到 WireCardGeneratorLifecycle（单一接线惯用法）。
type commitFailGenAdapter struct {
	*supplyCardGeneratorMem
}

func (a *commitFailGenAdapter) CommitGenerated(tx *sql.Tx) error {
	return errors.New("injected commit failure")
}

func TestSupplyOrderCreateOrderCommitFailureRollback(t *testing.T) {
	ctx := context.Background()
	pool := NewSupplyCardPoolMemory(nil, nil)
	baseStore := NewSupplyOrderStoreMemory()
	store := baseStore
	gen := NewSupplyCardGeneratorMemory(pool, defaultValidityResolver)
	// 注入 commitHook 错误，模拟 DB 提交失败（落盘中止）。注意：此路径下
	// gen.CommitGenerated 不被调用，生成器 pending 仅由 rollbackHook 清理。
	// 经 adapter 收口到 WireCardGeneratorLifecycle（RollbackGenerated 继承生成器本体）。
	store.WireCardGeneratorLifecycle(&commitFailGenAdapter{supplyCardGeneratorMem: gen})
	svc := NewSupplyOrderService(store, gen, pool, newTestGoodsSource(990, 1), NewSupplyCardPwdResolverMemory(pool))

	_, err := svc.CreateOrder(ctx, "MO-COMMITFAIL", "42", 2, 0)
	require.Error(t, err, "CommitTx 失败须返回错误")

	row, err := store.GetByManagerOrderNo(ctx, "MO-COMMITFAIL")
	require.NoError(t, err)
	require.Nil(t, row, "CommitTx 失败：订单不得落盘")
	require.Empty(t, gen.GeneratedCards(), "CommitTx 失败：生成器不得落库任何卡（GeneratedCards 为空）")
	require.Equal(t, 0, pool.CountByStatus("delivered"), "CommitTx 失败：内存池不得登记卡")
}

// TestSupplyOrderGeneratedCardsReflectsCommitOnly 验证 修复#2：正常路径下 GeneratedCards()
// 含全部生成卡且 pool.cards 同步（落库），而 GenerateCardsTx 本身不直接触碰 g.generated/pool.cards。
func TestSupplyOrderGeneratedCardsReflectsCommitOnly(t *testing.T) {
	ctx := context.Background()
	svc, _, gen, pool := newTestSupplyOrderService(nil, nil, newTestGoodsSource(990, 1))

	order, err := svc.CreateOrder(ctx, "MO-COMMIT-OK", "42", 3, 0)
	require.NoError(t, err)
	require.Len(t, order.CardItems, 3)

	recs := gen.GeneratedCards()
	require.Len(t, recs, 3, "正常路径：GeneratedCards 须含全部生成卡")
	require.Equal(t, 3, pool.CountByStatus("delivered"), "正常路径：pool.cards 须同步全部 delivered")
}
