//go:build unit

package service

// E6 卡的 AuthService token 层单测：user 字段编码、缓存命中与过期、失败关闭。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cursorTokenTestServer 是 token 端点的假上游。
type cursorTokenTestServer struct {
	calls      atomic.Int64
	lastBody   []byte
	lastHeader http.Header
	status     int
	respBody   func(call int64) []byte
}

func (s *cursorTokenTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := s.calls.Add(1)
		buf := make([]byte, 4096)
		read, _ := r.Body.Read(buf)
		s.lastBody = buf[:read]
		s.lastHeader = r.Header.Clone()
		status := s.status
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		_, _ = w.Write(s.respBody(n))
	})
}

// cursorTokenOKBody 返回一个含 session_token 的 GetSessionTokenResponse（字段 1）。
func cursorTokenOKBody(token string) []byte {
	return cursorAppendStringField(nil, 1, token)
}

// newTestTokenSource 构造一个指向假上游的 token source。
func newTestTokenSource(t *testing.T, srv *cursorTokenTestServer, opts ...cursorSessionTokenOption) (*cursorSessionTokenSource, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	// 把假上游预置为节点缓存：Resolve 会直接命中缓存返回，不触网络、不回落静态节点。
	nodes := NewCursorUpstreamNodesResolver(WithCursorNodesCacheTTL(time.Hour))
	nodes.cached = CursorUpstreamNodes{HTTP1Base: ts.URL, HTTP2Base: ts.URL}
	nodes.cachedAt = time.Now()

	fwd := NewCursorUpstreamForwarder(
		WithCursorUpstreamNodes(nodes),
		WithCursorUpstreamHTTPClient(ts.Client()),
		WithCursorUpstreamAuthProvider(cursorTestAuthProvider("CIPHER")),
	)
	src, err := newCursorSessionTokenSource(fwd, cursorTestAuthProvider("CIPHER"), "ACTIVATION-CODE",
		opts...)
	require.NoError(t, err)
	return src, ts
}

// TestCursorSessionTokenRequestShape 钉死 GetSessionToken 请求体的字段号与取值。
//
// 字段号依据官方客户端 @24696138 取证；user 结构依据 @24694117（**id 是字段 2**）。
func TestCursorSessionTokenRequestShape(t *testing.T) {
	srv := &cursorTokenTestServer{respBody: func(int64) []byte { return cursorTokenOKBody("TOK") }}
	src, _ := newTestTokenSource(t, srv, withCursorSessionTokenUserID("user-123"))

	tok, err := src.CursorSessionToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "TOK", tok)
	require.Equal(t, int64(1), srv.calls.Load())

	fields, err := cursorDecodeProtobuf(srv.lastBody)
	require.NoError(t, err, "body must be bare protobuf (Unary), not enveloped")

	// 2 destination = 2 (DESTINATION_AISERVER)
	dest, ok := cursorFindField(fields, cursorFldGetSessionTokenDestination)
	require.True(t, ok)
	require.Equal(t, uint64(cursorDestinationAIServer), dest.Varint)

	// 4 code = 激活码（活体：不带即 501 ERROR_DEPRECATED）
	code, ok := cursorFindField(fields, cursorFldGetSessionTokenCode)
	require.True(t, ok)
	require.Equal(t, "ACTIVATION-CODE", code.String())

	// 6 redirect_uri（活体：不带即 400 Invalid redirect_uri）
	ru, ok := cursorFindField(fields, cursorFldGetSessionTokenRedirectURI)
	require.True(t, ok)
	require.Equal(t, cursorDefaultAuthRedirectURI, ru.String())

	// 1 user{2 id}
	userField, ok := cursorFindField(fields, cursorFldGetSessionTokenUser)
	require.True(t, ok)
	userFields, err := cursorDecodeProtobuf(userField.Bytes)
	require.NoError(t, err)
	idField, ok := cursorFindField(userFields, cursorFldUserID)
	require.True(t, ok)
	require.Equal(t, "user-123", idField.String())

	// User 没有字段 1（取证确认 id 是字段 2）——若实现误用字段 1，此断言会失败。
	_, hasF1 := cursorFindField(userFields, 1)
	require.False(t, hasF1, "aiserver.v1.User has no field 1; id is field 2")

	// 请求体**不含** envelope 头（Unary 端点是裸 protobuf）。
	// 若误加 envelope，首字节会是 flags=0 且 [1:5] 是长度；此处长度字段位置
	// 落在一个远大于 body 的值上，据此判定"没有 envelope"。
	require.NotZero(t, srv.lastBody[0], "bare protobuf must start with a field tag")
	asEnvelopeLen := int(srv.lastBody[1])<<24 | int(srv.lastBody[2])<<16 |
		int(srv.lastBody[3])<<8 | int(srv.lastBody[4])
	require.Greater(t, asEnvelopeLen, len(srv.lastBody),
		"body must be bare protobuf, not a 5-byte envelope (envelope length would fit the body)")
}

