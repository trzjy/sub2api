package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// sseDataPrefix matches SSE data lines with optional whitespace after colon.
// Some upstream APIs return non-standard "data:" without space (should be "data: ").
var sseDataPrefix = regexp.MustCompile(`^data:\s*`)

const (
	testClaudeAPIURL            = "https://api.anthropic.com/v1/messages?beta=true"
	chatgptCodexAPIURL          = "https://chatgpt.com/backend-api/codex/responses"
	defaultAntigravityTestModel = "claude-sonnet-4-6"
)

// TestEvent represents a SSE event for account testing
type TestEvent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Model    string `json:"model,omitempty"`
	Status   string `json:"status,omitempty"`
	Code     string `json:"code,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	// AudioURL / VideoURL are data: or https URLs for in-browser media players.
	AudioURL string `json:"audio_url,omitempty"`
	VideoURL string `json:"video_url,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Data     any    `json:"data,omitempty"`
	Success  bool   `json:"success,omitempty"`
	Error    string `json:"error,omitempty"`
}

// AccountTestOptions carries optional media for admin connectivity tests.
// ImageDataURL / AudioDataURL are full data URLs (data:<mime>;base64,...).
type AccountTestOptions struct {
	ImageDataURL string
	AudioDataURL string
}

func firstAccountTestOptions(opts []AccountTestOptions) AccountTestOptions {
	if len(opts) == 0 {
		return AccountTestOptions{}
	}
	return opts[0]
}

// maxAccountTestMediaBytes caps inbound data-URL payloads for admin tests (~8 MiB).
const maxAccountTestMediaBytes = 8 << 20

const (
	defaultGeminiTextTestPrompt  = "hi"
	defaultGeminiImageTestPrompt = "Generate a cute orange cat astronaut sticker on a clean pastel background."
	defaultOpenAIImageTestPrompt = "Generate a cute orange cat astronaut sticker on a clean pastel background."
	defaultGrokImageTestPrompt   = "Generate a cute orange cat astronaut sticker on a clean pastel background."
	defaultGrokVideoTestPrompt   = "A red ball bouncing once on a white floor, short simple motion."
	defaultGrokSearchTestQuery   = "xAI Grok"
	defaultGrokTTSTestText       = "Hello from Sub2API account connectivity test."

	// Grok account-test modes (admin UI). Empty / default / text = Responses probe.
	// image/video may also be inferred from model_id when mode is default.
	AccountTestModeGrokText     = "text"
	AccountTestModeGrokImage    = "image"
	AccountTestModeGrokVideo    = "video"
	AccountTestModeGrokSearch   = "search"
	AccountTestModeGrokTTS      = "tts"
	AccountTestModeGrokSTT      = "stt"
	AccountTestModeGrokRealtime = "realtime"

	defaultGrokRealtimeTestModel = "grok-voice-latest"
	grokRealtimeProbeTimeout     = DefaultGrokRealtimeDialTimeout
)

// isOpenAIImageModel checks if the model is an OpenAI image generation model (e.g. gpt-image-2).
func isOpenAIImageModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "gpt-image-")
}

func isGrokVideoGenerationModel(model string) bool {
	return isGrokVideoBillingModel(model) ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "grok-video")
}

func normalizeGrokAccountTestMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case AccountTestModeGrokText:
		return AccountTestModeGrokText
	case AccountTestModeGrokImage:
		return AccountTestModeGrokImage
	case AccountTestModeGrokVideo:
		return AccountTestModeGrokVideo
	case AccountTestModeGrokSearch:
		return AccountTestModeGrokSearch
	case AccountTestModeGrokTTS:
		return AccountTestModeGrokTTS
	case AccountTestModeGrokSTT:
		return AccountTestModeGrokSTT
	case AccountTestModeGrokRealtime:
		return AccountTestModeGrokRealtime
	default:
		return AccountTestModeDefault
	}
}

// AccountTestService handles account testing operations
type AccountTestService struct {
	accountRepo               AccountRepository
	geminiTokenProvider       *GeminiTokenProvider
	claudeTokenProvider       *ClaudeTokenProvider
	grokTokenProvider         *GrokTokenProvider
	antigravityGatewayService *AntigravityGatewayService
	httpUpstream              HTTPUpstream
	cfg                       *config.Config
	settingService            *SettingService
	tlsFPProfileService       *TLSFingerprintProfileService
	modelMetadataRegistryMu   sync.Mutex
	modelMetadataRegistry     map[string]modelsDevProvider
	modelMetadataRegistryAt   time.Time
	pluginManager             *PluginManager
	openaiGatewayService      *OpenAIGatewayService
	agentIdentityTaskMu       sync.Mutex
	agentIdentityWS           agentIdentityWSConnectionInvalidator
	// grokWSDialer is optional; realtime account tests use the default OpenAI-style
	// WS dialer when nil (supports proxy + coder/websocket handshake).
	grokWSDialer openAIWSClientDialer
	// volcanoDocClient 是火山官方文档专用固定客户端（仅 docs.volcengine.com、无凭证、
	// 无代理、拒绝跳转）。测试可注入；nil 时由 getVolcanoDocClient 懒构造一次。
	volcanoDocClient     *http.Client
	volcanoDocClientOnce sync.Once
	// volcanoDocBaseURL 是文档基址覆盖（测试注入 httptest）；空时用 volcanoDocBaseURL。
	volcanoDocBaseURL string
}

func (s *AccountTestService) SetSettingService(settingService *SettingService) {
	if s != nil {
		s.settingService = settingService
	}
}

func (s *AccountTestService) SetPluginManager(pluginManager *PluginManager) {
	if s != nil {
		s.pluginManager = pluginManager
	}
}

func (s *AccountTestService) SetOpenAIGatewayService(gateway *OpenAIGatewayService) {
	if s != nil {
		s.openaiGatewayService = gateway
	}
}

// FetchOpenAIAccountModels uses the shared cached discovery path for the test picker.
func (s *AccountTestService) FetchOpenAIAccountModels(ctx context.Context, account *Account) ([]openai.Model, error) {
	if s == nil || s.openaiGatewayService == nil {
		return nil, errors.New("OpenAI model discovery service is unavailable")
	}
	response, err := s.openaiGatewayService.FetchOpenAIModelsList(ctx, account)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []openai.Model `json:"data"`
	}
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return nil, fmt.Errorf("decode OpenAI account models: %w", err)
	}
	// Standard model catalogs do not require the fields used by the admin picker.
	// Populate them here without changing the shared discovery response or cache.
	for i := range payload.Data {
		model := &payload.Data[i]
		if strings.TrimSpace(model.DisplayName) == "" {
			model.DisplayName = model.ID
		}
		if strings.TrimSpace(model.Type) == "" {
			model.Type = "model"
		}
	}
	// Codex discovery lists Responses drivers, not image_generation tool models.
	// Add locally supported image choices only to the OAuth test picker; keep the
	// shared upstream catalog and API-key discovery authoritative.
	if account != nil && account.IsOpenAIOAuthLike() {
		seen := make(map[string]bool, len(payload.Data))
		for _, model := range payload.Data {
			seen[model.ID] = true
		}
		for _, model := range openai.DefaultModels {
			if IsGPTImageGenerationModel(model.ID) && account.IsModelSupported(model.ID) && !seen[model.ID] {
				payload.Data = append(payload.Data, model)
				seen[model.ID] = true
			}
		}
		for model := range account.GetModelMapping() {
			if IsGPTImageGenerationModel(model) && !strings.Contains(model, "*") && !seen[model] {
				payload.Data = append(payload.Data, openai.Model{ID: model, Object: "model", Type: "model", OwnedBy: "openai", DisplayName: model})
			}
		}
	}
	return payload.Data, nil
}

// NewAccountTestService creates a new AccountTestService
func NewAccountTestService(
	accountRepo AccountRepository,
	geminiTokenProvider *GeminiTokenProvider,
	claudeTokenProvider *ClaudeTokenProvider,
	grokTokenProvider *GrokTokenProvider,
	antigravityGatewayService *AntigravityGatewayService,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	tlsFPProfileService *TLSFingerprintProfileService,
) *AccountTestService {
	return &AccountTestService{
		accountRepo:               accountRepo,
		geminiTokenProvider:       geminiTokenProvider,
		claudeTokenProvider:       claudeTokenProvider,
		grokTokenProvider:         grokTokenProvider,
		antigravityGatewayService: antigravityGatewayService,
		httpUpstream:              httpUpstream,
		cfg:                       cfg,
		tlsFPProfileService:       tlsFPProfileService,
	}
}

func (s *AccountTestService) validateUpstreamBaseURL(raw string) (string, error) {
	if s.cfg == nil {
		return "", errors.New("config is not available")
	}
	if !s.cfg.Security.URLAllowlist.Enabled {
		return urlvalidator.ValidateURLFormat(raw, s.cfg.Security.URLAllowlist.AllowInsecureHTTP)
	}
	normalized, err := urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{
		AllowedHosts:     s.cfg.Security.URLAllowlist.UpstreamHosts,
		RequireAllowlist: true,
		AllowPrivate:     s.cfg.Security.URLAllowlist.AllowPrivateHosts,
	})
	if err != nil {
		return "", err
	}
	return normalized, nil
}

// generateSessionString generates a Claude Code style session string.
// The output format is determined by the UA version in claude.DefaultHeaders,
// ensuring consistency between the user_id format and the UA sent to upstream.
func generateSessionString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	hex64 := hex.EncodeToString(b)
	sessionUUID := uuid.New().String()
	uaVersion := ExtractCLIVersion(claude.DefaultHeaders["User-Agent"])
	return FormatMetadataUserID(hex64, "", sessionUUID, uaVersion), nil
}

// createTestPayload creates a Claude Code style test request payload
func createTestPayload(modelID string) (map[string]any, error) {
	sessionID, err := generateSessionString()
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type": "text",
						"text": "hi",
						"cache_control": map[string]string{
							"type": "ephemeral",
						},
					},
				},
			},
		},
		"system": []map[string]any{
			{
				"type": "text",
				"text": claudeCodeSystemPrompt,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		},
		"metadata": map[string]string{
			"user_id": sessionID,
		},
		"max_tokens":  1024,
		"temperature": 1,
		"stream":      true,
	}, nil
}

// TestAccountConnection tests an account's connection by sending a test request
// All account types use full Claude Code client characteristics, only auth header differs
// modelID is optional - if empty, defaults to claude.DefaultTestModel
// mode is optional - "compact" routes OpenAI accounts to the /responses/compact probe path
// opts is optional media (image/audio data URLs for real generation / STT).
func (s *AccountTestService) TestAccountConnection(c *gin.Context, accountID int64, modelID string, prompt string, mode string, opts ...AccountTestOptions) error {
	ctx := c.Request.Context()
	testOpts := firstAccountTestOptions(opts)

	// Get account
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return s.sendErrorAndEnd(c, "Account not found")
	}

	// Synthetic UI load-test accounts exercise the real SSE parsing and modal
	// interactions, but intentionally do not send their placeholder credentials
	// to an upstream provider.
	if account.IsSyntheticUITest() {
		testModelID := modelID
		if testModelID == "" {
			testModelID = claude.DefaultTestModel
		}
		s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})
		s.sendEvent(c, TestEvent{Type: "content", Text: "Synthetic Anthropic OAuth account is healthy and interactive."})
		s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
		return nil
	}

	// 影子账号（CodeBuddy 影子等）自身 credentials 恒空（运行时透传母账号），
	// 因此平台判定与凭证读取必须走母账号；account 仍保留用于身份与模型映射。
	// 与 testOpenAICompactConnection 同范式：resolveCredentialAccount 只解一层，
	// 且内置二级影子防御；解析失败即 fail-closed 报错，绝不拿空凭证去探活。
	credentialAccount := account
	if account.IsShadow() {
		resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to resolve account credentials")
		}
		credentialAccount = resolved
	}

	// web逆向账号必须先于官方CN平台API分支判定；归并后两者共享platform，
	// 以官方平台+access_mode=web双键SSOT严格隔离，避免web账号误读api_key。
	if ResolveWebPlatform(credentialAccount) != "" {
		return s.testWebAccountConnection(c, credentialAccount, modelID, prompt)
	}

	// Route to platform-specific test method
	if credentialAccount.Platform == PlatformOther {
		// other 双协议（chat_completions | anthropic），复用国产供应商的通用
		// 探活（其内部经 GetOpenAIBaseURL/GetOpenAIProtocolAPIKey 取上游 base 与密钥）。
		if credentialAccount.GetAPIProtocol() == APIProtocolAnthropic {
			return s.testCNProviderAnthropicConnection(c, credentialAccount, modelID)
		}
		return s.testCNProviderChatCompletionsConnection(c, credentialAccount, modelID, prompt)
	}

	if credentialAccount.IsCNProvider() {
		switch credentialAccount.GetAPIProtocol() {
		case APIProtocolAdaptive:
			return s.testCNProviderAdaptiveConnection(c, credentialAccount, modelID, prompt)
		case APIProtocolResponses:
			return s.testOpenAIAccountConnection(c, credentialAccount, modelID, prompt, normalizeAccountTestMode(mode))
		case APIProtocolChatCompletions:
			return s.testCNProviderChatCompletionsConnection(c, credentialAccount, modelID, prompt)
		case APIProtocolAnthropic:
			return s.testCNProviderAnthropicConnection(c, credentialAccount, modelID)
		}
	}

	if credentialAccount.IsOpenAI() {
		return s.testOpenAIAccountConnection(c, credentialAccount, modelID, prompt, normalizeAccountTestMode(mode))
	}

	if credentialAccount.IsGemini() {
		return s.testGeminiAccountConnection(c, credentialAccount, modelID, prompt)
	}

	if credentialAccount.Platform == PlatformGrok {
		return s.testGrokAccountConnection(c, credentialAccount, modelID, prompt, mode, testOpts)
	}

	if credentialAccount.Platform == PlatformAntigravity {
		return s.routeAntigravityTest(c, credentialAccount, modelID, prompt)
	}

	// CodeBuddy（含解析到 CodeBuddy 母账号的影子）：此前无任何测试出口，影子会落进
	// CN 分支读到自己的空凭证（"No API key available"），非影子则掉进 claude 兜底。
	if credentialAccount.IsCodeBuddy() {
		return s.testCodeBuddyAccountConnection(c, account, credentialAccount, modelID)
	}

	return s.testClaudeAccountConnection(c, account, modelID)
}

// codeBuddyTestAccessToken 取 CodeBuddy 探活的 Bearer token，与正式转发链同口径
// （OpenAIGatewayService.GetAccessToken 的 codebuddy 分支：OAuth → credentials.access_token、
// APIKey（ck_ 定制 key）→ credentials.api_key；与配额服务 setBillingHeaders 一致）。
// 不能用 GetOpenAIProtocolAPIKey / GetOpenAIAccessToken：前者对 CN provider 且
// Type != apikey 恒返回空，后者要求 IsOpenAI()，对 codebuddy 恒为空。
func codeBuddyTestAccessToken(credentialAccount *Account) string {
	if credentialAccount == nil {
		return ""
	}
	if token := strings.TrimSpace(credentialAccount.GetCredential("access_token")); token != "" {
		return token
	}
	if credentialAccount.Type == AccountTypeAPIKey {
		return strings.TrimSpace(credentialAccount.GetCredential("api_key"))
	}
	return ""
}

