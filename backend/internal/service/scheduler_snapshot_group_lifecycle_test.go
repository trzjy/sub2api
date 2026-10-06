//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type groupLifecycleTestCache struct {
	*retirementRaceCache

	stateMu sync.Mutex

	leaseHeld       bool
	lease           SchedulerGroupLifecycleLease
	leaseSequence   int
	leaseBusy       bool
	leaseAcquireErr error
	leaseReleaseErr error
	acquireCalls    int
	releaseCalls    int
	acquireTTL      time.Duration
	acquireDeadline bool
	releaseDeadline bool
	releaseCtxErr   error

	listErr   error
	listCalls int

	retireCalls  []SchedulerBucket
	reopenTokens []SchedulerBucketWriteToken
	retireHeld   []bool
	reopenHeld   []bool
	retireErr    error
	retireErrAt  int
	reopenErr    error
	reopenErrAt  int

	bucketLockBusy bool
	bucketLockErr  error
	bucketLockTTLs []time.Duration
	unlockCalls    int
	setErr         error
}

func newGroupLifecycleTestCache(buckets ...SchedulerBucket) *groupLifecycleTestCache {
	return &groupLifecycleTestCache{retirementRaceCache: newRetirementRaceCache(buckets...)}
}

func (c *groupLifecycleTestCache) TryAcquireGroupLifecycleLease(ctx context.Context, groupID int64, ttl time.Duration) (SchedulerGroupLifecycleLease, bool, error) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.acquireCalls++
	c.acquireTTL = ttl
	_, c.acquireDeadline = ctx.Deadline()
	if c.leaseAcquireErr != nil {
		return SchedulerGroupLifecycleLease{}, false, c.leaseAcquireErr
	}
	if c.leaseBusy || c.leaseHeld {
		return SchedulerGroupLifecycleLease{}, false, nil
	}
	c.leaseSequence++
	c.lease = SchedulerGroupLifecycleLease{GroupID: groupID, OwnerToken: fmt.Sprintf("owner-%d", c.leaseSequence)}
	c.leaseHeld = true
	return c.lease, true, nil
}

func (c *groupLifecycleTestCache) ReleaseGroupLifecycleLease(ctx context.Context, lease SchedulerGroupLifecycleLease) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.releaseCalls++
	_, c.releaseDeadline = ctx.Deadline()
	c.releaseCtxErr = ctx.Err()
	if c.leaseReleaseErr != nil {
		return c.leaseReleaseErr
	}
	if !c.leaseHeld || lease != c.lease {
		return ErrSchedulerGroupLifecycleLeaseLost
	}
	c.leaseHeld = false
	return nil
}

func (c *groupLifecycleTestCache) RetireBucket(ctx context.Context, bucket SchedulerBucket) error {
	c.stateMu.Lock()
	c.retireCalls = append(c.retireCalls, bucket)
	c.retireHeld = append(c.retireHeld, c.leaseHeld)
	held := c.leaseHeld
	call := len(c.retireCalls)
	err := c.retireErr
	errAt := c.retireErrAt
	c.stateMu.Unlock()
	if !held {
		return errors.New("retire called outside group lifecycle lease")
	}
	if err != nil && (errAt <= 0 || call == errAt) {
		return err
	}
	return c.retirementRaceCache.RetireBucket(ctx, bucket)
}

func (c *groupLifecycleTestCache) ReopenBucket(ctx context.Context, bucket SchedulerBucket) (SchedulerBucketWriteToken, error) {
	if err := ctx.Err(); err != nil {
		return SchedulerBucketWriteToken{}, err
	}
	c.stateMu.Lock()
	c.reopenHeld = append(c.reopenHeld, c.leaseHeld)
	held := c.leaseHeld
	call := len(c.reopenHeld)
	reopenErr := c.reopenErr
	reopenErrAt := c.reopenErrAt
	c.stateMu.Unlock()
	if !held {
		return SchedulerBucketWriteToken{}, errors.New("reopen called outside group lifecycle lease")
	}
	if reopenErr != nil && (reopenErrAt <= 0 || call == reopenErrAt) {
		return SchedulerBucketWriteToken{}, reopenErr
	}
	token, err := c.retirementRaceCache.ReopenBucket(ctx, bucket)
	if err != nil {
		return SchedulerBucketWriteToken{}, err
	}
	c.stateMu.Lock()
	c.reopenTokens = append(c.reopenTokens, token)
	c.stateMu.Unlock()
	return token, nil
}

func (c *groupLifecycleTestCache) ListBuckets(ctx context.Context) ([]SchedulerBucket, error) {
	c.stateMu.Lock()
	c.listCalls++
	err := c.listErr
	c.stateMu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.retirementRaceCache.ListBuckets(ctx)
}

func (c *groupLifecycleTestCache) TryLockBucket(_ context.Context, _ SchedulerBucket, ttl time.Duration) (bool, error) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.bucketLockTTLs = append(c.bucketLockTTLs, ttl)
	if c.bucketLockErr != nil {
		return false, c.bucketLockErr
	}
	return !c.bucketLockBusy, nil
}

func (c *groupLifecycleTestCache) UnlockBucket(context.Context, SchedulerBucket) error {
	c.stateMu.Lock()
	c.unlockCalls++
	c.stateMu.Unlock()
	return nil
}

func (c *groupLifecycleTestCache) SetSnapshot(ctx context.Context, bucket SchedulerBucket, token SchedulerBucketWriteToken, accounts []Account) error {
	c.stateMu.Lock()
	err := c.setErr
	c.stateMu.Unlock()
	if err != nil {
		return err
	}
	return c.retirementRaceCache.SetSnapshot(ctx, bucket, token, accounts)
}

func (c *groupLifecycleTestCache) lifecycleCounts() (acquires, releases, listCalls int) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.acquireCalls, c.releaseCalls, c.listCalls
}

func (c *groupLifecycleTestCache) retiredBuckets() []SchedulerBucket {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return append([]SchedulerBucket(nil), c.retireCalls...)
}

func (c *groupLifecycleTestCache) tokens() []SchedulerBucketWriteToken {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return append([]SchedulerBucketWriteToken(nil), c.reopenTokens...)
}

func (c *groupLifecycleTestCache) leaseHeldAndTokenCount() (bool, int) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.leaseHeld, len(c.reopenTokens)
}

func (c *groupLifecycleTestCache) lockStats() ([]time.Duration, int) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return append([]time.Duration(nil), c.bucketLockTTLs...), c.unlockCalls
}

func (c *groupLifecycleTestCache) lifecycleMutationLeaseStates() (retire, reopen []bool) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return append([]bool(nil), c.retireHeld...), append([]bool(nil), c.reopenHeld...)
}

type groupLifecycleTestGroupRepo struct {
	GroupRepository

	mu       sync.Mutex
	group    *Group
	err      error
	calls    int
	afterGet func()
}

