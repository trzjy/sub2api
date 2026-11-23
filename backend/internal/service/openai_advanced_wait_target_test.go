//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func newAdvancedWaitTargetScheduler(snapshot, latest map[int64]*Account) *defaultOpenAIAccountScheduler {
	return &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{
		accountRepo:        &upstreamCostCountingAccountRepo{accounts: latest},
		schedulerSnapshot:  &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{accountsByID: snapshot}},
		concurrencyService: NewConcurrencyService(&upstreamCostTrackingConcurrencyCache{}),
	}}
}

func advancedWaitTargetAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
}

func TestAdvancedSchedulerWaitTargetPrefersStickyOriginalAcrossPasses(t *testing.T) {
	other := advancedWaitTargetAccount(1)
	original := advancedWaitTargetAccount(2)
	scheduler := newAdvancedWaitTargetScheduler(
		map[int64]*Account{other.ID: other, original.ID: original},
		map[int64]*Account{other.ID: other, original.ID: original},
	)

	// other 排在原号之前且未尝试；原号已尝试且已知满载。pass 循环会命中 other，
	// 因此等待目标必须在进入 pass 循环前由原号复核块锁定。
	selectionOrder := []openAIAccountCandidateScore{
		{account: other, loadInfo: &AccountLoadInfo{AccountID: other.ID}, loadKnown: false},
		{
			account:   original,
			loadInfo:  &AccountLoadInfo{AccountID: original.ID, CurrentConcurrency: original.Concurrency, LoadRate: 100},
			loadKnown: true,
		},
	}
	budget := newOpenAISelectionProbeBudget()
	budget.enableLimit()
	budget.attempted[original.ID] = struct{}{}

	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, StickyAccountID: original.ID}
	selection, _, _, _, err := scheduler.finishLoadBalanceSelectionFallback(
		context.Background(), req, openAIAccountLoadSelectionAttempt{selectionOrder: selectionOrder},
		budget, openAISelectionFilterStats{},
	)

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, original.ID, selection.Account.ID)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, original.ID, selection.WaitPlan.AccountID)
}

func TestAdvancedSchedulerWaitTargetSkipsExcludedSticky(t *testing.T) {
	other := advancedWaitTargetAccount(3)
	original := advancedWaitTargetAccount(4)
	scheduler := newAdvancedWaitTargetScheduler(
		map[int64]*Account{other.ID: other, original.ID: original},
		map[int64]*Account{other.ID: other, original.ID: original},
	)

	selectionOrder := []openAIAccountCandidateScore{
		{account: other, loadInfo: &AccountLoadInfo{AccountID: other.ID}, loadKnown: false},
		{account: original, loadInfo: &AccountLoadInfo{AccountID: original.ID}, loadKnown: false},
	}
	req := OpenAIAccountScheduleRequest{
		Platform:        PlatformOpenAI,
		StickyAccountID: original.ID,
		ExcludedIDs:     map[int64]struct{}{original.ID: {}},
	}
	selection, _, _, _, err := scheduler.finishLoadBalanceSelectionFallback(
		context.Background(), req, openAIAccountLoadSelectionAttempt{selectionOrder: selectionOrder},
		newOpenAISelectionProbeBudget(), openAISelectionFilterStats{},
	)

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, other.ID, selection.Account.ID)
	require.NotEqual(t, original.ID, selection.Account.ID)
}

func TestAdvancedSchedulerWaitTargetRejectsIneligibleSticky(t *testing.T) {
	other := advancedWaitTargetAccount(5)
	original := advancedWaitTargetAccount(6)
	latestOriginal := *original
	latestOriginal.Status = StatusDisabled
	scheduler := newAdvancedWaitTargetScheduler(
		map[int64]*Account{other.ID: other, original.ID: original},
		map[int64]*Account{other.ID: other, original.ID: &latestOriginal},
	)

	selectionOrder := []openAIAccountCandidateScore{
		{account: other, loadInfo: &AccountLoadInfo{AccountID: other.ID}, loadKnown: false},
		{account: original, loadInfo: &AccountLoadInfo{AccountID: original.ID}, loadKnown: false},
	}
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, StickyAccountID: original.ID}
	selection, _, _, _, err := scheduler.finishLoadBalanceSelectionFallback(
		context.Background(), req, openAIAccountLoadSelectionAttempt{selectionOrder: selectionOrder},
		newOpenAISelectionProbeBudget(), openAISelectionFilterStats{},
	)

	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, other.ID, selection.Account.ID)
	require.NotEqual(t, original.ID, selection.Account.ID)
}
