package xianguanjia

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestSupplyOrderService 构造内存版订单服务（含卡池与 card_pwd 解析器）。
func newTestSupplyOrderService(cardNos []string, pwds map[string]string) (*SupplyOrderService, *supplyOrderStoreMem, *supplyCardPoolMem) {
	store := NewSupplyOrderStoreMemory()
	pool := NewSupplyCardPoolMemory(cardNos, pwds)
	svc := NewSupplyOrderService(store, pool, NewSupplyCardPwdResolverMemory(pool))
	return svc, store, pool
}

func TestSupplyOrderCreateOrderSuccess(t *testing.T) {
	ctx := context.Background()
	svc, _, pool := newTestSupplyOrderService(
		[]string{"card-1", "card-2", "card-3"},
		map[string]string{"card-1": "pwd-1", "card-2": "pwd-2", "card-3": "pwd-3"},
	)

	order, err := svc.CreateOrder(ctx, "MO-1001", "42", 2)
	require.NoError(t, err)
	require.NotNil(t, order)
	require.Equal(t, "MO-1001", order.ManagerOrderNo)
	require.Equal(t, "42", order.GoodsNo)
	require.Equal(t, supplyOrderStatusSuccess, order.OrderStatus)
	require.False(t, order.Refunded)
	require.Len(t, order.CardItems, 2, "返回的 card_items 数量须等于下单数量")
	for _, it := range order.CardItems {
		require.NotEmpty(t, it.CardNo)
		require.NotEmpty(t, it.CardPwd)
	}
	require.Equal(t, 2, pool.CountByStatus("delivered"), "取卡后卡须为 delivered（已发货）")
	require.Equal(t, 1, pool.CountByStatus("unused"))
}

func TestSupplyOrderCreateOrderIdempotentNoReissue(t *testing.T) {
	ctx := context.Background()
	svc, _, pool := newTestSupplyOrderService(
		[]string{"card-1", "card-2", "card-3", "card-4"},
		nil,
	)

	first, err := svc.CreateOrder(ctx, "MO-1002", "42", 2)
	require.NoError(t, err)
	require.Len(t, first.CardItems, 2)
	deliveredAfterFirst := pool.CountByStatus("delivered")

	// 同订单号重发：必须返回同一批卡，且不新增取卡（资金红线）。
	second, err := svc.CreateOrder(ctx, "MO-1002", "42", 2)
	require.NoError(t, err)
	require.Equal(t, first.CardItems, second.CardItems, "重复订单号必须返回同一批卡")
	require.Equal(t, deliveredAfterFirst, pool.CountByStatus("delivered"), "重复订单号不得二次取卡")
	require.Equal(t, 2, pool.CountByStatus("delivered"))
}

func TestSupplyOrderCreateOrderInsufficientStock(t *testing.T) {
	ctx := context.Background()
	svc, store, pool := newTestSupplyOrderService([]string{"card-1"}, nil)

	_, err := svc.CreateOrder(ctx, "MO-1003", "42", 3)
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

func TestSupplyOrderGetOrder(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestSupplyOrderService([]string{"card-1", "card-2"}, nil)

	created, err := svc.CreateOrder(ctx, "MO-1004", "42", 1)
	require.NoError(t, err)

	got, err := svc.GetOrder(ctx, "MO-1004")
	require.NoError(t, err)
	require.Equal(t, created.CardItems, got.CardItems)
	require.Equal(t, supplyOrderStatusSuccess, got.OrderStatus)

	_, err = svc.GetOrder(ctx, "MO-UNKNOWN")
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeOrderNotFound, apiErr.Code, "订单不存在须返回 1200")
}

func TestSupplyOrderRefundNotifyVoidsCardsAndIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, _, pool := newTestSupplyOrderService([]string{"card-1", "card-2", "card-3"}, nil)

	created, err := svc.CreateOrder(ctx, "MO-1005", "42", 2)
	require.NoError(t, err)
	require.Equal(t, 2, pool.CountByStatus("delivered"))

	refunded, err := svc.RefundNotify(ctx, "MO-1005")
	require.NoError(t, err)
	require.True(t, refunded.Refunded)
	require.Equal(t, supplyOrderStatusRefunded, refunded.OrderStatus)
	require.NotNil(t, refunded.RefundedAt)
	require.Equal(t, 0, pool.CountByStatus("delivered"), "退款后卡不得再为 delivered")
	require.Equal(t, 2, pool.CountByStatus("expired"), "退款须把卡 delivered→expired")
	for _, it := range created.CardItems {
		require.Equal(t, "expired", pool.StatusOf(it.CardNo))
	}

	// 退款后再查单：可见退款态与已作废卡。
	got, err := svc.GetOrder(ctx, "MO-1005")
	require.NoError(t, err)
	require.True(t, got.Refunded)
	require.Equal(t, supplyOrderStatusRefunded, got.OrderStatus)
	require.Len(t, got.CardItems, 2)

	// 重复退款通知：幂等，不报错、不改变已作废状态。
	again, err := svc.RefundNotify(ctx, "MO-1005")
	require.NoError(t, err)
	require.True(t, again.Refunded)
	require.Equal(t, 2, pool.CountByStatus("expired"))
}

func TestSupplyOrderRefundNotifyUnknownOrder(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil)

	_, err := svc.RefundNotify(ctx, "MO-NOPE")
	require.Error(t, err)
	var apiErr *SupplyAPIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, SupplyCodeOrderNotFound, apiErr.Code)
}

func TestSupplyOrderCreateOrderInvalidParams(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestSupplyOrderService([]string{"card-1"}, nil)

	_, err := svc.CreateOrder(ctx, "", "42", 1)
	require.Error(t, err)
	_, err = svc.CreateOrder(ctx, "MO-X", "42", 0)
	require.Error(t, err)
}

// TestSupplyOrderCreateOrderConcurrentSameOrderNoOnce 验证并发同订单号仅发一次卡。
// 池中卡数远多于下单量，保证「若发生重复发卡则必然多消耗卡」可被观测。
func TestSupplyOrderCreateOrderConcurrentSameOrderNoOnce(t *testing.T) {
	ctx := context.Background()
	svc, _, pool := newTestSupplyOrderService(
		[]string{"card-1", "card-2", "card-3", "card-4", "card-5"},
		nil,
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
			order, err := svc.CreateOrder(ctx, "MO-CONCURRENT", "42", 1)
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
