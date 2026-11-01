//go:build unit

package service

// S2c：cursor_agent_duplex.go 两处 ErrCursorUnsupportedRequest 产生点改为带码 CursorRequestError
// 的定向测试（方案 §2.2.1 逐点分配表：两处均为 CUR-EXEC-02 / 502）。

import (
	"errors"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestTestCursorDuplexRequestErrorCodes(t *testing.T) {
	unknownExec := cursorAppendVarintField(nil, cursorFldExecMetaID, 99)
	unknownExec = cursorAppendBytesField(unknownExec, cursorFldExecMetaExecID, []byte("exec-99"))
	unknownExec = cursorAppendBytesField(unknownExec, 99, []byte("unknown-exec"))

	cases := []struct {
		name    string
		payload []byte
		code    string
		status  int
		detail  string
	}{
		{
			name:    "exec_unsupported_field",
			payload: cursorExecMetaTestPayload(unknownExec),
			code:    infraerrors.CursorExecUnsupportedCode,
			status:  http.StatusBadGateway,
			detail:  "AgentRun requested unsupported execution",
		},
		{
			name:    "interaction_query_unsupported_field",
			payload: cursorIQTestQuery(5, 12, cursorIQTestArgs("draw")),
			code:    infraerrors.CursorExecUnsupportedCode,
			status:  http.StatusBadGateway,
			detail:  "AgentRun interaction query field 12 not supported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _, cleanup := cursorExecRejectTestDuplex(t)
			defer cleanup()

			_, err := d.respond(tc.payload)
			require.Error(t, err)
			var reqErr *CursorRequestError
			require.True(t, errors.As(err, &reqErr), "got %T: %v", err, err)
			require.Equal(t, tc.code, reqErr.Code)
			require.Equal(t, tc.status, reqErr.Status)
			require.Equal(t, tc.detail, reqErr.Detail)
			require.True(t, errors.Is(err, ErrCursorUnsupportedRequest))
		})
	}
}