// testCodeBuddyAccountConnection 对 CodeBuddy 账号（含解析到 CodeBuddy 母账号的影子）
// 做轻量探活。
//
// 参数分工与正式转发 forwardCodeBuddy 一致：account 只承载身份与模型映射（影子的
// model_mapping 决定上游模型），credentialAccount（影子→母账号）承载站点与凭证。
//
// 探活端点选配额快照端点 POST {BillingBase}/v2/billing/meter/get-user-resource（body {}）：
//   - 只读、零模型消耗，比最小 token 的补全更廉价；
//   - intl 站点实测 HTTP 200 且 schema 与 CN 同构（docs/evidence/codebuddy-intl）；
//   - 绕开 intl 的两个坑：models 端点认证后 HTTP 500（动态模型不可用），
//     chat 补全要求首条消息为 system（否则 400 code=11128）。
//
// 出站域名/Origin/Referer 一律走站点 SSOT（codebuddy_site.go，缺省 cn）；
// Authorization 为 Bearer + token（与 buildCodeBuddyChatRequest 同口径）。
// 判定：2xx 且响应体通过内容校验（非空 + 无错误码）→ 健康；401/403 → 上游拒绝；
// 其余 → 失败。错误文案只含站点/端点/状态码，绝不回显凭证值。
func (s *AccountTestService) testCodeBuddyAccountConnection(c *gin.Context, account *Account, credentialAccount *Account, modelID string) error {
	ctx := c.Request.Context()
	if credentialAccount == nil {
		credentialAccount = account
	}
	site := credentialAccount.CodeBuddySite()
	ep := codeBuddyEndpointsFor(site)

	// 账号级 base_url 非空时显式校验，非法值直接报错（与 web/国产探活同范式），
	// 避免测试在错误出站目标上假通过。空值走站点默认。
	if rawBaseURL := strings.TrimSpace(credentialAccount.GetCredential("base_url")); rawBaseURL != "" {
		if _, err := s.validateUpstreamBaseURL(rawBaseURL); err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid base URL: %s", err.Error()))
		}
	}

	token := codeBuddyTestAccessToken(credentialAccount)
	if token == "" {
		return s.sendErrorAndEnd(c, "codebuddy account is missing access_token credential")
	}

	// 模型名只作展示/映射用途（探活端点不消费模型），映射取影子自身，与转发一致。
	testModelID := strings.TrimSpace(modelID)
	if testModelID != "" {
		testModelID = account.GetMappedModel(testModelID)
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})

	apiURL := strings.TrimRight(ep.BillingBase, "/") + codeBuddyBillingMeterPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create codebuddy probe request")
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", ep.OriginReferer)
	req.Header.Set("Referer", ep.OriginReferer+"/")
	req.Header.Set("User-Agent", CodeBuddyClientUA)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("Authorization", "Bearer "+token)
	// 身份头与配额服务 setBillingHeaders 同口径：空值用 X-No-*: 1 占位。
	uid := credentialAccount.GetCredential("uid")
	enterpriseID := credentialAccount.GetCredential("enterprise_id")
	domain := credentialAccount.GetCredential("domain")
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if enterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", enterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	if domain != "" {
		req.Header.Set("X-Domain", domain)
	} else {
		req.Header.Set("X-No-Domain", "1")
	}
	// 账号级请求头覆写最后应用，使管理员配置优先。
	credentialAccount.ApplyHeaderOverrides(req.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("codebuddy (%s) probe request failed: %s", site, err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return s.sendErrorAndEnd(c, fmt.Sprintf("codebuddy (%s) probe rejected by upstream (%s HTTP %d)", site, codeBuddyBillingMeterPath, resp.StatusCode))
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return s.sendErrorAndEnd(c, fmt.Sprintf("codebuddy (%s) probe failed (%s HTTP %d)", site, codeBuddyBillingMeterPath, resp.StatusCode))
	}
	// 2xx 不等于成功：配额端点的认证/业务错误随 HTTP 200 返回，上游异常时也可能只回
	// 空 body 或字符串 code。只看状态码会把失效账号判成 healthy —— 与 web 探活同源缺陷
	// （见 evaluateCodeBuddyProbeBody），必须按响应体内容判定后才能判成功。
	if reason := evaluateCodeBuddyProbeBody(raw); reason != "" {
		return s.sendErrorAndEnd(c, fmt.Sprintf("codebuddy (%s) probe rejected by upstream (%s HTTP %d, %s)", site, codeBuddyBillingMeterPath, resp.StatusCode, reason))
	}

	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("CodeBuddy (%s) account is healthy.", site)})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// CodeBuddy 探活 2xx 响应体的判定结论（拼在 "codebuddy (%s) probe rejected by upstream
// (%s HTTP %d, %s)" 末尾）。常量只描述结论本身，绝不回显上游原文（可能携带凭证/内部信息）。
const (
	// 空 body：响应体剔除首尾空白后没有任何内容（"upstream returned empty response" 与
	// antigravity_gateway_service.go 既有措辞对齐）。
	codeBuddyProbeReasonEmptyBody = "upstream returned empty response"
	// 认证类语义：code/msg/message 含 unauthenticated 等登录态失效信号。
	codeBuddyProbeReasonAuthErr = "upstream auth error (login state invalid)"
	// 业务信封非 0：code 为数字且 != 0，或为非空且非 "0" 的字符串。
	codeBuddyProbeReasonBusinessErr = "upstream business error"
)

// evaluateCodeBuddyProbeBody 判定 CodeBuddy 探活 2xx 响应体：返回 "" 表示健康，非空为
// 已脱敏的失败结论。仅当「body 非空 且 无错误码（数字 != 0 / 字符串非空且 != "0"）」
// 时才判健康。
//
// 背景（线上故障同源）：只看状态码时两种形态会被误判成功 ——
//   - 空 body：gjson 取不到 code → 直接跳过判成功；
//   - {"code":"unauthenticated"}：gjson.Int() 对字符串返回 0 → 同样跳过。
func evaluateCodeBuddyProbeBody(raw []byte) string {
	body := bytes.TrimSpace(raw)
	if len(body) == 0 {
		return codeBuddyProbeReasonEmptyBody
	}
	if codeBuddyProbeBodyHasAuthError(body) {
		return codeBuddyProbeReasonAuthErr
	}
	if reason, bad := codeBuddyProbeEnvelopeCode(body); bad {
		return reason
	}
	return ""
}

// codeBuddyProbeBodyHasAuthError 识别认证类语义：只看 code / msg / message 三个顶层字段，
// 与 web kimi 的分类器同口径（strings.Contains(lower, "unauthenticated")），不回显字段原文。
func codeBuddyProbeBodyHasAuthError(body []byte) bool {
	for _, field := range []string{"code", "msg", "message"} {
		value := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, field).String()))
		if strings.Contains(value, "unauthenticated") || strings.Contains(value, "unauthorized") {
			return true
		}
	}
	return false
}

// codeBuddyProbeEnvelopeCode 解析业务信封 code：
//   - 数字：!= 0 → 失败，回显数字 code（数值 ID 非凭证），保持既有 code=%d 口径；
//   - 字符串：非空且 != "0" → 失败；仅整串为数字时才回显，避免把上游原文拼进文案。
//
// 其余类型（bool / object / null / 不存在）按无信封处理。
func codeBuddyProbeEnvelopeCode(body []byte) (string, bool) {
	code := gjson.GetBytes(body, "code")
	if !code.Exists() {
		return "", false
	}
	switch code.Type {
	case gjson.Number:
		if code.Int() != 0 {
			return fmt.Sprintf("%s (code=%d)", codeBuddyProbeReasonBusinessErr, code.Int()), true
		}
	case gjson.String:
		value := strings.TrimSpace(code.String())
		if value == "" || value == "0" {
			return "", false
		}
		if allDigits(value) {
			return fmt.Sprintf("%s (code=%s)", codeBuddyProbeReasonBusinessErr, value), true
		}
		return codeBuddyProbeReasonBusinessErr, true
	}
	return "", false
}

// allDigits 判断字符串是否全为 ASCII 数字（决定字符串 code 能否安全回显到错误文案）。
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (s *AccountTestService) testCNProviderChatCompletionsConnection(c *gin.Context, account *Account, modelID string, prompt string) error {
	testModelID := strings.TrimSpace(modelID)
	if testModelID == "" {
		testModelID = openai.DefaultTestModel
	}
	testModelID = account.GetMappedModel(testModelID)

	authToken := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if authToken == "" {
		return s.sendErrorAndEnd(c, "No API key available")
	}

	baseURL := account.GetOpenAIBaseURL()
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid base URL: %s", err.Error()))
	}

	return s.testOpenAIChatCompletionsConnection(c, account, testModelID, prompt, normalizedBaseURL, authToken)
}

// resolveWebTestModel 把账号测试选中的 modelID 归一到实际出站模型：
//  1. 优先使用非空 modelID；
//  2. 应用账号现有 model_mapping（复用 GetMappedModel，不另写映射逻辑）；
//  3. 仅当 modelID 为空时，回落 DefaultWebModelIDs 的平台默认值（公开名口径，
//     与 /models 列表和 model_mapping 键一致；出站归一由各平台转发链同款函数完成）。
//
// 返回值即最终出站模型名（与 test_start 事件、请求体 model 字段一致）。
func resolveWebTestModel(account *Account, modelID, platform string) string {
	candidate := strings.TrimSpace(modelID)
	if candidate == "" {
		if ids := DefaultWebModelIDs(platform, AccountAccessModeWeb); len(ids) > 0 {
			candidate = ids[0]
		}
	}
	return account.GetMappedModel(candidate)
}

// testWebAccountConnection 对 web 逆向平台账号做轻量官方探活。
//
// 与 Claude / OpenAI API Key 走不同协议，web 平台登录态载体是整串 Cookie
// （deepseek web / zhipu web）或 Kimi access_token（kimi web）。探活只构造一个
// 最小出站请求打到达对应官方对话端点，按「状态码 + 响应流内容」双口径判定：
//   - 2xx → 继续消费响应体校验有效帧：≥1 个有效帧且无错误帧 → 登录态有效；
//     空 body / 只有心跳 / 上游业务错误帧 → 失败（见 evaluateWebProbeStream）；
//   - 401/403 → 上游拒绝（历史 401 不足为凭：测试链此前不完整，不能断言凭证失效）；
//   - 其余 → 请求失败。
//
// 双口径由来（线上故障）：Kimi Connect RPC 的认证/业务错误随 HTTP 200 返回，上游异常时
// 也可能只回空 body 或纯心跳帧，只看状态码会把过期 access_token 的坏账号判成 healthy。
// 错误文案只含平台/端点/状态码，绝不回显凭证值（含上游原文，可能携带凭证）。
func (s *AccountTestService) testWebAccountConnection(c *gin.Context, account *Account, modelID string, prompt string) error {
	ctx := c.Request.Context()

	// 平台归并重构：web 接入模式账号的 platform 已是官方值（zhipu/deepseek/kimi），
	// 但 web 探活 / 转发链仍按 web-* 平台分支组织，这里归一到 web 平台值，保持探活逻辑不变。
	webPlatform := ResolveWebPlatform(account)
	if webPlatform == "" {
		webPlatform = account.Platform
	}

	// #4：账号级 base_url 非空时显式校验，非法值直接报错，不静默回落平台默认
	// （避免转发侧 fail-closed 把非法值吞掉、测试假通过）。合法或空才继续（空走默认）。
	if rawBaseURL := strings.TrimSpace(account.GetCredential("base_url")); rawBaseURL != "" {
		if _, err := ValidateWebBaseURL(webPlatform, rawBaseURL); err != nil {
			return s.sendErrorAndEnd(c, "invalid base_url")
		}
	}

	baseURL := strings.TrimRight(account.GetWebBaseURL(), "/")
	var (
		chatPath     string
		testModel    string
		req          *http.Request
		probeReqBody []byte // kimi/zhipu 出站请求体（401 续期重试时按新凭据重建请求复用）
	)
	switch webPlatform {
	case PlatformDeepseek:
		if baseURL == "" {
			baseURL = webDeepseekDefaultBaseURL
		}
		chatPath = webDeepseekChatCompletionPath
		testModel = resolveWebTestModel(account, modelID, PlatformDeepseek)
		cookie := strings.TrimSpace(account.GetCredential("cookie"))
		if cookie == "" {
			return s.sendErrorAndEnd(c, "deepseek web account is missing login cookie credential")
		}
		// waf_cookie 追加由 buildWebDeepseekUpstreamRequest 内部统一处理，此处不再预拼。
		// 复用原生 model_type 归一（与正式转发 forwardWebDeepseek 同口径，09 §4 实测字段名）。
		modelType, thinkingEnabled := webDeepseekModelClass(testModel)
		// 新协议探活与正式转发同链：登录态强制 PoW（09 §3）+ 自动建会话（09 §1）。
		// 复用 openaiGatewayService 的同一实现，禁止第二套探活链（与 zhipu 探活同模式）。
		if s.openaiGatewayService == nil {
			return s.sendErrorAndEnd(c, "openai gateway service is unavailable for deepseek web probe")
		}
		powHeader, err := s.openaiGatewayService.fetchWebDeepseekPoWHeader(ctx, account, baseURL, cookie, accountProxyURL(account))
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("deepseek web probe failed to obtain PoW challenge: %v", err))
		}
		sessionID, err := s.openaiGatewayService.ensureWebDeepseekSession(ctx, account, baseURL, cookie, accountProxyURL(account))
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("deepseek web probe failed to create chat session: %v", err))
		}
		built, err := buildWebDeepseekCompletionBody(account, sessionID, modelType, thinkingEnabled, "hi")
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to build deepseek web probe request")
		}
		// 复用正式转发链构造函数：头族（x-client-*、Origin/Referer/UA、Cookie+waf_cookie）
		// 与真实转发完全一致，禁止第二套头逻辑；PoW 头随后单独附加。
		r, err := s.openaiGatewayService.buildWebDeepseekUpstreamRequest(ctx, account, baseURL+webDeepseekChatCompletionPath, cookie, built)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to build deepseek web probe request")
		}
		if powHeader != "" {
			r.Header.Set("X-Ds-PoW-Response", powHeader)
		}
		// 账号级请求头覆写：探活请求与真实转发保持一致的最终头。
		account.ApplyHeaderOverrides(r.Header)
		req = r
	case PlatformZhipu:
		if baseURL == "" {
			baseURL = DefaultWebZhipuBaseURL
		}
		chatPath = webZhipuStreamPath
		// 优先非空 modelID → 现有 model_mapping（GetMappedModel）→ 默认 DefaultWebModelIDs。
		testModel = resolveWebTestModel(account, modelID, PlatformZhipu)
		cookie := strings.TrimSpace(account.GetCredential("cookie"))
		if cookie == "" {
			return s.sendErrorAndEnd(c, "zhipu web account is missing login cookie credential")
		}
		// 探活与正式转发共用同一登录态判定：游客态 token（is_guest=true）出站必被上游
		// 以 HTTP 400 {"status":40011} 拒绝，属登录态问题而非凭证失效，提前失败关闭
		// （不重复实现 JWT 解析，直接调用 webZhipuTokenIsGuest，token 取值口径与正式
		// 转发链完全一致）。错误信息不得含任何 token/cookie 内容。
		if webZhipuTokenIsGuest(webZhipuResolveChatGLMToken(account)) {
			return s.sendErrorAndEnd(c, "zhipu web login state is a guest token, please re-capture login cookie")
		}
		// 复用正式转发链构造函数：完整指纹头（Content-Type/Accept/Authorization Bearer/
		// app-name/x-app-*/x-device-id/Origin/Referer/User-Agent/Cookie(+cdn_cookie)/
		// 账号头覆写），禁止第二套 GLM 头逻辑。
		reqBody := buildWebZhipuRequestBody([]webZhipuMessage{{
			Role:    "user",
			Content: []webZhipuMessageContent{{Type: "text", Text: "hi"}},
		}}, testModel, account)
		if s.openaiGatewayService == nil {
			return s.sendErrorAndEnd(c, "openai gateway service is unavailable for zhipu web probe")
		}
		r, err := s.openaiGatewayService.buildWebZhipuUpstreamRequest(ctx, account, baseURL+webZhipuStreamPath, cookie, reqBody)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to build zhipu web probe request")
		}
		req = r
		probeReqBody = reqBody
	case PlatformKimi:
		if baseURL == "" {
			baseURL = webKimiDefaultBaseURL
		}
		chatPath = webKimiChatPath
		testModel = resolveWebTestModel(account, modelID, PlatformKimi)
		accessToken := strings.TrimSpace(account.GetCredential("access_token"))
		if accessToken == "" {
			return s.sendErrorAndEnd(c, "kimi web account is missing access_token credential")
		}
		// 出站归一与正式转发同款（forwardWebKimi 同口径）：公开名 kimi-k3 → k3。
		// test_start 事件保留公开名，出站请求体用归一后内部名。
		reqBody := buildWebKimiRequestBody("hi", webKimiModelName(testModel), account)
		// 复用正式转发链构造函数：Connect RPC 头族（content-type/accept application/connect+json、
		// x-language/x-msh-*、双载体认证）与真实转发完全一致，禁止第二套头逻辑。
		if s.openaiGatewayService == nil {
			return s.sendErrorAndEnd(c, "openai gateway service is unavailable for kimi web probe")
		}
		r, err := s.openaiGatewayService.buildWebKimiUpstreamRequest(ctx, account, baseURL+webKimiChatPath, accessToken, reqBody)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to build kimi web probe request")
		}
		// 账号级请求头覆写：探活请求与真实转发保持一致的最终头。
		account.ApplyHeaderOverrides(r.Header)
		req = r
		probeReqBody = reqBody
	default:
		return s.testClaudeAccountConnection(c, account, modelID)
	}

	// SSE 响应头：与真实转发口径一致，便于前端复用同一套 test 事件解析。
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModel})
	s.sendEvent(c, TestEvent{Type: "status", Text: "正在通过 " + chatPath + " 探活 web 登录态"})

	if req == nil {
		return s.sendErrorAndEnd(c, "web probe request was not constructed")
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("web upstream (%s) request failed: %s", chatPath, err.Error()))
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// 2xx 不等于成功：Kimi Connect / zhipu / deepseek 的业务与认证错误都可能随
		// HTTP 200 返回，且上游异常时可能只回空 body 或纯心跳帧。必须消费响应体并
		// 校验有效帧（≥1 个有效帧且无错误帧），否则坏账号会被判成 healthy。
		if reason := evaluateWebProbeStream(webPlatform, resp.Body); reason != "" {
			return s.sendErrorAndEnd(c, fmt.Sprintf("web upstream (%s) returned %s", chatPath, reason))
		}
		s.sendEvent(c, TestEvent{Type: "content", Text: "Web login session is healthy."})
		s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// 401 单口径：与真实转发链同款 401→refresh→重试一次（kimi/zhipu 调用与
		// forwardWebKimi / forwardWebZhipu 完全同一 refresh 实现，对原账号凭据续期，
		// 无任何备用账号语义）。刷新成功 → 新凭据重建请求重试一次；再失败/无
		// refresh_token/刷新失败 → 落入下方既有文案分支。403 是封禁/拒绝语义，
		// refresh 无意义，不续期（与转发链一致）。deepseek 转发链本无 401→refresh
		// 语义，同样不续期（单口径=各自平台与转发对齐）。
		if resp.StatusCode == http.StatusUnauthorized &&
			(webPlatform == PlatformKimi || webPlatform == PlatformZhipu) && s.openaiGatewayService != nil {
			retryResp, retryErr := s.retryWebProbeWithRefresh(c, account, webPlatform, baseURL, chatPath, probeReqBody, proxyURL)
			if retryErr == nil && retryResp != nil {
				switch {
				case retryResp.StatusCode >= 200 && retryResp.StatusCode < 300:
					// 与首发同口径：续期成功不等于登录态可用，2xx 仍须消费流校验有效帧。
					if reason := evaluateWebProbeStream(webPlatform, retryResp.Body); reason != "" {
						_, _ = io.Copy(io.Discard, retryResp.Body)
						_ = retryResp.Body.Close()
						return s.sendErrorAndEnd(c, fmt.Sprintf("web upstream (%s) returned %s", chatPath, reason))
					}
					s.sendEvent(c, TestEvent{Type: "content", Text: "Web login session is healthy."})
					s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
					return nil
				case retryResp.StatusCode == http.StatusUnauthorized || retryResp.StatusCode == http.StatusForbidden:
					// 续期后仍被拒 → 落入既有口径报错（续期无效/凭证真失效）。
				default:
					return s.sendErrorAndEnd(c, fmt.Sprintf("web upstream (%s) returned HTTP %d", chatPath, retryResp.StatusCode))
				}
				resp = retryResp
			}
			// retryErr != nil（重建请求失败）或刷新失败/无 refresh_token → 落入既有文案分支。
		}
		// zhipu web 测试链此前不完整（缺指纹头），历史 401 不足为凭，不得直接断言凭证失效；
		// 只回显平台、端点与状态码，绝不回显 Cookie / access_token。
		// deepseek/kimi 探活头已完整（回归红线 #5），保留既有「凭证失效」判定与文案。
		if account.Platform == PlatformZhipu {
			return s.sendErrorAndEnd(c, fmt.Sprintf("web probe rejected by upstream (%s %s HTTP %d)", account.Platform, chatPath, resp.StatusCode))
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("web login credential is invalid (HTTP %d)", resp.StatusCode))
	default:
		return s.sendErrorAndEnd(c, fmt.Sprintf("web upstream (%s) returned HTTP %d", chatPath, resp.StatusCode))
	}
}

