package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
)

// 检测结果四态（docs/capability-routing-plan.md §3.5）。
const (
	// VisionDetectFailed 传输/HTTP 层失败（key 无效/4xx/超时/网络错），不落库。
	VisionDetectFailed = "detect_failed"
	// VisionDetectSupported 200 且响应精确含验证码，落库 supports_vision=true。
	VisionDetectSupported = "supported"
	// VisionDetectUnsupported 200 但响应不含验证码（非策略拒答），落库 false。
	VisionDetectUnsupported = "unsupported"
	// VisionDetectManualReview 200 但明确政策/意愿类拒答（仅限 policy/refusal-agency
	// 措辞 + 图片关键词两级命中，方案 §3.5），不落库 false，待人工裁决。
	// 普通能力性措辞（cannot identify/无法识别等）不自动落 manual_review，走 unsupported 落 false。
	VisionDetectManualReview = "manual_review"
)

// VisionDetectResult 单次 (账号, 模型, 协议) 检测结果。
type VisionDetectResult struct {
	AccountID      int64  `json:"account_id"`
	UpstreamModel  string `json:"upstream_model"`
	Protocol       string `json:"protocol"`
	Result         string `json:"result"` // 四态之一
	CaptchaCode    string `json:"captcha_code,omitempty"`
	ResponseSnippet string `json:"response_snippet,omitempty"`
	Message        string `json:"message,omitempty"`
	// 落库记录：supported/unsupported 时返回落库后的记录；其他态为 nil。
	Capability *model.AccountModelCapability `json:"capability,omitempty"`
}

// visionDetectHTTPDoer 抽象上游 HTTP 发送，便于测试注入 httptest。
type visionDetectHTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// VisionDetectService 手动一键检测（docs/capability-routing-plan.md §3.5）。
//
// 流程：用账号真实 key 发随机验证码图到指定模型 → 四态分类（detect_failed 不落库 /
// supported 落 true / unsupported 落 false / manual_review 不落 false）→ 命中
// supported/unsupported 时经 AccountModelCapabilityService.UpsertCapability 落库
// 并触发缓存失效广播。
type VisionDetectService struct {
	accountRepo AccountRepository
	capability  *AccountModelCapabilityService
	httpDoer    visionDetectHTTPDoer
	usageLog    UsageLogRepository
}

// visionDetectCaptchaCodeGenerator 可注入的验证码生成函数（测试可覆盖以固定验证码）。
var visionDetectCaptchaCodeGenerator = randomCaptchaCode

// NewVisionDetectService 创建手动检测服务。
func NewVisionDetectService(
	accountRepo AccountRepository,
	capability *AccountModelCapabilityService,
) *VisionDetectService {
	return &VisionDetectService{
		accountRepo: accountRepo,
		capability:  capability,
	}
}

// SetHTTPDoer 注入 HTTP 发送器（测试用）；nil 时使用默认实现。
func (s *VisionDetectService) SetHTTPDoer(doer visionDetectHTTPDoer) {
	if s != nil {
		s.httpDoer = doer
	}
}

// SetUsageLogRepository 注入 usage 落库仓库（测试用）。生产接线不在本次范围；
// 未注入（nil）时 WriteUsageLog 跳过落库。
func (s *VisionDetectService) SetUsageLogRepository(repo UsageLogRepository) {
	if s != nil {
		s.usageLog = repo
	}
}

