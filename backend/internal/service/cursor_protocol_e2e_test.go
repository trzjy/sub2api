//go:build unit

package service

// E6 卡的 forwardCursor 端到端单测：入站 OpenAI 请求 → 出站 Cursor 协议 → 回写 OpenAI。
//
// 覆盖真实链路的每一环：换 token（带缓存）→ 请求转换 → 出站 → 响应转换。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cursorE2EUpstream 是同时扮演 AuthService 与 ChatService 的假中转。
type cursorE2EUpstream struct {
	mu sync.Mutex
	// requests 记录出站请求（path + body）。
	requests []cursorE2ERequest
	// chatBody 是 ChatService 返回的 envelope 帧流。
	chatBody []byte
	// chatStatus 非 0 时 ChatService 返回该状态码。
	chatStatus int
	// tokenCalls / chatCalls 计数。
	tokenCalls int
	chatCalls  int
}

type cursorE2ERequest struct {
	path   string
	body   []byte
	header http.Header
}

func (u *cursorE2EUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		u.mu.Lock()
		u.requests = append(u.requests, cursorE2ERequest{
			path: r.URL.Path, body: buf[:n], header: r.Header.Clone(),
		})
		u.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/GetSessionToken"):
			u.mu.Lock()
			u.tokenCalls++
			u.mu.Unlock()
			w.Header().Set("Content-Type", "application/proto")
			_, _ = w.Write(cursorAppendStringField(nil, 1, "SESSION-TOKEN-XYZ"))
		case strings.HasSuffix(r.URL.Path, "/StreamUnifiedChatWithTools"):
			u.mu.Lock()
			u.chatCalls++
			status, body := u.chatStatus, u.chatBody
			u.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"Unauthorized","message":"鉴权失败: nope"}`))
				return
			}
			w.Header().Set("Content-Type", cursorConnectContentType)
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (u *cursorE2EUpstream) count() (token, chat int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.tokenCalls, u.chatCalls
}

func (u *cursorE2EUpstream) requestAt(i int) cursorE2ERequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[i]
}

// newCursorE2EService 装配一个指向假中转的 OpenAIGatewayService。
func newCursorE2EService(t *testing.T, upstream *cursorE2EUpstream) (*OpenAIGatewayService, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ts := httptest.NewServer(upstream.handler())
	t.Cleanup(ts.Close)

	nodes := NewCursorUpstreamNodesResolver(WithCursorNodesCacheTTL(time.Hour))
	nodes.cached = CursorUpstreamNodes{HTTP1Base: ts.URL, HTTP2Base: ts.URL}
	nodes.cachedAt = time.Now()

	svc := NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{
		7: {ID: 7, CodeEncrypted: "ACTIVATION-CODE-7"},
	}}
	svc.SetCursorAuthDeps(repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
	svc.SetCursorForwarder(NewCursorUpstreamForwarder(
		WithCursorUpstreamNodes(nodes),
		WithCursorUpstreamHTTPClient(ts.Client()),
	))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return svc, c, w
}

// cursorE2EAccount 返回一个模型映射到 f36 wire ID 的 cursor 账号。
func cursorE2EAccount() *Account {
	return &Account{
		ID: 1, Platform: PlatformCursor, QuotaKeyID: qkID(7),
		Credentials: map[string]any{
			"model_mapping": map[string]any{"cursor-sonnet": "claude-opus-4-8-low"},
		},
	}
}

// TestForwardCursorEndToEnd 覆盖流式入站的完整链路。
func TestForwardCursorEndToEnd(t *testing.T) {
	// 构造一条成功的 Cursor 响应流：stream_start + 两段文本 + EndStream。
	var chat []byte
	chat = append(chat, cursorStreamStartFrame(t)...)
	chat = append(chat, cursorTextFrame(t, "Hello")...)
	chat = append(chat, cursorTextFrame(t, " world")...)
	chat = append(chat, cursorTestEndStream(t, `{}`)...)

	upstream := &cursorE2EUpstream{chatBody: chat}
	svc, c, w := newCursorE2EService(t, upstream)

	body := []byte(`{"model":"cursor-sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	res, err := svc.forwardCursor(t.Context(), c, cursorE2EAccount(), body, "cursor-sonnet", true, time.Now())
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.Stream)
	require.Equal(t, "cursor-sonnet", res.Model)
	require.Equal(t, "claude-opus-4-8-low", res.UpstreamModel)

	tokenCalls, chatCalls := upstream.count()
	require.Equal(t, 1, tokenCalls, "exactly one token exchange")
	require.Equal(t, 1, chatCalls)

	// 出站请求顺序：先换 token，再对话。
	require.Equal(t, "/aiserver.v1.AuthService/GetSessionToken", upstream.requestAt(0).path)
	require.Equal(t, cursorPathStreamUnifiedChatWithTools, upstream.requestAt(1).path)

	// 对话请求必须带用户级身份（Bearer session_token）与设备级身份（x-jg-auth）。
	chatReq := upstream.requestAt(1)
	require.Equal(t, "Bearer SESSION-TOKEN-XYZ", chatReq.header.Get("Authorization"))
	require.NotEmpty(t, chatReq.header.Get("x-jg-auth"), "device-level auth must still be present")
	require.Equal(t, cursorConnectContentType, chatReq.header.Get("Content-Type"))

	// 请求体必须带 f36 wire ID，而不是展示名。
	require.Contains(t, string(chatReq.body), "claude-opus-4-8-low")

	// 回写客户端的 SSE 必须含完整文本。
	out := w.Body.String()
	require.Contains(t, out, `"role":"assistant"`)
	require.Contains(t, out, "Hello")
	require.Contains(t, out, " world")
	require.Contains(t, out, cursorSSEDoneSentinel)
}

