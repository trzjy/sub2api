package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 覆盖测试（缺陷4）：输出预算护栏必须挂在**每一个**生成类网关链上。
//
// 为什么是真实路由树而不是源码字符串断言：事故形态是客户端哨兵值
// max_tokens=999999999 把上游顶成 400 → 网关 502（24h 603 条）。护栏若只挂在
// 部分链上，未覆盖的入口仍会漏 502。旧的断言全是 require.Contains(text, ...)，
// 且正则只拦 `r.XXX(...)` 直接注册——本次漏掉的恰恰是 `r.Group("/v1beta")`
// 新建的链，旧断言对这种漏配完全无感（本次 Gemini 两链就是这样漏掉的）。
//
// 现在改为构造真实 gin 路由树，对每个生成类端点发哨兵值请求，断言 400 且响应体
// 含超限标识。新增一条生成类链若忘记挂护栏，本测试立即失败。
//
// 判定口径说明：断言「400 + 响应体含上限值」而不是「handler 未被触达」——
// 真实 handler 需要完整依赖装配，而超限拒绝发生在任何调度之前，400 与超限标识
// 足以证明请求被出站前拦下；另有一条 middleware 级测试断言 handler 未被触达。

// budgetCoverageAPIKeyRepo 是最小 APIKeyRepository 桩：只为让 Gemini 链的
// APIKeyAuthWithSubscriptionGoogle 能通过鉴权，走到护栏。
type budgetCoverageAPIKeyRepo struct {
	service.APIKeyRepository
	apiKey *service.APIKey
}

func (r *budgetCoverageAPIKeyRepo) GetByKeyForAuth(_ context.Context, key string) (*service.APIKey, error) {
	if r.apiKey == nil || key != r.apiKey.Key {
		return nil, service.ErrAPIKeyNotFound
	}
	clone := *r.apiKey
	return &clone, nil
}

// UpdateLastUsed 是 Google 鉴权链路收尾时的尽力而为写（TouchLastUsed）。
// 桩实现返回 nil，避免测试因未实现的仓储方法 panic。
func (r *budgetCoverageAPIKeyRepo) UpdateLastUsed(_ context.Context, _ int64, _ time.Time) error {
	return nil
}

func newBudgetCoverageRouter(t *testing.T, platform string) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	group := &service.Group{
		ID:               42,
		Name:             "budget-coverage",
		Status:           service.StatusActive,
		Hydrated:         true,
		Platform:         platform,
		SubscriptionType: service.SubscriptionTypeStandard,
	}
	user := &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 10}
	groupID := group.ID
	apiKey := &service.APIKey{
		ID:      100,
		UserID:  user.ID,
		Key:     "budget-coverage-key",
		Status:  service.StatusActive,
		User:    user,
		Group:   group,
		GroupID: &groupID,
	}
	cfg := &config.Config{
		RunMode: config.RunModeStandard,
		Gateway: config.GatewayConfig{
			MaxBodySize:     1024 * 1024,
			TextMaxBodySize: 1024 * 1024,
		},
	}
	apiKeyService := service.NewAPIKeyService(
		&budgetCoverageAPIKeyRepo{apiKey: apiKey}, nil, nil, nil, nil, nil, cfg,
	)

	router := gin.New()
	// 本文件装配的是零值 handler（缺 gatewayService 等依赖）。放行路径会在
	// handler 内部 panic，那是本测试的装配边界而非护栏行为；挂 recovery 把
	// panic 转成响应，使断言能干净地只看护栏行为。生产由 server 层的
	// Recovery 中间件承担同样职责。
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		c.AbortWithStatus(http.StatusInternalServerError)
	}))
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{
			Gateway:       &handler.GatewayHandler{},
			OpenAIGateway: &handler.OpenAIGatewayHandler{},
			AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
		},
		servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
			c.Set(string(servermiddleware.ContextKeyAPIKey), apiKey)
			c.Next()
		}),
		apiKeyService,
		nil,
		nil,
		nil,
		nil,
		cfg,
	)
	return router, apiKey.Key
}

