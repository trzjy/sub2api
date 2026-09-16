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
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// web-deepseek 网页逆向适配器（方案 W4，docs/web-reverse-embedded-login-plan.md §4.1、
// docs/web-reverse-analysis-plan.md §1.1/§3.3/§3.4）。
//
// 协议状态声明（权威来源：两份方案文档的实测记录，2026-09-16）：
//   - 已实测：对话端点 POST /api/v0/chat/completion（SSE）、PoW 挑战端点
//     POST /api/v0/chat/create_pow_challenge、WAF Cookie（HWWAFSESID/HWWAFSESTIME）、
//     关键请求字段 chat_session_id / parent_message_id / ref_file_ids / model_class /
//     prompt / thinking_enabled / search_enabled、业务错误码形态 {"code":40002,"msg":"..."}。
//   - 未实测（登录态缺失）：PoW 计算格式、SSE chunk 结构、登录后 Token 获取流程。
//     本文件对全部未知格式做集中封装并标注「待登录态实测补全」，不做任何臆测编造；
//     PoW 求解当前失败关闭（ErrWebDeepseekPoWNotImplemented）。
//
// 安全红线：凭证（cookie / waf_cookie）不得出现在日志或错误响应中——上游错误体在
// 任何透传前先经 redactWebDeepseekUpstreamErrorBody 脱敏。

const (
	// webDeepseekDefaultBaseURL 默认官方网页端域名（分析文档 §1.1 实测）。
	webDeepseekDefaultBaseURL = "https://chat.deepseek.com"
	// webDeepseekChatCompletionPath 对话端点（SSE，实测）。
	webDeepseekChatCompletionPath = "/api/v0/chat/completion"
	// webDeepseekPoWChallengePath PoW 挑战获取端点（实测，登录态下返回 challenge，格式未实测）。
	webDeepseekPoWChallengePath = "/api/v0/chat/create_pow_challenge"
	// webDeepseekClientUA 指纹对齐用浏览器 UA（方案 §3.5：请求头严格模拟官方网页端；
	// 具体 UA 以登录态抓包为准，当前为通用现代浏览器形态）。
	webDeepseekClientUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// ErrWebDeepseekPoWNotImplemented PoW 求解未实现（挑战计算格式未实测）。
// 在登录态实测补全之前，任何要求 PoW 的请求一律失败关闭，绝不臆测算法。
var ErrWebDeepseekPoWNotImplemented = errors.New(
	"web-deepseek: PoW challenge solving is not implemented (pending logged-in traffic capture)")

// forwardWebDeepseek 是 DeepSeek 网页逆向平台（web-deepseek）的转发入口，函数链模式
// 与 forwardCodeBuddy 同构：入站 OpenAI Chat Completions → 网页端请求（SSE）→ 回程
// 通用 SSE 解析 → OpenAI 形状回写。挂载点由 OpenAIGatewayService.Forward 的 platform
// 分支按 PlatformWebDeepseek 分发（该分发注册由并行任务落在共享注册点文件中）。
func (s *OpenAIGatewayService) forwardWebDeepseek(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
	mode webResponseMode,
) (*OpenAIForwardResult, error) {
	if account == nil || account.Platform != PlatformWebDeepseek {
		return nil, fmt.Errorf("forwardWebDeepseek requires a %s account", PlatformWebDeepseek)
	}

	cookie := strings.TrimSpace(account.GetCredential("cookie"))
	if cookie == "" {
		// 网页账号登录态载体就是整串 Cookie（credentials_sanitize.go 对 web 平台的例外语义）。
		return nil, errors.New("web-deepseek account is missing login cookie credential")
	}

	// base_url 统一走 account.GetWebBaseURL()（覆盖优先 → 平台默认），避免内联重复实现漂移。
	baseURL := strings.TrimRight(account.GetWebBaseURL(), "/")
	if baseURL == "" {
		return nil, errors.New("web-deepseek account has no base_url")
	}

	// 模型映射：account.GetModelMapping() 默认透传（GetMappedModel 未命中即原样返回），
	// 再做 model_class 归一（方案 §3.3 模型映射表；未知模型名按「默认透传同名」处理）。
	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = originalModel
	}
	modelClass, thinkingEnabled := webDeepseekModelClass(upstreamModel)
	SetOpsUpstreamModel(c, upstreamModel)

	upstreamBody, err := buildWebDeepseekRequestBody(body, account, modelClass, thinkingEnabled)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	// PoW：按实测已知端点取 challenge（data.biz_data.challenge），求解后随对话
	// 请求携带 x-ds-pow-response 头（公开实现相互印证，登录态抓包最终确认）。
	// 上游未给出可用 challenge（如未登录态 40002 / 结构未识别）时按无 PoW 出站。
	powHeader, err := s.fetchWebDeepseekPoWHeader(ctx, account, baseURL, cookie, proxyURL)
	if err != nil {
		return nil, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()

	req, err := s.buildWebDeepseekUpstreamRequest(upstreamCtx, account, baseURL+webDeepseekChatCompletionPath, cookie, upstreamBody)
	if err != nil {
		return nil, err
	}
	if powHeader != "" {
		req.Header.Set("X-Ds-PoW-Response", powHeader)
	}
	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(startTime).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		return s.handleWebDeepseekUpstreamError(ctx, c, account, resp, respBody, upstreamModel)
	}

	if reqStream {
		return s.handleWebDeepseekStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, mode)
	}
	return s.handleWebDeepseekNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, webDeepseekExtractPrompt(body), mode)
}

