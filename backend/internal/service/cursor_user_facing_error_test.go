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
)

const cursorUserFacingTestRequestID = "req-s1-test-0001"

// TestCursorUserFacingError_UnmappedHasNoFallback 锁定 2026-11-22 用户裁定：
// 未知的 "cursor:" 前缀错误必须返回 ok=false 交上层如实处理。
func TestCursorUserFacingError_UnmappedHasNoFallback(t *testing.T) {
	t.Parallel()
	_, _, ok := CursorUserFacingError(cursorUserFacingTestCtx(), errors.New("cursor: something new happened"))
	if ok {
		t.Fatalf("unmapped cursor error must not be mapped (fallback removed)")
	}
}

func cursorUserFacingTestCtx() context.Context {
	return context.WithValue(context.Background(), ctxkey.RequestID, cursorUserFacingTestRequestID)
}

func TestCursorUserFacingError_Table(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		zh     string
	}{
		// 1. *CursorResumeError
		{"1_resume_error", newCursorResumeError(http.StatusConflict, "session_finished", nil),
			http.StatusConflict, infraerrors.CursorResSessionEnded},
		{"1_resume_error_wrapped", fmt.Errorf("serve: %w", newCursorResumeError(http.StatusForbidden, "identity_mismatch", ErrCursorProtocol)),
			http.StatusForbidden, infraerrors.CursorResIdentityMismatch},
		// 2. *CursorRequestError
		{"2_request_error", newCursorRequestError(http.StatusBadRequest, infraerrors.CursorReqMissingModel, "request carries no model"),
			http.StatusBadRequest, infraerrors.CursorReqMissingModel},
		{"2_request_error_wrapped", fmt.Errorf("convert: %w", newCursorRequestError(http.StatusBadRequest, infraerrors.CursorReqUnsupportedMessage, "request content cannot be converted")),
			http.StatusBadRequest, infraerrors.CursorReqUnsupportedMessage},
		{"2_model_mapping", newCursorRequestError(http.StatusInternalServerError, infraerrors.CursorExecModelMappingInvalid, "empty upstream model"),
			http.StatusInternalServerError, infraerrors.CursorExecModelMappingInvalid},
		// 6. 写回失败由第 2 条命中（产生点在 S2b 改为 CursorRequestError）
		{"6_result_write_via_request_error", newCursorRequestError(http.StatusBadGateway, infraerrors.CursorExecResultWriteFailed, "deliver tool result transport failure: %v", errors.New("broken pipe")),
			http.StatusBadGateway, infraerrors.CursorExecResultWriteFailed},
		// 3. context.DeadlineExceeded
		{"3_deadline", context.DeadlineExceeded,
			http.StatusGatewayTimeout, infraerrors.CursorExecDeadlineExceeded},
		{"3_deadline_wrapped", fmt.Errorf("cursor: run: %w", context.DeadlineExceeded),
			http.StatusGatewayTimeout, infraerrors.CursorExecDeadlineExceeded},
		// 4. 认证类
		// 注：ErrCursorKeyRejectedUpstream（上游注入「密钥已过期或额度耗尽，请续费」）
		// 上游拒付提示与认证失败提示保持区分。
		{"4_key_rejected", ErrCursorKeyRejectedUpstream,
			http.StatusBadGateway, infraerrors.CursorExecAccountRejected},
		{"4_relay_auth_wrapped", fmt.Errorf("forward: %w", ErrCursorRelayAuthRejected),
			http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailed},
		{"4_auth_material", ErrCursorAuthMaterialUnavailable,
			http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailed},
		{"4_quota_key_unresolved", ErrCursorQuotaKeyUnresolved,
			http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailed},
		{"4_bearer_missing_wrapped", fmt.Errorf("x: %w", ErrCursorAccountBearerMissing),
			http.StatusBadGateway, infraerrors.CursorExecUpstreamAuthFailed},
		// 4a. 上游模型暂不可用
		{"4a_model_unavailable_struct", &CursorModelUnavailableError{TurnID: "1-a"},
			http.StatusBadGateway, infraerrors.CursorExecModelUnavailable},
		{"4a_model_unavailable_wrapped", fmt.Errorf("x: %w", ErrCursorUpstreamModelUnavailable),
			http.StatusBadGateway, infraerrors.CursorExecModelUnavailable},
		// 5. 批次
		{"5_batch_incomplete", ErrCursorBatchIncomplete,
			http.StatusBadGateway, infraerrors.CursorExecBatchIncomplete},
		{"5_batch_ambiguous_wrapped", fmt.Errorf("seal: %w", ErrCursorBatchAmbiguous),
			http.StatusBadGateway, infraerrors.CursorExecBatchIncomplete},
		// 7. 协议
		{"7_protocol", ErrCursorProtocol,
			http.StatusBadGateway, infraerrors.CursorExecUpstreamProtocol},
		{"7_missing_end_stream_wrapped", fmt.Errorf("read: %w", ErrCursorMissingEndStream),
			http.StatusBadGateway, infraerrors.CursorExecUpstreamProtocol},
		{"7_upstream_stream", ErrCursorUpstreamStream,
			http.StatusBadGateway, infraerrors.CursorExecUpstreamProtocol},
		// 8. 不可用
		{"8_execution_unavailable", ErrCursorExecutionUnavailable,
			http.StatusServiceUnavailable, infraerrors.CursorResServiceUnavailable},
		{"8_fingerprint_wrapped", fmt.Errorf("gate: %w", ErrCursorSourceFingerprintUnavailable),
			http.StatusServiceUnavailable, infraerrors.CursorResServiceUnavailable},
		// 9. 上游会话提前终止，保留明确提示。
		{"9a_resume_not_waiting", ErrCursorResumeNotWaiting,
			http.StatusBadGateway, infraerrors.CursorExecSessionNotWaiting},
		{"9b_resume_not_waiting_wrapped", fmt.Errorf("continuation: %w", ErrCursorResumeNotWaiting),
			http.StatusBadGateway, infraerrors.CursorExecSessionNotWaiting},
		{"9c_content_while_waiting_tools", ErrCursorContentWhileWaitingTools,
			http.StatusBadGateway, infraerrors.CursorExecSessionNotWaiting},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, msg, ok := CursorUserFacingError(cursorUserFacingTestCtx(), tc.err)
			if !ok {
				t.Fatalf("ok=false, want true")
			}
			if status != tc.status {
				t.Fatalf("got status %d, want %d", status, tc.status)
			}
			if !strings.HasPrefix(msg, tc.zh) {
				t.Fatalf("message %q does not start with %q", msg, tc.zh)
			}
			if strings.Contains(msg, "CUR-") {
				t.Fatalf("message %q contains a local error number", msg)
			}
			if !strings.Contains(msg, "请求号 "+cursorUserFacingTestRequestID) {
				t.Fatalf("message %q missing request id", msg)
			}
		})
	}
}

