//go:build unit

package service

// 方案 §2.2.1「逐点分配表」cursor_execution_service.go 部分 + §2.2.2 第 6 条：
// 每个产生点用真实触发输入走生产函数，断言 CursorRequestError 的 Code / Status，
// 且 errors.Is(err, ErrCursorUnsupportedRequest) 仍成立（既有归类不受影响）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestTestCursorExecutionServiceRequestErrorCodes(t *testing.T) {
	ctx := context.Background()
	account := &Account{Platform: PlatformCursor}
	svc := newCursorLifecycleTestService()

	cases := []struct {
		name   string
		run    func(t *testing.T) error
		code   string
		status int
	}{
		{"execute_nil_chat", func(t *testing.T) error {
			_, err := svc.Execute(ctx, nil, account, &CursorExecutionRequest{})
			return err
		}, infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError},
		{"execute_no_model", func(t *testing.T) error {
			_, err := svc.Execute(ctx, nil, account, &CursorExecutionRequest{Chat: &apicompat.ChatCompletionsRequest{}})
			return err
		}, infraerrors.CursorReqMissingModelCode, http.StatusBadRequest},
		{"chat_no_model", func(t *testing.T) error {
			_, err := cursorExecuteChat(ctx, nil, svc, account, []byte(`{"model":"  ","messages":[{"role":"user","content":"hi"}]}`))
			return err
		}, infraerrors.CursorReqMissingModelCode, http.StatusBadRequest},
		{"responses_nil", func(t *testing.T) error {
			_, err := cursorExecuteResponses(ctx, nil, svc, account, nil)
			return err
		}, infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError},
		{"responses_no_model", func(t *testing.T) error {
			_, err := cursorExecuteResponses(ctx, nil, svc, account, &apicompat.ResponsesRequest{})
			return err
		}, infraerrors.CursorReqMissingModelCode, http.StatusBadRequest},
		{"anthropic_nil", func(t *testing.T) error {
			_, err := cursorExecuteAnthropic(ctx, nil, svc, account, nil)
			return err
		}, infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError},
		{"anthropic_no_model", func(t *testing.T) error {
			_, err := cursorExecuteAnthropic(ctx, nil, svc, account, &apicompat.AnthropicRequest{})
			return err
		}, infraerrors.CursorReqMissingModelCode, http.StatusBadRequest},
		{"gemini_native_no_model", func(t *testing.T) error {
			_, err := cursorExecuteGeminiNative(ctx, nil, svc, account, " ", false, []byte(`{"contents":[]}`))
			return err
		}, infraerrors.CursorReqMissingModelCode, http.StatusBadRequest},
		{"gemini_native_convert_failed", func(t *testing.T) error {
			body := []byte(`{"contents":[{"role":"user","parts":[{"functionCall":{"name":"get_weather","args":{}}}]}]}`)
			_, err := cursorExecuteGeminiNative(ctx, nil, svc, account, "gemini-test", false, body)
			return err
		}, infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest},
		{"probe_empty_upstream_model", func(t *testing.T) error {
			_, _, _, err := svc.ProbeAgentRun(ctx, account, " ")
			return err
		}, infraerrors.CursorExecModelMappingInvalidCode, http.StatusInternalServerError},
		{"plan_chat_nil", func(t *testing.T) error {
			_, err := cursorPlanFromChat(nil, "m")
			return err
		}, infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError},
		{"plan_messages_nil", func(t *testing.T) error {
			_, err := cursorPlanFromMessages(nil, "m")
			return err
		}, infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError},
		{"plan_responses_nil", func(t *testing.T) error {
			_, err := cursorPlanFromResponses(nil, "m")
			return err
		}, infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError},
		{"deliver_tool_result_transport_failure", func(t *testing.T) error {
			ls := newCursorLifecycleTestService()
			record := startCursorLifecycleTestSession(t, ls)
			delegations := newCursorToolDelegations()
			require.NoError(t, delegations.noteDelegation("",
				cursorToolCallCapture{ID: "call_1", Name: "get_weather", ExecID: 7, IDField: []byte{0xaa}}))
			pr, pw := io.Pipe()
			require.NoError(t, pr.Close())
			d := newWritebackDuplex(pr, pw, delegations)
			require.NoError(t, ls.installIntake(record.id, &cursorAgentRunIntake{
				svc: ls, session: record, duplex: d, delegations: delegations,
			}))
			return record.intake.DeliverToolResults(ctx, record.id, cursorSessionLevelBatchID,
				[]CursorToolResult{{CallID: "call_1", Text: []string{"sunny"}}})
		}, infraerrors.CursorExecResultWriteFailedCode, http.StatusBadGateway},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t)
			require.Error(t, err)
			var reqErr *CursorRequestError
			require.True(t, errors.As(err, &reqErr), "应为 *CursorRequestError，实际: %v", err)
			require.Equal(t, tc.code, reqErr.Code)
			require.Equal(t, tc.status, reqErr.Status)
			require.True(t, errors.Is(err, ErrCursorUnsupportedRequest))
		})
	}
}