// webDeepseekModelClass 把（已映射的）模型名归一为网页端 model_class 与 thinking 开关。
//
// 方案 §3.3（分析文档）唯一实测映射：deepseek-chat → deepseek_chat；
// deepseek-reasoner → deepseek_chat + thinking_enabled=true；默认透传同名。
// 除 deepseek_chat 外的 model_class 取值未实测，透传行为待登录态实测补全。
func webDeepseekModelClass(model string) (modelClass string, thinkingEnabled bool) {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "deepseek-chat", "deepseek_chat":
		return "deepseek_chat", false
	case "deepseek-reasoner", "deepseek_reasoner":
		return "deepseek_chat", true
	default:
		return model, false
	}
}

// buildWebDeepseekRequestBody 把入站 OpenAI Chat Completions 请求体转换为网页端
// 对话请求体。字段仅使用分析文档 §1.1 实测已知字段；未实测字段一律不设。
type webDeepseekUpstreamRequest struct {
	ChatSessionID   string `json:"chat_session_id,omitempty"`
	ParentMessageID string `json:"parent_message_id,omitempty"`
	// RefFileIDs 入站 OpenAI 请求不含文件引用，恒缺省（omitempty）；结构待登录态实测补全。
	RefFileIDs      []any  `json:"ref_file_ids,omitempty"`
	ModelClass      string `json:"model_class"`
	Prompt          string `json:"prompt"`
	ThinkingEnabled bool   `json:"thinking_enabled"`
	SearchEnabled   bool   `json:"search_enabled"`
}

func buildWebDeepseekRequestBody(
	inboundBody []byte,
	account *Account,
	modelClass string,
	thinkingEnabled bool,
) ([]byte, error) {
	// 会话字段：credentials 可选覆盖（chat_session_id / parent_message_id）。
	// 会话创建端点 /api/v0/chat_session/create 已实测存在但登录态行为未实测，
	// 此处不主动建会话，待登录态实测补全后再决定是否自动创建。
	req := webDeepseekUpstreamRequest{
		ChatSessionID:   strings.TrimSpace(account.GetCredential("chat_session_id")),
		ParentMessageID: strings.TrimSpace(account.GetCredential("parent_message_id")),
		ModelClass:      modelClass,
		Prompt:          webDeepseekExtractPrompt(inboundBody),
		ThinkingEnabled: thinkingEnabled,
		// search_enabled 恒 false：入站协议无对应开关，是否支持联网待登录态实测补全。
		SearchEnabled: false,
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("web-deepseek requires at least one user message in the request")
	}
	return json.Marshal(req)
}

