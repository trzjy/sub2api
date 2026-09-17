package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// web-zhipu 网页逆向适配器（方案 W3，docs/web-reverse-embedded-login-plan.md §4.2）。
//
// 协议状态声明（权威来源：官网登录态抓包实测，2026-09-17）：
//   - 已实测：对话端点 POST /chatglm/backend-api/assistant/stream（SSE）；请求体以
//     assistant_id + meta_data.selected_model 选模型（不存在旧 v1/conversation 的
//     model 字段）；认证为 Cookie(chatglm_token) + Authorization: Bearer <chatglm_token>
//     双载体 + app-name/x-app-* /x-device-id 指纹头；响应 SSE 为 parts[].content[]
//     结构，按 content.type 分派（text=正文增量 / think=思考 / tool_calls=工具调用），
//     part 级 status=finish + 顶层 status=finish 终止，无 usage 字段。
//   - 官网请求校验 x-sign/x-nonce/x-timestamp 签名三件套：算法来自 2026-09-17 官方
//     main.js 逆向 + 真实抓包验证（md5 与官方 x-sign 完全一致），出站由
//     buildWebZhipuUpstreamRequest 按 webZhipuComputeSign 补齐；不带 → HTTP 400
//     {"status":40011}，带错 → 40012。
//
// 安全红线：凭证（cookie/chatglm_token）不得出现在日志或错误响应中——上游错误体在
// 任何透传前先经 redactWebZhipuUpstreamErrorBody 脱敏。
//
// 刷新语义（2026-09-17 官方 main.js 逆向确认）：Cookie 携带 chatglm_refresh_token，
// 401 时先用 refresh token 静默续期（POST {base}/user-api/user/refresh）并重试一次，
// 续期失败（或无 refresh token）才走冷却与错误回传（见 refreshWebZhipuAccessToken 与
// forwardWebZhipu 401 分支）。

const (
	// webZhipuClientUA 指纹对齐用浏览器 UA（以登录态抓包为准）。
	webZhipuClientUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
	// webZhipuStreamPath 对话端点（SSE，2026-09-17 登录态实测）。
	webZhipuStreamPath = "/chatglm/backend-api/assistant/stream"
	// webZhipuDefaultAssistantID GLM-Flash（极致）助手的内部 assistant_id
	// （2026-09-17 登录态抓包实测：meta_data.selected_model=glm-5.3-flash 时使用）。
	// 管理员可用 credentials["assistant_id"] 覆盖。
	webZhipuDefaultAssistantID = "65940acff94777010aa6b796"

	// webZhipuSignSalt 签名盐值（x-sign 计算输入末尾段）：来源 2026-09-17 官方 main.js
	// 逆向 + 真实抓包验证（md5 与官方 x-sign 完全一致）。盐值本身为公开常量，严禁将任何
	// Cookie / Token / 登录态内容写入本常量或周边注释。
	webZhipuSignSalt = "8a1317a7468aa3ad86e997d08f3f31cb"
)

// webZhipuExtractCookieField 从整串 Cookie 中提取指定字段的值。
func webZhipuExtractCookieField(cookie, name string) string {
	for _, part := range strings.Split(cookie, ";") {
		kv := strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// webZhipuSetCookieFieldValue 在整串 Cookie 中把指定字段 name 的值替换为 value，
// 其余字段与原始字符串（含字段间空白）原样保留；字段不存在则追加到末尾。用于刷新成功后
// 就地更新 cookie 中的 chatglm_token / chatglm_refresh_token，不破坏其它字段（如 CDN Cookie）。
func webZhipuSetCookieFieldValue(cookie, name, value string) string {
	if cookie == "" {
		return name + "=" + value
	}
	parts := strings.Split(cookie, ";")
	found := false
	for i, p := range parts {
		trimmed := strings.TrimSpace(p)
		if k, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(k) == name {
			parts[i] = name + "=" + value
			found = true
		}
	}
	if !found {
		return cookie + "; " + name + "=" + value
	}
	return strings.Join(parts, ";")
}

// webZhipuDeviceIDFromToken 从 chatglm_token JWT payload 解出 device_id
// （payload 为 base64url JSON，含 device_id 字段；解析失败返回空串，不阻断请求）。
func webZhipuDeviceIDFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "device_id").String())
}

