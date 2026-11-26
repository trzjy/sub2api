//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// TestCursorSemanticProtocolDecoderMatrix 钉死四协议“原始字段 → 语义序列”的唯一解码：
// 规范角色、源顺序、源容器索引（消息数组零基 / 顶层指令 -1）、工具调用与结果的
// ID / 内容 / 错误标记全部保真；同一原容器展开出的语义项共享索引与完整性标记。
func TestCursorSemanticProtocolDecoderMatrix(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		req := &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
			{Role: "system", Content: json.RawMessage(`"SYS"`)},
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "assistant", ToolCalls: []apicompat.ChatToolCall{{
				ID: "call_1", Type: "function",
				Function: apicompat.ChatFunctionCall{Name: "lookup", Arguments: `{}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"sunny"`), IsError: true},
		}}
		sem := cursorDecodeChatSemantic(req)
		require.Len(t, sem, 4)
		require.Equal(t, []string{cursorSemanticInstruction, cursorSemanticUser, cursorSemanticAssistant, cursorSemanticTool},
			[]string{sem[0].role, sem[1].role, sem[2].role, sem[3].role})
		for i := range sem {
			require.Equal(t, i, sem[i].sourceContainerIndex, "Chat 消息下标即源容器下标")
		}
		require.Equal(t, "call_1", sem[2].raw.ToolCalls[0].ID, "assistant 工具调用 ID 保真")
		require.Equal(t, cursorContainerMixed, sem[2].container)
		require.Equal(t, "call_1", sem[3].raw.ToolCallID, "工具结果 call ID 保真")
		require.True(t, sem[3].raw.IsError, "工具结果错误标记保真")
		require.Equal(t, cursorContainerAllToolResults, sem[3].container)
	})

	t.Run("anthropic messages", func(t *testing.T) {
		req := &apicompat.AnthropicRequest{
			System: json.RawMessage(`"SYS"`),
			Messages: []apicompat.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"q"}]`)},
				{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"call_a","name":"lookup","input":{}}]`)},
				{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_a","content":[{"type":"text","text":"sunny"}],"is_error":true},{"type":"tool_result","tool_use_id":"call_b","content":"rainy"}]`)},
			},
		}
		sem, err := cursorDecodeMessagesSemantic(req)
		require.NoError(t, err)
		require.Len(t, sem, 5)
		require.Equal(t, cursorSemanticInstruction, sem[0].role)
		require.Equal(t, -1, sem[0].sourceContainerIndex, "顶层 system 是前导容器 -1")
		require.Equal(t, 0, sem[1].sourceContainerIndex, "消息数组从 0 起算，不被 -1 平移")
		require.Less(t, sem[0].sourceContainerIndex, sem[1].sourceContainerIndex)
		require.Equal(t, cursorSemanticUser, sem[1].role)
		require.Equal(t, cursorSemanticAssistant, sem[2].role)
		require.Equal(t, "call_a", sem[2].raw.ToolCalls[0].ID)
		// 同一原容器展开出的两个 tool_result 共享索引 2 与纯结果标记。
		require.Equal(t, cursorSemanticTool, sem[3].role)
		require.Equal(t, 2, sem[3].sourceContainerIndex)
		require.Equal(t, 2, sem[4].sourceContainerIndex)
		require.Equal(t, cursorContainerAllToolResults, sem[3].container)
		require.Equal(t, cursorContainerAllToolResults, sem[4].container)
		require.Equal(t, "call_a", sem[3].raw.ToolCallID)
		require.True(t, sem[3].raw.IsError)
		require.Equal(t, "call_b", sem[4].raw.ToolCallID)
		require.False(t, sem[4].raw.IsError)

		emptyContainerReq := &apicompat.AnthropicRequest{Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_a","content":"a"}]`)},
			{Role: "assistant", Content: json.RawMessage(`[]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_b","content":"b"}]`)},
		}}
		emptySem, err := cursorDecodeMessagesSemantic(emptyContainerReq)
		require.NoError(t, err)
		require.Len(t, emptySem, 3, "空内容源容器必须保留 Mixed 占位")
		emptyAnalysis := cursorAnalyzeSemanticMessages(emptySem)
		require.Len(t, emptyAnalysis.results, 1)
		require.Equal(t, "call_b", emptyAnalysis.results[0].callID)
	})

	t.Run("responses", func(t *testing.T) {
		req := &apicompat.ResponsesRequest{
			Instructions: "SYS",
			Input: json.RawMessage(`[
				{"type":"message","role":"user","content":"q"},
				{"type":"function_call","call_id":"call_r","name":"lookup","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_r","output":"sunny"}
			]`),
		}
		sem, err := cursorDecodeResponsesSemantic(req)
		require.NoError(t, err)
		require.Len(t, sem, 4)
		require.Equal(t, cursorSemanticInstruction, sem[0].role)
		require.Equal(t, -1, sem[0].sourceContainerIndex, "顶层 instructions 是前导容器 -1")
		require.Equal(t, 0, sem[1].sourceContainerIndex)
		require.Equal(t, cursorSemanticUser, sem[1].role)
		require.Equal(t, cursorSemanticAssistant, sem[2].role)
		require.Equal(t, "call_r", sem[2].raw.ToolCalls[0].ID)
		require.Equal(t, cursorSemanticTool, sem[3].role)
		require.Equal(t, 2, sem[3].sourceContainerIndex)
		require.Equal(t, "call_r", sem[3].raw.ToolCallID)
		require.Equal(t, cursorContainerAllToolResults, sem[3].container)
		// ResultType 由 plan 按源容器下标映射回原始 item，不靠 call ID 猜测。
		plan, err := cursorPlanFromResponses(req, "cursor-test-model")
		require.NoError(t, err)
		require.Len(t, plan.results, 1)
		require.Equal(t, "function_call_output", plan.results[0].ResultType)
		require.Equal(t, "call_r", plan.results[0].CallID)

		withDeclaration := &apicompat.ResponsesRequest{
			Input: json.RawMessage(`[
				{"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_old","output":"sunny"},
				{"type":"additional_tools","tools":[]}
			]`),
		}
		declarationSem, err := cursorDecodeResponsesSemantic(withDeclaration)
		require.NoError(t, err)
		declarationAnalysis := cursorAnalyzeSemanticMessages(declarationSem)
		require.Empty(t, declarationAnalysis.results, "a declaration/ordinary item after a result stops the shared tail analyzer")

		withSystemRole := []byte(`{"contents":[{"role":"system","parts":[{"text":"inline instruction"}]}]}`)
		systemSem, err := cursorDecodeGeminiSemantic(withSystemRole)
		require.NoError(t, err)
		require.Len(t, systemSem, 1)
		require.Equal(t, cursorSemanticInstruction, systemSem[0].role)
	})

	t.Run("gemini native", func(t *testing.T) {
		body := []byte(`{
			"systemInstruction":{"parts":[{"text":"SYS"}]},
			"contents":[
				{"role":"user","parts":[{"text":"q"}]},
				{"role":"model","parts":[{"functionCall":{"id":"call_g","name":"lookup","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"id":"call_g","name":"lookup","response":{"result":"sunny"}}}]}
			]
		}`)
		sem, err := cursorDecodeGeminiSemantic(body)
		require.NoError(t, err)
		require.Len(t, sem, 4)
		require.Equal(t, cursorSemanticInstruction, sem[0].role)
		require.Equal(t, -1, sem[0].sourceContainerIndex, "systemInstruction 是前导容器 -1")
		require.Equal(t, 0, sem[1].sourceContainerIndex)
		require.Equal(t, cursorSemanticUser, sem[1].role)
		require.Equal(t, cursorSemanticAssistant, sem[2].role, "model 源角色是 assistant，不从桥接输出反推")
		require.Equal(t, "call_g", sem[2].raw.ToolCalls[0].ID)
		require.Equal(t, cursorSemanticTool, sem[3].role)
		require.Equal(t, 2, sem[3].sourceContainerIndex)
		require.Equal(t, "call_g", sem[3].raw.ToolCallID)
		require.Equal(t, cursorContainerAllToolResults, sem[3].container)

		emptyBody := []byte(`{"contents":[
			{"role":"user","parts":[{"functionResponse":{"id":"call_a","response":{"result":"a"}}}]},
			{"role":"model","parts":[]},
			{"role":"user","parts":[{"functionResponse":{"id":"call_b","response":{"result":"b"}}}]}
		]}`)
		emptyGeminiSem, err := cursorDecodeGeminiSemantic(emptyBody)
		require.NoError(t, err)
		require.Len(t, emptyGeminiSem, 3, "空 parts 源容器必须保留 Mixed 占位")
		emptyGeminiAnalysis := cursorAnalyzeSemanticMessages(emptyGeminiSem)
		require.Len(t, emptyGeminiAnalysis.results, 1)
		require.Equal(t, "call_b", emptyGeminiAnalysis.results[0].callID)
	})
}

