//go:build unit

package service

// 本文件是 CHA-02 的定向测试：Responses 续轮三处同源（交付侧历史登记 / 续轮回传解析 /
// 结果条目类型选择）必须读同一套语义（§25.14-D「实际 ID」/ §25.17.2）。
//
// 每条用例都走「完整历史回放」形态：续轮请求体带 input（含已交付调用项 + 结果条目），
// 不只用 previous_response_id + 新结果（那样会绕过本次修的前缀比对，正是旧测试的洞）。
//
// 纯内存组件测试：无 DB、无网络；真实结果写入通道用替身 intake。

import (
	"context"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// --- 各用例复用的工具声明与首/续轮请求体（首轮与续轮 tools 必须完全一致）---

const cursorAlignResponsesUserMsg = `{"type":"message","role":"user","content":"run the task"}`

// cursorAlignNamespaceFirst 首轮声明 collaboration 命名空间子工具 spawn_agent。
const cursorAlignNamespaceFirst = `{
  "model": "cursor-test-model",
  "input": [` + cursorAlignResponsesUserMsg + `],
  "tools": [{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent"}]}]
}`

// cursorAlignNamespaceResume 续轮完整回放：原样回传 namespace 子工具调用 + 结果。
// 注意：客户端回传的是裸子工具名 + namespace 字段（与交付侧实际字节一致）。
const cursorAlignNamespaceResume = `{
  "model": "cursor-test-model",
  "input": [
    ` + cursorAlignResponsesUserMsg + `,
    {"type":"function_call","call_id":"call_1","name":"spawn_agent","namespace":"collaboration","arguments":"{}"},
    {"type":"function_call_output","call_id":"call_1","output":"spawned"}
  ],
  "tools": [{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent"}]}]
}`

// cursorAlignCustomFirst 首轮声明 custom 工具 exec。
const cursorAlignCustomFirst = `{
  "model": "cursor-test-model",
  "input": [` + cursorAlignResponsesUserMsg + `],
  "tools": [{"type":"custom","name":"exec","description":"freeform exec"}]
}`

// cursorAlignCustomResume 续轮完整回放：原样回传 custom_tool_call（input 为自由文本）。
const cursorAlignCustomResume = `{
  "model": "cursor-test-model",
  "input": [
    ` + cursorAlignResponsesUserMsg + `,
    {"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"text(42)"},
    {"type":"custom_tool_call_output","call_id":"call_1","output":"ran"}
  ],
  "tools": [{"type":"custom","name":"exec","description":"freeform exec"}]
}`

// cursorAlignCustomResumeAltInput 同一调用但不同 input 文本：必须得到不同摘要。
const cursorAlignCustomResumeAltInput = `{
  "model": "cursor-test-model",
  "input": [
    ` + cursorAlignResponsesUserMsg + `,
    {"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"text(999)"},
    {"type":"custom_tool_call_output","call_id":"call_1","output":"ran"}
  ],
  "tools": [{"type":"custom","name":"exec","description":"freeform exec"}]
}`

// cursorAlignToolSearchFirst 首轮声明 tool_search 服务端工具。
const cursorAlignToolSearchFirst = `{
  "model": "cursor-test-model",
  "input": [` + cursorAlignResponsesUserMsg + `],
  "tools": [{"type":"tool_search"}]
}`

// cursorAlignToolSearchResume 续轮完整回放：原样回传 tool_search_call + tool_search_output。
const cursorAlignToolSearchResume = `{
  "model": "cursor-test-model",
  "input": [
    ` + cursorAlignResponsesUserMsg + `,
    {"type":"tool_search_call","call_id":"call_1","arguments":{"query":"hello"}},
    {"type":"tool_search_output","call_id":"call_1","output":"hits"}
  ],
  "tools": [{"type":"tool_search"}]
}`

// cursorAlignCustomMismatchResume 交付的是 custom，但回传 function_call_output（类型不符）。
// 必须仍被拒（证明校验没被放宽）。
const cursorAlignCustomMismatchResume = `{
  "model": "cursor-test-model",
  "input": [
    ` + cursorAlignResponsesUserMsg + `,
    {"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"text(42)"},
    {"type":"function_call_output","call_id":"call_1","output":"ran"}
  ],
  "tools": [{"type":"custom","name":"exec","description":"freeform exec"}]
}`

// cursorAlignPlainFunctionFirst 首轮声明普通 function 工具 get_weather。
const cursorAlignPlainFunctionFirst = `{
  "model": "cursor-test-model",
  "input": [` + cursorAlignResponsesUserMsg + `],
  "tools": [{"type":"function","name":"get_weather","parameters":{"type":"object"}}]
}`

// cursorAlignPlainFunctionResume 普通 function 续轮（完整回放）。
const cursorAlignPlainFunctionResume = `{
  "model": "cursor-test-model",
  "input": [
    ` + cursorAlignResponsesUserMsg + `,
    {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"hz\"}"},
    {"type":"function_call_output","call_id":"call_1","output":"sunny"}
  ],
  "tools": [{"type":"function","name":"get_weather","parameters":{"type":"object"}}]
}`

// TestCursorResumeNamespaceToolHistoryAligned：namespace 子工具按实际交付语义（裸名 + namespace）
// 与续轮回传解析同源 → 续轮成功、上游写入 1。
func TestCursorResumeNamespaceToolHistoryAligned(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignNamespaceFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "collaboration__spawn_agent", ArgsJSON: "{}"}},
		"mc-1", "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignNamespaceResume), false)
	require.NoError(t, err, "namespace 子工具续轮必须成功（交付侧与回传解析必须同源）")
	require.Equal(t, 1, fixture.intake.callCount(), "真实结果必须整批写入一次")
	require.Equal(t, []string{"call_1"}, []string{fixture.intake.results[0][0].CallID})
	require.Equal(t, []string{"spawned"}, fixture.intake.results[0][0].Text)
}

