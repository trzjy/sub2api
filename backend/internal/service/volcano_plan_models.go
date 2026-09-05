package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

// 火山方舟（Volcengine Ark）Coding / Agent Plan 订阅号专用路由与模型同步。
//
// 火山订阅号账号的 accounts.platform 仍是 deepseek，识别完全依赖 base_url 的
// host/path（ark.cn-beijing.volces.com + /api/plan → Agent Plan；+ /api/coding →
// Coding Plan），普通 api.deepseek.com 不得命中。路由端点由同一套
// volcanoPlanProfile 派生，复用于模型同步、账号测试、真实网关转发与额度查询，
// 绝不回落 api.deepseek.com / api.openai.com。
const (
	volcanoPlanHost        = "ark.cn-beijing.volces.com"
	volcanoPlanKindAgent   = "agent"
	volcanoPlanKindCoding  = "coding"

	// VolcanoModelSyncPartialCode 是火山模型同步部分成功的 warning code
	// （429/5xx/timeout 目标未探通时带上，提示用户重试而非当作模型不存在）。
	VolcanoModelSyncPartialCode = "volcano_model_sync_partial"

	volcanoProbeTimeout   = 15 * time.Second
	volcanoProbeMaxBytes  = 64 << 10
	volcanoProbeMaxConcurrent = 3
)

// volcanoProbeOutcome 单个候选模型探测结果的分类。
type volcanoProbeOutcome int

const (
	volcanoProbeAuth        volcanoProbeOutcome = iota // 401/403：凭证无效，终止
	volcanoProbeOK                                     // HTTP 200 且响应体有效：可用
	volcanoProbeUnavailable                            // 400/404：模型不可用
	volcanoProbeTransient                              // 429/5xx/timeout/传输错误：未探通，不当作模型不存在
)

// volcanoProbeResult 单个候选模型的探测结果汇总（并发收集用）。
type volcanoProbeResult struct {
	model      string
	outcome    volcanoProbeOutcome
	statusCode int
}

// volcanoPlanProfile 由 base_url 解析得到的火山订阅号路由 profile（SSOT）。
type volcanoPlanProfile struct {
	BaseURL string // 规范化后的 base，如 https://ark.cn-beijing.volces.com/api/plan
	Host    string
	Path    string // /api/plan 或 /api/coding
	Kind    string // volcanoPlanKindAgent | volcanoPlanKindCoding
}

// parseVolcanoPlanProfile 用 net/url 严格解析 base_url 的 host/path，判定火山订阅号
// 及其套餐。host 精确匹配 ark.cn-beijing.volces.com（不区分大小写），path 必须落在
// /api/plan（Agent Plan）或 /api/coding（Coding Plan）精确首段，/api/plan-evil、
// /api/coding-v2 等相似子串一律不命中；普通 api.deepseek.com / api.openai.com 返回 false。
// 不使用字符串 Contains 判定 host/path，避免把带相似子串的主机误判为火山。
func parseVolcanoPlanProfile(baseURL string) (volcanoPlanProfile, bool) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return volcanoPlanProfile{}, false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return volcanoPlanProfile{}, false
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return volcanoPlanProfile{}, false
	}
	host := parsed.Hostname()
	if !strings.EqualFold(host, volcanoPlanHost) {
		return volcanoPlanProfile{}, false
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == "/api/plan" || strings.HasPrefix(path, "/api/plan/"):
		kind := volcanoPlanKindAgent
		return volcanoPlanProfile{
			BaseURL: parsed.Scheme + "://" + host + "/api/plan",
			Host:    host,
			Path:    "/api/plan",
			Kind:    kind,
		}, true
	case path == "/api/coding" || strings.HasPrefix(path, "/api/coding/"):
		return volcanoPlanProfile{
			BaseURL: parsed.Scheme + "://" + host + "/api/coding",
			Host:    host,
			Path:    "/api/coding",
			Kind:    volcanoPlanKindCoding,
		}, true
	default:
		return volcanoPlanProfile{}, false
	}
}