func (r *groupLifecycleTestGroupRepo) GetByIDLite(context.Context, int64) (*Group, error) {
	r.mu.Lock()
	r.calls++
	if r.err != nil {
		err := r.err
		r.mu.Unlock()
		return nil, err
	}
	if r.group == nil {
		r.mu.Unlock()
		return nil, ErrGroupNotFound
	}
	copyGroup := *r.group
	afterGet := r.afterGet
	r.mu.Unlock()
	if afterGet != nil {
		afterGet()
	}
	return &copyGroup, nil
}

func (r *groupLifecycleTestGroupRepo) set(group *Group, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.group = group
	r.err = err
}

func (r *groupLifecycleTestGroupRepo) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type groupLifecycleTestAccountRepo struct {
	AccountRepository

	mu              sync.Mutex
	calls           int
	callsByPlatform map[string]int
	err             error
	started         chan struct{}
	release         chan struct{}
	once            sync.Once
	beforeLoad      func()
	beforeLoadOnce  sync.Once
}

func (r *groupLifecycleTestAccountRepo) load(ctx context.Context, platform string) ([]Account, error) {
	r.mu.Lock()
	r.calls++
	if r.callsByPlatform == nil {
		r.callsByPlatform = make(map[string]int)
	}
	r.callsByPlatform[platform]++
	err := r.err
	started := r.started
	release := r.release
	r.mu.Unlock()
	if started != nil {
		r.once.Do(func() { close(started) })
	}
	if r.beforeLoad != nil {
		r.beforeLoadOnce.Do(r.beforeLoad)
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return []Account{{ID: 9001, Platform: platform, Status: StatusActive, Schedulable: true}}, nil
}

func (r *groupLifecycleTestAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, platform string) ([]Account, error) {
	return r.load(ctx, platform)
}

func (r *groupLifecycleTestAccountRepo) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, _ int64, platforms []string) ([]Account, error) {
	platform := "mixed"
	if len(platforms) > 0 {
		platform = platforms[0]
	}
	return r.load(ctx, platform)
}

func (r *groupLifecycleTestAccountRepo) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *groupLifecycleTestAccountRepo) platformCallCount(platform string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.callsByPlatform[platform]
}

func newGroupLifecycleTestService(cache SchedulerCache, accounts AccountRepository, groups GroupRepository, runMode string) *SchedulerSnapshotService {
	return NewSchedulerSnapshotService(cache, nil, accounts, groups, &config.Config{RunMode: runMode})
}

func expectedGroupLifecycleBuckets(groupID int64) []SchedulerBucket {
	return schedulerCanonicalBuckets(groupID)
}

// schedulerCanonicalAccountQueryCount 返回单活跃分组（groupID>0）一次 canonical full
// rebuild 触发的账号仓库查询数（按 groupID+platform 去重后）：每平台自身查询 +
// anthropic/gemini 的 mixed 额外查询。候选超集单链（v16 §2.1-3）：分组主查询已经 repo 层
// expandPlatformsForAggregatePool 并入 codebuddy 候选，快照层不再补入（D-13 修复重复合并），
// 故聚合族平台不再有 +1。
func schedulerCanonicalAccountQueryCount() int {
	count := 0
	for _, platform := range schedulerSnapshotPlatforms() {
		count++
		if platform == PlatformAnthropic || platform == PlatformGemini {
			count++
		}
	}
	return count
}

// schedulerCanonicalGroupZeroAccountQueryCount 返回 group0 canonical rebuild（standard
// 未分组 / simple 模式）的账号仓库查询数。ungrouped/simple 查询不经过
// queryAccountsByGroup，快照层 withAggregatedCodeBuddy 是 codebuddy 候选的唯一补入链
// （v16 §2.1-3），故聚合族平台（deepseek/zhipu/kimi/minimax/other）各 +1。
// 复用生产契约判定 isCodeBuddyAggregatedPlatform，禁止硬编码聚合族集合或 @agg 字面量。
// schedulerCanonicalGroupZeroAccountQueryCount 统计标准模式未分组（groupID=0）
// 桶的账号查询数。标准模式未分组桶失败关闭后不再发起 codebuddy 补入查询
// （withAggregatedCodeBuddy 未分组分支直接返回，B2 聚合开关批次），组零查询数
// 回归与分组路径同基数。
func schedulerCanonicalGroupZeroAccountQueryCount() int {
	return schedulerCanonicalAccountQueryCount()
}

// schedulerCanonicalGroupZeroAccountQueryCountSimpleMode 统计 simple 模式未分组
// 桶的账号查询数：simple 分支保留 codebuddy 补入（每聚合平台一次查询）。
func schedulerCanonicalGroupZeroAccountQueryCountSimpleMode() int {
	count := schedulerCanonicalAccountQueryCount()
	for _, platform := range schedulerSnapshotPlatforms() {
		if isCodeBuddyAggregatedPlatform(platform) && platform != PlatformCodeBuddy {
			count++
		}
	}
	return count
}

func bucketStrings(buckets []SchedulerBucket) map[string]struct{} {
	out := make(map[string]struct{}, len(buckets))
	for _, bucket := range buckets {
		out[bucket.String()] = struct{}{}
	}
	return out
}

func requireLifecycleSeen(t *testing.T, seen map[batchSeenKey]struct{}, groupID int64) {
	t.Helper()
	_, ok := seen[batchSeenKey{groupID: groupID, lifecycle: true}]
	require.True(t, ok)
	for _, platform := range schedulerSnapshotPlatforms() {
		_, ok = seen[batchSeenKey{groupID: groupID, platform: platform}]
		require.True(t, ok)
	}
}

func requireLifecycleNotSeen(t *testing.T, seen map[batchSeenKey]struct{}, groupID int64) {
	t.Helper()
	_, ok := seen[batchSeenKey{groupID: groupID, lifecycle: true}]
	require.False(t, ok)
	for _, platform := range schedulerSnapshotPlatforms() {
		_, ok = seen[batchSeenKey{groupID: groupID, platform: platform}]
		require.False(t, ok)
	}
}

