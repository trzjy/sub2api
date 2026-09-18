package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// deepseek web 网页逆向适配器（协议重写：依据 09-deepseek-logged-in-probe.md 登录态实测）。
//
// 协议状态声明（权威来源：09 登录态实测 + 06 实施包 §A/§A'，全部来自真实抓包，不再臆测）：
//   - 认证：Cookie 载体（无 Authorization 头）+ x-client-* / x-device-id / x-ds-pow-response /
//     x-hif-dliq / x-hif-leim 头族（09 §2）。
//   - 每轮链：POST /api/v0/chat/create_pow_challenge（target_path）→ POST /api/v0/chat_session/create
//     （body {}）→ POST /api/v0/chat/completion（SSE）。网关无状态：每轮新建会话，parent_message_id
//     恒 null，历史多轮以单 prompt 拼接（09 §1/§4/§5）。
//   - 请求体字段：chat_session_id / parent_message_id(null) / model_type / prompt / ref_file_ids /
//     thinking_enabled / search_enabled(true) / source(缺省省略) / action(null) / preempt(false)。
//   - SSE：event:/data: 帧对；delta 为 JSON-Patch（p/o/v，省略形态沿用当前路径）；正文取 RESPONSE
//     fragment content，THINK 丢弃；usage 为 accumulated_token_usage（BATCH 更新，语义累计 token）；
//     终止 event:close / event:finish + response/status=FINISHED；无 [DONE]。
//   - 错误：HTTP 200 + {"code":0,...,"data":{"biz_code":N,"biz_msg","biz_data":null}} 双路径判定
//     （顶层 code 或 data.biz_code 非 0 均按业务错误）；WAF：x-amzn-waf-action / 405 失败关闭。
//
// 安全红线：凭证（cookie / waf_cookie）不得出现在日志或错误响应中——上游错误体在
// 任何透传前先经 redactWebDeepseekUpstreamErrorBody 脱敏。

