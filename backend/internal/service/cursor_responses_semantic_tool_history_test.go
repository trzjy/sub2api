//go:build unit

package service

// SHAI-MAP-01：Responses 原始工具历史的源解码映射。
//
// 每条用例都是「user → 历史工具调用 → 匹配结果 → user」的完整回放形状：
//  1. 先断言源解码（cursorDecodeResponsesSemantic）的 role / sourceContainerIndex /
//     container / call ID / 顺序 / Function.Name / Function.Arguments；
//  2. 再用唯一的原生 builder（cursorBuildAgentRunFromSemantic）构造 AgentRun，
//     从 cursorBlobsOf 读回真实 history blob，断言其中的工具名与实参。
// 不把 apicompat 桥生成的整份 Messages 当角色事实源，也不只对桥输出作断言。

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// cursorResponsesRJ 把一条 Responses input item 编成原始 JSON 文本。
func cursorResponsesRJ(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// cursorResponsesToolHistoryInput 构造 user → 调用 → 结果 → user 的原始 input 数组。
func cursorResponsesToolHistoryInput(t *testing.T, call, result json.RawMessage) json.RawMessage {
	t.Helper()
	return cursorResponsesRJ(t, []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":"do task"}`),
		call,
		result,
		json.RawMessage(`{"type":"message","role":"user","content":"now continue"}`),
	})
}

type cursorResponsesToolHistoryCase struct {
	name       string
	call       map[string]any
	resultType string
	wantName   string
	wantArgs   string
	wantInput  string // 仅 custom：原文必须从 args.input 逐字恢复
}