// web 探活 2xx 响应体的判定结论（拼在 "web upstream (%s) returned %s" 之后）。
// 常量只描述结论本身，绝不回显上游原文（上游错误体可能携带凭证/内部信息）。
const (
	// 空流：正常结束（done / EOF）但没有任何有效帧（空 body、只有心跳、不可识别结构）。
	webProbeReasonEmptyStream = "an empty response (no valid frames parsed)"
	// 上游业务错误：HTTP 200 + 非 0 code 帧（zhipu code / deepseek code|data.biz_code）。
	webProbeReasonBusinessErr = "an upstream business error (login state rejected)"
	// 上游认证错误：帧内 code/message 含 unauthenticated（kimi 登录态失效典型形态）。
	webProbeReasonAuthErr = "an upstream auth error (unauthenticated)"
)

// webProbeMaxStreamBytes 探活响应体的读取上限：判定只需前若干帧，无需消费整条长流
// （超出部分由调用方的 drain defer 丢弃）。
const webProbeMaxStreamBytes = 1 << 20

// evaluateWebProbeStream 消费 web 探活 2xx 响应体并判定登录态是否真的可用：
// 返回 "" 表示健康（≥1 个有效帧且无错误帧），非空为失败原因（已脱敏）。
//
// 背景（线上故障）：Kimi Connect RPC 的认证/业务错误随 HTTP 200 返回，上游异常时也可能
// 只回空 body 或纯心跳帧 —— 只看 HTTP 状态码会把坏账号判成 healthy。
//
// 解析器一律复用转发链既有实现，禁止第二套：
//   - kimi: readWebKimiConnectEnvelope + parseWebKimiEnvelopePayload；
//   - zhipu: parseWebZhipuSSEFrame + mapWebZhipuPayload（裸 JSON 走 webZhipuBareJSONError）；
//   - deepseek: webDeepseekNextSSEEvent + webDeepseekSSEParser.applyDelta。
//
// ⚠️ 不可用 usage 判空：kimi / zhipu 流内无 usage 字段（转发链靠 estimateWebUsage 本地估算）。
func evaluateWebProbeStream(webPlatform string, body io.Reader) string {
	limited := io.LimitReader(body, webProbeMaxStreamBytes)
	switch webPlatform {
	case PlatformKimi:
		return evaluateWebKimiProbeStream(limited)
	case PlatformZhipu:
		return evaluateWebZhipuProbeStream(limited)
	case PlatformDeepseek:
		return evaluateWebDeepseekProbeStream(limited)
	}
	// 未知平台不做内容判定（保持既有状态码口径），避免误伤。
	return ""
}

// evaluateWebKimiProbeStream 逐帧读 Connect envelope 判定 kimi 登录态。
// 有效信号：TextDelta / ThinkDelta / AssistantID / ChatID / Done（与转发链同款字段）。
func evaluateWebKimiProbeStream(body io.Reader) string {
	reader := bufio.NewReader(body)
	// Connect 流首字节是 envelope flag（实测 0x00）。以 '{' 起始说明上游回了裸 JSON
	// （异常形态，如 {"code":"unauthenticated"}）：此时按二进制帧读会把长度字段解释成
	// 巨型帧，故直接用同一个 parseWebKimiEnvelopePayload 判定。
	if head, err := reader.Peek(1); err == nil && head[0] == '{' {
		raw, _ := io.ReadAll(reader)
		// 裸 JSON 分支与下方帧分支同口径：先判认证/业务错误，再看有效帧（chat.id /
		// 正文 / assistant id / done）。此前本分支无条件返回 empty，会把
		// {"error":{"code":"invalid_argument"}} 报成「空流」，掩盖真实错误。
		ev := parseWebKimiEnvelopePayload(raw)
		switch {
		case ev.AuthFailed:
			return webProbeReasonAuthErr
		case ev.ErrCode != 0 || ev.ErrCodeStr != "":
			return webProbeReasonBusinessErr
		}
		if ev.TextDelta != "" || ev.ThinkDelta != "" || ev.AssistantID != "" || ev.ChatID != "" || ev.Done {
			return ""
		}
		return webProbeReasonEmptyStream
	}

	validFrames := 0
	for {
		payload, more, err := readWebKimiConnectEnvelope(reader)
		if err != nil || !more {
			break // 读错/截断按流结束处理，结论交给有效帧计数
		}
		if len(payload) == 0 {
			continue // 零长 keepalive 帧
		}
		ev := parseWebKimiEnvelopePayload(payload)
		switch {
		case ev.AuthFailed:
			return webProbeReasonAuthErr
		case ev.ErrCode != 0 || ev.ErrCodeStr != "":
			return webProbeReasonBusinessErr
		case ev.Heartbeat:
			continue
		}
		if ev.TextDelta != "" || ev.ThinkDelta != "" || ev.AssistantID != "" || ev.ChatID != "" || ev.Done {
			validFrames++
		}
	}
	if validFrames == 0 {
		return webProbeReasonEmptyStream
	}
	return ""
}

// evaluateWebZhipuProbeStream 逐帧读 /assistant/stream SSE 判定 zhipu 登录态。
// 有效信号：正文增量 / response id / 终止标记（mapWebZhipuPayload 与转发链同款字段）。
func evaluateWebZhipuProbeStream(body io.Reader) string {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), webProbeMaxStreamBytes)
	validFrames := 0
	for scanner.Scan() {
		line := scanner.Text()
		if payload, ok := parseWebZhipuSSEFrame(line); ok {
			view := mapWebZhipuPayload(payload)
			if view.ErrCode != 0 {
				return webProbeReasonBusinessErr
			}
			if view.Content != "" || view.ResponseID != "" || view.FinishReason != "" {
				validFrames++
			}
			continue
		}
		// 非 SSE 行：裸 JSON 业务错误与转发链同口径（webZhipuBareJSONError）。
		if _, isErr := webZhipuBareJSONError(line); isErr {
			return webProbeReasonBusinessErr
		}
	}
	if validFrames == 0 {
		return webProbeReasonEmptyStream
	}
	return ""
}

// evaluateWebDeepseekProbeStream 逐事件读 completion SSE 判定 deepseek 登录态。
// 有效信号：正文增量 / response id（首帧完整 response）/ 终止标记；
// 顶层 code 或 data.biz_code 非 0 → 业务错误（与转发链双路径同口径）。
func evaluateWebDeepseekProbeStream(body io.Reader) string {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), webProbeMaxStreamBytes)
	parser := newWebDeepseekSSEParser()
	validFrames := 0
	for {
		_, data, ok := webDeepseekNextSSEEvent(scanner)
		if !ok {
			break
		}
		prevResponseID := parser.responseID
		prevFinished := parser.finished
		delta, isErr, _, _ := parser.applyDelta(data)
		if isErr {
			return webProbeReasonBusinessErr
		}
		if delta != "" || parser.responseID != prevResponseID || parser.finished != prevFinished {
			validFrames++
		}
	}
	if validFrames == 0 {
		return webProbeReasonEmptyStream
	}
	return ""
}

// retryWebProbeWithRefresh 探活 401 的单口径静默续期重试：按平台调用与真实转发链
// **完全同一** 的 refresh 实现（kimi: refreshWebKimiAccessToken；zhipu:
// refreshWebZhipuAccessToken），对原账号凭据续期，无任何备用账号语义。刷新成功时补发
// 一条脱敏 status 事件（不含任何 token 值），并用新凭据经同一 build*UpstreamRequest
// 重建探活请求重试一次。刷新失败/无 refresh_token/重建失败 → 返回 (nil, err/nil)，
// 由调用方落入既有 401 文案分支（不发续期 status 事件，与转发链行为一致）。
func (s *AccountTestService) retryWebProbeWithRefresh(
	c *gin.Context,
	account *Account,
	webPlatform string,
	baseURL string,
	chatPath string,
	reqBody []byte,
	proxyURL string,
) (*http.Response, error) {
	gw := s.openaiGatewayService
	if gw == nil {
		return nil, fmt.Errorf("openai gateway service is unavailable for web probe refresh")
	}
	var newToken string
	switch webPlatform {
	case PlatformKimi:
		// 与 forwardWebKimi（web_kimi_gateway_forward.go:141）同一刷新实现。
		newToken = gw.refreshWebKimiAccessToken(c.Request.Context(), account, proxyURL)
	case PlatformZhipu:
		// 与 forwardWebZhipu（web_zhipu_gateway_forward.go:260）同一刷新实现；
		// 刷新就地更新 cookie 串中的 chatglm_token/chatglm_refresh_token 并同步内存
		// account.Credentials，重试请求按转发链同款取法取最新 cookie。
		newToken = gw.refreshWebZhipuAccessToken(c.Request.Context(), account)
	default:
		return nil, fmt.Errorf("platform %s does not support web probe refresh", webPlatform)
	}
	if newToken == "" {
		return nil, fmt.Errorf("web probe refresh returned no new credential")
	}

	// 刷新成功，补发脱敏续期 status（不得包含任何 token/cookie 值）。
	s.sendEvent(c, TestEvent{Type: "status", Text: "登录态已过期，正在用 refresh token 静默续期并重试"})

	var (
		req *http.Request
		err error
	)
	switch webPlatform {
	case PlatformKimi:
		req, err = gw.buildWebKimiUpstreamRequest(c.Request.Context(), account, baseURL+webKimiChatPath, newToken, reqBody)
	case PlatformZhipu:
		newCookie := strings.TrimSpace(account.GetCredential("cookie"))
		req, err = gw.buildWebZhipuUpstreamRequest(c.Request.Context(), account, baseURL+webZhipuStreamPath, newCookie, reqBody)
	}
	if err != nil {
		return nil, err
	}
	account.ApplyHeaderOverrides(req.Header)
	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		return nil, fmt.Errorf("web upstream (%s) retry request failed: %s", chatPath, err.Error())
	}
	return resp, nil
}

// testClaudeAccountConnection tests an Anthropic Claude account's connection
func (s *AccountTestService) testClaudeAccountConnection(c *gin.Context, account *Account, modelID string) error {
	ctx := c.Request.Context()

	// Determine the model to use
	testModelID := modelID
	if testModelID == "" {
		testModelID = claude.DefaultTestModel
	}

	// API Key 账号测试连接时也需要应用通配符模型映射。
	if account.Type == "apikey" {
		testModelID = account.GetMappedModel(testModelID)
	}

	// Bedrock accounts use a separate test path
	if account.IsBedrock() {
		return s.testBedrockAccountConnection(c, ctx, account, testModelID)
	}
	if account.Type == AccountTypeServiceAccount {
		return s.testClaudeVertexServiceAccountConnection(c, ctx, account, testModelID)
	}

	// Determine authentication method and API URL
	var authToken string
	var apiURL string

	if account.IsOAuth() {
		apiURL = testClaudeAPIURL
		authToken = account.GetCredential("access_token")
		if authToken == "" {
			return s.sendErrorAndEnd(c, "No access token available")
		}
	} else if account.Type == "apikey" {
		authToken = account.GetCredential("api_key")
		if authToken == "" {
			return s.sendErrorAndEnd(c, "No API key available")
		}

		baseURL := account.GetBaseURL()
		if baseURL == "" {
			baseURL = "https://api.anthropic.com"
		}
		normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid base URL: %s", err.Error()))
		}
		apiURL = strings.TrimSuffix(normalizedBaseURL, "/") + "/v1/messages?beta=true"
	} else {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Unsupported account type: %s", account.Type))
	}

	// Set SSE headers
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// Create Claude Code style payload (same for all account types)
	payload, err := createTestPayload(testModelID)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create test payload")
	}
	payloadBytes, _ := json.Marshal(payload)

	// Send test_start event
	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create request")
	}

	// Set common headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	// Apply Claude Code client headers
	for key, value := range claude.DefaultHeaders {
		req.Header.Set(key, value)
	}

	// Set authentication header
	if account.IsOAuth() {
		req.Header.Set("anthropic-beta", claude.DefaultBetaHeader)
		req.Header.Set("Authorization", "Bearer "+authToken)
	} else {
		req.Header.Set("anthropic-beta", claude.APIKeyBetaHeader)
		// Ollama Cloud Anthropic 兼容端点按实际 base_url 强制 Bearer，
		// 其余保持 extra/default 行为。
		setAnthropicAPIKeyAuthHeader(req.Header, account, authToken, account.GetBaseURL())
	}

	// 账号级请求头覆写：测试请求与真实转发保持一致的最终头
	account.ApplyHeaderOverrides(req.Header)

	// Get proxy URL
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		errMsg := fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(body))

		// 403 表示账号被上游封禁，标记为 error 状态
		if resp.StatusCode == http.StatusForbidden {
			_ = s.accountRepo.SetError(ctx, account.ID, errMsg)
		}

		return s.sendErrorAndEnd(c, errMsg)
	}

	// Process SSE stream
	return s.processClaudeStream(c, resp.Body)
}

