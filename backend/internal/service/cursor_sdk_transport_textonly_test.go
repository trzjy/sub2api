//go:build unit

package service

// CUR-SVC6B：Cursor writer 禁用共享 DSML 的端到端验收。
//
// 真实 protobuf 文本帧（reasoning/content 各含完整 DSML 块 + 跨帧前缀 + 残片）
// 经 parser→聚合→newCursorProtocolWriter 实际接线，流/非流 × Messages/Responses：
//   - DSML 正文绝不被还原成 tool_use / function_call，无 call_dsml_，无工具 item
//     索引与待回填委托，正文增量逐字等于原文；
//   - 同一响应混入真实 f11 委托时，只有真实调用进入原批次/索引并保留
//     ExecID / IDField，DSML 正文仍按原文保留。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cursorTextOnlyDSMLFull 是可被官方解析器完整还原的 DSML 工具调用块。
const cursorTextOnlyDSMLFull = "<｜DSML｜tool_calls>\n" +
	"<｜DSML｜invoke name=\"Bash\">\n" +
	"<｜DSML｜parameter name=\"command\" string=\"true\">echo hi</｜DSML｜parameter>\n" +
	"</｜DSML｜invoke>\n" +
	"</｜DSML｜tool_calls>"

// cursorTextOnlyDSMLFragment 是只有 invoke 开标签的残片（默认路径会清理，禁用后原样保留）。
const cursorTextOnlyDSMLFragment = "<｜DSML｜invoke name=\"Read\">残片"

// cursorTextOnlyReasoningFrames 构造 reasoning 通道的两帧：完整 DSML 块 +
// 跨帧前缀（在标记中间切开）+ 残片。外层用 <think> 包裹，使 parser 的文本双通道
// 切分把它们送入 reasoning_content。
func cursorTextOnlyReasoningFrames(t *testing.T) []string {
	t.Helper()
	first := "<think>思考A " + cursorTextOnlyDSMLFull + " 思考B <｜DS"
	second := "ML｜" + strings.TrimPrefix(cursorTextOnlyDSMLFull, "<｜DSML｜") +
		" 残片 " + cursorTextOnlyDSMLFragment + " 思考C</think>"
	return []string{first, second}
}

// cursorTextOnlyContentFrames 构造可见正文通道的两帧：完整 DSML 块 +
// 跨帧前缀 + 残片（无 think 标签，直接进 content）。
func cursorTextOnlyContentFrames(t *testing.T) []string {
	t.Helper()
	first := "正文A " + cursorTextOnlyDSMLFull + " 正文B <｜DS"
	second := "ML｜" + strings.TrimPrefix(cursorTextOnlyDSMLFull, "<｜DSML｜") +
		" 残片 " + cursorTextOnlyDSMLFragment + " 正文C"
	return []string{first, second}
}

// cursorTextOnlyBody 把文本帧（及可选真实 f11 委托帧）与 turn_ended 组装成带
// envelope 的上游流体，走真实 parser。
func cursorTextOnlyBody(t *testing.T, withRealCall bool) []byte {
	t.Helper()
	var frames []byte
	for _, text := range cursorTextOnlyReasoningFrames(t) {
		frames = append(frames, cursorTextDeltaFrame(t, text)...)
	}
	if withRealCall {
		// f11 mcp_args：exec_id=7、f15 id=exec-id、tool=get_weather。
		args := cursorSDKTransportBuildMcpArgs("call_real_f11", "get_weather", `{"city":"Beijing"}`)
		frames = append(frames, cursorEncodeEnvelope(cursorSDKTransportExecFrame(args))...)
	}
	for _, text := range cursorTextOnlyContentFrames(t) {
		frames = append(frames, cursorTextDeltaFrame(t, text)...)
	}
	frames = append(frames, cursorTurnEndedFrame(t,
		[2]uint64{1, 10}, [2]uint64{2, 5})...)
	return frames
}

// cursorTextOnlyRun 是一次端到端驱动的结果：目标协议原始输出、writer（供断言
// item 索引/state 选项）、parser 捕获的真实委托。
type cursorTextOnlyRun struct {
	body   string
	writer *cursorProtocolWriter
	calls  []cursorToolCallCapture
}

