//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 闸②终审第 3 轮整改（派发单 7）四项的定向测试：
//   1. keepalive 窗口：CN watchdog 覆盖的尝试在首字节前不提交心跳，60s 无数据
//      时首包超时仍能换号；非 CN 路径心跳行为不变。
//   2. transport profile：wctx 从 request.Context() 派生，profile 值不丢失。
//   3. 取消纳入原子裁决：父 context 取消与定时器竞争同一原子状态，唯一胜者。
//   4. 非超时读取错误契约：超限/解析失败/读取错误三类各按原 endpoint 契约。

// —— 整改项 1：keepalive 首字节前不提交心跳 ——

// openAICNKeepaliveIdleBody 首字节前完全静默（不产出任何数据），收到 context
// 取消后返回 ctx 错误：模拟"60s 内上游一个 body 字节都没给"的 CN 上游。
// ctx 为 watchdog 派生的 request context：首包超时取消它，读取随即以
// context.Canceled 中断，外层包装器再翻译为内部超时（与真实链路一致）。
type openAICNKeepaliveIdleBody struct {
	ctx    context.Context
	closed chan struct{}
}

func newOpenAICNKeepaliveIdleBody(ctx context.Context) *openAICNKeepaliveIdleBody {
	return &openAICNKeepaliveIdleBody{ctx: ctx, closed: make(chan struct{})}
}

func (b *openAICNKeepaliveIdleBody) Read(p []byte) (int, error) {
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.EOF
	}
}

func (b *openAICNKeepaliveIdleBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

// TestOpenAICNFirstByteTimeout_KeepaliveDoesNotBlockFailover 钉死整改项 1 的核心
// 契约：CN 账号 + keepalive 开启 + 上游 body 静默时，keepalive 到期（首字节
// 到达前）不得提交响应头/心跳，60s 首包超时的透明换号窗口必须完整保留。旧行为
// 会先写 SSE 注释心跳/Anthropic ping 提交响应头（c.Writer.Written()==true），
// 使 handler 因 Writer.Size() 变化拒绝 failover、无法换号。
func TestOpenAICNFirstByteTimeout_KeepaliveDoesNotBlockFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			StreamKeepaliveInterval: 1,
			MaxLineSize:             defaultMaxLineSize,
		}},
	}

	for _, tc := range []struct {
		name    string
		account *Account
		path    string
		consume func(svc *OpenAIGatewayService, resp *http.Response, c *gin.Context, account *Account) (*OpenAIForwardResult, error)
	}{
		{
			name:    "chat_completions",
			account: cnKimiAccount(),
			path:    "/v1/chat/completions",
			consume: func(svc *OpenAIGatewayService, resp *http.Response, c *gin.Context, account *Account) (*OpenAIForwardResult, error) {
				return svc.handleChatStreamingResponse(resp, c, account, "kimi-k3", "kimi-k3", "kimi-k3", time.Now(), 0)
			},
		},
		{
			name:    "messages",
			account: cnKimiAccount(),
			path:    "/v1/messages",
			consume: func(svc *OpenAIGatewayService, resp *http.Response, c *gin.Context, account *Account) (*OpenAIForwardResult, error) {
				return svc.handleAnthropicStreamingResponse(resp, c, account, "kimi-k3", "kimi-k3", "kimi-k3", time.Now())
			},
		},
		{
			// 闸②终审第 4 轮整改项 1：native anthropic 直通是第三处 keepalive
			// 路径，此前 ping 未受首字节前抑制，60s 换号窗口被提前提交响应头打穿。
			name:    "messages_native_anthropic",
			account: cnKimiAccount(),
			path:    "/v1/messages",
			consume: func(svc *OpenAIGatewayService, resp *http.Response, c *gin.Context, account *Account) (*OpenAIForwardResult, error) {
				return svc.handleNativeAnthropicStreamingResponse(
					context.Background(), resp, c, account, "kimi-k3", "kimi-k3", "kimi-k3", nil, time.Now(),
				)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// watchdog 首包超时设 5s（不干扰 keepalive 节奏），keepalive 1s：
			// 首字节前若心跳被写出（旧行为），t≈1s 即提交响应头；整改后必须
			// 跳过心跳，t≈1.6s 时响应头仍未提交（换号窗口仍在）。
			watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 5*time.Second)
			idleBody := newOpenAICNKeepaliveIdleBody(wctx)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &openAICNFirstByteTimeoutBody{ReadCloser: idleBody, w: watchdog},
			}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)

			resultCh := make(chan consumeResult, 1)
			go func() {
				result, err := tc.consume(svc, resp, c, tc.account)
				resultCh <- consumeResult{result: result, err: err}
			}()

			// 等待 keepalive 至少触发一轮（1s）+ 余量：首字节前心跳若被写出，
			// 此刻响应头必然已提交。
			time.Sleep(1600 * time.Millisecond)

			require.False(t, c.Writer.Written(),
				"首字节前 keepalive 心跳必须被跳过：响应头不得提前提交（换号窗口保留）")
			require.NotContains(t, rec.Body.String(), ":\n\n", "首字节前不得写出 SSE 注释心跳")
			require.NotContains(t, rec.Body.String(), "event: ping", "首字节前不得写出 Anthropic ping")

			// 结束静默（EOF），消费器退出。整改语义下无心跳写出，观察 Written
			// 已在上面钉死；收尾等待 goroutine 不泄漏。
			idleBody.Close()
			select {
			case r := <-resultCh:
				require.False(t, isOpenAICNFirstByteTimeout(r.err), "EOF 正常结束不得被当作首包超时")
			case <-time.After(2 * time.Second):
				t.Fatal("消费器在 EOF 后未退出")
			}
		})
	}
}