func TestSchedulerGroupLifecycleInactiveAndMissingRetireAllHistoricalBucketsWithoutAccountReads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		group *Group
		err   error
	}{
		{name: "inactive", group: &Group{ID: 81, Status: StatusDisabled, Hydrated: true}},
		{name: "missing", err: ErrGroupNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const groupID int64 = 81
			current := expectedGroupLifecycleBuckets(groupID)
			historical := SchedulerBucket{GroupID: groupID, Platform: "legacy", Mode: "obsolete"}
			other := SchedulerBucket{GroupID: groupID + 1, Platform: PlatformOpenAI, Mode: SchedulerModeForced}
			groupZero := SchedulerBucket{GroupID: 0, Platform: PlatformOpenAI, Mode: SchedulerModeForced}
			cache := newGroupLifecycleTestCache(current[0], historical, other, groupZero)
			groups := &groupLifecycleTestGroupRepo{group: tc.group, err: tc.err}
			accounts := &groupLifecycleTestAccountRepo{}
			svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)
			seen := make(map[batchSeenKey]struct{})

			require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), seen))

			expected := bucketStrings(append(current, historical))
			got := bucketStrings(cache.retiredBuckets())
			require.Equal(t, expected, got)
			retireHeld, _ := cache.lifecycleMutationLeaseStates()
			require.Len(t, retireHeld, len(expected))
			for _, held := range retireHeld {
				require.True(t, held)
			}
			require.NotContains(t, got, other.String())
			require.NotContains(t, got, groupZero.String())
			require.Zero(t, accounts.callCount())
			require.Equal(t, 1, groups.callCount())
			_, _, listCalls := cache.lifecycleCounts()
			require.Equal(t, 1, listCalls)
			requireLifecycleSeen(t, seen, groupID)
		})
	}
}

func TestSchedulerPrepareGroupLifecycleUsesKnownHistoricalBucketsWithoutListingRegistry(t *testing.T) {
	const groupID int64 = 811
	historical := SchedulerBucket{GroupID: groupID, Platform: "legacy", Mode: "obsolete"}
	cache := newGroupLifecycleTestCache()
	cache.listErr = errors.New("registry must not be listed")
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Status: StatusDisabled, Hydrated: true}}
	accounts := &groupLifecycleTestAccountRepo{}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)

	plan, err := svc.prepareGroupLifecycle(context.Background(), groupID, []SchedulerBucket{historical})
	require.NoError(t, err)
	require.False(t, plan.active)
	require.Empty(t, plan.tasks)
	_, _, listCalls := cache.lifecycleCounts()
	require.Zero(t, listCalls)
	require.Contains(t, bucketStrings(cache.retiredBuckets()), historical.String())
	require.Zero(t, accounts.callCount())
}

func TestSchedulerGroupLifecycleActiveReopensAndRebuildsAllCurrentBuckets(t *testing.T) {
	const groupID int64 = 82
	current := expectedGroupLifecycleBuckets(groupID)
	historical := SchedulerBucket{GroupID: groupID, Platform: "legacy", Mode: "obsolete"}
	cache := newGroupLifecycleTestCache(historical)
	for _, bucket := range current {
		require.NoError(t, cache.retirementRaceCache.RetireBucket(context.Background(), bucket))
	}
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}}
	accounts := &groupLifecycleTestAccountRepo{}
	accounts.beforeLoad = func() {
		held, tokenCount := cache.leaseHeldAndTokenCount()
		require.False(t, held, "the group lifecycle lease must be released before the first account query")
		require.Equal(t, len(current), tokenCount, "all reopen tokens must be prepared before the first account query")
	}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)
	seen := make(map[batchSeenKey]struct{})

	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), seen))

	require.Equal(t, bucketStrings(current), bucketStrings(cache.reopens))
	require.Empty(t, cache.retiredBuckets())
	registered, err := cache.retirementRaceCache.ListBuckets(context.Background())
	require.NoError(t, err)
	require.Contains(t, bucketStrings(registered), historical.String())
	require.Len(t, cache.tokens(), len(current))
	require.Equal(t, schedulerCanonicalAccountQueryCount(), accounts.callCount())
	require.Equal(t, 1, accounts.platformCallCount(PlatformOpenAI))
	for _, bucket := range current {
		_, published := cache.counts(bucket)
		require.Equal(t, 1, published, bucket.String())
	}
	require.Contains(t, bucketStrings(current), SchedulerBucket{GroupID: groupID, Platform: PlatformAntigravity, Mode: SchedulerModeForced}.String())
	require.Contains(t, bucketStrings(current), SchedulerBucket{GroupID: groupID, Platform: PlatformAnthropic, Mode: SchedulerModeMixed}.String())
	require.Contains(t, bucketStrings(current), SchedulerBucket{GroupID: groupID, Platform: PlatformGemini, Mode: SchedulerModeMixed}.String())
	acquires, releases, listCalls := cache.lifecycleCounts()
	require.Equal(t, 1, acquires)
	require.Equal(t, 1, releases)
	require.Zero(t, listCalls)
	require.Equal(t, schedulerGroupLifecycleLeaseTTL, cache.acquireTTL)
	require.True(t, cache.acquireDeadline)
	require.True(t, cache.releaseDeadline)
	require.NoError(t, cache.releaseCtxErr)
	_, reopenHeld := cache.lifecycleMutationLeaseStates()
	require.Len(t, reopenHeld, len(current))
	for _, held := range reopenHeld {
		require.True(t, held)
	}
	lockTTLs, unlockCalls := cache.lockStats()
	require.Len(t, lockTTLs, len(current))
	for _, ttl := range lockTTLs {
		require.Equal(t, 30*time.Second, ttl)
	}
	require.Equal(t, len(current), unlockCalls)
	requireLifecycleSeen(t, seen, groupID)
}

func TestSchedulerGroupLifecycleInactiveThenActiveAuthoritativelyReopens(t *testing.T) {
	const groupID int64 = 83
	cache := newGroupLifecycleTestCache()
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Status: StatusDisabled, Hydrated: true}}
	accounts := &groupLifecycleTestAccountRepo{}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)

	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	require.Zero(t, accounts.callCount())
	groups.set(&Group{ID: groupID, Status: StatusActive, Hydrated: true}, nil)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))

	require.Len(t, cache.tokens(), schedulerCanonicalBucketCount())
	require.Equal(t, schedulerCanonicalAccountQueryCount(), accounts.callCount())
	for _, bucket := range expectedGroupLifecycleBuckets(groupID) {
		_, published := cache.counts(bucket)
		require.Equal(t, 1, published, bucket.String())
	}
}

func TestSchedulerGroupLifecycleLaterInactiveFencesLongActiveRebuild(t *testing.T) {
	const groupID int64 = 84
	cache := newGroupLifecycleTestCache()
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Status: StatusActive, Hydrated: true}}
	started := make(chan struct{})
	release := make(chan struct{})
	accounts := &groupLifecycleTestAccountRepo{started: started, release: release}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)
	activeSeen := make(map[batchSeenKey]struct{})
	inactiveSeen := make(map[batchSeenKey]struct{})
	activeResult := make(chan error, 1)

	go func() {
		activeResult <- svc.handleGroupEvent(context.Background(), ptrInt64(groupID), activeSeen)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active rebuild did not reach the account load")
	}

	groups.set(&Group{ID: groupID, Status: StatusDisabled, Hydrated: true}, nil)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), inactiveSeen))
	close(release)
	err := <-activeResult
	require.ErrorIs(t, err, ErrSchedulerBucketRetired)
	requireLifecycleNotSeen(t, activeSeen, groupID)
	requireLifecycleSeen(t, inactiveSeen, groupID)
}