// cursorTextOnlyDrive 驱动完整链路：真实 protobuf 帧 → cursorSSEStream（parser）
// → 按 stream/nonstream 产出 Chat Completions 字节 → 实际 newCursorProtocolWriter
// → finalize → 目标协议输出。svc/session 传 nil（本卡不接索引登记，行为幂等跳过）。
func cursorTextOnlyDrive(t *testing.T, mode cursorProtocol, stream, withRealCall bool) cursorTextOnlyRun {
	t.Helper()
	p := cursorNewStreamParser("m")
	if withRealCall {
		// 独立 parser 路径需要真实 f4 原始字节才能按 tool_name enum 解码 mcp_args。
		p.attachToolSchemas(cursorSDKTransportDeclaredDefs(t, "get_weather"))
	}
	s := &cursorSSEStream{
		source: io.NopCloser(bytes.NewReader(cursorTextOnlyBody(t, withRealCall))),
		parser: p,
	}
	sseBody, err := io.ReadAll(s)
	require.NoError(t, err, "parser 必须成功产出 Chat Completions SSE")
	require.Contains(t, string(sseBody), "data:", "parser 必须产出数据帧")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	w := newCursorProtocolWriter(c.Writer, stream, mode, "m", cursorResponsesToolMeta{}, "resp-textonly", nil, nil)
	// 校验接线本身：state 上的禁用选项必须来自 newCursorProtocolWriter，而非测试手设。
	switch mode {
	case cursorProtocolAnthropic:
		require.NotNil(t, w.anthState)
		require.True(t, w.anthState.ConversionOptions.DisableDSMLToolCalls)
	case cursorProtocolResponses:
		require.NotNil(t, w.respState)
		require.True(t, w.respState.ConversionOptions.DisableDSMLToolCalls)
	}

	if stream {
		_, err = w.Write(sseBody)
	} else {
		var completion []byte
		completion, err = cursorAggregateSSEToCompletion(context.Background(), sseBody)
		require.NoError(t, err)
		_, err = w.Write(completion)
	}
	require.NoError(t, err)
	require.NoError(t, w.finalize())
	return cursorTextOnlyRun{body: rec.Body.String(), writer: w, calls: p.capturedToolCalls()}
}

// cursorTextOnlySSEData 取目标协议 SSE 的每条 data 负载（跳过注释与 [DONE]）。
func cursorTextOnlySSEData(body string) []string {
	var out []string
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data != "" && data != "[DONE]" {
				out = append(out, data)
			}
		}
	}
	return out
}

// cursorTextOnlyAssertNoDSML 两种协议共同的底线：不得出现 DSML 专用 ID 前缀。
func cursorTextOnlyAssertNoDSML(t *testing.T, body string) {
	t.Helper()
	require.NotContains(t, body, "call_dsml_", "输出不得出现 DSML 专用 ID 前缀")
}

// cursorTextOnlyAssertClean 纯 DSML 正文场景的额外底线：没有任何工具块/工具项。
func cursorTextOnlyAssertClean(t *testing.T, body string) {
	t.Helper()
	cursorTextOnlyAssertNoDSML(t, body)
	require.NotContains(t, body, `"tool_use"`, "纯 DSML 正文不得产生 tool_use")
	require.NotContains(t, body, `"function_call"`, "纯 DSML 正文不得产生 function_call")
}

// cursorTextOnlyAnthropicStreamText 收集 Messages 流式的 thinking/text 增量（不禁止真实工具项）。
func cursorTextOnlyAnthropicStreamText(t *testing.T, body string) (string, string) {
	t.Helper()
	var thinking, text string
	for _, data := range cursorTextOnlySSEData(body) {
		var e apicompat.AnthropicStreamEvent
		require.NoError(t, json.Unmarshal([]byte(data), &e))
		if e.Delta == nil {
			continue
		}
		switch e.Delta.Type {
		case "thinking_delta":
			thinking += e.Delta.Thinking
		case "text_delta":
			text += e.Delta.Text
		}
	}
	return thinking, text
}

// cursorTextOnlyResponsesStreamText 收集 Responses 流式的 reasoning/output_text 增量。
func cursorTextOnlyResponsesStreamText(t *testing.T, body string) (string, string) {
	t.Helper()
	var reasoning, text string
	for _, data := range cursorTextOnlySSEData(body) {
		var e apicompat.ResponsesStreamEvent
		require.NoError(t, json.Unmarshal([]byte(data), &e))
		if e.Item != nil {
			require.NotContains(t, e.Item.CallID, "call_dsml_")
		}
		switch e.Type {
		case "response.reasoning_summary_text.delta":
			reasoning += e.Delta
		case "response.output_text.delta":
			text += e.Delta
		}
	}
	return reasoning, text
}