// TestCursorResumeCustomToolInputAligned：custom 工具 input 原样回传 → 续轮成功、写入 1；
// 同一调用不同 input 必须得到不同摘要（证明输入不再坍缩）。
func TestCursorResumeCustomToolInputAligned(t *testing.T) {
	// 同一调用、不同 input：两条 fixture 各自独立，互不干扰。
	fixed := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "exec", ArgsJSON: "text(42)"}},
		"mc-1", "resp_1")
	_, err := fixed.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomResume), false)
	require.NoError(t, err, "custom 工具 input 原样回传必须成功")
	require.Equal(t, 1, fixed.intake.callCount())

	// 同一调用、不同 input（text(999)）：摘要必须不同 → 被拒，证明输入不再坍缩。
	other := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "exec", ArgsJSON: "text(42)"}},
		"mc-1", "resp_1")
	_, err = other.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomResumeAltInput), false)
	require.Error(t, err, "同一调用不同 input 必须得到不同摘要（不得坍缩成同一项）")
	require.Equal(t, 0, other.intake.callCount(), "摘要不符时零上游写入")
}

// TestCursorResumeToolSearchOutputTypeAligned：tool_search 调用 + tool_search_output 结果 → 续轮成功。
func TestCursorResumeToolSearchOutputTypeAligned(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignToolSearchFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "tool_search", ArgsJSON: `{"query":"hello"}`}},
		"mc-1", "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignToolSearchResume), false)
	require.NoError(t, err, "tool_search 续轮必须成功")
	require.Equal(t, 1, fixture.intake.callCount())
	require.Equal(t, []string{"call_1"}, []string{fixture.intake.results[0][0].CallID})
}

// TestCursorResumeResultTypeMismatchStillRejected：交付 custom 但回传 function_call_output
// 必须仍被拒（证明 history_mismatch / 结果校验未被放宽）。
func TestCursorResumeResultTypeMismatchStillRejected(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "exec", ArgsJSON: "text(42)"}},
		"mc-1", "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomMismatchResume), false)
	require.Error(t, err, "交付 custom 但回传 function_call_output 必须仍被拒")
	require.Equal(t, 0, fixture.intake.callCount(), "校验未通过时零上游写入")
}

