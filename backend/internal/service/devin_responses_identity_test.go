//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 外审 r7 must_fix ②：重复 function 名在 Responses→Chat 摊平时必须失败关闭，
// 且由调用方写出既有 400 invalid_request_error（不得被 handler 误判成 502 upstream_error）。
// 本用例直接驱动转换入口，不访问上游。
func TestDevinResponsesDuplicateToolNameReturns400(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools": [
			{"type":"function","name":"dup","parameters":{"type":"object"}},
			{"type":"function","name":"dup","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}
		]
	}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	svc := &OpenAIGatewayService{}

	out, err := svc.normalizeDevinResponsesBodyWithIdentity(c, body)
	require.Error(t, err, "同名 function 工具必须失败关闭")
	require.Nil(t, out, "失败时不得返回可被转发的 body")
	require.Contains(t, err.Error(), "duplicate")
	require.Equal(t, http.StatusBadRequest, rec.Code, "必须写 400，不得落到 502")
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String(),
		"error.type 必须为 invalid_request_error")
	// 外审 r8 must_fix：错误出口必须标记响应已提交，阻止 handler 收尾把已写出的 JSON
	// 误判成 SSE 并追加 event: response.failed（客户端拿到 JSON+SSE 坏 body）。
	require.True(t, IsResponseCommitted(c), "错误路径写 400 后必须 IsResponseCommitted(c)==true")
}

// 外审 r7 must_fix ①：custom / namespace 工具经摊平后，回程必须按身份还原为
// custom_tool_call / 带 namespace 字段的 function_call，而不是普通 function_call。
func TestDevinResponsesToolIdentityRoundTrip(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"run pwd"}]}],
		"tools": [
			{"type":"custom","name":"exec","description":"Runs a command"},
			{"type":"namespace","name":"functions","tools":[
				{"type":"function","name":"wait","parameters":{"type":"object"}}
			]}
		]
	}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	svc := &OpenAIGatewayService{}

	devinBody, err := svc.normalizeDevinResponsesBodyWithIdentity(c, body)
	require.NoError(t, err)
	require.NotNil(t, devinBody)

	ident := devinResponsesToolIdentityFrom(c)
	require.NotNil(t, ident, "转换成功后必须把工具身份写入 gin 上下文")
	require.True(t, ident.custom["exec"], "custom 工具名应登记")
	require.False(t, ident.toolSearch, "本请求不含 tool_search")
	_, hasWait := ident.namespace["functions__wait"]
	require.True(t, hasWait, "namespace 子工具摊平名应登记")

	// 上游把 custom / namespace 工具都当成普通 function 调用返回。
	resp := &apicompat.ChatCompletionsResponse{Choices: []apicompat.ChatChoice{{
		Message: apicompat.ChatMessage{ToolCalls: []apicompat.ChatToolCall{
			{ID: "call_exec", Function: apicompat.ChatFunctionCall{Name: "exec", Arguments: `{"input":"pwd"}`}},
			{ID: "call_wait", Function: apicompat.ChatFunctionCall{Name: "functions__wait", Arguments: `{"cell_id":"1"}`}},
		}},
	}}}

	// 用身份（而非 nil 四集合）回程转换，断言项类型被正确还原。
	out := apicompat.ChatCompletionsResponseToResponses(resp, "gpt-4o",
		ident.custom, ident.function, ident.toolSearch, ident.namespace)
	require.Len(t, out.Output, 2)
	require.Equal(t, "custom_tool_call", out.Output[0].Type, "custom 工具应还原为 custom_tool_call")
	require.Equal(t, "exec", out.Output[0].Name)
	require.Equal(t, "function_call", out.Output[1].Type, "namespace 子工具按带 namespace 的 function_call 交付")
	require.Equal(t, "functions", out.Output[1].Namespace)
	require.Equal(t, "wait", out.Output[1].Name)

	// 对照：不带身份时两项都退化为普通 function_call —— 正是 r7 修复前的缺陷。
	plain := apicompat.ChatCompletionsResponseToResponses(resp, "gpt-4o", nil, nil, false, nil)
	require.Len(t, plain.Output, 2)
	require.Equal(t, "function_call", plain.Output[0].Type)
	require.Equal(t, "function_call", plain.Output[1].Type)
	require.Empty(t, plain.Output[1].Namespace)
}

// newDevinIdentWithAllTools 构造含 custom / namespace / tool_search 三类工具身份，
// 用于驱动"真实回桥"用例（身份集合口径与 normalizeDevinResponsesBodyWithIdentity 写入一致）。
func newDevinIdentWithAllTools() *devinResponsesToolIdentity {
	return &devinResponsesToolIdentity{
		custom:     map[string]bool{"exec": true},
		function:   nil,
		toolSearch: true,
		namespace: map[string]apicompat.NamespacedToolName{
			"functions__wait": {Namespace: "functions", Name: "wait"},
		},
	}
}