func TestCursorResponsesSemanticToolHistory(t *testing.T) {
	const ns = "very_long_namespace_prefix_for_testing_purposes"
	const child = "and_a_rather_long_tool_name_too"

	cases := []cursorResponsesToolHistoryCase{
		{
			name:       "custom/plain command",
			call:       map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "input": "cat historical.txt"},
			resultType: "custom_tool_call_output",
			wantName:   "exec",
			wantArgs:   `{"input":"cat historical.txt"}`,
			wantInput:  "cat historical.txt",
		},
		{
			name:       "custom/quotes newline unicode",
			call:       map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "input": "printf \"héllo\nworld\" 'x' & <tag>"},
			resultType: "custom_tool_call_output",
			wantName:   "exec",
			wantArgs:   `{"input":"printf \"héllo\nworld\" 'x' & <tag>"}`,
			wantInput:  "printf \"héllo\nworld\" 'x' & <tag>",
		},
		{
			name:       "custom/JSON-looking text stays text",
			call:       map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "input": `{"cmd":"ls","path":"/tmp"}`},
			resultType: "custom_tool_call_output",
			wantName:   "exec",
			wantArgs:   `{"input":"{\"cmd\":\"ls\",\"path\":\"/tmp\"}"}`,
			wantInput:  `{"cmd":"ls","path":"/tmp"}`,
		},
		{
			name:       "custom/empty string",
			call:       map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "input": ""},
			resultType: "custom_tool_call_output",
			wantName:   "exec",
			wantArgs:   `{"input":""}`,
			wantInput:  "",
		},
		{
			name:       "custom/whitespace only",
			call:       map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "input": "  \n\t "},
			resultType: "custom_tool_call_output",
			wantName:   "exec",
			wantArgs:   `{"input":"  \n\t "}`,
			wantInput:  "  \n\t ",
		},
		{
			name:       "custom/literal null is still text",
			call:       map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "input": "null"},
			resultType: "custom_tool_call_output",
			wantName:   "exec",
			wantArgs:   `{"input":"null"}`,
			wantInput:  "null",
		},
		{
			name:       "function/namespace flattened",
			call:       map[string]any{"type": "function_call", "call_id": "call_1", "name": "spawn_agent", "namespace": "collaboration", "arguments": "{}"},
			resultType: "function_call_output",
			wantName:   apicompat.FlattenNamespaceToolName("collaboration", "spawn_agent"),
			wantArgs:   `{}`,
		},
		{
			name:       "function/whitespace namespace is still a namespace",
			call:       map[string]any{"type": "function_call", "call_id": "call_1", "name": "spawn_agent", "namespace": "   ", "arguments": "{}"},
			resultType: "function_call_output",
			wantName:   apicompat.FlattenNamespaceToolName("   ", "spawn_agent"),
			wantArgs:   `{}`,
		},
		{
			name:       "function/namespace long name truncated with hash",
			call:       map[string]any{"type": "function_call", "call_id": "call_1", "name": child, "namespace": ns, "arguments": "{}"},
			resultType: "function_call_output",
			wantName:   apicompat.FlattenNamespaceToolName(ns, child),
			wantArgs:   `{}`,
		},
		{
			name:       "tool_search/object arguments",
			call:       map[string]any{"type": "tool_search_call", "call_id": "call_1", "arguments": map[string]any{"query": "hello"}},
			resultType: "tool_search_output",
			wantName:   apicompat.ToolSearchProxyName,
			wantArgs:   `{"query":"hello"}`,
		},
		{
			name:       "tool_search/string arguments",
			call:       map[string]any{"type": "tool_search_call", "call_id": "call_1", "arguments": `{"query":"docs","limit":2}`},
			resultType: "tool_search_output",
			wantName:   apicompat.ToolSearchProxyName,
			wantArgs:   `{"query":"docs","limit":2}`,
		},
		{
			name:       "plain function unchanged",
			call:       map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"hz"}`},
			resultType: "function_call_output",
			wantName:   "get_weather",
			wantArgs:   `{"city":"hz"}`,
		},
		{
			name:       "plain function empty arguments becomes object",
			call:       map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": ""},
			resultType: "function_call_output",
			wantName:   "get_weather",
			wantArgs:   `{}`,
		},
		{
			name:       "plain function literal null arguments becomes object",
			call:       map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": nil},
			resultType: "function_call_output",
			wantName:   "get_weather",
			wantArgs:   `{}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := cursorResponsesRJ(t, tc.call)
			result := cursorResponsesRJ(t, map[string]any{
				"type": tc.resultType, "call_id": "call_1", "output": "done",
			})
			req := &apicompat.ResponsesRequest{
				Model:        "cursor-test-model",
				Instructions: "SYS",
				Input:        cursorResponsesToolHistoryInput(t, call, result),
			}

			// 1) 源解码：角色、源容器下标、容器标记、调用 ID、顺序与映射后的实参。
			sem, err := cursorDecodeResponsesSemantic(req)
			require.NoError(t, err)
			require.Len(t, sem, 5)
			require.Equal(t, []string{
				cursorSemanticInstruction, cursorSemanticUser, cursorSemanticAssistant,
				cursorSemanticTool, cursorSemanticUser,
			}, []string{sem[0].role, sem[1].role, sem[2].role, sem[3].role, sem[4].role})
			require.Equal(t, []int{-1, 0, 1, 2, 3}, []int{
				sem[0].sourceContainerIndex, sem[1].sourceContainerIndex,
				sem[2].sourceContainerIndex, sem[3].sourceContainerIndex,
				sem[4].sourceContainerIndex,
			})
			require.Equal(t, cursorContainerMixed, sem[2].container)
			require.Equal(t, cursorContainerAllToolResults, sem[3].container)
			require.Equal(t, "call_1", sem[2].raw.ToolCalls[0].ID)
			require.Equal(t, "call_1", sem[3].raw.ToolCallID)
			require.Equal(t, "assistant", sem[2].raw.Role)
			require.Equal(t, "tool", sem[3].raw.Role)
			require.Len(t, sem[2].raw.ToolCalls, 1)
			require.Equal(t, "function", sem[2].raw.ToolCalls[0].Type)
			require.Equal(t, tc.wantName, sem[2].raw.ToolCalls[0].Function.Name, "工具名必须按源协议映射")
			require.JSONEq(t, tc.wantArgs, sem[2].raw.ToolCalls[0].Function.Arguments, "实参必须按源协议映射")
			if tc.wantInput != "" || tc.call["type"] == "custom_tool_call" {
				var packed map[string]string
				require.NoError(t, json.Unmarshal([]byte(sem[2].raw.ToolCalls[0].Function.Arguments), &packed))
				require.Equal(t, tc.wantInput, packed["input"], "custom 自由文本必须逐字放进 args.input")
			}

			// 2) 原生 builder / history blob：真实出站实参必须是合法 JSON 且名称一致。
			body, _, err := cursorBuildAgentRunFromSemantic(sem, "cursor-upstream-model", nil, nil, cursorSkipLedger{})
			require.NoError(t, err, "history serializer 必须能构造 AgentRun")
			run := cursorAgentRunOf(t, body)
			blobs := cursorBlobsOf(t, run)
			require.Len(t, blobs, 3, "user / assistant(tool-call) / tool 三段历史保序")
			var assistant cursorHistoryAssistantBlob
			require.NoError(t, json.Unmarshal(blobs[1][1], &assistant), "history blob 必须可序列化")
			require.Equal(t, "assistant", assistant.Role)
			require.Len(t, assistant.Content, 1)
			part := assistant.Content[0]
			require.Equal(t, "tool-call", part.Type)
			require.Equal(t, "call_1", part.ToolCallID)
			require.Equal(t, cursorHistoryCallDynamicToolName, part.ToolName)
			require.NotNil(t, part.Args)
			require.Equal(t, cursorMcpProviderIdentifier, part.Args.Namespace)
			require.Equal(t, tc.wantName, part.Args.ToolName, "history blob 中的实参名必须是映射值")
			require.True(t, json.Valid(part.Args.Arguments), "history blob 实参必须是合法 JSON")
			require.JSONEq(t, tc.wantArgs, string(part.Args.Arguments), "history blob 实参必须与源映射一致")
			if tc.call["type"] == "custom_tool_call" {
				var restored map[string]string
				require.NoError(t, json.Unmarshal(part.Args.Arguments, &restored))
				require.Equal(t, tc.wantInput, restored["input"], "custom 原文必须从真实 history blob 逐字恢复")
			}

			// 3) 白空 namespace 也按 Namespace != "" 走同一摊平：不 trim、不退回裸名；
			//    期望值由已有 helper 给出，再钉死其字面精确结果。
			if tc.name == "function/whitespace namespace is still a namespace" {
				require.Equal(t, "   __spawn_agent", tc.wantName)
				require.NotEqual(t, "spawn_agent", tc.wantName)
			}
			// 4) namespace 超长名必须命中权威摊平规则（64 字符 + sha256 后缀）。
			if tc.name == "function/namespace long name truncated with hash" {
				require.Len(t, tc.wantName, 64)
				require.Contains(t, tc.wantName, "__")
			}
		})
	}
}

