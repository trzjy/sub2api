//go:build unit

package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestCursorDiagnosticConversionFailureRetainsShapeAndSkippedLedger(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-conversion-evidence")
	req := &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
		{Role: "user", Content: json.RawMessage(`"PRIVATE-USER-CONTENT"`)},
		{Role: "PRIVATE-UNKNOWN-ROLE", Content: json.RawMessage(`"PRIVATE-UNKNOWN-CONTENT"`)},
		{Role: "assistant", Content: json.RawMessage(`"PRIVATE-ASSISTANT-CONTENT"`)},
	}}
	gw, c := newCursorRLStopService(t, nil)
	body, err := json.Marshal(req)
	require.NoError(t, err)
	_, err = executeCursorForTest(t, gw, ctx, c, cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"),
		body, "cursor-sonnet", false, time.Now())
	assertCursorRequestError(t, err, infraerrors.CursorReqPrefill, http.StatusBadRequest)
	require.Equal(t, 1, phaseCount(logs, "request_conversion_failed"))
	require.Equal(t, "req-conversion-evidence", frameEvidencePhaseField(logs, "request_conversion_failed", "request_id"))
	require.Equal(t, "3", frameEvidencePhaseField(logs, "request_conversion_failed", "semantic_message_count"))
	require.Equal(t, "[user unknown assistant]", frameEvidencePhaseField(logs, "request_conversion_failed", "semantic_tail_roles"))
	require.Equal(t, "message_unknown_role×1", frameEvidencePhaseField(logs, "chat_messages_dropped", "skipped_types"))
	for _, private := range []string{"PRIVATE-USER-CONTENT", "PRIVATE-UNKNOWN-ROLE", "PRIVATE-UNKNOWN-CONTENT", "PRIVATE-ASSISTANT-CONTENT"} {
		require.False(t, observedContainsText(logs, private), private)
	}
	t.Logf("conversion failure evidence: %v", logs.FilterField(zap.String("phase", "request_conversion_failed")).All()[0].ContextMap())
}

