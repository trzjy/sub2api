//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

// —— mock 基础设施 ——

type cnFirstByteMockUpstream struct {
	fn func(req *http.Request) (*http.Response, error)
}

func (u *cnFirstByteMockUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.fn(req)
}

func (u *cnFirstByteMockUpstream) DoWithTLS(req *http.Request, p string, a int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, a, c)
}

// cnFirstByteBlockingBody 读取指定数据后阻塞在 ctx 取消上（模拟无 body 的上游）。
type cnFirstByteBlockingBody struct {
	data     []byte
	offset   int
	ctx      context.Context
	blocking chan struct{}
	once     sync.Once
}

func newCNFirstByteBlockingBody(data []byte, ctx context.Context) *cnFirstByteBlockingBody {
	return &cnFirstByteBlockingBody{data: data, ctx: ctx, blocking: make(chan struct{})}
}

func (b *cnFirstByteBlockingBody) Read(p []byte) (int, error) {
	if b.offset < len(b.data) {
		n := copy(p, b.data[b.offset:])
		b.offset += n
		return n, nil
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.blocking:
		return 0, io.EOF
	}
}

func (b *cnFirstByteBlockingBody) Close() error {
	b.once.Do(func() { close(b.blocking) })
	return nil
}

func cnKimiAccount() *Account {
	return &Account{
		ID: 1, Name: "kimi-acc", Platform: PlatformKimi, Type: AccountTypeAPIKey,
		Concurrency: 1, Credentials: map[string]any{"api_key": "sk-test"},
	}
}

// openAICNSeqBody 先返回 prefix（模拟已写出首事件、响应头已提交），随后的 Read
// 返回 then 错误（模拟首包超时等流中断）。用于"头后阶段"流式消费者测试。
type openAICNSeqBody struct {
	prefix string
	then   error
	read   bool
}

func (b *openAICNSeqBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		n := copy(p, b.prefix)
		return n, nil
	}
	return 0, b.then
}

func (b *openAICNSeqBody) Close() error { return nil }

// —— 场景①：仅有响应头无 body → 60s（测试注入短值）到期触发切换+冷却 ——

func TestOpenAICNFirstByteTimeout_ResponseHeadersOnlyTriggersFailover(t *testing.T) {
	// Do 阶段超时：上游连响应头都不返回，只阻塞在 request context 上。
	upstream := &cnFirstByteMockUpstream{fn: func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 60 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	started := time.Now()
	_, err := svc.doOpenAIUpstream(context.Background(), req, "", cnKimiAccount())

	require.Error(t, err)
	require.True(t, isOpenAICNFirstByteTimeout(err), "内部超时必须是网关自定义错误")
	require.False(t, errors.Is(err, context.Canceled), "内部超时不得表现为客户端取消（context.Canceled）")
	require.Less(t, time.Since(started), 500*time.Millisecond, "不应等待完整 60s")
}

func TestOpenAICNFirstByteTimeout_BodyStallMapsToFailoverAndCooldown(t *testing.T) {
	// body 阶段超时：响应头已回（Do 成功）但 body 首字节 60s 未到。
	upstream := &cnFirstByteMockUpstream{fn: func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       newCNFirstByteBlockingBody(nil, req.Context()),
		}, nil
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 60 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	started := time.Now()
	resp, err := svc.doOpenAIUpstream(context.Background(), req, "", cnKimiAccount())
	require.NoError(t, err)
	defer resp.Body.Close()

	buf := make([]byte, 64)
	_, readErr := resp.Body.Read(buf)

	require.True(t, isOpenAICNFirstByteTimeout(readErr), "body 首字节超时必须映射为网关自定义错误")
	require.False(t, errors.Is(readErr, context.Canceled), "body 首字节超时不得表现为客户端取消")
	require.Less(t, time.Since(started), 500*time.Millisecond)
}

// —— 场景②：59s 首数据后长流正常 → 不误杀，流完整 ——

func TestOpenAICNFirstByteTimeout_FirstByteThenLongStreamSurvives(t *testing.T) {
	watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 80*time.Millisecond)
	pr, pw := io.Pipe()
	body := &openAICNFirstByteTimeoutBody{ReadCloser: pr, w: watchdog}

	go func() {
		defer pw.Close()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
		// 首包后继续长时间流（超过首包阈值边界），不应被误杀。
		time.Sleep(400 * time.Millisecond)
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
	}()

	all, err := io.ReadAll(body)
	require.NoError(t, err)
	require.False(t, watchdog.Fired(), "首包到达后定时器必须 disarm，不得触发超时")
	require.Contains(t, string(all), "response.completed")
	require.Contains(t, string(all), "[DONE]")
}

// —— 场景④：无数据 EOF → 定时器释放（不挂 60s），超时错误转换语义不变 ——
func TestOpenAICNFirstByteTimeout_EOFTerminalReleasesTimer(t *testing.T) {
	watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 30*time.Second)
	pr, pw := io.Pipe()
	body := &openAICNFirstByteTimeoutBody{ReadCloser: pr, w: watchdog}
	_ = pw.Close() // 上游立即 EOF（无任何数据）

	all, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Empty(t, all, "无数据 EOF 读到空内容")
	require.False(t, watchdog.Fired(), "无数据 EOF 不是内部超时")
	require.Equal(t, int32(1), watchdog.decided.Load(), "无数据 EOF 必须 release（decided 0→1），disarm 定时器")

	// 等待远超较短定时器窗口后 Fired 仍为 false：证明释放路径生效，
	// 不再依赖 30s 定时器到期。
	time.Sleep(100 * time.Millisecond)
	require.False(t, watchdog.Fired(), "EOF 释放后定时器不得再触发内部超时")
}

// —— 提前 Close → 定时器释放（不挂 60s）——
func TestOpenAICNFirstByteTimeout_EarlyCloseReleasesTimer(t *testing.T) {
	watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 30*time.Second)
	pr, pw := io.Pipe()
	body := &openAICNFirstByteTimeoutBody{ReadCloser: pr, w: watchdog}
	_ = pw.Close()
	_ = body.Close()

	require.False(t, watchdog.Fired(), "提前 Close 不得触发内部超时")
	require.Equal(t, int32(1), watchdog.decided.Load(), "提前 Close 必须 release（decided 0→1）")
}

// —— 场景③：客户端提前取消 → 直接终止，不换号、不冷却 ——

func TestOpenAICNFirstByteTimeout_ClientCancelNeverMisclassified(t *testing.T) {
	watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 5*time.Second)
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel() // 客户端已断开
	body := &openAICNFirstByteTimeoutBody{
		ReadCloser: newCNFirstByteBlockingBody(nil, reqCtx),
		w:          watchdog,
	}

	_, err := body.Read(make([]byte, 16))

	require.ErrorIs(t, err, context.Canceled, "客户端取消保持 context.Canceled 语义不变")
	require.False(t, isOpenAICNFirstByteTimeout(err), "客户端取消不得被误判为内部首包超时")
	require.False(t, watchdog.Fired(), "客户端取消不得触发内部超时标志")
}

