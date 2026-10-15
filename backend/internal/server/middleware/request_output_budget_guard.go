package middleware

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// RequestOutputBudgetGuard 是「最大输出 token」准入护栏：在请求进入选号与
// 出站转发之前，拦掉 max_tokens 超出模型承受范围的请求。
//
// 生产事故（2026-10，24h 603 条 502）：客户端发送哨兵值 max_tokens=999999999，
// 上游按「prompt + 预留输出」判超 100 万 context → 400 → 网关包成 502。
// 判定逻辑全部在 service.CheckRequestOutputBudget（唯一 SSOT），本文件只负责
// 「在哪一跳、读什么、怎么写回」。
//
// 为什么挂在中间件而不是各 handler：网关的全部生成类入口（/v1/messages、
// /v1/responses、/v1/chat/completions 及其根路径别名、/backend-api/codex/*、
// /antigravity/v1/messages）都经过同一条 middleware 链，在链上挂一次即可全覆盖；
// 放进 handler 则每个入口都要复制一遍调用，漏一个就留下一条 502 通路。
//
// 读体策略与 GroupModelAllowlist / compositeTargetPlatformMiddleware 一致：
// ReadRequestBodyWithPrealloc + ResetRequestBody（PrereadBody），下游零拷贝重读。
//
// 失败开放（fail-open）的只有 token 估算一步，且刻意如此：
// 护栏的第一道（max_tokens 硬上限）不依赖任何估算，已独立兜住事故形态；
// 估算器解析不了 body 时不应把本来合法的请求挡在门外。

// RequestOutputBudgetGuard 返回输出预算准入中间件。
func RequestOutputBudgetGuard(limits service.RequestOutputBudgetLimits) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil || !requestCarriesOutputBudget(c) {
			c.Next()
			return
		}

		body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
		if err != nil {
			// 读体失败不在本护栏职责内（bodyLimit 等上游中间件已先处理），
			// 放行让下游按既有路径报错，保持既有错误语义不变。
			c.Next()
			return
		}
		requestmodel.ResetRequestBody(c.Request, body)

		model := requestmodel.FromBodyForRoute(c.FullPath(), c.GetHeader("Content-Type"), body)
		if model == "" {
			// Gemini 原生链的模型名在 URL 路径（/v1beta/models/{model}:generateContent）
			// 而不在请求体里，取不到就等于没有估算口径。复用白名单中间件的同一
			// 提取器，保证两个中间件看到同一个模型名。
			model = groupModelAllowlistModelFromParams(c)
		}
		if model == "" {
			c.Next()
			return
		}
		// 协议取空串（Gemini 原生链无估算器分支）不是放行条件：CheckRequestOutputBudget
		// 的 max_tokens 硬上限先于任何估算执行，哨兵值照样被拦；空协议只让第二步
		// context 估算 fail-open。此处若一并跳过，Gemini 链就等于没挂护栏。
		protocol := outputBudgetProtocolForPath(c.Request.URL.Path)

		verdict := service.CheckRequestOutputBudget(model, protocol, body, limits)
		if !verdict.Rejected {
			c.Next()
			return
		}

		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
		MarkIngressRejected(c, IngressRejectOutputBudgetExceeded)
		requestOutputBudgetErrorWriter(c)(c, http.StatusBadRequest, verdict.Reason)
		c.Abort()
	}
}

