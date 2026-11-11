//go:build unit

package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/ent/quotakey"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/lib/pq"
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

	// 观测/换码事务面（W2-B1）的观测点：
	// updateSnapshotCalls 记录同事务快照写入次数（无论成功）；
	// failSnapshotWrite 注入"存储失败"（§25.14-C 步骤 5：不得报成功）；
	// clearSnapshotCalls 记录凭据写入时清空快照的次数（异 MAC 才清）。
	updateSnapshotCalls int
	failSnapshotWrite   bool
	clearSnapshotCalls  int

	// SoftDeleteCursorAccountsByExpiredQuotaKey 的观测点（仅满足接口 + 行为断言）：
	// cursorDeleteCalls 记录调用次数；cursorDeleteKeyIDs 记录每次传入的 quotaKeyID；
	// cursorDeleteErr 非 nil 时直接返回该错误；否则返回 cursorDeletedIDs。
	cursorDeletedIDs   []int64
	cursorDeleteErr    error
	cursorDeleteCalls  int
	cursorDeleteKeyIDs []int64
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
	priority := input.Priority
	if priority <= 0 {
		priority = 1 // 列默认值
	}
	qk := &ent.QuotaKey{
		ID:            f.nextID,
		CodeEncrypted: input.CodeEncrypted,
		CodeMAC:       input.CodeMAC,
		CodeLast4:     input.CodeLast4,
		Status:        input.Status,
		Exhausted:     input.Exhausted,
		ExpiresAt:     input.ExpiresAt,
		Priority:      priority,
	}
	f.byID[qk.ID] = qk
	return qk, nil
}

