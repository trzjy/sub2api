package repository

import (
	"context"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/quotaupstream"
)

// quotaUpstreamRepository 提供 quota_upstreams 注册表的极简 CRUD（CARD-KC-16）：
//   - 按 key 取单条（含软删过滤，由 SoftDeleteMixin 自动附加 deleted_at IS NULL）；
//   - 列出全部（供管理端选择与显示）；
//   - 更新显示名 / 续费 URL / 备注（上游标识 key 不可改，故不提供 setter）。
//
// 不触碰 accounts / quota_keys，仅读写 quota_upstreams 表。
type quotaUpstreamRepository struct {
	client *dbent.Client
}

// NewQuotaUpstreamRepository 构造基于 ent 的上游注册表仓储。
func NewQuotaUpstreamRepository(client *dbent.Client) *quotaUpstreamRepository {
	return &quotaUpstreamRepository{client: client}
}

// GetByKey 按上游稳定标识 key 取单条（软删行自动被 SoftDeleteMixin 过滤）。
// 找不到返回 (nil, err)；调用方据错误判定是否存在。
func (r *quotaUpstreamRepository) GetByKey(ctx context.Context, key string) (*dbent.QuotaUpstream, error) {
	u, err := r.client.QuotaUpstream.Query().
		Where(quotaupstream.Key(key)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_upstream_repo: get by key %q: %w", key, err)
	}
	return u, nil
}

// List 列出全部上游条目（软删行自动被过滤）。供管理端选择与显示。
func (r *quotaUpstreamRepository) List(ctx context.Context) ([]*dbent.QuotaUpstream, error) {
	items, err := r.client.QuotaUpstream.Query().All(ctx)
	if err != nil {
		return nil, fmt.Errorf("quota_upstream_repo: list: %w", err)
	}
	return items, nil
}

// Update 更新上游的显示名 / 续费 URL / 备注。
//
// 上层指针 nil 表示省略该字段；指针指向的 nil（内层）表示显式清空（仅 renewal_url / notes 可空）。
// 上游标识 key 不可改，本方法不提供 key 的 setter。
func (r *quotaUpstreamRepository) Update(ctx context.Context, id int64, name string, renewalURL **string, notes **string) error {
	q := r.client.QuotaUpstream.UpdateOneID(id).SetName(name)
	if renewalURL != nil {
		if *renewalURL == nil {
			q = q.ClearRenewalURL()
		} else {
			q = q.SetRenewalURL(**renewalURL)
		}
	}
	if notes != nil {
		if *notes == nil {
			q = q.ClearNotes()
		} else {
			q = q.SetNotes(**notes)
		}
	}
	if _, err := q.Save(ctx); err != nil {
		return fmt.Errorf("quota_upstream_repo: update id=%d: %w", id, err)
	}
	return nil
}
