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

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestAnthropicCompatStreamTerminationLogCoversConvertedEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, endpoint := range []struct {
		name string
		path string
		mode string
	}{
		{name: "responses", path: "/v1/responses", mode: "forward_as_responses"},
		{name: "chat_completions", path: "/v1/chat/completions", mode: "forward_as_cc"},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "compat-request-123")))
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, endpoint.path, nil).WithContext(ctx)

			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
					"X-Request-Id": []string{"compat-upstream-456"},
				},
				Body: io.NopCloser(strings.NewReader(strings.Join([]string{
					"event: message_start",
					`data: {"type":"message_start","message":{"usage":{"input_tokens":17}}}`,
					"",
					"event: content_block_delta",
					`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"private-stream-body-canary"}}`,
					"",
					"event: message_delta",
					`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":31}}`,
					"",
					"event: message_stop",
					`data: {"type":"message_stop"}`,
					"",
					"",
				}, "\n"))),
			}
			account := &Account{ID: 52, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
			svc := &GatewayService{}
			var err error
			switch endpoint.name {
			case "responses":
				_, err = svc.handleResponsesStreamingResponse(resp, c, account, "claude-test", "claude-test", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
			case "chat_completions":
				_, err = svc.handleCCStreamingFromAnthropic(resp, c, account, "claude-test", "claude-test", nil, time.Now())
			}
			require.NoError(t, err)

			entries := logs.FilterMessage("anthropic_stream_termination").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.Equal(t, endpoint.mode, fields["forwarding_mode"])
			require.Equal(t, endpoint.path, fields["request_path"])
			require.Equal(t, "compat-request-123", fields["request_id"])
			require.Equal(t, "compat-upstream-456", fields["upstream_request_id"])
			require.Equal(t, "upstream_eof_after_terminal_event", fields["termination_reason"])
			require.Equal(t, "max_tokens", fields["stop_reason"])
			require.Equal(t, true, fields["terminal_event_seen"])
			require.Equal(t, true, fields["terminal_event_forwarded"])
			require.EqualValues(t, 31, fields["usage_output_tokens"])
			require.EqualValues(t, len("private-stream-body-canary"), fields["text_output_bytes"])
			for _, entry := range logs.All() {
				require.NotContains(t, entry.Message, "private-stream-body-canary")
				for _, value := range entry.ContextMap() {
					require.NotContains(t, fmt.Sprint(value), "private-stream-body-canary")
				}
			}
		})
	}
}

func TestAnthropicCompatStreamTerminationLogReportsIncompleteAndReadError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, endpoint := range []struct {
		name   string
		path   string
		invoke func(*GatewayService, *http.Response, *gin.Context) error
	}{
		{
			name: "responses",
			path: "/v1/responses",
			invoke: func(svc *GatewayService, resp *http.Response, c *gin.Context) error {
				_, err := svc.handleResponsesStreamingResponse(resp, c, nil, "claude-test", "claude-test", nil, time.Now(), apicompat.ResponsesClientToolMapping{})
				return err
			},
		},
		{
			name: "chat_completions",
			path: "/v1/chat/completions",
			invoke: func(svc *GatewayService, resp *http.Response, c *gin.Context) error {
				_, err := svc.handleCCStreamingFromAnthropic(resp, c, nil, "claude-test", "claude-test", nil, time.Now())
				return err
			},
		},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				body          io.ReadCloser
				wantReason    string
				wantReadError string
			}{
				{
					name:       "missing_terminal",
					body:       io.NopCloser(strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n")),
					wantReason: "eof_missing_terminal_event",
				},
				{
					name:          "unexpected_eof",
					body:          &anthropicStreamReadErrorBody{err: io.ErrUnexpectedEOF},
					wantReason:    "upstream_read_error",
					wantReadError: "unexpected_eof",
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					core, logs := observer.New(zap.InfoLevel)
					ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "compat-failure-123")))
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, endpoint.path, nil).WithContext(ctx)
					resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: tc.body}

					require.Error(t, endpoint.invoke(&GatewayService{}, resp, c))
					entries := logs.FilterMessage("anthropic_stream_termination").All()
					require.Len(t, entries, 1)
					fields := entries[0].ContextMap()
					require.Equal(t, tc.wantReason, fields["termination_reason"])
					wantReadError := tc.wantReadError
					if wantReadError == "" {
						wantReadError = "none"
					}
					require.Equal(t, wantReadError, fields["read_error_category"])
					require.Equal(t, false, fields["terminal_event_seen"])
				})
			}
		})
	}
}
