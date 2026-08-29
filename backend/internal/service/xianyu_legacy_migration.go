package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// legacyMigrationMarkerKey 标记旧 item_pools 已迁移；避免重复执行。
const legacyMigrationMarkerKey = "xianyu_delivery.legacy_migrated"

// XianyuLegacyMigration 将 config.yaml 中的 xianyu_delivery.item_pools 一次性迁移到数据库。
// 迁移在单个事务内完成，失败即回滚并以启动失败关闭。
type XianyuLegacyMigration struct {
	db        *sql.DB
	control   XianyuControlRepository
	encryptor SecretEncryptor
}

// NewXianyuLegacyMigration 创建旧配置迁移器。
func NewXianyuLegacyMigration(db *sql.DB, control XianyuControlRepository, encryptor SecretEncryptor) *XianyuLegacyMigration {
	return &XianyuLegacyMigration{db: db, control: control, encryptor: encryptor}
}

// Migrate 读取并迁移旧 item_pools（配置键仅迁移器读取）。
func (m *XianyuLegacyMigration) Migrate(ctx context.Context, itemPools map[string]string) error {
	if m == nil || m.db == nil || m.control == nil {
		return nil
	}
	done, err := m.isMigrated(ctx)
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	if len(itemPools) == 0 {
		// 无旧配置可迁移，直接标记完成。
		return m.markMigrated(ctx)
	}

	if err := m.migrateInTx(ctx, itemPools); err != nil {
		return err
	}
	return nil
}

func (m *XianyuLegacyMigration) isMigrated(ctx context.Context) (bool, error) {
	var value string
	err := m.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1`, legacyMigrationMarkerKey).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read xianyu legacy migration marker: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(value), "true"), nil
}

func (m *XianyuLegacyMigration) markMigrated(ctx context.Context) error {
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		VALUES ($1, 'true', NOW())
		ON CONFLICT (key) DO UPDATE SET value = 'true', updated_at = NOW()`, legacyMigrationMarkerKey); err != nil {
		return fmt.Errorf("mark xianyu legacy migration: %w", err)
	}
	return nil
}

func (m *XianyuLegacyMigration) migrateInTx(ctx context.Context, itemPools map[string]string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin xianyu legacy migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 确保存在一个 Worker 配置（旧配置没有一个明确的 worker 占位）。
	if err := m.ensurePlaceholderWorker(ctx, tx); err != nil {
		return err
	}

	// 逐个解析 cookie_id:item_id = pool_slug
	seenPools := map[string]int64{}
	seenAccounts := map[string]int64{}
	for key, slug := range itemPools {
		cookieID, itemID, ok := strings.Cut(strings.TrimSpace(key), ":")
		if !ok {
			return fmt.Errorf("invalid xianyu item_pools key %q: expected cookie_id:item_id", key)
		}
		cookieID = strings.TrimSpace(cookieID)
		itemID = strings.TrimSpace(itemID)
		slug = strings.TrimSpace(slug)
		if cookieID == "" || itemID == "" || slug == "" {
			return fmt.Errorf("invalid xianyu item_pools entry %q: cookie_id, item_id and pool_slug are required", key)
		}

		poolID, err := m.ensurePool(ctx, tx, slug, seenPools)
		if err != nil {
			return err
		}
		accountID, err := m.ensurePlaceholderAccount(ctx, tx, cookieID, seenAccounts)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO xianyu_products
				(account_pk, account_id, item_id, title, spec_name, spec_value, pool_id, binding_status, binding_source, status)
			VALUES ($1, $2, $3, '', '', '', $4, 'mapped', 'manual', 'active')
			ON CONFLICT (account_pk, item_id, spec_name, spec_value) DO UPDATE SET
				pool_id = EXCLUDED.pool_id, binding_status = 'mapped', binding_source = 'manual', status = 'active', updated_at = NOW()`,
			accountID, cookieID, itemID, poolID); err != nil {
			return fmt.Errorf("insert xianyu legacy product: %w", err)
		}
	}

	// 回填历史 claim：池可定位则绑定，不可定位则置空并保留 legacy_unresolved。
	if err := m.backfillClaims(ctx, tx, itemPools); err != nil {
		return err
	}

	// 标记迁移完成。
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		VALUES ($1, 'true', NOW())
		ON CONFLICT (key) DO UPDATE SET value = 'true', updated_at = NOW()`, legacyMigrationMarkerKey); err != nil {
		return fmt.Errorf("mark xianyu legacy migration in tx: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit xianyu legacy migration: %w", err)
	}
	return nil
}

