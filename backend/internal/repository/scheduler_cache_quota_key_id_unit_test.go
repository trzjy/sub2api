//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 调度投影必须复制 Cursor 账号的激活码归属 QuotaKeyID。
//
// 候选过滤走 ListSchedulableAccounts，读的是 buildSchedulerMetadataAccount 产出的精简投影；
// Cursor 共同来源门读到 nil QuotaKeyID 即判 quota_key_source_unbound 并否决该候选，
// 于是池里有可用账号却全部被排除，表现为 HTTP 503。这里守的是喂给来源门的输入。

// 用例 1：投影 + 序列化读回。
func TestSchedulerMetadataQuotaKeyID_ProjectionRoundTripPreservesBound(t *testing.T) {
	quotaKeyID := int64(5)
	account := service.Account{
		ID:         328,
		Name:       "cursor-bound",
		Platform:   service.PlatformCursor,
		Type:       service.AccountTypeOAuth,
		QuotaKeyID: &quotaKeyID,
		Credentials: map[string]any{
			"access_token":  "drop-me-secret",
			"refresh_token": "drop-me-secret",
			"model_mapping": map[string]any{"gpt-5.3": "gpt-5.3"},
		},
		Extra: map[string]any{
			"huge_internal_blob": "this-must-not-enter-meta",
		},
	}

	_, metaPayload, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)

	var cached service.Account
	require.NoError(t, json.Unmarshal(metaPayload, &cached))

	// QuotaKeyID 必须随投影进入 meta，否则来源门会否决。
	require.NotNil(t, cached.QuotaKeyID, "绑定账号的 QuotaKeyID 必须进入调度投影")
	require.Equal(t, int64(5), *cached.QuotaKeyID)
	// model_mapping 应保留。
	require.Equal(t, map[string]any{"gpt-5.3": "gpt-5.3"}, cached.Credentials["model_mapping"])
	// 不应进入 meta 的大字段被裁掉。
	require.Nil(t, cached.Extra["huge_internal_blob"])
	// 凭据机密不能进入 meta。
	require.Nil(t, cached.Credentials["access_token"])
	require.Nil(t, cached.Credentials["refresh_token"])
}

// 用例 2：空来源保持原行为。
func TestSchedulerMetadataQuotaKeyID_ProjectionRoundTripNilSource(t *testing.T) {
	account := service.Account{
		ID:         329,
		Name:       "cursor-unbound",
		Platform:   service.PlatformCursor,
		Type:       service.AccountTypeOAuth,
		QuotaKeyID: nil,
		Credentials: map[string]any{
			"access_token":  "drop-me-secret",
			"refresh_token": "drop-me-secret",
			"model_mapping": map[string]any{"gpt-5.3": "gpt-5.3"},
		},
	}

	_, metaPayload, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)

	var cached service.Account
	require.NoError(t, json.Unmarshal(metaPayload, &cached))

	require.Nil(t, cached.QuotaKeyID, "未绑定账号的 QuotaKeyID 应保持 nil")
	require.Nil(t, cached.Credentials["access_token"])
	require.Nil(t, cached.Credentials["refresh_token"])
}

// 用例 3：真实缓存读写路径。
func TestSchedulerMetadataQuotaKeyID_CacheReadWritePath(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)

	boundID := int64(328)
	unboundID := int64(329)
	boundKey := int64(5)

	bound := service.Account{
		ID:          boundID,
		Name:        "cursor-bound",
		Platform:    service.PlatformCursor,
		Type:        service.AccountTypeOAuth,
		QuotaKeyID:  &boundKey,
		Credentials: map[string]any{"access_token": "drop-me-secret", "model_mapping": map[string]any{"gpt-5.3": "gpt-5.3"}},
	}
	unbound := service.Account{
		ID:          unboundID,
		Name:        "cursor-unbound",
		Platform:    service.PlatformCursor,
		Type:        service.AccountTypeOAuth,
		QuotaKeyID:  nil,
		Credentials: map[string]any{"access_token": "drop-me-secret", "model_mapping": map[string]any{"gpt-5.3": "gpt-5.3"}},
	}

	bucket := service.SchedulerBucket{GroupID: 991, Platform: service.PlatformCursor, Mode: service.SchedulerModeSingle}
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{bound, unbound}))

	snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit, "快照必须命中")
	require.Len(t, snapshot, 2)

	var cachedBound, cachedUnbound *service.Account
	for _, acc := range snapshot {
		switch acc.ID {
		case boundID:
			cachedBound = acc
		case unboundID:
			cachedUnbound = acc
		}
	}
	require.NotNil(t, cachedBound)
	require.NotNil(t, cachedUnbound)

	// 绑定账号的 QuotaKeyID 经完整缓存路径后仍为 5。
	require.NotNil(t, cachedBound.QuotaKeyID, "绑定账号的 QuotaKeyID 必须保留")
	require.Equal(t, int64(5), *cachedBound.QuotaKeyID)
	// 未绑定账号仍为 nil。
	require.Nil(t, cachedUnbound.QuotaKeyID, "未绑定账号的 QuotaKeyID 应保持 nil")
	// 候选账号凭据里没有 access_token（机密不进 meta）。
	require.Nil(t, cachedBound.Credentials["access_token"])
	require.Nil(t, cachedUnbound.Credentials["access_token"])
	require.Equal(t, map[string]any{"gpt-5.3": "gpt-5.3"}, cachedBound.Credentials["model_mapping"])
}