func TestCursorUserFacingError_NotCursor(t *testing.T) {
	if _, _, ok := CursorUserFacingError(cursorUserFacingTestCtx(), nil); ok {
		t.Fatal("nil error must return ok=false")
	}
	if _, _, ok := CursorUserFacingError(cursorUserFacingTestCtx(), errors.New("boom")); ok {
		t.Fatal("non-cursor error must return ok=false")
	}
}

func TestCursorUserFacingError_MissingRequestID(t *testing.T) {
	_, msg, ok := CursorUserFacingError(context.Background(), ErrCursorProtocol)
	if !ok || !strings.Contains(msg, "请求号 未知") {
		t.Fatalf("ok=%v msg=%q, want 请求号 未知", ok, msg)
	}
}

// 同时满足第 2 条与第 7 条（Detail 文本里带 ErrCursorProtocol，按约定 %v 而非 wrap）必须命中第 2 条。
func TestCursorUserFacingError_OrderRequestErrorBeforeProtocol(t *testing.T) {
	err := newCursorRequestError(http.StatusBadRequest, infraerrors.CursorReqUnsupportedMessage, "convert: %v", ErrCursorProtocol)
	if !strings.Contains(err.Error(), ErrCursorProtocol.Error()) {
		t.Fatalf("detail should carry protocol text, got %q", err.Error())
	}
	if errors.Is(err, ErrCursorProtocol) {
		t.Fatal("args error must be formatted as text, not wrapped")
	}
	status, message, ok := CursorUserFacingError(cursorUserFacingTestCtx(), err)
	if !ok || status != http.StatusBadRequest || !strings.HasPrefix(message, infraerrors.CursorReqUnsupportedMessage) {
		t.Fatalf("got (%d, %s, %v), want request error message and status 400", status, message, ok)
	}
}

// 同时匹配上游拒付与模型不可用时，仍保留更具体的拒付提示。
func TestCursorUserFacingError_OrderAuthBeforeModelUnavailable(t *testing.T) {
	for _, err := range []error{
		ErrCursorKeyRejectedUpstream,
		errors.Join(ErrCursorKeyRejectedUpstream, ErrCursorUpstreamModelUnavailable),
	} {
		status, message, ok := CursorUserFacingError(cursorUserFacingTestCtx(), err)
		if !ok || status != http.StatusBadGateway || !strings.HasPrefix(message, infraerrors.CursorExecAccountRejected) {
			t.Fatalf("err=%v got (%d, %s, %v), want rejection message and 502", err, status, message, ok)
		}
	}
}

func TestCursorRequestError_IsAndAs(t *testing.T) {
	err := fmt.Errorf("outer: %w", newCursorRequestError(http.StatusBadRequest, infraerrors.CursorReqUnsupportedMessage, "tool %q: invalid input_schema JSON", "search"))
	if !errors.Is(err, ErrCursorUnsupportedRequest) {
		t.Fatal("errors.Is(ErrCursorUnsupportedRequest) must be true")
	}
	var reqErr *CursorRequestError
	if !errors.As(err, &reqErr) {
		t.Fatal("errors.As must find *CursorRequestError")
	}
	if reqErr.Message != infraerrors.CursorReqUnsupportedMessage || reqErr.Status != http.StatusBadRequest {
		t.Fatalf("got (%s, %d)", reqErr.Message, reqErr.Status)
	}
	if want := `cursor: tool "search": invalid input_schema JSON`; reqErr.Error() != want {
		t.Fatalf("Error()=%q, want %q", reqErr.Error(), want)
	}
}
