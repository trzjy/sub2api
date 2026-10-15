package service

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// kimi web 网页逆向适配器（方案 W2，docs-local/web-reverse-embedded-login-plan.md §4.3、
// docs-local/web-reverse-analysis-plan.md §1.3/§3.3/§3.4）。
//
// 协议状态声明（权威来源：登录态实测 10-kimi-logged-in-probe.md，2026-09-18）：
//   - 已实测（10 §3/§4/§2）：对话端点 POST /apiv2/kimi.chat.v1.ChatService/Chat 走
//     Connect RPC 流式（envelope：1 字节 flag + 4 字节大端长度 + payload JSON）；请求体块
//     结构为 blocks[].text.content 简单形态、scenario=SCENARIO_CHAT、options 含
//     thinking/enable_plugin/reasoning_effort/model、project_id，**请求体无 chatId**
//     （chatId 由服务端在流首帧 chat.id 返回）；流式增量走 op/mask/eventOffset
//     （block.text.content 正文、block.think.content 思考），终止为 done 帧 +
//     message.status=MESSAGE_STATUS_COMPLETED，首帧/中途 heartbeat 帧跳过。
//   - 头族实测：content-type application/connect+json、x-language / x-msh-device-id /
//     x-msh-platform: web / x-msh-session-id / x-msh-version: 2.2.0 / x-traffic-id。
//   - 残余 unverified（不得补猜，按兼容/缺省处理并标注）：
//     · 认证载体：access_token 带 Authorization: Bearer（单入口短信登录后仅此一种，不再有 cookie 候选）。
//     · x-msh-shield-data：生成算法未逆向，不实现生成；仅 credentials 有值时透传。
//     · x-msh-device-id / x-msh-session-id / x-traffic-id：实测为动态值，不生成；仅 credentials
//       有值时透传（管理员抓包配置），缺省不带。
//     · 刷新成功响应结构、access/refresh token 生命周期：历史未见 RefreshToken 请求，沿用
//       现有多路径提取（unverified）。
//     · usage：流内无 usage/token 字段（实测 0 处），本地 estimateWebUsage 兜底。
//
// 安全红线：凭证（access_token / refresh_token / cookie）不得出现在日志或错误响应中——上游
// 错误体在任何透传前先经 redactWebKimiUpstreamErrorBody 脱敏。

const (
	// webKimiDefaultBaseURL 默认官方网页端域名（分析文档 §1.3 实测）。
	webKimiDefaultBaseURL = "https://www.kimi.com"
	// webKimiChatPath 对话端点（Connect RPC JSON，实测）。
	webKimiChatPath = "/apiv2/kimi.chat.v1.ChatService/Chat"
	// webKimiRefreshTokenURL Token 刷新端点（实测存在；成功响应结构未实测）。
	webKimiRefreshTokenURL = "https://auth.kimi.com/api/account.gateway.v1.AuthService/RefreshToken"
	// webKimiClientUA 指纹对齐用浏览器 UA（方案 §3.5；具体 UA 以登录态抓包为准）。
	webKimiClientUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// forwardWebKimi 是 Kimi 网页逆向平台（kimi web）的转发入口，函数链模式与
// forwardCodeBuddy / forwardWebDeepseek 同构：入站 OpenAI Chat Completions → Connect
// RPC 请求 → 回程通用解析 → OpenAI 形状回写。挂载点由 OpenAIGatewayService 转发链的
// isWebKimiAccount 断言分发。
// assertWebKimiAccount 是 forwardWebKimi 入口的双断言 fail-closed（隔离红线 §6）：官方
// kimi 平台 + web access mode 双断言（平台归并后唯一形态，PR-4）。API 模式 kimi 账号
// 绝不进入网页协议链，web 模式 zhipu/deepseek 账号绝不误入。
func assertWebKimiAccount(account *Account) error {
	if account == nil {
		return fmt.Errorf("forwardWebKimi requires a non-nil account")
	}
	if account.IsWebAccessMode() && account.IsKimi() {
		return nil
	}
	if !account.IsWebAccessMode() {
		return fmt.Errorf("forwardWebKimi requires web access mode, got %q", account.GetAccessMode())
	}
	return fmt.Errorf("forwardWebKimi requires a kimi platform, got %q", account.Platform)
}

func (s *OpenAIGatewayService) forwardWebKimi(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
	mode webResponseMode,
) (*OpenAIForwardResult, error) {
	if err := assertWebKimiAccount(account); err != nil {
		return nil, err
	}

	accessToken := strings.TrimSpace(account.GetCredential("access_token"))
	if accessToken == "" {
		return nil, errors.New("kimi web account is missing access_token credential")
	}

	// base_url 统一走 account.GetWebBaseURL()（覆盖优先 → 平台默认），避免内联重复实现漂移。
	baseURL := strings.TrimRight(account.GetWebBaseURL(), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("kimi web account has no base_url (platform %s)", account.Platform)
	}

	// 模型映射：account.GetModelMapping() 默认透传（GetMappedModel 未命中即原样返回），
	// 再做网页端模型名归一（方案 §3.3 实测映射：kimi-k3 → k3、kimi-k3-agent-ultra → k3-agent-ultra）。
	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = originalModel
	}
	webModel := webKimiModelName(upstreamModel)
	SetOpsUpstreamModel(c, webModel)

	prompt := webKimiExtractPrompt(body)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("kimi web requires at least one user message in the request")
	}
	upstreamBody := buildWebKimiRequestBody(prompt, webModel, account)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()

	req, err := s.buildWebKimiUpstreamRequest(upstreamCtx, account, baseURL+webKimiChatPath, accessToken, upstreamBody)
	if err != nil {
		return nil, err
	}
	resp, err := s.doOpenAIUpstream(ctx, req, proxyURL, account)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(startTime).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false, upstreamModel)
	}
	defer func() { _ = resp.Body.Close() }()

	// 401 → 先用 refresh_token 刷新一次再重试（方案 W2：Token 刷新链路）；
	// 刷新失败走 handleWebKimiUpstreamError 正常冷却与错误回传。
	if resp.StatusCode == http.StatusUnauthorized {
		refreshed := s.refreshWebKimiAccessToken(ctx, account, proxyURL)
		if refreshed != "" {
			accessToken = refreshed
			retryCtx, releaseRetry := detachUpstreamContext(ctx)
			defer releaseRetry()
			retryReq, reqErr := s.buildWebKimiUpstreamRequest(retryCtx, account, baseURL+webKimiChatPath, accessToken, upstreamBody)
			if reqErr != nil {
				return nil, reqErr
			}
			retryResp, retryErr := s.doOpenAIUpstream(ctx, retryReq, proxyURL, account)
			if retryErr != nil {
				return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, retryErr, false, upstreamModel)
			}
			_ = resp.Body.Close()
			resp = retryResp
		}
	}

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		return s.handleWebKimiUpstreamError(ctx, c, account, resp, respBody, upstreamModel)
	}

	if reqStream {
		return s.handleWebKimiStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, prompt, mode)
	}
	return s.handleWebKimiNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, prompt, mode)
}

