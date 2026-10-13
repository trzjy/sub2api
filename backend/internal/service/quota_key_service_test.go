//go:build unit

package service

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/quotakey"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/lib/pq"
)

// testEncryptor 是 QuotaKeyEncryptor 的测试替身（合同 §5，仅测试代码）：以确定性前缀
// 变换模拟"静态加密"。在 credcrypt 退化为明文 passthrough（未配置主密钥）的环境下，
// 仍能断言 service 层确实把明文交给了加密器、且明文不以原文落库。
type testEncryptor struct{}

func (testEncryptor) Encrypt(plain []byte) ([]byte, error) {
	return []byte("enc:" + string(plain)), nil
}
func (testEncryptor) Decrypt(cipher []byte) ([]byte, error) {
	return bytes.TrimPrefix(cipher, []byte("enc:")), nil
}

// ---------------------------------------------------------------------------
// 既有骨架测试（C3.1）：内存 fake repo，不连数据库、不发起上游请求。
// ---------------------------------------------------------------------------

// fakeQuotaKeyRepo 是 QuotaKeyRepository 的内存测试替身，仅存在于测试代码（合同 §5）。
// 不连接数据库、不发起任何上游请求、不使用 mock 框架。
type fakeQuotaKeyRepo struct {
	byID             map[int64]*ent.QuotaKey
	accounts         []*ent.Account
	nextID           int64
	updateCredsCalls int
	updateMetaCalls  int
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

// MarkExpiredIfStillExpired 内存替身：按"当前值"判定，仅当当前行仍 status=active 且
// expires_at 非空且早于 now 才标 expired。模拟 DB 条件化写入（D-CURSOR-032）。
func (f *fakeQuotaKeyRepo) MarkExpiredIfStillExpired(_ context.Context, _ *ent.Tx, id int64, now time.Time) (int, error) {
	qk, ok := f.byID[id]
	if !ok {
		return 0, errQuotaKeyNotFound
	}
	if qk.Status == domain.QuotaKeyStatusActive && qk.ExpiresAt != nil && qk.ExpiresAt.Before(now) {
		qk.Status = domain.QuotaKeyStatusExpired
		return 1, nil
	}
	return 0, nil
}

func (f *fakeQuotaKeyRepo) HardDelete(_ context.Context, _ *ent.Tx, id int64) error {
	delete(f.byID, id)
	return nil
}

func (f *fakeQuotaKeyRepo) UpdateCredentials(_ context.Context, id int64, codeEncrypted, codeMAC, codeLast4 string) error {
	f.updateCredsCalls++
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

// C3.2 扩展方法（内存替身，满足接口即可，不参与行为断言）。
func (f *fakeQuotaKeyRepo) CountWorkingAccounts(_ context.Context, _ *ent.Tx, _ int64) (int, error) {
	return 0, nil
}

func (f *fakeQuotaKeyRepo) SoftDeleteAccountsByQuotaKey(_ context.Context, _ *ent.Tx, _ int64) ([]int64, error) {
	return nil, nil
}

func (f *fakeQuotaKeyRepo) EnqueueAccountChangedOutbox(_ context.Context, _ *ent.Tx, _ []int64) error {
	return nil
}

func (f *fakeQuotaKeyRepo) SoftDelete(_ context.Context, _ *ent.Tx, _ int64) error {
	return nil
}

func (f *fakeQuotaKeyRepo) ListEligibleForRefresh(_ context.Context) ([]*ent.QuotaKey, error) {
	return nil, nil
}

// C5a 扩展方法（内存替身）。
func (f *fakeQuotaKeyRepo) List(_ context.Context) ([]*ent.QuotaKey, error) {
	var out []*ent.QuotaKey
	for _, qk := range f.byID {
		out = append(out, qk)
	}
	return out, nil
}

func (f *fakeQuotaKeyRepo) UpdateMeta(_ context.Context, id int64, expiresAt **time.Time, notes **string) error {
	f.updateMetaCalls++
	qk, ok := f.byID[id]
	if !ok {
		return errQuotaKeyNotFound
	}
	if expiresAt != nil {
		qk.ExpiresAt = *expiresAt
	}
	if notes != nil {
		qk.Notes = *notes
	}
	return nil
}

func (f *fakeQuotaKeyRepo) ListAccountsByQuotaKey(_ context.Context, quotaKeyID int64) ([]*ent.Account, error) {
	var out []*ent.Account
	for _, a := range f.accounts {
		if a.QuotaKeyID != nil && *a.QuotaKeyID == quotaKeyID {
			out = append(out, a)
		}
	}
	return out, nil
}

// errQuotaKeyNotFound 是测试替身的哨兵错误。
var errQuotaKeyNotFound = &quotaKeyNotFoundError{}

type quotaKeyNotFoundError struct{}

func (e *quotaKeyNotFoundError) Error() string { return "quota key not found" }

// TestQuotaKeyGetStatus_MasksCodeValue 验证 DTO 的 code_masked 为掩码、不含明文。
func TestQuotaKeyGetStatus_MasksCodeValue(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{
		ID:            1,
		CodeEncrypted: "enc:v1:someciphertext", // 模拟库中存储的密文
		CodeLast4:     "1234",
		Status:        "active",
	}

	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)
	dto, err := svc.GetStatus(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if dto.CodeValueMasked != "XXXX-****-****-1234" {
		t.Fatalf("unexpected code_masked: %q", dto.CodeValueMasked)
	}
	if strings.Contains(dto.CodeValueMasked, "enc:v1:someciphertext") {
		t.Fatalf("code_masked leaked ciphertext: %q", dto.CodeValueMasked)
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

	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)
	dto, err := svc.GetStatus(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}

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
}

// TestQuotaKeyRotateCredentials_RoundTrip 验证 Encrypt → 存储 → 读取 → Decrypt 一致。
func TestQuotaKeyRotateCredentials_RoundTrip(t *testing.T) {
	plain := "CURSOR-ACTIVATION-CODE-9F3A-1234"
	repo := newFakeQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: "active"}

	enc := NewCredcryptEncryptor()
	svc := NewQuotaKeyService(repo, enc, nil, nil)

	dto, err := svc.RotateCredentials(context.Background(), 1, plain)
	if err != nil {
		t.Fatalf("RotateCredentials returned error: %v", err)
	}

	stored := repo.byID[1]
	dec, err := enc.Decrypt([]byte(stored.CodeEncrypted))
	if err != nil {
		t.Fatalf("Decrypt returned error: %v", err)
	}
	if string(dec) != plain {
		t.Fatalf("round-trip mismatch: got %q want %q", string(dec), plain)
	}

	if stored.CodeLast4 != "1234" {
		t.Fatalf("code_last4 = %q, want %q", stored.CodeLast4, "1234")
	}
	if len(stored.CodeMAC) != 64 {
		t.Fatalf("code_mac length = %d, want 64 (hex HMAC-SHA256)", len(stored.CodeMAC))
	}
	if strings.Contains(dto.CodeValueMasked, plain) {
		t.Fatalf("dto leaked plaintext: %q", dto.CodeValueMasked)
	}
	if dto.CodeValueMasked != "XXXX-****-****-1234" {
		t.Fatalf("unexpected code_masked: %q", dto.CodeValueMasked)
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

	_ = NewQuotaKeyService(newFakeQuotaKeyRepo(), nil, nil, nil)
}

// ---------------------------------------------------------------------------
// C3.2 数据库集成测试（unit 标签，连本地 quota_test 库）。
// 通过 advisory 事务锁 / FOR UPDATE / 软删验证状态机与编排语义。
// ---------------------------------------------------------------------------

var (
	qkTestOnce    sync.Once
	qkTestClient  *ent.Client
	qkTestInitErr error
)

// quotaKeyTestEntClient 返回连接本地 quota_test 库的 ent client（懒初始化，幂等迁移）。
// DSN 可通过 QUOTA_TEST_DSN 覆盖；默认连 127.0.0.1:5432/quota_test。
func quotaKeyTestEntClient(t *testing.T) *ent.Client {
	t.Helper()
	qkTestOnce.Do(func() {
		dsn := os.Getenv("QUOTA_TEST_DSN")
		if dsn == "" {
			dsn = "postgres://quota_test:quota_test_pw@127.0.0.1:5432/quota_test?sslmode=disable"
		}
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			qkTestInitErr = fmt.Errorf("open sql: %w", err)
			return
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			qkTestInitErr = fmt.Errorf("ping quota_test db (set QUOTA_TEST_DSN if different): %w", err)
			return
		}
		// 用 ent 运行时 schema 在测试库建表（含 regen 后 quota_keys.deleted_at 软删列）。
		// 注意：本测试不依赖 migrations 包，避免 service_test -> repository 的导入环。
		drv := entsql.OpenDB(dialect.Postgres, db)
		client := ent.NewClient(ent.Driver(drv))
		if err := client.Schema.Create(context.Background()); err != nil {
			_ = db.Close()
			qkTestInitErr = fmt.Errorf("schema create: %w", err)
			return
		}
		qkTestClient = client
	})
	if qkTestInitErr != nil {
		t.Fatalf("quota_key test DB init failed: %v", qkTestInitErr)
	}
	return qkTestClient
}

// fakeUpstreamClient 是测试用的上游客户端替身（仅存在于测试代码，合同 §5 允许）。
// batch 非 nil 时表示拉号成功；err 非 nil 时表示上游错误（任意错误 → exhausted）。
// delay 用于拉宽临界区，便于观测 advisory 锁互斥。
type fakeUpstreamClient struct {
	batch      *UpstreamPullResult
	err        error
	delay      time.Duration
	inFlight   *int32
	overlapped *atomic.Bool
}

func (f *fakeUpstreamClient) PullBatch(_ context.Context, _ *ent.QuotaKey) (*UpstreamPullResult, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.inFlight != nil && f.overlapped != nil {
		n := atomic.AddInt32(f.inFlight, 1)
		if n > 1 {
			f.overlapped.Store(true)
		}
		defer atomic.AddInt32(f.inFlight, -1)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.batch, nil
}

func mustCreateQuotaKey(t *testing.T, client *ent.Client, status string) *ent.QuotaKey {
	t.Helper()
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:test").
		SetStatus(status).
		Save(context.Background())
	require.NoError(t, err)
	return qk
}

func mustCreateAccounts(t *testing.T, client *ent.Client, quotaKeyID int64, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, err := client.Account.Create().
			SetName(fmt.Sprintf("acc-%d-%d", quotaKeyID, i)).
			SetPlatform("cursor").
			SetType("oauth").
			SetCredentials(map[string]any{"k": "v"}).
			SetQuotaKeyID(quotaKeyID).
			Save(ctx)
		require.NoError(t, err)
	}
}

func qkNewTestService(t *testing.T, upstream QuotaKeyUpstreamClient) (*QuotaKeyService, *ent.Client) {
	t.Helper()
	client := quotaKeyTestEntClient(t)
	repo := &testDBRepo{client: client}
	return NewQuotaKeyService(repo, NewCredcryptEncryptor(), upstream, client), client
}

// testDBRepo 是 QuotaKeyRepository 的真实实现替身（测试代码内，命中真实 Postgres）。
// 与 production repository 行为一致，但避免 service_test -> repository 的导入环。
type testDBRepo struct {
	client *ent.Client
	// outboxCalls 记录 EnqueueAccountChangedOutbox 每次调用的账号 ID 集合。
	outboxCalls [][]int64
}

func (r *testDBRepo) GetByID(ctx context.Context, id int64) (*ent.QuotaKey, error) {
	return r.client.QuotaKey.Get(ctx, id)
}

func (r *testDBRepo) Create(ctx context.Context, in CreateQuotaKeyInput) (*ent.QuotaKey, error) {
	q := r.client.QuotaKey.Create().SetCodeEncrypted(in.CodeEncrypted)
	if in.CodeMAC != "" {
		q = q.SetCodeMAC(in.CodeMAC)
	}
	if in.CodeLast4 != "" {
		q = q.SetCodeLast4(in.CodeLast4)
	}
	if in.Status != "" {
		q = q.SetStatus(in.Status)
	}
	if in.Exhausted {
		q = q.SetExhausted(true)
	}
	if in.ExpiresAt != nil {
		q = q.SetNillableExpiresAt(in.ExpiresAt)
	}
	if in.Notes != nil {
		q = q.SetNillableNotes(in.Notes)
	}
	return q.Save(ctx)
}

func (r *testDBRepo) UpdateStatusFields(ctx context.Context, tx *ent.Tx, id int64, status string, exhausted bool, lastError *string, lastRefreshAt *time.Time) error {
	q := tx.QuotaKey.UpdateOneID(id).SetStatus(status).SetExhausted(exhausted)
	if lastError != nil {
		q = q.SetNillableLastRefreshError(lastError)
	} else {
		q = q.ClearLastRefreshError()
	}
	if lastRefreshAt != nil {
		q = q.SetNillableLastRefreshAt(lastRefreshAt)
	}
	return q.Exec(ctx)
}

func (r *testDBRepo) HardDelete(ctx context.Context, tx *ent.Tx, id int64) error {
	return tx.QuotaKey.DeleteOneID(id).Exec(ctx)
}

func (r *testDBRepo) UpdateCredentials(ctx context.Context, id int64, codeEncrypted, codeMAC, codeLast4 string) error {
	_, err := r.client.QuotaKey.UpdateOneID(id).
		SetCodeEncrypted(codeEncrypted).SetCodeMAC(codeMAC).SetCodeLast4(codeLast4).Save(ctx)
	return err
}

func (r *testDBRepo) ListExpired(ctx context.Context, now time.Time) ([]*ent.QuotaKey, error) {
	return r.client.QuotaKey.Query().
		Where(quotakey.ExpiresAtNotNil(), quotakey.ExpiresAtLT(now)).All(ctx)
}

// MarkExpiredIfStillExpired 真实 DB 替身：与 production repository 同语义的条件化写入。
func (r *testDBRepo) MarkExpiredIfStillExpired(ctx context.Context, tx *ent.Tx, id int64, now time.Time) (int, error) {
	return tx.QuotaKey.Update().
		Where(quotakey.ID(id), quotakey.Status(domain.QuotaKeyStatusActive), quotakey.ExpiresAtNotNil(), quotakey.ExpiresAtLT(now)).
		SetStatus(domain.QuotaKeyStatusExpired).
		Save(ctx)
}

func (r *testDBRepo) ListByStatus(ctx context.Context, status string) ([]*ent.QuotaKey, error) {
	return r.client.QuotaKey.Query().Where(quotakey.Status(status)).All(ctx)
}

func (r *testDBRepo) CountWorkingAccounts(ctx context.Context, tx *ent.Tx, quotaKeyID int64) (int, error) {
	return tx.Account.Query().
		Where(account.QuotaKeyID(quotaKeyID), account.DeletedAtIsNil()).Count(ctx)
}

func (r *testDBRepo) SoftDeleteAccountsByQuotaKey(ctx context.Context, tx *ent.Tx, quotaKeyID int64) ([]int64, error) {
	ids, err := tx.Account.Query().
		Where(account.QuotaKeyID(quotaKeyID), account.DeletedAtIsNil()).
		IDs(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	_, err = tx.Account.Update().
		Where(account.IDIn(ids...), account.DeletedAtIsNil()).
		SetDeletedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// EnqueueAccountChangedOutbox 记录调用参数供断言使用：scheduler_outbox 表由 migrations
// 建表、不在 ent runtime schema 中，故测试替身只断言 DeleteWithAccounts 确实命中了
// 既有 outbox 入口，并传入了全部被软删账号 ID。
func (r *testDBRepo) EnqueueAccountChangedOutbox(_ context.Context, _ *ent.Tx, accountIDs []int64) error {
	r.outboxCalls = append(r.outboxCalls, accountIDs)
	return nil
}

func (r *testDBRepo) SoftDelete(ctx context.Context, tx *ent.Tx, id int64) error {
	_, err := tx.QuotaKey.UpdateOneID(id).SetDeletedAt(time.Now()).Save(ctx)
	return err
}

func (r *testDBRepo) ListEligibleForRefresh(ctx context.Context) ([]*ent.QuotaKey, error) {
	now := time.Now()
	return r.client.QuotaKey.Query().
		Where(
			quotakey.Status(domain.QuotaKeyStatusActive),
			quotakey.Exhausted(false),
			quotakey.Or(quotakey.ExpiresAtIsNil(), quotakey.ExpiresAtGT(now)),
			quotakey.DeletedAtIsNil(),
		).All(ctx)
}

// C5a 扩展方法（命中真实 Postgres）。
func (r *testDBRepo) List(ctx context.Context) ([]*ent.QuotaKey, error) {
	return r.client.QuotaKey.Query().All(ctx)
}

func (r *testDBRepo) UpdateMeta(ctx context.Context, id int64, expiresAt **time.Time, notes **string) error {
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

func (r *testDBRepo) ListAccountsByQuotaKey(ctx context.Context, quotaKeyID int64) ([]*ent.Account, error) {
	return r.client.Account.Query().Where(account.QuotaKeyID(quotaKeyID)).All(ctx)
}

// TestQuotaKeyPull_Error_SetsExhaustedCAS：任意上游错误 → exhausted=true（CAS 写），
// 且不得插入任何账号。
func TestQuotaKeyPull_Error_SetsExhaustedCAS(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{err: errors.New("upstream boom")})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.True(t, got.Exhausted, "error path must set exhausted=true")
	require.NotNil(t, got.LastRefreshError)
	require.Contains(t, *got.LastRefreshError, "upstream boom")

	// 不得插入任何账号。
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "error path must not insert fake accounts")
}

// TestQuotaKeyCasSetExhausted_OnlyWhenActive：CAS 写仅在 status='active' 时生效，
// 不覆盖 sweep 已置的 expired/disabled。
func TestQuotaKeyCasSetExhausted_OnlyWhenActive(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	active := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, (&QuotaKeyService{}).casSetExhausted(ctx, tx, active.ID, errors.New("boom")))
	require.NoError(t, tx.Commit())
	got, err := client.QuotaKey.Get(ctx, active.ID)
	require.NoError(t, err)
	require.True(t, got.Exhausted, "CAS must set exhausted when status=active")

	// 模拟 sweep 已将状态改为 expired。
	expired := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusExpired)
	tx2, err := client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, (&QuotaKeyService{}).casSetExhausted(ctx, tx2, expired.ID, errors.New("boom")))
	require.NoError(t, tx2.Commit())
	got2, err := client.QuotaKey.Get(ctx, expired.ID)
	require.NoError(t, err)
	require.False(t, got2.Exhausted, "CAS must NOT override sweep-set status")
}

