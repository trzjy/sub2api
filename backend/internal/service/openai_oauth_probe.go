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
//  3. 管理员维护的账号模型名单（credentials.model_mapping）解析出的上游真名：
//     **探针模型取自管理员维护的账号模型名单**（前端「模型限制 → 模型白名单」写的就是
//     它，键=值恒等映射）；管理员随时维护该名单，探针用它选一个可用模型做文本探测。
//     名单是本账号「可服务模型 → 上游真名」的唯一 SSOT（见 Account.IsModelSupported），
//     映射的值即上游真名；逐条经账号级 model_mapping 解析出站名（与 cursor 分支
//     cursorProbeOutboundModel 同口径：命中且非空才用，解析不到则跳过该候选）。
//     只排除**物理上不可探**的候选：含 `*` 的通配符（不是具体模型）、图片/媒体模型
//     （isOpenAIImageGenerationModel —— 图片模型无法做文本探测）。
//     **刻意不做任何基于模型名形态的猜测性过滤**：名单里非 `gpt-` 命名的模型
//     （如 codex-auto-review 一类管理员自定义模型）同样是管理员认定可服务的上游模型，
//     按名字丢弃它会把「明明有可用模型」变成「无模型可探」⇒ 账号永远回不了池。
//     命中多个候选时按字典序取首个以保证可复现（与 selectResponsesProbeModel 同口径）；
//     **要逐个试其它候选**请用 openAIOAuthProbeCandidates（探测侧用它回退）。
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
// 候选必定选中 `gpt-5.4`（字典序小于 `gpt-5.6-luna`）⇒ 上游恒 400
// model_not_found（生产实证）⇒ 账号恒判不健康。故快照只作「能力描述」目录，
// 不得作「可服务模型」目录；探针模型只能来自管理员维护的账号模型名单
// （credentials.model_mapping）。
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
	// 「选一个」语义 = 候选列表的首个（候选列表已按字典序排好 ⇒ 取首个即可复现）。
	return pickOpenAIOAuthProbeCandidate(openAIOAuthProbeCandidates(acc, explicitModel))
}

// openAIOAuthProbeFailureKind 分类一次候选模型探测失败的性质。
type openAIOAuthProbeFailureKind int

const (
	// openAIOAuthProbeFailureModelLevel 表示失败只针对被探测的那个模型（上游禁掉了它），
	// 换一个模型可能成功 ⇒ 探测侧继续试下一个候选。
	openAIOAuthProbeFailureModelLevel openAIOAuthProbeFailureKind = iota
	// openAIOAuthProbeFailureAccountLevel 表示失败与具体模型无关（凭据/限流/上游故障），
	// 换模型也没用 ⇒ 探测侧立即结束，不再试其它候选。
	openAIOAuthProbeFailureAccountLevel
)

// openAIOAuthProbeModelRejectionMarkers 是「模型级拒绝」的响应体文案特征（归一化后
// 比较：站内既有 normalizeModelNotFoundBody 把 "_"/"-" 折成空格并小写，故
// "model_not_found" 与 "model not found" 两种写法都命中）。
//
// 站内既有口径（复用，不新造）：Codex 上游对不可用模型回 400
// model_not_found / "The 'x' model is not supported when using Codex with a ChatGPT
// account."（见 openAICodexPlanGatedModelPhrase 与 model_not_found_error.go 的
// normalizeModelNotFoundBody），devin 恢复探针则按 403 forbidden / "Request not
// allowed" 判模型级拒绝（见 account_health_recovery_probe_service.go 的
// probeDevinUpstream）。
var openAIOAuthProbeModelRejectionMarkers = []string{
	"model not found",
	"not supported",
	"forbidden",
	"not allowed",
}

// classifyOpenAIOAuthProbeFailure 判定一次候选探测失败是模型级还是账号级。
//
// 模型级（换模型可能成功 ⇒ 继续试下一个候选）：
//   - HTTP 400（含 model_not_found）、403、404：上游对「这个模型」的拒绝；
//   - 响应体含模型级拒绝文案（model_not_found / not supported / forbidden /
//     not allowed）：覆盖 2xx 携带 error 帧、以及其它状态码（如 422）的形态。
//
// 账号级（换模型也没用 ⇒ 立即结束）：
//   - HTTP 401（OAuth 凭据失效）、429（限流/额度）、5xx（上游故障）；
//   - 其余未命中模型级特征的失败（含 2xx 但终态不健康：空 completed 帧、无终态帧）。
//
// 状态码优先于文案：401/429/5xx 一律账号级（凭据/额度/上游故障换个模型同样失败）。
func classifyOpenAIOAuthProbeFailure(status int, body []byte) openAIOAuthProbeFailureKind {
	switch status {
	case http.StatusUnauthorized, http.StatusTooManyRequests:
		return openAIOAuthProbeFailureAccountLevel
	}
	if status >= http.StatusInternalServerError {
		return openAIOAuthProbeFailureAccountLevel
	}
	switch status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound:
		return openAIOAuthProbeFailureModelLevel
	}
	// 其余状态码（2xx 携带 error 帧、422 等）：看响应体有没有模型级拒绝文案。
	if openAIOAuthProbeBodyHasModelRejection(body) {
		return openAIOAuthProbeFailureModelLevel
	}
	return openAIOAuthProbeFailureAccountLevel
}

