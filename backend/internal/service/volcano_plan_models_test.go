package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func volcanoTestConfig() *config.Config {
	return &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}
}

// volcanoFixtureBase 是火山官方文档夹具目录。
const volcanoFixtureBase = "testdata/volcano"

// loadVolcanoFixture 读取测试夹具文件（getDocDetail 响应体）。
func loadVolcanoFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(volcanoFixtureBase, name))
	require.NoError(t, err)
	return b
}

// newVolcanoDocFixtureServer 启动一个伪造 docs.volcengine.com 文档 API 的 httptest 服务，
// 用真实抓取的 getDocList/getDocDetail 夹具响应。返回 (server, cleanup)。
// 将 svc.volcanoDocClient/server.URL 注入即可让文档读取走夹具（绕过主机守卫）。
func newVolcanoDocFixtureServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	doclist := loadVolcanoFixture(t, "doclist_82379.json")
	// DocumentID → 夹具文件名 的固定映射（与真实套餐概览一致）。
	docs := map[string]string{
		"2366394": "agent_personal_2366394.json",
		"2374452": "agent_enterprise_2374452.json",
		"1925114": "coding_personal_1925114.json",
		"2276791": "coding_enterprise_2276791.json",
	}
	loaded := map[string][]byte{}
	for id, file := range docs {
		loaded[id] = loadVolcanoFixture(t, file)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(volcanoDocGetListPath, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(doclist)
	})
	mux.HandleFunc(volcanoDocGetDetailPath, func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("DocumentID")
		body, ok := loaded[id]
		if !ok {
			http.Error(w, fmt.Sprintf("unknown document %s", id), http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	})
	server := httptest.NewServer(mux)
	return server, func() { server.Close() }
}

func volcanoTestAccount(baseURL, protocol string) *Account {
	return &Account{
		ID:       29,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "ark-key",
			"base_url":     baseURL,
			"api_protocol": protocol,
		},
	}
}

// volcanoSyncHTTPStub 捕获 DoWithTLS 调用（代理/TLS 复用证据）并按请求体 model 分发响应。
type volcanoSyncHTTPStub struct {
	mu          sync.Mutex
	respond     func(req *http.Request, model string) (int, string, error)
	proxyURLs   []string
	lastProfile *tlsfingerprint.Profile
	calledDo    bool
}

func (u *volcanoSyncHTTPStub) Do(req *http.Request, proxyURL string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calledDo = true
	u.mu.Unlock()
	return u.handle(req)
}

func (u *volcanoSyncHTTPStub) DoWithTLS(req *http.Request, proxyURL string, _ int64, _ int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.mu.Lock()
	u.proxyURLs = append(u.proxyURLs, proxyURL)
	u.lastProfile = profile
	u.mu.Unlock()
	return u.handle(req)
}

