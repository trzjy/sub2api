//go:build unit

package service

// 请求形状诊断的定向测试（2026-11-04 用户裁定：加日志，后续再出现才有判断依据）。
//
// 硬约束：形状里只能有计数与开关，绝不能出现正文、工具名、文件名等用户内容。
// 这条测试用"正文里放一个独特标记词"的方式钉死它。

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const shapeLeakProbe = "SECRET-PROMPT-MARKER"

func TestDescribeRequestShapeChat(t *testing.T) {
	body := []byte(`{
	  "model":"deepseek-v4.1-flash",
	  "max_tokens":999999,
	  "stream":true,
	  "messages":[
	    {"role":"system","content":"` + shapeLeakProbe + `"},
	    {"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
	    {"role":"user","content":"again"}
	  ],
	  "tools":[{"type":"function","function":{"name":"get_weather","parameters":{}}},{"type":"function","function":{"name":"run","parameters":{}}}],
	  "tool_choice":"auto",
	  "reasoning":{"effort":"high"}
	}`)
	shape := DescribeRequestShape(body, "chat")

	require.Equal(t, 3, shape["items"])
	require.Equal(t, 2, shape["tools"])
	require.Equal(t, 1, shape["images"])
	require.Equal(t, int64(999999), shape["max_tokens"])
	require.Equal(t, true, shape["stream"])
	require.Equal(t, "high", shape["reasoning_effort"])
	require.Equal(t, len(body), shape["body_bytes"])

	// 绝不泄漏正文或工具名。
	encoded, err := json.Marshal(shape)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), shapeLeakProbe, "形状里不得出现正文")
	require.NotContains(t, string(encoded), "get_weather", "形状里不得出现工具名")
}

func TestDescribeRequestShapeResponses(t *testing.T) {
	body := []byte(`{
	  "model":"gpt-6.1",
	  "input":[{"type":"message","role":"user","content":"hi"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}],
	  "tools":[{"type":"function","name":"f","parameters":{}}],
	  "max_output_tokens":32000,
	  "previous_response_id":"resp_123",
	  "thinking":{"type":"adaptive","budget_tokens":8192}
	}`)
	shape := DescribeRequestShape(body, "responses")

	require.Equal(t, 2, shape["items"])
	require.Equal(t, 1, shape["tools"])
	require.Equal(t, int64(32000), shape["max_output_tokens"])
	require.Equal(t, true, shape["has_previous_response_id"])
	require.Equal(t, "adaptive", shape["thinking_type"])
	require.Equal(t, int64(8192), shape["thinking_budget"])
}

func TestDescribeRequestShapeInvalidJSON(t *testing.T) {
	body := []byte(`{not json ` + shapeLeakProbe)
	shape := DescribeRequestShape(body, "chat")
	require.Equal(t, false, shape["json_valid"])
	require.Equal(t, len(body), shape["body_bytes"])
	encoded, _ := json.Marshal(shape)
	require.NotContains(t, string(encoded), shapeLeakProbe, "非法 JSON 也不得把原文带进形状")
}

func TestIsGenericUpstreamRejectionMessage(t *testing.T) {
	cases := map[string]bool{
		"inference request is invalid":                     true,
		"The request could not be processed.":              true,
		"invalid request body":                             true,
		"field MaxTokens invalid, should be in [1, 65536]": false,
		"model not found":                                  false,
		"":                                                 false,
	}
	for msg, want := range cases {
		require.Equal(t, want, IsGenericUpstreamRejectionMessage(msg), "msg=%q", msg)
	}
}

func TestOpsRequestShapeRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	require.Nil(t, OpsRequestShapeFrom(c))
	SetOpsRequestShape(c, map[string]any{"tools": 2})
	shape := OpsRequestShapeFrom(c)
	require.Equal(t, 2, shape["tools"])

	// 上游错误事件应自动带上本次请求的形状（无需每个失败分支自己拼）。
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{Kind: "http_error", Message: "inference request is invalid"})
	events, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	list, ok := events.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, list, 1)
	require.Equal(t, 2, list[0].RequestShape["tools"])

	encoded, _ := json.Marshal(list[0])
	require.True(t, strings.Contains(string(encoded), `"request_shape"`))
}