// webKimiModelName 把（已映射的）模型名归一为网页端模型名。
// 方案 §3.3（分析文档）+ 登录态实测（10 §3：默认 k2d6-chat）：kimi-k3 → k3、
// kimi-k3-agent-ultra → k3-agent-ultra、kimi-k2d6 → k2d6、kimi-k2d6-chat → k2d6-chat；
// 默认透传同名。其余取值待登录态实测补全。
//
// 公开名与内部代号（2026-09-22 命名修正）：上游网页端模型 ID 实为内部代号 k2d6
// （官方模型列表 displayName=Instant、场景代号 SCENARIO_K2D5，即 Kimi 2.6 代次），
// 对外不可读。公开目录（DefaultWebModelIDs）改用 kimi-2.6 / kimi-2.6-chat 可读名，
// 由此处归一回上游代号；旧公开名 kimi-k2d6 / kimi-k2d6-chat 与裸代号 k2d6 /
// k2d6-chat 继续兼容（已下发客户端与历史 model_mapping 不破坏）。
func webKimiModelName(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "kimi-k3", "k3":
		return "k3"
	case "kimi-k3-agent-ultra", "k3-agent-ultra":
		return "k3-agent-ultra"
	case "kimi-2.6", "kimi-k2d6", "k2d6":
		return "k2d6"
	case "kimi-2.6-chat", "kimi-k2d6-chat", "k2d6-chat":
		return "k2d6-chat"
	default:
		return model
	}
}

// buildWebKimiRequestBody 按登录态实测（10 §3）构建 Connect RPC 请求体：
//   - 块结构实测为 blocks[].text.content 简单形态（非历史 content{case,value} 复杂形态）；
//   - 请求体不带 chatId（chatId 由服务端在流首帧 chat.id 返回）；
//   - options 含 thinking / enable_plugin / reasoning_effort / model，scenario=SCENARIO_CHAT；
//     实测请求 thinking=true、enable_plugin=true、reasoning_effort=REASONING_EFFORT_LOW。
//
// 未实测字段一律缺省（不臆测填充）；thinking 开关与入站映射 unverified，按实测默认 true。
type webKimiTextBlock struct {
	MessageID string      `json:"message_id"`
	Text      webKimiText `json:"text"`
}

type webKimiText struct {
	Content string `json:"content"`
}

type webKimiMessage struct {
	Role     string             `json:"role"` // "user"
	Blocks   []webKimiTextBlock `json:"blocks"`
	Scenario string             `json:"scenario"`
	IsGoal   bool               `json:"is_goal"`
}

type webKimiRequestOptions struct {
	Thinking        bool   `json:"thinking"`
	EnablePlugin    bool   `json:"enable_plugin"`
	ReasoningEffort string `json:"reasoning_effort"`
	Model           string `json:"model"`
}

type webKimiUpstreamRequest struct {
	Scenario  string                `json:"scenario"`
	Tools     []any                 `json:"tools"`
	Message   webKimiMessage        `json:"message"`
	Options   webKimiRequestOptions `json:"options"`
	ProjectID string                `json:"project_id"`
}

// webKimiDefaultTools 实测（10 §3）默认工具清单：搜索 + 定时任务。
var webKimiDefaultTools = []any{
	map[string]any{"type": "TOOL_TYPE_SEARCH", "search": map[string]any{}},
	map[string]any{"type": "TOOL_TYPE_CRON_JOB"},
}

func buildWebKimiRequestBody(prompt, webModel string, account *Account) []byte {
	req := webKimiUpstreamRequest{
		Scenario: "SCENARIO_CHAT",
		Tools:    webKimiDefaultTools,
		Message: webKimiMessage{
			Role:     "user",
			Blocks:   []webKimiTextBlock{{MessageID: "", Text: webKimiText{Content: prompt}}},
			Scenario: "SCENARIO_CHAT",
			IsGoal:   false,
		},
		Options: webKimiRequestOptions{
			Thinking:        true,
			EnablePlugin:    true,
			ReasoningEffort: "REASONING_EFFORT_LOW",
			Model:           webModel,
		},
		ProjectID: "",
	}
	data, _ := json.Marshal(req)
	// Connect RPC 流式 RPC 的**请求体**同样必须帧化（分析文档 §1.4 生产复测：发裸 JSON
	// 会被上游以 HTTP 200 + flag=2 trailer {"error":{"code":"invalid_argument"}} 拒绝）。
	// 帧化放在这里而不是 buildWebKimiUpstreamRequest：探活与转发的 401 重试都复用同一份
	// 已构建 body，在 build-request 处帧化会导致重试时二次封帧（双帧）。
	return encodeWebKimiConnectEnvelope(data)
}

// webKimiExtractPrompt 从入站 OpenAI 请求取最后一条 user 消息文本
// （网页端单 prompt 语义；多轮上下文展开方式待登录态实测补全）。
func webKimiExtractPrompt(body []byte) string {
	messages := gjson.GetBytes(body, "messages").Array()
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Get("role").String() != "user" {
			continue
		}
		content := m.Get("content")
		if content.Type == gjson.String {
			return content.String()
		}
		var b strings.Builder
		for _, part := range content.Array() {
			if part.Get("type").String() == "text" {
				b.WriteString(part.Get("text").String())
			}
		}
		return b.String()
	}
	return ""
}

