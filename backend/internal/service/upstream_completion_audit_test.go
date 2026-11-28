//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 本组用例钉住「上游空完成」这条判据（upstream_completion_audit.go）。
//
// 线上背景（2026-11-06）：账号 137 三天内 163 次「200 + 完整终态 + 零正文」，
// 占其 /v1/messages 流量 12.5%，同分组另外三个账号 0 次。这些请求既没有错误码、
// 也不进 ops 错误表，客户只看到「模型不说话」。判据一旦失效，问题就回到不可见状态。

func anthropicStreamTerminationLogTestContext(t *testing.T) (*gin.Context, context.Context, *observer.ObservedLogs, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "client-request-123")))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	return c, ctx, logs, rec
}

type anthropicStreamReadErrorBody struct {
	err error
}

func (b *anthropicStreamReadErrorBody) Read([]byte) (int, error) { return 0, b.err }
func (b *anthropicStreamReadErrorBody) Close() error             { return nil }

func TestAnthropicPassthroughStreamTerminationLogContainsSafeDiagnostics(t *testing.T) {
	c, ctx, logs, rec := anthropicStreamTerminationLogTestContext(t)
	const privateOutput = "private-output-canary"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"upstream-request-456"},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: message_start",
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			"event: content_block_delta",
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"` + privateOutput + `"}}`,
			"",
			"event: message_delta",
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
			"",
			"event: message_stop",
			`data: {"type":"message_stop"}`,
			"",
			"",
		}, "\n"))),
	}
	svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}

	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, &Account{ID: 41}, time.Now(), "claude-test")
	require.NoError(t, err)

	entries := logs.FilterMessage("anthropic_stream_termination").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "api_key_passthrough", fields["forwarding_mode"])
	require.Equal(t, "/v1/messages", fields["request_path"])
	require.EqualValues(t, http.StatusOK, fields["client_response_status"])
	require.Equal(t, "client-request-123", fields["request_id"])
	require.Equal(t, "upstream-request-456", fields["upstream_request_id"])
	require.Equal(t, "upstream_eof_after_terminal_event", fields["termination_reason"])
	require.Equal(t, "message_stop_event", fields["terminal_signal"])
	require.Equal(t, "end_turn", fields["stop_reason"])
	require.Equal(t, "non_empty_output", fields["empty_output_audit_result"])
	require.EqualValues(t, 1, fields["text_delta_events"])
	require.EqualValues(t, len(privateOutput), fields["text_output_bytes"])
	require.EqualValues(t, 4, fields["usage_output_tokens"])
	require.True(t, fields["terminal_event_forwarded"].(bool))
	require.Contains(t, rec.Body.String(), privateOutput)

	for _, entry := range logs.All() {
		require.NotContains(t, entry.Message, privateOutput)
		for _, value := range entry.ContextMap() {
			require.NotContains(t, fmt.Sprint(value), privateOutput)
		}
	}
}

func TestAnthropicPassthroughStreamTerminationLogReportsMissingTerminal(t *testing.T) {
	c, ctx, logs, _ := anthropicStreamTerminationLogTestContext(t)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}` + "\n\n")),
	}
	svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}

	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, &Account{ID: 42}, time.Now(), "claude-test")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing terminal event")

	entries := logs.FilterMessage("anthropic_stream_termination").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "eof_missing_terminal_event", fields["termination_reason"])
	require.Equal(t, false, fields["terminal_event_seen"])
	require.Equal(t, "not_evaluated", fields["empty_output_audit_result"])
}

func TestAnthropicPassthroughStreamTerminationLogClassifiesReadError(t *testing.T) {
	c, ctx, logs, _ := anthropicStreamTerminationLogTestContext(t)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &anthropicStreamReadErrorBody{err: io.ErrUnexpectedEOF},
	}
	svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}

	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(ctx, resp, c, &Account{ID: 43}, time.Now(), "claude-test")
	require.Error(t, err)

	entries := logs.FilterMessage("anthropic_stream_termination").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "upstream_read_error", fields["termination_reason"])
	require.Equal(t, "unexpected_eof", fields["read_error_category"])
	require.Equal(t, false, fields["terminal_event_seen"])
}