func (s *AccountTestService) testClaudeVertexServiceAccountConnection(c *gin.Context, ctx context.Context, account *Account, testModelID string) error {
	if mappedModel, matched := account.ResolveMappedModel(testModelID); matched {
		testModelID = mappedModel
	} else {
		testModelID = normalizeVertexAnthropicModelID(claude.NormalizeModelID(testModelID))
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	payload, err := createTestPayload(testModelID)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create test payload")
	}
	payloadBytes, _ := json.Marshal(payload)
	vertexBody, err := buildVertexAnthropicRequestBody(payloadBytes)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to create Vertex request body: %s", err.Error()))
	}

	if s.claudeTokenProvider == nil {
		return s.sendErrorAndEnd(c, "Claude token provider not configured")
	}
	accessToken, err := s.claudeTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to get service account access token: %s", err.Error()))
	}

	fullURL, err := buildVertexAnthropicURL(account.VertexProjectID(), account.VertexLocation(testModelID), testModelID, true)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to build Vertex URL: %s", err.Error()))
	}

	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(vertexBody))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		errMsg := fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(body))
		if resp.StatusCode == http.StatusForbidden {
			_ = s.accountRepo.SetError(ctx, account.ID, errMsg)
		}
		return s.sendErrorAndEnd(c, errMsg)
	}

	return s.processClaudeStream(c, resp.Body)
}

// testBedrockAccountConnection tests a Bedrock (SigV4 or API Key) account using non-streaming invoke
func (s *AccountTestService) testBedrockAccountConnection(c *gin.Context, ctx context.Context, account *Account, testModelID string) error {
	region := bedrockRuntimeRegion(account)
	resolvedModelID, ok := ResolveBedrockModelID(account, testModelID)
	if !ok {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Unsupported Bedrock model: %s", testModelID))
	}
	testModelID = resolvedModelID

	// Set SSE headers (test UI expects SSE)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// Create a minimal Bedrock-compatible payload (no stream, no cache_control)
	bedrockPayload := map[string]any{
		"anthropic_version": "bedrock-2023-05-31",
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type": "text",
						"text": "hi",
					},
				},
			},
		},
		"max_tokens":  256,
		"temperature": 1,
	}
	bedrockBody, _ := json.Marshal(bedrockPayload)

	// Use non-streaming endpoint (response is standard Claude JSON)
	apiURL := BuildBedrockURL(region, testModelID, false)

	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(bedrockBody))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create request")
	}
	req.Header.Set("Content-Type", "application/json")

	// Sign or set auth based on account type
	if account.IsBedrockAPIKey() {
		apiKey := account.GetCredential("api_key")
		if apiKey == "" {
			return s.sendErrorAndEnd(c, "No API key available")
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		signer, err := NewBedrockSignerFromAccount(account)
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to create Bedrock signer: %s", err.Error()))
		}
		if err := signer.SignRequest(ctx, req, bedrockBody); err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to sign request: %s", err.Error()))
		}
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, nil)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return s.sendErrorAndEnd(c, fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(body)))
	}

	// Bedrock non-streaming response is standard Claude JSON, extract the text
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to parse response: %s", err.Error()))
	}

	text := ""
	if len(result.Content) > 0 {
		text = result.Content[0].Text
	}
	if text == "" {
		text = "(empty response)"
	}

	s.sendEvent(c, TestEvent{Type: "content", Text: text})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// testOpenAIAccountConnection tests an OpenAI account's connection
func (s *AccountTestService) testOpenAIAccountConnection(c *gin.Context, account *Account, modelID string, prompt string, mode string) error {
	ctx := c.Request.Context()
	mode = normalizeAccountTestMode(mode)

	// Default to openai.DefaultTestModel for OpenAI testing
	testModelID := modelID
	if testModelID == "" {
		testModelID = openai.DefaultTestModel
	}

	// Align test routing with gateway behavior: OpenAI accounts apply normal
	// account model mapping. Native remote compaction v2 rides the ordinary
	// /responses wire and does NOT apply the legacy compact-only mapping
	// (post-#5641 semantics: compact_model_mapping is /responses/compact-only).
	testModelID = account.GetMappedModel(testModelID)
	if mode == AccountTestModeCompact {
		return s.testOpenAICompactConnection(c, account, testModelID)
	}

	// Route to image generation test if an image model is selected
	if isOpenAIImageModel(testModelID) {
		imagePrompt := strings.TrimSpace(prompt)
		if imagePrompt == "" {
			imagePrompt = defaultOpenAIImageTestPrompt
		}
		if account.Type == "apikey" {
			return s.testOpenAIImageAPIKey(c, ctx, account, testModelID, imagePrompt)
		}
		return s.testOpenAIImageOAuth(c, ctx, account, testModelID, imagePrompt)
	}

	credentialAccount := account
	if account.IsCredentialShadow() {
		resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
		credentialAccount = resolved
	}

	// Determine authentication method and API URL
	var authToken string
	var apiURL string
	var isOAuth bool

	if credentialAccount.IsOAuth() {
		isOAuth = true
		// Agent Identity signs each request and does not retain the OAuth token.
		if !credentialAccount.IsOpenAIAgentIdentity() {
			authToken = credentialAccount.GetOpenAIAccessToken()
		}
		if authToken == "" && !credentialAccount.IsOpenAIAgentIdentity() {
			return s.sendErrorAndEnd(c, "No access token available")
		}

		// OAuth uses ChatGPT internal API
		apiURL = chatgptCodexAPIURL
	} else if credentialAccount.Type == "apikey" {
		// API Key - use Platform API
		authToken = credentialAccount.GetOpenAIProtocolAPIKey()
		if authToken == "" {
			return s.sendErrorAndEnd(c, "No API key available")
		}

		baseURL := credentialAccount.GetOpenAIBaseURL()
		if baseURL == "" {
			baseURL = "https://api.openai.com"
		}
		normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid base URL: %s", err.Error()))
		}
		if !openai_compat.ShouldUseResponsesAPI(account.Extra) {
			return s.testOpenAIChatCompletionsConnection(c, account, testModelID, prompt, normalizedBaseURL, authToken)
		}
		apiURL = openAIResponsesURLForBase(credentialAccount.Platform, normalizedBaseURL)
	} else {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Unsupported account type: %s", account.Type))
	}

	// Set SSE headers
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// Create OpenAI Responses API payload. OAuth accounts use ChatGPT Codex
	// upstream and must apply the same model normalization as real forwarding.
	upstreamTestModelID := testModelID
	if isOAuth {
		upstreamTestModelID = normalizeOpenAIModelForUpstream(credentialAccount, testModelID)
	}
	payload := createOpenAITestPayload(upstreamTestModelID, isOAuth)
	payloadBytes, _ := json.Marshal(payload)

	// Send test_start event once. A task-invalid Agent Identity response may
	// restart this probe after registering a replacement task.
	if !agentIdentityTaskRecoveryWasTried(ctx) {
		s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create request")
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// Set common headers
	req.Header.Set("Content-Type", "application/json")
	if !isOAuth {
		applyOpenAICodexProbeHeaders(req.Header)
	}
	if credentialAccount.IsOpenAIAgentIdentity() {
		authHeaders, authErr := buildAgentIdentityAuthenticationHeaders(ctx, s.accountRepo, s.agentIdentityWS, &s.agentIdentityTaskMu, credentialAccount)
		if authErr != nil {
			return s.sendErrorAndEnd(c, "Failed to build Agent Identity authentication")
		}
		for key, values := range authHeaders {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	// Set OAuth-specific headers for ChatGPT internal API
	if isOAuth {
		req.Host = "chatgpt.com"
		req.Header.Set("accept", "text/event-stream")
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		canonical := resolveCodexOutboundIdentity("")
		req.Header.Set("Originator", canonical.originator)
		if customUA := strings.TrimSpace(credentialAccount.GetOpenAIUserAgent()); customUA != "" {
			req.Header.Set("User-Agent", customUA)
		} else {
			req.Header.Set("User-Agent", canonical.userAgent)
		}
		setOpenAIChatGPTAccountHeaders(req.Header, credentialAccount)
		// 与真实转发一致：账号级自定义 UA 同样作为管理员显式配置传入，否则测试用的身份
		// 与该账号真实出站的身份不是同一个（issue #3901 的配对不变式由收口保证）。
		enforceCodexIdentityHeadersWithUA(req.Header, credentialAccount.GetOpenAIUserAgent())
	}

	// 账号级请求头覆写：测试请求与真实转发保持一致的最终头
	credentialAccount.ApplyHeaderOverrides(req.Header)

	// Get proxy URL
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.doOpenAIAccountTestUpstream(req, proxyURL, account, true)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if isOAuth && s.accountRepo != nil {
		if updates, err := extractOpenAICodexProbeUpdates(resp); err == nil && len(updates) > 0 {
			_ = s.accountRepo.UpdateExtra(ctx, account.ID, updates)
			mergeAccountExtra(account, updates)
		}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		body = redactAgentIdentitySensitiveBodyForAccount(ctx, s.accountRepo, credentialAccount, body)
		if !agentIdentityTaskRecoveryWasTried(ctx) && credentialAccount.IsOpenAIAgentIdentity() && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, body) {
			expectedTaskID := credentialAccount.GetCredential("task_id")
			if err := ensureAgentIdentityTaskForAccount(ctx, s.accountRepo, s.agentIdentityWS, &s.agentIdentityTaskMu, credentialAccount, expectedTaskID); err != nil {
				return s.sendErrorAndEnd(c, fmt.Sprintf("Agent Identity task recovery failed: %s", err.Error()))
			}
			c.Request = c.Request.WithContext(markAgentIdentityTaskRecoveryTried(ctx))
			return s.testOpenAIAccountConnection(c, account, modelID, prompt, mode)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			s.reconcileOpenAI429State(ctx, account, resp.Header, body)
		}
		// 401 Unauthorized: 标记账号为永久错误
		if resp.StatusCode == http.StatusUnauthorized && s.accountRepo != nil {
			errMsg := fmt.Sprintf("Authentication failed (401): %s", string(body))
			_ = s.accountRepo.SetError(ctx, account.ID, errMsg)
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(body)))
	}

	// Process SSE stream
	return s.processOpenAIStream(c, resp.Body)
}

// testGrokAccountConnection routes Grok admin connectivity tests by explicit mode first,
// then by selected model family for media. Standalone modes (search/tts/stt) never share
// the text Responses path; image/video never hit Responses either.
//
// Modes:
//   - default/text → Responses (optional model)
//   - image → /v1/images/generations (model optional; defaults to grok-imagine-image)
//   - video → /v1/videos/generations (model optional; defaults to grok-imagine-video)
//   - search → standalone web-search probe (gateway /v1/web_search semantics)
//   - tts → HTTP /v1/tts
//   - stt → HTTP /v1/stt (synthetic tiny wav probe)
//   - realtime → WS /v1/realtime dial + optional first server event
//
// When mode is default, image/video can still be inferred from model_id for backward compat.
func (s *AccountTestService) testGrokAccountConnection(c *gin.Context, account *Account, modelID, prompt, mode string, opts AccountTestOptions) error {
	ctx := c.Request.Context()

	// Realtime is WebSocket-only and does not need HTTP upstream.
	mode = normalizeGrokAccountTestMode(mode)
	if mode != AccountTestModeGrokRealtime && s.httpUpstream == nil {
		return s.sendErrorAndEnd(c, "HTTP upstream not configured")
	}

	authToken, err := s.grokTestAccessToken(ctx, account)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}

	// Explicit standalone / media modes always win over model id.
	switch mode {
	case AccountTestModeGrokSearch:
		return s.testGrokWebSearch(c, ctx, account, authToken, prompt)
	case AccountTestModeGrokTTS:
		return s.testGrokTTS(c, ctx, account, authToken, prompt)
	case AccountTestModeGrokSTT:
		return s.testGrokSTT(c, ctx, account, authToken, opts.AudioDataURL)
	case AccountTestModeGrokRealtime:
		return s.testGrokRealtime(c, ctx, account, authToken, modelID)
	case AccountTestModeGrokImage:
		return s.testGrokImageGeneration(c, ctx, account, authToken, resolveGrokImageTestModel(account, modelID), resolveGrokImagePrompt(prompt), opts.ImageDataURL)
	case AccountTestModeGrokVideo:
		return s.testGrokVideoGeneration(c, ctx, account, authToken, resolveGrokVideoTestModel(account, modelID), resolveGrokVideoPrompt(prompt), opts)
	case AccountTestModeGrokText:
		// Force text Responses even if model_id looks like media.
		testModelID := strings.TrimSpace(modelID)
		if testModelID == "" {
			testModelID = grokDefaultResponsesModel
		}
		if mapped := strings.TrimSpace(account.GetMappedModel(testModelID)); mapped != "" {
			testModelID = mapped
		}
		return s.testGrokResponsesConnection(c, ctx, account, authToken, testModelID)
	}

	// mode == default: infer from model family (legacy UI / API clients).
	testModelID := strings.TrimSpace(modelID)
	if testModelID == "" {
		testModelID = grokDefaultResponsesModel
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(testModelID)); mapped != "" {
		testModelID = mapped
	}

	switch {
	case isGrokImageGenerationModel(testModelID):
		return s.testGrokImageGeneration(c, ctx, account, authToken, testModelID, resolveGrokImagePrompt(prompt), opts.ImageDataURL)
	case isGrokVideoGenerationModel(testModelID):
		return s.testGrokVideoGeneration(c, ctx, account, authToken, testModelID, resolveGrokVideoPrompt(prompt), opts)
	default:
		return s.testGrokResponsesConnection(c, ctx, account, authToken, testModelID)
	}
}

func resolveGrokImagePrompt(prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return defaultGrokImageTestPrompt
	}
	return strings.TrimSpace(prompt)
}

func resolveGrokVideoPrompt(prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return defaultGrokVideoTestPrompt
	}
	return strings.TrimSpace(prompt)
}

func resolveGrokImageTestModel(account *Account, modelID string) string {
	testModelID := strings.TrimSpace(modelID)
	if testModelID == "" {
		testModelID = "grok-imagine-image"
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(testModelID)); mapped != "" {
		return mapped
	}
	return testModelID
}

func resolveGrokVideoTestModel(account *Account, modelID string) string {
	testModelID := strings.TrimSpace(modelID)
	if testModelID == "" {
		testModelID = "grok-imagine-video"
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(testModelID)); mapped != "" {
		return mapped
	}
	return testModelID
}

func (s *AccountTestService) grokTestAccessToken(ctx context.Context, account *Account) (string, error) {
	switch account.Type {
	case AccountTypeOAuth:
		if s.grokTokenProvider == nil {
			return "", fmt.Errorf("grok token provider not configured")
		}
		// Manual tests skip production scheduling eligibility so paused/rate-limited
		// accounts can still be probed by admins (same as Codex/OpenAI tests).
		token, err := s.grokTokenProvider.GetAccessTokenForManualTest(ctx, account)
		if err != nil {
			return "", fmt.Errorf("failed to get grok access token: %s", err.Error())
		}
		return token, nil
	case AccountTypeAPIKey:
		authToken := strings.TrimSpace(account.GetCredential("api_key"))
		if authToken == "" {
			return "", fmt.Errorf("grok api key is missing")
		}
		return authToken, nil
	default:
		return "", fmt.Errorf("unsupported grok account type: %s", account.Type)
	}
}