// buildWebKimiUpstreamRequest 构造 Connect RPC 出站请求（头族对齐登录态实测 10 §2）。
//
// 认证载体：access_token 带 Authorization: Bearer（单入口短信登录后仅此一种，不再有 cookie 候选）。
//
// 头族（10 §2 实测）：content-type/accept 用 application/connect+json（Connect RPC 流式）；
// x-language / x-msh-platform: web / x-msh-version: 2.2.0 为实测常量直接设置；
// x-msh-device-id / x-msh-session-id / x-traffic-id 实测为动态值、生成算法未逆向——
// 仅 credentials 有值时透传（管理员抓包配置），缺省不带（unverified，注释标注）；
// x-msh-shield-data 生成算法未逆向——不实现生成，仅 credentials 有值时透传（unverified）。
// 账号级请求头覆写最后应用，使管理员配置优先。
func (s *OpenAIGatewayService) buildWebKimiUpstreamRequest(
	ctx context.Context,
	account *Account,
	targetURL string,
	accessToken string,
	body []byte,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// Connect RPC 流式协议：content-type / accept 实测为 application/connect+json。
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Accept", "application/connect+json")
	req.Header.Set("Origin", webKimiDefaultBaseURL)
	req.Header.Set("Referer", webKimiDefaultBaseURL+"/")
	req.Header.Set("User-Agent", webKimiClientUA)

	// 认证载体：Bearer access_token（单入口后仅此一种）。
	req.Header.Set("Authorization", "Bearer "+accessToken)

	// 头族实测常量（10 §2）。
	req.Header.Set("x-language", "zh-CN")
	req.Header.Set("x-msh-platform", "web")
	req.Header.Set("x-msh-version", "2.2.0")
	// 动态值（unverified）：仅在管理员抓包配置 credentials 时透传，缺省不带。
	if v := strings.TrimSpace(account.GetCredential("x_msh_device_id")); v != "" {
		req.Header.Set("x-msh-device-id", v)
	}
	if v := strings.TrimSpace(account.GetCredential("x_msh_session_id")); v != "" {
		req.Header.Set("x-msh-session-id", v)
	}
	if v := strings.TrimSpace(account.GetCredential("x_traffic_id")); v != "" {
		req.Header.Set("x-traffic-id", v)
	}
	// TrustDecision 设备指纹（unverified）：生成算法未逆向，不实现生成，仅透传。
	if v := strings.TrimSpace(account.GetCredential("x_msh_shield_data")); v != "" {
		req.Header.Set("x-msh-shield-data", v)
	}

	// 账号级请求头覆写最后应用，使管理员配置优先（与 CodeBuddy 出站口径一致）。
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// refreshWebKimiAccessToken 用 refresh_token 刷新 access_token。
//
// 刷新端点已实测存在（无效 token 时 401 "Session expired"），但成功响应结构未实测。
// 这里只识别 access_token / refresh_token 在顶层 / data / token 包装下的常见 JSON 位置；
// 均未命中或刷新失败时返回空串（调用方继续按原 401 走冷却与错误路径），绝不臆造响应语义。
//
// 刷新成功后把新 access_token（以及上游若一并返回的新 refresh_token）经统一汇聚点
// persistAccountCredentials 回写账号凭据（D1 持久化）。凭据只写入非影子账号，且错误
// 文案一律脱敏，绝不回显 token 值。
func (s *OpenAIGatewayService) refreshWebKimiAccessToken(ctx context.Context, account *Account, proxyURL string) string {
	refreshToken := strings.TrimSpace(account.GetCredential("refresh_token"))
	if refreshToken == "" {
		return ""
	}
	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()

	payload, _ := json.Marshal(map[string]string{"refreshToken": refreshToken})
	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, webKimiRefreshTokenURL, bytes.NewReader(payload))
	if err != nil {
		return ""
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", webKimiClientUA)

	resp, err := s.doOpenAIUpstreamNoWatchdog(req, proxyURL, account)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return ""
	}

	var newAccessToken, newRefreshToken string
	for _, path := range []string{"accessToken", "data.accessToken", "token", "data.token"} {
		if v := strings.TrimSpace(gjson.GetBytes(body, path).String()); v != "" {
			newAccessToken = v
			break
		}
	}
	if newAccessToken == "" {
		return ""
	}
	// refresh_token 仅在上游一并返回时持久化（轮转场景）；缺失则保持原值不变。
	for _, path := range []string{"refreshToken", "data.refreshToken", "refresh_token", "data.refresh_token"} {
		if v := strings.TrimSpace(gjson.GetBytes(body, path).String()); v != "" {
			newRefreshToken = v
			break
		}
	}

	credentials := shallowCopyMap(account.Credentials)
	credentials["access_token"] = newAccessToken
	if newRefreshToken != "" {
		credentials["refresh_token"] = newRefreshToken
	}
	if persistErr := persistAccountCredentials(ctx, s.accountRepo, account, credentials); persistErr != nil {
		// 持久化失败不阻断本次转发（已拿到新 token 可继续重试），仅脱敏告警。
		slog.Warn("kimi web refresh succeeded but credential persist failed",
			"account_id", account.ID, "error", persistErr.Error())
	}
	return newAccessToken
}

// webKimiErrKind Kimi 网页端上游错误分类（分析文档 §3.4 冷却触发：401 / code=unauthenticated）。
type webKimiErrKind int

const (
	webKimiErrKindOther webKimiErrKind = iota
	webKimiErrKindAuth
	webKimiErrKindRateLimited
)

// classifyWebKimiUpstreamError 纯函数错误分类：HTTP 401（含 Connect RPC 的
// unauthenticated）→ 认证类；HTTP 429 → 限流类。
func classifyWebKimiUpstreamError(statusCode int, body []byte) webKimiErrKind {
	if statusCode == http.StatusUnauthorized {
		return webKimiErrKindAuth
	}
	if statusCode == http.StatusTooManyRequests {
		return webKimiErrKindRateLimited
	}
	if msg := strings.ToLower(gjson.GetBytes(body, "code").String()); strings.Contains(msg, "unauthenticated") {
		return webKimiErrKindAuth
	}
	return webKimiErrKindOther
}

