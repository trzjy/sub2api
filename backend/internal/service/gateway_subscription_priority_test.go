//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewaySubscriptionPriority_MixedPoolPrefersSubscriptionBeforePriority(t *testing.T) {
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Priority: 1, Status: StatusActive, Schedulable: true},
			{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Priority: 9, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	svc := &GatewayService{accountRepo: repo, cache: &mockGatewayCacheForPlatform{}, cfg: testConfig()}
	selected, err := svc.selectAccountWithMixedScheduling(context.Background(), nil, "", "claude-sonnet-4-5", nil, PlatformAnthropic)
	require.NoError(t, err)
	require.Equal(t, int64(2), selected.ID)
}

func TestGatewaySubscriptionPriority_MixedPoolFallsBackWhenSubscriptionUnschedulable(t *testing.T) {
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Priority: 1, Status: StatusActive, Schedulable: false},
			{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Priority: 9, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	svc := &GatewayService{accountRepo: repo, cache: &mockGatewayCacheForPlatform{}, cfg: testConfig()}
	selected, err := svc.selectAccountWithMixedScheduling(context.Background(), nil, "", "claude-sonnet-4-5", nil, PlatformAnthropic)
	require.NoError(t, err)
	require.Equal(t, int64(2), selected.ID)
}

func TestGatewaySubscriptionPriority_LoadPoolUsesRelayOnlyAfterSubscriptionsExhausted(t *testing.T) {
	subscription := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeSetupToken}
	relay := &Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	available := []accountWithLoad{
		{account: relay},
		{account: subscription},
	}

	pool := subscriptionPriorityLoadPool(available)
	require.Len(t, pool, 1)
	require.Equal(t, int64(1), pool[0].account.ID)

	pool = subscriptionPriorityLoadPool([]accountWithLoad{{account: relay}})
	require.Len(t, pool, 1)
	require.Equal(t, int64(2), pool[0].account.ID)
}

func TestGatewaySubscriptionPriority_GeminiMessagesCompatUsesSubscriptionPoolFirst(t *testing.T) {
	svc := &GeminiMessagesCompatService{}
	relay := &Account{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, Priority: 1}
	subscription := &Account{ID: 2, Platform: PlatformGemini, Type: AccountTypeOAuth, Priority: 9}

	require.True(t, svc.isBetterGeminiAccount(subscription, relay))
}
