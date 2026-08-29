package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// xianyuControlRepository 实现 service.XianyuControlRepository。
// 使用原生 SQL，因为控制面表不在 ent schema 中，且需要部分唯一索引约束。
type xianyuControlRepository struct {
	db *sql.DB
}

// NewXianyuControlRepository 创建控制面仓库。
func NewXianyuControlRepository(db *sql.DB) service.XianyuControlRepository {
	return &xianyuControlRepository{db: db}
}

const xianyuWorkerConfigColumns = `id, base_url, api_token_encrypted, status, health_status, last_checked_at, created_at, updated_at`

func scanWorkerConfig(row interface{ Scan(...any) error }) (*service.XianyuWorkerConfig, error) {
	var c service.XianyuWorkerConfig
	var lastChecked sql.NullTime
	if err := row.Scan(&c.ID, &c.BaseURL, &c.APITokenEncrypted, &c.Status, &c.HealthStatus, &lastChecked, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	if lastChecked.Valid {
		c.LastCheckedAt = &lastChecked.Time
	}
	return &c, nil
}

func (r *xianyuControlRepository) ListWorkerConfigs(ctx context.Context) ([]service.XianyuWorkerConfig, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+xianyuWorkerConfigColumns+` FROM xianyu_worker_configs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list xianyu worker configs: %w", err)
	}
	defer rows.Close()
	var out []service.XianyuWorkerConfig
	for rows.Next() {
		c, err := scanWorkerConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (r *xianyuControlRepository) CreateWorkerConfig(ctx context.Context, cfg service.XianyuWorkerConfig) (*service.XianyuWorkerConfig, error) {
	if cfg.Status == "" {
		cfg.Status = service.XianyuWorkerStatusDisabled
	}
	if cfg.HealthStatus == "" {
		cfg.HealthStatus = service.XianyuWorkerHealthUnknown
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO xianyu_worker_configs (base_url, api_token_encrypted, status, health_status)
		VALUES ($1, $2, $3, $4)
		RETURNING `+xianyuWorkerConfigColumns,
		cfg.BaseURL, cfg.APITokenEncrypted, cfg.Status, cfg.HealthStatus)
	created, err := scanWorkerConfig(row)
	if err != nil {
		return nil, fmt.Errorf("create xianyu worker config: %w", err)
	}
	return created, nil
}

func (r *xianyuControlRepository) UpdateWorkerConfig(ctx context.Context, cfg service.XianyuWorkerConfig) (*service.XianyuWorkerConfig, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE xianyu_worker_configs
		SET base_url = $2, api_token_encrypted = $3, status = $4, health_status = $5,
		    last_checked_at = $6, updated_at = NOW()
		WHERE id = $1
		RETURNING `+xianyuWorkerConfigColumns,
		cfg.ID, cfg.BaseURL, cfg.APITokenEncrypted, cfg.Status, cfg.HealthStatus, nullableTime(cfg.LastCheckedAt))
	updated, err := scanWorkerConfig(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuWorkerConfigNotFound
		}
		return nil, fmt.Errorf("update xianyu worker config: %w", err)
	}
	return updated, nil
}

func (r *xianyuControlRepository) GetActiveWorkerConfig(ctx context.Context) (*service.XianyuWorkerConfig, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+xianyuWorkerConfigColumns+`
		FROM xianyu_worker_configs
		WHERE status = $1
		ORDER BY id LIMIT 1`, service.XianyuWorkerStatusActive)
	cfg, err := scanWorkerConfig(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuWorkerConfigNotFound
		}
		return nil, fmt.Errorf("get active xianyu worker config: %w", err)
	}
	return cfg, nil
}

const xianyuAccountColumns = `id, worker_config_id, account_id, nickname, status, cookie_status, task_status, last_login_at, last_seen_at, created_at, updated_at`

func scanAccount(row interface{ Scan(...any) error }) (*service.XianyuAccount, error) {
	var a service.XianyuAccount
	var lastLogin, lastSeen sql.NullTime
	if err := row.Scan(&a.ID, &a.WorkerConfigID, &a.AccountID, &a.Nickname, &a.Status, &a.CookieStatus, &a.TaskStatus, &lastLogin, &lastSeen, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	if lastLogin.Valid {
		a.LastLoginAt = &lastLogin.Time
	}
	if lastSeen.Valid {
		a.LastSeenAt = &lastSeen.Time
	}
	return &a, nil
}

func (r *xianyuControlRepository) ListAccounts(ctx context.Context, workerConfigID int64) ([]service.XianyuAccount, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+xianyuAccountColumns+` FROM xianyu_accounts WHERE worker_config_id = $1 ORDER BY id`, workerConfigID)
	if err != nil {
		return nil, fmt.Errorf("list xianyu accounts: %w", err)
	}
	defer rows.Close()
	var out []service.XianyuAccount
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (r *xianyuControlRepository) GetAccountByWorkerAndAccountID(ctx context.Context, workerConfigID int64, accountID string) (*service.XianyuAccount, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+xianyuAccountColumns+` FROM xianyu_accounts WHERE worker_config_id = $1 AND account_id = $2`, workerConfigID, accountID)
	a, err := scanAccount(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuAccountNotFound
		}
		return nil, fmt.Errorf("get xianyu account: %w", err)
	}
	return a, nil
}

func (r *xianyuControlRepository) UpsertAccount(ctx context.Context, account service.XianyuAccount) (*service.XianyuAccount, error) {
	if account.Status == "" {
		account.Status = service.XianyuAccountStatusDisabled
	}
	if account.CookieStatus == "" {
		account.CookieStatus = service.XianyuCookieStatusUnknown
	}
	if account.TaskStatus == "" {
		account.TaskStatus = service.XianyuTaskStatusUnknown
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO xianyu_accounts
			(worker_config_id, account_id, nickname, status, cookie_status, task_status, last_login_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (worker_config_id, account_id) DO UPDATE SET
			nickname = EXCLUDED.nickname,
			status = EXCLUDED.status,
			cookie_status = EXCLUDED.cookie_status,
			task_status = EXCLUDED.task_status,
			last_login_at = COALESCE(EXCLUDED.last_login_at, xianyu_accounts.last_login_at),
			last_seen_at = COALESCE(EXCLUDED.last_seen_at, xianyu_accounts.last_seen_at),
			updated_at = NOW()
		RETURNING `+xianyuAccountColumns,
		account.WorkerConfigID, account.AccountID, account.Nickname, account.Status, account.CookieStatus, account.TaskStatus,
		nullableTime(account.LastLoginAt), nullableTime(account.LastSeenAt))
	saved, err := scanAccount(row)
	if err != nil {
		return nil, fmt.Errorf("upsert xianyu account: %w", err)
	}
	return saved, nil
}

