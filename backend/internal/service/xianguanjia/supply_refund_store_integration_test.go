//go:build integration

package xianguanjia

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/lib/pq"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// integration harness：复用 repository 集成测试同款 Docker Postgres + 迁移 + ent client。
// 与 repository 包的 integration_harness_test.go 同源但本包独立（不同 package，无法直接复用
// 其未导出变量）；用 sync.Once 惰性初始化，缺 Docker 时测试 Skip，不阻塞编译/无 Docker 环境。
var (
	integrationDB        *sql.DB
	integrationEntClient *dbent.Client
	integrationOnce      sync.Once
	integrationErr       error
)

func ensureIntegrationHarness(t *testing.T) {
	t.Helper()
	integrationOnce.Do(func() { integrationErr = setupIntegrationHarness() })
	if integrationErr != nil {
		if !dockerAvailable(context.Background()) {
			t.Skipf("docker unavailable, integration harness skipped")
		}
		t.Fatalf("integration harness setup failed: %v", integrationErr)
	}
}

func setupIntegrationHarness() error {
	ctx := context.Background()
	if !dockerAvailable(ctx) {
		return fmt.Errorf("docker is not available")
	}
	pg, err := tcpostgres.Run(ctx, "postgres:18.1-alpine3.23",
		tcpostgres.WithDatabase("sub2api_test"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies())
	if err != nil {
		return fmt.Errorf("start postgres: %w", err)
	}
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	if err != nil {
		return fmt.Errorf("dsn: %w", err)
	}
	db, err := openSQLWithRetry(ctx, dsn, 30*time.Second)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}

	// 引导缺失基表 xianyu_xianguanjia_config（仅存在于 integration 测试域）：
	// 1) 仓库迁移链缺 252（backend/migrations/252_xianyu_xianguanjia.sql 曾被历史
	//    revert d216b55ba 删除，生产表仍在）。本引导使 scratch 库可走完 268+ 迁移
	//    链（268 会对该表追加 supply_app_id / supply_app_secret_encrypted 两列，
	//    故此处不预置这两列）。252 是否恢复入链属用户裁定事项，不在本卡范围。
	// 2) 引导 DDL 须与生产表保持一致（生产已验证）。若后续恢复 252 迁移文件入链，
	//    本引导应删除；当前 CREATE TABLE IF NOT EXISTS 幂等，恢复前共存亦无害。
	// 3) 268 迁移会在其后追加 supply_app_id / supply_app_secret_encrypted 两列，
	//    故此处不预置这两列，避免与 268 迁移冲突。
	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS xianyu_xianguanjia_config (
    id                   BIGSERIAL PRIMARY KEY,
    base_url             VARCHAR(255)  NOT NULL DEFAULT 'https://open.goofish.pro',
    app_id               VARCHAR(120)  NOT NULL DEFAULT '',
    app_secret_encrypted TEXT          NOT NULL DEFAULT '',
    mch_id               VARCHAR(120)  NOT NULL DEFAULT '',
    mch_secret_encrypted TEXT          NOT NULL DEFAULT '',
    push_url             VARCHAR(512)  NOT NULL DEFAULT '',
    status               VARCHAR(16)   NOT NULL DEFAULT 'disabled',
    health_status        VARCHAR(16)   NOT NULL DEFAULT 'unknown',
    last_checked_at      TIMESTAMPTZ,
    created_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW()
)`); err != nil {
		return fmt.Errorf("bootstrap xianyu_xianguanjia_config: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE UNIQUE INDEX IF NOT EXISTS uq_xianyu_xianguanjia_config_active
    ON xianyu_xianguanjia_config (status)
    WHERE status = 'active'`); err != nil {
		return fmt.Errorf("bootstrap xianyu_xianguanjia_config index: %w", err)
	}

	if err := repository.ApplyMigrations(ctx, db); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	drv := entsql.OpenDB(dialect.Postgres, db)
	integrationDB = db
	integrationEntClient = dbent.NewClient(dbent.Driver(drv))
	return nil
}

func dockerAvailable(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Env = os.Environ()
	return cmd.Run() == nil
}

func openSQLWithRetry(ctx context.Context, dsn string, timeout time.Duration) (*sql.DB, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if err := db.PingContext(ctx); err != nil {
			lastErr = err
			_ = db.Close()
			time.Sleep(250 * time.Millisecond)
			continue
		}
		return db, nil
	}
	return nil, fmt.Errorf("db not ready after %s: %w", timeout, lastErr)
}

// ---- 真实 clawback（订阅扣天/取消，与闲鱼链同款 RedeemService）----

