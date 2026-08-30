//go:build integration

package repository

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// legacyTestEncryptor 是可逆明文加密器，供迁移测试使用。
type legacyTestEncryptor struct{}

func (legacyTestEncryptor) Encrypt(plaintext string) (string, error) { return "cipher:" + plaintext, nil }
func (legacyTestEncryptor) Decrypt(ciphertext string) (string, error) {
	return strings.TrimPrefix(ciphertext, "cipher:"), nil
}

func legacyTestEncryptorInstance() service.SecretEncryptor { return legacyTestEncryptor{} }

func TestXianyuControlRepoWorkerConfigSingleActive(t *testing.T) {
	ctx := context.Background()
	db := integrationDB

	_, err := db.ExecContext(ctx, `DELETE FROM xianyu_binding_rules`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM xianyu_products`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM xianyu_accounts`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM xianyu_item_pools`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM xianyu_worker_configs`)
	require.NoError(t, err)

	repo := NewXianyuControlRepository(db)

	first, err := repo.CreateWorkerConfig(ctx, service.XianyuWorkerConfig{
		BaseURL: "http://xianyu-worker-backend:8089", APITokenEncrypted: "enc", Status: service.XianyuWorkerStatusActive,
	})
	require.NoError(t, err)
	require.Equal(t, service.XianyuWorkerStatusActive, first.Status)

	// 第二个 active 必须被数据库部分唯一索引拒绝。
	_, err = repo.CreateWorkerConfig(ctx, service.XianyuWorkerConfig{
		BaseURL: "http://xianyu-worker-backend:8089", APITokenEncrypted: "enc", Status: service.XianyuWorkerStatusActive,
	})
	require.Error(t, err)

	active, err := repo.GetActiveWorkerConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, first.ID, active.ID)

	_, err = db.ExecContext(ctx, `DELETE FROM xianyu_worker_configs`)
	require.NoError(t, err)
}

