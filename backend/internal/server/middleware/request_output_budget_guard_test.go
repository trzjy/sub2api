package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newOutputBudgetTestRouter 把护栏挂到与生产一致的相对位置
// （apiKeyAuth 之后、handler 之前），handler 回显是否真的到达了出站。
func newOutputBudgetTestRouter(limits service.RequestOutputBudgetLimits, reached *bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestOutputBudgetGuard(limits))
	handler := func(c *gin.Context) {
		*reached = true
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
	for _, path := range []string{
		"/v1/messages",
		"/v1/responses",
		"/v1/chat/completions",
		"/chat/completions",
		"/responses",
		"/messages",
		"/backend-api/codex/responses",
		"/antigravity/v1/messages",
	} {
		router.POST(path, handler)
	}
	return router
}

// newGeminiOutputBudgetTestRouter 按生产形状注册 Gemini 原生链：模型名在
// URL 路径（通配段 modelAction），不在请求体里——这正是 Gemini 链与其它入口的
// 关键差异，也是漏挂护栏最难被察觉的地方。
func newGeminiOutputBudgetTestRouter(limits service.RequestOutputBudgetLimits, reached *bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestOutputBudgetGuard(limits))
	handler := func(c *gin.Context) {
		*reached = true
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
	for _, prefix := range []string{"/v1beta/models", "/antigravity/v1beta/models"} {
		router.POST(prefix+"/*modelAction", handler)
	}
	return router
}

// TestOutputBudgetGuardRejectsSentinelOnEveryEntry 整改2 的核心断言：
// 事故哨兵值 max_tokens=999999999 在**每一个** OpenAI 兼容/消息类入口
// 都被出站前拦成 400，handler 一律没被触达（即没有走到上游）。
func TestOutputBudgetGuardRejectsSentinelOnEveryEntry(t *testing.T) {
	limits := service.RequestOutputBudgetLimitsOrDefault(0, 0)
	paths := []string{
		"/v1/messages",
		"/v1/responses",
		"/v1/chat/completions",
		"/chat/completions",
		"/responses",
		"/messages",
		"/backend-api/codex/responses",
		"/antigravity/v1/messages",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			reached := false
			router := newOutputBudgetTestRouter(limits, &reached)
			body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":999999999}`
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusBadRequest, recorder.Code, "sentinel must be rejected with 400, not forwarded")
			require.False(t, reached, "request must not reach dispatch (upstream 400->502 path)")
			require.Contains(t, recorder.Body.String(), "131072")
		})
	}
}

// TestOutputBudgetGuardNoRegressionNormalMaxTokens 无回归：正常 max_tokens
// 一律照常到达 handler，且响应体不被护栏改写。
func TestOutputBudgetGuardNoRegressionNormalMaxTokens(t *testing.T) {
	limits := service.RequestOutputBudgetLimitsOrDefault(0, 0)
	for _, value := range []int{1024, 4096, 8192, 32000, 65535, 128000} {
		t.Run(fmt.Sprintf("max_tokens=%d", value), func(t *testing.T) {
			reached := false
			router := newOutputBudgetTestRouter(limits, &reached)
			body := fmt.Sprintf(
				`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}],"max_tokens":%d}`, value)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			require.True(t, reached, "normal max_tokens must reach dispatch")
		})
	}
}

// TestOutputBudgetGuardSkipsNonGenerationRoutes count_tokens 只做分词不预留输出，
// 不能被预检拦下。
func TestOutputBudgetGuardSkipsNonGenerationRoutes(t *testing.T) {
	limits := service.RequestOutputBudgetLimitsOrDefault(0, 0)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestOutputBudgetGuard(limits))
	reached := false
	router.POST("/v1/messages/count_tokens", func(c *gin.Context) {
		reached = true
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"m","messages":[],"max_tokens":999999999}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.True(t, reached, "count_tokens must not be gated by the output budget guard")
}

// TestOutputBudgetGuardErrorFormatPerProtocol 错误体按入口协议成形：
// Messages 入口用 Anthropic 格式，OpenAI 兼容入口用 OpenAI 格式。
func TestOutputBudgetGuardErrorFormatPerProtocol(t *testing.T) {
	limits := service.RequestOutputBudgetLimitsOrDefault(0, 0)

	anthropicReached, openaiReached := false, false
	anthropicRouter := newOutputBudgetTestRouter(limits, &anthropicReached)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","messages":[],"max_tokens":999999999}`))
	request.Header.Set("Content-Type", "application/json")
	anthropicRouter.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"type":"error"`)
	require.Contains(t, recorder.Body.String(), "invalid_request_error")

	openaiRouter := newOutputBudgetTestRouter(limits, &openaiReached)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[],"max_tokens":999999999}`))
	request.Header.Set("Content-Type", "application/json")
	openaiRouter.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "max_tokens_exceeds_limit")
	require.Contains(t, recorder.Body.String(), `"param":"max_tokens"`)
}

