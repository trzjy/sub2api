package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// TestOpenAIAPIKeyHealthCacheTripsWithinRollingWindow verifies the three-tier
// escalation: each tier trips exactly once while the failure count climbs inside
// a single rolling window (debounce via the companion tier key prevents repeated
// alerts for the same tier).
func TestOpenAIAPIKeyHealthCacheTripsWithinRollingWindow(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewTempUnschedCache(client).(service.OpenAIAPIKeyHealthCache)
	require.True(t, ok)

	ctx := context.Background()
	// watch=1, warning=2, trip=3.
	want := []struct {
		count   int64
		watch   bool
		warning bool
		trip    bool
	}{
		{1, true, false, false},
		{2, false, true, false},
		{3, false, false, true},
	}
	for i, exp := range want {
		res, err := store.RecordOpenAIAPIKeyHealthFailure(ctx, 42, 1, 1, 2, 3)
		require.NoError(t, err)
		require.EqualValues(t, exp.count, res.Count, "attempt %d count", i)
		require.Equal(t, exp.watch, res.TrippedWatch, "attempt %d watch", i)
		require.Equal(t, exp.warning, res.TrippedWarning, "attempt %d warning", i)
		require.Equal(t, exp.trip, res.TrippedTrip, "attempt %d trip", i)
	}
}

// TestOpenAIAPIKeyHealthCacheDropsFailuresOutsideRollingWindow verifies that
// failures older than the rolling window are expired, so a later failure starts a
// fresh window at count 1 (no spurious accumulation / trip).
func TestOpenAIAPIKeyHealthCacheDropsFailuresOutsideRollingWindow(t *testing.T) {
	server := miniredis.RunT(t)
	now := time.Unix(1_700_000_000, 0)
	server.SetTime(now)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewTempUnschedCache(client).(service.OpenAIAPIKeyHealthCache)
	require.True(t, ok)

	ctx := context.Background()
	res, err := store.RecordOpenAIAPIKeyHealthFailure(ctx, 42, 1, 1, 2, 3)
	require.NoError(t, err)
	require.EqualValues(t, 1, res.Count)

	// Advance beyond the 1-minute window: the old failure is dropped, so the next
	// record starts a fresh window at count 1.
	server.SetTime(now.Add(61 * time.Second))
	res, err = store.RecordOpenAIAPIKeyHealthFailure(ctx, 42, 1, 1, 2, 3)
	require.NoError(t, err)
	require.EqualValues(t, 1, res.Count)
	require.False(t, res.TrippedTrip)
}
