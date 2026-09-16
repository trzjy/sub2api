package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// web-kimi 网页逆向适配器（方案 W2，docs/web-reverse-embedded-login-plan.md §4.3、
// docs/web-reverse-analysis-plan.md §1.3/§3.3/§3.4）。
//
// 协议状态声明（权威来源：两份方案文档的实测记录，2026-09-16）：
//   - 已实测：对话端点 POST /apiv2/kimi.chat.v1.ChatService/Chat（Connect RPC，401
//     unauthenticated）、模型列表端点（免登录，k3 / k3-agent-ultra）、Token 刷新端点
//     POST auth.kimi.com/api/account.gateway.v1.AuthService/RefreshToken、设备注册端点、
//     已验证请求体结构（方案 §4.3：chatId / kimiPlusId / scenario / projectId / tools /
//     message.blocks / options.thinking+model）。
//   - 未实测（登录态缺失）：SSE chunk 结构、刷新成功响应结构、TrustDecision 设备指纹要求。
//     本文件对全部未知格式做集中封装并标注「待登录态实测补全」，不做任何臆测编造。
//
// 安全红线：凭证（access_token / refresh_token）不得出现在日志或错误响应中——上游错误体
// 在任何透传前先经 redactWebKimiUpstreamErrorBody 脱敏。

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

// forwardWebKimi 是 Kimi 网页逆向平台（web-kimi）的转发入口，函数链模式与
// forwardCodeBuddy / forwardWebDeepseek 同构：入站 OpenAI Chat Completions → Connect
// RPC 请求 → 回程通用解析 → OpenAI 形状回写。挂载点由 OpenAIGatewayService.Forward
// 的 platform 分支按 PlatformWebKimi 分发（分发注册由共享注册点接线任务完成）。
func (s *OpenAIGatewayService) forwardWebKimi(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if account == nil || account.Platform != PlatformWebKimi {
		return nil, fmt.Errorf("forwardWebKimi requires a %s account", PlatformWebKimi)
	}

	accessToken := strings.TrimSpace(account.GetCredential("access_token"))
	if accessToken == "" {
		return nil, errors.New("web-kimi account is missing access_token credential")
	}

	// base_url 统一走 account.GetWebBaseURL()（覆盖优先 → 平台默认），避免内联重复实现漂移。
	baseURL := strings.TrimRight(account.GetWebBaseURL(), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("web-kimi account has no base_url (platform %s)", account.Platform)
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
		return nil, errors.New("web-kimi requires at least one user message in the request")
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
	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(startTime).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
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
			retryResp, retryErr := s.doOpenAIUpstream(retryReq, proxyURL, account)
			if retryErr != nil {
				return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, retryErr, false)
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
		return s.handleWebKimiStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime)
	}
	return s.handleWebKimiNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime)
}

// webKimiModelName 把（已映射的）模型名归一为网页端模型名。
// 方案 §3.3（分析文档）唯一实测映射：kimi-k3 → k3、kimi-k3-agent-ultra → k3-agent-ultra；
// 默认透传同名。其余取值待登录态实测补全。
func webKimiModelName(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "kimi-k3", "k3":
		return "k3"
	case "kimi-k3-agent-ultra", "k3-agent-ultra":
		return "k3-agent-ultra"
	default:
		return model
	}
}

// buildWebKimiRequestBody 按方案 §4.3 已验证请求结构构建 Connect RPC 请求体。
// 仅使用已验证字段；未实测字段一律缺省（不臆测填充）。
type webKimiTextBlockValue struct {
	TypeName string `json:"$typeName"`
	Content  string `json:"content"`
}

type webKimiBlockContent struct {
	Case  string               `json:"case"`
	Value webKimiTextBlockValue `json:"value"`
}

type webKimiBlock struct {
	ID        string              `json:"id"`
	ParentID  string              `json:"parentId"`
	MessageID string              `json:"messageId"`
	Content   webKimiBlockContent `json:"content"`
}

type webKimiRefs struct {
	TypeName string `json:"$typeName"`
}

