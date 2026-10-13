package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/quotakey"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
)

// MaxAccountsPerQuotaKey 是单个激活码可持有（working）账号的上限；达到后停止拉号。
const MaxAccountsPerQuotaKey = 15

// QuotaKeyRepository 是 QuotaKeyService 依赖的存储契约。
//
// 仅包含与 accounts 表无关的 quota_keys 操作（C3.1 骨架范围），以及 C3.2 扩展的
// 账号软删编排与刷新扫描能力。实现位于 internal/repository（repository 包引用本
// service 包的类型，故接口定义在此处以避免 service -> repository 的反向依赖）。
type QuotaKeyRepository interface {
	GetByID(ctx context.Context, quotaKeyID int64) (*ent.QuotaKey, error)
	Create(ctx context.Context, input CreateQuotaKeyInput) (*ent.QuotaKey, error)
	UpdateStatusFields(ctx context.Context, tx *ent.Tx, id int64, status string, exhausted bool, lastError *string, lastRefreshAt *time.Time) error
	HardDelete(ctx context.Context, tx *ent.Tx, id int64) error
	UpdateCredentials(ctx context.Context, id int64, codeEncrypted, codeMAC, codeLast4 string) error
	ListExpired(ctx context.Context, now time.Time) ([]*ent.QuotaKey, error)
	ListByStatus(ctx context.Context, status string) ([]*ent.QuotaKey, error)
	// C5a 扩展（D-CURSOR-020）：admin CRUD 读/写支撑方法。
	List(ctx context.Context) ([]*ent.QuotaKey, error)
	UpdateMeta(ctx context.Context, id int64, expiresAt *time.Time, notes *string) error
	ListAccountsByQuotaKey(ctx context.Context, quotaKeyID int64) ([]*ent.Account, error)
	// C3.2 扩展：账号软删编排与刷新扫描（均在调用方已开启的事务内执行，除 ListEligibleForRefresh）。
	CountWorkingAccounts(ctx context.Context, tx *ent.Tx, quotaKeyID int64) (int, error)
	SoftDeleteAccountsByQuotaKey(ctx context.Context, tx *ent.Tx, quotaKeyID int64) error
	SoftDelete(ctx context.Context, tx *ent.Tx, id int64) error
	ListEligibleForRefresh(ctx context.Context) ([]*ent.QuotaKey, error)
}

// QuotaKeyUpstreamClient 抽象"向上游拉取账号"的客户端。
//
// 合同 §5 与 D-CURSOR-014 硬口径（2026-10-13 用户裁定，锁定，不得改）：
// 占位实现必须是"失败"占位——任意错误返回 exhausted=true 分支所需的信息，
// 绝不允许返回假账号 / 固定成功（上一轮 D-CURSOR-009 就因此作废）。
// 生产默认占位见 NotConnectedUpstreamClient。
type QuotaKeyUpstreamClient interface {
	// PullBatch 尝试从上游拉取一批账号。
	// 成功返回待入库账号列表；任意错误必须返回（不得返回假账号）。
	PullBatch(ctx context.Context, qk *ent.QuotaKey) (*UpstreamPullResult, error)
}

// UpstreamAccount 是上游返回的一条待入库账号的规格。
// 不含 id / quota_key_id（由 service 在插入时写入归属）。
type UpstreamAccount struct {
	Name        string
	Platform    string
	Type        string
	Credentials map[string]any
	Extra       map[string]any
}

// UpstreamPullResult 是上游一次拉号返回的批量结果。
type UpstreamPullResult struct {
	Accounts []*UpstreamAccount
}

// ErrUpstreamNotConnected 是生产默认占位客户端返回的"未接入上游"错误。
var ErrUpstreamNotConnected = errors.New("quota key upstream not connected")

// NotConnectedUpstreamClient 是生产默认的占位客户端：未接入上游时返回明确错误，
// 由 PullAccounts 走 exhausted 分支。绝不返回假账号 / 固定成功。
type NotConnectedUpstreamClient struct{}

