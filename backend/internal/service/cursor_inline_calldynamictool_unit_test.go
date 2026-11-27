//go:build unit

package service

// Cursor CallDynamicTool 私有块归一化的定向回归。
//
// 事故原文（sub2api-inline-toolcall-rootcause-20261127/04-cli-response-evidence.txt）
// 里的外层标记、分隔符、字段边界都按真实字节构造；不从终端视觉猜码点。
// 所有用例都不碰生产账号/Key，fixture 只含本地临时路径与虚构命令。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const cursorCDTTestCommand = "tail -30 /mnt/data/work/.omx/tmp/ruleA-fix-20261127/grid4.log; echo '--- ps ---'; ps -p 2180385 -o pid,etime,rss 2>/dev/null || echo END"

// cursorCDTIncidentText 是事故第 4 轮的文本原文（逐字节）。
const cursorCDTIncidentText = "]<]minimax[>[<invoke name=\"CallDynamicTool\">]" +
	"<]minimax[>[<arguments>]" +
	"<]minimax[>[<command>" + cursorCDTTestCommand + "]" +
	"<]minimax[>[</command>]" +
	"<]minimax[>[</arguments>]" +
	"<]minimax[>[<namespace>sub2api]" +
	"<]minimax[>[</namespace>]" +
	"<]minimax[>[<toolName>Bash]" +
	"<]minimax[>[</toolTime>]" + // 上游笔误，真实原文如此
	"<]minimax[>[</arguments>]" +
	"<]minimax[>[<toolName>Bash]" +
	"<]minimax[>[</toolName>]" +
	"<]minimax[>[</invoke>"

func cursorCDTDeclaredTools() []apicompat.ChatTool {
	return []apicompat.ChatTool{{
		Type: "function",
		Function: &apicompat.ChatFunction{
			Name:       "Bash",
			Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		},
	}}
}

// cursorCDTStubUUID 固定 call id，便于精确断言；所有用例结束后还原。
func cursorCDTStubUUID(t *testing.T) string {
	t.Helper()
	old := cursorNewUUID
	cursorNewUUID = func() string { return "call-cdt-unit-uuid" }
	t.Cleanup(func() { cursorNewUUID = old })
	return "call-cdt-unit-uuid"
}

func cursorCDTAssertNoLeak(t *testing.T, text string) {
	t.Helper()
	for _, marker := range []string{"<]minimax[>", "CallDynamicTool", "<invoke", "<command>", "</command>", "<toolName>", "</toolTime>"} {
		require.NotContains(t, text, marker, "私有标记不得泄漏给客户端")
	}
}

// 1. 事故格式的确定性 fixture。
func TestCursorInlineCallDynamicToolIncidentFixture(t *testing.T) {
	wantID := cursorCDTStubUUID(t)
	n := newCursorCDTNormalizer(cursorCDTDeclaredTools())
	safe, calls, err := n.consume(cursorCDTIncidentText)
	require.NoError(t, err)
	tail, err := n.finish()
	require.NoError(t, err)
	safe += tail

	require.Len(t, calls, 1)
	require.Equal(t, wantID, calls[0].ID)
	require.NotEqual(t, "CallDynamicTool", calls[0].ID, "包装名不得当 call id")
	require.Equal(t, "Bash", calls[0].Name)
	require.JSONEq(t, `{"command":"`+cursorCDTTestCommand+`"}`, calls[0].ArgsJSON)
	require.NotContains(t, calls[0].ArgsJSON, "CallDynamicTool")
	require.NotContains(t, calls[0].ArgsJSON, "namespace")
	cursorCDTAssertNoLeak(t, safe)
	require.Empty(t, safe, "整块私有文本必须从正文完全剔除")
}

