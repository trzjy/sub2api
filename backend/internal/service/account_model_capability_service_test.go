package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/stretchr/testify/require"
)

// mockAccountModelCapabilityRepo 能力标记测试用 mock repo。
type mockAccountModelCapabilityRepo struct {
	caps            []*model.AccountModelCapability
	upserted        []*model.AccountModelCapability
	upsertErr       error
	getErr          error
	listErr         error
	deleteErr       error
	upsertCount     int
	deleteByAccount int
}

func (m *mockAccountModelCapabilityRepo) Get(ctx context.Context, accountID int64, upstreamModel, protocol string) (*model.AccountModelCapability, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	for _, cap := range m.caps {
		if cap.AccountID == accountID && cap.UpstreamModel == upstreamModel && cap.Protocol == protocol {
			return cap, nil
		}
	}
	return nil, nil
}

func (m *mockAccountModelCapabilityRepo) Upsert(ctx context.Context, cap *model.AccountModelCapability) (*model.AccountModelCapability, error) {
	m.upsertCount++
	if m.upsertErr != nil {
		return nil, m.upsertErr
	}
	now := time.Now()
	cap.ID = 42
	cap.UpdatedAt = now
	m.upserted = append(m.upserted, cap)
	// 同步到查询面，模拟 DB 生效
	m.caps = append(m.caps, cap)
	return cap, nil
}

func (m *mockAccountModelCapabilityRepo) ListByAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	result := make([]*model.AccountModelCapability, 0)
	for _, cap := range m.caps {
		if cap.AccountID == accountID {
			result = append(result, cap)
		}
	}
	return result, nil
}

func (m *mockAccountModelCapabilityRepo) DeleteByAccount(ctx context.Context, accountID int64) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deleteByAccount++
	kept := make([]*model.AccountModelCapability, 0, len(m.caps))
	for _, cap := range m.caps {
		if cap.AccountID != accountID {
			kept = append(kept, cap)
		}
	}
	m.caps = kept
	return nil
}

// mockAccountModelCapabilityCache 能力标记测试用 mock cache。
// 内部 map 访问加锁，便于 -race 并发读写测试（不影响既有单线程测试语义）。
type mockAccountModelCapabilityCache struct {
	mu           sync.Mutex
	data         map[int64][]*model.AccountModelCapability
	hasData      bool
	getCount     int
	setCount     int
	invalidate   int
	notify       []int64
	notifySuspect []bool
	handlers     []func(accountID int64, suspectRedis bool)
	notifyCalled int
	invalidateErr error
	notifyErr     error
}

func newMockAccountModelCapabilityCache(data map[int64][]*model.AccountModelCapability, hasData bool) *mockAccountModelCapabilityCache {
	return &mockAccountModelCapabilityCache{
		data:    data,
		hasData: hasData,
	}
}

func (m *mockAccountModelCapabilityCache) GetAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCount++
	if !m.hasData {
		return nil, false
	}
	caps, ok := m.data[accountID]
	return caps, ok
}

func (m *mockAccountModelCapabilityCache) SetAccount(ctx context.Context, accountID int64, caps []*model.AccountModelCapability) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setCount++
	if m.data == nil {
		m.data = make(map[int64][]*model.AccountModelCapability)
	}
	m.data[accountID] = caps
	m.hasData = true
	return nil
}

func (m *mockAccountModelCapabilityCache) InvalidateAccount(ctx context.Context, accountID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invalidate++
	if m.invalidateErr != nil {
		return m.invalidateErr
	}
	if m.data != nil {
		delete(m.data, accountID)
	}
	return nil
}

func (m *mockAccountModelCapabilityCache) NotifyUpdate(ctx context.Context, accountID int64, suspectRedis bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifyCalled++
	m.notify = append(m.notify, accountID)
	m.notifySuspect = append(m.notifySuspect, suspectRedis)
	if m.notifyErr != nil {
		return m.notifyErr
	}
	return nil
}