// UpdatePriority 内存替身：改写码优先级，并同步该码名下未软删账号的 priority
// （与生产仓储同口径：软删账号不改，返回同步行数）。
func (f *fakeQuotaKeyRepo) UpdatePriority(_ context.Context, _ *ent.Tx, id int64, priority int) (int, error) {
	qk, ok := f.byID[id]
	if !ok {
		return 0, errQuotaKeyNotFound
	}
	qk.Priority = priority
	synced := 0
	for _, acc := range f.accounts {
		if acc == nil || acc.QuotaKeyID == nil || *acc.QuotaKeyID != id || acc.DeletedAt != nil {
			continue
		}
		acc.Priority = priority
		synced++
	}
	return synced, nil
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

// GetByIDInTx 内存替身：无真实事务，恒按内存行返回（tx 参数忽略）。
func (f *fakeQuotaKeyRepo) GetByIDInTx(_ context.Context, _ *ent.Tx, id int64) (*ent.QuotaKey, error) {
	qk, ok := f.byID[id]
	if !ok {
		return nil, errQuotaKeyNotFound
	}
	return qk, nil
}

// UpdateUsageSnapshot 内存替身：写 usage_snapshot，合法期限同事务写 expires_at（nil 保留原值）。
func (f *fakeQuotaKeyRepo) UpdateUsageSnapshot(_ context.Context, _ *ent.Tx, id int64, snapshot []byte, expiresAt **time.Time) error {
	f.updateSnapshotCalls++
	qk, ok := f.byID[id]
	if !ok {
		return errQuotaKeyNotFound
	}
	if f.failSnapshotWrite {
		return errors.New("injected usage snapshot write failure")
	}
	qk.UsageSnapshot = append([]byte(nil), snapshot...)
	if expiresAt != nil && *expiresAt != nil {
		qk.ExpiresAt = *expiresAt
	}
	return nil
}

// UpdateCredentialsAtomically 内存替身：写凭据；clearUsageSnapshot=true 时清空快照。
func (f *fakeQuotaKeyRepo) UpdateCredentialsAtomically(_ context.Context, _ *ent.Tx, id int64, codeEncrypted, codeMAC, codeLast4 string, clearUsageSnapshot bool) error {
	f.updateCredsCalls++
	qk, ok := f.byID[id]
	if !ok {
		return errQuotaKeyNotFound
	}
	qk.CodeEncrypted = codeEncrypted
	qk.CodeMAC = codeMAC
	qk.CodeLast4 = codeLast4
	if clearUsageSnapshot {
		f.clearSnapshotCalls++
		qk.UsageSnapshot = nil
	}
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

// SoftDeleteCursorAccountsByExpiredQuotaKey 内存替身：记录调用次数与 key ID，
// cursorDeleteErr 非 nil 时返回该错误，否则返回预设的 cursorDeletedIDs。
// 不参与"到期判定 / 平台过滤"的行为断言（见 testDBRepo 与 production repository）。
func (f *fakeQuotaKeyRepo) SoftDeleteCursorAccountsByExpiredQuotaKey(_ context.Context, _ *ent.Tx, quotaKeyID int64, _ time.Time) ([]int64, error) {
	f.cursorDeleteCalls++
	f.cursorDeleteKeyIDs = append(f.cursorDeleteKeyIDs, quotaKeyID)
	if f.cursorDeleteErr != nil {
		return nil, f.cursorDeleteErr
	}
	return f.cursorDeletedIDs, nil
}

func (f *fakeQuotaKeyRepo) SoftDelete(_ context.Context, _ *ent.Tx, _ int64) error {
	return nil
}

func (f *fakeQuotaKeyRepo) ListEligibleForRefresh(_ context.Context) ([]*ent.QuotaKey, error) {
	return nil, nil
}

// C5a 扩展方法（内存替身）。List 按 ID 升序返回：真实仓储是稳定有序的查询结果，
// 替身若按 map 迭代返回会让"逐来源扫描"类断言随实现抖动（reconciler 同轮刷新用例依赖顺序）。
func (f *fakeQuotaKeyRepo) List(_ context.Context) ([]*ent.QuotaKey, error) {
	out := make([]*ent.QuotaKey, 0, len(f.byID))
	for _, qk := range f.byID {
		out = append(out, qk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeQuotaKeyRepo) UpdateMeta(_ context.Context, _ *ent.Tx, id int64, expiresAt **time.Time, notes **string) error {
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

// ListAccountsByQuotaKeys 批量返回归属给定激活码的账号（一次遍历 → 内存按 key 过滤），
// 与真实仓储的 QuotaKeyIDIn 单查询语义对齐，杜绝逐激活码查询的 N+1。
func (f *fakeQuotaKeyRepo) ListAccountsByQuotaKeys(_ context.Context, ids []int64) ([]*ent.Account, error) {
	var out []*ent.Account
	for _, a := range f.accounts {
		if a.QuotaKeyID == nil {
			continue
		}
		for _, id := range ids {
			if *a.QuotaKeyID == id {
				out = append(out, a)
				break
			}
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
	preserveCursorQuotaKeyStateProvider(t)
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
	preserveCursorQuotaKeyStateProvider(t)
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

// preserveCursorQuotaKeyStateProvider 在用例构造 fake-backed QuotaKeyService 前后保存/恢复
// 包级 CursorQuotaKeyStateProvider。
//
// 为什么必须恢复：NewQuotaKeyService 会把自身安装成包级调度资格门的状态读取器（生产装配
// 副作用）。若用例用内存替身仓储构造服务且不恢复，同包后续的**真实 DB** 用例（水位计数 /
// 资格门）会读到替身仓储，读不到自己的激活码 → 失败关闭 → 计数变 0。这是用例间的全局
// 状态污染，不是业务缺陷；恢复后顺序无关。
func preserveCursorQuotaKeyStateProvider(t *testing.T) {
	t.Helper()
	prev := currentCursorQuotaKeyStateProvider()
	t.Cleanup(func() { InstallCursorQuotaKeyStateProvider(prev) })
}

// TestQuotaKeyRotateCredentials_RoundTrip 验证 Encrypt → 存储 → 读取 → Decrypt 一致。
func TestQuotaKeyRotateCredentials_RoundTrip(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
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
	// onPull 是每次上游被调用时的钩子（参数为被拉号的 key）。R4-2：停轮断言改为直接记录
	// 上游调用过的 key ID，不再以"账号数为代理"间接推断。
	onPull func(qk *ent.QuotaKey)
}

func (f *fakeUpstreamClient) PullBatch(_ context.Context, qk *ent.QuotaKey) (*UpstreamPullResult, error) {
	if f.onPull != nil {
		f.onPull(qk)
	}
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

// RealSupplyModelsFor / TargetAccountsPerKeyFor：测试替身默认按 shareai 口径
// （16 个静态模型 / 目标 5），不干扰既有单测对 shareai 路径的断言。
func (f *fakeUpstreamClient) RealSupplyModelsFor(_ string) ([]string, error) {
	return CursorStaticEntryModels(), nil
}

func (f *fakeUpstreamClient) TargetAccountsPerKeyFor(_ string) (int, error) {
	return CursorAutoPullAccountsPerKey, nil
}

// ---------------------------------------------------------------------------
// KC-17：upstreamDispatchingClient 分派单测（不连 DB、不发起真实上游请求）。
// 仅验证"按 upstream_key 选路 + 未知标识失败关闭"语义，不触碰既有 shareAI/knCy 行为。
// ---------------------------------------------------------------------------

// recordingUpstreamClient 是 QuotaKeyUpstreamClient 的带调用记录的测试替身，仅用于断言分派选路。
type recordingUpstreamClient struct {
	calls       int
	lastQK      *ent.QuotaKey
	toReturn    *UpstreamPullResult
	errToReturn error
}

func (r *recordingUpstreamClient) PullBatch(_ context.Context, qk *ent.QuotaKey) (*UpstreamPullResult, error) {
	r.calls++
	r.lastQK = qk
	if r.errToReturn != nil {
		return nil, r.errToReturn
	}
	return r.toReturn, nil
}

// RealSupplyModelsFor / TargetAccountsPerKeyFor：仅用于分派选路断言的替身，
// 默认按 shareai 口径返回（静态 16 模型 / 目标 5）。
func (r *recordingUpstreamClient) RealSupplyModelsFor(_ string) ([]string, error) {
	return CursorStaticEntryModels(), nil
}

func (r *recordingUpstreamClient) TargetAccountsPerKeyFor(_ string) (int, error) {
	return CursorAutoPullAccountsPerKey, nil
}

// entQuotaKeyWithUpstreamKey 构造一个带指定 upstream_key 的激活码对象（仅字段级，不落库）。
func entQuotaKeyWithUpstreamKey(key string) *ent.QuotaKey {
	qk := &ent.QuotaKey{}
	qk.UpstreamKey = key
	return qk
}

// TestUpstreamDispatchingClient_SelectsByUpstreamKey 验证按 upstream_key 选路：
// shareai → shareAI 客户端；kn_cy → knCy 客户端；两者互不串路。
func TestUpstreamDispatchingClient_SelectsByUpstreamKey(t *testing.T) {
	shareAI := &recordingUpstreamClient{toReturn: &UpstreamPullResult{Accounts: []*UpstreamAccount{{Name: "a"}}}}
	knCy := &recordingUpstreamClient{toReturn: &UpstreamPullResult{Accounts: []*UpstreamAccount{{Name: "b"}}}}

	dc, err := NewUpstreamDispatchingClient(shareAI, knCy)
	require.NoError(t, err)

	// shareai 路径：只命中 shareAI 客户端。
	res, err := dc.PullBatch(context.Background(), entQuotaKeyWithUpstreamKey(upstreamKeyShareAI))
	require.NoError(t, err)
	require.Len(t, res.Accounts, 1)
	require.Equal(t, "a", res.Accounts[0].Name)
	require.Equal(t, 1, shareAI.calls, "shareai key must route to shareAI client")
	require.Equal(t, 0, knCy.calls, "shareai key must NOT route to knCy client")

	// kn_cy 路径：只命中 knCy 客户端。
	res, err = dc.PullBatch(context.Background(), entQuotaKeyWithUpstreamKey(upstreamKeyKnCy))
	require.NoError(t, err)
	require.Len(t, res.Accounts, 1)
	require.Equal(t, "b", res.Accounts[0].Name)
	require.Equal(t, 1, knCy.calls, "kn_cy key must route to knCy client")
	require.Equal(t, 1, shareAI.calls, "kn_cy key must NOT route to shareAI client")
}

// TestUpstreamDispatchingClient_UnknownKeyFailsClosed 验证未知/空上游标识失败关闭：
// 返回 error，绝不静默回落到任一已知实现（回落会让 kn-cy 卡密去打 ShareAI 上游）。
func TestUpstreamDispatchingClient_UnknownKeyFailsClosed(t *testing.T) {
	for _, badKey := range []string{"", "unknown", "KN_CY", "shareai "} {
		shareAI := &recordingUpstreamClient{}
		knCy := &recordingUpstreamClient{}
		dc, err := NewUpstreamDispatchingClient(shareAI, knCy)
		require.NoError(t, err)

		res, err := dc.PullBatch(context.Background(), entQuotaKeyWithUpstreamKey(badKey))
		require.Error(t, err, "unknown/empty upstream key %q must fail closed", badKey)
		require.Nil(t, res, "must not return a result on unknown key")
		require.Contains(t, err.Error(), "unknown upstream key")
		require.Equal(t, 0, shareAI.calls, "unknown key must NOT call shareAI client (no fallback)")
		require.Equal(t, 0, knCy.calls, "unknown key must NOT call knCy client (no fallback)")
	}
}

// dispatchProbeClient 是 QuotaKeyUpstreamClient 的测试替身，记录被分派到的上游标识
// 与被问及的实供模型/每码目标账号数，用于断言分派器的按 key 选路语义（KC-26）。
type dispatchProbeClient struct {
	models    []string
	target    int
	modelKey  string
	targetKey string
}

func (p *dispatchProbeClient) PullBatch(_ context.Context, _ *ent.QuotaKey) (*UpstreamPullResult, error) {
	return &UpstreamPullResult{}, nil
}

func (p *dispatchProbeClient) RealSupplyModelsFor(k string) ([]string, error) {
	p.modelKey = k
	return p.models, nil
}

func (p *dispatchProbeClient) TargetAccountsPerKeyFor(k string) (int, error) {
	p.targetKey = k
	return p.target, nil
}

// TestUpstreamDispatchingClient_DispatchesSupplyAndTargetByKey 验证 RealSupplyModelsFor /
// TargetAccountsPerKeyFor 同样按 upstream_key 选路，且未知标识失败关闭（KC-26）。
func TestUpstreamDispatchingClient_DispatchesSupplyAndTargetByKey(t *testing.T) {
	shareAI := &dispatchProbeClient{models: []string{"a"}, target: 5}
	knCy := &dispatchProbeClient{models: []string{"b", "c"}, target: 1}
	dc, err := NewUpstreamDispatchingClient(shareAI, knCy)
	require.NoError(t, err)

	// shareai 路径：只问 shareAI 客户端，不得串路到 knCy。
	models, err := dc.RealSupplyModelsFor(upstreamKeyShareAI)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, models)
	require.Equal(t, upstreamKeyShareAI, shareAI.modelKey)
	require.Empty(t, knCy.modelKey, "shareai key must NOT dispatch to knCy client")

	// kn_cy 路径：只问 knCy 客户端，目标 1。
	target, err := dc.TargetAccountsPerKeyFor(upstreamKeyKnCy)
	require.NoError(t, err)
	require.Equal(t, 1, target)
	require.Equal(t, upstreamKeyKnCy, knCy.targetKey)

	// 未知标识失败关闭：绝不静默按 shareai 处理。
	_, err = dc.RealSupplyModelsFor("bogus")
	require.Error(t, err)
	_, err = dc.TargetAccountsPerKeyFor("bogus")
	require.Error(t, err)
}

// TestUpstreamDispatchingClient_DefaultShareAI 验证存量激活码的默认 upstream_key='shareai'
// 走 shareAI 路径（KC-16 字段 NOT NULL DEFAULT 'shareai'）。
func TestUpstreamDispatchingClient_DefaultShareAI(t *testing.T) {
	shareAI := &recordingUpstreamClient{toReturn: &UpstreamPullResult{}}
	knCy := &recordingUpstreamClient{}
	dc, err := NewUpstreamDispatchingClient(shareAI, knCy)
	require.NoError(t, err)

	// 模拟存量默认值的读取结果（入库时 NOT NULL DEFAULT 'shareai'）。
	_, err = dc.PullBatch(context.Background(), entQuotaKeyWithUpstreamKey(upstreamKeyShareAI))
	require.NoError(t, err)
	require.Equal(t, 1, shareAI.calls, "default 'shareai' key must route to shareAI client")
	require.Equal(t, 0, knCy.calls, "default 'shareai' key must NOT route to knCy client")
}

// TestNewUpstreamDispatchingClient_NilArgs 验证构造器对 nil 实参失败关闭。
func TestNewUpstreamDispatchingClient_NilArgs(t *testing.T) {
	_, err := NewUpstreamDispatchingClient(nil, &recordingUpstreamClient{})
	require.Error(t, err, "nil shareAI client must fail")
	_, err = NewUpstreamDispatchingClient(&recordingUpstreamClient{}, nil)
	require.Error(t, err, "nil knCy client must fail")
}

// TestNewQuotaKeyService_DispatcherResolvesInnerShareAI 验证分派器场景下 P1-B4 构造闭环
// 被保留：NewQuotaKeyService 必须穿透分派器、把自身回填到内嵌的 *shareAIUpstreamClient
// 的 deviceResolver（照抄既有"先建客户端→后回填 resolver"顺序）。
func TestNewQuotaKeyService_DispatcherResolvesInnerShareAI(t *testing.T) {
	sac := &shareAIUpstreamClient{enc: testEncryptor{}}
	dc, err := NewUpstreamDispatchingClient(sac, &fakeUpstreamClient{})
	require.NoError(t, err)

	// 传入分派器、client=nil（内存装配，不触发包级状态读取器安装）。
	svc := NewQuotaKeyService(newFakeQuotaKeyRepo(), testEncryptor{}, dc, nil)
	require.NotNil(t, svc)

	// 穿透断言：内嵌 shareAI 客户端的 deviceResolver 必须被回填（本 *QuotaKeyService）。
	require.NotNil(t, sac.deviceResolver, "dispatcher must drill into inner shareAI client and backfill deviceResolver")
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

// mustCreateQuotaKeyWithUpstream 创建指定上游标识的激活码（默认 mustCreateQuotaKey 不设置，
// 会落到 NOT NULL DEFAULT 'shareai'，由此可特判 kn-cy 分派路径）。
func mustCreateQuotaKeyWithUpstream(t *testing.T, client *ent.Client, status string, upstreamKey string) *ent.QuotaKey {
	t.Helper()
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:test").
		SetStatus(status).
		SetUpstreamKey(upstreamKey).
		Save(context.Background())
	require.NoError(t, err)
	return qk
}

// knCySupplyFake 是 QuotaKeyUpstreamClient 的测试替身：对 kn_cy 上游返回其实供的
// 3 个模型与目标 1，对 shareai 回落静态 16 模型与目标 5。PullBatch 复用嵌入的
// fakeUpstreamClient（按 batch 返回账号）。用于断言 KC-26 落库口径按上游分派。
type knCySupplyFake struct {
	fakeUpstreamClient
}

// knCySupplyModels 是 kn-cy 实供、且应做恒等映射的三个模型（与 knCyModelMappingModels 同集）。
var knCySupplyModels = []string{"gpt-6.1-sol", "claude-fable-5-1", "gemini-4-argon"}

func (f *knCySupplyFake) RealSupplyModelsFor(k string) ([]string, error) {
	if k == upstreamKeyKnCy {
		out := make([]string, len(knCySupplyModels))
		copy(out, knCySupplyModels)
		return out, nil
	}
	return CursorStaticEntryModels(), nil
}

func (f *knCySupplyFake) TargetAccountsPerKeyFor(k string) (int, error) {
	if k == upstreamKeyKnCy {
		return 1, nil
	}
	return CursorAutoPullAccountsPerKey, nil
}

// quotaKeyTestCredcryptKeyB64 是 qkNewTestService 公共 setup 一次性安装的固定测试主密钥
// （R6-2）。凭据加密写路径（credcryptEncryptor）在未配置主密钥时退化为明文 passthrough，
// 导致既有拉号成功用例因 "credential encryption not enabled" 失败。此处统一启用加密链，
// 由 t.Cleanup 在测试结束后复原为未配置态（passthrough），与既有
// TestCURF1_CredentialEncryptionOnPull 的复位约定一致。生产门槛（config.Totp.EncryptionKeyConfigured）
// 不在本处放宽。
const quotaKeyTestCredcryptKeyB64 = "WlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlI=" // 32×0x5a

func qkNewTestService(t *testing.T, upstream QuotaKeyUpstreamClient) (*QuotaKeyService, *ent.Client) {
	t.Helper()
	client := quotaKeyTestEntClient(t)
	// R6-2：公共 setup 安装固定测试主密钥，激活加密写路径；测试结束复原未配置态。
	_, err := credcrypt.Configure(quotaKeyTestCredcryptKeyB64, "")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = credcrypt.Configure("", "") })
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
	// UpstreamKey 空串不写，取列默认值（'shareai'）；非空显式写入（与生产仓储同口径）。
	if in.UpstreamKey != "" {
		q = q.SetUpstreamKey(in.UpstreamKey)
	}
	return q.Save(ctx)
}

// UpdatePriority 与 production repository 同语义：同一事务内改写激活码优先级，
// 并把该码名下未软删账号的 accounts.priority 同步为该值。
//
// 补位说明：QuotaKeyRepository 新增该方法后替身未同步，会让 unit tag 下整包编译
// 失败（存量断裂，2026-11-03 修）。
func (r *testDBRepo) UpdatePriority(ctx context.Context, tx *ent.Tx, id int64, priority int) (int, error) {
	if _, err := tx.QuotaKey.UpdateOneID(id).SetPriority(priority).Save(ctx); err != nil {
		return 0, err
	}
	return tx.Account.Update().
		Where(account.QuotaKeyID(id), account.DeletedAtIsNil()).
		SetPriority(priority).
		Save(ctx)
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

// GetByIDInTx 真实事务内读取（观测/换码在每 key 锁内复核来源身份用）。
func (r *testDBRepo) GetByIDInTx(ctx context.Context, tx *ent.Tx, id int64) (*ent.QuotaKey, error) {
	if tx != nil {
		return tx.QuotaKey.Get(ctx, id)
	}
	return r.client.QuotaKey.Get(ctx, id)
}

// UpdateUsageSnapshot 真实事务内写 usage_snapshot（+ 可选 expires_at）。
func (r *testDBRepo) UpdateUsageSnapshot(ctx context.Context, tx *ent.Tx, id int64, snapshot []byte, expiresAt **time.Time) error {
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
	_, err := q.Save(ctx)
	return err
}

// UpdateCredentialsAtomically 真实事务内写凭据并（异 MAC 时）同事务清空快照。
func (r *testDBRepo) UpdateCredentialsAtomically(ctx context.Context, tx *ent.Tx, id int64, codeEncrypted, codeMAC, codeLast4 string, clearUsageSnapshot bool) error {
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
	// 镜像 production repository 口径：uq_quota_keys_code_mac 唯一冲突翻译为
	// ErrQuotaKeyDuplicateCode（其余错误原样返回），使服务层经由 testDBRepo 也能验证
	// 「撞码 → ErrQuotaKeyDuplicateCode」契约。
	if isQuotaKeyCodeMacUniqueViolationTest(err) {
		return ErrQuotaKeyDuplicateCode
	}
	return err
}

// isQuotaKeyCodeMacUniqueViolationTest 镜像 repository.isQuotaKeyCodeMacUniqueViolation：
// 仅认 uq_quota_keys_code_mac 唯一约束冲突（23505 + 约束名），主键 / CHECK 冲突不误译。
// 服务包不反向导入 repository，故在此同口径复制。
func isQuotaKeyCodeMacUniqueViolationTest(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr != nil {
		return pqErr.Code == "23505" && pqErr.Constraint == "uq_quota_keys_code_mac"
	}
	return false
}

func (r *testDBRepo) ListExpired(ctx context.Context, now time.Time) ([]*ent.QuotaKey, error) {
	return r.client.QuotaKey.Query().
		Where(quotakey.ExpiresAtNotNil(), quotakey.ExpiresAtLT(now)).All(ctx)
}

// SetDeviceIDIfAbsent 内存替身：模拟 DB 条件化写入（WHERE device_id IS NULL OR =""），
// 与 production repository / service 读判定三者语义一致。
func (f *fakeQuotaKeyRepo) SetDeviceIDIfAbsent(_ context.Context, id int64, deviceID string) (int, error) {
	qk, ok := f.byID[id]
	if !ok {
		return 0, errQuotaKeyNotFound
	}
	if qk.DeviceID != nil && *qk.DeviceID != "" {
		return 0, nil
	}
	qk.DeviceID = &deviceID
	return 1, nil
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

// SetDeviceIDIfAbsent 真实 DB 替身：与 production repository 同语义的条件化写入。
func (r *testDBRepo) SetDeviceIDIfAbsent(ctx context.Context, id int64, deviceID string) (int, error) {
	return r.client.QuotaKey.Update().
		Where(quotakey.ID(id), quotakey.Or(quotakey.DeviceIDIsNil(), quotakey.DeviceIDEQ(""))).
		SetDeviceID(deviceID).
		Save(ctx)
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

// SoftDeleteCursorAccountsByExpiredQuotaKey 真实 DB 替身：与 production repository
// 同机制——tx 必填；单条 `UPDATE ... RETURNING id`，到期判定下沉为 EXISTS 子查询
// （status='expired' AND expires_at IS NOT NULL AND expires_at < now），与 quota_key_id /
// platform='cursor' / deleted_at IS NULL 过滤在同一条语句内由数据库原子裁决；
// RETURNING 直接返回本次实际受影响行，无回读、无候选 SELECT、无方言分支。
// tx 为 nil 时失败关闭。
func (r *testDBRepo) SoftDeleteCursorAccountsByExpiredQuotaKey(ctx context.Context, tx *ent.Tx, quotaKeyID int64, now time.Time) ([]int64, error) {
	if tx == nil {
		return nil, errors.New("testDBRepo: SoftDeleteCursorAccountsByExpiredQuotaKey requires the caller's transaction")
	}
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

// UpdateMeta 与生产仓储同签名：tx 必填，nil 时失败关闭——不给"锁外也能写"的旁路，
// 否则测试替身会掩盖 §25.17.6 的同 key 事务边界。
func (r *testDBRepo) UpdateMeta(ctx context.Context, tx *ent.Tx, id int64, expiresAt **time.Time, notes **string) error {
	if tx == nil {
		return errors.New("testDBRepo: UpdateMeta requires the caller's transaction")
	}
	q := tx.QuotaKey.UpdateOneID(id)
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

// ListAccountsByQuotaKeys 是 QuotaKeyRepository 新增方法的真实 DB 替身：一次
// QuotaKeyIDIn 查询返回归属给定激活码的账号（软删由 SoftDeleteMixin 过滤），
// 与 production repository 同语义，供 List 端点批量聚合计数。
func (r *testDBRepo) ListAccountsByQuotaKeys(ctx context.Context, ids []int64) ([]*ent.Account, error) {
	if len(ids) == 0 {
		return []*ent.Account{}, nil
	}
	return r.client.Account.Query().Where(account.QuotaKeyIDIn(ids...)).All(ctx)
}

// TestQuotaKeyPull_Error_KeepsExhaustedFalseWritesMarker：任意（未知/普通）上游错误 →
// 只写 last_refresh_error + last_refresh_at，exhausted 保持 false
// （CUR-P1-FIX-KEY-EXHAUSTED，方案 §15.2/§16.1：未知失败不得推导停供），且不得插入任何账号。
func TestQuotaKeyPull_Error_KeepsExhaustedFalseWritesMarker(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{err: errors.New("upstream boom")})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 自动链路（manual=false）遇未知上游错误：不再裸 return nil，而是透传含
	// ErrUpstreamKeyBlocked 哨兵的包裹错误（R13-2，使 reconciler 如实计入失败）。
	err := svc.PullAccounts(ctx, qk.ID)
	require.Error(t, err, "automatic chain must surface the blocked sentinel (R13-2)")
	require.True(t, errors.Is(err, ErrUpstreamKeyBlocked),
		"automatic unknown upstream error must wrap ErrUpstreamKeyBlocked; got=%v", err)

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	// 核心：未知/普通上游失败不是已确认 key 级终态 ⇒ exhausted 必须保持 false（不得标"已耗尽"）。
	require.False(t, got.Exhausted, "unknown upstream failure must NOT set exhausted=true")
	require.NotNil(t, got.LastRefreshError, "failure marker must still be written (observability)")
	require.Contains(t, *got.LastRefreshError, "upstream boom")
	require.NotNil(t, got.LastRefreshAt, "failure marker must stamp last_refresh_at")

	// 失败不改变停供事实：该 key 仍在候选集内（后续周期可继续尝试）。
	w := NewQuotaKeyModelWater(client, nil)
	cands, err := w.ListCandidateKeys(ctx, "cursor-fix-key-exhausted", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qk.ID, "unknown failure must NOT remove the key from candidates")

	// 不得插入任何账号。
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "error path must not insert fake accounts")
}

// TestQuotaKeyWriteUpstreamFailureMarker_OnlyWhenActive：失败标记（last_refresh_error）
// 的 CAS 写仅在 status='active' 时生效，不覆盖 sweep 已置的 expired/disabled；
// 且两个分支都不得改写 exhausted（未知失败不标耗尽）。
func TestQuotaKeyWriteUpstreamFailureMarker_OnlyWhenActive(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	active := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, (&QuotaKeyService{}).writeUpstreamFailureMarker(ctx, tx, active.ID, errors.New("boom")))
	require.NoError(t, tx.Commit())
	got, err := client.QuotaKey.Get(ctx, active.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastRefreshError, "marker must be written when status=active")
	require.Contains(t, *got.LastRefreshError, "boom")
	require.False(t, got.Exhausted, "unknown failure marker must NOT set exhausted")

	// 模拟 sweep 已将状态改为 expired：CAS 不命中 ⇒ 标记不写入。
	expired := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusExpired)
	tx2, err := client.Tx(ctx)
	require.NoError(t, err)
	require.NoError(t, (&QuotaKeyService{}).writeUpstreamFailureMarker(ctx, tx2, expired.ID, errors.New("boom")))
	require.NoError(t, tx2.Commit())
	got2, err := client.QuotaKey.Get(ctx, expired.ID)
	require.NoError(t, err)
	require.Nil(t, got2.LastRefreshError, "CAS must NOT override sweep-set status")
	require.False(t, got2.Exhausted, "sweep-set status must not be reclassified as exhausted")
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

// TestQuotaKeyPull_TruncatesBatchToRemainingCapacity：1 + 上游返 2 ⇒ 只落 1 条，
// 总数恰好 CursorAutoPullAccountsPerKey(2)（自动补位目标在拉号批次溢出路径不可突破）。
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
	mustCreateAccounts(t, client, qk.ID, CursorAutoPullAccountsPerKey-1) // 1

	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, CursorAutoPullAccountsPerKey, n, "1 working + 2 upstream must be truncated to exactly CursorAutoPullAccountsPerKey")

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

// fakeReconcilerNotifier 是 quotaKeyReconcilerNotifier 的测试替身，记录
// NotifyAccountRemoved 的调用次数，供断言软删提交后水位快路径唤醒被精确触发。
type fakeReconcilerNotifier struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeReconcilerNotifier) NotifyAccountRemoved() {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
}

func (f *fakeReconcilerNotifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestQuotaKeyDeleteWithAccounts_NotifiesReconcilerAfterCommit 验证 a/b/c 软删唯一真实入口
// DeleteWithAccounts 在事务提交成功之后，恰好唤醒一次 reconciler 水位快路径（C12-v2）。
func TestQuotaKeyDeleteWithAccounts_NotifiesReconcilerAfterCommit(t *testing.T) {
	svc, client := qkNewTestService(t, &fakeUpstreamClient{err: errors.New("unused")})
	fake := &fakeReconcilerNotifier{}
	svc.SetReconciler(fake)

	ctx := context.Background()
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, 3)

	require.NoError(t, svc.DeleteWithAccounts(ctx, qk.ID))

	// 提交成功后应恰好唤醒一次（不直接拉号、只置 pending）。
	if got := fake.callCount(); got != 1 {
		t.Fatalf("expected reconciler.NotifyAccountRemoved called exactly once after commit, got %d", got)
	}
}

// TestQuotaKeyDeleteWithAccounts_NoNotifyOnPreCommitFailure 验证软删在事务提交前失败
// （激活码不存在）时，不触发水位快路径唤醒——唤醒点必须在事务提交之后。
func TestQuotaKeyDeleteWithAccounts_NoNotifyOnPreCommitFailure(t *testing.T) {
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{err: errors.New("unused")})
	fake := &fakeReconcilerNotifier{}
	svc.SetReconciler(fake)

	// 使用不存在的 quotaKeyID → 提交前即返回 ErrQuotaKeyNotFound。
	err := svc.DeleteWithAccounts(context.Background(), 999999)
	require.Error(t, err)

	if got := fake.callCount(); got != 0 {
		t.Fatalf("expected no reconciler notify when delete fails before commit, got %d", got)
	}
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
	// 自动链路（manual=false）遇占位客户端错误：R13-2 后透传含 ErrUpstreamKeyBlocked 哨兵的
	// 包裹错误（不再裸 return nil）；DB 侧只落失败标记，exhausted 保持 false
	// （未知/普通失败不标耗尽，CUR-P1-FIX-KEY-EXHAUSTED）。
	err := svc.PullAccounts(ctx, qk.ID)
	require.Error(t, err, "automatic chain must surface the blocked sentinel (R13-2)")
	require.True(t, errors.Is(err, ErrUpstreamKeyBlocked),
		"placeholder upstream error must wrap ErrUpstreamKeyBlocked; got=%v", err)

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, got.Exhausted, "placeholder upstream failure must NOT set exhausted=true")
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

	dto, err := svc.Create(context.Background(), plain, nil, nil, 0, "")
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
	dto, err := svc.Update(context.Background(), 1, &newCode, nil, nil, nil)
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

	_, err := svc.Update(context.Background(), 1, nil, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 0, repo.updateMetaCalls, "omitted metadata must not call UpdateMeta")
	require.Equal(t, initialExpiresAt, *repo.byID[1].ExpiresAt)
	require.Equal(t, initialNotes, *repo.byID[1].Notes)

	var clearExpiresAt *time.Time
	var clearNotes *string
	_, err = svc.Update(context.Background(), 1, nil, &clearExpiresAt, &clearNotes, nil)
	require.NoError(t, err)
	require.Equal(t, 1, repo.updateMetaCalls)
	require.Nil(t, repo.byID[1].ExpiresAt, "explicit null must clear expires_at")
	require.Nil(t, repo.byID[1].Notes, "explicit null must clear notes")

	expiresAt := time.Now().Add(24 * time.Hour)
	notes := "rotation note"
	expiresAtValue := &expiresAt
	notesValue := &notes
	dto, err := svc.Update(context.Background(), 1, nil, &expiresAtValue, &notesValue, nil)
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

// ---------------------------------------------------------------------------
// D-CURSOR-033 #3：短激活码脱敏不泄露（终审）。
// 不变量：原文长度 ≤ 4 的短码，code_masked / 任何 DTO 字段都不得包含原文任何字符
// （L11/L16）；正常长度仍保持 XXXX-****-****-<last4> 既有契约。
// ---------------------------------------------------------------------------

// TestQuotaKeyService_ShortCodeNeverLeaksPlaintext：经完整 Create 路径，短码
// 的 code_masked 不得包含原文任何字符（不回显、不部分泄露）。
func TestQuotaKeyService_ShortCodeNeverLeaksPlaintext(t *testing.T) {
	shortCodes := []string{"a", "ab", "abc", "1234", "XY", "q"}
	repo := newFakeQuotaKeyRepo()
	enc := testEncryptor{}
	svc := NewQuotaKeyService(repo, enc, nil, nil)

	for _, code := range shortCodes {
		dto, err := svc.Create(context.Background(), code, nil, nil, 0, "")
		require.NoError(t, err, "Create must accept short code %q without error", code)
		require.NotNil(t, dto)
		// 不得包含原文任何字符（逐字符校验，杜绝部分泄露）。
		for _, r := range code {
			require.NotContains(t, dto.CodeValueMasked, string(r),
				"code_masked %q must not contain any char of short code %q", dto.CodeValueMasked, code)
		}
		// 落库的 CodeLast4 也不得是原文（短码应存为空，断绝回显源）。
		require.NotEqual(t, code, repo.byID[dto.ID].CodeLast4,
			"CodeLast4 must not equal the short code %q", code)
	}
}

// TestQuotaKeyMaskCodeValue_ShortCodeFallback：直接锁定脱敏兜底。
//   - 正常 last4（4 位）→ 既有契约 XXXX-****-****-1234
//   - 空 → ****
//   - 不足 4 位（非空）→ 固定全掩码，绝不回显原文片段
func TestQuotaKeyMaskCodeValue_ShortCodeFallback(t *testing.T) {
	require.Equal(t, "XXXX-****-****-1234", maskCodeValue("1234"), "normal last4 keeps contract")
	require.Equal(t, "****", maskCodeValue(""), "empty last4 -> ****")
	require.Equal(t, "XXXX-****-****-****", maskCodeValue("ab"), "short last4 -> full mask, no echo")
	require.Equal(t, "XXXX-****-****-****", maskCodeValue("1"), "single-char last4 -> full mask")
	require.Equal(t, "XXXX-****-****-****", maskCodeValue("XYZ"), "3-char last4 -> full mask")
}

// TestLast4Of_ShortCodeReturnsEmpty：锁定 last4Of 对 ≤4 位原文返回空，
// 避免短码全文落入 CodeLast4 进而被掩码回显（D-CURSOR-033 #3）。
func TestLast4Of_ShortCodeReturnsEmpty(t *testing.T) {
	cases := map[string]string{
		"a":                                "",
		"ab":                               "",
		"abc":                              "",
		"1234":                             "",
		"12345":                            "2345",
		"CURSOR-ACTIVATION-CODE-9F3A-1234": "1234",
	}
	for in, want := range cases {
		require.Equal(t, want, last4Of(in), "last4Of(%q) mismatch", in)
	}
}

// ---------------------------------------------------------------------------
// C11：§13.3 挂载 / §13.2 冷却 / 非幂等失败边界（unit 标签，连本地 quota_test 库）。
// 仅改本文件（quota_key_service.go 直接配对的窄接口测试）。
// ---------------------------------------------------------------------------

// mustCreateGroup 创建带模型白名单的分组（仅供 C11 挂载测试）。name 含时间戳后缀以在
// 持久化测试库（quota_test）中保持全局唯一（group.name 为部分唯一索引）。
func mustCreateGroup(t *testing.T, client *ent.Client, allowlist domain.GroupModelAllowlist) *ent.Group {
	t.Helper()
	g, err := client.Group.Create().
		SetName(fmt.Sprintf("c11-grp-%d", time.Now().UnixNano())).
		SetModelAllowlist(allowlist).
		Save(context.Background())
	require.NoError(t, err)
	return g
}

// mustCreateCursorGroup 创建 cursor 平台分组并挂模型白名单，仅用于构造【特定覆盖形态】的用例
// （水位/挂载对分组平台无偏好，裁定 1 已不限定 platform=cursor；取 cursor 平台仅为直观构造
// 覆盖某模型的账号分组，不影响口径判定）。
func mustCreateCursorGroup(t *testing.T, client *ent.Client, allowlist domain.GroupModelAllowlist) *ent.Group {
	t.Helper()
	g, err := client.Group.Create().
		SetName(fmt.Sprintf("c11-grp-cursor-%d", time.Now().UnixNano())).
		SetPlatform(domain.PlatformCursor).
		SetModelAllowlist(allowlist).
		Save(context.Background())
	require.NoError(t, err)
	return g
}

// mustCreateCompositeGroup 创建 composite 聚合组并挂模型白名单（P1-B2 U7 挂载目标形态：
// 生产库无 cursor 平台分组，cursor 账号由 composite 聚合组调度）。
func mustCreateCompositeGroup(t *testing.T, client *ent.Client, allowlist domain.GroupModelAllowlist) *ent.Group {
	t.Helper()
	g, err := client.Group.Create().
		SetName(fmt.Sprintf("c11-grp-composite-%d", time.Now().UnixNano())).
		SetPlatform(domain.PlatformComposite).
		SetModelAllowlist(allowlist).
		Save(context.Background())
	require.NoError(t, err)
	return g
}

// accountGroupLinked 报告指定账号是否已挂入指定分组（account_groups M2M）。
func accountGroupLinked(t *testing.T, ctx context.Context, client *ent.Client, accountID, groupID int64) bool {
	t.Helper()
	ok, err := client.AccountGroup.Query().
		Where(accountgroup.AccountID(accountID), accountgroup.GroupID(groupID)).
		Exist(ctx)
	require.NoError(t, err)
	return ok
}

// TestQuotaKeyService_PullForModel_MountsToCoveringGroups：自动补池（PullAccountsForModel）
// 与手动挂组同源同判（§20.2-A）：新号在落库事务提交前挂入 U7 目标分组（模型真实厂商
// 原生平台组 ∪ cursor 专属 ∪ composite，白名单启用且覆盖静态实供模型）；allow-all
// （enabled=false，无对应模型）与非目标平台组不挂。
func TestQuotaKeyService_PullForModel_MountsToCoveringGroups(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// U7（§20.2-A）：挂载按账号模型能力（静态实供清单）计算，modelM 不再收窄挂载。
	// grp：cursor 平台组、白名单启用且覆盖实供模型 claude-opus-5-5 → 挂。
	modelM := "claude-opus-5-5"
	grp := mustCreateCursorGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{modelM},
	})
	// 不覆盖任何实供模型的分组，不应被挂入。
	other := mustCreateGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"some-other-model"},
	})
	// R10-1（CUR-F13）：复原本用例自建分组，避免残留共享库污染他用例。
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range []int64{grp.ID, other.ID} {
			_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(gid)).Exec(cctx)
			_, _ = client.Group.Delete().Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
	})

	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "m1", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	require.NoError(t, svc.PullAccountsForModel(ctx, qk.ID, modelM))

	// 新号落库。
	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1, "pulled account must be persisted")
	accID := accs[0].ID

	// 挂入覆盖组，不挂入非覆盖组（按成员断言，兼容持久化库的重复分组累积）。
	require.True(t, accountGroupLinked(t, ctx, client, accID, grp.ID), "account must be linked to covering group")
	require.False(t, accountGroupLinked(t, ctx, client, accID, other.ID), "account must NOT be linked to non-covering group")
}

