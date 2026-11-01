//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const cursorUserFacingTestRequestID = "req-s1-test-0001"

func cursorUserFacingTestCtx() context.Context {
	return context.WithValue(context.Background(), ctxkey.RequestID, cursorUserFacingTestRequestID)
}

func TestCursorUserFacingError_Table(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
		zh     string
	}{
		// 1. *CursorResumeError
		{"1_resume_error", newCursorResumeError(http.StatusConflict, "session_finished", nil),
			http.StatusConflict, infraerrors.CursorResSessionEndedCode, infraerrors.CursorResSessionEnded},
		{"1_resume_error_wrapped", fmt.Errorf("serve: %w", newCursorResumeError(http.StatusForbidden, "identity_mismatch", ErrCursorProtocol)),
			http.StatusForbidden, infraerrors.CursorResIdentityMismatchCode, infraerrors.CursorResIdentityMismatch},
		// 2. *CursorRequestError
		{"2_request_error", newCursorRequestError(infraerrors.CursorReqMissingModelCode, http.StatusBadRequest, "request carries no model"),
			http.StatusBadRequest, infraerrors.CursorReqMissingModelCode, infraerrors.CursorReqMissingModel},
		{"2_request_error_wrapped", fmt.Errorf("convert: %w", newCursorRequestError(infraerrors.CursorReqInvalidImageCode, http.StatusBadRequest, "remote image url is not supported")),
			http.StatusBadRequest, infraerrors.CursorReqInvalidImageCode, infraerrors.CursorReqInvalidImage},
		{"2_model_mapping", newCursorRequestError(infraerrors.CursorExecModelMappingInvalidCode, http.StatusInternalServerError, "empty upstream model"),
			http.StatusInternalServerError, infraerrors.CursorExecModelMappingInvalidCode, infraerrors.CursorExecModelMappingInvalid},
		// 6. 写回失败由第 2 条命中（产生点在 S2b 改为 CursorRequestError）
		{"6_result_write_via_request_error", newCursorRequestError(infraerrors.CursorExecResultWriteFailedCode, http.StatusBadGateway, "deliver tool result transport failure: %v", errors.New("broken pipe")),
			http.StatusBadGateway, infraerrors.CursorExecResultWriteFailedCode, infraerrors.CursorExecResultWriteFailed},
		// 3. context.DeadlineExceeded
		{"3_deadline", context.DeadlineExceeded,
			http.StatusGatewayTimeout, infraerrors.CursorExecDeadlineExceededCode, infraerrors.CursorExecDeadlineExceeded},
		{"3_deadline_wrapped", fmt.Errorf("cursor: run: %w", context.DeadlineExceeded),
			http.StatusGatewayTimeout, infraerrors.CursorExecDeadlineExceededCode, infraerrors.CursorExecDeadlineExceeded},
		// 4. 认证类
		{"4_key_rejected", ErrCursorKeyRejectedUpstream,
			http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailedCode, infraerrors.CursorExecUpstreamAuthFailed},
		{"4_relay_auth_wrapped", fmt.Errorf("forward: %w", ErrCursorRelayAuthRejected),
			http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailedCode, infraerrors.CursorExecUpstreamAuthFailed},
		{"4_auth_material", ErrCursorAuthMaterialUnavailable,
			http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailedCode, infraerrors.CursorExecUpstreamAuthFailed},
		{"4_quota_key_unresolved", ErrCursorQuotaKeyUnresolved,
			http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailedCode, infraerrors.CursorExecUpstreamAuthFailed},
		{"4_bearer_missing_wrapped", fmt.Errorf("x: %w", ErrCursorAccountBearerMissing),
			http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailedCode, infraerrors.CursorExecUpstreamAuthFailed},
		// 4a. 上游模型暂不可用
		{"4a_model_unavailable_struct", &CursorModelUnavailableError{TurnID: "1-a"},
			http.StatusBadGateway, infraerrors.CursorExecModelUnavailableCode, infraerrors.CursorExecModelUnavailable},
		{"4a_model_unavailable_wrapped", fmt.Errorf("x: %w", ErrCursorUpstreamModelUnavailable),
			http.StatusBadGateway, infraerrors.CursorExecModelUnavailableCode, infraerrors.CursorExecModelUnavailable},
		// 5. 批次
		{"5_batch_incomplete", ErrCursorBatchIncomplete,
			http.StatusBadGateway, infraerrors.CursorExecBatchIncompleteCode, infraerrors.CursorExecBatchIncomplete},
		{"5_batch_ambiguous_wrapped", fmt.Errorf("seal: %w", ErrCursorBatchAmbiguous),
			http.StatusBadGateway, infraerrors.CursorExecBatchIncompleteCode, infraerrors.CursorExecBatchIncomplete},
		// 7. 协议
		{"7_protocol", ErrCursorProtocol,
			http.StatusBadGateway, infraerrors.CursorExecUpstreamProtocolCode, infraerrors.CursorExecUpstreamProtocol},
		{"7_missing_end_stream_wrapped", fmt.Errorf("read: %w", ErrCursorMissingEndStream),
			http.StatusBadGateway, infraerrors.CursorExecUpstreamProtocolCode, infraerrors.CursorExecUpstreamProtocol},
		{"7_upstream_stream", ErrCursorUpstreamStream,
			http.StatusBadGateway, infraerrors.CursorExecUpstreamProtocolCode, infraerrors.CursorExecUpstreamProtocol},
		// 8. 不可用
		{"8_execution_unavailable", ErrCursorExecutionUnavailable,
			http.StatusServiceUnavailable, infraerrors.CursorResServiceUnavailableCode, infraerrors.CursorResServiceUnavailable},
		{"8_fingerprint_wrapped", fmt.Errorf("gate: %w", ErrCursorSourceFingerprintUnavailable),
			http.StatusServiceUnavailable, infraerrors.CursorResServiceUnavailableCode, infraerrors.CursorResServiceUnavailable},
		// 9. 带 cursor 前缀的未命中错误
		{"9_unmapped_cursor_prefix", errors.New("cursor: something new happened"),
			http.StatusBadGateway, infraerrors.CursorExecInternalErrorCode, infraerrors.CursorExecInternalError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, msg, ok := CursorUserFacingError(cursorUserFacingTestCtx(), tc.err)
			if !ok {
				t.Fatalf("ok=false, want true")
			}
			if status != tc.status || code != tc.code {
				t.Fatalf("got (%d, %s), want (%d, %s)", status, code, tc.status, tc.code)
			}
			if !strings.HasPrefix(msg, tc.zh) {
				t.Fatalf("message %q does not start with %q", msg, tc.zh)
			}
			if !strings.Contains(msg, "错误码 "+tc.code) {
				t.Fatalf("message %q missing code", msg)
			}
			if !strings.Contains(msg, "请求号 "+cursorUserFacingTestRequestID) {
				t.Fatalf("message %q missing request id", msg)
			}
		})
	}
}