func (m *mockAccountModelCapabilityCache) SubscribeUpdates(ctx context.Context, handler func(accountID int64, suspectRedis bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = append(m.handlers, handler)
}

func newCapabilityService(repo AccountModelCapabilityRepository, cache *mockAccountModelCapabilityCache) *AccountModelCapabilityService {
	return NewAccountModelCapabilityService(repo, cache)
}

func TestModelSupportsVisionInput_NoRecordReturnsUnknown(t *testing.T) {
	repo := &mockAccountModelCapabilityRepo{}
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	supported, known, _ := svc.ModelSupportsVisionInput(ctx, 52, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.False(t, supported)
	require.False(t, known)
}

func TestModelSupportsVisionInput_SupportedRecord(t *testing.T) {
	repo := &mockAccountModelCapabilityRepo{
		caps: []*model.AccountModelCapability{
			{AccountID: 137, UpstreamModel: "deepseek-v4.1-flash", Protocol: model.CapabilityProtocolChatCompletions, SupportsVision: true, Source: model.CapabilitySourceDetect},
		},
	}
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	supported, known, _ := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.True(t, supported)
	require.True(t, known)
}

func TestModelSupportsVisionInput_UnsupportedRecord(t *testing.T) {
	repo := &mockAccountModelCapabilityRepo{
		caps: []*model.AccountModelCapability{
			{AccountID: 52, UpstreamModel: "deepseek-v4.1-flash", Protocol: model.CapabilityProtocolChatCompletions, SupportsVision: false, Source: model.CapabilitySourceDetect},
		},
	}
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	supported, known, _ := svc.ModelSupportsVisionInput(ctx, 52, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.False(t, supported)
	require.True(t, known)
}

// TestModelSupportsVisionInput_ProtocolIsolation 同一账号同一模型不同协议不互相污染。
func TestModelSupportsVisionInput_ProtocolIsolation(t *testing.T) {
	repo := &mockAccountModelCapabilityRepo{
		caps: []*model.AccountModelCapability{
			{AccountID: 137, UpstreamModel: "deepseek-v4.1-flash", Protocol: model.CapabilityProtocolChatCompletions, SupportsVision: true, Source: model.CapabilitySourceDetect},
		},
	}
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	// chat_completions 支持
	supported, known, _ := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.True(t, supported)
	require.True(t, known)
	// anthropic 协议未标记 → unknown（协议隔离）
	supported, known, _ = svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolAnthropic)
	require.False(t, supported)
	require.False(t, known)
}

// TestUpsertCapability_TriggersCacheInvalidationAndBroadcast 验证 Upsert 后缓存失效
// + 本地刷新 + 跨实例广播全部接通（方案 §3.3 缓存失效路径必测项）。
func TestUpsertCapability_TriggersCacheInvalidationAndBroadcast(t *testing.T) {
	repo := &mockAccountModelCapabilityRepo{}
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	detectedAt := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	cap := &model.AccountModelCapability{
		AccountID:      137,
		UpstreamModel:  "deepseek-v4.1-flash",
		Protocol:       model.CapabilityProtocolChatCompletions,
		SupportsVision: true,
		Source:         model.CapabilitySourceDetect,
		DetectedAt:     &detectedAt,
	}

	upserted, err := svc.UpsertCapability(ctx, cap)
	require.NoError(t, err)
	require.Equal(t, int64(42), upserted.ID)
	require.Equal(t, int64(137), upserted.AccountID)
	require.True(t, upserted.SupportsVision)

	// 缓存失效 + 广播已触发
	require.Equal(t, 1, cache.invalidate)
	require.Equal(t, 1, cache.notifyCalled)
	require.Equal(t, []int64{137}, cache.notify)

	// 落库后读路径立即生效（本地缓存已刷新）
	supported, known, _ := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.True(t, supported)
	require.True(t, known)
}

// TestUpsertCapability_InvalidRecordRejected 非法记录（未知协议/未知来源）被拒。
func TestUpsertCapability_InvalidRecordRejected(t *testing.T) {
	repo := &mockAccountModelCapabilityRepo{}
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	_, err := svc.UpsertCapability(ctx, &model.AccountModelCapability{
		AccountID:      137,
		UpstreamModel:  "deepseek-v4.1-flash",
		Protocol:       "bogus-protocol",
		SupportsVision: true,
		Source:         model.CapabilitySourceDetect,
	})
	require.ErrorIs(t, err, ErrAccountModelCapabilityInvalid)

	_, err = svc.UpsertCapability(ctx, &model.AccountModelCapability{
		AccountID:      137,
		UpstreamModel:  "deepseek-v4.1-flash",
		Protocol:       model.CapabilityProtocolChatCompletions,
		SupportsVision: true,
		Source:         "bogus-source",
	})
	require.ErrorIs(t, err, ErrAccountModelCapabilityInvalid)
}

// TestUpsertCapability_CacheFallbackPath 缓存未命中时从 DB 回源并回写缓存。
func TestUpsertCapability_CacheFallbackPath(t *testing.T) {
	repo := &mockAccountModelCapabilityRepo{
		caps: []*model.AccountModelCapability{
			{AccountID: 97, UpstreamModel: "deepseek-v4.1-flash", Protocol: model.CapabilityProtocolChatCompletions, SupportsVision: true, Source: model.CapabilitySourceManual},
		},
	}
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	// 第一次读：本地无缓存、Redis 无数据 → 回源 DB
	supported, known, _ := svc.ModelSupportsVisionInput(ctx, 97, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.True(t, supported)
	require.True(t, known)
	// 回源后回写缓存
	require.Equal(t, 1, cache.setCount)
}

// TestModelSupportsVisionInput_ReadErrorPropagated 验证缓存刷新/DB 读取失败时，
// 读取错误显式传播（不被静默降级为 unknown）。调用方据此失败关闭（合同 §5；方案 §3.4）。
func TestModelSupportsVisionInput_ReadErrorPropagated(t *testing.T) {
	repo := &mockAccountModelCapabilityRepo{listErr: errors.New("capability db unavailable")}
	// 缓存未命中，强制回源 DB；本地缓存空，读取失败直接传播。
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	supported, known, err := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.Error(t, err)
	require.False(t, supported)
	require.False(t, known, "读取失败不得被当作 unknown 放行")
}

// TestUpsertCapability_InvalidateFailurePropagatesAndBypassesStaleCache
// 场景①：InvalidateAccount 失败 → 本地缓存被清 + 后续读直连 DB 拿到新值 +
// 写路径报错，且禁止从可能陈旧的二级缓存回填。
func TestUpsertCapability_InvalidateFailurePropagatesAndBypassesStaleCache(t *testing.T) {
	stale := &model.AccountModelCapability{
		AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
		Protocol: model.CapabilityProtocolChatCompletions,
		SupportsVision: false, Source: model.CapabilitySourceManual,
	}
	// 二级缓存（Redis 模拟）中放置陈旧值，用于验证失效失败后不会回填陈旧值。
	cache := newMockAccountModelCapabilityCache(
		map[int64][]*model.AccountModelCapability{137: {stale}}, true,
	)
	cache.invalidateErr = errors.New("redis unavailable")
	// repo 不预置陈旧值；新值将由 Upsert 落库。
	repo := &mockAccountModelCapabilityRepo{}
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	// 先读一次，把陈旧值填进本地缓存（模拟"之前命中过旧值"）。
	_, known, err := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.True(t, known)

	// 手动覆盖为新值并落库；缓存失效应当失败。
	newCap := &model.AccountModelCapability{
		AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
		Protocol: model.CapabilityProtocolChatCompletions,
		SupportsVision: true, Source: model.CapabilitySourceManual,
	}
	_, err = svc.UpsertCapability(ctx, newCap)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalidate vision capability cache")

	// 写路径报错后本地缓存被清：后续读直连 DB，拿到新值（非陈旧缓存）。
	supported, known, err := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, supported, "失效失败后读应直连 DB 拿到新值，而非命中陈旧二级缓存")

	// 失效失败后禁止从二级缓存回填陈旧值：SetAccount 不应被调用。
	require.Equal(t, 0, cache.setCount, "失效失败后不得回填可能陈旧的二级缓存")
}

// TestUpsertCapability_NotifyFailurePropagatesWithoutSilence
// 场景②：NotifyUpdate 失败 → 本地缓存被清 + 后续读直连 DB 拿到新值 + 写路径报错，
// 且不静默忽略广播失败。
func TestUpsertCapability_NotifyFailurePropagatesWithoutSilence(t *testing.T) {
	cache := newMockAccountModelCapabilityCache(nil, false)
	cache.notifyErr = errors.New("broadcast unavailable")
	repo := &mockAccountModelCapabilityRepo{}
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	newCap := &model.AccountModelCapability{
		AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
		Protocol: model.CapabilityProtocolChatCompletions,
		SupportsVision: true, Source: model.CapabilitySourceManual,
	}
	_, err := svc.UpsertCapability(ctx, newCap)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalidate vision capability cache")
	// 广播确实被触发（虽失败），证明未静默跳过。
	require.Equal(t, 1, cache.notifyCalled)

	// 本地缓存已清：后续读直连 DB 拿到新值。
	supported, known, err := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, supported, "广播失败后读应直连 DB 拿到新值")
}

// TestInvalidateAndNotify_NormalZeroRegression
// 场景③：正常失效 → 失效成功、广播成功、无错误，既有读路径行为零回归
// （落库后读路径立即生效，本地缓存被刷新）。
func TestInvalidateAndNotify_NormalZeroRegression(t *testing.T) {
	cache := newMockAccountModelCapabilityCache(nil, false)
	repo := &mockAccountModelCapabilityRepo{}
	svc := newCapabilityService(repo, cache)

	ctx := context.Background()
	newCap := &model.AccountModelCapability{
		AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
		Protocol: model.CapabilityProtocolChatCompletions,
		SupportsVision: true, Source: model.CapabilitySourceManual,
	}
	upserted, err := svc.UpsertCapability(ctx, newCap)
	require.NoError(t, err)
	require.NotNil(t, upserted)
	require.Equal(t, 1, cache.invalidate)
	require.Equal(t, 1, cache.notifyCalled)

	// 正常失效后读路径立即生效。
	supported, known, err := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, supported)
}

