//go:build unit

package service

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------- fakes ----------

// dimensionsMatch / looselyEqualValue / toFloat64 为测试 fake 仓储的维度匹配辅助
// （生产查询已改走仓储维度精确过滤，此处仅用于 fake 内存匹配模拟行为）。
func dimensionsMatch(have, want map[string]any) bool {
	if len(have) < len(want) {
		return false
	}
	for k, wv := range want {
		hv, ok := have[k]
		if !ok {
			return false
		}
		if !looselyEqualValue(hv, wv) {
			return false
		}
	}
	return true
}

func looselyEqualValue(have, want any) bool {
	switch w := want.(type) {
	case int64:
		if hf, ok := toFloat64(have); ok {
			return hf == float64(w)
		}
	case float64:
		if hf, ok := toFloat64(have); ok {
			return hf == w
		}
	case string:
		if hs, ok := have.(string); ok {
			return hs == w
		}
	}
	return fmt.Sprintf("%v", have) == fmt.Sprintf("%v", want)
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

type fakeFreshnessStore struct {
	mu     sync.Mutex
	events []*OpsAlertEvent
	nextID int64
	// 调用路径计数器（E47）：孤儿关闭应走 ResolveFreshnessAlertOnRecovery（未门禁窄面），
	// 不应触发 syncFreshness 的 GetActiveFreshnessAlert / UpdateAlertEventStatus。
	resolveRecoveryCalls int
	getActiveCalls       int
	updateCalls          int
}

func (f *fakeFreshnessStore) CreateAlertEvent(_ context.Context, e *OpsAlertEvent) (*OpsAlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	cp := *e
	cp.ID = f.nextID
	if cp.FiredAt.IsZero() {
		cp.FiredAt = time.Now()
	}
	f.events = append(f.events, &cp)
	return &cp, nil
}

func (f *fakeFreshnessStore) GetActiveFreshnessAlert(_ context.Context, dims map[string]any) (*OpsAlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getActiveCalls++
	for _, ev := range f.events {
		if ev.Status == OpsAlertStatusFiring && dimensionsMatch(ev.Dimensions, dims) {
			return ev, nil
		}
	}
	return nil, nil
}

func (f *fakeFreshnessStore) UpdateAlertEventStatus(_ context.Context, id int64, status string, resolvedAt *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	for _, ev := range f.events {
		if ev.ID == id {
			ev.Status = status
			ev.ResolvedAt = resolvedAt
			return nil
		}
	}
	return nil
}

// ResolveFreshnessAlertOnRecovery 实现未门禁恢复关闭窄面（E47）：无活动告警 no-op，有则原子关闭。
// 与 OpsService 同源语义，但不经 RequireMonitoringEnabled，供孤儿清扫直接调用。
func (f *fakeFreshnessStore) ResolveFreshnessAlertOnRecovery(_ context.Context, dims map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveRecoveryCalls++
	for _, ev := range f.events {
		if ev.Status == OpsAlertStatusFiring && dimensionsMatch(ev.Dimensions, dims) {
			now := time.Now()
			ev.Status = OpsAlertStatusResolved
			ev.ResolvedAt = &now
			return nil
		}
	}
	return nil
}

// ListActiveFreshnessAlerts 列出 firing 的账号+模型维新鲜度告警（孤儿清扫用，E45）。
func (f *fakeFreshnessStore) ListActiveFreshnessAlerts(_ context.Context) ([]*OpsAlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*OpsAlertEvent
	for _, ev := range f.events {
		if ev.Status == OpsAlertStatusFiring && ev.Dimensions[freshnessDimKind] == freshnessDimAccountModel {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (f *fakeFreshnessStore) activeFiring(dims map[string]any) *OpsAlertEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range f.events {
		if ev.Status == OpsAlertStatusFiring && dimensionsMatch(ev.Dimensions, dims) {
			return ev
		}
	}
	return nil
}

type fakeFreshnessReader struct {
	entries map[string]map[string]any
	// err 非空时对所有读取返回该错误，用于注入权威读取失败。
	err error
}

func (r *fakeFreshnessReader) GetModelRateLimitObservation(_ context.Context, accountID int64, scope string) (map[string]any, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.entries[fmt.Sprintf("%d:%s", accountID, scope)], nil
}

type fakeBounds struct{ ub time.Duration }

func (b *fakeBounds) AccountFreshnessUpperBound(int64, string) time.Duration { return b.ub }

type fakeDeriver struct {
	status     string
	observedAt time.Time
	eligible   bool
}

func (d *fakeDeriver) DeriveChannelStatus(context.Context, int64) (ChannelStatusDerivation, error) {
	return ChannelStatusDerivation{Status: d.status, ObservedAt: d.observedAt, AlertEligible: d.eligible}, nil
}

func newStaleService(store FreshnessAlertStore, reader FreshnessObservationReader, bounds FreshnessUpperBoundSource, deriver FreshnessChannelDeriver) *FreshnessAlertService {
	return NewFreshnessAlertService(store, reader, bounds, deriver)
}

func observedEntry(observedAgo, attemptedAgo time.Duration, now time.Time) map[string]any {
	e := map[string]any{}
	if observedAgo >= 0 {
		e[entryObservedAtKey] = now.Add(-observedAgo).UTC().Format(time.RFC3339)
	}
	if attemptedAgo >= 0 {
		e[entryAttemptedAtKey] = now.Add(-attemptedAgo).UTC().Format(time.RFC3339)
	}
	return e
}

// ---------- 账号+模型维生命周期 ----------

// TestStaleAccountModelAlert_FiresAndResolves 陈旧触发告警，观测刷新后关闭。
func TestStaleAccountModelAlert_FiresAndResolves(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": observedEntry(10*time.Minute, -1, now), // observed 10min 前，超 120s 阈值
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "陈旧应触发 firing 告警")

	// 观测刷新到 10s 前（阈值内）
	reader.entries["1:gpt-4"] = observedEntry(10*time.Second, -1, now)
	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.Nil(t, store.activeFiring(dims), "观测刷新后告警应关闭")
}

// TestStaleAlert_AttemptTimeNotClear 探测尝试时间不解除告警：observed_at 陈旧、
// attempted_at 新鲜，告警仍 firing。
func TestStaleAlert_AttemptTimeNotClear(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	// observed 10min 前陈旧；attempted 10s 前新鲜（仅尝试，非权威观测）。
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": observedEntry(10*time.Minute, 10*time.Second, now),
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "尝试时间不得解除陈旧告警")
}

// TestStaleHealthyNoTrafficNoPermanentAlert 健康无流量账号（限流条目已清除 → 无 observed_at）
// 不创建陈旧告警；若存在历史告警则关闭（无永久告警）。
func TestStaleHealthyNoTrafficNoPermanentAlert(t *testing.T) {
	store := &fakeFreshnessStore{}
	// 条目不存在（已恢复清除）。
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.Nil(t, store.activeFiring(dims), "健康无流量账号不应产生永久陈旧告警")
}

// TestStaleHealthyModelMaskStillAlerts 健康模型掩盖场景：同账号模型 A 健康（新鲜）、
// 模型 B 陈旧，评估 B 仍触发告警（两维互不掩盖）。
func TestStaleHealthyModelMaskStillAlerts(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": observedEntry(10*time.Second, -1, now), // 健康
		"1:gpt-3": observedEntry(10*time.Minute, -1, now), // 陈旧
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)

	healthyDims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}
	staleDims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-3"}

	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.Nil(t, store.activeFiring(healthyDims), "健康模型不应告警")
	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-3"))
	require.NotNil(t, store.activeFiring(staleDims), "被掩盖的陈旧模型仍应告警")
}