// 外审 r8 must_fix：非流式走真实回桥 writeWebCompletion（Responses 模式），断言
// custom / namespace / tool_search 三类工具调用被还原为正确的 Responses 输出项类型。
func TestDevinResponsesBridgeCompletionRestoresToolTypes(t *testing.T) {
	var completion map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{
		"id":"chatcmpl-1","object":"chat.completion","created":0,"model":"gpt-4o",
		"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[
			{"id":"call_exec","type":"function","function":{"name":"exec","arguments":"{\"input\":\"pwd\"}"}},
			{"id":"call_wait","type":"function","function":{"name":"functions__wait","arguments":"{\"cell_id\":\"1\"}"}},
			{"id":"call_search","type":"function","function":{"name":"tool_search","arguments":"{\"query\":\"git\"}"}}
		]}}]
	}`), &completion))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	st := newWebClientStreamStateWithIdentity(webResponseModeResponses, "gpt-4o", newDevinIdentWithAllTools())
	require.NoError(t, writeWebCompletion(c, st, completion))

	body := rec.Body.String()
	require.Equal(t, "custom_tool_call", gjson.Get(body, "output.0.type").String(), "custom 工具应还原为 custom_tool_call")
	require.Equal(t, "exec", gjson.Get(body, "output.0.name").String())
	require.Equal(t, "function_call", gjson.Get(body, "output.1.type").String(), "namespace 子工具应按 function_call 交付")
	require.Equal(t, "functions", gjson.Get(body, "output.1.namespace").String(), "namespace 字段应还原")
	require.Equal(t, "wait", gjson.Get(body, "output.1.name").String(), "namespace 子工具名应为裸名")
	require.Equal(t, "tool_search_call", gjson.Get(body, "output.2.type").String(), "tool_search 应还原为 tool_search_call")
}

// 外审 r8 must_fix：流式走真实回桥 writeWebStreamChunk（Responses 模式），按一帧
// chat.completion.chunk 断言 output_item.added 事件携带正确的项类型 / namespace。
func TestDevinResponsesBridgeStreamRestoresToolTypes(t *testing.T) {
	cases := []struct {
		name         string
		functionJSON string
		wantType     string
		wantNS       string
		wantName     string
	}{
		{
			name:         "custom -> custom_tool_call",
			functionJSON: `{"name":"exec","arguments":"{\"input\":\"pwd\"}"}`,
			wantType:     "custom_tool_call",
			wantName:     "exec",
		},
		{
			name:         "namespace -> function_call with namespace",
			functionJSON: `{"name":"functions__wait","arguments":"{\"cell_id\":\"1\"}"}`,
			wantType:     "function_call",
			wantNS:       "functions",
			wantName:     "wait",
		},
		{
			name:         "tool_search -> tool_search_call",
			functionJSON: `{"name":"tool_search","arguments":"{\"query\":\"git\"}"}`,
			wantType:     "tool_search_call",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunkJSON := fmt.Sprintf(`{
				"id":"chatcmpl-1","object":"chat.completion.chunk","created":0,"model":"gpt-4o",
				"choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","tool_calls":[
					{"index":0,"id":"call_1","type":"function","function":%s}
				]}}]
			}`, tc.functionJSON)

			var chunk map[string]any
			require.NoError(t, json.Unmarshal([]byte(chunkJSON), &chunk))

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			st := newWebClientStreamStateWithIdentity(webResponseModeResponses, "gpt-4o", newDevinIdentWithAllTools())
			require.NoError(t, writeWebStreamChunk(c, st, chunk))

			items := sseOutputItemAddedItems(t, rec.Body.String())
			var toolItem *gjson.Result
			for i := range items {
				typ := items[i].Get("type").String()
				if typ != "message" && typ != "reasoning" {
					toolItem = &items[i]
					break
				}
			}
			require.NotNil(t, toolItem, "应产出工具调用的 response.output_item.added 事件")
			require.Equal(t, tc.wantType, toolItem.Get("type").String())
			require.Equal(t, tc.wantNS, toolItem.Get("namespace").String())
			if tc.wantName != "" {
				require.Equal(t, tc.wantName, toolItem.Get("name").String())
			}
		})
	}
}

// sseOutputItemAddedItems 从 Responses SSE 流里提取所有 response.output_item.added
// 事件的 item 对象，供断言项类型 / namespace。
func sseOutputItemAddedItems(t *testing.T, body string) []gjson.Result {
	t.Helper()
	var out []gjson.Result
	for _, frame := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "" || gjson.Get(data, "type").String() != "response.output_item.added" {
			continue
		}
		if item := gjson.Get(data, "item"); item.Exists() {
			out = append(out, item)
		}
	}
	require.NotEmpty(t, out, "SSE 流应含 response.output_item.added 事件，实际: %s", body)
	return out
}