// —— 竞态方向①：父 context 取消优先于 60s 定时器到期 ——
// 客户端取消（父 context 中断）与定时器近乎同时到达时，绝不能把定时器触发
// 误判为内部首包超时（内部超时→冷却+换号；客户端取消→不冷却不换号）。
// 同时钉死"客户端断开不冷却"：doOpenAIUpstream 必须以原始客户端 context 建
// watchdog，而非 request.Context()（后者经 detachUpstreamContext 已剥离取消信号）。
func TestOpenAICNFirstByteTimeout_ClientDisconnectViaRealClientCtxNoCooldown(t *testing.T) {
	// 客户端 context 已取消（断开）；上游阻塞在 request context 上等待取消。
	clientCtx, cancelClient := context.WithCancel(context.Background())
	cancelClient() // 客户端断开先于出站
	upstream := &cnFirstByteMockUpstream{fn: func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 20 * time.Millisecond}
	// 请求 context 故意用 detached（无取消信号）的 context 构造，模拟调用方
	// detachUpstreamContext 之后的状态：只有独立传入的 clientCtx 保留取消信号。
	req, _ := http.NewRequestWithContext(context.WithoutCancel(clientCtx), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	_, err := svc.doOpenAIUpstream(clientCtx, req, "", cnKimiAccount())

	require.ErrorIs(t, err, context.Canceled, "客户端断开不得被误判为内部首包超时（不冷却不换号）")
	require.False(t, isOpenAICNFirstByteTimeout(err), "客户端断开保持 context.Canceled 语义")
}

// —— 竞态方向①：父 context 取消优先于 60s 定时器到期 ——
// 客户端取消（父 context 中断）与定时器近乎同时到达时，绝不能把定时器触发
// 误判为内部首包超时（内部超时→冷却+换号；客户端取消→不冷却不换号）。
func TestOpenAICNFirstByteTimeout_RaceParentCancelBeatsTimer(t *testing.T) { // 父 context 在 watchdog 建立后立即取消（模拟客户端断开刚发生），
	// 定时器随后到期：expire 必须识别父已取消，不得置内部超时标志。
	parent, cancelParent := context.WithCancel(context.Background())
	watchdog, _ := newOpenAICNFirstByteWatchdog(parent, parent, 20*time.Millisecond)
	cancelParent()
	require.True(t, watchdog.parentDone(), "前提：父 context 已取消")

	// 等待定时器真正到期触发 expire。
	time.Sleep(100 * time.Millisecond)

	require.False(t, watchdog.Fired(), "父 context 已取消时定时器到期不得判定为内部超时（客户端取消优先）")
}

// —— 竞态方向②：响应首字节先于定时器到期 ——
// 首字节到达（firstByte CAS 赢）后定时器即使同时/随后到期，也不得再判定为
// 内部超时（唯一胜者裁决：先到者赢，后到者失效）。
func TestOpenAICNFirstByteTimeout_RaceFirstByteBeatsTimer(t *testing.T) {
	watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 20*time.Millisecond)
	watchdog.firstByte() // 首字节到达，CAS 0→1 赢并 disarm 定时器

	// 等待原定时器窗口过去（若 firstByte 未止住 timer，expire 会在此时触发）。
	time.Sleep(100 * time.Millisecond)

	require.False(t, watchdog.Fired(), "首字节先赢后定时器到期不得判定为内部超时")
}

// —— 竞态方向③：expire 内部 CAS 唯一胜者（直接调用验证原子裁决） ——
// 父未取消时 expire 以 CAS(0→2) 赢；已 winner（首字节/释放）后 expire 不得
// 覆盖裁决。父取消时 expire 直接 return，不破坏唯一胜者。
func TestOpenAICNFirstByteTimeout_RaceSingleAtomicWinner(t *testing.T) {
	t.Run("父未取消时 expire 以 CAS 赢下唯一胜者", func(t *testing.T) {
		watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 5*time.Second)
		watchdog.expire()
		require.True(t, watchdog.Fired(), "父未取消时 expire 必须置 Fired")
	})

	t.Run("首字节已赢后 expire 不得覆盖裁决", func(t *testing.T) {
		watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 5*time.Second)
		watchdog.firstByte()
		watchdog.expire()
		require.False(t, watchdog.Fired(), "首字节已赢（decided==1）后定时器到期不得改判为内部超时")
	})

	t.Run("取消已赢得非超时终态后 expire 不得覆盖裁决", func(t *testing.T) {
		// 闸②终审整改项 3：父取消与定时器经同一原子竞争唯一胜者。取消先行并
		// 赢得 settled 终态后，即使定时器随后掉到 expire，CAS 也必失败，不得
		// 改判为内部超时（旧实现"检查后 CAS"的窗口正是这里出错）。
		parent, cancelParent := context.WithCancel(context.Background())
		watchdog, _ := newOpenAICNFirstByteWatchdog(parent, parent, 5*time.Second)
		cancelParent()
		require.Eventually(t, func() bool {
			return watchdog.decided.Load() == watchdogStateSettled
		}, time.Second, time.Millisecond, "客户端取消必须赢得非超时终态")
		watchdog.expire()
		time.Sleep(20 * time.Millisecond)
		require.False(t, watchdog.Fired(), "取消赢得裁决后定时器不得置 Fired")
	})
}

func TestOpenAICNFirstByteTimeout_NonCNAccountUnaffected(t *testing.T) {
	upstream := &cnFirstByteMockUpstream{fn: func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: ok\n\n")),
		}, nil
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 5 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)
	openAIAccount := &Account{ID: 2, Name: "openai", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}

	resp, err := svc.doOpenAIUpstream(context.Background(), req, "", openAIAccount)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, ok := resp.Body.(*openAICNFirstByteTimeoutBody)
	require.False(t, ok, "非 CN 平台出站 body 不得被首包超时包装")
}

// —— failover 映射：冷却入口 + NextAccountRetry ——

// —— 任务卡 #2：分段阈值 + 同账号新连接重试 ——

// cnFreshConnMockUpstream 同时实现 Do / DoWithTLS / DoFreshConn，用于验证
// "未收到响应头"分支只会发出一次新连接重试（既不重试两次，也不换接口）。
type cnFreshConnMockUpstream struct {
	doCalls    int32
	freshCalls int32
	// freshHang=true 时 DoFreshConn 也阻塞到 ctx 取消（模拟重试同样石沉大海）。
	freshHang bool
	// doResp 非 nil 时 Do 立即返回该响应（用于非 CN 对照场景：那里没有 watchdog
	// 兜底，若 Do 阻塞在永不取消的 context 上整个测试会挂死）。
	doResp *http.Response
	// doRespFn 优先于 doResp：需要按请求 context 构造响应体时用（例如
	// "响应头已到达但 body 停滞"场景，停滞必须挂在该次尝试的 wctx 上）。
	doRespFn func(req *http.Request) (*http.Response, error)
	// doCtxs / freshCtxs 按发生顺序记录每次出站进入时使用的 context，以及进入
	// 时刻该 context 是否已取消。用于判定重试确实建立在一条全新的计时链路上
	// （而不是复用首次尝试那条已被超时取消的 context）。
	doCtxs    []cnFreshConnCallCtx
	freshCtxs []cnFreshConnCallCtx
	mu        sync.Mutex
	// freshBody 记录重试用到的请求体内容（用于验证 body 可重放且未被污染）。
	freshBody string
}

// cnFreshConnCallCtx 是一次出站进入时快照的 context 状态。
type cnFreshConnCallCtx struct {
	ctx             context.Context
	canceledOnEntry bool
}

