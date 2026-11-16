package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestOpenAIEmptyContentGatePayloadMatchesChunkJudge 钉死「两条判据同一结论」。
//
// raw 直转链路刻意不反序列化每一帧（gjson 判据），两条回退链路本来就要解析
// （chatChunkCarriesPayloadContent）。同一份载荷在两个实现下必须给出相同答案，
// 否则同一个故障会因走哪条出站链路而得到不同处置——这正是本次收口要消灭的
// 认知分裂。
func TestOpenAIEmptyContentGatePayloadMatchesChunkJudge(t *testing.T) {
	payloads := []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":""}}]}`,
		`{"choices":[{"delta":{"content":"你好"}}]}`,
		`{"choices":[{"delta":{"content":"0"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"想了很久"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"refusal":"我不能"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":0}}`,
		`{"choices":[{"delta":{"content":null},"finish_reason":"length"}]}`,
		// 非 delta 形态（上游直接回 message 体）不该被当成内容：流式契约只认 delta。
		`{"choices":[{"message":{"content":"整段正文"}}]}`,
	}

	for _, payload := range payloads {
		var gate openAIEmptyContentGate
		gate.ObserveChunkPayload([]byte(payload))

		var chunk apicompat.ChatCompletionsChunk
		require.NoError(t, json.Unmarshal([]byte(payload), &chunk), "payload=%s", payload)
		want := chatChunkCarriesPayloadContent(chunk)

		require.Equal(t, want, gate.sawContent, "payload=%s", payload)
	}
}