// isVolcanoPlanAccount 报告账号是否指向火山方舟订阅号（Agent/Coding Plan）。
func isVolcanoPlanAccount(account *Account) bool {
	if account == nil {
		return false
	}
	_, ok := parseVolcanoPlanProfile(account.GetBaseURL())
	return ok
}

// Anthropic 协议端点：{base}/v1/messages。
func (p volcanoPlanProfile) anthropicMessagesURL() string {
	return strings.TrimRight(p.BaseURL, "/") + "/v1/messages"
}

// OpenAI 兼容 base：{base}/v3（供 /v3/chat/completions 等 OpenAI 端点派生）。
func (p volcanoPlanProfile) openAIChatCompletionsBaseURL() string {
	return strings.TrimRight(p.BaseURL, "/") + "/v3"
}

// OpenAI 协议端点：{base}/v3/chat/completions。
func (p volcanoPlanProfile) openAIChatCompletionsURL() string {
	return p.openAIChatCompletionsBaseURL() + "/chat/completions"
}

// OpenAI Responses 端点：{base}/v3/responses。
func (p volcanoPlanProfile) openAIResponsesURL() string {
	return p.openAIChatCompletionsBaseURL() + "/responses"
}

// openAIChatCompletionsURLForBase 派生 OpenAI Chat Completions 上游端点：base 命中
// 火山订阅号时走 {base}/v3/chat/completions，否则回落平台通用 /v1/chat/completions。
// 供真实 CC 转发与账号测试复用，杜绝火山 base 被打到 /v1/chat/completions。
func openAIChatCompletionsURLForBase(baseURL string) string {
	if profile, ok := parseVolcanoPlanProfile(baseURL); ok {
		return profile.openAIChatCompletionsURL()
	}
	return buildOpenAIChatCompletionsURL(baseURL)
}

// openAIResponsesURLForBase 派生 OpenAI Responses 上游端点：base 命中火山订阅号时走
// {base}/v3/responses，否则回落平台通用 builder（deepseek 无 /v1 前缀）。供真实
// Responses 转发与账号测试复用，杜绝火山 base 被打到 /responses 或 /v1/responses。
func openAIResponsesURLForBase(platform string, baseURL string) string {
	if profile, ok := parseVolcanoPlanProfile(baseURL); ok {
		return profile.openAIResponsesURL()
	}
	return buildOpenAIResponsesURLForPlatform(platform, baseURL)
}

// volcanoPlanModelsFile 是 resources/volcano-plan-models.json 的结构（官方候选清单）。
type volcanoPlanModelsFile struct {
	Source volcanoPlanModelsSource `json:"source"`
	Plans  map[string][]string     `json:"plans"`
}

type volcanoPlanModelsSource struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Updated  string `json:"updated"`
}

func volcanoPlanModelsPath(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.Volcano.PlanModelsFile) != "" {
		return cfg.Volcano.PlanModelsFile
	}
	return "./resources/volcano-plan-models.json"
}

// loadVolcanoPlanCandidates 从官方候选 JSON 读取某套餐的候选模型名（agent/coding）。
// 过滤空名与 'auto'（供应商侧动态路由别名，无稳定语义，不列入账号可用模型）。文件缺失或
// 解析失败失败关闭，绝不运行时爬取官方文档 HTML。
func loadVolcanoPlanCandidates(path, kind string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read volcano plan models file: %w", err)
	}
	var file volcanoPlanModelsFile
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("parse volcano plan models file: %w", err)
	}
	raw := file.Plans[kind]
	if raw == nil {
		return nil, fmt.Errorf("volcano plan kind %q not present in candidates file", kind)
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, name := range raw {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" || strings.EqualFold(trimmed, "auto") {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out, nil
}

// whitelistedAccountModels 返回账号 model_mapping 中已有的具名模型键（白名单，需保留）。
func whitelistedAccountModels(account *Account) []string {
	var out []string
	for modelID := range account.GetModelMapping() {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" || strings.Contains(modelID, "*") {
			continue
		}
		out = append(out, modelID)
	}
	return out
}