// webZhipuTokenIsGuest 解析 chatglm_token JWT payload，判定是否为游客登录态
// （is_guest=true）。2026-09-17 生产消融实验证实：游客态 token 出站必被上游以
// HTTP 400 {"status":40011} 拒绝，属登录态问题而非凭证失效，需在上游请求发出前
// 失败关闭（详见 forwardWebZhipu 的游客态防线）。
//
// 解析失败 / is_guest 字段缺失 / token 空 均返回 false：真实登录态 token 无该字段
// （或 false），不得误伤；Cookie 缺失由 forwardWebZhipu 的独立失败关闭处理，本函数
// 不重复报错。参考 webZhipuDeviceIDFromToken 的 base64.RawURLEncoding 解析口径。
func webZhipuTokenIsGuest(token string) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	// is_guest 缺失或非布尔时 gjson.Bool() 返回 false（真实登录态不误伤）。
	return gjson.GetBytes(payload, "is_guest").Bool()
}

// webZhipuResolveChatGLMToken 从账号凭证解析用于出站鉴权的 chatglm_token：优先
// credentials["chatglm_token"] 显式 Bearer，否则从整串 Cookie 提取。取值口径与
// buildWebZhipuUpstreamRequest 完全一致（含 cdn_cookie 合并），便于登录态判定与正式
// 转发链共用同一 token 来源，避免口径漂移。仅用于登录态判定，不对外脱敏传递。
func webZhipuResolveChatGLMToken(account *Account) string {
	token := strings.TrimSpace(account.GetCredential("chatglm_token"))
	if token != "" {
		return token
	}
	cookie := strings.TrimSpace(account.GetCredential("cookie"))
	if cookie == "" {
		return ""
	}
	fullCookie := cookie
	if cdn := strings.TrimSpace(account.GetCredential("cdn_cookie")); cdn != "" {
		fullCookie = strings.TrimRight(fullCookie, "; ") + "; " + cdn
	}
	return webZhipuExtractCookieField(fullCookie, "chatglm_token")
}

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
	// 出站模型即 meta_data.selected_model（2026-09-17 实测：请求 selected_model 公开名，
	// 上游 parts[].model 返回内部名 moe_5）；未知模型名按「默认透传同名」处理。
	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = originalModel
	}
	// 未知模型失败关闭：仅允许默认目录内模型（DefaultWebModelIDs(PlatformWebZhipu)），
	// 与 "web-zhipu requires at least one user message" 同风格，在 handleErrorResponse
	// 之前直接 return error，不发出任何上游请求。
	if err := ValidateWebZhipuModel(upstreamModel); err != nil {
		return nil, err
	}
	SetOpsUpstreamModel(c, upstreamModel)

	// 登录态防线（2026-09-17 生产消融实验证实）：chatglm_token 为游客态
	// （is_guest=true）时，上游 /assistant/stream 必返 HTTP 400 {"status":40011}，
	// 这是注定失败的请求——属登录态问题而非凭证失效。故在发出任何上游请求前失败关闭，
	// 且不调用 SetError/SetRateLimited 冷却账号（不误判为凭证失效）。错误信息不得含
	// 任何 token/cookie 内容（安全红线）。
	if webZhipuTokenIsGuest(webZhipuResolveChatGLMToken(account)) {
		return nil, errors.New("web-zhipu login state is a guest token, please re-capture login cookie")
	}

	prompt := webZhipuExtractPrompt(body)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("web-zhipu requires at least one user message in the request")
	}
	messages := webZhipuExtractUserMessages(body)
	upstreamBody := buildWebZhipuRequestBody(messages, upstreamModel, account)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()

	req, err := s.buildWebZhipuUpstreamRequest(upstreamCtx, account, baseURL+webZhipuStreamPath, cookie, upstreamBody)
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
		// 401 → 先用 chatglm_refresh_token 静默续期一次再重试（官方前端同款刷新链，
		// 2026-09-17 官方 main.js 逆向确认）。续期失败（或无 refresh token）则保持原
		// 冷却与错误路径不变（不重复请求，仅重试一次）。
		if resp.StatusCode == http.StatusUnauthorized {
			if refreshed := s.refreshWebZhipuAccessToken(ctx, account); refreshed != "" {
				retryCtx, releaseRetry := detachUpstreamContext(ctx)
				defer releaseRetry()
				newCookie := strings.TrimSpace(account.GetCredential("cookie"))
				retryReq, reqErr := s.buildWebZhipuUpstreamRequest(retryCtx, account, baseURL+webZhipuStreamPath, newCookie, upstreamBody)
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
	}

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		return s.handleWebZhipuUpstreamError(ctx, c, account, resp, respBody, upstreamModel)
	}

	if reqStream {
		return s.handleWebZhipuStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, mode)
	}
	return s.handleWebZhipuNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, webZhipuExtractPrompt(body), mode)
}

