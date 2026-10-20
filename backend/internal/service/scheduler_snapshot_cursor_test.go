//go:build unit

package service

import (
	"context"
	"slices"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type cursorSnapshotAccountRepo struct {
	*batchAccountQueryRepo
	account   *Account
	platforms [][]string
}

func (r *cursorSnapshotAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *cursorSnapshotAccountRepo) GetByIDs(context.Context, []int64) ([]*Account, error) {
	return []*Account{r.account}, nil
}

func (r *cursorSnapshotAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	return r.ListSchedulableByGroupIDAndPlatforms(ctx, groupID, []string{platform})
}

func (r *cursorSnapshotAccountRepo) ListSchedulableByGroupIDAndPlatforms(_ context.Context, groupID int64, platforms []string) ([]Account, error) {
	r.platforms = append(r.platforms, slices.Clone(platforms))
	if r.account != nil && slices.Contains(r.account.GroupIDs, groupID) && slices.Contains(platforms, r.account.Platform) {
		return []Account{*r.account}, nil
	}
	return nil, nil
}

type cursorSupplySnapshotCache struct{ *bulkEventSnapshotCache }

func (c *cursorSupplySnapshotCache) GetSnapshot(_ context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	writes := c.writes[bucket]
	if len(writes) == 0 {
		return nil, false, nil
	}
	accounts := writes[len(writes)-1].accounts
	result := make([]*Account, len(accounts))
	for i := range accounts {
		account := accounts[i]
		result[i] = &account
	}
	return result, true, nil
}

func TestSchedulerCursorSnapshotLoadsAllGroupedModes(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGemini} {
		for _, mode := range []string{SchedulerModeSingle, SchedulerModeForced, SchedulerModeMixed} {
			t.Run(platform+"/"+mode, func(t *testing.T) {
				repo := &cursorSnapshotAccountRepo{batchAccountQueryRepo: newBatchAccountQueryRepo(), account: &Account{ID: 901, Platform: PlatformCursor, GroupIDs: []int64{41}}}
				svc := NewSchedulerSnapshotService(nil, nil, repo, nil, &config.Config{RunMode: config.RunModeStandard})
				accounts, err := svc.loadAccountsFromDB(t.Context(), SchedulerBucket{GroupID: 41, Platform: platform, Mode: mode}, mode == SchedulerModeMixed)
				require.NoError(t, err)
				require.Len(t, accounts, 1)
				require.Equal(t, int64(901), accounts[0].ID)
				wantPlatforms := []string{platform}
				if mode == SchedulerModeMixed {
					wantPlatforms = append(wantPlatforms, PlatformAntigravity)
				}
				wantPlatforms = append(wantPlatforms, PlatformCursor)
				require.Equal(t, [][]string{wantPlatforms}, repo.platforms)
			})
		}
	}
}

func TestSchedulerCursorSnapshotCandidateSurvivesCacheHit(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformGemini} {
		for _, forced := range []bool{false, true} {
			repo := &cursorSnapshotAccountRepo{batchAccountQueryRepo: newBatchAccountQueryRepo(), account: &Account{ID: 901, Platform: PlatformCursor, GroupIDs: []int64{41}}}
			cache := &cursorSupplySnapshotCache{newBulkEventSnapshotCache()}
			cfg := &config.Config{RunMode: config.RunModeStandard}
			cfg.Gateway.Scheduling.DbFallbackEnabled = true
			svc := NewSchedulerSnapshotService(cache, nil, repo, nil, cfg)
			groupID := int64(41)
			for range 2 {
				accounts, _, err := svc.ListSchedulableAccounts(t.Context(), &groupID, platform, forced)
				require.NoError(t, err)
				require.Len(t, accounts, 1)
				require.Equal(t, int64(901), accounts[0].ID)
			}
			require.Len(t, repo.platforms, 1, "second lookup must use the published candidate snapshot")
		}
	}
}

func TestSchedulerCursorSnapshotDoesNotBroadenUngroupedOrSimpleQueries(t *testing.T) {
	for _, runMode := range []string{config.RunModeStandard, config.RunModeSimple} {
		for _, platform := range []string{PlatformAnthropic, PlatformGemini} {
			for _, mode := range []string{SchedulerModeSingle, SchedulerModeForced, SchedulerModeMixed} {
				repo := &cursorSnapshotAccountRepo{batchAccountQueryRepo: newBatchAccountQueryRepo(), account: &Account{ID: 901, Platform: PlatformCursor, GroupIDs: []int64{41}}}
				svc := NewSchedulerSnapshotService(nil, nil, repo, nil, &config.Config{RunMode: runMode})
				groupID := int64(0)
				if runMode == config.RunModeSimple {
					groupID = 41 // Simple mode must discard even a supplied group.
				}
				accounts, err := svc.loadAccountsFromDB(t.Context(), SchedulerBucket{GroupID: groupID, Platform: platform, Mode: mode}, mode == SchedulerModeMixed)
				require.NoError(t, err)
				require.Empty(t, repo.platforms, "must not issue a grouped Cursor supply query")
				for _, account := range accounts {
					require.NotEqual(t, PlatformCursor, account.Platform)
				}
			}
		}
	}
}