func (u *volcanoSyncHTTPStub) handle(req *http.Request) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	model := ""
	if body, err := readRequestBody(req); err == nil {
		model = body
	}
	status, respBody, err := u.respond(req, model)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: status,
		Body:       io_NopCloser(strings.NewReader(respBody)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

type nopCloserReader struct{ *strings.Reader }

func (nopCloserReader) Close() error { return nil }

// io_NopCloser 模拟 io.NopCloser 以避免与内部包命名冲突；仅用于测试桩。
func io_NopCloser(r *strings.Reader) *nopCloserReader { return &nopCloserReader{r} }

func readRequestBody(req *http.Request) (string, error) {
	if req == nil || req.Body == nil {
		return "", nil
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		return "", err
	}
	return payload.Model, nil
}

// newVolcanoSyncService 构造绑定了官方文档夹具与探测桩的服务。文档客户端与基址经
// t.Cleanup 自动关闭。既有探活/同步测试调用 fetchVolcanoPlanSupportedModels 时，候选
// 直接来自真实抓取的官方文档夹具，而不是静态 JSON。
func newVolcanoSyncService(t *testing.T, stub *volcanoSyncHTTPStub) *AccountTestService {
	t.Helper()
	svc := &AccountTestService{
		cfg:          volcanoTestConfig(),
		httpUpstream: stub,
	}
	server, cleanup := newVolcanoDocFixtureServer(t)
	svc.volcanoDocClient = server.Client()
	svc.volcanoDocBaseURL = server.URL
	t.Cleanup(cleanup)
	return svc
}

// TestParseVolcanoPlanProfileRecognizesAgentAndCoding 覆盖 Agent/Coding Plan 自动识别
// 与普通 deepseek 不误判（net/url 主机判定，非字符串 Contains）。
func TestParseVolcanoPlanProfileRecognizesAgentAndCoding(t *testing.T) {
	t.Parallel()

	agent, ok := parseVolcanoPlanProfile("https://ark.cn-beijing.volces.com/api/plan")
	require.True(t, ok)
	require.Equal(t, volcanoPlanKindAgent, agent.Kind)
	require.Equal(t, "/api/plan", agent.Path)
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/plan", agent.BaseURL)
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/plan/v1/messages", agent.anthropicMessagesURL())
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions", agent.openAIChatCompletionsURL())

	coding, ok := parseVolcanoPlanProfile("https://ark.cn-beijing.volces.com/api/coding")
	require.True(t, ok)
	require.Equal(t, volcanoPlanKindCoding, coding.Kind)
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/coding/v1/messages", coding.anthropicMessagesURL())
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions", coding.openAIChatCompletionsURL())

	// 明文 http 一律不命中（探活请求会携带账号 API Key，绝不走非 TLS）。
	require.False(t, mustParseVolcano(t, "http://ark.cn-beijing.volces.com/api/plan"))

	// 普通 deepseek / openai / 未知 host 一律不命中。
	require.False(t, mustParseVolcano(t, "https://api.deepseek.com"))
	require.False(t, mustParseVolcano(t, "https://api.openai.com/v1"))
	require.False(t, mustParseVolcano(t, "https://ark.cn-beijing.volces.com"))               // 缺套餐 path
	require.False(t, mustParseVolcano(t, "https://ark.cn-beijing.volces.com/api/other"))      // 非套餐 path
	require.False(t, mustParseVolcano(t, "https://evil-ark.cn-beijing.volces.com/api/plan"))  // 相似子串主机
	require.False(t, mustParseVolcano(t, ""))
	require.False(t, mustParseVolcano(t, "not a url"))
}

func mustParseVolcano(t *testing.T, raw string) bool {
	t.Helper()
	_, ok := parseVolcanoPlanProfile(raw)
	return ok
}

// TestFetchVolcanoPlanSupportedModelsProbesAnthropicEndpoint 验证 Anthropic 协议账号
// 探测打到 {base}/v1/messages，鉴权用 x-api-key、带 anthropic-version，且复用 DoWithTLS。
func TestFetchVolcanoPlanSupportedModelsProbesAnthropicEndpoint(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	endpoints := map[string]int{} // url -> count
	stub := &volcanoSyncHTTPStub{
		respond: func(req *http.Request, model string) (int, string, error) {
			mu.Lock()
			endpoints[req.URL.String()]++
			mu.Unlock()
			return 200, `{"content":[{"type":"text","text":"hi"}]}`, nil
		},
	}
	svc := newVolcanoSyncService(t, stub)

	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/plan", "anthropic")
	models, warnings, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.NotEmpty(t, models)
	for url := range endpoints {
		require.True(t, strings.Contains(url, "/api/plan/v1/messages"), url)
		require.NotContains(t, url, "deepseek.com")
		require.NotContains(t, url, "openai.com")
	}
	// 每条探测都走 DoWithTLS（非 Do），复用共享 TLS 通道。
	require.False(t, stub.calledDo)
	require.True(t, len(stub.proxyURLs) > 0)
}

// TestFetchVolcanoPlanSupportedModelsProbesOpenAIEndpoint 验证 OpenAI 协议账号探测
// 打到 {base}/v3/chat/completions，Bearer 鉴权。
func TestFetchVolcanoPlanSupportedModelsProbesOpenAIEndpoint(t *testing.T) {
	t.Parallel()

	stub := &volcanoSyncHTTPStub{
		respond: func(req *http.Request, model string) (int, string, error) {
			require.Equal(t, "https://ark.cn-beijing.volces.com/api/coding/v3/chat/completions", req.URL.String())
			require.Equal(t, "Bearer ark-key", req.Header.Get("Authorization"))
			return 200, `{"choices":[{"message":{"content":"hi"}}]}`, nil
		},
	}
	svc := newVolcanoSyncService(t, stub)

	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions")
	models, _, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.NoError(t, err)
	require.NotEmpty(t, models)
	require.NotContains(t, models, "auto")
}

// TestFetchVolcanoPlanSupportedModelsPreservesWhitelistAndReprobesExisting 验证全量
// 探活语义：白名单已收录的候选仍重新探活（确认当前真实可用性），且既有白名单模型
// 保留、不删减。
func TestFetchVolcanoPlanSupportedModelsPreservesWhitelistAndReprobesExisting(t *testing.T) {
	t.Parallel()

	var probedMu sync.Mutex
	probed := map[string]bool{}
	statusByModel := map[string]int{"glm-5.3": 200, "kimi-k2.7-code": 404}
	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, model string) (int, string, error) {
			probedMu.Lock()
			probed[model] = true
			probedMu.Unlock()
			status, ok := statusByModel[model]
			if !ok {
				return 200, `{"content":[{"type":"text","text":"hi"}]}`, nil
			}
			return status, `{"error":"x"}`, nil
		},
	}
	svc := newVolcanoSyncService(t, stub)

	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/coding", "anthropic")
	// 白名单已含 glm-5.3：全量语义下仍重新探活。
	account.Credentials["model_mapping"] = map[string]any{"glm-5.3": "glm-5.3"}

	models, _, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.NoError(t, err)

	probedMu.Lock()
	defer probedMu.Unlock()
	// glm-5.3 已收录但仍重新探活（200 确认可用）→ 保留在结果；kimi-k2.7-code 探测 404
	// → 不可用不入列。
	require.True(t, probed["glm-5.3"], "glm-5.3 whitelisted but must still be re-probed")
	require.True(t, probed["kimi-k2.7-code"], "kimi-k2.7-code must be probed")
	require.Contains(t, models, "glm-5.3")
	require.NotContains(t, models, "kimi-k2.7-code")
}