func realSupplyClawback() service.XianyuRedeemClawback {
	userRepo := repository.NewUserRepository(integrationEntClient, integrationDB)
	subSvc := service.NewSubscriptionService(nil, repository.NewUserSubscriptionRepository(integrationEntClient), nil, integrationEntClient, nil)
	return service.NewRedeemService(nil, userRepo, subSvc, nil, nil, integrationEntClient, nil, nil, nil)
}

// ---- fixtures ----

// insertSupplyOrderFixture 插入一笔货源订单（含 card_nos JSON），返回其主键 id。
func insertSupplyOrderFixture(t *testing.T, ctx context.Context, mgrNo string, cardNos []string, status int) int64 {
	t.Helper()
	raw, err := json.Marshal(cardNos)
	require.NoError(t, err)
	var id int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO xianguanjia_supply_orders
			(manager_order_no, goods_no, quantity, status, card_nos, goods_name, unit_price, order_amount, created_at)
		VALUES ($1, '42', $2, $3, $4, '测试商品', 990, $5, NOW()) RETURNING id`,
		mgrNo, len(cardNos), status, string(raw), int64(990*len(cardNos))).Scan(&id))
	return id
}

// insertRedeemCodeFixture 插入一张兑换码（与买家兑换/作废同表），返回 id。
func insertRedeemCodeFixture(t *testing.T, ctx context.Context, code, status string, groupID *int64, usedBy *int64, value float64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO redeem_codes (code, type, status, group_id, used_by, value, validity_days, notes)
		VALUES ($1, 'subscription', $2, $3, $4, $5, 30, 'supply-refund-it') RETURNING id`,
		code, status, groupID, usedBy, value).Scan(&id))
	return id
}

func cleanupSupplyOrderFixture(t *testing.T, ctx context.Context, mgrNo string, codes []string) {
	t.Helper()
	_, _ = integrationDB.ExecContext(ctx, `DELETE FROM xianguanjia_supply_orders WHERE manager_order_no = $1`, mgrNo)
	if len(codes) > 0 {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM redeem_codes WHERE code = ANY($1)`, pq.Array(codes))
	}
}

func supplyOrderStatus(t *testing.T, ctx context.Context, mgrNo string) int {
	t.Helper()
	var status int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT status FROM xianguanjia_supply_orders WHERE manager_order_no = $1`, mgrNo).Scan(&status))
	return status
}

func supplyOrderRefundedAt(t *testing.T, ctx context.Context, mgrNo string) *time.Time {
	t.Helper()
	var refunded sql.NullTime
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT refunded_at FROM xianguanjia_supply_orders WHERE manager_order_no = $1`, mgrNo).Scan(&refunded))
	if refunded.Valid {
		v := refunded.Time
		return &v
	}
	return nil
}

func redeemCodeStatusByCode(t *testing.T, ctx context.Context, code string) string {
	t.Helper()
	var status string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT status FROM redeem_codes WHERE code = $1`, code).Scan(&status))
	return status
}

func supplyRefundStoreUnderTest() SupplyRefundStore {
	return NewSupplyRefundStore(integrationEntClient, realSupplyClawback())
}

// ---- ① 未兑换作废 ----

func TestSupplyRefundIntegrationVoidUndeliveredCodes(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-void"
	codes := []string{"IT-SUPPLY-VOID-1", "IT-SUPPLY-VOID-2"}
	insertSupplyOrderFixture(t, ctx, mgrNo, codes, 20)
	insertRedeemCodeFixture(t, ctx, codes[0], "delivered", nil, nil, 0)
	insertRedeemCodeFixture(t, ctx, codes[1], "unused", nil, nil, 0)
	t.Cleanup(func() { cleanupSupplyOrderFixture(t, ctx, mgrNo, codes) })

	store := supplyRefundStoreUnderTest()
	rec, err := store.ProcessRefundAtomically(ctx, mgrNo)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Empty(t, rec.Clawed, "未兑换码无 clawed")
	require.Equal(t, "expired", redeemCodeStatusByCode(t, ctx, codes[0]))
	require.Equal(t, "expired", redeemCodeStatusByCode(t, ctx, codes[1]))
	require.Equal(t, 30, supplyOrderStatus(t, ctx, mgrNo))
	require.NotNil(t, supplyOrderRefundedAt(t, ctx, mgrNo), "须写入 refunded_at")
}

// ---- ② 已兑换追回（订阅扣天 / 剩余不足取消 / 订阅已不存在视为追回完成）----