// buildVolcanoProbeRequest 构造对账号真实对话端点的最小非流式探活请求。
// Anthropic 协议→ /v1/messages；OpenAI 协议（chat_completions/responses）→ /v3/chat/completions。
// 复用 setAnthropicAPIKeyAuthHeader / ApplyHeaderOverrides，不复制第二套鉴权。
func (s *AccountTestService) buildVolcanoProbeRequest(ctx context.Context, account *Account, profile volcanoPlanProfile, model string) (*http.Request, error) {
	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return nil, newUpstreamModelSyncConfigError("No volcano API key is available", nil)
	}

	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return nil, newUpstreamModelSyncInternalError("Failed to build volcano probe payload", err)
	}

	endpoint := profile.openAIChatCompletionsURL()
	if account.GetAPIProtocol() == APIProtocolAnthropic {
		endpoint = profile.anthropicMessagesURL()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid volcano probe URL", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if account.GetAPIProtocol() == APIProtocolAnthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", claude.APIKeyBetaHeader)
		setAnthropicAPIKeyAuthHeader(req.Header, account, apiKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	// 与真实转发保持一致的最终头
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// probeVolcanoModel 对单个候选执行真实对话端点探活并分类。
// 复用 doUpstreamModelsRequest（代理 + TLS 与真实转发一致）。
func (s *AccountTestService) probeVolcanoModel(ctx context.Context, account *Account, profile volcanoPlanProfile, model string) (volcanoProbeOutcome, int) {
	callCtx, cancel := context.WithTimeout(ctx, volcanoProbeTimeout)
	defer cancel()

	req, err := s.buildVolcanoProbeRequest(callCtx, account, profile, model)
	if err != nil {
		slog.Warn("volcano_model_probe_request_failed", "model", model, "error", err)
		return volcanoProbeTransient, 0
	}

	proxyURL := upstreamModelsProxyURL(account)
	resp, err := s.doUpstreamModelsRequest(req, proxyURL, account)
	if err != nil {
		// 传输/超时错误：目标未探通，不当作模型不存在。
		slog.Warn("volcano_model_probe_transport_failed", "model", model, "error", err)
		return volcanoProbeTransient, 0
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return volcanoProbeAuth, resp.StatusCode
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound:
		// 模型级 400/404：该模型本账号不可用（跳过，不入列表）。
		return volcanoProbeUnavailable, resp.StatusCode
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		// 429 / 5xx：转瞬失败，部分同步警告。
		return volcanoProbeTransient, resp.StatusCode
	case resp.StatusCode == http.StatusOK:
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, volcanoProbeMaxBytes))
		if readErr != nil {
			return volcanoProbeTransient, resp.StatusCode
		}
		var parsed map[string]any
		if json.Unmarshal(body, &parsed) != nil || len(parsed) == 0 {
			// HTTP 200 但响应体无效：异常，不当作可用也不当作模型不存在。
			return volcanoProbeTransient, resp.StatusCode
		}
		if _, hasErr := parsed["error"]; hasErr {
			return volcanoProbeUnavailable, resp.StatusCode
		}
		// 按协议校验响应结构，避免把非生成响应（如配额/告警）误判为模型可用：
		// anthropic /v1/messages 需 content；openai /v3/chat/completions 需 choices。
		if account.GetAPIProtocol() == APIProtocolAnthropic {
			if _, ok := parsed["content"]; !ok {
				return volcanoProbeTransient, resp.StatusCode
			}
		} else {
			if _, ok := parsed["choices"]; !ok {
				return volcanoProbeTransient, resp.StatusCode
			}
		}
		return volcanoProbeOK, resp.StatusCode
	default:
		return volcanoProbeTransient, resp.StatusCode
	}
}