// redactWebKimiUpstreamErrorBody 上游错误体脱敏：清除可能被上游回显的凭证片段。
func redactWebKimiUpstreamErrorBody(body []byte, account *Account) []byte {
	if len(body) == 0 || account == nil {
		return body
	}
	text := string(body)
	for _, key := range []string{"access_token", "refresh_token"} {
		if v := strings.TrimSpace(account.GetCredential(key)); len(v) >= 8 {
			text = strings.ReplaceAll(text, v, "***")
		}
	}
	return []byte(text)
}

// handleWebKimiUpstreamError 对上游错误做分类、脱敏、限流副作用（复用
// RateLimitService.HandleUpstreamError 的 CN 供应商语义），最后经 handleErrorResponse
// 把错误回传客户端。
func (s *OpenAIGatewayService) handleWebKimiUpstreamError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	respBody []byte,
	upstreamModel string,
) (*OpenAIForwardResult, error) {
	kind := classifyWebKimiUpstreamError(resp.StatusCode, respBody)
	respBody = redactWebKimiUpstreamErrorBody(respBody, account)
	if resp != nil {
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	upstreamMsg := webKimiUpstreamErrorMessage(respBody)
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("kimi web upstream returned status %d", resp.StatusCode)
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("trace-id")),
		Kind:               "http_error",
		Message:            upstreamMsg,
	})

	if kind == webKimiErrKindAuth && resp.StatusCode < 400 {
		// Connect RPC 业务错误随 HTTP 200 返回（实测 401 unauthenticated 的对称形态）；
		// 改写状态码为 401，让 handleErrorResponse 内部的 handleOpenAIAccountUpstreamError
		// 单点按 401 冷却（与 forwardCodeBuddy 模板同口径，此处不重复调用避免双重冷却）。
		resp.StatusCode = http.StatusUnauthorized
	}
	return s.handleErrorResponse(ctx, resp, c, account, respBody, upstreamModel)
}