// TestStaleAlert_RecoveryAtomicClose 恢复即在同一维度原子关闭告警（非轮询收敛）。
// 生产关闭路径已由 OpsService.ResolveFreshnessAlertOnRecovery 承担（监控关闭也不阻断），
// 此处经由等价维度调用该窄面验证关闭语义（写入入口 resolver 闭包与之同源）。
func TestStaleAlert_RecoveryAtomicClose(t *testing.T) {
	repo := &inMemoryAlertRepo{opsRepoMock: &opsRepoMock{}}
	svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	// 先建 firing 告警（模拟 Evaluate* 已触发）。
	_, err := repo.CreateAlertEvent(context.Background(), &OpsAlertEvent{Status: OpsAlertStatusFiring, Dimensions: dims, FiredAt: time.Now()})
	require.NoError(t, err)

	// 恢复路径：限流条目清除后调用恢复关闭窄面（同一次状态变更内）。
	require.NoError(t, svc.ResolveFreshnessAlertOnRecovery(context.Background(), dims))
	ev, err := svc.GetActiveFreshnessAlert(context.Background(), dims)
	require.NoError(t, err)
	require.Nil(t, ev, "恢复应原子关闭对应告警")
}

// ---------- #6 失败关闭：读取错误不得错误关闭 firing 告警 ----------

// TestStaleAlert_ReadErrorKeepsFiring 权威读取（仓储）报错时，既有 firing 告警必须保持 firing，
// 不得因瞬时 DB 故障被当作「条目不存在/健康」而错误关闭（终审 #6）。
func TestStaleAlert_ReadErrorKeepsFiring(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": observedEntry(10*time.Minute, -1, now), // 陈旧
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	// 先建立 firing 告警。
	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：陈旧应触发 firing 告警")

	// 注入权威读取错误（模拟瞬时 DB 故障）：评估必须返回错误（跳过本轮状态变更），
	// 既有 firing 告警保持 firing，不得被关闭。
	sentinel := fmt.Errorf("injected read failure")
	reader.err = sentinel
	err := svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4")
	require.ErrorIs(t, err, sentinel, "读取错误应沿评估调用链返回")
	require.NotNil(t, store.activeFiring(dims), "读取错误不得关闭既有 firing 告警")

	// 恢复读取成功后，非陈旧（条目不存在）语义仍生效：关闭既有告警。
	reader.err = nil
	reader.entries["1:gpt-4"] = observedEntry(10*time.Second, -1, now)
	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.Nil(t, store.activeFiring(dims), "权威读取成功且非陈旧时应关闭既有告警")
}