// cursorTextOnlyAnthropicText 收集 Messages 非流式的 text 块正文（不禁止真实工具项）。
func cursorTextOnlyAnthropicText(t *testing.T, body string) string {
	t.Helper()
	var out apicompat.AnthropicResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	var text string
	for _, b := range out.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	return text
}

// cursorTextOnlyResponsesText 收集 Responses 非流式的 message 输出项正文。
func cursorTextOnlyResponsesText(t *testing.T, body string) string {
	t.Helper()
	var out apicompat.ResponsesResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	var text string
	for _, item := range out.Output {
		require.NotContains(t, item.CallID, "call_dsml_")
		if item.Type == "message" {
			for _, c := range item.Content {
				text += c.Text
			}
		}
	}
	return text
}

// 纯 DSML 正文四象限校验：无任何工具块/项，且增量/正文逐字等于原文。
func cursorTextOnlyCheckAnthropicStream(t *testing.T, body, wantReason, wantText string) {
	t.Helper()
	cursorTextOnlyAssertClean(t, body)
	reason, text := cursorTextOnlyAnthropicStreamText(t, body)
	require.Equal(t, wantReason, reason, "reasoning 增量必须逐字保留 DSML 原文")
	require.Equal(t, wantText, text, "正文增量必须逐字保留 DSML 原文")
}

func cursorTextOnlyCheckResponsesStream(t *testing.T, body, wantReason, wantText string) {
	t.Helper()
	cursorTextOnlyAssertClean(t, body)
	reason, text := cursorTextOnlyResponsesStreamText(t, body)
	require.Equal(t, wantReason, reason, "reasoning 增量必须逐字保留 DSML 原文")
	require.Equal(t, wantText, text, "正文增量必须逐字保留 DSML 原文")
}

// 非流式聚合器不带 reasoning_content（既有行为），仅断言正文。
func cursorTextOnlyCheckAnthropicCompletion(t *testing.T, body, _ string, wantText string) {
	t.Helper()
	cursorTextOnlyAssertClean(t, body)
	require.Equal(t, wantText, cursorTextOnlyAnthropicText(t, body), "非流式正文必须逐字保留 DSML 原文")
}

func cursorTextOnlyCheckResponsesCompletion(t *testing.T, body, _ string, wantText string) {
	t.Helper()
	cursorTextOnlyAssertClean(t, body)
	require.Equal(t, wantText, cursorTextOnlyResponsesText(t, body), "非流式正文必须逐字保留 DSML 原文")
}

// TestCursorSDKTransportTextOnlyDSML 流/非流 × Messages/Responses 四象限：
// 纯 DSML 正文不得变工具调用，正文增量逐字等于原文，无工具 item 索引/待回填委托。
func TestCursorSDKTransportTextOnlyDSML(t *testing.T) {
	reason := cursorTextOnlyReasoningFrames(t)
	content := cursorTextOnlyContentFrames(t)
	wantReason := strings.ReplaceAll(strings.ReplaceAll(reason[0]+reason[1], "<think>", ""), "</think>", "")
	wantText := content[0] + content[1]

	type checkFn func(*testing.T, string, string, string)
	cases := []struct {
		name   string
		mode   cursorProtocol
		stream bool
		check  checkFn
	}{
		{"stream_messages", cursorProtocolAnthropic, true, cursorTextOnlyCheckAnthropicStream},
		{"stream_responses", cursorProtocolResponses, true, cursorTextOnlyCheckResponsesStream},
		{"nonstream_messages", cursorProtocolAnthropic, false, cursorTextOnlyCheckAnthropicCompletion},
		{"nonstream_responses", cursorProtocolResponses, false, cursorTextOnlyCheckResponsesCompletion},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			run := cursorTextOnlyDrive(t, c.mode, c.stream, false)
			require.Empty(t, run.writer.collectedItemIDs(), "纯文本不得登记任何工具 item 索引/待回填委托")
			require.Empty(t, run.calls, "纯 DSML 正文不得产生任何委托")
			if c.stream {
				c.check(t, run.body, wantReason, wantText)
			} else {
				// 非流式聚合器不携带 reasoning_content（既有行为），仅断言正文。
				c.check(t, run.body, "", wantText)
			}
		})
	}
}

