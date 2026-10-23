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
	"github.com/stretchr/testify/require"
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
	probe, err := CursorProbeResumeRequest(protocol, body)
	require.NoError(t, err)
	ticket, err := f.svc.IdentifyResume(probe)
	require.NoError(t, err)
	require.NotNil(t, ticket, "探针必须定位到唯一待完成批")
	return f.svc.ResumeCursorSession(context.Background(), nil, ticket, cursorResumeTestAccount(), CursorResumeServeInput{
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

// TestCursorResumeBigIntegerPrecision 覆盖清单第 2 条：大整数精度不失真。
func TestCursorResumeBigIntegerPrecision(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"n": 9007199254740993}`}}, "mc-1", "")

	const roundedArg = `{
  "model": "cursor-test-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"n\": 9007199254740992}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`
	_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(roundedArg), false)
	require.ErrorIs(t, err, ErrCursorResumeHistoryMismatch, "相邻大整数不得被折叠成同一个值")
	require.Equal(t, 0, fixture.intake.callCount())
}

// TestCursorResumeConfigChangesRejected 覆盖清单第 3 条：改配置各项零上游写入拒绝。
func TestCursorResumeConfigChangesRejected(t *testing.T) {
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
			name: "model_changed",
			body: `{
  "model": "cursor-other-model",
  "messages": [
    {"role": "user", "content": "weather please"},
    {"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"hz\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
}`,
		},
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
			_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(tc.body), false)
			require.Error(t, err, "改固定生成配置必须拒绝")
			require.Equal(t, 0, fixture.intake.callCount(), "拒绝必须零上游写入")
			require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, err))
		})
	}
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
	_, err := fixture.resume(t, CursorResumeProtocolChat, []byte(rewrittenUser), false)
	require.ErrorIs(t, err, ErrCursorResumeHistoryMismatch)
	require.Equal(t, 0, fixture.intake.callCount())
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
	_, err := CursorProbeResumeRequest(CursorResumeProtocolGemini, []byte(geminiBody))
	require.ErrorIs(t, err, ErrCursorResumeAmbiguousResults, "同名并行缺 ID 必须明确拒绝歧义")
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

// TestCursorResumeExecuteAdmissionAllUnknownResults 覆盖：请求携带本批工具结果（全部未知，
// 含重启孤儿），进入 Cursor 执行分支时必须在模型出站/建 session/转交资源之前 400，
// AgentRun=0 且零结果写入。
func TestCursorResumeExecuteAdmissionAllUnknownResults(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore()}
	probe, err := CursorProbeResumeRequest(CursorResumeProtocolChat, []byte(cursorResumeTestChatResume))
	require.NoError(t, err)
	require.True(t, probe.ClaimsContinuation(), "请求必须明确声称续轮")

	// 同一探针计划进入新建 Execute 入口（等价于 helper 未接管、原路由选中 Cursor 分支）。
	plan := cursorResumePlanForTest(t, CursorResumeProtocolChat, []byte(cursorResumeTestChatResume), "cursor-test-model")
	require.True(t, plan.claimsContinuation(), "转换不得抹掉续轮标记")

	var ccReq apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal([]byte(cursorResumeTestChatResume), &ccReq))
	_, execErr := svc.Execute(context.Background(), nil, cursorResumeTestAccount(), &CursorExecutionRequest{
		Chat:     &ccReq,
		Model:    "cursor-test-model",
		Protocol: cursorProtocolChat,
		Identity: cursorResumeTestIdentity,
		Plan:     plan,
	})
	require.ErrorIs(t, execErr, ErrCursorResumeUnknownAssociation,
		"声明续轮却无有效接管必须返回失效关联哨兵")
	require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, execErr))
	require.Zero(t, svc.sessions.sessionCount(), "不得创建 session/AgentRun")
	require.Zero(t, svc.sessions.indexCount(), "不得登记任何索引")
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
	ccReq, convErr := apicompat.ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, convErr)
	_, execErr := svc.Execute(context.Background(), nil, cursorResumeTestAccount(), &CursorExecutionRequest{
		Chat:     ccReq,
		Model:    "cursor-test-model",
		Protocol: cursorProtocolResponses,
		Identity: cursorResumeTestIdentity,
		Plan:     plan,
	})
	require.ErrorIs(t, execErr, ErrCursorResumeUnknownAssociation)
	require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, execErr))
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

// cursorResumeUnknownAssociationBody 返回「明确声称续轮但全部 ID 未命中」的原始请求体。
//
// 四协议各自用它自己的续轮表示携带本批工具结果（Responses 用**省略历史**的仅结果形状：
// 只有 function_call_output，正文里没有对应 function_call，因此不构成"历史中已完成工具对"）。
func cursorResumeUnknownAssociationBody(t *testing.T, protocol CursorResumeProtocol) []byte {
	t.Helper()
	switch protocol {
	case CursorResumeProtocolChat:
		return []byte(cursorResumeTestChatResume)
	case CursorResumeProtocolMessages:
		return []byte(cursorResumeTestMessagesResume)
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
    {"role": "model", "parts": [{"functionCall": {"id": "call_1", "name": "get_weather", "args": {"city": "hz"}}}]},
    {"role": "user", "parts": [{"functionResponse": {"id": "call_1", "name": "get_weather", "response": {"result": "sunny"}}}]}
  ],
  "tools": [{"functionDeclarations": [{"name": "get_weather", "parameters": {"type": "object"}}]}]
}`)
	default:
		t.Fatalf("unsupported test protocol %q", protocol)
		return nil
	}
}

// TestCursorResumeRestartOrphanAllProtocolsRejected 覆盖 §25.17.4「必须证明」的
// 「各 Cursor 协议的全未知结果（含重启后原 ID）均 400，AgentRun 与结果写入计数均 0」：
// 用**空的会话表**模拟重启后孤儿——原样重发各协议的续轮体，Identify 必须返回可区分哨兵，
// 即便被原路由送进新建 Execute 入口，也必须在模型出站/建 session/转交资源之前 400。
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

			var ccReq apicompat.ChatCompletionsRequest
			require.NoError(t, json.Unmarshal([]byte(cursorResumeTestChatFirst), &ccReq))
			_, execErr := svc.Execute(context.Background(), nil, cursorResumeTestAccount(), &CursorExecutionRequest{
				Chat:     &ccReq,
				Model:    "cursor-test-model",
				Protocol: cursorResumeWriterProtocol(protocol),
				Identity: cursorResumeTestIdentity,
				Plan:     plan,
			})
			require.ErrorIs(t, execErr, ErrCursorResumeUnknownAssociation,
				"声明续轮却无有效接管必须在 Cursor 准入边界 400")
			require.Equal(t, http.StatusBadRequest, mustCursorResumeStatus(t, execErr))
			require.Zero(t, svc.sessions.sessionCount(), "不得创建 session/AgentRun")
			require.Zero(t, svc.sessions.indexCount(), "不得登记任何索引")
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
