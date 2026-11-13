//go:build unit

package service

// 本文件是 **§25.2 续轮识别 / §25.14-D 摘要校验与整批接管**的定向测试（派发单 W2-E）。
//
// 覆盖 §25.14-D「定向验证」清单逐条：
//  1. 完整历史原样重传成功（四协议各自表示）；
//  2. JSON 键序/空白不误拒、大整数不失真；
//  3. 改 system/正文/模型/工具/schema/推理配置 → 零上游写入拒绝；
//  4. Responses 合法引用省略历史成功、instructions 省略不继承、未知/旧/混合 ID 拒绝；
//  5. stream 切换不新建 AgentRun、不重结算；
//  6. required→auto 合法（集合不变）、具名扩集合拒绝；
//  7. 并行结果容器差异/同名无 ID/重复/缺失/并发消费；
//  8. finalizer 全索引清零。
//
// §25.17.4（A08）追加：
//  9. 四协议「全未知结果 / 仅未知 previous_response_id / 重启后原 ID」在 Cursor 准入边界
//     400（Identify 可区分哨兵 + Execute 准入断言），AgentRun 与结果写入计数均 0；
//     Responses 完整 input 回放（尾部结果的 function_call 同在一个正文里）例外——那是
//     「历史中已完成工具对」，按原普通请求继续。
//
// 纯内存组件测试：无 DB、无网络；真实结果写入通道用替身 intake。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// cursorTestIntake 是真实结果回写通道的替身（记录调用次数与批次）。
type cursorTestIntake struct {
	mu      sync.Mutex
	calls   int
	batches []string
	results [][]CursorToolResult
	failErr error
}

// DeliverToolResults 实现 CursorSessionIntake。
func (i *cursorTestIntake) DeliverToolResults(_ context.Context, _, batchID string, results []CursorToolResult) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	i.batches = append(i.batches, batchID)
	copied := make([]CursorToolResult, len(results))
	copy(copied, results)
	i.results = append(i.results, copied)
	return i.failErr
}

// callCount 返回被调用的次数（"零上游写入"断言的唯一来源）。
func (i *cursorTestIntake) callCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

// cursorResumeFixture 是一次续轮测试的夹具。
type cursorResumeFixture struct {
	svc      *CursorExecutionService
	record   *cursorSessionRecord
	intake   *cursorTestIntake
	identity CursorSessionIdentity
	batchID  string
}

// cursorResumeTestIdentity 是测试用的会话身份快照。
var cursorResumeTestIdentity = CursorSessionIdentity{UserID: 11, APIKeyID: 22, GroupID: 33}

// cursorResumeTestAccount 是测试用的 Cursor 账号（与夹具 spec.accountID 一致）。
func cursorResumeTestAccount() *Account {
	return &Account{ID: 42, Platform: PlatformCursor}
}

// cursorResumeWriterProtocol 返回该续轮协议对应的**客户端可见响应形状**。
func cursorResumeWriterProtocol(protocol CursorResumeProtocol) cursorProtocol {
	switch protocol {
	case CursorResumeProtocolMessages:
		return cursorProtocolAnthropic
	case CursorResumeProtocolResponses:
		return cursorProtocolResponses
	}
	return cursorProtocolChat
}

// cursorResumePlanForTest 按协议生成上下文计划（与生产入口同一批函数）。
func cursorResumePlanForTest(t *testing.T, protocol CursorResumeProtocol, body []byte, model string) *cursorContextPlan {
	t.Helper()
	switch protocol {
	case CursorResumeProtocolChat:
		var req apicompat.ChatCompletionsRequest
		require.NoError(t, json.Unmarshal(body, &req))
		plan, err := cursorPlanFromChat(&req, model)
		require.NoError(t, err)
		return plan
	case CursorResumeProtocolMessages:
		var req apicompat.AnthropicRequest
		require.NoError(t, json.Unmarshal(body, &req))
		plan, err := cursorPlanFromMessages(&req, model)
		require.NoError(t, err)
		return plan
	case CursorResumeProtocolResponses:
		var req apicompat.ResponsesRequest
		require.NoError(t, json.Unmarshal(body, &req))
		plan, err := cursorPlanFromResponses(&req, model)
		require.NoError(t, err)
		return plan
	case CursorResumeProtocolGemini:
		plan, err := cursorPlanFromGemini(body, model)
		require.NoError(t, err)
		return plan
	default:
		t.Fatalf("unsupported test protocol %q", protocol)
		return nil
	}
}

// newCursorResumeFixture 构造"首 HTTP 已交付一个工具批次、会话正在等结果"的夹具。
func newCursorResumeFixture(
	t *testing.T,
	protocol CursorResumeProtocol,
	firstBody []byte,
	model string,
	calls []cursorToolCallCapture,
	batchID string,
	responseID string,
) *cursorResumeFixture {
	t.Helper()
	svc := &CursorExecutionService{sessions: newCursorSessionStore()}
	record := svc.sessions.start(cursorSessionSpec{
		protocol:       cursorResumeWriterProtocol(protocol),
		requestedModel: model,
		upstreamModel:  "cursor-upstream-" + model,
		accountID:      42,
		quotaKeyID:     7,
		indexProtocol:  string(protocol),
		identity:       cursorResumeTestIdentity,
	})
	svc.sessions.attachOwner(record, svc)

	plan := cursorResumePlanForTest(t, protocol, firstBody, model)
	record.recordPlan(cursorResumeTestIdentity, plan)

	delegations := newLocalCursorToolDelegations()
	batch := &cursorToolBatch{modelCallID: batchID, sealed: true}
	for _, call := range calls {
		batch.delegations = append(batch.delegations, call)
		batch.started = append(batch.started, call.ID)
	}
	delegations.mu.Lock()
	delegations.all = append(delegations.all, batch)
	delegations.mu.Unlock()
	// 会话必须绑定本轮委托记录（生产路径 Execute 里 session.attachDelegations 做同一件事）：
	// 续轮的 call_id 解析与归属判定要读它。
	record.attachDelegations(delegations)
	// 会话必须绑定本轮委托记录（生产路径 Execute 里 session.attachDelegations 做同一件事）：
	// 续轮的 call_id 解析与归属判定要读它。
	record.attachDelegations(delegations)
	// 会话必须绑定本轮委托记录（生产路径 Execute 里 session.attachDelegations 做同一件事）：
	// 续轮的 call_id 解析与归属判定要读它。
	record.attachDelegations(delegations)

	require.NoError(t, svc.recordDeliveredBatch(record, &CursorExecutionRequest{
		Protocol:      cursorResumeWriterProtocol(protocol),
		IndexProtocol: string(protocol),
		Model:         model,
	}, delegations, responseID))

	intake := &cursorTestIntake{}
	require.NoError(t, svc.installIntake(record.SessionID(), intake))
	return &cursorResumeFixture{
		svc:      svc,
		record:   record,
		intake:   intake,
		identity: cursorResumeTestIdentity,
		batchID:  batchID,
	}
}

// resume 走生产同一路径：探针 → 统一索引定位 → 整批接管 + 摘要校验 + 串行写入。
func (f *cursorResumeFixture) resume(
	t *testing.T, protocol CursorResumeProtocol, body []byte, stream bool,
) (*ForwardResult, error) {
	t.Helper()
	return f.resumeCtx(t, context.Background(), protocol, body, stream)
}

// resumeCtx 与 resume 相同，但由调用方提供 ctx（用于把结构化日志收进 observer）。
func (f *cursorResumeFixture) resumeCtx(
	t *testing.T, ctx context.Context, protocol CursorResumeProtocol, body []byte, stream bool,
) (*ForwardResult, error) {
	t.Helper()
	probe, err := CursorProbeResumeRequest(protocol, body)
	require.NoError(t, err)
	ticket, err := f.svc.IdentifyResume(probe)
	require.NoError(t, err)
	require.NotNil(t, ticket, "探针必须定位到唯一待完成批")
	return f.svc.ResumeCursorSession(ctx, nil, ticket, cursorResumeTestAccount(), CursorResumeServeInput{
		Body:     body,
		Stream:   stream,
		Identity: f.identity,
	})
}

// --- 各协议的请求体构造（只含测试需要的最小形态） ---

const cursorResumeTestChatFirst = `{
  "model": "cursor-test-model",
  "messages": [{"role": "user", "content": "weather please"}],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`

const cursorResumeTestChatResume = `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`

const cursorResumeTestMessagesFirst = `{
  "model": "cursor-test-model",
  "max_tokens": 64,
  "messages": [{"role": "user", "content": [{"type": "text", "text": "weather please"}]}],
  "tools": [{"name": "get_weather", "input_schema": {"type": "object"}}]
}`

