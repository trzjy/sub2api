//go:build unit

package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// heartbeatObsWriter 是线程安全的观测型 gin.ResponseWriter：在 mu 下记录每次
// Write 的字节与每次 Flush 时的累计长度，供测试按时间轴断言"客户端实际收到帧"
// 与 Flush 契约，同时避免与组件 goroutine 共享 gin 内部 size 字段产生 data race
// （测试侧只经 writeLog 自身 mutex 访问，绝不读 gin.Size()）。
type heartbeatObsWriter struct {
	gin.ResponseWriter
	mu            sync.Mutex
	writeLog      [][]byte
	flushLens     []int
	headerWritten bool
}

func (w *heartbeatObsWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	w.writeLog = append(w.writeLog, append([]byte(nil), b...))
	w.mu.Unlock()
	return w.ResponseWriter.Write(b)
}

// WriteString 覆盖嵌入 gin.ResponseWriter 的 WriteString，确保 io.WriteString
// （组件写帧用）经本 writer 的 Write 记录，避免绕过 writeLog 观测。
func (w *heartbeatObsWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// WriteHeader 覆盖嵌入实现以追踪响应头是否真正写出（httptest.ResponseRecorder
// 的 Code 默认即为 200，不能用于"未写响应头"断言）。
func (w *heartbeatObsWriter) WriteHeader(code int) {
	w.mu.Lock()
	w.headerWritten = true
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w *heartbeatObsWriter) HeaderWritten() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.headerWritten
}

func (w *heartbeatObsWriter) Flush() {
	w.mu.Lock()
	w.flushLens = append(w.flushLens, w.totalWrittenLocked())
	w.mu.Unlock()
	w.ResponseWriter.Flush()
}

func (w *heartbeatObsWriter) totalWrittenLocked() int {
	n := 0
	for _, b := range w.writeLog {
		n += len(b)
	}
	return n
}

func (w *heartbeatObsWriter) heartbeatFrameCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, b := range w.writeLog {
		if string(b) == heartbeatSSEFrame {
			n++
		}
	}
	return n
}

func (w *heartbeatObsWriter) totalWritten() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.totalWrittenLocked()
}

func newHeartbeatTestWriter() (*httptest.ResponseRecorder, *heartbeatObsWriter) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	w := &heartbeatObsWriter{ResponseWriter: c.Writer}
	c.Writer = w
	return rec, w
}

// noFlusherWriter 是不实现 http.Flusher 的裸 http.ResponseWriter，用于验证"无
// Flush 能力时失败关闭"。
type noFlusherWriter struct {
	header http.Header
	code   int
	body   *bytes.Buffer
}

func (w *noFlusherWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *noFlusherWriter) Write(b []byte) (int, error) {
	if w.body == nil {
		w.body = bytes.NewBuffer(nil)
	}
	return w.body.Write(b)
}
func (w *noFlusherWriter) WriteHeader(c int) { w.code = c }

func hbSnapshot(delaySec int) *RequestBudgetSnapshot {
	return &RequestBudgetSnapshot{
		HeartbeatDelaySeconds: delaySec,
		EntryMonotonic:        time.Now(),
	}
}

func newTestHeartbeat(delay, interval time.Duration) (*UpstreamHeartbeat, *httptest.ResponseRecorder, *heartbeatObsWriter) {
	ctx := context.Background()
	rec, w := newHeartbeatTestWriter()
	now := time.Now()
	hb := NewUpstreamHeartbeat(ctx, w, hbSnapshot(1), now)
	hb.initialDelay = delay
	hb.keepAliveInterval = interval
	return hb, rec, w
}

// 晚触发：delay 前 Stop → 零字节写出。
func TestUpstreamHeartbeatLateStopZeroWrite(t *testing.T) {
	hb, _, w := newTestHeartbeat(100*time.Millisecond, 20*time.Millisecond)
	require.NoError(t, hb.Start())
	time.Sleep(20 * time.Millisecond) // 仍在 delay 内
	hb.Stop()

	require.False(t, hb.IsCommitted())
	require.Equal(t, int64(0), hb.CommittedBytes())
	require.Zero(t, w.totalWritten())
	require.False(t, w.HeaderWritten())
	require.Empty(t, w.flushLens)
}

// 悬挂 ≥ delay → 客户端按时间轴实际收到初始头与连续 keep-alive 帧（Flush 契约）。
func TestUpstreamHeartbeatFlushContract(t *testing.T) {
	hb, _, w := newTestHeartbeat(50*time.Millisecond, 20*time.Millisecond)
	require.NoError(t, hb.Start())

	require.Eventually(t, func() bool {
		return w.heartbeatFrameCount() >= 2
	}, 2*time.Second, 5*time.Millisecond, "client should actually receive >=2 keep-alive frames")

	require.True(t, w.HeaderWritten())
	require.Contains(t, w.writeLogContent(), heartbeatSSEFrame)
	// Flush 契约：初始头与每帧后均显式 Flush（至少 header flush + 多帧 flush）
	require.GreaterOrEqual(t, len(w.flushLens), 2)

	hb.Stop()
}

