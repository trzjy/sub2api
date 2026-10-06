//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------- 测试桩（仅实现 GetByID，其余方法借接口嵌入留 nil） ----------

type cmAnomalyMonitorRepoStub struct {
	ChannelMonitorRepository
	m      *ChannelMonitor
	latest []*ChannelMonitorLatest
}

func (s *cmAnomalyMonitorRepoStub) GetByID(_ context.Context, _ int64) (*ChannelMonitor, error) {
	return s.m, nil
}

// ListLatestPerModel 补齐 D3b 接线所需的读取面（前次会话缺此方法会 panic）。
// 测试通过 latest 字段注入 per-model 观测，不触碰生产 repo。
func (s *cmAnomalyMonitorRepoStub) ListLatestPerModel(_ context.Context, _ int64) ([]*ChannelMonitorLatest, error) {
	return s.latest, nil
}

type cmAnomalyAccountRepoStub struct {
	AccountRepository
	a *Account
}

func (s *cmAnomalyAccountRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
	return s.a, nil
}

type cmAnomalyEncryptor struct{}

func (cmAnomalyEncryptor) Encrypt(string) (string, error) { return "x", nil }
func (cmAnomalyEncryptor) Decrypt(string) (string, error) { return "x", nil }

func newWiredChannelMonitorService(t *testing.T, m *ChannelMonitor, a *Account, latest ...*ChannelMonitorLatest) *ChannelMonitorService {
	t.Helper()
	monitorRepo := &cmAnomalyMonitorRepoStub{m: m, latest: latest}
	accountRepo := &cmAnomalyAccountRepoStub{a: a}
	svc := NewChannelMonitorService(monitorRepo, cmAnomalyEncryptor{})
	svc.SetAccountAnomalySource(NewChannelAccountAnomalySource(monitorRepo, accountRepo))
	return svc
}

// ---------- 异常源自身（D2 读取面收敛） ----------