// gateCapRepo 可在"首次回源读取途中"暂停，便于确定性地注入一次并发写，
// 复现终审 P1 竞态：读请求回源读到旧值 → 写请求完成 Upsert+失效+刷新 →
// 读请求把旧值 setLocalAccount 装回本地缓存。
type gateCapRepo struct {
	mu            sync.Mutex
	staleSnapshot []*model.AccountModelCapability // 第一次（读者）回源读到的旧值
	freshSnapshot []*model.AccountModelCapability // 之后（写已提交后）读到的新值
	readStarted   chan struct{}
	proceed       chan struct{}
	armed         bool
}

func (r *gateCapRepo) Get(ctx context.Context, accountID int64, upstreamModel, protocol string) (*model.AccountModelCapability, error) {
	return nil, nil
}

func (r *gateCapRepo) Upsert(ctx context.Context, cap *model.AccountModelCapability) (*model.AccountModelCapability, error) {
	r.mu.Lock()
	r.freshSnapshot = append(r.freshSnapshot, cap)
	r.mu.Unlock()
	cap.ID = 42
	return cap, nil
}

func (r *gateCapRepo) ListByAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error) {
	if r.armed {
		r.armed = false
		close(r.readStarted)
		<-r.proceed
		// 读者在"写完成前"已快照到的旧值。
		return r.staleSnapshot, nil
	}
	// 写提交后，后续回源读到新值。
	return r.freshSnapshot, nil
}