// 2. 标记跨帧：开标记、分隔符、字段标签、闭合标记分别切开，行为与单帧一致。
func TestCursorInlineCallDynamicToolCrossFrame(t *testing.T) {
	cursorCDTStubUUID(t)
	whole := "AAA" + cursorCDTIncidentText + "BBB"
	wantSafe, wantCalls, err := newCursorCDTNormalizer(cursorCDTDeclaredTools()).consume(whole)
	require.NoError(t, err)
	require.Len(t, wantCalls, 1)

	// 在每个标记边界附近切帧（含开标记中间、分隔符中间、command 标签中间、闭标记中间）。
	cuts := []int{
		3 + 5,                                 // 开标记内部
		3 + len(cursorCDTBlockOpen) + 3,       // 第一个字段分隔符内部
		strings.Index(whole, "<command>") + 4, // command 标签内部
		strings.Index(whole, cursorCDTTestCommand) + 7, // 命令值内部
		strings.Index(whole, "</invoke>") + 3,          // 闭标记内部
	}
	for _, cut := range cuts {
		if cut <= 0 || cut >= len(whole) {
			t.Fatalf("bad cut %d", cut)
		}
		n := newCursorCDTNormalizer(cursorCDTDeclaredTools())
		var safe strings.Builder
		var calls []cursorInlineCDTCall
		for _, part := range []string{whole[:cut], whole[cut:]} {
			s, c, e := n.consume(part)
			require.NoError(t, e)
			safe.WriteString(s)
			calls = append(calls, c...)
		}
		tail, e := n.finish()
		require.NoError(t, e)
		safe.WriteString(tail)
		require.Equal(t, wantSafe, safe.String(), "跨帧切分不得改变正文")
		require.Equal(t, wantCalls, calls, "跨帧切分不得改变结构化调用")
		cursorCDTAssertNoLeak(t, safe.String())
	}
}

// 3. 私有块前后普通文本：只输出一次、顺序不变。
func TestCursorInlineCallDynamicToolSurroundingText(t *testing.T) {
	cursorCDTStubUUID(t)
	n := newCursorCDTNormalizer(cursorCDTDeclaredTools())
	safe, calls, err := n.consume("前言 text " + cursorCDTIncidentText + " 尾注 tail")
	require.NoError(t, err)
	tail, err := n.finish()
	require.NoError(t, err)
	require.Equal(t, "前言 text  尾注 tail", safe+tail)
	require.Len(t, calls, 1)
	require.Equal(t, 1, strings.Count(safe+tail, "前言 text"))
	require.Equal(t, 1, strings.Count(safe+tail, "尾注 tail"))
	cursorCDTAssertNoLeak(t, safe+tail)
}

// 4. 无关文本 / 提及字符串但不完整的结构不得误转。
func TestCursorInlineCallDynamicToolNoFalsePositive(t *testing.T) {
	cursorCDTStubUUID(t)
	for _, text := range []string{
		"普通正文，提到了 CallDynamicTool 与 <invoke> 但没有私有块",
		"<]minimax[> 只有分隔符没有成对结构",
		"toolName Bash command echo hi",
		"</invoke> 只有闭标记",
	} {
		n := newCursorCDTNormalizer(cursorCDTDeclaredTools())
		safe, calls, err := n.consume(text)
		require.NoError(t, err)
		tail, err := n.finish()
		require.NoError(t, err)
		require.Empty(t, calls, "不得误转: "+text)
		require.Equal(t, text, safe+tail, "普通正文必须逐字保留: "+text)
	}

	// 开标记出现但闭标记缺失：必须失败关闭，不退回正文、不正常收尾。
	n := newCursorCDTNormalizer(cursorCDTDeclaredTools())
	safe, calls, err := n.consume("前言" + strings.TrimSuffix(cursorCDTIncidentText, "<]minimax[>[</invoke>"))
	require.NoError(t, err)
	require.Empty(t, calls)
	require.Equal(t, "前言", safe)
	_, err = n.finish()
	require.Error(t, err, "半块必须失败关闭")
}

