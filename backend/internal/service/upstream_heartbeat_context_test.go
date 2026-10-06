//go:build unit

package service

// 文件：internal/service/upstream_heartbeat_context_test.go
//
// CF524 G2a owner 传递契约单测：请求级心跳 owner 在 context 中的写/读。
// 与 G1a 快照 helper 同型；无 owner 时 (nil,false) 不兜底。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 写读一致：WithUpstreamHeartbeat 写入的 owner 必须能原样读回。
func TestUpstreamHeartbeatContextWriteReadRoundTrip(t *testing.T) {
	base := context.Background()
	_, w := newHeartbeatTestWriter()
	hb := g2aHeartbeat(base, w, 50*time.Millisecond, 20*time.Millisecond)

	ctx := WithUpstreamHeartbeat(base, hb)
	got, ok := UpstreamHeartbeatFromContext(ctx)

	require.True(t, ok, "installed heartbeat owner must be readable")
	require.Same(t, hb, got, "read-back owner must be the exact installed instance")
}

// 无 owner：返回 (nil,false)，不兜底（挂点据以走既有无护栏旧路径）。
func TestUpstreamHeartbeatContextAbsentNoFallback(t *testing.T) {
	hb, ok := UpstreamHeartbeatFromContext(context.Background())
	require.False(t, ok, "context without owner must report not-ok (no fallback)")
	require.Nil(t, hb)
}

// nil context 安全。
func TestUpstreamHeartbeatContextNilSafe(t *testing.T) {
	hb, ok := UpstreamHeartbeatFromContext(nil)
	require.False(t, ok)
	require.Nil(t, hb)
}

// 与 G1a 预算快照 helper 并存：两 key 互不干扰、各自可读。
func TestUpstreamHeartbeatContextCoexistsWithBudgetSnapshot(t *testing.T) {
	base := context.Background()
	_, w := newHeartbeatTestWriter()
	hb := g2aHeartbeat(base, w, 50*time.Millisecond, 20*time.Millisecond)
	snap := g2aSnapshot(90, 15, 190*time.Second)

	ctx := WithRequestBudgetSnapshot(base, snap)
	ctx = WithUpstreamHeartbeat(ctx, hb)

	gotSnap, okSnap := RequestBudgetSnapshotFromContext(ctx)
	gotHB, okHB := UpstreamHeartbeatFromContext(ctx)

	require.True(t, okSnap)
	require.Same(t, snap, gotSnap)
	require.True(t, okHB)
	require.Same(t, hb, gotHB)
}
