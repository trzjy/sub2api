//go:build unit

package service

// CUR-TXT4P：Cursor writer 禁用共享 DSML 的端到端验收（四协议 × 流/非流）。
//
// 真实 protobuf 文本帧（reasoning/content 各含完整 DSML 块 + 跨帧前缀 + 残片）
// 经 parser→聚合→newCursorProtocolWriter 实际接线，Chat/Messages/Responses/Gemini ×
// 流/非流 八象限：
//   - DSML 正文绝不被还原成 tool_use / function_call / functionCall / tool_calls，
//     无 call_dsml_，正文增量逐字等于原文；会话委托记录为空、toolCallIDs 为空；
//   - 同一响应混入真实 f11 委托时，由真实 duplex 解码并写入会话共享委托记录，
//     writer 带非空 svc/session，只有真实调用进入交付与登记：ExecID/IDField/
//     ArgsJSON 原样保留，DSML 正文仍按原文保留，不产生第二条登记。
//
// Chat 出口按生产 beginResponseWriter 直出（writer 即 c.Writer），其余三协议走
// newCursorProtocolWriter 转换；四协议共用同一 parser 与同一会话委托记录。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
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

// cursorTextOnlyRealCallFrame 构造真实 f11 mcp_args 帧（exec_id=7、f15 id=exec-id、
// tool=get_weather）。duplex 吃未包 envelope 的原始帧，parser 流体内吃 envelope 帧。
func cursorTextOnlyRealCallFrame(t *testing.T) []byte {
	t.Helper()
	args := cursorSDKTransportBuildMcpArgs("call_real_f11", "get_weather", `{"city":"Beijing"}`)
	return cursorSDKTransportExecFrame(args)
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
		frames = append(frames, cursorEncodeEnvelope(cursorTextOnlyRealCallFrame(t))...)
	}
	for _, text := range cursorTextOnlyContentFrames(t) {
		frames = append(frames, cursorTextDeltaFrame(t, text)...)
	}
	frames = append(frames, cursorTurnEndedFrame(t,
		[2]uint64{1, 10}, [2]uint64{2, 5})...)
	return frames
}

// cursorTextOnlyRun 是一次端到端驱动的结果：目标协议原始输出、writer、parser 捕获的
// 真实委托、以及绑定了同一委托记录与真实 writer 依赖的 session。
type cursorTextOnlyRun struct {
	body    string
	writer  *cursorProtocolWriter
	calls   []cursorToolCallCapture
	session *cursorSessionRecord
}