func (w *heartbeatObsWriter) writeLogContent() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var sb bytes.Buffer
	for _, b := range w.writeLog {
		sb.Write(b)
	}
	return sb.String()
}

// Flush 失败关闭：无 Flusher 的 ResponseWriter → Start 返回错误、零写出。
func TestUpstreamHeartbeatNoFlusherFailsClosed(t *testing.T) {
	raw := &noFlusherWriter{}
	hb := NewUpstreamHeartbeat(context.Background(), raw, hbSnapshot(1), time.Now())

	err := hb.Start()
	require.Error(t, err)
	require.False(t, hb.IsCommitted())
	require.Equal(t, int64(0), hb.CommittedBytes())
	// Resume 同样失败关闭
	require.Error(t, hb.Resume())
	require.Nil(t, raw.body)
	require.Equal(t, 0, raw.code)
}

// 晚到交接：Stop 后调用方写语义流/error 帧，无帧交错、无 data race。
func TestUpstreamHeartbeatLateHandoffNoInterleave(t *testing.T) {
	hb, _, w := newTestHeartbeat(30*time.Millisecond, 20*time.Millisecond)
	require.NoError(t, hb.Start())
	require.Eventually(t, hb.IsCommitted, 2*time.Second, 5*time.Millisecond)

	// 上游晚到（4xx/5xx）：组件返回 AfterCommit，G2 走晚到契约
	res := hb.OnUpstreamHeaderArrived()
	require.Equal(t, HeartbeatHeaderAfterCommit, res)

	framesBeforeCaller := w.heartbeatFrameCount()
	hb.Stop() // stop-and-wait，返回后调用方才可写

	// 调用方（G2）写出 SSE error 帧
	callerFrame := "event: error\ndata: {\"type\":\"error\"}\n\n"
	_, err := w.Write([]byte(callerFrame))
	require.NoError(t, err)

	// 心跳 goroutine 已退出，不应再有任何 keep-alive 帧交错写入
	require.Equal(t, framesBeforeCaller, w.heartbeatFrameCount())
	require.Contains(t, w.writeLogContent(), callerFrame)
	require.True(t, w.HeaderWritten())
}

// 交接连续性（v6）：heartbeat=60、首轮 guard=90 超时、次轮续挂 → 交接空窗 < delay
// （Resume 首帧即时，不重等 delay）。
func TestUpstreamHeartbeatResumeHandoffGap(t *testing.T) {
	hb, _, w := newTestHeartbeat(200*time.Millisecond, 20*time.Millisecond)
	require.NoError(t, hb.Start())
	require.Eventually(t, hb.IsCommitted, 2*time.Second, 5*time.Millisecond)

	framesBeforeGuard := w.heartbeatFrameCount()
	// 首轮 guard 超时（终态）
	hb.OnGuardDecided()

	// 交接空窗内（>keepAlive，证明 guard 终态后零 tick 写出）保持无新帧
	time.Sleep(80 * time.Millisecond)
	framesDuringGap := w.heartbeatFrameCount()
	require.Equal(t, framesBeforeGuard, framesDuringGap, "guard terminal must drop all ticks during handoff gap")

	// 次轮 attempt 开始：Resume 即时续写首帧（不重等 initialDelay）
	start := time.Now()
	require.NoError(t, hb.Resume())
	require.Eventually(t, func() bool {
		return w.heartbeatFrameCount() > framesDuringGap
	}, 200*time.Millisecond, 5*time.Millisecond, "Resume must flush a frame immediately")
	elapsed := time.Since(start)
	require.Less(t, elapsed, hb.initialDelay, "resume handoff gap must be < delay (no re-wait)")

	hb.Stop()
}

// 优先级断言（v4）：响应事件 vs tick 同至 —— 临界 4xx/5xx 用例1：响应先到 → 无 SSE 200 提交。
func TestUpstreamHeartbeatPriorityResponseWins(t *testing.T) {
	hb, _, w := newTestHeartbeat(100*time.Millisecond, 20*time.Millisecond)
	require.NoError(t, hb.Start())

	// 响应头在 delay 内先到（优先级①：响应事件胜出）
	res := hb.OnUpstreamHeaderArrived()
	require.Equal(t, HeartbeatHeaderRejectedHeartbeatNotStarted, res)

	time.Sleep(150 * time.Millisecond) // 越过 delay，确认心跳永不启动
	require.False(t, hb.IsCommitted())
	require.Equal(t, int64(0), hb.CommittedBytes())
	require.Zero(t, w.totalWritten())
	require.False(t, w.HeaderWritten())

	hb.Stop()
}

