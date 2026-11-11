package repository

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/ent/quotakey"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// 编译期守卫：生产仓储必须提供"观测/换码"所需的事务内方法（service.quotaKeyObservationRepo
// 未导出，故在实现侧以等价签名断言，签名漂移即编译失败而不是运行时才失败关闭）。
var _ interface {
	GetByIDInTx(ctx context.Context, tx *ent.Tx, id int64) (*ent.QuotaKey, error)
	UpdateUsageSnapshot(ctx context.Context, tx *ent.Tx, id int64, snapshot []byte, expiresAt **time.Time) error
	UpdateCredentialsAtomically(ctx context.Context, tx *ent.Tx, id int64, codeEncrypted, codeMAC, codeLast4 string, clearUsageSnapshot bool) error
	UpdateMeta(ctx context.Context, tx *ent.Tx, id int64, expiresAt **time.Time, notes **string) error
} = (*quotaKeyRepository)(nil)

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
	if input.Priority > 0 {
		q = q.SetPriority(input.Priority)
	}
	// UpstreamKey 空串 = 未指定，不显式写该列，让它取 ent schema 的 Default("shareai")
	//（与 Priority 同理：0 不写、取列默认值 1）。非空时显式写入。
	if input.UpstreamKey != "" {
		q = q.SetUpstreamKey(input.UpstreamKey)
	}
	// B 批上移：码级并发 / 负载因子 / 计费倍率 / 模型映射。
	// 仅在显式给出合法值时落地（>0 / >=0 / 非空），其余维持列默认或沿用上游，
	// 与 quota_key_dto.go 的"省略即不覆盖"约定一致；不触碰 accounts。
	if input.Concurrency != nil && *input.Concurrency > 0 {
		q = q.SetConcurrency(*input.Concurrency)
	}
	if input.LoadFactor != nil && *input.LoadFactor > 0 {
		q = q.SetLoadFactor(*input.LoadFactor)
	}
	if input.RateMultiplier != nil && *input.RateMultiplier >= 0 {
		q = q.SetRateMultiplier(*input.RateMultiplier)
	}
	if len(input.ModelMapping) > 0 {
		q = q.SetModelMapping(copyStringMap(input.ModelMapping))
	}
	qk, err := q.Save(ctx)
	if err != nil {
		// code_mac 唯一冲突 = 同一张激活码已有未软删记录。
		// 只认 uq_quota_keys_code_mac 这个唯一索引的冲突；主键冲突（同为 23505，
		// 约束名 quota_keys_pkey）与 status CHECK 冲突（23514）不得被误译成重复激活码。
		if isQuotaKeyCodeMacUniqueViolation(err) {
			return nil, service.ErrQuotaKeyDuplicateCode
		}
		return nil, err
	}
	return qk, nil
}

// isQuotaKeyCodeMacUniqueViolation 判定 err 是否为 uq_quota_keys_code_mac 唯一冲突。
// 只认该索引的约束名：quota_keys 主键冲突（同为 23505）与 status CHECK 冲突（23514）
// 都不算「重复激活码」，必须放回原样错误，不得误译。
func isQuotaKeyCodeMacUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr != nil {
		return pqErr.Code == "23505" && pqErr.Constraint == "uq_quota_keys_code_mac"
	}
	return false
}