func (r *xianyuControlRepository) UpdateAccount(ctx context.Context, account service.XianyuAccount) (*service.XianyuAccount, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE xianyu_accounts
		SET nickname = $2, status = $3, cookie_status = $4, task_status = $5,
		    last_login_at = $6, last_seen_at = $7, updated_at = NOW()
		WHERE id = $1
		RETURNING `+xianyuAccountColumns,
		account.ID, account.Nickname, account.Status, account.CookieStatus, account.TaskStatus,
		nullableTime(account.LastLoginAt), nullableTime(account.LastSeenAt))
	saved, err := scanAccount(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuAccountNotFound
		}
		return nil, fmt.Errorf("update xianyu account: %w", err)
	}
	return saved, nil
}

const xianyuItemPoolColumns = `id, name, slug, description, low_stock_threshold, status, created_at, updated_at`

func scanItemPool(row interface{ Scan(...any) error }) (*service.XianyuItemPool, error) {
	var p service.XianyuItemPool
	if err := row.Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.LowStockThreshold, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *xianyuControlRepository) ListItemPools(ctx context.Context) ([]service.XianyuItemPool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+xianyuItemPoolColumns+` FROM xianyu_item_pools ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list xianyu item pools: %w", err)
	}
	defer rows.Close()
	var out []service.XianyuItemPool
	for rows.Next() {
		p, err := scanItemPool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *xianyuControlRepository) GetItemPoolBySlug(ctx context.Context, slug string) (*service.XianyuItemPool, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+xianyuItemPoolColumns+` FROM xianyu_item_pools WHERE slug = $1`, slug)
	p, err := scanItemPool(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuItemPoolNotFound
		}
		return nil, fmt.Errorf("get xianyu item pool: %w", err)
	}
	return p, nil
}

func (r *xianyuControlRepository) GetItemPoolByID(ctx context.Context, id int64) (*service.XianyuItemPool, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+xianyuItemPoolColumns+` FROM xianyu_item_pools WHERE id = $1`, id)
	p, err := scanItemPool(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuItemPoolNotFound
		}
		return nil, fmt.Errorf("get xianyu item pool by id: %w", err)
	}
	return p, nil
}

