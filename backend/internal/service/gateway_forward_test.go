//go:build unit

package service

// 文件：internal/service/gateway_forward_test.go
//
// CF524 G2a 挂点 1（gateway_forward.go Forward 主路径，/v1/messages）单测。
// 本文件同时承载 G2a 三挂点共享的 mock 上游与注入 helper（ctx 内构造预算快照 +
// 心跳 owner，与 G2b 在 handler 的安装契约同型）。

import (
	"bytes"
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
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// g2aCall 是一次 mock 上游调用的脚本决策。
type g2aCall func(req *http.Request) (*http.Response, error)

// g2aMockUpstream 是按调用序号脚本化的 HTTPUpstream mock。未提供脚本的调用默认
// 阻塞直到请求 context 取消，再返回取消错误——精确模拟"上游收请求后不返回响应头"
// 且"护栏取消 reqCtx 中断 header 等待"的 CF524 受害形态。
type g2aMockUpstream struct {
	mu    sync.Mutex
	calls []*http.Request
	steps []g2aCall
}

func (u *g2aMockUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.DoWithTLS(req, "", 0, 0, nil)
}

func (u *g2aMockUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.mu.Lock()
	u.calls = append(u.calls, req)
	n := len(u.calls)
	var step g2aCall
	if n-1 < len(u.steps) {
		step = u.steps[n-1]
	}
	u.mu.Unlock()
	if step != nil {
		return step(req)
	}
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-time.After(5 * time.Second):
		return nil, errors.New("g2a mock: upstream hang was never canceled")
	}
}