// 全量语义：候选名即使等于某条已有 mapping 的上游目标值（alias -> 火山模型 v），
// 仍重新探活以确认当前真实可用性（不再按 mapping value 跳过）。
func TestFetchVolcanoPlanSupportedModelsReprobesMappingValueCandidate(t *testing.T) {
	t.Parallel()

	var probedMu sync.Mutex
	probed := map[string]bool{}
	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, model string) (int, string, error) {
			probedMu.Lock()
			probed[model] = true
			probedMu.Unlock()
			return 200, `{"content":[{"type":"text","text":"hi"}]}`, nil
		},
	}
	svc := newVolcanoSyncService(t, stub)

	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/coding", "anthropic")
	// alias -> glm-5.3：glm-5.3 只是 mapping 目标值，不是白名单 key；全量语义下仍重新探活。
	account.Credentials["model_mapping"] = map[string]any{"my-alias": "glm-5.3"}

	models, _, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.NoError(t, err)

	probedMu.Lock()
	defer probedMu.Unlock()
	require.True(t, probed["glm-5.3"], "glm-5.3 is an existing mapping value but must still be re-probed")
	require.True(t, probed["kimi-k2.7-code"], "kimi-k2.7-code (not mapped) must be probed")
	require.Contains(t, models, "glm-5.3", "探活确认的 glm-5.3 应入列")
}

// TestFetchVolcanoPlanSupportedModelsCredentialError 验证 401/403 终止并返回凭证错误。
func TestFetchVolcanoPlanSupportedModelsCredentialError(t *testing.T) {
	t.Parallel()

	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, _ string) (int, string, error) {
			return 401, `{"error":"invalid api key"}`, nil
		},
	}
	svc := newVolcanoSyncService(t, stub)

	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/plan", "anthropic")
	_, _, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.Error(t, err)
	var syncErr *UpstreamModelSyncError
	require.ErrorAs(t, err, &syncErr)
	require.Equal(t, UpstreamModelSyncErrorCredential, syncErr.Kind)
	require.Equal(t, http.StatusUnauthorized, syncErr.StatusCode)
}

// TestFetchVolcanoPlanSupportedModelsAllCandidatesFail 验证所有候选均失败时返回明确错误。
func TestFetchVolcanoPlanSupportedModelsAllCandidatesFail(t *testing.T) {
	t.Parallel()

	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, _ string) (int, string, error) {
			return 404, `{"error":"model not found"}`, nil
		},
	}
	svc := newVolcanoSyncService(t, stub)

	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/plan", "anthropic")
	_, _, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.Error(t, err)
	var syncErr *UpstreamModelSyncError
	require.ErrorAs(t, err, &syncErr)
	require.Equal(t, UpstreamModelSyncErrorUpstream, syncErr.Kind)
}