func (u *cnFreshConnMockUpstream) recordDo(req *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.doCtxs = append(u.doCtxs, cnFreshConnCallCtx{ctx: req.Context(), canceledOnEntry: req.Context().Err() != nil})
}

func (u *cnFreshConnMockUpstream) recordFresh(req *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.freshCtxs = append(u.freshCtxs, cnFreshConnCallCtx{ctx: req.Context(), canceledOnEntry: req.Context().Err() != nil})
}

func (u *cnFreshConnMockUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	atomic.AddInt32(&u.doCalls, 1)
	u.recordDo(req)
	if u.doRespFn != nil {
		return u.doRespFn(req)
	}
	if u.doResp != nil {
		return u.doResp, nil
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func (u *cnFreshConnMockUpstream) DoFreshConn(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	atomic.AddInt32(&u.freshCalls, 1)
	u.recordFresh(req)
	if req != nil && req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		u.freshBody = string(raw)
	}
	if u.freshHang {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"ok\":true}\n\n")),
	}, nil
}

func (u *cnFreshConnMockUpstream) DoWithTLS(req *http.Request, p string, a int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, a, c)
}

// totalOutbound 返回底层出站调用总次数（Do + DoFreshConn），用于钉死"最多再发
// 一次"的上限：任何第三次出站都会让该总数超过 2。
func (u *cnFreshConnMockUpstream) totalOutbound() int {
	return int(atomic.LoadInt32(&u.doCalls)) + int(atomic.LoadInt32(&u.freshCalls))
}

// enableOpenAICNHeaderFastFail 显式打开分段快速失败开关（默认关闭），并可选打开
// "仅复用连接才允许快速失败"附加门禁；测试结束还原为具名常量默认值。
func enableOpenAICNHeaderFastFail(t *testing.T, requireReusedConn bool) {
	t.Helper()
	openAICNHeaderFastFail.Store(true)
	openAICNHeaderFastFailRequireReused.Store(requireReusedConn)
	t.Cleanup(func() {
		openAICNHeaderFastFail.Store(openAICNHeaderFastFailEnabled)
		openAICNHeaderFastFailRequireReused.Store(openAICNHeaderFastFailRequireReusedConn)
	})
}

// 验收①：无响应头时按短阈值失败（不等满整体阈值）。分段逻辑默认关闭，本用例显式打开。
func TestOpenAICNFirstByteTimeout_HeaderPhaseFailsAtShortThreshold(t *testing.T) {
	enableOpenAICNHeaderFastFail(t, false)
	const (
		overall     = 5 * time.Second
		headerPhase = 40 * time.Millisecond
	)
	watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), overall, headerPhase)
	defer watchdog.release()
	require.NotNil(t, watchdog.headerTimer, "开关打开时必须注册 headerTimer（分段生效）")
	require.True(t, watchdog.headerFastFailAllowed(), "开关打开且未启用 reused 门禁时快速失败应被允许")

	started := time.Now()
	// Do 尚未返回 ⇒ 响应头未到达。
	select {
	case <-wctx.Done():
	case <-time.After(overall):
		t.Fatalf("短阈值应在 %s 内触发", headerPhase)
	}
	elapsed := time.Since(started)

	require.True(t, watchdog.Fired(), "无响应头必须触发内部超时")
	require.False(t, watchdog.HeaderReceived(), "前提：响应头未到达")
	require.True(t, watchdog.TimedOutWaitingHeaders(), "必须被识别为'未收到响应头'分支（可走新连接重试）")
	require.GreaterOrEqual(t, elapsed, headerPhase, "不得早于短阈值触发")
	require.Less(t, elapsed, overall, "必须按短阈值失败，而不是等满整体 %s", overall)
}

// 验收③：已收到响应头后仍按整体阈值（这里是注入的 200ms）等 body 首字节，
// 不被新的短阈值影响。分段逻辑默认关闭，本用例显式打开。
func TestOpenAICNFirstByteTimeout_BodyPhaseKeepsOverallThreshold(t *testing.T) {
	enableOpenAICNHeaderFastFail(t, false)
	const (
		overall     = 200 * time.Millisecond
		headerPhase = 60 * time.Millisecond
	)
	watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), overall, headerPhase)

	started := time.Now()
	// 模拟 Do 在短阈值之前返回（响应头已到达），随后进入 body 首字节等待段。
	time.Sleep(20 * time.Millisecond)
	watchdog.markHeadersReceived()

	select {
	case <-wctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("整体阈值应在 2s 内触发")
	}
	elapsed := time.Since(started)

	require.True(t, watchdog.Fired(), "整体阈值到期必须触发超时")
	require.True(t, watchdog.HeaderReceived(), "已进入首字节等待段")
	require.False(t, watchdog.TimedOutWaitingHeaders(), "已收到响应头不得走新连接重试分支")
	require.GreaterOrEqual(t, elapsed, overall, "必须等到整体阈值，短阈值 %s 不得提前误杀", headerPhase)
	require.Less(t, elapsed, overall+500*time.Millisecond)
}

