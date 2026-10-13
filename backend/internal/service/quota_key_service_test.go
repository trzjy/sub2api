//go:build unit

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
)

// fakeQuotaKeyRepo 是 QuotaKeyRepository 的内存测试替身，仅存在于测试代码（合同 §5）。
// 不连接数据库、不发起任何上游请求、不使用 mock 框架。
type fakeQuotaKeyRepo struct {
	byID   map[int64]*ent.QuotaKey
	nextID int64
}

func newFakeQuotaKeyRepo() *fakeQuotaKeyRepo {
	return &fakeQuotaKeyRepo{byID: make(map[int64]*ent.QuotaKey)}
}

func (f *fakeQuotaKeyRepo) GetByID(_ context.Context, id int64) (*ent.QuotaKey, error) {
	qk, ok := f.byID[id]
	if !ok {
		return nil, errQuotaKeyNotFound
	}
	return qk, nil
}

func (f *fakeQuotaKeyRepo) Create(_ context.Context, input CreateQuotaKeyInput) (*ent.QuotaKey, error) {
	f.nextID++
	qk := &ent.QuotaKey{
		ID:            f.nextID,
		CodeEncrypted: input.CodeEncrypted,
		CodeMAC:       input.CodeMAC,
		CodeLast4:     input.CodeLast4,
		Status:        input.Status,
		Exhausted:     input.Exhausted,
		ExpiresAt:     input.ExpiresAt,
	}
	f.byID[qk.ID] = qk
	return qk, nil
}

func (f *fakeQuotaKeyRepo) UpdateStatusFields(_ context.Context, _ *ent.Tx, id int64, status string, exhausted bool, lastError *string, lastRefreshAt *time.Time) error {
	qk, ok := f.byID[id]
	if !ok {
		return errQuotaKeyNotFound
	}
	qk.Status = status
	qk.Exhausted = exhausted
	qk.LastRefreshError = lastError
	qk.LastRefreshAt = lastRefreshAt
	return nil
}

func (f *fakeQuotaKeyRepo) HardDelete(_ context.Context, _ *ent.Tx, id int64) error {
	delete(f.byID, id)
	return nil
}

func (f *fakeQuotaKeyRepo) UpdateCredentials(_ context.Context, id int64, codeEncrypted, codeMAC, codeLast4 string) error {
	qk, ok := f.byID[id]
	if !ok {
		return errQuotaKeyNotFound
	}
	qk.CodeEncrypted = codeEncrypted
	qk.CodeMAC = codeMAC
	qk.CodeLast4 = codeLast4
	return nil
}

func (f *fakeQuotaKeyRepo) ListExpired(_ context.Context, now time.Time) ([]*ent.QuotaKey, error) {
	var out []*ent.QuotaKey
	for _, qk := range f.byID {
		if qk.ExpiresAt != nil && qk.ExpiresAt.Before(now) {
			out = append(out, qk)
		}
	}
	return out, nil
}

func (f *fakeQuotaKeyRepo) ListByStatus(_ context.Context, status string) ([]*ent.QuotaKey, error) {
	var out []*ent.QuotaKey
	for _, qk := range f.byID {
		if qk.Status == status {
			out = append(out, qk)
		}
	}
	return out, nil
}

// errQuotaKeyNotFound 是测试替身的哨兵错误。
var errQuotaKeyNotFound = &quotaKeyNotFoundError{}

type quotaKeyNotFoundError struct{}

func (e *quotaKeyNotFoundError) Error() string { return "quota key not found" }

// TestQuotaKeyGetStatus_MasksCodeValue 验证 DTO 的 code_value_masked 为掩码、不含明文。
func TestQuotaKeyGetStatus_MasksCodeValue(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{
		ID:            1,
		CodeEncrypted: "enc:v1:someciphertext", // 模拟库中存储的密文
		CodeLast4:     "1234",
		Status:        "active",
	}

	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor())
	dto, err := svc.GetStatus(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if dto.CodeValueMasked != "XXXX-****-****-1234" {
		t.Fatalf("unexpected code_value_masked: %q", dto.CodeValueMasked)
	}
	if strings.Contains(dto.CodeValueMasked, "enc:v1:someciphertext") {
		t.Fatalf("code_value_masked leaked ciphertext: %q", dto.CodeValueMasked)
	}
}