// cursorTextOnlyDrive 驱动完整链路：真实 protobuf 帧 → 真实 duplex 解码 f11 写入
// session 共享委托记录 → cursorSSEStream（parser 只读同一记录）→ 按 stream/nonstream
// 产出目标协议字节 → 实际 newCursorProtocolWriter（非空 svc/session）→ finalize。
func cursorTextOnlyDrive(t *testing.T, mode cursorProtocol, stream, withRealCall bool) cursorTextOnlyRun {
	t.Helper()

	// 会话委托记录是真实 newCursorToolDelegations；parser 挂同一份只读，不二次解码。
	// toolCallIDs 由本驱动按"实际交付的 wireID"登记（生产同语义在统一索引与待交付批里
	// 完成；record 上的镜像由本测试维护，供断言观察）。
	delegations := newCursorToolDelegations()
	stamp := fmt.Sprintf("p%d-s%t-r%t", mode, stream, withRealCall)
	session := &cursorSessionRecord{
		id:          "sess-" + stamp,
		spec:        cursorSessionSpec{protocol: mode},
		state:       cursorSessionRunning,
		toolCallIDs: make(map[string]struct{}),
		pending:     &cursorPendingBatch{batchID: "batch-" + stamp},
	}
	session.attachDelegations(delegations)

	// 真实 duplex：f4 原始字节来自 cursorConvertRequest，duplex 拥有 mcp_args 解码权。
	initBody, err := cursorConvertRequest(cursorSDKTransportRequest("get_weather"), "m")
	require.NoError(t, err)
	d, err := newCursorAgentDuplex(initBody, delegations)
	require.NoError(t, err)
	defer d.close(nil)

	p := cursorNewStreamParser("m")
	p.attachToolDelegations(delegations)
	p.attachToolSchemas(d.declaredToolDefs)
	if withRealCall {
		done, e := d.respond(cursorTextOnlyRealCallFrame(t))
		require.NoError(t, e)
		require.False(t, done, "f11 委托不得终结双向连接")
	}

	s := &cursorSSEStream{
		source: io.NopCloser(bytes.NewReader(cursorTextOnlyBody(t, withRealCall))),
		parser: p,
	}
	sseBody, err := io.ReadAll(s)
	require.NoError(t, err, "parser 必须成功产出 Chat Completions SSE")
	require.Contains(t, string(sseBody), "data:", "parser 必须产出数据帧")

	// 非空 svc/session：Responses 写出前 registerDeliveredItems 走真实索引登记。
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{}}
	svc := newCursorTestExecutionService(t, repo, NewCursorTestEncryptor(),
		cursorTestDeviceIDs(repo), NewCursorUpstreamForwarder())

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	w := newCursorProtocolWriter(c.Writer, stream, mode, "m", cursorResponsesToolMeta{},
		"resp-textonly", svc, session)
	// 校验接线本身：state 上的禁用选项必须来自 newCursorProtocolWriter，而非测试手设。
	switch mode {
	case cursorProtocolAnthropic:
		require.NotNil(t, w.anthState)
		require.True(t, w.anthState.ConversionOptions.DisableDSMLToolCalls)
	case cursorProtocolResponses:
		require.NotNil(t, w.respState)
		require.True(t, w.respState.ConversionOptions.DisableDSMLToolCalls)
	}

	if mode == cursorProtocolChat {
		// 生产 beginResponseWriter 对 Chat 直出 c.Writer（writer 即父 writer），
		// finalizeResponseWriter 只 Flush；本测试按同一路径写给 w.ResponseWriter。
		if stream {
			_, err = io.WriteString(w.ResponseWriter, string(sseBody))
		} else {
			var completion []byte
			completion, err = cursorAggregateSSEToCompletion(context.Background(), sseBody)
			require.NoError(t, err)
			_, err = io.WriteString(w.ResponseWriter, string(completion))
		}
	} else {
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
	}
	require.NoError(t, err)

	for _, call := range delegations.snapshot() {
		session.toolCallIDs[call.wireID()] = struct{}{}
	}
	return cursorTextOnlyRun{body: rec.Body.String(), writer: w,
		calls: p.capturedToolCalls(), session: session}
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

// cursorTextOnlyAssertNoDSML 四协议共同的底线：不得出现 DSML 专用 ID 前缀。
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
	require.NotContains(t, body, `"functionCall"`, "纯 DSML 正文不得产生 functionCall")
	require.NotContains(t, body, `"tool_calls"`, "纯 DSML 正文不得产生 tool_calls")
}

// ---- Messages（Anthropic）输出读取 ----

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

// ---- Responses 输出读取 ----

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

// ---- Chat（直出）输出读取 ----

func cursorTextOnlyChatStreamText(t *testing.T, body string) (string, string) {
	t.Helper()
	var reason, text string
	for _, data := range cursorTextOnlySSEData(body) {
		var e apicompat.ChatCompletionsChunk
		require.NoError(t, json.Unmarshal([]byte(data), &e))
		for _, ch := range e.Choices {
			if ch.Delta.ReasoningContent != nil {
				reason += *ch.Delta.ReasoningContent
			} else if ch.Delta.Reasoning != nil {
				reason += *ch.Delta.Reasoning
			}
			if ch.Delta.Content != nil {
				text += *ch.Delta.Content
			}
		}
	}
	return reason, text
}

func cursorTextOnlyChatText(t *testing.T, body string) string {
	t.Helper()
	var out apicompat.ChatCompletionsResponse
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	if len(out.Choices) == 0 {
		return ""
	}
	return cursorChatMessageText(out.Choices[0].Message.Content)
}

// ---- Gemini 输出读取（按 Gemini 实际字段，不新造解析器） ----

type cursorTextOnlyGeminiCall struct {
	ID   string
	Name string
	Args map[string]any
}

