package repository

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// TestNewConcurrencyCacheSlotTTLSource 验证槽位 TTL 的取值来源：
// 配置 > 0 时原样使用（分钟 → 秒），<= 0 时回落到 concurrencySlotTTLFallbackMinutes（30），
// 绝不出现 slotTTLSeconds == 0（否则 Lua 里 expireBefore = now，会立刻清空全部槽位）。
func TestNewConcurrencyCacheSlotTTLSource(t *testing.T) {
	cases := []struct {
		name               string
		slotTTLMinutes     int
		wantSlotTTLSeconds int
	}{
		{name: "configured 30 minutes (production default)", slotTTLMinutes: 30, wantSlotTTLSeconds: 1800},
		{name: "configured 15 minutes", slotTTLMinutes: 15, wantSlotTTLSeconds: 900},
		{name: "configured 1 minute", slotTTLMinutes: 1, wantSlotTTLSeconds: 60},
		{name: "zero falls back to 30 minutes", slotTTLMinutes: 0, wantSlotTTLSeconds: 1800},
		{name: "negative falls back to 30 minutes", slotTTLMinutes: -7, wantSlotTTLSeconds: 1800},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache, ok := NewConcurrencyCache(nil, tc.slotTTLMinutes, 0).(*concurrencyCache)
			require.True(t, ok)
			require.Equal(t, tc.wantSlotTTLSeconds, cache.slotTTLSeconds)
			// 等待队列 TTL 未显式配置时跟随 slot TTL。
			require.Equal(t, tc.wantSlotTTLSeconds, cache.waitQueueTTLSeconds)
			require.Positive(t, cache.slotTTLSeconds, "slot TTL must never be zero")
		})
	}
}

// TestNewConcurrencyCacheWaitQueueTTLExplicit 验证显式传入的等待队列 TTL 不被 slot TTL 覆盖。
func TestNewConcurrencyCacheWaitQueueTTLExplicit(t *testing.T) {
	cache, ok := NewConcurrencyCache(nil, 30, 120).(*concurrencyCache)
	require.True(t, ok)
	require.Equal(t, 1800, cache.slotTTLSeconds)
	require.Equal(t, 120, cache.waitQueueTTLSeconds)
}

// TestConcurrencyCacheSlotKeyTTLUsesConfiguredValue 端到端确认写进 Redis 的 EXPIRE
// 确实来自构造参数（生产是 30 分钟 = 1800s），而不是某个写死的常量。
func TestConcurrencyCacheSlotKeyTTLUsesConfiguredValue(t *testing.T) {
	for _, slotTTLMinutes := range []int{15, 30} {
		t.Run(time.Duration(slotTTLMinutes).String(), func(t *testing.T) {
			redisServer := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			cache, ok := NewConcurrencyCache(client, slotTTLMinutes, 0).(*concurrencyCache)
			require.True(t, ok)
			ctx := context.Background()

			acquired, err := cache.AcquireAccountSlot(ctx, 4242, 1, "ttl-probe")
			require.NoError(t, err)
			require.True(t, acquired)

			want := time.Duration(slotTTLMinutes) * time.Minute
			require.Equal(t, want, redisServer.TTL(accountSlotKey(4242)))
		})
	}
}

// TestConcurrencyCacheZeroSlotTTLWouldNotHappen 反证：若兜底被误删，slotTTLSeconds 会变成 0，
// 此时 EXPIRE 传 0 会让键立即消失、已占用槽位无法续命。兜底存在时该路径不可达。
func TestConcurrencyCacheZeroSlotTTLWouldNotHappen(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	cache, ok := NewConcurrencyCache(client, 0, 0).(*concurrencyCache)
	require.True(t, ok)
	ctx := context.Background()

	require.Equal(t, 30*time.Minute, time.Duration(cache.slotTTLSeconds)*time.Second)

	acquired, err := cache.AcquireAccountSlot(ctx, 777, 1, "fallback-probe")
	require.NoError(t, err)
	require.True(t, acquired)
	require.Equal(t, 30*time.Minute, redisServer.TTL(accountSlotKey(777)))
}