// TestCursorResumePlainFunctionUnchanged：普通 function 续轮行为与改动前一致（成功、写入 1）。
func TestCursorResumePlainFunctionUnchanged(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignPlainFunctionFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city":"hz"}`}},
		"mc-1", "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignPlainFunctionResume), false)
	require.NoError(t, err, "普通 function 续轮必须成功（行为与改动前一致）")
	require.Equal(t, 1, fixture.intake.callCount())
	require.Equal(t, []string{"call_1"}, []string{fixture.intake.results[0][0].CallID})
	require.Equal(t, []string{"sunny"}, fixture.intake.results[0][0].Text)
}

// TestCursorResumeMissingDeliveredContextFailsClosedBeforeWrite：本卡核心断言。
// 即使 fixtures 正常填充了 deliveredToolCalls，只要它缺失（模拟结构性风险：未登记即跳过），
// 校验必须失败关闭、且发生在写上游（intake.DeliverToolResults）之前 → 零上游写入。
func TestCursorResumeMissingDeliveredContextFailsClosedBeforeWrite(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "exec", ArgsJSON: "text(42)"}},
		"mc-1", "resp_1")

	// 清空已交付工具上下文（在 record.mu 下置 nil），复现「未登记」场景。
	fixture.record.mu.Lock()
	fixture.record.deliveredToolCalls = nil
	fixture.record.mu.Unlock()

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomResume), false)
	require.Error(t, err, "缺少已交付工具上下文时必须失败关闭（未登记即失败关闭）")
	require.Contains(t, err.Error(), "delivered",
		"错误文本必须说明缺少已交付工具上下文")
	require.Contains(t, err.Error(), "result_type_mismatch",
		"失败关闭必须带 result_type_mismatch 状态码")
	require.Equal(t, 0, fixture.intake.callCount(), "校验未通过时零上游写入（核心断言：写上游之前就失败关闭）")
}

// --- 必修 1：一次交付跨多个已封口批时，deliveredToolCalls 必须覆盖全部已交付 call_id ---

// cursorResumeMultiBatch 描述一个已封口批及其委托调用。
type cursorResumeMultiBatch struct {
	batchID string
	calls   []cursorToolCallCapture
}

// newCursorResumeMultiBatchFixture 构造"首 HTTP 同一次交付多个已封口批、会话正在等结果"的夹具。
// 多个批同时进入 delegations.all，单次 recordDeliveredBatch 交付（模拟同一 HTTP 跨批交付）。
func newCursorResumeMultiBatchFixture(
	t *testing.T, protocol CursorResumeProtocol, firstBody []byte, model string,
	batches []cursorResumeMultiBatch, responseID string,
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
	for _, b := range batches {
		batch := &cursorToolBatch{modelCallID: b.batchID, sealed: true}
		for _, call := range b.calls {
			batch.delegations = append(batch.delegations, call)
			batch.started = append(batch.started, call.ID)
		}
		delegations.mu.Lock()
		delegations.all = append(delegations.all, batch)
		delegations.mu.Unlock()
	}

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
		batchID:  batches[len(batches)-1].batchID,
	}
}

// cursorAlignMultiBatchResume 续轮用 previous_response_id 省略历史，只回传 A/B 两个结果。
const cursorAlignMultiBatchResume = `{
  "model": "cursor-test-model",
  "previous_response_id": "resp_1",
  "input": [
    {"type":"function_call_output","call_id":"call_a","output":"ra"},
    {"type":"function_call_output","call_id":"call_b","output":"rb"}
  ],
  "tools": [{"type":"function","name":"get_weather","parameters":{"type":"object"}}]
}`

