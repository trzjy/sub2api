package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
		Volcano: config.VolcanoConfig{
			PlanModelsFile: "../../resources/volcano-plan-models.json",
		},
	}
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

func newVolcanoSyncService(stub *volcanoSyncHTTPStub) *AccountTestService {
	return &AccountTestService{
		cfg:          volcanoTestConfig(),
		httpUpstream: stub,
	}
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
	svc := newVolcanoSyncService(stub)

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
	svc := newVolcanoSyncService(stub)

	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/coding", "chat_completions")
	models, _, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.NoError(t, err)
	require.NotEmpty(t, models)
	require.NotContains(t, models, "auto")
}

// TestFetchVolcanoPlanSupportedModelsPreservesWhitelistAndSkipsExisting 验证只探测
// model_mapping 中不存在的候选（增量去重），且既有白名单模型保留、不删减。
func TestFetchVolcanoPlanSupportedModelsPreservesWhitelistAndSkipsExisting(t *testing.T) {
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
	svc := newVolcanoSyncService(stub)

	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/coding", "anthropic")
	// 白名单已含 glm-5.3：不再探测它。
	account.Credentials["model_mapping"] = map[string]any{"glm-5.3": "glm-5.3"}

	models, _, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.NoError(t, err)

	probedMu.Lock()
	defer probedMu.Unlock()
	// glm-5.3 已存在 → 不探测但保留在结果；kimi-k2.7-code 探测 404 → 不可用不入列。
	require.False(t, probed["glm-5.3"], "glm-5.3 already whitelisted, must not be re-probed")
	require.True(t, probed["kimi-k2.7-code"], "kimi-k2.7-code must be probed")
	require.Contains(t, models, "glm-5.3")
	require.NotContains(t, models, "kimi-k2.7-code")
}

// 增量语义：候选名若等于某条已有 mapping 的上游目标值（alias -> 火山模型 v），
// 说明 v 已在账号能力内，不应重复探活（比较 mapping value，而非仅 key）。
func TestFetchVolcanoPlanSupportedModelsSkipsMappingValueCandidate(t *testing.T) {
	t.Parallel()

	var probedMu sync.Mutex
	probed := map[string]bool{}
	// 若 glm-5.3 被误探活，返回 404，应能从断言暴露。
	statusByModel := map[string]int{"glm-5.3": 404, "kimi-k2.7-code": 200}
	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, model string) (int, string, error) {
			probedMu.Lock()
			probed[model] = true
			probedMu.Unlock()
			status, ok := statusByModel[model]
			if !ok {
				return 200, `{"content":[{"type":"text","text":"hi"}]}`, nil
			}
			if status != 200 {
				return status, `{"error":"x"}`, nil
			}
			return 200, `{"content":[{"type":"text","text":"hi"}]}`, nil
		},
	}
	svc := newVolcanoSyncService(stub)

	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/coding", "anthropic")
	// alias -> glm-5.3：glm-5.3 只是 mapping 目标值，不是白名单 key，但仍应被认作已收录。
	account.Credentials["model_mapping"] = map[string]any{"my-alias": "glm-5.3"}

	_, _, err := svc.fetchVolcanoPlanSupportedModels(context.Background(), account)
	require.NoError(t, err)

	probedMu.Lock()
	defer probedMu.Unlock()
	require.False(t, probed["glm-5.3"], "glm-5.3 is an existing mapping value, must not be re-probed")
	require.True(t, probed["kimi-k2.7-code"], "kimi-k2.7-code (not mapped) must be probed")
}

// TestFetchVolcanoPlanSupportedModelsCredentialError 验证 401/403 终止并返回凭证错误。
func TestFetchVolcanoPlanSupportedModelsCredentialError(t *testing.T) {
	t.Parallel()

	stub := &volcanoSyncHTTPStub{
		respond: func(_ *http.Request, _ string) (int, string, error) {
			return 401, `{"error":"invalid api key"}`, nil
		},
	}
	svc := newVolcanoSyncService(stub)

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
	svc := newVolcanoSyncService(stub)

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
			svc := newVolcanoSyncService(stub)
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
	svc := newVolcanoSyncService(stub)
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
	svc := newVolcanoSyncService(stub)

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

// TestLoadVolcanoPlanCandidatesSkipsAuto 验证候选列表过滤空名与 auto，按套餐区分。
func TestLoadVolcanoPlanCandidatesSkipsAuto(t *testing.T) {
	t.Parallel()

	agent, err := loadVolcanoPlanCandidates("../../resources/volcano-plan-models.json", volcanoPlanKindAgent)
	require.NoError(t, err)
	require.Contains(t, agent, "deepseek-v4-pro")
	require.NotContains(t, agent, "auto")

	coding, err := loadVolcanoPlanCandidates("../../resources/volcano-plan-models.json", volcanoPlanKindCoding)
	require.NoError(t, err)
	require.Contains(t, coding, "glm-5.3")
	require.NotContains(t, coding, "auto")
	require.NotContains(t, coding, "doubao-seed-2.0-pro") // agent 专属，不应出现在 coding

	// 套餐缺失 → 失败关闭。
	_, err = loadVolcanoPlanCandidates("../../resources/volcano-plan-models.json", "bogus")
	require.Error(t, err)
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
	svc := newVolcanoSyncService(stub)
	account := volcanoTestAccount("https://ark.cn-beijing.volces.com/api/coding", "anthropic")
	models, body, _, err := svc.fetchUpstreamModelList(context.Background(), account)
	require.NoError(t, err)
	require.NotEmpty(t, models)
	require.Nil(t, body)
	require.True(t, calledHTTP)
}