// TestQuotaKeyService_Pull_NewCodeDeviceIDFirstWriteNoDeadlock：C-U7-A 死锁回归。
//
// 复现 f6 实测 20s 挂死场景：新码首次自动补池（PullAccountsForModel），旧码在 PullBatch→
// resolveDeviceID 分支③经全局连接 SetDeviceIDIfAbsent 更新 quota_keys 行——若此事发生在
// 已 FOR UPDATE 持有行锁的容量事务内，会等待本事务自身持有的锁 → 应用层自死锁。
//
// 修复（O01）：pull 把新码 device_id 首写前置到容量事务之外（无行锁、独立提交，经 DeviceIDFor
// 走全局连接 SetDeviceIDIfAbsent），事务内只读到已提交的 device_id、分支①命中、不再触发全局连接写。
//
// 本测试走真实 shareAI 上游（newTestClient 构造的 *shareAIUpstreamClient 由 NewQuotaKeyService
// 自动回填 deviceResolver=svc），故确实经过分支③的全局连接写路径——修复前会在 25s 内部超时判定死锁，
// 修复后在秒级完成并落库账号 + device_id（内部超时独立于 -timeout，不依赖 harness 兜底）。
func TestQuotaKeyService_Pull_NewCodeDeviceIDFirstWriteNoDeadlock(t *testing.T) {
	md, mail, ts, _, _, _, _, _ := loadUpstreamVector(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// refresh 直接返回 code=0 + 真实 metadata → 成功拉号（无需 activate）。
		fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"metadata":%q,"mail":%q,"timestamp":%q}}`, md, mail, ts)
	})
	up := newTestClient(t, handler)
	svc, client := qkNewTestService(t, up)
	ctx := context.Background()

	// U7 挂载收敛：建一个覆盖实供模型的 cursor 平台组，使补池成功落号并挂组（避免挂载失败回滚）。
	modelM := "claude-opus-5-5"
	grp := mustCreateCursorGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{modelM},
	})
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(grp.ID)).Exec(cctx)
		_, _ = client.Group.Delete().Where(entgroup.IDEQ(grp.ID)).Exec(cctx)
	})

	// 新码：active、device_id 为空、无账号。
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	// 内部超时：旧码（事务内改走全局连接写 device_id）会自死锁挂死，此处 25s 即判定失败，
	// 不依赖 go test -timeout=60s 兜底（满足 Done when 2 "不依赖 -timeout 兜底"）。
	done := make(chan error, 1)
	go func() {
		done <- svc.PullAccountsForModel(ctx, qk.ID, modelM)
	}()
	select {
	case err := <-done:
		require.NoError(t, err, "new-code first PullAccountsForModel must complete without deadlock")
	case <-time.After(25 * time.Second):
		t.Fatal("deadlock: new-code first PullAccountsForModel blocked >25s (in-tx device_id first-write fix missing)")
	}

	// 落库账号 + device_id 双验证。
	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1, "pulled account must be persisted")

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.DeviceID, "device_id must be persisted by in-tx first write (deadlock fix)")
	require.NotEqual(t, "", *got.DeviceID, "device_id must be non-empty")
}

// TestQuotaKeyPullForModel_CooldownReturnsSentinelAndWritesMarker：上游冷却返回
// ErrUpstreamCooldown（errors.Is 可识别），同事务写 last_refresh_error/last_refresh_at，
// 唯独豁免 exhausted，且冷却不落号（§13.2 F-b / R4）。
func TestQuotaKeyService_PullForModel_CooldownReturnsSentinelAndWritesMarker(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{err: ErrUpstreamCooldown})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.PullAccountsForModel(ctx, qk.ID, "cursor-model-x")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrUpstreamCooldown), "cooldown must be recognizable via errors.Is")

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, got.Exhausted, "cooldown must NOT set exhausted")
	require.NotNil(t, got.LastRefreshError, "cooldown must write last_refresh_error")
	require.Contains(t, *got.LastRefreshError, "cooldown")
	require.NotNil(t, got.LastRefreshAt, "cooldown must write last_refresh_at")

	// 冷却不落号。
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "cooldown must not insert accounts")
}

// TestQuotaKeyService_CooldownClearsStaleExhausted：B2 缺陷修复——存量 exhausted=true 的
// key（v3.5 部署前旧代码在冷却响应里把 exhausted 写为 true）遇冷却响应时，冷却分支同事务
// 顺带 SetExhausted(false)，把冷却态与耗尽态解耦（存量纠正）；同时 last_refresh_error 标记写入。
func TestQuotaKeyService_CooldownClearsStaleExhausted(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{err: ErrUpstreamCooldown})

	// 前置存量态：exhausted=true 且 last_refresh_error 已置位（旧冷却文案）。
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	_, err := client.QuotaKey.UpdateOneID(qk.ID).
		SetExhausted(true).
		SetLastRefreshError("cooldown: rate limited (stale)").
		Save(ctx)
	require.NoError(t, err)

	// 冷却响应（不携带模型上下文亦可，冷却分支与挂载无关）。
	err = svc.PullAccountsForModel(ctx, qk.ID, "cursor-model-x")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrUpstreamCooldown), "cooldown must be recognizable via errors.Is")

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	// B2 核心：冷却分支纠正存量 exhausted=true → false（冷却≠耗尽）。
	require.False(t, got.Exhausted, "cooldown must clear stale exhausted=true (cooldown != exhaustion)")
	// 标记写入。
	require.NotNil(t, got.LastRefreshError, "cooldown must write last_refresh_error marker")
	require.Contains(t, *got.LastRefreshError, "cooldown")
	require.NotNil(t, got.LastRefreshAt, "cooldown must write last_refresh_at")
}

// TestQuotaKeyService_PullForModel_ModelNoCoverageFailClosed：零目标组 → 失败关闭，
// 不静默落成裸号（回滚），并以 ErrQuotaKeyModelNoCoverage 可识别（§20.2-A U7）。
//
// FIX-3（增量复审 R2 F4'）：本分支位于上游发号成功之后（白名单与发号非同快照，期间可
// 变更），上游号已消耗且不可回滚，故必须同时挂 ErrQuotaKeyMountFailed 哨兵——触发方按
// 非幂等边界"告警 + 停轮"处理，不得当作普通错误继续拉下一个候选。
func TestQuotaKeyService_PullForModel_ModelNoCoverageFailClosed(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// U7（§20.2-A）：挂载按账号模型能力（静态实供清单）判定。本用例唯一自建分组不含任何
	// 实供模型 → 零覆盖（共享库若被他用例遗留覆盖实供模型的启用组，会使本断言失败并暴露污染）。
	grpOtherModel := mustCreateGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"other-model"},
	})
	// R10-1（CUR-F13）：复原本用例自建分组，避免残留共享库污染他用例。
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(grpOtherModel.ID)).Exec(cctx)
		_, _ = client.Group.Delete().Where(entgroup.IDEQ(grpOtherModel.ID)).Exec(cctx)
	})
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "m2", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.PullAccountsForModel(ctx, qk.ID, "uncovered-model")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrQuotaKeyModelNoCoverage), "model with no covering group must fail-closed")
	// FIX-3：零覆盖发生在上游发号成功之后 → 同时必须命中挂载哨兵（errors.Is 双链）。
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"zero coverage after upstream success must also be recognizable via ErrQuotaKeyMountFailed")

	// 失败关闭：号不入库（回滚）。
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "fail-closed must not persist a bare account")
}

// fix3CountingUpstream 记录上游真实被拉号次数（FIX-3 用）：证明"零覆盖"判定的时点在
// 上游发号成功之后，而非发号前的预先校验。
type fix3CountingUpstream struct {
	batch *UpstreamPullResult
	calls atomic.Int64
}

func (f *fix3CountingUpstream) PullBatch(_ context.Context, _ *ent.QuotaKey) (*UpstreamPullResult, error) {
	f.calls.Add(1)
	return f.batch, nil
}

// RealSupplyModelsFor / TargetAccountsPerKeyFor：测试替身默认按 shareai 口径。
func (f *fix3CountingUpstream) RealSupplyModelsFor(_ string) ([]string, error) {
	return CursorStaticEntryModels(), nil
}

func (f *fix3CountingUpstream) TargetAccountsPerKeyFor(_ string) (int, error) {
	return CursorAutoPullAccountsPerKey, nil
}

// TestQuotaKeyService_PullForModel_ZeroCoverageAfterUpstreamPullCarriesMountFailedSentinel
// 验证 R2 F4' 精确场景：白名单与上游发号非同快照——上游成功发号后，解析"白名单含 M"的
// 分组返回空集（覆盖在期间消失）→ 错误同时可被 ErrQuotaKeyMountFailed 与
// ErrQuotaKeyModelNoCoverage 识别（errors.Is 皆为真），且事务回滚、号不入库。
func TestQuotaKeyService_PullForModel_ZeroCoverageAfterUpstreamPullCarriesMountFailedSentinel(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	up := &fix3CountingUpstream{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "fix3-zero-cov", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	}
	svc, _ := qkNewTestService(t, up)
	// 注入"覆盖在发号后消失"：解析成功但返回空集（非解析错误）。
	svc.cursorGroupResolver = func(_ context.Context, _ *ent.Tx) ([]int64, error) {
		return []int64{}, nil
	}
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.PullAccountsForModel(ctx, qk.ID, "cursor-model-zero-cov")
	require.Error(t, err)

	// 时点证据：上游已被真实调用（发号已发生）→ 零覆盖判定发生在发号之后。
	require.Equal(t, int64(1), up.calls.Load(), "upstream must have been called once before coverage resolve")

	// 双哨兵可达：errors.Is 对两者皆为真（%w 双链）。
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"zero coverage after upstream pull must be recognizable via ErrQuotaKeyMountFailed")
	require.True(t, errors.Is(err, ErrQuotaKeyModelNoCoverage),
		"zero coverage must remain recognizable via ErrQuotaKeyModelNoCoverage")

	// 失败关闭 + 回滚：号不入库。
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "zero coverage after upstream pull must roll back (no account persisted)")
}

// TestQuotaKeyPullForModel_MountFailureRollback：上游成功、本地挂载失败
// （shareai 非幂等，上游发号不可回滚）→ 本地事务回滚（号不入库），并以
// ErrQuotaKeyMountFailed 可识别返回（§13.3 F-c）。
func TestQuotaKeyService_PullForModel_MountFailureRollback(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "m3", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	})
	// 注入挂载失败：解析返回不存在的分组 ID → AccountGroup.Save 触发 FK 违规 → 回滚。
	svc.cursorGroupResolver = func(_ context.Context, _ *ent.Tx) ([]int64, error) {
		return []int64{1 << 40}, nil // 不存在的 group_id，制造 FK 失败
	}
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.PullAccountsForModel(ctx, qk.ID, "cursor-model-y")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed), "upstream-success/local-mount-failure must be recognizable")

	// 非幂等边界：本地事务回滚，上游发号不入库。
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "mount failure must roll back the pulled account (not persisted)")
}

// TestQuotaKeyPull_NoModelContextSkipsMount：2 参 PullAccounts（reconciler/手动）不携带
// 模型上下文，成功拉号但不挂载、不失败关闭（保持 §13.2 既有语义，挂载接线由触发方传 M）。
func TestQuotaKeyService_Pull_NoModelContextSkipsMount(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// 存在覆盖某模型的分组；但若走 2 参 PullAccounts（无 M），不应触发挂载。
	grp := mustCreateGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"some-model"},
	})
	// R10-1（CUR-F13）：复原本用例自建分组，避免残留共享库污染他用例。
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(grp.ID)).Exec(cctx)
		_, _ = client.Group.Delete().Where(entgroup.IDEQ(grp.ID)).Exec(cctx)
	})
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "m4", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	// 2 参入口：无 M → 不挂载，但账号正常落库、清 exhausted。
	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1, "2-arg pull must still persist accounts")
	require.False(t, accountGroupLinked(t, ctx, client, accs[0].ID, grp.ID),
		"2-arg pull without model M must NOT mount to groups")
}

// TestQuotaKeyService_RefreshAccounts_MountsToCursorGroups：B1 缺陷修复——手动入口
// RefreshAccounts 改走挂载语义，拉到的 cursor 账号按 P1-B2 U7「按模型平台挂组」挂入
// 【静态实供清单】∩【模型所属平台（cursor）的目标分组】∩【分组白名单实际覆盖】
// （findCursorPlatformGroups，与 ListAllowedModels 同源同判 cursorMountTargetGroups），
// 而非落成裸号（group_count=0）。
//
// 用例构造（U7 三条件逐条钉死）：
//   - 入选：composite 聚合组 + 白名单启用且覆盖实供模型（生产形态）；
//   - 不入选：composite 聚合组但白名单未启用（allow-all，无对应模型）；
//   - 不入选：白名单覆盖实供模型但分组平台非 cursor/composite（挂了也路由不到 cursor 账号）；
//   - 不入选：白名单启用但不覆盖任何实供模型（显式拒绝型）。
func TestQuotaKeyService_RefreshAccounts_MountsToCursorGroups(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 应被挂入的分组（生产形态复刻）：composite 聚合组，白名单启用且覆盖实供模型。
	grpComposite := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"claude-sonnet-5-5"},
	})
	// 不应挂入：composite 聚合组但白名单未启用（allow-all，无对应模型）。
	grpAllowAll := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: false})
	// 同厂商正例（U7 修正）：分组平台为 anthropic（groups.platform 列默认即 anthropic），
	// 白名单覆盖 claude-sonnet-5-5——其真实厂商平台正是 anthropic，故【应挂】。
	// 修复前误判为"不应挂入"（与 U7 相反）。
	grpAnthropicVendor := mustCreateGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"claude-sonnet-5-5"},
	})
	// 异厂商反例（U7 修正）：分组平台为 grok，但其白名单含 claude-sonnet-5-5
	//（anthropic 厂商）。平台不匹配模型真实厂商 → 非目标分组，【不挂】。
	grpGrokVendor, gerr := client.Group.Create().
		SetName(fmt.Sprintf("c11-grp-grok-%d", time.Now().UnixNano())).
		SetPlatform(domain.PlatformGrok).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{"claude-sonnet-5-5"}}).
		Save(context.Background())
	require.NoError(t, gerr)
	// 不应挂入：白名单启用且明确不含任何实供模型（如 DeepSeek 计量组形态）。
	grpRejectCursor := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"deepseek-v4-1-flash"},
	})

	// 本用例自建的分组（含 allow-all 的 composite 聚合组）必须自行清理，不得遗留为共享库
	// 污染：allow-all 组（grpAllowAll）若被他用例（如 TestCURF1_AllowedModels_... 的全等
	// 断言）读到，会使其允许集合混入非预期模型而断言失败。R9-1 已删除"遍历删全库 allow-all"
	// 的隔离段，故此处显式复原本用例数据。
	t.Cleanup(func() {
		ctx := context.Background()
		for _, gid := range []int64{grpComposite.ID, grpAllowAll.ID, grpAnthropicVendor.ID, grpGrokVendor.ID, grpRejectCursor.ID} {
			_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(gid)).Exec(ctx)
			_, _ = client.Group.Delete().Where(entgroup.IDEQ(gid)).Exec(ctx)
		}
	})

	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "refresh-mount", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	// 手动刷新：不带模型上下文，按 U7 口径挂载。
	require.NoError(t, svc.RefreshAccounts(ctx, qk.ID))

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).WithGroups().All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1, "manual refresh must persist the pulled account")
	accID := accs[0].ID

	// group_count > 0：手动拉到的账号已挂组（修复前恒 0）。
	require.Greater(t, len(accs[0].Edges.Groups), 0, "manual refresh must mount account to groups (group_count > 0)")
	// 挂入 composite 聚合组（白名单启用且覆盖实供模型）。
	require.True(t, accountGroupLinked(t, ctx, client, accID, grpComposite.ID),
		"manual refresh must mount account to the composite group covering a real-supply model")
	// allow-all（白名单未启用）聚合组不挂：无对应模型。
	require.False(t, accountGroupLinked(t, ctx, client, accID, grpAllowAll.ID),
		"manual refresh must NOT mount to an allow-all group (no corresponding model)")
	// 同厂商正例（U7 修正）：anthropic 平台组覆盖 claude-sonnet-5-5（其厂商即 anthropic）→ 应挂。
	require.True(t, accountGroupLinked(t, ctx, client, accID, grpAnthropicVendor.ID),
		"manual refresh must mount account to the same-vendor (anthropic) group covering claude-sonnet-5-5 (U7: mount by model's real vendor platform)")
	// 异厂商反例（U7 修正）：grok 平台组即使白名单含 claude-sonnet-5-5 也不挂（平台不匹配）。
	require.False(t, accountGroupLinked(t, ctx, client, accID, grpGrokVendor.ID),
		"manual refresh must NOT mount to a cross-vendor (grok) group even if its allowlist names claude-sonnet-5-5")
	// 不挂入白名单启用且不覆盖任何实供模型的组。
	require.False(t, accountGroupLinked(t, ctx, client, accID, grpRejectCursor.ID),
		"manual refresh must NOT mount account to whitelist-rejecting-cursor group")
	// 手动刷新成功路径清 exhausted。
	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, got.Exhausted, "manual refresh success path must clear exhausted")
}

// ---------------------------------------------------------------------------
// FIX-1 F1/F4：生产用 QuotaKeyModelWaterProvider 口径用例 + §13.3 解析哨兵用例
//（unit 标签，连本地 quota_test 库）。
//   - F1：WaterLevel 的池级去重总量（P1-D2 总量制，不按模型收窄）/ temp 窗口排除；
//     ListCandidateKeys 的候选 key 状态过滤；阈值恒为 quotaKeyWaterLevelThreshold
//     （=10，语义为 cursor 可服务账号总量，不随模型变化）。
//   - F4：挂载解析器（cursorGroupResolver）出错（上游成功之后）→ errors.Is(err, ErrQuotaKeyMountFailed)。
// ---------------------------------------------------------------------------

// fix1AccSeq 为 F1 口径测试生成全局唯一账号名（quota_test 持久库下跨测试累积，避免唯一冲突）。
var fix1AccSeq atomic.Int64

// fix1UniqueModel 生成全局唯一的测试模型名。quota_test 是持久库，上一轮运行遗留的分组
// 白名单会与新一轮模型名重名，导致水位把遗留账号一并计入（用例不可重复运行）。
// 故每个用例的模型名带时间戳 + 序号，保证水位口径只命中本轮新建的分组。
func fix1UniqueModel(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), fix1AccSeq.Add(1))
}

// mustCreateCursorAccount 创建一条可调度、正常、未软删的 cursor 账号（仅 F1 口径测试用）。
// quotaKeyID 为合法归属激活码 ID（account.quota_key_id 有外键约束，须指向真实 quota_key）。
func mustCreateCursorAccount(t *testing.T, client *ent.Client, quotaKeyID int64, opts ...func(*ent.AccountCreate)) *ent.Account {
	t.Helper()
	c := client.Account.Create().
		SetName(fmt.Sprintf("fix1-acc-%d-%d", time.Now().UnixNano(), fix1AccSeq.Add(1))).
		SetPlatform(domain.PlatformCursor).
		SetType("oauth").
		SetCredentials(map[string]any{"k": "v"}).
		SetQuotaKeyID(quotaKeyID).
		SetSchedulable(true).
		SetStatus(domain.StatusActive)
	for _, o := range opts {
		o(c)
	}
	a, err := c.Save(context.Background())
	require.NoError(t, err)
	return a
}

// mustLinkAccountToGroup 在 account_groups M2M 中把账号挂入分组（仅 F1 口径测试用）。
func mustLinkAccountToGroup(t *testing.T, client *ent.Client, accountID, groupID int64) {
	t.Helper()
	_, err := client.AccountGroup.Create().
		SetAccountID(accountID).
		SetGroupID(groupID).
		Save(context.Background())
	require.NoError(t, err)
}

// TestQuotaKeyModelWater_ListAllowedModelsPlatformScoped 验证 P1-B2 U7：ListAllowedModels
// 按【模型所属平台的目标分组】枚举可补池模型。实供模型 grok-4.7 由 cursor 平台分组覆盖 →
// 入选；gpt-6-sol 仅由非 cursor/composite 平台分组（默认 anthropic）覆盖 → 不入选
// （挂进去也路由不到 cursor 账号，U7 不再让补池把号灌进对模型无声明的裸组）。
//
// 补池左项不再取"人工勾选"（入库退出人工勾选定义能力），恒为静态实供清单。
func TestQuotaKeyModelWater_ListAllowedModelsPlatformScoped(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// 配置勾选两个实供模型（vendor 随实供清单）；U7 下配置勾选不参与补池左项，
	// 保留配置源仅为钉死"人工勾选取值不再是模型集合来源"。
	w := NewQuotaKeyModelWater(client, curF1ConfigSelecting("grok-4.7", "gpt-6-sol"))

	// cursor 平台分组允许 grok-4.7。
	cursorGrp, err := client.Group.Create().
		SetName(fmt.Sprintf("c11-grp-cursor-%d", time.Now().UnixNano())).
		SetPlatform(domain.PlatformCursor).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{"grok-4.7"}}).
		Save(ctx)
	require.NoError(t, err)
	// 非模型所属平台分组（默认 anthropic）允许 gpt-6-sol。
	// 注意：本用例只创建"白名单启用"的分组——白名单未启用（enabled=false）的分组按既有
	// Allows 语义是"放行一切"，会污染 quota_test 持久库里其它用例的挂载/水位断言。
	anthropicGrp, err := client.Group.Create().
		SetName(fmt.Sprintf("c11-grp-anthropic-%d", time.Now().UnixNano())).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-sol"}}).
		Save(ctx)
	require.NoError(t, err)

	// R10-1（CUR-F13）：显式复原本用例自建分组，避免其（覆盖 grok-4.7 / gpt-6-sol 的启用组）
	// 残留共享库、污染他用例（如 F10 的全等挂载断言、F1 的交集断言）。先清挂载链接再删分组。
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range []int64{cursorGrp.ID, anthropicGrp.ID} {
			_, _ = client.AccountGroup.Delete().
				Where(accountgroup.GroupIDEQ(gid)).Exec(cctx)
			_, _ = client.Group.Delete().
				Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
	})

	models, err := w.ListAllowedModels(ctx)
	require.NoError(t, err)
	require.Contains(t, models, "grok-4.7", "model covered by a cursor group allowlist must be enumerated")
	require.NotContains(t, models, "gpt-6-sol",
		"model covered ONLY by a group outside the model's owning platform must NOT be enumerated (P1-B2 U7)")
}

// TestQuotaKeyModelWater_WaterLevelIsPoolTotalDedupedAcrossModels 验证 P1-D2 总量口径：
// WaterLevel 返回【cursor 可服务账号去重总量】，不随 modelM 收窄；同一账号被多个覆盖模型
// 的分组命中只计一次（§6.3），阈值恒为常量 10。
//
// 用例用 mustCreateGroup 构造覆盖分组（任意平台分组均可，裁定 1 不限定 platform=cursor）；
// 断言一律走【基线 + 增量】而非绝对计数：quota_test 是持久库，历史轮次可能遗留可服务账号，
// 绝对计数会随运行次序漂移，增量才表达"本用例新增账号是否按总量去重计入"。
func TestQuotaKeyModelWater_WaterLevelIsPoolTotalDedupedAcrossModels(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// D-CRT-01W-FIX2：前序 FIX 把 WaterLevel 改成依赖齐备走 routed 口径、缺依赖即 fail-closed。
	// 原构造 NewQuotaKeyModelWater(client, nil) 不注入路由依赖会正确撞门；此处改为注入齐备依赖走
	// routed 路径（复用 newRoutedWater + stubResolverAllCursor），断言语义（池内去重总量）不变。
	w := newRoutedWater(t, client, stubResolverAllCursor(), stubChannelServiceNoChannel(), &GatewayService{})

	// 两个覆盖分组各自允许【不同】的 cursor 实供模型（挂组门槛：白名单允许任一 cursor 实供模型）。
	// 注：§18.4 W routed 口径对分组平台有准入过滤（cursor/composite/native-bridge），旧用例用
	// 空平台 mustCreateGroup 在旧代码无平台过滤时也能覆盖，新代码下改用 composite 组。
	require.GreaterOrEqual(t, len(CursorRealSupplyModels), 2, "fixture needs at least two real-supply cursor models")
	modelA := CursorRealSupplyModels[0].ID
	modelB := CursorRealSupplyModels[1].ID
	require.NotEqual(t, modelA, modelB, "fixture needs two distinct real-supply cursor models")

	grpA := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{modelA}})
	// 覆盖第二个实供模型的分组：用于构造"同一账号跨多个 cursor 模型命中"。
	grpB := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{modelB}})
	// 允许一切的分组（白名单未启用 → Allows 放行全部，含 cursor 实供模型）：用于构造去重边界
	// ——同一账号再挂一个覆盖组也不得被计第二次。
	grpAllowAll := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: false})
	// 不覆盖任何 cursor 实供模型的分组（白名单只允许一个无关模型）：其中的账号不计入池。
	grpNonCursor := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{fix1UniqueModel("cursor-fix1-nocursor")},
	})

	holder := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// A04-G 来源门修复（c1p1-I）：countsTowardWaterLevel 现走 cursorSupplySourceAvailabilityVetoReason，
	// provider 未装配即失败关闭、账号不计（0）。本夹具装齐状态+快照两个窄读取器，使 holder 名下
	// 账号通过来源门计入水位；断言增量语义不变。
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{holder.ID: domain.QuotaKeyStatusActive},
		snapshots: map[int64][]byte{holder.ID: []byte(fmt.Sprintf(
			`{"total":400,"used":0,"status_code":200,"checked_at":%q,"error_code":""}`,
			time.Now().UTC().Format(time.RFC3339)))},
	})
	accs := make([]int64, 0, 4)
	// R10-1（CUR-F13）：复原本用例自建分组与账号，避免残留共享库污染他用例
	//（如 F10 全等挂载断言、F1 交集断言）。先清挂载链接，再删分组、软删账号。
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range []int64{grpA.ID, grpB.ID, grpAllowAll.ID, grpNonCursor.ID} {
			_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(gid)).Exec(cctx)
			_, _ = client.Group.Delete().Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
		for _, aid := range accs {
			_ = client.Account.UpdateOneID(aid).SetDeletedAt(time.Now()).Exec(cctx)
		}
	})

	// 基线：持久库可能已有无关系的可服务账号，故以基线 + 增量断言。
	base, thr, err := w.WaterLevel(ctx, modelA)
	require.NoError(t, err)
	require.Equal(t, quotaKeyWaterLevelThreshold, thr, "threshold must be constant 10")
	// 总量口径：问哪个模型结果都相同（modelM 不再收窄计数）。
	baseB, _, err := w.WaterLevel(ctx, modelB)
	require.NoError(t, err)
	require.Equal(t, base, baseB, "pool total must not vary with modelM")

	// ① 同号跨双模型：同时挂在允许不同模型的两个分组上 → 只计 1（§6.3 去重）。
	accDual := mustCreateCursorAccount(t, client, holder.ID, withCursorWaterModels())
	accs = append(accs, accDual.ID)
	mustLinkAccountToGroup(t, client, accDual.ID, grpA.ID)
	mustLinkAccountToGroup(t, client, accDual.ID, grpB.ID)

	curA, _, err := w.WaterLevel(ctx, modelA)
	require.NoError(t, err)
	require.Equal(t, base+1, curA, "one account covered by TWO groups (different models) must count exactly once")
	curB, _, err := w.WaterLevel(ctx, modelB)
	require.NoError(t, err)
	require.Equal(t, base+1, curB, "per-model WaterLevel must return the same pool total")

	// ② 去重边界：同一账号再挂一个"允许一切"的覆盖组 → 仍只计 1，不得翻番。
	mustLinkAccountToGroup(t, client, accDual.ID, grpAllowAll.ID)
	curAfterThirdGroup, _, err := w.WaterLevel(ctx, modelA)
	require.NoError(t, err)
	require.Equal(t, base+1, curAfterThirdGroup, "same account in a third covering group must still count once")

	// ③ 第二个账号只挂在 grpB → 总量 +1（累计 +2）。
	accSecond := mustCreateCursorAccount(t, client, holder.ID, withCursorWaterModels())
	accs = append(accs, accSecond.ID)
	mustLinkAccountToGroup(t, client, accSecond.ID, grpB.ID)
	curTwo, _, err := w.WaterLevel(ctx, modelA)
	require.NoError(t, err)
	require.Equal(t, base+2, curTwo, "a second distinct servable account must add exactly one to the pool total")

	// ④ 不计入：裸号（未显式挂组）。
	accBare := mustCreateCursorAccount(t, client, holder.ID, withCursorWaterModels())
	accs = append(accs, accBare.ID)
	curBare, _, err := w.WaterLevel(ctx, modelA)
	require.NoError(t, err)
	require.Equal(t, base+2, curBare, "bare cursor account (no group) must NOT count")

	// ⑤ 不计入：只挂在"不覆盖任何 cursor 实供模型"的分组上的账号。
	accNonCover := mustCreateCursorAccount(t, client, holder.ID, withCursorWaterModels())
	accs = append(accs, accNonCover.ID)
	mustLinkAccountToGroup(t, client, accNonCover.ID, grpNonCursor.ID)
	curNonCover, _, err := w.WaterLevel(ctx, modelA)
	require.NoError(t, err)
	require.Equal(t, base+2, curNonCover, "account mounted only to a non-cursor-covering group must NOT count")
}

// TestQuotaKeyModelWater_WaterLevelCursorPlatformOnly 验证裁定 1：水位不再限定 platform=cursor。
// 同一实供模型被 cursor 平台分组与非 cursor 平台分组的白名单同时覆盖时，两个分组的候选账号
// 都计入水位（§13.1"当前允许 M 的分组"不再限定 cursor 平台）。
func TestQuotaKeyModelWater_WaterLevelCursorPlatformOnly(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// D-CRT-01W-FIX2：注入齐备依赖走 routed 路径（复用 newRoutedWater + stubResolverAllCursor），
	// 替换原 NewQuotaKeyModelWater(client, curF1ConfigSelecting(...))（不注入路由依赖会 fail-closed）。
	// 断言语义（裁定 1：水位不再限定 platform=cursor）不变。配置源仅被 ListAllowedModels 消费，
	// WaterLevel 不依赖它，故此处不再传 curF1ConfigSelecting。
	w := newRoutedWater(t, client, stubResolverAllCursor(), stubChannelServiceNoChannel(), &GatewayService{})

	// 同名实供模型：cursor 分组与非 cursor 分组白名单各覆盖一次。
	modelM := "grok-4.7"
	grpCursor := mustCreateCursorGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{modelM}})
	// 非 cursor 平台分组（取 composite：生产现实里 cursor 账号由 composite 聚合组调度，且 §18.4 W
	// routed 口径准入 composite 平台），验证"非 cursor 平台分组同样计入"。旧用例用空平台
	// mustCreateGroup 在旧代码无平台过滤时也能覆盖，新代码下改用 composite 组。
	grpOther := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{modelM}})

	holder := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// A04-G 来源门修复（c1p1-I）：装齐状态+快照 provider，使 holder 名下账号通过来源门计入水位。
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{holder.ID: domain.QuotaKeyStatusActive},
		snapshots: map[int64][]byte{holder.ID: []byte(fmt.Sprintf(
			`{"total":400,"used":0,"status_code":200,"checked_at":%q,"error_code":""}`,
			time.Now().UTC().Format(time.RFC3339)))},
	})
	// 两条同规格候选账号（schedulable / active / 未软删 / 非 temp 窗口），仅所属分组平台不同。
	accCursor := mustCreateCursorAccount(t, client, holder.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, accCursor.ID, grpCursor.ID)
	accOther := mustCreateCursorAccount(t, client, holder.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, accOther.ID, grpOther.ID)

	cur, thr, err := w.WaterLevel(ctx, modelM)
	require.NoError(t, err)
	require.Equal(t, quotaKeyWaterLevelThreshold, thr, "threshold must be constant 10")
	// 持久库可能已有其它覆盖 grok-4.7 的分组/账号，故用 >= 容错；本用例自有 2 条必计入。
	require.GreaterOrEqual(t, cur, 2,
		"accounts in BOTH cursor and non-cursor covering groups count; non-cursor platform no longer excluded (裁定 1)")

	// 反向对照：仅非 cursor 平台分组（composite）覆盖的实供模型，水位仍 >0（不再虚高为 0）。
	modelOtherOnly := "gpt-6-sol"
	grpOtherOnly := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{modelOtherOnly}})
	accOtherOnly := mustCreateCursorAccount(t, client, holder.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, accOtherOnly.ID, grpOtherOnly.ID)

	// R10-1（CUR-F13）：复原本用例自建分组（覆盖 grok-4.7 / gpt-6-sol 的启用组）与自有账号，
	// 避免其残留共享库污染他用例（如 F10 全等挂载断言、F7 零覆盖判定）。先清挂载链接、再删分组、
	// 软删自有账号（清链接后账号变裸号，若不软删会在共享库累积；水位不数裸号，但仍应复原）。
	leakedGroupIDs := []int64{grpCursor.ID, grpOther.ID, grpOtherOnly.ID}
	leakedAccountIDs := []int64{accCursor.ID, accOther.ID, accOtherOnly.ID}
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range leakedGroupIDs {
			_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(gid)).Exec(cctx)
			_, _ = client.Group.Delete().Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
		for _, aid := range leakedAccountIDs {
			_ = client.Account.UpdateOneID(aid).SetDeletedAt(time.Now()).Exec(cctx)
		}
	})

	cur, _, err = w.WaterLevel(ctx, modelOtherOnly)
	require.NoError(t, err)
	require.GreaterOrEqual(t, cur, 1, "model covered only by a non-cursor group must have water level > 0 (裁定 1)")
}

// TestQuotaKeyModelWater_WaterLevelExcludesTempWindow 验证 temp 窗口排除：
// temp_unschedulable_until 在将来 → 排除（水位 0）；置为过去 → 计入（水位 1）。
func TestQuotaKeyModelWater_WaterLevelExcludesTempWindow(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// D-CRT-01W-FIX2：注入齐备依赖走 routed 路径（复用 newRoutedWater + stubResolverAllCursor），
	// 替换原 NewQuotaKeyModelWater(client, nil)（不注入路由依赖会 fail-closed）。temp 窗口排除
	// 语义由 IsSchedulable（含 TempUnschedulableUntil 判定）在 routed 入口统一处理，断言不变。
	w := newRoutedWater(t, client, stubResolverAllCursor(), stubChannelServiceNoChannel(), &GatewayService{})

	// 总量口径：分组必须"允许任一 cursor 实供模型"才构成挂组门槛，愿条件是 cover 判定本身，
	// 故这里用一个实供模型构造覆盖组（不再使用与实供清单无关的临时模型名）。§18.4 W routed 口径
	// 对分组平台有准入过滤，旧用例用空平台 mustCreateGroup 在旧代码无平台过滤时也能覆盖，新代码
	// 下改用 composite 组。
	modelM := CursorRealSupplyModels[0].ID
	grp := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{modelM}})

	holder := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// A04-G 来源门修复（c1p1-I）：装齐状态+快照 provider，使 holder 名下账号通过来源门计入水位。
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{holder.ID: domain.QuotaKeyStatusActive},
		snapshots: map[int64][]byte{holder.ID: []byte(fmt.Sprintf(
			`{"total":400,"used":0,"status_code":200,"checked_at":%q,"error_code":""}`,
			time.Now().UTC().Format(time.RFC3339)))},
	})
	acc := mustCreateCursorAccount(t, client, holder.ID, withCursorWaterModels(), func(c *ent.AccountCreate) {
		c.SetTempUnschedulableUntil(time.Now().Add(time.Hour)) // 将来 → 临时不可调度
	})
	mustLinkAccountToGroup(t, client, acc.ID, grp.ID)
	// R10-1：复原（清挂载链接 → 删分组 → 软删账号），避免残留污染他用例的总量基数。
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(grp.ID)).Exec(cctx)
		_, _ = client.Group.Delete().Where(entgroup.IDEQ(grp.ID)).Exec(cctx)
		_ = client.Account.UpdateOneID(acc.ID).SetDeletedAt(time.Now()).Exec(cctx)
	})

	// 基线：持久库可能已有无关系的可服务账号，故以"基线 + 增量"断言。
	base, _, err := w.WaterLevel(ctx, modelM)
	require.NoError(t, err)

	cur, _, err := w.WaterLevel(ctx, modelM)
	require.NoError(t, err)
	require.Equal(t, base, cur, "account in temp window (future) must be excluded from pool total")

	// 窗口过期后重新判定 → 计入（总量 +1，不多不少）。
	_, err = client.Account.UpdateOneID(acc.ID).
		SetTempUnschedulableUntil(time.Now().Add(-time.Hour)).
		Save(ctx)
	require.NoError(t, err)
	cur, _, err = w.WaterLevel(ctx, modelM)
	require.NoError(t, err)
	require.Equal(t, base+1, cur, "account past temp window must be counted exactly once in pool total")
}

// TestQuotaKeyModelWater_ListCandidateKeysFiltersByKeyState 验证候选 key 按状态过滤：
// 仅 active + 非 exhausted + 非冷却(last_refresh_error 为空) + 未过期 + 有剩余容量
// （working<15）的 key 入选；其余被排除（模型 M 不收窄候选集）。
func TestQuotaKeyModelWater_ListCandidateKeysFiltersByKeyState(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	w := NewQuotaKeyModelWater(client, nil)

	keyOK := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	keyExhausted := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	_, err := client.QuotaKey.UpdateOneID(keyExhausted.ID).SetExhausted(true).Save(ctx)
	require.NoError(t, err)
	keyExpired := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	_, err = client.QuotaKey.UpdateOneID(keyExpired.ID).SetExpiresAt(time.Now().Add(-time.Hour)).Save(ctx)
	require.NoError(t, err)
	keyCooldown := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 2026-11-06 用户裁定后：冷却分类前缀 + 近期 last_refresh_at **不再**让码退出候选，
	// 因为拉号只看号数。此处构造该状态，用来反向断言"冷却挡不住号数不足的码"。
	_, err = client.QuotaKey.UpdateOneID(keyCooldown.ID).
		SetLastRefreshError(ErrUpstreamCooldown.Error() + ": 每5分钟内仅可刷新1次").
		SetLastRefreshAt(time.Now()).
		Save(ctx)
	require.NoError(t, err)
	keyFull := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, keyFull.ID, MaxAccountsPerQuotaKey) // 号数 ≥ 5 → 自动补位已满

	cands, err := w.ListCandidateKeys(ctx, "cursor-fix1-cand", 0)
	require.NoError(t, err)
	require.Contains(t, cands, keyOK.ID, "active key with capacity must be a candidate")
	require.NotContains(t, cands, keyExhausted.ID, "exhausted key must be excluded")
	require.NotContains(t, cands, keyExpired.ID, "expired key must be excluded")
	require.Contains(t, cands, keyCooldown.ID,
		"cooldown marker must NOT exclude a key with < 5 accounts (2026-11-06 裁定：只看号数)")
	require.NotContains(t, cands, keyFull.ID, "key with no remaining capacity must be excluded")
}

// TestQuotaKeyService_PullForModel_ResolverErrorReturnsMountFailedSentinel 验证 §13.3 F4：
// 上游成功之后、挂载解析器（cursorGroupResolver）出错（解析 U7 目标分组失败）→ 与挂载失败
// 同哨兵 ErrQuotaKeyMountFailed 可识别返回（调用方据以记录"上游可能已被消耗一次"），且事务回滚（号不入库）。
func TestQuotaKeyService_PullForModel_ResolverErrorReturnsMountFailedSentinel(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "m5", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	})
	// 注入挂载解析器错误：模拟解析 U7 目标分组时 DB 失败（上游已成功发号）。
	svc.cursorGroupResolver = func(_ context.Context, _ *ent.Tx) ([]int64, error) {
		return nil, errors.New("db: resolve covering groups failed")
	}
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.PullAccountsForModel(ctx, qk.ID, "cursor-model-z")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"mount resolver error after upstream success must be recognizable via ErrQuotaKeyMountFailed")

	// 非幂等边界：本地事务回滚，上游发号不入库。
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "resolver failure must roll back the pulled account (not persisted)")
}

// ---------------------------------------------------------------------------
// FIX-4 R3-1：上游发号成功后的【落库失败】分支哨兵用例（unit 标签，连本地 quota_test 库）。
// ---------------------------------------------------------------------------

// TestQuotaKeyService_Pull_InsertFailureCarriesMountFailedSentinel 验证 R3-1：
// pull 内 create.Save 失败（上游发号成功、本地落库失败）与解析失败 / 零覆盖 / 挂组失败
// 同一"上游可能已被消耗"语义 —— 补挂 ErrQuotaKeyMountFailed 哨兵，使触发方（reconciler
// pullKey）按非幂等边界"告警 + 停轮"处理，而不当作普通错误继续拉下一个候选。
//
// 断言三件事：① errors.Is 双路可达（哨兵可识别 + 底层落库失败原因保留在错误链中，未吞）；
// ② 号未入库（事务回滚）；③ 停轮链保持绿（见 reconciler 侧
// TestQuotaKeyReconciler_InsertFailure_StopsRoundAndRecordsHeartbeat 端到端用例）。
func TestQuotaKeyService_Pull_InsertFailureCarriesMountFailedSentinel(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// 上游发号成功（返回 1 个号），但载荷不合规（空 name）→ 本地 create.Save 被拒（落库失败）。
	up := &fix3CountingUpstream{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "", Platform: domain.PlatformCursor, Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	}
	svc, _ := qkNewTestService(t, up)
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.PullAccountsForModel(ctx, qk.ID, "cursor-model-insert-fail")
	require.Error(t, err)

	// 时点证据：上游已被真实调用（号已发出）→ 落库失败发生在上游成功之后。
	require.Equal(t, int64(1), up.calls.Load(),
		"upstream must have issued the account before the local insert failed")

	// ① 双路可达：哨兵 errors.Is 命中，且底层落库失败原因仍在错误链中（未被哨兵吞掉）。
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"create.Save failure after upstream success must be recognizable via ErrQuotaKeyMountFailed")
	require.Contains(t, err.Error(), "create account",
		"underlying insert failure must remain in the error chain (not swallowed by the sentinel)")

	// ② 号未入库：非幂等边界下本地事务回滚。
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "insert failure after upstream success must roll back (no account persisted)")
}

// ---------------------------------------------------------------------------
// FIX-5 R4-1：插入失败 key 退出候选（标记驱动 + 可人工恢复）用例
//（unit 标签，连本地 quota_test 库）。
// ---------------------------------------------------------------------------

// TestQuotaKeyService_Pull_InsertFailureMarksKeyExitsCandidatesAndRecoverable 验证 R4-1：
// pull 内 create.Save 失败（上游发号成功、本地落库失败）在返回 ErrQuotaKeyMountFailed
// 哨兵的同时，独立事务补写 last_refresh_error + exhausted=true —— 该分支属"上游已发号、
// 仅本地落库失败"的非幂等边界（区别于 CUR-P1-FIX-KEY-EXHAUSTED 约束的"未知/普通上游失败"：
// 后者只写标记、不动 exhausted），故仍以 exhausted=true 占位排除：
//  1. last_refresh_error 非空且 exhausted=true（非幂等边界占位排除）；
//  2. 该 key 被 ListCandidateKeys 的 Exhausted(false) 查询自然排出候选集 —— 后续周期
//     不再自动消耗上游（零新字段、零新状态；替代原 last_refresh_error IS NULL 过滤）；
//  3. 人工 ClearExhausted（既有的清除路径，清 exhausted + last_refresh_error）后 key 回到
//     候选集 —— 证明"退出"是标记驱动、可人工恢复，而非永久封禁。
func TestQuotaKeyService_Pull_InsertFailureMarksKeyExitsCandidatesAndRecoverable(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// 上游发号成功（返回 1 个号），但载荷不合规（空 name）→ 本地 create.Save 被拒。
	up := &fix3CountingUpstream{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "", Platform: domain.PlatformCursor, Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	}
	svc, _ := qkNewTestService(t, up)
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	w := NewQuotaKeyModelWater(client, nil)

	// 前置：失败前该 key 在候选集内（active / 非 exhausted / 无错误标记 / 有剩余容量）。
	cands, err := w.ListCandidateKeys(ctx, "cursor-fix5-r4-cand", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qk.ID, "fresh active key must be a candidate before the failure")

	err = svc.PullAccountsForModel(ctx, qk.ID, "cursor-fix5-r4-insert-fail")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"insert failure must still surface the non-idempotent sentinel")
	require.Equal(t, int64(1), up.calls.Load(), "upstream must have issued the account before the local insert failed")

	// ① 标记写入：last_refresh_error 非空且 exhausted=true（非限频错误占位排除，裁定 3 同口径）。
	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastRefreshError, "insert failure must record last_refresh_error (key needs human attention)")
	require.Contains(t, *got.LastRefreshError, "create account",
		"marker must keep the underlying insert failure reason")
	require.True(t, got.Exhausted,
		"insert failure is a non-cooldown error: per 裁定 3 it must be excluded via exhausted=true placeholder")

	// ② 脱离候选：ListCandidateKeys 的 Exhausted(false) 查询（替代原 last_refresh_error IS NULL
	// 过滤）把该 key 排出后续周期候选。
	cands, err = w.ListCandidateKeys(ctx, "cursor-fix5-r4-cand", 0)
	require.NoError(t, err)
	require.NotContains(t, cands, qk.ID, "key with a pending insert failure must exit the candidate set")

	// ③ 人工恢复：ClearExhausted（既有清除路径）清掉标记 → key 回到候选集。
	require.NoError(t, svc.ClearExhausted(ctx, qk.ID))
	after, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.Nil(t, after.LastRefreshError, "manual clear must drop the failure marker")
	cands, err = w.ListCandidateKeys(ctx, "cursor-fix5-r4-cand", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qk.ID, "after manual clear the key must return to the candidate set")
}

// ---------------------------------------------------------------------------
// FIX-6 R5-1：多账号交错用例（部分成功 + 失败 → 零裸号入库 + 标记持久 + 哨兵可达）
//（unit 标签，连本地 quota_test 库）。
// ---------------------------------------------------------------------------

// TestQuotaKeyService_Pull_InsertFailureMultiAccountRollsBackPartial 是 R5-1 核心的多账号
// 交错用例：上游一次返多号（≥2），第 2 个插入失败（载荷不合规）→ 断言第 1 个账号【未入库】
// （事务整体回滚，零裸号）、key 的 last_refresh_error 已标记（跨轮退出候选）、且哨兵
// errors.Is(err, ErrQuotaKeyMountFailed) 可达。
func TestQuotaKeyService_Pull_InsertFailureMultiAccountRollsBackPartial(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// 上游一次返 2 号：第 1 个合规（本应入库）、第 2 个空 name（插入必失败）。
	up := &fix3CountingUpstream{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "ok-account", Platform: domain.PlatformCursor, Type: "oauth", Credentials: map[string]any{"k": "v"}},
			{Name: "", Platform: domain.PlatformCursor, Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	}
	svc, _ := qkNewTestService(t, up)
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	w := NewQuotaKeyModelWater(client, nil)

	// 前置：失败前该 key 在候选集内（active / 非 exhausted / 无错误标记 / 有剩余容量）。
	pre, err := w.ListCandidateKeys(ctx, "cursor-r5-multi", 0)
	require.NoError(t, err)
	require.Contains(t, pre, qk.ID, "fresh active key must be a candidate before the failure")

	err = svc.PullAccounts(ctx, qk.ID)
	require.Error(t, err)

	// 时点证据：上游已被真实调用（号已发出）→ 落库失败发生在上游成功之后。
	require.Equal(t, int64(1), up.calls.Load(),
		"upstream must have issued accounts before the local insert failed")

	// ① 哨兵可达：触发方据此停轮 + 记录"上游可能已被消耗一次"告警（非幂等边界）。
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"insert failure (multi-account) must be recognizable via ErrQuotaKeyMountFailed")
	require.Contains(t, err.Error(), "create account",
		"underlying insert failure must remain in the error chain (not swallowed by the sentinel)")

	// ② 零裸号：第 1 个账号已插入到容量事务内，但插入失败触发整体回滚 ⇒ 一个都不入库。
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "partial insert before failure must be rolled back (no naked account persisted)")

	// ③ 标记持久：last_refresh_error 非空且 exhausted=true（非限频错误占位排除，裁定 3 同口径）
	// ⇒ 被 ListCandidateKeys 的 Exhausted(false) 查询排出，跨轮退出候选（R5-1 a）。
	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastRefreshError, "insert failure must record last_refresh_error (cross-round exit)")
	require.Contains(t, *got.LastRefreshError, "create account",
		"marker must keep the underlying insert failure reason")
	require.True(t, got.Exhausted,
		"insert failure is a non-cooldown error: per 裁定 3 it must be excluded via exhausted=true placeholder")

	// ④ 脱离候选：ListCandidateKeys 的 Exhausted(false) 查询（替代原 last_refresh_error IS NULL
	// 过滤）把该 key 排出后续周期候选。
	post, err := w.ListCandidateKeys(ctx, "cursor-r5-multi", 0)
	require.NoError(t, err)
	require.NotContains(t, post, qk.ID, "key with a pending insert failure must exit the candidate set")
}

// TestQuotaKeyService_Pull_InsertFailureMarkerWriteFailsRollsBackOnlyThisRound 是 R5-1(b) 边界
// 用例：插入失败 + 标记写失败 ⇒ 整体回滚（号不入库）+ 哨兵停轮；但 last_refresh_error 未持久
// ⇒ 该 key 仍可能被后续周期重新选为候选（R5-2 边界：标记写失败 = 仅本轮哨兵停轮，不夸大保护
// 范围为"跨轮退出"）。
//
// 通过注入 insertFailureMarkerWriter 使"独立事务补标"失败，确定性触发 R5-1(b) 降级路径
// （与生产 cursorGroupResolver 同一类测试 seams）。
func TestQuotaKeyService_Pull_InsertFailureMarkerWriteFailsRollsBackOnlyThisRound(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	up := &fix3CountingUpstream{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "", Platform: domain.PlatformCursor, Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	}
	svc, _ := qkNewTestService(t, up)
	// 注入"标记写失败"：独立事务补标失败，走 R5-1(b) 降级（仅本轮哨兵停轮）。
	svc.insertFailureMarkerWriter = func(_ context.Context, _ int64, _ error) error {
		return fmt.Errorf("injected marker write failure")
	}
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	w := NewQuotaKeyModelWater(client, nil)

	pre, err := w.ListCandidateKeys(ctx, "cursor-r5-markerfail", 0)
	require.NoError(t, err)
	require.Contains(t, pre, qk.ID, "fresh active key must be a candidate before the failure")

	err = svc.PullAccounts(ctx, qk.ID)
	require.Error(t, err)
	require.Equal(t, int64(1), up.calls.Load(), "upstream must have issued the account before the local insert failed")

	// 哨兵仍可达（本轮停轮）。
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"insert failure with marker write failure must still surface the sentinel (stop this round)")

	// 整体回滚：号不入库（现状已对）。
	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n, "insert failure must roll back (no account persisted) even when marker write fails")

	// R5-2 边界：标记未持久（marker writer 被注入为失败）⇒ last_refresh_error 仍空、exhausted 不变
	// ⇒ 该 key 不脱离候选（不夸大"跨轮退出"保护，仅本轮停轮）。
	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.Nil(t, got.LastRefreshError, "R5-2 boundary: failed marker write must NOT persist last_refresh_error")
	require.False(t, got.Exhausted)
	post, err := w.ListCandidateKeys(ctx, "cursor-r5-markerfail", 0)
	require.NoError(t, err)
	require.Contains(t, post, qk.ID, "R5-2 boundary: marker-write failure must NOT exit candidate set (only this-round stop)")
}

// ---------------------------------------------------------------------------
// B-11：拉号成功路径写入 expires_at（上游 key_info.expired_at 的事实源）。
// 修复前 expires_at 永远为 NULL → sweeper/reconciler 的"未过期"判定形同虚设。
// ---------------------------------------------------------------------------

// TestQuotaKeyPull_WritesExpiresAtFromUpstream：上游给出到期时间 → 成功落库写入 expires_at。
func TestQuotaKeyPull_WritesExpiresAtFromUpstream(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	upstreamExpiry := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{
			Accounts:  []*UpstreamAccount{{Name: "ok-1", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}}},
			ExpiresAt: &upstreamExpiry,
		},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt, "pull success must persist expires_at from upstream")
	require.WithinDuration(t, upstreamExpiry, *got.ExpiresAt, time.Second,
		"persisted expires_at must equal upstream key_info.expired_at")
}

// TestQuotaKeyPull_UpstreamWithoutExpiry_KeepsExistingExpiresAt：
// 上游未给出到期时间（nil）→ expires_at 原值不动。
// 既不能被清空（管理员手工设置会丢），也不能被填一个推算出的假日期。
func TestQuotaKeyPull_UpstreamWithoutExpiry_KeepsExistingExpiresAt(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{
			Accounts: []*UpstreamAccount{{Name: "ok-1", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}}},
		},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	manual := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	_, err := client.QuotaKey.UpdateOneID(qk.ID).SetExpiresAt(manual).Save(ctx)
	require.NoError(t, err)

	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt, "upstream without expiry must not clear existing expires_at")
	require.WithinDuration(t, manual, *got.ExpiresAt, time.Second,
		"existing expires_at must be preserved when upstream gives no expiry")
}

// TestQuotaKeyPull_UpstreamExpiryOverwritesStaleValue：
// 本地存的是陈旧到期时间 → 上游值必须覆盖（上游是唯一权威）。
func TestQuotaKeyPull_UpstreamExpiryOverwritesStaleValue(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	upstreamExpiry := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{
			Accounts:  []*UpstreamAccount{{Name: "ok-1", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}}},
			ExpiresAt: &upstreamExpiry,
		},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	stale := time.Now().UTC().Add(365 * 24 * time.Hour).Truncate(time.Second)
	_, err := client.QuotaKey.UpdateOneID(qk.ID).SetExpiresAt(stale).Save(ctx)
	require.NoError(t, err)

	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	require.WithinDuration(t, upstreamExpiry, *got.ExpiresAt, time.Second,
		"stale local expires_at must be overwritten by upstream value")
}

// TestQuotaKeyPull_WrittenExpiryDrivesSweeper_EndToEnd：
// 端到端证明修复目的——写入 expires_at 后，sweeper 能把到期码标为 expired，
// 从此不再进入 reconciler 候选（修复前 expires_at 恒 NULL，该链路不可能发生）。
func TestQuotaKeyPull_WrittenExpiryDrivesSweeper_EndToEnd(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// 上游告知的到期时间已过（仍在成功响应里返回，模拟"到期时间刚过但拉号未报错"）。
	past := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{
			Accounts:  []*UpstreamAccount{{Name: "ok-1", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}}},
			ExpiresAt: &past,
		},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	stored, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.ExpiresAt)
	require.True(t, stored.ExpiresAt.Before(time.Now()), "test precondition: stored expiry is in the past")

	// sweeper 标 expired（复用既有 sweepOnce，不新造机制）。
	// 计数只作"本轮确有改写"的下限断言：测试库与其它 QuotaKey 用例共用且不清库，
	// 其它用例遗留的到期码会一并被扫，故不断言精确条数（本 key 的状态才是判据）。
	sweeper := NewQuotaKeySweeper(&testDBRepo{client: client}, nil, client)
	n, err := sweeper.SweepNow(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1, "sweeper must mark at least the expired key")

	after, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusExpired, after.Status)

	// 已 expired 的码不再是候选 —— 这正是 B-11 修复要恢复的语义。
	water := &quotaKeyModelWater{client: client}
	candidates, err := water.ListCandidateKeys(ctx, "cursor-any", 0)
	require.NoError(t, err)
	require.NotContains(t, candidates, qk.ID, "expired key must not remain a rotation candidate")
}

// ---------------------------------------------------------------------------
// CUR-F1（续作卡）：8 条定向测试，逐条对应 /root/CUR-F1.md 裁定点。
// 仅改本测试文件；不动工作树其它文件。需本地 quota_test 库（unit 标签）。
// ---------------------------------------------------------------------------

// curF1ConfigSource 是 AutoEntryConfigProvider 的最小测试替身：仅 GetConfig 返回预设配置，
// ApplyEntryDefaults 原样放行（本卡测试不依赖挂组属性写入）。
type curF1ConfigSource struct {
	cfg *CursorAutoEntryConfig
}

func (c *curF1ConfigSource) GetConfig(_ context.Context) (*CursorAutoEntryConfig, error) {
	return c.cfg, nil
}
func (c *curF1ConfigSource) ApplyEntryDefaults(_ context.Context, _ *ent.Tx, acc *ent.Account, _ *CursorAutoEntryConfig) (*ent.Account, error) {
	return acc, nil
}

// curF1ConfigSelecting 构造一个 AutoEntryConfigProvider 测试替身：启用全部已知厂商，
// 并勾选给定的【实供】模型（vendor 取自 CursorRealSupplyModels）。
// P1-B2 起配置勾选不再是入库模型 / 补池的输入；保留该替身仅用于钉死"勾选态不影响
// 静态清单产出与会挂结果"（回归用例仍以它构造非空勾选态）。
func curF1ConfigSelecting(models ...string) *curF1ConfigSource {
	vendors := make(map[string]bool, len(cursorKnownVendors))
	for v := range cursorKnownVendors {
		vendors[v] = true
	}
	refs := make([]CursorModelRef, 0, len(models))
	for _, m := range models {
		var v string
		for _, sm := range CursorRealSupplyModels {
			if sm.ID == m {
				v = sm.Vendor
				break
			}
		}
		refs = append(refs, CursorModelRef{ID: m, Vendor: v})
	}
	return &curF1ConfigSource{cfg: &CursorAutoEntryConfig{
		Vendors:       vendors,
		Models:        refs,
		EntryDefaults: CursorEntryDefaults{RateMultiplier: 1.0, Priority: 1},
	}}
}

// TestCURF1_AllowedModels_NoCursorGroup_RealSupplyModel（裁定 1，最关键的一条）：
// 库里零 cursor 平台分组、存在若干 active 未软删分组且其白名单允许实供模型 X，
// 配置勾选 X → ListAllowedModels 必含 X、WaterLevel(X) 正常返回、ListCandidateKeys(X) 非空。
// X 必须取实供模型名（grok-4.7），不能用官方模型名——否则会掩盖勾选侧门控清单错。
func TestCURF1_AllowedModels_NoCursorGroup_RealSupplyModel(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 配置勾选实供模型 grok-4.7（vendor xai）。
	cfg := &CursorAutoEntryConfig{
		Vendors:       map[string]bool{CursorVendorXAI: true},
		Models:        []CursorModelRef{{ID: "grok-4.7", Vendor: CursorVendorXAI}},
		EntryDefaults: CursorEntryDefaults{RateMultiplier: 1.0, Priority: 1},
	}
	w := NewQuotaKeyModelWater(client, &curF1ConfigSource{cfg: cfg})

	// composite 聚合组（模型所属平台 cursor 的目标分组；生产无 cursor 平台分组，
	// cursor 账号由 composite 聚合组调度），白名单启用且允许 grok-4.7。
	grp := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"grok-4.7"},
	})
	// R10-1（CUR-F13）：复原本用例自建分组，避免其（覆盖 grok-4.7 的启用组）残留共享库、
	// 污染他用例（如 F10 的全等挂载断言、F7 的零覆盖判定）。先清挂载链接再删分组。
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(grp.ID)).Exec(cctx)
		_, _ = client.Group.Delete().Where(entgroup.IDEQ(grp.ID)).Exec(cctx)
	})

	// ListAllowedModels 必含实供模型 grok-4.7（交集：勾选集合 ∩ 分组白名单命中）。
	allowed, err := w.ListAllowedModels(ctx)
	require.NoError(t, err)
	require.Contains(t, allowed, "grok-4.7",
		"real-supply model selected in config AND allowed by an active group must be in ListAllowedModels")

	// WaterLevel(grok-4.7) 正常返回（阈值恒为 quotaKeyWaterLevelThreshold）。
	cur, thr, err := w.WaterLevel(ctx, "grok-4.7")
	require.NoError(t, err)
	require.Equal(t, quotaKeyWaterLevelThreshold, thr, "water level threshold must be the unified constant")

	// 挂一条账号进覆盖组，令水位可数（证明 WaterLevel 真的按白名单统计，而非空跑）。
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	acc := mustCreateCursorAccount(t, client, qk.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, acc.ID, grp.ID)
	cur, _, err = w.WaterLevel(ctx, "grok-4.7")
	require.NoError(t, err)
	require.GreaterOrEqual(t, cur, 1, "water level must count the account linked into the covering group")

	// ListCandidateKeys(grok-4.7) 非空：qk 为 active / 非 exhausted / 未过期 / 有剩余容量。
	cands, err := w.ListCandidateKeys(ctx, "grok-4.7", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qk.ID, "active key with capacity must be a candidate for the real-supply model")
}

// TestCURF1_ComputeEntryModels_GatingFixedForRealSupplyModels（1b，勾选侧门控已修）：
// ComputeEntryModels 对实供清单 11 项 + §2 保留项全部不剔除；Validate 对 muse/composer
// 厂商不报 unknown vendor。
func TestCURF1_ComputeEntryModels_GatingFixedForRealSupplyModels(t *testing.T) {
	// 用实供清单全集构造配置（所有厂商启用）。
	vendors := make(map[string]bool, len(cursorKnownVendors))
	for v := range cursorKnownVendors {
		vendors[v] = true
	}
	models := make([]CursorModelRef, 0, len(CursorRealSupplyModels))
	for _, m := range CursorRealSupplyModels {
		models = append(models, CursorModelRef{ID: m.ID, Vendor: m.Vendor})
	}
	cfg := &CursorAutoEntryConfig{
		Vendors:       vendors,
		Models:        models,
		EntryDefaults: CursorEntryDefaults{RateMultiplier: 1.0, Priority: 1},
	}

	// 实供清单每一项都必须被保留（不得因官方模型名快照门控而剔除）。
	got := ComputeEntryModels(cfg)
	require.Len(t, got, len(CursorRealSupplyModels),
		"all real-supply models must survive ComputeEntryModels gating")
	for _, m := range CursorRealSupplyModels {
		require.Contains(t, got, m.ID, "real-supply model %q must NOT be filtered out by gating", m.ID)
	}

	// Validate 不得报 unknown vendor（muse-spark-1.3 / composer-2.5 已纳入 cursorKnownVendors）。
	require.Equal(t, "", cfg.Validate(),
		"config with muse/composer real-supply models must pass Validate (no unknown vendor)")
}

// TestCURF1_AllowedModels_IntersectionExcludesOutside（2，交集之外不纳入）：
// 配置勾选 [grok-4.7, gpt-6.1-sol]，但仅 grok-4.7 被本用例创建的 active 分组白名单允许；
// gpt-6.1-sol（同为实供模型，vendor openai）无任何分组白名单 Allows → 不得出现在
// ListAllowedModels（禁止把"勾选了但无分组覆盖"的模型盲目纳入补池）。
//
// 注意：本用例的"应排除"模型刻意取 gpt-6.1-sol 而非 gpt-6-sol——quota_test 是跨测试共享的
// 持久库，既有用例 TestQuotaKeyModelWater_ListAllowedModelsCursorPlatformOnly 会遗留一个允许
// gpt-6-sol 的 active 分组，使 gpt-6-sol 在共享库里恒被覆盖、断言 NotContains 永远失败。
// gpt-6.1-sol 未被任何其它用例的 allowlist 引用，故在共享库下该断言确定成立。
func TestCURF1_AllowedModels_IntersectionExcludesOutside(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// R9-1（CUR-F12，2026-10-17）：删除"遍历删全库 allow-all 组"的隔离段——该段会误删
	// 其他用例（如 TestQuotaKeyService_RefreshAccounts_MountsToCursorGroups 的 grpComposite）
	// 的 allow-all 数据。改为"确定性构造 + 全等断言"：本用例只自建一个精确覆盖组（见下），
	// 断言 ListAllowedModels == 勾选∩有覆盖；若共享库被他用例遗留 allow-all 组（Allows 恒真），
	// 会使 gpt-6.1-sol 混入结果使全等断言失败并暴露污染（而非旧版 NotContains 的静默假绿）。

	cfg := &CursorAutoEntryConfig{
		Vendors: map[string]bool{CursorVendorXAI: true, CursorVendorOpenAI: true},
		Models: []CursorModelRef{
			{ID: "grok-4.7", Vendor: CursorVendorXAI},
			{ID: "gpt-6.1-sol", Vendor: CursorVendorOpenAI},
		},
		EntryDefaults: CursorEntryDefaults{RateMultiplier: 1.0, Priority: 1},
	}
	w := NewQuotaKeyModelWater(client, &curF1ConfigSource{cfg: cfg})

	// 仅允许 grok-4.7 的分组（无分组允许 gpt-6.1-sol）。P1-B2 U7：须为模型所属平台的目标
	// 分组——composite 聚合组（生产形态），否则不构成覆盖。
	grp, err := client.Group.Create().
		SetName(fmt.Sprintf("c11-f1-out-%d", time.Now().UnixNano())).
		SetPlatform(domain.PlatformComposite).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{"grok-4.7"}}).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().
			Where(accountgroup.GroupIDEQ(grp.ID)).Exec(context.Background())
		_, _ = client.Group.Delete().
			Where(entgroup.IDEQ(grp.ID)).Exec(context.Background())
	})

	allowed, err := w.ListAllowedModels(ctx)
	require.NoError(t, err)
	// R9-1 全等断言：本用例仅自建一个只许 grok-4.7 的精确覆盖组，故允许集合应确定等于
	// 勾选∩有覆盖 = {grok-4.7}。若共享库被他用例遗留 allow-all 组（Enabled=false，Allows 恒真），
	// gpt-6.1-sol 会被它覆盖而混入结果，使本全等断言失败（暴露污染），而不是旧版 NotContains
	// 那种"因误删他用例数据才侥幸通过"的静默假绿。
	require.ElementsMatch(t, []string{"grok-4.7"}, allowed,
		"ListAllowedModels must equal exactly selected∩covered = {grok-4.7}")
}

// TestCURF1_UpstreamClassify_ChineseMsgPreserved（3，上游中文 msg 原样）：
// "每5分钟内仅可刷新1次，请3分钟后再试" → errors.Is(err, ErrUpstreamCooldown) 为真且文本含原文；
// "密钥额度耗尽或已过期" → 不判冷却，落到过期哨兵。
// 直接驱动上游客户端的 handleResponse（code=1 分类入口），不发起真实 HTTP。
func TestCURF1_UpstreamClassify_ChineseMsgPreserved(t *testing.T) {
	c := &shareAIUpstreamClient{}

	// 限频语义。
	cooldownMsg := "每5分钟内仅可刷新1次，请3分钟后再试"
	_, err := c.handleResponse(&shareAIResponse{Code: 1, Msg: cooldownMsg})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrUpstreamCooldown),
		"rate-limit msg must be classified as ErrUpstreamCooldown")
	require.Contains(t, err.Error(), cooldownMsg,
		"cooldown error text must embed the original Chinese msg verbatim")

	// 过期/耗尽终态语义。
	expiredMsg := "密钥额度耗尽或已过期"
	_, err = c.handleResponse(&shareAIResponse{Code: 1, Msg: expiredMsg})
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrUpstreamCooldown),
		"expired/quota-exhausted msg must NOT be classified as cooldown")
	require.True(t, errors.Is(err, ErrUpstreamKeyExpired),
		"expired/quota-exhausted msg must hit ErrUpstreamKeyExpired sentinel")
}

// TestCURF1_UpstreamClassify_UnknownCode1FailsClosed（4，未知 code=1 msg 失败关闭）：
// 既非限频也非过期耗尽 → 失败关闭，错误文本保留 msg 原文。
func TestCURF1_UpstreamClassify_UnknownCode1FailsClosed(t *testing.T) {
	c := &shareAIUpstreamClient{}

	msg := "系统当前繁忙，请稍后再试"
	_, err := c.handleResponse(&shareAIResponse{Code: 1, Msg: msg})
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrUpstreamCooldown),
		"unknown code=1 msg must NOT be classified as cooldown")
	require.False(t, errors.Is(err, ErrUpstreamKeyExpired),
		"unknown code=1 msg must NOT be classified as expired")
	require.Contains(t, err.Error(), msg,
		"unknown code=1 error text must preserve the original msg verbatim (fail-closed)")
}

// TestCURF1_CandidateKeys_CooldownWindow（5，冷却不再参与候选过滤）：
// 2026-11-06 用户裁定："有 5 个号，就不再拉，少于 5 个号就拉，其他条件都不需要设。"
// 故冷却标记（无论窗口内 now-1min 还是窗口外 now-6min）都不再把"号数不足"的码挡在候选之外，
// 两个时间点都必须入选。旧行为（窗口内排除 / 窗口外重新入选）已随裁定废弃。
func TestCURF1_CandidateKeys_CooldownWindow(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	w := NewQuotaKeyModelWater(client, nil)

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 冷却错误标记：前缀须为 ErrUpstreamCooldown.Error()（与 writeCooldownMarker 写入形态一致）。
	cooldownErr := ErrUpstreamCooldown.Error() + ": 每5分钟内仅可刷新1次"

	// 窗口内（now-1min）：冷却窗口不得阻挡拉号 → 仍入选。
	_, err := client.QuotaKey.UpdateOneID(qk.ID).
		SetLastRefreshError(cooldownErr).
		SetLastRefreshAt(time.Now().Add(-1 * time.Minute)).
		Save(ctx)
	require.NoError(t, err)
	cands, err := w.ListCandidateKeys(ctx, "cursor-f1-cool", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qk.ID,
		"cooldown marker must not exclude a key with < 5 accounts (no cooldown window filtering)")

	// 窗口外（now-6min）：同样入选（行为与窗口内一致）。
	_, err = client.QuotaKey.UpdateOneID(qk.ID).
		SetLastRefreshAt(time.Now().Add(-6 * time.Minute)).
		Save(ctx)
	require.NoError(t, err)
	cands, err = w.ListCandidateKeys(ctx, "cursor-f1-cool", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qk.ID,
		"cooldown marker must not exclude the key regardless of elapsed time")
}

// TestListCandidateKeys_StoppedAccountsCountTowardTotal（2026-11-10 派发单 c1p1-A）：
// 候选计数已收敛为「非软删总行数 total」，停调/待恢复的号同样计入水位。
// 背景：实测单号并发峰值 1，「1 主 1 热备」看的是该码名下所有未软删账号总数，
// 停调的号也占水位，避免热备码被反复补超过目标数。
//
// 这与旧口径（只数可用数、停调不算）正好相反——旧口径会让"2 个号 1 个停调"误判为缺口并重补。
// 旧用例 TestListCandidateKeys_StoppedAccountsDoNotCount（working 口径）随本次收敛作废，
// 由本用例按 total 口径重新钉死。
func TestListCandidateKeys_StoppedAccountsCountTowardTotal(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	w := NewQuotaKeyModelWater(client, nil) // 无 upstream：target = CursorAutoPullAccountsPerKey = 2

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, 2) // 表里 2 个号，全部可用

	// 对照：2 个号都可用 → 已达目标 2 → 不该入选。
	cands, err := w.ListCandidateKeys(ctx, "cursor-stopped", 0)
	require.NoError(t, err)
	require.NotContains(t, cands, qk.ID, "2 个可用号已达目标，不该再拉")

	// 其中一个置为"停调中且未到期"：total 仍为 2（停调号计入总行数），不因此出现"缺口"。
	_, err = client.Account.Update().
		Where(account.QuotaKeyID(qk.ID), account.NameEQ(fmt.Sprintf("acc-%d-0", qk.ID))).
		SetTempUnschedulableUntil(time.Now().Add(30 * time.Minute)).
		Save(ctx)
	require.NoError(t, err)

	cands, err = w.ListCandidateKeys(ctx, "cursor-stopped", 0)
	require.NoError(t, err)
	require.NotContains(t, cands, qk.ID,
		"停调号计入 total（2026-11-10）：1 主 1 热备，total=2 已满，不得因停调重补")

	// 真正不足：删到 1 个号（total=1 < 2）→ 必须补。
	_, err = client.Account.Delete().
		Where(account.QuotaKeyID(qk.ID), account.NameEQ(fmt.Sprintf("acc-%d-1", qk.ID))).
		Exec(ctx)
	require.NoError(t, err)

	cands, err = w.ListCandidateKeys(ctx, "cursor-stopped", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qk.ID, "total=1 < 2 必须补号")
}

// TestCursorAutoPullAccountsPerKeyIsTwo（c1p1-A 要求 1）：
// 自动补位目标收敛为 1 主 1 热备（2），用户裁定 2026-11-10。
func TestCursorAutoPullAccountsPerKeyIsTwo(t *testing.T) {
	require.Equal(t, 2, CursorAutoPullAccountsPerKey,
		"c1p1-A: 自动补位目标应收敛为 1 主 1 热备（2）")
}

// TestListCandidateKeys_TotalCountNotWorking（c1p1-A 要求 2）：
// 候选计数收敛为「非软删总行数 total」。total=2 即便其中 1 个停调（working=1）也不入选；
// total=1 才入选。
func TestListCandidateKeys_TotalCountNotWorking(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	w := NewQuotaKeyModelWater(client, nil) // 无 upstream：target = CursorAutoPullAccountsPerKey = 2

	// total=2、working=1（一个号停调）→ 不返回。
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, 2)
	_, err := client.Account.Update().
		Where(account.QuotaKeyID(qk.ID), account.NameEQ(fmt.Sprintf("acc-%d-0", qk.ID))).
		SetTempUnschedulableUntil(time.Now().Add(30 * time.Minute)).
		Save(ctx)
	require.NoError(t, err)
	cands, err := w.ListCandidateKeys(ctx, "cursor-total-2", 0)
	require.NoError(t, err)
	require.NotContains(t, cands, qk.ID, "total=2（含 1 停调）→ 不补")

	// total=1 → 返回。
	qk1 := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk1.ID, 1)
	cands, err = w.ListCandidateKeys(ctx, "cursor-total-1", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qk1.ID, "total=1 < 2 → 必须补")
}

// TestListCandidateKeys_QuotaExhaustionGate（c1p1-A 要求 3；c1p1-G 修正）：
// 码级额度耗尽门：新鲜快照 remaining=0 → 跳过；remaining=100 → 不跳过（入选）；
// checked_at 超过 5 分钟（陈旧）但曾确认耗尽（remaining=0）→ 跳过（沿用最近成功结论，不放行）；
// 陈旧且曾确认健康（remaining=100）→ 不跳过（入选）。
func TestListCandidateKeys_QuotaExhaustionGate(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	w := NewQuotaKeyModelWater(client, nil)

	now := time.Now()
	makeKeyWithSnapshot := func(total, used int, checkedAt time.Time) *ent.QuotaKey {
		qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
		snap := fmt.Sprintf(
			`{"total":%d,"used":%d,"status_code":200,"checked_at":%q,"attempted_at":%q,"error_code":""}`,
			total, used, checkedAt.Format(time.RFC3339), checkedAt.Format(time.RFC3339))
		_, err := client.QuotaKey.UpdateOneID(qk.ID).SetUsageSnapshot([]byte(snap)).Save(ctx)
		require.NoError(t, err)
		// 该码 0 个账号：total=0 < 2，若未被耗尽门拦下本应入选（用于对照）。
		return qk
	}

	// remaining=0（total=10, used=10，新鲜）→ 跳过。
	qkZero := makeKeyWithSnapshot(10, 10, now)
	cands, err := w.ListCandidateKeys(ctx, "cursor-gate-0", 0)
	require.NoError(t, err)
	require.NotContains(t, cands, qkZero.ID, "新鲜 remaining=0 → 跳过")

	// remaining=100（total=200, used=100，新鲜）→ 不跳过（入选）。
	qk100 := makeKeyWithSnapshot(200, 100, now)
	cands, err = w.ListCandidateKeys(ctx, "cursor-gate-100", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qk100.ID, "新鲜 remaining=100 → 不跳过（入选）")

	// checked_at 超过 5 分钟（remaining=0 且陈旧）→ 跳过（沿用最近成功结论：曾确认耗尽，继续阻止补池）。
	qkStale := makeKeyWithSnapshot(10, 10, now.Add(-10*time.Minute))
	cands, err = w.ListCandidateKeys(ctx, "cursor-gate-stale", 0)
	require.NoError(t, err)
	require.NotContains(t, cands, qkStale.ID, "checked_at 陈旧且 remaining=0 → 跳过（沿用最近成功结论）")

	// checked_at 超过 5 分钟（remaining=100 且陈旧）→ 不跳过（入选，曾确认健康则放行）。
	qkStaleHealthy := makeKeyWithSnapshot(200, 100, now.Add(-10*time.Minute))
	cands, err = w.ListCandidateKeys(ctx, "cursor-gate-stale-healthy", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qkStaleHealthy.ID, "checked_at 陈旧且 remaining=100 → 不跳过（曾确认健康）")
}

// TestPull_AutoPathTotalGateZeroInsert（c1p1-A 要求 4）：
// 自动路径（manual=false）以「非软删总行数 total」为基准。total=2、working=1（一个停调）、
// 上游返回 1 个号 → maxInsert = target(2) - total(2) = 0 → 零插入。
func TestPull_AutoPathTotalGateZeroInsert(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "auto-overflow", Platform: "cursor", Type: "oauth", Credentials: map[string]any{}},
		}},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, 2)
	// 其中一个停调：working(usable)=1，但 total（非软删）=2。
	_, err := client.Account.Update().
		Where(account.QuotaKeyID(qk.ID), account.NameEQ(fmt.Sprintf("acc-%d-0", qk.ID))).
		SetTempUnschedulableUntil(time.Now().Add(30 * time.Minute)).
		Save(ctx)
	require.NoError(t, err)

	// 自动路径（PullAccounts → manual=false）。
	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n, "自动路径 total=2 已达目标：上游的 1 个号必须被零插入丢弃")
}

// TestPull_ManualPathUnaffectedByTotalGate（c1p1-A 要求 5）：
// 人工路径（RefreshAccounts，manual=true）仍以 working 为基准，不受 total 闸影响。
// 同一 key 状态：total=2、working=1、上游返回 1 个号 → 人工路径 base=working=1 →
// maxInsert = target(2) - 1 = 1 → 插入 1 个（总数变 3）。若误用 total 闸则零插入。
func TestPull_ManualPathUnaffectedByTotalGate(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 手动入口走挂载语义，需一个覆盖实供模型的 composite 聚合组（与既有 RefreshAccounts 用例同型）。
	grp := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"claude-sonnet-5-5"},
	})
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(grp.ID)).Exec(ctx)
		_, _ = client.Group.Delete().Where(entgroup.IDEQ(grp.ID)).Exec(ctx)
	})

	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "manual-1", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"k": "v"}},
		}},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, qk.ID, 2)
	_, err := client.Account.Update().
		Where(account.QuotaKeyID(qk.ID), account.NameEQ(fmt.Sprintf("acc-%d-0", qk.ID))).
		SetTempUnschedulableUntil(time.Now().Add(30 * time.Minute)).
		Save(ctx)
	require.NoError(t, err)

	// 人工路径（RefreshAccounts → manual=true）。
	require.NoError(t, svc.RefreshAccounts(ctx, qk.ID))

	n, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, n, "人工路径以 working=1 为基准：total=2 不应阻止，应插入 1 个（总数 3）")
}

// TestCURF1_CandidateKeys_NonCooldownNotReinstatedByTime（6，非冷却错误不得因时间被当冷却）：
// 带非冷却 last_refresh_error 的 key，即便 last_refresh_at 很旧，也不产生限频豁免
// （仍按 exhausted 占位排除）。判据是错误分类而非时间。
func TestCURF1_CandidateKeys_NonCooldownNotReinstatedByTime(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	w := NewQuotaKeyModelWater(client, nil)

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 非冷却错误（未知结果）且已置 exhausted=true（本用例直接在 DB 构造该既有态：例如插入
	// 失败标记），时间很旧。
	nonCooldownErr := "shareai: upstream code=1 unknown result: boom"
	_, err := client.QuotaKey.UpdateOneID(qk.ID).
		SetExhausted(true).
		SetLastRefreshError(nonCooldownErr).
		SetLastRefreshAt(time.Now().Add(-1 * time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	// 单元断言：非冷却错误字符串绝不被识别为冷却（判据是分类，不是时间）。
	require.False(t, isCooldownRefreshError(&nonCooldownErr),
		"non-cooldown error must not be classified as cooldown")

	// 集成断言：即便时间久远，也不得因"冷却窗口"重新入选（仍按 exhausted 占位排除）。
	cands, err := w.ListCandidateKeys(ctx, "cursor-f1-noncool", 0)
	require.NoError(t, err)
	require.NotContains(t, cands, qk.ID,
		"non-cooldown error must NOT be reinstated by elapsed time (stays excluded via exhausted)")
}

// TestCURF1_UpstreamExpired_MarksExpiredNoDateForged（7，上游确认过期 → 状态与日期）：
// 过期分支后 key status='expired'；且断言没有从 msg 推算或伪造任何日期
// （expires_at 保持原值，本测试初始为 nil）。
func TestCURF1_UpstreamExpired_MarksExpiredNoDateForged(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 上游确认过期：哨兵包中文 msg 原文（与 classifyUpstreamError 产出形态一致）。
	upErr := fmt.Errorf("%w: 密钥额度耗尽或已过期", ErrUpstreamKeyExpired)
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{err: upErr})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	require.Nil(t, qk.ExpiresAt, "test precondition: expires_at starts nil")

	err := svc.PullAccountsForModel(ctx, qk.ID, "cursor-f1-exp")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrUpstreamKeyExpired),
		"expired upstream must surface ErrUpstreamKeyExpired sentinel")

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusExpired, got.Status,
		"key must be marked expired after upstream confirms expiry")
	// 关键约束：绝不得据 msg 反推或伪造任何日期。
	require.Nil(t, got.ExpiresAt,
		"expires_at must NOT be written from/derived off the upstream msg (no forged date)")
	require.NotNil(t, got.LastRefreshError)
	require.Contains(t, *got.LastRefreshError, "密钥额度耗尽或已过期",
		"last_refresh_error must preserve the original msg text")
}

// TestQuotaKeyPull_OnlyConfirmedExpiredStopsSupply：停供只认已确认 key 级终态
// （CUR-P1-FIX-KEY-EXHAUSTED，方案 §15.2/§15.4/§16.1）：
//   - 明确 ErrUpstreamKeyExpired ⇒ status 落 expired（不被回退为 exhausted=true 占位）、
//     该 key 退出候选集（真正停供）；
//   - 未知/普通上游失败 ⇒ status 保持 active、exhausted 保持 false、仍在候选集（不停供）。
func TestQuotaKeyPull_OnlyConfirmedExpiredStopsSupply(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	w := NewQuotaKeyModelWater(client, nil)

	// ① 明确过期：落 expired ⇒ 停供（退出候选）。
	expSvc, _ := qkNewTestService(t, &fakeUpstreamClient{
		err: fmt.Errorf("%w: 密钥额度耗尽或已过期", ErrUpstreamKeyExpired),
	})
	qkExp := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	pre, err := w.ListCandidateKeys(ctx, "cursor-only-expired-stops", 0)
	require.NoError(t, err)
	require.Contains(t, pre, qkExp.ID, "fresh active key must be a candidate before the confirmed expiry")

	require.Error(t, expSvc.PullAccountsForModel(ctx, qkExp.ID, "cursor-only-expired-stops"))
	gotExp, err := client.QuotaKey.Get(ctx, qkExp.ID)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusExpired, gotExp.Status,
		"confirmed upstream expiry must land status=expired")
	require.False(t, gotExp.Exhausted,
		"confirmed expiry must NOT be degraded into the exhausted=true placeholder")
	post, err := w.ListCandidateKeys(ctx, "cursor-only-expired-stops", 0)
	require.NoError(t, err)
	require.NotContains(t, post, qkExp.ID, "expired key must stop supplying (exit candidates)")

	// ② 未知失败：不停供（仍在候选），且不得被标 exhausted。
	unkSvc, _ := qkNewTestService(t, &fakeUpstreamClient{err: NewUpstreamUnknownError("upstream code=1 unknown")})
	qkUnk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	require.Error(t, unkSvc.PullAccountsForModel(ctx, qkUnk.ID, "cursor-only-expired-stops"))
	gotUnk, err := client.QuotaKey.Get(ctx, qkUnk.ID)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusActive, gotUnk.Status,
		"unknown upstream failure must NOT stop supply (status stays active)")
	require.False(t, gotUnk.Exhausted, "unknown upstream failure must NOT be marked exhausted")
	require.NotNil(t, gotUnk.LastRefreshError, "unknown failure must still record the marker")
	post2, err := w.ListCandidateKeys(ctx, "cursor-only-expired-stops", 0)
	require.NoError(t, err)
	require.Contains(t, post2, qkUnk.ID, "unknown failure must NOT remove the key from candidates")
}

// TestCURF1_CredentialEncryptionOnPull（8，凭证加密）：
// 拉号落库后 accounts.credentials 中的 access_token/refresh_token 带 enc:v1: 前缀且不等于明文，
// 且 credentials_mac 非空。需在测试内启用 credcrypt 主密钥以激活加密写路径，结束后复原 passthrough。
func TestCURF1_CredentialEncryptionOnPull(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 启用 credcrypt 主密钥（测试专属），激活加密写路径；defer 复原未配置态（passthrough）。
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = 0x5a
	}
	keyB64 := base64.StdEncoding.EncodeToString(raw)
	_, err := credcrypt.Configure(keyB64, "")
	require.NoError(t, err)
	defer credcrypt.Configure("", "")

	modelM := fix1UniqueModel("cursor-f1-enc")
	// U7（§20.2-A）：挂载按账号模型能力（静态实供清单）判定——用实供模型 + composite
	// 聚合组保证挂载成功（本用例只断言加密与落库，模型名本身与断言无关）。
	encGrp, err := client.Group.Create().
		SetName(fmt.Sprintf("c11-f1-enc-%d", time.Now().UnixNano())).
		SetPlatform(domain.PlatformComposite).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-sol"}}).
		Save(ctx)
	require.NoError(t, err)
	// R10-1（CUR-F13）：复原本用例自建分组，避免其残留共享库污染他用例。
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(encGrp.ID)).Exec(cctx)
		_, _ = client.Group.Delete().Where(entgroup.IDEQ(encGrp.ID)).Exec(cctx)
	})

	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{
				Name:     "f1-enc@x.com",
				Platform: "cursor",
				Type:     "oauth",
				Credentials: map[string]any{
					"mail":          "f1-enc@x.com",
					"access_token":  "PLAIN_ACCESS_TOKEN",
					"refresh_token": "PLAIN_REFRESH_TOKEN",
				},
			},
		}},
	})

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	err = svc.PullAccountsForModel(ctx, qk.ID, modelM)
	require.NoError(t, err, "pull must succeed and persist the account")

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1, "exactly one account must be persisted")

	creds := accs[0].Credentials
	at, _ := creds["access_token"].(string)
	rt, _ := creds["refresh_token"].(string)
	require.True(t, strings.HasPrefix(at, credcrypt.Prefix),
		"access_token must be encrypted with enc:v1: prefix, got %q", at)
	require.NotEqual(t, "PLAIN_ACCESS_TOKEN", at, "access_token must not be stored as plaintext")
	require.True(t, strings.HasPrefix(rt, credcrypt.Prefix),
		"refresh_token must be encrypted with enc:v1: prefix, got %q", rt)
	require.NotEqual(t, "PLAIN_REFRESH_TOKEN", rt, "refresh_token must not be stored as plaintext")
	require.NotEmpty(t, accs[0].CredentialsMAC, "credentials_mac must be non-empty")
}

// ---------------------------------------------------------------------------
// W2-B1 / §25.14-C「换码原子边界」：RotateCredentials 与 Update(code) 归入唯一凭据写方法，
// 同一每 key 锁 + 同一事务内按新旧 MAC 决定是否清空 usage_snapshot。
// （以上用例走内存替身仓储，验证"决策 + 写入"语义；真实事务/锁语义见 DB 用例。）
// ---------------------------------------------------------------------------

// mustSeedSnapshotKey 构造一个带已提交快照的激活码行（旧来源观测：total=400/used=86）。
func mustSeedSnapshotKey(repo *fakeQuotaKeyRepo, id int64, code string) {
	repo.byID[id] = &ent.QuotaKey{
		ID:            id,
		Status:        domain.QuotaKeyStatusActive,
		CodeEncrypted: "enc:" + code,
		CodeMAC:       quotaKeyMACOfPlaintext(code),
		UsageSnapshot: []byte(`{"total":400,"used":86,"status_code":200,` +
			`"checked_at":"2026-10-01T08:00:00Z","attempted_at":"2026-10-01T08:00:00Z","error_code":""}`),
	}
}

// TestQuotaKeyRotateCredentials_ChangedSourceClearsSnapshot：换了一串 code ⇒ 同事务清空
// usage_snapshot（旧来源观测对新来源无效）。
func TestQuotaKeyRotateCredentials_ChangedSourceClearsSnapshot(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	repo := newFakeQuotaKeyRepo()
	mustSeedSnapshotKey(repo, 1, "JG-OLD-CODE")
	svc := NewQuotaKeyService(repo, testEncryptor{}, nil, nil)

	_, err := svc.RotateCredentials(context.Background(), 1, "JG-NEW-CODE")
	require.NoError(t, err)

	got := repo.byID[1]
	require.Equal(t, 1, repo.clearSnapshotCalls, "异 MAC 必须清一次快照")
	require.Nil(t, got.UsageSnapshot, "换码后旧来源快照必须被原子清空")
	require.Equal(t, quotaKeyMACOfPlaintext("JG-NEW-CODE"), got.CodeMAC)
	require.Equal(t, "CODE", got.CodeLast4)
	require.Equal(t, "enc:JG-NEW-CODE", got.CodeEncrypted)
}

// TestQuotaKeyRotateCredentials_SameSourceKeepsSnapshot：同一串 code 重新加密 ⇒ 只重加密，
// 不清已有观测（来源身份没变，观测仍然成立）。
func TestQuotaKeyRotateCredentials_SameSourceKeepsSnapshot(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	repo := newFakeQuotaKeyRepo()
	mustSeedSnapshotKey(repo, 1, "JG-SAME-CODE")
	before := []byte(repo.byID[1].UsageSnapshot)
	svc := NewQuotaKeyService(repo, testEncryptor{}, nil, nil)

	_, err := svc.RotateCredentials(context.Background(), 1, "JG-SAME-CODE")
	require.NoError(t, err)

	require.Zero(t, repo.clearSnapshotCalls, "同 MAC 不得清空快照")
	require.Equal(t, before, []byte(repo.byID[1].UsageSnapshot), "同源重加密必须保留已有观测")
}

// TestQuotaKeyUpdateCode_UsesSameAtomicCredentialWrite：admin Update(code) 与
// RotateCredentials 走同一实现（异 MAC 同样清快照、同 MAC 同样保留）。
func TestQuotaKeyUpdateCode_UsesSameAtomicCredentialWrite(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	repo := newFakeQuotaKeyRepo()
	mustSeedSnapshotKey(repo, 7, "JG-OLD-CODE")
	svc := NewQuotaKeyService(repo, testEncryptor{}, nil, nil)

	newCode := "JG-NEW-CODE"
	_, err := svc.Update(context.Background(), 7, &newCode, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, repo.clearSnapshotCalls, "Update(code) 必须与 RotateCredentials 同一边界")
	require.Nil(t, repo.byID[7].UsageSnapshot)

	// 同一 code 再提交一次：不清快照。
	mustSeedSnapshotKey(repo, 7, "JG-NEW-CODE")
	before := []byte(repo.byID[7].UsageSnapshot)
	_, err = svc.Update(context.Background(), 7, &newCode, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, repo.clearSnapshotCalls, "同 MAC 不得再清快照")
	require.Equal(t, before, []byte(repo.byID[7].UsageSnapshot))
}

// TestQuotaKeyUpdateMeta_DoesNotTouchSnapshot：仅编辑元数据不得触碰凭据或观测。
func TestQuotaKeyUpdateMeta_DoesNotTouchSnapshot(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	repo := newFakeQuotaKeyRepo()
	mustSeedSnapshotKey(repo, 9, "JG-CODE")
	before := []byte(repo.byID[9].UsageSnapshot)
	svc := NewQuotaKeyService(repo, testEncryptor{}, nil, nil)

	expires := time.Date(2030, 5, 1, 0, 0, 0, 0, time.UTC)
	expPtr := &expires
	_, err := svc.Update(context.Background(), 9, nil, &expPtr, nil, nil)
	require.NoError(t, err)
	require.Zero(t, repo.clearSnapshotCalls)
	require.Zero(t, repo.updateCredsCalls, "无 code 时不得写凭据")
	require.Equal(t, before, []byte(repo.byID[9].UsageSnapshot), "元数据编辑不得覆盖观测")
}

// TestQuotaKeyRotateCredentials_MissingKeyIsExplicit：来源不存在 ⇒ 明确错误，不复活来源。
func TestQuotaKeyRotateCredentials_MissingKeyIsExplicit(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	repo := newFakeQuotaKeyRepo()
	svc := NewQuotaKeyService(repo, testEncryptor{}, nil, nil)
	_, err := svc.RotateCredentials(context.Background(), 404, "JG-X")
	require.Error(t, err)
	require.Zero(t, repo.updateCredsCalls, "不存在的来源不得写入凭据")
}

// compile-time guard：内存替身仓储满足观测/换码所需的事务内能力（否则观测会失败关闭）。
var _ quotaKeyObservationRepo = (*fakeQuotaKeyRepo)(nil)
var _ quotaKeyObservationRepo = (*testDBRepo)(nil)

// TestQuotaKeyGetStatus_CarriesNotes 验证状态映射把管理员备注带出来：
// 1) 实体 Notes 非空 → dto.Notes 非 nil 且逐字相等；
// 2) 实体 Notes 为 nil → dto.Notes 为 nil（不得落成空串）；
// 3) 备注内容不得串入 CodeValueMasked。
func TestQuotaKeyGetStatus_CarriesNotes(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	repo := newFakeQuotaKeyRepo()
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)

	// 场景一：实体 Notes 非空 → 非 nil 且逐字相等，且不串入 code_masked。
	notes := "周卡（用户于 2026-10-20 提供）"
	repo.byID[1] = &ent.QuotaKey{
		ID:            1,
		CodeEncrypted: "enc:v1:someciphertext",
		CodeLast4:     "1234",
		Status:        "active",
		Notes:         &notes,
	}
	dto, err := svc.GetStatus(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if dto.Notes == nil {
		t.Fatalf("expected dto.Notes to be non-nil, got nil")
	}
	if *dto.Notes != notes {
		t.Fatalf("unexpected notes: got %q, want %q", *dto.Notes, notes)
	}
	if strings.Contains(dto.CodeValueMasked, notes) {
		t.Fatalf("notes leaked into code_masked: %q", dto.CodeValueMasked)
	}

	// 场景二：实体 Notes 为 nil → dto.Notes 必须为 nil。
	repo.byID[2] = &ent.QuotaKey{
		ID:            2,
		CodeEncrypted: "enc:v1:otherciphertext",
		CodeLast4:     "abcd",
		Status:        "active",
	}
	dto, err = svc.GetStatus(context.Background(), 2)
	if err != nil {
		t.Fatalf("GetStatus returned error: %v", err)
	}
	if dto.Notes != nil {
		t.Fatalf("expected dto.Notes to be nil, got %q", *dto.Notes)
	}
}

// TestQuotaKeyToStatus_UpstreamKey_KC18B 验证 service 层唯一投影 quotaKeyToStatus 把
// ent.QuotaKey.UpstreamKey 透传到 QuotaKeyStatus.UpstreamKey（KC-18B 后端回填缺口）。
// 所有对外构造点（List/Create/Update/GetStatus/GetWithAccounts/RotateCredentials）均经此函数，
// 故此处一处断言即覆盖全部构造路径。
func TestQuotaKeyToStatus_UpstreamKey_KC18B(t *testing.T) {
	qk := &ent.QuotaKey{
		ID:          42,
		Status:      "active",
		CodeLast4:   "1234",
		Priority:    1,
		UpstreamKey: "shareai",
	}
	st := quotaKeyToStatus(qk)
	require.Equal(t, "shareai", st.UpstreamKey)
}

// TestQuotaKeyPull_KnCyModelMappingRespectsUpstreamSupply 验证 KC-26 问题 1：kn-cy 账号落库的
// model_mapping 恰为上游自报的 3 个模型恒等映射，不含 ShareAI 静态清单里的其它 13 个
// （Done when #1）。
func TestQuotaKeyPull_KnCyModelMappingRespectsUpstreamSupply(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	up := &knCySupplyFake{
		fakeUpstreamClient: fakeUpstreamClient{
			batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
				{Name: "kncy-pulled", Platform: "cursor", Type: "apikey", Credentials: map[string]any{"api_key": "s", "base_url": "x"}},
			}},
		},
	}
	svc, _ := qkNewTestService(t, up)
	// 生产装配 autoEntry 才写 entryModelMapping；测试注入默认配置以触发落库映射写入。
	svc.SetAutoEntry(&fakeAutoEntryConfigProvider{cfg: DefaultAutoEntryConfig()})

	qk := mustCreateQuotaKeyWithUpstream(t, client, domain.QuotaKeyStatusActive, upstreamKeyKnCy)
	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	accts, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accts, 1, "kn-cy 1 卡 1 号：应只落 1 个账号")

	creds := cursorDecryptStoredCredentials(t, accts[0].Credentials)
	mm, ok := creds["model_mapping"].(map[string]any)
	require.True(t, ok, "model_mapping 必须以明文 map 落库")
	require.Len(t, mm, 3, "kn-cy 映射必须恰好 3 个模型")
	for _, m := range knCySupplyModels {
		require.Equal(t, m, mm[m], "model_mapping[%s] 必须恒等映射", m)
		// 反向校验：不含 ShareAI 静态清单里的其它模型。
	}
	_, has := mm["claude-opus-5-5"]
	require.False(t, has, "kn-cy 映射不得包含 shareai-only 模型 claude-opus-5-5")
	_, has = mm["grok-4.6"]
	require.False(t, has, "kn-cy 映射不得包含 shareai-only 模型 grok-4.6")
}

// TestQuotaKeyModelWater_KnCyCandidateStopsAtOne 验证 KC-26 问题 2：kn-cy 卡已有 1 个可用账号后，
// 自动补池不再把它列为候选（每码目标 1）；而 shareai 卡（目标 5）仍应列为候选（Done when #3 / #4）。
func TestQuotaKeyModelWater_KnCyCandidateStopsAtOne(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	w := &quotaKeyModelWater{
		client:   client,
		upstream: &knCySupplyFake{},
	}

	// kn-cy 卡：1 个可用账号，目标 1 → 不应再补。
	knCyKey := mustCreateQuotaKeyWithUpstream(t, client, domain.QuotaKeyStatusActive, upstreamKeyKnCy)
	mustCreateAccounts(t, client, knCyKey.ID, 1)

	// shareai 卡：1 个可用账号，目标 5 → 仍应补。
	shareKey := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	mustCreateAccounts(t, client, shareKey.ID, 1)

	cands, err := w.ListCandidateKeys(ctx, "", 0)
	require.NoError(t, err)
	require.NotContains(t, cands, knCyKey.ID, "kn-cy 卡已有 1 个账号，不得再入选候选")
	require.Contains(t, cands, shareKey.ID, "shareai 卡仅有 1 个账号（目标 5），仍应入选候选")
}

// ---------------------------------------------------------------------------
// QKG-B2c：SetStatus（B6 停用开关）与 kn_cy 换码同步账号密钥（B7）。
// 以下用例命中真实 Postgres（qkNewTestService），走 withQuotaKeyLock 真实事务。
// ---------------------------------------------------------------------------

// mustCreateQuotaKeyWithExpiry 创建指定状态 / 上游 / 到期时间的激活码；expiresAt 为 nil
// 时不设置（沿用列默认，NULL）。不含 code_mac，避免撞 uq_quota_keys_code_mac 唯一索引。
func mustCreateQuotaKeyWithExpiry(t *testing.T, client *ent.Client, status, upstreamKey string, expiresAt *time.Time) *ent.QuotaKey {
	t.Helper()
	q := client.QuotaKey.Create().SetCodeEncrypted("enc:v1:seed").SetStatus(status)
	if upstreamKey != "" {
		q = q.SetUpstreamKey(upstreamKey)
	}
	if expiresAt != nil {
		q = q.SetNillableExpiresAt(expiresAt)
	}
	qk, err := q.Save(context.Background())
	require.NoError(t, err)
	return qk
}

// mustCreateAPIKeyAccount 在指定激活码下创建一条 cursor apikey 账号（B7 同步目标）。
// creds 直接落库（不走 prepareCursorAccountCredentials，故可为明文），credMAC /
// apiKeyMAC 为入参给定值，供断言"刷新后不等于入参"。
func mustCreateAPIKeyAccount(t *testing.T, client *ent.Client, quotaKeyID int64, creds map[string]any, credMAC, apiKeyMAC string) *ent.Account {
	t.Helper()
	b := client.Account.Create().
		SetName(fmt.Sprintf("apikey-acc-%d-%d", quotaKeyID, time.Now().UnixNano())).
		SetPlatform(domain.PlatformCursor).
		SetType(AccountTypeAPIKey).
		SetCredentials(creds).
		SetQuotaKeyID(quotaKeyID)
	if credMAC != "" {
		b = b.SetCredentialsMAC(credMAC)
	}
	if apiKeyMAC != "" {
		b = b.SetCredentialsAPIKeyMAC(apiKeyMAC)
	}
	acc, err := b.Save(context.Background())
	require.NoError(t, err)
	return acc
}

// quotaKeyTestCleanup 软删本测试创建的激活码及其名下账号，释放 uq_quota_keys_code_mac
// 唯一坑位（测试库跨 go test 调用持久化，不复原会污染后续重复运行，与已知基线红 sk-b2a-3
// 同类，但本卡用例必须可重复运行）。
func quotaKeyTestCleanup(t *testing.T, client *ent.Client, qkIDs ...int64) {
	t.Helper()
	ctx := context.Background()
	for _, id := range qkIDs {
		_ = client.Account.Update().Where(account.QuotaKeyID(id)).SetDeletedAt(time.Now()).Exec(ctx)
		_ = client.QuotaKey.UpdateOneID(id).SetDeletedAt(time.Now()).Exec(ctx)
	}
}

// appErrorReason 从 *infraerrors.ApplicationError 取出 reason 码。
func appErrorReason(t *testing.T, err error) string {
	t.Helper()
	var ae *infraerrors.ApplicationError
	require.ErrorAs(t, err, &ae)
	return ae.Reason
}

// TestSetStatus_ActiveToDisabled_Success：active → disabled 成功，落库状态正确。
func TestSetStatus_ActiveToDisabled_Success(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	svc, client := qkNewTestService(t, nil)
	qk := mustCreateQuotaKeyWithExpiry(t, client, domain.QuotaKeyStatusActive, "", nil)
	t.Cleanup(func() { quotaKeyTestCleanup(t, client, qk.ID) })

	st, err := svc.SetStatus(context.Background(), qk.ID, domain.QuotaKeyStatusDisabled)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusDisabled, st.Status)
	reloaded, rerr := client.QuotaKey.Get(context.Background(), qk.ID)
	require.NoError(t, rerr)
	require.Equal(t, domain.QuotaKeyStatusDisabled, reloaded.Status)
}

// TestSetStatus_DisabledToActive_Success：disabled → active 成功（到期在未来，不触发 R3a 保护）。
func TestSetStatus_DisabledToActive_Success(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	svc, client := qkNewTestService(t, nil)
	future := time.Now().Add(24 * time.Hour)
	qk := mustCreateQuotaKeyWithExpiry(t, client, domain.QuotaKeyStatusDisabled, "", &future)
	t.Cleanup(func() { quotaKeyTestCleanup(t, client, qk.ID) })

	st, err := svc.SetStatus(context.Background(), qk.ID, domain.QuotaKeyStatusActive)
	require.NoError(t, err)
	require.Equal(t, domain.QuotaKeyStatusActive, st.Status)
}

// TestSetStatus_ExpiredKeyToActive_Rejected：status=expired（expires_at 已过期）→ active 400
// QUOTA_KEY_EXPIRED_CANNOT_ENABLE（外审 R3a：只看 expires_at 是否早于 now）。
func TestSetStatus_ExpiredKeyToActive_Rejected(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	svc, client := qkNewTestService(t, nil)
	past := time.Now().Add(-24 * time.Hour)
	qk := mustCreateQuotaKeyWithExpiry(t, client, domain.QuotaKeyStatusExpired, "", &past)
	t.Cleanup(func() { quotaKeyTestCleanup(t, client, qk.ID) })

	_, err := svc.SetStatus(context.Background(), qk.ID, domain.QuotaKeyStatusActive)
	require.Error(t, err)
	require.Equal(t, "QUOTA_KEY_EXPIRED_CANNOT_ENABLE", appErrorReason(t, err))
}

// TestSetStatus_ActiveExpiredCodeToActive_Rejected：仍 active 但 expires_at 已过期 → active 400
// QUOTA_KEY_EXPIRED_CANNOT_ENABLE（保护判的是真实到期，与 status 字段无关）。
func TestSetStatus_ActiveExpiredCodeToActive_Rejected(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	svc, client := qkNewTestService(t, nil)
	past := time.Now().Add(-1 * time.Hour)
	qk := mustCreateQuotaKeyWithExpiry(t, client, domain.QuotaKeyStatusActive, "", &past)
	t.Cleanup(func() { quotaKeyTestCleanup(t, client, qk.ID) })

	_, err := svc.SetStatus(context.Background(), qk.ID, domain.QuotaKeyStatusActive)
	require.Error(t, err)
	require.Equal(t, "QUOTA_KEY_EXPIRED_CANNOT_ENABLE", appErrorReason(t, err))
}

// TestSetStatus_InvalidStatus_Rejected：非法状态 → 400 QUOTA_KEY_STATUS_INVALID。
func TestSetStatus_InvalidStatus_Rejected(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	svc, client := qkNewTestService(t, nil)
	qk := mustCreateQuotaKeyWithExpiry(t, client, domain.QuotaKeyStatusActive, "", nil)
	t.Cleanup(func() { quotaKeyTestCleanup(t, client, qk.ID) })

	_, err := svc.SetStatus(context.Background(), qk.ID, "frobnicate")
	require.Error(t, err)
	require.Equal(t, "QUOTA_KEY_STATUS_INVALID", appErrorReason(t, err))
}

// TestSetStatus_NotFound：不存在的 id → ErrQuotaKeyNotFound。
func TestSetStatus_NotFound(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	svc, _ := qkNewTestService(t, nil)
	_, err := svc.SetStatus(context.Background(), 987654321, domain.QuotaKeyStatusDisabled)
	require.ErrorIs(t, err, ErrQuotaKeyNotFound)
}

// TestKnCyRotateCredentials_SyncsAccountAPIKey：kn_cy 码换新卡密 → 名下 apikey 账号
// credentials.api_key 变为新值，credentials_mac / credentials_api_key_mac 已刷新，且提交后
// 入队 account_changed。
func TestKnCyRotateCredentials_SyncsAccountAPIKey(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	client := quotaKeyTestEntClient(t)
	_, err := credcrypt.Configure(quotaKeyTestCredcryptKeyB64, "")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = credcrypt.Configure("", "") })
	repo := &testDBRepo{client: client}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, client)

	qk := mustCreateQuotaKeyWithUpstream(t, client, domain.QuotaKeyStatusActive, upstreamKeyKnCy)
	t.Cleanup(func() { quotaKeyTestCleanup(t, client, qk.ID) })
	acc := mustCreateAPIKeyAccount(t, client, qk.ID, map[string]any{
		"api_key":       "old-kncy-key",
		"refresh_token": "old-rt",
	}, "before-cred-mac", "before-akey-mac")

	newPlain := fmt.Sprintf("new-kncy-card-%d", time.Now().UnixNano())
	_, err = svc.RotateCredentials(context.Background(), qk.ID, newPlain)
	require.NoError(t, err)

	reloaded, rerr := client.Account.Get(context.Background(), acc.ID)
	require.NoError(t, rerr)

	// credentials.api_key 变为新值（存储为密文，解密后比对）。
	storedAPIKey, ok := reloaded.Credentials["api_key"].(string)
	require.True(t, ok, "api_key 子键应存在")
	decrypted, derr := credcrypt.Decrypt(storedAPIKey)
	require.NoError(t, derr)
	require.Equal(t, newPlain, decrypted, "kn_cy 换码后账号 api_key 应为新卡密")

	// credentials_mac / credentials_api_key_mac 已刷新。
	require.NotEqual(t, "before-cred-mac", reloaded.CredentialsMAC, "credentials_mac 应刷新")
	require.Equal(t, credentialsHMACOfJSON([]byte(newPlain)), reloaded.CredentialsAPIKeyMAC, "credentials_api_key_mac 应按新 api_key 重算")

	// 提交后入队 account_changed（与 SetStatus 同型）。
	found := false
	for _, call := range repo.outboxCalls {
		for _, id := range call {
			if id == acc.ID {
				found = true
			}
		}
	}
	require.True(t, found, "kn_cy 换码应入队 account_changed outbox")
}

// TestKnCyRotateCredentials_ShareAIAccountAPIKeyUnchanged：shareai 码换码 → 账号凭证一字未变
// （断言语义相等：shareai 不触发 B7 同步），且不得入队无关账号。
func TestKnCyRotateCredentials_ShareAIAccountAPIKeyUnchanged(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	client := quotaKeyTestEntClient(t)
	_, err := credcrypt.Configure(quotaKeyTestCredcryptKeyB64, "")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = credcrypt.Configure("", "") })
	repo := &testDBRepo{client: client}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, client)

	// shareai 上行（默认）：不触发账号密钥同步。
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	t.Cleanup(func() { quotaKeyTestCleanup(t, client, qk.ID) })
	acc := mustCreateAPIKeyAccount(t, client, qk.ID, map[string]any{
		"api_key": "sk-shareai-keep",
	}, "keep-cred-mac", "keep-akey-mac")

	_, err = svc.RotateCredentials(context.Background(), qk.ID, fmt.Sprintf("new-shareai-code-%d", time.Now().UnixNano()))
	require.NoError(t, err)

	reloaded, rerr := client.Account.Get(context.Background(), acc.ID)
	require.NoError(t, rerr)
	require.Equal(t, "sk-shareai-keep", reloaded.Credentials["api_key"], "shareai 换码不得改动账号凭证")
	require.Equal(t, "keep-cred-mac", reloaded.CredentialsMAC, "shareai 换码不得改动 credentials_mac")
	require.Equal(t, "keep-akey-mac", reloaded.CredentialsAPIKeyMAC, "shareai 换码不得改动 credentials_api_key_mac")
	require.Empty(t, repo.outboxCalls, "shareai 换码不得入队 account_changed（无账号同步）")
}

// TestKnCyRotateCredentials_DuplicateCodeAPIKey：换码撞 uq_quota_keys_code_mac → 返回
// ErrQuotaKeyDuplicateCode（与 Create 同口径，由仓储层翻译）。
func TestKnCyRotateCredentials_DuplicateCodeAPIKey(t *testing.T) {
	preserveCursorQuotaKeyStateProvider(t)
	client := quotaKeyTestEntClient(t)
	_, err := credcrypt.Configure(quotaKeyTestCredcryptKeyB64, "")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = credcrypt.Configure("", "") })
	repo := &testDBRepo{client: client}
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, client)

	newPlain := fmt.Sprintf("dup-target-code-%d", time.Now().UnixNano())
	dupMAC := quotaKeyMACOfPlaintext(newPlain)
	// 已占用该 code_mac 的未软删激活码（任意上行），使 uq_quota_keys_code_mac 唯一冲突可被触发。
	occupied, oerr := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:occupied").
		SetCodeMAC(dupMAC).
		SetStatus(domain.QuotaKeyStatusActive).
		Save(context.Background())
	require.NoError(t, oerr)
	t.Cleanup(func() { quotaKeyTestCleanup(t, client, occupied.ID) })

	qk := mustCreateQuotaKeyWithUpstream(t, client, domain.QuotaKeyStatusActive, upstreamKeyKnCy)
	t.Cleanup(func() { quotaKeyTestCleanup(t, client, qk.ID) })
	_, err = svc.RotateCredentials(context.Background(), qk.ID, newPlain)
	require.ErrorIs(t, err, ErrQuotaKeyDuplicateCode)
}
