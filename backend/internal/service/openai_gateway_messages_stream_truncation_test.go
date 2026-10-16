//go:build unit

package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIMessagesResponsesStreamTruncationReportsRealUpstreamStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	partial := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_cut","object":"response","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`,
		"",
		"",
	}, "\n")
	readFailure := errors.New("simulated upstream read failure")

	for _, tc := range []struct {
		name              string
		readErr           error
		keepaliveInterval int
		wantCode          string
		wantKind          string
	}{
		{name: "clean EOF", wantCode: OpenAIUpstreamStreamTruncatedCode, wantKind: "stream_missing_terminal"},
		{name: "read error synchronous", readErr: readFailure, wantCode: OpenAIUpstreamStreamReadErrorCode, wantKind: "stream_read_error"},
		{name: "read error with keepalive", readErr: readFailure, keepaliveInterval: 1, wantCode: OpenAIUpstreamStreamReadErrorCode, wantKind: "stream_read_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			var body io.ReadCloser = io.NopCloser(strings.NewReader(partial))
			if tc.readErr != nil {
				body = &errTailReader{data: []byte(partial), err: tc.readErr}
			}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}
			resp.Header.Set("x-request-id", "rid_oauth_cut")
			cfg := rawChatCompletionsTestConfig()
			cfg.Gateway.StreamKeepaliveInterval = tc.keepaliveInterval
			svc := &OpenAIGatewayService{cfg: cfg}

			result, err := svc.handleAnthropicStreamingResponse(resp, c, rawChatCompletionsTestAccount(), "gpt-5.4", "gpt-5.4", "gpt-5.4", time.Now())
			require.Error(t, err)
			require.NotNil(t, result)
			code, _, ok := OpenAIUpstreamStreamReadErrorDetails(err)
			require.True(t, ok)
			require.Equal(t, tc.wantCode, code)
			require.Contains(t, rec.Body.String(), `"text":"partial"`)
			require.NotContains(t, rec.Body.String(), "event: message_stop")
			status, exists := c.Get(OpsUpstreamStatusCodeKey)
			if exists {
				require.NotEqual(t, http.StatusBadGateway, status)
			}
			events, exists := c.Get(OpsUpstreamErrorsKey)
			require.True(t, exists)
			list := events.([]*OpsUpstreamErrorEvent)
			require.Len(t, list, 1)
			require.Equal(t, http.StatusOK, list[0].UpstreamStatusCode)
			require.Equal(t, tc.wantKind, list[0].Kind)
			require.Equal(t, "rid_oauth_cut", list[0].UpstreamRequestID)
		})
	}
}
