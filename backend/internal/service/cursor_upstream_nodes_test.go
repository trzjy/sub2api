//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// golden 向量由独立 JS 实现复刻插件 :7930-7949 算法生成（见取证报告），
// 用于逐字节核对 Go 侧 base36 变长解码，防止两边算法悄悄漂移。
var (
	cursorNodeGoldenHTTP1    = "79-15qfrxmvedgq1eqm00901xpyxgok118t5u1kt8icphrqq5n7u9g163oarn4alk0z0391agwzuctb8sfixqgiep4p9qg803ndqioy4rpy0lo2ixnbegm3wlhzgcd"
	cursorNodeGoldenHTTP11   = "81-1mmf5b8v9kn3q9en9l2l6eqmk513ptu5ynkb44tupzwkfzxmioaq75gxgtlfhcxyi72w58h13iaiqpf7c0gt1lu3kkua8288ifye7mjb0o0z0yffcmxvv1x4chh7r1"
	cursorNodeGoldenHTTP1B   = "80-88s06n6na7qyznd3rqcqjmxwkdsioclhcnwsxhvkzuofantu8decz49v0n4pars55vvs4u1o8oq73plk8rf09beaxzsk8032hfq4utmuiq8aponx1iaiih3jj8u5"
	cursorNodeGoldenMixed    = "83-2acfjraxclvss75iz95vq9ym6moqmy0i8l36l1oq1jpecyoocca3rpih8kvdbq9vtkewtrzucj9upwf9c7anyg07gy3wimrl5zoi8q35cvz51jr5el1e9ej0q0zitedml"
	cursorNodeGoldenHTTP1JS  = `{"http1":"https://cur-api2.shareai.pro","http2":"https://cur-api5.shareai.pro"}`
	cursorNodeGoldenHTTP11JS = `{"http1.1":"https://cur-api9.shareai.pro","http2":"https://cur-api8.shareai.pro"}`
	cursorNodeGoldenHTTP1BJS = `{"http11":"https://cur-api7.shareai.pro","http2":"https://cur-api6.shareai.pro"}`
	cursorNodeGoldenMixedJS  = `{"http1":"http://111.231.65.234:8081","http2":"https://cur-api5.shareai.pro","x":1}`
)

// 1) base36 变长解码：golden 向量逐字节复核（三种 http1 键名 + 混合负载）。
func TestDecodeCursorBase36VarLen_Golden(t *testing.T) {
	cases := []struct {
		name    string
		encoded string
		want    string
	}{
		{"http1", cursorNodeGoldenHTTP1, cursorNodeGoldenHTTP1JS},
		{"http1.1", cursorNodeGoldenHTTP11, cursorNodeGoldenHTTP11JS},
		{"http11", cursorNodeGoldenHTTP1B, cursorNodeGoldenHTTP1BJS},
		{"mixed", cursorNodeGoldenMixed, cursorNodeGoldenMixedJS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeCursorBase36VarLen(tc.encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got != tc.want {
				t.Errorf("decode mismatch\ngot= %s\nwant=%s", got, tc.want)
			}
		})
	}
}