func (r *gateCapRepo) DeleteByAccount(ctx context.Context, accountID int64) error {
	return nil
}

// TestAccountModelCapability_StaleReadDiscardedOnConcurrentInvalidate
// 场景①：模拟"读出旧值→并发失效→安装"时序 → 旧值不入本地缓存。
// 关键不变量：并发下旧值不可能毒化本地缓存，下一请求重读必拿到失效后的新值。
func TestAccountModelCapability_StaleReadDiscardedOnConcurrentInvalidate(t *testing.T) {
	stale := &model.AccountModelCapability{
		AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
		Protocol: model.CapabilityProtocolChatCompletions,
		SupportsVision: false, Source: model.CapabilitySourceManual, // 旧值：不支持视觉
	}
	repo := &gateCapRepo{
		staleSnapshot: []*model.AccountModelCapability{stale},
		readStarted:   make(chan struct{}),
		proceed:       make(chan struct{}),
		armed:         true,
	}
	// 二级缓存无数据，强制回源 DB。
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)
	ctx := context.Background()

	newCap := &model.AccountModelCapability{
		AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
		Protocol: model.CapabilityProtocolChatCompletions,
		SupportsVision: true, Source: model.CapabilitySourceManual, // 新值：支持视觉
	}

	// 读者 goroutine 发起读取：回源途中（已快照旧值）会停在 ListByAccount 等待放行。
	done := make(chan struct{})
	go func() {
		_, _, _ = svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
		close(done)
	}()

	// 等读者开始回源读取（旧值已快照）。
	<-repo.readStarted
	// 此时读者停在 ListByAccount 等待 proceed；并发执行写请求：Upsert+失效+刷新。
	_, err := svc.UpsertCapability(ctx, newCap)
	require.NoError(t, err, "写请求须成功完成 Upsert+失效+刷新")

	// 写完成后放行读者安装：读者安装前复核代际已变，必须丢弃旧值。
	close(repo.proceed)
	<-done

	// 断言：本地缓存未被旧值毒化，而是拿到失效后的新值（支持视觉=true）。
	supported, known, err := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, supported, "并发回源读到的旧值不得毒化本地缓存：下一请求必须拿到失效后的新值")
}

