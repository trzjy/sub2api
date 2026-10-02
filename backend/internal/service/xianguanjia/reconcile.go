package xianguanjia

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

const (
	defaultReconcileInterval = 30 * time.Minute
	defaultReconcileLimit    = 50
)

// ReconcileClient 对账所需的客户端方法集。*Client 满足该接口；
// 单测用内存桩实现同样满足，以便脱离真实网络测试对账逻辑。
type ReconcileClient interface {
	ListAftersaleOrders(ctx context.Context, since time.Time, limit int) ([]AftersaleOrder, error)
	GetAftersaleOrder(ctx context.Context, aftersaleID string) (*AftersaleOrder, error)
}

// ReconcileJob 闲管家对账任务骨架。
// 默认关闭（enabled=false），生产不会启动；开关/间隔走环境变量，接入既有任务框架是后续事项。
type ReconcileJob struct {
	client   ReconcileClient
	store    ExternalCardStore
	enabled  bool
	interval time.Duration
}

// NewReconcileJob 构造对账任务。从环境变量读取开关与间隔：
//   - XIANGUANJIA_RECONCILE_ENABLED 默认 "false"（关闭）；置 "true" 才启用。
//   - XIANGUANJIA_RECONCILE_INTERVAL 为 Go duration 字符串，解析失败回退 30m。
func NewReconcileJob(client *Client, store ExternalCardStore) *ReconcileJob {
	enabled := os.Getenv("XIANGUANJIA_RECONCILE_ENABLED") == "true"
	interval := defaultReconcileInterval
	if v := os.Getenv("XIANGUANJIA_RECONCILE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}
	return &ReconcileJob{client: client, store: store, enabled: enabled, interval: interval}
}

// Run 执行一次对账。默认关闭时直接返回 nil（什么都不做）。
func (j *ReconcileJob) Run(ctx context.Context) error {
	if j == nil || !j.enabled {
		return nil
	}
	if j.client == nil || j.store == nil {
		return fmt.Errorf("xianguanjia reconcile job not fully configured")
	}
	orders, err := j.client.ListAftersaleOrders(ctx, time.Time{}, defaultReconcileLimit)
	if err != nil {
		return fmt.Errorf("xianguanjia reconcile list aftersales: %w", err)
	}
	for _, o := range orders {
		// 把 aftersale 映射成 ExternalCardRow；卡密信息若列表里没有则留空。
		row := ExternalCardRow{OrderNo: o.OrderNo, SoldType: ""}
		if err := j.store.UpsertExternalCard(ctx, row); err != nil {
			// 记录失败留痕，不中断其余订单对账。
			slog.Warn("xianguanjia reconcile upsert failed", "order_no", o.OrderNo, "err", err)
		}
	}
	return nil
}

// Start 启动定时对账。默认关闭时直接 return（不启动 ticker）；否则 goroutine 内按 interval 调 Run。
func (j *ReconcileJob) Start(ctx context.Context) {
	if j == nil || !j.enabled {
		return
	}
	go func() {
		ticker := time.NewTicker(j.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := j.Run(ctx); err != nil {
					slog.Warn("xianguanjia reconcile run failed", "err", err)
				}
			}
		}
	}()
}
