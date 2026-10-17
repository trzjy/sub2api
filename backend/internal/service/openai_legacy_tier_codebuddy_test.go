//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type hy3LegacyAggregateGroupRepo struct {
	GroupRepository
	group *Group
	err   error
}

func (r hy3LegacyAggregateGroupRepo) GetByIDLite(_ context.Context, id int64) (*Group, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.group == nil || r.group.ID != id {
		return nil, nil
	}
	group := *r.group
	return &group, nil
}

type hy3LegacyAggregateAccountRepo struct {
	AccountRepository
	accounts  []Account
	groupID   int64
	aggregate bool
}

func (r hy3LegacyAggregateAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, errors.New("account not found")
}

func (r hy3LegacyAggregateAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, groupID int64, platform string) ([]Account, error) {
	var accounts []Account
	for _, account := range r.accounts {
		if !openAIStickyAccountMatchesGroup(&account, &groupID) {
			continue
		}
		if account.Platform == platform || (groupID == r.groupID && r.aggregate && account.Platform == PlatformCodeBuddy && isCodeBuddyAggregatedPlatform(platform)) {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func (r hy3LegacyAggregateAccountRepo) ListSchedulableByGroupID(_ context.Context, groupID int64) ([]Account, error) {
	var accounts []Account
	for _, account := range r.accounts {
		if openAIStickyAccountMatchesGroup(&account, &groupID) {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func hy3LegacyTestAccount(id int64, platform string, groupID int64, priority int) Account {
	account := Account{
		ID: id, Platform: platform, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: priority,
		GroupIDs: []int64{groupID}, AccountGroups: []AccountGroup{{AccountID: id, GroupID: groupID}},
	}
	if platform == PlatformCodeBuddy {
		account.Extra = map[string]any{"mask_upstream_identity": true}
		account.Credentials = map[string]any{"model_mapping": map[string]any{"kimi-k3": "hy3", "deepseek-v4.1-flash": "hy3", "glm-5.3-flash": "hy3"}}
		account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{"hy3": {ID: "hy3"}}})
	}
	return account
}

func newHy3LegacyTestService(group *Group, accounts []Account, groupErr error) *OpenAIGatewayService {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(5001)
	repo := hy3LegacyAggregateAccountRepo{
		accounts: accounts, groupID: groupID,
		aggregate: group != nil && group.AggregateCodeBuddyEnabled,
	}
	snapshot := &SchedulerSnapshotService{
		accountRepo: repo,
		groupRepo:   hy3LegacyAggregateGroupRepo{group: group, err: groupErr},
	}
	cfg := &config.Config{}
	return &OpenAIGatewayService{
		accountRepo:       repo,
		schedulerSnapshot: snapshot,
		cache:             &schedulerTestGatewayCache{},
		cfg:               cfg,
		rateLimitService:  newOpenAIAdvancedSchedulerRateLimitService("false"),
	}
}

func TestLegacyTierRoutingCodeBuddySelectsMappedAccountByPool(t *testing.T) {
	for _, route := range []struct {
		platform string
		model    string
	}{
		{PlatformKimi, "kimi-k3"},
		{PlatformDeepseek, "deepseek-v4.1-flash"},
		{PlatformZhipu, "glm-5.3-flash"},
	} {
		t.Run(route.platform, func(t *testing.T) {
			groupID := int64(5001)
			group := &Group{ID: groupID, Platform: route.platform, AggregateCodeBuddyEnabled: true}
			accounts := []Account{
				hy3LegacyTestAccount(5010, PlatformCodeBuddy, groupID, 0),
				hy3LegacyTestAccount(5011, route.platform, groupID, 1),
				hy3LegacyTestAccount(5012, route.platform, groupID, 2),
			}
			accounts[1].Extra = map[string]any{"mask_upstream_identity": true}
			svc := newHy3LegacyTestService(group, accounts, nil)
			t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
			require.Nil(t, svc.getOpenAIAccountScheduler(context.Background()), "test requires the production legacy path")

			selection, _, err := svc.SelectAccountWithSchedulerForCapability(
				WithTierRouting(context.Background(), TierRoutingSmall), &groupID, "", "", route.model, nil,
				OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, false, route.platform,
			)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(5010), selection.Account.ID, "small/masked pool should select the aggregated CodeBuddy HY3 account")
			releaseLegacySchedulerDecisionSelection(selection)

			selection, _, err = svc.SelectAccountWithSchedulerForCapability(
				WithTierRouting(context.Background(), TierRoutingBig), &groupID, "", "", route.model, nil,
				OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, false, route.platform,
			)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(5012), selection.Account.ID, "big/upstream pool must not select masked CodeBuddy HY3")
			releaseLegacySchedulerDecisionSelection(selection)
		})
	}
}

func TestLegacyTierRoutingCodeBuddyStickySmallThenBig(t *testing.T) {
	groupID := int64(5001)
	group := &Group{ID: groupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}
	accounts := []Account{
		hy3LegacyTestAccount(5020, PlatformCodeBuddy, groupID, 0),
		hy3LegacyTestAccount(5021, PlatformKimi, groupID, 1),
		hy3LegacyTestAccount(5022, PlatformKimi, groupID, 2),
	}
	accounts[1].Extra = map[string]any{"mask_upstream_identity": true}
	svc := newHy3LegacyTestService(group, accounts, nil)
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	ctx := context.Background()
	session := "hy3-tier-sticky"
	require.NoError(t, svc.setStickySessionAccountID(ctx, &groupID, session, 5020, openaiStickySessionTTL))

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		WithTierRouting(ctx, TierRoutingSmall), &groupID, "", session, "kimi-k3", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, false, PlatformKimi,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(5020), selection.Account.ID)
	releaseLegacySchedulerDecisionSelection(selection)

	selection, _, err = svc.SelectAccountWithSchedulerForCapability(
		WithTierRouting(ctx, TierRoutingBig), &groupID, "", session, "kimi-k3", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, false, PlatformKimi,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(5022), selection.Account.ID, "sticky CodeBuddy account must be discarded outside the current pool")
	releaseLegacySchedulerDecisionSelection(selection)
}

func TestLegacyTierRoutingCodeBuddyRejectsOutsideQualification(t *testing.T) {
	groupID := int64(5001)
	for _, tc := range []struct {
		name     string
		group    *Group
		groupErr error
		model    string
		withTier bool
		catalog  map[string]UpstreamModelMetadata
	}{
		{name: "group disabled", group: &Group{ID: groupID, Platform: PlatformKimi}, model: "kimi-k3", withTier: true},
		{name: "group lookup failed", group: &Group{ID: groupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}, groupErr: errors.New("group lookup failed"), model: "kimi-k3", withTier: true},
		{name: "wrong group id", group: &Group{ID: groupID + 1, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}, model: "kimi-k3", withTier: true},
		{name: "wrong group platform", group: &Group{ID: groupID, Platform: PlatformDeepseek, AggregateCodeBuddyEnabled: true}, model: "kimi-k3", withTier: true},
		{name: "missing tier", group: &Group{ID: groupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}, model: "kimi-k3"},
		{name: "unmapped public model", group: &Group{ID: groupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}, model: "kimi-k2", withTier: true},
		{name: "empty catalog", group: &Group{ID: groupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}, model: "kimi-k3", withTier: true, catalog: map[string]UpstreamModelMetadata{}},
		{name: "mapped model absent from catalog", group: &Group{ID: groupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}, model: "kimi-k3", withTier: true, catalog: map[string]UpstreamModelMetadata{"other-model": {ID: "other-model"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := []Account{
				hy3LegacyTestAccount(5030, PlatformCodeBuddy, groupID, 0),
				hy3LegacyTestAccount(5031, PlatformKimi, groupID, 1),
				hy3LegacyTestAccount(5032, PlatformKimi, groupID, 2),
			}
			accounts[1].Extra = map[string]any{"mask_upstream_identity": true}
			if tc.catalog != nil {
				accounts[0].SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: tc.catalog})
			}
			svc := newHy3LegacyTestService(tc.group, accounts, tc.groupErr)
			t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
			ctx := context.Background()
			if tc.withTier {
				ctx = WithTierRouting(ctx, TierRoutingSmall)
			}
			selection, _, err := svc.SelectAccountWithSchedulerForCapability(
				ctx, &groupID, "", "", tc.model, nil,
				OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, false, PlatformKimi,
			)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.NotEqual(t, int64(5030), selection.Account.ID, "CodeBuddy must fail closed outside the exact mapped HY3 qualification")
			releaseLegacySchedulerDecisionSelection(selection)
		})
	}
}