func TestChannelAccountAnomalySource_ListsActiveKinds(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	accountID := int64(7)
	m := &ChannelMonitor{ID: 1, AccountID: &accountID}
	a := &Account{
		ID:                     accountID,
		TempUnschedulableUntil: &future,
		Extra: map[string]any{
			"model_rate_limits": map[string]any{"gpt-5": map[string]any{"rate_limit_reset_at": "2099-01-01T00:00:00Z"}},
		},
	}
	src := NewChannelAccountAnomalySource(&cmAnomalyMonitorRepoStub{m: m}, &cmAnomalyAccountRepoStub{a: a})

	got, err := src.ListActiveAnomalies(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	kinds := map[string]bool{}
	for _, an := range got {
		kinds[an.Kind] = an.Active
	}
	require.True(t, kinds["temp_unschedulable"], "temp_unschedulable must be active")
	require.True(t, kinds["model_rate_limited"], "model_rate_limited must be reported (no-op on channel)")
}

func TestChannelAccountAnomalySource_UnlinkedReturnsNil(t *testing.T) {
	m := &ChannelMonitor{ID: 1} // 无 AccountID
	a := &Account{ID: 99, TempUnschedulableUntil: ptrTime(time.Now().Add(time.Hour))}
	src := NewChannelAccountAnomalySource(&cmAnomalyMonitorRepoStub{m: m}, &cmAnomalyAccountRepoStub{a: a})
	got, err := src.ListActiveAnomalies(context.Background(), 1)
	require.NoError(t, err)
	require.Nil(t, got)
}

// repoErrorAnomalySourceStub 让 monitor/account repo 读取返回错误（非 not-found），
// 用于验证 #8 失败关闭：读取失败必须沿 ListActiveAnomalies 返回 error。
type repoErrorAnomalySourceStub struct {
	ChannelMonitorRepository
	monitorErr error
}

func (s *repoErrorAnomalySourceStub) GetByID(_ context.Context, _ int64) (*ChannelMonitor, error) {
	if s.monitorErr != nil {
		return nil, s.monitorErr
	}
	return nil, nil
}

type repoErrorAccountRepoStub struct {
	AccountRepository
	getErr error
}

func (s *repoErrorAccountRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
	return nil, s.getErr
}

func TestChannelAccountAnomalySource_ReadErrorPropagates(t *testing.T) {
	accountID := int64(7)
	readErr := errors.New("db transient failure")

	t.Run("monitor repo read error", func(t *testing.T) {
		src := NewChannelAccountAnomalySource(
			&repoErrorAnomalySourceStub{monitorErr: readErr},
			&cmAnomalyAccountRepoStub{},
		)
		got, err := src.ListActiveAnomalies(context.Background(), 1)
		require.ErrorIs(t, err, readErr, "monitor read error must propagate (fail closed)")
		require.Nil(t, got)
	})

	t.Run("account repo read error", func(t *testing.T) {
		src := NewChannelAccountAnomalySource(
			&cmAnomalyMonitorRepoStub{m: &ChannelMonitor{ID: 1, AccountID: &accountID}},
			&repoErrorAccountRepoStub{getErr: readErr},
		)
		got, err := src.ListActiveAnomalies(context.Background(), 1)
		require.ErrorIs(t, err, readErr, "account read error must propagate (fail closed)")
		require.Nil(t, got)
	})

	t.Run("not found is not an error", func(t *testing.T) {
		src := NewChannelAccountAnomalySource(
			&cmAnomalyMonitorRepoStub{m: nil},
			&repoErrorAccountRepoStub{getErr: readErr},
		)
		got, err := src.ListActiveAnomalies(context.Background(), 1)
		require.NoError(t, err, "nil monitor (unlinked) must not surface as read error")
		require.Nil(t, got)
	})
}

// ---------- 接线定向（Done when） ----------

// 注入停调异常源 → 渠道聚合状态 degraded。
func TestChannelDerive_WiredTempUnschedulableDegrades(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	accountID := int64(42)
	m := &ChannelMonitor{ID: 1, AccountID: &accountID}
	a := &Account{ID: accountID, TempUnschedulableUntil: &future}

	svc := newWiredChannelMonitorService(t, m, a)
	got, err := svc.DeriveChannelStatus(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, MonitorStatusDegraded, got.Status, "活跃停调必须使渠道降到 degraded")
	require.True(t, got.AlertEligible)
}

// 异常源恢复（停调过期）+ 权威观测 → 随聚合回升 operational。
func TestChannelDerive_WiredRecoveryRises(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	accountID := int64(42)
	m := &ChannelMonitor{ID: 1, AccountID: &accountID}
	a := &Account{ID: accountID, TempUnschedulableUntil: &past}

	svc := newWiredChannelMonitorService(t, m, a,
		&ChannelMonitorLatest{Model: "a", Status: MonitorStatusOperational, CheckedAt: now})
	got, err := svc.DeriveChannelStatus(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, MonitorStatusOperational, got.Status, "过期停调且有权威观测不得再降级")
}

// #9：停调过期且无任何权威观测 → 空状态（横线），不伪装为 operational。
func TestChannelDerive_NoObservationEmptyState(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	accountID := int64(42)
	m := &ChannelMonitor{ID: 1, AccountID: &accountID}
	a := &Account{ID: accountID, TempUnschedulableUntil: &past}

	svc := newWiredChannelMonitorService(t, m, a)
	got, err := svc.DeriveChannelStatus(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, "", got.Status, "无权威观测必须输出空状态")
	require.True(t, got.ObservedAt.IsZero())
	require.False(t, got.AlertEligible)
}

// 顺序测试：停调持续中出现业务成功 → 仍保持 degraded（业务成功不解除停调事实）。
func TestChannelDerive_WiredBusinessSuccessKeepsDegraded(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	accountID := int64(42)
	m := &ChannelMonitor{ID: 1, AccountID: &accountID}
	// 注入一条业务成功的渠道观测信号（经桩的 latest 字段传递）。
	a := &Account{ID: accountID, TempUnschedulableUntil: &future}

	svc := newWiredChannelMonitorService(t, m, a, &ChannelMonitorLatest{Model: "a", Status: MonitorStatusOperational, CheckedAt: now})
	got, err := svc.DeriveChannelStatus(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, MonitorStatusDegraded, got.Status, "业务成功不得解除停调导致的 degraded")
}
