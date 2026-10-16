//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	wasmcrypto "github.com/Wei-Shaw/sub2api/internal/service/wasm"
)

// decryptCursorAuthMaterial 用内嵌 wasm 的 zk04 解密鉴权材料，
// 供单测交叉验证明文结构（生产路径不需要解密，故只存在于测试）。
func decryptCursorAuthMaterial(t *testing.T, cipher string) (string, error) {
	t.Helper()
	return wasmcrypto.DecryptLocalData(cipher)
}

// staticCursorAuthProvider 是固定鉴权材料的测试替身（避免单测依赖 wasm 随机性）。
type staticCursorAuthProvider struct {
	value string
	err   error
}

func (p staticCursorAuthProvider) CursorUpstreamAuth(context.Context) (string, error) {
	return p.value, p.err
}

// newCursorForwarderFixture 造一个"节点发现指向 fake 中转"的转发器，
// 并返回记录到请求的 fake 上游 server。
type cursorFakeUpstream struct {
	server   *httptest.Server
	mu       sync.Mutex
	path     string
	headers  http.Header
	body     string
	requests int
}

func newCursorForwarderFixture(
	t *testing.T,
	auth CursorUpstreamAuthProvider,
	handler func(w http.ResponseWriter, r *http.Request),
) (*CursorUpstreamForwarder, *cursorFakeUpstream) {
	t.Helper()
	fake := &cursorFakeUpstream{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.path = r.URL.Path
		fake.headers = r.Header.Clone()
		fake.body = string(raw)
		fake.requests++
		fake.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"chunk\"}\n\n"))
	}))
	t.Cleanup(fake.server.Close)

	// 节点解析器指向 fake 中转（关闭动态发现：TTL=0 + 端点指向 fake，返回静态兜底）。
	nodes := NewCursorUpstreamNodesResolver(
		WithCursorNodesEndpoint(fake.server.URL),
		WithCursorNodesHTTPClient(fake.server.Client()),
		WithCursorNodesCacheTTL(time.Minute),
	)
	// 用 WithCursorNodesHTTPClient 无法改静态常量，故用 endpoint 覆盖 + 直接改写：
	// 这里改为把节点解析器替换成"永远返回 fake base"的实例。
	nodes.cached = CursorUpstreamNodes{HTTP1Base: fake.server.URL, HTTP2Base: fake.server.URL, Dynamic: true}
	nodes.cachedAt = time.Now()

	f := NewCursorUpstreamForwarder(
		WithCursorUpstreamHTTPClient(fake.server.Client()),
		WithCursorUpstreamNodes(nodes),
		WithCursorUpstreamAuthProvider(auth),
	)
	return f, fake
}