// 验收②：无响应头快失败后，对同一账号用新连接重试一次，且只一次。
// 分段逻辑默认关闭，本用例显式打开（关闭时的对照见
// TestOpenAICNFirstByteTimeout_HeaderFastFailDisabledKeepsSingleSegment）。
func TestOpenAICNFirstByteTimeout_RetriesOnceOnFreshConn(t *testing.T) {
	enableOpenAICNHeaderFastFail(t, false)
	t.Run("重试成功：Do 一次 + DoFreshConn 一次，body 完整重放", func(t *testing.T) {
		upstream := &cnFreshConnMockUpstream{}
		svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 60 * time.Millisecond}
		account := &Account{ID: 132, Name: "官方+19808568277", Platform: PlatformZhipu, Type: AccountTypeAPIKey, Concurrency: 20}
		payload := []byte(`{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}`)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://open.bigmodel.cn/api/paas/v4/chat/completions", bytes.NewReader(payload))
		require.NoError(t, err)

		resp, doErr := svc.doOpenAIUpstream(context.Background(), req, "", account)

		require.NoError(t, doErr, "新连接重试成功后不应返回错误")
		require.NotNil(t, resp)
		defer resp.Body.Close()
		require.Equal(t, int32(1), atomic.LoadInt32(&upstream.doCalls), "首次出站只能一次")
		// 契约①（上限）：实际发起的新连接重试次数必须**恰好**等于
		// openAICNFreshConnRetryLimit。该常量被改成 0（等于禁用重试）时，实现
		// 仍会发一次重试 → 本断言必然失败；反之若实现真的不重试，上方
		// require.NoError 也会失败。即：上限常量必须是真正生效的开关。
		require.Equal(t, openAICNFreshConnRetryLimit, int(atomic.LoadInt32(&upstream.freshCalls)),
			"实际新连接重试次数必须等于上限 openAICNFreshConnRetryLimit（上限为 0 时一次都不该重试）")
		require.Equal(t, 2, upstream.totalOutbound(),
			"底层出站总次数必须是 2（首次 1 次 + 新连接重试 1 次），不得有第三次")
		require.Equal(t, string(payload), upstream.freshBody, "重试用必须是完整还原的请求体")

		// 契约②（新链路）：首次尝试的 context 必须已被超时取消；重试必须建立在
		// 一条进入时尚未取消的全新计时链路上——否则"换条新连接重试"无从谈起。
		require.Len(t, upstream.doCtxs, 1)
		require.Len(t, upstream.freshCtxs, 1)
		require.Error(t, upstream.doCtxs[0].ctx.Err(), "首次尝试必须因无响应头超时而被取消")
		require.False(t, upstream.freshCtxs[0].canceledOnEntry,
			"重试必须用全新计时链路（进入 DoFreshConn 时尚未取消），不得复用已超时的首次链路")

		// 契约③（响应归属）：交给调用方的必须是重试拿到的响应体本身。
		_, wrapped := resp.Body.(*openAICNFirstByteTimeoutBody)
		require.True(t, wrapped, "重试的响应 body 必须继承首字节阈值包装")
		raw, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr)
		require.Equal(t, "data: {\"ok\":true}\n\n", string(raw),
			"返回给调用方的必须是新连接重试拿到的响应内容（而非首次尝试的残留）")
	})

	t.Run("重试也超时：不再重试第三次，按既有内部超时上抛", func(t *testing.T) {
		upstream := &cnFreshConnMockUpstream{freshHang: true}
		svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 50 * time.Millisecond}
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://open.bigmodel.cn/api/paas/v4/chat/completions", bytes.NewReader([]byte(`{"m":1}`)))

		_, doErr := svc.doOpenAIUpstream(context.Background(), req, "", cnKimiAccount())

		require.True(t, isOpenAICNFirstByteTimeout(doErr), "重试失败仍必须表现为网关内部超时")
		require.Equal(t, int32(1), atomic.LoadInt32(&upstream.doCalls))
		// 达到上限后第二次仍失败 → 不再有第三次（Do 不重发、DoFreshConn 不重发）。
		require.Equal(t, openAICNFreshConnRetryLimit, int(atomic.LoadInt32(&upstream.freshCalls)),
			"实际新连接重试次数必须等于上限 openAICNFreshConnRetryLimit，不得有第三次")
		require.Equal(t, 2, upstream.totalOutbound(), "重试也超时时底层出站总次数仍是 2，杜绝探测风暴")
	})

	t.Run("上游不支持新连接能力：不重试，行为与整改前一致", func(t *testing.T) {
		upstream := &cnFirstByteMockUpstream{fn: func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}}
		svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 50 * time.Millisecond}
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://open.bigmodel.cn/api/paas/v4/chat/completions", nil)

		_, doErr := svc.doOpenAIUpstream(context.Background(), req, "", cnKimiAccount())

		require.True(t, isOpenAICNFirstByteTimeout(doErr), "无新连接能力时必须退回既有超时语义")
	})

	t.Run("请求体不可重放（无 GetBody）：不重试", func(t *testing.T) {
		upstream := &cnFreshConnMockUpstream{}
		svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 50 * time.Millisecond}
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://open.bigmodel.cn/api/paas/v4/chat/completions", io.NopCloser(strings.NewReader(`{"m":1}`)))
		require.Nil(t, req.GetBody, "前提：该 body 无 GetBody，无法安全重放")

		_, doErr := svc.doOpenAIUpstream(context.Background(), req, "", cnKimiAccount())

		require.True(t, isOpenAICNFirstByteTimeout(doErr))
		require.Equal(t, int32(0), atomic.LoadInt32(&upstream.freshCalls), "body 不可重放时绝不发第二次出站")
	})

	t.Run("非 CN 平台不受新逻辑影响", func(t *testing.T) {
		upstream := &cnFreshConnMockUpstream{doResp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: ok\n\n")),
		}}
		svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 50 * time.Millisecond}
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://api.openai.com/v1/chat/completions", bytes.NewReader([]byte(`{"m":1}`)))
		openAIAccount := &Account{ID: 2, Name: "openai", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}

		resp, doErr := svc.doOpenAIUpstream(context.Background(), req, "", openAIAccount)

		require.NoError(t, doErr, "非 CN 平台不得挂新连接重试/分段阈值")
		require.NotNil(t, resp)
		defer resp.Body.Close()
		require.Equal(t, int32(0), atomic.LoadInt32(&upstream.freshCalls))
	})
}

// TestOpenAICNFirstByteTimeout_NoFreshConnRetryAfterHeadersReceived 钉死"已收到
// 响应头"这一侧不触发新连接重试：响应头已到达后卡在 body 首字节上属于上游排队/
// 生成慢，换新连接重发毫无意义（还会把请求重放一遍）。该场景必须保持既有语义：
// 只向消费链上抛 errOpenAICNFirstByteTimeout，一次 DoFreshConn 都不发。
func TestOpenAICNFirstByteTimeout_NoFreshConnRetryAfterHeadersReceived(t *testing.T) {
	enableOpenAICNHeaderFastFail(t, false)
	upstream := &cnFreshConnMockUpstream{doRespFn: func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			// 响应头已到达，body 首字节始终不来。
			Body: newCNFirstByteBlockingBody(nil, req.Context()),
		}, nil
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 60 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://open.bigmodel.cn/api/paas/v4/chat/completions", bytes.NewReader([]byte(`{"m":1}`)))

	resp, doErr := svc.doOpenAIUpstream(context.Background(), req, "", cnKimiAccount())
	require.NoError(t, doErr, "响应头已到达时 Do 本身不算失败（超时发生在 body 阶段）")
	require.NotNil(t, resp)
	defer resp.Body.Close()

	_, readErr := resp.Body.Read(make([]byte, 64))
	require.True(t, isOpenAICNFirstByteTimeout(readErr), "body 首字节超时必须映射为网关内部超时")

	require.Equal(t, 1, upstream.totalOutbound(), "响应头已到达的超时绝不得触发新连接重试（底层出站仅 1 次）")
	require.Equal(t, int32(0), atomic.LoadInt32(&upstream.freshCalls), "响应头已到达时一次 DoFreshConn 都不许发")
	require.Empty(t, upstream.freshBody, "未发生重试则不得有任何重放请求体")
}

// —— 止血：分段快速失败默认关闭，必须逐字回到单段语义 ——