// concurrentCapRepo 并发安全的 mock repo，供 -race 压力测试使用。
type concurrentCapRepo struct {
	mu   sync.Mutex
	caps []*model.AccountModelCapability
}

func (r *concurrentCapRepo) Get(ctx context.Context, accountID int64, upstreamModel, protocol string) (*model.AccountModelCapability, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cap := range r.caps {
		if cap.AccountID == accountID && cap.UpstreamModel == upstreamModel && cap.Protocol == protocol {
			return cap, nil
		}
	}
	return nil, nil
}

func (r *concurrentCapRepo) Upsert(ctx context.Context, cap *model.AccountModelCapability) (*model.AccountModelCapability, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cap.ID = 42
	r.caps = append(r.caps, cap)
	return cap, nil
}

func (r *concurrentCapRepo) ListByAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]*model.AccountModelCapability, 0, len(r.caps))
	for _, cap := range r.caps {
		if cap.AccountID == accountID {
			result = append(result, cap)
		}
	}
	return result, nil
}

func (r *concurrentCapRepo) DeleteByAccount(ctx context.Context, accountID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := make([]*model.AccountModelCapability, 0, len(r.caps))
	for _, cap := range r.caps {
		if cap.AccountID != accountID {
			kept = append(kept, cap)
		}
	}
	r.caps = kept
	return nil
}

