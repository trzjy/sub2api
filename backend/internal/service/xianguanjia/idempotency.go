package xianguanjia

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// 推送处理语义（本批资金安全核心，派发单 D2 / 外审 F1）：
//
// 幂等 = 「处理成功才去重」。回执（xianguanjia_push_receipts）只在业务动作
// （作废等）成功后落库；失败时返回 result=fail 且不落回执，闲管家按官方契约
// 重试（失败最多重试 3 次），重推时因无回执会重新执行作废，杜绝
// 「回执已落库 → 重推被去重 → 作废永久丢失」的资金损失链。
//
// 并发安全：同一 (order_no, refund_status, order_status, modify_time) 的并发
// 推送靠主键冲突去重（INSERT ... ON CONFLICT DO NOTHING 影响行数=0 即重复），
// 处理动作本身由调用方在上层做（本存储只管回执的「提交」这一步）。

// PushIdempotencyStore 推送幂等回执存储（xianguanjia_push_receipts 表）。
type PushIdempotencyStore interface {
	// CommitReceipt 在处理成功后落回执。返回 false 表示该回执已存在
	// （并发重复推送中他人已提交，本次处理成果与其等价，按幂等成功消费）。
	CommitReceipt(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error)

	// HasReceipt 查询回执是否已存在（重推去重依据）。查重失败返回 error，
	// 调用方须按 fail 处理（宁可重做不可漏做——重做的业务动作自身幂等）。
	HasReceipt(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error)
}

// ---- DB 实现 ----

type pushIdempotencyStoreDB struct {
	db *sql.DB
}

// NewPushIdempotencyStore 返回基于 PostgreSQL 的推送幂等存储。
func NewPushIdempotencyStore(db *sql.DB) *pushIdempotencyStoreDB {
	return &pushIdempotencyStoreDB{db: db}
}

func (s *pushIdempotencyStoreDB) CommitReceipt(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error) {
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

func (s *pushIdempotencyStoreDB) HasReceipt(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("xianguanjia push idempotency store unavailable")
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM xianguanjia_push_receipts
			WHERE order_no = $1 AND refund_status = $2 AND order_status = $3 AND modify_time = $4
		)`, orderNo, refundStatus, orderStatus, modifyTime).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("xianguanjia query push receipt: %w", err)
	}
	return exists, nil
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

func (s *pushIdempotencyStoreMem) CommitReceipt(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error) {
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

func (s *pushIdempotencyStoreMem) HasReceipt(ctx context.Context, orderNo, refundStatus, orderStatus, modifyTime string) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("xianguanjia push idempotency store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[idemKey(orderNo, refundStatus, orderStatus, modifyTime)], nil
}
