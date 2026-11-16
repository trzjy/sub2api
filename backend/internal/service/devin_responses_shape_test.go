//go:build unit

package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 以下三个形状用例走生产入口 normalizeDevinResponsesBodyWithIdentity（旧的无身份
// 转换函数已随外审 r8 must_fix 删除），断言保持等价。
func TestNormalizeDevinResponsesBody(t *testing.T) {
	// ① Responses 形状（input + tools[].type=function + max_output_tokens）→
	// 输出含 messages、tools[].function.name，不含顶层 input。
	t.Run("responses shape is converted to chat completions", func(t *testing.T) {
		responsesBody := []byte(`{
			"model": "gpt-4o",
			"instructions": "be helpful",
			"input": [{"type": "message", "role": "user", "content": "hello devin"}],
			"tools": [{
				"type": "function",
				"name": "get_weather",
				"description": "lookup weather",
				"parameters": {"type": "object", "properties": {}}
			}],
			"max_output_tokens": 256
		}`)

		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		svc := &OpenAIGatewayService{}
		out, err := svc.normalizeDevinResponsesBodyWithIdentity(c, responsesBody)
		require.NoError(t, err)

		require.True(t, gjson.GetBytes(out, "messages").Exists(), "输出应含 messages")
		require.Contains(t, string(out), "hello devin", "input 内容应进入 messages")
		require.Equal(t, "get_weather", gjson.GetBytes(out, "tools.0.function.name").String(),
			"function 工具应转为 tools[].function.name")
		require.False(t, gjson.GetBytes(out, "input").Exists(), "输出不应含顶层 input")
		require.Equal(t, int64(256), gjson.GetBytes(out, "max_completion_tokens").Int(),
			"max_output_tokens 应映射为 max_completion_tokens")
	})

	// ② 已是 Chat 形状（含 messages）→ 字节不变。
	t.Run("chat shape stays byte-identical", func(t *testing.T) {
		chatBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		svc := &OpenAIGatewayService{}
		out, err := svc.normalizeDevinResponsesBodyWithIdentity(c, chatBody)
		require.NoError(t, err)
		require.Equal(t, chatBody, out, "已是 Chat 形状应字节不变")
	})

	// ③ 非法 JSON 且像 Responses（含 "input"）→ 返回 error（失败关闭），不回落原 body。
	t.Run("malformed responses-shaped json fails closed", func(t *testing.T) {
		malformed := []byte(`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":"hi"}],"broken":}`)
		require.True(t, gjson.GetBytes(malformed, "input").Exists(),
			"前置条件：该报文应被 gjson 判定为像 Responses（含 input）")

		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		svc := &OpenAIGatewayService{}
		out, err := svc.normalizeDevinResponsesBodyWithIdentity(c, malformed)
		require.Error(t, err, "非法 JSON 且像 Responses 时应返回错误")
		require.Nil(t, out, "失败时不得回落原 Responses body")
		require.True(t, IsResponseCommitted(c), "错误出口必须标记响应已提交，阻止 handler 补 SSE")
	})
}