func TestCursorDiagnosticResponsesFailureRetainsOriginalPhase(t *testing.T) {
	for _, phase := range []string{"commentary", "final_answer"} {
		t.Run(phase, func(t *testing.T) {
			ctx, logs := cursorPhaseObserverCtx(t, "req-responses-evidence")
			gw, c := newCursorRLStopService(t, nil)
			req := &apicompat.ResponsesRequest{
				Model: "cursor-sonnet",
				Input: json.RawMessage(`[{"role":"user","content":"PRIVATE-USER-CONTENT"},` +
					`{"type":"message","role":"assistant","phase":"` + phase + `","content":"PRIVATE-ASSISTANT-CONTENT"}]`),
			}
			result, err := cursorExecuteResponses(ctx, c, gw.cursorExecution,
				cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"), req)
			require.Nil(t, result)
			assertCursorRequestError(t, err, infraerrors.CursorReqPrefill, http.StatusBadRequest)
			require.Equal(t, 1, phaseCount(logs, "responses_request_failed"))
			require.Equal(t, "req-responses-evidence", frameEvidencePhaseField(logs, "responses_request_failed", "request_id"))
			require.Equal(t, "[message:user:unspecified message:assistant:"+phase+"]",
				frameEvidencePhaseField(logs, "responses_request_failed", "responses_input_tail"))
			require.Equal(t, "[user assistant]", frameEvidencePhaseField(logs, "responses_request_failed", "semantic_tail_roles"))
			for _, private := range []string{"PRIVATE-USER-CONTENT", "PRIVATE-ASSISTANT-CONTENT", "BEARER-ACCOUNT-A", "ACTIVATION-CODE-ELEVEN"} {
				require.False(t, observedContainsText(logs, private), private)
			}
			require.Zero(t, phaseCount(logs, "http_request_start"), "prefill rejection must remain before outbound I/O")
			t.Logf("responses failure evidence: %v", logs.FilterField(zap.String("phase", "responses_request_failed")).All()[0].ContextMap())
		})
	}
}

func TestCursorDiagnosticShapesAreBoundedAndUseOnlyProtocolLabels(t *testing.T) {
	ctx, logs := cursorPhaseObserverCtx(t, "req-shape-evidence")
	var messages []apicompat.ChatMessage
	var items []map[string]any
	for i := 0; i < 12; i++ {
		messages = append(messages, apicompat.ChatMessage{Role: "assistant", Content: json.RawMessage(`"PRIVATE-CONTENT"`)})
		items = append(items, map[string]any{"type": "message", "role": "assistant", "phase": "commentary", "content": "PRIVATE-CONTENT"})
	}
	messages[11].Role = "PRIVATE-ROLE"
	items[11] = map[string]any{"type": "PRIVATE-TYPE", "role": "PRIVATE-ROLE", "phase": "PRIVATE-PHASE", "arguments": "PRIVATE-ARGS"}
	input, err := json.Marshal(items)
	require.NoError(t, err)
	fields := cursorResponsesShapeFields(input)
	fields = append(fields, cursorSemanticShapeFields(cursorDecodeChatSemantic(&apicompat.ChatCompletionsRequest{Messages: messages}))...)
	cursorPhaseLog(ctx, zap.InfoLevel, "test_shape", "m", fields...)
	entry := logs.FilterField(zap.String("phase", "test_shape")).All()[0].ContextMap()
	require.EqualValues(t, 12, entry["responses_input_items"])
	require.EqualValues(t, 12, entry["semantic_message_count"])
	require.Len(t, entry["responses_input_tail"], 8)
	require.Len(t, entry["semantic_tail_roles"], 8)
	for _, private := range []string{"PRIVATE-CONTENT", "PRIVATE-TYPE", "PRIVATE-ROLE", "PRIVATE-PHASE", "PRIVATE-ARGS"} {
		require.False(t, observedContainsText(logs, private), private)
	}
	for _, input := range []string{`null`, `"PRIVATE-STRING-INPUT"`, `{}`, `not-json`} {
		fields := cursorResponsesShapeFields(json.RawMessage(input))
		require.Len(t, fields, 1)
		require.Equal(t, zap.Bool("responses_input_array", false), fields[0])
	}
	require.Equal(t, zap.Bool("responses_input_array", true), cursorResponsesShapeFields(json.RawMessage(`[]`))[0])
}

func TestCursorDiagnosticEndStreamSummaryPrecedesErrorMapping(t *testing.T) {
	for _, code := range []string{"ERROR_BAD_MODEL_NAME", "internal"} {
		for _, stream := range []bool{false, true} {
			t.Run(code+"/stream="+boolToJSON(stream), func(t *testing.T) {
				ctx, logs := cursorPhaseObserverCtx(t, "req-endstream-evidence")
				payload := `{"error":{"code":"` + code + `","message":"unsupported parameter temperature; BEARER-ACCOUNT-A; api_key=PRIVATE-KEY"}}`
				gw, c := newCursorRLStopService(t, cursorTestEndStream(t, payload))
				result, err := executeCursorForTest(t, gw, ctx, c,
					cursorOutboundAccount(1, 11, "BEARER-ACCOUNT-A"),
					cursorRLStopRequestBody(stream), "cursor-sonnet", stream, time.Now())
				require.Error(t, err)
				require.Nil(t, result)
				var fe *UpstreamFailoverError
				if code == "internal" {
					require.ErrorAs(t, err, &fe)
					require.True(t, fe.ShouldRetryNextAccount())
				} else {
					require.False(t, errors.As(err, &fe))
					require.ErrorIs(t, err, ErrCursorUpstreamStream)
				}
				require.Equal(t, code, frameEvidencePhaseField(logs, "response_parse_done", "upstream_code"))
				require.Contains(t, frameEvidencePhaseField(logs, "response_parse_done", "upstream_message"), "unsupported parameter temperature")
				require.Equal(t, "1", frameEvidencePhaseField(logs, "response_parse_done", "account_id"))
				require.Equal(t, "upstream-sonnet", frameEvidencePhaseField(logs, "response_parse_done", "upstream_model"))
				require.Equal(t, "req-endstream-evidence", frameEvidencePhaseField(logs, "response_parse_done", "request_id"))
				phaseLogs := logs.FilterField(zap.String("phase", "response_parse_done"))
				require.False(t, observedContainsText(phaseLogs, "BEARER-ACCOUNT-A"))
				require.False(t, observedContainsText(phaseLogs, "PRIVATE-KEY"))
				t.Logf("endstream failure evidence: %v", phaseLogs.All()[0].ContextMap())
			})
		}
	}
}
