package service

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestHasOpenAIInputImage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		// ---- chat_completions: messages[].content[].type == "image_url" ----
		{
			name: "chat simple image_url part",
			body: `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`,
			want: true,
		},
		{
			name: "chat image_url with string content variant",
			body: `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"},{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/a.png"}]}]}`,
			want: true,
		},
		{
			name: "chat nested content array inside tool result",
			body: `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"go"},{"role":"tool","tool_call_id":"call_1","content":[{"type":"text","text":"rendered"},{"type":"image_url","image_url":{"url":"https://example.com/tool-out.png"}}]}]}`,
			want: true,
		},
		{
			name: "chat tool result image in deep array under system",
			body: `{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":[{"type":"text","text":"ref"},{"type":"image_url","image_url":{"url":"https://example.com/ref.png"}}]}]}`,
			want: true,
		},
		// ---- responses: input[].content[].type == "input_image" ----
		{
			name: "responses input_image in message content",
			body: `{"model":"deepseek-v4.1-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`,
			want: true,
		},
		{
			name: "responses input_image in function_call_output output",
			body: `{"model":"deepseek-v4.1-flash","input":[{"type":"function_call_output","call_id":"call_A","output":[{"type":"input_image","image_url":{"url":"https://example.com/a.png"}}]}]}`,
			want: true,
		},
		// ---- anthropic: content[].type == "image" ----
		{
			name: "anthropic image block",
			body: `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}`,
			want: true,
		},
		// ---- negatives ----
		{
			name: "plain text chat",
			body: `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}]}`,
			want: false,
		},
		{
			name: "empty messages",
			body: `{"model":"deepseek-v4.1-flash","messages":[]}`,
			want: false,
		},
		{
			name: "responses plain text",
			body: `{"model":"deepseek-v4.1-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			want: false,
		},
		{
			name: "tool schema mentions image_url property but not an image block",
			body: `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"inspect_image","parameters":{"properties":{"image_url":{"type":"string"}}}}}]}`,
			want: false,
		},
		{
			name: "invalid json",
			body: `{"model":`,
			want: false,
		},
		{
			name: "empty body",
			body: ``,
			want: false,
		},
		// ---- edge: "image" type in unrelated place should still match only block types ----
		{
			name: "type text only",
			body: `{"model":"deepseek-v4.1-flash","input":[{"type":"message","role":"user","content":[{"type":"text","text":"x"}]}]}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasOpenAIInputImage([]byte(tt.body))
			require.Equal(t, tt.want, got, "body=%s", tt.body)
		})
	}
}

func TestOpenAIHasImageInputHintSetGet(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("set true then get", func(t *testing.T) {
		c := &gin.Context{}
		SetOpenAIHasImageInputHint(c, true)
		got, known := GetOpenAIHasImageInputHint(c)
		require.True(t, got)
		require.True(t, known)
	})

	t.Run("set false then get", func(t *testing.T) {
		c := &gin.Context{}
		SetOpenAIHasImageInputHint(c, false)
		got, known := GetOpenAIHasImageInputHint(c)
		require.False(t, got)
		require.True(t, known)
	})

	t.Run("missing is unknown", func(t *testing.T) {
		c := &gin.Context{}
		got, known := GetOpenAIHasImageInputHint(c)
		require.False(t, got)
		require.False(t, known)
	})

	t.Run("nil context is no-op", func(t *testing.T) {
		SetOpenAIHasImageInputHint(nil, true)
		got, known := GetOpenAIHasImageInputHint(nil)
		require.False(t, got)
		require.False(t, known)
	})
}