// TestOutputBudgetGuardPreservesBodyForDownstream 读体后必须回填，下游 handler
// 能零拷贝重读到完整请求体（护栏不得吞掉 body）。
func TestOutputBudgetGuardPreservesBodyForDownstream(t *testing.T) {
	limits := service.RequestOutputBudgetLimitsOrDefault(0, 0)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestOutputBudgetGuard(limits))

	original := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"max_tokens":4096}`
	var seen string
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		buf := make([]byte, 1024)
		n, _ := c.Request.Body.Read(buf)
		seen = string(buf[:n])
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(original))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, original, seen, "guard must refill the request body for downstream handlers")
}

// TestOutputBudgetGuardPathClassification 钉住入口分类：只有生成类 POST 入口
// 进入预检，其余（GET / 媒体端点 / 无关路径）直接放行。
func TestOutputBudgetGuardPathClassification(t *testing.T) {
	for _, tc := range []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodPost, "/v1/messages", true},
		{http.MethodPost, "/v1/responses", true},
		{http.MethodPost, "/v1/chat/completions", true},
		{http.MethodPost, "/messages", true},
		{http.MethodPost, "/backend-api/codex/responses", true},
		{http.MethodPost, "/v1/messages/count_tokens", false},
		{http.MethodPost, "/messages/count_tokens", false},
		{http.MethodPost, "/v1/images/generations", false},
		{http.MethodPost, "/v1/tts", false},
		{http.MethodGet, "/v1/responses", false},
		{http.MethodGet, "/v1/models", false},
		// Gemini 原生生成动作必须进入预检（此前整条链无护栏）。
		{http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", true},
		{http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent", true},
		{http.MethodPost, "/antigravity/v1beta/models/gemini-3-pro:generateContent", true},
		// Gemini 的分词动作与 :generateContent 同族但只读，不得被拦。
		{http.MethodPost, "/v1beta/models/gemini-2.5-pro:countTokens", false},
		{http.MethodGet, "/v1beta/models/gemini-2.5-pro", false},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(tc.method, tc.path, nil)
			require.Equal(t, tc.want, requestCarriesOutputBudget(c))
		})
	}
}

// 缺陷4 核心断言：Gemini 原生链的哨兵值 maxOutputTokens=999999999
// 必须被出站前拦成 400，且错误体是 Google 形状（Gemini SDK 不解析 OpenAI 体）。
func TestOutputBudgetGuardRejectsSentinelOnGeminiChain(t *testing.T) {
	limits := service.RequestOutputBudgetLimitsOrDefault(0, 0)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"generationConfig":{"maxOutputTokens":999999999,"temperature":0.7}}`

	for _, path := range []string{
		"/v1beta/models/gemini-2.5-pro:generateContent",
		"/v1beta/models/gemini-2.5-pro:streamGenerateContent",
		"/antigravity/v1beta/models/gemini-3-pro:generateContent",
	} {
		t.Run(path, func(t *testing.T) {
			reached := false
			router := newGeminiOutputBudgetTestRouter(limits, &reached)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusBadRequest, recorder.Code,
				"Gemini 链的哨兵值必须被拦成 400，不得出站")
			require.False(t, reached, "请求不得到达调度/出站")
			require.Contains(t, recorder.Body.String(), "131072", "响应体须含超限标识")
			require.Contains(t, recorder.Body.String(), `"status"`,
				"Gemini 原生入口必须用 Google 错误格式")
			require.Contains(t, recorder.Body.String(), "INVALID_ARGUMENT")
		})
	}
}

// 无回归：正常 maxOutputTokens 照常到达 handler。
func TestOutputBudgetGuardAllowsNormalGeminiOutputTokens(t *testing.T) {
	limits := service.RequestOutputBudgetLimitsOrDefault(0, 0)
	for _, value := range []int{1024, 8192, 65536} {
		t.Run(fmt.Sprintf("maxOutputTokens=%d", value), func(t *testing.T) {
			reached := false
			router := newGeminiOutputBudgetTestRouter(limits, &reached)
			body := fmt.Sprintf(
				`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":%d}}`, value)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost,
				"/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			require.True(t, reached, "正常 maxOutputTokens 必须放行")
		})
	}
}

// 读体后必须回填：Gemini handler 也要零拷贝重读到完整请求体。
func TestOutputBudgetGuardPreservesGeminiBodyForDownstream(t *testing.T) {
	limits := service.RequestOutputBudgetLimitsOrDefault(0, 0)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestOutputBudgetGuard(limits))

	original := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":4096}}`
	var seen string
	router.POST("/v1beta/models/*modelAction", func(c *gin.Context) {
		buf := make([]byte, 1024)
		n, _ := c.Request.Body.Read(buf)
		seen = string(buf[:n])
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost,
		"/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(original))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, original, seen, "护栏不得吞掉 Gemini 请求体")
}

// TestOutputBudgetProtocolForPath 协议映射：估算器按协议收敛，映射错会导致
// 估算口径错误。
func TestOutputBudgetProtocolForPath(t *testing.T) {
	require.Equal(t, service.TierRoutingProtocolChatCompletions, outputBudgetProtocolForPath("/v1/chat/completions"))
	require.Equal(t, service.TierRoutingProtocolMessages, outputBudgetProtocolForPath("/v1/messages"))
	require.Equal(t, service.TierRoutingProtocolMessages, outputBudgetProtocolForPath("/antigravity/v1/messages"))
	require.Equal(t, service.TierRoutingProtocolResponses, outputBudgetProtocolForPath("/backend-api/codex/responses"))
	require.Empty(t, outputBudgetProtocolForPath("/v1/images/generations"))
}
