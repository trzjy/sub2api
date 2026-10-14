//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	rawCCPurifyToolSentinel       = "TOOL_MIDDLE_NOISE_MUST_DISAPPEAR"
	rawCCPurifyAttachmentSentinel = "ATTACHMENT_MIDDLE_NOISE_MUST_DISAPPEAR"
)

type rawCCPurifyDecodedRequest struct {
	Model    string                      `json:"model"`
	Messages []rawCCPurifyDecodedMessage `json:"messages"`
}

type rawCCPurifyDecodedMessage struct {
	Role       string `json:"role"`
	Content    any    `json:"content"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID string `json:"id"`
	} `json:"tool_calls"`
}

func TestPurifyRawCCMessages_BelowTriggerReturnsOriginalBody(t *testing.T) {
	body := []byte(`{"model":"kimi-k3","messages":[{"role":"system","content":"keep this"},{"role":"user","content":"hello"}],"stream":false}`)
	estimated, err := EstimateTierRoutingInputTokens("kimi-k3", TierRoutingProtocolChatCompletions, body)
	require.NoError(t, err)
	require.Less(t, estimated, defaultRawCCPurifyTriggerTokens)

	purified, rejected, err := purifyRawCCMessages(body, &Account{}, "kimi-k3")
	require.NoError(t, err)
	require.False(t, rejected)
	require.Equal(t, body, purified)
}

func TestRawCCPurifyThresholds_ReadsAccountExtra(t *testing.T) {
	trigger, redline := rawCCPurifyThresholds(&Account{Extra: map[string]any{
		"purify_trigger_tokens": "12000",
		"purify_redline_tokens": json.Number("48000"),
	}})
	require.Equal(t, 12000, trigger)
	require.Equal(t, 48000, redline)

	trigger, redline = rawCCPurifyThresholds(&Account{})
	require.Equal(t, defaultRawCCPurifyTriggerTokens, trigger)
	require.Equal(t, defaultRawCCPurifyRedlineTokens, redline)
}

func TestPurifyRawCCMessages_TruncatesToolAndAttachmentPreservingAnchors(t *testing.T) {
	body := rawCCPurifyLargeFixture(t)
	beforeTokens, err := EstimateTierRoutingInputTokens("kimi-k3", TierRoutingProtocolChatCompletions, body)
	require.NoError(t, err)
	require.GreaterOrEqual(t, beforeTokens, defaultRawCCPurifyTriggerTokens)

	purified, rejected, err := purifyRawCCMessages(body, &Account{}, "kimi-k3")
	require.NoError(t, err)
	require.False(t, rejected)
	require.True(t, json.Valid(purified))

	afterTokens, err := EstimateTierRoutingInputTokens("kimi-k3", TierRoutingProtocolChatCompletions, purified)
	require.NoError(t, err)
	require.Less(t, afterTokens, beforeTokens)
	require.NotContains(t, string(purified), rawCCPurifyToolSentinel)
	require.NotContains(t, string(purified), rawCCPurifyAttachmentSentinel)

	decoded := rawCCPurifyDecode(t, purified)
	require.Equal(t, "project identity and constraints must remain intact", rawCCPurifyStringContent(t, &decoded.Messages[0]))
	require.Equal(t, "latest user question must remain byte-for-byte", rawCCPurifyStringContent(t, &decoded.Messages[len(decoded.Messages)-1]))

	toolMessage := rawCCPurifyMessageByRole(t, decoded.Messages, "tool")
	toolContent := rawCCPurifyStringContent(t, toolMessage)
	require.Contains(t, toolContent, "工具结果已截断")
	require.Less(t, len(toolContent), rawCCPurifyToolTextMaxBytes)
	require.Equal(t, "call_context_1", toolMessage.ToolCallID)

	assistantCall := rawCCPurifyMessageWithToolCall(t, decoded.Messages, "call_context_1")
	require.NotNil(t, assistantCall)

	attachmentMessage := rawCCPurifyMessageContaining(t, decoded.Messages, "[附件提纯]")
	attachmentContent := rawCCPurifyStringContent(t, attachmentMessage)
	require.Contains(t, attachmentContent, "filename: historical-context.txt")
	require.Contains(t, attachmentContent, "MUST preserve this interface constraint")
}

func TestPurifyRawCCMessages_RejectsOverRedlineWithUnifiedMessage(t *testing.T) {
	body := rawCCPurifyUncompressibleBody(t)
	estimated, err := EstimateTierRoutingInputTokens("kimi-k3", TierRoutingProtocolChatCompletions, body)
	require.NoError(t, err)
	require.Greater(t, estimated, defaultRawCCPurifyRedlineTokens)

	purified, rejected, err := purifyRawCCMessages(body, rawChatCompletionsTestAccount(), "kimi-k3")
	require.NoError(t, err)
	require.True(t, rejected)
	require.Equal(t, body, purified, "system anchor must never be compressed")

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, forwardErr := svc.forwardAsRawChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body, "")
	require.Error(t, forwardErr)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, "invalid_request_error", gjson.Get(recorder.Body.String(), "error.type").String())
	require.Equal(t, messagesOversizeClientMessage, gjson.Get(recorder.Body.String(), "error.message").String())
	require.Empty(t, upstream.requests, "redline rejection must not spend an upstream request")
}

func TestPurifyRawCCMessages_CompletesWithinBudget(t *testing.T) {
	body := rawCCPurifyLargeFixture(t)
	account := &Account{}

	// 预热 tokenizer 初始化，不把进程级一次性初始化算进单请求提纯预算。
	_, _, err := purifyRawCCMessages(body, account, "kimi-k3")
	require.NoError(t, err)

	durations := make([]time.Duration, 9)
	for i := range durations {
		startedAt := time.Now()
		purified, rejected, purifyErr := purifyRawCCMessages(body, account, "kimi-k3")
		durations[i] = time.Since(startedAt)
		require.NoError(t, purifyErr)
		require.False(t, rejected)
		require.Less(t, len(purified), len(body), "timed run must execute purification instead of timing out to passthrough")
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	median := durations[len(durations)/2]
	t.Logf("raw CC context purification median: %s", median)
	require.Less(t, median, rawCCPurifyBudget)
}

func TestForwardAsRawChatCompletions_PurifiesOutboundBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := rawCCPurifyLargeFixture(t)
	beforeTokens, err := EstimateTierRoutingInputTokens("kimi-k3", TierRoutingProtocolChatCompletions, body)
	require.NoError(t, err)
	require.GreaterOrEqual(t, beforeTokens, defaultRawCCPurifyTriggerTokens)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-purified"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl-purified","object":"chat.completion","model":"kimi-k3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101}}`,
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          rawChatCompletionsTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.forwardAsRawChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.NotContains(t, string(upstream.lastBody), rawCCPurifyToolSentinel)
	require.NotContains(t, string(upstream.lastBody), rawCCPurifyAttachmentSentinel)
	require.Contains(t, string(upstream.lastBody), "latest user question must remain byte-for-byte")

	afterTokens, err := EstimateTierRoutingInputTokens("kimi-k3", TierRoutingProtocolChatCompletions, upstream.lastBody)
	require.NoError(t, err)
	require.Less(t, afterTokens, beforeTokens)
}