func TestLegacyTierRoutingCodeBuddyClosedGroupRejectsStickyAccount(t *testing.T) {
	groupID := int64(5001)
	group := &Group{ID: groupID, Platform: PlatformKimi}
	accounts := []Account{
		hy3LegacyTestAccount(5040, PlatformCodeBuddy, groupID, 0),
		hy3LegacyTestAccount(5041, PlatformKimi, groupID, 1),
		hy3LegacyTestAccount(5042, PlatformKimi, groupID, 2),
	}
	accounts[1].Extra = map[string]any{"mask_upstream_identity": true}
	svc := newHy3LegacyTestService(group, accounts, nil)
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	ctx := context.Background()
	session := "hy3-closed-group-sticky"
	require.NoError(t, svc.setStickySessionAccountID(ctx, &groupID, session, 5040, openaiStickySessionTTL))

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		WithTierRouting(ctx, TierRoutingSmall), &groupID, "", session, "kimi-k3", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, false, PlatformKimi,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotEqual(t, int64(5040), selection.Account.ID, "a sticky binding cannot bypass the current group aggregate switch")
	releaseLegacySchedulerDecisionSelection(selection)
}

func TestLegacyTierRoutingCodeBuddySmallPoolExhaustionUsesExistingCrossPool(t *testing.T) {
	groupID := int64(5001)
	group := &Group{ID: groupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}
	accounts := []Account{
		hy3LegacyTestAccount(5050, PlatformCodeBuddy, groupID, 0),
		hy3LegacyTestAccount(5051, PlatformKimi, groupID, 1),
	}
	accounts[0].Schedulable = false
	svc := newHy3LegacyTestService(group, accounts, nil)
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		WithTierRouting(context.Background(), TierRoutingSmall), &groupID, "", "", "kimi-k3", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, false, PlatformKimi,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(5051), selection.Account.ID, "small-pool exhaustion must use the existing single upstream cross-pool retry")
	releaseLegacySchedulerDecisionSelection(selection)
}