// TestOpenAIEmptyContentGateSignals 覆盖「空回答」的判定边界：思考不算内容、
// 空串不算内容、上游明确报错时不再记成空回答。
func TestOpenAIEmptyContentGateSignals(t *testing.T) {
	t.Run("只有思考算空回答", func(t *testing.T) {
		var gate openAIEmptyContentGate
		gate.ObserveChunkPayload([]byte(`{"choices":[{"delta":{"reasoning_content":"思考"}}]}`))
		gate.ObserveChunkPayload([]byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
		require.True(t, gate.Empty())
		require.Equal(t, "stop", gate.FinishReason())
	})

	t.Run("正文到了就不算空", func(t *testing.T) {
		var gate openAIEmptyContentGate
		gate.ObserveChunkPayload([]byte(`{"choices":[{"delta":{"content":"答"}}]}`))
		require.False(t, gate.Empty())
	})

	t.Run("上游报过错就不再判空回答", func(t *testing.T) {
		var gate openAIEmptyContentGate
		gate.ObserveChunkPayload([]byte(`{"error":{"message":"overloaded"}}`))
		require.False(t, gate.Empty())
	})

	t.Run("DONE 与空载荷不改判", func(t *testing.T) {
		var gate openAIEmptyContentGate
		gate.ObserveChunkPayload([]byte(`[DONE]`))
		gate.ObserveChunkPayload(nil)
		require.True(t, gate.Empty())
		require.Equal(t, "", gate.FinishReason())
	})

	t.Run("nil gate 不 panic", func(t *testing.T) {
		var gate *openAIEmptyContentGate
		gate.ObserveChunkPayload([]byte(`{"choices":[]}`))
		require.False(t, gate.Empty())
		require.Equal(t, "", gate.FinishReason())
	})
}

// TestEmptyContentClientBudgetFinishReason 钉死「客户预算给小」的豁免口径，
// 防止把客户端 max_tokens 不够误判成账号故障（会白摘一个健康账号）。
func TestEmptyContentClientBudgetFinishReason(t *testing.T) {
	for _, reason := range []string{"length", "max_tokens", "max_output_tokens", " LENGTH "} {
		require.True(t, emptyContentClientBudgetFinishReason(reason), "reason=%q", reason)
	}
	for _, reason := range []string{"", "stop", "tool_calls", "content_filter", "lengthy", "max_tokens_exceeded"} {
		require.False(t, emptyContentClientBudgetFinishReason(reason), "reason=%q", reason)
	}
}

// TestUpstreamEmptyContentMessage 钉死错误原文的稳定前缀：列表搜索按它命中，
// 括号里的现场只给人看，不参与任何分类。
func TestUpstreamEmptyContentMessage(t *testing.T) {
	require.Equal(t,
		"upstream returned 200 without any renderable content (stage=complete, finish_reason=stop)",
		upstreamEmptyContentMessage("complete", "stop"))
	require.Equal(t,
		"upstream returned 200 without any renderable content (stage=eof, finish_reason=-)",
		upstreamEmptyContentMessage("eof", ""))
	require.Equal(t,
		"upstream returned 200 without any renderable content (stage=unknown, finish_reason=-)",
		upstreamEmptyContentMessage("", ""))
	require.True(t, strings.HasPrefix(upstreamEmptyContentMessage("x", "y"), upstreamEmptyContentMessagePrefix))
}

func newEmptyContentTestService() (*OpenAIGatewayService, *gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return &OpenAIGatewayService{}, c, recorder
}

func opsUpstreamEventsOf(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	raw, ok := c.Get(OpsUpstreamErrorsKey)
	if !ok {
		return nil
	}
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	return events
}

// TestReportEmptyContent 覆盖执法点的三条出口：记账并换号 / 只记账（换号窗口
// 已关）/ 客户预算豁免（不记账不冷却）。
func TestReportEmptyContent(t *testing.T) {
	t.Run("能换号时记账并返回换号错误", func(t *testing.T) {
		svc, c, _ := newEmptyContentTestService()
		_, err := svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
			Account:           &Account{ID: 7, Platform: PlatformOpenAI, Name: "a7"},
			Resp:              &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": []string{"up-1"}}},
			UpstreamModel:     "m",
			UpstreamRequestID: "up-1",
			LogPrefix:         "test",
			Stage:             "complete",
			FinishReason:      "stop",
			CanFailover:       true,
		})
		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover)
		require.Equal(t, http.StatusBadGateway, failover.StatusCode)
		require.True(t, failover.SafeToFailoverAfterWrite)
		require.Equal(t, upstreamEmptyContentReason, failover.Reason)

		events := opsUpstreamEventsOf(t, c)
		require.Len(t, events, 1)
		require.Equal(t, upstreamEmptyContentErrorCode, events[0].Kind)
		require.Contains(t, events[0].Message, "finish_reason=stop")
		require.Equal(t, int64(7), events[0].AccountID)
		require.Equal(t, http.StatusOK, events[0].UpstreamStatusCode)
	})

	t.Run("换号窗口已关时只记账", func(t *testing.T) {
		svc, c, _ := newEmptyContentTestService()
		_, err := svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
			Account:      &Account{ID: 8, Platform: PlatformOpenAI},
			Resp:         &http.Response{StatusCode: http.StatusOK, Header: http.Header{}},
			LogPrefix:    "test",
			Stage:        "complete",
			FinishReason: "stop",
			CanFailover:  false,
		})
		require.NoError(t, err)
		require.Len(t, opsUpstreamEventsOf(t, c), 1)
	})

	t.Run("客户预算不足不记账", func(t *testing.T) {
		svc, c, _ := newEmptyContentTestService()
		_, err := svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
			Account:      &Account{ID: 9, Platform: PlatformOpenAI},
			LogPrefix:    "test",
			Stage:        "complete",
			FinishReason: "length",
			CanFailover:  true,
		})
		require.NoError(t, err)
		require.Empty(t, opsUpstreamEventsOf(t, c))
	})

	t.Run("有内容时什么都不做", func(t *testing.T) {
		svc, c, _ := newEmptyContentTestService()
		_, err := svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
			Account:     &Account{ID: 10, Platform: PlatformOpenAI},
			SawContent:  true,
			CanFailover: true,
		})
		require.NoError(t, err)
		require.Empty(t, opsUpstreamEventsOf(t, c))
	})
}

func rawChatStreamResponse(sse string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Request-Id": []string{"up-raw"}},
		Body:       ioNopCloser{strings.NewReader(sse)},
	}
}

type ioNopCloser struct{ *strings.Reader }

func (ioNopCloser) Close() error { return nil }

