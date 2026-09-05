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
	"sort"
	"strings"
	"sync"
	"time"

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
	volcanoPlanHost       = "ark.cn-beijing.volces.com"
	volcanoPlanKindAgent  = "agent"
	volcanoPlanKindCoding = "coding"

	// VolcanoModelSyncPartialCode 是火山模型同步部分成功的 warning code
	// （429/5xx/timeout 目标未探通时带上，提示用户重试而非当作模型不存在）。
	VolcanoModelSyncPartialCode = "volcano_model_sync_partial"

	volcanoProbeTimeout       = 15 * time.Second
	volcanoProbeMaxBytes      = 64 << 10
	volcanoProbeMaxConcurrent = 3
)

// volcanoProbeOutcome 单个候选模型探测结果的分类。
type volcanoProbeOutcome int

const (
	volcanoProbeAuth        volcanoProbeOutcome = iota // 401/403：凭证无效，终止（不回落不删除）
	volcanoProbeOK                                     // HTTP 200 且响应体有效：可用（confirmed）
	volcanoProbeUnavailable                            // 明确“模型不可用”：404，或 400 body 含明确 model-not-found 字样
	volcanoProbeUnverified                             // 未知 400 / 无效 200 body / 429 / 5xx / 超时 / 传输错误：未探通，不当作模型不存在
)

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
	// 生产火山云端固定为 HTTPS；接受 http 会把携带在对话探活请求里的账号 API Key
	// 明文外发，安全上不可接受（firewall 无本地自建火山场景）。一律拒绝 http。
	if parsed.Scheme != "https" {
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

// IsVolcanoPlanAccount 是对外暴露的火山订阅号判定（供 handler 使用，同 isVolcanoPlanAccount）。
func IsVolcanoPlanAccount(account *Account) bool {
	return isVolcanoPlanAccount(account)
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
		return volcanoProbeUnverified, 0
	}

	proxyURL := upstreamModelsProxyURL(account)
	resp, err := s.doUpstreamModelsRequest(req, proxyURL, account)
	if err != nil {
		// 传输/超时错误：目标未探通，不当作模型不存在。
		slog.Warn("volcano_model_probe_transport_failed", "model", model, "error", err)
		return volcanoProbeUnverified, 0
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return volcanoProbeAuth, resp.StatusCode
	case resp.StatusCode == http.StatusNotFound:
		// 404：模型级明确不存在，判为不可用（跳过，不入列表）。
		return volcanoProbeUnavailable, resp.StatusCode
	case resp.StatusCode == http.StatusBadRequest:
		// 400：拆两级——body 含明确 model-not-found 字样才算不可用；未知 400 视为未探通。
		if body, readErr := io.ReadAll(io.LimitReader(resp.Body, volcanoProbeMaxBytes)); readErr == nil {
			lower := strings.ToLower(string(body))
			if volcanoDefinitiveModelNotFound(lower) {
				return volcanoProbeUnavailable, resp.StatusCode
			}
		}
		return volcanoProbeUnverified, resp.StatusCode
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		// 429 / 5xx：未探通，不当作模型不存在。
		return volcanoProbeUnverified, resp.StatusCode
	case resp.StatusCode == http.StatusOK:
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, volcanoProbeMaxBytes))
		if readErr != nil {
			return volcanoProbeUnverified, resp.StatusCode
		}
		var parsed map[string]any
		if json.Unmarshal(body, &parsed) != nil || len(parsed) == 0 {
			// HTTP 200 但响应体无效：异常，不当作可用也不当作模型不存在。
			return volcanoProbeUnverified, resp.StatusCode
		}
		if _, hasErr := parsed["error"]; hasErr {
			// 带 error 字段的 200：按内容判不可用，否则未探通。
			if raw, ok := parsed["error"].(string); ok && volcanoDefinitiveModelNotFound(strings.ToLower(raw)) {
				return volcanoProbeUnavailable, resp.StatusCode
			}
			return volcanoProbeUnverified, resp.StatusCode
		}
		// 按协议校验响应结构，避免把非生成响应（如配额/告警）误判为模型可用：
		// anthropic /v1/messages 需 content；openai /v3/chat/completions 需 choices。
		if account.GetAPIProtocol() == APIProtocolAnthropic {
			if _, ok := parsed["content"]; !ok {
				return volcanoProbeUnverified, resp.StatusCode
			}
		} else {
			if _, ok := parsed["choices"]; !ok {
				return volcanoProbeUnverified, resp.StatusCode
			}
		}
		return volcanoProbeOK, resp.StatusCode
	default:
		return volcanoProbeUnverified, resp.StatusCode
	}
}

