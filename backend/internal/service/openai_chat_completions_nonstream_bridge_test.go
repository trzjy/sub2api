//go:build unit

package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// fakeCCStreamResponse 用若干 data 帧构造一条假上游 SSE 响应（强制流式 CC）。
// 每个 event 是已 JSON 编码的 data 负载；[DONE] 由调用方显式写入。
func fakeCCStreamResponse(t *testing.T, events []string) *http.Response {
	t.Helper()
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: ")
		b.WriteString(e)
		b.WriteString("\n")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(b.String())),
	}
}

func newBridgeTestCtx() (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	return c, rec
}

// bridgeErrReader 是一个自定义 io.Reader：依次读出若干字节后返回哨兵错误（非 EOF）。
// 用于 F4 构造「先有效 SSE 帧、再中段读错误」的可靠故障注入（区别于往 body 里塞
// 非法 UTF-8 的旧做法——bufio.Scanner 的 ScanLines 并不检查 UTF-8，旧用例实际走
// 的是「缺 [DONE] 旁路」而非读错误路径）。以 bridge 前缀命名避免与
// vision_detect_service_test.go 既有 errReader 重名。
type bridgeErrReader struct {
	data []byte
	off  int
	err  error
}

func (r *bridgeErrReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

var errMidStreamSentinel = errors.New("sentinel mid-stream read failure")

// sentinelErrorReader 先输出 1 个有效 SSE data 帧，随后返回非 EOF 的哨兵错误。
func sentinelErrorReader() io.Reader {
	frame := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n"
	return &bridgeErrReader{data: []byte(frame), err: errMidStreamSentinel}
}

// ---- Done when #1 四要点定向用例 ----

// ① include_usage 末帧 usage 写回聚合响应 + 返回 OpenAIUsage 非零。
func TestCollectCCStreamAsResponse_UsageWrittenBack(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-x",` +
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`,
		`{"choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}
	resp, usage, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotNil(t, resp.Usage, "usage 必须写回聚合响应")
	require.Equal(t, 10, resp.Usage.PromptTokens)
	require.Equal(t, 3, resp.Usage.CompletionTokens)
	require.Equal(t, 13, resp.Usage.TotalTokens)
	// 计费口径 OpenAIUsage 非零。
	require.NotZero(t, usage.InputTokens, "返回 OpenAIUsage 应非零")
	require.NotZero(t, usage.OutputTokens)
	require.Equal(t, 0, rec.Body.Len(), "聚合期间零字节写客户端")
}

// ①b F5：上游仅发一帧显式空串增量 delta.content: ""（从未发非空 content）→
// content 序列化为 JSON 空字符串 ""（gjson Type==JSON String），非 null。
func TestCollectCCStreamAsResponse_ExplicitEmptyContentDelta(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
			`"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	}
	resp, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)

	body, err := json.Marshal(resp)
	require.NoError(t, err)
	cv := gjson.GetBytes(body, "choices.0.message.content")
	require.Equal(t, gjson.String, cv.Type,
		"显式空串增量必须序列化为 JSON 空字符串（gjson String），非 null")
	require.Equal(t, `""`, string(resp.Choices[0].Message.Content),
		"message.content 应为 JSON 空字符串 \"\"")
	require.Equal(t, 0, rec.Body.Len())
}

// ② tool_calls 分片按 index 合并（arguments 拼接、name 首片、Index=nil）。
func TestCollectCCStreamAsResponse_ToolCallsMerged(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
			`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function",` +
			`"function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,` +
			`"function":{"arguments":"{\"loc\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,` +
			`"function":{"arguments":"\"NYC\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	}
	resp, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)

	// F1：纯工具调用 choice 从未收到 content 增量 → content 为 JSON null。
	body, err := json.Marshal(resp)
	require.NoError(t, err)
	require.Equal(t, gjson.Null, gjson.GetBytes(body, "choices.0.message.content").Type,
		"纯工具调用响应 message.content 应为 JSON null（与原生非流式工具调用一致）")

	msg := resp.Choices[0].Message
	require.Len(t, msg.ToolCalls, 1)
	tc := msg.ToolCalls[0]
	require.Nil(t, tc.Index, "非流式契约 Index 应为 nil")
	require.Equal(t, "call_1", tc.ID)
	require.Equal(t, "function", tc.Type)
	require.Equal(t, "get_weather", tc.Function.Name, "name 取首片")
	require.Equal(t, `{"loc":"NYC"}`, tc.Function.Arguments, "arguments 跨片拼接")
	require.Equal(t, "tool_calls", resp.Choices[0].FinishReason)
	require.Equal(t, 0, rec.Body.Len())
}

// ③ n>1 两 choice 按 index 全量组装升序。
func TestCollectCCStreamAsResponse_NGreaterThanOne(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
			`"choices":[{"index":1,"delta":{"content":"B"},"finish_reason":null},` +
			`{"index":0,"delta":{"content":"A"},"finish_reason":null}]}`,
		`{"choices":[{"index":1,"delta":{"content":"b"},"finish_reason":"stop"},` +
			`{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	}
	resp, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.NoError(t, err)
	require.Len(t, resp.Choices, 2)
	require.Equal(t, 0, resp.Choices[0].Index)
	require.Equal(t, 1, resp.Choices[1].Index)
	require.Equal(t, "\"Aa\"", string(resp.Choices[0].Message.Content))
	require.Equal(t, "\"Bb\"", string(resp.Choices[1].Message.Content))
	require.Equal(t, "stop", resp.Choices[0].FinishReason)
	require.Equal(t, "stop", resp.Choices[1].FinishReason)
	require.Equal(t, 0, rec.Body.Len())
}

// ④ reasoning_content 聚合写回。
func TestCollectCCStreamAsResponse_ReasoningContent(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
			`"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think "},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"hard"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"ans"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	}
	resp, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.NoError(t, err)
	require.Equal(t, "think hard", resp.Choices[0].Message.ReasoningContent, "reasoning 不丢弃")
	require.Equal(t, `"ans"`, string(resp.Choices[0].Message.Content))
	require.Equal(t, 0, rec.Body.Len())
}

// ---- Done when #2 失败语义用例 ----

// 中段读错误 → 显式失败、errors.Is 命中哨兵错误、零字节写客户端。
// F4：改用自定义 Reader（先输出有效 SSE data 帧，再返回非 EOF 的哨兵错误），
// 确保走的是读错误路径而非「缺 [DONE] 旁路」。
func TestCollectCCStreamAsResponse_MidStreamReadError(t *testing.T) {
	c, rec := newBridgeTestCtx()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(sentinelErrorReader()),
	}
	_, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(resp, c, "test")
	require.Error(t, err, "中段读错误应显式失败")
	require.True(t, errors.Is(err, errMidStreamSentinel),
		"错误应包装哨兵读取错误，errors.Is 命中（而非缺 [DONE] 旁路）")
	require.Equal(t, 0, rec.Body.Len(), "聚合期间零字节写客户端")
}

// 畸形帧（"有效帧—畸形帧—有效帧"序列）→ 显式失败、零字节写客户端。
func TestCollectCCStreamAsResponse_MalformedFrame(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`not-valid-json`,
		`{"choices":[{"index":0,"delta":{"content":"more"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	}
	_, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.Error(t, err, "畸形帧应失败关闭")
	require.Equal(t, 0, rec.Body.Len())
}

// 有效内容后无 [DONE] 干净 EOF → 显式失败、不默认 finish_reason。
func TestCollectCCStreamAsResponse_MissingDoneSentinel(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
			`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		// 故意不发 [DONE]
	}
	_, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.Error(t, err, "缺 [DONE] 应失败关闭")
	require.Equal(t, 0, rec.Body.Len())
}

// 正常 choice + [DONE] 但无 usage 帧 → 显式失败、零计费零响应。
func TestCollectCCStreamAsResponse_MissingUsage(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
			`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	}
	_, usage, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.Error(t, err, "无 usage 帧应失败关闭（资金边界）")
	require.Zero(t, usage.InputTokens, "不应产生计费 usage")
	require.Equal(t, 0, rec.Body.Len())
}

// usage-only 帧 + [DONE]（零 choice chunk）→ 失败关闭、不产出空 choices 成功响应。
func TestCollectCCStreamAsResponse_UsageOnlyNoChoices(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	}
	resp, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.Error(t, err, "usage-only 流应失败关闭")
	require.Nil(t, resp)
	require.Equal(t, 0, rec.Body.Len())
}

// [DONE] 前单 choice 缺 finish_reason（多 choice 仅一个缺失）→ 整桥失败。
func TestCollectCCStreamAsResponse_MissingFinishReason(t *testing.T) {
	// 单 choice 缺失
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
			`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	}
	_, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.Error(t, err, "单 choice 缺 finish_reason 应失败")
	require.Equal(t, 0, rec.Body.Len())

	// 多 choice 仅一个缺失
	c2, rec2 := newBridgeTestCtx()
	events2 := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
			`"choices":[{"index":0,"delta":{"content":"A"},"finish_reason":null},` +
			`{"index":1,"delta":{"content":"B"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"},` +
			`{"index":1,"delta":{},"finish_reason":null}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	}
	_, _, err2 := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events2), c2, "test")
	require.Error(t, err2, "多 choice 仅一个缺 finish_reason 应失败")
	require.Equal(t, 0, rec2.Body.Len())
}

// ---- Done when #3 envelope 字段断言 ----

func TestCollectCCStreamAsResponse_EnvelopeFields(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"id":"chatcmpl-env","object":"chat.completion.chunk","created":1700000000,"model":"m",` +
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	}
	resp, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.NoError(t, err)
	require.Equal(t, "chat.completion", resp.Object, "envelope object 常量")
	require.Len(t, resp.Choices, 1)
	require.Equal(t, "assistant", resp.Choices[0].Message.Role, "message.role 常量 assistant")
	// F1：收到过 content 增量的 choice → content 仍为 JSON 字符串。
	require.Equal(t, `"hello"`, string(resp.Choices[0].Message.Content))
	require.Equal(t, 0, rec.Body.Len())
}

// 验证零字节写客户端的通用不变量：所有成功路径也不应在聚合期写 Body。
func TestCollectCCStreamAsResponse_NoClientBytesOnSuccess(t *testing.T) {
	c, rec := newBridgeTestCtx()
	events := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
			`"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		`[DONE]`,
	}
	_, _, err := (&OpenAIGatewayService{}).collectCCStreamAsResponse(
		fakeCCStreamResponse(t, events), c, "test")
	require.NoError(t, err)
	require.Equal(t, 0, rec.Body.Len())
	require.False(t, rec.Flushed, "聚合期不应提交响应")
}