const rawChatEmptySSE = "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"index\":0}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"index\":0,\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":0,\"total_tokens\":100}}\n\n" +
	"data: [DONE]\n\n"

const rawChatContentSSE = "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"index\":0}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"答案\"},\"index\":0}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"index\":0,\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

const rawChatBudgetSSE = "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"index\":0}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"index\":0,\"finish_reason\":\"length\"}]}\n\n" +
	"data: [DONE]\n\n"

// TestStreamRawChatCompletionsEmptyContentFailsOver 是 raw 直转链路（聊天补全
// 客户直连 CC 上游）的端到端闸门验证：大请求会扣留客户端输出，因此零正文时
// 响应头未提交，可以透明换号。
func TestStreamRawChatCompletionsEmptyContentFailsOver(t *testing.T) {
	_, c, recorder := newEmptyContentTestService()
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 42, Platform: PlatformOpenAI, Name: "raw-empty"}

	result, err := svc.streamRawChatCompletions(c, rawChatStreamResponse(rawChatEmptySSE), account,
		"m", "m", "m", nil, nil, time.Now(), 100_000)

	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.True(t, failover.SafeToFailoverAfterWrite)
	require.Empty(t, recorder.Body.String(), "换号窗口内不应向客户端写出任何字节")

	events := opsUpstreamEventsOf(t, c)
	require.Len(t, events, 1)
	require.Equal(t, upstreamEmptyContentErrorCode, events[0].Kind)
	require.Contains(t, events[0].Message, "stage=complete")
	require.Contains(t, events[0].Message, "finish_reason=stop")
}

// TestStreamRawChatCompletionsEmptyContentAfterOutputStarted 验证 P0 的核心场景：
// 小请求不扣留输出（网关已写出 role 帧），换号窗口关闭，此时**只记账不动行为**
// ——客户拿到的还是原来的空流，但后台终于有一条记录。
func TestStreamRawChatCompletionsEmptyContentAfterOutputStarted(t *testing.T) {
	_, c, _ := newEmptyContentTestService()
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 43, Platform: PlatformOpenAI, Name: "raw-empty-small"}

	result, err := svc.streamRawChatCompletions(c, rawChatStreamResponse(rawChatEmptySSE), account,
		"m", "m", "m", nil, nil, time.Now(), 1024)

	require.NoError(t, err)
	require.NotNil(t, result)
	events := opsUpstreamEventsOf(t, c)
	require.Len(t, events, 1)
	require.Equal(t, upstreamEmptyContentErrorCode, events[0].Kind)
}

// TestStreamRawChatCompletionsBudgetTruncationPassesThrough：客户端 max_tokens
// 给小导致的空正文是预期截断 —— 不报错、不记账、不冷却，但必须给客户一句
// 「为什么空、怎么改」的中文说明，且响应仍是完整的 200 正常流。
func TestStreamRawChatCompletionsBudgetTruncationPassesThrough(t *testing.T) {
	_, c, recorder := newEmptyContentTestService()
	c.Request.URL.Path = "/v1/chat/completions"
	SetOpsRequestShape(c, map[string]any{"max_tokens": int64(1)})
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 44, Platform: PlatformOpenAI, Name: "raw-budget"}

	result, err := svc.streamRawChatCompletions(c, rawChatStreamResponse(rawChatBudgetSSE), account,
		"m", "m", "m", nil, nil, time.Now(), 100_000)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, opsUpstreamEventsOf(t, c))
	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	require.Contains(t, body, "[站点提示]")
	require.Contains(t, body, "max_tokens=1")
	require.Contains(t, body, "data: [DONE]")
	// 上游那条空流（role / finish_reason=length）不能重复放行，否则客户会看到两段。
	require.Equal(t, 1, strings.Count(body, "data: [DONE]"))
}

// TestStreamRawChatCompletionsContentNotFlagged：正常回答不能被闸门误伤。
func TestStreamRawChatCompletionsContentNotFlagged(t *testing.T) {
	_, c, _ := newEmptyContentTestService()
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 45, Platform: PlatformOpenAI, Name: "raw-ok"}

	result, err := svc.streamRawChatCompletions(c, rawChatStreamResponse(rawChatContentSSE), account,
		"m", "m", "m", nil, nil, time.Now(), 100_000)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, opsUpstreamEventsOf(t, c))
}