// webDeepseekExtractPrompt 从入站 OpenAI 请求取最后一条 user 消息文本作为 prompt
// （网页端单 prompt 语义，实测已知字段仅 prompt；多轮上下文展开方式待登录态实测补全）。
func webDeepseekExtractPrompt(body []byte) string {
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

// buildWebDeepseekUpstreamRequest 构造网页端出站请求（指纹头对齐方案 §3.5）。
//
// Cookie 同串携带：登录 Cookie 为整串（通常已含 WAF Cookie HWWAFSESID/HWWAFSESTIME）；
// 如管理员把 WAF Cookie 单独存入 credentials["waf_cookie"]，则追加到同一 Cookie 串。
//
// PoW 携带头名称未实测：当前求解未实现（ErrWebDeepseekPoWNotImplemented），落地时
// 需一并实测确认头名并在此补全，绝不臆测。
func (s *OpenAIGatewayService) buildWebDeepseekUpstreamRequest(
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

	origin := webDeepseekOriginFromURL(targetURL)
	req.Header.Set("Content-Type", "application/json")
	// 对话端点为 SSE（分析文档 §1.1 实测）；Accept 精确形态待登录态实测补全。
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", webDeepseekClientUA)

	fullCookie := cookie
	if waf := strings.TrimSpace(account.GetCredential("waf_cookie")); waf != "" {
		fullCookie = strings.TrimRight(fullCookie, "; ") + "; " + waf
	}
	req.Header.Set("Cookie", fullCookie)

	// 账号级请求头覆写最后应用，使管理员配置优先（与 CodeBuddy 出站口径一致）。
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

func webDeepseekOriginFromURL(targetURL string) string {
	parsed, err := url.Parse(targetURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return webDeepseekDefaultBaseURL
	}
	return parsed.Scheme + "://" + parsed.Host
}

// fetchWebDeepseekPoWHeader 取 challenge 并求解，返回 x-ds-pow-response 头值。
// 挑战端点不可达或响应无可用 challenge（未登录态 40002 / 结构未识别）时返回空串，
// 调用方按无 PoW 继续出站；求解失败（nonce 未收敛）失败关闭返回错误。
func (s *OpenAIGatewayService) fetchWebDeepseekPoWHeader(
	ctx context.Context,
	account *Account,
	baseURL string,
	cookie string,
	proxyURL string,
) (string, error) {
	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()

	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, baseURL+webDeepseekPoWChallengePath, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	origin := webDeepseekOriginFromURL(baseURL)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", webDeepseekClientUA)
	req.Header.Set("Cookie", cookie)

	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	if err != nil {
		// 挑战端点不可达不阻断出站尝试：是否强制 PoW 待登录态实测补全。
		return "", nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", nil
	}

	challenge, ok := webDeepseekExtractPoWChallenge(body)
	if !ok {
		return "", nil
	}
	return webDeepseekSolvePoW(challenge, webDeepseekChatCompletionPath)
}

// webDeepseekErrKind DeepSeek 网页端上游错误分类（方案分析文档 §3.4 冷却触发：429 / code=40002）。
type webDeepseekErrKind int

const (
	webDeepseekErrKindOther webDeepseekErrKind = iota
	webDeepseekErrKindRateLimited
)

// classifyWebDeepseekUpstreamError 纯函数错误分类：HTTP 429 或业务码 code=40002
// （实测形态 {"code":40002,"msg":"Missing Token"}，HTTP 200 亦可能返回）→ 限流类。
func classifyWebDeepseekUpstreamError(statusCode int, body []byte) webDeepseekErrKind {
	if statusCode == http.StatusTooManyRequests {
		return webDeepseekErrKindRateLimited
	}
	if gjson.GetBytes(body, "code").Int() == 40002 {
		return webDeepseekErrKindRateLimited
	}
	return webDeepseekErrKindOther
}

// redactWebDeepseekUpstreamErrorBody 上游错误体脱敏：清除可能被上游回显的凭证片段
// （整串 cookie / waf_cookie），保证凭证不进入日志或下游错误响应。
func redactWebDeepseekUpstreamErrorBody(body []byte, account *Account) []byte {
	if len(body) == 0 || account == nil {
		return body
	}
	text := string(body)
	for _, key := range []string{"cookie", "waf_cookie"} {
		if v := strings.TrimSpace(account.GetCredential(key)); len(v) >= 8 {
			text = strings.ReplaceAll(text, v, "***")
		}
	}
	return []byte(text)
}

// handleWebDeepseekUpstreamError 对上游错误做分类、脱敏，最后经 handleErrorResponse
// 把错误回传客户端。限流副作用复用 CN 供应商语义：handleErrorResponse 内部的
// handleOpenAIAccountUpstreamError 会以（归一化后的）状态码调用
// RateLimitService.HandleUpstreamError（429 → 冷却，401 → 认证处置），此处不重复调用。
func (s *OpenAIGatewayService) handleWebDeepseekUpstreamError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	respBody []byte,
	upstreamModel string,
) (*OpenAIForwardResult, error) {
	kind := classifyWebDeepseekUpstreamError(resp.StatusCode, respBody)
	respBody = redactWebDeepseekUpstreamErrorBody(respBody, account)
	if resp != nil {
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	if kind == webDeepseekErrKindRateLimited && resp.StatusCode < 400 {
		// 实测形态：业务错误码随 HTTP 200 返回（{"code":40002,"msg":"..."}）；
		// 冷却语义按 429 归一（分析文档 §3.4 冷却触发：429 / code=40002）。
		resp.StatusCode = http.StatusTooManyRequests
	}
	upstreamMsg := webDeepseekUpstreamErrorMessage(respBody)
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("web-deepseek upstream returned status %d", resp.StatusCode)
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
	return s.handleErrorResponse(ctx, resp, c, account, respBody, upstreamModel)
}

// webDeepseekUpstreamErrorMessage 提取上游错误文案：实测形态为 {"code":...,"msg":"..."}，
// 其余回落通用提取。
func webDeepseekUpstreamErrorMessage(body []byte) string {
	if m := strings.TrimSpace(gjson.GetBytes(body, "msg").String()); m != "" {
		return sanitizeUpstreamErrorMessage(m)
	}
	return sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
}

// webDeepseekChunkView 通用 SSE JSON 载荷的解析视图。
// 字段映射集中在此（待登录态实测补全）：只识别 OpenAI 兼容形状与网页端常见的
// 顶层 content 字段，绝不编造未实测的 DeepSeek 专有字段。
type webDeepseekChunkView struct {
	Content      string
	FinishReason string
	Usage        *OpenAIUsage
	ResponseID   string
	ErrCode      int64
	ErrMsg       string
}

// parseWebDeepseekSSEFrame 解析单行 SSE 帧，返回 data 载荷。
// 标准 SSE 允许多行 data（待登录态实测补全是否出现）；当前按单行 JSON 处理。
func parseWebDeepseekSSEFrame(line string) ([]byte, bool) {
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

// webDeepseekBareJSONError 判定一行裸 JSON（非 SSE data: 前缀）是否为业务错误：
// 携带非 0 的 code 字段（实测 40002 形态）按业务错误返回其载荷，否则返回 isError=false。
// 纯内容裸 JSON（无 code）交由调用方按安全策略忽略。仅解析合法 JSON，杜绝对无法识别
// 行的臆测处理。
func webDeepseekBareJSONError(line string) (payload []byte, isError bool) {
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

// mapWebDeepseekPayload 把一帧通用 JSON 载荷映射为 webDeepseekChunkView。
//
// 待登录态实测补全：以下识别顺序按「通用 SSE JSON」由强到弱排列——
//  1. OpenAI 兼容：choices[0].delta.content（流式增量）；
//  2. OpenAI 兼容：choices[0].message.content（末帧整段形态）；
//  3. 网页端常见：顶层 content 字符串字段；
//  4. usage：OpenAI 命名（prompt_tokens/completion_tokens）或 input_tokens/output_tokens；
//  5. 业务错误码：code（非 0）/msg（实测 40002 形态）。
func mapWebDeepseekPayload(payload []byte) webDeepseekChunkView {
	v := gjson.ParseBytes(payload)
	view := webDeepseekChunkView{}
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
	if usage := v.Get("usage"); usage.IsObject() {
		u := &OpenAIUsage{
			InputTokens:  int(usage.Get("prompt_tokens").Int()) + int(usage.Get("input_tokens").Int()),
			OutputTokens: int(usage.Get("completion_tokens").Int()) + int(usage.Get("output_tokens").Int()),
		}
		if u.InputTokens > 0 || u.OutputTokens > 0 {
			view.Usage = u
		}
	}
	if code := v.Get("code"); code.Exists() && code.Type == gjson.Number {
		view.ErrCode = code.Int()
	}
	if m := v.Get("msg"); m.Type == gjson.String {
		view.ErrMsg = m.String()
	}
	return view
}

// writeWebDeepseekClientChunk 以 OpenAI chat.completion.chunk 形状向客户端写一帧 SSE。
// 帧必须带标准 SSE 前缀 `data: `（C1，#3）：标准 OpenAI SDK 只解析 `data: ` 前缀的帧，
// 缺少前缀会被整帧忽略。终帧由调用方单独写 `data: [DONE]\n\n`。
func writeWebDeepseekClientChunk(c *gin.Context, chunk gin.H) error {
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

func webDeepseekClientChunkEnvelope(responseID, originalModel string, delta gin.H, finishReason string, usage *OpenAIUsage) gin.H {
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

// handleWebDeepseekStreamingResponse 流式回程：对上游 SSE 做通用 JSON 增量解析，
// 重包为 OpenAI chat.completion.chunk 流回写客户端，终止写 data: [DONE]。
//
// 未实测部分（待登录态实测补全）：chunk 具体字段结构、终止条件、usage 出现位置。
// 首帧即携带业务错误码（实测 {"code":40002,"msg":"..."} 形态）时，按上游错误路径处理。
// writeWebStreamMidstreamError 流式中段业务错误收口（三平台统一模式）：流已向客户端写出
// 过正文（HTTP 状态可能已是 200），此时上游突发业务错误——不能伪造正常 finish_reason+usage
// 终止帧把失败请求当成功流处理。按出站协议向客户端写一帧明确的 error 标记，再按模式发出流
// 终止符，使客户端能区分「正常完成」与「中途失败」：
//   - chat 模式：data: {"error":{...}} 后补 data: [DONE]（闭合 SSE）；
//   - responses / anthropic 模式：event: error 事件本身即终结（不写 response.completed /
//     message_stop 等成功终止事件）。
//
// 与仓库 antigravity_gateway_compat_stream.go 的 WriteError 惯例对齐。message 经
// sanitizeUpstreamErrorMessage 脱敏，绝不写入任何凭证值。
func writeWebStreamMidstreamError(c *gin.Context, st *webClientStreamState, message string) error {
	safe := sanitizeUpstreamErrorMessage(message)
	if safe == "" {
		safe = "upstream midstream business error"
	}
	switch st.mode {
	case webResponseModeChat:
		if _, err := c.Writer.WriteString(fmt.Sprintf(
			"data: {\"error\":{\"message\":%q,\"type\":\"upstream_error\"}}\n\n", safe)); err != nil {
			return err
		}
		if _, err := c.Writer.WriteString("data: [DONE]\n\n"); err != nil {
			return err
		}
	case webResponseModeResponses, webResponseModeAnthropic:
		if _, err := c.Writer.WriteString(fmt.Sprintf(
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"upstream_error\",\"message\":%q}}\n\n", safe)); err != nil {
			return err
		}
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (s *OpenAIGatewayService) handleWebDeepseekStreamingResponse(
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
	written := false
	finishReason := ""
	midstreamErr := false
	midstreamErrMsg := ""
	st := newWebClientStreamState(mode, originalModel)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	for scanner.Scan() {
		payload, ok := parseWebDeepseekSSEFrame(scanner.Text())
		if !ok {
			// #3 裸 JSON 业务错误判定：上游 HTTP 200 返回 {"code":40002,...}（非 SSE、
			// 无 data: 前缀）时，原 parseWebDeepseekSSEFrame 忽略该行会漏判为正常流，
			// 最终以 [DONE] 伪成功。含非 0 code 字段的裸 JSON 行按业务错误处理。
			// 首帧未写任何客户端字节（written=false）→ 走完整错误路径（可改写 4xx 状态码）；
			// 流中后段已写出正文（written=true）→ 记 ops 错误并向客户端写流内 error 标记后
			// 中断收口（writeWebStreamMidstreamError），不伪造正常 finish_reason+usage 终止帧。
			if errPayload, isErr := webDeepseekBareJSONError(scanner.Text()); isErr {
				if !written {
					errResp := &http.Response{
						StatusCode: resp.StatusCode,
						Header:     resp.Header,
						Body:       io.NopCloser(bytes.NewReader(errPayload)),
					}
					return s.handleWebDeepseekUpstreamError(ctx, c, account, errResp, errPayload, upstreamModel)
				}
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					Kind:               "midstream_error",
					Message:            sanitizeUpstreamErrorMessage(webDeepseekUpstreamErrorMessage(errPayload)),
				})
				midstreamErr = true
				midstreamErrMsg = sanitizeUpstreamErrorMessage(webDeepseekUpstreamErrorMessage(errPayload))
				break
			}
			continue
		}
		view := mapWebDeepseekPayload(payload)
		if view.ResponseID != "" && responseID == "" {
			responseID = view.ResponseID
		}
		if view.ErrCode != 0 {
			// 业务错误码（实测 40002 形态）。首帧未写任何客户端字节（written=false）→
			// 走完整错误路径（可改写 4xx 状态码）；流中后段已写出正文（written=true，罕见
			// 未实测）→ 记 ops 错误并向客户端写流内 error 标记后中断收口，绝不伪造正常结束。
			if !written {
				errResp := &http.Response{
					StatusCode: resp.StatusCode,
					Header:     resp.Header,
					Body:       io.NopCloser(bytes.NewReader(payload)),
				}
				return s.handleWebDeepseekUpstreamError(ctx, c, account, errResp, payload, upstreamModel)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				Kind:               "midstream_error",
				Message:            sanitizeUpstreamErrorMessage(view.ErrMsg),
			})
			midstreamErr = true
			midstreamErrMsg = sanitizeUpstreamErrorMessage(view.ErrMsg)
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
		if err := writeWebStreamChunk(c, st, webDeepseekClientChunkEnvelope(responseID, originalModel, gin.H{"content": view.Content}, "", nil)); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("web-deepseek stream read: %w", err)
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
			UpstreamEndpoint: webDeepseekChatCompletionPath,
			Stream:           true,
			ResponseHeaders:  resp.Header.Clone(),
			Duration:         time.Since(startTime),
		}, nil
	}

	// 正常终止帧：finish_reason（若观测到）+ usage（若观测到），再按出站协议收口。
	if err := writeWebStreamChunk(c, st, webDeepseekClientChunkEnvelope(responseID, originalModel, gin.H{}, finishReason, usage)); err != nil {
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
		UpstreamEndpoint: webDeepseekChatCompletionPath,
		Stream:           true,
		ResponseHeaders:  resp.Header.Clone(),
		Duration:         time.Since(startTime),
	}
	if usage != nil {
		result.Usage = *usage
	}
	return result, nil
}

// handleWebDeepseekNonStreamingResponse 非流式回程：读取完整上游响应。SSE 时经通用
// 映射聚合为单条 chat.completion JSON；纯 JSON 且携带业务错误码（实测 40002 形态）
// 时走上游错误路径；结构不可识别时失败关闭并给出明确错误（不伪造成功响应）。
func (s *OpenAIGatewayService) handleWebDeepseekNonStreamingResponse(
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
		payload, ok := parseWebDeepseekSSEFrame(scanner.Text())
		if !ok {
			continue
		}
		frames = true
		view := mapWebDeepseekPayload(payload)
		if view.ErrCode != 0 {
			errResp := &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header,
				Body:       io.NopCloser(bytes.NewReader(body)),
			}
			return s.handleWebDeepseekUpstreamError(ctx, c, account, errResp, body, upstreamModel)
		}
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
		return nil, fmt.Errorf("web-deepseek upstream response scan failed: %w", err)
	}

	if !frames {
		// 非 SSE：业务错误码形态（HTTP 200 + {"code":40002,...}）走错误路径；
		// 其余未识别结构失败关闭，待登录态实测补全后再扩展。
		if classifyWebDeepseekUpstreamError(resp.StatusCode, body) == webDeepseekErrKindRateLimited {
			errResp := &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header,
				Body:       io.NopCloser(bytes.NewReader(body)),
			}
			return s.handleWebDeepseekUpstreamError(ctx, c, account, errResp, body, upstreamModel)
		}
		return nil, errors.New(
			"web-deepseek upstream returned an unrecognized non-stream response shape (pending logged-in traffic capture)")
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
		UpstreamEndpoint: webDeepseekChatCompletionPath,
		Stream:           false,
		ResponseHeaders:  resp.Header.Clone(),
		Duration:         time.Since(startTime),
	}, nil
}
