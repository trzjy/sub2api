//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// aggRepoStub 实现聚合读路径所需的 repo 子集（其余方法由嵌入的接口 nil 占位）。
// 仅覆盖 BatchMonitorStatusSummary / GetUserDetail 用到的查询方法。
type aggRepoStub struct {
	ChannelMonitorRepository
	latestByID  map[int64][]*ChannelMonitorLatest
	availByID   map[int64][]*ChannelMonitorAvailability
	monitorByID map[int64]*ChannelMonitor
}

func (r *aggRepoStub) ListLatestForMonitorIDs(_ context.Context, ids []int64) (map[int64][]*ChannelMonitorLatest, error) {
	out := make(map[int64][]*ChannelMonitorLatest, len(ids))
	for _, id := range ids {
		out[id] = r.latestByID[id]
	}
	return out, nil
}

func (r *aggRepoStub) ComputeAvailabilityForMonitors(_ context.Context, ids []int64, _ int) (map[int64][]*ChannelMonitorAvailability, error) {
	out := make(map[int64][]*ChannelMonitorAvailability, len(ids))
	for _, id := range ids {
		out[id] = r.availByID[id]
	}
	return out, nil
}

func (r *aggRepoStub) ListLatestPerModel(_ context.Context, id int64) ([]*ChannelMonitorLatest, error) {
	return r.latestByID[id], nil
}

func (r *aggRepoStub) GetByID(_ context.Context, id int64) (*ChannelMonitor, error) {
	return r.monitorByID[id], nil
}

func (r *aggRepoStub) ComputeAvailability(_ context.Context, _ int64, _ int) ([]*ChannelMonitorAvailability, error) {
	return nil, nil
}

// newAggService 构造注入账号侧异常源的聚合测试服务。
func newAggService(t *testing.T, repo *aggRepoStub, anomalies []AccountSideAnomaly) *ChannelMonitorService {
	t.Helper()
	svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{})
	if anomalies != nil {
		svc.SetAccountAnomalySource(&stubAnomalySource{anomalies: anomalies})
	}
	return svc
}

// TestAggChannelStatus_SuspensionAnomalyDegrades 验证：per-model operational +
// 账号停调异常 → 聚合输出的渠道级档位为 degraded（渠道级档位来自推导，非 per-model 直读）。
func TestAggChannelStatus_SuspensionAnomalyDegrades(t *testing.T) {
	now := time.Now()
	repo := &aggRepoStub{
		latestByID: map[int64][]*ChannelMonitorLatest{
			1: {{Model: "a", Status: MonitorStatusOperational, CheckedAt: now}},
		},
	}
	svc := newAggService(t, repo, []AccountSideAnomaly{{Kind: "temp_unschedulable", Active: true}})

	// 1) BatchMonitorStatusSummary（admin/user list 聚合输出）
	summaries := svc.BatchMonitorStatusSummary(context.Background(), []int64{1},
		map[int64]string{1: "a"}, map[int64][]string{1: nil})
	require.Equal(t, MonitorStatusDegraded, summaries[1].ChannelStatus,
		"channel tier must be degraded from account suspension anomaly")
	require.False(t, summaries[1].ChannelObservedAt.IsZero(), "account anomaly advances observed time to Now")

	// 2) GetUserDetail（详情输出）
	repo.monitorByID = map[int64]*ChannelMonitor{1: {ID: 1, PrimaryModel: "a", Enabled: true}}
	detail, err := svc.GetUserDetail(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, MonitorStatusDegraded, detail.ChannelStatus)
}

// TestAggChannelStatus_FreeTier429NoDowngrade 验证：免费档 429（model_rate_limited /
// free_tier_429）不降渠道档位，聚合输出保持 operational。
func TestAggChannelStatus_FreeTier429NoDowngrade(t *testing.T) {
	now := time.Now()
	makeRepo := func() *aggRepoStub {
		return &aggRepoStub{
			latestByID: map[int64][]*ChannelMonitorLatest{
				1: {{Model: "a", Status: MonitorStatusOperational, CheckedAt: now}},
			},
			monitorByID: map[int64]*ChannelMonitor{1: {ID: 1, PrimaryModel: "a", Enabled: true}},
		}
	}

	for _, kind := range []string{"model_rate_limited", "free_tier_429"} {
		svc := newAggService(t, makeRepo(), []AccountSideAnomaly{{Kind: kind, Active: true}})

		summaries := svc.BatchMonitorStatusSummary(context.Background(), []int64{1},
			map[int64]string{1: "a"}, map[int64][]string{1: nil})
		require.Equal(t, MonitorStatusOperational, summaries[1].ChannelStatus,
			"free-tier 429 must not downgrade channel tier (kind=%s)", kind)

		detail, err := svc.GetUserDetail(context.Background(), 1)
		require.NoError(t, err)
		require.Equal(t, MonitorStatusOperational, detail.ChannelStatus,
			"free-tier 429 must not downgrade channel tier in detail (kind=%s)", kind)
	}
}