// DetectCapability 对 (accountID, upstreamModel, protocol) 执行一次视觉能力检测。
//
// protocol 为空时默认 chat_completions（方案 §3.5 当前先覆盖该协议）。
// model 传入的是发给上游的模型名（已按调用方意图映射）；内部不再二次映射。
func (s *VisionDetectService) DetectCapability(
	ctx context.Context,
	accountID int64,
	upstreamModel string,
	protocol string,
) (*VisionDetectResult, error) {
	if s == nil {
		return nil, errors.New("vision detect service is nil")
	}
	start := time.Now()
	if accountID <= 0 {
		return nil, errors.New("invalid account id")
	}
	if strings.TrimSpace(upstreamModel) == "" {
		return nil, errors.New("upstream model is required")
	}
	if strings.TrimSpace(protocol) == "" {
		protocol = model.CapabilityProtocolChatCompletions
	}
	if protocol != model.CapabilityProtocolChatCompletions {
		// 方案 §3.5：先覆盖 chat_completions 协议；responses/anthropic 待后续接入。
		return nil, fmt.Errorf("vision detection only supports chat_completions protocol, got %q", protocol)
	}

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("load account %d: %w", accountID, err)
	}
	apiKey := account.GetOpenAIProtocolAPIKey()
	if strings.TrimSpace(apiKey) == "" {
		// P2-D：缺 key 早期终态经统一日志（不记验证码真值 / 不记 key）。
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return &VisionDetectResult{
			AccountID:     accountID,
			UpstreamModel: upstreamModel,
			Protocol:      protocol,
			Result:        VisionDetectFailed,
			Message:       "no API key available for OpenAI protocol",
		}, nil
	}
	baseURL := account.GetOpenAIBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		// P2-D：缺 base URL 早期终态经统一日志。
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return &VisionDetectResult{
			AccountID:     accountID,
			UpstreamModel: upstreamModel,
			Protocol:      protocol,
			Result:        VisionDetectFailed,
			Message:       "no upstream base URL available",
		}, nil
	}

	code, err := visionDetectCaptchaCodeGenerator()
	if err != nil {
		// P2-D：生成验证码失败早期终态经统一日志后上抛 error。
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return nil, fmt.Errorf("generate captcha code: %w", err)
	}
	imageDataURL, err := renderCaptchaPNGDataURL(code)
	if err != nil {
		// P2-D：渲染图片失败早期终态经统一日志后上抛 error。
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return nil, fmt.Errorf("render captcha image: %w", err)
	}

	payload := buildVisionDetectPayload(upstreamModel, code, imageDataURL)
	body, err := json.Marshal(payload)
	if err != nil {
		// P2-D：序列化请求失败早期终态经统一日志后上抛 error。
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return nil, fmt.Errorf("encode detect payload: %w", err)
	}

	apiURL := openAIChatCompletionsURLForBase(baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create detect request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	account.ApplyHeaderOverrides(req.Header)

	// P1-A（安全）：ProxyID 非空但 account.Proxy 关系缺失 → 代理配置关系缺失。
	// 终止检测，绝不退回无代理直连；请求尚未发出，不记 usage，仅写日志。
	if account != nil && account.ProxyID != nil && account.Proxy == nil {
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return &VisionDetectResult{
			AccountID:      accountID,
			UpstreamModel:  upstreamModel,
			Protocol:       protocol,
			Result:         VisionDetectFailed,
			Message:        "proxy configuration relation missing (ProxyID set but account.Proxy is nil)",
		}, nil
	}

	// P1-B：代理已配置且关系存在，但 URL 解析失败 → 终止检测，绝不退回无代理直连。
	// 请求尚未发出，按既有边界不记 usage，仅写 vision_detect_result 日志。
	var proxyURL *url.URL
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		parsed, perr := parseProxyURL(account.Proxy.URL())
		if perr != nil {
			s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
			return &VisionDetectResult{
				AccountID:      accountID,
				UpstreamModel:  upstreamModel,
				Protocol:       protocol,
				Result:         VisionDetectFailed,
				Message:        fmt.Sprintf("proxy configuration invalid: %v", perr),
			}, nil
		}
		proxyURL = parsed
	}

	doer := s.httpDoer
	if doer == nil {
		doer = s.defaultDoer(proxyURL)
	}
	resp, err := doer.Do(req)
	if err != nil {
		// 网络/超时等传输层错误 → detect_failed，记平台 usage 但不进用户账单。
		if recErr := s.writeVisionDetectUsage(ctx, accountID, upstreamModel, protocol, VisionDetectFailed, 0, 0); recErr != nil {
			return nil, fmt.Errorf("record vision detect usage: %w", recErr)
		}
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return &VisionDetectResult{
			AccountID:      accountID,
			UpstreamModel:  upstreamModel,
			Protocol:       protocol,
			Result:         VisionDetectFailed,
			CaptchaCode:    code,
			ResponseSnippet: "",
			Message:        fmt.Sprintf("request failed: %v", err),
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		// 请求已发出后体读失败 → 收敛到统一失败终结路径：先记 usage 再记日志，最后返回 detect_failed（P2-D）。
		if recErr := s.writeVisionDetectUsage(ctx, accountID, upstreamModel, protocol, VisionDetectFailed, 0, 0); recErr != nil {
			return nil, fmt.Errorf("record vision detect usage: %w", recErr)
		}
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return &VisionDetectResult{
			AccountID:      accountID,
			UpstreamModel:  upstreamModel,
			Protocol:       protocol,
			Result:         VisionDetectFailed,
			CaptchaCode:    code,
			Message:        "read response body failed",
		}, nil
	}

	// 传输/HTTP 层失败（key 无效/4xx/5xx）→ detect_failed，记平台 usage 但不进用户账单。
	if resp.StatusCode != http.StatusOK {
		if recErr := s.writeVisionDetectUsage(ctx, accountID, upstreamModel, protocol, VisionDetectFailed, 0, 0); recErr != nil {
			return nil, fmt.Errorf("record vision detect usage: %w", recErr)
		}
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return &VisionDetectResult{
			AccountID:      accountID,
			UpstreamModel:  upstreamModel,
			Protocol:       protocol,
			Result:         VisionDetectFailed,
			CaptchaCode:    code,
			ResponseSnippet: truncateString(string(respBody), 200),
			Message:        fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode),
		}, nil
	}

	// P2-C：200 响应先做结构校验。非有效 JSON / 显式 error payload / 缺失有效
	// choices·message 一律判 detect_failed（协议/上游故障），保留 usage 与日志，
	// 不写能力标记；仅结构有效的普通文本无验证码响应才落 unsupported。
	valid, content := validateVisionDetect200Response(respBody)
	if !valid {
		if recErr := s.writeVisionDetectUsage(ctx, accountID, upstreamModel, protocol, VisionDetectFailed, 0, 0); recErr != nil {
			return nil, fmt.Errorf("record vision detect usage: %w", recErr)
		}
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return &VisionDetectResult{
			AccountID:      accountID,
			UpstreamModel:  upstreamModel,
			Protocol:       protocol,
			Result:         VisionDetectFailed,
			CaptchaCode:    code,
			Message:        "upstream returned malformed or error response (protocol/upstream fault)",
		}, nil
	}

	snippet := truncateString(content, 200)

	// 200 + 策略性拒答 → manual_review，不落库 false。
	if isVisionRefusalResponse(content) {
		if recErr := s.writeVisionDetectUsage(ctx, accountID, upstreamModel, protocol, VisionDetectManualReview, 0, 0); recErr != nil {
			return nil, fmt.Errorf("record vision detect usage: %w", recErr)
		}
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectManualReview, time.Since(start).Milliseconds())
		return &VisionDetectResult{
			AccountID:      accountID,
			UpstreamModel:  upstreamModel,
			Protocol:       protocol,
			Result:         VisionDetectManualReview,
			CaptchaCode:    code,
			ResponseSnippet: snippet,
			Message:        "upstream refused to read the captcha (policy-like refusal); needs manual review",
		}, nil
	}

	supports := visionDetectCodeMatches(content, code)

	// 200 且响应恰含验证码（精确数字匹配）→ 落 true；否则落 false。
	result := &VisionDetectResult{
		AccountID:      accountID,
		UpstreamModel:  upstreamModel,
		Protocol:       protocol,
		CaptchaCode:    code,
		ResponseSnippet: snippet,
	}
	if supports {
		result.Result = VisionDetectSupported
	} else {
		result.Result = VisionDetectUnsupported
	}

	// 已真正向上游发起检测并得到结果 → 记平台 usage（不进用户账单）。
	inTokens, outTokens := extractVisionDetectUsageTokens(respBody)
	if recErr := s.writeVisionDetectUsage(ctx, accountID, upstreamModel, protocol, result.Result, inTokens, outTokens); recErr != nil {
		return nil, fmt.Errorf("record vision detect usage: %w", recErr)
	}

	now := time.Now()
	capability, err := s.capability.UpsertCapability(ctx, &model.AccountModelCapability{
		AccountID:      accountID,
		UpstreamModel:  upstreamModel,
		Protocol:       protocol,
		SupportsVision: supports,
		Source:         model.CapabilitySourceDetect,
		DetectedAt:     &now,
	})
	if err != nil {
		// 落库失败视为传输/存储层失败，仍返回失败态（不误报成功）；但 usage 已记。
		result.Result = VisionDetectFailed
		result.Message = fmt.Sprintf("persist capability failed: %v", err)
		s.logDetectResult(accountID, upstreamModel, protocol, VisionDetectFailed, time.Since(start).Milliseconds())
		return result, nil
	}
	s.logDetectResult(accountID, upstreamModel, protocol, result.Result, time.Since(start).Milliseconds())
	result.Capability = capability
	return result, nil
}