func (s *AccountTestService) grokTestProxyURL(account *Account) string {
	if account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func (s *AccountTestService) prepareGrokTestSSE(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()
}

func (s *AccountTestService) applyGrokTestRequestHeaders(req *http.Request, account *Account, authToken string, accept string) {
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	// Match gateway media/voice: CLI identity headers only on the CLI chat proxy.
	// api.x.ai media (images/videos) rejects or mistreats OAuth when CLI headers
	// are stamped on the official API host (e.g. ZDR upload_url false positives).
	if account.IsGrokOAuth() && req.URL != nil && isGrokCLIProxyTarget(req.URL.String()) {
		applyGrokCLIHeaders(req.Header)
	}
	account.ApplyHeaderOverrides(req.Header)
}

func (s *AccountTestService) observeGrokTestResponse(ctx context.Context, account *Account, resp *http.Response) {
	if resp == nil {
		return
	}
	now := time.Now()
	// Error bodies carry Grok's free-usage, billing, and content-policy
	// classifications when quota headers are absent. Read only non-success
	// responses here, then restore the body because the caller still needs it
	// for the user-facing test result.
	var responseBody []byte
	if resp.StatusCode >= http.StatusBadRequest && resp.Body != nil {
		responseBody, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(responseBody))
	}
	snapshot := parseGrokQuotaSnapshot(resp.Header, resp.StatusCode, now)
	stampGrokQuotaSnapshotForPlan(account, snapshot, grokRequestedModelFromCtx(ctx))
	if snapshot != nil && s.accountRepo != nil {
		resetAt, limited := grokRateLimitResetAtForAccount(account, snapshot, now)
		if limited {
			normalizeGrokExhaustedWindowResets(snapshot, resetAt, now)
		}
		_ = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
			grokQuotaSnapshotExtraKey: snapshot,
		})
		if limited {
			persistGrokRateLimit(ctx, s.accountRepo, account, resetAt)
		} else if isSuccessfulGrokRateLimitRecovery(account, snapshot) {
			clearGrokRateLimitAfterRecovery(ctx, s.accountRepo, account)
		}
	} else if s.accountRepo != nil && isSuccessfulGrokRateLimitRecovery(account, &xai.QuotaSnapshot{StatusCode: resp.StatusCode}) {
		clearGrokRateLimitAfterRecovery(ctx, s.accountRepo, account)
	}
	if s.accountRepo == nil || len(responseBody) == 0 {
		if resp.StatusCode == http.StatusPaymentRequired && s.accountRepo != nil {
			stateCtx, cancel := openAIAccountStateContext(ctx)
			defer cancel()
			_ = s.accountRepo.SetTempUnschedulable(stateCtx, account.ID, now.Add(30*time.Minute), "grok payment required")
		}
		return
	}
	if isGrokContentPolicyRejection(resp.StatusCode, responseBody) {
		return
	}
	decision := classifyGrokUpstreamFailure(resp.StatusCode, responseBody, "")
	if decision.Class == GrokFailureFreeUsage {
		if resetAt, limited := grokRateLimitResetAtForAccount(account, snapshot, now); limited && resetAt.After(now) {
			persistGrokRateLimit(ctx, s.accountRepo, account, resetAt)
		} else {
			stateCtx, cancel := openAIAccountStateContext(ctx)
			_ = s.accountRepo.SetTempUnschedulable(stateCtx, account.ID, now.Add(grokFreeUsageProbeCooldown), "grok free usage exhausted")
			cancel()
		}
		return
	}
	if decision.Class == GrokFailureBilling && (isGrokSpendingLimitError(responseBody) || strings.Contains(strings.ToLower(decision.Reason), "credit")) {
		persistGrokRateLimit(ctx, s.accountRepo, account, grokSpendingLimitResetAt(account, now))
		return
	}
	cooldown := time.Duration(0)
	reason := ""
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		cooldown, reason = 10*time.Minute, "grok oauth token unauthorized"
	case http.StatusPaymentRequired:
		cooldown, reason = 30*time.Minute, "grok payment required"
	case http.StatusForbidden:
		cooldown, reason = 30*time.Minute, "grok entitlement or subscription tier denied"
	default:
		if resp.StatusCode >= 500 {
			cooldown, reason = 2*time.Minute, "grok upstream temporary error"
		}
	}
	if decision.Class == GrokFailureBilling && cooldown == 0 {
		cooldown, reason = 30*time.Minute, "grok payment required"
	}
	if cooldown > 0 {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		until := now.Add(cooldown)
		if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(until) {
			until = *account.TempUnschedulableUntil
		}
		_ = s.accountRepo.SetTempUnschedulable(
			stateCtx,
			account.ID,
			until,
			reason,
		)
	}
}

func (s *AccountTestService) testGrokResponsesConnection(c *gin.Context, ctx context.Context, account *Account, authToken, testModelID string) error {
	apiURL, err := buildGrokResponsesURL(account, s.cfg, s.settingService)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok base URL: %s", err.Error()))
	}

	s.prepareGrokTestSSE(c)

	payloadBytes, err := buildGrokQuotaProbeBody(testModelID)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok test payload")
	}

	if !agentIdentityTaskRecoveryWasTried(ctx) {
		s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok request")
	}
	s.applyGrokTestRequestHeaders(req, account, authToken, "application/json, text/event-stream")

	resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok Responses API request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	s.observeGrokTestResponse(withGrokTeamRateLimitModel(ctx, testModelID), account, resp)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok Responses API returned %d: %s", resp.StatusCode, string(body)))
	}

	return s.processOpenAIStream(c, resp.Body)
}