// TestQuotaKeyPull_StopsAtWorkingCount15：working 账号数达到上限（>=15）即停止拉号，
// 不调用上游、不改 exhausted、不插入。
func TestQuotaKeyPull_StopsAtWorkingCount15(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// 成功上游：若被调用会插入账号；此处用于验证它不会被调用。
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{{Name: "x", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}}}},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, MaxAccountsPerQuotaKey) // 15 个

	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, got.Exhausted, "stop-at-limit must not set exhausted")

	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, MaxAccountsPerQuotaKey, n, "must not insert beyond the cap")
}

// TestQuotaKeyPull_TruncatesBatchToRemainingCapacity：14 + 上游返 2 ⇒ 只落 1 条，
// 总数恰好 15（D-CURSOR-026 #2：≤15 在拉号批次溢出路径不可突破）。
func TestQuotaKeyPull_TruncatesBatchToRemainingCapacity(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "ovf-1", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}},
			{Name: "ovf-2", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}},
		}},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, MaxAccountsPerQuotaKey-1) // 14

	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, MaxAccountsPerQuotaKey, n, "14 working + 2 upstream must be truncated to exactly 15")

	// 溢出条目不落库（UpstreamPullResult 无重放字段，溢出的号按丢弃处理）。
	overflow, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID), account.NameEQ("ovf-2")).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, overflow, "overflowing upstream accounts must be dropped, not persisted")
}