// TestCursorSemanticTailAnalysisMatrix 覆盖唯一尾部分析器的分组/截断边界：
// instruction 可跨越、完整结果容器保序收集；普通 user/assistant/混合/unknown 立即截断。
func TestCursorSemanticTailAnalysisMatrix(t *testing.T) {
	tool := func(idx int, id string) cursorSemanticMessage {
		return cursorSemanticMessage{
			role: cursorSemanticTool, container: cursorContainerAllToolResults,
			sourceContainerIndex: idx,
			raw:                  apicompat.ChatMessage{Role: "tool", ToolCallID: id},
		}
	}
	instruction := func(idx int) cursorSemanticMessage {
		return cursorSemanticMessage{
			role: cursorSemanticInstruction, sourceContainerIndex: idx,
			raw: apicompat.ChatMessage{Role: "system"},
		}
	}
	user := func(idx int) cursorSemanticMessage {
		return cursorSemanticMessage{
			role: cursorSemanticUser, container: cursorContainerMixed, sourceContainerIndex: idx,
			raw: apicompat.ChatMessage{Role: "user"},
		}
	}
	assistant := func(idx int) cursorSemanticMessage {
		return cursorSemanticMessage{
			role: cursorSemanticAssistant, container: cursorContainerMixed, sourceContainerIndex: idx,
			raw: apicompat.ChatMessage{Role: "assistant"},
		}
	}
	mixed := func(idx int) cursorSemanticMessage {
		m := user(idx)
		m.container = cursorContainerMixed
		return m
	}
	unknown := func(idx int) cursorSemanticMessage {
		return cursorSemanticMessage{
			role: cursorSemanticUnknown, container: cursorContainerMixed, sourceContainerIndex: idx,
			raw: apicompat.ChatMessage{Role: "critic"},
		}
	}
	ids := func(a cursorSemanticAnalysis) []string {
		out := make([]string, 0, len(a.results))
		for _, r := range a.results {
			out = append(out, r.callID)
		}
		return out
	}

	cases := []struct {
		name    string
		sem     []cursorSemanticMessage
		wantIDs []string
	}{
		{name: "结果后跟指令", sem: []cursorSemanticMessage{tool(0, "c1"), instruction(-1)}, wantIDs: []string{"c1"}},
		{name: "指令后跟结果", sem: []cursorSemanticMessage{instruction(-1), tool(0, "c1")}, wantIDs: []string{"c1"}},
		{name: "多结果容器保序", sem: []cursorSemanticMessage{tool(0, "c1"), tool(1, "c2"), tool(2, "c3")}, wantIDs: []string{"c1", "c2", "c3"}},
		{name: "同容器多结果保序", sem: []cursorSemanticMessage{tool(2, "c1"), tool(2, "c2")}, wantIDs: []string{"c1", "c2"}},
		{name: "结果后普通用户截断", sem: []cursorSemanticMessage{tool(0, "c1"), user(1)}, wantIDs: nil},
		{name: "结果后助手截断", sem: []cursorSemanticMessage{tool(0, "c1"), assistant(1)}, wantIDs: nil},
		{name: "结果后混合正文容器截断", sem: []cursorSemanticMessage{tool(0, "c1"), mixed(1)}, wantIDs: nil},
		{name: "结果后未知容器截断", sem: []cursorSemanticMessage{tool(0, "c1"), unknown(1)}, wantIDs: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(cursorAnalyzeSemanticMessages(tc.sem))
			if len(tc.wantIDs) == 0 {
				require.Empty(t, got)
			} else {
				require.Equal(t, tc.wantIDs, got)
			}
		})
	}
}