type webKimiMessage struct {
	ID                  string      `json:"id"`
	ParentID            string      `json:"parentId"`
	ChildrenMessageIDs  []any       `json:"childrenMessageIds"`
	Role                int         `json:"role"`
	Blocks              []webKimiBlock `json:"blocks"`
	Scenario            string      `json:"scenario"`
	Labels              []any       `json:"labels"`
	References          webKimiRefs `json:"references"`
}

type webKimiRequestOptions struct {
	TypeName string `json:"$typeName"`
	// Thinking 网页端样例为 true，但入站协议到 thinking 开关的映射未实测：
	// 当前恒 false（保守默认），映射规则待登录态实测补全。
	Thinking bool   `json:"thinking"`
	Model    string `json:"model"`
}

type webKimiUpstreamRequest struct {
	ChatID     string               `json:"chatId"`
	KimiPlusID string               `json:"kimiPlusId"`
	Scenario   string               `json:"scenario"`
	ProjectID  string               `json:"projectId"`
	Tools      []any                `json:"tools"`
	Message    webKimiMessage       `json:"message"`
	Options    webKimiRequestOptions `json:"options"`
}

func buildWebKimiRequestBody(prompt, webModel string, account *Account) []byte {
	req := webKimiUpstreamRequest{
		// kimiPlusId=ok-computer 为方案 §4.3 已验证样例值（免费长上下文口径）。
		KimiPlusID: "ok-computer",
		Tools:      []any{},
		Message: webKimiMessage{
			Role:               1,
			ChildrenMessageIDs: []any{},
			Labels:             []any{},
			Blocks: []webKimiBlock{{
				Content: webKimiBlockContent{
					Case: "text",
					Value: webKimiTextBlockValue{
						TypeName: "kimi.chat.v1.TextBlock",
						Content:  prompt,
					},
				},
			}},
			References: webKimiRefs{TypeName: "kimi.chat.v1.Refs"},
		},
		Options: webKimiRequestOptions{
			TypeName: "kimi.gateway.chat.v1.ChatRequestOptions",
			Thinking: false,
			Model:    webModel,
		},
	}
	// chatId/scenario/projectId 缺省：会话管理语义未实测，待登录态实测补全。
	// credentials 可选覆盖 chat_id（管理员显式配置时透传）。
	if chatID := strings.TrimSpace(account.GetCredential("chat_id")); chatID != "" {
		req.ChatID = chatID
	}
	data, _ := json.Marshal(req)
	return data
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

// buildWebKimiUpstreamRequest 构造 Connect RPC 出站请求（指纹头对齐方案 §3.5）。
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

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", webKimiDefaultBaseURL)
	req.Header.Set("Referer", webKimiDefaultBaseURL+"/")
	req.Header.Set("User-Agent", webKimiClientUA)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	// 账号级请求头覆写最后应用，使管理员配置优先（与 CodeBuddy 出站口径一致）。
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// refreshWebKimiAccessToken 用 refresh_token 刷新 access_token。
//
// 刷新端点已实测存在（无效 token 时 401 "Session expired"），但成功响应结构未实测。
// 这里只识别 access_token 在顶层 / data / token 包装下的常见 JSON 位置；均未命中或
// 刷新失败时返回空串（调用方继续按原 401 走冷却与错误路径），绝不臆造响应语义。
// 刷新结果不回写 credentials（账号凭证更新属管理端职责，避免调度热路径写库）。
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

	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
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
	for _, path := range []string{"accessToken", "data.accessToken", "token", "data.token"} {
		if v := strings.TrimSpace(gjson.GetBytes(body, path).String()); v != "" {
			return v
		}
	}
	return ""
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
		upstreamMsg = fmt.Sprintf("web-kimi upstream returned status %d", resp.StatusCode)
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
func webKimiUpstreamErrorMessage(body []byte) string {
	if m := strings.TrimSpace(gjson.GetBytes(body, "message").String()); m != "" {
		return m
	}
	if m := strings.TrimSpace(gjson.GetBytes(body, "error.message").String()); m != "" {
		return m
	}
	return sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
}

// parseWebKimiSSEFrame 解析单行 SSE 帧，返回 data 载荷（标准 SSE 单行 JSON 处理；
// Kimi 具体 SSE 形态待登录态实测补全）。
func parseWebKimiSSEFrame(line string) ([]byte, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return nil, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return nil, false
	}
	if !gjson.Valid(payload) {
		return nil, false
	}
	return []byte(payload), true
}

