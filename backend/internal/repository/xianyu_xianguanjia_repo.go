package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type xianGuanJiaRepository struct {
	db *sql.DB
}

// NewXianGuanJiaRepository 创建闲管家配置数据访问。
func NewXianGuanJiaRepository(db *sql.DB) service.XianGuanJiaRepository {
	return &xianGuanJiaRepository{db: db}
}

const xianGuanJiaConfigColumns = `id, base_url, app_id, app_secret_encrypted, mch_id, mch_secret_encrypted, push_url, status, health_status, last_checked_at, created_at, updated_at`

func scanXianGuanJiaConfig(row interface{ Scan(...any) error }) (*service.XianGuanJiaConfig, error) {
	var c service.XianGuanJiaConfig
	var lastChecked sql.NullTime
	if err := row.Scan(&c.ID, &c.BaseURL, &c.AppID, &c.AppSecretEncrypted, &c.MchID, &c.MchSecretEncrypted, &c.PushURL, &c.Status, &c.HealthStatus, &lastChecked, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	if lastChecked.Valid {
		c.LastCheckedAt = &lastChecked.Time
	}
	return &c, nil
}

func (r *xianGuanJiaRepository) ListConfigs(ctx context.Context) ([]service.XianGuanJiaConfig, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+xianGuanJiaConfigColumns+` FROM xianyu_xianguanjia_config ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list xianguanjia configs: %w", err)
	}
	defer rows.Close()
	out := make([]service.XianGuanJiaConfig, 0)
	for rows.Next() {
		c, err := scanXianGuanJiaConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (r *xianGuanJiaRepository) GetActiveConfig(ctx context.Context) (*service.XianGuanJiaConfig, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+xianGuanJiaConfigColumns+`
		FROM xianyu_xianguanjia_config
		WHERE status = $1
		ORDER BY id LIMIT 1`, service.XianGuanJiaConfigStatusActive)
	c, err := scanXianGuanJiaConfig(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuWorkerConfigNotFound
		}
		return nil, fmt.Errorf("get active xianguanjia config: %w", err)
	}
	return c, nil
}

func (r *xianGuanJiaRepository) CreateConfig(ctx context.Context, cfg service.XianGuanJiaConfig) (*service.XianGuanJiaConfig, error) {
	if cfg.Status == "" {
		cfg.Status = service.XianGuanJiaConfigStatusDisabled
	}
	if cfg.HealthStatus == "" {
		cfg.HealthStatus = service.XianGuanJiaHealthUnknown
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO xianyu_xianguanjia_config (base_url, app_id, app_secret_encrypted, mch_id, mch_secret_encrypted, push_url, status, health_status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+xianGuanJiaConfigColumns,
		cfg.BaseURL, cfg.AppID, cfg.AppSecretEncrypted, cfg.MchID, cfg.MchSecretEncrypted, cfg.PushURL, cfg.Status, cfg.HealthStatus)
	c, err := scanXianGuanJiaConfig(row)
	if err != nil {
		return nil, fmt.Errorf("create xianguanjia config: %w", err)
	}
	return c, nil
}

func (r *xianGuanJiaRepository) UpdateConfig(ctx context.Context, cfg service.XianGuanJiaConfig) (*service.XianGuanJiaConfig, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE xianyu_xianguanjia_config
		SET base_url = $2, app_id = $3, app_secret_encrypted = $4, mch_id = $5, mch_secret_encrypted = $6,
		    push_url = $7, status = $8, health_status = $9, last_checked_at = $10, updated_at = NOW()
		WHERE id = $1
		RETURNING `+xianGuanJiaConfigColumns,
		cfg.ID, cfg.BaseURL, cfg.AppID, cfg.AppSecretEncrypted, cfg.MchID, cfg.MchSecretEncrypted, cfg.PushURL, cfg.Status, cfg.HealthStatus, nullableTime(cfg.LastCheckedAt))
	c, err := scanXianGuanJiaConfig(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrXianyuWorkerConfigNotFound
		}
		return nil, fmt.Errorf("update xianguanjia config: %w", err)
	}
	return c, nil
}