// PullBatch 返回"未接入上游"错误，触发 PullAccounts 的 exhausted 占位分支。
func (NotConnectedUpstreamClient) PullBatch(_ context.Context, _ *ent.QuotaKey) (*UpstreamPullResult, error) {
	return nil, ErrUpstreamNotConnected
}

// ErrQuotaKeyNotFound 表示激活码不存在（或已软删，普通查询不可见）。
var ErrQuotaKeyNotFound = errors.New("quota key not found")

// QuotaKeyService 提供激活码凭据加密落库/读取 + C3.2 拉号状态机 + 软删编排。
type QuotaKeyService struct {
	repo     QuotaKeyRepository
	enc      QuotaKeyEncryptor
	upstream QuotaKeyUpstreamClient
	client   *ent.Client
}

// NewQuotaKeyService 构造激活码服务。
// enc 为 nil 时 panic：加密器缺失禁止静默回落明文（合同 §5）。
// upstream 为 nil 时默认使用 NotConnectedUpstreamClient（失败占位）。
// client 为 ent 客户端，用于 advisory 事务锁与账号/激活码的事务编排。
func NewQuotaKeyService(repo QuotaKeyRepository, enc QuotaKeyEncryptor, upstream QuotaKeyUpstreamClient, client *ent.Client) *QuotaKeyService {
	if enc == nil {
		panic("quota_key_service: encryptor must not be nil")
	}
	if upstream == nil {
		upstream = NotConnectedUpstreamClient{}
	}
	return &QuotaKeyService{repo: repo, enc: enc, upstream: upstream, client: client}
}

// GetStatus 读取激活码状态并脱敏（明文 code 绝不出现在返回 DTO 中）。
func (s *QuotaKeyService) GetStatus(ctx context.Context, id int64) (*QuotaKeyStatus, error) {
	qk, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return quotaKeyToStatus(qk), nil
}

// RotateCredentials 加密新激活码原文后写入 code_encrypted / code_mac / code_last4，
// 返回脱敏后的状态。
//
// 不触碰 accounts 表、不发起任何上游请求、不引入 mock。MAC 复用与
// account_credentials 同型的 HMAC-SHA256 指纹（供 uq_quota_keys_code_mac 唯一索引
// 做 SQL 级查重），不引入新加密机制。
func (s *QuotaKeyService) RotateCredentials(ctx context.Context, id int64, newPlain string) (*QuotaKeyStatus, error) {
	enc, err := s.enc.Encrypt([]byte(newPlain))
	if err != nil {
		return nil, err
	}
	codeEncrypted := string(enc)
	codeMAC := quotaKeyMACOfPlaintext(newPlain)
	codeLast4 := last4Of(newPlain)
	if err := s.repo.UpdateCredentials(ctx, id, codeEncrypted, codeMAC, codeLast4); err != nil {
		return nil, err
	}
	return s.GetStatus(ctx, id)
}

// List 全量返回脱敏后的激活码状态列表（明文 code 绝不出现在返回 DTO 中）。
func (s *QuotaKeyService) List(ctx context.Context) ([]*QuotaKeyStatus, error) {
	qks, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*QuotaKeyStatus, 0, len(qks))
	for _, qk := range qks {
		out = append(out, quotaKeyToStatus(qk))
	}
	return out, nil
}

// Create 接收明文激活码 → 加密（复用与 RotateCredentials 同一实现）→ 落库 → 返回脱敏状态。
//
// 明文 code 只进不出：加密后仅以密文/last4/MAC 形态入库，返回值一律脱敏，
// 明文绝不进入任何返回值或日志（F1）。新激活码默认 status=active。
func (s *QuotaKeyService) Create(ctx context.Context, code string, expiresAt *time.Time, notes *string) (*QuotaKeyStatus, error) {
	enc, err := s.enc.Encrypt([]byte(code))
	if err != nil {
		return nil, err
	}
	codeEncrypted := string(enc)
	codeMAC := quotaKeyMACOfPlaintext(code)
	codeLast4 := last4Of(code)
	qk, err := s.repo.Create(ctx, CreateQuotaKeyInput{
		CodeEncrypted: codeEncrypted,
		CodeMAC:       codeMAC,
		CodeLast4:     codeLast4,
		Status:        domain.QuotaKeyStatusActive,
		ExpiresAt:     expiresAt,
		Notes:         notes,
	})
	if err != nil {
		return nil, err
	}
	return quotaKeyToStatus(qk), nil
}