type consumeResult struct {
	result *OpenAIForwardResult
	err    error
}

// TestOpenAICNFirstByteTimeout_KeepaliveResumesAfterFirstByte 钉死整改项 1 的
// 后半段：上游首字节到达后恢复既有心跳节奏（首字节后心跳必须能提交响应头），
// 避免把"首字节前抑制"误实现成"永久关闭心跳"。
func TestOpenAICNFirstByteTimeout_KeepaliveResumesAfterFirstByte(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			StreamKeepaliveInterval: 1,
			MaxLineSize:             defaultMaxLineSize,
		}},
	}

	// 首字节（response.created，尚未提交客户端输出）后长时间静默：keepalive 必须
	// 恢复既有节奏并写出 SSE 注释心跳。
	pr, pw := io.Pipe()
	watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 30*time.Second)
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"))
		time.Sleep(1600 * time.Millisecond)
	}()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICNFirstByteTimeoutBody{ReadCloser: pr, w: watchdog},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	_, _ = svc.handleChatStreamingResponse(resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", time.Now(), 0)

	require.Contains(t, rec.Body.String(), ":\n\n", "首字节到达后 keepalive 心跳必须恢复")
	require.True(t, c.Writer.Written(), "首字节后心跳应能提交响应头")
}

// TestOpenAICNFirstByteTimeout_NonCNKeepaliveUnchanged 钉死禁区：非 CN 路径的
// keepalive 行为必须与整改前完全一致（body 未被 watchdog 包装时心跳照旧提交
// 响应头）。本测试与旧行为逐字对齐。
func TestOpenAICNFirstByteTimeout_NonCNKeepaliveUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			StreamKeepaliveInterval: 1,
			MaxLineSize:             defaultMaxLineSize,
		}},
	}

	// 非 CN 账号：body 不会被 watchdog 包装，静默 1.6s 后必须写出心跳。
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_non_cn\"}}\n\n"))
		time.Sleep(1600 * time.Millisecond)
	}()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr, // 未包装：无 watchdog
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	openAIAccount := &Account{ID: 2, Name: "openai", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}

	_, _ = svc.handleChatStreamingResponse(resp, c, openAIAccount, "gpt-4o", "gpt-4o", "gpt-4o", time.Now(), 0)

	require.Contains(t, rec.Body.String(), ":\n\n", "非 CN 路径 keepalive 行为不得改变")
	require.True(t, c.Writer.Written(), "非 CN 路径首字节后心跳应能提交响应头")
}

// —— 整改项 2：transport profile 保留 ——

// TestOpenAICNFirstByteTimeout_TransportProfilePreserved 钉死整改项 2：CN 请求
// 携带的 HTTPUpstreamProfileOpenAI（连接池/协议/代理选路依据）不得因 watchdog
// 派生 request context 而丢失。
func TestOpenAICNFirstByteTimeout_TransportProfilePreserved(t *testing.T) {
	var seenProfile HTTPUpstreamProfile
	upstream := &cnFirstByteMockUpstream{fn: func(req *http.Request) (*http.Response, error) {
		seenProfile = HTTPUpstreamProfileFromContext(req.Context())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: ok\n\n")),
		}, nil
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 5 * time.Second}

	// 模拟 sendCCUpstreamRequest 的构建方式：request 自身 context 带 profile。
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	// 客户端 context 与 request context 是两个独立对象（后者经 detach 剥离取消）。
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()

	resp, err := svc.doOpenAIUpstream(clientCtx, req, "", cnKimiAccount())
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, HTTPUpstreamProfileOpenAI, seenProfile,
		"CN 请求的 transport profile 不得因 watchdog 派生 context 而丢失")
	_, wrapped := resp.Body.(*openAICNFirstByteTimeoutBody)
	require.True(t, wrapped, "CN 路径出站 body 仍应被首包超时包装")
}