// DetectCapabilities 对 (账号, 多模型, 协议) 批量检测。单模型失败返回错误，
// 检测失败/拒答不视为整体错误（结果在 Result 字段中表达）。
func (s *VisionDetectService) DetectCapabilities(
	ctx context.Context,
	accountID int64,
	models []string,
	protocol string,
) ([]*VisionDetectResult, error) {
	if len(models) == 0 {
		return nil, nil
	}
	results := make([]*VisionDetectResult, 0, len(models))
	for _, m := range models {
		res, err := s.DetectCapability(ctx, accountID, m, protocol)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

// visionDetectTimeout 单次检测请求超时。检测为管理员触发的低频动作，
// 取相对宽松的上限（视觉模型首 token 较慢），超时归入 detect_failed 不落库。
const visionDetectTimeout = 90 * time.Second

// defaultDoer 构造带账号代理与超时的默认 HTTP client。
// proxy 为调用方已校验解析的代理 URL；nil 表示不使用代理。
// 代理解析失败的校验已前置到 DetectCapability（P1-B），此处不再静默退回无代理直连。
func (s *VisionDetectService) defaultDoer(proxy *url.URL) visionDetectHTTPDoer {
	transport := &http.Transport{Proxy: nil}
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	}
	return &http.Client{Timeout: visionDetectTimeout, Transport: transport}
}

// buildVisionDetectPayload 构造 chat_completions 检测请求体：
// 一张验证码图 + 引导 prompt（方案 §3.5：精确读出验证码）。
func buildVisionDetectPayload(model, code, imageDataURL string) map[string]any {
	_ = code // 验证码同时嵌入图像内容，由上游视觉识别读出；文本提示不带真值。
	return map[string]any{
		"model": model,
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "image_url", "image_url": map[string]any{"url": imageDataURL}},
					{"type": "text", "text": "Please read the digits in the image and reply with only the digits."},
				},
			},
		},
		"stream": false,
		"max_tokens": 32,
	}
}

