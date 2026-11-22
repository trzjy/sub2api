package service

// OpenAI OAuth 订阅号恢复探测的无 gin 依赖原语。
//
// 本文件只负责「构造请求」与「判定响应」两件可独立单测的事，探测的触发、
// 传输（httpUpstream.DoWithTLS + 代理/TLS）与停调状态回写由恢复探针服务负责。
// 所有协议细节均复用站内现成原语，不在此新造协议逻辑。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/tidwall/gjson"
)

// isOpenAIOAuthProbeAccount 判定账号是否为恢复探针应走 OAuth 凭据路径的账号。
//
// 刻意收窄为 IsOpenAIOAuthLike（IsOpenAI && type∈{oauth,setup_token}），
// 不用平台无关的 UsesOpenAICodexProtocol——后者会误命中其它平台的 OAuth 号。
func isOpenAIOAuthProbeAccount(acc *Account) bool {
	return acc.IsOpenAIOAuthLike()
}

// selectOpenAIOAuthProbeModel 选出 OpenAI OAuth/SetupToken 订阅号恢复探针要用的上游模型名。
//
// 取模型顺序（与 probeOpenAIOAuthDataPlane 的调用顺序一致）：
//  1. explicitModel 非空（调用方显式传入的模型，如模型级限流恢复通道传的 scope 模型）
//     → 直接采用，不做任何回落。
//  2. 熔断 state 记录的模型（parseHealthBreakerState(acc.TempUnschedulableReason).Model）
//     非空 → 采用（它就是触发本次停调的上游模型，探它才是真实故障面）。
//  3. 以上都空 → 从账号 model_mapping 里挑一个上游真实存在的模型：账号 model_mapping 是
//     本账号「可服务模型 → 上游真名」的唯一 SSOT（见 Account.IsModelSupported），映射的
//     值即上游真名；逐条经账号级 model_mapping 解析出站名（与 cursor 分支
//     cursorProbeOutboundModel 同口径：命中且非空才用，解析不到则跳过该候选）。
//     图片/媒体模型（isOpenAIImageGenerationModel）不能做文本探针，直接排除；候选里
//     优先挑通用 GPT 文本模型（gpt- 前缀），同类按字典序取首个以保证可复现
//     （与 selectResponsesProbeModel 同口径）。
//  4. 账号没有 model_mapping（len(acc.GetModelMapping()) == 0）→ 回落 openai.DefaultTestModel
//     （保持对无 mapping OAuth 号的既有行为，不扩大影响面）。mapping 非空却没有可用候选
//     时返回空串——由调用方按既有 degraded（到期恢复）处理，绝不拿上游不支持的占位模型
//     去探：探针恒 400 model-not-found ⇒ 账号永不解封（本次修复要消除的正反馈）。
//
// 返回值恒为上游模型名（空串表示无可用模型）；出站归一（normalizeOpenAIModelForUpstream）
// 不在本函数内，仍由 buildOpenAIOAuthProbeRequest 逐字保留执行。
//
// 非 OpenAI OAuth/SetupToken 账号（isOpenAIOAuthProbeAccount 为假）返回空串：调用方按
// 无模型处理，不得借此放宽 OAuth 分流谓词。
func selectOpenAIOAuthProbeModel(acc *Account, explicitModel string) string {
	if acc == nil || !isOpenAIOAuthProbeAccount(acc) {
		return ""
	}
	if m := strings.TrimSpace(explicitModel); m != "" {
		return m
	}
	if state, ok := parseHealthBreakerState(acc.TempUnschedulableReason); ok && state != nil {
		if m := strings.TrimSpace(state.Model); m != "" {
			return m
		}
	}

	mapping := acc.GetModelMapping()
	if len(mapping) == 0 {
		// 无 model_mapping 的 OAuth 号：沿用既有回落（不扩大影响面）。
		return strings.TrimSpace(openai.DefaultTestModel)
	}

	// 候选 = 账号 model_mapping 解析出的上游真名（去重 + 排除不可探的模型）。
	candidates := make([]string, 0, len(mapping))
	seen := make(map[string]struct{}, len(mapping))
	for key := range mapping {
		upstream, matched := acc.ResolveMappedModel(key)
		upstream = strings.TrimSpace(upstream)
		if !matched || upstream == "" || strings.Contains(upstream, "*") {
			continue
		}
		if isOpenAIImageGenerationModel(upstream) {
			continue
		}
		if _, dup := seen[upstream]; dup {
			continue
		}
		seen[upstream] = struct{}{}
		candidates = append(candidates, upstream)
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Strings(candidates)
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, "gpt-") {
			return candidate
		}
	}
	return candidates[0]
}