// TestOpenAICNFirstByteTimeout_ClientCancelStillArbitrated 钉死整改项 2 不破坏
// 整改项 1 之外的取消语义：profile 从 request.Context() 派生后，客户端取消仍
// 必须经独立 clientCtx 信号参与裁决（保持 context.Canceled，不冷却不换号）。
func TestOpenAICNFirstByteTimeout_ClientCancelStillArbitrated(t *testing.T) {
	clientCtx, cancelClient := context.WithCancel(context.Background())
	cancelClient()
	upstream := &cnFirstByteMockUpstream{fn: func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 20 * time.Millisecond}
	// request context 已 detach（无取消信号），只有 clientCtx 保留取消信号。
	req, _ := http.NewRequestWithContext(context.WithoutCancel(clientCtx), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	_, err := svc.doOpenAIUpstream(clientCtx, req, "", cnKimiAccount())

	require.Error(t, err)
	require.False(t, isOpenAICNFirstByteTimeout(err), "客户端取消不得被误判为内部首包超时")
	require.NotErrorIs(t, err, errOpenAICNFirstByteTimeout)
}

// —— 整改项 3：取消纳入原子裁决 ——

// TestOpenAICNFirstByteTimeout_ParentCancelWinsAtomicRace 钉死整改项 3：父
// context 取消不再走"先检查再 CAS"的窗口，而是作为一个非超时终态候选，与
// 定时器通过同一个原子状态 CAS 竞争唯一胜者。
//
// "取消恰在检查与 CAS 之间"方向（旧实现的真实竞态）：
// 1) 客户端断开 → watchParent 已把裁决 CAS 成非超时终态；
// 2) 定时器随后到期 → expire 的 CAS 失败，不得改判为内部超时。
// 旧实现里若取消恰好落在 parentDone() 检查通过之后、CAS(0,2) 执行之前，
// expire 会把客户端取消误判成内部超时（→错误冷却+换号）。
func TestOpenAICNFirstByteTimeout_ParentCancelWinsAtomicRace(t *testing.T) {
	t.Run("取消赢下唯一胜者后定时器不得改判为内部超时", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())
		watchdog, wctx := newOpenAICNFirstByteWatchdog(parent, parent, 30*time.Second)

		cancelParent()
		// 等待 watchParent 完成 CAS 裁决（唯一胜者 = 非超时终态）。
		require.Eventually(t, func() bool {
			return watchdog.decided.Load() == watchdogStateSettled
		}, time.Second, time.Millisecond, "客户端取消必须赢得非超时终态")

		// 定时器随后"到期"（直接触发 expire，等价于检查与 CAS 之间发生取消）。
		watchdog.expire()
		time.Sleep(30 * time.Millisecond)

		require.False(t, watchdog.Fired(), "取消赢得裁决后定时器不得改判为内部超时（误判窗口已消除）")
		<-wctx.Done()
		require.Error(t, wctx.Err(), "客户端取消应取消当前上游尝试")
	})

	t.Run("取消与定时器同时竞争时必须唯一胜者", func(t *testing.T) {
		// 反复并发触发取消与 expire：二者经同一原子 CAS 竞争，裁决必须是唯一
		// 终态（settled 或 timedOut），绝不出现中间态双写——旧实现在"检查后
		// CAS"的窗口里会让取消被定时器吞掉、出现不可裁决的双结果。
		for i := 0; i < 300; i++ {
			parent, cancelParent := context.WithCancel(context.Background())
			watchdog, _ := newOpenAICNFirstByteWatchdog(parent, parent, time.Duration(i%5)*time.Millisecond)
			cancelParent()
			watchdog.expire()
			state := watchdog.decided.Load()
			require.True(t, state == watchdogStateSettled || state == watchdogStateTimedOut,
				"裁决状态必须是唯一终态（settled 或 timedOut），got=%d", state)
			// 取消先行完成（watchParent 已赢得 settled）后再触发定时器，
			// 定时器必须失效——这是"取消先到"方向，绝不允许出现内部超时。
			if state == watchdogStateSettled {
				require.False(t, watchdog.Fired(),
					"settled（含取消胜出）后不得同时成立 Fired（第 %d 轮）", i)
			}
		}
	})

	t.Run("父取消不得把 context.Canceled 转译为内部超时", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())
		reqCtx, cancelReq := context.WithCancel(context.Background())
		watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), parent, 5*time.Second)
		defer cancelReq()
		cancelParent()
		// 等取消裁决落地，再让底层读以 context.Canceled 中断。
		require.Eventually(t, func() bool {
			return watchdog.decided.Load() == watchdogStateSettled
		}, time.Second, time.Millisecond)

		body := &openAICNFirstByteTimeoutBody{
			ReadCloser: newCNFirstByteBlockingBody(nil, reqCtx),
			w:          watchdog,
		}
		cancelReq()
		_, err := body.Read(make([]byte, 16))

		require.ErrorIs(t, err, context.Canceled, "客户端取消必须保持 context.Canceled 语义")
		require.False(t, isOpenAICNFirstByteTimeout(err), "context.Canceled 不得被转译为内部超时")
	})
}