// TestFetchVolcanoPlanSupportedModelsPartialTransientWarning 验证 429/5xx/timeout
// 返回部分同步 warning，且成功模型仍列入。
func TestFetchVolcanoPlanSupportedModelsPartialTransientWarning(t *testing.T) {
	t.Parallel()

	scenarios := map[string]func(model string) (int, string, error){
		"rate_limited": func(_ string) (int, string, error) { return 429, `{"error":"rate limit"}`, nil },
		"server_err":   func(_ string) (int, string, error) { return 500, `{"error":"boom"}`, nil },
		"timeout":      func(_ string) (int, string, error) { return 0, "", errors.New("dial tcp: i/o timeout") },
	}
	for name, respond := range scenarios {
		name := name
		respond := respond
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// 至少一个候选成功 200，其余转瞬失败 → 部分成功 + warning。
			first := true
			stub := &volcanoSyncHTTPStub{
				respond: func(_ *http.Request, model string) (int, string, error) {
					_ = model
					if first {
						first = false
						return 200, `{"content":[{"type":"text","text":"hi"}]}`, nil
					}
					return respond(model)
				},
			}
			svc := newVolcanoSyncService(t, stub)
			account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/plan", "anthropic")
			models, warnings, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
			require.NoError(t, err)
			require.NotEmpty(t, models)
			require.Len(t, warnings, 1)
			require.Equal(t, VolcanoModelSyncPartialCode, warnings[0].Code)
		})
	}
}

// TestFetchVolcanoPlanSupportedModelsAllTransientFails 验证全部候选均为转瞬失败时
// 返回明确错误而非静默当作模型不存在。
func TestFetchVolcanoPlanSupportedModelsAllTransientFails(t *testing.T) {
	t.Parallel()

	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, _ string) (int, string, error) {
			return 0, "", errors.New("context deadline exceeded")
		},
	}
	svc := newVolcanoSyncService(t, stub)
	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/plan", "anthropic")
	_, _, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.Error(t, err)
	var syncErr *UpstreamModelSyncError
	require.ErrorAs(t, err, &syncErr)
	require.Equal(t, UpstreamModelSyncErrorUpstream, syncErr.Kind)
}

// TestFetchVolcanoPlanSupportedModelsRoutesThroughProxy 验证探活复用账号代理配置
// （proxyURL 透传给共享 HTTP 通道）。
func TestFetchVolcanoPlanSupportedModelsRoutesThroughProxy(t *testing.T) {
	t.Parallel()

	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, _ string) (int, string, error) {
			return 200, `{"content":[{"type":"text","text":"hi"}]}`, nil
		},
	}
	svc := newVolcanoSyncService(t, stub)

	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/plan", "anthropic")
	pid := int64(7)
	account.ProxyID = &pid
	account.Proxy = &Proxy{ID: 7, Protocol: "http", Host: "127.0.0.1", Port: 8888}
	account.Concurrency = 2

	_, _, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.NoError(t, err)
	stub.mu.Lock()
	defer stub.mu.Unlock()
	require.NotEmpty(t, stub.proxyURLs)
	for _, proxyURL := range stub.proxyURLs {
		require.Equal(t, "http://127.0.0.1:8888", proxyURL)
	}
}


func TestFetchUpstreamModelListVolcanoSkipsModelsDirectory(t *testing.T) {
	t.Parallel()

	var calledHTTP bool
	stub := &volcanoSyncHTTPStub{
		respond: func(req *http.Request, _ string) (int, string, error) {
			calledHTTP = true
			// 必须命中对话端点而非 /models 目录。
			require.True(t, strings.Contains(req.URL.Path, "/v1/messages") ||
				strings.Contains(req.URL.Path, "/v3/chat/completions"), req.URL.String())
			require.NotContains(t, req.URL.Path, "/models")
			return 200, `{"content":[{"type":"text","text":"hi"}]}`, nil
		},
	}
	svc := newVolcanoSyncService(t, stub)
	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/coding", "anthropic")
	models, body, _, err := svc.fetchUpstreamModelList(context.Background(), account)
	require.NoError(t, err)
	require.NotEmpty(t, models)
	require.Nil(t, body)
	require.True(t, calledHTTP)
}