// TestStaleAccountLevel_ReadErrorKeepsFiring 账号级维同样受 #6 保护：读取错误保持 firing。
func TestStaleAccountLevel_ReadErrorKeepsFiring(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	key := fmt.Sprintf("%d:%s", 1, tokenHarborAccountLevelProbeScope)
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		key: observedEntry(10*time.Minute, -1, now),
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountLevel, freshnessDimAccountID: int64(1)}

	require.NoError(t, svc.EvaluateAccountLevelFreshness(context.Background(), 1))
	require.NotNil(t, store.activeFiring(dims), "前置：陈旧应触发账号级 firing 告警")

	sentinel := fmt.Errorf("injected account level read failure")
	reader.err = sentinel
	err := svc.EvaluateAccountLevelFreshness(context.Background(), 1)
	require.ErrorIs(t, err, sentinel, "读取错误应沿评估调用链返回")
	require.NotNil(t, store.activeFiring(dims), "读取错误不得关闭既有账号级 firing 告警")

	// 恢复读取成功后，条目不存在 → 关闭（既有语义不变）。
	reader.err = nil
	reader.entries[key] = observedEntry(10*time.Second, -1, now)
	require.NoError(t, svc.EvaluateAccountLevelFreshness(context.Background(), 1))
	require.Nil(t, store.activeFiring(dims), "权威读取成功且非陈旧时应关闭账号级告警")
}

// TestStaleAlert_MissingEntryClosesFiring 条目不存在（权威读取成功但无条目）→ 既有 firing
// 告警关闭（既有语义不变），与读取错误严格区分（终审 #6）。
func TestStaleAlert_MissingEntryClosesFiring(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": observedEntry(10*time.Minute, -1, now),
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	// 先建立 firing 告警。
	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：陈旧应触发 firing 告警")

	// 条目不存在（限流条目已清除，健康恢复）→ 关闭既有告警（无永久告警）。
	reader.entries["1:gpt-4"] = nil
	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.Nil(t, store.activeFiring(dims), "条目不存在应关闭既有 firing 告警")
}

// TestStaleAlert_InvalidObservedAtFailsClosed 权威数据损坏：observed_at 存在但格式非法时，
// 评估必须返回明确错误并跳过本轮状态变更（失败关闭，与 E4 读取错误同口径）——不得把损坏
// 降级为「无观测/健康」而错误关闭既有 firing 告警（第三轮终审 #5）。
func TestStaleAlert_InvalidObservedAtFailsClosed(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": observedEntry(10*time.Minute, -1, now), // 陈旧，先建 firing
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：陈旧应触发 firing 告警")

	// 注入损坏 observed_at（存在但无法解析）：评估返回错误，既有 firing 告警保持 firing。
	reader.entries["1:gpt-4"] = map[string]any{entryObservedAtKey: "corrupted-not-rfc3339"}
	err := svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4")
	require.Error(t, err, "非法 observed_at 应沿评估调用链返回错误")
	require.Contains(t, err.Error(), "invalid observed_at")
	require.NotNil(t, store.activeFiring(dims), "权威数据损坏不得关闭既有 firing 告警（不得降级为健康态）")
}

// TestStaleAlert_NonStringObservedAtFailsClosed observed_at 存在但类型非法（数字/布尔/对象）
// 同样权威数据损坏：评估返回明确错误，既有 firing 告警保持 firing，不得降级为「无观测/健康」
// 而错误关闭（第四轮终审 E21，E16 语义的类型维度补全）。
func TestStaleAlert_NonStringObservedAtFailsClosed(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": observedEntry(10*time.Minute, -1, now), // 陈旧，先建 firing
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：陈旧应触发 firing 告警")

	for _, bad := range []any{float64(1720000000), true, map[string]any{"x": "y"}} {
		reader.entries["1:gpt-4"] = map[string]any{entryObservedAtKey: bad}
		err := svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4")
		require.Error(t, err, "类型 %T 的 observed_at 应沿评估调用链返回错误", bad)
		require.Contains(t, err.Error(), "invalid observed_at", "类型 %T 应返回明确损坏错误", bad)
		require.NotNil(t, store.activeFiring(dims), "类型 %T 损坏不得关闭既有 firing 告警", bad)
	}
}

// TestStaleAlert_NullObservedAtIsNoObservation 字面 null observed_at 视作无观测：不建告警、
// 不报错（与「存在但类型非法」严格区分，第四轮终审 E21）。
func TestStaleAlert_NullObservedAtIsNoObservation(t *testing.T) {
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": {entryObservedAtKey: nil},
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.Nil(t, store.activeFiring(dims), "字面 null observed_at = 无观测，不得建告警")
}

// TestStaleAccountLevel_InvalidObservedAtFailsClosed 账号级维同样受 #5 保护：损坏保持 firing。
func TestStaleAccountLevel_InvalidObservedAtFailsClosed(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	key := fmt.Sprintf("%d:%s", 1, tokenHarborAccountLevelProbeScope)
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		key: observedEntry(10*time.Minute, -1, now),
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountLevel, freshnessDimAccountID: int64(1)}

	require.NoError(t, svc.EvaluateAccountLevelFreshness(context.Background(), 1))
	require.NotNil(t, store.activeFiring(dims), "前置：陈旧应触发账号级 firing 告警")

	reader.entries[key] = map[string]any{entryObservedAtKey: "corrupted"}
	err := svc.EvaluateAccountLevelFreshness(context.Background(), 1)
	require.Error(t, err, "账号级非法 observed_at 应返回错误")
	require.NotNil(t, store.activeFiring(dims), "账号级权威数据损坏不得关闭既有 firing 告警")
}

