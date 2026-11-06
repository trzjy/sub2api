//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 本组用例钉住「上游空完成」这条判据（upstream_completion_audit.go）。
//
// 线上背景（2026-11-06）：账号 137 三天内 163 次「200 + 完整终态 + 零正文」，
// 占其 /v1/messages 流量 12.5%，同分组另外三个账号 0 次。这些请求既没有错误码、
// 也不进 ops 错误表，客户只看到「模型不说话」。判据一旦失效，问题就回到不可见状态。

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
