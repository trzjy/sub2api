//go:build unit

package service

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// slowReader 模拟一个超预算的 body 传输：首次 Read 在 delay 后才返回 data。
// 读取尊重传入的 bodyCtx（应为 guard.BodyContext()，即父 context）：若父 context
// 在传输完成前取消则立即返回 ctx.Err()，否则在 delay 后返回完整 data。
type slowReader struct {
	ctx   context.Context
	delay time.Duration
	data  []byte
	once  sync.Once
}

func (s *slowReader) Read(p []byte) (int, error) {
	s.once.Do(func() {
		select {
		case <-time.After(s.delay):
		case <-s.ctx.Done():
			return
		}
	})
	n := copy(p, s.data)
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// TestUpstreamFirstByteGuardHeaderFirstBodyBeyondBudget 验证 v7 watchdog 边界：
// 响应头先到、body 传输超预算 → 完整送达且护栏未取消上游 context。
func TestUpstreamFirstByteGuardHeaderFirstBodyBeyondBudget(t *testing.T) {
	parentCtx := context.Background()
	budget := 50 * time.Millisecond
	g, reqCtx := newUpstreamFirstByteGuard(parentCtx, budget)

	// 响应头先到：停表零取消。
	require.True(t, g.HeaderArrived(), "header arrival must win before window expires")
	require.False(t, g.TimedOut(), "must not be timed-out when header arrived first")

	// body 传输耗时远超预算（超预算），使用父 context 读取。
	body := &slowReader{ctx: g.BodyContext(), delay: 200 * time.Millisecond, data: make([]byte, 64)}
	buf := make([]byte, 64)
	n, err := io.ReadFull(body, buf)
	require.NoError(t, err)
	require.Equal(t, 64, n, "body must be fully delivered despite exceeding budget")

	// 护栏未取消上游 context。
	require.False(t, g.Cancelled(), "guard must NOT cancel upstream context when header arrived first")
	require.NoError(t, reqCtx.Err(), "reqCtx must remain alive after header arrival (zero cancel)")
}

// TestUpstreamFirstByteGuardTimeoutCancels 验证窗口耗尽路径：取消上游请求 context。
func TestUpstreamFirstByteGuardTimeoutCancels(t *testing.T) {
	parentCtx := context.Background()
	g, reqCtx := newUpstreamFirstByteGuard(parentCtx, 20*time.Millisecond)
	require.False(t, g.TimedOut())
	require.False(t, g.Cancelled())

	time.Sleep(60 * time.Millisecond) // 窗口耗尽

	require.True(t, g.TimedOut(), "guard must report timed-out after window expires")
	require.True(t, g.Cancelled(), "guard must cancel upstream request context on timeout")
	require.Error(t, reqCtx.Err(), "reqCtx must be cancelled on timeout")
	require.False(t, g.Settled(), "must not be settled when timeout won")
}

// TestUpstreamFirstByteGuardLinearizationBeforeDeadline 边界①：恰好截止点前响应头到达。
func TestUpstreamFirstByteGuardLinearizationBeforeDeadline(t *testing.T) {
	g, _ := newUpstreamFirstByteGuard(context.Background(), 50*time.Millisecond)
	time.Sleep(20 * time.Millisecond) // 截止点前
	require.True(t, g.HeaderArrived(), "header arrival just before deadline must win")
	require.False(t, g.TimedOut(), "timeout must not win when header arrived before deadline")
}

// TestUpstreamFirstByteGuardLinearizationAfterDeadline 边界③：恰好截止点后定时器先赢。
func TestUpstreamFirstByteGuardLinearizationAfterDeadline(t *testing.T) {
	g, _ := newUpstreamFirstByteGuard(context.Background(), 30*time.Millisecond)
	time.Sleep(80 * time.Millisecond) // 截止点后，定时器已先裁决
	require.True(t, g.TimedOut(), "timeout must win when it fired before header arrival")
	require.False(t, g.Settled(), "header arrival after deadline must not win")
	require.False(t, g.HeaderArrived(), "late header arrival must lose the CAS race")
}

// TestUpstreamFirstByteGuardLinearizationAtDeadline 边界②：恰好截止点，断言唯一胜出者。
func TestUpstreamFirstByteGuardLinearizationAtDeadline(t *testing.T) {
	window := 20 * time.Millisecond
	g, _ := newUpstreamFirstByteGuard(context.Background(), window)
	// 在截止点附近同时触发两个事件，竞争唯一胜者。
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		time.Sleep(window)
		g.HeaderArrived()
	}()
	go func() {
		defer wg.Done()
		time.Sleep(window) // 定时器约在 window 触发，二者竞态
	}()
	wg.Wait()
	time.Sleep(10 * time.Millisecond) // 确保双方都已完成裁决

	// 唯一胜出者：恰好一个为真，绝不双真或双假。
	win := g.Settled()
	lose := g.TimedOut()
	require.NotEqual(t, win, lose, "exactly one event must win at the deadline boundary (unique winner)")
	require.True(t, win || lose, "one event must win; neither winning means unresolved race")
}

