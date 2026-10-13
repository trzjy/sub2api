package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/quotakey"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// QuotaKeyRepository 实现 service.QuotaKeyRepository，提供与 accounts 表无关的
// quota_keys 基础 CRUD（C3.1 骨架范围）。
//
// 严禁声明/使用任何 account 谓词或 accounts 表查询（C2/C3.2 范围，禁区）：
// 激活码与账号的关联（quota_key_id 外键、accounts 反向边）由后续卡片负责，
// 本文件不得触碰。
type quotaKeyRepository struct {
	client *ent.Client
}

// NewQuotaKeyRepository 构造基于 ent 的 QuotaKeyRepository。
func NewQuotaKeyRepository(client *ent.Client) service.QuotaKeyRepository {
	return &quotaKeyRepository{client: client}
}

// GetByID 按主键读取单个激活码。
func (r *quotaKeyRepository) GetByID(ctx context.Context, quotaKeyID int64) (*ent.QuotaKey, error) {
	return r.client.QuotaKey.Get(ctx, quotaKeyID)
}

// Create 新建激活码。仅写入 quota_keys 自有字段，不触碰 accounts。
func (r *quotaKeyRepository) Create(ctx context.Context, input service.CreateQuotaKeyInput) (*ent.QuotaKey, error) {
	q := r.client.QuotaKey.Create().
		SetCodeEncrypted(input.CodeEncrypted).
		SetExhausted(input.Exhausted)
	if input.CodeMAC != "" {
		q = q.SetCodeMAC(input.CodeMAC)
	}
	if input.CodeLast4 != "" {
		q = q.SetCodeLast4(input.CodeLast4)
	}
	if input.Status != "" {
		q = q.SetStatus(input.Status)
	}
	if input.ExpiresAt != nil {
		q = q.SetNillableExpiresAt(input.ExpiresAt)
	}
	if input.Notes != nil {
		q = q.SetNillableNotes(input.Notes)
	}
	return q.Save(ctx)
}

// UpdateStatusFields 在事务内更新拉号状态相关字段。tx 为调用方已开启的事务。
func (r *quotaKeyRepository) UpdateStatusFields(ctx context.Context, tx *ent.Tx, id int64, status string, exhausted bool, lastError *string, lastRefreshAt *time.Time) error {
	q := tx.QuotaKey.UpdateOneID(id).
		SetStatus(status).
		SetExhausted(exhausted)
	if lastError != nil {
		q = q.SetNillableLastRefreshError(lastError)
	} else {
		q = q.ClearLastRefreshError()
	}
	if lastRefreshAt != nil {
		q = q.SetNillableLastRefreshAt(lastRefreshAt)
	}
	_, err := q.Save(ctx)
	return err
}

// HardDelete 在事务内硬删除激活码（不级联 accounts，C2 负责删除编排）。
func (r *quotaKeyRepository) HardDelete(ctx context.Context, tx *ent.Tx, id int64) error {
	return tx.QuotaKey.DeleteOneID(id).Exec(ctx)
}

// UpdateCredentials 写入加密后的凭据形态：code_encrypted / code_mac / code_last4。
func (r *quotaKeyRepository) UpdateCredentials(ctx context.Context, id int64, codeEncrypted, codeMAC, codeLast4 string) error {
	_, err := r.client.QuotaKey.UpdateOneID(id).
		SetCodeEncrypted(codeEncrypted).
		SetCodeMAC(codeMAC).
		SetCodeLast4(codeLast4).
		Save(ctx)
	return err
}

// ListExpired 返回 expires_at 非空且早于 now 的激活码（sweep_expired 用）。
func (r *quotaKeyRepository) ListExpired(ctx context.Context, now time.Time) ([]*ent.QuotaKey, error) {
	return r.client.QuotaKey.Query().
		Where(
			quotakey.ExpiresAtNotNil(),
			quotakey.ExpiresAtLT(now),
		).
		All(ctx)
}

// ListByStatus 按状态列出激活码（sweep / reconciler 用）。
func (r *quotaKeyRepository) ListByStatus(ctx context.Context, status string) ([]*ent.QuotaKey, error) {
	return r.client.QuotaKey.Query().
		Where(quotakey.Status(status)).
		All(ctx)
}