// TestQuotaKeyPull_SuccessClearsLastRefreshError：成功拉号必须清空 last_refresh_error
// （D-CURSOR-026 #5；SetNillableLastRefreshError(nil) 对 nil 指针是 no-op，故须 Clear）。
func TestQuotaKeyPull_SuccessClearsLastRefreshError(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "ok-1", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}},
		}},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 前置失败态：exhausted=true 且 last_refresh_error 已置位。
	_, err := client.QuotaKey.UpdateOneID(qk.ID).
		SetExhausted(true).
		SetLastRefreshError("previous upstream boom").
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, got.Exhausted, "success path must clear exhausted")
	require.Nil(t, got.LastRefreshError, "success path must null out last_refresh_error")
	require.NotNil(t, got.LastRefreshAt, "success path must refresh last_refresh_at")
}

// TestQuotaKeyClearExhausted_ClearsLastRefreshError：ClearExhausted 必须同时清错误占位。
func TestQuotaKeyClearExhausted_ClearsLastRefreshError(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{err: errors.New("unused")})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	_, err := client.QuotaKey.UpdateOneID(qk.ID).
		SetExhausted(true).
		SetLastRefreshError("upstream boom").
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, svc.ClearExhausted(ctx, qk.ID))

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, got.Exhausted, "ClearExhausted must clear the flag")
	require.Nil(t, got.LastRefreshError, "ClearExhausted must null out last_refresh_error")
}