const cursorResumeTestMessagesResume = `{
  "model": "cursor-test-model",
  "max_tokens": 64,
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "weather please"}]},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "hz"}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "sunny"}]}]}
  ],
  "tools": [{"name": "get_weather", "input_schema": {"type": "object"}}]
}`

const cursorResumeTestResponsesFirst = `{
  "model": "cursor-test-model",
  "instructions": "be brief",
  "input": [{"type": "message", "role": "user", "content": "weather please"}],
  "tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
}`

const cursorResumeTestResponsesResume = `{
  "model": "cursor-test-model",
  "instructions": "be brief",
  "input": [
    {"type": "message", "role": "user", "content": "weather please"},
    {"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\": \"hz\"}"},
    {"type": "function_call_output", "call_id": "call_1", "output": "sunny"}
  ],
  "tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
}`

// TestCursorResumeChatFullHistoryReplay 覆盖清单第 1 条（Chat）：完整历史原样重传成功。
func TestCursorResumeChatFullHistoryReplay(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")

	result, err := fixture.resume(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatResume), false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, fixture.intake.callCount(), "真实结果必须整批写入一次")
	require.Equal(t, []string{"call_1"}, []string{fixture.intake.results[0][0].CallID})
	require.Equal(t, []string{"sunny"}, fixture.intake.results[0][0].Text)
	require.Equal(t, 1, fixture.svc.sessions.sessionCount(), "续轮不得新建 AgentRun（会话表数量不变）")
}

// TestCursorResumeMessagesFullHistoryReplay 覆盖清单第 1 条（Messages）。
func TestCursorResumeMessagesFullHistoryReplay(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolMessages, []byte(cursorResumeTestMessagesFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "toolu_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")

	_, err := fixture.resume(t, CursorResumeProtocolMessages, []byte(cursorResumeTestMessagesResume), false)
	require.NoError(t, err)
	require.Equal(t, 1, fixture.intake.callCount())
	require.Equal(t, "toolu_1", fixture.intake.results[0][0].CallID)
}

// TestCursorResumeMessagesSingleVsMultiContainer 覆盖清单第 7 条：单/多容器分组差异规范化。
func TestCursorResumeMessagesSingleVsMultiContainer(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolMessages, []byte(cursorResumeTestMessagesFirst), "cursor-test-model",
		[]cursorToolCallCapture{
			{ID: "toolu_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`},
			{ID: "toolu_2", Name: "get_weather", ArgsJSON: `{"city": "sh"}`},
		}, "mc-1", "")

	const multiContainer = `{
  "model": "cursor-test-model",
  "max_tokens": 64,
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "weather please"}]},
    {"role": "assistant", "content": [
      {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "hz"}},
      {"type": "tool_use", "id": "toolu_2", "name": "get_weather", "input": {"city": "sh"}}
    ]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "sunny"}]}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_2", "content": [{"type": "text", "text": "rainy"}]}]}
  ],
  "tools": [{"name": "get_weather", "input_schema": {"type": "object"}}]
}`
	_, err := fixture.resume(t, CursorResumeProtocolMessages, []byte(multiContainer), false)
	require.NoError(t, err, "同一批结果的单/多容器差异必须规范化")
	require.Equal(t, 1, fixture.intake.callCount())
	require.Equal(t, 2, len(fixture.intake.results[0]))
}

// TestCursorResumeResponsesFullHistoryReplay 覆盖清单第 1 条（Responses 完整 input）。
func TestCursorResumeResponsesFullHistoryReplay(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorResumeTestResponsesFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorResumeTestResponsesResume), false)
	require.NoError(t, err)
	require.Equal(t, 1, fixture.intake.callCount())
}

// TestCursorResumeResponsesPreviousResponseIDOmittedHistory 覆盖清单第 4 条：
// previous_response_id 合法引用、仅携带本批结果、省略历史 —— 成功。
func TestCursorResumeResponsesPreviousResponseIDOmittedHistory(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorResumeTestResponsesFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "resp_1")

	const onlyResults = `{
  "model": "cursor-test-model",
  "instructions": "be brief",
  "previous_response_id": "resp_1",
  "input": [{"type": "function_call_output", "call_id": "call_1", "output": "sunny"}],
  "tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
}`
	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(onlyResults), true)
	require.NoError(t, err, "省略历史的合法引用必须接纳")
	require.Equal(t, 1, fixture.intake.callCount())
}

// TestCursorResumeResponsesUnknownPreviousResponseID 覆盖清单第 4 条：
// 携带 previous_response_id 未命中索引、但本批 call_1 命中（混合关联）→ 明确 400 拒绝，
// 不得复用全未知哨兵（§25.17.4：任一 ID 命中则整批必须一致，previous_response_id 不匹配即 400）。
func TestCursorResumeResponsesUnknownPreviousResponseID(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorResumeTestResponsesFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "resp_1")

	probe, err := CursorProbeResumeRequest(CursorResumeProtocolResponses, []byte(`{
  "model": "cursor-test-model",
  "previous_response_id": "resp_unknown",
  "input": [{"type": "function_call_output", "call_id": "call_1", "output": "sunny"}]
}`))
	require.NoError(t, err)
	_, identifyErr := fixture.svc.IdentifyResume(probe)
	require.ErrorIs(t, identifyErr, ErrCursorResumeStaleBatch,
		"previous_response_id 未命中但 call_1 命中属混合关联，必须明确 400 而非全未知哨兵")
	status, _ := CursorResumeHTTPStatus(identifyErr)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, 0, fixture.intake.callCount())
}

// TestCursorResumeResponsesStaleResponseID 覆盖清单第 4 条：旧 response 明确拒绝。
func TestCursorResumeResponsesStaleResponseID(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorResumeTestResponsesFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "resp_1")

	// 把会话的当前 response 推进成 resp_2：resp_1 变成旧批。
	fixture.record.mu.Lock()
	fixture.record.pending.responseID = "resp_2"
	fixture.record.mu.Unlock()

	probe, err := CursorProbeResumeRequest(CursorResumeProtocolResponses, []byte(`{
  "model": "cursor-test-model",
  "previous_response_id": "resp_1",
  "input": [{"type": "function_call_output", "call_id": "call_1", "output": "sunny"}]
}`))
	require.NoError(t, err)
	_, identifyErr := fixture.svc.IdentifyResume(probe)
	require.ErrorIs(t, identifyErr, ErrCursorResumeStaleBatch)
	require.Equal(t, 0, fixture.intake.callCount())
}

// TestCursorResumeResponsesInstructionsNotInherited 覆盖清单第 4 条：
// 首轮非空 instructions + 续轮省略 = 明确入参错误（不偷偷继承）。
func TestCursorResumeResponsesInstructionsNotInherited(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorResumeTestResponsesFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "resp_1")

	const noInstructions = `{
  "model": "cursor-test-model",
  "previous_response_id": "resp_1",
  "input": [{"type": "function_call_output", "call_id": "call_1", "output": "sunny"}],
  "tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
}`
	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(noInstructions), false)
	require.ErrorIs(t, err, ErrCursorResumeInstructionsWithdrawn)
	require.Equal(t, 0, fixture.intake.callCount(), "校验失败必须零上游写入")
}

// TestCursorResumeMixedAndDuplicateIDs 覆盖清单第 4/7 条：混合会话与重复 ID 拒绝。
func TestCursorResumeMixedAndDuplicateIDs(t *testing.T) {
	first := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_a", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-a", "")

	// 另一个会话登记 call_b（同一 store，模拟跨会话混批）。
	second := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_b", Name: "get_weather", ArgsJSON: `{"city": "sh"}`}}, "mc-b", "")
	second.svc = first.svc
	second.record.mu.Lock()
	second.record.spec.accountID = 42
	second.record.mu.Unlock()
	// 把第二个会话搬进同一 store（同一索引）。
	first.svc.sessions.mu.Lock()
	first.svc.sessions.sessions[second.record.SessionID()] = second.record
	first.svc.sessions.mu.Unlock()
	require.NoError(t, first.svc.sessions.registerSessionID(second.record.SessionID(), "mc-b", CursorIDKindCall, "call_b"))

	mixedProbe := &CursorResumeProbe{
		Protocol:      CursorResumeProtocolChat,
		ResultCallIDs: []string{"call_a", "call_b"},
	}
	_, mixedErr := first.svc.IdentifyResume(mixedProbe)
	require.ErrorIs(t, mixedErr, ErrCursorResumeMixedSessions)
	require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, mixedErr))

	dupProbe := &CursorResumeProbe{
		Protocol:      CursorResumeProtocolChat,
		ResultCallIDs: []string{"call_a", "call_a"},
	}
	_, dupErr := first.svc.IdentifyResume(dupProbe)
	require.ErrorIs(t, dupErr, ErrCursorResumeDuplicateResults)
	require.Equal(t, 0, first.intake.callCount())
}

