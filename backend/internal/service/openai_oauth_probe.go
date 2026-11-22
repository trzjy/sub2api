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
//  3. 从账号 model_mapping 里挑一个上游真实存在的模型：账号 model_mapping 是
//     本账号「可服务模型 → 上游真名」的唯一 SSOT（见 Account.IsModelSupported），映射的
//     值即上游真名；逐条经账号级 model_mapping 解析出站名（与 cursor 分支
//     cursorProbeOutboundModel 同口径：命中且非空才用，解析不到则跳过该候选）。
//     图片/媒体模型（isOpenAIImageGenerationModel）不能做文本探针，直接排除。
//     候选**只接受 `gpt-` 前缀的通用文本模型**（在候选收集时过滤）：映射目标本身
//     是账号配置，不能独立证明它仍在上游实时目录里；已下架/非文本的取值
//     （含 codex-auto-review 一类非 `gpt-` 命名的账号自定义模型）拿去做文本探针
//     只会恒失败或压根不是文本模型 ⇒ 宁可视为无候选，也不探一个证明不了可服务的模型。
//     命中多个 `gpt-` 候选时按字典序取首个以保证可复现（与 selectResponsesProbeModel 同口径）。
//  4. 仍无候选 → 返回空串，由调用方按既有 degraded 语义处理（"no model to probe"
//     ⇒ 到期恢复，不伪造失败）。
//
// **绝不取账号 extra.upstream_model_metadata 快照作模型来源**：该快照由
// SyncUpstreamModelCatalog 写回，其内容 = 上游清单 ∪ 管理员配置的映射目标
// （configuredUpstreamModelsForCapabilitySync）∪ models.dev 补齐的元数据条目
// ∪ 本轮未拿到完整元数据时保留的旧快照 —— **不等于上游实时目录**。仓库既有用例
// 明确断言「上游清单里没有的模型可以出现在落库快照」
// （backend/internal/service/upstream_models_test.go:1165 附近）。
// 于是「映射已移除、快照仍留 gpt-5.4 与 gpt-5.6-luna」时，枚举快照键排序取首个
// `gpt-` 候选必定选中 `gpt-5.4`（字典序小于 `gpt-5.6-luna`）⇒ 上游恒 400
// model_not_found（生产实证）⇒ 账号恒判不健康。故快照只作「能力描述」目录，
// 不得作「可服务模型」目录；探针模型只能来自账号自己的 model_mapping。
//
// **绝不回落占位默认模型（= "gpt-5.4"）**：该模型在 ChatGPT 订阅号的
// Codex 上游不存在（生产实证：400 model_not_found "The 'gpt-5.4' model is not
// supported when using Codex with a ChatGPT account."），用它探测恒失败 ⇒
// 账号恒判不健康 ⇒ 永远卡在"探测中，确认恢复后自动恢复调度"。
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

	// 3) 账号 model_mapping 解析出的上游真名（只接受 gpt- 前缀的通用文本模型）。
	if model := pickOpenAIOAuthProbeCandidate(openAIOAuthProbeMappingCandidates(acc)); model != "" {
		return model
	}
	// 4) 无模型可探：返回空串，交调用方走既有 degraded 语义。
	//    绝不使用 extra.upstream_model_metadata 快照兜底（快照 ≠ 上游实时目录，
	//    可能含已下架模型，见本函数注释）。
	return ""
}

// openAIOAuthProbeMappingCandidates 收集账号 model_mapping 解析出的上游真名候选
// （去重 + 排除不可探的模型）。
//
// 只收 `gpt-` 前缀的通用文本模型：映射目标是账号配置，不能独立证明它仍在上游
// 实时目录；非 `gpt-` 命名的取值（codex-auto-review 一类账号自定义模型、以及
// 任何含 `*` 的通配符）不作为文本探针目标。
func openAIOAuthProbeMappingCandidates(acc *Account) []string {
	mapping := acc.GetModelMapping()
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
		if !strings.HasPrefix(upstream, "gpt-") {
			continue
		}
		if _, dup := seen[upstream]; dup {
			continue
		}
		seen[upstream] = struct{}{}
		candidates = append(candidates, upstream)
	}
	return candidates
}

