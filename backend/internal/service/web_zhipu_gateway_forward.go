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

// web-zhipu 网页逆向适配器（方案 W3，docs/web-reverse-embedded-login-plan.md §4.2、
// docs/web-reverse-analysis-plan.md §1.2/§3.3/§3.4）。
//
// 协议状态声明（权威来源：两份方案文档的实测记录，2026-09-16）：
//   - 已实测：对话端点 POST /chatglm/backend-api/v1/conversation（SSE，未登录 401
//     "You need to be authenticated"）、Cookie 认证 + X-Requested-With: XMLHttpRequest、
//     阿里 CDN Cookie（acw_tc / cdn_sec_tc）、模型名前端硬编码（glm-4 等，不走
//     model_version 端点）。
//   - 未实测（登录态缺失）：完整请求体、SSE chunk 结构、Cookie 有效期。
//     本文件对全部未知格式做集中封装并标注「待登录态实测补全」，不做任何臆测编造。
//
// 安全红线：凭证（cookie）不得出现在日志或错误响应中——上游错误体在任何透传前先经
// redactWebZhipuUpstreamErrorBody 脱敏。
//
// 刷新语义（分析文档 §3.5）：Zhipu 无刷新机制，Cookie 过期即冷却——401 不做刷新重试，
// 直接走冷却与错误回传。

const (
	// webZhipuClientUA 指纹对齐用浏览器 UA（方案 §3.5；具体 UA 以登录态抓包为准）。
	webZhipuClientUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	// webZhipuConversationPath 对话端点（SSE，实测）。
	webZhipuConversationPath = "/chatglm/backend-api/v1/conversation"
)

// forwardWebZhipu 是 Zhipu 网页逆向平台（web-zhipu）的转发入口，函数链模式与
// forwardCodeBuddy / forwardWebDeepseek 同构：入站 OpenAI Chat Completions → 网页端
// 请求（SSE）→ 回程通用 SSE 解析 → OpenAI 形状回写。挂载点由 OpenAIGatewayService
// 的 platform 分支按 PlatformWebZhipu 分发（分发注册由共享注册点接线任务完成）。
func (s *OpenAIGatewayService) forwardWebZhipu(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
	mode webResponseMode,
) (*OpenAIForwardResult, error) {
	if account == nil || account.Platform != PlatformWebZhipu {
		return nil, fmt.Errorf("forwardWebZhipu requires a %s account", PlatformWebZhipu)
	}

	cookie := strings.TrimSpace(account.GetCredential("cookie"))
	if cookie == "" {
		// 网页账号登录态载体就是整串 Cookie（credentials_sanitize.go 对 web 平台的例外语义）。
		return nil, errors.New("web-zhipu account is missing login cookie credential")
	}

	// base_url 统一走 account.GetWebBaseURL()（覆盖优先 → 平台默认），避免内联重复实现漂移。
	baseURL := strings.TrimRight(account.GetWebBaseURL(), "/")
	if baseURL == "" {
		return nil, errors.New("web-zhipu account has no base_url")
	}

	// 模型映射：account.GetModelMapping() 默认透传（GetMappedModel 未命中即原样返回）。
	// 网页端模型名前端硬编码（实测：glm-4 等），未知模型名按「默认透传同名」处理，
	// 真实取值待登录态实测补全。
	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = originalModel
	}
	SetOpsUpstreamModel(c, upstreamModel)

	prompt := webZhipuExtractPrompt(body)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("web-zhipu requires at least one user message in the request")
	}
	upstreamBody := buildWebZhipuRequestBody(prompt, upstreamModel, account)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()

	req, err := s.buildWebZhipuUpstreamRequest(upstreamCtx, account, baseURL+webZhipuConversationPath, cookie, upstreamBody)
	if err != nil {
		return nil, err
	}
	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(startTime).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		return s.handleWebZhipuUpstreamError(ctx, c, account, resp, respBody, upstreamModel)
	}

	if reqStream {
		return s.handleWebZhipuStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, mode)
	}
	return s.handleWebZhipuNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, webZhipuExtractPrompt(body), mode)
}

