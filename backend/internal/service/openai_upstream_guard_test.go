//go:build unit

package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// —— guard 测试基础设施 ——

// guardTestUpstream 是定向测试用的 HTTPUpstream mock：Do 行为由 fn 注入，called 记录
// 是否真正发出了上游请求（用于断言“剩余<5s 不向上游发请求”）。不依赖真实网络。
type guardTestUpstream struct {
	fn     func(req *http.Request) (*http.Response, error)
	called *atomic.Bool
}

func (u *guardTestUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if u.called != nil {
		u.called.Store(true)
	}
	return u.fn(req)
}

func (u *guardTestUpstream) DoWithTLS(req *http.Request, p string, a int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, a, c)
}

func guardTestOpenAIAccount() *Account {
	return &Account{
		ID: 99, Name: "openai-acc", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Concurrency: 1, Credentials: map[string]any{"api_key": "sk-test"},
	}
}

// guardCaptureHandler 捕获 slog 记录，用于断言 G5 观测事件发射（零新事件、复用 cf524_observation）。
type guardCaptureHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *guardCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *guardCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *guardCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *guardCaptureHandler) WithGroup(string) slog.Handler      { return h }

func (h *guardCaptureHandler) findEvent(msg, attrKey, attrVal string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Message != msg {
			continue
		}
		var ok bool
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == attrKey && a.Value.String() == attrVal {
				ok = true
				return false
			}
			return true
		})
		if ok {
			return true
		}
	}
	return false
}

// —— 场景①：无快照透传等价（未安装快照的调用方——embeddings/count_tokens/alpha_search/
// 测试桩——零行为变化，护栏零介入） ——

func TestOpenAIUpstreamGuard_NoSnapshotPassthrough(t *testing.T) {
	up := &guardTestUpstream{fn: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
	}}
	svc := &OpenAIGatewayService{httpUpstream: up}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	resp, err := svc.doOpenAIUpstreamWithGuard(c, context.Background(), req, "", guardTestOpenAIAccount())
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// —— 场景②：有快护肤栏超时（窗口耗尽返回 *UpstreamFailoverError，ShouldRetryNextAccount
// 与剩余预算关系正确：剩余≥5s → true） ——

func TestOpenAIUpstreamGuard_WindowExhaustedReturnsFailoverError(t *testing.T) {
	called := &atomic.Bool{}
	up := &guardTestUpstream{
		called: called,
		fn: func(req *http.Request) (*http.Response, error) {
			// 阻塞在护栏 reqCtx 上，等待护栏窗口耗尽取消。
			<-req.Context().Done()
			return nil, req.Context().Err()
		},
	}
	svc := &OpenAIGatewayService{httpUpstream: up}

	// 快照：guard=1s（窗口=1s），剩余≈55s（≥5s）→ 应返回可换号错误。
	now := time.Now()
	snap := NewRequestBudgetSnapshot(1, 0, now, false)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := WithRequestBudgetSnapshot(context.Background(), snap)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	resp, err := svc.doOpenAIUpstreamWithGuard(c, ctx, req, "", guardTestOpenAIAccount())
	require.Nil(t, resp)
	require.Error(t, err)

	var fe *UpstreamFailoverError
	require.True(t, errors.As(err, &fe), "护栏超时错误必须是 *UpstreamFailoverError")
	require.True(t, fe.ShouldRetryNextAccount(), "剩余≥5s 时应允许换号（NextAccountRetry）")
	require.True(t, called.Load(), "窗口耗尽前仍发起过一次上游请求")
}

// —— 场景③：剩余<5s 不发请求即返回超时错误（ShouldRetryNextAccount=false） ——

func TestOpenAIUpstreamGuard_RemainingBelowMinViableNoRequest(t *testing.T) {
	called := &atomic.Bool{}
	up := &guardTestUpstream{
		called: called,
		fn: func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
		},
	}
	svc := &OpenAIGatewayService{httpUpstream: up}

	// 快照：绝对截止时间已过 → 剩余=0 <5s → 不应发起上游请求。
	snap := &RequestBudgetSnapshot{
		GuardSeconds:          1,
		HeartbeatDelaySeconds: 0,
		AbsoluteDeadline:      time.Now().Add(-1 * time.Second),
		EntryMonotonic:        time.Now(),
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := WithRequestBudgetSnapshot(context.Background(), snap)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	resp, err := svc.doOpenAIUpstreamWithGuard(c, ctx, req, "", guardTestOpenAIAccount())
	require.Nil(t, resp)
	require.Error(t, err)
	require.False(t, called.Load(), "剩余<5s 不应向上游发请求")

	var fe *UpstreamFailoverError
	require.True(t, errors.As(err, &fe), "应是 *UpstreamFailoverError")
	require.False(t, fe.ShouldRetryNextAccount(), "剩余<5s 应耗尽换号（NextAccountStop）")
}