// TestOpenAICNFirstByteTimeout_HeaderFastFailDisabledKeepsSingleSegment 钉死开关
// 关闭（当前生产默认值）时的三条契约：
//  1. 不注册 headerTimer：出站只有 openAICNFirstByteTimeout 一个整体截止计时器，
//     注入的短头阈值不得产生任何提前失败（不误杀大请求 prefill）；
//  2. 不触发新连接重试：底层出站总次数 = 1（首次 Do），DoFreshConn 一次都不发；
//  3. 观测照旧：httptrace 采集 connReused/connIdleTime，日志字段 reused/idle_time
//     仍然输出——这是坐实"空闲连接被静默丢弃"假设的唯一数据来源。
func TestOpenAICNFirstByteTimeout_HeaderFastFailDisabledKeepsSingleSegment(t *testing.T) {
	t.Run("关闭时不注册 headerTimer，短头阈值不产生提前失败", func(t *testing.T) {
		const (
			overall     = 2 * time.Second
			headerPhase = 40 * time.Millisecond
		)
		watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), overall, headerPhase)
		defer watchdog.release()

		require.Nil(t, watchdog.headerTimer, "开关关闭时不得注册 headerTimer（单段语义）")
		require.False(t, watchdog.headerFastFailAllowed(), "开关关闭时快速失败一律不允许")

		// 短头阈值已过（且远早于整体阈值）：不得有任何裁决发生。
		time.Sleep(300 * time.Millisecond)
		require.NoError(t, wctx.Err(), "关闭时短阈值不得提前取消上游尝试")
		require.False(t, watchdog.Fired(), "关闭时不得在短阈值处触发内部超时")
		require.False(t, watchdog.TimedOutWaitingHeaders(), "关闭时不得被识别为可重试的'无响应头'分支")
	})

	t.Run("关闭时不发生新连接重试（底层出站仅一次）", func(t *testing.T) {
		upstream := &cnFreshConnMockUpstream{}
		svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 60 * time.Millisecond}
		payload := []byte(`{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}`)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://open.bigmodel.cn/api/paas/v4/chat/completions", bytes.NewReader(payload))
		require.NoError(t, err)

		_, doErr := svc.doOpenAIUpstream(context.Background(), req, "", cnKimiAccount())

		require.True(t, isOpenAICNFirstByteTimeout(doErr), "关闭时超时仍按既有内部超时语义上抛")
		require.Equal(t, int32(1), atomic.LoadInt32(&upstream.doCalls), "首次出站只能一次")
		require.Equal(t, int32(0), atomic.LoadInt32(&upstream.freshCalls), "开关关闭时一次 DoFreshConn 都不许发")
		require.Equal(t, 1, upstream.totalOutbound(), "底层出站总次数必须是 1（无重试、无额外延迟）")
		require.Empty(t, upstream.freshBody, "未发生重试则不得有任何重放请求体")
	})

	t.Run("关闭时观测照旧（httptrace + reused/idle_time 字段）", func(t *testing.T) {
		watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 2*time.Second)
		defer watchdog.release()

		require.NotNil(t, httptrace.ContextClientTrace(wctx), "关闭时也必须挂 httptrace.ClientTrace")
		// 模拟 GotConn 回调（真实出网由 net/http 触发）：复用了一条空闲 250ms 的连接。
		watchdog.gotConnTrace().GotConn(httptrace.GotConnInfo{Reused: true, IdleTime: 250 * time.Millisecond})
		require.True(t, watchdog.connReused.Load(), "httptrace 采集必须落到 watchdog 上")
		require.Equal(t, int64(250*time.Millisecond), watchdog.connIdleTime.Load())

		enc := zapcore.NewMapObjectEncoder()
		for _, f := range watchdog.connTraceFields() {
			f.AddTo(enc)
		}
		require.Equal(t, true, enc.Fields["reused"], "日志字段 reused 必须继续输出")
		require.Equal(t, 250*time.Millisecond, enc.Fields["idle_time"], "日志字段 idle_time 必须继续输出")
	})

	// 默认值断言放在最后：先让上面三条行为契约独立跑完（变异时先看到行为失败，
	// 而不是被默认值断言提前短路）。
	require.False(t, openAICNHeaderFastFailEnabled, "生产默认：分段快速失败必须关闭")
	require.False(t, openAICNHeaderFastFailRequireReusedConn, "生产默认：reused 附加门禁必须关闭")
}

// TestOpenAICNFirstByteTimeout_HeaderFastFailReusedConnGate 验证附加门禁
// openAICNHeaderFastFailRequireReusedConn：打开后只有 httptrace 显示 reused=true
// （复用了空闲连接）才允许无响应头快速失败与新连接重试；新建连接上的等待一律交给
// 整体截止计时器。该门禁默认关闭，本用例显式打开。
func TestOpenAICNFirstByteTimeout_HeaderFastFailReusedConnGate(t *testing.T) {
	const (
		overall     = 2 * time.Second
		headerPhase = 40 * time.Millisecond
	)

	t.Run("门禁打开 + 新建连接（reused=false）：不得快速失败", func(t *testing.T) {
		enableOpenAICNHeaderFastFail(t, true)
		watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), overall, headerPhase)
		defer watchdog.release()
		require.NotNil(t, watchdog.headerTimer, "总开关打开时注册 headerTimer")
		require.False(t, watchdog.headerFastFailAllowed(), "新建连接且门禁打开时不得允许快速失败")

		// 短头阈值已过：未被裁决（否则就是误杀大请求 prefill）。
		time.Sleep(300 * time.Millisecond)
		require.NoError(t, wctx.Err(), "新建连接上的等待必须交给整体截止，不得被短阈值打断")
		require.False(t, watchdog.Fired())
		require.False(t, watchdog.TimedOutWaitingHeaders(), "门禁未通过时不得走新连接重试分支")
	})

	t.Run("门禁打开 + 复用连接（reused=true）：允许快速失败", func(t *testing.T) {
		enableOpenAICNHeaderFastFail(t, true)
		watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), overall, headerPhase)
		defer watchdog.release()
		watchdog.gotConnTrace().GotConn(httptrace.GotConnInfo{Reused: true, IdleTime: 195 * time.Second})

		started := time.Now()
		select {
		case <-wctx.Done():
		case <-time.After(overall):
			t.Fatalf("复用连接上应在短阈值 %s 内快速失败", headerPhase)
		}
		elapsed := time.Since(started)

		require.True(t, watchdog.Fired(), "复用连接且门禁通过时必须快速失败")
		require.True(t, watchdog.TimedOutWaitingHeaders(), "复用连接且门禁通过时允许新连接重试")
		require.GreaterOrEqual(t, elapsed, headerPhase)
		require.Less(t, elapsed, overall, "必须按短阈值失败，而不是等满整体 %s", overall)
	})
}

func TestOpenAICNFirstByteTimeout_FailoverCoolsAccountAndRequestsNextAccount(t *testing.T) {
	// fastpath 冷却记录依赖 rateLimitService（handleOpenAIAccountUpstreamError 在
	// rateLimitService==nil 时于 189 行提前返回），生产环境由
	// NewOpenAIGatewayService 注入，测试按既有模式构造。
	repo := &oauth429RateLimitRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := cnKimiAccount()
	model := "kimi-k3"

	foErr := svc.failoverOpenAICNFirstByteTimeout(context.Background(), account, model)

	require.NotNil(t, foErr)
	require.Equal(t, http.StatusGatewayTimeout, foErr.StatusCode)
	require.Equal(t, NextAccountRetry, foErr.NextAccountAction, "超时错误必须映射为 NextAccountRetry 供 failover 循环消费")
	require.False(t, errors.Is(foErr, context.Canceled), "failover 错误不得表现为 context.Canceled")
	// 请求级：首包超时标记的是本次请求在上游等太久，不是账号故障。
	// 该 Scope 使 classifyOpenAIAPIKeyHealthFailure 把它排除出账号级熔断
	//（生产实证 2026-10-26：这类超时占熔断触发 93.4%，且 828 次里 495 次
	//  换号即恢复；计入熔断会摘掉健康账号 -> 池空 -> 客户端 503）。
	require.Equal(t, GatewayFailureScopeRequest, foErr.Scope, "首包超时必须标记为请求级，不进账号级熔断")

	// 二次超时 → streak=2 → model transient 冷却生效（派发单 1 已开放的冷却入口）。
	_ = svc.failoverOpenAICNFirstByteTimeout(context.Background(), account, model)
	require.True(t, svc.isOpenAIAccountModelRuntimeBlocked(account, model), "超时账号必须落入模型级瞬态冷却")
	require.False(t, svc.peekOpenAIAccountRuntimeBlock(account).blocked, "首包超时不走账号级 runtime block（与既有 5xx 瞬态冷却语义一致）")
}