// webKimiUpstreamErrorMessage 提取上游错误文案；Connect RPC 错误常见 message 字段，
// 其余回落通用提取。
//
// 标量守卫：message 字段在业务帧里承载消息对象（op/set mask=message 的 user/assistant
// 回显），gjson 对对象值取 String() 会返回其 JSON 序列化——把它当错误文案透传会把用户
// prompt 全文塞进客户端 error 响应与 ops_error_logs（2026-09-22 线上事故伴随损伤）。
// 因此 message / error.message / detail 仅在为标量（gjson Type != JSON）时才视为错误
// 文案；对象值一律跳过，宁可回落到通用 status 文案也不回显业务载荷。
func webKimiUpstreamErrorMessage(body []byte) string {
	for _, path := range []string{"message", "error.message", "detail"} {
		m := gjson.GetBytes(body, path)
		if m.Exists() && m.Type != gjson.JSON {
			if s := strings.TrimSpace(m.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

// webKimiStreamEvent 是单帧 Connect envelope 载荷解析后的结构化视图。
// 解析只识别登录态实测（10 §4）的字段，绝不编造未实测结构。
type webKimiStreamEvent struct {
	Heartbeat   bool   // {"heartbeat":{}} 心跳帧，跳过
	Done        bool   // {"eventOffset":N,"done":{}} 终止帧
	ChatID      string // 首帧 chat.id（服务端生成的会话 ID）
	AssistantID string // assistant message 的 id（响应 id 来源）
	TextDelta   string // block.text.content 增量（正文，op set/append 均追加）
	ThinkDelta  string // block.think.content 增量（思考，op set/append 均追加）
	AuthFailed  bool   // code/message（含嵌套 error）含 unauthenticated
	ErrCode     int64  // 业务错误码：数字码（非 0）
	ErrCodeStr  string // 业务错误码：字符串码（如 "invalid_argument" / "unauthenticated"）
}

// encodeWebKimiConnectEnvelope 是 readWebKimiConnectEnvelope 的对称编码端：把一帧 payload
// 包成 Connect envelope（flag=0x00 + 4 字节大端长度 + payload）。
//
// 必要性（分析文档 §1.4，2026-09-21 生产复测）：Kimi Connect RPC 流式 RPC 的**请求体**也
// 必须走 envelope 帧；此前生产侧只 Marshal 裸 JSON，上游一律回 HTTP 200 + flag=2 trailer
// {"error":{"code":"invalid_argument"}}，转发表象为 502、探活表象为 "no valid frames parsed"。
func encodeWebKimiConnectEnvelope(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = 0x00 // 不压缩
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// readWebKimiConnectEnvelope 读取一帧 Connect RPC 流式 envelope：
// 1 字节 flag（0x00 不压缩 / 0x01 gzip，实测为 0x00）+ 4 字节大端长度 + payload JSON。
// 返回 payload、是否还有后续帧（more）、错误。more=false 表示流结束（正常 EOF 或截断）。
func readWebKimiConnectEnvelope(r *bufio.Reader) (payload []byte, more bool, err error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, false, nil
		}
		return nil, false, err
	}
	flag := header[0]
	length := binary.BigEndian.Uint32(header[1:5])
	if length == 0 {
		// 零长消息（keepalive 等），跳过不产出，继续读下一帧。
		return nil, true, nil
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, false, nil
		}
		return nil, false, err
	}
	if flag == 0x01 {
		// gzip 压缩 envelope（理论存在，实测未出现，unverified 最佳努力解压）。
		if decoded, derr := webKimiGunzip(payload); derr == nil {
			payload = decoded
		}
		// 解压失败则原样返回（避免丢帧，交由 JSON 解析失败安全忽略）。
	}
	return payload, true, nil
}

// webKimiGunzip 解 gzip；仅用于 flag=0x01 的 envelope（unverified 分支）。
func webKimiGunzip(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// webKimiExtractCode 归一化读取一个 code 节点：数字非 0 → int64，非空字符串 → 原样字符串；
// 其余（缺失 / 数字 0 / 空串）返回零值。用于顶层 code 与 trailer 嵌套 error.code 同口径。
func webKimiExtractCode(node gjson.Result) (int64, string) {
	switch node.Type {
	case gjson.Number:
		if n := node.Int(); n != 0 {
			return n, ""
		}
	case gjson.String:
		if s := strings.TrimSpace(node.String()); s != "" {
			return 0, s
		}
	}
	return 0, ""
}

// parseWebKimiEnvelopePayload 把一帧 Connect envelope 的 JSON payload 解析为 webKimiStreamEvent。
//
// 登录态实测（10 §4）字段语义：
//   - {"heartbeat":{}} → 心跳，跳过；
//   - {"eventOffset":N,"done":{}} → 终止；
//   - op/mask/eventOffset 信封：mask "block.text.content" 为正文增量、"block.think.content"
//     为思考增量（op=set 时值为当前完整内容，op=append 时为增量，按追加重排即可还原）；
//   - 顶层 chat.id 提取会话 id；mask "message" 的 assistant message.id 提取响应 id；
//   - usage 流内无（实测 0 处），本地估算兜底；
//   - 业务/认证错误：扁平形态为顶层 code/message，Connect trailer 形态（flag=2，实测
//     {"error":{"code":"invalid_argument","details":[...]}}）嵌套在 error 对象里。二者必须
//     统一识别——只认顶层会让 trailer 帧被判为「无字段的空事件」，进而不计有效帧、表象为
//     "an empty response (no valid frames parsed)"（线上故障，2026-09-21）。
//     数字码 → ErrCode，字符串码 → ErrCodeStr（gjson 对字符串 code 取 Int() 恒为 0，
//     历史上字符串业务码被吞正是此原因）。
func parseWebKimiEnvelopePayload(payload []byte) webKimiStreamEvent {
	var ev webKimiStreamEvent
	if !gjson.Valid(string(payload)) {
		return ev
	}
	v := gjson.ParseBytes(payload)
	if v.Get("heartbeat").Exists() {
		ev.Heartbeat = true
		return ev
	}
	if v.Get("done").Exists() {
		ev.Done = true
		return ev
	}
	// 业务/认证错误判定：顶层 code / error.code 取其一即可，字符串码落到 ErrCodeStr。
	for _, node := range []gjson.Result{v, v.Get("error")} {
		num, str := webKimiExtractCode(node.Get("code"))
		if num != 0 {
			ev.ErrCode = num
		}
		if str != "" && ev.ErrCodeStr == "" {
			ev.ErrCodeStr = str
		}
	}
	// 认证错误判定（只认结构化错误位置，绝不扫业务载荷文本）：
	//   - code 字段值本身含 unauthenticated（顶层或 trailer 嵌套）；或
	//   - HTTP 401 对称形态：error.message 是【标量文案】时对它做子串匹配。
	//
	// 线上事故（2026-09-22）：旧实现把顶层 message 也拼进扫描文本，而 op/set mask=message
	// 的用户消息回显帧里 message.role=user、message.blocks[].text.content 承载的是用户
	// prompt 全文——正文里出现英文单词 "unauthenticated"（来自 AGENTS.md 协议文本）即被
	// 误判为认证失败，正常流被收口成 502。message 字段承载业务载荷，不参与错误判定；
	// error.message 仅在其为标量（gjson Type 不是 JSON 对象）时才可能是错误文案。
	authFailed := strings.Contains(strings.ToLower(ev.ErrCodeStr), "unauthenticated")
	if !authFailed {
		if em := v.Get("error.message"); em.Exists() && em.Type != gjson.JSON &&
			strings.Contains(strings.ToLower(em.String()), "unauthenticated") {
			authFailed = true
		}
	}
	if !authFailed {
		// 顶层 message 仅在【没有 code 字段且是标量】时才可能是错误文案
		//（实测 401 的 HTTP 层错误体 {"code":"unauthenticated","message":"..."} 已由
		// code 覆盖；此处兜底 code 缺失但 message 为标量文案的形态）。
		if m := v.Get("message"); m.Exists() && m.Type != gjson.JSON &&
			strings.Contains(strings.ToLower(m.String()), "unauthenticated") {
			authFailed = true
		}
	}
	if authFailed {
		ev.AuthFailed = true
	}
	// 会话 id（首帧 chat.id 返回）。
	if id := strings.TrimSpace(v.Get("chat.id").String()); id != "" {
		ev.ChatID = id
	}
	// message 帧：assistant message.id 作为响应 id；assistant 整消息块全文（非流式全量
	// 形态兜底）也并入正文增量。
	if m := v.Get("message"); m.Exists() {
		if role := strings.TrimSpace(m.Get("role").String()); role == "assistant" {
			if id := strings.TrimSpace(m.Get("id").String()); id != "" {
				ev.AssistantID = id
			}
			for _, b := range m.Get("blocks").Array() {
				if t := strings.TrimSpace(b.Get("text.content").String()); t != "" {
					ev.TextDelta += t
				}
			}
		}
	}
	// block 增量：block.text.content（正文）/ block.think.content（思考）。
	if blk := v.Get("block"); blk.Exists() {
		if t := blk.Get("text.content"); t.Exists() {
			ev.TextDelta += t.String()
		}
		if t := blk.Get("think.content"); t.Exists() {
			ev.ThinkDelta += t.String()
		}
	}
	return ev
}

// webKimiBareJSONError 判定一行裸 JSON（非 SSE data: 前缀）是否为业务错误：携带非 0
// 的 code 字段时按业务错误返回其载荷，否则 isError=false。与 webDeepseekBareJSONError
// 同口径（#3）。注意：Connect envelope 协议下裸 JSON 行不会出现，本函数仅保留给共享
// 测试判定（web_protocol_bridge_test.go）使用，转发链路已改用 envelope 解析。
func webKimiBareJSONError(line string) (payload []byte, isError bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "data:") {
		return nil, false
	}
	if !gjson.Valid(trimmed) {
		return nil, false
	}
	code := gjson.GetBytes([]byte(trimmed), "code")
	// #3 同时识别数字 code（非 0）与字符串 code（非空）：上游返回
	// {"code":"unauthenticated"} 这类字符串 code 也应判为业务错误，否则会被忽略
	// 导致空流伪成功。纯内容裸 JSON（无 code / 空字符串 code）按安全策略忽略。
	if code.Exists() && ((code.Type == gjson.Number && code.Int() != 0) || (code.Type == gjson.String && code.String() != "")) {
		return []byte(trimmed), true
	}
	return nil, false
}

// webKimiClientChunkEnvelope 以 OpenAI chat.completion.chunk 形状构造一帧回_client 包络。
// content 为正文增量，reasoningContent 为思考增量（reasoning_content 字段，DeepSeek/
// Kimi 思考模式同口径）；二者可单独或同时出现。终帧由调用方单独写 data: [DONE]。
func webKimiClientChunkEnvelope(responseID, originalModel, content, reasoningContent, finishReason string, usage *OpenAIUsage) gin.H {
	delta := gin.H{}
	if content != "" {
		delta["content"] = content
	}
	if reasoningContent != "" {
		delta["reasoning_content"] = reasoningContent
	}
	choice := gin.H{"index": 0, "delta": delta, "finish_reason": finishReason}
	chunk := gin.H{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   originalModel,
		"choices": []gin.H{choice},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	return chunk
}

// handleWebKimiStreamingResponse 流式回程：解析 Connect RPC envelope 流（op/mask/eventOffset），
// 重包为 OpenAI chat.completion.chunk 流回写客户端；正文走 block.text.content、思考走
// block.think.content（reasoning_content）；终止为 done 帧 + message.status=COMPLETED。
//
// 登录态实测（10 §4）：心跳帧跳过、done 帧终止。usage 流内无（实测 0 处），
// 与非流式路径同口径走 estimateWebUsage 本地估算兜底（流式此前恒为 nil，
// 导致 token 计费模式下真实零计费——扣费门禁见 gateway_usage_billing.go）。
// 首帧即携带 unauthenticated 错误时，按上游错误路径处理。
//
// 读侧三件套（interval 超时 / 下游 keepalive / ctx 可唤醒）由 webStreamGuard 统一承担，
// 与 anthropic 回程 handleStreamingResponse 同构，详见 web_protocol_bridge.go。
//
// ── 整改2：读取可取消 / 计费不可取消的分离设计 ──
// 出站请求 ctx 走 detachUpstreamContext（= context.WithoutCancel），见本文件
// forwardWebKimi。它**不是**为了让计费落库在客户端断开后仍能完成——计费落库的
// 取消独立性由三处各自的 detached 上下文保证，与上游读 ctx 无关：
//   - gateway_usage_billing.go detachedBillingContext：WithoutCancel + 15s 超时，
//     供 postUsageBilling / applyUsageBilling / writeUsageLogBestEffort 使用；
//   - handler submitUsageRecordTask 的同步回退路径：context.Background()+10s。
//
// 因此「读取阶段可取消」不会连带取消计费落库，两阶段可以安全解耦。
//
// 具体做法：上游读的 ctx 仍是 WithoutCancel（保持响应提交后的排水语义），
// 但读动作被拆进守卫 goroutine，主循环用 select 观察 ctx.Done()——客户端断开
// 能立刻唤醒主循环、停止向下游写；同时不掐断上游读，继续把 usage 采完
// （与 anthropic 回程 clientDisconnected 后 drain 同构）。最终释放由 interval
// 超时兜底：上游挂死时最多 interval（默认 180s）后强制收口，goroutine 归还。
func (s *OpenAIGatewayService) handleWebKimiStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	upstreamModel string,
	startTime time.Time,
	inputPrompt string,
	mode webResponseMode,
) (*OpenAIForwardResult, error) {
	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	responseID := ""
	var usage *OpenAIUsage
	var textAggregated strings.Builder
	written := false
	finishReason := "stop"
	midstreamErr := false
	midstreamErrMsg := ""
	st := newWebClientStreamState(mode, originalModel)

	reader := bufio.NewReader(resp.Body)
	// 读侧守卫：把阻塞读搬进独立 goroutine，使主循环能 select 到
	// 「数据 / 间隔超时 / keepalive / ctx 取消」。readFn 每次读一帧 Connect
	// envelope 并返回其 payload；more=false（正常 EOF 或截断）按 io.EOF 上抛，
	// 由守卫视作流结束。底层 io.ReadFull 无 deadline，超时兜底只能在此层做。
	guardParams := s.webStreamGuardParams(func() ([]byte, error) {
		payload, more, err := readWebKimiConnectEnvelope(reader)
		if err != nil {
			return nil, err
		}
		if !more {
			return nil, io.EOF
		}
		// 零长帧（Connect keepalive）：readWebKimiConnectEnvelope 以
		// (nil, true, nil) 表示「跳过本帧、继续读下一帧」。守卫对零长帧
		// 统一跳过且不重置 interval 计时，故此处原样返回即可。
		return payload, nil
	})
	guard := newWebStreamGuard(guardParams)
	defer guard.stop()

	lastDataAt := time.Now()
	clientDisconnected := false
	// ctxDoneCh 是 ctx.Done() 的一次性 latch：客户端断开触发一次后即置 nil，
	// 该 case 在 select 中永久变为「不就绪」。直接监听 ctx.Done() 会热自旋——
	// 取消后该 channel 永久就绪，select 立刻命中、空转、立刻再命中，最坏
	// （客户端断开 + 上游挂死）可占满单核 CPU 直到 interval 兜底（默认 180s）。
	// anthropic 回程回程 select 同样不监听 ctx.Done()：断开靠下游写失败
	// （writeWebStreamChunk/writeWebStreamKeepalive 返错）感知。这里保留
	// ctxDoneCh 而非直接删 case，是为了在「尚未写出首帧、下游一次都没写」的
	// 窗口里也能立刻停止向下游写——那时写失败路径还不会被触发。
	ctxDoneCh := ctx.Done()
	// 具名循环：select 内的 break 只跳出 select，中断收口（业务错误 / done 帧）
	// 必须跳出整个读循环，故一律用 break streamLoop。
streamLoop:
	for {
		select {
		case <-ctxDoneCh:
			// 客户端断开：不立刻掐断上游读——响应已提交，计费需要把 usage 采完
			// （与 anthropic 回程 clientDisconnected 后继续 drain 同构）。interval
			// 超时是这次排水的最终上界，故此处不 return，只停止向下游写。
			// 置 nil 是一次性 latch，绝不能 continue（continue 会让 select 立刻
			// 再次命中同一就绪 channel，退化成满 CPU 自旋）。
			clientDisconnected = true
			ctxDoneCh = nil

		case ev, ok := <-guard.events:
			if !ok {
				break streamLoop
			}
			if ev.err != nil {
				if isOpenAICNFirstByteTimeout(ev.err) && !written {
					return nil, s.failoverOpenAICNFirstByteTimeout(ctx, account, upstreamModel)
				}
				recordWebStreamReadError(c, account, upstreamModel, ev.err)
				return nil, fmt.Errorf("kimi web stream read: %w", ev.err)
			}
			lastDataAt = time.Now()
			guard.resetKeepalive()

			ev2 := parseWebKimiEnvelopePayload(ev.data)
			if ev2.Heartbeat {
				continue
			}
			if ev2.AssistantID != "" {
				responseID = ev2.AssistantID
			}
			// 业务/认证错误：首帧未写任何客户端字节（written=false）走完整错误路径；
			// 流中后段已写出正文（written=true）则记 ops 错误并向客户端写流内 error
			// 标记后中断收口，绝不伪造正常结束。
			if ev2.AuthFailed || ev2.ErrCode != 0 || ev2.ErrCodeStr != "" {
				if !written {
					errResp := &http.Response{
						StatusCode: resp.StatusCode,
						Header:     resp.Header,
						Body:       io.NopCloser(bytes.NewReader(ev.data)),
					}
					return s.handleWebKimiUpstreamError(ctx, c, account, errResp, ev.data, upstreamModel)
				}
				// 凭证红线：与 handleWebKimiUpstreamError 同口径，先把可能被上游回显在
				// trailer 里的 access_token / refresh_token 擦掉，再提取错误文案。本分支
				// 不走 handleWebKimiUpstreamError，缺这一步会把凭证原文写进客户端 SSE
				// error 帧与 ops_error_logs。
				opsMsg := sanitizeUpstreamErrorMessage(webKimiUpstreamErrorMessage(redactWebKimiUpstreamErrorBody(ev.data, account)))
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					Kind:               "midstream_error",
					Message:            opsMsg,
				})
				midstreamErr = true
				midstreamErrMsg = opsMsg
				break streamLoop
			}
			if ev2.Done {
				break streamLoop
			}
			if ev2.ThinkDelta != "" {
				written = true
				if err := writeWebStreamChunk(c, st, webKimiClientChunkEnvelope(responseID, originalModel, "", ev2.ThinkDelta, "", nil)); err != nil {
					clientDisconnected = true
					continue
				}
			}
			if ev2.TextDelta != "" {
				textAggregated.WriteString(ev2.TextDelta)
				written = true
				if err := writeWebStreamChunk(c, st, webKimiClientChunkEnvelope(responseID, originalModel, ev2.TextDelta, "", "", nil)); err != nil {
					clientDisconnected = true
					continue
				}
			}

		case <-guard.intervalCh:
			// 上游开了流却在 interval 内未送来任何字节：判定断流。已写出正文时
			// 不伪造正常终止，交由下方收口；未写出时按流中断语义返回错误。
			if !guard.streamIdle(guardParams.interval) {
				continue
			}
			if clientDisconnected {
				return nil, fmt.Errorf("kimi web stream read: %w", errWebStreamIdleTimeout)
			}
			slog.Warn("web kimi stream data interval timeout",
				"account_id", account.ID,
				"model", originalModel,
				"upstream_model", upstreamModel,
				"interval", guardParams.interval.String())
			recordWebStreamReadError(c, account, upstreamModel, errWebStreamIdleTimeout)
			return nil, fmt.Errorf("kimi web stream read: %w", errWebStreamIdleTimeout)

		case <-guard.keepaliveCh:
			// 仅在尚未写出首帧时保活：首帧前的思考/检索阶段上游可能长时间静默，
			// 此刻保活才能防住 Cloudflare Tunnel / 反代按空闲断开连接；已出首帧
			// 后保活帧只会污染 TTFT 观测（与 anthropic 回程 data != "" 判定同构）。
			if clientDisconnected || !webStreamKeepalivePending(written, guardParams.keepalive, lastDataAt) {
				guard.resetKeepalive()
				continue
			}
			if err := writeWebStreamKeepalive(c); err != nil {
				clientDisconnected = true
				continue
			}
			lastDataAt = time.Now()
			guard.resetKeepalive()
		}
	}

	// 流中段业务错误收口：已向客户端写出过正文，上游突发业务错误。记录 ops 后向客户端
	// 写明确 error 标记并终结 SSE（不伪造正常 finish_reason+usage 终止帧）。HTTP 状态可能
	// 已是 200，但流内 error 标记使客户端能区分「正常完成」与「中途失败」。
	if midstreamErr {
		if err := writeWebStreamMidstreamError(c, st, midstreamErrMsg); err != nil {
			return nil, err
		}
		MarkResponseCommitted(c)
		return &OpenAIForwardResult{
			UpstreamHeaders:  resp.Header,
			ResponseID:       responseID,
			Model:            originalModel,
			UpstreamModel:    upstreamModel,
			UpstreamEndpoint: webKimiChatPath,
			Stream:           true,
			ResponseHeaders:  resp.Header.Clone(),
			Duration:         time.Since(startTime),
		}, nil
	}

	// 流式 usage 估算兜底（整改3）：Connect envelope 流内实测无 usage 字段，
	// usage 恒为 nil。token 计费模式下这会让 InputTokens/OutputTokens 全 0，
	// 计费金额为 0，扣费门禁（gateway_usage_billing.go）整条拦下——真实零计费。
	// 此处与非流式路径（handleWebKimiNonStreamingResponse）同口径兜底：仅当
	// 两个方向都为 0 且账号为 web 接入模式时，用入参 prompt 与聚合正文估算。
	// 与非流式的差别仅在输出文本来源——流式用 textAggregated（已聚合的正文增量）。
	finalUsage := usage
	if finalUsage == nil {
		finalUsage = &OpenAIUsage{}
	}
	if finalUsage.InputTokens == 0 && finalUsage.OutputTokens == 0 && account.IsWebAccessMode() {
		estimated := estimateWebUsage(inputPrompt, textAggregated.String())
		finalUsage.InputTokens = estimated.InputTokens
		finalUsage.OutputTokens = estimated.OutputTokens
	}

	// 正常终止帧：写出终帧（finish_reason + usage），再按出站协议收口写 data: [DONE]。
	//
	// 客户端断开后不再向下游写，但**必须**继续走完收口并返回带 usage 的结果：
	// 响应早已提交、上游已真实消耗额度，此时因写失败 return nil 会把 usage 整体
	// 丢掉 → 平台漏记（与 anthropic 回程 clientDisconnected 后仍 submitUsage 同理）。
	if clientDisconnected {
		MarkResponseCommitted(c)
		return &OpenAIForwardResult{
			UpstreamHeaders:  resp.Header,
			ResponseID:       responseID,
			Usage:            *finalUsage,
			Model:            originalModel,
			UpstreamModel:    upstreamModel,
			UpstreamEndpoint: webKimiChatPath,
			Stream:           true,
			ResponseHeaders:  resp.Header.Clone(),
			Duration:         time.Since(startTime),
			ClientDisconnect: true,
		}, nil
	}
	if err := writeWebStreamChunk(c, st, webKimiClientChunkEnvelope(responseID, originalModel, "", "", finishReason, finalUsage)); err != nil {
		return nil, err
	}
	if err := finalizeWebStream(c, st); err != nil {
		return nil, err
	}
	MarkResponseCommitted(c)

	result := &OpenAIForwardResult{
		UpstreamHeaders:  resp.Header,
		ResponseID:       responseID,
		Usage:            *finalUsage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: webKimiChatPath,
		Stream:           true,
		ResponseHeaders:  resp.Header.Clone(),
		Duration:         time.Since(startTime),
		ClientDisconnect: clientDisconnected,
	}
	return result, nil
}