func (r *xianyuControlRepository) CreateItemPool(ctx context.Context, pool service.XianyuItemPool) (*service.XianyuItemPool, error) {
	if pool.Status == "" {
		pool.Status = service.XianyuItemPoolStatusActive
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO xianyu_item_pools (name, slug, description, low_stock_threshold, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+xianyuItemPoolColumns,
		pool.Name, pool.Slug, pool.Description, pool.LowStockThreshold, pool.Status)
	created, err := scanItemPool(row)
	if err != nil {
		return nil, fmt.Errorf("create xianyu item pool: %w", err)
	}
	return created, nil
}

func (r *xianyuControlRepository) UpdateItemPool(ctx context.Context, pool service.XianyuItemPool) (*service.XianyuItemPool, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE xianyu_item_pools
		SET name = $2, description = $3, low_stock_threshold = $4, status = $5, updated_at = NOW()
		WHERE id = $1
		RETURNING `+xianyuItemPoolColumns,
		pool.ID, pool.Name, pool.Description, pool.LowStockThreshold, pool.Status)
	updated, err := scanItemPool(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuItemPoolNotFound
		}
		return nil, fmt.Errorf("update xianyu item pool: %w", err)
	}
	return updated, nil
}

const xianyuProductColumns = `id, account_pk, account_id, item_id, title, spec_name, spec_value, pool_id, binding_status, binding_source, status, last_seen_at, created_at, updated_at`

func scanProduct(row interface{ Scan(...any) error }) (*service.XianyuProduct, error) {
	var p service.XianyuProduct
	var poolID sql.NullInt64
	var lastSeen sql.NullTime
	if err := row.Scan(&p.ID, &p.AccountPK, &p.AccountID, &p.ItemID, &p.Title, &p.SpecName, &p.SpecValue, &poolID, &p.BindingStatus, &p.BindingSource, &p.Status, &lastSeen, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if poolID.Valid {
		p.PoolID = &poolID.Int64
	}
	if lastSeen.Valid {
		p.LastSeenAt = &lastSeen.Time
	}
	return &p, nil
}

func (r *xianyuControlRepository) ListProducts(ctx context.Context) ([]service.XianyuProduct, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+xianyuProductColumns+` FROM xianyu_products ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list xianyu products: %w", err)
	}
	defer rows.Close()
	var out []service.XianyuProduct
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *xianyuControlRepository) ListProductsByAccount(ctx context.Context, accountPK int64) ([]service.XianyuProduct, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+xianyuProductColumns+` FROM xianyu_products WHERE account_pk = $1 ORDER BY id`, accountPK)
	if err != nil {
		return nil, fmt.Errorf("list xianyu products by account: %w", err)
	}
	defer rows.Close()
	var out []service.XianyuProduct
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *xianyuControlRepository) GetProductByIdentity(ctx context.Context, accountPK int64, itemID, specName, specValue string) (*service.XianyuProduct, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+xianyuProductColumns+`
		FROM xianyu_products
		WHERE account_pk = $1 AND item_id = $2 AND spec_name = $3 AND spec_value = $4`,
		accountPK, itemID, specName, specValue)
	p, err := scanProduct(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuProductNotFound
		}
		return nil, fmt.Errorf("get xianyu product by identity: %w", err)
	}
	return p, nil
}

func (r *xianyuControlRepository) UpsertProduct(ctx context.Context, product service.XianyuProduct) (*service.XianyuProduct, error) {
	if product.BindingStatus == "" {
		product.BindingStatus = service.XianyuBindingStatusUnmapped
	}
	if product.BindingSource == "" {
		product.BindingSource = service.XianyuBindingSourceAutoNew
	}
	if product.Status == "" {
		product.Status = service.XianyuProductStatusActive
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO xianyu_products
			(account_pk, account_id, item_id, title, spec_name, spec_value, pool_id, binding_status, binding_source, status, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (account_pk, item_id, spec_name, spec_value) DO UPDATE SET
			account_id = EXCLUDED.account_id,
			title = EXCLUDED.title,
			spec_name = EXCLUDED.spec_name,
			spec_value = EXCLUDED.spec_value,
			last_seen_at = EXCLUDED.last_seen_at,
			updated_at = NOW()
		RETURNING `+xianyuProductColumns,
		product.AccountPK, product.AccountID, product.ItemID, product.Title, product.SpecName, product.SpecValue,
		nullableInt64(product.PoolID), product.BindingStatus, product.BindingSource, product.Status, nullableTime(product.LastSeenAt))
	saved, err := scanProduct(row)
	if err != nil {
		return nil, fmt.Errorf("upsert xianyu product: %w", err)
	}
	return saved, nil
}

func (r *xianyuControlRepository) UpdateProduct(ctx context.Context, product service.XianyuProduct) (*service.XianyuProduct, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE xianyu_products
		SET title = $2, pool_id = $3, binding_status = $4, binding_source = $5, status = $6, updated_at = NOW()
		WHERE id = $1
		RETURNING `+xianyuProductColumns,
		product.ID, product.Title, nullableInt64(product.PoolID), product.BindingStatus, product.BindingSource, product.Status)
	updated, err := scanProduct(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuProductNotFound
		}
		return nil, fmt.Errorf("update xianyu product: %w", err)
	}
	return updated, nil
}

