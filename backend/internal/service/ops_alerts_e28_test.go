//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------- 内存 OpsRepository（仅实现告警事件相关方法，其余继承 opsRepoMock 零值实现） ----------

// inMemoryAlertRepo 是测试用内存 OpsRepository：基于 opsRepoMock 嵌入满足完整接口，
// 仅重写告警事件读写方法并支持错误注入，用于验证恢复关闭窄面与 syncFreshness 错误传播。
type inMemoryAlertRepo struct {
	*opsRepoMock
	mu     sync.Mutex
	events []*OpsAlertEvent
	nextID int64
	getErr, createErr, updateErr error
}

func (r *inMemoryAlertRepo) CreateAlertEvent(_ context.Context, e *OpsAlertEvent) (*OpsAlertEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return nil, r.createErr
	}
	r.nextID++
	cp := *e
	cp.ID = r.nextID
	if cp.FiredAt.IsZero() {
		cp.FiredAt = time.Now()
	}
	r.events = append(r.events, &cp)
	return &cp, nil
}

func (r *inMemoryAlertRepo) ListAlertEvents(_ context.Context, filter *OpsAlertEventFilter) ([]*OpsAlertEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	if filter == nil {
		return nil, nil
	}
	var out []*OpsAlertEvent
	for _, ev := range r.events {
		if filter.Status != "" && ev.Status != filter.Status {
			continue
		}
		if filter.DimensionExact != nil && !dimensionsMatch(ev.Dimensions, filter.DimensionExact) {
			continue
		}
		cp := *ev
		out = append(out, &cp)
	}
	return out, nil
}

func (r *inMemoryAlertRepo) UpdateAlertEventStatus(_ context.Context, id int64, status string, resolvedAt *time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return r.updateErr
	}
	for _, ev := range r.events {
		if ev.ID == id {
			ev.Status = status
			ev.ResolvedAt = resolvedAt
			return nil
		}
	}
	return nil
}

func newInMemoryAlertRepo() *inMemoryAlertRepo {
	return &inMemoryAlertRepo{opsRepoMock: &opsRepoMock{}}
}

// ---------- 错误注入版 FreshnessAlertStore（验证 syncFreshness 错误传播 #4） ----------

type errorInjectingFreshnessStore struct {
	mu                 sync.Mutex
	events             []*OpsAlertEvent
	nextID             int64
	getErr, createErr, updateErr error
}

func (s *errorInjectingFreshnessStore) CreateAlertEvent(_ context.Context, e *OpsAlertEvent) (*OpsAlertEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.nextID++
	cp := *e
	cp.ID = s.nextID
	s.events = append(s.events, &cp)
	return &cp, nil
}

func (s *errorInjectingFreshnessStore) GetActiveFreshnessAlert(_ context.Context, dims map[string]any) (*OpsAlertEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	for _, ev := range s.events {
		if ev.Status == OpsAlertStatusFiring && dimensionsMatch(ev.Dimensions, dims) {
			return ev, nil
		}
	}
	return nil, nil
}

func (s *errorInjectingFreshnessStore) UpdateAlertEventStatus(_ context.Context, id int64, status string, resolvedAt *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErr != nil {
		return s.updateErr
	}
	for _, ev := range s.events {
		if ev.ID == id {
			ev.Status = status
			ev.ResolvedAt = resolvedAt
			return nil
		}
	}
	return nil
}

// ListActiveFreshnessAlerts 列出 firing 的账号+模型维新鲜度告警（孤儿清扫用，E45）。
func (s *errorInjectingFreshnessStore) ListActiveFreshnessAlerts(_ context.Context) ([]*OpsAlertEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	var out []*OpsAlertEvent
	for _, ev := range s.events {
		if ev.Status == OpsAlertStatusFiring && ev.Dimensions[freshnessDimKind] == freshnessDimAccountModel {
			out = append(out, ev)
		}
	}
	return out, nil
}

// ResolveFreshnessAlertOnRecovery 满足 FreshnessAlertStore 接口（E47 接口扩面）：未门禁恢复关闭窄面
// 的测试桩实现（无活动告警 no-op，有则原子关闭）。
func (s *errorInjectingFreshnessStore) ResolveFreshnessAlertOnRecovery(_ context.Context, dims map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if ev.Status == OpsAlertStatusFiring && dimensionsMatch(ev.Dimensions, dims) {
			now := time.Now()
			ev.Status = OpsAlertStatusResolved
			ev.ResolvedAt = &now
			return nil
		}
	}
	return nil
}

// ---------- E28 #3：恢复关闭窄面脱离监控开关门禁 ----------

