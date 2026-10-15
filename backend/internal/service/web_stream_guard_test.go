package service

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// webStreamGuard 的 interval 兜底测试。
//
// 缺陷1：intervalTimer 曾是 time.NewTimer（一次性）。它只 fire 一次，若那一次因
// 「距上次读不足 interval」被 continue 掉，intervalCh 就永久枯竭——上游随后挂死
// （连接保持、不吐字节、不返 EOF）时读 goroutine 永久阻塞，正是守卫要修的场景。
// 改为 Ticker 后每次 tick 都按 lastReadAt 重新判定，天然覆盖「第一次未 idle」
// 的情形，无需 Reset。

// blockingReaderAfter 是一个「立刻挂死」的 ReadCloser：连接保持、不吐字节、
// 不返 EOF（EOF 只在 Close 后解除阻塞时才有）——精确模拟本轮整改要修的挂死上游。
type blockingReaderAfter struct {
	block  chan struct{}
	closed atomic.Bool
}

func (r *blockingReaderAfter) Read(p []byte) (int, error) {
	<-r.block
	return 0, io.EOF
}

func (r *blockingReaderAfter) Close() error {
	if r.closed.CompareAndSwap(false, true) {
		close(r.block)
	}
	return nil
}

func newBlockingReadCloser(frames ...[]byte) *blockingReaderAfter {
	// frames 参数仅为兼容调用方语义；挂死体不消费它们，首帧由 readFn 直接产出。
	_ = frames
	return &blockingReaderAfter{block: make(chan struct{})}
}

// TestWebStreamGuard_IntervalStillFiresAfterNonIdleTick 是缺陷1 的核心回归测试：
// 首帧之后先制造一次「tick 到达但距上次读不足 interval」的判定（streamIdle 返回
// false，上层会 continue），随后让上游挂死 —— 守卫必须在**后续**的 tick 上
// 再次触发并判定 idle。一次性 Timer 在这个场景下会永久枯竭。
func TestWebStreamGuard_IntervalStillFiresAfterNonIdleTick(t *testing.T) {
	const interval = 200 * time.Millisecond

	// 门控首帧投递时刻：让它落在第一个 tick（t=interval）之前、且距 tick 明显小于
	// interval。这样第一拍必然判定「未 idle」，无需依赖 sleep 精度，也避开
	// 「tick 恰好落在 interval 边界」的竞态。
	releaseFirstFrame := make(chan struct{})
	body := newBlockingReadCloser()

	var delivered atomic.Bool
	guard := newWebStreamGuard(webStreamGuardParams{
		readFn: func() ([]byte, error) {
			if delivered.CompareAndSwap(false, true) {
				<-releaseFirstFrame // 等测试放行后才送出首帧
				return []byte("frame-1"), nil
			}
			// 上游挂死：连接保持、不吐字节、不返 EOF。
			b := make([]byte, 64)
			n, err := body.Read(b)
			if n == 0 && err != nil {
				return nil, err
			}
			return b[:n], nil
		},
		interval: interval,
	})
	defer guard.stop()
	t.Cleanup(func() { _ = body.Close() })

	// 在第一个 tick 之前、留足余量的时刻放行首帧。
	time.Sleep(interval / 4)
	close(releaseFirstFrame)

	// 等读 goroutine 真正处理该帧（lastReadAt 被更新）。
	require.Eventually(t, func() bool {
		return time.Since(time.Unix(0, guard.lastReadAt.Load())) < interval/2
	}, 2*time.Second, 5*time.Millisecond, "guard must record the first frame's read time")

	// 第一拍 tick 到达（距 lastReadAt 约 3/4 interval）→ 上层 continue。
	select {
	case <-guard.intervalCh:
		require.False(t, guard.streamIdle(interval),
			"first tick must find the stream NOT idle (a read just happened)")
	case <-time.After(5 * time.Second):
		t.Fatal("first interval tick never arrived")
	}

	// 上游此刻挂死（body 永久阻塞）。守卫必须在接下来的 tick 上判定 idle
	// —— 一次性 Timer 在这里会永久枯竭，因为它的唯一一次触发已经被 continue 掉。
	guard.lastReadAt.Store(time.Now().Add(-interval).UnixNano())
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-guard.intervalCh:
			if guard.streamIdle(interval) {
				return // 通过：非 idle 那一拍之后，后续拍依然触发且判定为 idle
			}
			// 仍不 idle 说明刚读完数据，继续等。
		case <-deadline:
			t.Fatal("interval ticker went permanently dead after a non-idle tick: " +
				"the guard no longer detects a hung upstream (one-shot Timer regression)")
		}
	}
}

// TestWebStreamGuard_ZeroLengthFramesDoNotMaskIdleUpstream 守住「零长帧不重置」
// 的设计意图：只发心跳的死上游必须被 interval 兜底。用 Ticker 后天然成立，无需
// 在零长帧上 Reset 计时器。
func TestWebStreamGuard_ZeroLengthFramesDoNotMaskIdleUpstream(t *testing.T) {
	const interval = 50 * time.Millisecond

	// 心跳流：先送一帧实质数据，之后无限送零长帧（永不 EOF）。
	heartbeats := make(chan struct{})
	var sentRealFrame atomic.Bool
	guard := newWebStreamGuard(webStreamGuardParams{
		readFn: func() ([]byte, error) {
			if sentRealFrame.CompareAndSwap(false, true) {
				return []byte("real"), nil
			}
			select {
			case <-heartbeats:
				return nil, io.EOF
			default:
			}
			time.Sleep(time.Millisecond)
			return nil, nil // 零长帧：不投递、不更新 lastReadAt
		},
		interval: interval,
	})
	defer guard.stop()
	t.Cleanup(func() { close(heartbeats) })

	// 取走那帧实质数据（模拟上层消费）。
	select {
	case ev := <-guard.events:
		require.NoError(t, ev.err)
		require.Equal(t, "real", string(ev.data))
	case <-time.After(5 * time.Second):
		t.Fatal("real frame never delivered")
	}

	// 心跳持续不断，但 interval 必须仍然兜底。
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-guard.intervalCh:
			if guard.streamIdle(interval) {
				return
			}
		case <-deadline:
			t.Fatal("heartbeat-only upstream was never caught by the interval guard")
		}
	}
}