// openAIOAuthProbeBodyHasModelRejection 判断响应体是否含模型级拒绝文案。
func openAIOAuthProbeBodyHasModelRejection(body []byte) bool {
	normalized := normalizeModelNotFoundBody(body)
	if normalized == "" {
		return false
	}
	for _, marker := range openAIOAuthProbeModelRejectionMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// openAIOAuthProbeCandidates 返回 OpenAI OAuth/SetupToken 订阅号恢复探针要试的**全部**
// 候选模型（顺序确定：字典序 ⇒ 可复现）。
//
// 候选来源与 selectOpenAIOAuthProbeModel 完全同源（同一份口径、同一处注释）：
//  1. explicitModel 非空 ⇒ 候选列表只有它一个（显式模型优先，不回退到名单）。
//  2. 否则熔断 state 记录的模型非空 ⇒ 候选列表只有它一个（它就是真实故障面）。
//  3. 否则 ⇒ 管理员维护的账号模型名单（credentials.model_mapping）解析出的全部候选，
//     按字典序排好（候选收集处已排除物理上不可探的取值：解析不到/为空、含 `*` 通配符、
//     图片/媒体模型；**不做任何按模型名形态的过滤**）。
//
// 探针侧（probeOpenAIOAuthDataPlane）拿它逐个试：模型级失败（上游禁掉这个模型）才换
// 下一个候选；账号级失败（凭据/限流/上游故障）立即结束。上限由
// openAIOAuthProbeMaxCandidates 约束（探测侧截断，不在此处截断：本函数的口径是
// 「管理员维护的名单解析出的全部候选」）。
//
// 非 OpenAI OAuth/SetupToken 账号返回空切片（调用方按无模型处理，不得借此放宽
// OAuth 分流谓词）。
func openAIOAuthProbeCandidates(acc *Account, explicitModel string) []string {
	if acc == nil || !isOpenAIOAuthProbeAccount(acc) {
		return nil
	}
	if m := strings.TrimSpace(explicitModel); m != "" {
		return []string{m}
	}
	if state, ok := parseHealthBreakerState(acc.TempUnschedulableReason); ok && state != nil {
		if m := strings.TrimSpace(state.Model); m != "" {
			return []string{m}
		}
	}
	// 管理员维护的账号模型名单解析出的全部候选（去重 + 排除物理上不可探的模型），
	// 按字典序排好以保证候选顺序可复现。
	candidates := openAIOAuthProbeMappingCandidates(acc)
	sort.Strings(candidates)
	return candidates
}

// openAIOAuthProbeMappingCandidates 收集管理员维护的账号模型名单
// （credentials.model_mapping）解析出的上游真名候选（去重 + 排除物理上不可探的模型）。
//
// 名单来源即管理员随时维护的「模型限制 → 模型白名单」（前端写回
// credentials.model_mapping），是管理员认定该账号可服务的模型集合；探针从里面
// 选一个做文本探测。
//
// 只排除**物理上不可探**的候选：
//   - 解析不到 / 为空（不是可发出的模型名）；
//   - 含 `*` 的通配符（不是具体模型名）；
//   - 图片/媒体模型（isOpenAIImageGenerationModel —— 图片模型无法做文本探测）。
//
// **不做任何基于模型名形态的过滤**：名单里的模型名一律可用（管理员可自定义命名，
// 例如 codex-auto-review 一类非 `gpt-` 命名的模型）。按名字形态丢弃候选会把
// 「明明有可用模型」变成「无模型可探」⇒ 账号永远回不了池。
//
// 候选收集已收敛到全站唯一事实源（probeModelCandidates），本函数只做 OAuth 账号
// 谓词过滤，避免与通用路径各挑各的。
func openAIOAuthProbeMappingCandidates(acc *Account) []string {
	if acc == nil || !isOpenAIOAuthProbeAccount(acc) {
		return nil
	}
	return probeModelUpstreams(acc)
}

// pickOpenAIOAuthProbeCandidate 在候选里挑一个模型：按字典序取首个以保证可复现
// （与 selectResponsesProbeModel 同口径）。
//
// 候选收集处（openAIOAuthProbeMappingCandidates）已排除物理上不可探的取值，
// 故此处对任何候选一视同仁，不再按模型名形态二次筛选，也不丢弃任何候选。
func pickOpenAIOAuthProbeCandidate(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	sort.Strings(candidates)
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