func TestSchedulerGroupLifecycleEpochPreventsABA(t *testing.T) {
	const groupID int64 = 85
	cache := newGroupLifecycleTestCache()
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Status: StatusDisabled, Hydrated: true}}
	accounts := &groupLifecycleTestAccountRepo{}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)

	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	groups.set(&Group{ID: groupID, Status: StatusActive, Hydrated: true}, nil)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	firstActiveTokens := cache.tokens()
	canonicalCount := schedulerCanonicalBucketCount()
	require.Len(t, firstActiveTokens, canonicalCount)

	groups.set(&Group{ID: groupID, Status: StatusDisabled, Hydrated: true}, nil)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	groups.set(&Group{ID: groupID, Status: StatusActive, Hydrated: true}, nil)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	allTokens := cache.tokens()
	require.Len(t, allTokens, 2*canonicalCount)
	require.Greater(t, allTokens[canonicalCount].Epoch, firstActiveTokens[0].Epoch)
	require.ErrorIs(t, cache.SetSnapshot(context.Background(), firstActiveTokens[0].Bucket, firstActiveTokens[0], nil), ErrSchedulerBucketWriteFenced)
}

func TestSchedulerGroupLifecycleSeenIsIndependentAndDeduplicatesGroupEvents(t *testing.T) {
	const groupID int64 = 86
	cache := newGroupLifecycleTestCache()
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Status: StatusActive, Hydrated: true}}
	accounts := &groupLifecycleTestAccountRepo{}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)
	seen := make(map[batchSeenKey]struct{})
	for _, platform := range schedulerSnapshotPlatforms() {
		seen[batchSeenKey{groupID: groupID, platform: platform}] = struct{}{}
	}

	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), seen))
	require.Equal(t, 1, groups.callCount())
	require.Equal(t, schedulerCanonicalAccountQueryCount(), accounts.callCount())
	requireLifecycleSeen(t, seen, groupID)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), seen))
	require.Equal(t, 1, groups.callCount())
	require.Equal(t, schedulerCanonicalAccountQueryCount(), accounts.callCount())
}

func TestSchedulerGroupLifecycleFailuresDoNotMarkSeen(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*groupLifecycleTestCache, *groupLifecycleTestGroupRepo, *groupLifecycleTestAccountRepo)
		check   func(*testing.T, error)
	}{
		{
			name: "lease busy",
			prepare: func(cache *groupLifecycleTestCache, _ *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				cache.leaseBusy = true
			},
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, ErrSchedulerGroupLifecycleLeaseBusy) },
		},
		{
			name: "lease error",
			prepare: func(cache *groupLifecycleTestCache, _ *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				cache.leaseAcquireErr = errors.New("lease failed")
			},
			check: func(t *testing.T, err error) { require.EqualError(t, err, "lease failed") },
		},
		{
			name: "release lost",
			prepare: func(cache *groupLifecycleTestCache, _ *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				cache.leaseReleaseErr = ErrSchedulerGroupLifecycleLeaseLost
			},
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, ErrSchedulerGroupLifecycleLeaseLost) },
		},
		{
			name: "release error",
			prepare: func(cache *groupLifecycleTestCache, _ *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				cache.leaseReleaseErr = errors.New("release failed")
			},
			check: func(t *testing.T, err error) { require.EqualError(t, err, "release failed") },
		},
		{
			name: "group query error",
			prepare: func(_ *groupLifecycleTestCache, groups *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				groups.err = errors.New("group query failed")
			},
			check: func(t *testing.T, err error) { require.EqualError(t, err, "group query failed") },
		},
		{
			name: "list buckets error",
			prepare: func(cache *groupLifecycleTestCache, groups *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				groups.group.Status = StatusDisabled
				cache.listErr = errors.New("list buckets failed")
			},
			check: func(t *testing.T, err error) { require.EqualError(t, err, "list buckets failed") },
		},
		{
			name: "retire bucket error",
			prepare: func(cache *groupLifecycleTestCache, groups *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				groups.group.Status = StatusDisabled
				cache.retireErr = errors.New("retire bucket failed")
				cache.retireErrAt = 2
			},
			check: func(t *testing.T, err error) { require.EqualError(t, err, "retire bucket failed") },
		},
		{
			name: "reopen bucket error",
			prepare: func(cache *groupLifecycleTestCache, _ *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				cache.reopenErr = errors.New("reopen bucket failed")
				cache.reopenErrAt = 2
			},
			check: func(t *testing.T, err error) { require.EqualError(t, err, "reopen bucket failed") },
		},
		{
			name: "account rebuild error",
			prepare: func(_ *groupLifecycleTestCache, _ *groupLifecycleTestGroupRepo, accounts *groupLifecycleTestAccountRepo) {
				accounts.err = errors.New("account load failed")
			},
			check: func(t *testing.T, err error) { require.EqualError(t, err, "account load failed") },
		},
		{
			name: "bucket lock busy",
			prepare: func(cache *groupLifecycleTestCache, _ *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				cache.bucketLockBusy = true
			},
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, ErrSchedulerBucketRebuildBusy) },
		},
		{
			name: "bucket lock error",
			prepare: func(cache *groupLifecycleTestCache, _ *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				cache.bucketLockErr = errors.New("bucket lock failed")
			},
			check: func(t *testing.T, err error) { require.EqualError(t, err, "bucket lock failed") },
		},
		{
			name: "set snapshot error",
			prepare: func(cache *groupLifecycleTestCache, _ *groupLifecycleTestGroupRepo, _ *groupLifecycleTestAccountRepo) {
				cache.setErr = errors.New("set snapshot failed")
			},
			check: func(t *testing.T, err error) { require.EqualError(t, err, "set snapshot failed") },
		},
	}

	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(870 + index)
			cache := newGroupLifecycleTestCache()
			groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Status: StatusActive, Hydrated: true}}
			accounts := &groupLifecycleTestAccountRepo{}
			tc.prepare(cache, groups, accounts)
			svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)
			seen := make(map[batchSeenKey]struct{})

			err := svc.handleGroupEvent(context.Background(), ptrInt64(groupID), seen)
			tc.check(t, err)
			requireLifecycleNotSeen(t, seen, groupID)
			if tc.name == "release lost" || tc.name == "release error" {
				require.Zero(t, accounts.callCount())
			}
			if tc.name == "retire bucket error" || tc.name == "reopen bucket error" {
				_, releases, _ := cache.lifecycleCounts()
				require.Equal(t, 1, releases)
				require.Zero(t, accounts.callCount())
			}
			if tc.name == "account rebuild error" || tc.name == "set snapshot error" {
				lockTTLs, unlockCalls := cache.lockStats()
				require.Len(t, lockTTLs, 1)
				require.Equal(t, 1, unlockCalls)
				require.Equal(t, 1, accounts.callCount())
			}
		})
	}
}

