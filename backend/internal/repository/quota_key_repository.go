package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
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

// MarkExpiredIfStillExpired 在事务内将激活码标为 expired，但仅当数据库当前行仍满足
// "当前已到期"条件：status='active' AND expires_at IS NOT NULL AND expires_at < now。
//
// 这是幂等、按当前值判定的条件化写入（D-CURSOR-032）：sweepOnce 先 ListExpired 取旧值，
// 再凭旧值发起写入；管理员可在两步间用 UpdateMeta 清空 expires_at。若直接无条件写
// UpdateStatusFields，会把"曾经到期"误判为"当前已到期"。本语句把判定下沉到 UPDATE 的
// WHERE 条件，按 DB 当前行重新校验，不受旧值影响；条件不满足时影响 0 行，天然幂等。
// 返回受影响行数（命中 1，未命中 0）。
func (r *quotaKeyRepository) MarkExpiredIfStillExpired(ctx context.Context, tx *ent.Tx, id int64, now time.Time) (int, error) {
	return tx.QuotaKey.Update().
		Where(
			quotakey.ID(id),
			quotakey.Status(domain.QuotaKeyStatusActive),
			quotakey.ExpiresAtNotNil(),
			quotakey.ExpiresAtLT(now),
		).
		SetStatus(domain.QuotaKeyStatusExpired).
		Save(ctx)
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

// SetDeviceIDIfAbsent 在 device_id 当前为 NULL 或空串时写入给定值，返回受影响行数（1 / 0）。
//
// 并发防重（B-10）：条件判定放在 UPDATE 的 WHERE 里由 DB 原子裁决，而不是"先查后写"。
// 后者在两个请求同时首次进入时会双双读到 NULL、各自生成一个不同的 deviceId，
// 后写者覆盖先写者，同一激活码出现两份设备标识（上游会视为两个不同客户端）。
// 这里只有把 NULL/空串改成非空的那一个事务命中，其余受影响 0 行，由调用方重读赢家值。
//
// WHERE 覆盖"NULL 或空串"两种未生成形态，与 service 侧 DeviceIDFor 的读判定
// （DeviceID == nil || *DeviceID == ""）逐字一致——两侧判定若不一致，会出现
// "读侧认为已有、写侧仍可写"的错配，进而在竞态分支读到本请求自己刚写的值。
//
// 与 MarkExpiredIfStillExpired 同款写法：条件化写入天然幂等，重复调用第二次即 0 行。
func (r *quotaKeyRepository) SetDeviceIDIfAbsent(ctx context.Context, id int64, deviceID string) (int, error) {
	return r.client.QuotaKey.Update().
		Where(
			quotakey.ID(id),
			quotakey.Or(
				quotakey.DeviceIDIsNil(),
				quotakey.DeviceIDEQ(""),
			),
		).
		SetDeviceID(deviceID).
		Save(ctx)
}

// SetDeviceIDIfAbsentTx 是 SetDeviceIDIfAbsent 的事务内版本：同一条件化 UPDATE，
// 但在调用方已开启的事务（tx）上执行，复用事务的连接与行锁。
//
// 首次拉号必须用它：pull 容量事务已 FOR UPDATE 持有 quota_keys 行锁，此时再用
// 全局连接 SetDeviceIDIfAbsent 更新同一行，会等待本事务自身持有的锁 → 应用层自死锁
// （新码自动补池首写 device_id，2026-10-21 实测）。事务内条件写由 DB 原子裁决并发，
// 命中 1 才写、0 行表示已有并发写入，语义与全局版本完全一致。
func (r *quotaKeyRepository) SetDeviceIDIfAbsentTx(ctx context.Context, tx *ent.Tx, id int64, deviceID string) (int, error) {
	return tx.QuotaKey.Update().
		Where(
			quotakey.ID(id),
			quotakey.Or(
				quotakey.DeviceIDIsNil(),
				quotakey.DeviceIDEQ(""),
			),
		).
		SetDeviceID(deviceID).
		Save(ctx)
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
// 返回本次实际被软删的账号 ID，供调用方在同一事务内触发调度 outbox。
// 严禁物理删除 accounts 表账号行（D-CURSOR-014 硬口径：删除 = 软删除）。
func (r *quotaKeyRepository) SoftDeleteAccountsByQuotaKey(ctx context.Context, tx *ent.Tx, quotaKeyID int64) ([]int64, error) {
	ids, err := tx.Account.Query().
		Where(
			account.QuotaKeyID(quotaKeyID),
			account.DeletedAtIsNil(),
		).
		IDs(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	_, err = tx.Account.Update().
		Where(
			account.IDIn(ids...),
			account.DeletedAtIsNil(),
		).
		SetDeletedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// EnqueueAccountChangedOutbox 在事务内为每个被软删账号写一条 scheduler_outbox
// （event_type=account_changed），复用既有唯一入口 enqueueSchedulerOutbox，
// 不新造表/事件类型。写在本事务内 ⇒ 与软删同生共死：回滚则 outbox 一并消失。
// 既有 outbox consumer 消费该事件时，账号已从号池不可见，于是把它从 Redis 调度快照移除。
func (r *quotaKeyRepository) EnqueueAccountChangedOutbox(ctx context.Context, tx *ent.Tx, accountIDs []int64) error {
	if len(accountIDs) == 0 {
		return nil
	}
	groupIDsByAccount, err := loadAccountGroupIDsByAccount(ctx, tx, accountIDs)
	if err != nil {
		return err
	}
	for _, accountID := range accountIDs {
		if accountID <= 0 {
			continue
		}
		id := accountID
		payload := buildSchedulerGroupPayload(groupIDsByAccount[id])
		if err := enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged, &id, nil, payload); err != nil {
			return err
		}
	}
	return nil
}

// loadAccountGroupIDsByAccount 在给定事务内批量读取账号的分组归属，
// 供 outbox payload 携带 group_ids（与 accountRepository.Delete 同款），
// 使 consumer 能重建受影响分组的 bucket 快照。
func loadAccountGroupIDsByAccount(ctx context.Context, tx *ent.Tx, accountIDs []int64) (map[int64][]int64, error) {
	entries, err := tx.AccountGroup.Query().
		Where(accountgroup.AccountIDIn(accountIDs...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]int64, len(accountIDs))
	for _, entry := range entries {
		out[entry.AccountID] = append(out[entry.AccountID], entry.GroupID)
	}
	return out, nil
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

// ListAccountsByQuotaKeys 批量返回归属给定激活码的账号（一次 QuotaKeyIDIn 查询），
// SoftDeleteMixin 已自动过滤软删账号。供 List 端点按 quota_key_id 分组聚合计数，
// 避免逐激活码查询退化为 N+1。仅读取，不改写 accounts。
func (r *quotaKeyRepository) ListAccountsByQuotaKeys(ctx context.Context, ids []int64) ([]*ent.Account, error) {
	if len(ids) == 0 {
		return []*ent.Account{}, nil
	}
	return r.client.Account.Query().
		Where(account.QuotaKeyIDIn(ids...)).
		All(ctx)
}