// webKimiChunkView 通用 SSE JSON 载荷的解析视图。
// 字段映射集中在此（待登录态实测补全）：只识别 OpenAI 兼容形状与 Connect RPC 常见的
// 嵌套位置，绝不编造未实测的 Kimi 专有字段。
type webKimiChunkView struct {
	Content      string
	FinishReason string
	Usage        *OpenAIUsage
	ResponseID   string
	AuthFailed   bool
}

// mapWebKimiPayload 把一帧通用 JSON 载荷映射为 webKimiChunkView。
//
// 待登录态实测补全：以下识别顺序按「通用 SSE JSON」由强到弱排列——
//  1. OpenAI 兼容：choices[0].delta.content / choices[0].message.content；
//  2. Connect RPC 常见嵌套：message.blocks[].content.value.content（对话正文位置）；
//  3. 顶层 content 字符串；
//  4. usage：OpenAI 命名或 input_tokens/output_tokens；
//  5. 认证错误：code/message 含 unauthenticated。
func mapWebKimiPayload(payload []byte) webKimiChunkView {
	v := gjson.ParseBytes(payload)
	view := webKimiChunkView{}
	if id := strings.TrimSpace(v.Get("id").String()); id != "" {
		view.ResponseID = id
	}
	switch {
	case v.Get("choices.0.delta.content").Exists():
		view.Content = v.Get("choices.0.delta.content").String()
	case v.Get("choices.0.message.content").Exists():
		view.Content = v.Get("choices.0.message.content").String()
	case v.Get("message.blocks.0.content.value.content").Exists():
		view.Content = v.Get("message.blocks.0.content.value.content").String()
	case v.Get("content").Type == gjson.String:
		view.Content = v.Get("content").String()
	}
	if fr := v.Get("choices.0.finish_reason"); fr.Exists() && fr.Type == gjson.String {
		view.FinishReason = fr.String()
	}
	if usage := v.Get("usage"); usage.IsObject() {
		u := &OpenAIUsage{
			InputTokens:  int(usage.Get("prompt_tokens").Int()) + int(usage.Get("input_tokens").Int()),
			OutputTokens: int(usage.Get("completion_tokens").Int()) + int(usage.Get("output_tokens").Int()),
		}
		if u.InputTokens > 0 || u.OutputTokens > 0 {
			view.Usage = u
		}
	}
	code := strings.ToLower(strings.TrimSpace(v.Get("code").String()))
	msg := strings.ToLower(strings.TrimSpace(v.Get("message").String()))
	if strings.Contains(code, "unauthenticated") || strings.Contains(msg, "unauthenticated") {
		view.AuthFailed = true
	}
	return view
}

