package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// accountModelCapabilityRepository 以原生 SQL 访问 account_model_capabilities 表。
//
// 选用原生 SQL 而非 ent 客户端：本表为新增表，未纳入 ent 代码生成（派发单 B 只新增
// schema 文件、不重跑 generate，避免触碰 backend/ent/ 既有生成文件）。对齐
// auth_cache_invalidation_outbox_repo.go 的既有原生 SQL repo 模式。
type accountModelCapabilityRepository struct {
	db *sql.DB
}

// NewAccountModelCapabilityRepository 创建能力标记仓库。
func NewAccountModelCapabilityRepository(db *sql.DB) service.AccountModelCapabilityRepository {
	return &accountModelCapabilityRepository{db: db}
}

// Get 按 (accountID, upstreamModel, protocol) 三元组读取一条标记。
// 未命中返回 (nil, nil)；其他错误返回错误。
func (r *accountModelCapabilityRepository) Get(
	ctx context.Context,
	accountID int64,
	upstreamModel, protocol string,
) (*model.AccountModelCapability, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil account model capability database")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, account_id, upstream_model, protocol, supports_vision, source, detected_at, updated_at
		FROM account_model_capabilities
		WHERE account_id = $1 AND upstream_model = $2 AND protocol = $3
	`, accountID, upstreamModel, protocol)

	cap, err := scanAccountModelCapability(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cap, nil
}

// Upsert 按唯一键 (account_id, upstream_model, protocol) 插入或更新标记。
// 返回更新后的记录（含数据库侧生成的 id / updated_at）。
func (r *accountModelCapabilityRepository) Upsert(
	ctx context.Context,
	cap *model.AccountModelCapability,
) (*model.AccountModelCapability, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil account model capability database")
	}
	if cap == nil {
		return nil, errors.New("nil account model capability record")
	}

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO account_model_capabilities (
			account_id, upstream_model, protocol, supports_vision, source, detected_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, NOW())
	ON CONFLICT (account_id, upstream_model, protocol)
	DO UPDATE SET
		supports_vision = CASE
			WHEN account_model_capabilities.source = 'manual' AND EXCLUDED.source = 'detect'
			THEN account_model_capabilities.supports_vision
			ELSE EXCLUDED.supports_vision END,
		source = CASE
			WHEN account_model_capabilities.source = 'manual' AND EXCLUDED.source = 'detect'
			THEN account_model_capabilities.source
			ELSE EXCLUDED.source END,
		detected_at = CASE
			WHEN account_model_capabilities.source = 'manual' AND EXCLUDED.source = 'detect'
			THEN account_model_capabilities.detected_at
			ELSE EXCLUDED.detected_at END,
		updated_at = NOW()
	RETURNING id, account_id, upstream_model, protocol, supports_vision, source, detected_at, updated_at
	`, cap.AccountID, cap.UpstreamModel, cap.Protocol, cap.SupportsVision, cap.Source, cap.DetectedAt)

	upserted, err := scanAccountModelCapability(row)
	if err != nil {
		return nil, err
	}
	return upserted, nil
}

// ListByAccount 返回某账号下的全部能力标记（按 upstream_model, protocol 排序）。
// 无记录返回空切片（非 nil）。
func (r *accountModelCapabilityRepository) ListByAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil account model capability database")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, account_id, upstream_model, protocol, supports_vision, source, detected_at, updated_at
		FROM account_model_capabilities
		WHERE account_id = $1
		ORDER BY upstream_model ASC, protocol ASC
	`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make([]*model.AccountModelCapability, 0)
	for rows.Next() {
		cap, err := scanAccountModelCapabilityRows(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, cap)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteByAccount 删除某账号下的全部能力标记。
func (r *accountModelCapabilityRepository) DeleteByAccount(ctx context.Context, accountID int64) error {
	if r == nil || r.db == nil {
		return errors.New("nil account model capability database")
	}
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM account_model_capabilities
		WHERE account_id = $1
	`, accountID)
	return err
}

// scanAccountModelCapability 扫描单行（QueryRow 结果）。
func scanAccountModelCapability(row *sql.Row) (*model.AccountModelCapability, error) {
	cap := &model.AccountModelCapability{}
	var detectedAt sql.NullTime
	err := row.Scan(
		&cap.ID,
		&cap.AccountID,
		&cap.UpstreamModel,
		&cap.Protocol,
		&cap.SupportsVision,
		&cap.Source,
		&detectedAt,
		&cap.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if detectedAt.Valid {
		value := detectedAt.Time
		cap.DetectedAt = &value
	}
	return cap, nil
}

// scanAccountModelCapabilityRows 扫描一行（Query 结果游标）。
func scanAccountModelCapabilityRows(rows *sql.Rows) (*model.AccountModelCapability, error) {
	cap := &model.AccountModelCapability{}
	var detectedAt sql.NullTime
	err := rows.Scan(
		&cap.ID,
		&cap.AccountID,
		&cap.UpstreamModel,
		&cap.Protocol,
		&cap.SupportsVision,
		&cap.Source,
		&detectedAt,
		&cap.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan account model capability: %w", err)
	}
	if detectedAt.Valid {
		value := detectedAt.Time
		cap.DetectedAt = &value
	}
	return cap, nil
}

var _ service.AccountModelCapabilityRepository = (*accountModelCapabilityRepository)(nil)
