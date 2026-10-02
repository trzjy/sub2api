package xianguanjia

import (
	"context"
	"testing"
	"time"
)

// fakeReconcileClient 是 ReconcileClient 的内存桩。
type fakeReconcileClient struct {
	orders    []AftersaleOrder
	detail    *AftersaleOrder
	listErr   error
	detailErr error
}

func (f *fakeReconcileClient) ListAftersaleOrders(ctx context.Context, since time.Time, limit int) ([]AftersaleOrder, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.orders, nil
}

func (f *fakeReconcileClient) GetAftersaleOrder(ctx context.Context, aftersaleID string) (*AftersaleOrder, error) {
	if f.detailErr != nil {
		return nil, f.detailErr
	}
	return f.detail, nil
}

func TestReconcileJobDisabled(t *testing.T) {
	// 默认关闭：Run 立即返回 nil，Start 不启动 ticker。
	j := &ReconcileJob{enabled: false}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("disabled Run should return nil, got %v", err)
	}
	j.Start(context.Background()) // 不应 panic，也不应启动 goroutine。
}

func TestReconcileJobEnabledWritesToStore(t *testing.T) {
	store := NewExternalCardStoreMemory()
	fc := &fakeReconcileClient{
		orders: []AftersaleOrder{
			{AftersaleID: "A1", OrderNo: "OA", Status: "refunding", RefundAmount: 5.5, Reason: "r"},
			{AftersaleID: "A2", OrderNo: "OB", Status: "refunded", RefundAmount: 1, Reason: "x"},
		},
	}
	j := &ReconcileJob{client: fc, store: store, enabled: true, interval: time.Minute}
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("enabled Run returned error: %v", err)
	}
	cards, err := store.GetExternalCards(context.Background(), "OA")
	if err != nil {
		t.Fatalf("GetExternalCards returned error: %v", err)
	}
	if len(cards) != 1 || cards[0].OrderNo != "OA" {
		t.Fatalf("expected 1 row for OA, got %+v", cards)
	}
	cardsB, err := store.GetExternalCards(context.Background(), "OB")
	if err != nil {
		t.Fatalf("GetExternalCards (OB) returned error: %v", err)
	}
	if len(cardsB) != 1 || cardsB[0].OrderNo != "OB" {
		t.Fatalf("expected 1 row for OB, got %+v", cardsB)
	}
}