// volcanoDefinitiveModelNotFound 报告小写化错误体是否含明确的“模型不存在”签名。
// 只认稳定字样，避免把参数/配额类报错误判成“该模型不可用”。
var volcanoModelNotFoundSignatures = []string{
	"model not found",
	"model_not_found",
	"invalid model",
	"model doesn't exist",
	"model does not exist",
	"unknown model",
	"模型不存在",
}

func volcanoDefinitiveModelNotFound(lower string) bool {
	for _, sig := range volcanoModelNotFoundSignatures {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// volcanoPlanScanResult 是一次同步扫描（官方文档候选 + 真实端点探活）的分类汇总。
type volcanoPlanScanResult struct {
	candidates  []string // 套餐概览并集候选（含全部可见模型，探活前）
	confirmed   []string // HTTP 200 且响应体有效：可用
	unavailable []string // 明确“模型不可用”（404 / 明确 model-not-found）
	unverified  []string // 未知 400 / 无效 200 / 429 / 5xx / 超时 / 传输：未探通
	warnings    []UpstreamModelSyncWarning
	evidence    VolcanoPlanDocEvidence
}

// VolcanoPlanSyncResult 是火山订阅号同步接口对外的分类结果与差异（预览/应用共用）。
type VolcanoPlanSyncResult struct {
	Kind        string                 `json:"kind"`
	Confirmed   []string               `json:"confirmed"`    // 本账号真实端点确认可用的模型
	Unavailable []string               `json:"unavailable"`  // 明确不可用（提示，不并入）
	Unverified  []string               `json:"unverified"`   // 未能确认（提示，不并入）
	WillAdd     []string               `json:"will_add"`     // 应用后将新增到 model_mapping
	WillRemove  []string               `json:"will_remove"`  // 完全确认替换时将下架（官方下线收敛）
	FullConfirm bool                   `json:"full_confirm"` // 完全确认：托管集按候选全集替换；部分确认只取本轮探活确认集
	Applied     bool                   `json:"applied"`      // 本次是否已落库
	Evidence    VolcanoPlanDocEvidence `json:"evidence"`
}

// VolcanoPlanManagedModelExtraKey 是账号 extra 中火山订阅号系统托管模型快照的 key。
const VolcanoPlanManagedModelExtraKey = "volcano_plan_managed_models"

// VolcanoPlanManagedModels 是系统托管的火山订阅号模型集合快照（随账号 extra 持久化）。
// 用于“部分确认取本轮探活确认集、完全确认替换、绝不删人工映射”的收敛判定。
// Models=最后一次同步的官方候选并集（托管范围）；IdentityKeys=本同步器历次真正写入
// model_mapping 的身份键（key==value）的累积集合。完全确认下架只允许删除 IdentityKeys
// 中的键——用户手动添加的 identity（首轮同步前已存在于 mapping，或自行新增但不在
// IdentityKeys）永不受影响，杜绝误删人工配置（R3-2 provenance）。
type VolcanoPlanManagedModels struct {
	Models       []string               `json:"models"`
	IdentityKeys []string               `json:"identity_keys,omitempty"`
	SyncedAt     time.Time              `json:"synced_at"`
	Evidence     VolcanoPlanDocEvidence `json:"evidence"`
}

// volcanoCandidatesResult 携带某账号套餐的候选与来源证据。
type volcanoCandidatesResult struct {
	models   []string
	evidence VolcanoPlanDocEvidence
}

// volcanoPlanCandidates 读取官方文档对应套餐概览（个人版∪企业版）的可直调模型候选并集。
// 任一文档获取/解析失败均失败关闭（返回明确 error，绝不回落静态清单、绝不删既有）。
func (s *AccountTestService) volcanoPlanCandidates(ctx context.Context, profile volcanoPlanProfile) (*volcanoCandidatesResult, error) {
	kind := volcanoCoding
	if profile.Kind == volcanoPlanKindAgent {
		kind = volcanoAgent
	}
	entry, err := s.fetchVolcanoPlanReports(ctx, kind)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to read volcengine official docs", err)
	}

	added := map[string]struct{}{}
	var candidates []string
	evidence := VolcanoPlanDocEvidence{Kind: profile.Kind}
	collect := func(ref *volcanoPlanDocRef) error {
		if ref == nil {
			return nil
		}
		ids, err := extractVolcanoPlanReportModels(ref.MDContent)
		if err != nil {
			return newUpstreamModelSyncUpstreamError(
				fmt.Sprintf("failed to parse volcengine doc %d (%s)", ref.DocumentID, ref.Title), err)
		}
		evidence.URLs = append(evidence.URLs, ref.URL)
		evidence.DocumentIDs = append(evidence.DocumentIDs, ref.DocumentID)
		evidence.Titles = append(evidence.Titles, ref.Title)
		evidence.UpdatedTimes = append(evidence.UpdatedTimes, ref.UpdatedTime)
		for _, id := range ids {
			if _, dup := added[id]; dup {
				continue
			}
			added[id] = struct{}{}
			candidates = append(candidates, id)
		}
		return nil
	}
	if err := collect(entry.Personal); err != nil {
		return nil, err
	}
	if err := collect(entry.Enterprise); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, newUpstreamModelSyncUpstreamError("volcano docs: no callable model IDs across personal+business overviews", nil)
	}
	sort.Strings(candidates)
	evidence.CandidateCount = len(candidates)
	return &volcanoCandidatesResult{models: candidates, evidence: evidence}, nil
}