// TestCursorSessionTokenUserOmitted 覆盖 user 为空时**整段省略**（活体证明省略安全）。
func TestCursorSessionTokenUserOmitted(t *testing.T) {
	srv := &cursorTokenTestServer{respBody: func(int64) []byte { return cursorTokenOKBody("TOK") }}
	src, _ := newTestTokenSource(t, srv) // 不设 userID

	tok, err := src.CursorSessionToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "TOK", tok)

	fields, err := cursorDecodeProtobuf(srv.lastBody)
	require.NoError(t, err)
	_, hasUser := cursorFindField(fields, cursorFldGetSessionTokenUser)
	require.False(t, hasUser, "empty user id ⇒ user field must be omitted entirely")
}

// TestCursorSessionTokenCacheHitAndExpiry 钉死缓存命中与过期。
func TestCursorSessionTokenCacheHitAndExpiry(t *testing.T) {
	srv := &cursorTokenTestServer{respBody: func(n int64) []byte {
		return cursorTokenOKBody("TOK" + string(rune('0'+n)))
	}}

	now := time.Unix(1_700_000_000, 0)
	clock := now
	src, _ := newTestTokenSource(t, srv, withCursorSessionTokenClock(func() time.Time { return clock }, time.Minute))

	// 第一次：穿透到上游。
	first, err := src.CursorSessionToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, "TOK1", first)
	require.Equal(t, int64(1), srv.calls.Load())

	// TTL 内：命中缓存，不再打上游。
	clock = now.Add(30 * time.Second)
	second, err := src.CursorSessionToken(context.Background())
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, int64(1), srv.calls.Load(), "cache hit must not call upstream")

	// TTL 过期：重新换取。
	clock = now.Add(90 * time.Second)
	third, err := src.CursorSessionToken(context.Background())
	require.NoError(t, err)
	require.NotEqual(t, second, third, "expired cache must be refreshed")
	require.Equal(t, int64(2), srv.calls.Load())
}

// TestCursorSessionTokenFailsClosed 覆盖 token 拿不到时一律失败关闭。
func TestCursorSessionTokenFailsClosed(t *testing.T) {
	t.Run("上游非 200", func(t *testing.T) {
		srv := &cursorTokenTestServer{
			status:   401,
			respBody: func(int64) []byte { return []byte(`{"error":"Unauthorized","message":"鉴权失败"}`) },
		}
		src, _ := newTestTokenSource(t, srv)
		_, err := src.CursorSessionToken(context.Background())
		require.ErrorIs(t, err, ErrCursorRelayAuthRejected)
	})

	t.Run("响应无 session_token 字段", func(t *testing.T) {
		srv := &cursorTokenTestServer{respBody: func(int64) []byte {
			return cursorAppendVarintField(nil, 3, 1) // 只有 should_show_trial
		}}
		src, _ := newTestTokenSource(t, srv)
		_, err := src.CursorSessionToken(context.Background())
		require.ErrorIs(t, err, ErrCursorSessionTokenUnavailable)
	})

	t.Run("session_token 为空串", func(t *testing.T) {
		srv := &cursorTokenTestServer{respBody: func(int64) []byte {
			return cursorAppendStringField(nil, 1, "   ")
		}}
		src, _ := newTestTokenSource(t, srv)
		_, err := src.CursorSessionToken(context.Background())
		require.ErrorIs(t, err, ErrCursorSessionTokenUnavailable)
	})

	t.Run("空响应体", func(t *testing.T) {
		srv := &cursorTokenTestServer{respBody: func(int64) []byte { return nil }}
		src, _ := newTestTokenSource(t, srv)
		_, err := src.CursorSessionToken(context.Background())
		require.ErrorIs(t, err, ErrCursorSessionTokenUnavailable)
	})

	t.Run("响应非 protobuf", func(t *testing.T) {
		srv := &cursorTokenTestServer{respBody: func(int64) []byte { return []byte("not-protobuf-at-all") }}
		src, _ := newTestTokenSource(t, srv)
		_, err := src.CursorSessionToken(context.Background())
		require.ErrorIs(t, err, ErrCursorSessionTokenUnavailable)
	})

	t.Run("失败不写缓存（下次仍会重试）", func(t *testing.T) {
		srv := &cursorTokenTestServer{status: 500, respBody: func(int64) []byte { return []byte(`{}`) }}
		src, _ := newTestTokenSource(t, srv)
		_, err1 := src.CursorSessionToken(context.Background())
		require.Error(t, err1)
		_, err2 := src.CursorSessionToken(context.Background())
		require.Error(t, err2)
		require.Equal(t, int64(2), srv.calls.Load(), "a failed exchange must not be cached")
	})
}