// TestE28_RecoveryNarrowFace_ClosesWithMonitoringDisabled 监控开关关闭场景：恢复关闭窄面仍能关闭
// 既有 firing 告警；含告警关闭的恢复事务不再因开关回滚。管理端公共面（GetActiveFreshnessAlert /
// UpdateAlertEventStatus）在开关关闭时仍被门禁拒绝（回归：门禁保留）。
func TestE28_RecoveryNarrowFace_ClosesWithMonitoringDisabled(t *testing.T) {
	repo := newInMemoryAlertRepo()
	svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	// 建立 firing 告警（模拟 Evaluate* 已触发）。
	_, err := repo.CreateAlertEvent(context.Background(), &OpsAlertEvent{Status: OpsAlertStatusFiring, Dimensions: dims, FiredAt: time.Now()})
	require.NoError(t, err)

	// 关闭监控开关。
	svc.runtimeSettings.Store(&opsRuntimeSettingsSnapshot{monitoringEnabled: false})

	// 回归：管理端公共面在监控关闭时仍被门禁拒绝。
	_, pubErr := svc.GetActiveFreshnessAlert(context.Background(), dims)
	require.ErrorIs(t, pubErr, ErrOpsDisabled, "管理端公共查询面在监控关闭时仍应被拒绝")
	updErr := svc.UpdateAlertEventStatus(context.Background(), 1, OpsAlertStatusResolved, nil)
	require.ErrorIs(t, updErr, ErrOpsDisabled, "管理端公共关闭面在监控关闭时仍应被拒绝")

	// 恢复窄面不受门禁约束，仍能关闭 firing 告警（恢复事务不因开关回滚）。
	require.NoError(t, svc.ResolveFreshnessAlertOnRecovery(context.Background(), dims), "监控关闭时恢复窄面仍须成功关闭告警")

	// 重新开启监控，确认告警已被关闭（resolved）。
	svc.runtimeSettings.Store(&opsRuntimeSettingsSnapshot{monitoringEnabled: true})
	ev, err := svc.GetActiveFreshnessAlert(context.Background(), dims)
	require.NoError(t, err)
	require.Nil(t, ev, "恢复窄面应已原子关闭 firing 告警")
}

// TestE28_RecoveryNarrowFace_EdgeCases 恢复窄面边界：无活动告警正常空结果；空维度拒绝；
// 存储故障失败关闭；opsRepo 不可用 ServiceUnavailable。
func TestE28_RecoveryNarrowFace_EdgeCases(t *testing.T) {
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	t.Run("no active alert returns nil", func(t *testing.T) {
		svc := NewOpsService(newInMemoryAlertRepo(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		require.NoError(t, svc.ResolveFreshnessAlertOnRecovery(context.Background(), dims), "无活动告警应为正常空结果")
	})

	t.Run("empty dims rejected", func(t *testing.T) {
		svc := NewOpsService(newInMemoryAlertRepo(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		require.Error(t, svc.ResolveFreshnessAlertOnRecovery(context.Background(), map[string]any{}), "空维度应被拒绝")
	})

	t.Run("get storage failure propagates", func(t *testing.T) {
		repo := newInMemoryAlertRepo()
		repo.getErr = errors.New("db down on get")
		svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		err := svc.ResolveFreshnessAlertOnRecovery(context.Background(), dims)
		require.Error(t, err, "存储读取故障应失败关闭")
		require.Contains(t, err.Error(), "db down on get")
	})

	t.Run("close storage failure propagates", func(t *testing.T) {
		repo := newInMemoryAlertRepo()
		_, cerr := repo.CreateAlertEvent(context.Background(), &OpsAlertEvent{Status: OpsAlertStatusFiring, Dimensions: dims, FiredAt: time.Now()})
		require.NoError(t, cerr)
		repo.updateErr = errors.New("db down on close")
		svc := NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		err := svc.ResolveFreshnessAlertOnRecovery(context.Background(), dims)
		require.Error(t, err, "存储关闭故障应失败关闭")
		require.Contains(t, err.Error(), "db down on close")
	})
}

// ---------- E28 #4：syncFreshness 三类错误传播 ----------

// TestE28_SyncFreshness_PropagatesErrors 验证 syncFreshness 把 Get/Create/Update(close) 三类
// store 错误沿返回值传播（不再 Warn + return nil），且「无活动告警」保持正常空结果。
func TestE28_SyncFreshness_PropagatesErrors(t *testing.T) {
	dims := map[string]any{freshnessDimKind: freshnessDimAccountModel, freshnessDimAccountID: int64(1), freshnessDimScope: "gpt-4"}

	t.Run("get error propagates", func(t *testing.T) {
		store := &errorInjectingFreshnessStore{getErr: errors.New("get fail")}
		svc := NewFreshnessAlertService(store, &fakeFreshnessReader{}, &fakeBounds{}, nil)
		err := svc.syncFreshness(context.Background(), dims, true, "t", "d")
		require.Error(t, err, "Get 错误应沿调用链传播")
		require.Contains(t, err.Error(), "get fail")
	})

	t.Run("create error propagates", func(t *testing.T) {
		store := &errorInjectingFreshnessStore{createErr: errors.New("create fail")}
		svc := NewFreshnessAlertService(store, &fakeFreshnessReader{}, &fakeBounds{}, nil)
		err := svc.syncFreshness(context.Background(), dims, true, "t", "d")
		require.Error(t, err, "Create 错误应沿调用链传播")
		require.Contains(t, err.Error(), "create fail")
	})

	t.Run("close error propagates", func(t *testing.T) {
		store := &errorInjectingFreshnessStore{}
		store.events = append(store.events, &OpsAlertEvent{ID: 1, Status: OpsAlertStatusFiring, Dimensions: dims})
		store.updateErr = errors.New("close fail")
		svc := NewFreshnessAlertService(store, &fakeFreshnessReader{}, &fakeBounds{}, nil)
		err := svc.syncFreshness(context.Background(), dims, false, "t", "d")
		require.Error(t, err, "Update(close) 错误应沿调用链传播")
		require.Contains(t, err.Error(), "close fail")
	})

	t.Run("no active alert is normal empty result", func(t *testing.T) {
		store := &errorInjectingFreshnessStore{}
		svc := NewFreshnessAlertService(store, &fakeFreshnessReader{}, &fakeBounds{}, nil)
		require.NoError(t, svc.syncFreshness(context.Background(), dims, false, "t", "d"), "无活动告警应为正常空结果而非错误")
	})
}