// TestCursorResumeMultiBatchAllDeliveredToolCallsAligned：两个已封口批 A/B 同一次交付，
// 续轮用 previous_response_id 同时提交 A/B 结果：必须完整写入，且不出现 result kind missing。
func TestCursorResumeMultiBatchAllDeliveredToolCallsAligned(t *testing.T) {
	fixture := newCursorResumeMultiBatchFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignPlainFunctionFirst), "cursor-test-model",
		[]cursorResumeMultiBatch{
			{batchID: "mc-a", calls: []cursorToolCallCapture{{ID: "call_a", Name: "get_weather", ArgsJSON: "{}"}}},
			{batchID: "mc-b", calls: []cursorToolCallCapture{{ID: "call_b", Name: "get_weather", ArgsJSON: "{}"}}},
		}, "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignMultiBatchResume), false)
	require.NoError(t, err, "A/B 两批同时交付后续轮必须成功（每个已交付 call_id 都能查到 kind）")
	require.Equal(t, 1, fixture.intake.callCount(), "真实结果必须整批写入一次")
	require.Len(t, fixture.intake.results[0], 2, "A/B 两个结果必须都写入")
	got := map[string]string{}
	for _, r := range fixture.intake.results[0] {
		got[r.CallID] = r.Text[0]
	}
	require.Equal(t, "ra", got["call_a"], "call_a 结果必须完整写入")
	require.Equal(t, "rb", got["call_b"], "call_b 结果必须完整写入")
}

// --- 必修 2：tool_search_output 仅含 tools（无 output）必须保留工具发现内容 ---

// cursorAlignToolSearchToolsOnlyResume 续轮回传 tool_search_output，只带顶层 tools、无 output。
// 注意：discoveries 内容必须出现在结果摘要里，绝不能坍缩成空字符串。
const cursorAlignToolSearchToolsOnlyResume = `{
  "model": "cursor-test-model",
  "input": [
    ` + cursorAlignResponsesUserMsg + `,
    {"type":"tool_search_call","call_id":"call_1","arguments":{"query":"hello"}},
    {"type":"tool_search_output","call_id":"call_1","tools":[{"type":"tool","name":"discovered_tool_a"},{"type":"tool","name":"discovered_tool_b"}]}
  ],
  "tools": [{"type":"tool_search"}]
}`

// TestCursorResumeToolSearchToolsOnlyOutputAligned：tool_search_output 仅有顶层 tools、无 output
// 时，必须完整回放成功，且结果/历史摘要包含工具发现内容（不得变成空字符串）。
func TestCursorResumeToolSearchToolsOnlyOutputAligned(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignToolSearchFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "tool_search", ArgsJSON: `{"query":"hello"}`}},
		"mc-1", "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignToolSearchToolsOnlyResume), false)
	require.NoError(t, err, "tool_search tools-only 结果必须完整回放成功")
	require.Equal(t, 1, fixture.intake.callCount())

	res := fixture.intake.results[0][0]
	require.Equal(t, "call_1", res.CallID)
	joined := res.Text[0]
	require.Contains(t, joined, "discovered_tool_a", "结果摘要必须含工具发现内容（不得为空）")
	require.Contains(t, joined, "discovered_tool_b", "结果摘要必须含工具发现内容（不得为空）")
}

// --- 必修 3：长 namespace/name 复用权威摊平（SHA-256 截断），三侧分类一致 ---

// cursorAlignLongNamespaceFirst 首轮声明一个超长 namespace 子工具（摊平后超过 64 字符）。
const cursorAlignLongNamespaceFirst = `{
  "model": "cursor-test-model",
  "input": [` + cursorAlignResponsesUserMsg + `],
  "tools": [{"type":"namespace","name":"very_long_namespace_prefix_for_testing_purposes","tools":[{"type":"function","name":"and_a_rather_long_tool_name_too"}]}]
}`

// cursorAlignLongNamespaceResume 续轮完整回放：裸子工具名 + namespace 字段（与交付侧一致）。
const cursorAlignLongNamespaceResume = `{
  "model": "cursor-test-model",
  "input": [
    ` + cursorAlignResponsesUserMsg + `,
    {"type":"function_call","call_id":"call_1","name":"and_a_rather_long_tool_name_too","namespace":"very_long_namespace_prefix_for_testing_purposes","arguments":"{}"},
    {"type":"function_call_output","call_id":"call_1","output":"spawned"}
  ],
  "tools": [{"type":"namespace","name":"very_long_namespace_prefix_for_testing_purposes","tools":[{"type":"function","name":"and_a_rather_long_tool_name_too"}]}]
}`