// TestStaleAlert_ValidObservedAtUnchanged 合法时间路径行为不变：非法观测修复为合法陈旧值后
// 评估仍按陈旧语义触发；再刷新为阈值内则关闭（锁定 #5 未改既有生命周期）。
func TestStaleAlert_ValidObservedAtUnchanged(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": observedEntry(10*time.Minute, -1, now),
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims))
	reader.entries["1:gpt-4"] = observedEntry(10*time.Second, -1, now)
	require.NoError(t, svc.EvaluateAccountModelFreshness(context.Background(), 1, "gpt-4"))
	require.Nil(t, store.activeFiring(dims), "合法观测刷新为阈值内应关闭告警（既有语义不变）")
}

// ---------- 账号级维 ----------

// TestStaleAccountLevelAlert_FiresAndResolves 账号级（无模型键）维度陈旧告警生命周期。
func TestStaleAccountLevelAlert_FiresAndResolves(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		fmt.Sprintf("%d:%s", 1, tokenHarborAccountLevelProbeScope): observedEntry(10*time.Minute, -1, now),
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountLevel, freshnessDimAccountID: int64(1)}

	require.NoError(t, svc.EvaluateAccountLevelFreshness(context.Background(), 1))
	require.NotNil(t, store.activeFiring(dims))

	reader.entries[fmt.Sprintf("%d:%s", 1, tokenHarborAccountLevelProbeScope)] = observedEntry(10*time.Second, -1, now)
	require.NoError(t, svc.EvaluateAccountLevelFreshness(context.Background(), 1))
	require.Nil(t, store.activeFiring(dims))
}

// ---------- 渠道维 ----------

// TestChannelAlert_Lifecycle 渠道陈旧告警生命周期：error 且陈旧 → firing；
// 回升 operational → 原子关闭；operational/仅无数据不建告警；TokenHarbor 零流量无永久告警。
func TestChannelAlert_Lifecycle(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	svc := newStaleService(store, nil, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimChannel, freshnessDimChannelID: int64(7)}

	// error + 陈旧观测 -> firing
	svc.deriver = &fakeDeriver{status: MonitorStatusError, observedAt: now.Add(-10 * time.Minute), eligible: true}
	require.NoError(t, svc.EvaluateChannelFreshness(context.Background(), 7))
	require.NotNil(t, store.activeFiring(dims), "error 且陈旧应触发渠道告警")

	// 回升 operational -> 关闭
	svc.deriver = &fakeDeriver{status: MonitorStatusOperational, observedAt: now.Add(-10 * time.Minute), eligible: false}
	require.NoError(t, svc.EvaluateChannelFreshness(context.Background(), 7))
	require.Nil(t, store.activeFiring(dims), "聚合状态回升应原子关闭渠道告警")
}

// TestChannelAlert_NoAlertForOperationalOrNoData operational / 仅无数据（无权威观测时间）
// 渠道不建告警；TokenHarbor 零流量渠道（operational + 观测时间零值）无永久告警。
func TestChannelAlert_NoAlertForOperationalOrNoData(t *testing.T) {
	now := time.Time{}
	store := &fakeFreshnessStore{}
	svc := newStaleService(store, nil, &fakeBounds{ub: 0}, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimChannel, freshnessDimChannelID: int64(7)}

	// operational + 观测时间零值（零流量无数据） -> 不建告警。
	svc.deriver = &fakeDeriver{status: MonitorStatusOperational, observedAt: now, eligible: false}
	require.NoError(t, svc.EvaluateChannelFreshness(context.Background(), 7))
	require.Nil(t, store.activeFiring(dims), "零流量/仅无数据渠道不得建陈旧告警")

	// 仅 degraded 但观测时间零值（无权威观测）-> 不建告警。
	svc.deriver = &fakeDeriver{status: MonitorStatusDegraded, observedAt: now, eligible: true}
	require.NoError(t, svc.EvaluateChannelFreshness(context.Background(), 7))
	require.Nil(t, store.activeFiring(dims), "无权威观测时间的渠道不得建陈旧告警")
}

// TestChannelAlertClosedOnRecovery_Pure 直接验证聚合状态回升的关闭判定（验收 6）。
func TestChannelAlertClosedOnRecovery_Pure(t *testing.T) {
	require.True(t, ChannelAlertClosedOnRecovery(MonitorStatusError, MonitorStatusOperational))
	require.True(t, ChannelAlertClosedOnRecovery(MonitorStatusFailed, MonitorStatusOperational))
	require.True(t, ChannelAlertClosedOnRecovery(MonitorStatusDegraded, MonitorStatusOperational))
	require.False(t, ChannelAlertClosedOnRecovery(MonitorStatusError, MonitorStatusError))
	require.False(t, ChannelAlertClosedOnRecovery(MonitorStatusFailed, MonitorStatusDegraded))
	require.False(t, ChannelAlertClosedOnRecovery(MonitorStatusOperational, MonitorStatusDegraded))
}