// writeWebKimiClientChunk 以 OpenAI chat.completion.chunk 形状向客户端写一帧 SSE。
func writeWebKimiClientChunk(c *gin.Context, chunk gin.H) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(append(data, '\n', '\n')); err != nil {
		return err
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func webKimiClientChunkEnvelope(responseID, originalModel string, delta gin.H, finishReason string, usage *OpenAIUsage) gin.H {
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

// handleWebKimiStreamingResponse 流式回程：对上游 SSE 做通用 JSON 增量解析，重包为
// OpenAI chat.completion.chunk 流回写客户端，终止写 data: [DONE]。
//
// 未实测部分（待登录态实测补全）：chunk 具体字段结构、终止条件、usage 出现位置。
// 首帧即携带 unauthenticated 错误时，按上游错误路径处理。
func (s *OpenAIGatewayService) handleWebKimiStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	upstreamModel string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}

	responseID := ""
	var usage *OpenAIUsage
	var aggregated strings.Builder
	written := false
	finishReason := ""

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	for scanner.Scan() {
		payload, ok := parseWebKimiSSEFrame(scanner.Text())
		if !ok {
			continue
		}
		view := mapWebKimiPayload(payload)
		if view.ResponseID != "" && responseID == "" {
			responseID = view.ResponseID
		}
		if view.AuthFailed {
			// 首帧未写任何客户端字节时走完整错误路径；流中后段出现（罕见，未实测）
			// 则记录 ops 错误后结束流，不伪造正常结束。
			if !written {
				errResp := &http.Response{
					StatusCode: resp.StatusCode,
					Header:     resp.Header,
					Body:       io.NopCloser(bytes.NewReader(payload)),
				}
				return s.handleWebKimiUpstreamError(ctx, c, account, errResp, payload, upstreamModel)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				Kind:               "midstream_error",
				Message:            "web-kimi midstream unauthenticated error",
			})
			break
		}
		if view.Usage != nil {
			usage = view.Usage
		}
		if view.FinishReason != "" {
			finishReason = view.FinishReason
		}
		if view.Content == "" {
			continue
		}
		aggregated.WriteString(view.Content)
		written = true
		if err := writeWebKimiClientChunk(c, webKimiClientChunkEnvelope(responseID, originalModel, gin.H{"content": view.Content}, "", nil)); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("web-kimi stream read: %w", err)
	}

	// 终止帧：finish_reason（若观测到）+ usage（若观测到），再收口 [DONE]。
	if err := writeWebKimiClientChunk(c, webKimiClientChunkEnvelope(responseID, originalModel, gin.H{}, finishReason, usage)); err != nil {
		return nil, err
	}
	if _, err := c.Writer.WriteString("data: [DONE]\n\n"); err != nil {
		return nil, err
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	MarkResponseCommitted(c)

	result := &OpenAIForwardResult{
		UpstreamHeaders:  resp.Header,
		ResponseID:       responseID,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: webKimiChatPath,
		Stream:           true,
		ResponseHeaders:  resp.Header.Clone(),
		Duration:         time.Since(startTime),
	}
	if usage != nil {
		result.Usage = *usage
	}
	return result, nil
}

// handleWebKimiNonStreamingResponse 非流式回程：读取完整上游响应。SSE 时经通用映射
// 聚合为单条 chat.completion JSON；纯 JSON 且携带 unauthenticated 时走上游错误路径；
// 结构不可识别时失败关闭并给出明确错误（不伪造成功响应）。
func (s *OpenAIGatewayService) handleWebKimiNonStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	upstreamModel string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}

	responseID := ""
	var usage *OpenAIUsage
	var aggregated strings.Builder
	frames := false
	for scanner := bufio.NewScanner(bytes.NewReader(body)); scanner.Scan(); {
		payload, ok := parseWebKimiSSEFrame(scanner.Text())
		if !ok {
			continue
		}
		frames = true
		view := mapWebKimiPayload(payload)
		if view.AuthFailed {
			errResp := &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header,
				Body:       io.NopCloser(bytes.NewReader(body)),
			}
			return s.handleWebKimiUpstreamError(ctx, c, account, errResp, body, upstreamModel)
		}
		if view.ResponseID != "" && responseID == "" {
			responseID = view.ResponseID
		}
		if view.Usage != nil {
			usage = view.Usage
		}
		aggregated.WriteString(view.Content)
	}

	if !frames {
		// 非 SSE：Connect RPC 单 JSON 响应——正文落在 message.blocks[].content.value.content
		// （已验证请求结构的对称位置，响应侧待登录态实测补全）；unauthenticated 走错误路径；
		// 其余未识别结构失败关闭，待登录态实测补全后再扩展。
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
				"web-kimi upstream returned an unrecognized non-stream response shape (pending logged-in traffic capture)")
		}
		aggregated.WriteString(content)
		if id := strings.TrimSpace(gjson.GetBytes(body, "id").String()); id != "" {
			responseID = id
		}
	}

	finalUsage := usage
	if finalUsage == nil {
		finalUsage = &OpenAIUsage{}
	}
	completion := gin.H{
		"id":      responseID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   originalModel,
		"choices": []gin.H{{
			"index":         0,
			"message":       gin.H{"role": "assistant", "content": aggregated.String()},
			"finish_reason": "stop",
		}},
		"usage": finalUsage,
	}
	data, err := json.Marshal(completion)
	if err != nil {
		return nil, err
	}
	c.Header("Content-Type", "application/json")
	c.Data(http.StatusOK, "application/json", data)
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