// TestQuotaKeyDeleteWithAccounts_EnqueuesSchedulerOutbox：软删必须写既有 scheduler outbox，
// 使被软删账号经既有 consumer 从 Redis 调度快照移除（D-CURSOR-026 #4）。
func TestQuotaKeyDeleteWithAccounts_EnqueuesSchedulerOutbox(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	repo := &testDBRepo{client: client}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), &fakeUpstreamClient{err: errors.New("unused")}, client)

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, 3)

	require.NoError(t, svc.DeleteWithAccounts(ctx, qk.ID))

	// 既有 outbox 入口被触发，且覆盖全部被软删账号（一个不多一个不少）。
	require.Len(t, repo.outboxCalls, 1, "delete must hit the scheduler outbox entry exactly once")
	softDeleted, err := client.Account.Query().
		Where(account.QuotaKeyID(qk.ID), account.DeletedAtNotNil()).
		IDs(mixins.SkipSoftDelete(ctx))
	require.NoError(t, err)
	require.Len(t, softDeleted, 3, "all three accounts must be soft-deleted")
	require.ElementsMatch(t, softDeleted, repo.outboxCalls[0],
		"outbox must cover exactly the soft-deleted accounts")
}

// TestQuotaKeyPull_ReadsStatusBeforeAfter：拉号前/中/后三读驱动状态转移。
// 成功上游 → 插入账号并清 exhausted；错误上游 → 置 exhausted。
func TestQuotaKeyPull_ReadsStatusBeforeAfter(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 成功路径：清 exhausted 并插入账号。
	okSvc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "a1", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"t": "1"}},
			{Name: "a2", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"t": "2"}},
		}},
	})
	qkOK := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	require.NoError(t, okSvc.PullAccounts(ctx, qkOK.ID))
	gotOK, err := client.QuotaKey.Get(ctx, qkOK.ID)
	require.NoError(t, err)
	require.False(t, gotOK.Exhausted, "success path clears exhausted")
	nOK, err := client.Account.Query().Where(account.QuotaKeyID(qkOK.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, nOK, "success path inserts pulled accounts")
}