func (u *g2aMockUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

// g2aHangFn 是悬挂脚本：直到护栏取消 reqCtx 才返回。
func g2aHangFn(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// g2aSuccessFn 返回一个固定 SSE body 的 200 响应。
func g2aSuccessFn(body string) g2aCall {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"rid-g2a"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

// g2aSlowBody 在 delay 后返回完整 data；若 ctx 先取消则返回 ctx.Err()——用于断言
// "响应头先到、body 传输超预算仍完整送达（护栏未取消 body 读取 context）"。
type g2aSlowBody struct {
	ctx   context.Context
	data  []byte
	delay time.Duration
	read  bool
}

func (b *g2aSlowBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		select {
		case <-time.After(b.delay):
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (b *g2aSlowBody) Close() error { return nil }

// g2aAnthropicSSE 是一段完整的 Anthropic 流式响应（含 message_stop 终态）。
const g2aAnthropicSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_g2a","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":5}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func g2aService(upstream HTTPUpstream) *GatewayService {
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	return &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     &RateLimitService{},
		deferredService:      &DeferredService{},
	}
}

// g2aSnapshot 直接构造不可变预算快照（GuardSeconds 秒级；AbsoluteDeadline 决定
// 剩余预算与 attempt 窗口 = min(guard, 剩余)）。
func g2aSnapshot(guardSec, hbDelaySec int, deadline time.Duration) *RequestBudgetSnapshot {
	now := time.Now()
	return &RequestBudgetSnapshot{
		GuardSeconds:          guardSec,
		HeartbeatDelaySeconds: hbDelaySec,
		EntryMonotonic:        now,
		AbsoluteDeadline:      now.Add(deadline),
	}
}

// g2aHeartbeat 构造请求级心跳 owner 并覆写 initialDelay/keepAliveInterval 以加速
// 时间轴断言（同包可访问组件内部字段；生产值由快照 delay 驱动）。
func g2aHeartbeat(base context.Context, w http.ResponseWriter, delay, interval time.Duration) *UpstreamHeartbeat {
	entry := time.Now()
	snap := &RequestBudgetSnapshot{HeartbeatDelaySeconds: 1, EntryMonotonic: entry}
	hb := NewUpstreamHeartbeat(base, w, snap, entry)
	hb.initialDelay = delay
	hb.keepAliveInterval = interval
	return hb
}

// g2aInject 把预算快照与心跳 owner 写入请求 context（模拟 G2b 入口安装点）。
func g2aInject(base context.Context, snap *RequestBudgetSnapshot, hb *UpstreamHeartbeat) context.Context {
	ctx := WithRequestBudgetSnapshot(base, snap)
	if hb != nil {
		ctx = WithUpstreamHeartbeat(ctx, hb)
	}
	return ctx
}

// g2aGinContext 构造带可观测 writer 的 gin context（writer 记录心跳帧与 Flush）。
func g2aGinContext(method, target string, body []byte) (*httptest.ResponseRecorder, *gin.Context, *heartbeatObsWriter) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	c.Request = httptest.NewRequest(method, target, rdr)
	obs := &heartbeatObsWriter{ResponseWriter: c.Writer}
	c.Writer = obs
	return rec, c, obs
}

func g2aParsed(t *testing.T, body []byte) *ParsedRequest {
	t.Helper()
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)
	return parsed
}

func g2aAnthropicOAuthAccount() *Account {
	return &Account{
		ID: 901, Name: "g2a-anthropic-oauth", Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Concurrency: 1, Credentials: map[string]any{"access_token": "g2a-oauth-token"},
		Status: StatusActive, Schedulable: true,
	}
}

func g2aAnthropicAPIKeyAccount(id int64) *Account {
	return &Account{
		ID: id, Name: "g2a-anthropic-apikey", Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Concurrency: 1, Credentials: map[string]any{"api_key": "g2a-upstream-key", "base_url": "https://api.anthropic.com"},
		Status: StatusActive, Schedulable: true,
	}
}

func g2aAPIKeyAccount(id int64, platform, name string) *Account {
	return &Account{
		ID: id, Name: name, Platform: platform, Type: AccountTypeAPIKey,
		Concurrency: 1, Credentials: map[string]any{"api_key": "g2a-upstream-key"},
		Status: StatusActive, Schedulable: true,
	}
}

func g2aStreamBody(model string) []byte {
	return []byte(`{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
}

func g2aNonStreamBody(model string) []byte {
	return []byte(`{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hello"}]}`)
}

// —— 挂点 1：claude-sonnet-5 OAuth 主路径（CF524 受害形态之一） ——

// 上游悬挂 > guard：心跳先起（客户端收到 SSE 200 + keep-alive 帧），guard 触发后
// stop-and-wait 停心跳再返回可换号错误（剩余预算 ≥5s）。
func TestG2AMount1ClaudeSonnet5OAuthHangHeartbeatThenGuardFailover(t *testing.T) {
	upstream := &g2aMockUpstream{} // 默认悬挂
	svc := g2aService(upstream)
	body := g2aStreamBody("claude-sonnet-5")
	parsed := g2aParsed(t, body)
	_, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)

	snap := g2aSnapshot(1, 1, 30*time.Second)
	hb := g2aHeartbeat(context.Background(), obs, 60*time.Millisecond, 40*time.Millisecond)
	ctx := g2aInject(context.Background(), snap, hb)

	start := time.Now()
	_, err := svc.Forward(ctx, c, g2aAnthropicOAuthAccount(), parsed)
	elapsed := time.Since(start)

	var fe *UpstreamFailoverError
	require.ErrorAs(t, err, &fe)
	require.Equal(t, http.StatusGatewayTimeout, fe.StatusCode)
	require.Contains(t, string(fe.ResponseBody), "upstream_first_byte_timeout")
	require.True(t, fe.ShouldRetryNextAccount(), "剩余预算 ≥5s 必须允许既有 FailoverContinue 换号")
	require.Equal(t, NextAccountRetry, fe.NextAccountAction)

	require.True(t, obs.HeaderWritten(), "guard 触发前心跳必须已提交 SSE 200")
	require.Greater(t, obs.heartbeatFrameCount(), 0, "guard 触发前客户端必须已收到 keep-alive 帧")
	require.True(t, hb.IsCommitted(), "心跳已提交 ⇒ streamStarted 语义")

	// guard 触发后必须 stop-and-wait：goroutine 已退出才返回 failover 错误。
	select {
	case <-hb.doneCh:
	default:
		t.Fatal("heartbeat goroutine must have exited before failover (stop-and-wait)")
	}
	framesAtReturn := obs.heartbeatFrameCount()
	_, _ = obs.Write([]byte("event: error\ndata: {\"type\":\"error\"}\n\n"))
	time.Sleep(90 * time.Millisecond)
	require.Equal(t, framesAtReturn, obs.heartbeatFrameCount(), "stop-and-wait 后心跳帧不得与调用方写出交错")

	// 窗口 = min(guard=1s, 剩余=30s) = 1s：耗时应在窗口附近，而非整段预算。
	require.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	require.Less(t, elapsed, 5*time.Second)
}

// 上游悬挂 > guard 且剩余预算 <5s：NextAccountStop（既有 FailoverExhausted 消费）。
func TestG2AMount1ClaudeSonnet5OAuthHangExhaustedWhenRemainingBelow5s(t *testing.T) {
	upstream := &g2aMockUpstream{}
	svc := g2aService(upstream)
	body := g2aStreamBody("claude-sonnet-5")
	parsed := g2aParsed(t, body)
	_, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)

	// 窗口 = min(1s, 1.2s) = 1s；超时后剩余 ≈0.2s <5s。
	snap := g2aSnapshot(1, 1, 1200*time.Millisecond)
	hb := g2aHeartbeat(context.Background(), obs, 60*time.Millisecond, 40*time.Millisecond)
	ctx := g2aInject(context.Background(), snap, hb)

	_, err := svc.Forward(ctx, c, g2aAnthropicOAuthAccount(), parsed)

	var fe *UpstreamFailoverError
	require.ErrorAs(t, err, &fe)
	require.False(t, fe.ShouldRetryNextAccount(), "剩余预算 <5s 必须耗尽（无候选）")
	require.Equal(t, NextAccountStop, fe.NextAccountAction)
	require.True(t, obs.HeaderWritten(), "心跳已提交（SSE 200），耗尽渲染归 G2b（streamStarted=true）")
}

// 换号交接连续性：首轮 guard 超时（心跳已提交）→ 下一 attempt 即时续写首帧（不重等
// delay），交接空窗 < delay；换号后成功返回。
func TestG2AMount1ClaudeSonnet5OAuthFailoverHandoffResumesWithoutDelay(t *testing.T) {
	upstream := &g2aMockUpstream{steps: []g2aCall{g2aHangFn, g2aSuccessFn(g2aAnthropicSSE)}}
	svc := g2aService(upstream)
	body := g2aStreamBody("claude-sonnet-5")
	_, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)

	snap := g2aSnapshot(1, 1, 20*time.Second)
	hb := g2aHeartbeat(context.Background(), obs, 60*time.Millisecond, 40*time.Millisecond)
	ctx := g2aInject(context.Background(), snap, hb)

	// 第一轮：guard 超时（心跳先提交）。
	_, err1 := svc.Forward(ctx, c, g2aAnthropicOAuthAccount(), g2aParsed(t, body))
	var fe *UpstreamFailoverError
	require.ErrorAs(t, err1, &fe)
	require.True(t, fe.ShouldRetryNextAccount())
	require.True(t, obs.HeaderWritten())
	framesBeforeResume := obs.heartbeatFrameCount()

	// 放大 initialDelay：若 Resume 重等 delay，则 5s 内不会出现新帧。
	hb.initialDelay = 5 * time.Second

	start := time.Now()
	result, err2 := svc.Forward(ctx, c, g2aAnthropicOAuthAccount(), g2aParsed(t, body))
	handoff := time.Since(start)

	require.NoError(t, err2, "换号后成功必须正常返回")
	require.NotNil(t, result)
	require.Greater(t, obs.heartbeatFrameCount(), framesBeforeResume, "Resume 必须即时续写首帧")
	require.Less(t, handoff, 3*time.Second, "交接空窗必须 < delay（不重等 initialDelay）")
}

