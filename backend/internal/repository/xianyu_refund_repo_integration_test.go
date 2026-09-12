//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// fakeClawback 记录事务内追回调用，可配置失败用于验证整体回滚。
type fakeClawback struct {
	txCode *service.RedeemCode
	txErr  error
}

func (c *fakeClawback) ClawbackXianyuRedeemCodeTx(ctx context.Context, code *service.RedeemCode) (string, error) {
	c.txCode = code
	if c.txErr != nil {
		return "", c.txErr
	}
	return "追回余额 12.00", nil
}

func (c *fakeClawback) InvalidateAfterClawback(ctx context.Context, code *service.RedeemCode) {}

// insertRefundFixture 插入兑换码 + claim 行，返回 redeem_code id。
func insertRefundFixture(t *testing.T, ctx context.Context, orderNo, code, codeStatus string, usedBy *int64) int64 {
	t.Helper()
	db := integrationDB
	_, err := db.ExecContext(ctx, `DELETE FROM xianyu_order_claims WHERE order_no = $1`, orderNo)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE code = $1`, code)
	require.NoError(t, err)

	var usedByAny any
	if usedBy != nil {
		usedByAny = *usedBy
	}
	var codeID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO redeem_codes (code, type, value, status, notes, validity_days, used_by)
		VALUES ($1, 'balance', 12, $2, 'refund-test', 30, $3) RETURNING id`,
		code, codeStatus, usedByAny).Scan(&codeID))
	_, err = db.ExecContext(ctx, `
		INSERT INTO xianyu_order_claims
			(order_no, redeem_code_id, account_id, item_id, buyer_id, chat_id, delivery_status, attempt_count)
		VALUES ($1, $2, 'acc-a', 'item', 'buyer', '', 'sent', 0)`,
		orderNo, codeID)
	require.NoError(t, err)
	return codeID
}

func cleanupRefundFixture(t *testing.T, ctx context.Context, orderNo, code string) {
	t.Helper()
	_, err := integrationDB.ExecContext(ctx, `DELETE FROM xianyu_order_claims WHERE order_no = $1`, orderNo)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `DELETE FROM redeem_codes WHERE code = $1`, code)
	require.NoError(t, err)
}

func refundCodeStatus(t *testing.T, ctx context.Context, codeID int64) string {
	t.Helper()
	var status string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT status FROM redeem_codes WHERE id = $1`, codeID).Scan(&status))
	return status
}

func refundClaimState(t *testing.T, ctx context.Context, orderNo string) (bool, string) {
	t.Helper()
	var handled bool
	var action any
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT (refund_handled_at IS NOT NULL), COALESCE(refund_action,'')
		 FROM xianyu_order_claims WHERE order_no = $1`, orderNo).Scan(&handled, &action))
	a, _ := action.(string)
	return handled, a
}

// realClawbackService 构造仅启用余额追回原语的 RedeemService（userRepo 提供原子扣减）。
func realClawbackService() *service.RedeemService {
	userRepo := NewUserRepository(integrationEntClient, integrationDB)
	return service.NewRedeemService(nil, userRepo, nil, nil, nil, integrationEntClient, nil, nil)
}

func userBalance(t *testing.T, ctx context.Context, userID int64) float64 {
	t.Helper()
	var balance float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance))
	return balance
}