// TestQuotaKeyClearExhausted_ThenRepull：清 exhausted 后可再次拉号（成功路径插入）。
func TestQuotaKeyClearExhausted_ThenRepull(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "b1", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"t": "1"}},
		}},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 先置 exhausted（模拟此前失败）。
	_, err := client.QuotaKey.UpdateOneID(qk.ID).SetExhausted(true).Save(ctx)
	require.NoError(t, err)

	require.NoError(t, svc.ClearExhausted(ctx, qk.ID))
	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, got.Exhausted, "ClearExhausted must clear the flag")

	// 再次拉号（成功路径），应插入账号并维持非 exhausted。
	require.NoError(t, svc.PullAccounts(ctx, qk.ID))
	got2, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, got2.Exhausted)
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "re-pull after clear must insert accounts")
}

// TestQuotaKeyLockMutex_SameKeySerial：同 key 并发拉号必须串行（不重叠临界区）。
func TestQuotaKeyLockMutex_SameKeySerial(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	var inFlight int32
	var overlapped atomic.Bool
	// 两个并发调用共用同一 key 与同一上游（带延时，拉宽临界区）。
	up := &fakeUpstreamClient{
		delay:      300 * time.Millisecond,
		inFlight:   &inFlight,
		overlapped: &overlapped,
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "m", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}},
		}},
	}
	svc, _ := qkNewTestService(t, up)

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, svc.PullAccounts(ctx, qk.ID), "same-key concurrent pull must succeed under advisory lock")
		}()
	}
	wg.Wait()

	require.False(t, overlapped.Load(), "same-key concurrent pulls must not overlap critical section")
}