// kimi/deepseek/zhipu 三平台 × 挂点 1 悬挂回放（63-victim 形态）。
func TestG2AMount1KimiDeepSeekZhipuHangReplay(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		model    string
	}{
		{"kimi", PlatformKimi, "kimi-k3"},
		{"deepseek", PlatformDeepseek, "deepseek-v4-pro"},
		{"zhipu", PlatformZhipu, "glm-5.2"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &g2aMockUpstream{}
			svc := g2aService(upstream)
			body := g2aStreamBody(tc.model)
			parsed := g2aParsed(t, body)
			_, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)

			snap := g2aSnapshot(1, 1, 30*time.Second)
			hb := g2aHeartbeat(context.Background(), obs, 60*time.Millisecond, 40*time.Millisecond)
			ctx := g2aInject(context.Background(), snap, hb)

			_, err := svc.Forward(ctx, c, g2aAPIKeyAccount(int64(910+i), tc.platform, "g2a-"+tc.name), parsed)

			var fe *UpstreamFailoverError
			require.ErrorAs(t, err, &fe)
			require.True(t, fe.ShouldRetryNextAccount())
			require.Equal(t, http.StatusGatewayTimeout, fe.StatusCode)
			require.Contains(t, string(fe.ResponseBody), "upstream_first_byte_timeout")
			require.True(t, obs.HeaderWritten(), "平台 %s：心跳须先提交 SSE 200", tc.platform)
			require.Greater(t, obs.heartbeatFrameCount(), 0, "平台 %s：须收到 keep-alive 帧", tc.platform)
			require.Equal(t, 1, upstream.callCount(), "单次 attempt 内不满足重试条件，不得重复调用上游")
		})
	}
}

