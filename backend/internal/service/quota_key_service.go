package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
)

// QuotaKeyRepository 是 QuotaKeyService 依赖的存储契约。
//
// 仅包含与 accounts 表无关的 quota_keys 操作（C3.1 骨架范围）。
// 实现位于 internal/repository（repository 包引用本 service 包的类型，
// 故接口定义在此处以避免 service -> repository 的反向依赖）。
type QuotaKeyRepository interface {
	GetByID(ctx context.Context, quotaKeyID int64) (*ent.QuotaKey, error)
	Create(ctx context.Context, input CreateQuotaKeyInput) (*ent.QuotaKey, error)
	UpdateStatusFields(ctx context.Context, tx *ent.Tx, id int64, status string, exhausted bool, lastError *string, lastRefreshAt *time.Time) error
	HardDelete(ctx context.Context, tx *ent.Tx, id int64) error
	UpdateCredentials(ctx context.Context, id int64, codeEncrypted, codeMAC, codeLast4 string) error
	ListExpired(ctx context.Context, now time.Time) ([]*ent.QuotaKey, error)
	ListByStatus(ctx context.Context, status string) ([]*ent.QuotaKey, error)
}

// QuotaKeyService 提供激活码凭据加密落库/读取的骨架能力。
//
// 本阶段只做：状态读取（脱敏）+ 凭据轮转（加密写库）。
// 禁止：拉号状态机、advisory lock、删除编排、metrics、任何上游 HTTP client、
// 任何 mock / 桩上游（C3.2/C3.3/C5/C6 范围，禁区）。
type QuotaKeyService struct {
	repo QuotaKeyRepository
	enc  QuotaKeyEncryptor
}

// NewQuotaKeyService 构造激活码服务。
// enc 为 nil 时 panic：加密器缺失禁止静默回落明文（合同 §5）。
func NewQuotaKeyService(repo QuotaKeyRepository, enc QuotaKeyEncryptor) *QuotaKeyService {
	if enc == nil {
		panic("quota_key_service: encryptor must not be nil")
	}
	return &QuotaKeyService{repo: repo, enc: enc}
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