// mustCursorResumeStatus 取续轮错误的 HTTP 状态（非续轮错误直接失败）。
func mustCursorResumeStatus(t *testing.T, err error) int {
	t.Helper()
	status, reason := CursorResumeHTTPStatus(err)
	require.NotZero(t, status, "必须是带明确状态的续轮错误：%v", reason)
	return status
}

// TestCursorResumeUnknownCallID 覆盖清单第 4 条：未知（含重启孤儿）ID 的处理口径。
//
// §25.17.4（A08）钉死：请求**明确声称续轮**（携带本批工具结果或 previous_response_id），
// 但全部 ID 都未登记 → Identify 返回可区分哨兵 ErrCursorResumeUnknownAssociation（400），
// **不得**再被当作普通新请求开新 AgentRun。该可区分哨兵交给 helper 只按供应方判定：
// 非 Cursor 原样转发；进入 Cursor 执行分支由 Execute 准入断言在模型出站/建 session/转交
// 资源之前拒绝。已命中但夹入未知/跨会话/旧批/重复 ID 仍沿既有 400/409 拒绝。
func TestCursorResumeUnknownCallID(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")

	probe, err := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(`{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_x", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}}]},
    {"role": "tool", "tool_call_id": "call_x", "content": "sunny"}
  ]
}`))
	require.NoError(t, err)
	ticket, identifyErr := fixture.svc.IdentifyResume(probe)
	require.ErrorIs(t, identifyErr, ErrCursorResumeUnknownAssociation,
		"全部 ID 未命中（含重启孤儿）必须返回可区分哨兵，不得当作普通新请求")
	require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, identifyErr))
	require.Nil(t, ticket)
	require.Equal(t, 0, fixture.intake.callCount())

	// 已登记 ID 与未登记 ID 混在同一批：整批明确 400（不部分消费），
	// 严格区分于全未知哨兵（§25.17.4 行 4：混合关联不得复用 ErrCursorResumeUnknownAssociation，
	// 否则会让混合续轮逃到普通/非 Cursor 供应方）。
	mixedProbe, probeErr := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(`{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [
      {"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}},
      {"id": "call_unknown", "type": "function", "function": {"name": "get_weather", "arguments": "{}"}}
    ]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
    {"role": "tool", "tool_call_id": "call_unknown", "content": "sunny"}
  ]
}`))
	require.NoError(t, probeErr)
	_, mixedErr := fixture.svc.IdentifyResume(mixedProbe)
	require.ErrorIs(t, mixedErr, ErrCursorResumeUnknownCallID,
		"已命中+夹未知属混合关联，必须走 unknown_call_id 而非全未知哨兵")
	require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, mixedErr))
	require.Equal(t, 0, fixture.intake.callCount())
}

// TestCursorResumeJSONWhitespaceAndKeyOrder 覆盖清单第 2 条：
// 工具参数 JSON 仅空白/键序差异不误拒。
func TestCursorResumeJSONWhitespaceAndKeyOrder(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz", "days": 2}`}}, "mc-1", "")

	// 键序调换 + 空白不同：同值。
	const reordered = `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{ \"days\" : 2 ,\n \"city\" : \"hz\" }"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
	_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(reordered), false)
	require.NoError(t, err, "仅空白/键序差异必须视为同值")
	require.Equal(t, 1, fixture.intake.callCount())
}

// TestCursorResumeConfigChangesRejected 覆盖清单第 3 条：改**硬组**配置（工具声明与 schema）
// 各项零上游写入拒绝。
//
// 硬组是"还是不是同一次生成"的判据：工具集合或 schema 换了，上游 wire 契约与工具模式
// 都变了，必须拒绝。system 文本与采样参数属软组，见
// TestCursorResumeSoftConfigDriftAccepted（只留痕不拒绝）。
//
// 注意：**模型变了不在这里**。2026-11-04 用户裁定后，换模型且有完整历史 = 按新模型重开
// 一局（见 cursor_resume_model_rebuild_test.go），不再 400——"防串号"的目的由"开新局"
// 达成，不该顺带砍掉"用户换模型继续聊"这个合法需求。
func TestCursorResumeConfigChangesRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "tool_schema_changed",
			body: `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}]
}`,
		},
		{
			name: "tools_expanded",
			body: `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [
    {"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}},
    {"type": "function", "function": {"name": "send_email", "parameters": {"type": "object"}}}
  ]
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
				[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")
			// 2026-11-07 五道闸退役：硬组（工具声明/schema）变了**不再拒绝** ——
			// 客户端每轮重新拼装工具声明是合法行为（生产实测续轮比首轮少声明一个工具
			// 22→21 / 24→23），跨轮比对只会把正常用户拒成 CUR-RES-04。
			// 现在一律按**本次正文声明的**配置构造本轮。
			_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(tc.body), false)
			require.NoError(t, err, "改硬组配置不再拒绝（五道闸退役）")
			require.Equal(t, 1, fixture.intake.callCount(), "按本次正文继续后应当正常出站一轮")
		})
	}
}

// TestCursorResumeSoftConfigDriftAccepted 覆盖"软组漂移只留痕不拒绝"。
//
// 这两项都是客户端每轮**重新拼装**的字节：system 里带环境快照/工具说明，重试时
// reasoning/thinking 字段会丢。把整链哈希当准入闸门，就会把正常续轮拒成 CUR-RES-04
// （线上实证：客户端截断流后重试，system 与 thinking 双双与首轮不同）。
// 准入闸门只能是硬组（模型 + 工具声明/schema），这已由上一个用例覆盖。
func TestCursorResumeSoftConfigDriftAccepted(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "system_changed",
			body: `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "system", "content": "you are terse"},
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`,
		},
		{
			name: "reasoning_config_changed",
			body: `{
  "model": "cursor-test-model",
  "reasoning_effort": "high",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`,
		},
		{
			name: "sampling_changed",
			body: `{
  "model": "cursor-test-model",
  "temperature": 0.9,
  "max_tokens": 4096,
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
				[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")
			_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(tc.body), false)
			require.NoError(t, err, "软组漂移不得拒绝续轮")
			require.Equal(t, 1, fixture.intake.callCount(), "软组漂移必须正常写回本批结果")
		})
	}
}

// cursorPhaseEntries 从 observer 里挑出指定 phase 的 cursor 阶段事件
// （cursorPhaseLog 的统一 message 是 "cursor_phase_event"，事件名在 phase 字段里）。
func cursorPhaseEntries(logs *observer.ObservedLogs, phase string) []observer.LoggedEntry {
	out := make([]observer.LoggedEntry, 0, 4)
	for _, entry := range logs.All() {
		if got, _ := entry.ContextMap()["phase"].(string); got == phase {
			out = append(out, entry)
		}
	}
	return out
}

// TestCursorResumeConfigDriftLogged 是软组/硬组的**可观测性**取证：两组都必须落日志。
//
// 没有这两个事件，线上看到 CUR-RES-04 只能看到一个 config_changed，无法回答"到底哪一组
// 变了、是 system 还是 thinking"——那正是上一轮排查走弯路的直接原因。这里用 observer 把
// 事件收进内存并逐字段断言（字段只含分组名/项数/摘要前缀，不含任何正文）。
func TestCursorResumeConfigDriftLogged(t *testing.T) {
	t.Run("soft_drift_logged_and_accepted", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		ctx := logger.IntoContext(context.Background(), zap.New(core))

		fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
			[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")
		body := `{
  "model": "cursor-test-model",
  "reasoning_effort": "high",
  "messages": [
    {"role": "system", "content": "you are terse"},
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
		_, err := fixture.resumeCtx(t, ctx, CursorResumeProtocolChat, []byte(body), false)
		require.NoError(t, err, "软组漂移必须放行")
		require.Equal(t, 1, fixture.intake.callCount())

		entries := cursorPhaseEntries(logs, "resume_config_drift")
		require.Len(t, entries, 1, "软组漂移必须落 resume_config_drift")
		fields := entries[0].ContextMap()
		groups, _ := fields["drift_groups"].([]any)
		require.NotEmpty(t, groups, "必须写明是哪一组漂移")
		require.Contains(t, fields, "system_digest_stored")
		require.Contains(t, fields, "system_digest_request")
		require.NotEmpty(t, fields["session_id"])
		// 安全字段：不得把正文/参数写进日志。
		for key := range fields {
			require.NotContains(t, []string{"system", "content", "body", "args"}, key)
		}
	})

	t.Run("hard_mismatch_logged_and_rejected", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		ctx := logger.IntoContext(context.Background(), zap.New(core))

		fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
			[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")
		body := `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}]
}`
		// 2026-11-07 五道闸退役：硬组不一致**不再拒绝**，但**仍必须留痕**
		// （线上看到 CUR-RES-04 时要能回答「到底哪一组变了」——那正是上一轮
		//  排查走弯路的直接原因。可观测性不因退役而丢失）。
		_, err := fixture.resumeCtx(t, ctx, CursorResumeProtocolChat, []byte(body), false)
		require.NoError(t, err, "硬组不一致不再拒绝（五道闸退役）")
		require.Equal(t, 1, fixture.intake.callCount(), "按本次正文继续后应当正常出站一轮")

		entries := cursorPhaseEntries(logs, "resume_config_hard_mismatch")
		require.Len(t, entries, 1, "硬组不一致必须落 resume_config_hard_mismatch")
		fields := entries[0].ContextMap()
		require.Contains(t, fields, "hard_digest_stored")
		require.Contains(t, fields, "hard_digest_request")
		require.Equal(t, int64(1), fields["tool_count_stored"])
		require.Equal(t, int64(1), fields["tool_count_request"])
	})
}