// —— 整改项 4：非超时读取错误的原契约 ——

// openAICNFailingCCBody 返回指定读取错误的响应体。
type openAINonTimeoutBody struct {
	err error
}

func (b *openAINonTimeoutBody) Read(p []byte) (int, error) { return 0, b.err }
func (b *openAINonTimeoutBody) Close() error               { return nil }

// openAIParseFailBody 返回可读但非 JSON 的响应体（触发解析失败契约）。
type openAIParseFailBody struct{ data string }

func (b *openAIParseFailBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}
func (b *openAIParseFailBody) Close() error { return nil }

// ccFallbackConsumer 描述两条 CC fallback 缓冲路径的共同调用形状。
type ccFallbackConsumer struct {
	name       string
	path       string
	invoke     func(svc *OpenAIGatewayService, c *gin.Context, account *Account, resp *http.Response) (*OpenAIForwardResult, error)
	wantTypeAt func(t *testing.T, rec *httptest.ResponseRecorder)
}

func ccFallbackConsumers() []ccFallbackConsumer {
	return []ccFallbackConsumer{
		{
			name: "responses→CC",
			path: "/v1/responses",
			invoke: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, resp *http.Response) (*OpenAIForwardResult, error) {
				return svc.bufferChatCompletionsAsResponses(
					c, account, resp, "kimi-k3", map[string]bool{}, map[string]bool{}, false,
					map[string]apicompat.NamespacedToolName{}, "kimi-k3", "kimi-k3", nil, nil, time.Now(),
				)
			},
			// /v1/responses 回退路径为裸 error 对象。
			wantTypeAt: func(t *testing.T, rec *httptest.ResponseRecorder) {
				require.Contains(t, rec.Body.String(), `"type":"api_error"`)
			},
		},
		{
			name: "messages→CC",
			path: "/v1/messages",
			invoke: func(svc *OpenAIGatewayService, c *gin.Context, account *Account, resp *http.Response) (*OpenAIForwardResult, error) {
				return svc.bufferChatCompletionsAsAnthropic(
					c, account, resp, "kimi-k3", "kimi-k3", "kimi-k3", nil, nil, time.Now(),
				)
			},
			// /v1/messages 为 Anthropic 错误信封。
			wantTypeAt: func(t *testing.T, rec *httptest.ResponseRecorder) {
				require.Contains(t, rec.Body.String(), `"type":"error"`, "messages 路径必须用 Anthropic 错误信封")
			},
		},
	}
}

// TestOpenAICNFirstByteTimeout_NonTimeoutReadErrorsKeepEndpointContract 钉死整改
// 项 4：内部首包超时仍走 failover；其余三类读取/解析错误按原 endpoint 契约
// 分别回写，三类不得互相归并：
//   - ErrUpstreamResponseBodyTooLarge → upstream_error: Upstream response too large
//   - JSON 解析失败 → api_error: Failed to parse upstream response
//   - 其他读取错误 → api_error: Failed to read upstream response
func TestOpenAICNFirstByteTimeout_NonTimeoutReadErrorsKeepEndpointContract(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oversizeSvc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{UpstreamResponseReadMaxBytes: 16}},
	}
	parseFailSvc := &OpenAIGatewayService{}
	readFailSvc := &OpenAIGatewayService{}

	t.Run("响应超限走 TooLarge 契约", func(t *testing.T) {
		for _, consumer := range ccFallbackConsumers() {
			t.Run(consumer.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, consumer.path, nil)
				big := strings.Repeat("x", 128)
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &openAIParseFailBody{data: big}}

				result, err := consumer.invoke(oversizeSvc, c, cnKimiAccount(), resp)

				require.Error(t, err)
				require.Nil(t, result)
				require.Equal(t, http.StatusBadGateway, rec.Code, "超限契约是 502")
				require.Contains(t, rec.Body.String(), "Upstream response too large",
					"超限必须走 TooLarge 契约，不得归并到 read/parse")
				require.NotContains(t, rec.Body.String(), "Failed to read upstream response")
				require.NotContains(t, rec.Body.String(), "Failed to parse upstream response")
			})
		}
	})

	t.Run("JSON 解析失败走 parse 契约", func(t *testing.T) {
		for _, consumer := range ccFallbackConsumers() {
			t.Run(consumer.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, consumer.path, nil)
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &openAIParseFailBody{data: "not-json"}}

				result, err := consumer.invoke(parseFailSvc, c, cnKimiAccount(), resp)

				require.Error(t, err)
				require.Nil(t, result)
				require.Equal(t, http.StatusBadGateway, rec.Code)
				require.Contains(t, rec.Body.String(), "Failed to parse upstream response",
					"解析失败必须走 parse 契约，不得归并到 read")
				require.NotContains(t, rec.Body.String(), "Failed to read upstream response")
				consumer.wantTypeAt(t, rec)
			})
		}
	})

	t.Run("其他读取错误走 read 契约", func(t *testing.T) {
		for _, consumer := range ccFallbackConsumers() {
			t.Run(consumer.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, consumer.path, nil)
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &openAINonTimeoutBody{err: errors.New("boom")}}

				result, err := consumer.invoke(readFailSvc, c, cnKimiAccount(), resp)

				require.Error(t, err)
				require.Nil(t, result)
				require.Equal(t, http.StatusBadGateway, rec.Code)
				require.Contains(t, rec.Body.String(), "Failed to read upstream response",
					"读取错误必须走 read 契约，不得归并到 parse/TooLarge")
				require.NotContains(t, rec.Body.String(), "Failed to parse upstream response")
				require.NotContains(t, rec.Body.String(), "Upstream response too large")
				consumer.wantTypeAt(t, rec)
			})
		}
	})
}