func TestAnthropicGatewayStreamTerminationLogCapturesEndTurn(t *testing.T) {
	c, ctx, logs, rec := anthropicStreamTerminationLogTestContext(t)
	const privateOutput = "gateway-output-canary"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"gateway-upstream-789"},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: message_start",
			`data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}`,
			"",
			"event: content_block_delta",
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"` + privateOutput + `"}}`,
			"",
			"event: message_delta",
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			"",
			"event: message_stop",
			`data: {"type":"message_stop"}`,
			"",
			"",
		}, "\n"))),
	}
	svc := &GatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: &RateLimitService{},
	}

	_, err := svc.handleStreamingResponse(ctx, resp, c, &Account{ID: 44, Platform: PlatformAnthropic}, time.Now(), "claude-test", "claude-test", true)
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), privateOutput)

	entries := logs.FilterMessage("anthropic_stream_termination").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "gateway_service", fields["forwarding_mode"])
	require.Equal(t, "/v1/messages", fields["request_path"])
	require.EqualValues(t, http.StatusOK, fields["client_response_status"])
	require.Equal(t, "client-request-123", fields["request_id"])
	require.Equal(t, "gateway-upstream-789", fields["upstream_request_id"])
	require.Equal(t, "upstream_eof_after_terminal_event", fields["termination_reason"])
	require.Equal(t, "message_stop_event", fields["terminal_signal"])
	require.Equal(t, "end_turn", fields["stop_reason"])
	require.True(t, fields["terminal_event_forwarded"].(bool))
	for _, entry := range entries {
		for _, value := range entry.ContextMap() {
			require.NotContains(t, fmt.Sprint(value), privateOutput)
		}
	}
}

func TestUpstreamCompletionAudit_ThinkingOnlyIsEmpty(t *testing.T) {
	audit := &upstreamCompletionAudit{}
	audit.observeAnthropicSSE(`{"type":"message_start","message":{"usage":{"input_tokens":167191,"output_tokens":0}}}`)
	audit.observeAnthropicSSE(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`)
	audit.observeAnthropicSSE(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"用户要求我..."}}`)
	audit.observeAnthropicSSE(`{"type":"content_block_stop","index":0}`)
	audit.observeAnthropicSSE(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`)
	audit.observeAnthropicSSE(`{"type":"message_stop"}`)

	require.True(t, audit.thinkingSeen, "思考内容必须被看见（用于日志归因）")
	require.False(t, audit.hasVisibleContent(), "只有思考不算正文")
	require.True(t, audit.isEmptyCompletion(&ClaudeUsage{InputTokens: 167191}), "思考-only 且 output_tokens=0 必须判空完成")
}

func TestUpstreamCompletionAudit_TextDeltaIsContent(t *testing.T) {
	audit := &upstreamCompletionAudit{}
	audit.observeAnthropicSSE(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`)
	require.False(t, audit.isEmptyCompletion(&ClaudeUsage{}))
}

func TestUpstreamCompletionAudit_ToolUseIsContent(t *testing.T) {
	// 工具参数就是行动：只发了 tool_use 没有 text 的回复完全正常，不能判空。
	audit := &upstreamCompletionAudit{}
	audit.observeAnthropicSSE(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read"}}`)
	audit.observeAnthropicSSE(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":"}}`)
	require.False(t, audit.isEmptyCompletion(&ClaudeUsage{}))
}

func TestUpstreamCompletionAudit_ReportedOutputTokensSuppresses(t *testing.T) {
	// 上游说它产出了 token，我们却数不到正文 —— 属于协议差异，不能打成故障。
	audit := &upstreamCompletionAudit{}
	audit.observeAnthropicSSE(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":37}}`)
	require.False(t, audit.isEmptyCompletion(&ClaudeUsage{OutputTokens: 37}))
}

