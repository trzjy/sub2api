package xianguanjia

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// PushIdempotencyStore 推送幂等去重存储接口（对应 xianguanjia_push_receipts 表）。
// Record 返回 (true, nil) 表示首次记录（未重复），(false, nil) 表示已处理过（重复重试）。
type PushIdempotencyStore interface {
	Record(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error)
}

// ---- DB 实现 ----

type pushIdempotencyStoreDB struct {
	db *sql.DB
}

// NewPushIdempotencyStore 返回基于 PostgreSQL 的推送幂等存储。
func NewPushIdempotencyStore(db *sql.DB) *pushIdempotencyStoreDB {
	return &pushIdempotencyStoreDB{db: db}
}

func (s *pushIdempotencyStoreDB) Record(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("xianguanjia push idempotency store unavailable")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO xianguanjia_push_receipts (order_no, refund_status, order_status, modify_time, received_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (order_no, refund_status, order_status, modify_time) DO NOTHING`,
		orderNo, refundStatus, orderStatus, modifyTime)
	if err != nil {
		return false, fmt.Errorf("xianguanjia record push receipt: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("xianguanjia push receipt rows affected: %w", err)
	}
	return affected == 1, nil
}

// ---- 内存实现（单测 / handler 测试用） ----

type pushIdempotencyStoreMem struct {
	mu   sync.Mutex
	seen map[string]bool
}

// NewPushIdempotencyStoreMemory 返回内存版推送幂等存储。
func NewPushIdempotencyStoreMemory() *pushIdempotencyStoreMem {
	return &pushIdempotencyStoreMem{seen: make(map[string]bool)}
}

func idemKey(orderNo, refundStatus, orderStatus, modifyTime string) string {
	return orderNo + "|" + refundStatus + "|" + orderStatus + "|" + modifyTime
}

func (s *pushIdempotencyStoreMem) Record(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("xianguanjia push idempotency store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := idemKey(orderNo, refundStatus, orderStatus, modifyTime)
	if s.seen[k] {
		return false, nil
	}
	s.seen[k] = true
	return true, nil
}