// TestOpenAICNFirstByteTimeout_ReadHelperDistinguishesTimeoutFromOtherErrors
// 钉死 helper 层边界：内部首包超时时 readCCUpstreamJSONResponse 不得写任何
// 客户端响应（failover 透明切换窗口必须保留），而其他读取错误按契约回写。
func TestOpenAICNFirstByteTimeout_ReadHelperDistinguishesTimeoutFromOtherErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}

	t.Run("内部超时原样返回不写客户端响应", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &openAINonTimeoutBody{err: errOpenAICNFirstByteTimeout}}

		_, _, err := svc.readCCUpstreamJSONResponse(resp, c, writeOpenAIResponsesFallbackError)

		require.Error(t, err)
		require.True(t, isOpenAICNFirstByteTimeout(err), "内部超时须可被 failover 链识别")
		require.False(t, c.Writer.Written(), "内部超时时不得写任何客户端响应（否则 failover 无法重放）")
		require.Empty(t, rec.Body.String())
	})

	t.Run("其他读取错误按契约回写并保留错误", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &openAINonTimeoutBody{err: fmt.Errorf("read failed")}}

		_, _, err := svc.readCCUpstreamJSONResponse(resp, c, writeOpenAIResponsesFallbackError)

		require.Error(t, err)
		require.False(t, isOpenAICNFirstByteTimeout(err), "非内部超时不得被误判为内部超时")
		require.Contains(t, rec.Body.String(), "Failed to read upstream response")
	})
}

// —— 闸②终审第 4 轮整改（派发单 8）定向测试 ——