// —— 场景④：心跳分支（owner 存在时 Resume 被调、header 到达后 stop-and-wait） ——

func TestOpenAIUpstreamGuard_HeartbeatResumeAndStopAndWait(t *testing.T) {
	up := &guardTestUpstream{fn: func(req *http.Request) (*http.Response, error) {
		// 略微延迟，使心跳 goroutine（initialDelay 归零，立即启动）先于响应头到达提交 SSE 200，
		// 稳定复现"心跳已提交 → 响应头晚到 → AfterCommit → stop-and-wait"路径。
		time.Sleep(100 * time.Millisecond)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
	}}
	svc := &OpenAIGatewayService{httpUpstream: up}

	now := time.Now()
	// delay=15s 但其 initialDelay 经 entry 折算归零：entry 远早于现在 ⇒ 立即启动并提交 SSE 200。
	snap := NewRequestBudgetSnapshot(30, 15, now.Add(-20*time.Second), true)
	hbw := httptest.NewRecorder()
	hb := NewUpstreamHeartbeat(context.Background(), hbw, snap, now)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := WithRequestBudgetSnapshot(context.Background(), snap)
	ctx = WithUpstreamHeartbeat(ctx, hb)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	resp, err := svc.doOpenAIUpstreamWithGuard(c, ctx, req, "", guardTestOpenAIAccount())
	require.NoError(t, err)
	require.NotNil(t, resp, "响应头到达后应返回上游 resp")
	// Resume 成功并在 header 到达前已提交 SSE 200 ⇒ owner 确实被起搏且 stop-and-wait 干净退出。
	require.True(t, hb.IsCommitted(), "心跳 owner 应被 Resume 起搏并提交 SSE 200")
	require.Equal(t, http.StatusOK, hbw.Code, "心跳应写出 SSE 200 响应头")
}

// —— 场景⑤：观测——护栏超时发 gateway_first_byte_guard_triggered（phase=attempt）；
// 响应头到达后 upstream_headers_received_ms 可解析 ——

func TestOpenAIUpstreamGuard_ObservationEmitsGuardTriggered(t *testing.T) {
	ch := &guardCaptureHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(ch))
	defer slog.SetDefault(old)

	up := &guardTestUpstream{fn: func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}
	svc := &OpenAIGatewayService{httpUpstream: up}

	now := time.Now()
	snap := NewRequestBudgetSnapshot(1, 0, now, false) // 窗口=1s → 触发护栏超时

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := WithRequestBudgetSnapshot(context.Background(), snap)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	_, _ = svc.doOpenAIUpstreamWithGuard(c, ctx, req, "", guardTestOpenAIAccount())

	require.True(t, ch.findEvent("gateway_first_byte_guard_triggered", "phase", "attempt"),
		"护栏超时必须发射 gateway_first_byte_guard_triggered（phase=attempt）")
}

func TestOpenAIUpstreamGuard_HeadersReceivedMsParsable(t *testing.T) {
	up := &guardTestUpstream{fn: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
	}}
	svc := &OpenAIGatewayService{httpUpstream: up}

	now := time.Now()
	snap := NewRequestBudgetSnapshot(30, 0, now, false)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := WithRequestBudgetSnapshot(context.Background(), snap)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://upstream.example/v1/chat/completions", nil)

	resp, err := svc.doOpenAIUpstreamWithGuard(c, ctx, req, "", guardTestOpenAIAccount())
	require.NoError(t, err)
	require.NotNil(t, resp)

	ms, ok := UpstreamHeadersReceivedMs(c, snap.EntryMonotonic)
	require.True(t, ok, "响应头到达后应可解析 upstream_headers_received_ms")
	require.GreaterOrEqual(t, ms, int64(0))
}
