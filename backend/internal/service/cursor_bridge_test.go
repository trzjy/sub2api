//go:build unit

package service

// P1-C1 配对测试：Claude 组 Messages / Chat Completions / Responses 三出口对
// cursor 账号的早分流。
//
// 钉死五条硬性质：
//  1. cursor 账号命中前置分支，入站先由既有 apicompat 转换器归一为 Chat
//     Completions 再进入契约 0.1 文本桥接（出站 body 带映射后的模型与用户文本）；
//  2. 客户端可见响应回到各自入站协议（Messages=Anthropic SSE/JSON，
//     Responses=Responses 事件/JSON，Chat=chat.completion）；
//  3. 流式以该协议的正规终态结束（message_stop / response.completed /
//     [DONE]），不把 OpenAI 形状泄漏给 Anthropic/Responses 客户端；
//  4. usage 只出现一次且等于桥接返回的真实用量（上游只调用一次）；
//  5. 非 cursor 账号路径不变（桥接未安装也不影响既有 Anthropic 路径）。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// cursorBridgeInstallForTest 安装桥接实例并在用例结束后还原（包级单例）。
func cursorBridgeInstallForTest(t *testing.T, svc *OpenAIGatewayService) {
	t.Helper()
	prev := cursorBridgeInstalled()
	InstallCursorBridge(svc)
	t.Cleanup(func() { InstallCursorBridge(prev) })
}

// newCursorBridgeOutboundService 装配一个指向假中转的 cursor 出站服务，并安装为
// 契约 0.1 的桥接实例。
func newCursorBridgeOutboundService(t *testing.T, responseBody []byte) (*OpenAIGatewayService, *cursorOutboundUpstream) {
	t.Helper()
	// 先快照安装前的实例：NewOpenAIGatewayService 会在构造末尾自安装，若在
	// 构造之后才快照，cleanup 会把桥接还原成已随用例销毁的实例。
	prev := cursorBridgeInstalled()
	t.Cleanup(func() { InstallCursorBridge(prev) })

	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{
		11: {ID: 11, CodeEncrypted: "ACTIVATION-CODE-ELEVEN"},
	})
	up := &cursorOutboundUpstream{body: responseBody}
	svc, _ := newCursorOutboundTestService(t, up.handler(), repo, cursorTestDeviceIDs(repo))
	InstallCursorBridge(svc)
	return svc, up
}

// cursorBridgeGinContext 构造请求级 gin context；writer 使用独立 recorder 以便断言
// 客户端可见响应。
func cursorBridgeGinContext(t *testing.T, path string, body []byte) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	return rec, c
}

func cursorBridgeParsedAnthropic(t *testing.T, body []byte) *ParsedRequest {
	t.Helper()
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), domain.PlatformAnthropic)
	require.NoError(t, err)
	return parsed
}

// TestCursorBridgeForwardMessagesStreamKeepsAnthropicProtocol 覆盖 /v1/messages
// 流式：Anthropic → CC → cursor 桥接 → Anthropic SSE。
func TestCursorBridgeForwardMessagesStreamKeepsAnthropicProtocol(t *testing.T) {
	_, up := newCursorBridgeOutboundService(t, cursorE2ESuccessStream(t, "Hello", " world"))
	svc := &GatewayService{}

	body := []byte(`{"model":"cursor-sonnet","max_tokens":64,"stream":true,"system":"be brief","messages":[{"role":"user","content":"hi from messages"}]}`)
	parsed := cursorBridgeParsedAnthropic(t, body)
	rec, c := cursorBridgeGinContext(t, "/v1/messages", body)

	result, err := svc.Forward(context.Background(), c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), parsed)
	require.NoError(t, err)
	require.NotNil(t, result)

	// cursor 前置分支命中：上游只调用一次，且入站 Anthropic 已转换为 CC 形状。
	require.Equal(t, 1, up.count())
	require.Equal(t, cursorPathAgentRun, up.at(0).path)
	wire := string(up.at(0).body)
	require.Contains(t, wire, "upstream-sonnet", "账号 model_mapping 必须作为唯一模型出口")
	require.Contains(t, wire, "hi from messages")
	require.Contains(t, wire, "be brief", "system prompt 必须随入站转换进入上游请求")

	// 客户端可见结果必须是 Anthropic SSE 且以 message_stop 终止。
	out := rec.Body.String()
	require.Contains(t, out, "event: message_start")
	require.Contains(t, out, "event: content_block_delta")
	require.Contains(t, out, "event: message_delta")
	require.Contains(t, out, "event: message_stop")
	require.Contains(t, out, "Hello")
	require.Contains(t, out, " world")
	require.NotContains(t, out, "data: [DONE]", "Anthropic 出口不得泄漏 OpenAI 终止哨兵")
	require.NotContains(t, out, "chat.completion", "Anthropic 出口不得泄漏 OpenAI 包络")
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))

	// usage 只出现一次，且等于桥接返回的真实用量。
	require.Equal(t, 1, strings.Count(out, `"input_tokens":12601`))
	require.Equal(t, 1, strings.Count(out, `"output_tokens":15`))
	require.Equal(t, 12601, result.Usage.InputTokens)
	require.Equal(t, 15, result.Usage.OutputTokens)
	require.Equal(t, 128, result.Usage.CacheReadInputTokens)
}