// pickOpenAIOAuthProbeCandidate 在候选里挑一个通用 GPT 文本模型：优先 gpt- 前缀，
// 同类按字典序取首个以保证可复现（与 selectResponsesProbeModel 同口径）。
//
// 候选收集处（openAIOAuthProbeMappingCandidates）已只放行 `gpt-` 前缀候选，故此处
// 无 gpt- 候选时返回空串（不返回任意非 gpt- 取值）。
func pickOpenAIOAuthProbeCandidate(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	sort.Strings(candidates)
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, "gpt-") {
			return candidate
		}
	}
	return ""
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
//
// 额外排除「空 completed 帧」（上游静默拒绝）：response.completed / response.done
// 帧若只有 id/status、无 output、无 usage、无 error，站内转发侧判定为上游静默拒绝
// 并明确触发 failover（issue #5009，见
// openai_gateway_response_handling.go 的 openAIResponsesCompletedEventIsEmpty
// 调用点）。探针若把这种流当成健康，一次探测即可清除额度/传输分量停调、或计入
// 熔断的「连续 2 次成功」⇒ 误清不该解除的停调。故这里复用站内同一判定函数
// openAIResponsesCompletedEventIsEmpty（只读复用，不改转发侧任何一行）排除它：
// 终态帧满足「无 usage、无 error、response.output 不是非空数组」即为空完成帧，
// 不计为健康终态。有真实输出或真实 usage 的终态帧仍判健康。
//
// **累计 usage 必须一并传入**（openAIResponsesCompletedEventIsEmpty 的文档契约）：
// 该判定的注释明确写着 usage 可能出现在更早的事件里，故它会一并看累计 usage；
// 转发侧调用点传的就是 SSE 帧上累计出来的 usage（parseSSEUsageBytesWithType）。
// 这里传 nil 会漏掉「有效 usage 在前序帧、终态帧自身无 usage/output」的形态 ——
// 那种流确实产生了输出（不是静默拒绝），却被判成空完成帧 ⇒ 账号继续停调，
// 修复达不到「自动回池」的目的。故探针按转发侧同一口径累计 usage 再判定。
//
// 与转发侧的差异（有意保守）：转发侧在触发 failover 前还要求「此前无语义输出、
// 客户端输出未开始」（因为客户端已收到部分输出时不能再 failover）；探针没有客户端
// 上下文、也只判定「这次探测是否证明账号可用」，故一律把空完成帧判为不健康 ——
// 宁可多探一次（保持停调），也不误清不该解除的停调。
func probeOpenAIOAuthResponseOK(status int, body []byte) bool {
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return false
	}

	hasError := false
	emptyCompleted := false
	// 累计 usage：与转发侧同口径（parseSSEUsageBytesWithType 是站内 SSOT：
	// 非终态帧 merge、终态帧覆盖），供 openAIResponsesCompletedEventIsEmpty 判定。
	var accUsage OpenAIUsage
	// parseSSEUsageBytesWithType 不读取 OpenAIGatewayService 的任何字段，
	// 零值实例即可承载该口径（站内既有先例：BuildOpenAIAccountSchedulerScoreSnapshot）。
	usageParser := &OpenAIGatewayService{}
	forEachOpenAISSEFrame(string(body), func(eventType string, data []byte) {
		// 先累计 usage：OpenAI 可能把 usage 放在更早的事件里，终态帧自身未必带 usage。
		usageParser.parseSSEUsageBytesWithType(data, eventType, &accUsage)
		// effectiveOpenAISSEEventType 优先取 payload 的 "type"，故
		// event:error 与 data:{"type":"error",...} 两种形态都会落到 "error"。
		if eventType == "error" || gjson.GetBytes(data, "error").Exists() {
			hasError = true
			return
		}
		if eventType != "response.completed" && eventType != "response.done" {
			return
		}
		// 空 completed 帧 = 上游静默拒绝（与转发侧 issue #5009 同一判定）。
		// 传累计 usage（不是 nil）：判定原语的契约是「累计 usage 也会被查」，
		// 前序帧已有真实 usage 时说明上游确实产生了输出，不算静默拒绝。
		if openAIResponsesCompletedEventIsEmpty(data, &accUsage) {
			emptyCompleted = true
		}
	})
	if hasError || emptyCompleted {
		return false
	}

	finalResponse, ok := extractCodexFinalResponse(string(body))
	return ok && len(bytes.TrimSpace(finalResponse)) > 0
}