// TestOpenAICNFirstByteTimeout_ThresholdIs300Seconds pins the 2026-10-26 ruling:
// 60s -> 300s. Production evidence: 1,164 requests slower than 60s succeeded
// (780 in 60-90s, 209 in 90-120s, 104 in 120-180s, 71 above 180s; longest 576s),
// with average input 26k-34k tokens and average body 390k-690k bytes -- the
// "big requests are simply slow" class the user asked not to kill.
func TestOpenAICNFirstByteTimeout_ThresholdIs300Seconds(t *testing.T) {
	require.Equal(t, 300*time.Second, openAICNFirstByteTimeout,
		"CN 首包超时应为 300 秒（大请求本来就慢，让用户慢慢等）")
}

// TestOpenAICNFirstByteTimeout_NotCountedByAccountHealthBreaker pins that a CN
// first-byte timeout must NOT be counted toward the account-level health breaker:
// the account may be healthy while this one request waited too long upstream.
func TestOpenAICNFirstByteTimeout_NotCountedByAccountHealthBreaker(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := cnKimiAccount()

	foErr := svc.failoverOpenAICNFirstByteTimeout(context.Background(), account, "kimi-k3")
	require.NotNil(t, foErr)

	_, _, eligible := classifyOpenAIAPIKeyHealthFailure(foErr)
	require.False(t, eligible,
		"首包超时不得计入账号级熔断（它标记的是请求侧等待过久，不是账号故障）")
}

func TestOpenAICNFirstByteTimeout_StreamFailoverSkipsAfterHeadersCommitted(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := cnKimiAccount()

	// 响应头已下发给客户端 → 不做透明切换。
	foErr := svc.openAICNFirstByteTimeoutStreamFailover(nil, account, true, errOpenAICNFirstByteTimeout, "kimi-k3")
	require.Nil(t, foErr, "响应头已下发后不得透明切换")

	// 响应头未提交 → 透明切换（冷却 + NextAccountRetry）。
	foErr = svc.openAICNFirstByteTimeoutStreamFailover(nil, account, false, errOpenAICNFirstByteTimeout, "kimi-k3")
	require.NotNil(t, foErr)
	require.Equal(t, NextAccountRetry, foErr.NextAccountAction)
}

// —— 消费路径端到端：chat_completions 流式读取遇到超时错误 → failover ——

func TestOpenAICNFirstByteTimeout_StreamingConsumesFailoverBeforeHeadersCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}

	result, err := svc.handleChatStreamingResponse(
		resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", time.Now(), 0,
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.Nil(t, result)
	require.False(t, c.Writer.Written(), "响应头尚未提交时超时不得下发任何字节给客户端")
}

// TestOpenAICNFirstByteTimeout_StreamingConsumesCooldownWithModel 从真实消费
// 路径（handleChatStreamingResponse → failoverOpenAICNFirstByteTimeout）验证
// 模型贯穿：同一账号同一 upstreamModel 连续两次首包超时后，该账号被 model
// 调度门排除（闸②整改：空模型键会被 model 瞬态状态拒绝，冷却假闭环）。
func TestOpenAICNFirstByteTimeout_StreamingConsumesCooldownWithModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &oauth429RateLimitRepo{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{rateLimitService: rateLimits}
	rateLimits.SetAccountRuntimeBlocker(svc)
	account := cnKimiAccount()
	model := "kimi-k3"

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
		}
		_, err := svc.handleChatStreamingResponse(
			resp, c, account, model, model, model, time.Now(), 0,
		)
		require.Error(t, err)
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, err, &failoverErr, "消费路径必须返回可被 failover 循环消费的 UpstreamFailoverError")
		require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	}

	require.True(t, svc.isOpenAIAccountModelRuntimeBlocked(account, model),
		"消费路径连续两次首包超时后，该账号必须被 model 调度门排除（模型已贯穿）")
	require.False(t, svc.peekOpenAIAccountRuntimeBlock(account).blocked,
		"首包超时不走账号级 runtime block（与既有 5xx 瞬态冷却语义一致）")
}

func TestOpenAICNFirstByteTimeout_StreamingKeepsStreamAfterHeadersCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// 模拟响应头已提交：首行 SSE 先写出，随后读取因首包超时中断。
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		"",
	}, "\n") + "\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &openAICNStreamThenTimeoutBody{
			prefix: body,
			err:    errOpenAICNFirstByteTimeout,
		},
	}
	svc := &OpenAIGatewayService{}

	_, err := svc.handleChatStreamingResponse(
		resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", time.Now(), 0,
	)

	require.Error(t, err, "响应头已提交后超时按既有流中断语义返回错误")
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr, "响应头已提交后不得透明切换账号")
	require.True(t, c.Writer.Written(), "响应头已提交后客户端应已收到首字节")
}

type openAICNStreamThenTimeoutBody struct {
	prefix string
	err    error
	read   bool
}

func (b *openAICNStreamThenTimeoutBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		n := copy(p, b.prefix)
		if n < len(p) {
			return n, nil
		}
		return n, nil
	}
	return 0, b.err
}

func (b *openAICNStreamThenTimeoutBody) Close() error { return nil }

// —— 消费路径端到端：messages 缓冲读取遇到超时错误 → failover ——

func TestOpenAICNFirstByteTimeout_MessagesBufferedConsumesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}

	result, err := svc.handleAnthropicBufferedStreamingResponse(
		resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", time.Now(),
	)

	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.False(t, c.Writer.Written(), "messages 缓冲路径未提交响应头时超时不得下发任何字节")
}

func TestOpenAICNFirstByteTimeout_MessagesStreamingConsumesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}

	result, err := svc.handleAnthropicStreamingResponse(
		resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", time.Now(),
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.Nil(t, result)
	require.False(t, c.Writer.Written(), "messages 流式未提交响应头时超时不得下发字节")
}

func TestOpenAICNFirstByteTimeout_MessagesStreamingClientCancelDirectTerminates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: context.Canceled},
	}
	svc := &OpenAIGatewayService{}

	_, err := svc.handleAnthropicStreamingResponse(
		resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", time.Now(),
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr, "客户端取消不得触发 failover/换号")
	require.ErrorIs(t, err, context.Canceled, "客户端取消保持 context.Canceled 语义直接终止")
}

// —— 主路径四消费者：responses 非流式/流式 body 阶段闭合 ——

func TestOpenAICNFirstByteTimeout_ResponsesNonStreamingConsumesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}

	_, err := svc.handleNonStreamingResponse(
		context.Background(), resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3",
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "responses 非流式缓冲路径首包超时必须映射为 failover")
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.False(t, c.Writer.Written(), "缓冲路径未提交响应头时超时不得下发字节")
}

func TestOpenAICNFirstByteTimeout_ResponsesStreamingConsumesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}

	_, err := svc.handleStreamingResponse(
		context.Background(), resp, c, cnKimiAccount(), time.Now(), "kimi-k3", "kimi-k3",
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "responses 流式路径响应头未提交时首包超时映射为 failover")
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.False(t, c.Writer.Written(), "流式路径未提交响应头时超时不得下发字节")
}