// webZhipuUpstreamRequest 官网 /assistant/stream 请求体（2026-09-17 登录态实测结构；
// 字段仅设实测存在的项，不臆测填充）。模型选择 = meta_data.selected_model，
// 助手选择 = assistant_id（官方 GLM-Flash 助手默认值，credentials["assistant_id"] 可覆盖）。
type webZhipuUpstreamRequest struct {
	AssistantID    string            `json:"assistant_id"`
	ConversationID string            `json:"conversation_id"`
	ProjectID      string            `json:"project_id"`
	ChatType       string            `json:"chat_type"`
	MetaData       webZhipuMetaData  `json:"meta_data"`
	Messages       []webZhipuMessage `json:"messages"`
}

// webZhipuMetaData 实测 meta_data 结构（官网请求体原样字段）。
type webZhipuMetaData struct {
	Cogview           webZhipuCogview `json:"cogview"`
	IsTest            bool            `json:"is_test"`
	InputQuestionType string          `json:"input_question_type"`
	Channel           string          `json:"channel"`
	DraftID           string          `json:"draft_id"`
	ChatMode          string          `json:"chat_mode"`
	SelectedModel     string          `json:"selected_model"`
	IsNetworking      bool            `json:"is_networking"`
	QuoteLogID        string          `json:"quote_log_id"`
	Platform          string          `json:"platform"`
}

type webZhipuCogview struct {
	RmLabelWatermark bool `json:"rm_label_watermark"`
}

// webZhipuMessage 实测 messages 元素：content 为类型数组形态（text 片段）。
type webZhipuMessage struct {
	Role    string                   `json:"role"`
	Content []webZhipuMessageContent `json:"content"`
}

type webZhipuMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// webZhipuResolveAssistantID 是 assistant_id 的单一事实来源（credentials["assistant_id"]
// 凭证覆盖 > 默认常量）。默认值 webZhipuDefaultAssistantID 是 2026-09-17 登录态抓包实测的
// 公共 GLM-Flash 助手 ID，管理员可用 credentials["assistant_id"] 按账号覆盖。动态同步
// 目录（从官网 available_models 拉取）待有官网权威证据后再做。
func webZhipuResolveAssistantID(account *Account) string {
	assistantID := strings.TrimSpace(account.GetCredential("assistant_id"))
	if assistantID == "" {
		return webZhipuDefaultAssistantID
	}
	return assistantID
}

func buildWebZhipuRequestBody(messages []webZhipuMessage, model string, account *Account) []byte {
	assistantID := webZhipuResolveAssistantID(account)
	req := webZhipuUpstreamRequest{
		AssistantID:    assistantID,
		ConversationID: "",
		ProjectID:      "",
		ChatType:       "user_chat",
		MetaData: webZhipuMetaData{
			Cogview:       webZhipuCogview{RmLabelWatermark: false},
			IsTest:        false,
			ChatMode:      "deep_thinking",
			SelectedModel: model,
			Platform:      "pc",
		},
		Messages: messages,
	}
	data, _ := json.Marshal(req)
	return data
}