// TestCursorResumeRejectDiagnosticsLogged 是**拒轮可观测性**的取证：三种拒轮都必须带
// 可定位字段落盘，否则线上只有 error_code + reason，无法判断是谁改了什么。
//
// 这是用生产日志反推出来的需求：部署后 config_changed 归零，但 history_mismatch(30) 与
// model_changed(12) 各留一堆裸 reason，排查只能靠猜。三者都必须能回答"存量 vs 请求"。
func TestCursorResumeRejectDiagnosticsLogged(t *testing.T) {
	t.Run("history_mismatch_logged_with_counts", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		ctx := logger.IntoContext(context.Background(), zap.New(core))

		fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
			[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")
		// 客户端多带了一条历史（首轮没有的用户消息），项数必然与存量不同。
		body := `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "let me check"},
    {"role": "user", "content": "another question"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
		// 2026-11-07 五道闸退役：历史项数不同**不再拒绝**（客户端裁剪 assistant 可见正文
		// 是合法行为），但**仍必须留痕**（线上看到续轮被拒时要能回答「谁改了什么」）。
		_, err := fixture.resumeCtx(t, ctx, CursorResumeProtocolChat, []byte(body), false)
		require.NoError(t, err, "历史项数不同不再拒绝（五道闸退役）")
		require.Equal(t, 1, fixture.intake.callCount(), "按本次正文继续后应当正常出站一轮")

		entries := cursorPhaseEntries(logs, "resume_history_mismatch")
		require.Len(t, entries, 1, "历史不一致必须落带字段的诊断事件")
		fields := entries[0].ContextMap()
		require.Contains(t, fields, "history_items_stored")
		require.Contains(t, fields, "history_items_request")
		require.Contains(t, fields, "history_digest_stored")
		require.Contains(t, fields, "history_digest_request")
		require.NotEmpty(t, fields["session_id"])
	})

	t.Run("model_changed_logged_with_both_models", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		ctx := logger.IntoContext(context.Background(), zap.New(core))

		fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
			[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")
		body := `{
  "model": "cursor-other-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
		_, err := fixture.resumeCtx(t, ctx, CursorResumeProtocolChat, []byte(body), false)
		require.Error(t, err)
		require.Equal(t, 0, fixture.intake.callCount())

		entries := cursorPhaseEntries(logs, "resume_model_changed")
		require.Len(t, entries, 1, "model_changed 必须落带两侧型号的诊断事件")
		fields := entries[0].ContextMap()
		require.Equal(t, "cursor-test-model", fields["model_stored"])
		require.NotEmpty(t, fields["model_ticket"])
	})
}

// TestCursorResumeBodyChangedRejected 覆盖清单第 3 条：改正文拒绝。
func TestCursorResumeBodyChangedRejected(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")

	const rewrittenUser = `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please (rewritten)"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
	// 2026-11-07 五道闸退役：用户正文被改**不再拒绝** —— 用户自己改写历史是正常行为
	// （客户端重试、改写提问）。跨轮逐字节比对正文会把正常用户拒成 CUR-RES-04。
	// ⚠️ 注意「工具参数被改」仍然被拒（防篡改），那是另一条能力：
	// 见 TestCursorResumeCustomToolInputAligned（结果侧参数一致性校验）。
	_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(rewrittenUser), false)
	require.NoError(t, err, "正文被改不再拒绝（五道闸退役）")
	require.Equal(t, 1, fixture.intake.callCount(), "按本次正文继续后应当正常出站一轮")
}

// TestCursorResumeStreamSwitchNoNewAgentRun 覆盖清单第 5 条：
// stream/stream_options 切换不新建 AgentRun、不重结算。
func TestCursorResumeStreamSwitchNoNewAgentRun(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")

	settlements := 0
	fixture.record.mu.Lock()
	fixture.record.settlement = func(context.Context, CursorSettlementEvent) { settlements++ }
	fixture.record.mu.Unlock()

	const withStreamOptions = `{
  "model": "cursor-test-model",
  "stream_options": {"include_usage": true},
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
	_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(withStreamOptions), true)
	require.NoError(t, err, "stream/stream_options 切换属于输出选项，必须允许")
	require.Equal(t, 1, fixture.svc.sessions.sessionCount(), "不得新建 AgentRun")
	require.Equal(t, 0, settlements, "输出选项切换不得触发结算")
}

// TestCursorResumeToolChoicePerHTTP 覆盖清单第 6 条：
// required→auto 合法（集合不变）、具名扩集合拒绝、required 不跨 HTTP 永久套用。
func TestCursorResumeToolChoicePerHTTP(t *testing.T) {
	build := func(choice string) string {
		return `{
  "model": "cursor-test-model",
  "tool_choice": ` + choice + `,
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
	}

	newFixture := func() *cursorResumeFixture {
		return newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
			[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")
	}

	// 首轮 required → 续轮 auto：集合不变，合法。
	fixture := newFixture()
	_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(build(`"auto"`)), false)
	require.NoError(t, err, "required→auto 在不改变暴露集合时必须合法")
	require.Equal(t, 1, fixture.intake.callCount())

	// 具名选择命中已暴露工具：合法。
	fixture = newFixture()
	_, err = fixture.resume(t, CursorResumeProtocolChat, []byte(build(`{"type": "function", "function": {"name": "get_weather"}}`)), false)
	require.NoError(t, err)

	// 具名指向未暴露工具：拒绝。
	fixture = newFixture()
	_, err = fixture.resume(t, CursorResumeProtocolChat, []byte(build(`{"type": "function", "function": {"name": "send_email"}}`)), false)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, err))
	require.Equal(t, 0, fixture.intake.callCount())
}

// TestCursorResumeMissingAndDuplicateResults 覆盖清单第 7 条：缺失/重复结果明确拒绝。
func TestCursorResumeMissingAndDuplicateResults(t *testing.T) {
	newFixture := func() *cursorResumeFixture {
		return newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
			[]cursorToolCallCapture{
				{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`},
				{ID: "call_2", Name: "get_weather", ArgsJSON: `{"city": "sh"}`},
			}, "mc-1", "")
	}
	const head = `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [
      {"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}},
      {"id": "call_2", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"sh\"}"}}
    ]},
    `
	missing := head + `{"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
	fixture := newFixture()
	_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(missing), false)
	require.ErrorIs(t, err, ErrCursorResumeBatchIncomplete)
	require.Equal(t, 0, fixture.intake.callCount())

	duplicated := head + `{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
	fixture = newFixture()
	// 重复结果在**识别阶段**（探针/定位）就被明确拒绝，绝不进入写入。
	dupProbe, probeErr := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(duplicated))
	require.NoError(t, probeErr)
	_, dupErr := fixture.svc.IdentifyResume(dupProbe)
	if dupErr == nil {
		_, dupErr = fixture.resume(t, CursorResumeProtocolChat, []byte(duplicated), false)
	}
	require.Error(t, dupErr, "重复结果必须明确拒绝")
	require.Equal(t, 0, fixture.intake.callCount())
}

// TestCursorConcurrentResume 覆盖清单第 7 条并发消费：并发正确请求唯一胜者，后者 409。
func TestCursorConcurrentResume(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			probe, err := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(cursorResumeTestChatResume))
			if err != nil {
				errs[idx] = err
				return
			}
			ticket, identifyErr := fixture.svc.IdentifyResume(probe)
			if identifyErr != nil {
				errs[idx] = identifyErr
				return
			}
			if ticket == nil {
				errs[idx] = errors.New("ticket nil")
				return
			}
			_, serveErr := fixture.svc.ResumeCursorSession(context.Background(), nil, ticket, cursorResumeTestAccount(), CursorResumeServeInput{
				Body:     []byte(cursorResumeTestChatResume),
				Identity: fixture.identity,
			})
			errs[idx] = serveErr
		}(i)
	}
	close(start)
	wg.Wait()

	wins, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		status, _ := CursorResumeHTTPStatus(err)
		if status == http.StatusConflict {
			conflicts++
		}
	}
	require.Equal(t, 1, wins, "并发正确请求只能有一个胜者")
	require.Equal(t, workers-1, conflicts, "其余请求必须是明确 409")
	require.Equal(t, 1, fixture.intake.callCount(), "真实结果只写一次（不重复执行/发送）")
}