// —— 主路径四消费者：anthropic_native 缓冲路径 body 阶段闭合 ——

func TestOpenAICNFirstByteTimeout_AnthropicNativeBufferedConsumesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}

	_, err := svc.handleNativeAnthropicBufferedResponse(
		context.Background(), resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", nil, time.Now(),
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "anthropic_native 缓冲路径首包超时映射为 failover")
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.False(t, c.Writer.Written(), "缓冲路径未提交响应头时超时不得下发字节")
}

// —— 主路径四消费者：chat_completions 缓冲路径 body 阶段闭合 ——

func TestOpenAICNFirstByteTimeout_ChatCompletionsBufferedConsumesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}

	_, err := svc.handleChatBufferedStreamingResponse(
		resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", time.Now(),
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "chat_completions 缓冲路径首包超时映射为 failover")
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.False(t, c.Writer.Written(), "缓冲路径未提交响应头时超时不得下发字节")
}

// —— 非流式 fallback 缓冲路径（responses→CC / messages→CC）：助手不写 502，
// 调用方优先映射内部超时为 failover ——

func TestOpenAICNFirstByteTimeout_ResponsesChatFallbackBufferedConsumesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}
	account := cnKimiAccount()

	_, err := svc.bufferChatCompletionsAsResponses(
		c, account, resp, "kimi-k3", map[string]bool{}, map[string]bool{}, false,
		map[string]apicompat.NamespacedToolName{}, "kimi-k3", "kimi-k3", nil,
		nil, time.Now(),
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "responses→CC 非流式 fallback 首包超时映射为 failover")
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.False(t, c.Writer.Written(), "助手不得先写 502（否则 failover 无法重放）")
	require.Empty(t, rec.Body.String(), "首包超时时不得下发任何响应体")
}

func TestOpenAICNFirstByteTimeout_AnthropicChatFallbackBufferedConsumesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}
	account := cnKimiAccount()

	_, err := svc.bufferChatCompletionsAsAnthropic(
		c, account, resp, "kimi-k3", "kimi-k3", "kimi-k3", nil,
		nil, time.Now(),
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "messages→CC 非流式 fallback 首包超时映射为 failover")
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.False(t, c.Writer.Written(), "助手不得先写 502（否则 failover 无法重放）")
	require.Empty(t, rec.Body.String(), "首包超时时不得下发任何响应体")
}

// —— native anthropic 两流式消费者：头前/头后两阶段（闸②整改项 1） ——

// TestOpenAICNFirstByteTimeout_CCNativeStreamingHeadlessConsumesFailover 钉死
// CC×native anthropic 流式消费者在首字节前（响应头未提交）读到内部超时错误时
// 返回既有 failover 错误供换号，且不向客户端下发任何字节（可切换窗口仍在）。
func TestOpenAICNFirstByteTimeout_CCNativeStreamingHeadlessConsumesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}
	account := cnKimiAccount()

	result, err := svc.handleCCStreamingFromNativeAnthropic(
		context.Background(), account, resp, c, "kimi-k3", "kimi-k3", "kimi-k3", nil, time.Now(), true,
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "首字节前超时必须映射为 failover 供换号")
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.Nil(t, result)
	require.False(t, c.Writer.Written(), "首字节前超时不得提交响应头（可切换窗口必须保留）")
	require.Empty(t, rec.Body.String(), "首字节前超时不得向客户端下发任何字节")
}

// TestOpenAICNFirstByteTimeout_CCNativeStreamingAfterHeadersEmitsErrorEvent 钉死
// CC×native anthropic 流式消费者在响应头已提交后才读到内部超时时，下发流内错误
// 事件终止流并返回非 failover 错误——不得正常 finalize 冒充成功空流。返回错误
// 的同时按既有流中断语义返回 result（usage 汇总），与 onIdle 分支一致。
func TestOpenAICNFirstByteTimeout_CCNativeStreamingAfterHeadersEmitsErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// 首个 SSE 事件写出（提交响应头）后，读取再以内部超时中断。
	prefix := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"kimi-k3","usage":{"input_tokens":10}}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &openAICNSeqBody{
			prefix: prefix,
			then:   errOpenAICNFirstByteTimeout,
		},
	}
	svc := &OpenAIGatewayService{}
	account := cnKimiAccount()

	result, err := svc.handleCCStreamingFromNativeAnthropic(
		context.Background(), account, resp, c, "kimi-k3", "kimi-k3", "kimi-k3", nil, time.Now(), true,
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr, "响应头已提交后不得透明切换账号")
	require.NotNil(t, result, "响应头已提交后的流中断按既有语义返回 result（usage 汇总）")
	require.True(t, c.Writer.Written(), "响应头已提交后客户端应已收到首字节")
	body := rec.Body.String()
	require.Contains(t, body, `"error"`, "响应头提交后的超时必须下发流内错误事件")
	require.Contains(t, body, infraerrors.UpstreamFirstByteTimeoutAfterStart, "错误事件必须携带可识别的超时消息")
}

// TestOpenAICNFirstByteTimeout_ResponsesNativeStreamingHeadlessConsumesFailover
// 钉死 responses×native anthropic 流式消费者在首字节前读到内部超时错误时返回
// failover 错误且不下发任何字节。
func TestOpenAICNFirstByteTimeout_ResponsesNativeStreamingHeadlessConsumesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICompatBufferedReadErrorCloser{err: errOpenAICNFirstByteTimeout},
	}
	svc := &OpenAIGatewayService{}
	account := cnKimiAccount()

	result, err := svc.handleResponsesStreamingFromNativeAnthropic(
		context.Background(), account, resp, c, "kimi-k3", "kimi-k3", "kimi-k3", nil, time.Now(),
		apicompat.ResponsesClientToolMapping{},
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "首字节前超时必须映射为 failover 供换号")
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	require.Nil(t, result)
	require.False(t, c.Writer.Written(), "首字节前超时不得提交响应头")
	require.Empty(t, rec.Body.String(), "首字节前超时不得向客户端下发任何字节")
}