func TestUpstreamCompletionAudit_RefusalIsNotEmpty(t *testing.T) {
	audit := &upstreamCompletionAudit{}
	audit.observeAnthropicSSE(`{"type":"message_delta","delta":{"stop_reason":"refusal"}}`)
	require.False(t, audit.isEmptyCompletion(&ClaudeUsage{}), "模型明确拒绝时空正文是合法形态")
}

func TestUpstreamCompletionAudit_NonStreamingBody(t *testing.T) {
	empty := &upstreamCompletionAudit{}
	empty.observeAnthropicBody([]byte(`{"type":"message","stop_reason":"end_turn","content":[],"usage":{"input_tokens":167191,"output_tokens":0}}`))
	require.True(t, empty.isEmptyCompletion(&ClaudeUsage{InputTokens: 167191}))

	withText := &upstreamCompletionAudit{}
	withText.observeAnthropicBody([]byte(`{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":12,"output_tokens":2}}`))
	require.False(t, withText.isEmptyCompletion(&ClaudeUsage{InputTokens: 12, OutputTokens: 2}))
}

// 端到端：上游给一段只有 thinking、没有正文的完整流 -> 必须判失败并停调该账号。
func TestAnthropicPassthrough_EmptyCompletionRejectedAndAccountCooled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	repo := &tempUnscheduleRecorder{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{}}
	svc := &GatewayService{cfg: &config.Config{}, accountRepo: repo}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":167191,"output_tokens":0}}}`,
			"",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
			"",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"用户要求..."}}`,
			"",
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
		}, "\n"))),
	}

	account := &Account{ID: 137, Name: "星思云站Deepseek"}
	result, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, account, time.Now(), "deepseek-v4.1-flash-1017")

	require.Error(t, err)
	var emptyErr *upstreamEmptyCompletionError
	require.ErrorAs(t, err, &emptyErr, "必须返回专门的空完成错误，而不是静默成功")
	require.Equal(t, int64(137), emptyErr.AccountID)
	require.NotNil(t, result, "已观测到的 usage 必须带出，避免上游已计量的请求漏记")
	require.Equal(t, []int64{137}, repo.cooled, "空完成是账号级故障，必须停调该账号让客户端重试换号")
}

// 对照组：有正文的流不能被误伤。
func TestAnthropicPassthrough_NormalStreamNotRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	repo := &tempUnscheduleRecorder{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{}}
	svc := &GatewayService{cfg: &config.Config{}, accountRepo: repo}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}`,
			"",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`,
			"",
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			"",
			`data: {"type":"message_stop"}`,
			"",
		}, "\n"))),
	}

	_, err := svc.handleStreamingResponseAnthropicAPIKeyPassthrough(
		context.Background(), resp, c, &Account{ID: 1}, time.Now(), "m")
	require.NoError(t, err)
	require.Empty(t, repo.cooled, "正常流不得触发停调")
}

// 端到端（非流式）：合法但空的 content 数组同样要判失败。
func TestAnthropicPassthrough_NonStreamingEmptyCompletionRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	repo := &tempUnscheduleRecorder{mockAccountRepoForPlatform: &mockAccountRepoForPlatform{}}
	svc := &GatewayService{cfg: &config.Config{}, accountRepo: repo}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"type":"message","stop_reason":"end_turn","content":[],"usage":{"input_tokens":167191,"output_tokens":0}}`)),
	}

	_, err := svc.handleNonStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, &Account{ID: 137})
	require.Error(t, err)
	var emptyErr *upstreamEmptyCompletionError
	require.ErrorAs(t, err, &emptyErr)
	require.Equal(t, []int64{137}, repo.cooled)
}

// tempUnscheduleRecorder 记录停调调用，其余账号仓库方法沿用既有桩。
type tempUnscheduleRecorder struct {
	*mockAccountRepoForPlatform
	cooled []int64
}

func (r *tempUnscheduleRecorder) SetTempUnschedulable(_ context.Context, id int64, _ time.Time, _ string) error {
	r.cooled = append(r.cooled, id)
	return nil
}