// TestCursorResumeNamespaceLongNameFlattenAligned：超长 namespace/name 摊平触发 SHA-256 截断，
// delivery（登记）/ history（历史规范化）/ replay（续轮回传解析）三侧必须用同一权威摊平函数，
// 分类一致 → 续轮成功、写入 1。
func TestCursorResumeNamespaceLongNameFlattenAligned(t *testing.T) {
	const ns = "very_long_namespace_prefix_for_testing_purposes"
	const child = "and_a_rather_long_tool_name_too"
	// 权威摊平：超长时带 __ + 4 字符 sha256 后缀，恰好 64 字符。
	flat := apicompat.FlattenNamespaceToolName(ns, child)
	require.Len(t, flat, 64, "超长 namespace/name 摊平必须截断到 64 字符并带 sha256 后缀")

	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignLongNamespaceFirst), "cursor-test-model",
		// 交付侧实际写出的 wire 名是摊平名（可能已截断）。
		[]cursorToolCallCapture{{ID: "call_1", Name: flat, ArgsJSON: "{}"}},
		"mc-1", "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignLongNamespaceResume), false)
	require.NoError(t, err, "长 namespace/name 续轮必须成功（三侧摊平分类一致）")
	require.Equal(t, 1, fixture.intake.callCount())
	require.Equal(t, []string{"call_1"}, []string{fixture.intake.results[0][0].CallID})
	require.Equal(t, []string{"spawned"}, fixture.intake.results[0][0].Text)
}

// --- 必修 4：普通 function_call.arguments 非法/截断 JSON 必须失败关闭（零上游写入） ---

// cursorAlignPlainFunctionInvalidArgsResume 续轮回传的 function_call arguments 非法 JSON
// （语法错误，非合法 JSON 值）。
const cursorAlignPlainFunctionInvalidArgsResume = `{
  "model": "cursor-test-model",
  "input": [
    ` + cursorAlignResponsesUserMsg + `,
    {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\": }"},
    {"type":"function_call_output","call_id":"call_1","output":"sunny"}
  ],
  "tools": [{"type":"function","name":"get_weather","parameters":{"type":"object"}}]
}`

// cursorAlignPlainFunctionTruncatedArgsResume 续轮回传的 function_call arguments 截断 JSON。
const cursorAlignPlainFunctionTruncatedArgsResume = `{
  "model": "cursor-test-model",
  "input": [
    ` + cursorAlignResponsesUserMsg + `,
    {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"hz\""},
    {"type":"function_call_output","call_id":"call_1","output":"sunny"}
  ],
  "tools": [{"type":"function","name":"get_weather","parameters":{"type":"object"}}]
}`

// resumeExpectFail 走与生产相同的路径（探针 → 索引定位 → 接管 + 校验），但容忍任一阶段
// 失败关闭：只要探针/接管任一环节报错即视为失败关闭，并断言零上游写入。
func (f *cursorResumeFixture) resumeExpectFail(t *testing.T, protocol CursorResumeProtocol, body []byte) error {
	t.Helper()
	probe, err := CursorProbeResumeRequest(protocol, body)
	if err != nil {
		require.Equal(t, 0, f.intake.callCount(), "失败关闭时零上游写入")
		return err
	}
	ticket, err := f.svc.IdentifyResume(probe)
	if err != nil {
		require.Equal(t, 0, f.intake.callCount(), "失败关闭时零上游写入")
		return err
	}
	if ticket == nil {
		require.Equal(t, 0, f.intake.callCount(), "失败关闭时零上游写入")
		return fmt.Errorf("nil ticket")
	}
	_, err = f.svc.ResumeCursorSession(context.Background(), nil, ticket, cursorResumeTestAccount(), CursorResumeServeInput{
		Body:     body,
		Stream:   false,
		Identity: f.identity,
	})
	require.Equal(t, 0, f.intake.callCount(), "失败关闭时零上游写入")
	return err
}