const (
	// webDeepseekDefaultBaseURL 默认官方网页端域名（09 §2）。
	webDeepseekDefaultBaseURL = "https://chat.deepseek.com"
	// webDeepseekChatCompletionPath 对话端点（SSE，09 §1）。
	webDeepseekChatCompletionPath = "/api/v0/chat/completion"
	// webDeepseekPoWChallengePath PoW 挑战获取端点（09 §1，body {"target_path":...}）。
	webDeepseekPoWChallengePath = "/api/v0/chat/create_pow_challenge"
	// webDeepseekChatSessionCreatePath 会话创建端点（09 §1，body {}）。
	webDeepseekChatSessionCreatePath = "/api/v0/chat_session/create"
	// webDeepseekClientUA 指纹对齐用浏览器 UA（09 §2 实测请求头族）。
	webDeepseekClientUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// ErrWebDeepseekPoWNotImplemented PoW 求解不可用（挑战不可达 / 结构未识别 / 求解失败）。
// 登录态实测确认 PoW 强制（09 §3），因此任何不可解的 PoW 一律失败关闭，绝不臆测算法或绕过。
var ErrWebDeepseekPoWNotImplemented = errors.New(
	"deepseek web: PoW challenge is mandatory (logged-in capture) but could not be solved")

// forwardWebDeepseek 是 DeepSeek 网页逆向平台（deepseek web）的转发入口，函数链模式
// 与 forwardCodeBuddy 同构：入站 OpenAI Chat Completions → 网页端请求（SSE）→ 回程
// 通用 SSE 解析 → OpenAI 形状回写。挂载点由 OpenAIGatewayService 转发链的
// isWebDeepseekAccount 断言分发。
// assertWebDeepseekAccount 是 forwardWebDeepseek 入口的双断言 fail-closed（隔离红线）：
// 官方 deepseek 平台 + web access mode 双断言（平台归并后唯一形态，PR-4）。
// API 模式 deepseek 账号绝不进入网页协议链，web 模式 zhipu/kimi 账号绝不误入。
func assertWebDeepseekAccount(account *Account) error {
	if account == nil {
		return fmt.Errorf("forwardWebDeepseek requires a non-nil account")
	}
	if account.IsWebAccessMode() && account.IsDeepseek() {
		return nil
	}
	if !account.IsWebAccessMode() {
		return fmt.Errorf("forwardWebDeepseek requires web access mode, got %q", account.GetAccessMode())
	}
	return fmt.Errorf("forwardWebDeepseek requires a deepseek platform, got %q", account.Platform)
}

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
	if err := assertWebDeepseekAccount(account); err != nil {
		return nil, err
	}

	cookie := strings.TrimSpace(account.GetCredential("cookie"))
	if cookie == "" {
		// 网页账号登录态载体就是整串 Cookie（credentials_sanitize.go 对 web 平台的例外语义）。
		return nil, errors.New("deepseek web account is missing login cookie credential")
	}

	// base_url 统一走 account.GetWebBaseURL()（覆盖优先 → 平台默认），避免内联重复实现漂移。
	baseURL := strings.TrimRight(account.GetWebBaseURL(), "/")
	if baseURL == "" {
		return nil, errors.New("deepseek web account has no base_url")
	}

	// 模型映射：account.GetModelMapping() 默认透传；归一为网页端 model_type（09 §4 实测
	// deepseek-chat → "default"；deepseek-reasoner → "default" + thinking_enabled=true）。
	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = originalModel
	}
	modelType, thinkingEnabled := webDeepseekModelClass(upstreamModel)
	SetOpsUpstreamModel(c, upstreamModel)

	// PoW：登录态强制（09 §3）。取 challenge（target_path=/api/v0/chat/completion）并求解，
	// 失败一律 fail-closed（不再"无 PoW 继续出站"——实测证明强制）。
	powHeader, err := s.fetchWebDeepseekPoWHeader(ctx, account, baseURL, cookie, accountProxyURL(account))
	if err != nil {
		return nil, err
	}

	// 自动建会话：credentials 有 chat_session_id 覆盖则跳过创建；否则每轮新建（网关无状态）。
	sessionID, err := s.ensureWebDeepseekSession(ctx, account, baseURL, cookie, accountProxyURL(account))
	if err != nil {
		return nil, err
	}

	// 多轮上下文：入站 messages 全部文本拼接进单 prompt（网页端单 prompt 语义）；
	// parent_message_id 恒 null（无状态，不跨请求链式）。
	prompt := webDeepseekExtractPrompt(body)
	upstreamBody, err := buildWebDeepseekCompletionBody(account, sessionID, modelType, thinkingEnabled, prompt)
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
	resp, err := s.doOpenAIUpstream(req, accountProxyURL(account), account)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(startTime).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	// WAF 失败关闭（09/01 §8）：响应头含 x-amzn-waf-action，或状态 405/202（challenge/captcha）。
	// 不将 WAF 正文透传给客户端。
	if wafAction := resp.Header.Get("x-amzn-waf-action"); wafAction != "" ||
		resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusAccepted {
		return nil, fmt.Errorf(
			"deepseek web upstream returned a WAF challenge/captcha (x-amzn-waf-action=%q, status %d); failing closed without forwarding the body",
			wafAction, resp.StatusCode)
	}

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		return s.handleWebDeepseekUpstreamError(ctx, c, account, resp, respBody, upstreamModel)
	}

	if reqStream {
		return s.handleWebDeepseekStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, mode)
	}
	return s.handleWebDeepseekNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, webDeepseekExtractPrompt(body), mode)
}

// accountProxyURL 返回账号代理 URL（与旧实现一致；隔离取值避免重复内联）。
func accountProxyURL(account *Account) string {
	if account == nil || account.ProxyID == nil || account.Proxy == nil {
		return ""
	}
	return account.Proxy.URL()
}

// webDeepseekModelClass 把（已映射的）模型名归一为网页端 model_type 与 thinking 开关
// （字段名 model_type，09 §4 实测；旧名 model_class 在 main.js 中 0 次出现）。
//
// 实测映射：deepseek-chat → model_type "default"；deepseek-reasoner → "default" +
// thinking_enabled=true（thinking 字段 09 §4 确认存在）；默认透传同名（其它 model_type
// 值域 unverified）。返回值在此文件内按 model_type 语义使用；函数名保留以兼容
// account_test_service.go 的探活调用。
func webDeepseekModelClass(model string) (modelType string, thinkingEnabled bool) {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "deepseek-chat", "deepseek_chat":
		return "default", false
	case "deepseek-reasoner", "deepseek_reasoner":
		return "default", true
	default:
		return strings.TrimSpace(model), false
	}
}