// cursorResponsesHistoryVerifyTransport 是 test-only transport：先核对真实出站的
// history blob，核对成功才返回合法终态；核对失败返回 400，令入口测试真失败。
type cursorResponsesHistoryVerifyTransport struct {
	mu        sync.Mutex
	hits      int
	verified  bool
	wantName  string
	wantInput string
	terminal  []byte
}

func (tr *cursorResponsesHistoryVerifyTransport) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 复用本包既有帧读取器：io.ReadFull 按 envelope 头精确读满一帧 payload。
		// 读帧出错即验证失败，不用 ReadAll 等流关闭，也不假设单次 Read 读全。
		payload, err := cursorDuplexReadFrame(r.Body)
		ok := err == nil && tr.verify(payload)
		tr.mu.Lock()
		tr.hits++
		if ok {
			tr.verified = true
		}
		tr.mu.Unlock()
		if !ok {
			http.Error(w, "history blob mismatch", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tr.terminal)
	})
}

func (tr *cursorResponsesHistoryVerifyTransport) verify(payload []byte) bool {
	run, err := cursorDecodeProtobuf(payload)
	if err != nil {
		return false
	}
	runField, ok := cursorFindField(run, cursorFldAgentRunRequest)
	if !ok {
		return false
	}
	runFields, err := cursorDecodeProtobuf(runField.Bytes)
	if err != nil {
		return false
	}
	for _, field := range runFields {
		if field.Number != cursorFldRunPreFetchedBlobs {
			continue
		}
		blobFields, err := cursorDecodeProtobuf(field.Bytes)
		if err != nil {
			return false
		}
		value, ok := cursorFindField(blobFields, cursorFldBlobValue)
		if !ok {
			continue
		}
		var assistant cursorHistoryAssistantBlob
		if json.Unmarshal(value.Bytes, &assistant) != nil || assistant.Role != "assistant" {
			continue
		}
		for _, part := range assistant.Content {
			if part.Type != "tool-call" || part.Args == nil || part.ToolCallID != "call_1" {
				continue
			}
			var got map[string]string
			if json.Unmarshal(part.Args.Arguments, &got) != nil {
				continue
			}
			if part.Args.ToolName == tr.wantName && got["input"] == tr.wantInput {
				return true
			}
		}
	}
	return false
}

// TestCursorResponsesSemanticToolHistoryExecuteEntry 从生产入口 cursorExecuteResponses
// 发自由文本 custom 历史；transport 先核对真实发送的 history blob 才回终态，
// 因此「构造阶段吞错」或「固定无条件成功」都无法让本用例通过。
func TestCursorResponsesSemanticToolHistoryExecuteEntry(t *testing.T) {
	const toolInput = "cat historical.txt"
	tr := &cursorResponsesHistoryVerifyTransport{
		wantName:  "exec",
		wantInput: toolInput,
		terminal:  cursorOKStreamBody(t),
	}
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	gw, c := newCursorOutboundTestService(t, tr.handler(), repo, cursorTestDeviceIDs(repo))

	req := &apicompat.ResponsesRequest{
		Model: "cursor-sonnet",
		Input: cursorResponsesToolHistoryInput(t,
			cursorResponsesRJ(t, map[string]any{
				"type": "custom_tool_call", "call_id": "call_1", "name": "exec", "input": toolInput,
			}),
			cursorResponsesRJ(t, map[string]any{
				"type": "custom_tool_call_output", "call_id": "call_1", "output": "done",
			}),
		),
		Tools: []apicompat.ResponsesTool{{Type: "custom", Name: "exec", Description: "freeform exec"}},
	}

	result, err := cursorExecuteResponses(context.Background(), c, gw.cursorExecution,
		cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), req)
	require.NoError(t, err, "自由文本 custom 历史必须能从生产 Responses 入口成功构造并发送")
	require.NotNil(t, result)

	tr.mu.Lock()
	defer tr.mu.Unlock()
	require.Greater(t, tr.hits, 0, "请求必须真实到达 transport（不允许固定无条件成功）")
	require.True(t, tr.verified, "transport 必须在实际发送的 history blob 中核到工具名与原文实参")
}