func TestXianyuRefundRepositoryRealBalanceClawbackInSingleTx(t *testing.T) {
	ctx := context.Background()
	const orderNo = "refund-order-real-clawback"
	const code = "XYREFUND000000000000000000005"
	var userID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, role, status, balance, concurrency)
		VALUES ('refund-clawback@xianyu-test.invalid', 'hash', 'user', 'active', 20, 1)
		RETURNING id`).Scan(&userID))
	codeID := insertRefundFixture(t, ctx, orderNo, code, "used", &userID)
	require.NotZero(t, codeID)
	t.Cleanup(func() {
		cleanupRefundFixture(t, ctx, orderNo, code)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	repo := NewXianyuRefundEventRepository(integrationEntClient).(*xianyuRefundEventRepository)
	outcome, err := repo.ProcessRefundAtomically(ctx, orderNo, "acc-a", realClawbackService())
	require.NoError(t, err)
	require.Equal(t, service.XianyuRefundActionClawedBack, outcome.Action)
	require.NotNil(t, outcome.Code)
	require.Equal(t, "追回余额 12.00", outcome.Detail)
	// 同一事务：余额扣减与 claim 终态一起提交
	require.InDelta(t, 8.0, userBalance(t, ctx, userID), 0.0001)
	handled, action := refundClaimState(t, ctx, orderNo)
	require.True(t, handled)
	require.Equal(t, "clawed_back", action)
}

func TestXianyuRefundRepositoryVoidDeliveredCodeOnRefund(t *testing.T) {
	ctx := context.Background()
	const orderNo = "refund-order-void"
	const code = "XYREFUND000000000000000000001"
	codeID := insertRefundFixture(t, ctx, orderNo, code, "delivered", nil)
	t.Cleanup(func() { cleanupRefundFixture(t, ctx, orderNo, code) })

	repo := NewXianyuRefundEventRepository(integrationEntClient).(*xianyuRefundEventRepository)

	outcome, err := repo.ProcessRefundAtomically(ctx, orderNo, "acc-a", &fakeClawback{})
	require.NoError(t, err)
	require.Equal(t, service.XianyuRefundActionVoided, outcome.Action)
	require.Nil(t, outcome.Code)
	require.Equal(t, "expired", refundCodeStatus(t, ctx, codeID))
	handled, action := refundClaimState(t, ctx, orderNo)
	require.True(t, handled)
	require.Equal(t, "voided", action)

	// 重复事件：幂等回放既往 action，不再处置
	outcome2, err := repo.ProcessRefundAtomically(ctx, orderNo, "acc-a", &fakeClawback{})
	require.NoError(t, err)
	require.Equal(t, service.XianyuRefundActionVoided, outcome2.Action)
}

func TestXianyuRefundRepositoryAccountMismatchRejected(t *testing.T) {
	ctx := context.Background()
	const orderNo = "refund-order-mismatch"
	const code = "XYREFUND000000000000000000003"
	codeID := insertRefundFixture(t, ctx, orderNo, code, "delivered", nil)
	t.Cleanup(func() { cleanupRefundFixture(t, ctx, orderNo, code) })

	repo := NewXianyuRefundEventRepository(integrationEntClient).(*xianyuRefundEventRepository)
	_, err := repo.ProcessRefundAtomically(ctx, orderNo, "acc-b", &fakeClawback{})
	require.ErrorIs(t, err, service.ErrXianyuRefundAccountMismatch)
	// 处置未发生：码仍是 delivered
	require.Equal(t, "delivered", refundCodeStatus(t, ctx, codeID))
}

func TestXianyuRefundRepositoryNoClaim(t *testing.T) {
	ctx := context.Background()
	repo := NewXianyuRefundEventRepository(integrationEntClient).(*xianyuRefundEventRepository)

	_, err := repo.ProcessRefundAtomically(ctx, "refund-order-missing", "acc-a", &fakeClawback{})
	require.ErrorIs(t, err, service.ErrXianyuRefundClaimNotFound)
}

func TestXianyuRefundRepositoryClawbackFailureRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	const orderNo = "refund-order-rollback"
	const code = "XYREFUND000000000000000000004"
	insertRefundFixture(t, ctx, orderNo, code, "delivered", nil)
	t.Cleanup(func() { cleanupRefundFixture(t, ctx, orderNo, code) })

	repo := NewXianyuRefundEventRepository(integrationEntClient).(*xianyuRefundEventRepository)

	// 用失败回调触发 used 分支回滚：先把码改为 used（模拟已兑换）
	_, err := integrationDB.ExecContext(ctx,
		`UPDATE redeem_codes SET status = 'used', used_by = NULL WHERE code = $1`, code)
	require.NoError(t, err)

	_, err = repo.ProcessRefundAtomically(ctx, orderNo, "acc-a", &fakeClawback{txErr: errors.New("deduct failed")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "clawback redeem code")
	// 整体回滚：claim 未处置（可重报），码保持 used
	handled, _ := refundClaimState(t, ctx, orderNo)
	require.False(t, handled)
	refundCode := refundCodeStatusByCode(t, ctx, code)
	require.Equal(t, "used", refundCode)

	// 重试成功路径（本次仍为 used 分支，回调成功）
	outcome, err := repo.ProcessRefundAtomically(ctx, orderNo, "acc-a", &fakeClawback{})
	require.NoError(t, err)
	require.Equal(t, service.XianyuRefundActionClawedBack, outcome.Action)
	handled, action := refundClaimState(t, ctx, orderNo)
	require.True(t, handled)
	require.Equal(t, "clawed_back", action)
}

func refundCodeStatusByCode(t *testing.T, ctx context.Context, code string) string {
	t.Helper()
	var status string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT status FROM redeem_codes WHERE code = $1`, code).Scan(&status))
	return status
}