func TestLegacyTierRoutingCodeBuddyDBRecheckUsesFreshCatalog(t *testing.T) {
	groupID := int64(5001)
	group := &Group{ID: groupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}
	snapshotAccount := hy3LegacyTestAccount(5060, PlatformCodeBuddy, groupID, 0)
	nativeMasked := hy3LegacyTestAccount(5061, PlatformKimi, groupID, 1)
	nativeMasked.Extra = map[string]any{"mask_upstream_identity": true}
	upstream := hy3LegacyTestAccount(5062, PlatformKimi, groupID, 2)
	dbFresh := snapshotAccount
	dbFresh.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{"different-model": {ID: "different-model"}}})
	repo := hy3LegacyAggregateAccountRepo{accounts: []Account{dbFresh, nativeMasked, upstream}, groupID: groupID, aggregate: true}
	snapshotCache := &openAISnapshotCacheStub{
		snapshotAccounts: []*Account{&snapshotAccount, &nativeMasked, &upstream},
		accountsByID: map[int64]*Account{
			snapshotAccount.ID: &snapshotAccount,
			nativeMasked.ID:    &nativeMasked,
			upstream.ID:        &upstream,
		},
	}
	svc := newHy3LegacyTestService(group, repo.accounts, nil)
	svc.accountRepo = repo
	svc.schedulerSnapshot.accountRepo = repo
	svc.schedulerSnapshot.cache = snapshotCache
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		WithTierRouting(context.Background(), TierRoutingSmall), &groupID, "", "", "kimi-k3", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, false, PlatformKimi,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotEqual(t, snapshotAccount.ID, selection.Account.ID, "DB recheck must reject a stale cached catalog that no longer contains mapped HY3")
	releaseLegacySchedulerDecisionSelection(selection)
}

func TestLegacyTierRoutingCodeBuddySnapshotFreshAccountCatalog(t *testing.T) {
	groupID := int64(5001)
	group := &Group{ID: groupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}
	staleCandidate := hy3LegacyTestAccount(5070, PlatformCodeBuddy, groupID, 0)
	snapshotFresh := staleCandidate
	snapshotFresh.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{"different-model": {ID: "different-model"}}})
	nativeMasked := hy3LegacyTestAccount(5071, PlatformKimi, groupID, 1)
	nativeMasked.Extra = map[string]any{"mask_upstream_identity": true}
	upstream := hy3LegacyTestAccount(5072, PlatformKimi, groupID, 2)
	repo := hy3LegacyAggregateAccountRepo{accounts: []Account{staleCandidate, nativeMasked, upstream}, groupID: groupID, aggregate: true}
	snapshotCache := &openAISnapshotCacheStub{
		snapshotAccounts: []*Account{&staleCandidate, &nativeMasked, &upstream},
		accountsByID: map[int64]*Account{
			staleCandidate.ID: &snapshotFresh,
			nativeMasked.ID:   &nativeMasked,
			upstream.ID:       &upstream,
		},
	}
	svc := newHy3LegacyTestService(group, repo.accounts, nil)
	svc.accountRepo = repo
	svc.schedulerSnapshot.accountRepo = repo
	svc.schedulerSnapshot.cache = snapshotCache
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		WithTierRouting(context.Background(), TierRoutingSmall), &groupID, "", "", "kimi-k3", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, false, PlatformKimi,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotEqual(t, staleCandidate.ID, selection.Account.ID, "fresh snapshot account must reject the stale candidate catalog")
	releaseLegacySchedulerDecisionSelection(selection)
}

func TestCodeBuddyTierPlatformMismatchRequiresMatchingBoundGroup(t *testing.T) {
	const groupID = int64(5080)
	group := &Group{ID: groupID, Platform: PlatformKimi, AggregateCodeBuddyEnabled: true}
	account := hy3LegacyTestAccount(5081, PlatformCodeBuddy, groupID, 0)
	require.True(t, codeBuddyTierPlatformMismatchAllowed(&account, PlatformKimi, "kimi-k3", groupID, group, tierRoutingPoolMasked))

	account.GroupIDs = []int64{groupID + 1}
	account.AccountGroups = []AccountGroup{{AccountID: account.ID, GroupID: groupID + 1}}
	require.False(t, codeBuddyTierPlatformMismatchAllowed(&account, PlatformKimi, "kimi-k3", groupID, group, tierRoutingPoolMasked), "account must be bound to the actual scheduling group")
}