// TestQuotaKeyLockMutex_DiffKeyNoBlock：异 key 并发拉号互不阻塞（可并发执行）。
func TestQuotaKeyLockMutex_DiffKeyNoBlock(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	var inFlight int32
	var overlapped atomic.Bool
	up := &fakeUpstreamClient{
		delay:      300 * time.Millisecond,
		inFlight:   &inFlight,
		overlapped: &overlapped,
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "m", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}},
		}},
	}
	svc, _ := qkNewTestService(t, up)

	k1 := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	k2 := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	start := time.Now()
	var wg sync.WaitGroup
	for _, id := range []int64{k1.ID, k2.ID} {
		wg.Add(1)
		go func(keyID int64) {
			defer wg.Done()
			require.NoError(t, svc.PullAccounts(ctx, keyID), "different-key pull must not block")
		}(id)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// 若互不阻塞，二者并发执行，总耗时接近单次延时（< 2*延时）。
	require.Less(t, elapsed, 2*up.delay+200*time.Millisecond, "different keys must not block each other")
}

// TestQuotaKeyDeleteWithAccounts_NoOrphan：软删不留孤儿——
// 账号行仍存在于库（SkipSoftDelete 可取），但号池/列表查询取不到。
func TestQuotaKeyDeleteWithAccounts_NoOrphan(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{err: errors.New("unused")})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, 3)

	require.NoError(t, svc.DeleteWithAccounts(ctx, qk.ID))

	// quota_key 普通查询不可见（已软删），但 SkipSoftDelete 可见（行保留）。
	_, err := client.QuotaKey.Get(ctx, qk.ID)
	require.True(t, ent.IsNotFound(err), "soft-deleted quota key must be invisible to normal query")
	_, err = client.QuotaKey.Get(mixins.SkipSoftDelete(ctx), qk.ID)
	require.NoError(t, err, "soft-deleted row must still exist in DB")

	// 账号：号池查询取不到（普通查询排除 deleted_at），但库中仍保留（SkipSoftDelete=3）。
	normal, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, normal, "pool query must not return soft-deleted accounts")

	kept, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(mixins.SkipSoftDelete(ctx))
	require.NoError(t, err)
	require.Equal(t, 3, kept, "soft-deleted accounts must remain in DB (no physical DELETE)")
}

