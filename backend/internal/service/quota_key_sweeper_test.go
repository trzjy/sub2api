//go:build unit

package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
)

// recordingQuotaKeyRepo 在 fakeQuotaKeyRepo 之上记录被调用的方法名，
// 用于断言清扫过程只触碰允许的仓储方法：ListExpired + 条件化标 expired +
// cursor 账号软删与 outbox 入口，不得调用其它含 "Account" 的方法。
type recordingQuotaKeyRepo struct {
	*fakeQuotaKeyRepo
	mu    sync.Mutex
	calls []string
}

func newRecordingQuotaKeyRepo() *recordingQuotaKeyRepo {
	return &recordingQuotaKeyRepo{fakeQuotaKeyRepo: newFakeQuotaKeyRepo()}
}

func (r *recordingQuotaKeyRepo) record(name string) {
	r.mu.Lock()
	r.calls = append(r.calls, name)
	r.mu.Unlock()
}

func (r *recordingQuotaKeyRepo) ListExpired(ctx context.Context, now time.Time) ([]*ent.QuotaKey, error) {
	r.record("ListExpired")
	return r.fakeQuotaKeyRepo.ListExpired(ctx, now)
}

func (r *recordingQuotaKeyRepo) UpdateStatusFields(ctx context.Context, tx *ent.Tx, id int64, status string, exhausted bool, lastError *string, lastRefreshAt *time.Time) error {
	r.record("UpdateStatusFields")
	return r.fakeQuotaKeyRepo.UpdateStatusFields(ctx, tx, id, status, exhausted, lastError, lastRefreshAt)
}

func (r *recordingQuotaKeyRepo) MarkExpiredIfStillExpired(ctx context.Context, tx *ent.Tx, id int64, now time.Time) (int, error) {
	r.record("MarkExpiredIfStillExpired")
	return r.fakeQuotaKeyRepo.MarkExpiredIfStillExpired(ctx, tx, id, now)
}

func (r *recordingQuotaKeyRepo) SoftDeleteCursorAccountsByExpiredQuotaKey(ctx context.Context, tx *ent.Tx, quotaKeyID int64, now time.Time) ([]int64, error) {
	r.record("SoftDeleteCursorAccountsByExpiredQuotaKey")
	return r.fakeQuotaKeyRepo.SoftDeleteCursorAccountsByExpiredQuotaKey(ctx, tx, quotaKeyID, now)
}

func (r *recordingQuotaKeyRepo) EnqueueAccountChangedOutbox(ctx context.Context, tx *ent.Tx, accountIDs []int64) error {
	r.record("EnqueueAccountChangedOutbox")
	return r.fakeQuotaKeyRepo.EnqueueAccountChangedOutbox(ctx, tx, accountIDs)
}

func (r *recordingQuotaKeyRepo) calledMethods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// heartbeatRecordingOpsRepo 在 opsRepoMock 之上记录心跳写入次数与 job 名，
// 用于断言 runOnce 每轮写且仅写一次心跳、且 job 名正确。
type heartbeatRecordingOpsRepo struct {
	*opsRepoMock
	mu      sync.Mutex
	calls   int
	jobName string
}

func (r *heartbeatRecordingOpsRepo) UpsertJobHeartbeat(_ context.Context, input *OpsUpsertJobHeartbeatInput) error {
	r.mu.Lock()
	r.calls++
	r.jobName = input.JobName
	r.mu.Unlock()
	return nil
}

func (r *heartbeatRecordingOpsRepo) heartbeatCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *heartbeatRecordingOpsRepo) lastJobName() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobName
}

// newSweepTestSvc 构造一个不启动后台循环的测试用清扫器（tx 由测试直接提供）。
// opsRepo/entClient 为 nil：扫库逻辑在 sweepOnce 中直接驱动，心跳仅在 runOnce 测试中装配。
func newSweepTestSvc(repo QuotaKeyRepository) *QuotaKeySweeper {
	return NewQuotaKeySweeper(repo, nil, nil)
}

// TestQuotaKeySweep_ExpiredMarksExpired 验证到期（expires_at < now）激活码被标 expired。
func TestQuotaKeySweep_ExpiredMarksExpired(t *testing.T) {
	repo := newRecordingQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{
		ID:        1,
		Status:    "active",
		ExpiresAt: timePtr(time.Now().Add(-time.Hour)),
	}

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	if err != nil {
		t.Fatalf("sweepOnce returned error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 marked, got %d", n)
	}
	if repo.byID[1].Status != "expired" {
		t.Fatalf("expected status expired, got %q", repo.byID[1].Status)
	}
}

