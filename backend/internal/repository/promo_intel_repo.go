package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/promointelitem"
	"github.com/Wei-Shaw/sub2api/ent/promointelsource"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// promoIntelRepository 实现 service.PromoIntelRepository。
//
// 选型说明：
//   - 源 CRUD 与情报查询走 ent，复用项目事务上下文；
//   - ListDueSources（按行级 interval 判到期）与 UpsertItem（ON CONFLICT 保分诊状态）
//     用原生 SQL 表达，ent 难以简洁等价表达。
type promoIntelRepository struct {
	client *dbent.Client
	db     *sql.DB
}

// NewPromoIntelRepository 创建仓储实例。
func NewPromoIntelRepository(client *dbent.Client, db *sql.DB) service.PromoIntelRepository {
	return &promoIntelRepository{client: client, db: db}
}

// ---------- 资讯源 ----------

func (r *promoIntelRepository) CreateSource(ctx context.Context, src *service.PromoIntelSource) error {
	client := clientFromContext(ctx, r.client)
	created, err := client.PromoIntelSource.Create().
		SetName(src.Name).
		SetVendor(src.Vendor).
		SetCategory(src.Category).
		SetURL(src.URL).
		SetFetchIntervalMinutes(src.FetchIntervalMinutes).
		SetEnabled(src.Enabled).
		SetLlmExtract(src.LLMExtract).
		SetNotes(src.Notes).
		SetCreatedBy(src.CreatedBy).
		Save(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrPromoIntelSourceNotFound, nil)
	}
	promoIntelSourceWriteBack(src, created)
	return nil
}

func (r *promoIntelRepository) GetSourceByID(ctx context.Context, id int64) (*service.PromoIntelSource, error) {
	row, err := r.client.PromoIntelSource.Query().Where(promointelsource.IDEQ(id)).Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrPromoIntelSourceNotFound, nil)
	}
	return promoIntelSourceFromEnt(row), nil
}

func (r *promoIntelRepository) GetSourceByName(ctx context.Context, name string) (*service.PromoIntelSource, error) {
	row, err := r.client.PromoIntelSource.Query().Where(promointelsource.NameEQ(name)).Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrPromoIntelSourceNotFound, nil)
	}
	return promoIntelSourceFromEnt(row), nil
}

func (r *promoIntelRepository) UpdateSource(ctx context.Context, src *service.PromoIntelSource) error {
	builder := r.client.PromoIntelSource.UpdateOneID(src.ID).
		SetName(src.Name).
		SetVendor(src.Vendor).
		SetCategory(src.Category).
		SetURL(src.URL).
		SetFetchIntervalMinutes(src.FetchIntervalMinutes).
		SetEnabled(src.Enabled).
		SetLlmExtract(src.LLMExtract).
		SetNotes(src.Notes).
		SetLastExtractedHash(src.LastExtractedHash)
	updated, err := builder.Save(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrPromoIntelSourceNotFound, nil)
	}
	promoIntelSourceWriteBack(src, updated)
	return nil
}

func (r *promoIntelRepository) DeleteSource(ctx context.Context, id int64) error {
	err := r.client.PromoIntelSource.DeleteOneID(id).Exec(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrPromoIntelSourceNotFound, nil)
	}
	return nil
}

func (r *promoIntelRepository) ListSources(ctx context.Context, params service.PromoIntelSourceListParams) ([]*service.PromoIntelSource, int64, error) {
	q := r.client.PromoIntelSource.Query()
	if params.Vendor != "" {
		q = q.Where(promointelsource.VendorEQ(params.Vendor))
	}
	if params.Enabled != nil {
		q = q.Where(promointelsource.EnabledEQ(*params.Enabled))
	}
	if s := strings.TrimSpace(params.Search); s != "" {
		q = q.Where(promointelsource.Or(
			promointelsource.NameContainsFold(s),
			promointelsource.URLContainsFold(s),
			promointelsource.NotesContainsFold(s),
		))
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count promo intel sources: %w", err)
	}
	rows, err := q.Order(dbent.Asc(promointelsource.FieldName)).
		Offset((params.Page - 1) * params.PageSize).
		Limit(params.PageSize).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list promo intel sources: %w", err)
	}
	out := make([]*service.PromoIntelSource, 0, len(rows))
	for _, row := range rows {
		out = append(out, promoIntelSourceFromEnt(row))
	}
	return out, int64(total), nil
}