// 生成类端点清单：每条都必须挂上护栏。路径取各链的实际生成入口。
var outputBudgetGenerationEndpoints = []struct {
	name string
	path string
	// body 携带该协议的输出预留字段与事故哨兵值。
	body string
}{
	{
		name: "v1 messages",
		path: "/v1/messages",
		body: `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"max_tokens":999999999}`,
	},
	{
		name: "v1 responses",
		path: "/v1/responses",
		body: `{"model":"gpt-5.5","input":"hi","max_output_tokens":999999999}`,
	},
	{
		name: "v1 chat completions",
		path: "/v1/chat/completions",
		body: `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":999999999}`,
	},
	{
		name: "root responses alias",
		path: "/responses",
		body: `{"model":"gpt-5.5","input":"hi","max_output_tokens":999999999}`,
	},
	{
		name: "root chat completions alias",
		path: "/chat/completions",
		body: `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"max_tokens":999999999}`,
	},
	{
		name: "codex direct responses",
		path: "/backend-api/codex/responses",
		body: `{"model":"gpt-5.5","input":"hi","max_output_tokens":999999999}`,
	},
	{
		name: "antigravity messages",
		path: "/antigravity/v1/messages",
		body: `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}],"max_tokens":999999999}`,
	},
	{
		name: "gemini native generateContent",
		path: "/v1beta/models/gemini-2.5-pro:generateContent",
		body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
			`"generationConfig":{"maxOutputTokens":999999999}}`,
	},
	{
		name: "gemini native streamGenerateContent",
		path: "/v1beta/models/gemini-2.5-pro:streamGenerateContent",
		body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
			`"generationConfig":{"maxOutputTokens":999999999}}`,
	},
	{
		name: "antigravity gemini native generateContent",
		path: "/antigravity/v1beta/models/gemini-3-pro:generateContent",
		body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
			`"generationConfig":{"maxOutputTokens":999999999}}`,
	},
}

// TestGatewayRoutesOutputBudgetGuardMountedOnEveryChain 构造真实路由树，
// 对每个生成类端点发事故哨兵值，断言出站前被拦成 400 且响应体含超限标识。
// 新增生成类链忘记挂 outputBudgetGuard 时本测试立即失败。
func TestGatewayRoutesOutputBudgetGuardMountedOnEveryChain(t *testing.T) {
	// Gemini 原生链走 APIKeyAuthWithSubscriptionGoogle，分组平台须为 gemini；
	// 同一棵路由树同时服务两条鉴权链，故按端点前缀选平台。
	for _, tc := range outputBudgetGenerationEndpoints {
		t.Run(tc.name, func(t *testing.T) {
			platform := service.PlatformOpenAI
			if strings.Contains(tc.path, "/v1beta/") || strings.Contains(tc.path, "/antigravity/v1beta/") {
				platform = service.PlatformGemini
			}
			router, key := newBudgetCoverageRouter(t, platform)

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("x-goog-api-key", key)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equal(t, http.StatusBadRequest, w.Code,
				"%s %s 的哨兵值必须被出站前拦成 400（不得触达上游产生 502），实际响应体：%s",
				http.MethodPost, tc.path, w.Body.String())
			require.Contains(t, w.Body.String(), "131072",
				"%s %s 的响应体须含超限标识（DefaultMaxOutputTokensCeiling）",
				http.MethodPost, tc.path)
		})
	}
}

// TestGatewayRoutesOutputBudgetGuardAllowsNormalValues 无回归：正常输出预留值
// 一律放行，护栏不是「一律拒绝」。
//
// 判定口径是「响应体不含护栏的超限标识」而不是「状态码为 2xx」：本文件装配的
// handler 是零值 handler，正常请求放行后会在 handler 内部因缺依赖而报错，那是
// 本测试的装配边界而非护栏行为。护栏自身的判定完全由响应体形状证明。
// （handler 未被触达这一条由 middleware 层的
// TestOutputBudgetGuardRejectsSentinelOnEveryEntry / ...OnGeminiChain 断言。）
func TestGatewayRoutesOutputBudgetGuardAllowsNormalValues(t *testing.T) {
	for _, tc := range outputBudgetGenerationEndpoints {
		t.Run(tc.name, func(t *testing.T) {
			platform := service.PlatformOpenAI
			if strings.Contains(tc.path, "/v1beta/") || strings.Contains(tc.path, "/antigravity/v1beta/") {
				platform = service.PlatformGemini
			}
			router, key := newBudgetCoverageRouter(t, platform)

			// 把该端点 body 里的哨兵值替换为合法值。
			normal := strings.ReplaceAll(tc.body, "999999999", "4096")
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(normal))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("x-goog-api-key", key)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.NotContains(t, w.Body.String(), "131072",
				"%s %s 的正常 max_tokens 必须放行，护栏响应体：%s",
				http.MethodPost, tc.path, w.Body.String())
			require.NotContains(t, w.Body.String(), "max_tokens_exceeds_limit",
				"%s %s 的正常请求不得被护栏拒绝", http.MethodPost, tc.path)
		})
	}
}

