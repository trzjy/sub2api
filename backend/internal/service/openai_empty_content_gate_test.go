package service

import (
	"encoding/json"
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
		err := svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
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
		err := svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
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
		err := svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
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
		require.NoError(t, svc.reportEmptyContent(emptyContentRequestContext(c), c, emptyContentReport{
			Account:     &Account{ID: 10, Platform: PlatformOpenAI},
			SawContent:  true,
			CanFailover: true,
		}))
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
// 给小导致的空正文是预期截断，不能记成上游故障，更不能冷却账号。
func TestStreamRawChatCompletionsBudgetTruncationPassesThrough(t *testing.T) {
	_, c, _ := newEmptyContentTestService()
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 44, Platform: PlatformOpenAI, Name: "raw-budget"}

	result, err := svc.streamRawChatCompletions(c, rawChatStreamResponse(rawChatBudgetSSE), account,
		"m", "m", "m", nil, nil, time.Now(), 100_000)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, opsUpstreamEventsOf(t, c))
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