func (s *AccountTestService) testGrokImageGeneration(c *gin.Context, ctx context.Context, account *Account, authToken, modelID, prompt, imageDataURL string) error {
	// With a source image, prefer /images/edits; otherwise /images/generations.
	endpoint := GrokMediaEndpointImagesGenerations
	imageDataURL = strings.TrimSpace(imageDataURL)
	hasSourceImage := imageDataURL != ""
	if hasSourceImage {
		endpoint = GrokMediaEndpointImagesEdits
	}

	// Align model aliases with gateway (e.g. grok-imagine → grok-imagine-image-quality).
	modelID = NormalizeGrokMediaModelForEndpoint(endpoint, modelID, hasSourceImage)
	if modelID == "" {
		modelID = "grok-imagine-image-quality"
	}

	apiURL, err := buildGrokMediaURL(account, s.cfg, endpoint, "")
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok media base URL: %s", err.Error()))
	}

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: modelID})
	if endpoint == GrokMediaEndpointImagesEdits {
		s.sendEvent(c, TestEvent{Type: "status", Text: "Calling Grok /v1/images/edits with uploaded source image..."})
	} else {
		s.sendEvent(c, TestEvent{Type: "status", Text: "Calling Grok /v1/images/generations..."})
	}

	// Zero-data-retention teams reject URL format; always request base64 for admin tests.
	payload := map[string]any{
		"model":           modelID,
		"prompt":          prompt,
		"n":               1,
		"response_format": "b64_json",
	}
	if hasSourceImage {
		normalized, err := normalizeAccountTestImageDataURL(imageDataURL)
		if err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
		// Match gateway prepareGrokMediaForwardBody shape: {url, type:image_url}.
		payload["image"] = grokMediaImageObject(normalized)
		s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("source image ready (%d chars data URL)\n", len(normalized))})
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to marshal Grok image request")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok image request")
	}
	s.applyGrokTestRequestHeaders(req, account, authToken, "application/json")
	req.ContentLength = int64(len(payloadBytes))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payloadBytes)), nil
	}

	// One retry on transport EOF (proxies occasionally drop large edit payloads).
	var resp *http.Response
	var doErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			s.sendEvent(c, TestEvent{Type: "status", Text: "Retrying Grok image request after transport error..."})
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
			if err != nil {
				return s.sendErrorAndEnd(c, "Failed to create Grok image retry request")
			}
			s.applyGrokTestRequestHeaders(req, account, authToken, "application/json")
			req.ContentLength = int64(len(payloadBytes))
		}
		resp, doErr = s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
		if doErr == nil {
			break
		}
		if !isTransientGrokTransportError(doErr) || attempt == 1 {
			return s.sendErrorAndEnd(c, formatGrokImageTransportError(doErr, hasSourceImage, len(payloadBytes)))
		}
	}
	defer func() { _ = resp.Body.Close() }()
	s.observeGrokTestResponse(ctx, account, resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to read Grok image response: %s", err.Error()))
	}
	if resp.StatusCode != http.StatusOK {
		return s.sendErrorAndEnd(c, formatGrokImagesAPIError(resp.StatusCode, body, hasSourceImage))
	}

	var result struct {
		Data []struct {
			URL           string `json:"url"`
			B64JSON       string `json:"b64_json"`
			RevisedPrompt string `json:"revised_prompt"`
			MimeType      string `json:"mime_type"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to parse Grok image response: %s", err.Error()))
	}
	if len(result.Data) == 0 {
		return s.sendErrorAndEnd(c, "No images returned from Grok API")
	}

	for _, item := range result.Data {
		if item.RevisedPrompt != "" {
			s.sendEvent(c, TestEvent{Type: "content", Text: item.RevisedPrompt})
		}
		mimeType := strings.TrimSpace(item.MimeType)
		if mimeType == "" {
			mimeType = "image/jpeg"
		}
		switch {
		case strings.TrimSpace(item.B64JSON) != "":
			s.sendEvent(c, TestEvent{
				Type:     "image",
				ImageURL: "data:" + mimeType + ";base64," + item.B64JSON,
				MimeType: mimeType,
			})
		case strings.TrimSpace(item.URL) != "":
			s.sendEvent(c, TestEvent{Type: "image", ImageURL: item.URL, MimeType: mimeType})
		}
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) testGrokVideoGeneration(c *gin.Context, ctx context.Context, account *Account, authToken, modelID, prompt string, opts AccountTestOptions) error {
	apiURL, err := buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideosGenerations, "")
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok media base URL: %s", err.Error()))
	}

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: modelID})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Calling Grok /v1/videos/generations..."})

	payload := map[string]any{
		"model":        modelID,
		"prompt":       prompt,
		"duration":     6,
		"aspect_ratio": "16:9",
		"resolution":   "480p",
	}
	if img := strings.TrimSpace(opts.ImageDataURL); img != "" {
		normalized, err := normalizeAccountTestImageDataURL(img)
		if err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
		// First-frame / image-to-video input (xAI image field).
		payload["image"] = grokMediaImageObject(normalized)
		s.sendEvent(c, TestEvent{Type: "content", Text: "using uploaded first-frame / reference image\n"})
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok video request")
	}
	s.applyGrokTestRequestHeaders(req, account, authToken, "application/json")

	resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	s.observeGrokTestResponse(ctx, account, resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to read Grok video response: %s", err.Error()))
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok videos API returned %d: %s", resp.StatusCode, string(body)))
	}

	requestID := strings.TrimSpace(gjson.GetBytes(body, "request_id").String())
	if requestID == "" {
		requestID = strings.TrimSpace(gjson.GetBytes(body, "id").String())
	}
	if requestID == "" {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video create response missing request_id: %s", string(body)))
	}

	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("video request accepted: %s\n", requestID)})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Polling video status until done (max ~60s)..."})

	statusURL, err := buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideoStatus, requestID)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok video status URL: %s", err.Error()))
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return s.sendErrorAndEnd(c, "Grok video poll canceled")
		}
		statusReq, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to create Grok video status request")
		}
		s.applyGrokTestRequestHeaders(statusReq, account, authToken, "application/json")
		statusResp, err := s.httpUpstream.Do(statusReq, s.grokTestProxyURL(account), account.ID, account.Concurrency)
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video status failed: %s", err.Error()))
		}
		statusBody, _ := io.ReadAll(statusResp.Body)
		_ = statusResp.Body.Close()
		if statusResp.StatusCode != http.StatusOK && statusResp.StatusCode != http.StatusAccepted {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video status returned %d: %s", statusResp.StatusCode, string(statusBody)))
		}
		st := strings.ToLower(strings.TrimSpace(gjson.GetBytes(statusBody, "status").String()))
		progress := gjson.GetBytes(statusBody, "progress")
		if progress.Exists() {
			s.sendEvent(c, TestEvent{Type: "status", Text: fmt.Sprintf("status=%s progress=%v", st, progress.Value())})
		} else {
			s.sendEvent(c, TestEvent{Type: "status", Text: "status=" + st})
		}
		switch st {
		case "done", "completed", "succeeded", "success":
			return s.emitGrokVideoResult(c, ctx, account, authToken, requestID, statusBody)
		case "failed", "error", "canceled", "cancelled":
			return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video failed: %s", string(statusBody)))
		}
		select {
		case <-ctx.Done():
			return s.sendErrorAndEnd(c, "Grok video poll canceled")
		case <-time.After(3 * time.Second):
		}
	}
	return s.sendErrorAndEnd(c, "Grok video still processing after 60s (request_id="+requestID+")")
}

// emitGrokVideoResult surfaces a playable video URL or downloads /content as data URL.
func (s *AccountTestService) emitGrokVideoResult(c *gin.Context, ctx context.Context, account *Account, authToken, requestID string, statusBody []byte) error {
	videoURL := firstNonEmpty(
		strings.TrimSpace(gjson.GetBytes(statusBody, "video.url").String()),
		strings.TrimSpace(gjson.GetBytes(statusBody, "url").String()),
		strings.TrimSpace(gjson.GetBytes(statusBody, "video_url").String()),
		strings.TrimSpace(gjson.GetBytes(statusBody, "download_url").String()),
	)
	if videoURL != "" && (strings.HasPrefix(videoURL, "http://") || strings.HasPrefix(videoURL, "https://") || strings.HasPrefix(videoURL, "data:")) {
		s.sendEvent(c, TestEvent{Type: "content", Text: "video ready: " + videoURL + "\n"})
		s.sendEvent(c, TestEvent{Type: "video", VideoURL: videoURL, MimeType: "video/mp4"})
		s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
		return nil
	}

	// Fetch binary content via official /videos/{id}/content (Bearer-authenticated).
	contentURL, err := buildGrokMediaURL(account, s.cfg, GrokMediaEndpointVideoContent, requestID)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok video content URL: %s", err.Error()))
	}
	s.sendEvent(c, TestEvent{Type: "status", Text: "Downloading video content for preview..."})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, contentURL, nil)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok video content request")
	}
	s.applyGrokTestRequestHeaders(req, account, authToken, "video/*, application/octet-stream, */*")
	resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video content download failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20)) // 64 MiB cap for admin preview
	if resp.StatusCode != http.StatusOK {
		// Fall back to status URL when binary content is unavailable.
		if videoURL != "" {
			s.sendEvent(c, TestEvent{Type: "content", Text: "video completed; content download unavailable, reported url=" + videoURL + "\n"})
			s.sendEvent(c, TestEvent{Type: "video", VideoURL: videoURL, MimeType: "video/mp4"})
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok video content returned %d: %s", resp.StatusCode, truncateString(string(body), 300)))
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || strings.HasPrefix(ct, "application/octet-stream") {
		ct = "video/mp4"
	}
	// Keep only type/subtype for data URL.
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	dataURL := "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(body)
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("video content downloaded: content-type=%s bytes=%d\n", ct, len(body))})
	s.sendEvent(c, TestEvent{Type: "video", VideoURL: dataURL, MimeType: ct})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) testGrokWebSearch(c *gin.Context, ctx context.Context, account *Account, authToken, query string) error {
	query = strings.TrimSpace(query)
	if query == "" {
		query = defaultGrokSearchTestQuery
	}

	// Account-test "web_search" mode mirrors the standalone gateway endpoint
	// POST /v1/web_search (not a free-form chat with tools). Implementation still
	// uses the same DoGrokNativeResponsesJSON helper as the gateway handler so
	// results match production search.
	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: "grok-web-search"})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Calling standalone web_search probe (same as gateway /v1/web_search)..."})

	// Keep parity with handler.buildGrokWebSearchPrompt / include sources.
	const maxResults = 5
	prompt := fmt.Sprintf(
		`Search the web for the user query below. Return ONLY valid JSON with this exact shape: {"results":[{"url":"https://...","title":"page title","snippet":"concise factual summary"}]}. Return at most %d unique results. Every URL must be an actual web_search source. Populate a non-empty title and snippet for every result. Do not wrap the JSON in markdown.

User query:
%s`, maxResults, query)
	payload := map[string]any{
		"model":   grokDefaultResponsesModel,
		"input":   prompt,
		"tools":   []map[string]any{{"type": "web_search"}},
		"include": []string{"web_search_call.action.sources"},
		"store":   false,
		"stream":  false,
	}
	payloadBytes, _ := json.Marshal(payload)

	apiURL, err := buildGrokResponsesURL(account, s.cfg, s.settingService)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok base URL: %s", err.Error()))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create standalone web_search probe request")
	}
	s.applyGrokTestRequestHeaders(req, account, authToken, "application/json")

	resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("standalone web_search probe failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	s.observeGrokTestResponse(withGrokTeamRateLimitModel(ctx, grokDefaultResponsesModel), account, resp)

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return s.sendErrorAndEnd(c, fmt.Sprintf("standalone web_search probe returned %d: %s", resp.StatusCode, string(body)))
	}

	// Normalize like gateway extractGrokWebSearchSources (URL-only sources are enough for connectivity).
	sourceCount := 0
	gjson.GetBytes(body, "output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "web_search_call" {
			return true
		}
		sources := item.Get("action.sources")
		if sources.IsArray() {
			sourceCount += len(sources.Array())
		}
		return true
	})
	searchCount := countGrokNativeSearchCallsFromJSONBytes(body)
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("web_search ok: query=%q tool_calls=%d sources=%d\n", query, searchCount, sourceCount)})
	// Optional: first structured result title if model returned JSON text.
	gjson.GetBytes(body, "output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "message" {
			return true
		}
		for _, part := range item.Get("content").Array() {
			text := strings.TrimSpace(part.Get("text").String())
			if text == "" {
				continue
			}
			if len(text) > 300 {
				text = text[:300] + "..."
			}
			s.sendEvent(c, TestEvent{Type: "content", Text: text + "\n"})
			return false
		}
		return true
	})
	if searchCount == 0 && sourceCount == 0 {
		return s.sendErrorAndEnd(c, "standalone web_search probe completed but no search sources/tool calls were observed")
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) testGrokTTS(c *gin.Context, ctx context.Context, account *Account, authToken, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		text = defaultGrokTTSTestText
	}
	apiURL, err := buildGrokVoiceURL(account, s.cfg, "tts")
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok TTS URL: %s", err.Error()))
	}

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: "grok-voice-tts"})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Calling standalone /v1/tts..."})

	// xAI requires `language`; optional voice_id. Prefer the shape that matches
	// live gateway probes (text + language [+ voice_id]).
	payloads := []map[string]any{
		{"text": text, "language": "en", "voice_id": "Ara"},
		{"text": text, "language": "en"},
		{"text": text, "language": "English", "voice_id": "Ara"},
	}
	var lastBody string
	var lastCode int
	for _, payload := range payloads {
		payloadBytes, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to create Grok TTS request")
		}
		s.applyGrokTestRequestHeaders(req, account, authToken, "audio/*, application/json, */*")
		resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Grok TTS failed: %s", err.Error()))
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		s.observeGrokTestResponse(ctx, account, resp)
		lastCode = resp.StatusCode
		lastBody = string(body)
		if resp.StatusCode == http.StatusOK {
			ct := resp.Header.Get("Content-Type")
			if ct == "" {
				ct = "audio/mpeg"
			}
			if i := strings.Index(ct, ";"); i >= 0 {
				ct = strings.TrimSpace(ct[:i])
			}
			// Cap preview size so SSE stays manageable (~4 MiB audio).
			if len(body) > 4<<20 {
				body = body[:4<<20]
			}
			audioURL := "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(body)
			s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("tts ok: content-type=%s bytes=%d\n", ct, len(body))})
			s.sendEvent(c, TestEvent{Type: "audio", AudioURL: audioURL, MimeType: ct})
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		}
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			break
		}
	}
	return s.sendErrorAndEnd(c, fmt.Sprintf("Grok TTS returned %d: %s", lastCode, lastBody))
}

// testGrokSTT posts audio to /v1/stt. When audioDataURL is set, uses the
// uploaded file; otherwise a tiny synthetic silent WAV for connectivity only.
func (s *AccountTestService) testGrokSTT(c *gin.Context, ctx context.Context, account *Account, authToken, audioDataURL string) error {
	apiURL, err := buildGrokVoiceURL(account, s.cfg, "stt")
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok STT URL: %s", err.Error()))
	}

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: "grok-voice-stt"})

	var audioBytes []byte
	filename := "probe.wav"
	if audioDataURL = strings.TrimSpace(audioDataURL); audioDataURL != "" {
		if err := validateAccountTestDataURL(audioDataURL, "audio/"); err != nil {
			return s.sendErrorAndEnd(c, err.Error())
		}
		raw, mime, err := decodeAccountTestDataURL(audioDataURL)
		if err != nil {
			return s.sendErrorAndEnd(c, "Invalid audio data URL: "+err.Error())
		}
		audioBytes = raw
		filename = sttFilenameForMIME(mime)
		s.sendEvent(c, TestEvent{Type: "status", Text: "Calling standalone /v1/stt with uploaded audio..."})
		s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("uploaded audio: mime=%s bytes=%d\n", mime, len(audioBytes))})
	} else {
		audioBytes = minimalSilentWAV()
		s.sendEvent(c, TestEvent{Type: "status", Text: "Calling standalone /v1/stt with a synthetic silent WAV..."})
	}

	var bodyBuf bytes.Buffer
	w := multipart.NewWriter(&bodyBuf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to build STT multipart body")
	}
	if _, err := part.Write(audioBytes); err != nil {
		return s.sendErrorAndEnd(c, "Failed to write STT audio part")
	}
	_ = w.WriteField("model", "grok-stt")
	_ = w.WriteField("language", "en")
	if err := w.Close(); err != nil {
		return s.sendErrorAndEnd(c, "Failed to finalize STT multipart body")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, &bodyBuf)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok STT request")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+authToken)
	if account.IsGrokOAuth() {
		applyGrokCLIHeaders(req.Header)
	}
	account.ApplyHeaderOverrides(req.Header)

	resp, err := s.httpUpstream.Do(req, s.grokTestProxyURL(account), account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok STT failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	s.observeGrokTestResponse(ctx, account, resp)
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// 4xx on synthetic audio still proves the STT endpoint is wired; report clearly.
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok STT returned %d: %s", resp.StatusCode, string(respBody)))
	}
	text := strings.TrimSpace(gjson.GetBytes(respBody, "text").String())
	if text == "" {
		text = strings.TrimSpace(string(respBody))
		if len(text) > 200 {
			text = text[:200] + "..."
		}
	}
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("stt ok: %s\n", text)})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// testGrokRealtime dials the standalone xAI Voice Realtime WebSocket
// (wss://api.x.ai/v1/realtime?model=...) to verify auth + endpoint reachability.
// It does not run a full audio session — success is WS handshake, optionally
// enriched with the first server event type when one arrives quickly.
func (s *AccountTestService) testGrokRealtime(c *gin.Context, ctx context.Context, account *Account, authToken, modelID string) error {
	model := strings.TrimSpace(modelID)
	if model == "" {
		model = defaultGrokRealtimeTestModel
	}
	if mapped := strings.TrimSpace(account.GetMappedModel(model)); mapped != "" {
		model = mapped
	}

	base, err := buildGrokVoiceURL(account, s.cfg, "realtime")
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok Realtime URL: %s", err.Error()))
	}
	u, err := url.Parse(base)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid Grok Realtime URL: %s", err.Error()))
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
		// already websocket
	default:
		return s.sendErrorAndEnd(c, "Invalid Grok Realtime URL scheme")
	}
	q := u.Query()
	if q.Get("model") == "" {
		q.Set("model", model)
	}
	u.RawQuery = q.Encode()
	wsURL := u.String()

	s.prepareGrokTestSSE(c)
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	s.sendEvent(c, TestEvent{Type: "status", Text: "Dialing standalone wss /v1/realtime (connectivity probe)..."})
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("realtime target: %s\n", redactGrokRealtimeURLForLog(wsURL))})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+authToken)
	if account.IsGrokOAuth() {
		applyGrokCLIHeaders(headers)
	}
	account.ApplyHeaderOverrides(headers)

	dialer := s.grokWSDialer
	if dialer == nil {
		dialer = newDefaultOpenAIWSClientDialer()
	}

	dialCtx, cancel := context.WithTimeout(ctx, grokRealtimeProbeTimeout)
	defer cancel()

	conn, status, _, dialErr := dialer.Dial(dialCtx, wsURL, headers, s.grokTestProxyURL(account))
	if dialErr != nil {
		detail := dialErr.Error()
		var hs *openAIWSHandshakeError
		if errors.As(dialErr, &hs) && len(hs.Body) > 0 {
			body := strings.TrimSpace(string(hs.Body))
			if len(body) > 300 {
				body = body[:300] + "..."
			}
			detail = fmt.Sprintf("%s body=%s", detail, body)
		}
		if status > 0 {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Grok Realtime WS handshake failed (HTTP %d): %s", status, detail))
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok Realtime WS dial failed: %s", detail))
	}
	defer func() { _ = conn.Close() }()

	s.sendEvent(c, TestEvent{Type: "content", Text: "realtime ws handshake ok\n"})

	// Best-effort: read one server event if it arrives quickly (session.created etc.).
	// Handshake alone is enough for connectivity; missing first event is not a failure.
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	if msg, readErr := conn.ReadMessage(readCtx); readErr == nil && len(msg) > 0 {
		eventType := strings.TrimSpace(gjson.GetBytes(msg, "type").String())
		if eventType == "" {
			eventType = "unknown"
		}
		preview := strings.TrimSpace(string(msg))
		if len(preview) > 240 {
			preview = preview[:240] + "..."
		}
		s.sendEvent(c, TestEvent{
			Type: "content",
			Text: fmt.Sprintf("realtime first event: type=%s payload=%s\n", eventType, preview),
		})
	} else {
		s.sendEvent(c, TestEvent{
			Type: "content",
			Text: "realtime handshake succeeded (no server event within 3s; still connectivity OK)\n",
		})
	}

	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// validateAccountTestDataURL ensures data URLs are well-formed and size-bounded.
func validateAccountTestDataURL(raw, requiredPrefix string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("media data URL is empty")
	}
	if !strings.HasPrefix(raw, "data:") {
		return fmt.Errorf("media must be a data: URL (data:<mime>;base64,...)")
	}
	// Rough size check before decode (base64 expands ~4/3).
	if len(raw) > maxAccountTestMediaBytes*2 {
		return fmt.Errorf("media data URL exceeds size limit")
	}
	_, mime, err := decodeAccountTestDataURL(raw)
	if err != nil {
		return err
	}
	if requiredPrefix != "" && !strings.HasPrefix(strings.ToLower(mime), strings.ToLower(requiredPrefix)) {
		return fmt.Errorf("expected media type prefix %q, got %q", requiredPrefix, mime)
	}
	return nil
}

// normalizeAccountTestImageDataURL validates an image data URL, enforces xAI
// minimum dimensions (8x8), and rewrites to a clean data:image/<type>;base64,... form.
func normalizeAccountTestImageDataURL(raw string) (string, error) {
	if err := validateAccountTestDataURL(raw, "image/"); err != nil {
		return "", err
	}
	data, mime, err := decodeAccountTestDataURL(raw)
	if err != nil {
		return "", err
	}
	// Soft cap decoded bytes (~4 MiB) for edit payloads to avoid upstream/proxy EOF.
	const maxDecodedImage = 4 << 20
	if len(data) > maxDecodedImage {
		return "", fmt.Errorf(
			"source image is too large (%d bytes decoded). Please use a smaller image (under ~4 MB) for admin edit tests",
			len(data),
		)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// Keep raw data URL if decoder does not understand the codec (e.g. webp
		// without golang.org/x/image/webp); still send upstream and let xAI validate.
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	}
	if cfg.Width < 8 || cfg.Height < 8 {
		return "", fmt.Errorf(
			"source image is too small (%dx%d). xAI requires both width and height to be at least 8 pixels",
			cfg.Width, cfg.Height,
		)
	}
	// Prefer a stable mime from config when known.
	if mime == "" || mime == "application/octet-stream" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func isTransientGrokTransportError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "timeout awaiting response")
}

func formatGrokImageTransportError(err error, hasSourceImage bool, payloadBytes int) string {
	base := fmt.Sprintf("Grok image request failed: %s", err.Error())
	if !hasSourceImage {
		return base
	}
	return base + fmt.Sprintf(
		" (edit payload ~%d bytes). Tips: use a smaller source image (<4 MB / lower resolution), ensure the account proxy is stable, and retry. xAI /images/edits expects image as {\"url\":\"data:image/...;base64,...\",\"type\":\"image_url\"}.",
		payloadBytes,
	)
}

func formatGrokImagesAPIError(status int, body []byte, hasSourceImage bool) string {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 800 {
		msg = msg[:800] + "..."
	}
	prefix := fmt.Sprintf("Grok images API returned %d: %s", status, msg)
	lower := strings.ToLower(msg)
	if hasSourceImage && (strings.Contains(lower, "too small") || strings.Contains(lower, "at least 8")) {
		return prefix + " — upload a source image with both width and height ≥ 8 px."
	}
	return prefix
}

func decodeAccountTestDataURL(raw string) (data []byte, mime string, err error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "data:") {
		return nil, "", fmt.Errorf("not a data URL")
	}
	rest := strings.TrimPrefix(raw, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return nil, "", fmt.Errorf("invalid data URL (missing comma)")
	}
	meta := rest[:comma]
	payload := rest[comma+1:]
	mime = "application/octet-stream"
	if semi := strings.Index(meta, ";"); semi >= 0 {
		if t := strings.TrimSpace(meta[:semi]); t != "" {
			mime = t
		}
	} else if t := strings.TrimSpace(meta); t != "" {
		mime = t
	}
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return nil, "", fmt.Errorf("only base64 data URLs are supported")
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		// Some browsers emit URL-safe base64 without padding.
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(payload, "="))
		if err != nil {
			return nil, "", fmt.Errorf("base64 decode failed: %w", err)
		}
	}
	if len(decoded) == 0 {
		return nil, "", fmt.Errorf("decoded media is empty")
	}
	if len(decoded) > maxAccountTestMediaBytes {
		return nil, "", fmt.Errorf("media exceeds %d byte limit", maxAccountTestMediaBytes)
	}
	return decoded, mime, nil
}

func sttFilenameForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "audio/mpeg", "audio/mp3":
		return "upload.mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "upload.wav"
	case "audio/webm":
		return "upload.webm"
	case "audio/ogg", "audio/opus":
		return "upload.ogg"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return "upload.m4a"
	default:
		return "upload.bin"
	}
}

// redactGrokRealtimeURLForLog strips query secrets while keeping model for diagnostics.
func redactGrokRealtimeURLForLog(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return raw
	}
	// Keep model query only.
	model := u.Query().Get("model")
	u.RawQuery = ""
	if model != "" {
		u.RawQuery = "model=" + url.QueryEscape(model)
	}
	// Never log bearer in fragment/userinfo.
	u.User = nil
	u.Fragment = ""
	return u.String()
}

// minimalSilentWAV returns a valid tiny mono 8kHz 16-bit PCM WAV (~0.05s silence).
func minimalSilentWAV() []byte {
	// 400 samples * 2 bytes = 800 data bytes
	const sampleRate = 8000
	const numSamples = 400
	dataSize := numSamples * 2
	buf := make([]byte, 44+dataSize)
	copy(buf[0:], []byte("RIFF"))
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataSize))
	copy(buf[8:], []byte("WAVE"))
	copy(buf[12:], []byte("fmt "))
	binary.LittleEndian.PutUint32(buf[16:], 16) // PCM chunk size
	binary.LittleEndian.PutUint16(buf[20:], 1)  // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1)  // mono
	binary.LittleEndian.PutUint32(buf[24:], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:], sampleRate*2) // byte rate
	binary.LittleEndian.PutUint16(buf[32:], 2)            // block align
	binary.LittleEndian.PutUint16(buf[34:], 16)           // bits
	copy(buf[36:], []byte("data"))
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataSize))
	// samples already zero (silence)
	return buf
}

// testOpenAIChatCompletionsConnection tests an OpenAI-compatible APIKey account
// through the raw /v1/chat/completions endpoint.
func (s *AccountTestService) testOpenAIChatCompletionsConnection(
	c *gin.Context,
	account *Account,
	testModelID string,
	prompt string,
	normalizedBaseURL string,
	authToken string,
) error {
	ctx := c.Request.Context()
	apiURL := openAIChatCompletionsURLForBase(normalizedBaseURL)

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	payload := createOpenAIChatCompletionsTestPayload(testModelID, prompt)
	payloadBytes, _ := json.Marshal(payload)

	endpointLabel := "/v1/chat/completions"
	if _, ok := parseVolcanoPlanProfile(normalizedBaseURL); ok {
		endpointLabel = "/v3/chat/completions"
	}
	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})
	s.sendEvent(c, TestEvent{Type: "status", Text: "正在通过 " + endpointLabel + " 测试连接"})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Chat Completions request")
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+authToken)

	// 账号级请求头覆写：测试请求与真实转发保持一致的最终头
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Chat Completions API (/v1/chat/completions) request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusTooManyRequests {
			s.reconcileOpenAI429State(ctx, account, resp.Header, body)
		}
		if resp.StatusCode == http.StatusUnauthorized && s.accountRepo != nil {
			errMsg := fmt.Sprintf("Chat Completions authentication failed (401): %s", string(body))
			_ = s.accountRepo.SetError(ctx, account.ID, errMsg)
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("Chat Completions API (/v1/chat/completions) returned %d: %s", resp.StatusCode, string(body)))
	}

	return s.processOpenAIChatCompletionsStream(c, resp.Body)
}

// testOpenAICompactConnection probes native remote compaction v2 (streaming
// /responses with a compaction_trigger input item) and persists the resulting
// capability state on the account. The legacy unary /responses/compact
// endpoint has been sunset upstream (404, #5598/#5624) and is no longer probed.
func (s *AccountTestService) testOpenAICompactConnection(c *gin.Context, account *Account, testModelID string) error {
	ctx := c.Request.Context()
	credentialAccount := account
	if account.IsShadow() {
		resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to resolve account credentials")
		}
		credentialAccount = resolved
	}

	authToken := ""
	apiURL := ""
	isOAuth := false

	switch {
	case credentialAccount.IsOAuth():
		isOAuth = true
		if !credentialAccount.IsOpenAIAgentIdentity() {
			authToken = credentialAccount.GetOpenAIAccessToken()
		}
		if authToken == "" && !credentialAccount.IsOpenAIAgentIdentity() {
			return s.sendErrorAndEnd(c, "No access token available")
		}
		apiURL = chatgptCodexAPIURL
	case account.Type == AccountTypeAPIKey:
		authToken = account.GetOpenAIProtocolAPIKey()
		if authToken == "" {
			return s.sendErrorAndEnd(c, "No API key available")
		}
		baseURL := account.GetOpenAIBaseURL()
		if baseURL == "" {
			baseURL = "https://api.openai.com"
		}
		normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid base URL: %s", err.Error()))
		}
		apiURL = openAIResponsesURLForBase(account.Platform, normalizedBaseURL)
	default:
		return s.sendErrorAndEnd(c, fmt.Sprintf("Unsupported account type: %s", account.Type))
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// 原生 v2 走普通 /responses 线：OAuth 与真实转发一致做上游模型归一化。
	if isOAuth {
		testModelID = normalizeOpenAIModelForUpstream(credentialAccount, testModelID)
	}
	payloadBytes, _ := json.Marshal(createOpenAICompactProbePayload(testModelID, isOAuth))
	if !agentIdentityTaskRecoveryWasTried(ctx) {
		s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create request")
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	req.Header.Set("Content-Type", "application/json")
	// v2 探测是流式请求；同时补注协商头，与真实 codex 出站线型一致。
	req.Header.Set("Accept", "text/event-stream")
	ensureOpenAIRemoteCompactionV2BetaFeature(req.Header)
	if credentialAccount.IsOpenAIAgentIdentity() {
		authHeaders, authErr := buildAgentIdentityAuthenticationHeaders(ctx, s.accountRepo, s.agentIdentityWS, &s.agentIdentityTaskMu, credentialAccount)
		if authErr != nil {
			return s.sendErrorAndEnd(c, "Failed to build Agent Identity authentication")
		}
		for key, values := range authHeaders {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	applyOpenAICodexProbeHeaders(req.Header)
	if isOAuth {
		enforceCodexIdentityHeadersWithUA(req.Header, credentialAccount.GetOpenAIUserAgent())
	}
	probeSessionID := compactProbeSessionID(account.ID)
	req.Header.Set("Session_ID", probeSessionID)
	req.Header.Set("Conversation_ID", probeSessionID)

	if isOAuth {
		req.Host = "chatgpt.com"
		setOpenAIChatGPTAccountHeaders(req.Header, credentialAccount)
		// 指纹收敛：探测与真实转发走同一个 /responses 端点，身份也必须同构，
		// 否则探测流量会以「缺 x-codex-installation-id + 非收敛 session」的
		// 形态暴露在上游眼里。账号关闭收敛（off）时返回 nil，探测保持原样。
		if fpIDs := resolveCodexFingerprintIDsFromRequest(account, req.Header); fpIDs != nil {
			applyCodexFingerprintHeaders(req.Header, fpIDs)
		}
	}

	// 账号级请求头覆写：测试请求与真实转发保持一致的最终头
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.doOpenAIAccountTestUpstream(req, proxyURL, account, true)
	if err != nil {
		if s.accountRepo != nil {
			updates := buildOpenAICompactProbeExtraUpdates(nil, nil, err, false, time.Now())
			_ = s.accountRepo.UpdateExtra(ctx, account.ID, updates)
			mergeAccountExtra(account, updates)
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("Request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	body = redactAgentIdentitySensitiveBodyForAccount(ctx, s.accountRepo, credentialAccount, body)
	if !agentIdentityTaskRecoveryWasTried(ctx) && credentialAccount.IsOpenAIAgentIdentity() && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, body) {
		expectedTaskID := credentialAccount.GetCredential("task_id")
		if err := ensureAgentIdentityTaskForAccount(ctx, s.accountRepo, s.agentIdentityWS, &s.agentIdentityTaskMu, credentialAccount, expectedTaskID); err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Agent Identity task recovery failed: %s", err.Error()))
		}
		c.Request = c.Request.WithContext(markAgentIdentityTaskRecoveryTried(ctx))
		return s.testOpenAICompactConnection(c, account, testModelID)
	}

	compactionFound := openAICompactProbeFoundCompactionItem(body)
	if s.accountRepo != nil {
		updates := buildOpenAICompactProbeExtraUpdates(resp, body, nil, compactionFound, time.Now())
		if codexUpdates, err := extractOpenAICodexProbeUpdates(resp); err == nil && len(codexUpdates) > 0 {
			updates = mergeExtraUpdates(updates, codexUpdates)
		}
		if len(updates) > 0 {
			_ = s.accountRepo.UpdateExtra(ctx, account.ID, updates)
			mergeAccountExtra(account, updates)
		}
		// 探测如返回 429,主动同步限流状态,避免后续短时间内继续选中。
		if resp.StatusCode == http.StatusTooManyRequests {
			s.reconcileOpenAI429State(ctx, account, resp.Header, body)
		}
	}

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized && s.accountRepo != nil {
			errMsg := fmt.Sprintf("Authentication failed (401): %s", string(body))
			_ = s.accountRepo.SetError(ctx, account.ID, errMsg)
		}
		return s.sendErrorAndEnd(c, fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(body)))
	}

	if !compactionFound {
		return s.sendErrorAndEnd(c, "Upstream returned 2xx without a compaction output item (native remote compaction v2 unsupported on this chain)")
	}

	s.sendEvent(c, TestEvent{Type: "content", Text: "Compact probe succeeded (native remote compaction v2)"})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) reconcileOpenAI429State(ctx context.Context, account *Account, headers http.Header, body []byte) {
	if s == nil || s.accountRepo == nil || account == nil {
		return
	}

	persistOpenAI429PlanType(ctx, s.accountRepo, account, body)

	var resetAt *time.Time
	if calculated := calculateOpenAI429ResetTime(headers); calculated != nil {
		resetAt = calculated
	} else if unixTs := parseOpenAIRateLimitResetTime(body); unixTs != nil {
		t := time.Unix(*unixTs, 0)
		resetAt = &t
	}
	if resetAt == nil {
		return
	}

	if err := s.accountRepo.SetRateLimited(ctx, account.ID, *resetAt); err != nil {
		return
	}

	now := time.Now()
	account.RateLimitedAt = &now
	account.RateLimitResetAt = resetAt

	if account.Status == StatusError {
		if err := s.accountRepo.ClearError(ctx, account.ID); err != nil {
			return
		}
		account.Status = StatusActive
		account.ErrorMessage = ""
	}
}

// testGeminiAccountConnection tests a Gemini account's connection
func (s *AccountTestService) testGeminiAccountConnection(c *gin.Context, account *Account, modelID string, prompt string) error {
	ctx := c.Request.Context()

	// Determine the model to use
	testModelID := modelID
	if testModelID == "" {
		testModelID = geminicli.DefaultTestModel
	}

	// For static upstream credentials with model mapping, map the model
	if account.Type == AccountTypeAPIKey || account.Type == AccountTypeServiceAccount {
		mapping := account.GetModelMapping()
		if len(mapping) > 0 {
			if mappedModel, exists := mapping[testModelID]; exists {
				testModelID = mappedModel
			}
		}
	}

	// Set SSE headers
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// Create test payload (Gemini format)
	payload := createGeminiTestPayload(testModelID, prompt)

	// Build request based on account type
	var req *http.Request
	var err error

	switch account.Type {
	case AccountTypeAPIKey:
		req, err = s.buildGeminiAPIKeyRequest(ctx, account, testModelID, payload)
	case AccountTypeOAuth:
		req, err = s.buildGeminiOAuthRequest(ctx, account, testModelID, payload)
	case AccountTypeServiceAccount:
		req, err = s.buildGeminiServiceAccountRequest(ctx, account, testModelID, payload)
	default:
		return s.sendErrorAndEnd(c, fmt.Sprintf("Unsupported account type: %s", account.Type))
	}

	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to build request: %s", err.Error()))
	}

	// Send test_start event
	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})

	// Get proxy and execute request
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return s.sendErrorAndEnd(c, fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(body)))
	}

	// Process SSE stream
	return s.processGeminiStream(c, resp.Body)
}

// routeAntigravityTest 路由 Antigravity 账号的测试请求。
// APIKey 类型走原生协议（与 gateway_handler 路由一致），OAuth/Upstream 走 CRS 中转。
func (s *AccountTestService) routeAntigravityTest(c *gin.Context, account *Account, modelID string, prompt string) error {
	if account.Type == AccountTypeAPIKey {
		if strings.HasPrefix(modelID, "gemini-") {
			return s.testGeminiAccountConnection(c, account, modelID, prompt)
		}
		return s.testClaudeAccountConnection(c, account, modelID)
	}
	return s.testAntigravityAccountConnection(c, account, modelID)
}

// testAntigravityAccountConnection tests an Antigravity account's connection
// 支持 Claude 和 Gemini 两种协议，使用非流式请求
func (s *AccountTestService) testAntigravityAccountConnection(c *gin.Context, account *Account, modelID string) error {
	ctx := c.Request.Context()

	testModelID := antigravityConnectionTestModel(modelID)

	if s.antigravityGatewayService == nil {
		return s.sendErrorAndEnd(c, "Antigravity gateway service not configured")
	}

	// Set SSE headers
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	// Send test_start event
	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModelID})

	// 调用 AntigravityGatewayService.TestConnection（复用协议转换逻辑）
	result, err := s.antigravityGatewayService.TestConnection(ctx, account, testModelID)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}

	// 发送响应内容
	if result.Text != "" {
		s.sendEvent(c, TestEvent{Type: "content", Text: result.Text})
	}

	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func antigravityConnectionTestModel(modelID string) string {
	if modelID == "" {
		return defaultAntigravityTestModel
	}
	return modelID
}

// buildGeminiAPIKeyRequest builds request for Gemini API Key accounts
func (s *AccountTestService) buildGeminiAPIKeyRequest(ctx context.Context, account *Account, modelID string, payload []byte) (*http.Request, error) {
	apiKey := account.GetCredential("api_key")
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("no API key available")
	}

	baseURL := account.GetCredential("base_url")
	if baseURL == "" {
		baseURL = geminicli.AIStudioBaseURL
	}
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, err
	}

	// Use streamGenerateContent for real-time feedback
	fullURL, err := buildGeminiAIStudioModelActionURL(normalizedBaseURL, modelID, "streamGenerateContent", true)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	return req, nil
}

// buildGeminiOAuthRequest builds request for Gemini OAuth accounts
func (s *AccountTestService) buildGeminiOAuthRequest(ctx context.Context, account *Account, modelID string, payload []byte) (*http.Request, error) {
	if s.geminiTokenProvider == nil {
		return nil, fmt.Errorf("gemini token provider not configured")
	}

	// Get access token (auto-refreshes if needed)
	accessToken, err := s.geminiTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("failed to get access token: %w", err)
	}

	projectID := strings.TrimSpace(account.GetCredential("project_id"))
	if projectID == "" {
		// AI Studio OAuth mode (no project_id): call generativelanguage API directly with Bearer token.
		baseURL := account.GetCredential("base_url")
		if strings.TrimSpace(baseURL) == "" {
			baseURL = geminicli.AIStudioBaseURL
		}
		normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return nil, err
		}
		fullURL, err := buildGeminiAIStudioModelActionURL(normalizedBaseURL, modelID, "streamGenerateContent", true)
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+accessToken)
		return req, nil
	}

	// Code Assist mode (with project_id)
	return s.buildCodeAssistRequest(ctx, accessToken, projectID, modelID, payload)
}

func (s *AccountTestService) buildGeminiServiceAccountRequest(ctx context.Context, account *Account, modelID string, payload []byte) (*http.Request, error) {
	if s.geminiTokenProvider == nil {
		return nil, fmt.Errorf("gemini token provider not configured")
	}
	accessToken, err := s.geminiTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("failed to get service account access token: %w", err)
	}
	fullURL, err := buildVertexGeminiURL(account.VertexProjectID(), account.VertexLocation(modelID), modelID, "streamGenerateContent", true)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	return req, nil
}

// buildCodeAssistRequest builds request for Google Code Assist API (used by Gemini CLI and Antigravity)
func (s *AccountTestService) buildCodeAssistRequest(ctx context.Context, accessToken, projectID, modelID string, payload []byte) (*http.Request, error) {
	var inner map[string]any
	if err := json.Unmarshal(payload, &inner); err != nil {
		return nil, err
	}

	wrapped := map[string]any{
		"model":   modelID,
		"project": projectID,
		"request": inner,
	}
	wrappedBytes, _ := json.Marshal(wrapped)

	normalizedBaseURL, err := s.validateUpstreamBaseURL(geminicli.GeminiCliBaseURL)
	if err != nil {
		return nil, err
	}
	fullURL := fmt.Sprintf("%s/v1internal:streamGenerateContent?alt=sse", normalizedBaseURL)

	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(wrappedBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", geminicli.GeminiCLIUserAgent)

	return req, nil
}

// createGeminiTestPayload creates a minimal test payload for Gemini API.
// Image models use the image-generation path so the frontend can preview the returned image.
func createGeminiTestPayload(modelID string, prompt string) []byte {
	if isImageGenerationModel(modelID) {
		imagePrompt := strings.TrimSpace(prompt)
		if imagePrompt == "" {
			imagePrompt = defaultGeminiImageTestPrompt
		}

		payload := map[string]any{
			"contents": []map[string]any{
				{
					"role": "user",
					"parts": []map[string]any{
						{"text": imagePrompt},
					},
				},
			},
			"generationConfig": map[string]any{
				"responseModalities": []string{"TEXT", "IMAGE"},
				"imageConfig": map[string]any{
					"aspectRatio": "1:1",
				},
			},
		}
		bytes, _ := json.Marshal(payload)
		return bytes
	}

	textPrompt := strings.TrimSpace(prompt)
	if textPrompt == "" {
		textPrompt = defaultGeminiTextTestPrompt
	}

	payload := map[string]any{
		"contents": []map[string]any{
			{
				"role": "user",
				"parts": []map[string]any{
					{"text": textPrompt},
				},
			},
		},
		"systemInstruction": map[string]any{
			"parts": []map[string]any{
				{"text": "You are a helpful AI assistant."},
			},
		},
	}
	bytes, _ := json.Marshal(payload)
	return bytes
}

// processGeminiStream processes SSE stream from Gemini API
func (s *AccountTestService) processGeminiStream(c *gin.Context, body io.Reader) error {
	reader := bufio.NewReader(body)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
				return nil
			}
			return s.sendErrorAndEnd(c, fmt.Sprintf("Stream read error: %s", err.Error()))
		}

		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}

		jsonStr := strings.TrimPrefix(line, "data: ")
		if jsonStr == "[DONE]" {
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			continue
		}

		// Support two Gemini response formats:
		// - AI Studio: {"candidates": [...]}
		// - Gemini CLI: {"response": {"candidates": [...]}}
		if resp, ok := data["response"].(map[string]any); ok && resp != nil {
			data = resp
		}
		if candidates, ok := data["candidates"].([]any); ok && len(candidates) > 0 {
			if candidate, ok := candidates[0].(map[string]any); ok {
				// Extract content first (before checking completion)
				if content, ok := candidate["content"].(map[string]any); ok {
					if parts, ok := content["parts"].([]any); ok {
						for _, part := range parts {
							if partMap, ok := part.(map[string]any); ok {
								if text, ok := partMap["text"].(string); ok && text != "" {
									s.sendEvent(c, TestEvent{Type: "content", Text: text})
								}
								if inlineData, ok := partMap["inlineData"].(map[string]any); ok {
									mimeType, _ := inlineData["mimeType"].(string)
									data, _ := inlineData["data"].(string)
									if strings.HasPrefix(strings.ToLower(mimeType), "image/") && data != "" {
										s.sendEvent(c, TestEvent{
											Type:     "image",
											ImageURL: fmt.Sprintf("data:%s;base64,%s", mimeType, data),
											MimeType: mimeType,
										})
									}
								}
							}
						}
					}
				}

				// Check for completion after extracting content
				if finishReason, ok := candidate["finishReason"].(string); ok && finishReason != "" {
					s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
					return nil
				}
			}
		}

		// Handle errors
		if errData, ok := data["error"].(map[string]any); ok {
			errorMsg := "Unknown error"
			if msg, ok := errData["message"].(string); ok {
				errorMsg = msg
			}
			return s.sendErrorAndEnd(c, errorMsg)
		}
	}
}

// createOpenAITestPayload creates a test payload for OpenAI Responses API
func createOpenAITestPayload(modelID string, isOAuth bool) map[string]any {
	payload := map[string]any{
		"model": modelID,
		"input": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{
						"type": "input_text",
						"text": "hi",
					},
				},
			},
		},
		"stream": true,
	}

	// OAuth accounts using ChatGPT internal API require store: false
	if isOAuth {
		payload["store"] = false
	}

	// All accounts require instructions for Responses API
	payload["instructions"] = openai.DefaultInstructions

	return payload
}

func createOpenAIChatCompletionsTestPayload(modelID string, prompt string) map[string]any {
	testPrompt := strings.TrimSpace(prompt)
	if testPrompt == "" {
		testPrompt = "hi"
	}

	return map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": testPrompt,
			},
		},
		"stream": true,
	}
}

// processClaudeStream processes the SSE stream from Claude API
func (s *AccountTestService) processClaudeStream(c *gin.Context, body io.Reader) error {
	reader := bufio.NewReader(body)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
				return nil
			}
			return s.sendErrorAndEnd(c, fmt.Sprintf("Stream read error: %s", err.Error()))
		}

		line = strings.TrimSpace(line)
		if line == "" || !sseDataPrefix.MatchString(line) {
			continue
		}

		jsonStr := sseDataPrefix.ReplaceAllString(line, "")
		if jsonStr == "[DONE]" {
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			continue
		}

		eventType, _ := data["type"].(string)

		switch eventType {
		case "content_block_delta":
			if delta, ok := data["delta"].(map[string]any); ok {
				if text, ok := delta["text"].(string); ok {
					s.sendEvent(c, TestEvent{Type: "content", Text: text})
				}
			}
		case "message_stop":
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		case "error":
			errorMsg := "Unknown error"
			if errData, ok := data["error"].(map[string]any); ok {
				if msg, ok := errData["message"].(string); ok {
					errorMsg = msg
				}
			}
			return s.sendErrorAndEnd(c, errorMsg)
		}
	}
}

// processOpenAIChatCompletionsStream processes SSE chunks from the
// OpenAI-compatible Chat Completions API.
func (s *AccountTestService) processOpenAIChatCompletionsStream(c *gin.Context, body io.Reader) error {
	reader := bufio.NewReader(body)
	seenJSON := false
	seenFinish := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if seenFinish {
					s.sendEvent(c, TestEvent{Type: "status", Text: "已通过 /v1/chat/completions 验证"})
					s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
					return nil
				}
				if seenJSON {
					return s.sendErrorAndEnd(c, "Chat Completions stream from /v1/chat/completions ended before [DONE]")
				}
				return s.sendErrorAndEnd(c, "Invalid Chat Completions response from /v1/chat/completions: expected SSE JSON data")
			}
			return s.sendErrorAndEnd(c, fmt.Sprintf("Chat Completions stream read error from /v1/chat/completions: %s", err.Error()))
		}

		line = strings.TrimSpace(line)
		if line == "" || !sseDataPrefix.MatchString(line) {
			continue
		}

		jsonStr := sseDataPrefix.ReplaceAllString(line, "")
		if jsonStr == "[DONE]" {
			s.sendEvent(c, TestEvent{Type: "status", Text: "已通过 /v1/chat/completions 验证"})
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			return s.sendErrorAndEnd(c, "Invalid Chat Completions response from /v1/chat/completions: expected JSON data")
		}
		seenJSON = true

		if errData, ok := data["error"].(map[string]any); ok {
			errorMsg := "Chat Completions API (/v1/chat/completions) returned an error"
			if msg, ok := errData["message"].(string); ok && msg != "" {
				errorMsg = msg
			}
			return s.sendErrorAndEnd(c, fmt.Sprintf("Chat Completions API (/v1/chat/completions) error: %s", errorMsg))
		}

		choices, ok := data["choices"].([]any)
		if !ok {
			continue
		}
		for _, choiceValue := range choices {
			choice, ok := choiceValue.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				if text, ok := delta["content"].(string); ok && text != "" {
					s.sendEvent(c, TestEvent{Type: "content", Text: text})
				}
			}
			if message, ok := choice["message"].(map[string]any); ok {
				if text, ok := message["content"].(string); ok && text != "" {
					s.sendEvent(c, TestEvent{Type: "content", Text: text})
				}
			}
			if finishReason, ok := choice["finish_reason"].(string); ok && finishReason != "" {
				seenFinish = true
			}
		}
	}
}

// processOpenAIStream processes the SSE stream from OpenAI Responses API
func (s *AccountTestService) processOpenAIStream(c *gin.Context, body io.Reader) error {
	reader := bufio.NewReader(body)
	seenCompleted := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				if seenCompleted {
					s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
					return nil
				}
				return s.sendErrorAndEnd(c, "Stream ended before response.completed")
			}
			return s.sendErrorAndEnd(c, fmt.Sprintf("Stream read error: %s", err.Error()))
		}

		line = strings.TrimSpace(line)
		if line == "" || !sseDataPrefix.MatchString(line) {
			continue
		}

		jsonStr := sseDataPrefix.ReplaceAllString(line, "")
		if jsonStr == "[DONE]" {
			if seenCompleted {
				s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
				return nil
			}
			return s.sendErrorAndEnd(c, "Stream ended before response.completed")
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			continue
		}

		eventType, _ := data["type"].(string)

		switch eventType {
		case "response.output_text.delta":
			// OpenAI Responses API uses "delta" field for text content
			if delta, ok := data["delta"].(string); ok && delta != "" {
				s.sendEvent(c, TestEvent{Type: "content", Text: delta})
			}
		case "response.completed", "response.done":
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		case "response.failed":
			errorMsg := "OpenAI response failed"
			if responseData, ok := data["response"].(map[string]any); ok {
				if errData, ok := responseData["error"].(map[string]any); ok {
					if msg, ok := errData["message"].(string); ok && msg != "" {
						errorMsg = msg
					}
				}
			}
			return s.sendErrorAndEnd(c, errorMsg)
		case "error":
			errorMsg := "Unknown error"
			if errData, ok := data["error"].(map[string]any); ok {
				if msg, ok := errData["message"].(string); ok {
					errorMsg = msg
				}
			}
			return s.sendErrorAndEnd(c, errorMsg)
		}
	}
}

// testOpenAIImageAPIKey tests OpenAI image generation using an API Key account.
func (s *AccountTestService) testOpenAIImageAPIKey(c *gin.Context, ctx context.Context, account *Account, modelID, prompt string) error {
	authToken := account.GetOpenAIApiKey()
	if authToken == "" {
		return s.sendErrorAndEnd(c, "No API key available")
	}

	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	normalizedBaseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Invalid base URL: %s", err.Error()))
	}
	apiURL := buildOpenAIImagesURL(normalizedBaseURL, openAIImagesGenerationsEndpoint)

	// Set SSE headers
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	s.sendEvent(c, TestEvent{Type: "test_start", Model: modelID})

	payload := map[string]any{
		"model":           modelID,
		"prompt":          prompt,
		"n":               1,
		"response_format": "b64_json",
	}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create request")
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+authToken)

	// 账号级请求头覆写：测试请求与真实转发保持一致的最终头
	account.ApplyHeaderOverrides(req.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Request failed: %s", err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to read response: %s", err.Error()))
	}

	if resp.StatusCode != http.StatusOK {
		return s.sendErrorAndEnd(c, fmt.Sprintf("API returned %d: %s", resp.StatusCode, string(body)))
	}

	// Parse {"data": [{"b64_json": "...", "revised_prompt": "..."}]}
	var result struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to parse response: %s", err.Error()))
	}

	if len(result.Data) == 0 {
		return s.sendErrorAndEnd(c, "No images returned from API")
	}

	for _, item := range result.Data {
		if item.RevisedPrompt != "" {
			s.sendEvent(c, TestEvent{Type: "content", Text: item.RevisedPrompt})
		}
		if item.B64JSON != "" {
			s.sendEvent(c, TestEvent{
				Type:     "image",
				ImageURL: "data:image/png;base64," + item.B64JSON,
				MimeType: "image/png",
			})
		}
	}

	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// testOpenAIImageOAuth tests OpenAI image generation using an OAuth account via Codex /responses API.
func (s *AccountTestService) testOpenAIImageOAuth(c *gin.Context, ctx context.Context, account *Account, modelID, prompt string) error {
	credentialAccount := account
	if account.IsShadow() {
		resolved, err := resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to resolve account credentials")
		}
		credentialAccount = resolved
	}
	authToken := ""
	if !credentialAccount.IsOpenAIAgentIdentity() {
		authToken = credentialAccount.GetOpenAIAccessToken()
	}
	if authToken == "" && !credentialAccount.IsOpenAIAgentIdentity() {
		return s.sendErrorAndEnd(c, "No access token available")
	}

	// Set SSE headers
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	s.sendEvent(c, TestEvent{Type: "test_start", Model: modelID})
	s.sendEvent(c, TestEvent{Type: "content", Text: "Calling Codex /responses image tool...\n"})

	parsed := &OpenAIImagesRequest{
		Endpoint: openAIImagesGenerationsEndpoint,
		Model:    strings.TrimSpace(modelID),
		Prompt:   prompt,
	}
	applyOpenAIImagesDefaults(parsed)

	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("Responses driver: %s; image model: %s\n", openAIImagesResponsesMainModelValue(), parsed.Model)})

	responsesBody, err := buildOpenAIImagesResponsesRequest(parsed, parsed.Model)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to build image request: %s", err.Error()))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexAPIURL, bytes.NewReader(responsesBody))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create request")
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Host = "chatgpt.com"
	if credentialAccount.IsOpenAIAgentIdentity() {
		authHeaders, authErr := buildAgentIdentityAuthenticationHeaders(ctx, s.accountRepo, s.agentIdentityWS, &s.agentIdentityTaskMu, credentialAccount)
		if authErr != nil {
			return s.sendErrorAndEnd(c, "Failed to build Agent Identity authentication")
		}
		for key, values := range authHeaders {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	canonical := resolveCodexOutboundIdentity("")
	req.Header.Set("originator", canonical.originator)
	if customUA := strings.TrimSpace(credentialAccount.GetOpenAIUserAgent()); customUA != "" {
		req.Header.Set("User-Agent", customUA)
	} else {
		req.Header.Set("User-Agent", canonical.userAgent)
	}
	setOpenAIChatGPTAccountHeaders(req.Header, credentialAccount)
	// 与真实转发一致：账号级自定义 UA 同样作为管理员显式配置传入，否则测试用的身份
	// 与该账号真实出站的身份不是同一个（issue #3901 的配对不变式由收口保证）。
	enforceCodexIdentityHeadersWithUA(req.Header, credentialAccount.GetOpenAIUserAgent())

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.doOpenAIAccountTestUpstream(req, proxyURL, account, false)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Responses API request failed: %s", err.Error()))
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		body = redactAgentIdentitySensitiveBodyForAccount(ctx, s.accountRepo, credentialAccount, body)
		message := strings.TrimSpace(extractUpstreamErrorMessage(body))
		if message == "" {
			message = fmt.Sprintf("Responses API returned %d", resp.StatusCode)
		}
		return s.sendErrorAndEnd(c, message)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to read image response: %s", err.Error()))
	}
	body = redactAgentIdentitySensitiveBodyForAccount(ctx, s.accountRepo, credentialAccount, body)

	results, _, _, _, _, err := collectOpenAIImagesFromResponsesBody(body)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to parse image response: %s", err.Error()))
	}
	if len(results) == 0 {
		if upstreamErr := extractOpenAIImagesUpstreamError(body); upstreamErr != nil {
			return s.sendErrorAndEnd(c, upstreamErr.clientMessage())
		}
		if textErr := openAIImagesTextFallbackError(body); textErr != nil {
			return s.sendErrorAndEnd(c, textErr.clientMessage())
		}
		return s.sendErrorAndEnd(c, "No images returned from responses API")
	}

	for _, item := range results {
		if item.RevisedPrompt != "" {
			s.sendEvent(c, TestEvent{Type: "content", Text: item.RevisedPrompt})
		}
		mimeType := openAIImageOutputMIMEType(item.OutputFormat)
		s.sendEvent(c, TestEvent{
			Type:     "image",
			ImageURL: "data:" + mimeType + ";base64," + item.Result,
			MimeType: mimeType,
		})
	}

	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) sendEvent(c *gin.Context, event TestEvent) {
	if event.Type == "test_complete" {
		if suppress, ok := c.Get(accountTestSuppressCompletionContextKey); ok {
			if suppressCompletion, _ := suppress.(bool); suppressCompletion {
				return
			}
		}
	}
	eventJSON, _ := json.Marshal(event)
	if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", eventJSON); err != nil {
		log.Printf("failed to write SSE event: %v", err)
		return
	}
	c.Writer.Flush()
}

// sendErrorAndEnd sends an error event and ends the stream
func (s *AccountTestService) sendErrorAndEnd(c *gin.Context, errorMsg string) error {
	log.Printf("Account test error: %s", errorMsg)
	s.sendEvent(c, TestEvent{Type: "error", Error: errorMsg})
	return fmt.Errorf("%s", errorMsg)
}

// PauseAccountScheduling 定时测试连续失败后暂停账号调度（temp-unschedulable 至指定时间；
// 时间到达或恢复测试成功后自动解除）。
func (s *AccountTestService) PauseAccountScheduling(ctx context.Context, accountID int64, until time.Time, reason string) error {
	return s.accountRepo.SetTempUnschedulable(ctx, accountID, until, reason)
}

// RunTestBackground executes an account test in-memory (no real HTTP client),
// capturing SSE output via httptest.NewRecorder, then parses the result.
func (s *AccountTestService) RunTestBackground(ctx context.Context, accountID int64, modelID string) (*ScheduledTestResult, error) {
	startedAt := time.Now()

	w := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(w)
	ginCtx.Request = (&http.Request{}).WithContext(ctx)

	testErr := s.TestAccountConnection(ginCtx, accountID, modelID, "", AccountTestModeDefault)

	finishedAt := time.Now()
	body := w.Body.String()
	responseText, errMsg := parseTestSSEOutput(body)

	status := "success"
	if testErr != nil || errMsg != "" {
		status = "failed"
		if errMsg == "" && testErr != nil {
			errMsg = testErr.Error()
		}
	}

	return &ScheduledTestResult{
		Status:       status,
		ResponseText: responseText,
		ErrorMessage: errMsg,
		LatencyMs:    finishedAt.Sub(startedAt).Milliseconds(),
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
	}, nil
}

// parseTestSSEOutput extracts response text and error message from captured SSE output.
func parseTestSSEOutput(body string) (responseText, errMsg string) {
	var texts []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		jsonStr := strings.TrimPrefix(line, "data: ")
		var event TestEvent
		if err := json.Unmarshal([]byte(jsonStr), &event); err != nil {
			continue
		}
		switch event.Type {
		case "content":
			if event.Text != "" {
				texts = append(texts, event.Text)
			}
		case "error":
			errMsg = event.Error
		}
	}
	responseText = strings.Join(texts, "")
	return
}
