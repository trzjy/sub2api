//go:build unit

package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestMessagesAccountForwardPath(t *testing.T) {
	for _, tt := range []struct {
		name     string
		account  *service.Account
		expected string
	}{
		{"devin", &service.Account{Platform: service.PlatformDevin}, "devin"},
		{"antigravity", &service.Account{Platform: service.PlatformAntigravity}, "antigravity"},
		{"gemini", &service.Account{Platform: service.PlatformGemini}, "gemini-compat"},
		{"cursor", &service.Account{Platform: service.PlatformCursor}, "gemini-compat"},
		// nil 账号按空安全处理：返回 "gemini-compat"，交由既有转发器兜底。
		{"nil account", nil, "gemini-compat"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, messagesAccountForwardPath(tt.account))
		})
	}
}

func TestForwardResultFromOpenAIResult(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		require.Nil(t, forwardResultFromOpenAIResult(nil))
	})

	t.Run("copies every listed field by name", func(t *testing.T) {
		reasoningEffort := "high"
		requestedReasoningEffort := "xhigh"
		serviceTier := "priority"
		firstTokenMs := 123
		headers := http.Header{"X-Request-Id": {"req-1"}}

		in := &service.OpenAIForwardResult{
			RequestID:                     "req-1",
			ResponseID:                    "resp-must-not-leak",
			UpstreamHeaders:               headers,
			Model:                         "claude-sonnet-4-5",
			UpstreamModel:                 "gpt-5",
			UpstreamResponseModel:         "gpt-5-2026-01-01",
			UpstreamResponseModelConflict: true,
			UpstreamResponseServiceTier:   "priority",
			ServiceTier:                   &serviceTier,
			ReasoningEffort:               &reasoningEffort,
			RequestedReasoningEffort:      &requestedReasoningEffort,
			Stream:                        true,
			Duration:                      42 * time.Millisecond,
			FirstTokenMs:                  &firstTokenMs,
			ClientDisconnect:              true,
			Usage: service.OpenAIUsage{
				InputTokens:              100,
				OutputTokens:             20,
				CacheCreationInputTokens: 5,
				CacheReadInputTokens:     7,
				ImageOutputTokens:        3,
			},
		}

		got := forwardResultFromOpenAIResult(in)
		require.NotNil(t, got)

		// 顶层字段按同名直搬。
		require.Equal(t, in.RequestID, got.RequestID)
		require.Equal(t, in.UpstreamHeaders, got.UpstreamHeaders)
		require.Equal(t, in.Model, got.Model)
		require.Equal(t, in.UpstreamModel, got.UpstreamModel)
		require.Equal(t, in.UpstreamResponseModel, got.UpstreamResponseModel)
		require.Equal(t, in.UpstreamResponseModelConflict, got.UpstreamResponseModelConflict)
		require.Equal(t, in.UpstreamResponseServiceTier, got.UpstreamResponseServiceTier)
		require.Equal(t, in.Stream, got.Stream)
		require.Equal(t, in.Duration, got.Duration)
		require.Equal(t, in.FirstTokenMs, got.FirstTokenMs)
		require.Equal(t, in.ClientDisconnect, got.ClientDisconnect)

		// 指针字段保持同一性（不拷贝、不补默认值）。
		require.Same(t, in.ReasoningEffort, got.ReasoningEffort)
		require.Same(t, in.RequestedReasoningEffort, got.RequestedReasoningEffort)
		require.Same(t, in.ServiceTier, got.ServiceTier)
		require.Same(t, in.FirstTokenMs, got.FirstTokenMs)

		// Usage 四个字段按同名直搬，不折算；ClaudeUsage 与 OpenAIUsage
		// 同名的 ImageOutputTokens 也一并搬。
		require.Equal(t, in.Usage.InputTokens, got.Usage.InputTokens)
		require.Equal(t, in.Usage.OutputTokens, got.Usage.OutputTokens)
		require.Equal(t, in.Usage.CacheCreationInputTokens, got.Usage.CacheCreationInputTokens)
		require.Equal(t, in.Usage.CacheReadInputTokens, got.Usage.CacheReadInputTokens)
		require.Equal(t, in.Usage.ImageOutputTokens, got.Usage.ImageOutputTokens)
	})
}