func rawCCPurifyLargeFixture(t *testing.T) []byte {
	t.Helper()
	toolText := "summary: dependency scan completed\n" +
		rawCCPurifyNoise(6_500, 1) +
		rawCCPurifyToolSentinel +
		rawCCPurifyNoise(6_500, 2) +
		"\nresult: no actionable failures"
	attachmentText := "filename: historical-context.txt\nMUST preserve this interface constraint: POST /v1/jobs accepts request_id\n" +
		rawCCPurifyNoise(6_500, 3) +
		rawCCPurifyAttachmentSentinel +
		rawCCPurifyNoise(6_500, 4) +
		"\nend of historical-context.txt"

	messages := []map[string]any{
		{"role": "system", "content": "project identity and constraints must remain intact"},
		{"role": "user", "content": "inspect the dependency scan"},
		{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"id": "call_context_1", "type": "function", "function": map[string]any{"name": "scan", "arguments": `{}`}}}},
		{"role": "tool", "tool_call_id": "call_context_1", "content": toolText},
		{"role": "user", "content": attachmentText},
		{"role": "assistant", "content": "The old attachment was reviewed."},
	}
	for i := 1; i <= rawCCPurifyRecentRounds; i++ {
		messages = append(messages,
			map[string]any{"role": "user", "content": "recent question " + strings.Repeat("x", i)},
			map[string]any{"role": "assistant", "content": "recent answer " + strings.Repeat("y", i)},
		)
	}
	messages = append(messages, map[string]any{"role": "user", "content": "latest user question must remain byte-for-byte"})

	body, err := json.Marshal(map[string]any{
		"model":    "kimi-k3",
		"messages": messages,
		"stream":   false,
	})
	require.NoError(t, err)
	return body
}

func rawCCPurifyUncompressibleBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "kimi-k3",
		"messages": []map[string]any{
			{"role": "system", "content": "policy rules MUST remain unchanged:\n" + rawCCPurifyNoise(100_000, 5)},
			{"role": "user", "content": "answer using every system rule"},
		},
		"stream": false,
	})
	require.NoError(t, err)
	return body
}

func rawCCPurifyNoise(size int, seed uint64) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	if seed == 0 {
		seed = 1
	}
	noise := make([]byte, size)
	state := seed
	for i := range noise {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		noise[i] = alphabet[state&63]
	}
	return string(noise)
}

func rawCCPurifyDecode(t *testing.T, body []byte) rawCCPurifyDecodedRequest {
	t.Helper()
	var decoded rawCCPurifyDecodedRequest
	require.NoError(t, json.Unmarshal(body, &decoded))
	return decoded
}

func rawCCPurifyStringContent(t *testing.T, message *rawCCPurifyDecodedMessage) string {
	t.Helper()
	require.NotNil(t, message)
	content, ok := message.Content.(string)
	require.True(t, ok, "expected string content, got %T", message.Content)
	return content
}

func rawCCPurifyMessageByRole(t *testing.T, messages []rawCCPurifyDecodedMessage, role string) *rawCCPurifyDecodedMessage {
	t.Helper()
	for i := range messages {
		if messages[i].Role == role {
			return &messages[i]
		}
	}
	t.Fatalf("message with role %q not found", role)
	return nil
}

func rawCCPurifyMessageContaining(t *testing.T, messages []rawCCPurifyDecodedMessage, needle string) *rawCCPurifyDecodedMessage {
	t.Helper()
	for i := range messages {
		if content, ok := messages[i].Content.(string); ok && strings.Contains(content, needle) {
			return &messages[i]
		}
	}
	t.Fatalf("message containing %q not found", needle)
	return nil
}

func rawCCPurifyMessageWithToolCall(t *testing.T, messages []rawCCPurifyDecodedMessage, callID string) *rawCCPurifyDecodedMessage {
	t.Helper()
	for i := range messages {
		for _, call := range messages[i].ToolCalls {
			if call.ID == callID {
				return &messages[i]
			}
		}
	}
	t.Fatalf("assistant tool call %q not found", callID)
	return nil
}