func TestCursorUserFacingError_NotCursor(t *testing.T) {
	if _, _, _, ok := CursorUserFacingError(cursorUserFacingTestCtx(), nil); ok {
		t.Fatal("nil error must return ok=false")
	}
	if _, _, _, ok := CursorUserFacingError(cursorUserFacingTestCtx(), errors.New("boom")); ok {
		t.Fatal("non-cursor error must return ok=false")
	}
}

func TestCursorUserFacingError_MissingRequestID(t *testing.T) {
	_, _, msg, ok := CursorUserFacingError(context.Background(), ErrCursorProtocol)
	if !ok || !strings.Contains(msg, "请求号 未知") {
		t.Fatalf("ok=%v msg=%q, want 请求号 未知", ok, msg)
	}
}

// 同时满足第 2 条与第 7 条（Detail 文本里带 ErrCursorProtocol，按约定 %v 而非 wrap）必须命中第 2 条。
func TestCursorUserFacingError_OrderRequestErrorBeforeProtocol(t *testing.T) {
	err := newCursorRequestError(infraerrors.CursorReqUnsupportedMessageCode, http.StatusBadRequest, "convert: %v", ErrCursorProtocol)
	if !strings.Contains(err.Error(), ErrCursorProtocol.Error()) {
		t.Fatalf("detail should carry protocol text, got %q", err.Error())
	}
	if errors.Is(err, ErrCursorProtocol) {
		t.Fatal("args error must be formatted as text, not wrapped")
	}
	status, code, _, ok := CursorUserFacingError(cursorUserFacingTestCtx(), err)
	if !ok || status != http.StatusBadRequest || code != infraerrors.CursorReqUnsupportedMessageCode {
		t.Fatalf("got (%d, %s, %v), want rule 2 hit (400, %s)", status, code, ok, infraerrors.CursorReqUnsupportedMessageCode)
	}
}