// TestOpenAICNFirstByteTimeout_TimeoutWinnerSurvivesLateClientCancel 钉死整改项 2：
// 60s 定时器先 CAS 成 timedOut（唯一胜者）并取消请求后，客户端在 Do 返回前随后
// 取消（parentDone() 为真）。旧错误分类先查 parentDone()，把已裁定的超时覆盖为
// 底层取消错误→504 冷却与换号丢失。整改后先按 Fired() 处理已胜出的超时，客户端
// 后取消不得覆盖。
//
// 时序由 mock 上游确定性构造：watchdog 定时器到期触发 Fired()（取消 request
// context），mock 在等待到取消后立即取消 clientCtx（制造"Do 返回时 parentDone()
// 已为真"），再返回 context.Canceled。doOpenAIUpstream 必须判内部超时。
func TestOpenAICNFirstByteTimeout_TimeoutWinnerSurvivesLateClientCancel(t *testing.T) {
	clientCtx, cancelClient := context.WithCancel(context.Background())
	upstream := &cnFirstByteMockUpstream{fn: func(req *http.Request) (*http.Response, error) {
		// req.Context() 是 watchdog 派生的 wctx：Done() 触发意味着定时器已 CAS
		// 成 timedOut 并取消上游尝试（Fired() 已成立）。此时再让客户端后取消，
		// 制造"内部超时胜者与客户端取消同时成立"的错误分类输入。
		<-req.Context().Done()
		cancelClient()
		return nil, req.Context().Err()
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cnFirstByteTimeout: 20 * time.Millisecond}
	// request context 用 detached context 构造（调用方 detach 后状态）。
	req, _ := http.NewRequestWithContext(context.WithoutCancel(context.Background()), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	_, err := svc.doOpenAIUpstream(clientCtx, req, "", cnKimiAccount())

	require.Error(t, err)
	require.True(t, isOpenAICNFirstByteTimeout(err),
		"watchdog 已裁定超时胜者后，客户端后取消不得覆盖为 context.Canceled")
	require.False(t, errors.Is(err, context.Canceled), "已胜出的内部超时不得降级为客户端取消")
}

// TestOpenAICNFirstByteTimeout_NativeAnthropicIntervalDoesNotPreempt 钉死整改项 3：
// 合法配置 stream_data_interval_timeout=30..59 下，流式消费者的 interval ticker
// 会在 60s watchdog 之前到期。body 被 CN watchdog 包装且首字节未到达时，interval
// tick 不得抢先裁决（不得返回普通 interval timeout），首字节前统一由 watchdog 超时
// 裁决为 failover。此测试把 interval 配置为 1s（< watchdog 2s）验证抢占被抑制。
func TestOpenAICNFirstByteTimeout_NativeAnthropicIntervalDoesNotPreempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 1, // interval 1s，先于 watchdog 到期
			MaxLineSize:               defaultMaxLineSize,
		}},
	}
	watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 2*time.Second)
	// 首字节前完全静默：若 interval 未被抑制，t≈1s 即返回普通 interval timeout；
	// 被抑制后 t≈2s 由 watchdog 取消 wctx，读取以 context.Canceled 中断并经
	// 包装层转译为内部超时。
	idleBody := newOpenAICNKeepaliveIdleBody(wctx)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &openAICNFirstByteTimeoutBody{ReadCloser: idleBody, w: watchdog},
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := svc.handleNativeAnthropicStreamingResponse(
		context.Background(), resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", nil, time.Now(),
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr,
		"首字节前 interval tick 不得抢跑为普通 interval timeout：必须由 watchdog 超时映射 failover")
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Equal(t, NextAccountRetry, failoverErr.NextAccountAction)
	// 流中断路径按既有契约返回非 nil result（空 usage，与 :478 terminal 缺失
	// 路径同构），usage 带出语义不变；failover 语义由错误断言承载。
	require.NotNil(t, result)
	require.Zero(t, result.Usage.OutputTokens, "首字节前超时：不得产出任何 usage")
	require.False(t, c.Writer.Written(), "首字节前不得提交响应头（换号窗口保留）")
	require.NotContains(t, rec.Body.String(), "stream data interval timeout",
		"旧 interval timer 不得在首字节前返回普通 interval timeout")
}

// TestOpenAICNFirstByteTimeout_NativeAnthropicBlankLineCommitUsesRealState 钉死
// 第 6 轮 must_fix：本函数为原始行直通，上游先发空行（"\n\n"）时 writeStreamLine
// 写出 "\n" 即提交响应头，但本地 clientOutputStarted 只认非空行——旧判定仅看本地
// 标志，会对已提交响应返回 failover，与外层 c.Writer.Written() 换号门脱节（客户端
// 收到 200 空流）。整改后判定用真实下游提交状态（openAIStreamClientOutputStarted）：
// 已提交 → after-headers 流中断语义，不再尝试透明切换。
func TestOpenAICNFirstByteTimeout_NativeAnthropicBlankLineCommitUsesRealState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 2*time.Second)
	pr, pw := io.Pipe()
	body := &openAICNFirstByteTimeoutBody{ReadCloser: pr, w: watchdog}
	go func() {
		defer pw.Close()
		// 确定性模拟 body.Read 的边界竞态（expire 先于 firstByte 赢得 CAS）：
		// 先等 watchdog 到期取消 wctx，再吐两个空行（writeStreamLine 写 "\n"
		// 即提交响应头，但本地 clientOutputStarted 只认非空行），随后关闭写端，
		// 管道 Read 以 EOF 终止并经 watchdog body（Fired）转译为内部超时。
		// 注意不能先写字节：body.Read 在 n>0 时 firstByte() disarm 定时器，
		// 先写会让 watchdog 永不触发（管道无读者侧取消）而挂死。
		<-wctx.Done()
		_, _ = pw.Write([]byte("\n\n"))
	}()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	result, err := svc.handleNativeAnthropicStreamingResponse(
		context.Background(), resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", nil, time.Now(),
	)

	require.Error(t, err)
	require.True(t, c.Writer.Written(), "空行写出后响应头已真实提交")
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr,
		"响应头已提交时不得返回透明 failover（与外层 Written() 门脱节）")
	require.Contains(t, err.Error(), "stream read error",
		"已提交后超时必须走 after-headers 流中断语义")
	require.NotNil(t, result)
}

