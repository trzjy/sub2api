//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCursorStreamErrClass(t *testing.T) {
	wrapped := func(err error) error { return fmt.Errorf("wrapped: %w", err) }
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "ok"},
		{"eof", io.EOF, "eof"},
		{"context_canceled", context.Canceled, "context_canceled"},
		{"context_deadline", context.DeadlineExceeded, "context_deadline"},
		{"key_rejected", ErrCursorKeyRejectedUpstream, "key_rejected"},
		{"upstream_end_stream_error", ErrCursorUpstreamStream, "upstream_end_stream_error"},
		{"missing_end_stream", ErrCursorMissingEndStream, "missing_end_stream"},
		{"unsupported_execution", ErrCursorUnsupportedRequest, "unsupported_execution"},
		{"content_while_waiting_tools", ErrCursorContentWhileWaitingTools, "content_while_waiting_tools"},
		{"batch_incomplete", ErrCursorBatchIncomplete, "batch_incomplete"},
		{"batch_ambiguous", ErrCursorBatchAmbiguous, "batch_ambiguous"},
		{"protocol_frame", ErrCursorProtocol, "protocol_frame"},
		{"wrapped_protocol_frame", wrapped(ErrCursorProtocol), "protocol_frame"},
		{"unknown", errors.New("unclassified"), "protocol_other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, cursorStreamErrClass(tc.err))
		})
	}
}

func TestFrameTopFieldNumbers(t *testing.T) {
	payload := cursorAppendVarintField(nil, 19, 1)
	payload = cursorAppendStringField(payload, 2, "value")
	payload = cursorAppendBytesField(payload, 7, []byte{1, 2})
	require.Equal(t, []int{2, 7, 19}, frameTopFieldNumbers(payload))
	require.Equal(t, []int{1}, frameTopFieldNumbers(cursorAppendVarintField(nil, 1, 1)))
	require.Nil(t, frameTopFieldNumbers(nil))
	require.Nil(t, frameTopFieldNumbers([]byte{0x0a, 0x05, 0x01}))
}