// requestCarriesOutputBudget 报告该请求是否需要输出预算预检。
//
// 只覆盖生成类入口：它们才带 max_tokens 且会真的预留输出。count_tokens 只做
// 分词不产出会丢弃 max_tokens；GET/WS 入口与 images/videos/tts 等媒体端点
// 不适用，均不在链上或直接跳过。
//
// Gemini 原生入口（/v1beta、/antigravity/v1beta）的生成动作名为
// :generateContent / :streamGenerateContent，输出预留写作 generationConfig
// 下的 maxOutputTokens（见 service/gemini_messages_compat_service.go
// convertClaudeGenerationConfig），不经 /messages、/responses、/chat/completions
// 任一子串——漏掉它就等于给整条 Gemini 链留了一个不设防的哨兵值入口。
func requestCarriesOutputBudget(c *gin.Context) bool {
	if c.Request.Method != http.MethodPost {
		return false
	}
	path := c.Request.URL.Path
	// count_tokens 只做 tokenize，不预留输出；预检它等于凭空拒绝一个只读端点。
	if strings.Contains(path, "/count_tokens") {
		return false
	}
	// :countTokens 是 Gemini 的分词动作，与 /count_tokens 同义，同样跳过。
	if strings.Contains(path, ":countTokens") {
		return false
	}
	if isGeminiGenerateActionPath(path) {
		return true
	}
	return strings.Contains(path, "/messages") ||
		strings.Contains(path, "/responses") ||
		strings.Contains(path, "/chat/completions")
}

// isGeminiGenerateActionPath 报告是否为 Gemini 原生的生成动作。
// 覆盖 :generateContent 与 :streamGenerateContent 两种（含大小写变体，
// Gemini SDK 允许 :generateContent / :GenerateContent 之外的写法，统一按
// 「以 generatecontent 结尾的动作名」判定，避免漏配新别名）。
func isGeminiGenerateActionPath(path string) bool {
	lower := strings.ToLower(path)
	for _, action := range []string{":generatecontent", ":streamgeneratecontent"} {
		if strings.Contains(lower, action) {
			return true
		}
	}
	return false
}

// outputBudgetProtocolForPath 把入口路径映射到估算器认识的协议形态。
// 返回空串表示该路径不在预检范围。
//
// Gemini 原生链没有独立的估算器分支，且其请求体（contents/generationConfig）
// 无法被现有三种 apicompat 转换器解析。这里返回空串而非硬塞一个协议：
// CheckRequestOutputBudget 的 max_tokens 硬上限在协议判定之前就已完成
// （见该函数第 1 步），事故形态的哨兵值照样被拦；只有 context 长度预检这一
// fail-open 的第二步被跳过——宁可少一道估算，也不伪造协议口径。
func outputBudgetProtocolForPath(path string) service.TierRoutingRequestProtocol {
	switch {
	case strings.Contains(path, "/chat/completions"):
		return service.TierRoutingProtocolChatCompletions
	case strings.Contains(path, "/messages"):
		return service.TierRoutingProtocolMessages
	case strings.Contains(path, "/responses"):
		return service.TierRoutingProtocolResponses
	default:
		return ""
	}
}

// requestOutputBudgetErrorWriter 按入口协议选择错误格式，与
// groupModelAllowlistErrorWriter 同口径（Gemini 原生用 Google 格式、Anthropic
// 入口用 Anthropic 格式、其余 OpenAI 兼容入口用 OpenAI 格式），保证客户端按既有
// 错误解析路径拿到 400——Gemini SDK 不会去解析 OpenAI 形状的错误体。
func requestOutputBudgetErrorWriter(c *gin.Context) GatewayErrorWriter {
	path := ""
	if c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	switch {
	case strings.HasPrefix(path, "/v1beta") || strings.HasPrefix(path, "/antigravity/v1beta"):
		return GoogleErrorWriter
	case strings.Contains(path, "/messages"):
		return requestOutputBudgetAnthropicErrorWriter
	default:
		return requestOutputBudgetOpenAIErrorWriter
	}
}

// requestOutputBudgetOpenAIErrorWriter 输出 OpenAI 规范的 400。
func requestOutputBudgetOpenAIErrorWriter(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "invalid_request_error",
			"param":   "max_tokens",
			"code":    "max_tokens_exceeds_limit",
		},
	})
}

// requestOutputBudgetAnthropicErrorWriter 输出 Anthropic 规范的 400。
func requestOutputBudgetAnthropicErrorWriter(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{
		"type":  "error",
		"error": gin.H{"type": "invalid_request_error", "message": message},
	})
}