func (r *xianyuControlRepository) UpdateProductBinding(ctx context.Context, productID int64, bindingStatus, bindingSource string, poolID *int64) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE xianyu_products
		SET pool_id = $2, binding_status = $3, binding_source = $4, updated_at = NOW()
		WHERE id = $1`,
		productID, nullableInt64(poolID), bindingStatus, bindingSource)
	if err != nil {
		return fmt.Errorf("update xianyu product binding: %w", err)
	}
	return nil
}

const xianyuBindingRuleColumns = `id, priority, account_pk, match_type, keyword, pool_id, status, created_at, updated_at`

func scanBindingRule(row interface{ Scan(...any) error }) (*service.XianyuBindingRule, error) {
	var rl service.XianyuBindingRule
	if err := row.Scan(&rl.ID, &rl.Priority, &rl.AccountPK, &rl.MatchType, &rl.Keyword, &rl.PoolID, &rl.Status, &rl.CreatedAt, &rl.UpdatedAt); err != nil {
		return nil, err
	}
	return &rl, nil
}

func (r *xianyuControlRepository) ListBindingRules(ctx context.Context) ([]service.XianyuBindingRule, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+xianyuBindingRuleColumns+` FROM xianyu_binding_rules ORDER BY priority, id`)
	if err != nil {
		return nil, fmt.Errorf("list xianyu binding rules: %w", err)
	}
	defer rows.Close()
	var out []service.XianyuBindingRule
	for rows.Next() {
		rl, err := scanBindingRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rl)
	}
	return out, rows.Err()
}

func (r *xianyuControlRepository) CreateBindingRule(ctx context.Context, rule service.XianyuBindingRule) (*service.XianyuBindingRule, error) {
	if rule.Status == "" {
		rule.Status = "active"
	}
	if rule.MatchType == "" {
		rule.MatchType = service.XianyuBindingRuleKeyword
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO xianyu_binding_rules (priority, account_pk, match_type, keyword, pool_id, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+xianyuBindingRuleColumns,
		rule.Priority, rule.AccountPK, rule.MatchType, rule.Keyword, rule.PoolID, rule.Status)
	created, err := scanBindingRule(row)
	if err != nil {
		return nil, fmt.Errorf("create xianyu binding rule: %w", err)
	}
	return created, nil
}

func (r *xianyuControlRepository) UpdateBindingRule(ctx context.Context, rule service.XianyuBindingRule) (*service.XianyuBindingRule, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE xianyu_binding_rules
		SET priority = $2, account_pk = $3, match_type = $4, keyword = $5, pool_id = $6, status = $7, updated_at = NOW()
		WHERE id = $1
		RETURNING `+xianyuBindingRuleColumns,
		rule.ID, rule.Priority, rule.AccountPK, rule.MatchType, rule.Keyword, rule.PoolID, rule.Status)
	updated, err := scanBindingRule(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuBindingRuleNotFound
		}
		return nil, fmt.Errorf("update xianyu binding rule: %w", err)
	}
	return updated, nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// PoolStockCounts 返回池库存凭证的剩余/已用/禁用数量。
func (r *xianyuControlRepository) PoolStockCounts(ctx context.Context, poolSlug string) (remaining, used, disabled int, err error) {
	err = r.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = 'unused'),
			COUNT(*) FILTER (WHERE status = 'used'),
			COUNT(*) FILTER (WHERE status = 'disabled')
		FROM redeem_codes
		WHERE type = 'xianyu_delivery' AND notes = $1`, service.XianyuPoolNote(poolSlug)).
		Scan(&remaining, &used, &disabled)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("pool stock counts: %w", err)
	}
	return remaining, used, disabled, nil
}

// DeliveryStats 返回 since 之后 sent/failed 的发货记录数。
func (r *xianyuControlRepository) DeliveryStats(ctx context.Context, since time.Time) (sent, failed int, err error) {
	err = r.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE delivery_status = 'sent' AND created_at >= $1),
			COUNT(*) FILTER (WHERE delivery_status = 'failed' AND created_at >= $1)
		FROM xianyu_order_claims`, since).Scan(&sent, &failed)
	if err != nil {
		return 0, 0, fmt.Errorf("delivery stats: %w", err)
	}
	return sent, failed, nil
}

// PendingDeliveryCount 返回待处理发货数量。
func (r *xianyuControlRepository) PendingDeliveryCount(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM xianyu_order_claims WHERE delivery_status = 'pending'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("pending delivery count: %w", err)
	}
	return n, nil
}

// normalizeXianyuSpecKey 对规格键值做 TrimSpace，空串统一为空字符串。
func normalizeXianyuSpecKey(v string) string {
	return strings.TrimSpace(v)
}
