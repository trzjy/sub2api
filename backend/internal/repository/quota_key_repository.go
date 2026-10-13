package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/quotakey"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// QuotaKeyRepository 实现 service.QuotaKeyRepository，提供 quota_keys 基础 CRUD
// （C3.1/C3.2 骨架范围），以及 C5a 扩展的 List / UpdateMeta / 按激活码列出账号
// （ListAccountsByQuotaKey，用于 admin GetWithAccounts 端点，软删由 SoftDeleteMixin 自动过滤）。
//
// 注意：C5a 之前本文件声明"严禁触碰 accounts 表"，该限制随 C5a（D-CURSOR-020）解除——
// ListAccountsByQuotaKey 是本次显式授权的方法，仅用于读取归属账号，不写不改 accounts。
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

// CountWorkingAccounts 统计归属某激活码、且未软删的账号数（号池口径）。
// 达到上限（service.MaxAccountsPerQuotaKey）即停止拉号。
func (r *quotaKeyRepository) CountWorkingAccounts(ctx context.Context, tx *ent.Tx, quotaKeyID int64) (int, error) {
	return tx.Account.Query().
		Where(
			account.QuotaKeyID(quotaKeyID),
			account.DeletedAtIsNil(),
		).
		Count(ctx)
}

// SoftDeleteAccountsByQuotaKey 在事务内逐账号置 deleted_at（软删），保留账号行与全部历史记录。
// 严禁物理删除 accounts 表账号行（D-CURSOR-014 硬口径：删除 = 软删除）。
func (r *quotaKeyRepository) SoftDeleteAccountsByQuotaKey(ctx context.Context, tx *ent.Tx, quotaKeyID int64) error {
	_, err := tx.Account.Update().
		Where(
			account.QuotaKeyID(quotaKeyID),
			account.DeletedAtIsNil(),
		).
		SetDeletedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return err
	}
	return nil
}

// SoftDelete 在事务内软删激活码（置 deleted_at），保留行与全部历史记录。
func (r *quotaKeyRepository) SoftDelete(ctx context.Context, tx *ent.Tx, id int64) error {
	_, err := tx.QuotaKey.UpdateOneID(id).
		SetDeletedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return err
	}
	return nil
}

// ListEligibleForRefresh 返回可参与刷新扫描的激活码：
// status=active AND exhausted=false AND 未过期 AND 未软删。
// SoftDeleteMixin 的查询拦截器已自动附加 deleted_at IS NULL，这里再显式声明一次以提高可读性。
func (r *quotaKeyRepository) ListEligibleForRefresh(ctx context.Context) ([]*ent.QuotaKey, error) {
	now := time.Now()
	return r.client.QuotaKey.Query().
		Where(
			quotakey.Status(domain.QuotaKeyStatusActive),
			quotakey.Exhausted(false),
			quotakey.Or(
				quotakey.ExpiresAtIsNil(),
				quotakey.ExpiresAtGT(now),
			),
			quotakey.DeletedAtIsNil(),
		).
		All(ctx)
}

// List 全量返回激活码（SoftDeleteMixin 已自动过滤软删行）。
func (r *quotaKeyRepository) List(ctx context.Context) ([]*ent.QuotaKey, error) {
	return r.client.QuotaKey.Query().All(ctx)
}

// UpdateMeta 仅更新请求中显式出现的 expires_at / notes 元数据字段，不触碰 code / status。
// 外层指针 nil 表示省略，内层指针 nil 表示显式清空，否则设置为给定值。
func (r *quotaKeyRepository) UpdateMeta(ctx context.Context, id int64, expiresAt **time.Time, notes **string) error {
	q := r.client.QuotaKey.UpdateOneID(id)
	if expiresAt != nil {
		if *expiresAt == nil {
			q = q.ClearExpiresAt()
		} else {
			q = q.SetExpiresAt(**expiresAt)
		}
	}
	if notes != nil {
		if *notes == nil {
			q = q.ClearNotes()
		} else {
			q = q.SetNotes(**notes)
		}
	}
	_, err := q.Save(ctx)
	return err
}

// ListAccountsByQuotaKey 返回归属某激活码的账号（account.QuotaKeyIDEQ），
// SoftDeleteMixin 已自动过滤软删账号。仅读取，不改写 accounts。
func (r *quotaKeyRepository) ListAccountsByQuotaKey(ctx context.Context, quotaKeyID int64) ([]*ent.Account, error) {
	return r.client.Account.Query().
		Where(account.QuotaKeyID(quotaKeyID)).
		All(ctx)
}