// TestXianyuRefundSubscriptionClawbackLocksSubscriptionRow 验证订阅追回与并发续期互斥：
// 续期事务先持有订阅行锁，追回（reduceOrCancelSubscription）阻塞到锁释放，
// 基于续期后的新 ExpiresAt 计算扣减，续期天数不被覆盖丢失。
func TestXianyuRefundSubscriptionClawbackLocksSubscriptionRow(t *testing.T) {
	ctx := context.Background()
	var groupID, userID, subID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO groups (name, platform, rate_multiplier, status, subscription_type)
		VALUES ('refund-clawback-lock', 'anthropic', 1, 'active', 'standard') RETURNING id`).Scan(&groupID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, role, status, balance, concurrency)
		VALUES ('refund-sub-lock@xianyu-test.invalid', 'hash', 'user', 'active', 0, 1) RETURNING id`).Scan(&userID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO user_subscriptions (user_id, group_id, starts_at, expires_at, status, notes)
		VALUES ($1, $2, NOW() - interval '3 days', NOW() + interval '3 days', 'active', '')
		RETURNING id`, userID, groupID).Scan(&subID))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM user_subscriptions WHERE id = $1`, subID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
	})

	userRepo := NewUserRepository(integrationEntClient, integrationDB)
	subSvc := service.NewSubscriptionService(nil, NewUserSubscriptionRepository(integrationEntClient), nil, integrationEntClient, nil)
	clawSvc := service.NewRedeemService(nil, userRepo, subSvc, nil, nil, integrationEntClient, nil, nil)

	// 1) 模拟续期事务：先持有订阅行锁
	lockTx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	var lockHolder int64
	require.NoError(t, lockTx.QueryRowContext(ctx,
		`SELECT id FROM user_subscriptions WHERE id = $1 FOR UPDATE`, subID).Scan(&lockHolder))
	require.Equal(t, subID, lockHolder)

	// 2) 并发发起订阅追回（应阻塞在订阅行锁上）
	usedBy := userID
	groupIDArg := groupID
	code := &service.RedeemCode{
		ID: 0, Code: "XYREFUNDSUBLOCK1", Type: service.RedeemTypeSubscription,
		ValidityDays: 30, UsedBy: &usedBy, GroupID: &groupIDArg, Status: service.StatusUsed,
	}
	done := make(chan error, 1)
	var detail string
	go func() {
		d, err := clawSvc.ClawbackXianyuRedeemCodeTx(ctx, code)
		detail = d
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("clawback did not block on subscription row lock: err=%v", err)
	case <-time.After(300 * time.Millisecond):
		// 预期：仍在阻塞
	}

	// 3) 续期提交（+45 天）
	_, lockErr := lockTx.ExecContext(ctx,
		`UPDATE user_subscriptions SET expires_at = expires_at + interval '45 days' WHERE id = $1`, subID)
	require.NoError(t, lockErr)
	require.NoError(t, lockTx.Commit())

	// 4) 追回基于续期后的到期时间完成扣减
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("clawback did not complete after lock released")
	}
	require.Contains(t, detail, "追回订阅 30 天")

	var status string
	var expiresAt time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT status, expires_at FROM user_subscriptions WHERE id = $1`, subID).Scan(&status, &expiresAt))
	require.Equal(t, "active", status)
	// (now+3d) + 45d - 30d = now + 18d；若锁缺失会读到旧值并把订阅清零/取消
	expected := time.Now().Add(18 * 24 * time.Hour)
	require.WithinDuration(t, expected, expiresAt, 24*time.Hour)
}
