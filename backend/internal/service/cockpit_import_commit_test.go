package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeCockpitImportCommitRepo 是 CockpitImportCommitRepository 的内存假实现，
// 用于不依赖 DB 的 service 层编排测试。
type fakeCockpitImportCommitRepo struct {
	lastPayloads []CockpitImportAccountPayload
	lastResult   CockpitImportCommitResult
	lastErr      error
	calls        int
}

func (f *fakeCockpitImportCommitRepo) CommitCockpitImport(ctx context.Context, payloads []CockpitImportAccountPayload) (CockpitImportCommitResult, error) {
	f.calls++
	f.lastPayloads = payloads
	return f.lastResult, f.lastErr
}

// TestCockpitImportCommitServiceForwards 验证 service 入口正确转发载荷并汇总计数。
func TestCockpitImportCommitServiceForwards(t *testing.T) {
	fake := &fakeCockpitImportCommitRepo{
		lastResult: CockpitImportCommitResult{Created: 3, SkippedExisting: 1},
	}
	svc := NewCockpitImportCommitService(fake)

	payloads := []CockpitImportAccountPayload{
		{Platform: "claude", UID: "u1", Name: "n1", AccountType: "oauth", Credentials: map[string]any{"access_token": "a"}},
		{Platform: "claude", UID: "u2", Name: "n2", AccountType: "oauth", Credentials: map[string]any{"access_token": "b"}},
	}

	res, err := svc.Commit(context.Background(), payloads)
	require.NoError(t, err)
	require.Equal(t, 1, fake.calls, "应恰好调用一次仓储")
	require.Equal(t, payloads, fake.lastPayloads, "应原样转发载荷")
	require.Equal(t, 3, res.Created)
	require.Equal(t, 1, res.SkippedExisting)
}

// TestCockpitImportCommitServiceEmptyPayloads 验证空载荷直接返回零计数（无需落库）。
func TestCockpitImportCommitServiceEmptyPayloads(t *testing.T) {
	fake := &fakeCockpitImportCommitRepo{lastResult: CockpitImportCommitResult{}}
	svc := NewCockpitImportCommitService(fake)

	res, err := svc.Commit(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, fake.calls, "空载荷不应调用仓储")
	require.Equal(t, 0, res.Created)
	require.Equal(t, 0, res.SkippedExisting)
}

// TestCockpitImportCommitServicePropagatesError 验证仓储错误向上传播（失败关闭，不吞错）。
func TestCockpitImportCommitServicePropagatesError(t *testing.T) {
	fake := &fakeCockpitImportCommitRepo{lastErr: errBoom{}}
	svc := NewCockpitImportCommitService(fake)

	_, err := svc.Commit(context.Background(), []CockpitImportAccountPayload{
		{Platform: "claude", UID: "u1", Name: "n1", AccountType: "oauth"},
	})
	require.Error(t, err)
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