// webZhipuExtractPrompt 取最后一条 user 消息文本（多轮历史展开方式未实测——官网
// 同会话续传走 conversation_id，本适配器每轮新会话，故只发最后一条 user 消息，
// 多轮上下文合并语义待实测补全）。纯字符串 content 直接取值；数组形态取 text 片段拼接。
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

// webZhipuExtractUserMessages 把 prompt 包为实测 messages 元素形态
// {role:"user", content:[{type:"text", text}]}。
func webZhipuExtractUserMessages(body []byte) []webZhipuMessage {
	return []webZhipuMessage{{
		Role:    "user",
		Content: []webZhipuMessageContent{{Type: "text", Text: webZhipuExtractPrompt(body)}},
	}}
}

// buildWebZhipuUpstreamRequest 构造网页端出站请求（指纹头按 2026-09-17 登录态抓包对齐）。
//
// 认证双载体（实测）：Cookie 串（含 chatglm_token）+ Authorization: Bearer
// <chatglm_token>（从 Cookie 串提取；管理员可用 credentials["chatglm_token"] 覆盖）。
// 指纹头：app-name / x-app-platform / x-app-version / x-app-fr / x-lang / x-device-id
// （device_id 从 chatglm_token JWT payload 解出）。官网请求还校验 x-sign/x-nonce/
// x-timestamp 签名三件套（算法来自 2026-09-17 官方 main.js 逆向 + 抓包验证，见本文件
// webZhipuComputeSign），出站按算法补齐——不带/带错均被上游以签名类错误拒绝。
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
	fullCookie := cookie
	if cdn := strings.TrimSpace(account.GetCredential("cdn_cookie")); cdn != "" {
		fullCookie = strings.TrimRight(fullCookie, "; ") + "; " + cdn
	}
	token := strings.TrimSpace(account.GetCredential("chatglm_token"))
	if token == "" {
		token = webZhipuExtractCookieField(fullCookie, "chatglm_token")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", webZhipuClientUA)
	req.Header.Set("app-name", "chatglm")
	req.Header.Set("x-app-platform", "pc")
	req.Header.Set("x-app-version", "0.0.1")
	req.Header.Set("x-app-fr", "default")
	req.Header.Set("x-lang", "zh")
	if deviceID := webZhipuDeviceIDFromToken(token); deviceID != "" {
		req.Header.Set("x-device-id", deviceID)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Cookie", fullCookie)

	// 签名三件套（2026-09-17 官方 main.js 逆向 + 抓包验证）：x-timestamp / x-nonce /
	// x-sign，算法见 webZhipuComputeSign。每条出站请求即时生成，盐值取 webZhipuSignSalt。
	// 三个头必须由同一次 webZhipuComputeSign 生成，避免 nonce 不一致导致 x-sign 校验失败。
	nowMs := time.Now().UnixMilli()
	xTimestamp, xNonce, xSign := webZhipuComputeSign(nowMs)
	req.Header.Set("X-Timestamp", xTimestamp)
	req.Header.Set("X-Nonce", xNonce)
	req.Header.Set("X-Sign", xSign)

	// 每个请求新生成 32 hex 随机 X-Request-Id（与官方形态一致）。
	req.Header.Set("X-Request-Id", webZhipuUUIDHex())

	// 账号级请求头覆写最后应用，使管理员配置优先（与 CodeBuddy 出站口径一致）。
	// 签名头先于本步设置，故管理员覆写仍可覆盖签名头（不被破坏）。
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// webZhipuSignTimestamp 官方 x-timestamp 变换（2026-09-17 main.js 逆向 + 抓包验证）：
// now 为 13 位毫秒串；digits 为其各位数字；t = sum(digits) - digits[len-2]；
// 返回 now[0:len-2] + str(t%10) + now[len-1]。纯函数，便于单测固定输入断言。
func webZhipuSignTimestamp(nowMs int64) string {
	now := strconv.FormatInt(nowMs, 10)
	if len(now) < 3 {
		return now
	}
	digits := make([]int, len(now))
	sum := 0
	for i, c := range now {
		d := int(c - '0')
		digits[i] = d
		sum += d
	}
	t := sum - digits[len(digits)-2]
	return now[:len(now)-2] + strconv.Itoa(t%10) + string(now[len(now)-1])
}

// webZhipuUUIDHex 返回 UUID v4 去连字符的 32 hex 串（与官方 x-nonce / x-request-id 形态一致）。
func webZhipuUUIDHex() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// webZhipuSignFrom 由已确定的 x-timestamp 与 x-nonce 计算 x-sign（纯函数，便于单测）。
// x-sign = md5( xTimestamp + "-" + xNonce + "-" + webZhipuSignSalt )，32 位小写 hex。
func webZhipuSignFrom(xTimestamp, xNonce string) string {
	raw := xTimestamp + "-" + xNonce + "-" + webZhipuSignSalt
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// webZhipuComputeSign 生成签名三件套（x-timestamp / x-nonce / x-sign）。x-nonce 取
// UUID v4 去连字符的 32 hex（github.com/google/uuid）；x-sign 经 webZhipuSignFrom 计算。
// 纯函数（nowMs 为入参），便于单测。
func webZhipuComputeSign(nowMs int64) (xTimestamp, xNonce, xSign string) {
	xTimestamp = webZhipuSignTimestamp(nowMs)
	xNonce = webZhipuUUIDHex()
	xSign = webZhipuSignFrom(xTimestamp, xNonce)
	return
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

// webZhipuRefreshPath 官方前端 refresh 端点（2026-09-17 官方 main.js 逆向确认）：
// ${r}/user-api/user/refresh（r 为站点根，无 /chatglm 前缀差异；base 即现有
// DefaultWebZhipuBaseURL 域）。
const webZhipuRefreshPath = "/user-api/user/refresh"

// refreshWebZhipuAccessToken 用 chatglm_refresh_token 静默续期 access token（官方前端同款
// 刷新链，端点与形态源自 2026-09-17 官方 main.js 逆向）：
//   - 解析 chatglm_refresh_token：优先显式 credentials["refresh_token"] /
//     credentials["chatglm_token"]，否则从整串 Cookie 的 chatglm_refresh_token 字段取；
//   - POST {base}/user-api/user/refresh，Authorization: Bearer <chatglm_refresh_token>，
//     空 JSON 请求体 + 与 assistant/stream 同款指纹头（含 refresh token 的 device_id claim、
//     签名三件套、X-Request-Id）；
//   - 200 → 取 result.access_token / result.refresh_token（refresh token 也会轮换），持久化到
//     账号（cookie 两字段就地替换、显式键仅当原凭证存在才写）；返回新 access token；
//   - 非 200 / 解析失败 / 无 refresh token → 返回 ""，不写库。
//
// 与 web-kimi 刷新链同构：刷新成功持久化新凭证，失败不写库。
func (s *OpenAIGatewayService) refreshWebZhipuAccessToken(ctx context.Context, account *Account) string {
	if account == nil {
		return ""
	}
	cookie := strings.TrimSpace(account.GetCredential("cookie"))

	// 解析 chatglm_refresh_token：显式 credentials 优先，否则从整串 Cookie 字段取。
	refreshToken := strings.TrimSpace(account.GetCredential("refresh_token"))
	if refreshToken == "" {
		refreshToken = webZhipuExtractCookieField(cookie, "chatglm_refresh_token")
	}
	if refreshToken == "" {
		// 无 refresh token：不发刷新请求，交由调用方原 401 冷却路径。
		return ""
	}

	baseURL := strings.TrimRight(account.GetWebBaseURL(), "/")
	if baseURL == "" {
		return ""
	}
	refreshURL := baseURL + webZhipuRefreshPath

	upstreamCtx, release := detachUpstreamContext(ctx)
	defer release()

	body := []byte("{}")
	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, refreshURL, bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	origin := webZhipuOriginFromURL(refreshURL)
	fullCookie := cookie
	if cdn := strings.TrimSpace(account.GetCredential("cdn_cookie")); cdn != "" {
		fullCookie = strings.TrimRight(fullCookie, "; ") + "; " + cdn
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", webZhipuClientUA)
	req.Header.Set("app-name", "chatglm")
	req.Header.Set("x-app-platform", "pc")
	req.Header.Set("x-app-version", "0.0.1")
	req.Header.Set("x-app-fr", "default")
	req.Header.Set("x-lang", "zh")
	if deviceID := webZhipuDeviceIDFromToken(refreshToken); deviceID != "" {
		req.Header.Set("x-device-id", deviceID)
	}
	req.Header.Set("Authorization", "Bearer "+refreshToken)
	if fullCookie != "" {
		req.Header.Set("Cookie", fullCookie)
	}

	// 签名三件套（与 assistant/stream 同款，即时生成）+ 32 hex X-Request-Id。
	nowMs := time.Now().UnixMilli()
	xTimestamp, xNonce, xSign := webZhipuComputeSign(nowMs)
	req.Header.Set("X-Timestamp", xTimestamp)
	req.Header.Set("X-Nonce", xNonce)
	req.Header.Set("X-Sign", xSign)
	req.Header.Set("X-Request-Id", webZhipuUUIDHex())

	// 账号级请求头覆写最后应用，使管理员配置优先（与 assistant/stream 出站口径一致）。
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.doOpenAIUpstream(req, proxyURL, account)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		// 续期失败（401/400 等）：不持久化任何值。
		return ""
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return ""
	}

	newAccessToken := strings.TrimSpace(gjson.GetBytes(respBody, "result.access_token").String())
	newRefreshToken := strings.TrimSpace(gjson.GetBytes(respBody, "result.refresh_token").String())
	if newAccessToken == "" {
		return ""
	}
	if newRefreshToken == "" {
		// refresh token 未轮换：保持原值不变（仍按原字段写回 cookie）。
		newRefreshToken = refreshToken
	}

	if err := s.persistWebZhipuRefreshedCredentials(ctx, account, cookie, newAccessToken, newRefreshToken); err != nil {
		// 持久化失败不阻断本次重试（已拿到新 token），仅脱敏告警。
		slog.Warn("web-zhipu refresh succeeded but credential persist failed",
			"account_id", account.ID, "error", err.Error())
	}
	return newAccessToken
}

// persistWebZhipuRefreshedCredentials 把续期后的新凭证写回账号：就地替换 cookie 中的
// chatglm_token 与 chatglm_refresh_token 两个字段（其余 cookie 字段原样保留），并仅当
// 原凭证已存在 chatglm_token / refresh_token 显式键时才同步覆写（不凭空新增键）。
// 影子账号恒不持凭据（defense-in-depth，与 persistAccountCredentials 同口径）。
func (s *OpenAIGatewayService) persistWebZhipuRefreshedCredentials(
	ctx context.Context,
	account *Account,
	originalCookie string,
	newAccessToken string,
	newRefreshToken string,
) error {
	if account == nil {
		return nil
	}
	if account.IsCredentialShadow() {
		return nil
	}
	newCreds := shallowCopyMap(account.Credentials)
	if originalCookie != "" {
		updated := webZhipuSetCookieFieldValue(originalCookie, "chatglm_token", newAccessToken)
		updated = webZhipuSetCookieFieldValue(updated, "chatglm_refresh_token", newRefreshToken)
		newCreds["cookie"] = updated
	}
	// 显式键仅当原凭证存在时才写（不凭空新增）。
	if _, ok := account.Credentials["chatglm_token"]; ok {
		newCreds["chatglm_token"] = newAccessToken
	}
	if _, ok := account.Credentials["refresh_token"]; ok {
		newCreds["refresh_token"] = newRefreshToken
	}

	account.Credentials = newCreds
	if s.accountRepo == nil {
		return nil
	}
	// 持久化触点与 persistAccountCredentials 同口径：优先 accountCredentialsUpdater，
	// 否则回落 repo.Update（仓库未实现细粒度 UpdateCredentials 时仍可写回）。
	if updater, ok := any(s.accountRepo).(accountCredentialsUpdater); ok {
		return updater.UpdateCredentials(ctx, account.ID, newCreds)
	}
	return s.accountRepo.Update(ctx, account)
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

// webZhipuChunkView 单帧解析视图（parts[].content[] 实测结构）。
type webZhipuChunkView struct {
	Content      string
	FinishReason string
	Usage        *OpenAIUsage
	ResponseID   string
	ErrCode      int64
}

// parseWebZhipuSSEFrame 解析单行 SSE 帧，返回 data 载荷（标准 SSE 单行 JSON 处理）。
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

// mapWebZhipuPayload 把一帧 /assistant/stream JSON 载荷映射为 webZhipuChunkView
// （2026-09-17 登录态实测结构）：
//   - 顶层 parts[] 每个 part 内 content[] 按 type 分派：text → 正文增量（part 内
//     多个 text 元素取最后一个非空——实测同 logic_id 的 text 元素为「累积全文」形态，
//     取最后一个即最新全文，不做拼接防重复）；think → 思考过程，不进正文；
//     tool_calls → name=finish 时视为收尾标记。
//   - part.status=finish + 顶层 status=finish → 终止（FinishReason=stop）。
//   - 实测无 usage 字段 → 计费沿用本地估算兜底（非流式聚合路径）。
func mapWebZhipuPayload(payload []byte) webZhipuChunkView {
	v := gjson.ParseBytes(payload)
	view := webZhipuChunkView{}
	if id := strings.TrimSpace(v.Get("id").String()); id != "" {
		view.ResponseID = id
	}
	parts := v.Get("parts").Array()
	for _, part := range parts {
		status := part.Get("status").String()
		for _, c := range part.Get("content").Array() {
			switch c.Get("type").String() {
			case "text":
				// 累积全文形态：取最后一个非空 text 元素。
				if t := c.Get("text").String(); t != "" {
					view.Content = t
				}
			case "tool_calls":
				if c.Get("tool_calls.name").String() == "finish" && view.FinishReason == "" {
					view.FinishReason = "stop"
				}
			}
		}
		if status == "finish" && view.FinishReason == "" {
			view.FinishReason = "stop"
		}
	}
	if v.Get("status").String() == "finish" {
		view.FinishReason = "stop"
	}
	if code := v.Get("code"); code.Exists() && code.Type == gjson.Number {
		view.ErrCode = code.Int()
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
		// 实测 text 元素为累积全文形态（917 → 1714 → 9171714 均为全文），出站只发增量：
		// delta = 全文去掉已聚合前缀；前缀不匹配时（上游形态变化）回退发全文，宁重不丢。
		delta := strings.TrimPrefix(view.Content, aggregated.String())
		aggregated.Reset()
		aggregated.WriteString(view.Content)
		if delta == "" {
			continue
		}
		written = true
		if err := writeWebStreamChunk(c, st, webZhipuClientChunkEnvelope(responseID, originalModel, gin.H{"content": delta}, "", nil)); err != nil {
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
			UpstreamEndpoint: webZhipuStreamPath,
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
		UpstreamEndpoint: webZhipuStreamPath,
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
		// 实测 text 元素为累积全文形态（与流式回程同一口径）：前缀延续时只追加增量，
		// 形态变化（非前缀）时整段替换，避免累积全文被重复拼接。
		if strings.HasPrefix(view.Content, aggregated.String()) {
			aggregated.WriteString(strings.TrimPrefix(view.Content, aggregated.String()))
		} else {
			aggregated.Reset()
			aggregated.WriteString(view.Content)
		}
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
		UpstreamEndpoint: webZhipuStreamPath,
		Stream:           false,
		ResponseHeaders:  resp.Header.Clone(),
		Duration:         time.Since(startTime),
	}, nil
}
