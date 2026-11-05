//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		Path: cursorPathAvailableModels,
		Body: body,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := fake.headers.Get(cursorJgAuthHeader); got != "TEST-CIPHERTEXT" {
		t.Errorf("x-jg-auth = %q, want TEST-CIPHERTEXT", got)
	}
	if got := fake.headers.Get(cursorJgModelHeader); got != "" {
		t.Errorf("Standard request must omit x-jg-model, got %q", got)
	}
	// 协议不重写：路径与请求体逐字节沿用 Cursor 原生形态。
	if fake.path != cursorPathAvailableModels {
		t.Errorf("path = %q, want %q", fake.path, cursorPathAvailableModels)
	}
	if fake.body != string(body) {
		t.Errorf("body mutated: %q", fake.body)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if !strings.HasSuffix(resp.TargetURL, cursorPathAvailableModels) {
		t.Errorf("TargetURL = %q", resp.TargetURL)
	}
}

// 2) 调用方自定义头（如 Cursor Authorization）被透传，且不覆盖鉴权头。
func TestCursorForward_DoForwardsCallerHeaders(t *testing.T) {
	f, fake := newCursorForwarderFixture(t,
		staticCursorAuthProvider{value: "CIPHER"},
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	if _, err := f.Do(context.Background(), &CursorUpstreamRequest{
		Path: cursorPathAvailableModels,
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
		Path:     cursorPathAvailableModels,
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
		Path: cursorPathAvailableModels,
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
				Path: cursorPathAvailableModels,
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
		Path: cursorPathAvailableModels,
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

// 14) 路径白名单：已取证端点放行，未取证端点拒绝（含 B-1 纠错后已删除的三条）。
func TestCursorUpstreamForwardAllowedPathWhitelist(t *testing.T) {
	allowed := []string{
		cursorPathAvailableModels,
		cursorPathAgentRun,
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
		// P1-A1 换轨：ChatService 的两条对话路径不再是出站路径，必须拒绝。
		"/aiserver.v1.ChatService/StreamUnifiedChatWithTools",
		"/aiserver.v1.ChatService/StreamUnifiedChatWithToolsSSE",
		// P1-A2 旧链已删除（合同 §4）：AuthService 的换 token / 校验 token 两入口
		// 不再有任何客户端，白名单也必须保持拒绝——放行它们等于给旧链留后门。
		"/aiserver.v1.AuthService/GetSessionToken",
		"/aiserver.v1.AuthService/CheckSessionToken",
		// B-1 纠错：以下三条在 Cursor 3.24.12 全量资产中不存在（静态 0 命中），
		// 放行它们等于把任意路径打到中转节点，必须保持拒绝。
		"/aiserver.v1.AIServiceService/StreamUnifiedChatWithToolsSSE",
		"/aiserver.v1.AIServiceService/StreamUnifiedChatWithToolsSSEWithMcp",
		"/aiserver.v1.AIServiceService/GenerateUnifiedCompletionWithToolsSSE",
		"/aiserver.v1.AIServiceService/GetAvailableModels",
		// 服务名对但方法不存在（B-1 静态裁定）。
		"/aiserver.v1.ChatService/AvailableModels",
	}
	for _, p := range denied {
		if IsCursorAllowedUpstreamPath(p) {
			t.Errorf("path %q should be denied", p)
		}
	}
}

// 15) 取消传播：请求上下文被取消时，出站必须立刻失败，绝不当成功返回。
//
// 取消的请求拿不到终态标记（turn_ended / EndStream），若把它当成功就会回写一条
// 0 usage 的"成功"响应并误导计费，故本层必须把 ctx 错误原样抛出。
func TestCursorUpstreamForwardCanceledContextFailsClosed(t *testing.T) {
	f, _ := newCursorForwarderFixture(t,
		staticCursorAuthProvider{value: "CIPHER"},
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 发出前即取消

	resp, err := f.Do(ctx, &CursorUpstreamRequest{
		Path: cursorPathAvailableModels,
		Body: []byte(`{}`),
	})
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("want error for canceled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

// 16) 节点 base url 取法（E2 卡接口）：HTTP1BaseURL / HTTP2BaseURL 可用。
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

// ---------------------------------------------------------------------------
// UseAutoDirect（CARD-3）：官方直连双工流 + 重定向安全防御
//
// 直连模式由唯一转发器边界分流：锁定官方端点 api2.cursor.sh、拒绝所有 3xx 重定向、
// 绕过 shareai 鉴权，同时完整保留 AgentRun 双工流与 429 换号自愈。
// 测试无法触达真实 api2.cursor.sh，故用 TLS fake 官方节点 + WithCursorUpstreamAutoDirectHost
// 把锁定 host 指向 fake（受信配置覆盖，非请求注入），验证三件事：
//   直连官方 / 拒绝重定向 / 保留双工流。
// ---------------------------------------------------------------------------

// cursorAutoDirectFixture 造一个"官方直连指向 TLS fake 官方节点"的转发器。
// 返回的 host 即 fake 节点的 host（已通过 WithCursorUpstreamAutoDirectHost 锁进转发器），
// 断言阶段只校验它等于配置值、scheme=https，不依赖真实网络。
func newCursorAutoDirectFixture(
	t *testing.T,
	handler func(w http.ResponseWriter, r *http.Request),
) (*CursorUpstreamForwarder, *cursorFakeUpstream, string) {
	t.Helper()
	fake := &cursorFakeUpstream{}
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(tlsSrv.Close)

	u, err := url.Parse(tlsSrv.URL)
	if err != nil {
		t.Fatalf("parse tls server url: %v", err)
	}
	f := NewCursorUpstreamForwarder(
		WithCursorUpstreamHTTPClient(tlsSrv.Client()),
		WithCursorUpstreamAutoDirectHost(u.Host),
	)
	return f, fake, u.Host
}

// cursorEndStreamFrame 手工构造带 EndStream 标志的信封帧（cursorEncodeEnvelope 恒用 flags=0，
// 故此处直接写 header 的 flags 字节）。
func cursorEndStreamFrame(payload []byte) []byte {
	out := make([]byte, cursorEnvelopeHeaderSize+len(payload))
	out[0] = cursorEnvelopeFlagEndStream
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// 17) 直连官方：请求落到锁定 host、路径/请求体原样保留，且**不**注入 x-jg-auth。
func TestCursorForward_AutoDirectDirectsToOfficialAndBypassesShareAI(t *testing.T) {
	f, fake, host := newCursorAutoDirectFixture(t, nil)

	body := []byte(`{"model":"cursor-small","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := f.Do(context.Background(), &CursorUpstreamRequest{
		Path:          cursorPathAvailableModels,
		Body:          body,
		UseAutoDirect: true,
	})
	if err != nil {
		t.Fatalf("Do(auto-direct): %v", err)
	}
	defer resp.Body.Close()

	if fake.requests != 1 {
		t.Errorf("want exactly 1 upstream hit, got %d", fake.requests)
	}
	// 锁定 host：出站目标必须指向配置好的官方节点 host，而非 shareai 中转。
	if fake.headers == nil || fake.headers.Get("Host") != host {
		t.Errorf("request host = %q, want locked official host %q", hostOf(fake), host)
	}
	// 协议不重写：路径与请求体逐字节沿用 Cursor 原生形态。
	if fake.path != cursorPathAvailableModels {
		t.Errorf("path = %q, want %q", fake.path, cursorPathAvailableModels)
	}
	if fake.body != string(body) {
		t.Errorf("body mutated: %q", fake.body)
	}
	// 关键红线：直连模式不得注入 shareai 的 x-jg-auth。
	if got := fake.headers.Get(cursorJgAuthHeader); got != "" {
		t.Errorf("auto-direct must NOT inject x-jg-auth, got %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.HasSuffix(resp.TargetURL, cursorPathAvailableModels) {
		t.Errorf("TargetURL = %q", resp.TargetURL)
	}
}

// 18) 重定向防御：上游返回 302 时，客户端必须**拒绝**跟随（CheckRedirect=ErrUseLastResponse），
// 既不二次请求、也绝不把 Bearer 凭据带到重定向目标。上层据此拿到 429 同类状态自行换号。
func TestCursorForward_AutoDirectRejectsRedirect(t *testing.T) {
	f, fake, _ := newCursorAutoDirectFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/somewhere-else")
		w.WriteHeader(http.StatusFound)
	})

	resp, err := f.Do(context.Background(), &CursorUpstreamRequest{
		Path:          cursorPathAvailableModels,
		Body:          []byte(`{}`),
		UseAutoDirect: true,
	})
	if err != nil {
		t.Fatalf("Do(auto-direct): %v", err)
	}
	defer resp.Body.Close()

	// 拒绝跟随：只发一次请求，直接把 3xx 状态回给上层。
	if fake.requests != 1 {
		t.Errorf("redirect must NOT be followed, want 1 request, got %d", fake.requests)
	}
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want %d (redirect returned as-is)", resp.StatusCode, http.StatusFound)
	}
	// 同样不得注入 x-jg-auth（重定向场景下泄露风险最高）。
	if got := fake.headers.Get(cursorJgAuthHeader); got != "" {
		t.Errorf("auto-direct must NOT inject x-jg-auth, got %q", got)
	}
}

// 19) 保留双工流：AgentRun 路径下直连模式仍创建并接回 cursorAgentDuplex，
// 请求体经双工 pipe 完整送达官方节点，且响应体接管成功。
func TestCursorForward_AutoDirectPreservesDuplex(t *testing.T) {
	f, fake, _ := newCursorAutoDirectFixture(t, func(w http.ResponseWriter, r *http.Request) {
		// 读取经双工 pipe 送来的请求体。
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", cursorConnectContentType)
		// 回一个 EndStream 终态帧，双工读循环应当干净收尾。
		_, _ = w.Write(cursorEndStreamFrame([]byte(`{"type":"end"}`)))
	})

	body := cursorEncodeEnvelope(cursorAppendBytesField(nil, cursorFldAgentRunRequest, nil))
	resp, err := f.Do(context.Background(), &CursorUpstreamRequest{
		Path:          cursorPathAgentRun,
		Body:          body,
		UseAutoDirect: true,
	})
	if err != nil {
		t.Fatalf("Do(auto-direct agent run): %v", err)
	}
	defer resp.Body.Close()

	// 双工流被创建并随响应接回。
	if resp.Duplex == nil {
		t.Fatal("auto-direct AgentRun must preserve duplex (resp.Duplex == nil)")
	}
	// 请求体经双工 pipe 完整送达官方节点（证明双工 reader 已接为请求体）。
	if fake.body != string(body) {
		t.Errorf("duplex-fed request body = %q, want %q", fake.body, string(body))
	}
	// 响应体接管成功：读出 EndStream 帧，不应报错。
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read auto-direct response body: %v", err)
	}
	if !bytes.Equal(got, cursorEndStreamFrame([]byte(`{"type":"end"}`))) {
		t.Errorf("response body not passed through duplex: %q", got)
	}
}

// hostOf 从 fake 记录里取 Host 头（便捷/诊断用）。
func hostOf(fake *cursorFakeUpstream) string {
	if fake.headers == nil {
		return ""
	}
	return fake.headers.Get("Host")
}
