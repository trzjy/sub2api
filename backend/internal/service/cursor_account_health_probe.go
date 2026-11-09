package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

// cursorAccountHealthBreakerMarker 是 cursor Connect 会话型账号健康探针停调写入
// TempUnschedState.MatchedKeyword 的固定类别标记（CURSOR-PROBE-001-A1），与既有
// openAIAPIKeyHealthBreakerReason / PoolMode401EscalationMarker /
// oauth401RecoverableMarker 同型。全仓单一定义（SSOT：字面量禁止硬编码到多处），
// 供后续 CARD-C 类恢复探针按 marker 识别并纳入可探测恢复范围。
const cursorAccountHealthBreakerMarker = "cursor_account_health_breaker"

// cursorAccountHealthProbeMinBudget 是 cursor 原生 AgentRun 探测的最小输出预算下限。
// 思考型模型在 16 token 预算下探不出正文（生产实证：账号 406 连探 1.6 万次全败、
// 账号 411 实测 16/24/32 → 502 upstream_empty_result，48~256 → 200），故探针侧预算
// 须 ≥256。该值由 A2 卡实际传入本卡封装，A3 卡改造 ProbeAgentRun 增加预算参数后统一消费。
const cursorAccountHealthProbeMinBudget = 256

// BuildCursorAccountHealthBreakerState 构造 cursor Connect 会话型账号健康探针触发后的
// 可解析停调状态（*TempUnschedState），字段口径与 pool401EscalationState /
// buildOAuth401RecoverableReason 同源：
//   - MatchedKeyword = cursorAccountHealthBreakerMarker
//   - Tier          = HealthBreakerTierTrip
//   - RuleIndex     = -1
//   - UntilUnix     = 9999 哨兵（tempUnschedIndefiniteUntil.Unix()）
//   - TriggeredAtUnix = now
//   - StatusCode    = 上游状态码
//   - ErrorMessage  = 脱敏后的上游错误摘要（由调用方传入，本函数不负责脱敏/序列化）
//   - Model         = 触发模型（出站模型名）
//
// 本函数只提供状态构造，不负责 JSON 序列化（沿用 TempUnschedState 既有 json.Marshal
// 用法，由调用方序列化后写入），也不触碰停调写入（既有 accountRepo.SetTempUnschedulable
// 在其它卡完成）。now 为零值时回落 time.Now()。
func BuildCursorAccountHealthBreakerState(
	now time.Time,
	statusCode int,
	errorMessage string,
	model string,
) *TempUnschedState {
	if now.IsZero() {
		now = time.Now()
	}
	return &TempUnschedState{
		UntilUnix:       tempUnschedIndefiniteUntil.Unix(),
		TriggeredAtUnix: now.Unix(),
		StatusCode:      statusCode,
		MatchedKeyword:  cursorAccountHealthBreakerMarker,
		RuleIndex:       -1,
		ErrorMessage:    errorMessage,
		Tier:            HealthBreakerTierTrip,
		Model:           strings.TrimSpace(model),
	}
}

// ProbeCursorAccountHealth 对 cursor Connect 会话型账号（acc.IsCursorConnectSessionAccount()）
// 发一次最小原生 AgentRun 探测，复用既有 CursorExecutionService.ProbeAgentRun 的整条
// transport / 鉴权 / 转换链——本函数不自行组装 AgentRun 请求体（避免重复实现转发链）。
//
// 返回口径与 ProbeAgentRun 对齐：上游状态码、上游错误摘要、解析出的真实文本、错误。
//
// budget 为本次探测的输出预算下限（思考型模型需 ≥ cursorAccountHealthProbeMinBudget 才能
// 探出正文）。ProbeAgentRun 已由 A3 改造接纳 variadic maxTokens 预算参数，本卡直接前向转发
// 该入参（见下方调用点）。
//
// 账号非 cursor Connect 会话型（IsCursorConnectSessionAccount 为 false，含非 cursor 平台、
// 或 cursor 的 APIKey 反代账号）时，直接以安全的失败关闭返回（err != nil），不发起任何出站请求。
func ProbeCursorAccountHealth(
	ctx context.Context,
	exec *CursorExecutionService,
	account *Account,
	upstreamModel string,
	budget int,
) (int, string, string, error) {
	if account == nil || !account.IsCursorConnectSessionAccount() {
		return 0, "", "", errors.New("cursor: probe requires a cursor Connect session account")
	}
	// budget 前向转发：ProbeAgentRun 已接纳 variadic maxTokens 预算参数（A3 改造完成），探针显式传
	// budget（须 ≥ cursorAccountHealthProbeMinBudget）以避开思考型模型把预算先花在 reasoning 上导致探针恒失败。
	return exec.ProbeAgentRun(ctx, account, upstreamModel, budget)
}
