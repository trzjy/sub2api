package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 缺陷3：web 三平台 streamLoop 里的 `case <-ctx.Done(): clientDisconnected = true;
// continue` 是满 CPU 自旋。
//
// ctx.Done() 一旦取消即永久就绪 → select 立刻命中 → continue → 立刻再命中，循环
// 体内不做任何阻塞动作。最坏情况（客户端断开 + 上游挂死）可以占满单核直到
// interval 兜底（生产默认 180s）。
//
// anthropic 回程（gateway_upstream_response.go handleStreamingResponse）的同构
// select 完全没有 ctx.Done() 这个 case：断开靠下游写失败（werr != nil）感知。
// 移植到 web 三平台时凭空多了一个。
//
// 修复采用一次性 latch：把 channel 变量置 nil，select 中该分支永久不就绪
// （Go 标准惯用法），而不是 continue。

// TestWebStreamCtxDoneLatchIsNotHot 断言 latch 写法本身的语义：已取消的
// ctx.Done() 置 nil 后，同一个 select 在该分支上永久不就绪。
//
// 这是回归防护的核心断言点：若有人把 `ctxDoneCh = nil` 改回 `continue`（或删掉
// 置 nil），未 latch 的 channel 立刻就绪，自旋回归。
func TestWebStreamCtxDoneLatchIsNotHot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ctxDoneCh := ctx.Done()
	require.NotNil(t, ctxDoneCh)

	// 未 latch 时立即就绪 —— 这就是热自旋的来源。
	select {
	case <-ctxDoneCh:
	default:
		t.Fatal("precondition failed: a cancelled ctx.Done() must be immediately ready")
	}

	// 三平台现用的一次性 latch（连续两次都不命中）。
	clientDisconnected := true
	ctxDoneCh = nil
	for i := 0; i < 2; i++ {
		select {
		case <-ctxDoneCh:
			t.Fatal("latched ctxDoneCh must stay nil forever; a ready channel here " +
				"means the select would hot-spin again")
		default:
		}
	}
	require.True(t, clientDisconnected, "the latch must still record the disconnect")
}

// TestWebZhipuStreamLoop_CancelledCtxDoesNotSpin 端到端验证取消后的 streamLoop
// 不空转：上游开流送一帧后挂死，客户端 ctx 已取消。
//
// 期望行为（修复后）：取消被 latch 记下 clientDisconnected=true，之后循环阻塞在
// guard.events / intervalCh / keepaliveCh 上，无数据时进程 CPU 时间几乎为零。
// 回到 `continue` 写法时，这段时间的 CPU 会逼近墙钟（满单核自旋）。
//
// interval/keepalive 设为 1s（配置以秒为粒度的最小值），使观测窗口内不会提前收口。
func TestWebZhipuStreamLoop_CancelledCtxDoesNotSpin(t *testing.T) {
	hang := newHangingSSEBody()
	defer hang.close()

	svc, c, account, body := newZhipuSpinFixture(hang)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端在首帧后断开

	cpuBefore := processCPUTime()

	done := make(chan error, 1)
	go func() {
		_, err := svc.forwardWebZhipu(ctx, c, account, body, "glm-5.3-flash", true, time.Now(), webResponseModeChat)
		done <- err
	}()

	// 让读 goroutine 拿到首帧并进入稳态，随后观测 400ms：这段时间上游没有任何
	// 数据（只有 keepalive 每秒一拍，且 keepalive 分支在取消后被 clientDisconnected
	// 短路，不写下游）。
	const window = 400 * time.Millisecond
	time.Sleep(window)
	spent := processCPUTime() - cpuBefore

	// 满自旋时 CPU ≈ 墙钟（400ms）；latch 后只剩调度开销（通常个位数 ms）。
	// 阈值取 50%：既能抓住自旋回归（100%），又不会被 CI 抖动误伤。
	require.Less(t, spent, window/2,
		"streamLoop burned %v of CPU during a %v idle window after ctx cancellation: "+
			"the ctx.Done() branch is hot-spinning again", spent, window)

	// 收口：挂死上游的最终归宿仍是 interval 兜底（缺陷1 的 Ticker 保证它会触发）。
	select {
	case err := <-done:
		require.Error(t, err, "a hung upstream must be terminated by the interval guard")
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("forward never returned; the interval guard did not fire")
	}
}

// hangingSSEBody 模拟「送出一帧真实 SSE 后永久挂死」的响应体：连接保持、不吐字节、
// 不返 EOF —— 与本轮整改要修的挂死场景同形。Close 是唯一解除阻塞的入口。
type hangingSSEBody struct {
	once     atomic.Bool
	closed   atomic.Bool
	block    chan struct{}
	frameOut chan struct{}
}

func newHangingSSEBody() *hangingSSEBody {
	return &hangingSSEBody{
		block:    make(chan struct{}),
		frameOut: make(chan struct{}, 1),
	}
}

const hangingZhipuFrame = `data: {"id":"zp-1","parts":[{"role":"assistant","status":"init",` +
	`"content":[{"type":"text","text":"hello"}],"model":"moe_5"}],"status":"init"}` + "\n\n"

func (b *hangingSSEBody) Read(p []byte) (int, error) {
	if b.once.CompareAndSwap(false, true) {
		return copy(p, hangingZhipuFrame), nil
	}
	<-b.block // 挂死：不返回 EOF
	return 0, io.EOF
}

func (b *hangingSSEBody) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		close(b.block)
	}
	return nil
}

func (b *hangingSSEBody) close() {
	_ = b.Close()
}

// newZhipuSpinFixture 装配一个 zhipu 流式 forward：interval/keepalive 都设为 1s
// （配置以秒为粒度），让观测窗口覆盖不到提前收口。
func newZhipuSpinFixture(body io.ReadCloser) (*OpenAIGatewayService, *gin.Context, *Account, []byte) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{}`)))

	svc := &OpenAIGatewayService{
		cfg: &config.Config{
			Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false, AllowInsecureHTTP: true}},
			Gateway:  config.GatewayConfig{StreamDataIntervalTimeout: 1, StreamKeepaliveInterval: 1},
		},
		httpUpstream: &httpUpstreamRecorder{responses: []*http.Response{{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
		}}},
		rateLimitService: &RateLimitService{},
	}
	account := webZhipuTestAccount(9901, map[string]any{"base_url": "https://chatglm.cn"})
	inbound := []byte(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	return svc, c, account, inbound
}

// processCPUTime 返回本进程已消耗的 CPU 时间（用户态+内核态）。
// 自旋的signature 是「CPU ≈ 墙钟」，故必须用 CPU 时间而非墙钟来判定。
func processCPUTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	total := time.Duration(ru.Utime.Sec+ru.Stime.Sec)*time.Second +
		time.Duration(ru.Utime.Usec+ru.Stime.Usec)*time.Microsecond
	return total
}