// handleWebKimiNonStreamingResponse 非流式回程：读取完整上游响应，解析 Connect RPC
// envelope 流聚合为单条 chat.completion JSON。正文来自 block.text.content（思考不进正文）；
// chat.id / assistant message.id 提取为响应 id；终止为 done 帧。usage 流内无（实测 0 处）
// 走本地估算兜底。
//
// 兼容兜底：若整体不是 envelope 流（frames=false，例如 Connect RPC 单 JSON 响应或错误体），
// 回落到单 JSON 提取（message.blocks.0.content.value.content / content）与认证错误判定。
func (s *OpenAIGatewayService) handleWebKimiNonStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	upstreamModel string,
	startTime time.Time,
	inputPrompt string,
	mode webResponseMode,
) (*OpenAIForwardResult, error) {
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		// CN 首包超时：非流式缓冲路径响应头尚未提交，可安全透明切换（冷却+换号，带模型）。
		if isOpenAICNFirstByteTimeout(err) {
			return nil, s.failoverOpenAICNFirstByteTimeout(ctx, account, upstreamModel)
		}
		return nil, err
	}

	responseID := ""
	var usage *OpenAIUsage
	var textAggregated strings.Builder
	frames := false

	reader := bufio.NewReader(bytes.NewReader(body))
	for {
		payload, more, rerr := readWebKimiConnectEnvelope(reader)
		if rerr != nil {
			return nil, fmt.Errorf("kimi web upstream response read: %w", rerr)
		}
		if !more {
			break
		}
		frames = true
		ev := parseWebKimiEnvelopePayload(payload)
		if ev.Heartbeat {
			continue
		}
		if ev.AssistantID != "" {
			responseID = ev.AssistantID
		}
		if ev.ChatID != "" && responseID == "" {
			responseID = ev.ChatID
		}
		if ev.AuthFailed || ev.ErrCode != 0 || ev.ErrCodeStr != "" {
			errResp := &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header,
				Body:       io.NopCloser(bytes.NewReader(payload)),
			}
			return s.handleWebKimiUpstreamError(ctx, c, account, errResp, payload, upstreamModel)
		}
		textAggregated.WriteString(ev.TextDelta)
	}

	if !frames {
		// 非 envelope：Connect RPC 单 JSON 响应或错误体——回落到单 JSON 提取与认证错误判定。
		if classifyWebKimiUpstreamError(resp.StatusCode, body) == webKimiErrKindAuth {
			errResp := &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header,
				Body:       io.NopCloser(bytes.NewReader(body)),
			}
			return s.handleWebKimiUpstreamError(ctx, c, account, errResp, body, upstreamModel)
		}
		content := strings.TrimSpace(gjson.GetBytes(body, "message.blocks.0.content.value.content").String())
		if content == "" {
			content = strings.TrimSpace(gjson.GetBytes(body, "content").String())
		}
		if content == "" && resp.StatusCode < 400 {
			return nil, errors.New(
				"kimi web upstream returned an unrecognized non-stream response shape (pending logged-in traffic capture)")
		}
		textAggregated.WriteString(content)
		if id := strings.TrimSpace(gjson.GetBytes(body, "id").String()); id != "" {
			responseID = id
		}
	}

	text := strings.TrimSpace(textAggregated.String())
	if text == "" && resp.StatusCode < 400 {
		return nil, errors.New(
			"kimi web upstream returned an unrecognized non-stream response shape (pending logged-in traffic capture)")
	}

	finalUsage := usage
	if finalUsage == nil {
		finalUsage = &OpenAIUsage{}
	}
	// 网页逆向平台上游不返回 usage 时本地估算（D2），避免计费为 0；仅估算兜底，
	// 非真实 token 数（estimated）。归并后判定源为 web 接入模式（平台已并官方值）。
	if finalUsage.InputTokens == 0 && finalUsage.OutputTokens == 0 && account.IsWebAccessMode() {
		estimated := estimateWebUsage(inputPrompt, text)
		finalUsage.InputTokens = estimated.InputTokens
		finalUsage.OutputTokens = estimated.OutputTokens
	}
	completion := gin.H{
		"id":      responseID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   originalModel,
		"choices": []gin.H{{
			"index":         0,
			"message":       gin.H{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": finalUsage,
	}
	if err := writeWebCompletion(c, newWebClientStreamState(mode, originalModel), completion); err != nil {
		return nil, err
	}
	MarkResponseCommitted(c)

	return &OpenAIForwardResult{
		UpstreamHeaders:  resp.Header,
		ResponseID:       responseID,
		Usage:            *finalUsage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: webKimiChatPath,
		Stream:           false,
		ResponseHeaders:  resp.Header.Clone(),
		Duration:         time.Since(startTime),
	}, nil
}