// TestQuotaKeySweep_NotExpiredUntouched 验证未到期（expires_at > now）激活码不动。
func TestQuotaKeySweep_NotExpiredUntouched(t *testing.T) {
	repo := newRecordingQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{
		ID:        1,
		Status:    "active",
		ExpiresAt: timePtr(time.Now().Add(time.Hour)),
	}

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	if err != nil {
		t.Fatalf("sweepOnce returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 marked, got %d", n)
	}
	if repo.byID[1].Status != "active" {
		t.Fatalf("expected status unchanged (active), got %q", repo.byID[1].Status)
	}
}

// TestQuotaKeySweep_AlreadyExpiredSkipped 验证已是 expired 的 key 不再标 expired
// （幂等，不重复写库）：不触发 MarkExpiredIfStillExpired 与 UpdateStatusFields。
// 注意（EXP-B）：已 expired 的 key 允许账号补删（sweepOnce 对 expired 分支继续调用
// 到期账号软删），本用例的 fake 返回空集合，故无 outbox、不产生额外写入。
func TestQuotaKeySweep_AlreadyExpiredSkipped(t *testing.T) {
	repo := newRecordingQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{
		ID:        1,
		Status:    "expired",
		ExpiresAt: timePtr(time.Now().Add(-time.Hour)),
	}

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	if err != nil {
		t.Fatalf("sweepOnce returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 marked (idempotent skip), got %d", n)
	}
	if repo.byID[1].Status != "expired" {
		t.Fatalf("expected status still expired, got %q", repo.byID[1].Status)
	}
	for _, c := range repo.calledMethods() {
		if c == "UpdateStatusFields" || c == "MarkExpiredIfStillExpired" {
			t.Fatalf("already-expired key must not trigger %s", c)
		}
	}
}

// TestQuotaKeySweep_ExhaustedExpiredPreservesExhausted 验证到期且 exhausted 的
// 激活码：只标 expired，不重置 exhausted。
func TestQuotaKeySweep_ExhaustedExpiredPreservesExhausted(t *testing.T) {
	repo := newRecordingQuotaKeyRepo()
	repo.byID[1] = &ent.QuotaKey{
		ID:        1,
		Status:    "active",
		Exhausted: true,
		ExpiresAt: timePtr(time.Now().Add(-time.Hour)),
	}

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	if err != nil {
		t.Fatalf("sweepOnce returned error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 marked, got %d", n)
	}
	if repo.byID[1].Status != "expired" {
		t.Fatalf("expected status expired, got %q", repo.byID[1].Status)
	}
	if !repo.byID[1].Exhausted {
		t.Fatalf("expected exhausted to be preserved (true), got false")
	}
}

// TestQuotaKeySweep_OnlyCursorAccountDeleteCalls 验证清扫过程只触碰允许的仓储方法
// （EXP-B 接线后的新边界）：
//   - 允许被调用的方法集合恰为 {ListExpired, MarkExpiredIfStillExpired,
//     SoftDeleteCursorAccountsByExpiredQuotaKey, EnqueueAccountChangedOutbox}；
//   - 除 SoftDeleteCursorAccountsByExpiredQuotaKey / EnqueueAccountChangedOutbox 外，
//     不得出现其它含 "Account" 的方法；
//   - 未到期 key、expires_at=NULL key、非 active/expired 状态 key 不得触发账号删除调用。
func TestQuotaKeySweep_OnlyCursorAccountDeleteCalls(t *testing.T) {
	repo := newRecordingQuotaKeyRepo()
	// 混合：1 个到期 active（应标 expired + 补删）、1 个未到期 active、
	// 1 个到期 exhausted（非 active/expired，跳过）、1 个已 expired（只补删）、
	// 1 个 expires_at=NULL active（不在到期集合）。
	now := time.Now()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: "active", ExpiresAt: timePtr(now.Add(-time.Hour))}
	repo.byID[2] = &ent.QuotaKey{ID: 2, Status: "active", ExpiresAt: timePtr(now.Add(time.Hour))}
	repo.byID[3] = &ent.QuotaKey{ID: 3, Status: "exhausted", ExpiresAt: timePtr(now.Add(-time.Hour))}
	repo.byID[4] = &ent.QuotaKey{ID: 4, Status: "expired", ExpiresAt: timePtr(now.Add(-time.Hour))}
	repo.byID[5] = &ent.QuotaKey{ID: 5, Status: "active", ExpiresAt: nil}

	svc := newSweepTestSvc(repo)
	if _, err := svc.sweepOnce(context.Background(), &ent.Tx{}); err != nil {
		t.Fatalf("sweepOnce returned error: %v", err)
	}

	allowed := map[string]bool{
		"ListExpired":                               true,
		"MarkExpiredIfStillExpired":                 true,
		"SoftDeleteCursorAccountsByExpiredQuotaKey": true,
		"EnqueueAccountChangedOutbox":               true,
	}
	for _, c := range repo.calledMethods() {
		if !allowed[c] {
			t.Fatalf("sweep called unexpected method %q", c)
		}
		if strings.Contains(c, "Account") && c != "SoftDeleteCursorAccountsByExpiredQuotaKey" && c != "EnqueueAccountChangedOutbox" {
			t.Fatalf("sweep touched account-related method %q", c)
		}
	}
	// 未到期 / expires_at=NULL / 非 active/expired 状态的 key 不得触发账号删除调用：
	// 仅 key1（active 到期）与 key4（历史 expired）进入账号删除分派。
	if len(repo.cursorDeleteKeyIDs) != 2 {
		t.Fatalf("expected exactly 2 account-delete dispatches, got %d (%v)", len(repo.cursorDeleteKeyIDs), repo.cursorDeleteKeyIDs)
	}
	for _, id := range repo.cursorDeleteKeyIDs {
		if id != 1 && id != 4 {
			t.Fatalf("key %d must not trigger account deletion (keys 2/3/5 are out of scope)", id)
		}
	}
	// 仅 1 个到期 active 应被改写为 expired。
	if repo.byID[1].Status != "expired" {
		t.Fatalf("expected key 1 marked expired, got %q", repo.byID[1].Status)
	}
}