// TestCursorResumeFinalizerClearsWholeIndex 覆盖清单第 8 条：finalizer 全索引清零。
func TestCursorResumeFinalizerClearsWholeIndex(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorResumeTestResponsesFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "resp_1")

	require.NotZero(t, fixture.svc.sessions.indexCount(), "交付后索引必须有条目")
	fixture.svc.finishCursorSession(fixture.record, "test_closed")
	require.Zero(t, fixture.svc.sessions.indexCount(), "finalizer 必须删除该会话全部关联（含 response/item）")
	require.Zero(t, fixture.svc.sessions.sessionCount())
}

// TestCursorResumeGeminiAmbiguousParallel 覆盖清单第 7 条：Gemini 同名并行缺 ID 明确拒绝歧义。
func TestCursorResumeGeminiAmbiguousParallel(t *testing.T) {
	const geminiBody = `{
  "model": "cursor-test-model",
  "contents": [
    {"role": "user", "parts": [{"text": "weather please"}]},
    {"role": "model", "parts": [
      {"functionCall": {"name": "get_weather", "args": {"city": "hz"}}},
      {"functionCall": {"name": "get_weather", "args": {"city": "sh"}}}
    ]},
    {"role": "user", "parts": [
      {"functionResponse": {"name": "get_weather", "response": {"result": "sunny"}}},
      {"functionResponse": {"name": "get_weather", "response": {"result": "rainy"}}}
    ]}
  ]
}`
	// 2026-11-06 口径：同名并行结果缺 ID → 无法配对的那几条**丢掉并记账**，
	// 不整轮拒（客户少一条结果，也比整轮 400 好）。
	probe, err := CursorProbeResumeRequest(CursorResumeProtocolGemini, []byte(geminiBody))
	require.NoError(t, err, "缺 ID 的并行结果不得整轮拒")
	require.Empty(t, probe.ResultCallIDs, "没有 ID 的结果一律不配对")
	require.GreaterOrEqual(t, probe.Plan.skippedItems["gemini_function_response_without_id"], 1,
		"丢弃原因要记账，实际=%s", probe.Plan.skippedItems.summary())
}

