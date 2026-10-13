//go:build unit

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// ---- 测试替身 ----

// retentionOrderRecorder 跨 repo 记录调用顺序，用于断言“先删流水后删账号”。
type retentionOrderRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *retentionOrderRecorder) record(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// fakeCursorRetentionAccountRepo 是 CursorRetentionAccountRepository 的内存替身。
// 它按照 platform='cursor' + 已软删 + deleted_at<cutoff 的口径返回账号 ID，
// 并记录 PhysicallyDeleteAccounts 的调用。
type fakeCursorRetentionAccountRepo struct {
	mu        sync.Mutex
	accounts  []fakeRetentionAccount
	recorder  *retentionOrderRecorder
	deleted   [][]int64
	limitSeen int
	listCalls int
}

type fakeRetentionAccount struct {
	id        int64
	platform  string
	deletedAt *time.Time
}

func (f *fakeCursorRetentionAccountRepo) ListExpiredSoftDeletedCursorAccountIDs(ctx context.Context, cutoff time.Time, limit int) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitSeen = limit
	f.listCalls++
	var out []int64
	for _, a := range f.accounts {
		if a.platform == "cursor" && a.deletedAt != nil && a.deletedAt.Before(cutoff) {
			out = append(out, a.id)
			if len(out) >= limit {
				break
			}
		}
	}
	if f.recorder != nil {
		f.recorder.record("list")
	}
	return out, nil
}

func (f *fakeCursorRetentionAccountRepo) PhysicallyDeleteAccounts(ctx context.Context, ids []int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, append([]int64{}, ids...))
	if f.recorder != nil {
		f.recorder.record("delete")
	}
	// 模拟物理删除：从 store 中移除，确保下一轮 List 不再返回（贴近真实 DB 行为）。
	remove := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		remove[id] = struct{}{}
	}
	kept := f.accounts[:0]
	for _, a := range f.accounts {
		if _, ok := remove[a.id]; !ok {
			kept = append(kept, a)
		}
	}
	f.accounts = kept
	return int64(len(ids)), nil
}

// fakeRetentionUsageCleanupRepo 是 UsageCleanupRepository 的内存替身。
// 仅 DeleteUsageLogsBatch 有意义：记录每次调用（含 AccountID），并按 returnSeq 返回删除行数。
type fakeRetentionUsageCleanupRepo struct {
	mu        sync.Mutex
	recorder  *retentionOrderRecorder
	calls     []UsageCleanupFilters
	returnSeq []int64
	idx       int
}

func (f *fakeRetentionUsageCleanupRepo) CreateTask(ctx context.Context, task *UsageCleanupTask) error {
	return nil
}
func (f *fakeRetentionUsageCleanupRepo) ListTasks(ctx context.Context, params pagination.PaginationParams) ([]UsageCleanupTask, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (f *fakeRetentionUsageCleanupRepo) ClaimNextPendingTask(ctx context.Context, stale int64) (*UsageCleanupTask, error) {
	return nil, nil
}
func (f *fakeRetentionUsageCleanupRepo) GetTaskStatus(ctx context.Context, id int64) (string, error) {
	return "", nil
}
func (f *fakeRetentionUsageCleanupRepo) UpdateTaskProgress(ctx context.Context, id, rows int64) error {
	return nil
}
func (f *fakeRetentionUsageCleanupRepo) CancelTask(ctx context.Context, id, by int64) (bool, error) {
	return false, nil
}
func (f *fakeRetentionUsageCleanupRepo) MarkTaskSucceeded(ctx context.Context, id, rows int64) error {
	return nil
}
func (f *fakeRetentionUsageCleanupRepo) MarkTaskFailed(ctx context.Context, id, rows int64, msg string) error {
	return nil
}
func (f *fakeRetentionUsageCleanupRepo) DeleteUsageLogsBatch(ctx context.Context, filters UsageCleanupFilters, limit int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, filters)
	if f.recorder != nil {
		f.recorder.record("usagelog")
	}
	var ret int64
	if len(f.returnSeq) > 0 {
		ret = f.returnSeq[f.idx%len(f.returnSeq)]
		f.idx++
	}
	return ret, nil
}

// fakeRetentionSettingRepo 是 SettingRepository 的内存替身（仅 GetValue 有意义）。
type fakeRetentionSettingRepo struct {
	value string
}