// TestQuotaKeyPlaceholderUpstream_ReturnsErrorNotFake：生产默认占位客户端返回错误、
// 走 exhausted 分支，绝不插入假账号。
func TestQuotaKeyPlaceholderUpstream_ReturnsErrorNotFake(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// upstream=nil → 默认 NotConnectedUpstreamClient（失败占位）。
	svc, _ := qkNewTestService(t, nil)

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.True(t, got.Exhausted, "placeholder upstream must drive exhausted=true")
	require.NotNil(t, got.LastRefreshError)
	require.Contains(t, *got.LastRefreshError, "not connected")

	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "placeholder must NOT insert fake accounts")
}

// ---------------------------------------------------------------------------
// C5a 扩展单测（D-CURSOR-020）：admin CRUD 的 service 层（内存 fake repo，不连库）。
// 核心不变量：明文 code 只进不出（F1）；GetWithAccounts 不嵌套聚合（F7）。
// ---------------------------------------------------------------------------

// TestQuotaKeyService_CreateEncryptsAndMasks：Create 后 repo 收到密文（非明文），
// 返回的 CodeValueMasked 不含明文。
func TestQuotaKeyService_CreateEncryptsAndMasks(t *testing.T) {
	plain := "CURSOR-ACTIVATION-CODE-9F3A-1234"
	repo := newFakeQuotaKeyRepo()
	enc := testEncryptor{}
	svc := NewQuotaKeyService(repo, enc, nil, nil)

	dto, err := svc.Create(context.Background(), plain, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, dto)

	stored := repo.byID[dto.ID]
	// 明文必须经过加密器：库中所存为加密器输出（前缀 enc:），而非原文。
	require.Equal(t, "enc:"+plain, stored.CodeEncrypted, "repo must receive the encryptor output, not raw plaintext")
	require.NotEqual(t, plain, stored.CodeEncrypted, "repo must NOT receive raw plaintext")

	require.Equal(t, "XXXX-****-****-1234", dto.CodeValueMasked)
	require.NotContains(t, dto.CodeValueMasked, plain, "masked value must not leak plaintext")
}

// TestQuotaKeyService_ListNeverReturnsPlaintext：即便 DB 误存明文（最坏情况），
// List 返回项的任一字段都不得出现明文。
func TestQuotaKeyService_ListNeverReturnsPlaintext(t *testing.T) {
	plain := "CURSOR-ACTIVATION-CODE-9F3A-1234"
	repo := newFakeQuotaKeyRepo()
	// 故意把密文字段直接写成明文，模拟数据库误存明文的最坏情况。
	repo.byID[1] = &ent.QuotaKey{ID: 1, CodeEncrypted: plain, CodeLast4: "1234", Status: "active", Notes: &plain}
	repo.byID[2] = &ent.QuotaKey{ID: 2, CodeEncrypted: plain, CodeLast4: "9F3A", Status: "expired"}

	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)
	dtos, err := svc.List(context.Background())
	require.NoError(t, err)
	require.Len(t, dtos, 2)

	for _, dto := range dtos {
		fields := map[string]string{
			"Platform":         dto.Platform,
			"Status":           dto.Status,
			"LastRefreshError": dto.LastRefreshError,
			"CodeValueMasked":  dto.CodeValueMasked,
		}
		for name, v := range fields {
			require.NotContains(t, v, plain, "DTO field %s leaked plaintext", name)
		}
	}
}

// TestQuotaKeyService_UpdateReencryptsCode：传 code → 仅 UpdateCredentials 被调用（密文），
// 不触碰 UpdateMeta。
func TestQuotaKeyService_UpdateReencryptsCode(t *testing.T) {
	oldCipher := "enc:v1:oldsomething"
	repo := newFakeQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, CodeEncrypted: oldCipher, CodeLast4: "oldx", Status: "active"}

	enc := testEncryptor{}
	svc := NewQuotaKeyService(repo, enc, nil, nil)

	newCode := "CURSOR-ACTIVATION-CODE-NEW9-7777"
	dto, err := svc.Update(context.Background(), 1, &newCode, nil, nil)
	require.NoError(t, err)

	require.Equal(t, 1, repo.updateCredsCalls, "UpdateCredentials must be called exactly once")
	require.Equal(t, 0, repo.updateMetaCalls, "UpdateMeta must NOT be called when code is the only change")

	stored := repo.byID[1]
	require.Equal(t, "enc:"+newCode, stored.CodeEncrypted, "code must be re-encrypted via encryptor")
	require.NotEqual(t, oldCipher, stored.CodeEncrypted, "code must be re-encrypted")
	require.NotEqual(t, newCode, stored.CodeEncrypted, "stored value must be ciphertext, not plaintext")
	require.Equal(t, "7777", stored.CodeLast4)
	require.Equal(t, "XXXX-****-****-7777", dto.CodeValueMasked)
	require.NotContains(t, dto.CodeValueMasked, newCode)
}