// TestOpenAICNFirstByteTimeout_NativeAnthropicIntervalSurvivesAfterFirstByte 钉死
// 整改项 3 后半段：首字节到达后既有 interval 语义不变——interval tick 在首字节后
// 必须照常裁决（普通 interval timeout，不 failover）。避免把"首字节前抑制"误实现
// 成"首字节后也永久关闭 interval"。
func TestOpenAICNFirstByteTimeout_NativeAnthropicIntervalSurvivesAfterFirstByte(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 1,
			MaxLineSize:               defaultMaxLineSize,
		}},
	}
	watchdog, _ := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 30*time.Second)
	pr, pw := io.Pipe()
	body := &openAICNFirstByteTimeoutBody{ReadCloser: pr, w: watchdog}
	go func() {
		defer pw.Close()
		// 首字节（message_start）到达后静默超过 interval（1s）：既有语义下
		// interval tick 必须照常裁决为普通 interval timeout。
		_, _ = pw.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"kimi-k3\",\"usage\":{\"input_tokens\":10}}}\n\n"))
		time.Sleep(1500 * time.Millisecond)
	}()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_, err := svc.handleNativeAnthropicStreamingResponse(
		context.Background(), resp, c, cnKimiAccount(), "kimi-k3", "kimi-k3", "kimi-k3", nil, time.Now(),
	)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr, "首字节后 interval 语义不变：普通 interval timeout 不触发 failover")
	require.Contains(t, err.Error(), "stream data interval timeout", "首字节后 interval tick 必须照常裁决")
}

// —— 闸②终审第 5 轮整改（派发单 9）：responses native 空流 EOF 头提交 ——

// TestResponsesStreamingFromNativeAnthropic_EmptyStreamCommitsHeaders 钉死整改项
// #3：上游 200 后立即 EOF（零事件）时，FinalizeAnthropicResponsesStream 因
// !state.CreatedSent 返回 nil，响应头永不提交，客户端只收到无 SSE 头的隐式 200
// 空响应。整改后空流正常收尾也必须提交 SSE 契约头（与 CC native 姊妹路径
// openai_gateway_chat_completions_anthropic_native.go:524-528 语义一致）。
func TestResponsesStreamingFromNativeAnthropic_EmptyStreamCommitsHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newNativeAnthropicHangTestService(5)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	// 上游 200 + 立即 EOF（零事件）：空流场景。
	resp, pr, pw := newHangingUpstreamResponse()
	_ = pw.Close() // 立即 EOF

	res, err := svc.handleResponsesStreamingFromNativeAnthropic(
		context.Background(), &Account{ID: 1, Platform: PlatformZhipu, Type: AccountTypeAPIKey},
		resp, c, "glm-4.7", "glm-4.7", "glm-4.7", nil, time.Now(),
		apicompat.ResponsesClientToolMapping{},
	)
	_ = pr.Close()

	require.NoError(t, err, "空流正常 EOF 收尾必须返回 nil error（不 panic）")
	require.NotNil(t, res, "空流正常收尾按既有契约返回 result（usage 汇总）")
	require.True(t, c.Writer.Written(), "空流正常收尾必须提交响应头（SSE 契约头）")
	require.Equal(t, "text/event-stream", c.Writer.Header().Get("Content-Type"),
		"空流收尾必须携带 text/event-stream 契约头")
	require.Equal(t, http.StatusOK, rec.Code, "空流收尾必须提交 200")
}

// —— 闸②终审第 5 轮整改（派发单 9）：Forward 流式路径 keepalive 首字节前抑制 ——

// TestOpenAICNFirstByteTimeout_ForwardKeepaliveSuppressedBeforeFirstByte 钉死整改
// 项 1.4：CN 账号走 Forward（/v1/responses 通用转发）时，消费链
// openai_gateway_response_handling.go 的 keepalive case 此前完全没有 FirstByteSeen
// 抑制——CN 账号出站 body 被 watchdog 包装后若再经 forward.go:1130/:1245 双重包装，
// 旧直接断言失效，keepalive tick 会写出 ":\n\n" 并 flush 提交响应头，打穿换号窗口。
// 整改后首字节前 keepalive tick 不得写出任何字节（c.Writer.Written()==false）。
func TestOpenAICNFirstByteTimeout_ForwardKeepaliveSuppressedBeforeFirstByte(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			StreamKeepaliveInterval: 1,
			MaxLineSize:             defaultMaxLineSize,
		}},
	}

	// 完全静默的 watchdog 包装 body：首字节前 keepalive tick 若被写出（旧行为），
	// t≈1s 即提交响应头；整改后必须跳过，换号窗口保留。
	watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 5*time.Second)
	idleBody := newOpenAICNKeepaliveIdleBody(wctx)
	// 模拟 forward.go:1130 的 openAIRequestContextReadCloser 包装（载体接口穿透
	// 的关键场景：直接断言在此失效）。
	wrapped := &openAIRequestContextReadCloser{ReadCloser: &openAICNFirstByteTimeoutBody{ReadCloser: idleBody, w: watchdog}, cleanup: func() {}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       wrapped,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resultCh := make(chan struct {
		result *openaiStreamingResult
		err    error
	}, 1)
	go func() {
		result, err := svc.handleStreamingResponse(
			context.Background(), resp, c, cnKimiAccount(), time.Now(), "kimi-k3", "kimi-k3",
		)
		resultCh <- struct {
			result *openaiStreamingResult
			err    error
		}{result: result, err: err}
	}()

	// 等待 keepalive 至少触发一轮（1s）+ 余量：首字节前心跳若被写出，此刻响应头
	// 必然已提交。
	time.Sleep(1600 * time.Millisecond)

	require.False(t, c.Writer.Written(),
		"Forward 路径首字节前 keepalive 心跳必须被跳过：换号窗口保留")
	require.NotContains(t, rec.Body.String(), ":\n\n", "首字节前不得写出 SSE 注释心跳")

	// 结束静默（EOF），消费器退出，收尾不泄漏 goroutine。
	idleBody.Close()
	select {
	case r := <-resultCh:
		require.False(t, isOpenAICNFirstByteTimeout(r.err), "EOF 正常结束不得被当作首包超时")
	case <-time.After(2 * time.Second):
		t.Fatal("消费器在 EOF 后未退出")
	}
}