// TestForwardCursorTokenCachedAcrossCalls 覆盖两次对话只换一次 token。
func TestForwardCursorTokenCachedAcrossCalls(t *testing.T) {
	var chat []byte
	chat = append(chat, cursorTextFrame(t, "ok")...)
	chat = append(chat, cursorTestEndStream(t, `{}`)...)

	upstream := &cursorE2EUpstream{chatBody: chat}
	svc, c, _ := newCursorE2EService(t, upstream)
	body := []byte(`{"model":"cursor-sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	for i := 0; i < 3; i++ {
		_, err := svc.forwardCursor(t.Context(), c, cursorE2EAccount(), body, "cursor-sonnet", true, time.Now())
		require.NoError(t, err)
	}
	tokenCalls, chatCalls := upstream.count()
	require.Equal(t, 3, chatCalls)
	require.Equal(t, 1, tokenCalls, "session token must be cached across calls (TTL 30m)")
}

// TestForwardCursorNonStreaming 覆盖非流式入站（合成单条 chat.completion）。
func TestForwardCursorNonStreaming(t *testing.T) {
	var chat []byte
	chat = append(chat, cursorTextFrame(t, `a "quoted" answer`)...)
	chat = append(chat, cursorTestEndStream(t, `{}`)...)

	upstream := &cursorE2EUpstream{chatBody: chat}
	svc, c, w := newCursorE2EService(t, upstream)

	body := []byte(`{"model":"cursor-sonnet","messages":[{"role":"user","content":"hi"}]}`)
	res, err := svc.forwardCursor(t.Context(), c, cursorE2EAccount(), body, "cursor-sonnet", false, time.Now())
	require.NoError(t, err)
	require.False(t, res.Stream)

	out := w.Body.String()
	require.Contains(t, out, `"object":"chat.completion"`)
	require.Contains(t, out, `a \"quoted\" answer`)
	require.NotContains(t, out, cursorSSEDoneSentinel, "non-streaming must not emit SSE")
}

// TestForwardCursorUpstreamErrorPropagates 覆盖上游错误不回写成功响应。
func TestForwardCursorUpstreamErrorPropagates(t *testing.T) {
	upstream := &cursorE2EUpstream{chatStatus: 401}
	svc, c, _ := newCursorE2EService(t, upstream)

	body := []byte(`{"model":"cursor-sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	res, err := svc.forwardCursor(t.Context(), c, cursorE2EAccount(), body, "cursor-sonnet", true, time.Now())
	require.Error(t, err)
	require.Nil(t, res)
	require.ErrorIs(t, err, ErrCursorRelayAuthRejected)
	require.Contains(t, err.Error(), "鉴权失败: nope")
}

// TestForwardCursorStreamErrorPropagates 覆盖流内错误（含缺 EndStream）失败关闭。
func TestForwardCursorStreamErrorPropagates(t *testing.T) {
	t.Run("EndStream 报业务错误", func(t *testing.T) {
		var chat []byte
		chat = append(chat, cursorTestEndStream(t,
			`{"error":{"code":"unauthenticated","message":"User is unauthorized",`+
				`"details":[{"debug":{"error":"ERROR_UNAUTHORIZED","details":{"detail":"User is unauthorized"}}}]}}`)...)
		upstream := &cursorE2EUpstream{chatBody: chat}
		svc, c, _ := newCursorE2EService(t, upstream)
		body := []byte(`{"model":"cursor-sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		_, err := svc.forwardCursor(t.Context(), c, cursorE2EAccount(), body, "cursor-sonnet", true, time.Now())
		require.ErrorIs(t, err, ErrCursorUpstreamStream)
		require.Contains(t, err.Error(), "ERROR_UNAUTHORIZED")
	})

	t.Run("缺 EndStream 失败关闭", func(t *testing.T) {
		var chat []byte
		chat = append(chat, cursorTextFrame(t, "partial")...)
		upstream := &cursorE2EUpstream{chatBody: chat}
		svc, c, _ := newCursorE2EService(t, upstream)
		body := []byte(`{"model":"cursor-sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		_, err := svc.forwardCursor(t.Context(), c, cursorE2EAccount(), body, "cursor-sonnet", true, time.Now())
		require.ErrorIs(t, err, ErrCursorMissingEndStream)
	})
}

// TestForwardCursorNeverLeaksTokenInError 钉死会话 token 不进错误文案。
func TestForwardCursorNeverLeaksTokenInError(t *testing.T) {
	const token = "SESSION-TOKEN-LEAK-CANARY"

	// 构造一个会把 token 回显进错误体的中转。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/GetSessionToken") {
			w.Header().Set("Content-Type", "application/proto")
			_, _ = w.Write(cursorAppendStringField(nil, 1, token))
			return
		}
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"Unauthorized","message":"bad token ` + token + `"}`))
	}))
	t.Cleanup(srv.Close)

	nodes := NewCursorUpstreamNodesResolver(WithCursorNodesCacheTTL(time.Hour))
	nodes.cached = CursorUpstreamNodes{HTTP1Base: srv.URL, HTTP2Base: srv.URL}
	nodes.cachedAt = time.Now()

	svc := NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{
		7: {ID: 7, CodeEncrypted: "ACTIVATION-CODE-7"},
	}}
	svc.SetCursorAuthDeps(repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
	svc.SetCursorForwarder(NewCursorUpstreamForwarder(
		WithCursorUpstreamNodes(nodes),
		WithCursorUpstreamHTTPClient(srv.Client()),
	))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"model":"cursor-sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	_, err := svc.forwardCursor(t.Context(), c, cursorE2EAccount(), body, "cursor-sonnet", true, time.Now())
	require.Error(t, err)
	require.NotContains(t, err.Error(), token, "session token must never appear in error text")
	require.NotContains(t, w.Body.String(), token, "session token must never reach the client")
}
