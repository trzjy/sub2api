//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestCursorMessagesPlanPreservesOfficialSystemMessage(t *testing.T) {
	req := &apicompat.AnthropicRequest{
		Model: "cursor-test-model",
		Messages: []apicompat.AnthropicMessage{
			{Role: "system", Content: json.RawMessage(`"Follow the repository instructions."`)},
			{Role: "user", Content: json.RawMessage(`"inspect the code"`)},
		},
	}

	plan, err := cursorPlanFromMessages(req, req.Model)
	require.NoError(t, err)
	require.Equal(t, 1, plan.history.ItemCount())
	withoutSystem := *req
	withoutSystem.Messages = withoutSystem.Messages[1:]
	baseline, err := cursorPlanFromMessages(&withoutSystem, req.Model)
	require.NoError(t, err)
	// system 正文属**软组**（configSystem）：它进固定配置摘要、不进用户历史，但续轮对软组
	// 只留痕不拒绝（见 ResumeCursorSession 的分组说明）。硬组（模型 + 工具声明/schema）在
	// 这一对请求里完全相同——这正是"改 system 不再把正常续轮拒成 CUR-RES-04"的依据。
	require.NotEqual(t, baseline.configSystem.HexString(), plan.configSystem.HexString(),
		"system message belongs to the soft config group")
	require.Equal(t, baseline.config.HexString(), plan.config.HexString(),
		"system message must not enter the hard config group")
	require.Equal(t, baseline.history.HexString(), plan.history.HexString(), "system message must not enter user history")
}

func TestCursorResponsesPlanRejectsUnknownMessageRole(t *testing.T) {
	req := &apicompat.ResponsesRequest{
		Model: "cursor-test-model",
		Input: json.RawMessage(`[{"type":"message","role":"critic","content":"do not demote me"}]`),
	}

	_, err := cursorPlanFromResponses(req, req.Model)
	require.ErrorIs(t, err, ErrCursorResumeUnsupportedContent)
}

func TestCursorResponsesPlanRejectsUnknownItemType(t *testing.T) {
	req := &apicompat.ResponsesRequest{
		Model: "cursor-test-model",
		Input: json.RawMessage(`[{"type":"web_search_call","id":"ws_1"}]`),
	}

	_, err := cursorPlanFromResponses(req, req.Model)
	require.ErrorIs(t, err, ErrCursorResumeUnsupportedContent)
}

func TestCursorChatPlanRejectsToolCallsOnNonAssistantMessage(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Model: "cursor-test-model",
		Messages: []apicompat.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"hello"`),
			ToolCalls: []apicompat.ChatToolCall{{
				ID:       "call_1",
				Function: apicompat.ChatFunctionCall{Name: "lookup", Arguments: "{}"},
			}},
		}},
	}

	_, err := cursorPlanFromChat(req, req.Model)
	require.ErrorIs(t, err, ErrCursorResumeUnsupportedContent)
}

func TestCursorInboundProtocolErrorsAreClient400(t *testing.T) {
	ctx := context.Background()
	account := &Account{Platform: PlatformCursor}
	exec := newCursorLifecycleTestService()
	cases := []struct {
		name string
		run  func() error
	}{
		{name: "chat continuation", run: func() error {
			_, err := cursorExecuteChat(ctx, nil, exec, account, []byte(`{"model":"cursor-test-model","messages":[{"role":"user","content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`))
			return err
		}},
		{name: "responses continuation", run: func() error {
			_, err := cursorExecuteResponses(ctx, nil, exec, account, &apicompat.ResponsesRequest{Model: "cursor-test-model", Input: json.RawMessage(`[{"type":"web_search_call","id":"ws_1"}]`)})
			return err
		}},
		{name: "anthropic continuation", run: func() error {
			_, err := cursorExecuteAnthropic(ctx, nil, exec, account, &apicompat.AnthropicRequest{Model: "cursor-test-model", Messages: []apicompat.AnthropicMessage{{Role: "developer", Content: json.RawMessage(`"do not demote me"`)}}})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			var resumeErr *CursorResumeError
			require.True(t, errors.As(err, &resumeErr))
			status, reason := CursorResumeHTTPStatus(err)
			require.Equal(t, http.StatusBadRequest, status)
			require.Equal(t, "unsupported_content", reason)
		})
	}
}