// 非流式墙内预算：入口已耗时间被扣除、attempt 窗口被不可变剩余预算截断（非重建完整
// guard），第二次尝试窗口进一步被截断，总耗时 ≤ 总预算。
func TestG2AMount1NonStreamBudgetClampsSecondAttemptWindow(t *testing.T) {
	upstream := &g2aMockUpstream{}
	svc := g2aService(upstream)
	account := g2aAPIKeyAccount(920, PlatformKimi, "g2a-kimi-nonstream")

	// 入口前置延迟 200ms 已消耗：EntryMonotonic 提前，AbsoluteDeadline 仍为 entry+1s。
	entry := time.Now().Add(-200 * time.Millisecond)
	snap := &RequestBudgetSnapshot{
		GuardSeconds:          90,
		HeartbeatDelaySeconds: 0,
		EntryMonotonic:        entry,
		AbsoluteDeadline:      entry.Add(time.Second),
	}
	ctx := g2aInject(context.Background(), snap, nil)

	body := g2aNonStreamBody("kimi-k3")
	_, c1, _ := g2aGinContext(http.MethodPost, "/v1/messages", body)
	start1 := time.Now()
	_, err1 := svc.Forward(ctx, c1, account, g2aParsed(t, body))
	elapsed1 := time.Since(start1)

	var fe1 *UpstreamFailoverError
	require.ErrorAs(t, err1, &fe1)
	require.False(t, fe1.ShouldRetryNextAccount(), "剩余预算 <5s → 耗尽")
	// 窗口 = min(guard=90s, 剩余≈800ms) = 800ms：既证明入口已耗被扣除，又证明被剩余截断。
	require.GreaterOrEqual(t, elapsed1, 500*time.Millisecond)
	require.Less(t, elapsed1, 1000*time.Millisecond, "attempt 窗口必须被剩余预算截断（非完整 guard=90s）")

	_, c2, _ := g2aGinContext(http.MethodPost, "/v1/messages", body)
	start2 := time.Now()
	_, err2 := svc.Forward(ctx, c2, account, g2aParsed(t, body))
	elapsed2 := time.Since(start2)

	var fe2 *UpstreamFailoverError
	require.ErrorAs(t, err2, &fe2)
	require.False(t, fe2.ShouldRetryNextAccount())
	require.Less(t, elapsed2, elapsed1, "第二次尝试窗口必须被剩余预算进一步截断")
	require.Less(t, elapsed1+elapsed2, 1500*time.Millisecond, "入口前置延迟+首轮+次轮总耗时 ≤ 总预算（60s 量级的 1s，收紧断言）")
}

