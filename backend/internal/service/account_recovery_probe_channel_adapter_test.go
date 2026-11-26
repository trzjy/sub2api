//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// modelRateLimitAdapterRepoStub 嵌入 nil 的 AccountRepository，并显式实现批2 的两个方法，
// 用于验证适配器把调用委派给底层 repo 且原样返回结果。
type modelRateLimitAdapterRepoStub struct {
	AccountRepository

	listNow   time.Time
	listLimit int
	listTasks []ModelProbeTask
	listErr   error

	patchAccountID   int64
	patchScope       string
	patchExpectedGen int64
	patchPayload     ModelProbePatch
	patchOK          bool
	patchErr         error
}

func (s *modelRateLimitAdapterRepoStub) ListModelRateLimitProbeDue(_ context.Context, now time.Time, limit int) ([]ModelProbeTask, error) {
	s.listNow = now
	s.listLimit = limit
	return s.listTasks, s.listErr
}

func (s *modelRateLimitAdapterRepoStub) PatchModelRateLimitEntry(_ context.Context, accountID int64, scope string, expectedGen int64, patch ModelProbePatch) (bool, error) {
	s.patchAccountID = accountID
	s.patchScope = scope
	s.patchExpectedGen = expectedGen
	s.patchPayload = patch
	return s.patchOK, s.patchErr
}

// unsupportedModelRateLimitAdapterRepoStub 仅嵌入 nil 接口，不实现批2 方法，
// 用于验证适配器在底层不支持时失败关闭。
type unsupportedModelRateLimitAdapterRepoStub struct {
	AccountRepository
}

func TestAccountRecoveryProbeRepoAdapter_ListModelRateLimitProbeDue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	want := []ModelProbeTask{{AccountID: 7, Scope: "gpt-4o", MetaLastEventAt: "2026-11-26T11:00:00Z"}}
	stub := &modelRateLimitAdapterRepoStub{listTasks: want}
	adapter := accountRecoveryProbeRepoAdapter{AccountRepository: stub}

	got, err := adapter.ListModelRateLimitProbeDue(context.Background(), now, 11)

	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, now, stub.listNow)
	require.Equal(t, 11, stub.listLimit)
}

func TestAccountRecoveryProbeRepoAdapter_ListModelRateLimitProbeDue_Unsupported(t *testing.T) {
	t.Parallel()

	adapter := accountRecoveryProbeRepoAdapter{AccountRepository: &unsupportedModelRateLimitAdapterRepoStub{}}

	got, err := adapter.ListModelRateLimitProbeDue(context.Background(), time.Now(), 5)

	require.Nil(t, got)
	require.ErrorIs(t, err, ErrRecoveryLockUnavailable)
}

func TestAccountRecoveryProbeRepoAdapter_PatchModelRateLimitEntry(t *testing.T) {
	t.Parallel()

	patch := ModelProbePatch{Op: ModelProbePatchObserve, ObserveKey: "observed_at", Observe: "2026-11-26T12:00:00Z"}
	stub := &modelRateLimitAdapterRepoStub{patchOK: true}
	adapter := accountRecoveryProbeRepoAdapter{AccountRepository: stub}

	ok, err := adapter.PatchModelRateLimitEntry(context.Background(), 42, "claude-3", 9, patch)

	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(42), stub.patchAccountID)
	require.Equal(t, "claude-3", stub.patchScope)
	require.Equal(t, int64(9), stub.patchExpectedGen)
	require.Equal(t, patch, stub.patchPayload)
}

func TestAccountRecoveryProbeRepoAdapter_PatchModelRateLimitEntry_Unsupported(t *testing.T) {
	t.Parallel()

	adapter := accountRecoveryProbeRepoAdapter{AccountRepository: &unsupportedModelRateLimitAdapterRepoStub{}}

	ok, err := adapter.PatchModelRateLimitEntry(context.Background(), 1, "scope", 2, ModelProbePatch{Op: ModelProbePatchDeleteScope})

	require.False(t, ok)
	require.ErrorIs(t, err, ErrRecoveryLockUnavailable)
}

func TestAccountRecoveryProbeRepoAdapter_PatchModelRateLimitEntry_PassthroughError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("patch failed")
	stub := &modelRateLimitAdapterRepoStub{patchOK: false, patchErr: wantErr}
	adapter := accountRecoveryProbeRepoAdapter{AccountRepository: stub}

	ok, err := adapter.PatchModelRateLimitEntry(context.Background(), 1, "scope", 2, ModelProbePatch{Op: ModelProbePatchReschedule})

	require.False(t, ok)
	require.ErrorIs(t, err, wantErr)
}