func (f *fakeRetentionSettingRepo) Get(ctx context.Context, key string) (*Setting, error) {
	return &Setting{}, nil
}
func (f *fakeRetentionSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	return f.value, nil
}
func (f *fakeRetentionSettingRepo) Set(ctx context.Context, key, value string) error {
	f.value = value
	return nil
}
func (f *fakeRetentionSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	return nil, nil
}
func (f *fakeRetentionSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	return nil
}
func (f *fakeRetentionSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	return nil, nil
}
func (f *fakeRetentionSettingRepo) Delete(ctx context.Context, key string) error { return nil }

func newTestCursorRetentionService(acctRepo *fakeCursorRetentionAccountRepo, usageRepo *fakeRetentionUsageCleanupRepo, now time.Time, days string) *CursorAccountRetentionService {
	return &CursorAccountRetentionService{
		usageCleanupRepo: usageRepo,
		dbRepo:           acctRepo,
		settingService:   &SettingService{settingRepo: &fakeRetentionSettingRepo{value: days}},
		nowFunc:          func() time.Time { return now },
	}
}

func retentionInt64Ptr(v int64) *int64 { return &v }

func retentionTimeBefore(t time.Time, d time.Duration) *time.Time {
	v := t.Add(-d)
	return &v
}

// ---- 测试 ----

// 保留期内不删 / 超期才删 + 只清 cursor 平台。
func TestCursorAccountRetentionWithinPeriodAndPlatform(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	acctRepo := &fakeCursorRetentionAccountRepo{
		accounts: []fakeRetentionAccount{
			{id: 1, platform: "cursor", deletedAt: retentionTimeBefore(now, 80*24*time.Hour)},  // 80 天前软删，保留 90 天 → 不应删
			{id: 2, platform: "cursor", deletedAt: retentionTimeBefore(now, 100*24*time.Hour)}, // 100 天前软删，超期 → 应删
			{id: 3, platform: "openai", deletedAt: retentionTimeBefore(now, 100*24*time.Hour)}, // 其它平台 → 不应触碰
		},
	}
	usageRepo := &fakeRetentionUsageCleanupRepo{}
	svc := newTestCursorRetentionService(acctRepo, usageRepo, now, "90")

	svc.RunOnce(context.Background())

	if len(acctRepo.deleted) != 1 {
		t.Fatalf("期望仅删除 1 批账号，实际: %d 批 %v", len(acctRepo.deleted), acctRepo.deleted)
	}
	if len(acctRepo.deleted[0]) != 1 || acctRepo.deleted[0][0] != 2 {
		t.Fatalf("期望仅物理删除 cursor 超期账号 id=2，实际: %v", acctRepo.deleted)
	}
	// 流水删除只针对 id=2
	if len(usageRepo.calls) != 1 {
		t.Fatalf("期望对 id=2 调用一次 DeleteUsageLogsBatch，实际: %d 次", len(usageRepo.calls))
	}
	if usageRepo.calls[0].AccountID == nil || *usageRepo.calls[0].AccountID != 2 {
		t.Fatalf("期望 DeleteUsageLogsBatch 针对 account_id=2，实际 AccountID=%v", usageRepo.calls[0].AccountID)
	}
}

// 顺序正确：先删流水后删账号。
func TestCursorAccountRetentionOrderUsageLogsBeforeAccounts(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	rec := &retentionOrderRecorder{}
	acctRepo := &fakeCursorRetentionAccountRepo{
		recorder: rec,
		accounts: []fakeRetentionAccount{
			{id: 1, platform: "cursor", deletedAt: retentionTimeBefore(now, 100*24*time.Hour)},
			{id: 2, platform: "cursor", deletedAt: retentionTimeBefore(now, 100*24*time.Hour)},
		},
	}
	usageRepo := &fakeRetentionUsageCleanupRepo{recorder: rec}
	svc := newTestCursorRetentionService(acctRepo, usageRepo, now, "90")

	svc.RunOnce(context.Background())

	firstDelete := -1
	lastUsageLog := -1
	for i, e := range rec.events {
		if e == "delete" && firstDelete == -1 {
			firstDelete = i
		}
		if e == "usagelog" {
			lastUsageLog = i
		}
	}
	if firstDelete == -1 {
		t.Fatal("未发生账号物理删除")
	}
	if lastUsageLog == -1 {
		t.Fatal("未发生流水删除")
	}
	if lastUsageLog >= firstDelete {
		t.Fatalf("顺序错误：流水删除应全部早于账号删除；events=%v", rec.events)
	}
}