// TestOpenAICNFirstByteTimeout_ForwardKeepaliveUnchangedForNonCN 钉死禁区：非 CN
// 路径 Forward 流式 keepalive 行为与整改前一致——body 未被 watchdog 包装时，
// keepalive tick 照旧提交心跳/响应头。
func TestOpenAICNFirstByteTimeout_ForwardKeepaliveUnchangedForNonCN(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			StreamKeepaliveInterval: 1,
			MaxLineSize:             defaultMaxLineSize,
		}},
	}

	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_non_cn\"}}\n\n"))
		time.Sleep(1600 * time.Millisecond)
	}()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr, // 未包装：无 watchdog
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	openAIAccount := &Account{ID: 2, Name: "openai", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1}

	_, _ = svc.handleStreamingResponse(
		context.Background(), resp, c, openAIAccount, time.Now(), "gpt-4o", "gpt-4o",
	)

	require.Contains(t, rec.Body.String(), ":\n\n", "非 CN Forward 路径 keepalive 行为不得改变")
	require.True(t, c.Writer.Written(), "非 CN Forward 路径心跳应能提交响应头")
}

// TestOpenAICNFirstByteTimeout_ForwardIntervalDoesNotPreempt 钉死验收补强项（与
// 第 4 轮 messages native interval 抢跑同语义）：Forward 流式路径 interval tick
// 在首字节前不得抢先裁决——合法配置 stream_data_interval_timeout 早于 60s
// watchdog 到期时，tick 若返回普通 interval timeout 会中断流且不触发 CN
// failover/冷却，打穿换号窗口。整改后首字节前 tick 不裁决，由 60s 边界统一
// 负责；EOF 正常结束不回归。
func TestOpenAICNFirstByteTimeout_ForwardIntervalDoesNotPreempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 1, // 1s tick，早于 watchdog 5s（模拟 30..59 抢跑形态）
			MaxLineSize:               defaultMaxLineSize,
		}},
	}

	watchdog, wctx := newOpenAICNFirstByteWatchdog(context.Background(), context.Background(), 5*time.Second)
	idleBody := newOpenAICNKeepaliveIdleBody(wctx)
	wrapped := &openAIRequestContextReadCloser{ReadCloser: &openAICNFirstByteTimeoutBody{ReadCloser: idleBody, w: watchdog}, cleanup: func() {}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       wrapped,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resultCh := make(chan struct {
		result *openaiStreamingResult
		err    error
	}, 1)
	go func() {
		result, err := svc.handleStreamingResponse(
			context.Background(), resp, c, cnKimiAccount(), time.Now(), "kimi-k3", "kimi-k3",
		)
		resultCh <- struct {
			result *openaiStreamingResult
			err    error
		}{result: result, err: err}
	}()

	// 等待 interval tick 至少触发一轮（1s）+ 余量：若 tick 仍抢跑（旧行为），
	// 消费器此刻必然已带 "stream data interval timeout" 退出。
	time.Sleep(1600 * time.Millisecond)

	select {
	case r := <-resultCh:
		t.Fatalf("首字节前 interval tick 不得裁决（旧行为提前退出）：err=%v", r.err)
	default:
	}

	// 结束静默（EOF），消费器退出，收尾不泄漏 goroutine。
	idleBody.Close()
	select {
	case r := <-resultCh:
		require.False(t, isOpenAICNFirstByteTimeout(r.err), "EOF 正常结束不得被当作首包超时")
	case <-time.After(2 * time.Second):
		t.Fatal("消费器在 EOF 后未退出")
	}
}
