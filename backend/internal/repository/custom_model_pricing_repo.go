package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// customModelPricingRepository 全局自定义模型定价仓储（价格管理中心）。
// 表结构对齐 channel_model_pricing 系列，无 channel_id/platform/time_pricing，
// 增加 enabled/remark/created_by。
type customModelPricingRepository struct {
	db *sql.DB
}

func NewCustomModelPricingRepository(db *sql.DB) service.CustomModelPricingRepository {
	return &customModelPricingRepository{db: db}
}

const customModelPricingColumns = `id, models, billing_mode, input_price, output_price, cache_write_price, cache_read_price,
	 fast_multiplier, flex_multiplier, image_input_price, image_output_price, per_request_price,
	 enabled, remark, created_by, created_at, updated_at`

func (r *customModelPricingRepository) List(ctx context.Context) ([]service.CustomModelPricing, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+customModelPricingColumns+` FROM custom_model_pricing ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list custom model pricing: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries, ids, err := scanCustomModelPricingRows(rows)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		// 空表返回 [] 而非 nil：nil 会被 encoding/json 序列化成 null，
		// 前端按数组消费会直接抛错。
		entries = []service.CustomModelPricing{}
	}
	if len(ids) > 0 {
		intervalMap, err := r.batchLoadIntervals(ctx, ids)
		if err != nil {
			return nil, err
		}
		for i := range entries {
			entries[i].Intervals = intervalMap[entries[i].ID]
		}
	}
	return entries, nil
}

func (r *customModelPricingRepository) GetByID(ctx context.Context, id int64) (*service.CustomModelPricing, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+customModelPricingColumns+` FROM custom_model_pricing WHERE id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("get custom model pricing: %w", err)
	}
	defer func() { _ = rows.Close() }()

	entries, _, err := scanCustomModelPricingRows(rows)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("custom model pricing not found: %d", id)
	}
	entry := &entries[0]
	intervalMap, err := r.batchLoadIntervals(ctx, []int64{entry.ID})
	if err != nil {
		return nil, err
	}
	entry.Intervals = intervalMap[entry.ID]
	return entry, nil
}

func (r *customModelPricingRepository) Create(ctx context.Context, entry *service.CustomModelPricing) error {
	modelsJSON, err := json.Marshal(normalizeCustomModels(entry.Models))
	if err != nil {
		return fmt.Errorf("marshal models: %w", err)
	}
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO custom_model_pricing
		 (models, billing_mode, input_price, output_price, cache_write_price, cache_read_price,
		  fast_multiplier, flex_multiplier, image_input_price, image_output_price, per_request_price,
		  enabled, remark, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 RETURNING id, created_at, updated_at`,
		modelsJSON, normalizeCustomBillingMode(entry.BillingMode),
		entry.InputPrice, entry.OutputPrice, entry.CacheWritePrice, entry.CacheReadPrice,
		entry.FastMultiplier, entry.FlexMultiplier, entry.ImageInputPrice, entry.ImageOutputPrice,
		entry.PerRequestPrice, entry.Enabled, entry.Remark, entry.CreatedBy,
	).Scan(&entry.ID, &entry.CreatedAt, &entry.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert custom model pricing: %w", err)
	}
	if err := r.replaceIntervals(ctx, entry.ID, entry.Intervals); err != nil {
		return err
	}
	return nil
}

func (r *customModelPricingRepository) Update(ctx context.Context, entry *service.CustomModelPricing) error {
	modelsJSON, err := json.Marshal(normalizeCustomModels(entry.Models))
	if err != nil {
		return fmt.Errorf("marshal models: %w", err)
	}
	result, err := r.db.ExecContext(ctx,
		`UPDATE custom_model_pricing
		 SET models = $1, billing_mode = $2, input_price = $3, output_price = $4, cache_write_price = $5,
		     cache_read_price = $6, fast_multiplier = $7, flex_multiplier = $8, image_input_price = $9,
		     image_output_price = $10, per_request_price = $11, enabled = $12, remark = $13, updated_at = NOW()
		 WHERE id = $14`,
		modelsJSON, normalizeCustomBillingMode(entry.BillingMode),
		entry.InputPrice, entry.OutputPrice, entry.CacheWritePrice, entry.CacheReadPrice,
		entry.FastMultiplier, entry.FlexMultiplier, entry.ImageInputPrice, entry.ImageOutputPrice,
		entry.PerRequestPrice, entry.Enabled, entry.Remark, entry.ID,
	)
	if err != nil {
		return fmt.Errorf("update custom model pricing: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("custom model pricing not found: %d", entry.ID)
	}
	if err := r.replaceIntervals(ctx, entry.ID, entry.Intervals); err != nil {
		return err
	}
	return nil
}

func (r *customModelPricingRepository) Delete(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM custom_model_pricing WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete custom model pricing: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("custom model pricing not found: %d", id)
	}
	return nil
}

// replaceIntervals 全量替换条目的区间列表。
func (r *customModelPricingRepository) replaceIntervals(ctx context.Context, pricingID int64, intervals []service.PricingInterval) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM custom_model_pricing_intervals WHERE pricing_id = $1`, pricingID); err != nil {
		return fmt.Errorf("delete old custom intervals: %w", err)
	}
	for i := range intervals {
		iv := &intervals[i]
		iv.PricingID = pricingID
		if err := r.createInterval(ctx, iv); err != nil {
			return err
		}
	}
	return nil
}