// 5. 内层 toolName 未在本次声明集合中时不得映射到别处。
func TestCursorInlineCallDynamicToolUndeclaredRejected(t *testing.T) {
	cursorCDTStubUUID(t)
	tools := []apicompat.ChatTool{{
		Type: "function",
		Function: &apicompat.ChatFunction{
			Name:       "Read",
			Parameters: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}}}`),
		},
	}}
	n := newCursorCDTNormalizer(tools)
	_, calls, err := n.consume(cursorCDTIncidentText)
	require.Error(t, err, "Bash 未声明必须失败关闭")
	require.Empty(t, calls)
	_, err = n.finish()
	require.Error(t, err)
}

// 6/7. 流式与非流式 /v1/messages 出口：原生 tool_use + stop_reason=tool_use。
func TestCursorInlineCallDynamicToolMessagesStreamingAndBuffered(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := fmt.Sprintf("stream=%t", stream)
		t.Run(name, func(t *testing.T) {
			cursorCDTStubUUID(t)
			body := append(cursorTextDeltaFrame(t, cursorCDTIncidentText),
				cursorTurnEndedFrame(t,
					[2]uint64{cursorFldTurnEndedInputTokens, 10},
					[2]uint64{cursorFldTurnEndedOutputTokens, 5})...)
			p := cursorNewStreamParser("m")
			p.attachInlineDeclaredTools(cursorCDTDeclaredTools())
			s := &cursorSSEStream{source: io.NopCloser(bytes.NewReader(body)), parser: p}
			sse, err := io.ReadAll(s)
			require.NoError(t, err)
			require.Contains(t, string(sse), `"finish_reason":"tool_calls"`)
			require.Contains(t, string(sse), `"function":{"name":"Bash"`)
			cursorCDTAssertNoLeak(t, string(sse))
			require.Len(t, p.inlineCalls, 1)
			require.Empty(t, p.capturedToolCalls(), "内联调用不得进入原生委托记录")

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			w := newCursorProtocolWriter(c.Writer, stream, cursorProtocolAnthropic, "m",
				cursorResponsesToolMeta{}, "resp-cdt", nil, nil)
			if stream {
				_, err = w.Write(sse)
			} else {
				completion, ce := cursorAggregateSSEToCompletion(context.Background(), sse)
				require.NoError(t, ce)
				_, err = w.Write(completion)
			}
			require.NoError(t, err)
			require.NoError(t, w.finalize())
			out := rec.Body.String()
			require.Contains(t, out, `"type":"tool_use"`)
			require.Contains(t, out, `"name":"Bash"`)
			require.Contains(t, out, `"stop_reason":"tool_use"`)
			require.NotContains(t, out, `"stop_reason":"end_turn"`)
			if stream {
				require.Contains(t, out, "event: message_stop")
			} else {
				require.Contains(t, out, `"type":"message"`)
			}
			cursorCDTAssertNoLeak(t, out)
		})
	}
}

// cursorCDTMessagesHistoryBody 构造下一轮 Messages 请求：
// user → assistant(tool_use, 客户端已执行) → tool(tool_result) → user(继续)。
// callID 是本轮归一输出的真实 UUID；工具已由客户端完成，不得再次委派。
func cursorCDTMessagesHistoryBody(callID string) string {
	return fmt.Sprintf(`{
	  "model":"cursor-sonnet","max_tokens":512,"stream":true,
	  "tools":[{"name":"Bash","description":"execute shell commands",
	    "input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],
	  "messages":[
	    {"role":"user","content":"check the log"},
	    {"role":"assistant","content":[
	      {"type":"text","text":"running"},
	      {"type":"tool_use","id":%q,"name":"Bash","input":{"command":"tail -30 /tmp/grid4.log"}}
	    ]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"LOG-OK"}]},
	    {"role":"user","content":"continue with the result"}
	  ]
	}`, callID, callID)
}

// cursorCDTTextOnlyUpstream 是一次只吐普通文本 + 真实 usage 的假上游。
type cursorCDTTextOnlyUpstream struct {
	mu       sync.Mutex
	runs     int
	requests [][]byte
	upBuf    []byte
	readOff  int
}

func (u *cursorCDTTextOnlyUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// AgentRun 请求体是长连接（客户端写完后等响应），不能 ReadAll 阻塞；
		// 用独立读循环按 envelope 切帧，只取已完整到达的帧。
		go func() {
			tmp := make([]byte, 32*1024)
			for {
				n, err := r.Body.Read(tmp)
				if n > 0 {
					u.mu.Lock()
					u.upBuf = append(u.upBuf, tmp[:n]...)
					for u.readOff < len(u.upBuf) {
						frame, next, ferr := cursorReadEnvelopeFrame(u.upBuf, u.readOff)
						if ferr != nil {
							break
						}
						u.requests = append(u.requests, append([]byte(nil), frame.Data...))
						u.readOff = next
					}
					u.mu.Unlock()
				}
				if err != nil {
					return
				}
			}
		}()
		u.mu.Lock()
		u.runs++
		u.mu.Unlock()
		w.Header().Set("Content-Type", cursorConnectContentType)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		frames := append(joinFrames(
			fcTextDelta("RESULT-TEXT"),
			fcTurnEnded(100, 5, 0, 0),
		), fcEndStream()...)
		_, _ = w.Write(frames)
		if flusher != nil {
			flusher.Flush()
		}
	})
}

// 8. 真实 tool_use + tool_result 作为完整历史进入下一次 Cursor 执行。
func TestCursorInlineCallDynamicToolNextRoundFullHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	callID := "call-cdt-history-uuid"
	up := &cursorCDTTextOnlyUpstream{}
	ts := httptest.NewUnstartedServer(up.handler())
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)

	nodes := NewCursorUpstreamNodesResolver(WithCursorNodesCacheTTL(time.Hour))
	nodes.cached = CursorUpstreamNodes{HTTP1Base: ts.URL, HTTP2Base: ts.URL}
	nodes.cachedAt = time.Now()
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{7: {ID: 7, CodeEncrypted: "ACTIVATION-CODE-7"}}}
	svc := newCursorTestExecutionService(t, repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo),
		NewCursorUpstreamForwarder(WithCursorUpstreamNodes(nodes), WithCursorUpstreamHTTPClient(ts.Client())))

	var req apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(cursorCDTMessagesHistoryBody(callID)), &req))
	plan, err := cursorPlanFromMessages(&req, req.Model)
	require.NoError(t, err)
	// 工具结果位于历史前缀（不是本批待写回结果），本请求按普通请求发全新 AgentRun；
	// 完整历史由 root_prompt_messages_json + pre_fetched_blobs 携带，不重新委派旧工具。
	require.False(t, plan.claimsContinuation())
	require.True(t, plan.historySupportsRebuild(), "请求携带完整历史，可独立重建")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	result, err := cursorExecuteAnthropic(ctx, c, svc, cursorFullChainAccount(), &req)
	require.NoError(t, err)
	require.NotNil(t, result)
	_ = result // ForwardResult 只用于确认执行成功；正文按响应体断言

	out := rec.Body.String()
	require.Contains(t, out, `"type":"text"`)
	require.Contains(t, out, "RESULT-TEXT")
	require.Contains(t, out, `"stop_reason":"end_turn"`)
	require.Contains(t, out, "event: message_stop")
	require.NotContains(t, out, `"type":"tool_use"`, "已完成的旧工具不得重新委派")

	up.mu.Lock()
	require.Equal(t, 1, up.runs, "完整历史必须走一次全新 AgentRun")
	up.mu.Unlock()
	require.Eventually(t, func() bool {
		up.mu.Lock()
		defer up.mu.Unlock()
		return len(up.requests) >= 1
	}, 2*time.Second, 10*time.Millisecond)
	up.mu.Lock()
	request := string(up.requests[0])
	up.mu.Unlock()
	// 真实 tool_use / tool_result 都作为历史 blob 编码进 AgentRun，旧工具不被重新委派。
	require.Contains(t, request, `"tool-call"`, "历史必须包含 assistant tool-call")
	require.Contains(t, request, `"tool-result"`, "历史必须包含真实 tool-result")
	require.Contains(t, request, callID, "历史关联必须使用客户端回传的真实 call id")
	require.NotContains(t, request, cursorCDTIncidentText, "私有标记不得回灌上游")
}

// 9. 原生 mcp_args 交接 / DSML 禁用 / Cursor 普通文本不回归。
func TestCursorInlineCallDynamicToolNoRegression(t *testing.T) {
	// 原生 f11 委托仍按既有路径交付；DSML 正文仍不还原；writer 仍禁用共享 DSML。
	run := cursorTextOnlyDrive(t, cursorProtocolAnthropic, true, true)
	require.NotEmpty(t, run.calls, "原生 mcp_args 委托必须仍然被捕获")
	require.True(t, run.writer.anthState.ConversionOptions.DisableDSMLToolCalls)
	require.NotContains(t, run.body, "call_dsml_")

	// 普通 Cursor 文本请求：不产生任何工具调用，以 end_turn 收尾。
	body := append(cursorTextDeltaFrame(t, "plain text only"),
		cursorTurnEndedFrame(t, [2]uint64{cursorFldTurnEndedInputTokens, 1}, [2]uint64{cursorFldTurnEndedOutputTokens, 1})...)
	p := cursorNewStreamParser("m")
	p.attachInlineDeclaredTools(cursorCDTDeclaredTools())
	s := &cursorSSEStream{source: io.NopCloser(bytes.NewReader(body)), parser: p}
	sse, err := io.ReadAll(s)
	require.NoError(t, err)
	require.NotContains(t, string(sse), "tool_calls")
	require.Contains(t, string(sse), `"finish_reason":"stop"`)
	require.Empty(t, p.inlineCalls)
}

// cursorCDTBlockFor 按真实分隔符构造最小私有块，供非 Bash schema 的参数用例。
func cursorCDTBlockFor(toolName, argumentField string) string {
	return cursorCDTBlockOpen +
		cursorCDTFieldSep + "<arguments>" +
		cursorCDTFieldSep + argumentField +
		cursorCDTFieldSep + "</arguments>" +
		cursorCDTFieldSep + "<toolName>" + toolName +
		cursorCDTFieldSep + "</toolName>" +
		cursorCDTBlockClose
}

// 10. 原生委托与内联调用同轮共存：index 必须连续，不得并成一次。
func TestCursorInlineCallDynamicToolMixedNativeAndInlineIndexing(t *testing.T) {
	wantID := cursorCDTStubUUID(t)
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []apicompat.ChatTool{
			{Type: "function", Function: &apicompat.ChatFunction{
				Name:       "Bash",
				Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
			}},
			{Type: "function", Function: &apicompat.ChatFunction{
				Name:       "get_weather",
				Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			}},
		},
	}
	nativeArgs := cursorToolTestBuildMcpArgs("call_native_1", "get_weather",
		[]cursorToolTestMcpArgEntry{{key: "city", kind: "string", val: "Rome"}})
	var body []byte
	body = append(body, cursorTextDeltaFrame(t, "前言"+cursorCDTIncidentText)...)
	body = append(body, cursorEncodeEnvelope(cursorToolTestExecFrame(true, nativeArgs))...)
	body = append(body, cursorTurnEndedFrame(t,
		[2]uint64{cursorFldTurnEndedInputTokens, 10},
		[2]uint64{cursorFldTurnEndedOutputTokens, 5})...)

	p := cursorNewStreamParser("m")
	p.attachToolSchemas(cursorTestDeclaredToolDefs(t, req))
	p.attachInlineDeclaredTools(req.Tools)
	lines, err := p.cursorParseStream(body)
	require.NoError(t, err)
	sse := strings.Join(lines, "")
	require.Contains(t, sse, `"index":0`)
	require.Contains(t, sse, `"id":"call_native_1"`)
	require.Contains(t, sse, `"index":1`)
	require.Contains(t, sse, `"id":"`+wantID+`"`)

	completion, err := cursorAggregateSSEToCompletion(context.Background(), []byte(sse))
	require.NoError(t, err)
	var resp apicompat.ChatCompletionsResponse
	require.NoError(t, json.Unmarshal(completion, &resp))
	require.Len(t, resp.Choices, 1)
	calls := resp.Choices[0].Message.ToolCalls
	require.Len(t, calls, 2, "两类调用必须各占一个 index，不得合并")
	require.Equal(t, "call_native_1", calls[0].ID)
	require.Equal(t, "get_weather", calls[0].Function.Name)
	require.Equal(t, wantID, calls[1].ID)
	require.Equal(t, "Bash", calls[1].Function.Name)
	var inlineArgs map[string]string
	require.NoError(t, json.Unmarshal([]byte(calls[1].Function.Arguments), &inlineArgs))
	require.Equal(t, cursorCDTTestCommand, inlineArgs["command"])
}

// 11. 内联调用必须受本 HTTP 的 tool_choice 输出闸门约束。
func TestCursorInlineCallDynamicToolChoiceEnforcement(t *testing.T) {
	cases := []struct {
		name, mode, named string
		wantErr           bool
	}{
		{name: "none", mode: "none", wantErr: true},
		{name: "named_other", mode: "named", named: "Read", wantErr: true},
		{name: "named_match", mode: "named", named: "Bash"},
		{name: "required_satisfied", mode: "required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cursorCDTStubUUID(t)
			body := append(cursorTextDeltaFrame(t, cursorCDTIncidentText),
				cursorTurnEndedFrame(t,
					[2]uint64{cursorFldTurnEndedInputTokens, 10},
					[2]uint64{cursorFldTurnEndedOutputTokens, 5})...)
			p := cursorNewStreamParser("m")
			p.attachInlineDeclaredTools(cursorCDTDeclaredTools())
			p.attachInlineToolChoice(tc.mode, tc.named)
			s := &cursorSSEStream{source: io.NopCloser(bytes.NewReader(body)), parser: p}
			sse, err := io.ReadAll(s)
			if tc.wantErr {
				require.Error(t, err, "tool_choice 不满足必须失败关闭")
				require.NotContains(t, string(sse), `"tool_calls"`)
				return
			}
			require.NoError(t, err)
			require.Contains(t, string(sse), `"finish_reason":"tool_calls"`)
			require.Len(t, p.inlineCalls, 1)
		})
	}
}

// 12. 嵌套对象参数中的大整数必须原值保真（不得经 float64 丢精度）。
func TestCursorInlineCallDynamicToolBigIntegerPrecision(t *testing.T) {
	cursorCDTStubUUID(t)
	const bigInt = "9007199254740993"
	tools := []apicompat.ChatTool{{
		Type: "function",
		Function: &apicompat.ChatFunction{
			Name:       "ConfigTool",
			Parameters: json.RawMessage(`{"type":"object","properties":{"config":{"type":"object"}}}`),
		},
	}}
	block := cursorCDTBlockFor("ConfigTool", `<config>{"n":`+bigInt+`,"s":"x"}</config>`)
	n := newCursorCDTNormalizer(tools)
	_, calls, err := n.consume(block)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, "ConfigTool", calls[0].Name)
	require.JSONEq(t, `{"config":{"n":`+bigInt+`,"s":"x"}}`, calls[0].ArgsJSON)
	require.Contains(t, calls[0].ArgsJSON, bigInt)
}

// 13. 空参数段不得绕过 required；无必填项时可正常得到空对象。
func TestCursorInlineCallDynamicToolEmptyArgumentsRequired(t *testing.T) {
	cursorCDTStubUUID(t)
	block := cursorCDTBlockFor("Bash", "")
	n := newCursorCDTNormalizer(cursorCDTDeclaredTools())
	_, calls, err := n.consume(block)
	require.Error(t, err, "空参数段但工具声明了 required command，必须失败关闭")
	require.Empty(t, calls)

	noRequired := []apicompat.ChatTool{{
		Type: "function",
		Function: &apicompat.ChatFunction{
			Name:       "NoArgs",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}}
	n2 := newCursorCDTNormalizer(noRequired)
	safe, calls2, err := n2.consume(cursorCDTBlockFor("NoArgs", ""))
	require.NoError(t, err)
	require.Empty(t, safe)
	require.Len(t, calls2, 1)
	require.JSONEq(t, `{}`, calls2[0].ArgsJSON)
}

// 14. 缓冲解析器的归一尾段只在 parser.content 里落账一次。
func TestCursorInlineCallDynamicToolBufferedTailNotDuplicated(t *testing.T) {
	body := append(cursorTextDeltaFrame(t, "abc]"), cursorTestEndStream(t, `{}`)...)
	p := cursorNewStreamParser("m")
	p.attachInlineDeclaredTools(cursorCDTDeclaredTools())
	lines, err := p.cursorParseStream(body)
	require.NoError(t, err)
	require.NotEmpty(t, lines)
	require.Equal(t, "abc]", p.cursorContent(), "尾段不得重复累加")
}