func TestSupplyRefundIntegrationClawbackUsedCode(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-claw"
	code := "IT-SUPPLY-CLAW-1"
	var groupID, userID, subID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO groups (name, platform, rate_multiplier, status, subscription_type)
		VALUES ('supply-refund-it', 'anthropic', 1, 'active', 'standard') RETURNING id`).Scan(&groupID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, role, status, balance, concurrency)
		VALUES ('supply-refund-it@invalid', 'hash', 'user', 'active', 0, 1) RETURNING id`).Scan(&userID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO user_subscriptions (user_id, group_id, starts_at, expires_at, status, notes)
		VALUES ($1, $2, NOW() - interval '3 days', NOW() + interval '45 days', 'active', '') RETURNING id`,
		userID, groupID).Scan(&subID))
	// 该用户须存在一笔钱包/订阅相关记录；此处仅依赖 subscription 扣减路径。
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM user_subscriptions WHERE id = $1`, subID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
		cleanupSupplyOrderFixture(t, ctx, mgrNo, []string{code})
	})

	insertSupplyOrderFixture(t, ctx, mgrNo, []string{code}, 20)
	insertRedeemCodeFixture(t, ctx, code, "used", &groupID, &userID, 0)

	store := supplyRefundStoreUnderTest()
	rec, err := store.ProcessRefundAtomically(ctx, mgrNo)
	require.NoError(t, err)
	require.Len(t, rec.Clawed, 1, "used 码须走 clawback 并计入 Clawed")
	require.Equal(t, code, rec.Clawed[0].Code)
	require.Equal(t, 30, supplyOrderStatus(t, ctx, mgrNo), "订单须置退款态")

	// 订阅须被扣减（约 -30 天）：原 expires_at(+45d) 扣 30d → 约 now+15d（扣减路径：剩余 > 追回天数）。
	var expiresAt time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT expires_at FROM user_subscriptions WHERE id = $1`, subID).Scan(&expiresAt))
	expected := time.Now().Add(15 * 24 * time.Hour)
	require.WithinDuration(t, expected, expiresAt, 24*time.Hour)
}

// 剩余天数 ≤ 追回天数 → 取消订阅（status='expired' + expires_at=now），
// 锁定实现 reduceOrCancelSubscription 语义。
func TestSupplyRefundIntegrationClawbackCancelsShortSubscription(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-claw-cancel"
	code := "IT-SUPPLY-CLAW-CANCEL-1"
	var groupID, userID, subID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO groups (name, platform, rate_multiplier, status, subscription_type)
		VALUES ('supply-refund-it-cancel', 'anthropic', 1, 'active', 'standard') RETURNING id`).Scan(&groupID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, role, status, balance, concurrency)
		VALUES ('supply-refund-it-cancel@invalid', 'hash', 'user', 'active', 0, 1) RETURNING id`).Scan(&userID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO user_subscriptions (user_id, group_id, starts_at, expires_at, status, notes)
		VALUES ($1, $2, NOW() - interval '3 days', NOW() + interval '10 days', 'active', '') RETURNING id`,
		userID, groupID).Scan(&subID))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM user_subscriptions WHERE id = $1`, subID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
		cleanupSupplyOrderFixture(t, ctx, mgrNo, []string{code})
	})

	insertSupplyOrderFixture(t, ctx, mgrNo, []string{code}, 20)
	insertRedeemCodeFixture(t, ctx, code, "used", &groupID, &userID, 0)

	store := supplyRefundStoreUnderTest()
	rec, err := store.ProcessRefundAtomically(ctx, mgrNo)
	require.NoError(t, err)
	require.Len(t, rec.Clawed, 1, "used 码须走 clawback 并计入 Clawed")
	require.Equal(t, 30, supplyOrderStatus(t, ctx, mgrNo), "订单须置退款态")

	// 取消路径：剩余天数 ≤ 追回天数 → 订阅 status='expired'，expires_at 被置 now。
	var status string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT status FROM user_subscriptions WHERE id = $1`, subID).Scan(&status))
	require.Equal(t, "expired", status)
	var expiresAt time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT expires_at FROM user_subscriptions WHERE id = $1`, subID).Scan(&expiresAt))
	require.WithinDuration(t, time.Now(), expiresAt, 24*time.Hour)
}

// 订阅已不存在视为追回完成：used 码 GroupID 有效但无订阅行 → 仍整体成功提交。
func TestSupplyRefundIntegrationClawbackSubscriptionMissing(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-claw-missing"
	code := "IT-SUPPLY-CLAW-MISS-1"
	var groupID, userID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO groups (name, platform, rate_multiplier, status, subscription_type)
		VALUES ('supply-refund-it-miss', 'anthropic', 1, 'active', 'standard') RETURNING id`).Scan(&groupID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, role, status, balance, concurrency)
		VALUES ('supply-refund-it-miss@invalid', 'hash', 'user', 'active', 0, 1) RETURNING id`).Scan(&userID))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
		cleanupSupplyOrderFixture(t, ctx, mgrNo, []string{code})
	})

	insertSupplyOrderFixture(t, ctx, mgrNo, []string{code}, 20)
	insertRedeemCodeFixture(t, ctx, code, "used", &groupID, &userID, 0)

	store := supplyRefundStoreUnderTest()
	rec, err := store.ProcessRefundAtomically(ctx, mgrNo)
	require.NoError(t, err, "订阅已不存在须视为追回完成，整体成功")
	require.Len(t, rec.Clawed, 1)
	require.Equal(t, 30, supplyOrderStatus(t, ctx, mgrNo))
}

// ---- ③ 混合状态（delivered/unused/used/expired/disabled 混合）----

func TestSupplyRefundIntegrationMixedStatus(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-mixed"
	codes := []string{"IT-MIX-DELIVERED", "IT-MIX-UNUSED", "IT-MIX-USED", "IT-MIX-EXPIRED", "IT-MIX-DISABLED"}
	var groupID, userID, subID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO groups (name, platform, rate_multiplier, status, subscription_type)
		VALUES ('supply-refund-it-mix', 'anthropic', 1, 'active', 'standard') RETURNING id`).Scan(&groupID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, role, status, balance, concurrency)
		VALUES ('supply-refund-it-mix@invalid', 'hash', 'user', 'active', 0, 1) RETURNING id`).Scan(&userID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO user_subscriptions (user_id, group_id, starts_at, expires_at, status, notes)
		VALUES ($1, $2, NOW() - interval '3 days', NOW() + interval '10 days', 'active', '') RETURNING id`,
		userID, groupID).Scan(&subID))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM user_subscriptions WHERE id = $1`, subID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
		cleanupSupplyOrderFixture(t, ctx, mgrNo, codes)
	})

	insertSupplyOrderFixture(t, ctx, mgrNo, codes, 20)
	insertRedeemCodeFixture(t, ctx, codes[0], "delivered", nil, nil, 0)
	insertRedeemCodeFixture(t, ctx, codes[1], "unused", nil, nil, 0)
	insertRedeemCodeFixture(t, ctx, codes[2], "used", &groupID, &userID, 0)
	insertRedeemCodeFixture(t, ctx, codes[3], "expired", nil, nil, 0)
	insertRedeemCodeFixture(t, ctx, codes[4], "disabled", nil, nil, 0)

	store := supplyRefundStoreUnderTest()
	rec, err := store.ProcessRefundAtomically(ctx, mgrNo)
	require.NoError(t, err)
	require.Len(t, rec.Clawed, 1, "仅 used 码计入 Clawed")
	// delivered/unused → expired；expired/disabled → 无操作；used → clawback。
	require.Equal(t, "expired", redeemCodeStatusByCode(t, ctx, codes[0]))
	require.Equal(t, "expired", redeemCodeStatusByCode(t, ctx, codes[1]))
	require.Equal(t, "used", redeemCodeStatusByCode(t, ctx, codes[2]), "used 码追回后保持 used（不再改状态）")
	require.Equal(t, "expired", redeemCodeStatusByCode(t, ctx, codes[3]), "expired 保持终态")
	require.Equal(t, "disabled", redeemCodeStatusByCode(t, ctx, codes[4]), "disabled 保持终态")
	require.Equal(t, 30, supplyOrderStatus(t, ctx, mgrNo))
}

// ---- ④ 缺码回滚 / 重复码回滚 / 集合不等价失败关闭（R1 must_fix）----

func TestSupplyRefundIntegrationCodeSetMismatchFailsClosed(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	// 订单 card_nos 含一张库里不存在的码 → 集合不等价 → 失败关闭，订单/码零残留。
	const mgrNo = "it-supply-mismatch"
	codes := []string{"IT-OK-1", "IT-MISSING-1"}
	insertSupplyOrderFixture(t, ctx, mgrNo, codes, 20)
	insertRedeemCodeFixture(t, ctx, codes[0], "delivered", nil, nil, 0)
	t.Cleanup(func() { cleanupSupplyOrderFixture(t, ctx, mgrNo, codes) })

	store := supplyRefundStoreUnderTest()
	_, err := store.ProcessRefundAtomically(ctx, mgrNo)
	require.Error(t, err, "缺码须失败关闭（集合不等价）")
	// 回滚：订单保持原态、已存在的码不被作废。
	require.Equal(t, 20, supplyOrderStatus(t, ctx, mgrNo), "订单须保持原态")
	require.Equal(t, "delivered", redeemCodeStatusByCode(t, ctx, codes[0]), "缺码失败须零残留，已存在码不被作废")

	// 重复码：card_nos 含重复 → len 不等价 → 失败关闭。
	const mgrNoDup = "it-supply-dup"
	dupCodes := []string{"IT-DUP-1", "IT-DUP-1"}
	insertSupplyOrderFixture(t, ctx, mgrNoDup, dupCodes, 20)
	insertRedeemCodeFixture(t, ctx, "IT-DUP-1", "delivered", nil, nil, 0)
	t.Cleanup(func() { cleanupSupplyOrderFixture(t, ctx, mgrNoDup, []string{"IT-DUP-1"}) })
	_, err = store.ProcessRefundAtomically(ctx, mgrNoDup)
	require.Error(t, err, "重复码须失败关闭（集合不等价）")
	require.Equal(t, 20, supplyOrderStatus(t, ctx, mgrNoDup))
	require.Equal(t, "delivered", redeemCodeStatusByCode(t, ctx, "IT-DUP-1"))
}

// ---- ⑤ expired/disabled 无操作、未知状态失败关闭、前序码已处置后遇追回失败 → 全回滚 ----

func TestSupplyRefundIntegrationUnknownStatusFailsClosed(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-unknown"
	codes := []string{"IT-UNKNOWN-1"}
	insertSupplyOrderFixture(t, ctx, mgrNo, codes, 20)
	insertRedeemCodeFixture(t, ctx, codes[0], "weird_unknown_status", nil, nil, 0)
	t.Cleanup(func() { cleanupSupplyOrderFixture(t, ctx, mgrNo, codes) })

	store := supplyRefundStoreUnderTest()
	_, err := store.ProcessRefundAtomically(ctx, mgrNo)
	require.Error(t, err, "未知状态须失败关闭")
	require.Equal(t, 20, supplyOrderStatus(t, ctx, mgrNo), "未知状态失败须零残留")
	require.Equal(t, "weird_unknown_status", redeemCodeStatusByCode(t, ctx, codes[0]))
}

// 前序码已处置（delivered→expired 成功）后遇 used 追回失败 → 订单/全部码/订阅均回滚保持原状。
func TestSupplyRefundIntegrationClawbackFailureRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-clawfail"
	codes := []string{"IT-CLAWFAIL-DELIVERED", "IT-CLAWFAIL-USED"}
	var groupID, userID, subID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO groups (name, platform, rate_multiplier, status, subscription_type)
		VALUES ('supply-refund-it-clawfail', 'anthropic', 1, 'active', 'standard') RETURNING id`).Scan(&groupID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, role, status, balance, concurrency)
		VALUES ('supply-refund-it-clawfail@invalid', 'hash', 'user', 'active', 0, 1) RETURNING id`).Scan(&userID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO user_subscriptions (user_id, group_id, starts_at, expires_at, status, notes)
		VALUES ($1, $2, NOW() - interval '3 days', NOW() + interval '10 days', 'active', '') RETURNING id`,
		userID, groupID).Scan(&subID))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM user_subscriptions WHERE id = $1`, subID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
		cleanupSupplyOrderFixture(t, ctx, mgrNo, codes)
	})

	insertSupplyOrderFixture(t, ctx, mgrNo, codes, 20)
	insertRedeemCodeFixture(t, ctx, codes[0], "delivered", nil, nil, 0)
	// used 码但 userID 指向一个不存在的订阅（clawback 会真实扣减失败？此处模拟：用真实 clawback，
	// 订阅存在则扣减成功；为触发失败，构造一个使 reduceOrCancelSubscription 失败的场景较困难，
	// 故本用例改为：used 码指向一个被并发删除的订阅——通过失败注入 clawback 验证回滚。
	insertRedeemCodeFixture(t, ctx, codes[1], "used", &groupID, &userID, 0)

	// 注入失败 clawback（与单元测试不同，这里用真实 store + 失败桩验证整体回滚）。
	failClawback := &failClawbackTx{err: errors.New("injected clawback failure")}
	failStore := NewSupplyRefundStore(integrationEntClient, failClawback)
	_, err := failStore.ProcessRefundAtomically(ctx, mgrNo)
	require.Error(t, err, "追回失败须整体回滚")
	// 回滚：订单原态、delivered 码保持 delivered（前序作废被回滚）、订阅 unchanged。
	require.Equal(t, 20, supplyOrderStatus(t, ctx, mgrNo), "订单须保持原态")
	require.Equal(t, "delivered", redeemCodeStatusByCode(t, ctx, codes[0]), "前序作废须回滚")
	require.Equal(t, "used", redeemCodeStatusByCode(t, ctx, codes[1]))
	require.True(t, failClawback.called, "失败 clawback 须被调用（used 分支触发）")

	// 清理注入后，用真实 clawback 重试须成功（验证此前未留下中间态）。
	realStore := supplyRefundStoreUnderTest()
	rec, err := realStore.ProcessRefundAtomically(ctx, mgrNo)
	require.NoError(t, err, "重试须成功（此前回滚干净）")
	require.Len(t, rec.Clawed, 1)
	require.Equal(t, 30, supplyOrderStatus(t, ctx, mgrNo))
}

// failClawbackTx 是注入失败的 XianyuRedeemClawback（仅用于回滚验证）。
type failClawbackTx struct {
	called bool
	err    error
}

func (c *failClawbackTx) ClawbackXianyuRedeemCodeTx(ctx context.Context, code *service.RedeemCode) (string, error) {
	c.called = true
	return "", c.err
}

func (c *failClawbackTx) InvalidateAfterClawback(ctx context.Context, code *service.RedeemCode) {}

// ---- ⑥ 退款↔兑换竞态双序线性化（锁持有控序，非 sleep 竞速）----
//
// 注释：买家兑换路径的锁对象是 redeem_codes 行上的条件 UPDATE
// `UPDATE redeem_codes SET status='used' WHERE code=$1 AND status IN ('unused','delivered')`
// （乐观锁，与退款事务的 `SELECT ... FOR UPDATE` 互斥，先提交者胜出）。

// ⑥-a：退款事务先持码行 FOR UPDATE → 并发兑换条件 UPDATE 阻塞后 0 行失败拒绝。
func TestSupplyRefundIntegrationRaceRefundHoldsLockThenExchangeBlocked(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-race-a"
	code := "IT-RACE-A-1"
	insertSupplyOrderFixture(t, ctx, mgrNo, []string{code}, 20)
	insertRedeemCodeFixture(t, ctx, code, "delivered", nil, nil, 0)
	t.Cleanup(func() { cleanupSupplyOrderFixture(t, ctx, mgrNo, []string{code}) })

	// 退款事务：开 ent 事务并持码行 FOR UPDATE（与 store 内 ProcessRefundAtomically 同款锁定）。
	tx, err := integrationEntClient.Tx(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	// 持订单行 + 码行 FOR UPDATE（模拟 store 内部锁序列）。
	_, err = client.ExecContext(ctx, supplyOrderSelectForUpdateSQL, mgrNo)
	require.NoError(t, err)
	_, err = client.ExecContext(ctx, `SELECT id, status, code, type, value, validity_days, group_id, used_by FROM redeem_codes WHERE code = $1 FOR UPDATE`, code)
	require.NoError(t, err)

	// 持锁期内执行与生产一致的"退款作废"（条件 UPDATE），须影响 1 行。
	resVoid, err := client.ExecContext(ctx, `
		UPDATE redeem_codes SET status = 'expired' WHERE code = $1 AND status IN ('delivered','unused')`, code)
	require.NoError(t, err)
	voidAffected, err := resVoid.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), voidAffected, "退款持锁作废须影响 1 行")

	// 并发兑换：条件 UPDATE 应阻塞在码行锁上（用独立连接；落空即视为阻塞后 0 行）。
	exchangeDone := make(chan int64, 1)
	go func() {
		res, e := integrationDB.ExecContext(ctx, `
			UPDATE redeem_codes SET status = 'used', used_by = 1 WHERE code = $1 AND status IN ('unused','delivered')`, code)
		if e != nil {
			exchangeDone <- -1
			return
		}
		n, _ := res.RowsAffected()
		exchangeDone <- n
	}()
	select {
	case n := <-exchangeDone:
		// 若未阻塞到锁释放就返回，则退款尚未提交，兑换应 0 行（码仍 delivered，锁后提交）。
		require.Equal(t, int64(0), n, "并发兑换在退款持锁期间须 0 行失败拒绝")
	case <-time.After(500 * time.Millisecond):
		// 预期阻塞：说明锁互斥成立（持锁阻塞 → 持锁方作废提交 → 后到兑换 0 行）。
	}

	// 持锁方作废 + 提交：兑换将在锁释放后返回 0 行失败拒绝（完整线性化语义）。
	require.NoError(t, tx.Commit())

	// 提交后等待并断言并发兑换结果（2s 兜底防挂）：退款先提交 → 并发兑换 0 行失败拒绝。
	select {
	case n := <-exchangeDone:
		require.Equal(t, int64(0), n, "退款先提交 → 并发兑换须 0 行失败拒绝")
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent exchange goroutine did not finish within 2s")
	}
	require.Equal(t, "expired", redeemCodeStatusByCode(t, ctx, code), "退款提交后码须为 expired（已被持锁方作废）")
}

// ⑥-b：兑换先提交（码变 used）→ 退款走追回分支。
func TestSupplyRefundIntegrationRaceExchangeFirstThenRefundClawsBack(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-race-b"
	code := "IT-RACE-B-1"
	var groupID, userID, subID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO groups (name, platform, rate_multiplier, status, subscription_type)
		VALUES ('supply-refund-it-raceb', 'anthropic', 1, 'active', 'standard') RETURNING id`).Scan(&groupID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, role, status, balance, concurrency)
		VALUES ('supply-refund-it-raceb@invalid', 'hash', 'user', 'active', 0, 1) RETURNING id`).Scan(&userID))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO user_subscriptions (user_id, group_id, starts_at, expires_at, status, notes)
		VALUES ($1, $2, NOW() - interval '3 days', NOW() + interval '10 days', 'active', '') RETURNING id`,
		userID, groupID).Scan(&subID))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM user_subscriptions WHERE id = $1`, subID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
		cleanupSupplyOrderFixture(t, ctx, mgrNo, []string{code})
	})

	insertSupplyOrderFixture(t, ctx, mgrNo, []string{code}, 20)
	insertRedeemCodeFixture(t, ctx, code, "delivered", &groupID, nil, 0)

	// 兑换先提交（码变 used）。
	res, err := integrationDB.ExecContext(ctx, `
		UPDATE redeem_codes SET status = 'used', used_by = $1 WHERE code = $2 AND status IN ('unused','delivered')`, userID, code)
	require.NoError(t, err)
	affected, _ := res.RowsAffected()
	require.Equal(t, int64(1), affected)

	// 退款事务：读码已是 used → 走追回分支。
	store := supplyRefundStoreUnderTest()
	rec, err := store.ProcessRefundAtomically(ctx, mgrNo)
	require.NoError(t, err)
	require.Len(t, rec.Clawed, 1, "兑换先提交后退款须走追回分支")
	require.Equal(t, 30, supplyOrderStatus(t, ctx, mgrNo))
}

// ---- ⑦ 并发重放幂等 + refunded_at 幂等（R6 #1）----

func TestSupplyRefundIntegrationConcurrentReplayIdempotent(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-replay"
	codes := []string{"IT-REPLAY-1", "IT-REPLAY-2"}
	insertSupplyOrderFixture(t, ctx, mgrNo, codes, 20)
	insertRedeemCodeFixture(t, ctx, codes[0], "delivered", nil, nil, 0)
	insertRedeemCodeFixture(t, ctx, codes[1], "delivered", nil, nil, 0)
	t.Cleanup(func() { cleanupSupplyOrderFixture(t, ctx, mgrNo, codes) })

	store := supplyRefundStoreUnderTest()

	// 第一次退款：正常作废 + 置退款态。
	rec1, err := store.ProcessRefundAtomically(ctx, mgrNo)
	require.NoError(t, err)
	require.Empty(t, rec1.Clawed, "无 used 码时空 Clawed")
	require.Equal(t, 30, supplyOrderStatus(t, ctx, mgrNo))
	firstRefundedAt := supplyOrderRefundedAt(t, ctx, mgrNo)
	require.NotNil(t, firstRefundedAt)

	// 第二次退款（并发重放）：持锁后见已退款 → 返回原行 + 空 Clawed，零写入（R6 #1）。
	rec2, err := store.ProcessRefundAtomically(ctx, mgrNo)
	require.NoError(t, err)
	require.NotNil(t, rec2.Row)
	require.Empty(t, rec2.Clawed, "重放须返回空 Clawed")
	require.Equal(t, 30, rec2.Row.Status)
	secondRefundedAt := supplyOrderRefundedAt(t, ctx, mgrNo)
	require.NotNil(t, secondRefundedAt)
	require.Equal(t, firstRefundedAt.Unix(), secondRefundedAt.Unix(), "refunded_at 须幂等（COALESCE 不覆盖）")
	// 码保持 expired，不再被二次处置。
	require.Equal(t, "expired", redeemCodeStatusByCode(t, ctx, codes[0]))
	require.Equal(t, "expired", redeemCodeStatusByCode(t, ctx, codes[1]))
}

// barrierRefundStore 是 SupplyRefundStore 的测试桩：包装真实 store，在每次进入
// ProcessRefundAtomically 时向 entered 发一票并阻塞于 release，用于让主 goroutine
// 集齐"两请求都已通过 service 预检"后再放行，制造方案 §6 ⑦ 的锁定竞争序列。
type barrierRefundStore struct {
	inner   SupplyRefundStore
	entered chan struct{} // 每请求进入 store 时发一票
	release chan struct{} // 集齐后 close 放行
}

func (b *barrierRefundStore) ProcessRefundAtomically(ctx context.Context, mgrNo string) (*SupplyRefundRecord, error) {
	b.entered <- struct{}{}
	<-b.release
	return b.inner.ProcessRefundAtomically(ctx, mgrNo)
}

// noopSupplyCardGenerator / noopSupplyCardPool 是 RefundNotify 路径不触达的零实现桩。
// 注意：RefundNotify 入口 fail-closed 守卫要求 gen/pool 非 nil，故必须提供（传 nil 会被
// 守卫判为 service unavailable）；goods/pwdResolve 在退款路径不触达，可保持 nil。
type noopSupplyCardGenerator struct{}

func (noopSupplyCardGenerator) GenerateCardsTx(ctx context.Context, tx *sql.Tx, groupID int64, quantity int, note string) ([]string, error) {
	return nil, nil
}

type noopSupplyCardPool struct{}

// ---- ⑦-b service 预检竞态：两请求同过预检 → 行锁竞争 → 后到者持锁见已退款 → 原行 + 空 Clawed → agree ----
//
// 本用例覆盖方案 §6 ⑦ 锁定序列（两请求同过预检 → 订单行锁竞争 → 后到者持锁见已退款 →
// 原行 + 空 Clawed → agree），与串行重放用例 ⑦-a 互补：⑦-a 是"先到者已提交后，后到者
// 直接重放"的串行序；本用例则保证两请求都先完成 service 预检（读到未退款）才进入真实事务，
// 真正触发并发行锁竞争，验证后到者走 R6 零写入回放而非重复处置。

func TestSupplyRefundIntegrationServiceConcurrentReplayAgree(t *testing.T) {
	ctx := context.Background()
	ensureIntegrationHarness(t)

	const mgrNo = "it-supply-svc-race"
	const code = "IT-SVC-RACE-1"
	insertSupplyOrderFixture(t, ctx, mgrNo, []string{code}, 20)
	insertRedeemCodeFixture(t, ctx, code, "delivered", nil, nil, 0)
	t.Cleanup(func() { cleanupSupplyOrderFixture(t, ctx, mgrNo, []string{code}) })

	// 真实 service：gen/pool 为不触达的零实现桩（守卫要求非 nil），goods/pwdResolve 传 nil；
	// refunds 用 barrier 包装真实 store，clawback 用真实实现。
	barrier := &barrierRefundStore{
		inner:   supplyRefundStoreUnderTest(),
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	svc := NewSupplyOrderService(
		NewSupplyOrderStore(integrationDB),
		noopSupplyCardGenerator{},
		noopSupplyCardPool{},
		nil, // goods：RefundNotify 退款路径不触达
		nil, // pwdResolve：默认官方合规映射
		barrier,
		realSupplyClawback(),
	)

	type callResult struct {
		order *SupplyOrder
		err   error
	}
	results := make(chan callResult, 2)

	// 两 goroutine 各调 RefundNotify；两者都先通过 service 预检（读到未退款），再进入
	// 真实事务时经 barrier 阻塞，由主 goroutine 收满 2 票后放行，制造 §6 ⑦ 竞争序列。
	for i := 0; i < 2; i++ {
		go func() {
			o, e := svc.RefundNotify(ctx, mgrNo)
			results <- callResult{order: o, err: e}
		}()
	}
	for i := 0; i < 2; i++ {
		<-barrier.entered
	}
	close(barrier.release)

	res1 := <-results
	res2 := <-results

	// 双 agree：两次调用均成功且返回非 nil 订单。
	require.NoError(t, res1.err, "并发退款须 agree")
	require.NoError(t, res2.err, "并发退款须 agree")
	require.NotNil(t, res1.order)
	require.NotNil(t, res2.order)

	// 订单行须置退款态（仅被处置恰好一次）；码须被作废恰好一次。
	require.Equal(t, 30, supplyOrderStatus(t, ctx, mgrNo), "订单行须为已退款态")
	require.Equal(t, "expired", redeemCodeStatusByCode(t, ctx, code), "码须被作废恰好一次")

	// 退款时间一致且等于 DB refunded_at（幂等零覆盖）；第二次为 R6 零写入回放。
	dbRefunded := supplyOrderRefundedAt(t, ctx, mgrNo)
	require.NotNil(t, dbRefunded, "须写入 refunded_at")
	require.Equal(t, dbRefunded.Unix(), res1.order.EndTime, "EndTime 须等于 DB refunded_at")
	require.Equal(t, res1.order.EndTime, res2.order.EndTime, "两次退款 EndTime 须一致（R6 零写入回放）")
}