type cursorTextOnlyGeminiFrame struct {
	Candidates []struct {
		Content struct {
			Parts []map[string]any `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func cursorTextOnlyGeminiParts(t *testing.T, data string) (string, []cursorTextOnlyGeminiCall) {
	t.Helper()
	var frame cursorTextOnlyGeminiFrame
	require.NoError(t, json.Unmarshal([]byte(data), &frame))
	var text string
	var calls []cursorTextOnlyGeminiCall
	for _, cand := range frame.Candidates {
		for _, part := range cand.Content.Parts {
			if s, ok := part["text"].(string); ok {
				text += s
			}
			if fc, ok := part["functionCall"].(map[string]any); ok {
				call := cursorTextOnlyGeminiCall{}
				call.ID, _ = fc["id"].(string)
				call.Name, _ = fc["name"].(string)
				call.Args, _ = fc["args"].(map[string]any)
				calls = append(calls, call)
			}
		}
	}
	return text, calls
}

func cursorTextOnlyGeminiStreamText(t *testing.T, body string) (string, []cursorTextOnlyGeminiCall) {
	t.Helper()
	var text string
	var calls []cursorTextOnlyGeminiCall
	for _, data := range cursorTextOnlySSEData(body) {
		t1, c1 := cursorTextOnlyGeminiParts(t, data)
		text += t1
		calls = append(calls, c1...)
	}
	return text, calls
}

// cursorTextOnlyText 按协议实际字段取（reason, text）；非流式聚合器不带
// reasoning_content（既有行为），Gemini writer 不发射思考增量，二者 reason 为空。
func cursorTextOnlyText(t *testing.T, mode cursorProtocol, stream bool, body string) (string, string) {
	t.Helper()
	switch mode {
	case cursorProtocolAnthropic:
		if stream {
			return cursorTextOnlyAnthropicStreamText(t, body)
		}
		return "", cursorTextOnlyAnthropicText(t, body)
	case cursorProtocolResponses:
		if stream {
			return cursorTextOnlyResponsesStreamText(t, body)
		}
		return "", cursorTextOnlyResponsesText(t, body)
	case cursorProtocolChat:
		if stream {
			return cursorTextOnlyChatStreamText(t, body)
		}
		return "", cursorTextOnlyChatText(t, body)
	case cursorProtocolGemini:
		if stream {
			text, _ := cursorTextOnlyGeminiStreamText(t, body)
			return "", text
		}
		text, _ := cursorTextOnlyGeminiParts(t, body)
		return "", text
	default:
		t.Fatalf("unsupported protocol %d", mode)
		return "", ""
	}
}

// cursorTextOnlyToolItems 按协议实际字段取交付的工具项（ID 为交付给客户端的 wireID）。
func cursorTextOnlyToolItems(t *testing.T, mode cursorProtocol, stream bool, body string) []cursorTextOnlyGeminiCall {
	t.Helper()
	var out []cursorTextOnlyGeminiCall
	switch mode {
	case cursorProtocolGemini:
		if stream {
			_, out = cursorTextOnlyGeminiStreamText(t, body)
		} else {
			_, out = cursorTextOnlyGeminiParts(t, body)
		}
	case cursorProtocolChat:
		if stream {
			for _, data := range cursorTextOnlySSEData(body) {
				var e apicompat.ChatCompletionsChunk
				require.NoError(t, json.Unmarshal([]byte(data), &e))
				for _, ch := range e.Choices {
					for _, tc := range ch.Delta.ToolCalls {
						// Chat 流式同一 index 会先发 id/name 起点帧，再发若干仅含
						// arguments 的续帧；只按起点帧计一项工具调用。
						if tc.ID == "" {
							continue
						}
						out = append(out, cursorTextOnlyGeminiCall{ID: tc.ID, Name: tc.Function.Name})
					}
				}
			}
		} else {
			var resp apicompat.ChatCompletionsResponse
			require.NoError(t, json.Unmarshal([]byte(body), &resp))
			if len(resp.Choices) > 0 {
				for _, tc := range resp.Choices[0].Message.ToolCalls {
					out = append(out, cursorTextOnlyGeminiCall{ID: tc.ID, Name: tc.Function.Name})
				}
			}
		}
	case cursorProtocolAnthropic:
		if stream {
			for _, data := range cursorTextOnlySSEData(body) {
				var e apicompat.AnthropicStreamEvent
				require.NoError(t, json.Unmarshal([]byte(data), &e))
				if e.Type == "content_block_start" && e.ContentBlock != nil &&
					e.ContentBlock.Type == "tool_use" {
					out = append(out, cursorTextOnlyGeminiCall{ID: e.ContentBlock.ID, Name: e.ContentBlock.Name})
				}
			}
		} else {
			var resp apicompat.AnthropicResponse
			require.NoError(t, json.Unmarshal([]byte(body), &resp))
			for _, b := range resp.Content {
				if b.Type == "tool_use" {
					out = append(out, cursorTextOnlyGeminiCall{ID: b.ID, Name: b.Name})
				}
			}
		}
	case cursorProtocolResponses:
		if stream {
			for _, data := range cursorTextOnlySSEData(body) {
				var e apicompat.ResponsesStreamEvent
				require.NoError(t, json.Unmarshal([]byte(data), &e))
				if e.Type == "response.output_item.added" && e.Item != nil &&
					e.Item.Type == "function_call" {
					out = append(out, cursorTextOnlyGeminiCall{ID: e.Item.CallID, Name: e.Item.Name})
				}
			}
		} else {
			var resp apicompat.ResponsesResponse
			require.NoError(t, json.Unmarshal([]byte(body), &resp))
			for _, item := range resp.Output {
				if item.Type == "function_call" {
					out = append(out, cursorTextOnlyGeminiCall{ID: item.CallID, Name: item.Name})
				}
			}
		}
	}
	return out
}

// cursorTextOnlyAssertSessionEmpty 文本-only：会话侧无任何委托/调用登记。
func cursorTextOnlyAssertSessionEmpty(t *testing.T, run cursorTextOnlyRun) {
	t.Helper()
	require.Empty(t, run.session.delegations.snapshot(), "纯 DSML 正文不得写入任何委托")
	require.Empty(t, run.session.toolCallIDs, "纯 DSML 正文不得登记任何调用 ID")
	require.Empty(t, run.calls, "纯 DSML 正文不得产生任何委托")
	require.Empty(t, run.writer.collectedItemIDs(), "纯文本不得登记任何工具 item 索引")
}

// cursorTextOnlyAssertSessionRealCall 混入真实 f11：会话侧恰一条真实登记。
func cursorTextOnlyAssertSessionRealCall(t *testing.T, run cursorTextOnlyRun) {
	t.Helper()
	snap := run.session.delegations.snapshot()
	require.Len(t, snap, 1, "只有真实 f11 委托进入会话记录")
	call := snap[0]
	require.Equal(t, "call_real_f11", call.ID)
	require.Equal(t, "get_weather", call.Name)
	require.Equal(t, uint64(7), call.ExecID, "真实 ExecID 必须原样保留")
	require.Equal(t, []byte("exec-id"), call.IDField, "真实 IDField 必须原样保留")
	require.Equal(t, `{"city":"Beijing"}`, call.ArgsJSON, "真实 ArgsJSON 必须逐字节保留")
	require.NotContains(t, call.ID, "call_dsml_", "真实调用不得带 DSML 前缀")
	require.Equal(t, map[string]struct{}{"call_real_f11": {}}, run.session.toolCallIDs,
		"toolCallIDs 恰含交付给客户端的真实调用 ID")
	require.Equal(t, snap, run.calls, "parser 与会话读取同一份真实委托记录")
	cursorTextOnlyAssertNoDSML(t, run.body)
}

// cursorTextOnlyCases 四协议 × 流/非流八象限。
func cursorTextOnlyCases() []struct {
	name   string
	mode   cursorProtocol
	stream bool
} {
	return []struct {
		name   string
		mode   cursorProtocol
		stream bool
	}{
		{"stream_chat", cursorProtocolChat, true},
		{"stream_messages", cursorProtocolAnthropic, true},
		{"stream_responses", cursorProtocolResponses, true},
		{"stream_gemini", cursorProtocolGemini, true},
		{"nonstream_chat", cursorProtocolChat, false},
		{"nonstream_messages", cursorProtocolAnthropic, false},
		{"nonstream_responses", cursorProtocolResponses, false},
		{"nonstream_gemini", cursorProtocolGemini, false},
	}
}

// TestCursorSDKTransportTextOnlyDSML 流/非流 × Chat/Messages/Responses/Gemini 八象限：
// 纯 DSML 正文不得变工具调用，正文/推理增量逐字等于原文，会话无登记。
func TestCursorSDKTransportTextOnlyDSML(t *testing.T) {
	reason := cursorTextOnlyReasoningFrames(t)
	content := cursorTextOnlyContentFrames(t)
	wantReason := strings.ReplaceAll(strings.ReplaceAll(reason[0]+reason[1], "<think>", ""), "</think>", "")
	wantText := content[0] + content[1]

	for _, c := range cursorTextOnlyCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			run := cursorTextOnlyDrive(t, c.mode, c.stream, false)
			cursorTextOnlyAssertSessionEmpty(t, run)

			cursorTextOnlyAssertClean(t, run.body)
			require.Empty(t, cursorTextOnlyToolItems(t, c.mode, c.stream, run.body),
				"纯 DSML 正文不得交付任何工具项")
			gotReason, gotText := cursorTextOnlyText(t, c.mode, c.stream, run.body)
			require.Equal(t, wantText, gotText, "正文增量必须逐字保留 DSML 原文")
			// 非流式聚合器不带 reasoning_content；Gemini writer 不发射思考增量。
			if c.stream && c.mode != cursorProtocolGemini {
				require.Equal(t, wantReason, gotReason, "reasoning 增量必须逐字保留 DSML 原文")
			} else {
				require.Empty(t, gotReason, "该出口不得携带 reasoning 增量")
			}
			t.Logf("[DSML/%s] text=%d bytes reason=%d bytes tools=0 delegations=0 toolCallIDs=0",
				c.name, len(gotText), len(gotReason))
		})
	}
}

// TestCursorSDKTransportTextOnlyMixedRealCall 同一响应混入真实 f11 委托：
// 只有真实调用进入会话登记并交付，ExecID/IDField/ArgsJSON 原样保留，DSML 正文原样。
func TestCursorSDKTransportTextOnlyMixedRealCall(t *testing.T) {
	reason := cursorTextOnlyReasoningFrames(t)
	content := cursorTextOnlyContentFrames(t)
	wantReason := strings.ReplaceAll(strings.ReplaceAll(reason[0]+reason[1], "<think>", ""), "</think>", "")
	wantText := content[0] + content[1]

	for _, c := range cursorTextOnlyCases() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			run := cursorTextOnlyDrive(t, c.mode, c.stream, true)
			cursorTextOnlyAssertSessionRealCall(t, run)

			items := cursorTextOnlyToolItems(t, c.mode, c.stream, run.body)
			require.Len(t, items, 1, "只有真实 f11 委托产生一项工具调用")
			require.Equal(t, "call_real_f11", items[0].ID, "真实调用 ID 必须交付给客户端")
			require.Equal(t, "get_weather", items[0].Name, "真实工具名必须交付")
			if c.mode == cursorProtocolGemini {
				require.Equal(t, map[string]any{"city": "Beijing"}, items[0].Args,
					"Gemini functionCall 参数必须逐字节等价")
			}

			gotReason, gotText := cursorTextOnlyText(t, c.mode, c.stream, run.body)
			require.Equal(t, wantText, gotText, "混入真实调用时正文仍逐字保留")
			if c.stream && c.mode != cursorProtocolGemini {
				require.Equal(t, wantReason, gotReason, "混入真实调用时 reasoning 仍逐字保留")
			} else {
				require.Empty(t, gotReason, "该出口不得携带 reasoning 增量")
			}
			t.Logf("[MIXED/%s] text=%d bytes reason=%d bytes tools=1 delegations=1 toolCallIDs=1",
				c.name, len(gotText), len(gotReason))
		})
	}
}