// TestQuotaKeyGetStatus_NeverReturnsPlaintext 验证即便 DB 存了明文（测试替身），
// DTO 任何字段都不得出现明文。
func TestQuotaKeyGetStatus_NeverReturnsPlaintext(t *testing.T) {
	plain := "CURSOR-ACTIVATION-CODE-9F3A-1234"
	repo := newFakeQuotaKeyRepo()
	// 故意把密文字段直接写成明文，模拟数据库误存明文的最坏情况。
	repo.byID[1] = &ent.QuotaKey{
		ID:            1,
		CodeEncrypted: plain,
		CodeLast4:     "1234",
		Status:        "active",
		Notes:         &plain, // 备注也写成明文
	}

	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor())
	dto, err := svc.GetStatus(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}

	// 检查每个字符串字段都不含完整明文。
	fields := map[string]string{
		"Platform":         dto.Platform,
		"Status":           dto.Status,
		"LastRefreshError": dto.LastRefreshError,
		"CodeValueMasked":  dto.CodeValueMasked,
	}
	for name, v := range fields {
		if strings.Contains(v, plain) {
			t.Fatalf("DTO field %s leaked plaintext: %q", name, v)
		}
	}

	// 序列化后整体再校验一次（覆盖 json 标签字段）。
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if strings.Contains(string(raw), plain) {
		t.Fatalf("serialized DTO leaked plaintext: %s", string(raw))
	}
}

// TestQuotaKeyRotateCredentials_RoundTrip 验证 Encrypt → 存储 → 读取 → Decrypt 一致。
func TestQuotaKeyRotateCredentials_RoundTrip(t *testing.T) {
	plain := "CURSOR-ACTIVATION-CODE-9F3A-1234"
	repo := newFakeQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: "active"}

	enc := NewCredcryptEncryptor()
	svc := NewQuotaKeyService(repo, enc)

	dto, err := svc.RotateCredentials(context.Background(), 1, plain)
	if err != nil {
		t.Fatalf("RotateCredentials returned error: %v", err)
	}

	// 读回存储的密文并解密，应与原文一致。
	stored := repo.byID[1]
	dec, err := enc.Decrypt([]byte(stored.CodeEncrypted))
	if err != nil {
		t.Fatalf("Decrypt returned error: %v", err)
	}
	if string(dec) != plain {
		t.Fatalf("round-trip mismatch: got %q want %q", string(dec), plain)
	}

	// code_last4 应为原文末 4 位。
	if stored.CodeLast4 != "1234" {
		t.Fatalf("code_last4 = %q, want %q", stored.CodeLast4, "1234")
	}

	// code_mac 非空且为 hex（HMAC-SHA256 = 64 hex 字符）。
	if len(stored.CodeMAC) != 64 {
		t.Fatalf("code_mac length = %d, want 64 (hex HMAC-SHA256)", len(stored.CodeMAC))
	}

	// 返回的 DTO 不应包含明文，且掩码仅揭示末 4 位。
	if strings.Contains(dto.CodeValueMasked, plain) {
		t.Fatalf("dto leaked plaintext: %q", dto.CodeValueMasked)
	}
	if dto.CodeValueMasked != "XXXX-****-****-1234" {
		t.Fatalf("unexpected code_value_masked: %q", dto.CodeValueMasked)
	}
}

// TestQuotaKeyRotateCredentials_PanicsIfNoEncryptor 验证注入 nil 加密器时 panic。
func TestQuotaKeyRotateCredentials_PanicsIfNoEncryptor(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic when encryptor is nil, but did not panic")
		}
	}()

	_ = NewQuotaKeyService(newFakeQuotaKeyRepo(), nil)
}