// TestCursorResumeFunctionCallInvalidJSONRejected：普通 function_call arguments 非法 JSON
// 必须失败关闭，绝不包装成字符串继续摘要（零上游写入）。
func TestCursorResumeFunctionCallInvalidJSONRejected(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignPlainFunctionFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city":"hz"}`}},
		"mc-1", "resp_1")

	err := fixture.resumeExpectFail(t, CursorResumeProtocolResponses, []byte(cursorAlignPlainFunctionInvalidArgsResume))
	require.Error(t, err, "普通 function_call 非法 JSON arguments 必须失败关闭")
	require.Equal(t, 0, fixture.intake.callCount(), "失败关闭时零上游写入")
}

// TestCursorResumeFunctionCallTruncatedJSONRejected：普通 function_call arguments 截断 JSON
// 必须失败关闭（零上游写入）。
func TestCursorResumeFunctionCallTruncatedJSONRejected(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignPlainFunctionFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "get_weather", ArgsJSON: `{"city":"hz"}`}},
		"mc-1", "resp_1")

	err := fixture.resumeExpectFail(t, CursorResumeProtocolResponses, []byte(cursorAlignPlainFunctionTruncatedArgsResume))
	require.Error(t, err, "普通 function_call 截断 JSON arguments 必须失败关闭")
	require.Equal(t, 0, fixture.intake.callCount(), "失败关闭时零上游写入")
}

// --- 外审 P1-2：省略历史路径也要跑结果类型校验 ---

// cursorAlignCustomOmittedMismatchResume 走 previous_response_id 省略历史，但把已交付的
// custom 调用结果写成 function_call_output（跨类型伪装）。省略历史不再豁免类型校验（§7 矩阵），
// 必须失败关闭、零上游写入。
const cursorAlignCustomOmittedMismatchResume = `{
  "model": "cursor-test-model",
  "previous_response_id": "resp_1",
  "input": [{"type":"function_call_output","call_id":"call_1","output":"ran"}],
  "tools": [{"type":"custom","name":"exec","description":"freeform exec"}]
}`

// cursorAlignCustomOmittedValidResume 同样的省略历史路径，但结果类型正确
// （custom_tool_call_output）。必须成功、写入 1（防止 P1-2 改动把合法请求也拒了）。
const cursorAlignCustomOmittedValidResume = `{
  "model": "cursor-test-model",
  "previous_response_id": "resp_1",
  "input": [{"type":"custom_tool_call_output","call_id":"call_1","output":"ran"}],
  "tools": [{"type":"custom","name":"exec","description":"freeform exec"}]
}`

// TestCursorResumeOmittedHistoryStillValidatesResultType：省略历史路径下，已交付 custom 但
// 回传 function_call_output 必须仍被拒（证明校验不因省略历史而豁免），且发生在写入上游之前。
func TestCursorResumeOmittedHistoryStillValidatesResultType(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "exec", ArgsJSON: "text(42)"}},
		"mc-1", "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomOmittedMismatchResume), false)
	require.Error(t, err, "省略历史路径下，交付 custom 却回传 function_call_output 必须仍被拒")
	require.Contains(t, err.Error(), "result_type_mismatch",
		"失败关闭必须带 result_type_mismatch 状态码（类型校验不因省略历史而豁免）")
	require.Equal(t, 0, fixture.intake.callCount(), "校验未通过时零上游写入")
}

// TestCursorResumeOmittedHistoryValidResultTypeStillSucceeds：省略历史路径下，结果类型正确
// （custom_tool_call_output）必须成功、写入 1，证明 P1-2 改动只收紧伪装、不误伤合法请求。
func TestCursorResumeOmittedHistoryValidResultTypeStillSucceeds(t *testing.T) {
	fixture := newCursorResumeFixture(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomFirst), "cursor-test-model",
		[]cursorToolCallCapture{{ID: "call_1", Name: "exec", ArgsJSON: "text(42)"}},
		"mc-1", "resp_1")

	_, err := fixture.resume(t, CursorResumeProtocolResponses, []byte(cursorAlignCustomOmittedValidResume), false)
	require.NoError(t, err, "省略历史路径下，结果类型正确的 custom 续轮必须成功（不得误伤合法请求）")
	require.Equal(t, 1, fixture.intake.callCount(), "真实结果必须整批写入一次")
	require.Equal(t, []string{"call_1"}, []string{fixture.intake.results[0][0].CallID})
}