// TestWebStreamGuard_StopsWithoutLeaking 守住 stop() 的幂等收口：Ticker 与 Timer
// 都要释放，且 stop 之后读 goroutine 不再投递（否则调用方提前返回时 goroutine
// 会向满 channel 阻塞泄漏）。
func TestWebStreamGuard_StopsWithoutLeaking(t *testing.T) {
	body := newBlockingReadCloser()
	var stopped atomic.Bool
	guard := newWebStreamGuard(webStreamGuardParams{
		readFn: func() ([]byte, error) {
			if stopped.Load() {
				return nil, io.EOF
			}
			b := make([]byte, 64)
			n, err := body.Read(b)
			if n == 0 && err != nil {
				return nil, err
			}
			return b[:n], nil
		},
		interval:  10 * time.Millisecond,
		keepalive: 10 * time.Millisecond,
	})
	require.NotNil(t, guard.intervalTicker, "guard must own an interval ticker")
	require.NotNil(t, guard.keepaliveTimer)

	stopped.Store(true)
	guard.stop()
	// stop 后必须能再次调用而不 panic（幂等性由 close(done) 保证，此处只需不挂死）。
	_ = body.Close()

	select {
	case _, ok := <-guard.events:
		require.False(t, ok, "events must be closed after the reader goroutine exits")
	case <-time.After(5 * time.Second):
		t.Fatal("guard.events was never closed after stop()")
	}
}

// --- 缺陷4：keepalive 字节不得击穿 CN 首包 failover ----------------------------

// TestWriteWebStreamKeepaliveRegistersNonSemanticBytes 证明保活帧的字节被登记进
// 既有的「非语义输出」扣减口径。
//
// 不登记的后果：handler 的 openAIForwardMayFailover 按「Forward 前后
// OpenAICompactKeepaliveAdjustedWrittenSize 是否变化」判定是否已写出语义内容。
// keepalive 虽只写 SSE 注释（`: \n\n`），但已 Flush 提交 HTTP 头、Size 变大，
// 于是判定为「已写出语义内容」→ SafeToFailoverAfterWrite 为 false 也救不回来
// → 拒绝换号 → 客户端收到 200 + 若干注释 + 流内 error，失去透明换号，
// 与 CN 首包分段快失败整改目标直接冲突。
func TestWriteWebStreamKeepaliveRegistersNonSemanticBytes(t *testing.T) {
	c, rec := newTestKeepaliveCtx(t)

	require.NoError(t, writeWebStreamKeepalive(c))

	rawWritten := rec.Body.Len()
	require.Positive(t, rawWritten, "keepalive must actually write its SSE comment bytes")

	adjusted := OpenAICompactKeepaliveAdjustedWrittenSize(c)
	require.Equal(t, -1, adjusted,
		"a request whose only output is keepalive bytes must adjust to the gin "+
			"'nothing written' sentinel (-1), otherwise the handler treats the "+
			"comment as semantic output and refuses to switch accounts")

	// 再写实质内容后，扣减后的字节数应只剩语义部分。
	_, err := c.Writer.WriteString("data: real\n\n")
	require.NoError(t, err)
	require.Equal(t, rawWritten+len("data: real\n\n"), rec.Body.Len())
	require.Equal(t, len("data: real\n\n"), OpenAICompactKeepaliveAdjustedWrittenSize(c),
		"keepalive bytes must be deducted from the semantic byte count")
}

// TestWebStreamKeepaliveRegisteredBeforeFailoverRejection 端到端复现缺陷4 的
// 失败场景：先发保活、再遇到 CN 首包超时要换号，扣减后的字节数必须与 Forward 前
// 的基线相等 —— 这正是 openAIForwardMayFailover 判定「可以换号」的条件
// （current == before）。
func TestWebStreamKeepaliveRegisteredBeforeFailoverRejection(t *testing.T) {
	c, _ := newTestKeepaliveCtx(t)

	// Forward 前的基线：尚未写出任何字节 → gin 的 -1 哨兵，归一后为 0。
	before := OpenAICompactKeepaliveAdjustedWrittenSize(c)
	require.Equal(t, -1, before)

	// 上游开了流但迟迟不发首帧 → 前端 keepalive 先发了两拍。
	require.NoError(t, writeWebStreamKeepalive(c))
	require.NoError(t, writeWebStreamKeepalive(c))

	// 此刻 CN 首包超时，forward 返回 failover 错误。handler 的判定条件：
	// current == before 即允许透明换号。
	current := OpenAICompactKeepaliveAdjustedWrittenSize(c)
	require.Equal(t, before, current,
		"keepalive bytes must not change the failover baseline: with them counted "+
			"as semantic output the handler refuses to switch accounts and the client "+
			"gets 200 + ': ' comments + an in-stream error instead of a transparent retry")
}

// newTestKeepaliveCtx 造一个带真实 gin ResponseWriter 的测试上下文（Size()/
// Written() 语义与生产一致，httptest.ResponseRecorder + gin writer 包装）。
func newTestKeepaliveCtx(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	return c, rec
}