// TestCursorResumeGeminiPairedByCallID 覆盖清单第 1 条（Gemini）：按 call ID 配对成功。
func TestCursorResumeGeminiPairedByCallID(t *testing.T) {
	const geminiFirst = `{
  "model": "cursor-test-model",
  "contents": [{"role": "user", "parts": [{"text": "weather please"}]}]
}`
	const geminiResume = `{
  "model": "cursor-test-model",
  "contents": [
    {"role": "user", "parts": [{"text": "weather please"}]},
    {"role": "model", "parts": [{"functionCall": {"id": "call_1", "name": "get_weather", "args": {"city": "hz"}}}]},
    {"role": "user", "parts": [{"functionResponse": {"id": "call_1", "name": "get_weather", "response": {"result": "sunny"}}}]}
  ]
}`
	fixture := newCursorResumeFixture(t, CursorResumeProtocolGemini, []byte(geminiFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")

	probe, err := CursorProbeResumeRequest(CursorResumeProtocolGemini, []byte(geminiResume))
	require.NoError(t, err)
	require.Equal(t, []string{"call_1"}, probe.ResultCallIDs)
	ticket, identifyErr := fixture.svc.IdentifyResume(probe)
	require.NoError(t, identifyErr)
	require.NotNil(t, ticket)
	require.Equal(t, CursorResumeProtocolGemini, ticket.Protocol)
}

// TestCursorResumeIdentityMismatch 覆盖身份校验：他人会话不得续轮（不泄露存在性）。
func TestCursorResumeIdentityMismatch(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")

	probe, err := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(cursorResumeTestChatResume))
	require.NoError(t, err)
	ticket, err := fixture.svc.IdentifyResume(probe)
	require.NoError(t, err)
	_, serveErr := fixture.svc.ResumeCursorSession(context.Background(), nil, ticket, cursorResumeTestAccount(), CursorResumeServeInput{
		Body:     []byte(cursorResumeTestChatResume),
		Identity: CursorSessionIdentity{UserID: 99, APIKeyID: 98, GroupID: 97},
	})
	require.ErrorIs(t, serveErr, ErrCursorResumeIdentityMismatch)
	require.Equal(t, http.StatusForbidden, mustCursorResumeStatus(t, serveErr))
	require.Equal(t, 0, fixture.intake.callCount())
}

// TestCursorResumeIntakeUnavailableFailsClosed 覆盖"通道未接通失败关闭"：
// 校验通过但回写通道缺失时必须零上游写入且明确失败，绝不伪造成功。
func TestCursorResumeIntakeUnavailableFailsClosed(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`}}, "mc-1", "")
	require.NoError(t, fixture.svc.installIntake(fixture.record.SessionID(), nil))

	probe, err := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(cursorResumeTestChatResume))
	require.NoError(t, err)
	ticket, err := fixture.svc.IdentifyResume(probe)
	require.NoError(t, err)
	_, serveErr := fixture.svc.ResumeCursorSession(context.Background(), nil, ticket, cursorResumeTestAccount(), CursorResumeServeInput{
		Body:     []byte(cursorResumeTestChatResume),
		Identity: fixture.identity,
	})
	require.ErrorIs(t, serveErr, ErrCursorResumeIntakeUnavailable)
	require.Equal(t, http.StatusServiceUnavailable, mustCursorResumeStatus(t, serveErr))
	require.Equal(t, 0, fixture.intake.callCount())

	// 失败必须不改会话状态：会话仍在等工具结果，索引未被消费。
	fixture.record.mu.Lock()
	state := fixture.record.state
	consumed := fixture.record.pending.consumed
	fixture.record.mu.Unlock()
	require.Equal(t, cursorSessionWaitingTools, state)
	require.False(t, consumed)
}

// ---------------------------------------------------------------------------
// §25.17.4（A08）：Execute 续轮准入断言——声明续轮却无有效接管 → 出站前 400
// ---------------------------------------------------------------------------

// TestCursorResumeExecuteAdmissionAllUnknownResults 覆盖 2026-11-04 新裁定：
// chat 请求携带本批工具结果（全部未知，含重启孤儿）且正文携带完整历史 → Execute 准入
// **放行重建**（P2），不再返回 unknown_association；后续因依赖未装配而失败关闭。
//
// 历史不完整的同类请求（Responses 仅 previous_response_id）仍维持 400，见下一个用例。
func TestCursorResumeExecuteAdmissionAllUnknownResults(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore(), now: time.Now}
	probe, err := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(cursorResumeTestChatResume))
	require.NoError(t, err)
	require.True(t, probe.ClaimsContinuation(), "请求必须明确声称续轮")

	// 同一探针计划进入新建 Execute 入口（等价于 helper 未接管、原路由选中 Cursor 分支）。
	plan := cursorResumePlanForTest(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatResume), "cursor-test-model")
	require.True(t, plan.claimsContinuation(), "转换不得抹掉续轮标记")
	require.True(t, plan.historySupportsRebuild(), "chat 正文即完整历史，允许孤儿重建")

	var ccReq apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal([]byte(cursorResumeTestChatResume), &ccReq))
	_, execErr := svc.Execute(context.Background(), nil, cursorResumeTestAccount(), &CursorExecutionRequest{
		Chat:     &ccReq,
		Model:    "cursor-test-model",
		Protocol: cursorProtocolChat,
		Identity: cursorResumeTestIdentity,
		Plan:     plan,
	})
	require.Error(t, execErr, "依赖未装配 → 准入放行后按原链路失败关闭")
	require.False(t, errors.Is(execErr, ErrCursorResumeUnknownAssociation),
		"完整历史的孤儿续轮必须放行重建，不得再 400 unknown_association")
}

// TestCursorResumeExecuteAdmissionOnlyPreviousResponseID 覆盖：仅 previous_response_id 未知
// 的续轮同样在 Execute 准入边界 400（重启孤儿）。
func TestCursorResumeExecuteAdmissionOnlyPreviousResponseID(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore()}
	const body = `{
  "model": "cursor-test-model",
  "instructions": "be brief",
  "previous_response_id": "resp_unknown",
  "input": [{"type": "function_call_output", "call_id": "call_1", "output": "sunny"}],
  "tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
}`
	probe, err := CursorProbeResumeRequest(CursorResumeProtocolResponses, []byte(body))
	require.NoError(t, err)
	require.True(t, probe.ClaimsContinuation(), "previous_response_id 即显式续轮声明")

	plan := cursorResumePlanForTest(t, CursorResumeProtocolResponses, []byte(body), "cursor-test-model")
	require.True(t, plan.claimsContinuation(), "转换不得抹掉 previous_response_id 标记")

	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	_, convErr := apicompat.ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, convErr)
	// 孤儿批（previous_response_id 完全未登记）的拒绝发生在 **IdentifyResume** ——
	// 那是「这批结果没有任何已登记关联」的判定点（Execute 拿不到 ticket，
	// 无法区分孤儿与官方用法，故这道守卫只在 IdentifyResume）。
	// 生产实测（2026-11-07，版本 7f2eb50f0）：孤儿批 → HTTP 400 + CUR-RES-01。
	//
	// 本用例走完整路径（先 IdentifyResume 再 Execute），与生产一致：
	// 上一版直接调 Execute，绕过了这道判定 → 走到 s.now（测试未注入）而 panic，
	// 既测不到真实行为、又把「Execute 缺一道防御」误报成生产缺陷。
	ticket, identifyErr := svc.IdentifyResume(probe)
	require.ErrorIs(t, identifyErr, ErrCursorResumeUnknownAssociation,
		"孤儿批（previous_response_id 未登记）必须在 IdentifyResume 就拒绝")
	require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, identifyErr))
	require.Nil(t, ticket, "孤儿批不得拿到任何接管凭据")
	require.Zero(t, svc.sessions.sessionCount(), "不得创建 session/AgentRun")
	require.Zero(t, svc.sessions.indexCount(), "不得登记任何索引")
}

// TestCursorResumeExecuteAdmissionPlainRequestAllowed 覆盖：**不含**本批工具结果和
// previous_response_id 的原普通请求不受准入断言影响（历史中已完成工具对不构成本批续轮）。
func TestCursorResumeExecuteAdmissionPlainRequestAllowed(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore(), now: time.Now}
	probe, err := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst))
	require.NoError(t, err)
	require.False(t, probe.ClaimsContinuation(), "普通请求不声明续轮")

	plan := cursorResumePlanForTest(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model")
	require.False(t, plan.claimsContinuation(), "普通请求的计划也不携带续轮标记")

	var ccReq apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal([]byte(cursorResumeTestChatFirst), &ccReq))
	// 没有 forwarder 装配 → 走到出站阶段前的失败是装配错误，而不是未知关联拒绝。
	_, execErr := svc.Execute(context.Background(), nil, cursorResumeTestAccount(), &CursorExecutionRequest{
		Chat:     &ccReq,
		Model:    "cursor-test-model",
		Protocol: cursorProtocolChat,
		Identity: cursorResumeTestIdentity,
		Plan:     plan,
	})
	require.Error(t, execErr, "普通请求不被准入断言拒绝，但会因依赖未装配失败关闭")
	require.False(t, errors.Is(execErr, ErrCursorResumeUnknownAssociation),
		"普通请求不得被误判成未知关联")
}

// cursorResumeUnknownAssociationBody 返回「明确声称续轮但全部 ID 未命中**且历史不完整**」
// 的原始请求体（2026-11-04 起：完整历史会按重建/普通请求放行，不再属于本用例）。
//
// 四协议统一用"只有结果、没有对应调用"的孤儿形状：正文里没有 assistant tool_calls /
// tool_use / functionCall，也没有 function_call，因此 historyComplete=false，必须 400。
func cursorResumeUnknownAssociationBody(t *testing.T, protocol CursorResumeProtocol) []byte {
	t.Helper()
	switch protocol {
	case CursorResumeProtocolChat:
		return []byte(`{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`)
	case CursorResumeProtocolMessages:
		return []byte(`{
  "model": "cursor-test-model",
  "max_tokens": 64,
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "weather please"}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "sunny"}]}]}
  ],
  "tools": [{"name": "get_weather", "input_schema": {"type": "object"}}]
}`)
	case CursorResumeProtocolResponses:
		return []byte(`{
  "model": "cursor-test-model",
  "instructions": "be brief",
  "input": [{"type": "function_call_output", "call_id": "call_1", "output": "sunny"}],
  "tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
}`)
	case CursorResumeProtocolGemini:
		return []byte(`{
  "contents": [
    {"role": "user", "parts": [{"text": "weather please"}]},
    {"role": "user", "parts": [{"functionResponse": {"id": "call_1", "name": "get_weather", "response": {"result": "sunny"}}}]}
  ],
  "tools": [{"functionDeclarations": [{"name": "get_weather", "parameters": {"type": "object"}}]}]
}`)
	default:
		t.Fatalf("unsupported test protocol %q", protocol)
		return nil
	}
}

// TestCursorResumeRestartOrphanAllProtocolsRejected 覆盖 §25.17.4 与 2026-11-04 新裁定的
// 交界：「各 Cursor 协议的全未知结果（含重启后原 ID）且**历史不完整**」在 Identify 层必须
// 是可区分的 unknown_association，进入新建 Execute 入口后仍必须 400，AgentRun 与结果
// 写入计数均 0。正文携带完整历史的孤儿续轮则在准入层放行重建（见下一个用例）。
func TestCursorResumeRestartOrphanAllProtocolsRejected(t *testing.T) {
	protocols := []CursorResumeProtocol{
		CursorResumeProtocolChat,
		CursorResumeProtocolMessages,
		CursorResumeProtocolResponses,
		CursorResumeProtocolGemini,
	}
	for _, protocol := range protocols {
		t.Run(string(protocol), func(t *testing.T) {
			svc := &CursorExecutionService{sessions: newCursorSessionStore(), now: time.Now}
			body := cursorResumeUnknownAssociationBody(t, protocol)

			probe, err := CursorProbeResumeRequest(protocol, body)
			require.NoError(t, err)
			require.True(t, probe.ClaimsContinuation(), "请求必须明确声称续轮")

			ticket, identifyErr := svc.IdentifyResume(probe)
			require.Nil(t, ticket, "全部 ID 未命中时不得给出接管目标")
			require.ErrorIs(t, identifyErr, ErrCursorResumeUnknownAssociation,
				"重启后原 ID 必须是可区分的未知关联，不得当作普通新请求")
			require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, identifyErr))
			require.Zero(t, svc.sessions.sessionCount())
			require.Zero(t, svc.sessions.indexCount())

			// 同一探针计划进入新建 Execute（等价于 helper 未接管 + 原路由选中 Cursor 分支）。
			plan := cursorResumePlanForTest(t, protocol, body, "cursor-test-model")
			require.True(t, plan.claimsContinuation(), "转换不得抹掉续轮标记")
			require.False(t, plan.resultsReplayHistory(), "省略历史的仅结果形状不是回放")

			// 2026-11-07 五道闸退役：Execute 层的「历史不完整 → 400 unknown_association」
			// 守卫**不再存在**。
			//
			// 它为什么该退役：Responses 的官方用法就是「previous_response_id + 尾部
			// function_call_output」，客户端**不重放** function_call → 正文算不出
			// 「完整历史」→ 那道守卫恒定命中 → 官方用法被恒定拒成 CUR-RES-01
			// （生产近 7 天 57 次）。
			//
			// 它为什么**不该**原样恢复：Execute 手上只有 Plan（拿不到 ticket），
			// 分不清「孤儿批」（response_id 完全未登记 → 该拒）与
			// 「官方用法」（response_id 已登记、只是正文不带 function_call → 该放行）。
			// 恢复它 = 把官方用法又拒回去 = CUR-RES-01 复发。
			//
			// 孤儿批的拒绝由**上一层**保证：IdentifyResume 在「全部 call_id 未登记」时
			// 就返回可区分的 unknown_association（上面已断言，生产实测同样 400 + CUR-RES-01），
			// 生产路径 handler 先调 IdentifyResume 再决定是否进 Execute，
			// 孤儿批根本到不了 Execute。
			//
			// 所以这里断言的是：Execute 不再重复拦截（那道守卫已退役），
			// 而孤儿批的拒绝责任在 IdentifyResume（已由本用例前半段断言）。
			var ccReq apicompat.ChatCompletionsRequest
			require.NoError(t, json.Unmarshal([]byte(cursorResumeTestChatFirst), &ccReq))
			_, execErr := svc.Execute(context.Background(), nil, cursorResumeTestAccount(), &CursorExecutionRequest{
				Chat:     &ccReq,
				Model:    "cursor-test-model",
				Protocol: cursorResumeWriterProtocol(protocol),
				Identity: cursorResumeTestIdentity,
				Plan:     plan,
			})
			// Execute 不再因「历史不完整」拒绝（Execute 拿不到 ticket，分不清孤儿与官方用法）。
			// 孤儿批的拒绝在 IdentifyResume（前半段已断言）。
			require.NotErrorIs(t, execErr, ErrCursorResumeUnknownAssociation,
				"Execute 层不再重复拦截孤儿/官方用法（该判定属 IdentifyResume）")
		})
	}
}

// TestCursorResumeResponsesReplayHistoryIsOrdinaryRequest 覆盖 §25.17.4 行 1：
// 「历史中已完成工具对不单独构成本批续轮」——Responses 完整 input 回放里，尾部结果的
// function_call 也出现在同一正文中，客户端自带完整两端，不构成对本站待完成关联的声明，
// 因此按原普通请求继续（不判死、不接管），且不会被 Execute 准入断言误判成未知关联。
func TestCursorResumeResponsesReplayHistoryIsOrdinaryRequest(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore(), now: time.Now}
	body := []byte(cursorResumeTestResponsesResume)

	probe, err := CursorProbeResumeRequest(CursorResumeProtocolResponses, body)
	require.NoError(t, err)
	require.True(t, probe.ClaimsContinuation(), "尾部结果仍然构成续轮候选")

	plan := cursorResumePlanForTest(t, CursorResumeProtocolResponses, body, "cursor-test-model")
	require.True(t, plan.claimsContinuation())
	require.True(t, plan.resultsReplayHistory(),
		"结果 ID 的 function_call 就在同一正文里 = 历史中已完成工具对")

	ticket, identifyErr := svc.IdentifyResume(probe)
	require.NoError(t, identifyErr, "已完成工具对的回放不是未知关联")
	require.Nil(t, ticket, "没有已登记会话时不得接管")

	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal(body, &req))
	ccReq, convErr := apicompat.ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, convErr)
	// 没有 forwarder/鉴权依赖 → 走到出站前的装配错误；关键是**不是**未知关联拒绝。
	_, execErr := svc.Execute(context.Background(), nil, cursorResumeTestAccount(), &CursorExecutionRequest{
		Chat:     ccReq,
		Model:    "cursor-test-model",
		Protocol: cursorProtocolResponses,
		Identity: cursorResumeTestIdentity,
		Plan:     plan,
	})
	require.Error(t, execErr)
	require.False(t, errors.Is(execErr, ErrCursorResumeUnknownAssociation),
		"已完成工具对的回放不得被当成未知关联")
}

// TestCursorResumeResponsesUnpairedOutputRejected 钉住上面的例外边界：正文里**没有**对应
// function_call 的仅结果形状（重启孤儿 / 别家供应商的 ID）仍必须 400，不能借"回放"逃逸。
func TestCursorResumeResponsesUnpairedOutputRejected(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore(), now: time.Now}
	body := []byte(`{
  "model": "cursor-test-model",
  "instructions": "be brief",
  "input": [{"type": "function_call_output", "call_id": "call_orphan", "output": "sunny"}],
  "tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
}`)
	plan := cursorResumePlanForTest(t, CursorResumeProtocolResponses, body, "cursor-test-model")
	require.True(t, plan.claimsContinuation())
	require.False(t, plan.resultsReplayHistory(), "正文没有对应 function_call → 不是回放")

	probe, err := CursorProbeResumeRequest(CursorResumeProtocolResponses, body)
	require.NoError(t, err)
	_, identifyErr := svc.IdentifyResume(probe)
	require.ErrorIs(t, identifyErr, ErrCursorResumeUnknownAssociation)
	require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, identifyErr))

	// 混入"正文已声明的调用"也没用：另一条未配对结果仍然说明这是本站无法核验的关联。
	body = []byte(`{
  "model": "cursor-test-model",
  "instructions": "be brief",
  "input": [
    {"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\": \"hz\"}"},
    {"type": "function_call_output", "call_id": "call_1", "output": "sunny"},
    {"type": "function_call_output", "call_id": "call_orphan", "output": "rainy"}
  ],
  "tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
}`)
	mixedPlan := cursorResumePlanForTest(t, CursorResumeProtocolResponses, body, "cursor-test-model")
	require.False(t, mixedPlan.resultsReplayHistory(), "只要有一条结果未配对就不是回放")
	mixedProbe, probeErr := CursorProbeResumeRequest(CursorResumeProtocolResponses, body)
	require.NoError(t, probeErr)
	_, mixedErr := svc.IdentifyResume(mixedProbe)
	require.ErrorIs(t, mixedErr, ErrCursorResumeUnknownAssociation)
	require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, mixedErr))
	require.Zero(t, svc.sessions.sessionCount())
	require.Zero(t, svc.sessions.indexCount())
}

// ---------------------------------------------------------------------------
// T-8：交付侧 / 续轮侧摘要对称性（R5-1 缺陷的直接反例）
// ---------------------------------------------------------------------------
//
// 缺陷形态：交付侧登记 assistant 文本项时用**固定空文本**，而续轮侧按客户端回传的
// 正文重算前缀摘要 —— 同一逻辑内容两侧摘要必然不同，探针单独绿而全链路必然红。
//
// 反例断言（不依赖 HTTP/上游）：给定"已交付一批 + 客户端原样回传"，交付侧会话摘要必须
// 与续轮重算的前缀摘要**逐项相等**（四个协议各一条）。

// TestCursorDeliveredHistoryMatchesResumePrefix 锁死"交付侧 == 续轮前缀侧"摘要契约。
func TestCursorDeliveredHistoryMatchesResumePrefix(t *testing.T) {
	type testCase struct {
		name      string
		protocol  CursorResumeProtocol
		firstBody string
		// assistantText 是本次交付实际产出的 assistant 正文（续轮请求会原样回传）。
		assistantText string
		// resumeBody 按 assistantText 生成续轮请求体（含本批结果尾部）。
		resumeBody func(text string) string
		calls      []cursorToolCallCapture
	}

	weatherArgs := `{"city":"hz"}`
	chatResume := func(text string) string {
		return `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "` + text + `", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "` + `{\"city\": \"hz\"}` + `"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
	}
	messagesResume := func(text string) string {
		return `{
  "model": "cursor-test-model",
  "max_tokens": 64,
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "weather please"}]},
    {"role": "assistant", "content": [{"type": "text", "text": "` + text + `"}, {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": ` + weatherArgs + `}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "sunny"}]}]}
  ],
  "tools": [{"name": "get_weather", "input_schema": {"type": "object"}}]
}`
	}
	responsesResume := func(text string) string {
		return `{
  "model": "cursor-test-model",
  "instructions": "be brief",
  "input": [
    {"type": "message", "role": "user", "content": "weather please"},
    {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "` + text + `"}]},
    {"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "` + `{\"city\": \"hz\"}` + `"},
    {"type": "function_call_output", "call_id": "call_1", "output": "sunny"}
  ],
  "tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
}`
	}
	geminiFirst := `{
  "model": "cursor-test-model",
  "contents": [{"role": "user", "parts": [{"text": "weather please"}]}],
  "tools": [{"functionDeclarations": [{"name": "get_weather", "parameters": {"type": "object"}}]}]
}`
	geminiResume := func(text string) string {
		return `{
  "model": "cursor-test-model",
  "contents": [
    {"role": "user", "parts": [{"text": "weather please"}]},
    {"role": "model", "parts": [{"text": "` + text + `"}, {"functionCall": {"id": "call_1", "name": "get_weather", "args": ` + weatherArgs + `}}]},
    {"role": "user", "parts": [{"functionResponse": {"id": "call_1", "name": "get_weather", "response": {"result": "sunny"}}}]}
  ],
  "tools": [{"functionDeclarations": [{"name": "get_weather", "parameters": {"type": "object"}}]}]
}`
	}

	cases := []testCase{
		{
			name: "chat", protocol: CursorResumeProtocolChat,
			firstBody: cursorResumeTestChatFirst, assistantText: "checking the weather",
			resumeBody: chatResume,
			calls:      []cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: weatherArgs}},
		},
		{
			name: "messages", protocol: CursorResumeProtocolMessages,
			firstBody: cursorResumeTestMessagesFirst, assistantText: "checking the weather",
			resumeBody: messagesResume,
			calls:      []cursorToolCallCapture{{ID: "toolu_1", Name: "get_weather", ArgsJSON: weatherArgs}},
		},
		{
			name: "responses", protocol: CursorResumeProtocolResponses,
			firstBody: cursorResumeTestResponsesFirst, assistantText: "checking the weather",
			resumeBody: responsesResume,
			calls:      []cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: weatherArgs}},
		},
		{
			name: "gemini", protocol: CursorResumeProtocolGemini,
			firstBody: geminiFirst, assistantText: "checking the weather",
			resumeBody: geminiResume,
			calls:      []cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: weatherArgs}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 交付侧：首 HTTP 计划 + 按**实际正文**并入本批 assistant 输出（生产同一函数）。
			svc := &CursorExecutionService{sessions: newCursorSessionStore()}
			record := svc.sessions.start(cursorSessionSpec{
				protocol:       cursorResumeWriterProtocol(tc.protocol),
				requestedModel: "cursor-test-model",
				upstreamModel:  "cursor-upstream-cursor-test-model",
				accountID:      42,
				quotaKeyID:     7,
				indexProtocol:  string(tc.protocol),
				identity:       cursorResumeTestIdentity,
			})
			svc.sessions.attachOwner(record, svc)
			record.recordPlan(cursorResumeTestIdentity,
				cursorResumePlanForTest(t, tc.protocol, []byte(tc.firstBody), "cursor-test-model"))

			delegations := newLocalCursorToolDelegations()
			batch := &cursorToolBatch{modelCallID: "mc-1", sealed: true}
			for _, call := range tc.calls {
				batch.delegations = append(batch.delegations, call)
				batch.started = append(batch.started, call.ID)
			}
			delegations.mu.Lock()
			delegations.all = append(delegations.all, batch)
			delegations.mu.Unlock()
			// 会话必须绑定本轮委托记录（生产路径 Execute 里 session.attachDelegations 做同一件事）：
			// 续轮的 call_id 解析与归属判定要读它。
			record.attachDelegations(delegations)

			callIDs := make([]string, 0, len(tc.calls))
			for _, call := range tc.calls {
				callIDs = append(callIDs, call.ID)
			}
			require.NoError(t, appendCursorBatchAssistantHistory(
				record.historyDigest, tc.protocol, tc.assistantText, callIDs, delegations))

			// 续轮侧：客户端原样回传（正文 + 工具调用 + 本批结果）→ 前缀摘要须与交付侧相等。
			plan := cursorResumePlanForTest(t, tc.protocol, []byte(tc.resumeBody(tc.assistantText)), "cursor-test-model")
			require.Equal(t, record.historyDigest.ItemCount(), plan.history.ItemCount(),
				"交付侧与续轮前缀的逻辑项数必须一致（%s）", tc.name)
			require.True(t, record.historyDigest.Equal(plan.history),
				"交付侧摘要必须等于续轮前缀摘要（同一逻辑内容两侧必然一致；%s）", tc.name)

			// 反例：正文不同就必须不同（证明上面的相等不是"都退化成空文本"）。
			other := cursorResumePlanForTest(t, tc.protocol, []byte(tc.resumeBody("different text")), "cursor-test-model")
			require.False(t, record.historyDigest.Equal(other.history),
				"正文不同时摘要必须不同（两侧都真的纳入了正文；%s）", tc.name)
		})
	}
}