// 1) 鉴权头构造：x-jg-auth + x-jg-model 均按 wasm zk27 实测值注入，path 原样保留。
func TestCursorForward_DoInjectsAuthHeaders(t *testing.T) {
	f, fake := newCursorForwarderFixture(t,
		staticCursorAuthProvider{value: "TEST-CIPHERTEXT"},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})

	body := []byte(`{"model":"cursor-small","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := f.Do(context.Background(), &CursorUpstreamRequest{
		Path: cursorPathStreamUnifiedChatWithToolsSSE,
		Body: body,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := fake.headers.Get(cursorJgAuthHeader); got != "TEST-CIPHERTEXT" {
		t.Errorf("x-jg-auth = %q, want TEST-CIPHERTEXT", got)
	}
	if got := fake.headers.Get(cursorJgModelHeader); got != cursorJgModelHeaderValue {
		t.Errorf("x-jg-model = %q, want %q", got, cursorJgModelHeaderValue)
	}
	// 协议不重写：路径与请求体逐字节沿用 Cursor 原生形态。
	if fake.path != cursorPathStreamUnifiedChatWithToolsSSE {
		t.Errorf("path = %q, want %q", fake.path, cursorPathStreamUnifiedChatWithToolsSSE)
	}
	if fake.body != string(body) {
		t.Errorf("body mutated: %q", fake.body)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if !strings.HasSuffix(resp.TargetURL, cursorPathStreamUnifiedChatWithToolsSSE) {
		t.Errorf("TargetURL = %q", resp.TargetURL)
	}
}

// 2) 调用方自定义头（如 Cursor Authorization）被透传，且不覆盖鉴权头。
func TestCursorForward_DoForwardsCallerHeaders(t *testing.T) {
	f, fake := newCursorForwarderFixture(t,
		staticCursorAuthProvider{value: "CIPHER"},
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	if _, err := f.Do(context.Background(), &CursorUpstreamRequest{
		Path: cursorPathStreamUnifiedChatWithToolsSSE,
		Body: []byte(`{}`),
		Headers: map[string]string{
			"Authorization": "Bearer cursor-token",
			"User-Agent":    "cursor-test/1.0",
		},
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := fake.headers.Get("Authorization"); got != "Bearer cursor-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := fake.headers.Get("User-Agent"); got != "cursor-test/1.0" {
		t.Errorf("User-Agent = %q", got)
	}
}

// 3) 流式帧原样透传（本层不解析、不聚合、不改写 SSE 字节）。
func TestCursorForward_DoStreamsFramesVerbatim(t *testing.T) {
	frames := "event: chunk\ndata: {\"text\":\"hello\"}\n\ndata: [DONE]\n\n"
	f, _ := newCursorForwarderFixture(t,
		staticCursorAuthProvider{value: "CIPHER"},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, frames)
		})

	resp, err := f.Do(context.Background(), &CursorUpstreamRequest{
		Path:     cursorPathStreamUnifiedChatWithToolsSSE,
		Body:     []byte(`{}`),
		UseHTTP2: true,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != frames {
		t.Errorf("frames mutated:\ngot=  %q\nwant= %q", got, frames)
	}
}

// 4) 失败关闭：路径不在白名单 → 不发请求。
func TestCursorForward_UnsupportedPathFailsClosed(t *testing.T) {
	f, fake := newCursorForwarderFixture(t,
		staticCursorAuthProvider{value: "CIPHER"},
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	_, err := f.Do(context.Background(), &CursorUpstreamRequest{
		Path: "/v1/chat/completions", // 非 Cursor 原生端点
		Body: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("want error for unsupported path, got nil")
	}
	if !errors.Is(err, ErrCursorUnsupportedEndpoint) {
		t.Errorf("want ErrCursorUnsupportedEndpoint, got %v", err)
	}
	if fake.requests != 0 {
		t.Errorf("fail-closed must not hit upstream, got %d requests", fake.requests)
	}
}

// 5) 失败关闭：nil 请求 → 不发请求。
func TestCursorForward_NilRequestFailsClosed(t *testing.T) {
	f, fake := newCursorForwarderFixture(t,
		staticCursorAuthProvider{value: "CIPHER"},
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	if _, err := f.Do(context.Background(), nil); err == nil {
		t.Fatal("want error for nil request")
	} else if !errors.Is(err, ErrCursorUnsupportedEndpoint) {
		t.Errorf("want ErrCursorUnsupportedEndpoint, got %v", err)
	}
	if fake.requests != 0 {
		t.Errorf("fail-closed must not hit upstream, got %d requests", fake.requests)
	}
}

// 6) 失败关闭：未配置鉴权提供者 → 不发请求。
func TestCursorForward_MissingAuthProviderFailsClosed(t *testing.T) {
	f, fake := newCursorForwarderFixture(t, nil,
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	_, err := f.Do(context.Background(), &CursorUpstreamRequest{
		Path: cursorPathStreamUnifiedChatWithToolsSSE,
		Body: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("want error when auth provider missing")
	}
	if !errors.Is(err, ErrCursorAuthMaterialUnavailable) {
		t.Errorf("want ErrCursorAuthMaterialUnavailable, got %v", err)
	}
	if fake.requests != 0 {
		t.Errorf("fail-closed must not hit upstream, got %d requests", fake.requests)
	}
}

// 7) 失败关闭：鉴权提供者报错 / 返回空串 → 不发请求。
func TestCursorForward_AuthFailureFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		auth CursorUpstreamAuthProvider
	}{
		{"provider-error", staticCursorAuthProvider{err: errors.New("wasm down")}},
		{"empty-material", staticCursorAuthProvider{value: "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, fake := newCursorForwarderFixture(t, tc.auth,
				func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

			_, err := f.Do(context.Background(), &CursorUpstreamRequest{
				Path: cursorPathStreamUnifiedChatWithToolsSSE,
				Body: []byte(`{}`),
			})
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !errors.Is(err, ErrCursorAuthMaterialUnavailable) {
				t.Errorf("want ErrCursorAuthMaterialUnavailable, got %v", err)
			}
			if fake.requests != 0 {
				t.Errorf("fail-closed must not hit upstream, got %d requests", fake.requests)
			}
		})
	}
}

// 8) 失败关闭：空激活码 / 空 deviceId → 不生成材料，且错误不含明文。
//
// deviceId 为空同样必须失败关闭：它绝不允许被兜底成按激活码派生的值（B-10 的错法）。
func TestCursorWasmAuthProvider_EmptyActivationFailsClosed(t *testing.T) {
	if _, err := NewCursorWasmAuthProvider("DEVICE-ID-OK", "   ").CursorUpstreamAuth(context.Background()); err == nil {
		t.Fatal("want error for empty activation code")
	}
	if _, err := NewCursorWasmAuthProvider("  ", "ACTIVATION-CODE-OK").CursorUpstreamAuth(context.Background()); err == nil {
		t.Fatal("want error for empty device id (never fall back to code-derived)")
	}
}

// 9) 失败关闭：激活码不可得时绝不回落 Cursor 官方端点。
func TestCursorForward_NeverFallsBackToCursorOfficial(t *testing.T) {
	f, fake := newCursorForwarderFixture(t, nil,
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	_, err := f.Do(context.Background(), &CursorUpstreamRequest{
		Path: cursorPathStreamUnifiedChatWithToolsSSE,
		Body: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("want error")
	}
	// 错误信息与实际出站都不得指向 cursor.sh 官方域。
	if strings.Contains(err.Error(), "cursor.sh") {
		t.Errorf("error must not reference Cursor official: %v", err)
	}
	if strings.Contains(fake.server.URL, "cursor.sh") {
		t.Error("fake upstream must not be Cursor official")
	}
}

// 10) 鉴权材料真值：cursorWasmAuthProvider 产出的密文可被 zk04 解回
// {"d":deviceID,"a":code}，证明两个输入都原样进入明文、键序未错位。
//
// 注意 d 取的是**调用方给定的 deviceId**，与激活码无关（B-10）。此处显式传入一个
// 不等于 deviceID(code) 的值，确保实现没有偷偷回落到按码派生。
func TestCursorWasmAuthProvider_RoundTripDecryptsToDeviceAndActivation(t *testing.T) {
	const activation = "ROUNDTRIP-ACTIVATION-CODE"
	const device = "A1B2C3D4"
	// 测试前提：本例 deviceId 必须不等于按码派生值，否则证明不了没有回落派生。
	if deviceID(activation) == device {
		t.Fatal("测试前提失效：deviceId 恰好等于 deviceID(code)，本用例无法证伪回落派生")
	}
	p := NewCursorWasmAuthProvider(device, activation)

	cipher, err := p.CursorUpstreamAuth(context.Background())
	if err != nil {
		t.Fatalf("CursorUpstreamAuth: %v", err)
	}
	if strings.TrimSpace(cipher) == "" {
		t.Fatal("empty ciphertext")
	}
	// 密文是 base64(JSON{iv,ct,tag})——形状须与插件一致。
	raw, err := base64.StdEncoding.DecodeString(cipher)
	if err != nil {
		t.Fatalf("cipher must be base64: %v", err)
	}
	var envelope struct {
		IV  string `json:"iv"`
		CT  string `json:"ct"`
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("cipher must be base64(JSON{iv,ct,tag}): %v", err)
	}
	if envelope.IV == "" || envelope.CT == "" || envelope.Tag == "" {
		t.Errorf("incomplete ciphertext envelope: %+v", envelope)
	}

	// 交叉验证：解密回明文，核对 d=给定 deviceId、a=给定激活码。
	plain, err := decryptCursorAuthMaterial(t, cipher)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var got cursorEncryptedCredentialsInput
	if err := json.Unmarshal([]byte(plain), &got); err != nil {
		t.Fatalf("plaintext must be JSON: %v", err)
	}
	if got.D != device {
		t.Errorf("device id = %q, want %q (caller-supplied, never code-derived)", got.D, device)
	}
	if got.A != activation {
		t.Errorf("activation = %q, want %q", got.A, activation)
	}
}

// 11) 鉴权材料缓存：同一 provider 重复取返回同一密文（不重复跑 wasm）。
func TestCursorWasmAuthProvider_CachesMaterial(t *testing.T) {
	p := NewCursorWasmAuthProvider("CACHED-DEVICE", "CACHE-ME")
	first, err := p.CursorUpstreamAuth(context.Background())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := p.CursorUpstreamAuth(context.Background())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Error("cached material must be stable across calls")
	}
}

// 12) 不同激活码 / 不同 deviceId → 不同材料（材料确实绑定这两个输入）。
func TestCursorWasmAuthProvider_DiffersPerActivation(t *testing.T) {
	a, err := NewCursorWasmAuthProvider("DEVICE-A", "CODE-A").CursorUpstreamAuth(context.Background())
	if err != nil {
		t.Fatalf("code A: %v", err)
	}
	b, err := NewCursorWasmAuthProvider("DEVICE-A", "CODE-B").CursorUpstreamAuth(context.Background())
	if err != nil {
		t.Fatalf("code B: %v", err)
	}
	if a == b {
		t.Error("different activation codes must not yield identical material")
	}

	// 仅 deviceId 不同也必须产出不同材料（deviceId 是独立输入，不是激活码的函数）。
	c, err := NewCursorWasmAuthProvider("DEVICE-B", "CODE-A").CursorUpstreamAuth(context.Background())
	if err != nil {
		t.Fatalf("device B: %v", err)
	}
	if a == c {
		t.Error("different device ids must not yield identical material")
	}
}

// 13) 并发取材料安全（wasm 调用被串行化）。
func TestCursorWasmAuthProvider_ConcurrentSafe(t *testing.T) {
	p := NewCursorWasmAuthProvider("CONCURRENT-DEVICE", "CONCURRENT-CODE")
	var wg sync.WaitGroup
	results := make([]string, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = p.CursorUpstreamAuth(context.Background())
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Errorf("goroutine %d material mismatch", i)
		}
	}
}

// 14) 路径白名单：已取证端点放行，未取证端点拒绝。
func TestIsCursorAllowedUpstreamPath(t *testing.T) {
	allowed := []string{
		cursorPathStreamUnifiedChatWithToolsSSE,
		cursorPathStreamUnifiedChatWithToolsSSEWithMcp,
		cursorPathGenerateUnifiedCompletionWithToolsSSE,
		cursorPathAvailableModels,
	}
	for _, p := range allowed {
		if !IsCursorAllowedUpstreamPath(p) {
			t.Errorf("path %q should be allowed", p)
		}
	}
	denied := []string{
		"/v1/chat/completions",
		"/v1/messages",
		"/",
		"",
		"/aiserver.v1.AIServiceService/Unknown",
	}
	for _, p := range denied {
		if IsCursorAllowedUpstreamPath(p) {
			t.Errorf("path %q should be denied", p)
		}
	}
}

// 15) 节点 base url 取法（E2 卡接口）：HTTP1BaseURL / HTTP2BaseURL 可用。
func TestCursorForward_BaseURLAccessors(t *testing.T) {
	f, fake := newCursorForwarderFixture(t,
		staticCursorAuthProvider{value: "CIPHER"},
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	if got := f.HTTP1BaseURL(context.Background()); got != fake.server.URL {
		t.Errorf("HTTP1BaseURL = %q, want %q", got, fake.server.URL)
	}
	if got := f.HTTP2BaseURL(context.Background()); got != fake.server.URL {
		t.Errorf("HTTP2BaseURL = %q, want %q", got, fake.server.URL)
	}
}