// TestQuotaKeySweep_RunOnceWritesHeartbeat 验证 runOnce 每轮写且仅写一次心跳，
// 且心跳 job 名为 quota_key_sweep（测试替身断言）。
func TestQuotaKeySweep_RunOnceWritesHeartbeat(t *testing.T) {
	repo := newRecordingQuotaKeyRepo()
	ops := &heartbeatRecordingOpsRepo{opsRepoMock: &opsRepoMock{}}

	// entClient 为 nil：runOnce 走“仅报活”路径，便于无 DB 断言。
	svc := NewQuotaKeySweeper(repo, ops, nil)
	svc.runOnce()

	if got := ops.heartbeatCalls(); got != 1 {
		t.Fatalf("expected exactly 1 heartbeat written, got %d", got)
	}
	if name := ops.lastJobName(); name != quotaKeySweepJobName {
		t.Fatalf("expected heartbeat job name %q, got %q", quotaKeySweepJobName, name)
	}
}

// staleExpiredRepo 模拟交错场景（D-CURSOR-032）：ListExpired 返回的是旧快照（仍显示到期），
// 但当前存储中该 key 的 expires_at 已被管理员用 UpdateMeta 清空。MarkExpiredIfStillExpired
// 按"当前值"判定，因此不应命中——避免"曾经到期"被误标 expired。
type staleExpiredRepo struct {
	*fakeQuotaKeyRepo
	stale []*ent.QuotaKey
}

func (r *staleExpiredRepo) ListExpired(_ context.Context, _ time.Time) ([]*ent.QuotaKey, error) {
	return r.stale, nil
}

// TestQuotaKeySweep_ExpiredSnapshotButExpiresCleared 验证交错：ListExpired 取到旧值
// （仍到期）后，管理员在两步间清空 expires_at；清扫按 DB 当前条件判定，不误标 expired。
func TestQuotaKeySweep_ExpiredSnapshotButExpiresCleared(t *testing.T) {
	repo := &staleExpiredRepo{fakeQuotaKeyRepo: newFakeQuotaKeyRepo()}
	// 当前存储：expires_at 已被管理员清空（模拟 ListExpired 之后、条件写入之前的 UpdateMeta 交错）。
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: "active", ExpiresAt: nil}
	// ListExpired 仍返回旧快照（当时 expires_at 早于 now）。
	repo.stale = []*ent.QuotaKey{{ID: 1, Status: "active", ExpiresAt: timePtr(time.Now().Add(-time.Hour))}}

	svc := newSweepTestSvc(repo)
	n, err := svc.sweepOnce(context.Background(), &ent.Tx{})
	if err != nil {
		t.Fatalf("sweepOnce returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 marked (interleaving cleared expires_at), got %d", n)
	}
	if repo.byID[1].Status != "active" {
		t.Fatalf("expected status unchanged (active), got %q", repo.byID[1].Status)
	}
}