// validateVisionDetect200Response 校验 200 响应的结构有效性（P2-C）。
// 返回 (valid, content)：valid=false 表示协议/上游故障（非有效 JSON /
// 显式 error payload / 缺失有效 choices·message），此时 content 为空；
// valid=true 时 content 为首个 message 文本（已拼接文本块）。
func validateVisionDetect200Response(body []byte) (bool, string) {
	var payload struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// 非有效 JSON → 协议故障。
		return false, ""
	}
	if payload.Error != nil && payload.Error.Message != "" {
		// 显式 error payload → 上游故障。
		return false, ""
	}
	content := extractVisionDetectContent(body)
	if strings.TrimSpace(content) == "" {
		// 缺失有效 choices·message（或 message 为空）→ 结构无效。
		return false, ""
	}
	return true, content
}

// extractVisionDetectContent 从 chat_completions 响应中提取首个 message content。
// deepseek 推理模型（如 deepseek-v4.1-flash）可能把可见文本放在 reasoning_content
// 而 content 为空（生产实证：星思云 200 响应 content=""、reasoning_content 有文本；
// 与 channel_monitor_checker.go 同根因，d792507c9 先例），content 为空时回退读取。
func extractVisionDetectContent(body []byte) string {
	var payload struct {
		Choices []struct {
			Message struct {
				Content          any    `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return string(body)
	}
	if payload.Error != nil && payload.Error.Message != "" {
		return payload.Error.Message
	}
	for _, choice := range payload.Choices {
		content := choice.Message.Content
		switch v := content.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return v
			}
			if rc := strings.TrimSpace(choice.Message.ReasoningContent); rc != "" {
				return rc
			}
			continue
		case []any:
			var sb strings.Builder
			for _, part := range v {
				if partMap, ok := part.(map[string]any); ok {
					if text, ok := partMap["text"].(string); ok {
						sb.WriteString(text)
					}
				}
			}
			if s := sb.String(); strings.TrimSpace(s) != "" {
				return s
			}
			if rc := strings.TrimSpace(choice.Message.ReasoningContent); rc != "" {
				return rc
			}
			continue
		}
	}
	return ""
}

// visionDetectCodeMatches 精确数字匹配（P2-4）：剔除一切非数字字符后，
// content 中的纯数字串须恰等于 code 才判定为 supported。避免子串/前后附加数字误判。
func visionDetectCodeMatches(content, code string) bool {
	var digits strings.Builder
	for _, r := range content {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	return digits.String() == code
}

// extractVisionDetectUsageTokens 从 chat_completions 响应提取 usage 词元数；取不到返回 0。
func extractVisionDetectUsageTokens(body []byte) (input, output int) {
	var payload struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Usage == nil {
		return 0, 0
	}
	return payload.Usage.PromptTokens, payload.Usage.CompletionTokens
}

// logDetectResult 写一条 key=vision_detect_result 的结构化日志（P2-6）。
// 严禁记录 API key、验证码真值（captcha_code）、响应正文。
func (s *VisionDetectService) logDetectResult(accountID int64, upstreamModel, protocol, result string, elapsedMs int64) {
	slog.Info("vision_detect_result",
		"account_id", accountID,
		"upstream_model", upstreamModel,
		"protocol", protocol,
		"result", result,
		"duration_ms", elapsedMs,
	)
}

// writeVisionDetectUsage 记一条平台侧 usage（P2-7）：user_id=0、api_key_id=0 哨兵，
// 由仓库层 prepareUsageLogInsert 归一为 NULL 落库（迁移 261 两列可空），不进任何
// 用户账单/订阅计量（方案 §6）。usageLog 未注入时跳过。
func (s *VisionDetectService) writeVisionDetectUsage(ctx context.Context, accountID int64, upstreamModel, protocol, result string, inputTokens, outputTokens int) error {
	if s == nil || s.usageLog == nil {
		return nil
	}
	upstream := upstreamModel
	log := &UsageLog{
		UserID:       0,
		APIKeyID:     0,
		AccountID:    accountID,
		Model:        upstreamModel,
		UpstreamModel: &upstream,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalCost:    0,
		ActualCost:   0,
		BillingType:  BillingTypeBalance,
		RequestType:  RequestTypeUnknown,
	}
	if _, err := s.usageLog.Create(ctx, log); err != nil {
		return err
	}
	return nil
}

// isVisionRefusalResponse 识别"策略性拒答"（HTTP 200 但明确拒绝识别验证码）。
//
// 两级规则（方案 §3.5 权威口径）：
//   - 第一级（policy/refusal-agency 标记，判定 manual_review 的必要条件）：仅收明确政策/
//     意愿类措辞（拒绝/政策/合规/违规/不允许/禁止等），不含普通能力性措辞。
//   - 第二级（图片/验证码关键词）：沿用 codeRelated 列表。
//
// 判定 manual_review = 第一级命中 且 第二级命中（AND 锚定，防误扩）。
// 普通文本拒绝（能力性措辞如 cannot identify/无法识别/不能辨认等）不构成 manual_review，
// 走既有"200 但无码 → unsupported 落 false"路径（终审 v4 反例）。
func isVisionRefusalResponse(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return false
	}

	// 第一级：仅明确政策/意愿类拒答措辞。能力性措辞（cannot recognize/无法识别等）已排除。
	policyRefusal := []string{
		// EN
		"won't", "will not",
		"refuse", "refuses", "refused",
		"decline", "declines", "declined",
		"not allowed", "not permitted",
		"against my", "against the",
		"policy", "policies", "guidelines", "illegal",
		// 中文
		"拒绝", "不予", "政策", "违规", "违反", "合规", "条款",
		"不允许", "禁止",
	}

	// 第二级：图片/验证码关键词。
	codeRelated := []string{
		"captcha", "verification code", "验证码",
		"image", "picture", "photo", "digit", "数字", "图片", "图像", "验证",
	}

	hasPolicyRefusal := false
	for _, w := range policyRefusal {
		if strings.Contains(t, w) {
			hasPolicyRefusal = true
			break
		}
	}
	if !hasPolicyRefusal {
		return false
	}
	for _, w := range codeRelated {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// parseProxyURL 解析代理 URL。
func parseProxyURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}