// TestCursorBridgeForwardMessagesNonStreamKeepsAnthropicProtocol 覆盖 /v1/messages
// 非流式：客户端可见 Anthropic message JSON。
func TestCursorBridgeForwardMessagesNonStreamKeepsAnthropicProtocol(t *testing.T) {
	_, up := newCursorBridgeOutboundService(t, cursorE2ESuccessStream(t, "Hello", " world"))
	svc := &GatewayService{}

	body := []byte(`{"model":"cursor-sonnet","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	parsed := cursorBridgeParsedAnthropic(t, body)
	rec, c := cursorBridgeGinContext(t, "/v1/messages", body)

	result, err := svc.Forward(context.Background(), c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), parsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, up.count())
	require.Contains(t, string(up.at(0).body), "upstream-sonnet")

	var out struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "message", out.Type)
	require.Equal(t, "assistant", out.Role)
	require.Equal(t, "cursor-sonnet", out.Model)
	require.Equal(t, "end_turn", out.StopReason)
	require.Len(t, out.Content, 1)
	require.Equal(t, "Hello world", out.Content[0].Text)
	require.Equal(t, 12601, out.Usage.InputTokens)
	require.Equal(t, 15, out.Usage.OutputTokens)
	require.Equal(t, 128, out.Usage.CacheReadInputTokens)
	require.Equal(t, 12601, result.Usage.InputTokens)
}

// TestCursorBridgeForwardAsChatCompletionsCursorAccount 覆盖 /v1/chat/completions：
// body 直转桥接，客户端可见 chat.completion(.chunk)。
func TestCursorBridgeForwardAsChatCompletionsCursorAccount(t *testing.T) {
	_, up := newCursorBridgeOutboundService(t, cursorE2ESuccessStream(t, "Hello", " world"))
	svc := &GatewayService{}

	body := []byte(`{"model":"cursor-sonnet","stream":true,"messages":[{"role":"user","content":"hi from chat"}]}`)
	rec, c := cursorBridgeGinContext(t, "/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, 1, up.count())
	wire := string(up.at(0).body)
	require.Contains(t, wire, "upstream-sonnet")
	require.Contains(t, wire, "hi from chat")

	out := rec.Body.String()
	require.Contains(t, out, `"object":"chat.completion.chunk"`)
	require.Contains(t, out, "Hello")
	require.Contains(t, out, cursorSSEDoneSentinel)
	require.Equal(t, 1, strings.Count(out, `"input_tokens":12601`))
	require.Equal(t, 12601, result.Usage.InputTokens)
	require.Equal(t, 15, result.Usage.OutputTokens)
}

// TestCursorBridgeForwardAsChatCompletionsNonStream 覆盖 /v1/chat/completions
// 非流式：客户端可见 chat.completion JSON。
func TestCursorBridgeForwardAsChatCompletionsNonStream(t *testing.T) {
	_, up := newCursorBridgeOutboundService(t, cursorE2ESuccessStream(t, "Hello", " world"))
	svc := &GatewayService{}

	body := []byte(`{"model":"cursor-sonnet","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	rec, c := cursorBridgeGinContext(t, "/v1/chat/completions", body)

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, up.count())

	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "chat.completion", out.Object)
	require.Len(t, out.Choices, 1)
	require.Equal(t, "Hello world", out.Choices[0].Message.Content)
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"input_tokens":12601`))
	require.Equal(t, 12601, result.Usage.InputTokens)
	require.Equal(t, 15, result.Usage.OutputTokens)
}

// TestCursorBridgeForwardAsResponsesNonStream 覆盖 /v1/responses 非流式：
// 客户端可见 Responses JSON 对象。
func TestCursorBridgeForwardAsResponsesNonStream(t *testing.T) {
	_, up := newCursorBridgeOutboundService(t, cursorE2ESuccessStream(t, "Hello", " world"))
	svc := &GatewayService{}

	body := []byte(`{"model":"cursor-sonnet","stream":false,"input":"hi"}`)
	rec, c := cursorBridgeGinContext(t, "/v1/responses", body)

	result, err := svc.ForwardAsResponses(context.Background(), c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, up.count())

	var out struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, "response", out.Object)
	require.Equal(t, "completed", out.Status)
	require.NotEmpty(t, out.Output)
	require.Equal(t, "Hello world", out.Output[0].Content[0].Text)
	require.Equal(t, 12729, out.Usage.InputTokens)
	require.Equal(t, 15, out.Usage.OutputTokens)
	require.Equal(t, 12601, result.Usage.InputTokens)
	require.Equal(t, 128, result.Usage.CacheReadInputTokens)
}

// TestCursorBridgeForwardAsResponsesCursorAccount 覆盖 /v1/responses：
// Responses → CC → 桥接 → Responses 事件。
func TestCursorBridgeForwardAsResponsesCursorAccount(t *testing.T) {
	_, up := newCursorBridgeOutboundService(t, cursorE2ESuccessStream(t, "Hello", " world"))
	svc := &GatewayService{}

	body := []byte(`{"model":"cursor-sonnet","stream":true,"input":"hi from responses"}`)
	rec, c := cursorBridgeGinContext(t, "/v1/responses", body)

	result, err := svc.ForwardAsResponses(context.Background(), c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), body, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, 1, up.count())
	wire := string(up.at(0).body)
	require.Contains(t, wire, "upstream-sonnet")
	require.Contains(t, wire, "hi from responses")

	out := rec.Body.String()
	require.Contains(t, out, "event: response.created")
	require.Contains(t, out, "event: response.output_text.delta")
	require.Contains(t, out, "event: response.completed")
	require.Contains(t, out, "Hello")
	require.NotContains(t, out, "data: [DONE]", "Responses 出口不得泄漏 OpenAI 终止哨兵")
	// Responses usage 的 input_tokens 是含缓存的总输入（12601 + 128），缓存单列在
	// input_tokens_details.cached_tokens；与桥接 ForwardResult 的非缓存口径等价。
	require.Equal(t, 1, strings.Count(out, `"input_tokens":12729`))
	require.Contains(t, out, `"cached_tokens":128`)
	require.Equal(t, 12601, result.Usage.InputTokens)
	require.Equal(t, 128, result.Usage.CacheReadInputTokens)
	require.Equal(t, 15, result.Usage.OutputTokens)
}

// TestCursorBridgeNonCursorPathUnchanged 钉死非 cursor 账号不进入 cursor 分支：
// 即使桥接未安装（调用即失败），既有 Anthropic 三出口仍正常完成。
func TestCursorBridgeNonCursorPathUnchanged(t *testing.T) {
	// 未安装桥接：任何 cursor 分支调用都会返回 ErrCursorBridgeNotInstalled。
	cursorBridgeInstallForTest(t, nil)

	t.Run("messages", func(t *testing.T) {
		upstream := &g2aMockUpstream{steps: []g2aCall{g2aSuccessFn(g2aAnthropicSSE)}}
		svc := g2aService(upstream)
		body := g2aStreamBody("claude-sonnet-5")
		parsed := g2aParsed(t, body)
		rec, c, _ := g2aGinContext(http.MethodPost, "/v1/messages", body)

		result, err := svc.Forward(context.Background(), c, g2aAnthropicOAuthAccount(), parsed)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, 1, upstream.callCount())
		require.Contains(t, rec.Body.String(), "event: message_stop")
	})

	t.Run("chat_completions", func(t *testing.T) {
		upstream := &g2aMockUpstream{steps: []g2aCall{g2aSuccessFn(g2aAnthropicSSE)}}
		svc := g2aService(upstream)
		body := []byte(`{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
		rec, c, _ := g2aGinContext(http.MethodPost, "/v1/chat/completions", body)

		result, err := svc.ForwardAsChatCompletions(context.Background(), c, g2aAnthropicAPIKeyAccount(1101), body, nil)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, 1, upstream.callCount())
		require.Contains(t, rec.Body.String(), cursorSSEDoneSentinel)
	})

	t.Run("responses", func(t *testing.T) {
		upstream := &g2aMockUpstream{steps: []g2aCall{g2aSuccessFn(g2aAnthropicSSE)}}
		svc := g2aService(upstream)
		body := []byte(`{"model":"claude-sonnet-5","stream":true,"input":"hello"}`)
		rec, c, _ := g2aGinContext(http.MethodPost, "/v1/responses", body)

		result, err := svc.ForwardAsResponses(context.Background(), c, g2aAnthropicAPIKeyAccount(1102), body, nil)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, 1, upstream.callCount())
		require.Contains(t, rec.Body.String(), "response.completed")
	})
}