// TestFreshnessAlertResolver_HookedInWriteEntry 验证「恢复按同一原子状态变更关闭对应告警」
// 真的挂在唯一状态写入口恢复路径内（非轮询）：成功迁移触发关闭钩子且维度与候选一致；
// 免费档 429（确认仍受限）与不可分类结果（只记尝试时间）不得触发关闭。
func TestFreshnessAlertResolver_HookedInWriteEntry(t *testing.T) {
	rl := NewRateLimitService(newD2ProbeObsRepo(), nil, nil, nil, nil)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	var calls []string
	rl.SetFreshnessAlertResolver(func(_ context.Context, accountID int64, scope string, accountLevel bool) error {
		calls = append(calls, fmt.Sprintf("%d|%s|%t", accountID, scope, accountLevel))
		return nil
	})

	// 模型级成功（限流条目清除）→ 关闭账号+模型维告警。
	require.NoError(t, rl.ApplyModelRateLimitObservation(context.Background(), 7, "gpt-4", ModelRateLimitObservation{
		EventTime: now,
		Outcome:   ProbeOutcomeSuccess,
	}))
	// 账号级成功 → 关闭账号级维告警（两维互不掩盖）。
	require.NoError(t, rl.ApplyModelRateLimitObservation(context.Background(), 7, tokenHarborAccountLevelProbeScope, ModelRateLimitObservation{
		EventTime:    now,
		Outcome:      ProbeOutcomeSuccess,
		AccountLevel: true,
	}))
	// 已识别免费档 429：确认仍受限，只刷观测时间 → 不关闭告警。
	require.NoError(t, rl.ApplyModelRateLimitObservation(context.Background(), 8, "gpt-4", ModelRateLimitObservation{
		EventTime: now,
		Outcome:   ProbeOutcomeFreeTier429,
		ResetAt:   now.Add(time.Hour),
		Reason:    "tokenharbor_free_tier_exhausted",
	}))
	// 不可分类结果：只记尝试时间 → 不关闭告警（尝试时间不解除告警）。
	require.NoError(t, rl.ApplyModelRateLimitObservation(context.Background(), 9, "gpt-4", ModelRateLimitObservation{
		EventTime: now,
		Outcome:   ProbeOutcomeUnclassified,
	}))

	require.Equal(t, []string{
		"7|gpt-4|false",
		"7|tokenharbor_account_level_probe|true",
	}, calls, "关闭钩子只在恢复迁移的同一状态变更内触发，且维度与候选一致")
}

// ---------- #7 维度命中：GetActiveFreshnessAlert 按完整维度精确查询 ----------

// TestOpsAlertGetActiveFreshnessAlert_DimensionExact 验证 GetActiveFreshnessAlert 把完整
// dims 走仓储 DimensionExact 精确过滤（不设固定 Limit），活跃事件超上限不依赖内存扫描
// 上门限——false 阴性（目标不在前 N 条）只在仓储过滤结果为空时发生，且为确切"不存在"。
func TestOpsAlertGetActiveFreshnessAlert_DimensionExact(t *testing.T) {
	var captured *OpsAlertEventFilter
	repo := &opsRepoMock{
		ListAlertEventsFn: func(_ context.Context, filter *OpsAlertEventFilter) ([]*OpsAlertEvent, error) {
			captured = filter
			// 模拟仓储按维度精确过滤后仅返回目标事件（第 200+ 条也不会被截断丢失）。
			return []*OpsAlertEvent{{ID: 42, Status: OpsAlertStatusFiring}}, nil
		},
	}
	svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	dims := map[string]any{
		freshnessDimKind:      freshnessDimAccountModel,
		freshnessDimAccountID: int64(1),
		freshnessDimScope:     "gpt-4",
	}
	ev, err := svc.GetActiveFreshnessAlert(context.Background(), dims)
	require.NoError(t, err)
	require.NotNil(t, ev, "仓储精确过滤命中目标应返回事件")
	require.Equal(t, int64(42), ev.ID)

	require.NotNil(t, captured, "应构造查询 filter")
	require.Equal(t, OpsAlertStatusFiring, captured.Status, "只查 firing")
	require.Equal(t, dims, captured.DimensionExact, "完整维度须精确传入仓储过滤")
	require.Zero(t, captured.Limit, "不得携带固定上限（0=仓储默认），存在性判定不受上限约束")
	require.Nil(t, captured.BeforeFiredAt, "单事件精确查询不需要游标分页")
}

// TestOpsAlertGetActiveFreshnessAlert_NoHit 仓储按维度精确过滤返回空 → 返回 nil（存在性判定为假），
// 非固定上限截断的语义（终审 #7：全局固定上限不能代表"不存在"）。
func TestOpsAlertGetActiveFreshnessAlert_NoHit(t *testing.T) {
	repo := &opsRepoMock{
		ListAlertEventsFn: func(_ context.Context, filter *OpsAlertEventFilter) ([]*OpsAlertEvent, error) {
			return []*OpsAlertEvent{}, nil
		},
	}
	svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimChannel, freshnessDimChannelID: int64(9)}

	ev, err := svc.GetActiveFreshnessAlert(context.Background(), dims)
	require.NoError(t, err)
	require.Nil(t, ev, "无目标维度 firing 事件应返回 nil")
}

