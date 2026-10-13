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
// 用于断言清扫过程只触碰 quota_keys 操作、绝不触碰 accounts 相关调用。
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

// TestQuotaKeySweep_AlreadyExpiredSkipped 验证已是 expired 的跳过（幂等，不重复写库）。
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
		if c == "UpdateStatusFields" {
			t.Fatalf("already-expired key must not trigger UpdateStatusFields")
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

// TestQuotaKeySweep_NoAccountCalls 验证清扫全程不触碰 accounts 表：
// 仅允许 ListExpired + UpdateStatusFields，且任何被调用方法名都不含 "Account"。
func TestQuotaKeySweep_NoAccountCalls(t *testing.T) {
	repo := newRecordingQuotaKeyRepo()
	// 混合：1 个到期 active、2 个未到期、1 个已 expired。
	now := time.Now()
	repo.byID[1] = &ent.QuotaKey{ID: 1, Status: "active", ExpiresAt: timePtr(now.Add(-time.Hour))}
	repo.byID[2] = &ent.QuotaKey{ID: 2, Status: "active", ExpiresAt: timePtr(now.Add(time.Hour))}
	repo.byID[3] = &ent.QuotaKey{ID: 3, Status: "exhausted", ExpiresAt: timePtr(now.Add(time.Hour))}
	repo.byID[4] = &ent.QuotaKey{ID: 4, Status: "expired", ExpiresAt: timePtr(now.Add(-time.Hour))}

	svc := newSweepTestSvc(repo)
	if _, err := svc.sweepOnce(context.Background(), &ent.Tx{}); err != nil {
		t.Fatalf("sweepOnce returned error: %v", err)
	}

	for _, c := range repo.calledMethods() {
		if strings.Contains(c, "Account") {
			t.Fatalf("sweep touched account-related method %q", c)
		}
		if c != "ListExpired" && c != "UpdateStatusFields" {
			t.Fatalf("sweep called unexpected method %q", c)
		}
	}
	// 仅 1 个到期 active 应被改写。
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