// TestGatewayRoutesOutputBudgetGuardNotWiredToReadOnlyEndpoints 反向覆盖：
// 只读/媒体端点不得被护栏拦下（否则是凭空拒绝合法请求）。
func TestGatewayRoutesOutputBudgetGuardNotWiredToReadOnlyEndpoints(t *testing.T) {
	// count_tokens 只分词不预留输出。
	t.Run("count_tokens", func(t *testing.T) {
		router, key := newBudgetCoverageRouter(t, service.PlatformOpenAI)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
			strings.NewReader(`{"model":"gpt-5.5","messages":[],"max_tokens":999999999}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusBadRequest, w.Code,
			"count_tokens 只读端点不得被输出预算护栏拦下")
	})

	// Gemini :countTokens 同理。
	t.Run("gemini countTokens", func(t *testing.T) {
		router, key := newBudgetCoverageRouter(t, service.PlatformGemini)
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:countTokens",
			strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],`+
				`"generationConfig":{"maxOutputTokens":999999999}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusBadRequest, w.Code,
			"Gemini :countTokens 只读端点不得被输出预算护栏拦下")
	})
}

// TestGatewayRoutesOutputBudgetGuardUsesGatewayErrorFormatPerChain 错误体形状
// 按入口协议成形：Gemini 原生用 Google 格式，Anthropic 入口用 Anthropic 格式，
// OpenAI 兼容入口用 OpenAI 格式——否则客户端按既有解析路径拿不到可读错误。
func TestGatewayRoutesOutputBudgetGuardUsesGatewayErrorFormatPerChain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		body       string
		platform   string
		wantSubstr []string
	}{
		{
			name: "gemini uses google error format",
			path: "/v1beta/models/gemini-2.5-pro:generateContent",
			body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
				`"generationConfig":{"maxOutputTokens":999999999}}`,
			platform:   service.PlatformGemini,
			wantSubstr: []string{`"status":"INVALID_ARGUMENT"`},
		},
		{
			name:       "anthropic uses anthropic error format",
			path:       "/antigravity/v1/messages",
			body:       `{"model":"claude-sonnet-4.5","messages":[],"max_tokens":999999999}`,
			platform:   service.PlatformOpenAI,
			wantSubstr: []string{`"type":"error"`},
		},
		{
			name:       "openai uses openai error format",
			path:       "/v1/chat/completions",
			body:       `{"model":"gpt-5.5","messages":[],"max_tokens":999999999}`,
			platform:   service.PlatformOpenAI,
			wantSubstr: []string{"max_tokens_exceeds_limit"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, key := newBudgetCoverageRouter(t, tc.platform)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("x-goog-api-key", key)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equal(t, http.StatusBadRequest, w.Code)
			var payload map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload),
				"错误体必须是各协议可解析的 JSON：%s", w.Body.String())
			for _, want := range tc.wantSubstr {
				require.Contains(t, w.Body.String(), want)
			}
		})
	}
}

// TestGatewayRoutesOutputBudgetGuardLimitsComeFromConfig 护栏阈值必须来自配置
// 且缺省回落到内置默认值（而不是「配置没写就不检查」）。
func TestGatewayRoutesOutputBudgetGuardLimitsComeFromConfig(t *testing.T) {
	limits := service.RequestOutputBudgetLimitsOrDefault(0, 0)
	require.Equal(t, service.DefaultMaxOutputTokensCeiling, limits.MaxOutputTokens)
	require.Equal(t, service.DefaultModelContextWindow, limits.ContextWindow)

	custom := service.RequestOutputBudgetLimitsOrDefault(4096, 8192)
	require.Equal(t, int64(4096), custom.MaxOutputTokens)
	require.Equal(t, int64(8192), custom.ContextWindow)

	// RequestOutputTokenFields 必须覆盖四种协议的字段名，否则换字段名即可绕过。
	for _, field := range []string{
		"max_tokens",
		"max_completion_tokens",
		"max_output_tokens",
		"generationConfig.maxOutputTokens",
	} {
		require.Contains(t, service.RequestOutputTokenFields, field)
	}
}