func TestCursorMessagesPlanPreservesOfficialSystemMessage(t *testing.T) {
	req := &apicompat.AnthropicRequest{
		Model: "cursor-test-model",
		Messages: []apicompat.AnthropicMessage{
			{Role: "system", Content: json.RawMessage(`"Follow the repository instructions."`)},
			{Role: "user", Content: json.RawMessage(`"inspect the code"`)},
		},
	}

	plan, err := cursorPlanFromMessages(req, req.Model)
	require.NoError(t, err)
	require.Equal(t, 1, plan.history.ItemCount())
	withoutSystem := *req
	withoutSystem.Messages = withoutSystem.Messages[1:]
	baseline, err := cursorPlanFromMessages(&withoutSystem, req.Model)
	require.NoError(t, err)
	// system 正文属**软组**（configSystem）：它进固定配置摘要、不进用户历史，但续轮对软组
	// 只留痕不拒绝（见 ResumeCursorSession 的分组说明）。硬组（模型 + 工具声明/schema）在
	// 这一对请求里完全相同——这正是"改 system 不再把正常续轮拒成 CUR-RES-04"的依据。
	require.NotEqual(t, baseline.configSystem.HexString(), plan.configSystem.HexString(),
		"system message belongs to the soft config group")
	require.Equal(t, baseline.config.HexString(), plan.config.HexString(),
		"system message must not enter the hard config group")
	require.Equal(t, baseline.history.HexString(), plan.history.HexString(), "system message must not enter user history")
}