// TestAggChannelStatus_NoDataNoAdvance 验证：无数据场景（无权威观测、无账号侧异常）
// 渠道输出空状态（前端横线），观测时间不推进（不把从未获得可用性证据的渠道伪装成 operational）。
func TestAggChannelStatus_NoDataNoAdvance(t *testing.T) {
	repo := &aggRepoStub{
		latestByID:  map[int64][]*ChannelMonitorLatest{1: {}}, // 空 latest = 无权威观测
		monitorByID: map[int64]*ChannelMonitor{1: {ID: 1, PrimaryModel: "a", Enabled: true}},
	}
	// 无账号侧异常、无 per-model 权威观测：观测时间不得推进。
	svc := newAggService(t, repo, nil)

	summaries := svc.BatchMonitorStatusSummary(context.Background(), []int64{1},
		map[int64]string{1: "a"}, map[int64][]string{1: nil})
	require.True(t, summaries[1].ChannelObservedAt.IsZero(),
		"no-data must not advance channel observed time")
	require.Equal(t, "", summaries[1].ChannelStatus,
		"no-data channel must render empty state (dash), not operational")

	detail, err := svc.GetUserDetail(context.Background(), 1)
	require.NoError(t, err)
	require.True(t, detail.ChannelObservedAt.IsZero(),
		"no-data must not advance channel observed time in detail")
	require.Equal(t, "", detail.ChannelStatus,
		"no-data channel detail must render empty state (dash)")
}

// TestAggChannelStatus_AnomalySourceReadErrorFailClosed 验证 #8 失败关闭：
// 账号侧事实来源读取错误时，聚合/详情跳过本轮渠道状态变更（保持空状态），不静默降级为无异常。
func TestAggChannelStatus_AnomalySourceReadErrorFailClosed(t *testing.T) {
	now := time.Now()
	repo := &aggRepoStub{
		latestByID: map[int64][]*ChannelMonitorLatest{
			1: {{Model: "a", Status: MonitorStatusOperational, CheckedAt: now}},
		},
		monitorByID: map[int64]*ChannelMonitor{1: {ID: 1, PrimaryModel: "a", Enabled: true}},
	}
	svc := NewChannelMonitorService(repo, &duplicateChannelMonitorEncryptor{})
	svc.SetAccountAnomalySource(&stubAnomalySource{err: errors.New("db transient failure")})

	summaries := svc.BatchMonitorStatusSummary(context.Background(), []int64{1},
		map[int64]string{1: "a"}, map[int64][]string{1: nil})
	require.Equal(t, "", summaries[1].ChannelStatus,
		"read error must skip this round's channel status (empty), never assume operational")
	require.True(t, summaries[1].ChannelObservedAt.IsZero(),
		"read error must not advance observed time")
	// per-model 明细仍保留（读取失败只影响渠道级档位）。
	require.Equal(t, MonitorStatusOperational, summaries[1].PrimaryStatus)

	detail, err := svc.GetUserDetail(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, "", detail.ChannelStatus, "read error must not publish a derived tier")
	require.True(t, detail.ChannelObservedAt.IsZero())

	// DeriveChannelStatus 单条路径必须把读取失败作为 error 暴露（供告警/handler 失败关闭）。
	_, derr := svc.DeriveChannelStatus(context.Background(), 1)
	require.Error(t, derr, "single derive path must surface the read error")
}

// TestAggChannelStatus_PerModelRowsPreserved 验证：渠道级档位由推导产出，
// 同时 per-model 明细行仍保留 raw l.Status（未被推导覆盖）。
func TestAggChannelStatus_PerModelRowsPreserved(t *testing.T) {
	now := time.Now()
	repo := &aggRepoStub{
		latestByID: map[int64][]*ChannelMonitorLatest{
			1: {
				{Model: "a", Status: MonitorStatusOperational, CheckedAt: now},
				{Model: "b", Status: MonitorStatusFailed, CheckedAt: now},
			},
		},
		monitorByID: map[int64]*ChannelMonitor{1: {ID: 1, PrimaryModel: "a", ExtraModels: []string{"b"}, Enabled: true}},
	}
	// per-model a operational + b failed → 渠道级最坏档位 failed（E3 #5 跨模型最坏聚合），
	// 账号侧停调 degraded 被更坏的 per-model failed 覆盖；per-model 行保持原状。
	svc := newAggService(t, repo, []AccountSideAnomaly{{Kind: "temp_unschedulable", Active: true}})

	summaries := svc.BatchMonitorStatusSummary(context.Background(), []int64{1},
		map[int64]string{1: "a"}, map[int64][]string{1: {"b"}})
	require.Equal(t, MonitorStatusFailed, summaries[1].ChannelStatus,
		"worst per-model tier must win over account suspension degraded")
	require.Equal(t, MonitorStatusOperational, summaries[1].PrimaryStatus, "primary per-model row preserved")
	require.Len(t, summaries[1].ExtraModels, 1)
	require.Equal(t, MonitorStatusFailed, summaries[1].ExtraModels[0].Status, "extra per-model row preserved")

	detail, err := svc.GetUserDetail(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, MonitorStatusFailed, detail.ChannelStatus)
	for _, m := range detail.Models {
		if m.Model == "a" {
			require.Equal(t, MonitorStatusOperational, m.LatestStatus)
		}
		if m.Model == "b" {
			require.Equal(t, MonitorStatusFailed, m.LatestStatus)
		}
	}
}