// resolveVolcanoPlanScan 读取官方文档→并集候选→受限并发探活，返回分类结果。
// 401/403 终止返回凭证错误；所有候选均不可确认/不可用时失败关闭（不当作“无模型”）。
func (s *AccountTestService) resolveVolcanoPlanScan(ctx context.Context, account *Account) (*volcanoPlanScanResult, error) {
	profile, ok := parseVolcanoPlanProfile(account.GetBaseURL())
	if !ok {
		return nil, newUpstreamModelSyncConfigError("Volcano account base_url must target a Coding/Agent Plan endpoint", nil)
	}
	cands, err := s.volcanoPlanCandidates(ctx, profile)
	if err != nil {
		return nil, err
	}

	// 全量探活：每次同步都对官方候选并集全量重新探活，不按 model_mapping 已收录跳过——
	// 已收录模型可能已被官方下线或失效，只有重新探活才能保证 confirmed 反映当前真实可用性。
	type probeOut struct {
		model   string
		outcome volcanoProbeOutcome
		status  int
	}
	// 受限并发探活：最多 volcanoProbeMaxConcurrent 个并发；每个请求仍有独立 ctx 超时。
	results := make([]probeOut, len(cands.models))
	sem := make(chan struct{}, volcanoProbeMaxConcurrent)
	var wg sync.WaitGroup
	for i, model := range cands.models {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			sem <- struct{}{}
			outcome, status := s.probeVolcanoModel(ctx, account, profile, m)
			<-sem
			results[i] = probeOut{model: m, outcome: outcome, status: status}
		}(i, model)
	}
	wg.Wait()

	// 分类数组一律初始化为空切片：序列化必须输出 [] 而非 null（API 响应契约，
	// 前端直接读 .length，null 会触发 TypeError）。
	result := &volcanoPlanScanResult{
		candidates:  cands.models,
		confirmed:   []string{},
		unavailable: []string{},
		unverified:  []string{},
		evidence:    cands.evidence,
	}
	unverifiedCount := 0
	for _, r := range results {
		switch r.outcome {
		case volcanoProbeAuth:
			return nil, newUpstreamModelSyncCredentialError(
				fmt.Sprintf("Volcano credential authentication failed (HTTP %d)", r.status), r.status, nil)
		case volcanoProbeOK:
			result.confirmed = append(result.confirmed, r.model)
		case volcanoProbeUnavailable:
			result.unavailable = append(result.unavailable, r.model)
		case volcanoProbeUnverified:
			unverifiedCount++
			result.unverified = append(result.unverified, r.model)
			slog.Warn("volcano_model_probe_unverified", "model", r.model, "status", r.status)
		}
	}

	// 应对本轮完整候选集合全部明确不可用的情况：明确失败，不静默当作“模型不存在/无模型”。
	if len(result.confirmed) == 0 && unverifiedCount == 0 && len(result.unavailable) > 0 {
		return nil, newUpstreamModelSyncUpstreamError(
			fmt.Sprintf("Volcano account returned no supported models (%d candidates unavailable)", len(result.unavailable)), nil)
	}
	// 全部候选均未探通（未知 400/429/5xx/超时）且无已确认者：失败关闭。
	if len(result.confirmed) == 0 && unverifiedCount > 0 {
		return nil, newUpstreamModelSyncUpstreamError(
			"No volcano model could be confirmed (probes failed or timed out); not treating as empty model list", nil)
	}
	if unverifiedCount > 0 {
		result.warnings = append(result.warnings, UpstreamModelSyncWarning{
			Code:    VolcanoModelSyncPartialCode,
			Message: fmt.Sprintf("some volcano plan models could not be confirmed (unverified/transient failures); %d models confirmed", len(result.confirmed)),
		})
	}
	return result, nil
}

