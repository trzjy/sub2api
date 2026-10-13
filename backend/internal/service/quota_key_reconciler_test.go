//go:build unit

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
)

// fakeReconcilerRepo 在 fakeQuotaKeyRepo 之上覆写 ListEligibleForRefresh，
// 控制返回的可刷新激活码集合（仅供 reconciler 单测使用，合同 §5：替身仅存于测试）。
type fakeReconcilerRepo struct {
	*fakeQuotaKeyRepo
	mu   sync.Mutex
	keys []*ent.QuotaKey
	err  error
}

func newFakeReconcilerRepo() *fakeReconcilerRepo {
	return &fakeReconcilerRepo{fakeQuotaKeyRepo: newFakeQuotaKeyRepo()}
}

func (r *fakeReconcilerRepo) ListEligibleForRefresh(_ context.Context) ([]*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keys, r.err
}

// fakePuller 是 quotaKeyPuller 的测试替身，记录每次调用的 quotaKeyID，
// 并可按 keyID 注入错误（验证单 key 失败不中断其余 key）。
type fakePuller struct {
	mu        sync.Mutex
	calls     []int64
	errOnIDs  map[int64]error
	defaultErr error
}

func newFakePuller() *fakePuller {
	return &fakePuller{errOnIDs: make(map[int64]error)}
}

func (p *fakePuller) PullAccounts(_ context.Context, quotaKeyID int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, quotaKeyID)
	if err, ok := p.errOnIDs[quotaKeyID]; ok {
		return err
	}
	return p.defaultErr
}

func (p *fakePuller) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *fakePuller) calledIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int64, len(p.calls))
	copy(out, p.calls)
	return out
}

// TestReconciler_CallsPullForEligibleKeys 验证扫描返回 2 个 key 时，每个 key 的
// PullAccounts 都被调用一次（按 key.ID）。
func TestReconciler_CallsPullForEligibleKeys(t *testing.T) {
	repo := newFakeReconcilerRepo()
	repo.keys = []*ent.QuotaKey{
		{ID: 10, Status: "active"},
		{ID: 20, Status: "active"},
	}
	puller := newFakePuller()
	ops := &heartbeatRecordingOpsRepo{opsRepoMock: &opsRepoMock{}}

	r := NewQuotaKeyReconciler(repo, puller, ops)
	r.runOnce()

	if got := puller.callCount(); got != 2 {
		t.Fatalf("expected PullAccounts called 2 times, got %d", got)
	}
	ids := puller.calledIDs()
	if len(ids) == 2 && !(ids[0] == 10 && ids[1] == 20) {
		t.Fatalf("expected PullAccounts called for IDs [10 20], got %v", ids)
	}
}

// TestReconciler_NoEligibleKeys_NoPull 验证扫描返回空集合时，PullAccounts 零调用。
func TestReconciler_NoEligibleKeys_NoPull(t *testing.T) {
	repo := newFakeReconcilerRepo()
	repo.keys = nil
	puller := newFakePuller()
	ops := &heartbeatRecordingOpsRepo{opsRepoMock: &opsRepoMock{}}

	r := NewQuotaKeyReconciler(repo, puller, ops)
	r.runOnce()

	if got := puller.callCount(); got != 0 {
		t.Fatalf("expected 0 PullAccounts calls, got %d", got)
	}
}

// TestReconciler_HeartbeatWritten 验证 runOnce 每轮写且仅写一次心跳，且 job 名正确。
func TestReconciler_HeartbeatWritten(t *testing.T) {
	repo := newFakeReconcilerRepo()
	repo.keys = []*ent.QuotaKey{{ID: 1, Status: "active"}}
	puller := newFakePuller()
	ops := &heartbeatRecordingOpsRepo{opsRepoMock: &opsRepoMock{}}

	r := NewQuotaKeyReconciler(repo, puller, ops)
	r.runOnce()

	if got := ops.heartbeatCalls(); got != 1 {
		t.Fatalf("expected exactly 1 heartbeat written, got %d", got)
	}
	if name := ops.lastJobName(); name != quotaKeyReconcileJobName {
		t.Fatalf("expected heartbeat job name %q, got %q", quotaKeyReconcileJobName, name)
	}
}

// TestReconciler_PullErrorDoesNotAbort 验证第 1 个 key 拉号失败时，第 2 个 key 仍被调用。
func TestReconciler_PullErrorDoesNotAbort(t *testing.T) {
	repo := newFakeReconcilerRepo()
	repo.keys = []*ent.QuotaKey{
		{ID: 30, Status: "active"},
		{ID: 40, Status: "active"},
	}
	puller := newFakePuller()
	puller.errOnIDs[30] = context.DeadlineExceeded // 任意错误，模拟拉号失败
	ops := &heartbeatRecordingOpsRepo{opsRepoMock: &opsRepoMock{}}

	r := NewQuotaKeyReconciler(repo, puller, ops)
	r.runOnce()

	ids := puller.calledIDs()
	if len(ids) != 2 {
		t.Fatalf("expected both keys pulled despite first error, got %v", ids)
	}
	if !(ids[0] == 30 && ids[1] == 40) {
		t.Fatalf("expected PullAccounts called for IDs [30 40], got %v", ids)
	}
}

// TestReconciler_StopCancels 验证 Start 后 Stop 能无阻塞返回（goroutine 已退出）。
func TestReconciler_StopCancels(t *testing.T) {
	repo := newFakeReconcilerRepo()
	puller := newFakePuller()
	ops := &heartbeatRecordingOpsRepo{opsRepoMock: &opsRepoMock{}}

	r := NewQuotaKeyReconciler(repo, puller, ops)
	r.Start()

	done := make(chan struct{})
	go func() {
		r.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop 返回即说明 goroutine 已退出（否则 wg.Wait 会一直阻塞）。
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return; goroutine likely blocked and did not exit")
	}
}