// TestEmptyContentBudgetNoticeText 钉死给客户看的提示：必须带上客户自己设的字段名
// 和数值（否则客户不知道说的是哪个参数），且不能出现 max_tokens=0 这种占位符。
func TestEmptyContentBudgetNoticeText(t *testing.T) {
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		text := emptyContentBudgetNoticeText(map[string]any{key: int64(1)})
		require.Contains(t, text, key+"=1", "key=%s", key)
		require.Contains(t, text, "1024")
		require.Contains(t, text, "思考过程")
		require.Contains(t, text, "[站点提示]")
	}
	fallback := emptyContentBudgetNoticeText(nil)
	require.Contains(t, fallback, "[站点提示]")
	require.NotContains(t, fallback, "=0")
	require.NotContains(t, fallback, "%!")
}

func TestEmptyContentOutputBudget(t *testing.T) {
	key, value, ok := emptyContentOutputBudget(map[string]any{"max_tokens": int64(16)})
	require.True(t, ok)
	require.Equal(t, "max_tokens", key)
	require.Equal(t, int64(16), value)
	// 0 / 负数 / 类型不对都不回显具体数值。
	for _, shape := range []map[string]any{
		{"max_tokens": int64(0)}, {"max_tokens": int64(-1)}, {"max_tokens": "abc"}, nil,
	} {
		_, _, ok := emptyContentOutputBudget(shape)
		require.False(t, ok, "shape=%v", shape)
	}
}

// TestEmptyContentNoticeFramesByProtocol 验证三种入站协议都能生成完整合法的
// SSE 帧序列（不是只发一个孤零零的 delta 让客户端解析失败）。
func TestEmptyContentNoticeFramesByProtocol(t *testing.T) {
	cases := []struct {
		path    string
		want    []string
		exclude string
	}{
		{"/v1/chat/completions", []string{`"content":"[站点提示]"`, `"finish_reason":"stop"`, "data: [DONE]"}, ""},
		{"/v1/messages", []string{"event: message_start", "event: content_block_start", "event: content_block_delta", `"type":"text_delta"`, "event: message_stop"}, ""},
		{"/v1/responses", []string{"event: response.created", "event: response.output_text.delta", "event: response.completed", "data: [DONE]"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			_, c, _ := newEmptyContentTestService()
			c.Request.URL.Path = tc.path
			frames := emptyContentNoticeFrames(c, "m", "[站点提示]")
			joined := strings.Join(frames, "")
			for _, want := range tc.want {
				require.Contains(t, joined, want, "path=%s", tc.path)
			}
			require.NotEmpty(t, frames)
		})
	}
}

// TestReportEmptyContentWritesBudgetNotice 是这次改动的端到端契约：预算不足导致
// 空回答时，给客户一段说明文字、HTTP 200、原协议帧完整，并且不记账、不报错。
func TestReportEmptyContentWritesBudgetNotice(t *testing.T) {
	svc, c, recorder := newEmptyContentTestService()
	c.Request.URL.Path = "/v1/chat/completions"
	SetOpsRequestShape(c, map[string]any{"max_tokens": int64(1)})
	headersWritten := 0

	noticeWritten, err := svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
		Account:           &Account{ID: 984, Platform: PlatformOpenAI},
		Resp:              &http.Response{StatusCode: http.StatusOK, Header: http.Header{}},
		UpstreamModel:     "spark-x2.6",
		UpstreamRequestID: "up-1",
		LogPrefix:         "test",
		Stage:             "eof",
		FinishReason:      "max_tokens",
		CanFailover:       true,
		WriteHeaders:      func() { headersWritten++ },
	})

	require.NoError(t, err, "预算不足不是故障，不能返回错误给上层")
	require.True(t, noticeWritten)
	require.Equal(t, 1, headersWritten)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "max_tokens=1")
	require.Contains(t, recorder.Body.String(), "思考过程")
	require.Contains(t, recorder.Body.String(), "data: [DONE]")
	require.Empty(t, opsUpstreamEventsOf(t, c), "客户参数问题不该记成上游故障")
}