// 产品口径（2026-11-06）：认得类型但内容畸形（role 不认识）不得整轮拒——只丢这一条，
// 把本轮做完。客户看到的 400 会被理解成"模型不稳定"，代价远大于少一条消息。
func TestCursorResponsesPlanSkipsUnknownMessageRole(t *testing.T) {
	req := &apicompat.ResponsesRequest{
		Model: "cursor-test-model",
		Input: json.RawMessage(`[{"type":"message","role":"critic","content":"do not demote me"}]`),
	}

	plan, err := cursorPlanFromResponses(req, req.Model)
	require.NoError(t, err, "畸形 message 只丢这一条，不得整轮拒")
	require.Equal(t, 1, plan.skippedItems["message"])
	require.Equal(t, 0, plan.history.ItemCount())
}

// 旁支条目（模型自己联网搜的痕迹）本来就从不转发，回放时丢弃即可，不整轮拒。
func TestCursorResponsesPlanSkipsHostedSearchItem(t *testing.T) {
	req := &apicompat.ResponsesRequest{
		Model: "cursor-test-model",
		Input: json.RawMessage(`[{"type":"web_search_call","id":"ws_1"}]`),
	}

	plan, err := cursorPlanFromResponses(req, req.Model)
	require.NoError(t, err, "web_search_call 必须被丢弃而不是整轮拒")
	require.Equal(t, 1, plan.skippedItems["web_search_call"])
}

func TestCursorChatPlanSkipsToolCallsOnNonAssistantMessage(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Model: "cursor-test-model",
		Messages: []apicompat.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"hello"`),
			ToolCalls: []apicompat.ChatToolCall{{
				ID:       "call_1",
				Function: apicompat.ChatFunctionCall{Name: "lookup", Arguments: "{}"},
			}},
		}},
	}

	// 2026-11-06 口径：user 上挂 tool_calls 是客户端拼错了，不是"整轮不可用"。
	// 丢掉这些调用字段、保留文本，并记账——不能因为它把整轮打成 400。
	plan, err := cursorPlanFromChat(req, req.Model)
	require.NoError(t, err)
	require.Equal(t, 1, plan.skippedItems["chat_call_fields_outside_assistant"],
		"丢弃原因要记账，实际=%s", plan.skippedItems.summary())
}

// 2026-11-06 口径：这两类入站形状**不再**返回 400，而是丢弃后继续本轮。
// 历史：它们曾经是 TestCursorInboundProtocolErrorsAreClient400 的用例。
func TestCursorInboundShapesAreDroppedNotRejected(t *testing.T) {
	ctx := context.Background()
	account := &Account{Platform: PlatformCursor}
	exec := newCursorLifecycleTestService()
	cases := []struct {
		name string
		run  func() error
	}{
		{name: "chat tool_calls on user message", run: func() error {
			_, err := cursorExecuteChat(ctx, nil, exec, account, []byte(`{"model":"cursor-test-model","messages":[{"role":"user","content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`))
			return err
		}},
		{name: "anthropic unknown role", run: func() error {
			_, err := cursorExecuteAnthropic(ctx, nil, exec, account, &apicompat.AnthropicRequest{Model: "cursor-test-model", Messages: []apicompat.AnthropicMessage{{Role: "developer", Content: json.RawMessage(`"do not demote me"`)}}})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			var resumeErr *CursorResumeError
			require.False(t, errors.As(err, &resumeErr),
				"这类形状必须丢弃继续本轮，不能变成续轮错误：%v", err)
		})
	}
}

// 未知角色的消息只丢这一条，不整轮拒（2026-11-06 产品口径：客户把 400 当成模型不稳定）。
func TestCursorConvertMessagesSkipsUnknownRole(t *testing.T) {
	skipped := cursorSkipLedger{}
	turns, err := cursorConvertMessages([]apicompat.ChatMessage{
		{Role: "critic", Content: json.RawMessage(`"do not demote me"`)},
		{Role: "user", Content: json.RawMessage(`"真正的提问"`)},
	}, skipped)
	require.NoError(t, err, "未知角色不得整轮拒")
	require.Len(t, turns, 1, "只保留能落地的那一条")
	require.Equal(t, "user", turns[0].role)
	require.Contains(t, turns[0].text, "真正的提问")
	require.Equal(t, 1, skipped[cursorSkipUnknownRole], "丢弃原因要记账，实际=%s", skipped.summary())
}