func TestSchedulerGroupLifecycleOperationAndReleaseErrorsPreserveBothCauses(t *testing.T) {
	const groupID int64 = 880
	operationErr := errors.New("group query failed")
	cache := newGroupLifecycleTestCache()
	cache.leaseReleaseErr = ErrSchedulerGroupLifecycleLeaseLost
	groups := &groupLifecycleTestGroupRepo{err: operationErr}
	accounts := &groupLifecycleTestAccountRepo{}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)
	seen := make(map[batchSeenKey]struct{})

	err := svc.handleGroupEvent(context.Background(), ptrInt64(groupID), seen)
	require.ErrorIs(t, err, operationErr)
	require.ErrorIs(t, err, ErrSchedulerGroupLifecycleLeaseLost)
	requireLifecycleNotSeen(t, seen, groupID)
	require.Zero(t, accounts.callCount())
}

func TestSchedulerGroupLifecycleUntrustedGroupStateFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		group *Group
	}{
		{name: "not hydrated", group: &Group{ID: 88, Status: StatusActive}},
		{name: "mismatched id", group: &Group{ID: 89, Status: StatusActive, Hydrated: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const eventGroupID int64 = 88
			cache := newGroupLifecycleTestCache()
			groups := &groupLifecycleTestGroupRepo{group: tc.group}
			accounts := &groupLifecycleTestAccountRepo{}
			svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)
			seen := make(map[batchSeenKey]struct{})

			err := svc.handleGroupEvent(context.Background(), ptrInt64(eventGroupID), seen)
			require.Error(t, err)
			require.Empty(t, cache.retiredBuckets())
			require.Empty(t, cache.tokens())
			require.Zero(t, accounts.callCount())
			requireLifecycleNotSeen(t, seen, eventGroupID)
			acquires, releases, listCalls := cache.lifecycleCounts()
			require.Equal(t, 1, acquires)
			require.Equal(t, 1, releases)
			require.Zero(t, listCalls)
		})
	}
}

func TestSchedulerGroupLifecycleCanceledAfterFreshQueryUsesIndependentReleaseContext(t *testing.T) {
	const groupID int64 = 89
	ctx, cancel := context.WithCancel(context.Background())
	cache := newGroupLifecycleTestCache()
	groups := &groupLifecycleTestGroupRepo{
		group:    &Group{ID: groupID, Status: StatusActive, Hydrated: true},
		afterGet: cancel,
	}
	accounts := &groupLifecycleTestAccountRepo{}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)
	seen := make(map[batchSeenKey]struct{})

	err := svc.handleGroupEvent(ctx, ptrInt64(groupID), seen)
	require.ErrorIs(t, err, context.Canceled)
	requireLifecycleNotSeen(t, seen, groupID)
	require.Empty(t, cache.tokens())
	require.Zero(t, accounts.callCount())
	acquires, releases, _ := cache.lifecycleCounts()
	require.Equal(t, 1, acquires)
	require.Equal(t, 1, releases)
	require.True(t, cache.releaseDeadline)
	require.NoError(t, cache.releaseCtxErr)
}

func TestSchedulerGroupLifecycleGroupZeroAndSimpleModeAreNoOps(t *testing.T) {
	cache := newGroupLifecycleTestCache()
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: 88, Status: StatusActive, Hydrated: true}}
	accounts := &groupLifecycleTestAccountRepo{}
	standard := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)
	simple := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeSimple)

	require.NoError(t, standard.handleGroupEvent(context.Background(), nil, make(map[batchSeenKey]struct{})))
	require.NoError(t, standard.handleGroupEvent(context.Background(), ptrInt64(0), make(map[batchSeenKey]struct{})))
	require.NoError(t, simple.handleGroupEvent(context.Background(), ptrInt64(88), make(map[batchSeenKey]struct{})))

	acquires, releases, listCalls := cache.lifecycleCounts()
	require.Zero(t, acquires)
	require.Zero(t, releases)
	require.Zero(t, listCalls)
	require.Zero(t, groups.callCount())
	require.Zero(t, accounts.callCount())
}

// aggregatedSupersetAccountRepo 模拟 repo 层 expandPlatformsForAggregatePool 已生效的
// aggregatedSupersetAccountRepo 模拟聚合分组候选超集：
// 分组主查询（ListSchedulableByGroupIDAndPlatform(s)）在聚合族平台（如 deepseek）上按
// enabled 开关决定是否并入 codebuddy 账号——enabled 模拟 groups 行
// aggregate_codebuddy_enabled 的权威值（B2-B 翻转用例借其切换，无需新建 groupRepo/DB stub）；
// ungrouped/simple 查询不经过该扩展，原生平台与 codebuddy 各自独立返回。
type aggregatedSupersetAccountRepo struct {
	AccountRepository

	mu            sync.Mutex
	groupQueries  []string
	ungroupedCode int
	enabled       bool
}

// setEnabled 切换聚合直绑开关的权威值（模拟 groups 行 aggregate_codebuddy_enabled 翻转）。
func (r *aggregatedSupersetAccountRepo) setEnabled(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = enabled
}

// groupQuery 执行分组主查询，按 enabled 决定是否并入 codebuddy（与 repo 层
// expandPlatformsForAggregatePool + queryAccountsByGroup 单点读取语义一致）。
func (r *aggregatedSupersetAccountRepo) groupQuery(platform string) ([]Account, error) {
	r.mu.Lock()
	r.groupQueries = append(r.groupQueries, platform)
	enabled := r.enabled
	r.mu.Unlock()
	if isCodeBuddyAggregatedPlatform(platform) && platform != PlatformCodeBuddy && enabled {
		// 开关开启：repo 层候选池已并入 codebuddy（方案 §2.1-3 唯一纳入点）。
		return []Account{
			{ID: 9101, Platform: platform, Status: StatusActive, Schedulable: true},
			{ID: 9102, Platform: PlatformCodeBuddy, Status: StatusActive, Schedulable: true},
		}, nil
	}
	// 开关关闭或平台非聚合族：仅原生平台账号，绝不并入 codebuddy。
	return []Account{{ID: 9101, Platform: platform, Status: StatusActive, Schedulable: true}}, nil
}

func (r *aggregatedSupersetAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]Account, error) {
	return r.groupQuery(platform)
}

func (r *aggregatedSupersetAccountRepo) ListSchedulableByGroupIDAndPlatforms(_ context.Context, _ int64, platforms []string) ([]Account, error) {
	platform := "mixed"
	if len(platforms) > 0 {
		platform = platforms[0]
	}
	return r.groupQuery(platform)
}