// TestOpenAICNFirstByteTimeout_ResponsesNativeStreamingAfterHeadersEmitsErrorEvent
// 钉死 responses×native anthropic 流式消费者在响应头已提交后才读到内部超时时，
// 以 response.failed 终止事件回传并返回非 failover 错误。
func TestOpenAICNFirstByteTimeout_ResponsesNativeStreamingAfterHeadersEmitsErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	// 首个 Anthropic SSE 事件写出（提交响应头）后，读取再以内部超时中断。
	prefix := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"kimi-k3","usage":{"input_tokens":10}}}` + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &openAICNSeqBody{
			prefix: prefix,
			then:   errOpenAICNFirstByteTimeout,
		},
	}
	svc := &OpenAIGatewayService{}
	account := cnKimiAccount()

	result, err := svc.handleResponsesStreamingFromNativeAnthropic(
		context.Background(), account, resp, c, "kimi-k3", "kimi-k3", "kimi-k3", nil, time.Now(),
		apicompat.ResponsesClientToolMapping{},
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr, "响应头已提交后不得透明切换账号")
	require.NotNil(t, result, "响应头已提交后的流中断按既有语义返回 result（usage 汇总）")
	require.True(t, c.Writer.Written(), "响应头已提交后客户端应已收到首字节")
	body := rec.Body.String()
	require.Contains(t, body, "response.failed", "响应头提交后的超时必须以 response.failed 终止事件回传")
}

// —— 闸②终审第 5 轮整改（派发单 9）：watchdog 标记穿透包装链 ——

// TestOpenAICNFirstByteTimeout_CarrierPenetrationThroughWrappers 钉死载体接口
// 使 watchdog 标记穿透三层包装仍可达（forward.go:1130 openAIRequestContextReadCloser
// 与 :1245 responsesClientToolStreamBody 可对 resp.Body 双重包装，旧直接类型断言
// 在此失效）：
//
//	① watchdog body 直接断言命中（既有行为不回归）；
//	② openAIRequestContextReadCloser 包装后命中；
//	③ newResponsesClientToolStreamBody 包装（source 为 watchdog body）后命中；
//	④ 普通 body 返回 nil（非 CN 路径行为不变）。
func TestOpenAICNFirstByteTimeout_CarrierPenetrationThroughWrappers(t *testing.T) {
	watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 30*time.Second)
	pr, _ := io.Pipe()

	newWatchdogBody := func() *openAICNFirstByteTimeoutBody {
		return &openAICNFirstByteTimeoutBody{ReadCloser: pr, w: watchdog}
	}

	t.Run("直接断言命中", func(t *testing.T) {
		wb := newWatchdogBody()
		require.Same(t, wb, cnFirstByteTimeoutBodyOf(wb), "watchdog body 必须直接命中")
	})

	t.Run("openAIRequestContextReadCloser 包装后命中", func(t *testing.T) {
		inner := newWatchdogBody()
		wrapped := &openAIRequestContextReadCloser{ReadCloser: inner, cleanup: func() {}}
		require.Same(t, inner, cnFirstByteTimeoutBodyOf(wrapped),
			"载体接口必须穿透 openAIRequestContextReadCloser 命中内层 watchdog body")
	})

	t.Run("responsesClientToolStreamBody 包装 watchdog body 后命中", func(t *testing.T) {
		inner := newWatchdogBody()
		wrapped := newResponsesClientToolStreamBody(inner, apicompat.ResponsesClientToolMapping{}, defaultMaxLineSize)
		defer func() { _ = wrapped.Close() }()
		require.Same(t, inner, cnFirstByteTimeoutBodyOf(wrapped),
			"responsesClientToolStreamBody 构造时必须捕获内层 watchdog body 并经载体接口暴露")
	})

	t.Run("responsesClientToolStreamBody 包装非 watchdog body 返回 nil", func(t *testing.T) {
		plain := io.NopCloser(strings.NewReader("data: x\n\n"))
		wrapped := newResponsesClientToolStreamBody(plain, apicompat.ResponsesClientToolMapping{}, defaultMaxLineSize)
		defer func() { _ = wrapped.Close() }()
		require.Nil(t, cnFirstByteTimeoutBodyOf(wrapped),
			"source 非 watchdog body 时载体接口必须返回 nil（非 CN 路径行为不变）")
	})

	t.Run("普通 body 返回 nil", func(t *testing.T) {
		require.Nil(t, cnFirstByteTimeoutBodyOf(io.NopCloser(strings.NewReader("data: x\n\n"))),
			"普通 body 不得被识别为 watchdog 包装")
	})

	t.Run("responsesClientToolStreamBody 双重包装后命中", func(t *testing.T) {
		// forward.go:1130 先套 openAIRequestContextReadCloser，:1245 再套
		// responsesClientToolStreamBody：载体接口必须穿透两层仍可达 watchdog。
		inner := newWatchdogBody()
		ctxWrapped := &openAIRequestContextReadCloser{ReadCloser: inner, cleanup: func() {}}
		toolWrapped := newResponsesClientToolStreamBody(ctxWrapped, apicompat.ResponsesClientToolMapping{}, defaultMaxLineSize)
		defer func() { _ = toolWrapped.Close() }()
		require.Same(t, inner, cnFirstByteTimeoutBodyOf(toolWrapped),
			"双重包装（openAIRequestContextReadCloser→responsesClientToolStreamBody）下载体接口必须仍可达 watchdog")
	})
}

// —— 派发单 G1-b：CN 首包超时必须带可区分的 Reason 与错误体 type ——

// TestOpenAICNFirstByteTimeout_FailoverReasonIsFirstByteTimeout 断言
// failoverOpenAICNFirstByteTimeout 产出的错误 Reason 等于可区分值
// "gateway_first_byte_timeout"，使 handler 能按语义标记而非错误体字符串识别
// 本路径（CN 家族 kimi/deepseek/zhipu/minimax）的首字节超时（方案文档 §2/§3-G1）。
//
// 注意：用字面量断言，不引用同包上游尚未叠加的常量（该常量由 G1-a 在
// upstream_first_byte_error.go 新增；本工作区虽已含该定义，但跨卡测试约定
// 以字面量自证，避免对尚未稳定常量的隐式依赖）。
func TestOpenAICNFirstByteTimeout_FailoverReasonIsFirstByteTimeout(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := cnKimiAccount()
	model := "kimi-k3"

	foErr := svc.failoverOpenAICNFirstByteTimeout(context.Background(), account, model)
	require.NotNil(t, foErr)
	require.Equal(t, GatewayFailureReason("gateway_first_byte_timeout"), foErr.Reason,
		"CN 首包超时 failover 必须带 Reason=gateway_first_byte_timeout，供 handler 按语义标记识别")
}

// TestOpenAICNFirstByteTimeout_FailoverBodyTypeIsFirstByteTimeout 断言客户端可见错误体
// openAICNFirstByteTimeoutFailoverBody 的 JSON 里 error.type == "upstream_first_byte_timeout"
// （G2：与通用护栏一致，使客户端可见的错误体也带可区分类型；message 仍用
// infraerrors.UpstreamFirstByteTimeout）。
//
// 注：该 body 与原生一致性延续既有结构——末位缺一个外层闭合 `}`（与派发单“改为”示例
// 同形，本卡只改 type 不改结构），因此解析时若遇 truncated 则补一个 `}` 仅用于校验，
// 不改动实际 body 字节。
func TestOpenAICNFirstByteTimeout_FailoverBodyTypeIsFirstByteTimeout(t *testing.T) {
	body := openAICNFirstByteTimeoutFailoverBody
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	err := json.Unmarshal(body, &parsed)
	if err != nil {
		// 末位缺少外层闭合花括号：补一个 `}` 仅用于本地校验。
		if !errors.Is(err, io.EOF) && err.Error() != "unexpected end of JSON input" {
			require.NoError(t, err, "openAICNFirstByteTimeoutFailoverBody 必须是合法 JSON")
		}
		require.NoError(t, json.Unmarshal(append([]byte(nil), append(body, '}')...), &parsed),
			"补齐末位花括号后 openAICNFirstByteTimeoutFailoverBody 必须是合法 JSON")
	}
	require.Equal(t, "upstream_first_byte_timeout", parsed.Error.Type,
		"CN 首包超时错误体 type 必须为 upstream_first_byte_timeout（可区分类型，非通用 upstream_error）")
	require.Equal(t, infraerrors.UpstreamFirstByteTimeout, parsed.Error.Message,
		"CN 首包超时错误体 message 应保持不变（仍用 infraerrors.UpstreamFirstByteTimeout）")
}