// 第 4 条认证类在 4a 模型不可用之前：同时满足时必须仍是 CUR-EXEC-06，不被新 case 截走。
func TestCursorUserFacingError_OrderAuthBeforeModelUnavailable(t *testing.T) {
	for _, err := range []error{
		ErrCursorKeyRejectedUpstream,
		errors.Join(ErrCursorKeyRejectedUpstream, ErrCursorUpstreamModelUnavailable),
	} {
		status, code, _, ok := CursorUserFacingError(cursorUserFacingTestCtx(), err)
		if !ok || status != http.StatusBadGateway || code != infraerrors.CursorExecUpstreamAuthFailedCode {
			t.Fatalf("err=%v got (%d, %s, %v), want (502, %s)", err, status, code, ok, infraerrors.CursorExecUpstreamAuthFailedCode)
		}
	}
}

func TestCursorRequestError_IsAndAs(t *testing.T) {
	err := fmt.Errorf("outer: %w", newCursorRequestError(infraerrors.CursorReqInvalidToolSchemaCode, http.StatusBadRequest, "tool %q: invalid input_schema JSON", "search"))
	if !errors.Is(err, ErrCursorUnsupportedRequest) {
		t.Fatal("errors.Is(ErrCursorUnsupportedRequest) must be true")
	}
	var reqErr *CursorRequestError
	if !errors.As(err, &reqErr) {
		t.Fatal("errors.As must find *CursorRequestError")
	}
	if reqErr.Code != infraerrors.CursorReqInvalidToolSchemaCode || reqErr.Status != http.StatusBadRequest {
		t.Fatalf("got (%s, %d)", reqErr.Code, reqErr.Status)
	}
	if want := `cursor: tool "search": invalid input_schema JSON`; reqErr.Error() != want {
		t.Fatalf("Error()=%q, want %q", reqErr.Error(), want)
	}
}

func TestCursorUserFacingError_UnmappedLogsClassOnly(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	ctx := logger.IntoContext(cursorUserFacingTestCtx(), zap.New(core))
	const secret = "cursor: secret-upstream-body-xyz"
	status, code, _, ok := CursorUserFacingError(ctx, errors.New(secret))
	if !ok || status != http.StatusBadGateway || code != infraerrors.CursorExecInternalErrorCode {
		t.Fatalf("got (%d, %s, %v)", status, code, ok)
	}
	entries := logs.FilterMessage("cursor_user_error_unmapped").All()
	if len(entries) != 1 {
		t.Fatalf("want 1 cursor_user_error_unmapped entry, got %d", len(entries))
	}
	if _, has := entries[0].ContextMap()["error_class"]; !has {
		t.Fatal("missing error_class field")
	}
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "secret-upstream-body") {
			t.Fatal("log message leaks raw error text")
		}
		for k, v := range e.ContextMap() {
			if strings.Contains(fmt.Sprint(v), "secret-upstream-body") {
				t.Fatalf("log field %s leaks raw error text", k)
			}
		}
	}
}