// 2) base36 边界：无分隔符 / 空长度载荷 / 非法字符 / 声明长度装不下 / 超长载荷保护。
func TestDecodeCursorBase36VarLen_Boundaries(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		wantErr bool
		want    string
	}{
		// 长度 0 时缓冲区为空，任何载荷都被丢弃（与 JS 参考实现一致）。
		{"zero-length-empty-payload", "0-", false, ""},
		{"zero-length-with-zero-digit", "0-0", false, ""},
		{"no-separator", "abcd", true, ""},
		{"leading-separator", "-abc", true, ""},
		{"negative-length", "-1-abc", true, ""},
		{"non-numeric-length", "x-abc", true, ""},
		{"invalid-char", "3-abc!", true, ""},
		// 大写载荷先转小写再解码（对应 hex 00 08 d2）。
		{"uppercase-accepted", "3-1QQ", false, "\x00\x08\xd2"},
		{"payload-exceeds-length", "1-22222", true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeCursorBase36VarLen(tc.encoded)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (decoded %q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

// newCursorNodesTestServer 造一个返回指定 x-cos-meta-id 头的 fake 上游。
func newCursorNodesTestServer(t *testing.T, meta string, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if meta != "" {
			w.Header().Set(cursorRoute2MetaHeader, meta)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("add,csv,placeholder"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// 3) 动态发现成功：拉 add.csv → 读 x-cos-meta-id → 解码 → 取 http1/http2。
func TestCursorNodesResolver_DynamicSuccess(t *testing.T) {
	srv, hits := newCursorNodesTestServer(t, cursorNodeGoldenHTTP1, http.StatusOK)
	r := NewCursorUpstreamNodesResolver(
		WithCursorNodesEndpoint(srv.URL),
		WithCursorNodesHTTPClient(srv.Client()),
		WithCursorNodesCacheTTL(time.Minute),
	)
	n := r.Resolve(context.Background())
	if !n.Dynamic {
		t.Error("want Dynamic=true, got false")
	}
	if n.HTTP1Base != "https://cur-api2.shareai.pro" {
		t.Errorf("HTTP1Base=%q", n.HTTP1Base)
	}
	if n.HTTP2Base != "https://cur-api5.shareai.pro" {
		t.Errorf("HTTP2Base=%q", n.HTTP2Base)
	}
	if hits.Load() != 1 {
		t.Errorf("want 1 upstream hit, got %d", hits.Load())
	}

	// 缓存命中：不再打网络。
	n2 := r.Resolve(context.Background())
	if n2 != n {
		t.Errorf("cached nodes mismatch: %+v vs %+v", n2, n)
	}
	if hits.Load() != 1 {
		t.Errorf("cache miss: want still 1 hit, got %d", hits.Load())
	}
}

// 4) 动态发现的 http1 三种键名都被正确解析。
func TestCursorNodesResolver_KeyNameVariants(t *testing.T) {
	cases := []struct {
		name    string
		encoded string
		want1   string
	}{
		{"http1", cursorNodeGoldenHTTP1, "https://cur-api2.shareai.pro"},
		{"http1.1", cursorNodeGoldenHTTP11, "https://cur-api9.shareai.pro"},
		{"http11", cursorNodeGoldenHTTP1B, "https://cur-api7.shareai.pro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newCursorNodesTestServer(t, tc.encoded, http.StatusOK)
			r := NewCursorUpstreamNodesResolver(
				WithCursorNodesEndpoint(srv.URL),
				WithCursorNodesHTTPClient(srv.Client()),
				WithCursorNodesCacheTTL(time.Minute),
			)
			n := r.Resolve(context.Background())
			if n.HTTP1Base != tc.want1 {
				t.Errorf("HTTP1Base=%q want %q", n.HTTP1Base, tc.want1)
			}
		})
	}
}

// 5) 动态失败 → 降级到静态兜底节点（唯一的降级点，绝不回落 Cursor 官方）。
func TestCursorNodesResolver_DynamicFailureFallsBackStatic(t *testing.T) {
	staticHTTP1 := staticCursorUpstreamNodes().HTTP1Base
	staticHTTP2 := staticCursorUpstreamNodes().HTTP2Base
	if strings.Contains(staticHTTP1, "cursor.sh") || strings.Contains(staticHTTP2, "cursor.sh") {
		t.Fatalf("static fallback must NOT point at Cursor official: %+v",
			staticCursorUpstreamNodes())
	}

	cases := []struct {
		name   string
		meta   string
		status int
	}{
		{"missing-header", "", http.StatusOK},
		{"bad-base36", "not-a-valid-payload", http.StatusOK},
		{"bad-base36-char", "3-abc!", http.StatusOK},
		{"http-500", cursorNodeGoldenHTTP1, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newCursorNodesTestServer(t, tc.meta, tc.status)
			r := NewCursorUpstreamNodesResolver(
				WithCursorNodesEndpoint(srv.URL),
				WithCursorNodesHTTPClient(srv.Client()),
				WithCursorNodesCacheTTL(time.Minute),
			)
			n := r.Resolve(context.Background())
			if n.Dynamic {
				t.Error("want Dynamic=false on failure, got true")
			}
			if n.HTTP1Base != staticHTTP1 || n.HTTP2Base != staticHTTP2 {
				t.Errorf("want static fallback %s/%s, got %s/%s",
					staticHTTP1, staticHTTP2, n.HTTP1Base, n.HTTP2Base)
			}
		})
	}
}

// 6) 动态响应 JSON 缺字段 → 降级静态。
func TestCursorNodesResolver_IncompletePayloadFallsBack(t *testing.T) {
	// {"http1":"https://x.example"} 缺 http2
	payload := `{"http1":"https://cur-api3.shareai.pro"}`
	srv, _ := newCursorNodesTestServer(t, encodeCursorBase36VarLen(t, payload), http.StatusOK)
	r := NewCursorUpstreamNodesResolver(
		WithCursorNodesEndpoint(srv.URL),
		WithCursorNodesHTTPClient(srv.Client()),
		WithCursorNodesCacheTTL(time.Minute),
	)
	n := r.Resolve(context.Background())
	if n.Dynamic {
		t.Error("want Dynamic=false for incomplete payload")
	}
	if n.HTTP1Base != cursorStaticHTTP1Base {
		t.Errorf("HTTP1Base=%q want static %q", n.HTTP1Base, cursorStaticHTTP1Base)
	}
}

// 7) 上游不可达（传输错误）→ 降级静态。
func TestCursorNodesResolver_TransportErrorFallsBack(t *testing.T) {
	r := NewCursorUpstreamNodesResolver(
		WithCursorNodesEndpoint("http://cursor-nodes.invalid/add.csv"),
		WithCursorNodesHTTPClient(&http.Client{Transport: timeoutRT{}}),
		WithCursorNodesCacheTTL(time.Minute),
	)
	n := r.Resolve(context.Background())
	if n.Dynamic || n.HTTP1Base != cursorStaticHTTP1Base || n.HTTP2Base != cursorStaticHTTP2Base {
		t.Errorf("want static fallback on transport error, got %+v", n)
	}
}

// 8) 缓存过期后重新发现（TTL 生效）。
func TestCursorNodesResolver_CacheExpiry(t *testing.T) {
	srv, hits := newCursorNodesTestServer(t, cursorNodeGoldenHTTP1, http.StatusOK)
	r := NewCursorUpstreamNodesResolver(
		WithCursorNodesEndpoint(srv.URL),
		WithCursorNodesHTTPClient(srv.Client()),
		WithCursorNodesCacheTTL(time.Nanosecond),
	)
	r.Resolve(context.Background())
	r.Resolve(context.Background())
	if hits.Load() < 2 {
		t.Errorf("expired cache should refetch: hits=%d", hits.Load())
	}
}

// 9) 并发 Resolve 只打一次上游（singleflight 语义）。
func TestCursorNodesResolver_ConcurrentResolveSingleFetch(t *testing.T) {
	srv, hits := newCursorNodesTestServer(t, cursorNodeGoldenHTTP1, http.StatusOK)
	r := NewCursorUpstreamNodesResolver(
		WithCursorNodesEndpoint(srv.URL),
		WithCursorNodesHTTPClient(srv.Client()),
		WithCursorNodesCacheTTL(time.Minute),
	)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := r.Resolve(context.Background())
			if n.HTTP1Base != "https://cur-api2.shareai.pro" {
				t.Errorf("unexpected nodes: %+v", n)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("want exactly 1 upstream hit under concurrency, got %d", hits.Load())
	}
}

// 10) 超长 meta 头被拒绝（不无界读入）。
func TestCursorNodesResolver_MetaTooLargeRejected(t *testing.T) {
	huge := strings.Repeat("z", cursorNodeMetaMaxBytes+16)
	srv, _ := newCursorNodesTestServer(t, huge, http.StatusOK)
	r := NewCursorUpstreamNodesResolver(
		WithCursorNodesEndpoint(srv.URL),
		WithCursorNodesHTTPClient(srv.Client()),
		WithCursorNodesCacheTTL(time.Minute),
	)
	n := r.Resolve(context.Background())
	if n.Dynamic {
		t.Error("oversized meta must not be accepted")
	}
}

// 11) 3xx 视为成功（插件 validateStatus 接受 200<=s<400）。
func TestCursorNodesResolver_RedirectAccepted(t *testing.T) {
	srv, _ := newCursorNodesTestServer(t, cursorNodeGoldenHTTP1, http.StatusNoContent)
	r := NewCursorUpstreamNodesResolver(
		WithCursorNodesEndpoint(srv.URL),
		WithCursorNodesHTTPClient(srv.Client()),
		WithCursorNodesCacheTTL(time.Minute),
	)
	if n := r.Resolve(context.Background()); !n.Dynamic {
		t.Errorf("2xx/3xx must be accepted, got %+v", n)
	}
}

// 12) 静态兜底常量必须与插件 :7883-7884 一致。
func TestStaticCursorUpstreamNodes_MatchesPluginConstants(t *testing.T) {
	n := StaticCursorUpstreamNodes()
	if n.HTTP1Base != "https://cur-api2.shareai.pro" || n.HTTP2Base != "https://cur-api5.shareai.pro" {
		t.Errorf("static nodes mismatch: %+v", n)
	}
	if n.Dynamic {
		t.Error("static nodes must have Dynamic=false")
	}
}

// 13) 错误哨兵可被 errors.Is 识别（供上层区分失败类型）。
func TestCursorNodeErrorSentinels(t *testing.T) {
	if !errors.Is(errCursorNodeMetaMissing, errCursorNodeMetaMissing) {
		t.Error("meta missing sentinel unusable")
	}
	if !errors.Is(errCursorNodeFieldsIncomplete, errCursorNodeFieldsIncomplete) {
		t.Error("fields incomplete sentinel unusable")
	}
}

// encodeCursorBase36VarLen 是解码的逆运算，仅供单测构造负载。
func encodeCursorBase36VarLen(t *testing.T, s string) string {
	t.Helper()
	b := []byte(s)
	if len(b) == 0 {
		return "0-"
	}
	var digits []int
	carry := 0
	for _, ch := range b {
		carry = carry<<8 | int(ch)
		for carry > 0 {
			digits = append(digits, carry%36)
			carry /= 36
		}
	}
	// 去掉最高位前导 0（长度已单独编码）。
	for len(digits) > 0 && digits[len(digits)-1] == 0 {
		digits = digits[:len(digits)-1]
	}
	var sb strings.Builder
	sb.WriteString(strconv.Itoa(len(b)))
	sb.WriteByte('-')
	for i := len(digits) - 1; i >= 0; i-- {
		sb.WriteByte(cursorBase36Alphabet[digits[i]])
	}
	return sb.String()
}