// TestQuotaKeyService_UpdateMetaTriState：省略不改、null 清空、有值设置；均不触碰 credentials/code/status。
func TestQuotaKeyService_UpdateMetaTriState(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	initialExpiresAt := time.Now().Add(-time.Hour)
	initialNotes := "keep"
	repo.byID[1] = &ent.QuotaKey{
		ID:            1,
		CodeEncrypted: "enc:v1:keepme",
		CodeLast4:     "keep",
		Status:        "active",
		ExpiresAt:     &initialExpiresAt,
		Notes:         &initialNotes,
	}

	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)

	_, err := svc.Update(context.Background(), 1, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 0, repo.updateMetaCalls, "omitted metadata must not call UpdateMeta")
	require.Equal(t, initialExpiresAt, *repo.byID[1].ExpiresAt)
	require.Equal(t, initialNotes, *repo.byID[1].Notes)

	var clearExpiresAt *time.Time
	var clearNotes *string
	_, err = svc.Update(context.Background(), 1, nil, &clearExpiresAt, &clearNotes)
	require.NoError(t, err)
	require.Equal(t, 1, repo.updateMetaCalls)
	require.Nil(t, repo.byID[1].ExpiresAt, "explicit null must clear expires_at")
	require.Nil(t, repo.byID[1].Notes, "explicit null must clear notes")

	expiresAt := time.Now().Add(24 * time.Hour)
	notes := "rotation note"
	expiresAtValue := &expiresAt
	notesValue := &notes
	dto, err := svc.Update(context.Background(), 1, nil, &expiresAtValue, &notesValue)
	require.NoError(t, err)
	require.Equal(t, 2, repo.updateMetaCalls)

	stored := repo.byID[1]
	require.Equal(t, 0, repo.updateCredsCalls, "metadata updates must not call UpdateCredentials")
	require.Equal(t, "enc:v1:keepme", stored.CodeEncrypted, "code must remain untouched")
	require.Equal(t, "active", stored.Status, "status must remain untouched")
	require.Equal(t, "keep", stored.CodeLast4, "code_last4 must remain untouched")
	require.NotNil(t, stored.ExpiresAt)
	require.WithinDuration(t, expiresAt, *stored.ExpiresAt, time.Second)
	require.NotNil(t, stored.Notes)
	require.Equal(t, notes, *stored.Notes)
	require.Equal(t, "XXXX-****-****-keep", dto.CodeValueMasked)
}

// TestQuotaKeyService_GetWithAccounts_NoNesting：返回脱敏 status + 账号切片，
// 二者为独立返回值（非聚合对象，F7）。
func TestQuotaKeyService_GetWithAccounts_NoNesting(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{ID: 1, CodeEncrypted: "enc:v1:x", CodeLast4: "abcd", Status: "active"}
	qk1 := int64(1)
	qk2 := int64(2)
	repo.accounts = []*ent.Account{
		{ID: 10, QuotaKeyID: &qk1, Name: "acc-1"},
		{ID: 11, QuotaKeyID: &qk1, Name: "acc-2"},
		{ID: 12, QuotaKeyID: &qk2, Name: "acc-other"}, // 归属其它激活码，不应出现在结果中
	}

	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)
	status, accounts, err := svc.GetWithAccounts(context.Background(), 1)
	require.NoError(t, err)

	require.NotNil(t, status)
	require.Equal(t, "XXXX-****-****-abcd", status.CodeValueMasked, "status must be masked")
	require.NotContains(t, status.CodeValueMasked, "enc:v1:x")

	// 账号以独立切片返回，且只含归属本激活码的账号（非聚合对象）。
	require.NotNil(t, accounts)
	require.Len(t, accounts, 2)
	for _, a := range accounts {
		require.NotNil(t, a.QuotaKeyID)
		require.Equal(t, int64(1), *a.QuotaKeyID, "only accounts of the requested quota key should be returned")
	}
}
