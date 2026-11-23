//go:build unit

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestSchedulerCacheSnapshotPreservesMaskUpstreamIdentity 钉死 OC-T1-FIX3 的根因形态：
// 动态门控 TierRoutingGateOpen 经 schedulerSnapshot.ListSchedulableAccounts → GetSnapshot
// 读的是元数据投影（sched:meta:，由 buildSchedulerMetadataAccount 经 filterSchedulerExtra
// 裁剪）。若该投影未透传 mask_upstream_identity，则快照解码出的账号丢失伪装标记，
// IsIdentityMaskedAccount 恒为 false → 门控在生产永不开（事故观测：门零次开启、
// kimi-k3 小请求全落未伪装号）。
//
// 本测试用真实 miniredis 缓存走完整 写入→激活→读取 投影链路，验证 GetSnapshot 解出的
// 账号仍保留 mask_upstream_identity（即与选号路径经同一 ListSchedulableAccounts 同源拿到
// 同一候选形状，门控可见伪装账号）。
func TestSchedulerCacheSnapshotPreservesMaskUpstreamIdentity(t *testing.T) {
	ctx := context.Background()
	cache, _ := newSchedulerCacheUnitWithRedis(t)

	bucket := service.SchedulerBucket{
		GroupID:  52,
		Platform: "kimi@agg1",
		Mode:     service.SchedulerModeSingle,
	}

	// 对应事故账号 241：Status=active、Schedulable=true、Extra.mask_upstream_identity=true、
	// Credentials.model_mapping 含 kimi-k3。
	masked := service.Account{
		ID:          241,
		Platform:    service.PlatformKimi,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra: map[string]any{
			"mask_upstream_identity": true,
		},
		Credentials: map[string]any{
			"model_mapping": map[string]any{"kimi-k3": "kimi-k3-upstream"},
		},
	}

	// 写入并激活快照（与 rebuild 路径一致）。
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{masked}))

	// 门控时刻消费的正是这一行来源。
	got, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit, "快照应为命中")
	require.Len(t, got, 1)

	// 根因断言：投影必须保留伪装标记，门控才能识别 241 为伪装账号。
	require.True(t, service.IsIdentityMaskedAccount(got[0]),
		"快照元数据投影必须保留 mask_upstream_identity，否则门控经同一来源也判不出伪装账号")
	require.Equal(t, map[string]string{"kimi-k3": "kimi-k3-upstream"}, got[0].GetModelMapping(),
		"快照投影必须保留 model_mapping（选号/门控均依赖）")
}

func TestSchedulerCacheSnapshotPreservesUpstreamModelMetadata(t *testing.T) {
	ctx := context.Background()
	cache, _ := newSchedulerCacheUnitWithRedis(t)
	bucket := service.SchedulerBucket{
		GroupID:  53,
		Platform: "kimi@agg1",
		Mode:     service.SchedulerModeSingle,
	}

	account := service.Account{
		ID:          242,
		Platform:    service.PlatformCodeBuddy,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"model_mapping": map[string]any{"kimi-k3": "hy3"}},
	}
	account.SetUpstreamModelMetadataSnapshot(service.UpstreamModelMetadataSnapshot{
		Source: "account",
		Models: map[string]service.UpstreamModelMetadata{"hy3": {ID: "hy3"}},
	})
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{account}))

	got, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, got, 1)
	snapshot := got[0].GetUpstreamModelMetadataSnapshot()
	require.NotNil(t, snapshot, "scheduler metadata projection must retain the upstream catalog used by HY3 eligibility")
	require.Equal(t, map[string]service.UpstreamModelMetadata{"hy3": {ID: "hy3"}}, snapshot.Models)
}

func TestSchedulerCacheSetAccountRefreshesUpstreamModelMetadataProjection(t *testing.T) {
	ctx := context.Background()
	cache, _ := newSchedulerCacheUnitWithRedis(t)
	bucket := service.SchedulerBucket{
		GroupID:  54,
		Platform: "kimi@agg1",
		Mode:     service.SchedulerModeSingle,
	}

	account := service.Account{
		ID:          243,
		Platform:    service.PlatformCodeBuddy,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{account}))

	account.SetUpstreamModelMetadataSnapshot(service.UpstreamModelMetadataSnapshot{
		Source: "account",
		Models: map[string]service.UpstreamModelMetadata{"hy3": {ID: "hy3"}},
	})
	require.NoError(t, cache.SetAccount(ctx, &account))

	got, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, got, 1)
	snapshot := got[0].GetUpstreamModelMetadataSnapshot()
	require.NotNil(t, snapshot, "SetAccount refresh must update the same metadata projection consumed by directory eligibility")
	require.Contains(t, snapshot.Models, "hy3")
}

func TestSchedulerCacheSnapshotDoesNotInventUpstreamModelMetadata(t *testing.T) {
	ctx := context.Background()
	cache, _ := newSchedulerCacheUnitWithRedis(t)
	bucket := service.SchedulerBucket{
		GroupID:  55,
		Platform: "kimi@agg1",
		Mode:     service.SchedulerModeSingle,
	}

	accounts := []service.Account{
		{ID: 244, Platform: service.PlatformCodeBuddy, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1},
		{ID: 245, Platform: service.PlatformCodeBuddy, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1,
			Extra: map[string]any{service.UpstreamModelMetadataExtraKey: map[string]any{"source": "account", "models": map[string]any{}}}},
	}
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, accounts))

	got, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, got, 2)
	require.Nil(t, got[0].GetUpstreamModelMetadataSnapshot(), "missing catalog remains missing")
	require.Nil(t, got[1].GetUpstreamModelMetadataSnapshot(), "empty catalog remains unusable")
}