// TestOpsAlertGetActiveFreshnessAlert_InvalidDims 空维度被拒绝（不空查全表）。
func TestOpsAlertGetActiveFreshnessAlert_InvalidDims(t *testing.T) {
	svc := NewOpsService(&opsRepoMock{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := svc.GetActiveFreshnessAlert(context.Background(), map[string]any{})
	require.Error(t, err, "空维度应报错")
}

// ---------- E45：候选过期孤儿告警清扫（CloseOrphanedFreshnessAlerts） ----------

func accountModelDims(accountID int64, scope string) map[string]any {
	return map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: accountID, freshnessDimScope: scope}
}

func firingAlert(accountID int64, scope string) *OpsAlertEvent {
	return &OpsAlertEvent{
		ID:         int64(len(scope) + int(accountID)), // 仅用于唯一性，测试内不依赖具体值
		Status:     OpsAlertStatusFiring,
		Dimensions: accountModelDims(accountID, scope),
		FiredAt:    time.Now(),
	}
}

// TestCloseOrphanedFreshnessAlerts_PreciseResetExpired 先触发陈旧 firing 告警，再令该 (account,scope)
// 条目 precise_reset=true 且 rate_limit_reset_at 已过期（E42 后从探测候选剔除）→ 孤儿清扫应关闭告警。
func TestCloseOrphanedFreshnessAlerts_PreciseResetExpired(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": {
			freshEntryPreciseResetKey: true,
			freshEntryResetAtKey:      now.Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		},
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	svc.SetFreshnessClock(func() time.Time { return now })

	dims := accountModelDims(1, "gpt-4")
	store.events = append(store.events, firingAlert(1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：应有 firing 孤儿告警")

	require.NoError(t, svc.CloseOrphanedFreshnessAlerts(context.Background()))
	require.Nil(t, store.activeFiring(dims), "precise_reset=true 且 reset_at 已过期 → 孤儿清扫应关闭该 firing 告警")
}

// TestCloseOrphanedFreshnessAlerts_EntryMissing 条目缺失（候选已退出/清除）→ 关闭。
func TestCloseOrphanedFreshnessAlerts_EntryMissing(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{}} // 无条目
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	svc.SetFreshnessClock(func() time.Time { return now })

	dims := accountModelDims(1, "gpt-4")
	store.events = append(store.events, firingAlert(1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：应有 firing 孤儿告警")

	require.NoError(t, svc.CloseOrphanedFreshnessAlerts(context.Background()))
	require.Nil(t, store.activeFiring(dims), "条目缺失 → 孤儿清扫应关闭该 firing 告警")
}

// TestCloseOrphanedFreshnessAlerts_UngatedClosePath 验证 E47：孤儿关闭走未门禁窄面
// ResolveFreshnessAlertOnRecovery，而非 syncFreshness 的 GetActiveFreshnessAlert / UpdateAlertEventStatus
// （后者在监控开关关闭时会被 RequireMonitoringEnabled 门禁拒绝 → ErrOpsDisabled）。通过 fake 计数器断言
// 调用路径：孤儿关闭只命中 ResolveFreshnessAlertOnRecovery，不触发 syncFreshness 的 Get/Update。
func TestCloseOrphanedFreshnessAlerts_UngatedClosePath(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{}} // 条目缺失 → 关闭
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	svc.SetFreshnessClock(func() time.Time { return now })

	dims := accountModelDims(1, "gpt-4")
	store.events = append(store.events, firingAlert(1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：应有 firing 孤儿告警")

	require.NoError(t, svc.CloseOrphanedFreshnessAlerts(context.Background()))

	require.Nil(t, store.activeFiring(dims), "孤儿告警应被关闭")
	require.Equal(t, 1, store.resolveRecoveryCalls, "孤儿关闭应走 ResolveFreshnessAlertOnRecovery（未门禁窄面）")
	require.Equal(t, 0, store.getActiveCalls, "孤儿关闭不得经 syncFreshness 的 GetActiveFreshnessAlert（会撞门禁）")
	require.Equal(t, 0, store.updateCalls, "孤儿关闭不得经 syncFreshness 的 UpdateAlertEventStatus（会撞门禁）")
}

// TestCloseOrphanedFreshnessAlerts_PreciseFalseSentinel precise_reset=false 哨兵（reset_at 远期）→ 保留。
func TestCloseOrphanedFreshnessAlerts_PreciseFalseSentinel(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": {
			freshEntryPreciseResetKey: false, // 哨兵（无信号）
			freshEntryResetAtKey:      now.Add(365 * 24 * time.Hour).UTC().Format(time.RFC3339Nano),
		},
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	svc.SetFreshnessClock(func() time.Time { return now })

	dims := accountModelDims(1, "gpt-4")
	store.events = append(store.events, firingAlert(1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：应有 firing 告警")

	require.NoError(t, svc.CloseOrphanedFreshnessAlerts(context.Background()))
	require.NotNil(t, store.activeFiring(dims), "precise_reset=false 哨兵不吃到期剔除 → 告警应保留（非孤儿）")
}

// TestCloseOrphanedFreshnessAlerts_PreciseTrueNotExpired precise=true 但未到期 → 保留。
func TestCloseOrphanedFreshnessAlerts_PreciseTrueNotExpired(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": {
			freshEntryPreciseResetKey: true,
			freshEntryResetAtKey:      now.Add(time.Hour).UTC().Format(time.RFC3339Nano),
		},
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	svc.SetFreshnessClock(func() time.Time { return now })

	dims := accountModelDims(1, "gpt-4")
	store.events = append(store.events, firingAlert(1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：应有 firing 告警")

	require.NoError(t, svc.CloseOrphanedFreshnessAlerts(context.Background()))
	require.NotNil(t, store.activeFiring(dims), "precise=true 未到期 → 仍活动，告警保留")
}

// TestCloseOrphanedFreshnessAlerts_ReaderError reader 读取错误 → 条目保持 firing 且返回聚合错误
// （E46：由静默跳过返回 nil 改为跳过并暴露，触发 RunOnce 既有 freshness_orphan_sweep_failed Warn）。
func TestCloseOrphanedFreshnessAlerts_ReaderError(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{}, err: fmt.Errorf("db down")}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	svc.SetFreshnessClock(func() time.Time { return now })

	dims := accountModelDims(1, "gpt-4")
	store.events = append(store.events, firingAlert(1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：应有 firing 告警")

	// 读取错误须跳过本轮（保持 firing）并沿返回路径聚合上报（非静默 nil）。
	err := svc.CloseOrphanedFreshnessAlerts(context.Background())
	require.Error(t, err, "reader 错误须以聚合错误暴露（触发 sweep_failed Warn）")
	require.Contains(t, err.Error(), "skipped 1 entries")
	require.NotNil(t, store.activeFiring(dims), "reader 错误不得错误关闭 firing 告警")
}

// TestCloseOrphanedFreshnessAlerts_ResetAtUnparseable reset_at 格式非法 → 条目保持 firing 且返回聚合错误。
func TestCloseOrphanedFreshnessAlerts_ResetAtUnparseable(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": {
			freshEntryPreciseResetKey: true,
			freshEntryResetAtKey:      "not-a-time",
		},
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	svc.SetFreshnessClock(func() time.Time { return now })

	dims := accountModelDims(1, "gpt-4")
	store.events = append(store.events, firingAlert(1, "gpt-4"))
	require.NotNil(t, store.activeFiring(dims), "前置：应有 firing 告警")

	// 解析失败须跳过（保持 firing）并聚合上报（失败关闭语义不变，只加可观测性）。
	err := svc.CloseOrphanedFreshnessAlerts(context.Background())
	require.Error(t, err, "reset_at 解析失败须以聚合错误暴露")
	require.Contains(t, err.Error(), "skipped 1 entries")
	require.NotNil(t, store.activeFiring(dims), "reset_at 解析失败 → 数据不明，告警保留（失败关闭）")
}

// TestCloseOrphanedFreshnessAlerts_StoreListError store 列表错误 → 沿调用链返回错误（失败关闭）。
func TestCloseOrphanedFreshnessAlerts_StoreListError(t *testing.T) {
	store := &errorInjectingFreshnessStore{getErr: fmt.Errorf("list down")}
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{}}
	svc := NewFreshnessAlertService(store, reader, &fakeBounds{ub: 0}, nil)

	err := svc.CloseOrphanedFreshnessAlerts(context.Background())
	require.Error(t, err, "store 列表错误应失败关闭并沿调用链返回")
	require.Contains(t, err.Error(), "list down")
}

// TestCloseOrphanedFreshnessAlerts_OtherDimsUnaffected channel / account_level 维 firing 告警不受本清扫影响
// （孤儿清扫仅处理账号+模型维：store 列表侧已按 kind 过滤，其余维度不会被传入评估/关闭路径）。
func TestCloseOrphanedFreshnessAlerts_OtherDimsUnaffected(t *testing.T) {
	now := time.Now()
	store := &fakeFreshnessStore{}
	// 账号+模型维条目仍活动（precise=false 哨兵）→ 该维告警保留；channel/account_level 被过滤不处理。
	reader := &fakeFreshnessReader{entries: map[string]map[string]any{
		"1:gpt-4": {freshEntryPreciseResetKey: false, freshEntryResetAtKey: now.Add(time.Hour).UTC().Format(time.RFC3339Nano)},
	}}
	svc := newStaleService(store, reader, &fakeBounds{ub: 0}, nil)
	svc.SetFreshnessClock(func() time.Time { return now })

	accountModel := accountModelDims(1, "gpt-4")
	channelDims := map[string]any{freshnessDimKind: freshnessDimChannel, freshnessDimChannelID: int64(9)}
	accountLevelDims := map[string]any{freshnessDimKind: freshnessDimAccountLevel, freshnessDimAccountID: int64(1)}

	store.events = append(store.events,
		&OpsAlertEvent{ID: 1, Status: OpsAlertStatusFiring, Dimensions: accountModel, FiredAt: now},
		&OpsAlertEvent{ID: 2, Status: OpsAlertStatusFiring, Dimensions: channelDims, FiredAt: now},
		&OpsAlertEvent{ID: 3, Status: OpsAlertStatusFiring, Dimensions: accountLevelDims, FiredAt: now},
	)

	require.NoError(t, svc.CloseOrphanedFreshnessAlerts(context.Background()))

	require.NotNil(t, store.activeFiring(accountModel), "账号+模型维仍活动 → 保留")
	require.NotNil(t, store.activeFiring(channelDims), "渠道维不受本清扫影响 → 保留")
	require.NotNil(t, store.activeFiring(accountLevelDims), "账号级维不受本清扫影响 → 保留")
}

// TestListActiveFreshnessAlerts_KindFilter 验证 OpsService 列表侧仅返回账号+模型维 firing 告警
// （其余维度被 Go 侧过滤），孤儿清扫据此只处理 account_model 维。
func TestListActiveFreshnessAlerts_KindFilter(t *testing.T) {
	repo := newInMemoryAlertRepo()
	svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	am := accountModelDims(1, "gpt-4")
	ch := map[string]any{freshnessDimKind: freshnessDimChannel, freshnessDimChannelID: int64(9)}
	al := map[string]any{freshnessDimKind: freshnessDimAccountLevel, freshnessDimAccountID: int64(1)}
	_, err := repo.CreateAlertEvent(context.Background(), &OpsAlertEvent{Status: OpsAlertStatusFiring, Dimensions: am, FiredAt: time.Now()})
	require.NoError(t, err)
	_, err = repo.CreateAlertEvent(context.Background(), &OpsAlertEvent{Status: OpsAlertStatusFiring, Dimensions: ch, FiredAt: time.Now()})
	require.NoError(t, err)
	_, err = repo.CreateAlertEvent(context.Background(), &OpsAlertEvent{Status: OpsAlertStatusFiring, Dimensions: al, FiredAt: time.Now()})
	require.NoError(t, err)
	// 一条已 resolved 的 account_model 不应被列出。
	_, err = repo.CreateAlertEvent(context.Background(), &OpsAlertEvent{Status: OpsAlertStatusResolved, Dimensions: accountModelDims(2, "gpt-3"), FiredAt: time.Now()})
	require.NoError(t, err)

	got, err := svc.ListActiveFreshnessAlerts(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1, "仅返回 firing 的 account_model 维告警")
	require.Equal(t, freshnessDimAccountModel, got[0].Dimensions[freshnessDimKind])
	require.Equal(t, int64(1), got[0].Dimensions[freshnessDimAccountID])
}

// ---------- E46：ListActiveFreshnessAlerts 完整游标分页 ----------

// 构造一页 firing 事件（内存侧按 kind 区分）。pageIndex 仅用于制造不同的 fired_at 排序，
// 不影响分页逻辑判定。
func pagingEvents(kind string, count int, startID int64, base time.Time) []*OpsAlertEvent {
	out := make([]*OpsAlertEvent, 0, count)
	for i := 0; i < count; i++ {
		dims := map[string]any{freshnessDimKind: kind}
		if kind == freshnessDimAccountModel {
			dims[freshnessDimAccountID] = int64(i + 1)
			dims[freshnessDimScope] = "model-" + strconv.Itoa(i)
		} else {
			dims[freshnessDimChannelID] = int64(i + 1)
		}
		out = append(out, &OpsAlertEvent{
			ID:         startID + int64(i),
			Status:     OpsAlertStatusFiring,
			Dimensions: dims,
			FiredAt:    base.Add(-time.Duration(i) * time.Second),
		})
	}
	return out
}

// TestListActiveFreshnessAlerts_PaginationAcrossPages 核心回归：firing 总数超过单页上限时，
// 按 fired_at DESC 占据前窗的渠道/其他维告警会挡住后续账号+模型维告警；默认单页（limit=100/500）
// 会漏掉这批 account_model 告警。完整游标分页必须跨页收集到它们。
func TestListActiveFreshnessAlerts_PaginationAcrossPages(t *testing.T) {
	const pageSize = 500
	// 第一页：满页非 account_model 维（渠道维），把 account_model 维挤出单页窗口。
	// 第二页：含 account_model 维 firing 告警（真实场景里 firing 总数超限时它们排在后面）。
	page1 := pagingEvents(freshnessDimChannel, pageSize, 1, time.Now())
	page2 := pagingEvents(freshnessDimAccountModel, 3, 1001, time.Now().Add(-time.Hour))

	repo := &opsRepoMock{
		ListAlertEventsFn: func(_ context.Context, filter *OpsAlertEventFilter) ([]*OpsAlertEvent, error) {
			if filter.BeforeFiredAt == nil && filter.BeforeID == nil {
				return page1, nil
			}
			return page2, nil
		},
	}
	svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	got, err := svc.ListActiveFreshnessAlerts(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 3, "跨页仍应收集到全部 account_model 维 firing 告警（默认单页会漏）")
	for _, ev := range got {
		require.Equal(t, freshnessDimAccountModel, ev.Dimensions[freshnessDimKind])
	}
}

// TestListActiveFreshnessAlerts_PaginationTerminates 分页到耗尽（最后一页 < limit）正常退出，
// 不陷入死循环。
func TestListActiveFreshnessAlerts_PaginationTerminates(t *testing.T) {
	calls := 0
	base := time.Now()
	repo := &opsRepoMock{
		ListAlertEventsFn: func(_ context.Context, filter *OpsAlertEventFilter) ([]*OpsAlertEvent, error) {
			calls++
			switch calls {
			case 1:
				return pagingEvents(freshnessDimChannel, 500, 1, base), nil
			case 2:
				return pagingEvents(freshnessDimChannel, 500, 1001, base.Add(-10*time.Minute)), nil
			default:
				// 末页 < 页上限 → 触发耗尽退出。
				return pagingEvents(freshnessDimChannel, 3, 2001, base.Add(-20*time.Minute)), nil
			}
		},
	}
	svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	got, err := svc.ListActiveFreshnessAlerts(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 0, "全为渠道维 → 内存过滤后无 account_model 告警；但必须正常终止（非死循环）")
	require.LessOrEqual(t, calls, 3, "应恰好 3 次分页调用后终止")
}

// ---------- E46：CloseOrphanedFreshnessAlerts 逐条失败可观测性 ----------