// Update 改激活码：code 非 nil 时重加密写 credentials；expiresAt/notes 非 nil 时写元数据。
// 返回脱敏状态。明文 code 只进不出（F1）。
func (s *QuotaKeyService) Update(ctx context.Context, id int64, code *string, expiresAt *time.Time, notes *string) (*QuotaKeyStatus, error) {
	if code != nil {
		enc, err := s.enc.Encrypt([]byte(*code))
		if err != nil {
			return nil, err
		}
		codeEncrypted := string(enc)
		codeMAC := quotaKeyMACOfPlaintext(*code)
		codeLast4 := last4Of(*code)
		if err := s.repo.UpdateCredentials(ctx, id, codeEncrypted, codeMAC, codeLast4); err != nil {
			return nil, err
		}
	}
	if expiresAt != nil || notes != nil {
		if err := s.repo.UpdateMeta(ctx, id, expiresAt, notes); err != nil {
			return nil, err
		}
	}
	return s.GetStatus(ctx, id)
}

// GetWithAccounts 返回脱敏状态 + 该激活码下的账号切片（不嵌套聚合成单一对象，F7）。
func (s *QuotaKeyService) GetWithAccounts(ctx context.Context, id int64) (*QuotaKeyStatus, []*ent.Account, error) {
	qk, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	accounts, err := s.repo.ListAccountsByQuotaKey(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return quotaKeyToStatus(qk), accounts, nil
}

// PullAccounts 按 §3.4 拉号状态机拉取账号：advisory 事务锁 → 读状态 → 计数停限 →
// 二次读防竞态 → 发上游 → 成功插入并清 exhausted / 失败 CAS 写 exhausted。
func (s *QuotaKeyService) PullAccounts(ctx context.Context, quotaKeyID int64) error {
	return s.pull(ctx, quotaKeyID)
}

// RefreshAccounts 手动触发拉号（§4.4），与 PullAccounts 共享同一 advisory 互斥域。
func (s *QuotaKeyService) RefreshAccounts(ctx context.Context, quotaKeyID int64) error {
	return s.pull(ctx, quotaKeyID)
}

// pull 是 PullAccounts / RefreshAccounts 共享的拉号状态机实现（同一互斥域、同一事务编排）。
func (s *QuotaKeyService) pull(ctx context.Context, quotaKeyID int64) error {
	if s.client == nil {
		return errors.New("quota_key_service: ent client not configured")
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("quota_key_service: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// advisory xact lock：同 key 串行，异 key 不阻塞；提交/回滚时自动释放。
	if err := acquireAdvisoryXactLock(ctx, tx, quotaKeyAdvisoryLockID(quotaKeyID)); err != nil {
		return err
	}

	// 读1：FOR UPDATE 锁定 quota_key 行，防止 sweep 并发改写。
	qk, err := tx.QuotaKey.Query().Where(quotakey.IDEQ(quotaKeyID)).ForUpdate().Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return ErrQuotaKeyNotFound
		}
		return fmt.Errorf("quota_key_service: load quota key: %w", err)
	}
	if qk.Status != domain.QuotaKeyStatusActive {
		return nil // 已被禁用/过期，停止拉号
	}

	// 读2：统计 working 账号数。
	working, err := s.repo.CountWorkingAccounts(ctx, tx, quotaKeyID)
	if err != nil {
		return fmt.Errorf("quota_key_service: count working accounts: %w", err)
	}
	if working >= MaxAccountsPerQuotaKey {
		return nil // 达到上限，停止拉号（不改 exhausted）
	}

	// 二次读状态（防 sweep 竞态）：重新 FOR UPDATE 确认仍为 active 且未过期。
	qk2, err := tx.QuotaKey.Query().Where(quotakey.IDEQ(quotaKeyID)).ForUpdate().Only(ctx)
	if err != nil {
		return fmt.Errorf("quota_key_service: re-load quota key: %w", err)
	}
	if qk2.Status != domain.QuotaKeyStatusActive || isExpired(qk2) {
		return nil
	}

	// 发上游。
	result, pullErr := s.upstream.PullBatch(ctx, qk2)
	if pullErr != nil {
		// 失败：CAS 写 exhausted=true，仅当当前 status='active'（不覆盖 sweep 的禁用/过期）。
		if cerr := s.casSetExhausted(ctx, tx, quotaKeyID, pullErr); cerr != nil {
			return fmt.Errorf("quota_key_service: cas set exhausted: %w", cerr)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("quota_key_service: commit after exhausted: %w", err)
		}
		committed = true
		return nil
	}

	// 成功：四读（再次 FOR UPDATE 确认状态）后插入账号并清 exhausted。
	qk3, err := tx.QuotaKey.Query().Where(quotakey.IDEQ(quotaKeyID)).ForUpdate().Only(ctx)
	if err != nil {
		return fmt.Errorf("quota_key_service: final-load quota key: %w", err)
	}
	if qk3.Status != domain.QuotaKeyStatusActive {
		return nil
	}

	for _, ua := range result.Accounts {
		create := tx.Account.Create().
			SetName(ua.Name).
			SetPlatform(ua.Platform).
			SetType(ua.Type).
			SetCredentials(ua.Credentials).
			SetQuotaKeyID(quotaKeyID)
		if ua.Extra != nil {
			create.SetExtra(ua.Extra)
		}
		if _, err := create.Save(ctx); err != nil {
			return fmt.Errorf("quota_key_service: create account: %w", err)
		}
	}

	// 清 exhausted 与错误占位。
	if _, err := tx.QuotaKey.UpdateOneID(quotaKeyID).
		SetExhausted(false).
		SetLastRefreshAt(time.Now()).
		SetNillableLastRefreshError(nil).
		Save(ctx); err != nil {
		return fmt.Errorf("quota_key_service: clear exhausted: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("quota_key_service: commit pull: %w", err)
	}
	committed = true
	return nil
}

// DeleteWithAccounts 软删激活码及其全部账号（§3.5 删除唯一路径 = 软删口径）。
// 锁 → tx：逐账号软删 → 软删 quota_key → commit（全删或不删）。
// 数据库账号行与全部历史记录保留，禁止物理 DELETE 账号行。
func (s *QuotaKeyService) DeleteWithAccounts(ctx context.Context, quotaKeyID int64) error {
	if s.client == nil {
		return errors.New("quota_key_service: ent client not configured")
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("quota_key_service: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := acquireAdvisoryXactLock(ctx, tx, quotaKeyAdvisoryLockID(quotaKeyID)); err != nil {
		return err
	}

	// 确认存在（FOR UPDATE 锁行）。已软删的行在普通查询中不可见。
	if _, err := tx.QuotaKey.Query().Where(quotakey.IDEQ(quotaKeyID)).ForUpdate().Only(ctx); err != nil {
		if ent.IsNotFound(err) {
			return ErrQuotaKeyNotFound
		}
		return fmt.Errorf("quota_key_service: load quota key for delete: %w", err)
	}

	// 逐账号软删（置 deleted_at；禁止物理删除 accounts 表账号行）。
	if err := s.repo.SoftDeleteAccountsByQuotaKey(ctx, tx, quotaKeyID); err != nil {
		return fmt.Errorf("quota_key_service: soft-delete accounts: %w", err)
	}
	// 软删 quota_key。
	if err := s.repo.SoftDelete(ctx, tx, quotaKeyID); err != nil {
		return fmt.Errorf("quota_key_service: soft-delete quota key: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("quota_key_service: commit delete: %w", err)
	}
	committed = true
	return nil
}

// ClearExhausted 仅清除 exhausted 标记，不触发拉号（§0.1 #4）。
func (s *QuotaKeyService) ClearExhausted(ctx context.Context, quotaKeyID int64) error {
	if s.client == nil {
		return errors.New("quota_key_service: ent client not configured")
	}
	_, err := s.client.QuotaKey.UpdateOneID(quotaKeyID).
		SetExhausted(false).
		SetNillableLastRefreshError(nil).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("quota_key_service: clear exhausted: %w", err)
	}
	return nil
}

// casSetExhausted 仅在当前 status='active' 时把 exhausted 写为 true（§3.4 CAS 写）。
// 若 sweep 已将状态改为 expired/disabled，则 0 行受影响，exhausted 不被覆盖。
func (s *QuotaKeyService) casSetExhausted(ctx context.Context, tx *ent.Tx, quotaKeyID int64, pullErr error) error {
	_, err := tx.QuotaKey.Update().
		Where(quotakey.IDEQ(quotaKeyID), quotakey.Status(domain.QuotaKeyStatusActive)).
		SetExhausted(true).
		SetLastRefreshAt(time.Now()).
		SetLastRefreshError(pullErr.Error()).
		Save(ctx)
	if err != nil {
		return err
	}
	return nil
}

// quotaKeyToStatus 将 ent 实体映射为脱敏 DTO。
func quotaKeyToStatus(qk *ent.QuotaKey) *QuotaKeyStatus {
	var lastErr string
	if qk.LastRefreshError != nil {
		lastErr = *qk.LastRefreshError
	}
	return &QuotaKeyStatus{
		ID:               qk.ID,
		Platform:         "", // quota_keys 无 platform 字段（C1 schema 未含），骨架阶段留空
		Status:           qk.Status,
		Exhausted:        qk.Exhausted,
		LastRefreshAt:    qk.LastRefreshAt,
		LastRefreshError: lastErr,
		CodeValueMasked:  maskCodeValue(qk.CodeLast4),
		ExpiresAt:        qk.ExpiresAt,
	}
}

// maskCodeValue 仅保留末 4 位，其余以掩码替代，绝不回显原文。
func maskCodeValue(last4 string) string {
	if last4 == "" {
		return "****"
	}
	return "XXXX-****-****-" + last4
}

// last4Of 取原文末 4 位；不足 4 位时取全部。
func last4Of(plain string) string {
	if len(plain) <= 4 {
		return plain
	}
	return plain[len(plain)-4:]
}

// quotaKeyMACOfPlaintext 计算原文 HMAC-SHA256 指纹（hex）。
// 使用与 account_credentials 同型的 credcrypt.MACKey() 派生密钥，
// 供 uq_quota_keys_code_mac 唯一索引做 SQL 级查重。
func quotaKeyMACOfPlaintext(plain string) string {
	sum := hmac.New(sha256.New, credcrypt.MACKey())
	sum.Write([]byte(plain))
	return hex.EncodeToString(sum.Sum(nil))
}

// isExpired 判断激活码是否已过期（expires_at 非 NULL 且早于 now）。
func isExpired(qk *ent.QuotaKey) bool {
	return qk.ExpiresAt != nil && qk.ExpiresAt.Before(time.Now())
}

// quotaKeyAdvisoryLockID 计算 advisory 锁的 int64 键（与 Pull/Refresh/Delete 共享同一互斥域）。
// 对 quota_key_id 做稳定 fnv 哈希，保证同一 key 映射到同一锁、不同 key 互不阻塞。
func quotaKeyAdvisoryLockID(quotaKeyID int64) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strconv.FormatInt(quotaKeyID, 10)))
	return int64(h.Sum64())
}

// acquireAdvisoryXactLock 在事务内获取 pg_try_advisory_xact_lock（事务级，提交/回滚自动释放）。
// 非阻塞 try；未获取则短暂退避重试，实现同 key 串行互斥。
func acquireAdvisoryXactLock(ctx context.Context, tx *ent.Tx, lockID int64) error {
	for {
		rows, err := tx.QueryContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", lockID)
		if err != nil {
			return fmt.Errorf("acquire advisory xact lock: %w", err)
		}
		var acquired bool
		if !rows.Next() {
			_ = rows.Close()
			return fmt.Errorf("acquire advisory xact lock: no row returned")
		}
		if err := rows.Scan(&acquired); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan advisory xact lock: %w", err)
		}
		_ = rows.Close()
		if acquired {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire advisory xact lock: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