// TestReportEmptyContentWritesBufferedNotice：非流式链路走 buffered 通道，把提示
// 交给调用方写进 JSON 响应体。即使此时还能换号、也给了 WriteHeaders，也不能误走
// SSE 通道——否则客户会收到一段夹在 JSON 里的 event: 帧。
func TestReportEmptyContentWritesBufferedNotice(t *testing.T) {
	svc, c, recorder := newEmptyContentTestService()
	c.Request.URL.Path = "/v1/messages"
	SetOpsRequestShape(c, map[string]any{"max_tokens": int64(8)})

	var gotText string
	noticeWritten, err := svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
		Account:      &Account{ID: 984, Platform: PlatformOpenAI},
		LogPrefix:    "test",
		Stage:        "buffered",
		FinishReason: "max_tokens",
		CanFailover:  true,
		WriteHeaders: func() { t.Fatal("非流式不该走 SSE 通道") },
		WriteBufferedNotice: func(text string) bool {
			gotText = text
			return true
		},
	})
	require.NoError(t, err)
	require.True(t, noticeWritten)
	require.Contains(t, gotText, "[站点提示]")
	require.Contains(t, gotText, "max_tokens=8")
	require.Empty(t, recorder.Body.String(), "非流式不该写出 SSE 帧")
	require.Empty(t, opsUpstreamEventsOf(t, c), "客户参数问题不该记成上游故障")
}

// TestReportEmptyContentSkipsNoticeAfterOutputStarted：响应头已提交时不能再插
// message_start 之类的前置帧，保持原有放行语义（只是日志里留一条 can_write_notice=false）。
func TestReportEmptyContentSkipsNoticeAfterOutputStarted(t *testing.T) {
	svc, c, recorder := newEmptyContentTestService()
	c.Request.URL.Path = "/v1/messages"

	noticeWritten, err := svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
		Account:      &Account{ID: 984, Platform: PlatformOpenAI},
		LogPrefix:    "test",
		Stage:        "eof",
		FinishReason: "max_tokens",
		CanFailover:  false,
		WriteHeaders: func() { t.Fatal("响应头已提交时不应再调用 WriteHeaders") },
	})
	require.NoError(t, err)
	require.False(t, noticeWritten)
	require.Empty(t, recorder.Body.String())
	require.Empty(t, opsUpstreamEventsOf(t, c))
}

// TestBufferedAnthropicBudgetNoticeIsRenderable 走真正的非流式处理函数（不是只测
// 闸门返回值）：上游回了 200 + 空 content + stop_reason=max_tokens，客户最终拿到
// 的 JSON 里必须是一个可渲染的 text 块，且 HTTP 仍是 200、后台不记账。
func TestBufferedAnthropicBudgetNoticeIsRenderable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	SetOpsRequestShape(c, map[string]any{"max_tokens": int64(1)})

	// 现场照抄 00:03:39 那单：output_tokens=64 但正文 0 字（额度被思考吃光）。
	upstreamBody := `{"id":"msg_1","type":"message","role":"assistant","model":"spark-x2.6","content":[],"stop_reason":"max_tokens","stop_sequence":null,"usage":{"input_tokens":60,"output_tokens":64}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}
	svc := &OpenAIGatewayService{}

	_, err := svc.handleNativeAnthropicBufferedResponse(
		context.Background(), resp, c, &Account{ID: 984}, "spark-x2.6", "spark-x2.6", "spark-x2.6", nil, time.Now(),
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, recorder.Code)

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &out))
	require.Len(t, out.Content, 1, "空白 content 必须被换成一段可渲染正文")
	require.Equal(t, "text", out.Content[0].Type)
	require.Contains(t, out.Content[0].Text, "[站点提示]")
	require.Contains(t, out.Content[0].Text, "max_tokens=1")
	require.Empty(t, opsUpstreamEventsOf(t, c), "客户参数问题不该记成上游故障")
}