// buildOpenAIOAuthProbeRequest 为 OpenAI OAuth/SetupToken 订阅号构造一次最小
// Codex /responses 流式探测请求（返回 *http.Request 供调用方经 DoWithTLS 发出）。
//
// 影子账号用窄接口 probeRepo.GetByID 解析母账号凭据（镜像
// probeCodeBuddyShadowUpstream 的手工解影子方式），不依赖全量 AccountRepository
// 的 resolveCredentialAccount。端点取转发侧同包常量 chatgptCodexURL（唯一 SSOT）。
//
// 端点是站内固定常量、不接受用户可控 base_url，无需 cnValidateProbeURL 的
// allowlist 校验，validateURL 同理省略。
//
// Agent Identity 号本轮不纳入，返回 error 走既有降级（到期恢复），不伪造探测结果。
func buildOpenAIOAuthProbeRequest(
	ctx context.Context,
	probeRepo accountHealthProbeRepository,
	acc *Account,
	model string,
) (*http.Request, error) {
	if acc == nil {
		return nil, errors.New("oauth probe account is nil")
	}
	if !isOpenAIOAuthProbeAccount(acc) {
		return nil, fmt.Errorf("oauth probe account %d is not an openai oauth-like account", acc.ID)
	}

	// 影子 → 母账号凭据解析：出站身份取母账号，校验语义与
	// probeCodeBuddyShadowUpstream 一致（fail-closed）。
	credAccount := acc
	if acc.ParentAccountID != nil {
		if probeRepo == nil {
			return nil, errors.New("oauth probe repo not configured")
		}
		parent, err := probeRepo.GetByID(ctx, *acc.ParentAccountID)
		if err != nil {
			return nil, fmt.Errorf("resolve oauth shadow parent: %w", err)
		}
		if parent == nil || parent.IsShadow() {
			return nil, fmt.Errorf("oauth shadow parent %d missing or itself a shadow", *acc.ParentAccountID)
		}
		if !(parent.IsOpenAI() && (parent.Type == AccountTypeOAuth || parent.Type == AccountTypeSetupToken)) {
			return nil, fmt.Errorf("oauth shadow parent %d is not an openai oauth/setup_token account", parent.ID)
		}
		credAccount = parent
	}

	if credAccount.IsOpenAIAgentIdentity() {
		return nil, fmt.Errorf("oauth probe account %d is an agent identity account (not supported this round)", credAccount.ID)
	}
	accessToken := credAccount.GetOpenAIAccessToken()
	if accessToken == "" {
		return nil, fmt.Errorf("oauth probe account %d is missing access_token credential", credAccount.ID)
	}

	// payload 与真实测试/转发同构：流式 Responses，OAuth 必须 store:false。
	upstreamModel := normalizeOpenAIModelForUpstream(credAccount, model)
	payload := createOpenAITestPayload(upstreamModel, true)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal oauth probe payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("create oauth probe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	// 与真实 Codex 出站同口径（参照 testOpenAIAccountConnection 的 OAuth 分支）：
	// 显式补 Host/accept/OpenAI-Beta，再套身份收口与账号级头覆写。
	req.Host = "chatgpt.com"
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	canonical := resolveCodexOutboundIdentity("")
	req.Header.Set("Originator", canonical.originator)
	if customUA := strings.TrimSpace(credAccount.GetOpenAIUserAgent()); customUA != "" {
		req.Header.Set("User-Agent", customUA)
	} else {
		req.Header.Set("User-Agent", canonical.userAgent)
	}
	setOpenAIChatGPTAccountHeaders(req.Header, credAccount)
	enforceCodexIdentityHeadersWithUA(req.Header, credAccount.GetOpenAIUserAgent())
	credAccount.ApplyHeaderOverrides(req.Header)

	return req, nil
}

// probeOpenAIOAuthResponseOK 判定 Codex /responses 流式探测响应是否代表账号健康。
//
// 条件（缺一即 false，保持停调）：HTTP 2xx + SSE 中出现非空终态响应对象
// （extractCodexFinalResponse 复用站内解析，只认 response.done/completed）+
// 全程无 error 帧。刻意不用 evaluateProbeDataPlaneResponse——它要求整体 JSON，
// 多帧 SSE 必然失败。
func probeOpenAIOAuthResponseOK(status int, body []byte) bool {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return false
	}

	hasError := false
	forEachOpenAISSEFrame(string(body), func(eventType string, data []byte) {
		// effectiveOpenAISSEEventType 优先取 payload 的 "type"，故
		// event:error 与 data:{"type":"error",...} 两种形态都会落到 "error"。
		if eventType == "error" || gjson.GetBytes(data, "error").Exists() {
			hasError = true
		}
	})
	if hasError {
		return false
	}

	finalResponse, ok := extractCodexFinalResponse(string(body))
	return ok && len(bytes.TrimSpace(finalResponse)) > 0
}