// webDeepseekUpstreamRequest 网页端对话请求体（09 §4 实测原文）。
type webDeepseekUpstreamRequest struct {
	ChatSessionID   string  `json:"chat_session_id"`
	ParentMessageID *string `json:"parent_message_id"` // 首轮/无状态恒 null（JSON null，非 omitempty）
	RefFileIDs      []any   `json:"ref_file_ids"`
	ModelType       string  `json:"model_type"`
	Prompt          string  `json:"prompt"`
	Source          *string `json:"source,omitempty"` // 缺省省略（实测样例不含）
	ThinkingEnabled bool    `json:"thinking_enabled"`
	SearchEnabled   bool    `json:"search_enabled"` // 09 §4 实测默认 true
	Action          *string `json:"action"`         // action:null（实测）
	Preempt         bool    `json:"preempt"`
}

// buildWebDeepseekCompletionBody 构造网页端对话请求体（带显式 sessionID）。
// parent_message_id 与 action 恒为 JSON null（实测）；source 缺省省略；preempt false。
func buildWebDeepseekCompletionBody(
	account *Account,
	sessionID string,
	modelType string,
	thinkingEnabled bool,
	prompt string,
) ([]byte, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("deepseek web requires at least one user message in the request")
	}
	req := webDeepseekUpstreamRequest{
		ChatSessionID:   strings.TrimSpace(sessionID),
		ParentMessageID: nil, // 无状态每轮恒 null（实测 09 §4）
		RefFileIDs:      []any{},
		ModelType:       modelType,
		Prompt:          prompt,
		ThinkingEnabled: thinkingEnabled,
		SearchEnabled:   true, // 09 §4 实测默认 true
		Action:          nil,  // action:null（实测）
		Preempt:         false,
	}
	return json.Marshal(req)
}