func (r *promoIntelRepository) ListDueSources(ctx context.Context, now time.Time, limit int) ([]*service.PromoIntelSource, error) {
	if limit <= 0 {
		limit = 20
	}
	// 行级 interval 判到期：last_fetched_at IS NULL OR last_fetched_at <= now - interval。
	// $1 必须显式 ::timestamptz：pq 以 unknown 传参时 PG 会把 "$1 - make_interval(...)"
	// 解析成 interval - interval（报 "operator does not exist: timestamp with time
	// zone <= interval"），扫描循环每轮失败。
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, vendor, category, url, fetch_interval_minutes, enabled, llm_extract,
		       notes, last_fetched_at, last_extracted_hash, last_status, last_error,
		       created_by, created_at, updated_at
		FROM promo_intel_sources
		WHERE enabled = TRUE
		  AND (last_fetched_at IS NULL OR last_fetched_at <= $1::timestamptz - make_interval(mins => fetch_interval_minutes))
		ORDER BY last_fetched_at ASC NULLS FIRST, id ASC
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list due promo intel sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]*service.PromoIntelSource, 0, limit)
	for rows.Next() {
		var src service.PromoIntelSource
		var lastFetched sql.NullTime
		if err := rows.Scan(&src.ID, &src.Name, &src.Vendor, &src.Category, &src.URL,
			&src.FetchIntervalMinutes, &src.Enabled, &src.LLMExtract,
			&src.Notes, &lastFetched, &src.LastExtractedHash, &src.LastStatus, &src.LastError,
			&src.CreatedBy, &src.CreatedAt, &src.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan due promo intel source: %w", err)
		}
		if lastFetched.Valid {
			t := lastFetched.Time
			src.LastFetchedAt = &t
		}
		out = append(out, &src)
	}
	return out, rows.Err()
}

func (r *promoIntelRepository) MarkSourceFetched(ctx context.Context, id int64, status, lastErr string, extractedHash *string, at time.Time) error {
	builder := r.client.PromoIntelSource.UpdateOneID(id).
		SetLastFetchedAt(at).
		SetLastStatus(status).
		SetLastError(lastErr)
	if extractedHash != nil {
		builder = builder.SetLastExtractedHash(*extractedHash)
	}
	if err := builder.Exec(ctx); err != nil {
		return translatePersistenceError(err, service.ErrPromoIntelSourceNotFound, nil)
	}
	return nil
}

// ---------- 情报条目 ----------