// fetchVolcanoPlanSupportedModels 是通用同步路径（SyncUpstreamModelCatalog → fetchUpstreamModelList）
// 对火山订阅号的兼容入口：复用同一扫描，只返回本轮真实探活确认的模型（全量语义，
// 不再并入 model_mapping 旧模型做差量保留；不写 model_mapping）。
func (s *AccountTestService) fetchVolcanoPlanSupportedModels(ctx context.Context, account *Account) ([]string, []UpstreamModelSyncWarning, error) {
	scan, err := s.resolveVolcanoPlanScan(ctx, account)
	if err != nil {
		return nil, nil, err
	}
	return dedupeAndSortModelIDs(scan.confirmed), scan.warnings, nil
}

// SyncVolcanoPlanModels 是火山订阅号专用同步服务入口。
// apply=false 只返回预演（分类 + 差异，不动库）；apply=true 落库系统托管快照并调整
// model_mapping：
//
//   - 完全确认（全部候选探通、无 unverified/unavailable 阻断）→ 托管集合替换为官方套餐
//     候选并集，官方已下线的旧托管模型从 model_mapping 移除（收敛）；
//   - 部分确认（存在 unverified/unavailable）→ 托管集合只取本轮探活确认的 confirmed 模型，
//     不再并入旧托管（全量语义：旧模型不等候本轮探活结果，官方下线/未探通即退出托管集）。
//     mapping 键的物理删除仍只由完全确认 + IdentityKeys 收敛驱动，部分确认不删除既有键，
//     杜绝瞬时 429/超时导致误删可用模型。
//   - 绝不删除人工映射：model_mapping 中非托管键（含 alias 键）一律保留，人工键永不动。
//
// Agent 与 Coding 各自独立：仅对账号 base_url 命中的单一套餐维护一套托管集合。
func (s *AccountTestService) SyncVolcanoPlanModels(ctx context.Context, account *Account, apply bool, allowedRemovals []string) (*VolcanoPlanSyncResult, error) {
	scan, err := s.resolveVolcanoPlanScan(ctx, account)
	if err != nil {
		return nil, err
	}

	// 可删身份键集 = 上一快照中由本同步器写入的 identity 键（非用户手动 identity）。
	oldIdentityKeys := loadVolcanoManagedModelIdentityKeys(account)
	// 完全确认 = 无 unverified/unavailable 阻断。全量探活下 confirmed 为空只可能是全部
	// 候选失败，而该情况已由 resolveVolcanoPlanScan fail-closed 返回 error；到达这里时
	// confirmed 即本轮真实探通的官方候选。
	fullConfirm := len(scan.unverified) == 0 && len(scan.unavailable) == 0
	// 全量更新：托管集合每轮以本次官方候选 + 真实探活结果重建，不按旧托管做差量保留。
	var managed []string
	if fullConfirm {
		// 官方套餐候选即权威集：全量探活后候选全集已重新确认，托管集合直接替换为候选并集。
		managed = append(managed, scan.candidates...)
	} else {
		// 部分确认：只取本轮探活确认的模型；官方下线/未探通的旧模型不再通过旧托管保留。
		managed = append(managed, scan.confirmed...)
	}
	managed = dedupeAndSortModelIDs(managed)

	// 差异计算。
	managedSet := map[string]struct{}{}
	for _, m := range managed {
		managedSet[m] = struct{}{}
	}
	current := account.GetModelMapping()
	curSet := map[string]struct{}{}
	curVals := map[string]struct{}{}
	for k, v := range current {
		curSet[strings.TrimSpace(k)] = struct{}{}
		curVals[strings.TrimSpace(v)] = struct{}{}
	}
	// 差异数组初始化为空切片：序列化必须输出 [] 而非 null（API 响应契约）。
	willAdd := []string{}
	for _, m := range managed {
		if _, ok := curSet[m]; ok {
			continue
		}
		if _, ok := curVals[m]; ok {
			continue // 已由某 alias 覆盖，无需重复身份键
		}
		willAdd = append(willAdd, m)
	}
	willRemove := []string{}
	if fullConfirm {
		// 只允许删除"上一快照中由本同步器写入的 identity 键"中官方已下架的：用户手动
		// identity（不在 IdentityKeys，例如首轮同步前就已存在于 mapping 的用户白名单）
		// 永不进入可删集，杜绝误删人工配置（R3-2）。
		for m := range oldIdentityKeys {
			if _, keep := managedSet[m]; keep {
				continue
			}
			if _, ok := current[m]; ok {
				willRemove = append(willRemove, m)
			}
		}
		sort.Strings(willRemove)
	}

	result := &VolcanoPlanSyncResult{
		Kind:        scan.evidence.Kind,
		Confirmed:   scan.confirmed,
		Unavailable: scan.unavailable,
		Unverified:  scan.unverified,
		WillAdd:     willAdd,
		WillRemove:  willRemove,
		FullConfirm: fullConfirm,
		Evidence:    scan.evidence,
	}
	if !apply {
		return result, nil
	}

	// R3-1：apply 的下架必须受用户已确认的 preview 约束。preview 是部分确认（有
	// unverified/unavailable）时 will_remove 为空；若 apply 重扫升级为完全确认、产生
	// preview 未提示的下架，绝不得静默落库——拒绝并让前端提示重新预览。用户在
	// preview 弹窗里确认的只是 preview 展示的差异。
	if !removalsCovered(willRemove, allowedRemovals) {
		unexpected := outsideRemovals(willRemove, allowedRemovals)
		return nil, newUpstreamModelSyncConfigError(
			fmt.Sprintf("volcano sync apply would remove models not confirmed in preview: %v; re-run preview and confirm", unexpected), nil)
	}

	newMapping := rebuildVolcanoModelMapping(account, oldIdentityKeys, managed, fullConfirm, current)

	// 先落 model_mapping、后落托管快照：若映射写失败，快照保持旧值（含旧托管 identity
	// 键），重试时 will_remove 仍能把官方下架模型可靠收敛；若快照写失败，映射已收敛，
	// 重试 fullConfirm 会重新推导候选集并幂等重写快照。两步任一失败都不会丢旧托管状态。
	account.Credentials = shallowCopyMap(account.Credentials)
	account.Credentials["model_mapping"] = newMapping
	if err := persistAccountCredentials(ctx, s.accountRepo, account, account.Credentials); err != nil {
		return nil, newUpstreamModelSyncInternalError("Failed to persist volcano model mapping", err)
	}
	// 累计本轮"由本同步器落地为 identity 键"的新托管键：预先在 mapping 中已以身份键存在
	// 的（用户手动/pre-existing，current 又是 identity）不入托管集，后续官方下线也不得删。
	identityKeys := make(map[string]struct{}, len(oldIdentityKeys))
	for k := range oldIdentityKeys {
		identityKeys[k] = struct{}{}
	}
	for m := range managedSet {
		v, ok := newMapping[m]
		if !ok {
			continue
		}
		val, isStr := v.(string)
		if !isStr || strings.TrimSpace(val) != m {
			continue
		}
		if cur, existed := current[m]; existed && strings.TrimSpace(cur) == m {
			continue // 本轮前已是 identity 的既有键→视为用户手动，不入托管
		}
		identityKeys[m] = struct{}{}
	}
	identityKeySlice := make([]string, 0, len(identityKeys))
	for k := range identityKeys {
		identityKeySlice = append(identityKeySlice, k)
	}
	sort.Strings(identityKeySlice)

	// 托管快照是派生缓存，model_mapping 才是生效事实源：映射已提交并 refresh 调度，
	// 快照写失败不推翻 Applied，否则出现“后端已生效、前端却报错误不更新”的不一致窗口
	// （P2-4）。快照留待下次同步幂等补写，不影响本次已确认同步的一致性。
	if err := s.persistVolcanoManagedSnapshot(ctx, account, &VolcanoPlanManagedModels{
		Models:       managed,
		IdentityKeys: identityKeySlice,
		SyncedAt:     time.Now().UTC(),
		Evidence:     scan.evidence,
	}); err != nil {
		slog.Warn("volcano_managed_snapshot_persist_failed", "account", account.ID, "err", err)
	}

	result.Applied = true
	return result, nil
}