func (r *customModelPricingRepository) createInterval(ctx context.Context, iv *service.PricingInterval) error {
	return r.db.QueryRowContext(ctx,
		`INSERT INTO custom_model_pricing_intervals
		 (pricing_id, min_tokens, max_tokens, tier_label, input_price, output_price,
		  cache_write_price, cache_read_price, input_multiplier, output_multiplier,
		  cache_write_multiplier, cache_read_multiplier, per_request_price, sort_order)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 RETURNING id, created_at, updated_at`,
		iv.PricingID, iv.MinTokens, iv.MaxTokens, iv.TierLabel,
		iv.InputPrice, iv.OutputPrice, iv.CacheWritePrice, iv.CacheReadPrice,
		iv.InputMultiplier, iv.OutputMultiplier, iv.CacheWriteMultiplier, iv.CacheReadMultiplier,
		iv.PerRequestPrice, iv.SortOrder,
	).Scan(&iv.ID, &iv.CreatedAt, &iv.UpdatedAt)
}

func (r *customModelPricingRepository) batchLoadIntervals(ctx context.Context, pricingIDs []int64) (map[int64][]service.PricingInterval, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, pricing_id, min_tokens, max_tokens, tier_label,
		        input_price, output_price, cache_write_price, cache_read_price,
		        input_multiplier, output_multiplier, cache_write_multiplier, cache_read_multiplier,
		        per_request_price, sort_order, created_at, updated_at
		 FROM custom_model_pricing_intervals
		 WHERE pricing_id = ANY($1) ORDER BY pricing_id, sort_order, id`,
		pq.Array(pricingIDs))
	if err != nil {
		return nil, fmt.Errorf("batch load custom intervals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	intervalMap := make(map[int64][]service.PricingInterval, len(pricingIDs))
	for rows.Next() {
		var iv service.PricingInterval
		if err := rows.Scan(
			&iv.ID, &iv.PricingID, &iv.MinTokens, &iv.MaxTokens, &iv.TierLabel,
			&iv.InputPrice, &iv.OutputPrice, &iv.CacheWritePrice, &iv.CacheReadPrice,
			&iv.InputMultiplier, &iv.OutputMultiplier, &iv.CacheWriteMultiplier, &iv.CacheReadMultiplier,
			&iv.PerRequestPrice, &iv.SortOrder, &iv.CreatedAt, &iv.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan custom interval: %w", err)
		}
		intervalMap[iv.PricingID] = append(intervalMap[iv.PricingID], iv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate custom intervals: %w", err)
	}
	return intervalMap, nil
}

func scanCustomModelPricingRows(rows *sql.Rows) ([]service.CustomModelPricing, []int64, error) {
	var result []service.CustomModelPricing
	var ids []int64
	for rows.Next() {
		var p service.CustomModelPricing
		var modelsJSON []byte
		var remark sql.NullString
		if err := rows.Scan(
			&p.ID, &modelsJSON, &p.BillingMode,
			&p.InputPrice, &p.OutputPrice, &p.CacheWritePrice, &p.CacheReadPrice,
			&p.FastMultiplier, &p.FlexMultiplier, &p.ImageInputPrice, &p.ImageOutputPrice,
			&p.PerRequestPrice, &p.Enabled, &remark, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, nil, fmt.Errorf("scan custom model pricing: %w", err)
		}
		if err := json.Unmarshal(modelsJSON, &p.Models); err != nil {
			p.Models = []string{}
		}
		p.Remark = strings.TrimSpace(remark.String)
		ids = append(ids, p.ID)
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate custom model pricing: %w", err)
	}
	return result, ids, nil
}

func normalizeCustomModels(models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if trimmed := strings.TrimSpace(m); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func normalizeCustomBillingMode(mode service.BillingMode) service.BillingMode {
	if mode == "" {
		return service.BillingModeToken
	}
	return mode
}