// 重放一致性（v4）：换号前后 wireBody 字节一致（挂点 1 的 buildUpstreamRequest 从
// 同一 body 重建）。
func TestG2AMount1ReplayWireBodyConsistent(t *testing.T) {
	svc := &GatewayService{cfg: &config.Config{}}
	account := g2aAnthropicAPIKeyAccount(930)
	body := g2aStreamBody("claude-sonnet-5")
	_, c, _ := g2aGinContext(http.MethodPost, "/v1/messages", nil)

	req1, wire1, err1 := svc.buildUpstreamRequest(context.Background(), c, account, body, "tok", "apikey", "claude-sonnet-5", true, false)
	require.NoError(t, err1)
	req2, wire2, err2 := svc.buildUpstreamRequest(context.Background(), c, account, body, "tok", "apikey", "claude-sonnet-5", true, false)
	require.NoError(t, err2)

	require.Equal(t, wire1, wire2, "换号前后 wireBody 必须完全一致（可重放契约）")
	require.Equal(t, req1.URL.String(), req2.URL.String())
}

// 客户端已断开：失败关闭、不重试（transport 取消错误原样返回，不转 failover）。
func TestG2AMount1ClientDisconnectedFailsClosedNoRetry(t *testing.T) {
	upstream := &g2aMockUpstream{steps: []g2aCall{func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	}}}
	svc := g2aService(upstream)
	account := g2aAPIKeyAccount(940, PlatformKimi, "g2a-kimi-disconnect")
	body := g2aNonStreamBody("kimi-k3")
	_, c, _ := g2aGinContext(http.MethodPost, "/v1/messages", body)

	base, cancel := context.WithCancel(context.Background())
	cancel() // 客户端已断开
	ctx := g2aInject(base, g2aSnapshot(90, 0, 30*time.Second), nil)

	_, err := svc.Forward(ctx, c, account, g2aParsed(t, body))
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, upstream.callCount(), "客户端已断开必须失败关闭、不重试")
}

// v6 ③：heartbeat=0 的流式客户端两次 attempt 均悬挂 → 总耗时 ≤ guard+25s（墙内收敛），
// 且因 heartbeat=0 禁用语义，零心跳帧。
func TestG2AMount1HeartbeatZeroStreamDoubleTimeoutWithinGuardPlus25s(t *testing.T) {
	upstream := &g2aMockUpstream{}
	svc := g2aService(upstream)
	account := g2aAnthropicOAuthAccount()
	body := g2aStreamBody("claude-sonnet-5")
	_, c1, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)
	_, c2, _ := g2aGinContext(http.MethodPost, "/v1/messages", body)

	entry := time.Now()
	snap := &RequestBudgetSnapshot{
		GuardSeconds:          1,
		HeartbeatDelaySeconds: 0, // 禁用
		EntryMonotonic:        entry,
		AbsoluteDeadline:      entry.Add(26 * time.Second), // guard+25s
	}
	hb := g2aHeartbeat(context.Background(), obs, 50*time.Millisecond, 20*time.Millisecond)
	ctx := g2aInject(context.Background(), snap, hb)

	start := time.Now()
	_, err1 := svc.Forward(ctx, c1, account, g2aParsed(t, body))
	_, err2 := svc.Forward(ctx, c2, account, g2aParsed(t, body))
	total := time.Since(start)

	var fe1, fe2 *UpstreamFailoverError
	require.ErrorAs(t, err1, &fe1)
	require.ErrorAs(t, err2, &fe2)
	require.True(t, fe1.ShouldRetryNextAccount())
	require.True(t, fe2.ShouldRetryNextAccount())

	require.Less(t, total, 26*time.Second, "heartbeat=0 流式双超时必须 ≤ guard+25s 墙内收敛")
	require.Zero(t, obs.totalWritten(), "heartbeat=0 为禁用语义：禁止起搏心跳")
	require.False(t, hb.IsCommitted())
}