// ensurePlaceholderWorker 若不存在任何 Worker 配置，创建一个 disabled 占位。
func (m *XianyuLegacyMigration) ensurePlaceholderWorker(ctx context.Context, tx *sql.Tx) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM xianyu_worker_configs`).Scan(&count); err != nil {
		return fmt.Errorf("count xianyu worker configs: %w", err)
	}
	if count > 0 {
		return nil
	}
	token, err := m.encryptor.Encrypt("placeholder-worker-token")
	if err != nil {
		return fmt.Errorf("encrypt placeholder worker token: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO xianyu_worker_configs (base_url, api_token_encrypted, status, health_status)
		VALUES ('http://xianyu-worker-backend:8089', $1, 'disabled', 'unknown')`, token); err != nil {
		return fmt.Errorf("insert placeholder xianyu worker config: %w", err)
	}
	return nil
}

func (m *XianyuLegacyMigration) ensurePool(ctx context.Context, tx *sql.Tx, slug string, cache map[string]int64) (int64, error) {
	if id, ok := cache[slug]; ok {
		return id, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM xianyu_item_pools WHERE slug = $1`, slug).Scan(&id)
	if err == nil {
		cache[slug] = id
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("lookup xianyu item pool: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO xianyu_item_pools (name, slug, status)
		VALUES ($1, $2, 'active')
		RETURNING id`, slug, slug).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert xianyu item pool: %w", err)
	}
	cache[slug] = id
	return id, nil
}

func (m *XianyuLegacyMigration) ensurePlaceholderAccount(ctx context.Context, tx *sql.Tx, accountID string, cache map[string]int64) (int64, error) {
	if id, ok := cache[accountID]; ok {
		return id, nil
	}
	// 复用当前唯一的（占位或已配置）Worker。
	var workerID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM xianyu_worker_configs ORDER BY id LIMIT 1`).Scan(&workerID); err != nil {
		return 0, fmt.Errorf("lookup placeholder xianyu worker: %w", err)
	}
	var id int64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM xianyu_accounts WHERE worker_config_id = $1 AND account_id = $2`,
		workerID, accountID).Scan(&id)
	if err == nil {
		cache[accountID] = id
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("lookup xianyu account: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO xianyu_accounts (worker_config_id, account_id, nickname, status, cookie_status, task_status)
		VALUES ($1, $2, '', 'disabled', 'unknown', 'unknown')
		RETURNING id`, workerID, accountID).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert placeholder xianyu account: %w", err)
	}
	cache[accountID] = id
	return id, nil
}

func (m *XianyuLegacyMigration) backfillClaims(ctx context.Context, tx *sql.Tx, itemPools map[string]string) error {
	rows, err := tx.QueryContext(ctx, `SELECT order_no, account_id, item_id FROM xianyu_order_claims`)
	if err != nil {
		return fmt.Errorf("scan xianyu legacy claims: %w", err)
	}
	defer rows.Close()
	type claimRow struct {
		orderNo   string
		accountID string
		itemID    string
	}
	var claims []claimRow
	for rows.Next() {
		var c claimRow
		if err := rows.Scan(&c.orderNo, &c.accountID, &c.itemID); err != nil {
			return fmt.Errorf("scan xianyu legacy claim: %w", err)
		}
		claims = append(claims, c)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate xianyu legacy claims: %w", err)
	}

	// 旧配置键格式为 cookie_id:item_id = pool_slug。
	claimToSlug := func(accountID, itemID string) (string, bool) {
		slug, ok := itemPools[accountID+":"+itemID]
		return strings.TrimSpace(slug), ok
	}

	for _, claim := range claims {
		slug, ok := claimToSlug(claim.accountID, claim.itemID)
		var poolID any
		bindingSource := XianyuBindingSourceLegacyUnresolved
		if ok && slug != "" {
			poolID = nil
			var pid int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM xianyu_item_pools WHERE slug = $1`, slug).Scan(&pid); err == nil {
				poolID = pid
				bindingSource = XianyuBindingSourceManual
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE xianyu_order_claims
			SET pool_id = $2, binding_source = $3,
			    delivery_status = 'legacy_unverified', delivery_error = NULL, attempt_count = 0
			WHERE order_no = $1`, claim.orderNo, poolID, bindingSource); err != nil {
			return fmt.Errorf("backfill xianyu legacy claim: %w", err)
		}
	}
	return nil
}