// webDeepseekExtractPrompt 从入站 OpenAI 请求拼接全部 messages 文本为单 prompt（网页端单
// prompt 语义，09 §5 多轮以多段文本拼接进 prompt；parent_message_id 不跨请求链式）。
// content 支持字符串与多模态数组（取 text 片段）；各消息以换行分隔。
func webDeepseekExtractPrompt(body []byte) string {
	messages := gjson.GetBytes(body, "messages").Array()
	var b strings.Builder
	for _, m := range messages {
		content := m.Get("content")
		switch content.Type {
		case gjson.String:
			b.WriteString(content.String())
		case gjson.JSON:
			for _, part := range content.Array() {
				if part.Get("type").String() == "text" {
					b.WriteString(part.Get("text").String())
				}
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// ensureWebDeepseekSession 取得本轮对话的 chat_session_id：credentials 有覆盖则直接用
// （跳过创建）；否则 POST /api/v0/chat_session/create（body {}）→ data.biz_data.chat_session.id。
// 网关无状态：每轮新建会话，不跨请求持久化。
func (s *OpenAIGatewayService) ensureWebDeepseekSession(
	ctx context.Context,
	account *Account,
	baseURL string,
	cookie string,
	proxyURL string,
) (string, error) {
	if cred := strings.TrimSpace(account.GetCredential("chat_session_id")); cred != "" {
		return cred, nil
	}
	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()

	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, baseURL+webDeepseekChatSessionCreatePath, bytes.NewReader([]byte("{}")))
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
	webDeepseekApplyRequestHeaders(req, account)

	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	if err != nil {
		return "", fmt.Errorf("deepseek web session create transport error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("deepseek web session create read error: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("deepseek web session create failed: status %d", resp.StatusCode)
	}
	id := strings.TrimSpace(gjson.GetBytes(b, "data.biz_data.chat_session.id").String())
	if id == "" {
		return "", fmt.Errorf("deepseek web session create returned no chat_session.id")
	}
	return id, nil
}

// buildWebDeepseekUpstreamRequest 构造网页端出站请求（指纹头对齐 09 §2 实测头族）。
//
// Cookie 同串携带：登录 Cookie 为整串（通常已含 WAF Cookie HWWAFSESID/HWWAFSESTIME）；
// 如管理员把 WAF Cookie 单独存入 credentials["waf_cookie"]，则追加到同一 Cookie 串。
// 账号级请求头覆写最后应用，使管理员配置优先（与 CodeBuddy 出站口径一致）。
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
	// 对话端点为 SSE（09 §1 实测）。
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", webDeepseekClientUA)

	fullCookie := cookie
	if waf := strings.TrimSpace(account.GetCredential("waf_cookie")); waf != "" {
		fullCookie = strings.TrimRight(fullCookie, "; ") + "; " + waf
	}
	req.Header.Set("Cookie", fullCookie)

	webDeepseekApplyRequestHeaders(req, account)
	// 账号级请求头覆写最后应用，使管理员配置优先。
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// webDeepseekApplyRequestHeaders 注入 09 §2 实测头族：x-client-* 常量头、x-device-id
// （credentials 覆盖，缺省按 account.ID 派生确定性 UUIDv4，每账号稳定）、x-device-model(空)、
// x-hif-dliq / x-hif-leim（指纹签名头，生成算法 unverified：credentials 有值则透传，无值不带）。
func webDeepseekApplyRequestHeaders(req *http.Request, account *Account) {
	req.Header.Set("x-client-bundle-id", "com.deepseek.chat")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("x-client-version", "2.5.0")
	req.Header.Set("x-client-locale", "zh_CN")
	req.Header.Set("x-client-timezone-offset", "28800")

	deviceID := strings.TrimSpace(account.GetCredential("device_id"))
	if deviceID == "" {
		deviceID = webDeepseekDeviceID(account) // 缺省：account.ID 派生确定性 UUIDv4（每账号稳定）
	}
	req.Header.Set("x-device-id", deviceID)
	req.Header.Set("x-device-model", "") // 实测为空串

	// x-hif-*：生成算法未实测（unverified），仅当 credentials 显式提供时透传。
	if hif := strings.TrimSpace(account.GetCredential("x-hif-dliq")); hif != "" {
		req.Header.Set("x-hif-dliq", hif)
	}
	if hif := strings.TrimSpace(account.GetCredential("x-hif-leim")); hif != "" {
		req.Header.Set("x-hif-leim", hif)
	}
}

// webDeepseekDeviceID 按 account.ID 派生确定性 UUIDv4（版本 4 + RFC4122 variant 位）。
// 每账号稳定生成（同一账号每次出站相同），避免随机 device_id 触发风控；credentials 可覆盖。
func webDeepseekDeviceID(account *Account) string {
	if account == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("deepseek web-device-%d", account.ID)))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // 版本 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func webDeepseekOriginFromURL(targetURL string) string {
	parsed, err := url.Parse(targetURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return webDeepseekDefaultBaseURL
	}
	return parsed.Scheme + "://" + parsed.Host
}

// fetchWebDeepseekPoWHeader 取 challenge 并求解，返回 x-ds-pow-response 头值（base64 JSON）。
// 登录态强制 PoW（09 §3）：挑战端点不可达 / 响应无可用 challenge / 求解失败 → 一律失败关闭
// 返回错误（不再"无 PoW 继续出站"——实测证明强制）。挑战请求体为 {"target_path":"/api/v0/chat/completion"}。
// 求解器本体在 web_deepseek_pow.go（并行任务实现），调用 webDeepseekSolvePoW 签名不变。
func (s *OpenAIGatewayService) fetchWebDeepseekPoWHeader(
	ctx context.Context,
	account *Account,
	baseURL string,
	cookie string,
	proxyURL string,
) (string, error) {
	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()

	challengeBody := []byte(`{"target_path":` + strconv.Quote(webDeepseekChatCompletionPath) + `}`)
	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, baseURL+webDeepseekPoWChallengePath, bytes.NewReader(challengeBody))
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
	webDeepseekApplyRequestHeaders(req, account)

	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	if err != nil {
		return "", fmt.Errorf("%w: pow challenge transport error: %v", ErrWebDeepseekPoWNotImplemented, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("%w: pow challenge read error: %v", ErrWebDeepseekPoWNotImplemented, err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%w: pow challenge returned status %d", ErrWebDeepseekPoWNotImplemented, resp.StatusCode)
	}
	challenge, ok := webDeepseekExtractPoWChallenge(b)
	if !ok {
		return "", fmt.Errorf("%w: no solvable challenge found in response", ErrWebDeepseekPoWNotImplemented)
	}
	return webDeepseekSolvePoW(challenge, webDeepseekChatCompletionPath)
}

// webDeepseekErrKind DeepSeek 网页端上游错误分类（06 §A 错误映射）。
type webDeepseekErrKind int

const (
	webDeepseekErrKindOther webDeepseekErrKind = iota
	webDeepseekErrKindRateLimited // 429 / 40029(IP 受限) → 冷却
	webDeepseekErrKindAuthFailed  // 40002/40003(认证失效) / 50006(禁言) → 账号处置
	webDeepseekErrKindPoWError    // 40300/40301(PoW 错误) → 失败关闭
)

// webDeepseekEffectiveErrorCode 双路径错误判定（08 §4 / 06 §A）：HTTP 429，或响应体顶层
// code 非 0，或 data.biz_code 非 0。返回生效业务码与是否错误。嵌套错误以 data.biz_code 为准，
// 顶层 code 为兜底（旧形态）。
func webDeepseekEffectiveErrorCode(statusCode int, body []byte) (code int64, isErr bool) {
	if statusCode == http.StatusTooManyRequests {
		return 429, true
	}
	topCode := gjson.GetBytes(body, "code").Int()
	bizCode := gjson.GetBytes(body, "data.biz_code").Int()
	if bizCode != 0 {
		return bizCode, true
	}
	if topCode != 0 {
		return topCode, true
	}
	return 0, false
}

// classifyWebDeepseekUpstreamError 纯函数错误分类：HTTP 429 / 业务码 40029 → 限流；
// 40002/40003/50006 → 认证/账号失效；40300/40301 → PoW 错误；其余非 0 业务码 → Other。
func classifyWebDeepseekUpstreamError(statusCode int, body []byte) webDeepseekErrKind {
	code, isErr := webDeepseekEffectiveErrorCode(statusCode, body)
	if !isErr {
		return webDeepseekErrKindOther
	}
	switch code {
	case 40002, 40003, 50006:
		return webDeepseekErrKindAuthFailed
	case 40029, 429:
		return webDeepseekErrKindRateLimited
	case 40300, 40301:
		return webDeepseekErrKindPoWError
	default:
		return webDeepseekErrKindOther
	}
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

// handleWebDeepseekUpstreamError 对上游错误做分类、脱敏，按类别归一到标准 HTTP 状态后，
// 经 handleErrorResponse 把错误回传客户端并触发对应冷却/账号处置链路：
//   - RateLimited：归一 429 → 冷却（RateLimitService.HandleUpstreamError）；
//   - AuthFailed：归一 401 → 账号失效处置（SetError）；
//   - PoWError：归一 403 → 失败关闭（不重试，返回错误）；
//   - Other：HTTP 200 携带未知业务错误时归一 502（不得伪造成功）。
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
	switch kind {
	case webDeepseekErrKindRateLimited:
		if resp.StatusCode < http.StatusTooManyRequests {
			resp.StatusCode = http.StatusTooManyRequests
		}
	case webDeepseekErrKindAuthFailed:
		resp.StatusCode = http.StatusUnauthorized
	case webDeepseekErrKindPoWError:
		// PoW 错误（40300/40301）：失败关闭，但不处置账号——PoW 为请求级挑战，客户端
		// 重新求解即可，并非账号健康度问题。直接回 403 给客户端，绕开账号失效链
		// （避免把可重试挑战误判为账号失效导致账号被禁用，06 §A 错误映射）。
		bizCode, _ := webDeepseekEffectiveErrorCode(resp.StatusCode, respBody)
		msg := webDeepseekUpstreamErrorMessage(respBody)
		if msg == "" {
			msg = "deepseek web upstream returned a PoW challenge/validation error"
		}
		setOpsUpstreamError(c, http.StatusForbidden, msg, "")
		MarkResponseCommitted(c)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": msg,
			},
		})
		return nil, fmt.Errorf("deepseek web upstream PoW error (biz_code %d): %s", bizCode, msg)
	default:
		if resp.StatusCode < http.StatusBadRequest {
			resp.StatusCode = http.StatusBadGateway
		}
	}
	upstreamMsg := webDeepseekUpstreamErrorMessage(respBody)
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("deepseek web upstream returned status %d", resp.StatusCode)
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

// webDeepseekUpstreamErrorMessage 提取上游错误文案：优先 data.biz_msg（嵌套实测形态），
// 回落顶层 msg，再回落通用提取。
func webDeepseekUpstreamErrorMessage(body []byte) string {
	if m := strings.TrimSpace(gjson.GetBytes(body, "data.biz_msg").String()); m != "" {
		return sanitizeUpstreamErrorMessage(m)
	}
	if m := strings.TrimSpace(gjson.GetBytes(body, "msg").String()); m != "" {
		return sanitizeUpstreamErrorMessage(m)
	}
	return sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
}

// webDeepseekBareJSONError 判定一行裸 JSON（非 SSE data: 前缀）是否为业务错误（双路径：
// 顶层 code 或 data.biz_code 非 0 均按业务错误）。纯内容裸 JSON（无 code）交由调用方按安全
// 策略忽略。仅解析合法 JSON，杜绝对无法识别行的臆测处理。保留为共享 helper（web_protocol_bridge_test
// 等跨平台测试复用同口径）。
func webDeepseekBareJSONError(line string) ([]byte, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "data:") {
		return nil, false
	}
	if !gjson.Valid(trimmed) {
		return nil, false
	}
	topCode := gjson.Get(trimmed, "code").Int()
	bizCode := gjson.Get(trimmed, "data.biz_code").Int()
	if topCode != 0 || bizCode != 0 {
		return []byte(trimmed), true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// SSE 解析器（09 §5 实测，JSON-Patch 形态 + 省略状态机）
// ---------------------------------------------------------------------------

// webDeepseekFragment 流内消息 fragment（THINK / RESPONSE 等）。content 为已累积文本。
type webDeepseekFragment struct {
	typ     string
	content string
}

// webDeepseekSSEParser 维护 SSE delta 解析状态机：当前路径 currentPath 与操作 currentOp，
// 使省略形态（仅 {"v":...}）能沿用上一帧路径/操作。正文只取 RESPONSE fragment content，
// THINK 等思考 fragment 丢弃（09 §5）。usage 取 accumulated_token_usage 最终值（累计语义）。
type webDeepseekSSEParser struct {
	responseID string
	body       strings.Builder
	usage      int64
	finished   bool
	hasError   bool
	errCode    int64
	errMsg     string

	fragments   []*webDeepseekFragment
	currentPath string
	currentOp   string
}

func newWebDeepseekSSEParser() *webDeepseekSSEParser { return &webDeepseekSSEParser{} }

// applyDelta 解析一帧 delta/业务错误数据，返回新增正文增量（RESPONSE 正文，思考丢弃）。
// 双路径错误判定（08 §4）：顶层 code 或 data.biz_code 非 0 → 标记 hasError。
func (p *webDeepseekSSEParser) applyDelta(data string) (string, bool, int64, string) {
	if !gjson.Valid(data) {
		return "", false, 0, ""
	}
	v := gjson.Parse(data)

	// 双路径业务错误（HTTP 200 也可能携带，06 §A / 08 §4）。
	topCode := v.Get("code").Int()
	bizCode := v.Get("data.biz_code").Int()
	if topCode != 0 || bizCode != 0 {
		msg := strings.TrimSpace(v.Get("data.biz_msg").String())
		if msg == "" {
			msg = strings.TrimSpace(v.Get("msg").String())
		}
		code := bizCode
		if code == 0 {
			code = topCode
		}
		p.hasError = true
		p.errCode = code
		p.errMsg = msg
		return "", true, code, msg
	}

	pObj := v.Get("p")
	oObj := v.Get("o")
	val := v.Get("v")

	if pObj.Exists() && pObj.Type == gjson.String {
		p.currentPath = pObj.String()
	}
	if oObj.Exists() && oObj.Type == gjson.String {
		p.currentOp = oObj.String()
	}

	if !pObj.Exists() {
		// 无 p：完整初始响应（v 内含 response）或续写省略形态（v 为标量）。
		if val.IsObject() {
			if resp := val.Get("response"); resp.IsObject() {
				p.applyFullResponse(resp)
			}
			return "", false, 0, ""
		}
		// 续写省略形态：沿用当前路径与操作。
	}

	if p.currentPath == "" {
		return "", false, 0, ""
	}
	return p.applyAtPath(p.currentPath, p.currentOp, val), false, 0, ""
}

// applyFullResponse 处理首帧完整 response 对象（data: {"v":{"response":{...}}}）。
func (p *webDeepseekSSEParser) applyFullResponse(resp gjson.Result) {
	if id := resp.Get("message_id").Int(); id != 0 {
		p.responseID = strconv.FormatInt(id, 10)
	}
	for _, f := range resp.Get("fragments").Array() {
		typ := f.Get("type").String()
		content := f.Get("content").String()
		p.fragments = append(p.fragments, &webDeepseekFragment{typ: typ, content: content})
		if typ == "RESPONSE" && content != "" {
			p.body.WriteString(content)
		}
	}
	if u := resp.Get("accumulated_token_usage").Int(); u != 0 {
		p.usage = u
	}
}

// applyAtPath 把一条 patch 应用到对应路径。返回新增 RESPONSE 正文增量（思考丢弃时为 ""）。
func (p *webDeepseekSSEParser) applyAtPath(path, op string, val gjson.Result) string {
	switch {
	case path == "response/fragments" && op == "APPEND" && val.IsArray():
		// 追加 fragment 数组：新增 RESPONSE fragment 的初值 content 必须计入正文增量与
		// body 累计（实测首 RESPONSE fragment 经此帧带入初值 "哈哈"，后续仅 -1/content 续写）。
		var delta strings.Builder
		for _, item := range val.Array() {
			typ := item.Get("type").String()
			content := item.Get("content").String()
			p.fragments = append(p.fragments, &webDeepseekFragment{typ: typ, content: content})
			if typ == "RESPONSE" && content != "" {
				p.body.WriteString(content)
				delta.WriteString(content)
			}
		}
		return delta.String()
	case path == "response/fragments/-1/content":
		if len(p.fragments) == 0 {
			return ""
		}
		last := p.fragments[len(p.fragments)-1]
		if op == "SET" {
			last.content = val.String()
		} else {
			// APPEND 及缺省 op 的续写省略形态
			last.content += val.String()
		}
		if last.typ == "RESPONSE" {
			p.body.WriteString(val.String())
			return val.String()
		}
		return "" // THINK 等思考 fragment 不进正文
	case path == "response/accumulated_token_usage":
		p.usage = val.Int()
		return ""
	case path == "response/status":
		if strings.EqualFold(val.String(), "FINISHED") {
			p.finished = true
		}
		return ""
	case op == "BATCH" && val.IsArray():
		// BATCH 的 v 为数组，路径按当前路径前缀拼接（实测 response BATCH →
		// response/accumulated_token_usage、response/quasi_status 等），逐项以 SET 应用。
		for _, item := range val.Array() {
			itemP := item.Get("p").String()
			itemV := item.Get("v")
			p.applyAtPath(webDeepseekJoinPath(path, itemP), "SET", itemV)
		}
		return ""
	}
	return ""
}

func webDeepseekJoinPath(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}

// webDeepseekNextSSEEvent 从 scanner 读取一个 SSE 事件（event 名 + data 载荷）。
// 默认事件名为 "delta"（无 event: 行的纯 data: 帧）；遇空行派发；EOF 无完整事件返回 ok=false。
func webDeepseekNextSSEEvent(sc *bufio.Scanner) (name, data string, ok bool) {
	name = "delta"
	var dataBuf strings.Builder
	sawData := false
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if sawData || name != "delta" {
				return name, dataBuf.String(), true
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE 注释行
		}
		if strings.HasPrefix(line, "event:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(d)
			sawData = true
			continue
		}
	}
	if sawData {
		return name, dataBuf.String(), true
	}
	return "", "", false
}

// webDeepseekClientChunkEnvelope 构造 OpenAI chat.completion.chunk 包络（供 writeWebStreamChunk 回桥）。
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

	parser := newWebDeepseekSSEParser()
	responseID := ""
	var usage *OpenAIUsage
	var aggregated strings.Builder
	written := false
	finishReason := "stop"
	midstreamErr := false
	midstreamErrMsg := ""
	st := newWebClientStreamState(mode, originalModel)

	// TeeReader 留存原始响应体：流结束无内容帧时用于整包业务错误判定/失败关闭
	// （与非流式路径同口径，HTTP 200 裸 JSON 错误不得被伪造成空成功流）。
	var rawUpstreamBody bytes.Buffer
	scanner := bufio.NewScanner(io.TeeReader(resp.Body, &rawUpstreamBody))
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	for {
		name, data, ok := webDeepseekNextSSEEvent(scanner)
		if !ok {
			break
		}
		switch name {
		case "ready":
			// ready 事件：提取 response_message_id 作 response ID（09 §5）。
			if id := gjson.Get(data, "response_message_id").Int(); id != 0 {
				responseID = strconv.FormatInt(id, 10)
			}
			continue
		case "close", "finish":
			// 正常终止（09 §5，无 [DONE]）。
			finishReason = "stop"
			break
		}

		contentDelta, isErr, _, errMsg := parser.applyDelta(data)
		if parser.responseID != "" && responseID == "" {
			responseID = parser.responseID
		}
		if isErr {
			// 业务错误：首帧未写任何客户端字节（written=false）→ 走完整错误路径
			// （可改写 4xx 状态码）；流中后段已写出正文（written=true）→ 记 ops 错误并向
			// 客户端写流内 error 标记后中断收口，绝不伪造正常结束。
			if !written {
				errResp := &http.Response{
					StatusCode: resp.StatusCode,
					Header:     resp.Header,
					Body:       io.NopCloser(bytes.NewReader([]byte(data))),
				}
				return s.handleWebDeepseekUpstreamError(ctx, c, account, errResp, []byte(data), upstreamModel)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				Kind:               "midstream_error",
				Message:            sanitizeUpstreamErrorMessage(errMsg),
			})
			midstreamErr = true
			midstreamErrMsg = sanitizeUpstreamErrorMessage(errMsg)
			break
		}
		if parser.usage != 0 {
			usage = &OpenAIUsage{InputTokens: 0, OutputTokens: int(parser.usage)}
		}
		if contentDelta != "" {
			aggregated.WriteString(contentDelta)
			written = true
			if err := writeWebStreamChunk(c, st, webDeepseekClientChunkEnvelope(responseID, originalModel, gin.H{"content": contentDelta}, "", nil)); err != nil {
				return nil, err
			}
		}
		if parser.finished {
			finishReason = "stop"
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("deepseek web stream read: %w", err)
	}

	// 无任何内容帧：可能是 HTTP 200 裸 JSON 业务错误（08 §4）或不可识别结构；
	// 与非流式路径同口径走错误路径/失败关闭，绝不伪造成空成功流。
	if aggregated.Len() == 0 && !parser.hasError {
		raw := rawUpstreamBody.Bytes()
		if _, isErr := webDeepseekEffectiveErrorCode(resp.StatusCode, raw); isErr {
			errResp := &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header,
				Body:       io.NopCloser(bytes.NewReader(raw)),
			}
			return s.handleWebDeepseekUpstreamError(ctx, c, account, errResp, raw, upstreamModel)
		}
		return nil, errors.New(
			"deepseek web upstream returned an unrecognized non-stream response shape (logged-in capture required for extension)")
	}

	// 流中段业务错误收口：已向客户端写出过正文，上游突发业务错误。
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

	// 正常终止帧：finish_reason + usage（流内最后 accumulated_token_usage 值），再按出站协议收口。
	if parser.usage != 0 {
		usage = &OpenAIUsage{InputTokens: 0, OutputTokens: int(parser.usage)}
	}
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