// v6 ①：响应头先到、body 传输超预算 → 完整送达（护栏已停表，未取消 body 读取 context）。
func TestG2AMount1HeaderFirstBodyBeyondBudgetDelivered(t *testing.T) {
	const payload = `{"usage":{"input_tokens":3,"output_tokens":1},"content":[{"type":"text","text":"ok"}]}`
	upstream := &g2aMockUpstream{steps: []g2aCall{func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"rid-g2a-body"}},
			Body:       &g2aSlowBody{ctx: req.Context(), data: []byte(payload), delay: 400 * time.Millisecond},
		}, nil
	}}}
	svc := g2aService(upstream)
	account := g2aAPIKeyAccount(950, PlatformKimi, "g2a-kimi-body")
	body := g2aNonStreamBody("kimi-k3")
	rec, c, _ := g2aGinContext(http.MethodPost, "/v1/messages", body)

	// 窗口 200ms < body 400ms：响应头先到即停表，body 超预算仍完整送达。
	ctx := g2aInject(context.Background(), g2aSnapshot(1, 0, 200*time.Millisecond), nil)

	start := time.Now()
	result, err := svc.Forward(ctx, c, account, g2aParsed(t, body))
	require.NoError(t, err, "响应头先到后 body 超预算必须完整送达，不被护栏截断")
	require.NotNil(t, result)
	require.GreaterOrEqual(t, time.Since(start), 400*time.Millisecond)
	require.Contains(t, rec.Body.String(), `"input_tokens":3`)
}

// 无快照（旧路径）：不装护栏、不起搏心跳，健康路径照常成功返回。
func TestG2AMount1NoSnapshotLegacyPathSuccess(t *testing.T) {
	upstream := &g2aMockUpstream{steps: []g2aCall{g2aSuccessFn(g2aAnthropicSSE)}}
	svc := g2aService(upstream)
	account := g2aAPIKeyAccount(960, PlatformKimi, "g2a-kimi-legacy")
	body := g2aStreamBody("kimi-k3")
	rec, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)

	// 未注入快照/owner：走既有无护栏旧路径（不兜底）。
	result, err := svc.Forward(context.Background(), c, account, g2aParsed(t, body))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 0, obs.heartbeatFrameCount(), "无快照不得起搏心跳")
	require.Equal(t, http.StatusOK, rec.Code)
}

// 健康路径零回归：上游 <delay 正常返回 → 零心跳帧、护栏零触发、状态码/流透传一致。
func TestG2AMount1HealthyPathZeroHeartbeatZeroGuardTrigger(t *testing.T) {
	upstream := &g2aMockUpstream{steps: []g2aCall{g2aSuccessFn(g2aAnthropicSSE)}}
	svc := g2aService(upstream)
	body := g2aStreamBody("claude-sonnet-5")
	rec, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)

	snap := g2aSnapshot(1, 1, 30*time.Second)
	hb := g2aHeartbeat(context.Background(), obs, 600*time.Millisecond, 100*time.Millisecond)
	ctx := g2aInject(context.Background(), snap, hb)

	result, err := svc.Forward(ctx, c, g2aAnthropicOAuthAccount(), g2aParsed(t, body))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, hb.IsCommitted(), "上游 < delay 正常返回时零心跳帧")
	require.Equal(t, 0, obs.heartbeatFrameCount())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "message_stop", "语义流必须照常送达")
}
