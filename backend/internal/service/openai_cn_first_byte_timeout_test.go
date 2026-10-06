//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
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
func TestOpenAICNFirstByteTimeout_RaceParentCancelBeatsTimer(t *testing.T) {	// 父 context 在 watchdog 建立后立即取消（模拟客户端断开刚发生），
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

	// 二次超时 → streak=2 → model transient 冷却生效（派发单 1 已开放的冷却入口）。
	_ = svc.failoverOpenAICNFirstByteTimeout(context.Background(), account, model)
	require.True(t, svc.isOpenAIAccountModelRuntimeBlocked(account, model), "超时账号必须落入模型级瞬态冷却")
	require.False(t, svc.peekOpenAIAccountRuntimeBlock(account).blocked, "首包超时不走账号级 runtime block（与既有 5xx 瞬态冷却语义一致）")
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
	require.Contains(t, body, "upstream first byte timeout", "错误事件必须携带可识别的超时消息")
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
//  ① watchdog body 直接断言命中（既有行为不回归）；
//  ② openAIRequestContextReadCloser 包装后命中；
//  ③ newResponsesClientToolStreamBody 包装（source 为 watchdog body）后命中；
//  ④ 普通 body 返回 nil（非 CN 路径行为不变）。
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