// copyStringMap 返回入参 map 的浅拷贝（避免调用方后续修改影响已落库的行）。
func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// toAnyMap 把 map[string]string 转成 map[string]any（供写入 credentials JSONB）。
func toAnyMap(in map[string]string) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// syncAccountsExpiry 在给定事务 tx（或默认 client，两者互斥）内把指定激活码名下
// 未软删账号的 accounts.expires_at 设为给定值（nil 清空），返回受影响账号 ID。
// 仅负责"写"与"收集 ID"：是否入队调度 outbox 由调用方按各自的 tx 约束决定。
//
// 返回 (nil, nil) 当该激活码名下无任何未软删账号（无变更、无需入队）。
func (r *quotaKeyRepository) syncAccountsExpiry(ctx context.Context, tx *ent.Tx, client *ent.Client, quotaKeyID int64, expiresAt *time.Time) ([]int64, error) {
	var ids []int64
	var err error
	if tx != nil {
		ids, err = tx.Account.Query().Where(account.QuotaKeyID(quotaKeyID), account.DeletedAtIsNil()).IDs(ctx)
	} else {
		ids, err = client.Account.Query().Where(account.QuotaKeyID(quotaKeyID), account.DeletedAtIsNil()).IDs(ctx)
	}
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	apply := func(u *ent.AccountUpdate) *ent.AccountUpdate {
		if expiresAt == nil {
			return u.ClearExpiresAt()
		}
		return u.SetExpiresAt(*expiresAt)
	}
	if tx != nil {
		_, err = apply(tx.Account.Update().Where(account.QuotaKeyID(quotaKeyID), account.DeletedAtIsNil())).Save(ctx)
	} else {
		_, err = apply(client.Account.Update().Where(account.QuotaKeyID(quotaKeyID), account.DeletedAtIsNil())).Save(ctx)
	}
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// UpdatePriority 在同一事务内改写激活码优先级，并把该码名下**未软删**账号的
// accounts.priority 同步为该值（"码的优先级 = 码下账号的优先级"，用户裁定 2026-11-03）。
//
// 口径：
//   - 只改 priority 一列，不触碰 status / exhausted / expires_at / 额度快照；
//   - 软删账号不参与同步（其调度资格已失效，不改写历史行）；
//   - 返回实际同步的账号行数，供调用方判断影响面。
func (r *quotaKeyRepository) UpdatePriority(ctx context.Context, tx *ent.Tx, id int64, priority int) (int, error) {
	if _, err := tx.QuotaKey.UpdateOneID(id).SetPriority(priority).Save(ctx); err != nil {
		return 0, err
	}
	return tx.Account.Update().
		Where(account.QuotaKeyID(id), account.DeletedAtIsNil()).
		SetPriority(priority).
		Save(ctx)
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

// GetByIDInTx 在给定事务内读取来源行（观测/换码在每 key 锁内复核时使用）。
// tx 为 nil 时退化为默认连接（无事务装配场景）。
func (r *quotaKeyRepository) GetByIDInTx(ctx context.Context, tx *ent.Tx, id int64) (*ent.QuotaKey, error) {
	if tx != nil {
		return tx.QuotaKey.Get(ctx, id)
	}
	return r.client.QuotaKey.Get(ctx, id)
}

// UpdateUsageSnapshot 在同一事务内写来源额度快照（usage_snapshot），并在有合法期限时
// 同事务更新 expires_at（唯一期限列，不复制进快照）。
//
// expiresAt 外层指针为 nil 表示上游本次未给出可用期限 → 保留原值不动（不伪造、不清空）。
// 失败观测与成功观测走同一条写入路径：调用方负责把"最近成功值 + 最近观测时刻/错误码"
// 合并进 snapshot 字节里，本方法只做持久化，不解释语义。
func (r *quotaKeyRepository) UpdateUsageSnapshot(ctx context.Context, tx *ent.Tx, id int64, snapshot []byte, expiresAt **time.Time) error {
	var q *ent.QuotaKeyUpdateOne
	if tx != nil {
		q = tx.QuotaKey.UpdateOneID(id)
	} else {
		q = r.client.QuotaKey.UpdateOneID(id)
	}
	if len(snapshot) > 0 {
		q = q.SetUsageSnapshot(jsontext.Value(snapshot))
	}
	if expiresAt != nil && *expiresAt != nil {
		q = q.SetExpiresAt(**expiresAt)
	}
	if _, err := q.Save(ctx); err != nil {
		return err
	}
	// 账号过期时间随激活码（用户裁定 2026-11-10）：合法期限时同事务（无 tx 时按
	// r.client）同步名下未软删账号的 accounts.expires_at，并收集受影响账号 ID 入队
	// 调度 outbox（仅 tx != nil 时入队；无 tx 场景不触发调度重建）。
	if expiresAt != nil && *expiresAt != nil {
		var client *ent.Client
		if tx == nil {
			client = r.client
		}
		ids, err := r.syncAccountsExpiry(ctx, tx, client, id, &(**expiresAt))
		if err != nil {
			return err
		}
		if tx != nil && len(ids) > 0 {
			if err := r.EnqueueAccountChangedOutbox(ctx, tx, ids); err != nil {
				return err
			}
		}
	}
	return nil
}

// UpdateCredentialsAtomically 在同一事务内写凭据；clearUsageSnapshot 为 true 时同事务清空
// usage_snapshot（§25.14-C 换码原子边界：来源身份已变，旧观测对新来源无效）。
//
// 相同 MAC 的"只重加密"路径带 false：保留已提交观测，不因同一来源重加密而丢事实。
func (r *quotaKeyRepository) UpdateCredentialsAtomically(ctx context.Context, tx *ent.Tx, id int64, codeEncrypted, codeMAC, codeLast4 string, clearUsageSnapshot bool) error {
	var q *ent.QuotaKeyUpdateOne
	if tx != nil {
		q = tx.QuotaKey.UpdateOneID(id)
	} else {
		q = r.client.QuotaKey.UpdateOneID(id)
	}
	q = q.SetCodeEncrypted(codeEncrypted).SetCodeMAC(codeMAC).SetCodeLast4(codeLast4)
	if clearUsageSnapshot {
		q = q.ClearUsageSnapshot()
	}
	_, err := q.Save(ctx)
	// code_mac 唯一冲突 = 同一张激活码已有未软删记录；只认 uq_quota_keys_code_mac
	// 这一唯一索引的冲突（与 Create 同口径），翻译成 ErrQuotaKeyDuplicateCode，
	// 其余错误原样返回。
	if isQuotaKeyCodeMacUniqueViolation(err) {
		return service.ErrQuotaKeyDuplicateCode
	}
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
//
// O01：首次拉号首写已前置到容量事务之外（pull 经 DeviceIDFor 走本方法，无行锁、独立提交），
// 故不再提供事务内版本——事务内更新同一行会等待自身持有的行锁 → 应用层自死锁。
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

// SoftDeleteCursorAccountsByExpiredQuotaKey 仅当激活码"当前仍到期"时，在调用方事务内
// 软删该激活码下所有 cursor 平台账号（置 accounts.deleted_at），返回本次实际被软删的账号 ID。
//
// 与人工删除 SoftDeleteAccountsByQuotaKey 的严格区分（语义不得混淆）：
//   - 人工删除覆盖"全平台"账号；本方法只覆盖 platform='cursor' 的账号。
//   - 本方法只由"激活码到期"触发：到期事实源唯一 = quota_keys.expires_at，
//     判定 status='expired' AND expires_at IS NOT NULL AND expires_at < now；
//     不处理未标 expired、未到期或 expires_at 为 NULL 的激活码。
//   - 只置 accounts.deleted_at：不物理删除账号、不触碰 usage_logs、不软删 quota_keys。
//   - 严禁用 account_expire_in 替代 expires_at 判定到期（到期判定只认 quota_keys.expires_at）。
//
// 实现要点（单条 UPDATE ... RETURNING，精确返回本次实际受影响行）：
//  1. tx 必填：退化到默认 client 会绕开调用方事务，故直接失败关闭。
//  2. 到期判定下沉为 EXISTS 子查询：断言账号所属激活码"当前仍到期"
//     （status='expired' AND expires_at IS NOT NULL AND expires_at < now），
//     由数据库在语句执行时按当前已提交状态裁决，不存在"先读后写"的 TOCTOU。
//  3. 单条 UPDATE ... RETURNING id：UPDATE 的 WHERE 同时含 quota_key_id /
//     platform='cursor' / deleted_at IS NULL / EXISTS 到期谓词——到期判定、软删与
//     精确返回在同一条语句内由数据库原子完成；不存在先读候选再写的中间窗口，
//     候选集变化也不会误删，也不存在按 deleted_at==now 回读的误报窗口
//     （历史软删行若 deleted_at 恰等于本次传入 now，不会被错误上报）。
//  4. 无候选 SELECT、无回读：RETURNING 直接返回本次 UPDATE 实际影响的行，
//     affected == 0（竞态下候选已先被并发事务软删，或激活码已不再到期）时返回空集合。
//  5. 原生 SQL 绕过 ent SoftDeleteMixin，故 accounts.deleted_at IS NULL 与
//     q.deleted_at IS NULL 均显式写出；platform='cursor' 为字面量，不参数化。
//  6. 同一语句同时兼容 PostgreSQL（生产）与 SQLite（既有 handler 链路测试）：
//     $1/$2 占位符与 UPDATE ... RETURNING 在两个方言下均已实测可用，无方言分支。
func (r *quotaKeyRepository) SoftDeleteCursorAccountsByExpiredQuotaKey(ctx context.Context, tx *ent.Tx, quotaKeyID int64, now time.Time) ([]int64, error) {
	if tx == nil {
		return nil, errors.New("quota_key_repository: SoftDeleteCursorAccountsByExpiredQuotaKey requires the caller's transaction")
	}

	// 单条语句：到期判定 + 平台/未删过滤 + 软删 + 精确返回受影响行。
	rows, err := tx.QueryContext(ctx, `
		UPDATE accounts
		   SET deleted_at = $1
		 WHERE quota_key_id = $2
		   AND platform = 'cursor'
		   AND deleted_at IS NULL
		   AND EXISTS (
		       SELECT 1 FROM quota_keys q
		        WHERE q.id = accounts.quota_key_id
		          AND q.deleted_at IS NULL
		          AND q.status = 'expired'
		          AND q.expires_at IS NOT NULL
		          AND q.expires_at < $1
		   )
		RETURNING id`, now, quotaKeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
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

// UpdateMeta 在调用方已开启的事务内更新请求中显式出现的 expires_at / notes 元数据字段，
// 不触碰 code / status。外层指针 nil 表示省略，内层指针 nil 表示显式清空，否则设置为给定值。
//
// tx 必填（§25.17.6）：管理员的到期日期编辑必须与来源观测共用同 key advisory 锁 + 同一事务。
// 若此处退回默认 client 走锁外独立提交，管理员的日期就能在 Probe 持锁等待 lookup 期间提交，
// 随后被 Probe 的旧观测结果覆盖——这正是本轮修复的 A07 缺陷。tx 为 nil 时失败关闭，
// 不提供"锁外也能写"的旁路。
func (r *quotaKeyRepository) UpdateMeta(ctx context.Context, tx *ent.Tx, id int64, expiresAt **time.Time, notes **string) error {
	if tx == nil {
		return errors.New("quota_key_repository: UpdateMeta requires the caller's transaction")
	}
	q := tx.QuotaKey.UpdateOneID(id)
	var expires *time.Time
	if expiresAt != nil {
		if *expiresAt == nil {
			q = q.ClearExpiresAt()
		} else {
			e := **expiresAt
			expires = &e
			q = q.SetExpiresAt(e)
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
		return err
	}
	// 账号过期时间随激活码（手工编辑口径）：同事务同步名下未软删账号的
	// accounts.expires_at，并对受影响账号入队调度 outbox。
	if expiresAt != nil {
		ids, err := r.syncAccountsExpiry(ctx, tx, nil, id, expires)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			if err := r.EnqueueAccountChangedOutbox(ctx, tx, ids); err != nil {
				return err
			}
		}
	}
	return nil
}

// SyncAccountsExpiry 在调用方已开启的事务内，把指定激活码名下未软删账号的
// accounts.expires_at 设为给定值（nil 清空）。仅当实际存在受影响账号（即发生变更）
// 时才入队调度 outbox；无受影响账号时直接返回，不写入 outbox。
//
// tx 必填（与 UpdateMeta 同理：元数据到期编辑必须与来源观测共用同 key 事务），
// tx 为 nil 时失败关闭。
func (r *quotaKeyRepository) SyncAccountsExpiry(ctx context.Context, tx *ent.Tx, quotaKeyID int64, expiresAt *time.Time) error {
	if tx == nil {
		return errors.New("quota_key_repository: SyncAccountsExpiry requires the caller's transaction")
	}
	ids, err := r.syncAccountsExpiry(ctx, tx, nil, quotaKeyID, expiresAt)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		return r.EnqueueAccountChangedOutbox(ctx, tx, ids)
	}
	return nil
}

// UpdateSettingsAndSyncAccounts 在调用方事务内更新激活码设置，并同事务把该码名下
// 未软删账号的对应字段同步（concurrency / load_factor / rate_multiplier 同步到账号列，
// model_mapping 同步进各账号 credentials JSONB 并重新加密、维护 credentials_mac /
// credentials_api_key_mac），最后对受影响账号入队调度 outbox。
//
// tx 必填：设置变更与账号同步必须共用同一事务，保证原子性；tx 为 nil 时失败关闭。
func (r *quotaKeyRepository) UpdateSettingsAndSyncAccounts(ctx context.Context, tx *ent.Tx, id int64, settings service.UpdateQuotaKeySettingsInput) error {
	if tx == nil {
		return errors.New("quota_key_repository: UpdateSettingsAndSyncAccounts requires the caller's transaction")
	}
	// 防御（defense-in-depth）：ModelMappingSet 但空映射，禁止落库。
	if settings.ModelMappingSet && len(settings.ModelMapping) == 0 {
		return errors.New("quota_key_repository: UpdateSettingsAndSyncAccounts refuses empty model_mapping when ModelMappingSet")
	}

	// 1. 更新 quota_keys 自身设置。
	q := tx.QuotaKey.UpdateOneID(id)
	if settings.Concurrency != nil {
		if *settings.Concurrency == nil {
			q = q.ClearConcurrency()
		} else {
			q = q.SetConcurrency(**settings.Concurrency)
		}
	}
	if settings.LoadFactor != nil {
		if *settings.LoadFactor == nil {
			q = q.ClearLoadFactor()
		} else {
			q = q.SetLoadFactor(**settings.LoadFactor)
		}
	}
	if settings.RateMultiplier != nil {
		q = q.SetRateMultiplier(*settings.RateMultiplier)
	}
	if settings.ModelMappingSet {
		q = q.SetModelMapping(copyStringMap(settings.ModelMapping))
	}
	if _, err := q.Save(ctx); err != nil {
		return err
	}

	// 1b. 写码级 group_ids 列（B3 分组同步）。
	//     三态语义：GroupIDs!=nil 才动该列；空切片必须写成 [] 而不是 NULL
	//     （[] = 显式解绑全部）。归一化保证 *settings.GroupIDs 指向 nil 切片时
	//     也落 []，避免 ent 的 field.JSON 把 nil 切片写成 null。
	if settings.GroupIDs != nil {
		newIDs := *settings.GroupIDs
		if newIDs == nil {
			newIDs = []int64{}
		}
		if err := tx.QuotaKey.UpdateOneID(id).SetGroupIds(newIDs).Exec(ctx); err != nil {
			return err
		}
	}

	// 2. 查询名下未软删账号（同步变更的实际目标）。
	accs, err := tx.Account.Query().Where(account.QuotaKeyID(id), account.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return err
	}
	if len(accs) == 0 {
		return nil
	}
	affectedIDs := make([]int64, 0, len(accs))
	for _, acc := range accs {
		affectedIDs = append(affectedIDs, acc.ID)
	}

	// 3. 标量字段（concurrency / load_factor / rate_multiplier）批量同步到账号列。
	changed := false
	accUpd := tx.Account.Update().Where(account.QuotaKeyID(id), account.DeletedAtIsNil())
	if settings.Concurrency != nil {
		changed = true
		if *settings.Concurrency == nil {
			// 清空 = 恢复账号默认并发 3（与账号列默认值一致）。
			accUpd = accUpd.SetConcurrency(3)
		} else {
			accUpd = accUpd.SetConcurrency(**settings.Concurrency)
		}
	}
	if settings.LoadFactor != nil {
		changed = true
		if *settings.LoadFactor == nil {
			accUpd = accUpd.ClearLoadFactor()
		} else {
			accUpd = accUpd.SetLoadFactor(**settings.LoadFactor)
		}
	}
	if settings.RateMultiplier != nil {
		changed = true
		accUpd = accUpd.SetRateMultiplier(*settings.RateMultiplier)
	}
	if changed {
		if _, err := accUpd.Save(ctx); err != nil {
			return err
		}
	}

	// 4. 模型映射：逐账号解密 credentials → 写入 model_mapping → 重新加密并维护 MAC，
	//    严禁跳过 prepareCredentialsForStorage 直接改 credentials JSONB（会破坏 credentials_mac）。
	if settings.ModelMappingSet {
		for _, acc := range accs {
			plain, err := decryptCredentialsMap(acc.Credentials)
			if err != nil {
				return err
			}
			if plain == nil {
				plain = map[string]any{}
			}
			plain["model_mapping"] = toAnyMap(settings.ModelMapping)
			prepared, err := prepareCredentialsForStorage(plain)
			if err != nil {
				return err
			}
			u := tx.Account.UpdateOneID(acc.ID).
				SetCredentials(prepared.storage).
				SetCredentialsMAC(prepared.mac).
				SetNillableCredentialsAPIKeyMAC(prepared.apiKeyMAC)
			if _, err := u.Save(ctx); err != nil {
				return err
			}
		}
		changed = true
	}

	// 6. 分组同步（B3）：把名下未软删账号的 account_groups 绑定整体替换为新分组集合，
	//    并对每个账号入队 account_groups_changed 事件。码级 group_ids 列已在 1b 写入。
	//
	//    外审强制口径（2026-11-11 R1b [P2] must_fix，必须照做）：
	//    - lockLiveGroups 必须在任何 account_groups 的 DELETE/INSERT 之前、且在同一事务内
	//      调用；不得先删后锁（依据 account_repo.go:2236-2240「先锁目标分组再替换绑定」，
	//      group_repo.go:44-61 用 ORDER BY id FOR SHARE 并在等待后复核分组有效性；
	//      删除路径持 FOR UPDATE 清理绑定并软删分组——省略前置锁会插入指向已软删分组的绑定）。
	//    - outbox 事件类型必须是 SchedulerOutboxEventAccountGroupsChanged；不得复用会发
	//      AccountChanged 的 EnqueueAccountChangedOutbox。
	//    - payload 必须是每个账号各自的「旧分组 ∪ 新分组」去重并集，不是全批并集。
	if settings.GroupIDs != nil {
		newIDs := *settings.GroupIDs
		if newIDs == nil {
			newIDs = []int64{} // 归一化：空切片落 [] 非 NULL（与 1b 同口径）。
		}
		// ① 先锁目标分组（必须在任何绑定写之前、同事务内）。
		if err := lockLiveGroups(ctx, tx.Client(), newIDs); err != nil {
			return err
		}
		// 旧分组归属：在 DELETE 之前一次性批量读取（与 loadAccountGroupIDsByAccount 同型，
		// 该 helper 为 AccountIDIn 版本，这里直接传 affectedIDs 复用）。
		oldIDsByAccount, err := loadAccountGroupIDsByAccount(ctx, tx, affectedIDs)
		if err != nil {
			return err
		}
		for _, acc := range accs {
			oldIDs := oldIDsByAccount[acc.ID]
			// 先删后插（同事务内，锁已先行持有）。
			if _, err := tx.AccountGroup.Delete().Where(accountgroup.AccountIDEQ(acc.ID)).Exec(ctx); err != nil {
				return err
			}
			for i, gid := range newIDs {
				if _, err := tx.AccountGroup.Create().
					SetAccountID(acc.ID).
					SetGroupID(gid).
					SetPriority(i + 1).
					Save(ctx); err != nil {
					return err
				}
			}
			// ② 逐账号 payload = 该账号旧 ∪ 新（去重并集）。
			payload := buildSchedulerGroupPayload(mergeGroupIDs(oldIDs, newIDs))
			if err := enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountGroupsChanged, &acc.ID, nil, payload); err != nil {
				return err
			}
		}
	}

	// 5. 受影响账号入队调度 outbox（仅当确有变更）。
	if changed {
		return r.EnqueueAccountChangedOutbox(ctx, tx, affectedIDs)
	}
	return nil
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
