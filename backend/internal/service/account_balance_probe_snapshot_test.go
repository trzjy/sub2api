package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type balanceProbeSnapshotRepoStub struct {
	AccountRepository
	accounts []Account
	updates  map[int64]map[string]any
}

func (r *balanceProbeSnapshotRepoStub) ListDueBalanceProbeAccounts(_ context.Context, _ time.Time, limit int) ([]Account, error) {
	if len(r.accounts) > limit {
		return r.accounts[:limit], nil
	}
	return r.accounts, nil
}

func (r *balanceProbeSnapshotRepoStub) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	if r.updates == nil {
		r.updates = map[int64]map[string]any{}
	}
	r.updates[id] = updates
	return nil
}

func TestAccountBalanceProbeCheckServicePersistsFailedResult(t *testing.T) {
	repo := &balanceProbeSnapshotRepoStub{accounts: []Account{{ID: 2, Type: AccountTypeAPIKey, Status: StatusActive}}}
	service := newTestAccountBalanceProbeCheckService(repo, func(context.Context, int64) (*BalanceProbeResult, error) {
		return nil, context.DeadlineExceeded
	}, time.Minute)

	err := service.RunDue(context.Background())

	require.NoError(t, err)
	snapshot := repo.updates[2][BalanceProbeSnapshotExtraKey].(*BalanceProbeResult)
	require.False(t, snapshot.Success)
	require.NotEmpty(t, snapshot.Error)
}

func TestAccountBalanceProbeCheckServicePersistsSuccessfulResult(t *testing.T) {
	repo := &balanceProbeSnapshotRepoStub{accounts: []Account{{ID: 10, Type: AccountTypeAPIKey, Status: StatusActive}}}
	service := newTestAccountBalanceProbeCheckService(repo, func(context.Context, int64) (*BalanceProbeResult, error) {
		remaining := 9.49
		return &BalanceProbeResult{Success: true, Valid: true, Remaining: &remaining, Unit: "USD", FetchedAt: time.Now().UTC()}, nil
	}, time.Minute)

	err := service.RunDue(context.Background())

	require.NoError(t, err)
	snapshot := repo.updates[10][BalanceProbeSnapshotExtraKey].(*BalanceProbeResult)
	require.True(t, snapshot.Success)
	require.Equal(t, 9.49, *snapshot.Remaining)
}