// removalsCovered 报告 willRemove ⊆ allowedRemovals。
func removalsCovered(willRemove, allowedRemovals []string) bool {
	allowed := make(map[string]struct{}, len(allowedRemovals))
	for _, r := range allowedRemovals {
		allowed[strings.TrimSpace(r)] = struct{}{}
	}
	for _, r := range willRemove {
		if _, ok := allowed[strings.TrimSpace(r)]; !ok {
			return false
		}
	}
	return true
}

// outsideRemovals 返回 willRemove 中不在 allowedRemovals 里的项（已排序）。
func outsideRemovals(willRemove, allowedRemovals []string) []string {
	allowed := make(map[string]struct{}, len(allowedRemovals))
	for _, r := range allowedRemovals {
		allowed[strings.TrimSpace(r)] = struct{}{}
	}
	var out []string
	for _, r := range willRemove {
		if _, ok := allowed[strings.TrimSpace(r)]; !ok {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// loadVolcanoManagedModelIdentityKeys 读取快照中"本同步器历次真正写入 model_mapping 的
// 身份键"累积集。完全确认下架只允许删除这些键；用户手动 identity（不在 IdentityKeys）
// 永不受影响。见 VolcanoPlanManagedModels.IdentityKeys。
func loadVolcanoManagedModelIdentityKeys(account *Account) map[string]struct{} {
	snap := account.GetVolcanoPlanManagedModels()
	if snap == nil {
		return nil
	}
	out := make(map[string]struct{}, len(snap.IdentityKeys))
	for _, k := range snap.IdentityKeys {
		out[strings.TrimSpace(k)] = struct{}{}
	}
	return out
}

// persistVolcanoManagedSnapshot 写入账号 extra 的系统托管模型快照。
func (s *AccountTestService) persistVolcanoManagedSnapshot(ctx context.Context, account *Account, snap *VolcanoPlanManagedModels) error {
	if s.accountRepo == nil {
		return nil
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra[VolcanoPlanManagedModelExtraKey] = snap
	return s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		VolcanoPlanManagedModelExtraKey: snap,
	})
}

// rebuildVolcanoModelMapping 计算应用后的 model_mapping。
// 人工键（含 alias 键与非托管身份键）一律保留；完全确认时才移除"上一快照中由本同步器
// 写入的 identity 键"（key==value 且值已不在新托管集合），人工 alias 键与用户手动
// identity 键永不动（R3-2）。
func rebuildVolcanoModelMapping(account *Account, oldIdentityKeys map[string]struct{}, managed []string, fullConfirm bool, current map[string]string) map[string]any {
	raw := map[string]any{}
	for k, v := range current {
		raw[k] = v
	}

	managedSet := map[string]struct{}{}
	for _, m := range managed {
		managedSet[strings.TrimSpace(m)] = struct{}{}
	}

	if fullConfirm {
		for m := range oldIdentityKeys {
			if _, keep := managedSet[m]; keep {
				continue
			}
			if v, ok := current[m]; ok && strings.TrimSpace(v) == m {
				delete(raw, m) // 本同步器落地的旧托管身份键，已官方下线，收敛删除
			}
		}
	}

	inval := map[string]struct{}{}
	for _, v := range raw {
		if s, ok := v.(string); ok {
			inval[strings.TrimSpace(s)] = struct{}{}
		}
	}
	for m := range managedSet {
		if _, ok := raw[m]; ok {
			continue
		}
		if _, ok := inval[m]; ok {
			continue // 已由某 alias 覆盖
		}
		raw[m] = m
	}
	return raw
}

// GetVolcanoPlanManagedModels 读取账号 extra 中的火山订阅号系统托管模型快照。
func (a *Account) GetVolcanoPlanManagedModels() *VolcanoPlanManagedModels {
	if a == nil || a.Extra == nil {
		return nil
	}
	raw, ok := a.Extra[VolcanoPlanManagedModelExtraKey]
	if !ok || raw == nil {
		return nil
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var snap VolcanoPlanManagedModels
	if err := json.Unmarshal(body, &snap); err != nil || len(snap.Models) == 0 {
		return nil
	}
	return &snap
}