func TestSchedulerCursorSnapshotAccountEventsRebuildOldAndCurrentGroups(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		for _, removeAll := range []bool{false, true} {
			repo := &cursorSnapshotAccountRepo{batchAccountQueryRepo: newBatchAccountQueryRepo(), account: &Account{ID: 901, Platform: PlatformCursor, GroupIDs: []int64{11}}}
			cache := &cursorSupplySnapshotCache{newBulkEventSnapshotCache()}
			svc := NewSchedulerSnapshotService(cache, nil, repo, nil, &config.Config{RunMode: config.RunModeStandard})
			// Warm both native candidate pools before moving/removing the binding.
			require.NoError(t, svc.rebuildBuckets(t.Context(), schedulerBucketsForTest([]int64{11, 12}, PlatformAnthropic, PlatformGemini), "warm"))
			cache.mu.Lock()
			cache.captures = nil
			cache.mu.Unlock()
			repo.account.GroupIDs = []int64{12}
			if removeAll {
				repo.account.GroupIDs = nil
			}
			seen := make(map[batchSeenKey]struct{})
			if bulk {
				require.NoError(t, svc.handleBulkAccountEvent(t.Context(), bulkEventPayload([]int64{901}, []int64{11}), seen))
			} else {
				id := int64(901)
				require.NoError(t, svc.handleAccountEvent(t.Context(), &id, map[string]any{"group_ids": []any{int64(11)}}, seen))
			}
			nativeGroups, cursorGroups := []int64{11, 12}, []int64{11, 12}
			if removeAll {
				nativeGroups, cursorGroups = []int64{11}, []int64{0, 11}
			}
			wantBuckets := append(schedulerBucketsForTest(nativeGroups, PlatformAnthropic, PlatformGemini), schedulerBucketsForTest(cursorGroups, PlatformCursor)...)
			require.ElementsMatch(t, wantBuckets, cache.capturedBuckets(), "each affected bucket must be rebuilt exactly once")
			for _, platform := range []string{PlatformAnthropic, PlatformGemini} {
				for _, mode := range []string{SchedulerModeSingle, SchedulerModeForced, SchedulerModeMixed} {
					old, hit, err := cache.GetSnapshot(t.Context(), SchedulerBucket{GroupID: 11, Platform: platform, Mode: mode})
					require.NoError(t, err)
					require.True(t, hit)
					require.Empty(t, old, "removed binding must leave the old native snapshot")
					if !removeAll {
						current, hit, err := cache.GetSnapshot(t.Context(), SchedulerBucket{GroupID: 12, Platform: platform, Mode: mode})
						require.NoError(t, err)
						require.True(t, hit)
						require.Len(t, current, 1)
						require.Equal(t, int64(901), current[0].ID)
					}
				}
			}
		}
	}
}

func TestSchedulerCursorSnapshotEventsKeepGroupZeroWithinCursor(t *testing.T) {
	for _, runMode := range []string{config.RunModeStandard, config.RunModeSimple} {
		for _, bulk := range []bool{false, true} {
			repo := &cursorSnapshotAccountRepo{batchAccountQueryRepo: newBatchAccountQueryRepo(), account: &Account{ID: 901, Platform: PlatformCursor}}
			cache := &cursorSupplySnapshotCache{newBulkEventSnapshotCache()}
			svc := NewSchedulerSnapshotService(cache, nil, repo, nil, &config.Config{RunMode: runMode})
			var payloadGroups []int64
			if runMode == config.RunModeSimple {
				repo.account.GroupIDs, payloadGroups = []int64{42}, []int64{41}
			}
			if bulk {
				require.NoError(t, svc.handleBulkAccountEvent(t.Context(), bulkEventPayload([]int64{901}, payloadGroups), nil))
			} else {
				id := int64(901)
				require.NoError(t, svc.handleAccountEvent(t.Context(), &id, bulkEventPayload(nil, payloadGroups), nil))
			}
			require.ElementsMatch(t, schedulerBucketsForTest([]int64{0}, PlatformCursor), cache.capturedBuckets())
			require.Empty(t, repo.platforms)
		}
	}
}