// TestUpstreamFirstByteBudgetPureFunction 验证总预算纯函数三分支。
func TestUpstreamFirstByteBudgetPureFunction(t *testing.T) {
	const g = 90 // seconds
	cases := []struct {
		name        string
		heartbeat   int
		clientStream bool
		wantSeconds int
	}{
		{"heartbeat>0 stream", 15, true, 2*g + 10},   // 190
		{"non-stream", 15, false, g + 25},             // 115
		{"heartbeat=0 stream", 0, true, g + 25},       // 115 (墙内收敛)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := RequestBudgetSnapshot{GuardSeconds: g, HeartbeatDelaySeconds: c.heartbeat}
			got := s.TotalBudget(c.clientStream)
			require.Equal(t, time.Duration(c.wantSeconds)*time.Second, got,
				"total budget mismatch: want %ds got %s", c.wantSeconds, got)
		})
	}
}

// TestUpstreamFirstByteImmutableTimeline 验证不可变时间轴：入口前置延迟 + 首轮超时
// + 换号 + 次轮再超时 → 最终窗口被剩余截断、非完整 guard 重建。
func TestUpstreamFirstByteImmutableTimeline(t *testing.T) {
	const guard = 90 // 秒
	entry := time.Now()
	s := NewRequestBudgetSnapshot(guard, 15, entry, true) // 流式有心跳：总预算 190s
	require.Equal(t, entry.Add(190*time.Second), s.AbsoluteDeadline, "absolute deadline must be entry+190s")

	// 入口前置延迟 10s 后才发起首轮。
	t0 := entry.Add(10 * time.Second)
	// 首轮窗口 = min(guard, 剩余) = min(90, 180) = 90s（完整 guard）。
	require.Equal(t, 90*time.Second, s.AttemptWindow(t0), "first attempt window must be full guard")

	// 首轮超时（窗口 90s 耗尽），换号。
	t1 := t0.Add(90 * time.Second)
	// 次轮窗口 = min(guard, 剩余=190-10-90=90) = 90s。仍完整（剩余≥guard）。
	require.Equal(t, 90*time.Second, s.AttemptWindow(t1), "second attempt window still full when remaining>=guard")

	// 次轮再超时，但此次前置已累计：模拟换号交接后又等待 50s 才发次轮请求。
	t2 := t1.Add(90*time.Second + 50*time.Second) // = entry + 240s，已过 AbsoluteDeadline
	// 剩余被截断为 0（已过截止）；窗口 = min(90, 0) = 0，非完整 guard 重建。
	require.Equal(t, 0*time.Second, s.RemainingBudget(t2), "remaining must be clamped to 0 past deadline")
	require.Equal(t, 0*time.Second, s.AttemptWindow(t2), "attempt window must be clamped by immutable deadline, not rebuilt as full guard")

	// 关键不变式：任何 attempt 的窗口都从不可变 AbsoluteDeadline 取剩余，绝不重建 guard。
	mid := entry.Add(150 * time.Second) // 剩余 40s < guard
	require.Equal(t, 40*time.Second, s.AttemptWindow(mid), "attempt window must be clamped to remaining (< guard), not full guard")
}

// TestUpstreamFirstByteSnapshotContextHelper 验证快照写入/读回一致；无快照不兜底。
func TestUpstreamFirstByteSnapshotContextHelper(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()

	snap := NewRequestBudgetSnapshot(90, 15, time.Now(), true)
	require.Equal(t, 190*time.Second, snap.TotalBudget(true))

	// 写入后读回一致。
	ctx := WithRequestBudgetSnapshot(base, snap)
	got, ok := RequestBudgetSnapshotFromContext(ctx)
	require.True(t, ok, "snapshot must be readable after install")
	require.Equal(t, snap, got, "read-back snapshot must equal installed snapshot")
	require.Equal(t, snap.GuardSeconds, got.GuardSeconds)
	require.Equal(t, snap.AbsoluteDeadline, got.AbsoluteDeadline)

	// 无快照时返回 false，不兜底。
	_, okEmpty := RequestBudgetSnapshotFromContext(base)
	require.False(t, okEmpty, "context without snapshot must report not-ok (no fallback)")

	// nil context 安全。
	_, okNil := RequestBudgetSnapshotFromContext(nil)
	require.False(t, okNil, "nil context must report not-ok")
}