// handleWebDeepseekNonStreamingResponse 非流式回程：读取完整上游响应，SSE 经新解析器聚合为
// 单条 chat.completion JSON；纯 JSON 业务错误走上游错误路径；结构不可识别时失败关闭。
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

	parser := newWebDeepseekSSEParser()
	responseID := ""
	var aggregated strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), defaultMaxLineSize)
	for {
		name, data, ok := webDeepseekNextSSEEvent(scanner)
		if !ok {
			break
		}
		switch name {
		case "ready":
			if id := gjson.Get(data, "response_message_id").Int(); id != 0 {
				responseID = strconv.FormatInt(id, 10)
			}
			continue
		case "close", "finish":
			finishReason := "stop"
			_ = finishReason
			break
		}

		contentDelta, isErr, _, _ := parser.applyDelta(data)
		if parser.responseID != "" && responseID == "" {
			responseID = parser.responseID
		}
		if isErr {
			errResp := &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header,
				Body:       io.NopCloser(bytes.NewReader(body)),
			}
			return s.handleWebDeepseekUpstreamError(ctx, c, account, errResp, body, upstreamModel)
		}
		if contentDelta != "" {
			aggregated.WriteString(contentDelta)
		}
		if parser.finished {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("deepseek web upstream response scan failed: %w", err)
	}

	// 无任何内容帧：可能是纯 JSON 业务错误（HTTP 200）或不可识别结构。
	if aggregated.Len() == 0 && !parser.hasError {
		if _, isErr := webDeepseekEffectiveErrorCode(resp.StatusCode, body); isErr {
			errResp := &http.Response{
				StatusCode: resp.StatusCode,
				Header:     resp.Header,
				Body:       io.NopCloser(bytes.NewReader(body)),
			}
			return s.handleWebDeepseekUpstreamError(ctx, c, account, errResp, body, upstreamModel)
		}
		return nil, errors.New(
			"deepseek web upstream returned an unrecognized non-stream response shape (logged-in capture required for extension)")
	}

	finalUsage := &OpenAIUsage{}
	if parser.usage != 0 {
		// accumulated_token_usage 为累计 token 语义（非 prompt/completion 拆分）；
		// OpenAIUsage 映射：InputTokens=0, OutputTokens=累计值（token 生命周期 unverified）。
		finalUsage.InputTokens = 0
		finalUsage.OutputTokens = int(parser.usage)
	} else if account.IsWebAccessMode() {
		// 上游未返回 usage 时本地估算兜底（D2），避免计费为 0（仅估算，非真实 token 数）。
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
