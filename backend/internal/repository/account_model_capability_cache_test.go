package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAccountModelCapabilityCache_SetGet(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cache := NewAccountModelCapabilityCache(rdb)

	// 未命中
	_, ok := cache.GetAccount(context.Background(), 137)
	require.False(t, ok)

	caps := []*model.AccountModelCapability{
		{ID: 1, AccountID: 137, UpstreamModel: "deepseek-v4.1-flash", Protocol: "chat_completions", SupportsVision: true, Source: "detect"},
	}
	require.NoError(t, cache.SetAccount(context.Background(), 137, caps))

	// 命中（经 Redis）
	got, ok := cache.GetAccount(context.Background(), 137)
	require.True(t, ok)
	require.Len(t, got, 1)
	require.Equal(t, "deepseek-v4.1-flash", got[0].UpstreamModel)
}

func TestAccountModelCapabilityCache_Invalidate(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cache := NewAccountModelCapabilityCache(rdb)

	caps := []*model.AccountModelCapability{
		{ID: 1, AccountID: 137, UpstreamModel: "deepseek-v4.1-flash", Protocol: "chat_completions", SupportsVision: true, Source: "detect"},
	}
	require.NoError(t, cache.SetAccount(context.Background(), 137, caps))
	require.NoError(t, cache.InvalidateAccount(context.Background(), 137))

	_, ok := cache.GetAccount(context.Background(), 137)
	require.False(t, ok)
}

// TestAccountModelCapabilityCache_PubSub 验证跨实例缓存失效广播：订阅方收到
// 携带 accountID 的通知，且本地缓存被清空（下次读取回源）。
func TestAccountModelCapabilityCache_PubSub(t *testing.T) {
	server := miniredis.RunT(t)
	publisherRedis := redis.NewClient(&redis.Options{Addr: server.Addr()})
	subscriberRedis := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = publisherRedis.Close()
		_ = subscriberRedis.Close()
	})

	publisher := NewAccountModelCapabilityCache(publisherRedis)
	subscriber := NewAccountModelCapabilityCache(subscriberRedis)

	// 订阅方先落一份本地缓存
	caps := []*model.AccountModelCapability{
		{ID: 1, AccountID: 137, UpstreamModel: "deepseek-v4.1-flash", Protocol: "chat_completions", SupportsVision: true, Source: "detect"},
	}
	require.NoError(t, subscriber.SetAccount(context.Background(), 137, caps))
	got, ok := subscriber.GetAccount(context.Background(), 137)
	require.True(t, ok)
	require.Len(t, got, 1)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	received := make(chan int64, 1)
	subscriber.SubscribeUpdates(ctx, func(accountID int64, _ bool) {
		select {
		case received <- accountID:
		default:
		}
	})

	require.Eventually(t, func() bool {
		return server.PubSubNumSub(accountModelCapabilityPubSubKey)[accountModelCapabilityPubSubKey] == 1
	}, time.Second, 10*time.Millisecond)

	// 模拟完整写路径：先删 Redis+清本地，再跨实例广播
	// （service.UpsertCapability 的 invalidateAndNotifyAccount 就是先 Invalidate 再 Notify）。
	require.NoError(t, publisher.InvalidateAccount(context.Background(), 137))
	require.NoError(t, publisher.NotifyUpdate(context.Background(), 137, false))

	select {
	case accountID := <-received:
		require.Equal(t, int64(137), accountID)
	case <-time.After(time.Second):
		t.Fatal("account model capability cache invalidation notification was not received")
	}

	// 订阅方收到广播后本地缓存被清空
	_, ok = subscriber.GetAccount(context.Background(), 137)
	require.False(t, ok)
}