func (r *aggregatedSupersetAccountRepo) ListSchedulableUngroupedByPlatform(_ context.Context, platform string) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if platform == PlatformCodeBuddy {
		r.ungroupedCode++
		return []Account{{ID: 9202, Platform: PlatformCodeBuddy, Status: StatusActive, Schedulable: true}}, nil
	}
	return []Account{{ID: 9201, Platform: platform, Status: StatusActive, Schedulable: true}}, nil
}

func (r *aggregatedSupersetAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]Account, error) {
	if platform == PlatformCodeBuddy {
		return []Account{{ID: 9202, Platform: PlatformCodeBuddy, Status: StatusActive, Schedulable: true}}, nil
	}
	return []Account{{ID: 9201, Platform: platform, Status: StatusActive, Schedulable: true}}, nil
}

func (r *aggregatedSupersetAccountRepo) ListSchedulableByPlatforms(_ context.Context, platforms []string) ([]Account, error) {
	platform := "mixed"
	if len(platforms) > 0 {
		platform = platforms[0]
	}
	if platform == PlatformCodeBuddy {
		return []Account{{ID: 9202, Platform: PlatformCodeBuddy, Status: StatusActive, Schedulable: true}}, nil
	}
	return []Account{{ID: 9201, Platform: platform, Status: StatusActive, Schedulable: true}}, nil
}

func (r *aggregatedSupersetAccountRepo) ListSchedulableUngroupedByPlatforms(_ context.Context, platforms []string) ([]Account, error) {
	platform := "mixed"
	if len(platforms) > 0 {
		platform = platforms[0]
	}
	if platform == PlatformCodeBuddy {
		return []Account{{ID: 9202, Platform: PlatformCodeBuddy, Status: StatusActive, Schedulable: true}}, nil
	}
	return []Account{{ID: 9201, Platform: platform, Status: StatusActive, Schedulable: true}}, nil
}

func (r *aggregatedSupersetAccountRepo) groupQueryPlatforms() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.groupQueries...)
}

func (r *aggregatedSupersetAccountRepo) ungroupedCodeBuddyQueries() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ungroupedCode
}

// 候选超集单链回归（D-13 / v16 §2.1-3）+ B2-B 未分组失败关闭：
// groupID>0 聚合桶加载后候选集账号 ID 无重复——repo 层已并入 codebuddy 候选，
// 快照层 withAggregatedCodeBuddy 不得再查询再 append；
// 标准模式未分组桶（groupID=0）失败关闭，不补入 codebuddy（无分组即无绑定授权）；
// simple 模式未分组桶保持既有补入语义（派发单禁区：不为 simple 新增失败关闭/开关语义）。
func TestSchedulerAggregatedBucketGroupedCandidatesHaveNoDuplicateAccountIDs(t *testing.T) {
	t.Run("grouped skips snapshot-layer codebuddy merge", func(t *testing.T) {
		accounts := &aggregatedSupersetAccountRepo{}
		accounts.setEnabled(true)
		svc := newGroupLifecycleTestService(nil, accounts, nil, config.RunModeStandard)
		bucket := SchedulerBucket{GroupID: 90, Platform: schedulerAggregationBucketPlatform(PlatformDeepseek), Mode: SchedulerModeSingle}

		loaded, err := svc.loadAccountsFromDB(context.Background(), bucket, false)
		require.NoError(t, err)

		seenIDs := make(map[int64]struct{}, len(loaded))
		for _, account := range loaded {
			_, dup := seenIDs[account.ID]
			require.False(t, dup, "duplicate account id %d in grouped aggregated bucket", account.ID)
			seenIDs[account.ID] = struct{}{}
		}
		require.Len(t, loaded, 2)
		require.Equal(t, []string{PlatformDeepseek}, accounts.groupQueryPlatforms(),
			"snapshot layer must not re-query codebuddy for grouped buckets")
	})

	t.Run("ungrouped standard mode fails closed (no codebuddy merge)", func(t *testing.T) {
		accounts := &aggregatedSupersetAccountRepo{}
		svc := newGroupLifecycleTestService(nil, accounts, nil, config.RunModeStandard)
		bucket := SchedulerBucket{GroupID: 0, Platform: schedulerAggregationBucketPlatform(PlatformDeepseek), Mode: SchedulerModeSingle}

		loaded, err := svc.loadAccountsFromDB(context.Background(), bucket, false)
		require.NoError(t, err)

		require.Len(t, loaded, 1, "standard-mode ungrouped bucket must fail closed: no codebuddy merge")
		require.False(t, hasCodeBuddyAccount(loaded), "standard-mode ungrouped bucket must not contain codebuddy")
		require.Equal(t, 0, accounts.ungroupedCodeBuddyQueries(),
			"snapshot layer must not query codebuddy for standard-mode ungrouped buckets")
	})

	t.Run("ungrouped simple mode keeps codebuddy merge", func(t *testing.T) {
		accounts := &aggregatedSupersetAccountRepo{}
		svc := newGroupLifecycleTestService(nil, accounts, nil, config.RunModeSimple)
		bucket := SchedulerBucket{GroupID: 0, Platform: schedulerAggregationBucketPlatform(PlatformDeepseek), Mode: SchedulerModeSingle}

		loaded, err := svc.loadAccountsFromDB(context.Background(), bucket, false)
		require.NoError(t, err)

		require.Len(t, loaded, 2, "simple-mode ungrouped bucket must still merge codebuddy candidates")
		require.True(t, hasCodeBuddyAccount(loaded), "simple-mode ungrouped bucket must contain codebuddy")
	})
}

// hasCodeBuddyAccount 报告候选集中是否含 codebuddy 平台账号。
func hasCodeBuddyAccount(accounts []Account) bool {
	for _, account := range accounts {
		if account.Platform == PlatformCodeBuddy {
			return true
		}
	}
	return false
}

// accountIDPlatforms 返回账号集的 id:platform 快照，用于比较两次重建结果是否逐位一致。
func accountIDPlatforms(accounts []Account) []string {
	out := make([]string, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, fmt.Sprintf("%d:%s", account.ID, account.Platform))
	}
	return out
}