// TestAccountModelCapability_ConcurrentReadWriteNoDataRace
// 场景③（-race）：并发读写不产生数据竞争，且最终一致（并发结束后读到最后一次写的值）。
func TestAccountModelCapability_ConcurrentReadWriteNoDataRace(t *testing.T) {
	repo := &concurrentCapRepo{}
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)
	ctx := context.Background()

	const accountID int64 = 137
	const iterations = 100

	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, _, _ = svc.ModelSupportsVisionInput(ctx, accountID, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
		}(i)
		go func(i int) {
			defer wg.Done()
			_, _ = svc.UpsertCapability(ctx, &model.AccountModelCapability{
				AccountID:      accountID,
				UpstreamModel: "deepseek-v4.1-flash",
				Protocol:      model.CapabilityProtocolChatCompletions,
				SupportsVision: i%2 == 0,
				Source:        model.CapabilitySourceManual,
			})
		}(i)
	}
	wg.Wait()

	// 最终一致性：并发结束后（无数据竞争）读取必须稳定且不崩溃，缓存未被陈旧值毒化。
	// 注意：并发写不保证"最后一次写必胜"，仅保证代际机制下旧回源结果不会毒化缓存；
	// 故只断言读路径稳定可用（known=true、无错误），具体 supported 值取决于最后落地的写。
	firstSupported, firstKnown, err := svc.ModelSupportsVisionInput(ctx, accountID, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.True(t, firstKnown, "并发结束后读路径应稳定可用（known=true）")
	// 连续多次读应返回同一稳定值（缓存已收敛，无并发抖动）。
	for i := 0; i < 5; i++ {
		supported, known, err := svc.ModelSupportsVisionInput(ctx, accountID, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
		require.NoError(t, err)
		require.True(t, known)
		require.Equal(t, firstSupported, supported, "并发结束后缓存应已收敛为稳定值")
	}
}

// TestAccountModelCapability_NormalNoConcurrencyZeroRegression
// 场景②：无并发路径下行为零回归——首读回源、Upsert 后失效、再读立即生效。
func TestAccountModelCapability_NormalNoConcurrencyZeroRegression(t *testing.T) {
	repo := &mockAccountModelCapabilityRepo{}
	cache := newMockAccountModelCapabilityCache(nil, false)
	svc := newCapabilityService(repo, cache)
	ctx := context.Background()

	const accountID int64 = 137
	// 首读：未知（无记录）→ 默认放行（known=false）。
	supported, known, err := svc.ModelSupportsVisionInput(ctx, accountID, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.False(t, known)
	require.False(t, supported)

	// 写入"不支持视觉"并失效。
	_, err = svc.UpsertCapability(ctx, &model.AccountModelCapability{
		AccountID:      accountID,
		UpstreamModel:  "deepseek-v4.1-flash",
		Protocol:       model.CapabilityProtocolChatCompletions,
		SupportsVision: false,
		Source:         model.CapabilitySourceManual,
	})
	require.NoError(t, err)

	// 再读立即生效：known=true，supported=false。
	supported, known, err = svc.ModelSupportsVisionInput(ctx, accountID, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.True(t, known)
	require.False(t, supported, "无并发路径下写后读必须立即反映新值，零回归")
}

// TestUpsertCapability_InvalidateFailureBroadcastsSuspectTrue（改动点 A / D.2）
// Redis 失效失败时，广播仍须发生且携带 suspectRedis=true，令其他实例绕开 Redis 直读 DB。
func TestUpsertCapability_InvalidateFailureBroadcastsSuspectTrue(t *testing.T) {
	cache := newMockAccountModelCapabilityCache(nil, false)
	cache.invalidateErr = errors.New("redis unavailable")
	repo := &mockAccountModelCapabilityRepo{}
	svc := newCapabilityService(repo, cache)
	ctx := context.Background()

	_, err := svc.UpsertCapability(ctx, &model.AccountModelCapability{
		AccountID:      137,
		UpstreamModel:  "deepseek-v4.1-flash",
		Protocol:       model.CapabilityProtocolChatCompletions,
		SupportsVision: true,
		Source:         model.CapabilitySourceManual,
	})
	require.Error(t, err)
	require.Equal(t, 1, cache.notifyCalled, "失效失败也须广播")
	require.Len(t, cache.notifySuspect, 1)
	require.True(t, cache.notifySuspect[0], "失效失败须广播 suspectRedis=true，令其他实例绕开 Redis")
}

// TestSubscribeCallback_SuspectTrueBypassesStaleRedis（改动点 A / D.3）
// 订阅回调收到 suspect=true（其他实例 Redis 失效失败）→ 本实例后续读绕开 Redis 直读 DB，
// 不命中预埋的陈旧二级缓存值。
func TestSubscribeCallback_SuspectTrueBypassesStaleRedis(t *testing.T) {
	stale := &model.AccountModelCapability{
		AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
		Protocol: model.CapabilityProtocolChatCompletions,
		SupportsVision: false, Source: model.CapabilitySourceManual, // 陈旧值
	}
	// 二级缓存（Redis 模拟）预埋陈旧值。
	cache := newMockAccountModelCapabilityCache(
		map[int64][]*model.AccountModelCapability{137: {stale}}, true,
	)
	// repo 持有新值；可疑时本实例须直读 DB 拿新值。
	repo := &mockAccountModelCapabilityRepo{
		caps: []*model.AccountModelCapability{
			{AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
				Protocol: model.CapabilityProtocolChatCompletions,
				SupportsVision: true, Source: model.CapabilitySourceManual},
		},
	}
	svc := newCapabilityService(repo, cache)
	require.NotEmpty(t, cache.handlers, "构造器应已注册订阅回调")
	// 模拟"其他实例广播失效且 Redis 失效失败"。
	cache.handlers[0](137, true)

	ctx := context.Background()
	supported, known, err := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, supported, "suspect=true 应绕开 Redis 直读 DB 拿到新值，而非陈旧二级缓存")
	// 可疑期间不得把可能陈旧的值回填 Redis。
	require.Equal(t, 0, cache.setCount, "suspect 期间不得回填二级缓存")
}

// TestSubscribeCallback_SuspectFalseResumesRedis（改动点 A / D.3）
// 订阅回调收到 suspect=false（其他实例 Redis 失效成功）→ 解除可疑，本实例恢复 Redis 回源。
func TestSubscribeCallback_SuspectFalseResumesRedis(t *testing.T) {
	caps := &model.AccountModelCapability{
		AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
		Protocol: model.CapabilityProtocolChatCompletions,
		SupportsVision: false, Source: model.CapabilitySourceManual,
	}
	// Redis 预埋值；repo 为空，证明读取走的是 Redis 而非 DB。
	cache := newMockAccountModelCapabilityCache(
		map[int64][]*model.AccountModelCapability{137: {caps}}, true,
	)
	repo := &mockAccountModelCapabilityRepo{}
	svc := newCapabilityService(repo, cache)
	require.NotEmpty(t, cache.handlers)
	cache.handlers[0](137, false) // 解除可疑，恢复 Redis 回源

	ctx := context.Background()
	getBefore := cache.getCount
	supported, known, err := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.True(t, known)
	require.False(t, supported, "suspect=false 应恢复 Redis 回源，读到预埋值")
	require.Greater(t, cache.getCount, getBefore, "suspect=false 后应恢复从 Redis 读取")
}

// TestSubscribeCallback_LegacyPayloadTreatedAsSuspect（改动点 A / D.6）
// 旧格式纯数字 payload（滚动升级期旧发布者）映射到订阅回调即 suspectRedis=true，
// 本实例须绕开可能陈旧的 Redis（保守失败关闭）。以 mock 订阅回调代真解析路径验证等价语义。
func TestSubscribeCallback_LegacyPayloadTreatedAsSuspect(t *testing.T) {
	stale := &model.AccountModelCapability{
		AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
		Protocol: model.CapabilityProtocolChatCompletions,
		SupportsVision: false, Source: model.CapabilitySourceManual,
	}
	cache := newMockAccountModelCapabilityCache(
		map[int64][]*model.AccountModelCapability{137: {stale}}, true,
	)
	repo := &mockAccountModelCapabilityRepo{
		caps: []*model.AccountModelCapability{
			{AccountID: 137, UpstreamModel: "deepseek-v4.1-flash",
				Protocol: model.CapabilityProtocolChatCompletions,
				SupportsVision: true, Source: model.CapabilitySourceManual},
		},
	}
	svc := newCapabilityService(repo, cache)
	// 旧发布者只发纯数字 accountID：等价 suspectRedis=true。
	cache.handlers[0](137, true)

	ctx := context.Background()
	supported, known, err := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, supported, "legacy 纯数字 payload 须按 suspectRedis=true 处理，绕开陈旧 Redis 直读 DB")
}

// churnCapRepo 在每次回源读取中翻转代际，用于确定性复现代际持续翻转（改动点 B / D.5）。
type churnCapRepo struct {
	mockAccountModelCapabilityRepo
	onList func(int64)
}

func (r *churnCapRepo) ListByAccount(ctx context.Context, accountID int64) ([]*model.AccountModelCapability, error) {
	if r.onList != nil {
		r.onList(accountID)
	}
	return r.mockAccountModelCapabilityRepo.ListByAccount(ctx, accountID)
}

// TestRefreshAccountCache_GenerationChurnReturnsError（改动点 B / D.5）
// 代际持续翻转 ⇒ refreshAccountCache 耗尽 3 次后返回显式错误，而非失配的陈旧 caps
// （上层 ModelSupportsVisionInput 据此失败关闭）。
func TestRefreshAccountCache_GenerationChurnReturnsError(t *testing.T) {
	cache := newMockAccountModelCapabilityCache(nil, false)
	repo := &churnCapRepo{}
	svc := newCapabilityService(repo, cache)
	// 每次回源读取都翻转代际，使安装前复核必失配。
	repo.onList = func(int64) { svc.bumpGeneration(137) }

	ctx := context.Background()
	supported, known, err := svc.ModelSupportsVisionInput(ctx, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions)
	require.Error(t, err, "代际持续翻转须返回显式错误而非陈旧 caps")
	require.Contains(t, err.Error(), "generation churned")
	require.False(t, known, "代际翻转耗尽后不得返回陈旧 caps（失败关闭）")
	require.False(t, supported)
}