// 优先级断言（v4）：响应事件 vs tick 同至 —— 临界 4xx/5xx 用例2：tick 先到 → 晚到契约 error 帧。
func TestUpstreamHeartbeatPriorityTickWins(t *testing.T) {
	hb, _, w := newTestHeartbeat(30*time.Millisecond, 20*time.Millisecond)
	require.NoError(t, hb.Start())

	// tick 先到：至少一帧 keep-alive 已写出（SSE 200 之后一个 interval 才发首帧）
	require.Eventually(t, func() bool {
		return w.heartbeatFrameCount() > 0
	}, 2*time.Second, 5*time.Millisecond)

	// 随后晚到 4xx/5xx：组件返回 AfterCommit，G2 据此走晚到契约 error 帧
	res := hb.OnUpstreamHeaderArrived()
	require.Equal(t, HeartbeatHeaderAfterCommit, res)
	require.True(t, hb.IsCommitted())
	require.GreaterOrEqual(t, w.heartbeatFrameCount(), 1)

	hb.Stop()
	require.True(t, w.HeaderWritten())
}

// 优先级断言（v4）：guard 终态 vs tick 同至 → 终态后零 tick 写出。
func TestUpstreamHeartbeatPriorityGuardTerminal(t *testing.T) {
	hb, _, w := newTestHeartbeat(30*time.Millisecond, 20*time.Millisecond)
	require.NoError(t, hb.Start())

	// guard 终态在 delay 内先到（优先级②：guard 终态胜出）
	hb.OnGuardDecided()

	time.Sleep(100 * time.Millisecond) // 越过 delay，确认零 tick 写出
	require.False(t, hb.IsCommitted())
	require.Equal(t, int64(0), hb.CommittedBytes())
	require.Zero(t, w.totalWritten())
	require.False(t, w.HeaderWritten())

	hb.Stop()
}

// 三交叠竞态：上游响应与 tick 同至 —— 唯一胜者（提交 XOR 拒绝），零写出污染，-race 干净。
func TestUpstreamHeartbeatRaceUpstreamVsTick(t *testing.T) {
	for i := 0; i < 80; i++ {
		hb, _, w := newTestHeartbeat(30*time.Millisecond, 20*time.Millisecond)
		hb.Start()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			hb.OnUpstreamHeaderArrived()
		}()
		wg.Wait()
		time.Sleep(60 * time.Millisecond) // 越过 delay
		committed := hb.IsCommitted()
		bytes := hb.CommittedBytes()
		if committed {
			require.Greater(t, bytes, int64(0))
			require.True(t, w.HeaderWritten())
		} else {
			require.Equal(t, int64(0), bytes)
			require.Zero(t, w.totalWritten())
			require.False(t, w.HeaderWritten())
		}
		hb.Stop()
	}
}

// 三交叠竞态：Stop 与 tick 同至 —— stop-and-wait 后无并发写出，-race 干净。
func TestUpstreamHeartbeatRaceStopVsTick(t *testing.T) {
	for i := 0; i < 80; i++ {
		hb, _, w := newTestHeartbeat(30*time.Millisecond, 20*time.Millisecond)
		hb.Start()
		require.Eventually(t, hb.IsCommitted, 2*time.Second, 5*time.Millisecond)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			hb.Stop()
		}()
		wg.Wait() // Stop 返回即 goroutine 已退出

		afterStop := w.heartbeatFrameCount()
		// 让任何潜在竞态窗口过去，确认帧数在 Stop 后稳定
		time.Sleep(50 * time.Millisecond)
		require.Equal(t, afterStop, w.heartbeatFrameCount())
	}
}

// 三交叠竞态：客户端断开（ctx cancel）时 goroutine 退出，-race 干净。
func TestUpstreamHeartbeatClientDisconnectExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, w := newHeartbeatTestWriter()
	now := time.Now()
	hb := NewUpstreamHeartbeat(ctx, w, hbSnapshot(1), now)
	hb.initialDelay = 30 * time.Millisecond
	hb.keepAliveInterval = 20 * time.Millisecond
	require.NoError(t, hb.Start())
	require.Eventually(t, hb.IsCommitted, 2*time.Second, 5*time.Millisecond)

	framesAtCancel := w.heartbeatFrameCount()
	cancel() // 客户端断开

	select {
	case <-hb.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat goroutine did not exit on ctx cancel")
	}
	// 断开后不再有新帧
	time.Sleep(60 * time.Millisecond)
	require.Equal(t, framesAtCancel, w.heartbeatFrameCount())
}