// TestCursorSDKTransportTextOnlyMixedRealCall 同一响应混入真实 f11 委托：
// 只有真实调用进入原批次/索引并保留 ExecID/IDField，DSML 正文仍按原文保留。
func TestCursorSDKTransportTextOnlyMixedRealCall(t *testing.T) {
	reason := cursorTextOnlyReasoningFrames(t)
	content := cursorTextOnlyContentFrames(t)
	wantReason := strings.ReplaceAll(strings.ReplaceAll(reason[0]+reason[1], "<think>", ""), "</think>", "")
	wantText := content[0] + content[1]

	assertDelegation := func(t *testing.T, run cursorTextOnlyRun) {
		t.Helper()
		require.Len(t, run.calls, 1, "只有真实 f11 委托进入批次")
		call := run.calls[0]
		require.Equal(t, "call_real_f11", call.ID)
		require.Equal(t, "get_weather", call.Name)
		require.Equal(t, uint64(7), call.ExecID, "真实 ExecID 必须原样保留")
		require.Equal(t, []byte("exec-id"), call.IDField, "真实 IDField 必须原样保留")
		require.NotContains(t, call.ID, "call_dsml_", "真实调用不得带 DSML 前缀")
		cursorTextOnlyAssertNoDSML(t, run.body)
		require.Contains(t, run.body, "get_weather", "真实工具名必须交付")
		require.Contains(t, run.body, "call_real_f11", "真实调用 ID（wireID）必须交付")
	}

	cases := []struct {
		name   string
		mode   cursorProtocol
		stream bool
	}{
		{"stream_messages", cursorProtocolAnthropic, true},
		{"stream_responses", cursorProtocolResponses, true},
		{"nonstream_messages", cursorProtocolAnthropic, false},
		{"nonstream_responses", cursorProtocolResponses, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			run := cursorTextOnlyDrive(t, c.mode, c.stream, true)
			assertDelegation(t, run)

			// 输出恰好一项真实工具调用（复用上游批次/ID，绝非 DSML 还原），且正文逐字保留。
			if c.stream {
				var tools int
				for _, data := range cursorTextOnlySSEData(run.body) {
					if c.mode == cursorProtocolAnthropic {
						var e apicompat.AnthropicStreamEvent
						require.NoError(t, json.Unmarshal([]byte(data), &e))
						if e.Type == "content_block_start" && e.ContentBlock != nil &&
							e.ContentBlock.Type == "tool_use" {
							tools++
							require.Equal(t, "call_real_f11", e.ContentBlock.ID)
							require.Equal(t, "get_weather", e.ContentBlock.Name)
						}
					} else {
						var e apicompat.ResponsesStreamEvent
						require.NoError(t, json.Unmarshal([]byte(data), &e))
						if e.Type == "response.output_item.added" && e.Item != nil &&
							e.Item.Type == "function_call" {
							tools++
							require.Equal(t, "call_real_f11", e.Item.CallID)
							require.Equal(t, "get_weather", e.Item.Name)
						}
					}
				}
				require.Equal(t, 1, tools, "只有真实 f11 委托产生一项工具调用")
				var gotReason, gotText string
				if c.mode == cursorProtocolAnthropic {
					gotReason, gotText = cursorTextOnlyAnthropicStreamText(t, run.body)
				} else {
					gotReason, gotText = cursorTextOnlyResponsesStreamText(t, run.body)
				}
				require.Equal(t, wantReason, gotReason, "混入真实调用时 reasoning 仍逐字保留")
				require.Equal(t, wantText, gotText, "混入真实调用时正文仍逐字保留")
			} else {
				// 非流式：真实调用 + 正文并存，正文仍逐字保留。
				var tools int
				var gotText string
				if c.mode == cursorProtocolAnthropic {
					var out apicompat.AnthropicResponse
					require.NoError(t, json.Unmarshal([]byte(run.body), &out))
					for _, b := range out.Content {
						switch b.Type {
						case "tool_use":
							tools++
							require.Equal(t, "call_real_f11", b.ID)
							require.Equal(t, "get_weather", b.Name)
						}
					}
					gotText = cursorTextOnlyAnthropicText(t, run.body)
				} else {
					var out apicompat.ResponsesResponse
					require.NoError(t, json.Unmarshal([]byte(run.body), &out))
					for _, item := range out.Output {
						if item.Type == "function_call" {
							tools++
							require.Equal(t, "call_real_f11", item.CallID)
							require.Equal(t, "get_weather", item.Name)
						}
					}
					gotText = cursorTextOnlyResponsesText(t, run.body)
				}
				require.Equal(t, 1, tools, "只有真实 f11 委托产生一项工具调用")
				require.Equal(t, wantText, gotText, "混入真实调用时正文仍逐字保留")
			}
		})
	}
}