// ============================================================================
// B2-B 第 2/3 条 + §5 矩阵：聚合直绑开关翻转、收敛、三维状态矩阵补缺
// ============================================================================
//
// 行 2 消费点登记（禁用优先于在途）——随 diff 落盘，外审 F3：
// 账号状态权威链上"实际发送路径"各消费点，逐一核对是否消费 schedulable 状态：
//   ① 快照已加载候选 → 选号结果：openai_account_scheduler.go:1646
//      `if !account.IsSchedulable() { filterStats.exclude("not_schedulable"); continue }`
//      —— 快照仅存候选超集，每次选号实时重判 IsSchedulable，禁用账号不会被选中。
//   ② 选号结果/租约（粘性会话）：openai_account_scheduler.go:718
//      `... || !account.IsSchedulable()` → clearBinding() 立即清除已建立的粘性绑定。
//      既有用例钉死：TestShouldClearStickySession「schedulable false → want:true（清除）」。
//   ③ 转发执行前校验：openai_gateway_scheduling.go:400（IsSchedulableForModelWithContext）
//      / gateway_service.go:562（account.IsSchedulable()）—— 转发前再次否决禁用账号。
//   ④ 续租：账号调度无独立"在途续租"路径持有已禁用账号；租约串行化仅用于分组生命周期
//      重建（TryAcquireGroupLifecycleLease，scheduler_snapshot_service.go:766），
//      不延长已禁用账号的使用窗口——非缺陷，属既有设计边界。
//   ⑤ 重试换号：重试回退到选号路径（①②③ 同一重判），按"新请求"重新过滤，
//      不复用已被禁用的已选账号。
// 结论：5 个消费点中 ①②③⑤ 均实时消费 IsSchedulable（Account.IsSchedulable 以
// IsActive()&&Schedulable 为真），④ 无独立在途持有路径；未发现"某消费点确实未消费
// schedulable"的真实缺陷，故不触发 BLOCKED。下列 TestSchedulerGroupAggregateMatrixRow2*
// 以可运行断言钉死底层不变量。

// TestSchedulerGroupAggregateMatrixRow2DisablePreemptsInflight 钉死行 2 的底层不变量：
// schedulable=false 的账号 IsSchedulable() 必为 false，使上述 ①②③⑤ 全部消费点正确
// 排除（禁用优先于在途）。具体 sticky 清除已由 TestShouldClearStickySession 钉死。
func TestSchedulerGroupAggregateMatrixRow2DisablePreemptsInflight(t *testing.T) {
	disabled := &Account{ID: 1, Status: StatusActive, Schedulable: false}
	require.False(t, disabled.IsSchedulable(), "schedulable=false 必须使 IsSchedulable()=false（禁用优先于在途的底层不变量）")
	inactive := &Account{ID: 2, Status: StatusDisabled, Schedulable: true}
	require.False(t, inactive.IsSchedulable(), "Status=disabled 必须使 IsSchedulable()=false")
	healthy := &Account{ID: 3, Status: StatusActive, Schedulable: true}
	require.True(t, healthy.IsSchedulable(), "active 且 schedulable 的账号必须可调度")
}

// capturingSnapshotCache 在既有 groupLifecycleTestCache 基建之上捕获每次 SetSnapshot
// 写入的账号集，供翻转/收敛用例断言"重建结果"等于权威开关值（B2-B 第 2/3 条）。
type capturingSnapshotCache struct {
	*groupLifecycleTestCache
	mu        sync.Mutex
	snapshots map[string][]Account
}

func (c *capturingSnapshotCache) SetSnapshot(ctx context.Context, bucket SchedulerBucket, token SchedulerBucketWriteToken, accounts []Account) error {
	c.mu.Lock()
	if c.snapshots == nil {
		c.snapshots = make(map[string][]Account)
	}
	c.snapshots[bucket.String()] = append([]Account(nil), accounts...)
	c.mu.Unlock()
	return c.groupLifecycleTestCache.SetSnapshot(ctx, bucket, token, accounts)
}

func (c *capturingSnapshotCache) snapshotFor(bucket SchedulerBucket) []Account {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Account(nil), c.snapshots[bucket.String()]...)
}

// B2-B 第 2 条：开关翻转（开→关）后 handleGroupEvent 重建该分组桶，重建结果不含 codebuddy。
func TestSchedulerGroupAggregateSwitchFlipOnToOff(t *testing.T) {
	const groupID int64 = 7101
	bucket := SchedulerBucket{GroupID: groupID, Platform: schedulerAggregationBucketPlatform(PlatformDeepseek), Mode: SchedulerModeSingle}
	accounts := &aggregatedSupersetAccountRepo{}
	accounts.setEnabled(true)
	cache := &capturingSnapshotCache{groupLifecycleTestCache: newGroupLifecycleTestCache()}
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Platform: PlatformDeepseek, Status: StatusActive, Hydrated: true}}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)

	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	require.True(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "switch ON: rebuilt grouped aggregated bucket must contain codebuddy")

	accounts.setEnabled(false)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	require.False(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "switch OFF after flip: rebuilt grouped aggregated bucket must NOT contain codebuddy")
}

// B2-B 第 2 条：开关翻转（关→开）反向。
func TestSchedulerGroupAggregateSwitchFlipOffToOn(t *testing.T) {
	const groupID int64 = 7102
	bucket := SchedulerBucket{GroupID: groupID, Platform: schedulerAggregationBucketPlatform(PlatformDeepseek), Mode: SchedulerModeSingle}
	accounts := &aggregatedSupersetAccountRepo{}
	accounts.setEnabled(false)
	cache := &capturingSnapshotCache{groupLifecycleTestCache: newGroupLifecycleTestCache()}
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Platform: PlatformDeepseek, Status: StatusActive, Hydrated: true}}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)

	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	require.False(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "switch OFF: rebuilt grouped aggregated bucket must NOT contain codebuddy")

	accounts.setEnabled(true)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	require.True(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "switch ON after flip: rebuilt grouped aggregated bucket must contain codebuddy")
}

// B2-B 第 3 条：翻转收敛。覆盖融合方案 §5 钉死的并发启停必测 + 连续翻转 + 事件延迟/乱序。
// 断言：每次重建在"执行时刻"实时读权威（accounts.enabled 即 groups 行开关，无缓存旁路），
// 最终桶状态恒等于最后设置的权威值。
func TestSchedulerGroupAggregateSwitchConvergence(t *testing.T) {
	const groupID int64 = 7201
	bucket := SchedulerBucket{GroupID: groupID, Platform: schedulerAggregationBucketPlatform(PlatformDeepseek), Mode: SchedulerModeSingle}
	accounts := &aggregatedSupersetAccountRepo{}
	cache := &capturingSnapshotCache{groupLifecycleTestCache: newGroupLifecycleTestCache()}
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Platform: PlatformDeepseek, Status: StatusActive, Hydrated: true}}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)
	drive := func(enabled bool) {
		accounts.setEnabled(enabled)
		// 每次事件独立 seen，模拟不同 outbox 事件独立消费（无 dedup 短路）。
		require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	}

	t.Run("continuous on->off->on converges to authority", func(t *testing.T) {
		drive(true)
		require.True(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "after ON: bucket must contain codebuddy")
		drive(false)
		require.False(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "after OFF: bucket must NOT contain codebuddy")
		drive(true)
		require.True(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "after ON again: bucket must converge back to codebuddy")
	})

	t.Run("concurrent off+on converges to last authority", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			accounts.setEnabled(false)
			_ = svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{}))
		}()
		go func() {
			defer wg.Done()
			accounts.setEnabled(true)
			_ = svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{}))
		}()
		wg.Wait()
		// 租约串行化重建；最后权威值设为 ON 再驱动一次以确定态。
		drive(true)
		require.True(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "concurrent flip: final authority ON => bucket contains codebuddy")
		require.Equal(t, []string{"9101:" + PlatformDeepseek, "9102:" + PlatformCodeBuddy}, accountIDPlatforms(cache.snapshotFor(bucket)))
	})

	t.Run("out-of-order delivery converges to last authority", func(t *testing.T) {
		// ON 事件先到，OFF 事件最后到达（OFF 为最终权威）。
		accounts.setEnabled(true)
		require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
		require.True(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "first ON event: bucket contains codebuddy")
		accounts.setEnabled(false)
		require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
		require.False(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "out-of-order last authority OFF => bucket must NOT contain codebuddy")
	})
}

