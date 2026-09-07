//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestXianyuOrderClaimRepositoryConcurrentFirstClaim(t *testing.T) {
	ctx := context.Background()
	db := integrationDB

	_, err := db.ExecContext(ctx, `DELETE FROM xianyu_order_claims`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE notes = $1`, service.XianyuPoolNote("standard"))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO redeem_codes (code, type, value, status, notes)
		VALUES
			('XY0000000000000000000000000001', $1, 0, 'unused', $2),
			('XY0000000000000000000000000002', $1, 0, 'unused', $2)`,
		service.RedeemTypeSubscription, service.XianyuPoolNote("standard"))
	require.NoError(t, err)

	// 创建池记录，供 claim 通过 PoolID 定位 slug。
	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_item_pools WHERE slug = 'standard'`)
	var poolID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO xianyu_item_pools (name, slug) VALUES ('standard', 'standard') RETURNING id`).Scan(&poolID))

	repo := NewXianyuOrderClaimRepository(db).(*xianyuOrderClaimRepository)
	claim := service.XianyuDeliveryClaim{
		OrderID: "order-concurrent", ItemID: "item", AccountID: "account", BuyerID: "buyer", PoolID: poolID,
	}

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := make([]string, 0, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, err := repo.Claim(ctx, claim)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			codes = append(codes, code)
			mu.Unlock()
		}()
	}
	wg.Wait()

	require.Len(t, codes, workers)
	for _, code := range codes {
		require.Equal(t, codes[0], code)
	}

	var deliveredCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM redeem_codes
		WHERE notes = $1 AND status = 'delivered'`,
		service.XianyuPoolNote("standard")).Scan(&deliveredCount))
	require.Equal(t, 1, deliveredCount)

	_, err = db.ExecContext(ctx, `
		DELETE FROM xianyu_order_claims WHERE order_no = $1`, claim.OrderID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		DELETE FROM redeem_codes WHERE notes = $1`, service.XianyuPoolNote("standard"))
	require.NoError(t, err)
}

func TestXianyuOrderClaimRepositoryRejectsRedeemCodeDelete(t *testing.T) {
	ctx := context.Background()
	db := integrationDB

	_, err := db.ExecContext(ctx, `DELETE FROM xianyu_order_claims`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE notes = $1`, service.XianyuPoolNote("standard"))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO redeem_codes (code, type, value, status, notes)
		VALUES ('XY0000000000000000000000000003', $1, 0, 'unused', $2)`,
		service.RedeemTypeSubscription, service.XianyuPoolNote("standard"))
	require.NoError(t, err)

	_, _ = db.ExecContext(ctx, `DELETE FROM xianyu_item_pools WHERE slug = 'standard'`)
	var poolID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO xianyu_item_pools (name, slug) VALUES ('standard', 'standard') RETURNING id`).Scan(&poolID))

	repo := NewXianyuOrderClaimRepository(db).(*xianyuOrderClaimRepository)
	code, err := repo.Claim(ctx, service.XianyuDeliveryClaim{
		OrderID: "order-protect", ItemID: "item", AccountID: "account", BuyerID: "buyer", PoolID: poolID,
	})
	require.NoError(t, err)

	var codeID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT id FROM redeem_codes WHERE code = $1`, code).Scan(&codeID))

	_, err = db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE id = $1`, codeID)
	require.Error(t, err)

	_, err = db.ExecContext(ctx, `DELETE FROM xianyu_order_claims WHERE order_no = 'order-protect'`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM redeem_codes WHERE notes = $1`, service.XianyuPoolNote("standard"))
	require.NoError(t, err)
}