// 流水删除走的是既有原语（带聚合失效的路径），而非依赖 accounts 的 CASCADE。
func TestCursorAccountRetentionUsesExistingPrimitiveNotCascade(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	acctRepo := &fakeCursorRetentionAccountRepo{
		accounts: []fakeRetentionAccount{
			{id: 5, platform: "cursor", deletedAt: retentionTimeBefore(now, 200*24*time.Hour)},
			{id: 6, platform: "cursor", deletedAt: retentionTimeBefore(now, 200*24*time.Hour)},
		},
	}
	usageRepo := &fakeRetentionUsageCleanupRepo{}
	svc := newTestCursorRetentionService(acctRepo, usageRepo, now, "90")

	svc.RunOnce(context.Background())

	if len(usageRepo.calls) == 0 {
		t.Fatal("未调用 DeleteUsageLogsBatch 原语（既有的带聚合失效路径）")
	}
	for i, c := range usageRepo.calls {
		if c.AccountID == nil {
			t.Fatalf("第 %d 次 DeleteUsageLogsBatch 未带 AccountID（未走按账号筛选的原语路径）", i)
		}
	}
	// 若依赖 CASCADE，则根本不会调用 DeleteUsageLogsBatch，此处已证明显式走了原语。
}

// 单轮批量上限生效：达上限继续下一轮，不足上限结束。
func TestCursorAccountRetentionBatchSizeLimit(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	const total = cursorRetentionBatchSize*2 + 5
	accounts := make([]fakeRetentionAccount, 0, total)
	for i := int64(1); i <= total; i++ {
		accounts = append(accounts, fakeRetentionAccount{
			id:        i,
			platform:  "cursor",
			deletedAt: retentionTimeBefore(now, 100*24*time.Hour),
		})
	}
	acctRepo := &fakeCursorRetentionAccountRepo{accounts: accounts}
	usageRepo := &fakeRetentionUsageCleanupRepo{}
	svc := newTestCursorRetentionService(acctRepo, usageRepo, now, "90")

	svc.RunOnce(context.Background())

	if acctRepo.limitSeen != cursorRetentionBatchSize {
		t.Fatalf("列表查询未使用批量上限 cursorRetentionBatchSize=%d，实际 limit=%d", cursorRetentionBatchSize, acctRepo.limitSeen)
	}
	if len(acctRepo.deleted) != 3 {
		t.Fatalf("期望分 3 轮（满/满/余），实际 %d 轮: %v", len(acctRepo.deleted), acctRepo.deleted)
	}
	if len(acctRepo.deleted[0]) != cursorRetentionBatchSize || len(acctRepo.deleted[1]) != cursorRetentionBatchSize {
		t.Fatalf("前两批应为满批量，实际 %d / %d", len(acctRepo.deleted[0]), len(acctRepo.deleted[1]))
	}
	if len(acctRepo.deleted[2]) != 5 {
		t.Fatalf("最后一批应为剩余 5 条，实际 %d", len(acctRepo.deleted[2]))
	}
	var sum int64
	for _, b := range acctRepo.deleted {
		sum += int64(len(b))
	}
	if sum != total {
		t.Fatalf("删除总数应为 %d，实际 %d", total, sum)
	}
}

// 保留天数 <=0 视为永久保留，不删除任何数据。
func TestCursorAccountRetentionPermanentWhenZero(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	acctRepo := &fakeCursorRetentionAccountRepo{
		accounts: []fakeRetentionAccount{
			{id: 1, platform: "cursor", deletedAt: retentionTimeBefore(now, 100*24*time.Hour)},
		},
	}
	usageRepo := &fakeRetentionUsageCleanupRepo{}
	svc := newTestCursorRetentionService(acctRepo, usageRepo, now, "0")

	svc.RunOnce(context.Background())

	if acctRepo.listCalls != 0 {
		t.Fatalf("days<=0 时不应查询任何账号，实际 listCalls=%d", acctRepo.listCalls)
	}
	if len(acctRepo.deleted) != 0 {
		t.Fatalf("days<=0 时不应删除任何账号，实际 deleted=%v", acctRepo.deleted)
	}
}