func TestXianyuControlRepoAccountAndProductLifecycle(t *testing.T) {
	ctx := context.Background()
	db := integrationDB

	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_binding_rules`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_products`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_accounts`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_item_pools`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_worker_configs`)

	repo := NewXianyuControlRepository(db)

	worker, err := repo.CreateWorkerConfig(ctx, service.XianyuWorkerConfig{
		BaseURL: "http://xianyu-worker-backend:8089", APITokenEncrypted: "enc", Status: service.XianyuWorkerStatusActive,
	})
	require.NoError(t, err)

	account, err := repo.UpsertAccount(ctx, service.XianyuAccount{
		WorkerConfigID: worker.ID, AccountID: "acc-1", Nickname: "n1",
		Status: service.XianyuAccountStatusEnabled, CookieStatus: service.XianyuCookieStatusValid,
	})
	require.NoError(t, err)
	require.Equal(t, worker.ID, account.WorkerConfigID)

	got, err := repo.GetAccountByWorkerAndAccountID(ctx, worker.ID, "acc-1")
	require.NoError(t, err)
	require.Equal(t, account.ID, got.ID)

	// 同一 Worker 内 account_id 唯一。
	_, err = repo.UpsertAccount(ctx, service.XianyuAccount{
		WorkerConfigID: worker.ID, AccountID: "acc-1", Nickname: "dup",
	})
	require.NoError(t, err)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM xianyu_accounts WHERE worker_config_id=$1 AND account_id='acc-1'`, worker.ID).Scan(&count))
	require.Equal(t, 1, count)

	pool, err := repo.CreateItemPool(ctx, service.XianyuItemPool{Name: "standard", Slug: "standard"})
	require.NoError(t, err)

	// unmapped 且 pool_id 为空。
	product, err := repo.UpsertProduct(ctx, service.XianyuProduct{
		AccountPK: account.ID, AccountID: "acc-1", ItemID: "item-1", Title: "t",
		BindingStatus: service.XianyuBindingStatusUnmapped, Status: service.XianyuProductStatusActive,
	})
	require.NoError(t, err)

	// 绑定：mapped 必须带 pool_id。
	require.NoError(t, repo.UpdateProductBinding(ctx, product.ID, service.XianyuBindingStatusMapped, service.XianyuBindingSourceManual, &pool.ID))

	// mapped 而 pool_id 为空必须被 CHECK 拒绝。
	err = repo.UpdateProductBinding(ctx, product.ID, service.XianyuBindingStatusMapped, service.XianyuBindingSourceManual, nil)
	require.Error(t, err)

	// unmapped 而 pool_id 非空必须被 CHECK 拒绝。
	err = repo.UpdateProductBinding(ctx, product.ID, service.XianyuBindingStatusUnmapped, service.XianyuBindingSourceAutoNew, &pool.ID)
	require.Error(t, err)

	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_binding_rules`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_products`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_accounts`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_item_pools`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_worker_configs`)
}

func TestXianyuDeliveryStateTransitions(t *testing.T) {
	ctx := context.Background()
	db := integrationDB

	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_order_claims`)
	_, _ = db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE type = $1`, service.RedeemTypeXianyuDelivery)

	var userID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ('xianyu-state-transition@test.local', 'not-a-real-password')
		RETURNING id`).Scan(&userID))

	// 创建池和库存。
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_binding_rules`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_products`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_accounts`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_item_pools`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_worker_configs`)
	control := NewXianyuControlRepository(db)
	pool, err := control.CreateItemPool(ctx, service.XianyuItemPool{Name: "standard", Slug: "standard"})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO redeem_codes (code, type, value, status, notes)
		VALUES ('XY0000000000000000000000000004', $1, 0, 'unused', $2)`,
		service.RedeemTypeXianyuDelivery, service.XianyuPoolNote("standard"))
	require.NoError(t, err)

	claimRepo := NewXianyuOrderClaimRepository(db).(*xianyuOrderClaimRepository)
	code, err := claimRepo.Claim(ctx, service.XianyuDeliveryClaim{
		OrderID: "order-state", ItemID: "item", AccountID: "account", BuyerID: "buyer", PoolID: pool.ID,
		BindingSource: service.XianyuBindingSourceManual,
	}, userID)
	require.NoError(t, err)
	require.NotEmpty(t, code)

	stateRepo := NewXianyuOrderClaimStateRepository(db)

	// 新 claim 的 attempt_count 必须为 0（初始自动发货语义）。
	claim, err := stateRepo.GetDeliveryClaim(ctx, "order-state")
	require.NoError(t, err)
	require.Equal(t, 0, claim.AttemptCount)

	// 自动发货回执 attempt=0 能关闭新 claim（bug A 修复：不再静默忽略回执）。
	require.NoError(t, stateRepo.RecordDeliveryResult(ctx, service.XianyuDeliveryStatusResult{OrderNo: "order-state", Success: true, Confirmed: true}))
	claim, err = stateRepo.GetDeliveryClaim(ctx, "order-state")
	require.NoError(t, err)
	require.Equal(t, service.XianyuDeliveryStatusSent, claim.DeliveryStatus)
	require.Equal(t, 0, claim.AttemptCount)

	// sent 为终态：旧失败回执按幂等忽略（2xx ack，不报错），状态不被覆盖。
	require.NoError(t, stateRepo.RecordDeliveryResult(ctx, service.XianyuDeliveryStatusResult{OrderNo: "order-state", Success: false}))
	claim, err = stateRepo.GetDeliveryClaim(ctx, "order-state")
	require.NoError(t, err)
	require.Equal(t, service.XianyuDeliveryStatusSent, claim.DeliveryStatus)

	// 已 sent 不允许补发。
	_, _, err = stateRepo.ResendOriginalCode(ctx, "order-state")
	require.ErrorIs(t, err, service.ErrXianyuDeliveryAlreadySent)

	// pending 且未确认成功回执 -> 保持 pending。
	_, err = db.ExecContext(ctx, `
		UPDATE xianyu_order_claims SET delivery_status='pending', attempt_count=0 WHERE order_no='order-state'`)
	require.NoError(t, err)
	require.NoError(t, stateRepo.RecordDeliveryResult(ctx, service.XianyuDeliveryStatusResult{OrderNo: "order-state", Success: true, Confirmed: false}))
	claim, err = stateRepo.GetDeliveryClaim(ctx, "order-state")
	require.NoError(t, err)
	require.Equal(t, service.XianyuDeliveryStatusPending, claim.DeliveryStatus)

	// pending 不允许补发（无失败态）。
	_, _, err = stateRepo.ResendOriginalCode(ctx, "order-state")
	require.ErrorIs(t, err, service.ErrXianyuResendNotPending)

	// pending -> failed -> 人工补发原码（不分配新码）。
	require.NoError(t, stateRepo.RecordDeliveryResult(ctx, service.XianyuDeliveryStatusResult{OrderNo: "order-state", Success: false}))
	claim, err = stateRepo.GetDeliveryClaim(ctx, "order-state")
	require.NoError(t, err)
	require.Equal(t, service.XianyuDeliveryStatusFailed, claim.DeliveryStatus)
	originalCode := claim.Code

	resent, attempt, err := stateRepo.ResendOriginalCode(ctx, "order-state")
	require.NoError(t, err)
	require.Equal(t, originalCode, resent)
	claim, err = stateRepo.GetDeliveryClaim(ctx, "order-state")
	require.NoError(t, err)
	require.Equal(t, service.XianyuDeliveryStatusPending, claim.DeliveryStatus)
	require.Equal(t, 1, claim.AttemptCount)
	// 返回的 attempt 必须与持久化 attempt_count 一致（回执关联用）。
	require.Equal(t, claim.AttemptCount, attempt)

	// 补发回执 attempt=1 生效：pending -> sent。
	require.NoError(t, stateRepo.RecordDeliveryResult(ctx, service.XianyuDeliveryStatusResult{OrderNo: "order-state", Success: true, Confirmed: true, Attempt: 1}))
	claim, err = stateRepo.GetDeliveryClaim(ctx, "order-state")
	require.NoError(t, err)
	require.Equal(t, service.XianyuDeliveryStatusSent, claim.DeliveryStatus)
	require.Equal(t, 1, claim.AttemptCount)

	// 旧代次 attempt=0 的迟到回执不得改变新代次状态（隔离断言）。
	require.NoError(t, stateRepo.RecordDeliveryResult(ctx, service.XianyuDeliveryStatusResult{OrderNo: "order-state", Success: false, Attempt: 0}))
	claim, err = stateRepo.GetDeliveryClaim(ctx, "order-state")
	require.NoError(t, err)
	require.Equal(t, service.XianyuDeliveryStatusSent, claim.DeliveryStatus)
	require.Equal(t, 1, claim.AttemptCount)

	// 同 attempt 重复回执幂等 ack，不报错、不改变状态。
	require.NoError(t, stateRepo.RecordDeliveryResult(ctx, service.XianyuDeliveryStatusResult{OrderNo: "order-state", Success: true, Confirmed: true, Attempt: 1}))
	claim, err = stateRepo.GetDeliveryClaim(ctx, "order-state")
	require.NoError(t, err)
	require.Equal(t, service.XianyuDeliveryStatusSent, claim.DeliveryStatus)

	// 库存只消耗一个码。
	var usedCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM redeem_codes WHERE type=$1 AND status='used'`, service.RedeemTypeXianyuDelivery).Scan(&usedCount))
	require.Equal(t, 1, usedCount)

	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_order_claims`)
	_, _ = db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE type = $1`, service.RedeemTypeXianyuDelivery)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_binding_rules`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_products`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_accounts`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_item_pools`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_worker_configs`)
}