// TestAccountModelCapabilityCache_PubSubJSONSuspect 验证 JSON payload 发布/订阅
// round-trip 的 suspect 两态：发布方失效成功（false）与失效失败（true）均正确透传。
func TestAccountModelCapabilityCache_PubSubJSONSuspect(t *testing.T) {
	server := miniredis.RunT(t)
	publisherRedis := redis.NewClient(&redis.Options{Addr: server.Addr()})
	subscriberRedis := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = publisherRedis.Close()
		_ = subscriberRedis.Close()
	})

	publisher := NewAccountModelCapabilityCache(publisherRedis)
	subscriber := NewAccountModelCapabilityCache(subscriberRedis)

	caps := []*model.AccountModelCapability{
		{ID: 1, AccountID: 137, UpstreamModel: "deepseek-v4.1-flash", Protocol: "chat_completions", SupportsVision: true, Source: "detect"},
	}
	require.NoError(t, subscriber.SetAccount(context.Background(), 137, caps))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	type evt struct {
		accountID    int64
		suspectRedis bool
	}
	received := make(chan evt, 2)
	subscriber.SubscribeUpdates(ctx, func(accountID int64, suspectRedis bool) {
		select {
		case received <- evt{accountID, suspectRedis}:
		default:
		}
	})

	require.Eventually(t, func() bool {
		return server.PubSubNumSub(accountModelCapabilityPubSubKey)[accountModelCapabilityPubSubKey] == 1
	}, time.Second, 10*time.Millisecond)

	// 发布方失效成功：suspectRedis=false。
	require.NoError(t, publisher.NotifyUpdate(context.Background(), 137, false))
	select {
	case e := <-received:
		require.Equal(t, int64(137), e.accountID)
		require.False(t, e.suspectRedis, "失效成功须透传 suspectRedis=false")
	case <-time.After(time.Second):
		t.Fatal("notify(false) not received")
	}

	// 发布方失效失败：suspectRedis=true。
	require.NoError(t, publisher.NotifyUpdate(context.Background(), 137, true))
	select {
	case e := <-received:
		require.Equal(t, int64(137), e.accountID)
		require.True(t, e.suspectRedis, "失效失败须透传 suspectRedis=true")
	case <-time.After(time.Second):
		t.Fatal("notify(true) not received")
	}
}

// TestAccountModelCapabilityCache_PubSubLegacyPayloadSuspectTrue 验证滚动升级期
// 旧发布者纯数字 payload → 订阅方保守按 suspectRedis=true 处理（强制 DB 回源）。
func TestAccountModelCapabilityCache_PubSubLegacyPayloadSuspectTrue(t *testing.T) {
	server := miniredis.RunT(t)
	publisherRedis := redis.NewClient(&redis.Options{Addr: server.Addr()})
	subscriberRedis := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = publisherRedis.Close()
		_ = subscriberRedis.Close()
	})

	publisher := NewAccountModelCapabilityCache(publisherRedis)
	subscriber := NewAccountModelCapabilityCache(subscriberRedis)

	_ = publisher // 旧发布者走 publisherRedis 直接 Publish 纯数字，不经新 NotifyUpdate
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	type evt struct {
		accountID    int64
		suspectRedis bool
	}
	received := make(chan evt, 1)
	subscriber.SubscribeUpdates(ctx, func(accountID int64, suspectRedis bool) {
		select {
		case received <- evt{accountID, suspectRedis}:
		default:
		}
	})

	require.Eventually(t, func() bool {
		return server.PubSubNumSub(accountModelCapabilityPubSubKey)[accountModelCapabilityPubSubKey] == 1
	}, time.Second, 10*time.Millisecond)

	// 旧发布者只发纯数字 accountID（模拟滚动升级期旧代码）。
	require.NoError(t, publisherRedis.Publish(ctx, accountModelCapabilityPubSubKey, "137").Err())
	select {
	case e := <-received:
		require.Equal(t, int64(137), e.accountID)
		require.True(t, e.suspectRedis, "旧格式纯数字 payload 须保守按 suspectRedis=true 处理")
	case <-time.After(time.Second):
		t.Fatal("legacy notify not received")
	}
}
