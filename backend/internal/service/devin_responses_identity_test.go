//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
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