// TestCursorRedactSecrets 钉死凭证不进错误信息。
//
// 上游错误文案可能回显请求里带过的 code / session token；本层必须把它们抹掉，
// 否则会话材料会随错误进 ops 事件与客户端回传。
func TestCursorRedactSecrets(t *testing.T) {
	const secret = "SUPER-SECRET-ACTIVATION-CODE"
	body := []byte(`{"error":"boom","message":"` + secret + `"}`)

	code, msg := cursorExtractUpstreamErrorSummary(body, secret)
	require.NotContains(t, msg, secret, "upstream message must be redacted")
	require.Contains(t, msg, "***")
	require.Equal(t, "boom", code)

	err := cursorUpstreamStatusError(500, body, secret)
	require.NotContains(t, err.Error(), secret, "error text must be redacted")
	require.Contains(t, err.Error(), "***")

	// EndStream 的 message 同样要脱敏。
	_, endMsg, perr := cursorParseEndStream(
		[]byte(`{"error":{"code":"x","message":"token is `+secret+`"}}`), secret)
	require.NoError(t, perr)
	require.NotContains(t, endMsg, secret)

	// 过短的值不抹（避免误伤正常文案，如 "TOKEN" 这种常见词）。
	require.Equal(t, "a TOKEN here", cursorRedactSecrets("a TOKEN here", "TOKEN"))
	require.Equal(t, "a TOKEN here", cursorRedactSecrets("a TOKEN here", ""))
	require.Equal(t, "", cursorRedactSecrets("", "LONG-SECRET-VALUE"))
}

// TestNewCursorSessionTokenSourceFailsClosed 覆盖构造期的失败关闭。
func TestNewCursorSessionTokenSourceFailsClosed(t *testing.T) {
	fwd := NewCursorUpstreamForwarder()
	auth := cursorTestAuthProvider("CIPHER")
	cases := []struct {
		name string
		fwd  *CursorUpstreamForwarder
		auth CursorUpstreamAuthProvider
		code string
	}{
		{"nil forwarder", nil, auth, "CODE"},
		{"nil auth", fwd, nil, "CODE"},
		{"empty code", fwd, auth, "  "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := newCursorSessionTokenSource(c.fwd, c.auth, c.code)
			require.ErrorIs(t, err, ErrCursorSessionTokenUnavailable)
		})
	}
}

// TestCursorSessionTokenRedirectURIOverride 覆盖 redirect_uri 可覆盖。
func TestCursorSessionTokenRedirectURIOverride(t *testing.T) {
	srv := &cursorTokenTestServer{respBody: func(int64) []byte { return cursorTokenOKBody("TOK") }}
	src, _ := newTestTokenSource(t, srv, withCursorSessionTokenRedirectURI("https://example.test/cb"))
	_, err := src.CursorSessionToken(context.Background())
	require.NoError(t, err)
	require.Contains(t, string(srv.lastBody), "https://example.test/cb")

	// 空串回落默认值（不产生空 redirect_uri——上游会 400）。
	srv2 := &cursorTokenTestServer{respBody: func(int64) []byte { return cursorTokenOKBody("TOK") }}
	src2, _ := newTestTokenSource(t, srv2, withCursorSessionTokenRedirectURI("   "))
	_, err = src2.CursorSessionToken(context.Background())
	require.NoError(t, err)
	require.Contains(t, string(srv2.lastBody), cursorDefaultAuthRedirectURI)
}

// TestCursorGetSessionTokenContentType 钉死 Unary 端点（GetSessionToken）的
// Content-Type 用 `application/proto`（裸 protobuf），而非 streaming 的
// `application/connect+proto`。
//
// 生产实证：Unary 端点收到 `application/connect+proto` 的裸 protobuf 体直接 415；
// 改用 `application/proto` 后请求穿透到鉴权层（后续 401 是号池账号过期，与内容类型无关）。
func TestCursorGetSessionTokenContentType(t *testing.T) {
	srv := &cursorTokenTestServer{respBody: func(int64) []byte { return cursorTokenOKBody("TOK") }}
	src, _ := newTestTokenSource(t, srv)

	_, err := src.CursorSessionToken(context.Background())
	require.NoError(t, err)

	require.Equal(t, cursorUnaryContentType, srv.lastHeader.Get("Content-Type"),
		"GetSessionToken 是 Unary 端点，必须用裸 protobuf 内容类型")
	require.NotEqual(t, cursorConnectContentType, srv.lastHeader.Get("Content-Type"),
		"GetSessionToken 不得用 streaming 内容类型（会 415）")
}

// cursorStaticAuthProvider 是固定值的鉴权材料替身。
type cursorStaticAuthProvider struct{ value string }

func (p cursorStaticAuthProvider) CursorUpstreamAuth(context.Context) (string, error) {
	return p.value, nil
}

// cursorTestAuthProvider 是 cursorStaticAuthProvider 的构造糖。
func cursorTestAuthProvider(value string) CursorUpstreamAuthProvider {
	return cursorStaticAuthProvider{value: value}
}