// buildWebZhipuRequestBody 把入站 prompt 转换为网页端对话请求体。
// 实测已知字段仅 prompt / model（分析文档 §1.2）；完整请求体待登录态实测补全，
// 此处只设已实测字段，其余一律不设（不臆测填充）。
type webZhipuUpstreamRequest struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
}

func buildWebZhipuRequestBody(prompt, model string, account *Account) []byte {
	req := webZhipuUpstreamRequest{
		Prompt: prompt,
		Model:  model,
	}
	data, _ := json.Marshal(req)
	return data
}

// webZhipuExtractPrompt 从入站 OpenAI 请求取最后一条 user 消息文本作为 prompt
// （网页端单 prompt 语义；多轮上下文展开方式待登录态实测补全）。
func webZhipuExtractPrompt(body []byte) string {
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
		// 多模态数组形态：取 text 片段拼接。
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

// buildWebZhipuUpstreamRequest 构造网页端出站请求（指纹头对齐方案 §3.5）。
//
// Cookie 同串携带：登录 Cookie 为整串（通常已含阿里 CDN Cookie acw_tc/cdn_sec_tc）；
// 如管理员把 CDN Cookie 单独存入 credentials["cdn_cookie"]，则追加到同一 Cookie 串。
// X-Requested-With: XMLHttpRequest 为实测已知要求。
func (s *OpenAIGatewayService) buildWebZhipuUpstreamRequest(
	ctx context.Context,
	account *Account,
	targetURL string,
	cookie string,
	body []byte,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	origin := webZhipuOriginFromURL(targetURL)
	req.Header.Set("Content-Type", "application/json")
	// 对话端点为 SSE（分析文档 §1.2 实测）；Accept 精确形态待登录态实测补全。
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", webZhipuClientUA)

	fullCookie := cookie
	if cdn := strings.TrimSpace(account.GetCredential("cdn_cookie")); cdn != "" {
		fullCookie = strings.TrimRight(fullCookie, "; ") + "; " + cdn
	}
	req.Header.Set("Cookie", fullCookie)

	// 账号级请求头覆写最后应用，使管理员配置优先（与 CodeBuddy 出站口径一致）。
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

func webZhipuOriginFromURL(targetURL string) string {
	origin := DefaultWebZhipuBaseURL
	if i := strings.Index(strings.TrimPrefix(targetURL, "https://"), "/"); i >= 0 {
		origin = "https://" + strings.TrimPrefix(targetURL, "https://")[:i]
	}
	return origin
}

// webZhipuErrKind Zhipu 网页端上游错误分类（分析文档 §3.4 冷却触发：401 / 429）。
type webZhipuErrKind int

const (
	webZhipuErrKindOther webZhipuErrKind = iota
	webZhipuErrKindAuth
	webZhipuErrKindRateLimited
)

// classifyWebZhipuUpstreamError 纯函数错误分类：401 → 认证类（Cookie 过期即冷却，
// 无刷新机制）；429 → 限流类。
func classifyWebZhipuUpstreamError(statusCode int, body []byte) webZhipuErrKind {
	switch statusCode {
	case http.StatusUnauthorized:
		return webZhipuErrKindAuth
	case http.StatusTooManyRequests:
		return webZhipuErrKindRateLimited
	}
	return webZhipuErrKindOther
}

// redactWebZhipuUpstreamErrorBody 上游错误体脱敏：清除可能被上游回显的凭证片段
// （整串 cookie / cdn_cookie），保证凭证不进入日志或下游错误响应。
func redactWebZhipuUpstreamErrorBody(body []byte, account *Account) []byte {
	if len(body) == 0 || account == nil {
		return body
	}
	text := string(body)
	for _, key := range []string{"cookie", "cdn_cookie"} {
		if v := strings.TrimSpace(account.GetCredential(key)); len(v) >= 8 {
			text = strings.ReplaceAll(text, v, "***")
		}
	}
	return []byte(text)
}

// handleWebZhipuUpstreamError 对上游错误做分类、脱敏、限流副作用（复用
// RateLimitService.HandleUpstreamError 的 CN 供应商语义：401/429 → 冷却），最后经
// handleErrorResponse 把错误回传客户端。
func (s *OpenAIGatewayService) handleWebZhipuUpstreamError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	respBody []byte,
	upstreamModel string,
) (*OpenAIForwardResult, error) {
	respBody = redactWebZhipuUpstreamErrorBody(respBody, account)
	if resp != nil {
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	upstreamMsg := webZhipuUpstreamErrorMessage(respBody)
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("web-zhipu upstream returned status %d", resp.StatusCode)
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

	// 冷却副作用由 handleErrorResponse 内部的 handleOpenAIAccountUpstreamError 单点
	// 施加（与 forwardCodeBuddy 模板同口径），此处不重复调用，避免双重冷却。
	return s.handleErrorResponse(ctx, resp, c, account, respBody, upstreamModel)
}

// webZhipuUpstreamErrorMessage 提取上游错误文案；实测形态为裸文本
// （"You need to be authenticated"），其余回落通用提取。
func webZhipuUpstreamErrorMessage(body []byte) string {
	if m := strings.TrimSpace(gjson.GetBytes(body, "msg").String()); m != "" {
		return m
	}
	if m := strings.TrimSpace(gjson.GetBytes(body, "message").String()); m != "" {
		return m
	}
	// 裸文本错误体（实测 401 形态）。
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" && !gjson.Valid(trimmed) {
		return sanitizeUpstreamErrorMessage(trimmed)
	}
	return sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
}

// webZhipuChunkView 通用 SSE JSON 载荷的解析视图。
// 字段映射集中在此（待登录态实测补全）：只识别 OpenAI 兼容形状与网页端常见的
// 顶层 content 字段，绝不编造未实测的 Zhipu 专有字段。
type webZhipuChunkView struct {
	Content      string
	FinishReason string
	Usage        *OpenAIUsage
	ResponseID   string
	ErrCode      int64
}

// parseWebZhipuSSEFrame 解析单行 SSE 帧，返回 data 载荷（标准 SSE 单行 JSON 处理；
// Zhipu 具体 SSE 形态待登录态实测补全）。
func parseWebZhipuSSEFrame(line string) ([]byte, bool) {
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

// webZhipuBareJSONError 判定一行裸 JSON（非 SSE data: 前缀）是否为业务错误：携带非 0
// 的 code 字段时按业务错误返回其载荷，否则 isError=false。与 webDeepseekBareJSONError
// 同口径（#3）。
func webZhipuBareJSONError(line string) (payload []byte, isError bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "data:") {
		return nil, false
	}
	if !gjson.Valid(trimmed) {
		return nil, false
	}
	code := gjson.GetBytes([]byte(trimmed), "code")
	if code.Exists() && code.Type == gjson.Number && code.Int() != 0 {
		return []byte(trimmed), true
	}
	return nil, false
}

// mapWebZhipuPayload 把一帧通用 JSON 载荷映射为 webZhipuChunkView。
//
// 待登录态实测补全：以下识别顺序按「通用 SSE JSON」由强到弱排列——
//  1. OpenAI 兼容：choices[0].delta.content（流式增量）；
//  2. OpenAI 兼容：choices[0].message.content（末帧整段形态）；
//  3. 网页端常见：顶层 content 字符串字段；
//  4. usage：OpenAI 命名（prompt_tokens/completion_tokens）或 input_tokens/output_tokens。
func mapWebZhipuPayload(payload []byte) webZhipuChunkView {
	v := gjson.ParseBytes(payload)
	view := webZhipuChunkView{}
	if id := strings.TrimSpace(v.Get("id").String()); id != "" {
		view.ResponseID = id
	}
	switch {
	case v.Get("choices.0.delta.content").Exists():
		view.Content = v.Get("choices.0.delta.content").String()
	case v.Get("choices.0.message.content").Exists():
		view.Content = v.Get("choices.0.message.content").String()
	case v.Get("content").Type == gjson.String:
		view.Content = v.Get("content").String()
	}
	if fr := v.Get("choices.0.finish_reason"); fr.Exists() && fr.Type == gjson.String {
		view.FinishReason = fr.String()
	}
	if code := v.Get("code"); code.Exists() && code.Type == gjson.Number {
		view.ErrCode = code.Int()
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
	return view
}

// writeWebZhipuClientChunk 以 OpenAI chat.completion.chunk 形状向客户端写一帧 SSE。
// 帧必须带标准 SSE 前缀 `data: `（C1，#3）：标准 OpenAI SDK 只解析 `data: ` 前缀的帧，
// 缺少前缀会被整帧忽略。终帧由调用方单独写 `data: [DONE]\n\n`。
func writeWebZhipuClientChunk(c *gin.Context, chunk gin.H) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	frame := append([]byte("data: "), data...)
	frame = append(frame, '\n', '\n')
	if _, err := c.Writer.Write(frame); err != nil {
		return err
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func webZhipuClientChunkEnvelope(responseID, originalModel string, delta gin.H, finishReason string, usage *OpenAIUsage) gin.H {
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

// handleWebZhipuStreamingResponse 流式回程：对上游 SSE 做通用 JSON 增量解析，重包为
// OpenAI chat.completion.chunk 流回写客户端，终止写 data: [DONE]。
//
// 未实测部分（待登录态实测补全）：chunk 具体字段结构、终止条件、usage 出现位置。
func (s *OpenAIGatewayService) handleWebZhipuStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	upstreamModel string,
	startTime time.Time,
	mode webResponseMode,
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
	finishReason := ""
	written := false
	midstreamErr := false
	midstreamErrMsg := ""
	st := newWebClientStreamState(mode, originalModel)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	for scanner.Scan() {
		payload, ok := parseWebZhipuSSEFrame(scanner.Text())
		if !ok {
			// #3 业务错误判定：上游 SSE 帧或裸 JSON 携带业务错误码（非 0 code）时，
			// 首帧未写任何客户端字节走完整错误路径；流中段已写出正文则记 ops 错误并向
			// 客户端写流内 error 标记后中断收口，不伪造成功。纯内容裸 JSON（无 code）
			// 按安全策略忽略。
			if errPayload, isErr := webZhipuBareJSONError(scanner.Text()); isErr {
				if !written {
					errResp := &http.Response{
						StatusCode: resp.StatusCode,
						Header:     resp.Header,
						Body:       io.NopCloser(bytes.NewReader(errPayload)),
					}
					return s.handleWebZhipuUpstreamError(ctx, c, account, errResp, errPayload, upstreamModel)
				}
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					Kind:               "midstream_error",
					Message:            sanitizeUpstreamErrorMessage(webZhipuUpstreamErrorMessage(errPayload)),
				})
				// 流中后段已写出正文（written=true）：记 ops 错误并向客户端写流内 error
				// 标记后中断收口，不伪造正常 finish_reason+usage 终止帧。
				midstreamErr = true
				midstreamErrMsg = sanitizeUpstreamErrorMessage(webZhipuUpstreamErrorMessage(errPayload))
				break
			}
			continue
		}
		view := mapWebZhipuPayload(payload)
		if view.ResponseID != "" && responseID == "" {
			responseID = view.ResponseID
		}
		if view.ErrCode != 0 {
			if !written {
				errResp := &http.Response{
					StatusCode: resp.StatusCode,
					Header:     resp.Header,
					Body:       io.NopCloser(bytes.NewReader(payload)),
				}
				return s.handleWebZhipuUpstreamError(ctx, c, account, errResp, payload, upstreamModel)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				Kind:               "midstream_error",
				Message:            sanitizeUpstreamErrorMessage(webZhipuUpstreamErrorMessage(payload)),
			})
			// 流中后段已写出正文（written=true）：记 ops 错误并向客户端写流内 error
			// 标记后中断收口，不伪造正常 finish_reason+usage 终止帧。
			midstreamErr = true
			midstreamErrMsg = sanitizeUpstreamErrorMessage(webZhipuUpstreamErrorMessage(payload))
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
		if err := writeWebStreamChunk(c, st, webZhipuClientChunkEnvelope(responseID, originalModel, gin.H{"content": view.Content}, "", nil)); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("web-zhipu stream read: %w", err)
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
			UpstreamEndpoint: webZhipuConversationPath,
			Stream:           true,
			ResponseHeaders:  resp.Header.Clone(),
			Duration:         time.Since(startTime),
		}, nil
	}

	// 正常终止帧：finish_reason（若观测到）+ usage（若观测到），再按出站协议收口。
	if err := writeWebStreamChunk(c, st, webZhipuClientChunkEnvelope(responseID, originalModel, gin.H{}, finishReason, usage)); err != nil {
		return nil, err
	}
	if err := finalizeWebStream(c, st); err != nil {
		return nil, err
	}
	MarkResponseCommitted(c)

	result := &OpenAIForwardResult{
		UpstreamHeaders:  resp.Header,
		ResponseID:       responseID,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: webZhipuConversationPath,
		Stream:           true,
		ResponseHeaders:  resp.Header.Clone(),
		Duration:         time.Since(startTime),
	}
	if usage != nil {
		result.Usage = *usage
	}
	return result, nil
}