// fetchVolcanoPlanSupportedModels 是火山订阅号模型同步入口：跳过 /models 目录，
// 用官方候选 + 对真实对话端点增量探活，HTTP 200 且响应体有效才算可用并列入模型列表。
//
// 只探测当前 model_mapping 中不存在的候选（增量）；已有白名单模型一律保留、不删减。
// 全候选成功时返回可用模型集合（含白名单）；部分转瞬失败带 partial warning；
// 401/403 终止返回凭证错误；所有候选均失败时返回明确 UpstreamModelSyncError。
// 同步接口本身不写 model_mapping，只返回结果供上层消费。
func (s *AccountTestService) fetchVolcanoPlanSupportedModels(ctx context.Context, account *Account) ([]string, []UpstreamModelSyncWarning, error) {
	profile, ok := parseVolcanoPlanProfile(account.GetBaseURL())
	if !ok {
		return nil, nil, newUpstreamModelSyncConfigError("Volcano account base_url must target a Coding/Agent Plan endpoint", nil)
	}

	candidates, err := loadVolcanoPlanCandidates(volcanoPlanModelsPath(s.cfg), profile.Kind)
	if err != nil {
		return nil, nil, newUpstreamModelSyncConfigError("Failed to load volcano plan candidate models", err)
	}

	existing := whitelistedAccountModels(account)
	known := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		known[m] = struct{}{}
	}
	// 已存在的映射上游目标值（请求 alias -> 火山模型 v）也视为已收录：v 已在账号
	// 能力内，不因 key 与候选名不同而重复探活，保证增量语义准确。
	for _, upstream := range account.GetModelMapping() {
		if trimmed := strings.TrimSpace(upstream); trimmed != "" {
			if _, dup := known[trimmed]; !dup {
				known[trimmed] = struct{}{}
			}
		}
	}

	// 增量：只探测候选清单中尚未收录的模型（命中 key 或已有 mapping 目标值均跳过）。
	toProbe := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if _, ok := known[name]; !ok {
			toProbe = append(toProbe, name)
		}
	}

	// 受限并发探活：最多 volcanoProbeMaxConcurrent 个并发，避免 ~13 个候选 × 15s
	// 超时串行拖垮管理接口/反代超时；每个请求仍有独立 ctx 超时。
	results := make([]volcanoProbeResult, len(toProbe))
	sem := make(chan struct{}, volcanoProbeMaxConcurrent)
	var wg sync.WaitGroup
	for i, model := range toProbe {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			sem <- struct{}{}
			outcome, statusCode := s.probeVolcanoModel(ctx, account, profile, m)
			<-sem
			results[i] = volcanoProbeResult{model: m, outcome: outcome, statusCode: statusCode}
		}(i, model)
	}
	wg.Wait()

	usable := make([]string, 0, len(toProbe))
	okCount, unavailableCount, transientCount := 0, 0, 0
	for _, r := range results {
		switch r.outcome {
		case volcanoProbeAuth:
			return nil, nil, newUpstreamModelSyncCredentialError(
				fmt.Sprintf("Volcano credential authentication failed (HTTP %d)", r.statusCode), r.statusCode, nil,
			)
		case volcanoProbeOK:
			okCount++
			usable = append(usable, r.model)
		case volcanoProbeUnavailable:
			unavailableCount++
		case volcanoProbeTransient:
			transientCount++
			slog.Warn("volcano_model_probe_transient", "model", r.model, "status", r.statusCode)
		}
	}

	// 所有被探测候选均未成功：明确失败，不静默当模型不存在。
	if len(toProbe) > 0 && okCount == 0 {
		if transientCount > 0 {
			return nil, nil, newUpstreamModelSyncUpstreamError(
				"Volcano plan model probe failed due to transient errors (timed out / rate limited / server error); no model confirmed available", nil,
			)
		}
		return nil, nil, newUpstreamModelSyncUpstreamError(
			fmt.Sprintf("Volcano plan returned no supported models (%d candidates unavailable)", unavailableCount), nil,
		)
	}

	// 结果 = 白名单（保留）∪ 新探通模型，去重排序。
	result := make([]string, 0, len(known)+len(usable))
	result = append(result, existing...)
	result = append(result, usable...)
	result = dedupeAndSortModelIDs(result)

	var warnings []UpstreamModelSyncWarning
	if transientCount > 0 {
		warnings = append(warnings, UpstreamModelSyncWarning{
			Code:    VolcanoModelSyncPartialCode,
			Message: fmt.Sprintf("some volcano plan models could not be confirmed (transient failures); %d models available", okCount),
		})
	}
	return result, warnings, nil
}