func TestXianyuLegacyMigrationBackfills(t *testing.T) {
	ctx := context.Background()
	db := integrationDB

	_, _ = db.ExecContext(ctx, `DELETE FROM settings WHERE key = 'xianyu_delivery.legacy_migrated'`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_order_claims`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_binding_rules`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_products`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_accounts`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_item_pools`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_worker_configs`)

	// 预置一条历史 claim。
	var userID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ('xianyu-legacy-migrate@test.local', 'not-a-real-password')
		RETURNING id`).Scan(&userID))
	_, err := db.ExecContext(ctx, `
		INSERT INTO redeem_codes (code, type, value, status, notes)
		VALUES ('XY0000000000000000000000000005', $1, 0, 'used', $2)`,
		service.RedeemTypeXianyuDelivery, service.XianyuPoolNote("standard"))
	require.NoError(t, err)
	var codeID int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM redeem_codes WHERE code='XY0000000000000000000000000005'`).Scan(&codeID))
	_, err = db.ExecContext(ctx, `
		INSERT INTO xianyu_order_claims (order_no, redeem_code_id, account_id, item_id, buyer_id)
		VALUES ('legacy-order-1', $1, 'acc-legacy', 'item-legacy', 'buyer')`, codeID)
	require.NoError(t, err)

	control := NewXianyuControlRepository(db)
	migrator := service.NewXianyuLegacyMigration(db, control, &legacyTestEncryptor{})

	require.NoError(t, migrator.Migrate(ctx, map[string]string{
		"acc-legacy:item-legacy": "standard",
		"acc-2:item-2":           "second",
	}))

	// 池创建。
	pools, err := control.ListItemPools(ctx)
	require.NoError(t, err)
	require.Len(t, pools, 2)
	var standardPoolID, secondPoolID int64
	for _, p := range pools {
		switch p.Slug {
		case "standard":
			standardPoolID = p.ID
		case "second":
			secondPoolID = p.ID
		}
	}
	require.NotZero(t, standardPoolID)
	require.NotZero(t, secondPoolID)

	// 占位账号（disabled）。
	var placeholderWorkerID int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id FROM xianyu_worker_configs ORDER BY id LIMIT 1`).Scan(&placeholderWorkerID))
	accounts, err := control.ListAccounts(ctx, placeholderWorkerID)
	require.NoError(t, err)
	require.Len(t, accounts, 2)
	for _, a := range accounts {
		require.Equal(t, service.XianyuAccountStatusDisabled, a.Status)
	}

	// 商品映射 mapped/manual。
	products, err := control.ListProducts(ctx)
	require.NoError(t, err)
	require.Len(t, products, 2)
	for _, p := range products {
		require.Equal(t, service.XianyuBindingStatusMapped, p.BindingStatus)
		require.Equal(t, service.XianyuBindingSourceManual, p.BindingSource)
	}

	// 历史 claim 回填：池可定位 -> 绑定 + legacy_unverified。
	var legacyStatus, legacySource string
	var legacyPoolID sql.NullInt64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT delivery_status, COALESCE(binding_source,''), pool_id
		FROM xianyu_order_claims WHERE order_no='legacy-order-1'`).Scan(&legacyStatus, &legacySource, &legacyPoolID))
	require.Equal(t, service.XianyuDeliveryStatusLegacyUnverified, legacyStatus)
	require.Equal(t, service.XianyuBindingSourceManual, legacySource)
	require.Equal(t, standardPoolID, legacyPoolID.Int64)

	// 幂等：再次执行无副作用。
	require.NoError(t, migrator.Migrate(ctx, map[string]string{"acc-legacy:item-legacy": "standard"}))
	pools, err = control.ListItemPools(ctx)
	require.NoError(t, err)
	require.Len(t, pools, 2)

	_, _ = db.ExecContext(ctx, `DELETE FROM settings WHERE key = 'xianyu_delivery.legacy_migrated'`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_order_claims`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_binding_rules`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_products`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_accounts`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_item_pools`)
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_worker_configs`)
	_, _ = db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE type = $1`, service.RedeemTypeXianyuDelivery)
}

var _ = time.Now