// handleWebZhipuNonStreamingResponse 非流式回程：读取完整上游响应。SSE 时经通用映射
// 聚合为单条 chat.completion JSON；结构不可识别时失败关闭并给出明确错误
// （不伪造成功响应）。
func (s *OpenAIGatewayService) handleWebZhipuNonStreamingResponse(
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
		return nil, err
	}

	responseID := ""
	var usage *OpenAIUsage
	var aggregated strings.Builder
	frames := false
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		payload, ok := parseWebZhipuSSEFrame(scanner.Text())
		if !ok {
			continue
		}
		frames = true
		view := mapWebZhipuPayload(payload)
		if view.ResponseID != "" && responseID == "" {
			responseID = view.ResponseID
		}
		if view.Usage != nil {
			usage = view.Usage
		}
		aggregated.WriteString(view.Content)
	}
	if err := scanner.Err(); err != nil {
		// 扫描中途失败不应静默当作成功：如实返回错误而非继续解析残帧。
		return nil, fmt.Errorf("web-zhipu upstream response scan failed: %w", err)
	}

	if !frames {
		// 非 SSE：结构未识别（完整请求体/响应体均待登录态实测补全）→ 失败关闭。
		return nil, errors.New(
			"web-zhipu upstream returned an unrecognized non-stream response shape (pending logged-in traffic capture)")
	}

	finalUsage := usage
	if finalUsage == nil {
		finalUsage = &OpenAIUsage{}
	}
	// 网页逆向平台上游不返回 usage 时本地估算（D2），避免计费为 0；仅估算兜底，
	// 非真实 token 数（estimated）。
	if finalUsage.InputTokens == 0 && finalUsage.OutputTokens == 0 && IsWebProvider(account.Platform) {
		estimated := estimateWebUsage(inputPrompt, aggregated.String())
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
			"message":       gin.H{"role": "assistant", "content": aggregated.String()},
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
		UpstreamEndpoint: webZhipuConversationPath,
		Stream:           false,
		ResponseHeaders:  resp.Header.Clone(),
		Duration:         time.Since(startTime),
	}, nil
}