// B2-B 第 1.3 条：翻转不影响 codebuddy 自有分组与非聚合分组桶（方案 §1.3 收敛语义附则）。
func TestSchedulerGroupAggregateSwitchFlipDoesNotAffectOtherBuckets(t *testing.T) {
	const groupID int64 = 7501
	accounts := &aggregatedSupersetAccountRepo{}
	cache := &capturingSnapshotCache{groupLifecycleTestCache: newGroupLifecycleTestCache()}
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Platform: PlatformDeepseek, Status: StatusActive, Hydrated: true}}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)

	codeBuddyOwn := SchedulerBucket{GroupID: groupID, Platform: schedulerAggregationBucketPlatform(PlatformCodeBuddy), Mode: SchedulerModeSingle}
	openAIBucket := SchedulerBucket{GroupID: groupID, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}

	accounts.setEnabled(true)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	beforeCodeBuddyOwn := accountIDPlatforms(cache.snapshotFor(codeBuddyOwn))
	require.False(t, hasCodeBuddyAccount(cache.snapshotFor(openAIBucket)), "non-aggregate (openai) bucket must not gain codebuddy")

	accounts.setEnabled(false)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	// codebuddy 自有分组桶内容翻转前后逐位一致（开关不波及）。
	require.Equal(t, beforeCodeBuddyOwn, accountIDPlatforms(cache.snapshotFor(codeBuddyOwn)),
		"codebuddy own group bucket must be unaffected by the aggregate switch flip")
	require.False(t, hasCodeBuddyAccount(cache.snapshotFor(openAIBucket)), "non-aggregate (openai) bucket still unaffected after flip")
}

// §5 矩阵行 3：聚合绑定关闭 × 新请求 → 原生路径（不含 codebuddy）。
func TestSchedulerGroupAggregateMatrixRow3BindingOffNewRequestNativePath(t *testing.T) {
	const groupID int64 = 7301
	bucket := SchedulerBucket{GroupID: groupID, Platform: schedulerAggregationBucketPlatform(PlatformDeepseek), Mode: SchedulerModeSingle}
	accounts := &aggregatedSupersetAccountRepo{}
	accounts.setEnabled(false) // 绑定关闭
	cache := &capturingSnapshotCache{groupLifecycleTestCache: newGroupLifecycleTestCache()}
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Platform: PlatformDeepseek, Status: StatusActive, Hydrated: true}}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)

	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	loaded := cache.snapshotFor(bucket)
	require.NotEmpty(t, loaded, "binding OFF × new request must still populate the grouped bucket via native path")
	require.False(t, hasCodeBuddyAccount(loaded), "binding OFF × new request must follow native path (no codebuddy)")
}

// §5 矩阵行 4：聚合绑定关闭 × 已建立请求 → 放行完成、不中途改道。
// 快照已加载集合（开关开启时构建、含 codebuddy）不因翻转被回收，直到下一次重建。
func TestSchedulerGroupAggregateMatrixRow4BindingOffEstablishedRequestNotRerouted(t *testing.T) {
	const groupID int64 = 7401
	bucket := SchedulerBucket{GroupID: groupID, Platform: schedulerAggregationBucketPlatform(PlatformDeepseek), Mode: SchedulerModeSingle}
	accounts := &aggregatedSupersetAccountRepo{}
	accounts.setEnabled(true) // 开关开启时构建桶（已建立请求，含 codebuddy）
	cache := &capturingSnapshotCache{groupLifecycleTestCache: newGroupLifecycleTestCache()}
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Platform: PlatformDeepseek, Status: StatusActive, Hydrated: true}}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)

	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	require.True(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "established bucket (switch ON) contains codebuddy")

	// 翻转关闭，但"不触发重建"——已建立的快照集合必须保留（不中途改道）。
	accounts.setEnabled(false)
	require.True(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)),
		"flipping OFF without rebuild must NOT reclaim the already-established bucket (no mid-flight reroute)")

	// 直到下一次重建（handleGroupEvent）才反映新开关值。
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	require.False(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)),
		"next rebuild after flip reflects the new switch value")
}

// §5 矩阵行 5：重试换号按"新请求"重新过滤。
// 翻转使分组候选集被重建为最新权威值；重试换号回退到选号路径时，按新请求重新加载候选
// 超集，不再持有已被关闭的 codebuddy 候选（与 ①②③⑤ 同一重判，无独立缓存旁路）。
func TestSchedulerGroupAggregateMatrixRow5RetryReselectFiltersFresh(t *testing.T) {
	const groupID int64 = 7601
	bucket := SchedulerBucket{GroupID: groupID, Platform: schedulerAggregationBucketPlatform(PlatformDeepseek), Mode: SchedulerModeSingle}
	accounts := &aggregatedSupersetAccountRepo{}
	accounts.setEnabled(true)
	cache := &capturingSnapshotCache{groupLifecycleTestCache: newGroupLifecycleTestCache()}
	groups := &groupLifecycleTestGroupRepo{group: &Group{ID: groupID, Platform: PlatformDeepseek, Status: StatusActive, Hydrated: true}}
	svc := newGroupLifecycleTestService(cache, accounts, groups, config.RunModeStandard)

	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	require.True(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "precondition: switch ON built a codebuddy-containing bucket")

	// 翻转关闭并重建（模拟在途请求失败、触发换号重试前分组配置已更新）。
	accounts.setEnabled(false)
	require.NoError(t, svc.handleGroupEvent(context.Background(), ptrInt64(groupID), make(map[batchSeenKey]struct{})))
	require.False(t, hasCodeBuddyAccount(cache.snapshotFor(bucket)), "rebuild after flip removed codebuddy from the candidate superset")

	// 重试换号按"新请求"重新加载候选集——必须反映最新权威值（不含 codebuddy），
	// 不复用翻转前已加载的旧候选。
	fresh, err := svc.loadAccountsFromDB(context.Background(), bucket, false)
	require.NoError(t, err)
	require.False(t, hasCodeBuddyAccount(fresh), "retry reselection must re-filter by the fresh authority (no stale codebuddy)")
}