func (r *promoIntelRepository) UpsertItem(ctx context.Context, item *service.PromoIntelItem) (bool, error) {
	if strings.TrimSpace(item.Fingerprint) == "" {
		return false, errors.New("promo intel item fingerprint is required")
	}
	digestDate := any(nil)
	if item.DigestDate != nil {
		digestDate = *item.DigestDate
	}
	// 冲突刷新白名单：summary/details/discount_info/valid_until/url/relevance/
	// raw_excerpt/extract_status/vendor/category/source_fetched_at——故意排除
	// status/digest_date/created_at，重复发现绝不覆盖管理员分诊与首次发现日。
	// RETURNING (xmax = 0)：PostgreSQL 惯用法，true = 新插入，false = 冲突更新。
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO promo_intel_items (
			source_id, vendor, category, title, summary, details, discount_info,
			valid_until, url, relevance, status, fingerprint, raw_excerpt,
			extract_status, digest_date, source_fetched_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (fingerprint) DO UPDATE SET
			vendor = EXCLUDED.vendor,
			category = EXCLUDED.category,
			summary = EXCLUDED.summary,
			details = EXCLUDED.details,
			discount_info = EXCLUDED.discount_info,
			valid_until = EXCLUDED.valid_until,
			url = EXCLUDED.url,
			relevance = EXCLUDED.relevance,
			raw_excerpt = EXCLUDED.raw_excerpt,
			extract_status = EXCLUDED.extract_status,
			source_fetched_at = EXCLUDED.source_fetched_at,
			updated_at = NOW()
		RETURNING id, created_at, updated_at, (xmax = 0) AS inserted`,
		item.SourceID, item.Vendor, item.Category, item.Title, item.Summary, item.Details,
		item.DiscountInfo, item.ValidUntil, item.URL, item.Relevance, item.Status,
		item.Fingerprint, item.RawExcerpt, item.ExtractStatus, digestDate, item.SourceFetchedAt)

	var created bool
	err := row.Scan(&item.ID, &item.CreatedAt, &item.UpdatedAt, &created)
	if err != nil {
		return false, fmt.Errorf("upsert promo intel item: %w", err)
	}
	return created, nil
}

func (r *promoIntelRepository) GetItemByID(ctx context.Context, id int64) (*service.PromoIntelItem, error) {
	row, err := r.client.PromoIntelItem.Query().Where(promointelitem.IDEQ(id)).Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrPromoIntelItemNotFound, nil)
	}
	return promoIntelItemFromEnt(row, ""), nil
}

func (r *promoIntelRepository) UpdateItemStatus(ctx context.Context, id int64, status string) error {
	err := r.client.PromoIntelItem.UpdateOneID(id).SetStatus(status).Exec(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrPromoIntelItemNotFound, nil)
	}
	return nil
}

func (r *promoIntelRepository) DeletePendingItemsBySource(ctx context.Context, sourceID int64) (int64, error) {
	deleted, err := r.client.PromoIntelItem.Delete().
		Where(promointelitem.And(
			promointelitem.SourceIDEQ(sourceID),
			promointelitem.ExtractStatusEQ(service.PromoIntelExtractPending),
		)).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete pending promo intel items: %w", err)
	}
	return int64(deleted), nil
}

func (r *promoIntelRepository) ListItems(ctx context.Context, params service.PromoIntelItemListParams) ([]*service.PromoIntelItem, int64, error) {
	q := r.client.PromoIntelItem.Query()
	if params.Vendor != "" {
		q = q.Where(promointelitem.VendorEQ(params.Vendor))
	}
	if params.Category != "" {
		q = q.Where(promointelitem.CategoryEQ(params.Category))
	}
	if params.Relevance != "" {
		q = q.Where(promointelitem.RelevanceEQ(params.Relevance))
	}
	if params.Status != "" {
		q = q.Where(promointelitem.StatusEQ(params.Status))
	}
	if params.DigestDate != "" {
		day, err := time.ParseInLocation("2006-01-02", params.DigestDate, time.UTC)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid digest date: %w", err)
		}
		q = q.Where(promointelitem.DigestDateEQ(day))
	}
	if s := strings.TrimSpace(params.Search); s != "" {
		q = q.Where(promointelitem.Or(
			promointelitem.TitleContainsFold(s),
			promointelitem.SummaryContainsFold(s),
			promointelitem.DetailsContainsFold(s),
			promointelitem.DiscountInfoContainsFold(s),
		))
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count promo intel items: %w", err)
	}
	rows, err := q.Order(dbent.Desc(promointelitem.FieldID)).
		Offset((params.Page - 1) * params.PageSize).
		Limit(params.PageSize).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list promo intel items: %w", err)
	}

	// 批量补源名（分页 ≤200 行，一次 IN 查询）。
	sourceNames := make(map[int64]string, len(rows))
	if len(rows) > 0 {
		ids := make([]int64, 0, len(rows))
		seen := make(map[int64]struct{}, len(rows))
		for _, row := range rows {
			if _, dup := seen[row.SourceID]; dup {
				continue
			}
			seen[row.SourceID] = struct{}{}
			ids = append(ids, row.SourceID)
		}
		srcRows, err := r.client.PromoIntelSource.Query().
			Where(promointelsource.IDIn(ids...)).All(ctx)
		if err == nil {
			for _, srcRow := range srcRows {
				sourceNames[srcRow.ID] = srcRow.Name
			}
		}
	}

	out := make([]*service.PromoIntelItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, promoIntelItemFromEnt(row, sourceNames[row.SourceID]))
	}
	return out, int64(total), nil
}

// ---------- ent → service 映射 ----------

func promoIntelSourceWriteBack(dst *service.PromoIntelSource, row *dbent.PromoIntelSource) {
	dst.ID = row.ID
	dst.LastFetchedAt = row.LastFetchedAt
	dst.LastExtractedHash = row.LastExtractedHash
	dst.LastStatus = row.LastStatus
	dst.LastError = row.LastError
	dst.CreatedAt = row.CreatedAt
	dst.UpdatedAt = row.UpdatedAt
}

func promoIntelSourceFromEnt(row *dbent.PromoIntelSource) *service.PromoIntelSource {
	return &service.PromoIntelSource{
		ID:                   row.ID,
		Name:                 row.Name,
		Vendor:               row.Vendor,
		Category:             row.Category,
		URL:                  row.URL,
		FetchIntervalMinutes: row.FetchIntervalMinutes,
		Enabled:              row.Enabled,
		LLMExtract:           row.LlmExtract,
		Notes:                row.Notes,
		LastFetchedAt:        row.LastFetchedAt,
		LastExtractedHash:    row.LastExtractedHash,
		LastStatus:           row.LastStatus,
		LastError:            row.LastError,
		CreatedBy:            row.CreatedBy,
		CreatedAt:            row.CreatedAt,
		UpdatedAt:            row.UpdatedAt,
	}
}

func promoIntelItemFromEnt(row *dbent.PromoIntelItem, sourceName string) *service.PromoIntelItem {
	return &service.PromoIntelItem{
		ID:              row.ID,
		SourceID:        row.SourceID,
		SourceName:      sourceName,
		Vendor:          row.Vendor,
		Category:        row.Category,
		Title:           row.Title,
		Summary:         row.Summary,
		Details:         row.Details,
		DiscountInfo:    row.DiscountInfo,
		ValidUntil:      row.ValidUntil,
		URL:             row.URL,
		Relevance:       row.Relevance,
		Status:          row.Status,
		Fingerprint:     row.Fingerprint,
		RawExcerpt:      row.RawExcerpt,
		ExtractStatus:   row.ExtractStatus,
		DigestDate:      row.DigestDate,
		SourceFetchedAt: row.SourceFetchedAt,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}