// TestCursorResumePrunedAssistantProseAccepted 是**生产 history_mismatch off-by-one**
// （stored 17 / request 16，恒差 1，全部 messages 协议）的回归用例。
//
// 交付侧登记了本批实际写出的 assistant 正文（"checking the weather"），而客户端在
// tool_use 轮按 Anthropic 语义**省略了 text 块**（只回传 tool_use + tool_result）。
// 修复前这必然被拒成 CUR-RES-04 history_mismatch；修复后按骨架口径放行，并落
// resume_history_prose_pruned 留痕。
func TestCursorResumePrunedAssistantProseAccepted(t *testing.T) {
	newFixture := func(t *testing.T) *cursorResumeFixture {
		t.Helper()
		svc := &CursorExecutionService{sessions: newCursorSessionStore()}
		record := svc.sessions.start(cursorSessionSpec{
			protocol:       cursorResumeWriterProtocol(CursorResumeProtocolMessages),
			requestedModel: "cursor-test-model",
			upstreamModel:  "cursor-upstream-cursor-test-model",
			accountID:      42,
			quotaKeyID:     7,
			indexProtocol:  string(CursorResumeProtocolMessages),
			identity:       cursorResumeTestIdentity,
		})
		svc.sessions.attachOwner(record, svc)
		record.recordPlan(cursorResumeTestIdentity,
			cursorResumePlanForTest(t, CursorResumeProtocolMessages, []byte(cursorResumeTestMessagesFirst), "cursor-test-model"))

		delegations := newLocalCursorToolDelegations()
		batch := &cursorToolBatch{modelCallID: "mc-1", sealed: true}
		batch.delegations = append(batch.delegations, cursorToolCallCapture{ID: "toolu_1", Name: "get_weather", ArgsJSON: `{"city": "hz"}`})
		batch.started = append(batch.started, "toolu_1")
		delegations.mu.Lock()
		delegations.all = append(delegations.all, batch)
		delegations.mu.Unlock()
		// 会话必须绑定本轮委托记录（生产路径 Execute 里 session.attachDelegations 做同一件事）：
		// 续轮的 call_id 解析与归属判定要读它。
		record.attachDelegations(delegations)

		// 关键：按生产同一路径交付，且**带正文**（这正是修复前造成 17 vs 16 的那一项）。
		prepared, err := svc.prepareDeliveredBatch(record, CursorResumeProtocolMessages, "cursor-test-model",
			"checking the weather", delegations, "")
		require.NoError(t, err)
		require.NoError(t, svc.commitDeliveredBatch(record, prepared, nil))

		intake := &cursorTestIntake{}
		require.NoError(t, svc.installIntake(record.SessionID(), intake))
		return &cursorResumeFixture{svc: svc, record: record, intake: intake,
			identity: cursorResumeTestIdentity, batchID: "mc-1"}
	}

	t.Run("客户端省略assistant正文仍接纳", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		ctx := logger.IntoContext(context.Background(), zap.New(core))
		fixture := newFixture(t)

		fixture.record.mu.Lock()
		storedItems := fixture.record.historyDigest.ItemCount()
		fixture.record.mu.Unlock()

		// cursorResumeTestMessagesResume 的 assistant 消息只有 tool_use，没有 text 块。
		_, err := fixture.resumeCtx(t, ctx, CursorResumeProtocolMessages, []byte(cursorResumeTestMessagesResume), false)
		require.NoError(t, err, "客户端裁剪 assistant 正文是合法历史管理行为，不得拒轮")
		require.Equal(t, 1, fixture.intake.callCount(), "必须整批写入上游一次")

		// 修复前那一项确实在存量里（证明用例真的复现了 off-by-one 的形态）。
		plan := cursorResumePlanForTest(t, CursorResumeProtocolMessages, []byte(cursorResumeTestMessagesResume), "cursor-test-model")
		require.Equal(t, storedItems, plan.history.ItemCount()+1, "存量比请求恒多 1 项（正文项），与原缺陷同形")

		entries := cursorPhaseEntries(logs, "resume_history_prose_pruned")
		require.Len(t, entries, 1, "正文裁剪必须留可统计的痕迹")
		fields := entries[0].ContextMap()
		require.Equal(t, int64(1), fields["prose_items_pruned"])
		require.Equal(t, string(CursorResumeProtocolMessages), fields["protocol"])
	})

	// 回归：2026-11-13 生产 CUR-RES-99（reason=tool_args_changed）—— 该 reason 已随旧契约
	// 整条下线（2026-11-14）。下线理由见 cursor_execution_service.go 续轮 serve 处的注释：
	// 客户端回传的 args 从不发给上游，上游早已按自己的参数执行完。
	t.Run("客户端回放改写历史工具参数不拦续轮", func(t *testing.T) {
		const historicalRewritten = `{
  "model": "cursor-test-model",
  "max_tokens": 64,
  "messages": [
    {"role": "user", "content": [{"type": "text", "text": "weather please"}]},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_old", "name": "get_weather", "input": {"city": "bj"}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_old", "content": [{"type": "text", "text": "cloudy"}]}]},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "hz"}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "sunny"}]}]}
  ],
  "tools": [{"name": "get_weather", "input_schema": {"type": "object"}}]
}`
		fixture := newFixture(t)
		// 给"上一轮"补一条交付基线：toolu_old 当初交付时参数是 {"city":"sh"}，
		// 请求正文回放时写成了 {"city":"bj"} —— 生产上客户端重建历史的真实形态。
		fixture.record.mu.Lock()
		delegations := fixture.record.delegations
		fixture.record.mu.Unlock()
		require.NotNil(t, delegations)
		delegations.mu.Lock()
		delegations.all = append(delegations.all, &cursorToolBatch{
			modelCallID: "mc-0", sealed: true,
			delegations: []cursorToolCallCapture{{ID: "toolu_old", Name: "get_weather", ArgsJSON: `{"city":"sh"}`}},
			started:     []string{"toolu_old"},
		})
		delegations.mu.Unlock()

		_, err := fixture.resume(t, CursorResumeProtocolMessages, []byte(historicalRewritten), false)
		require.NoError(t, err, "客户端回放改写历史参数不得拒绝（旧防篡改契约已下线）")
		require.Equal(t, 1, fixture.intake.callCount(), "本批结果必须照常整批写回上游一次")
	})
}
