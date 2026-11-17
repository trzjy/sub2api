//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func rawJSON(t *testing.T, v string) json.RawMessage {
	t.Helper()
	return json.RawMessage(v)
}

// TestValidateDevinAnthropicImages 覆盖 devin /v1/messages 入口的图片前置校验：
// system 图片与非 base64（远程 URL）图片必须失败关闭，合法 base64 图片放行。
func TestValidateDevinAnthropicImages(t *testing.T) {
	t.Run("system array with image block is rejected", func(t *testing.T) {
		req := &apicompat.AnthropicRequest{
			System: rawJSON(t, `[{"type":"text","text":"sys"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]`),
			Messages: []apicompat.AnthropicMessage{
				{Role: "user", Content: rawJSON(t, `"hello"`)},
			},
		}
		require.ErrorIs(t, validateDevinAnthropicImages(req), ErrDevinSystemImageUnsupported)
	})

	t.Run("user message with url image is rejected", func(t *testing.T) {
		req := &apicompat.AnthropicRequest{
			Messages: []apicompat.AnthropicMessage{
				{Role: "user", Content: rawJSON(t, `[{"type":"text","text":"look"},{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}]`)},
			},
		}
		require.ErrorIs(t, validateDevinAnthropicImages(req), ErrDevinRemoteImageUnsupported)
	})

	t.Run("tool_result with embedded url image is rejected", func(t *testing.T) {
		req := &apicompat.AnthropicRequest{
			Messages: []apicompat.AnthropicMessage{
				{Role: "user", Content: rawJSON(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"image","source":{"type":"url","url":"https://example.com/y.png"}}]}]`)},
			},
		}
		require.ErrorIs(t, validateDevinAnthropicImages(req), ErrDevinRemoteImageUnsupported)
	})

	t.Run("all base64 images pass", func(t *testing.T) {
		img := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}`
		req := &apicompat.AnthropicRequest{
			Messages: []apicompat.AnthropicMessage{
				{Role: "user", Content: rawJSON(t, `[{"type":"text","text":"look"},`+img+`]`)},
				{Role: "user", Content: rawJSON(t, `[{"type":"tool_result","tool_use_id":"toolu_1","content":[`+img+`]}]`)},
			},
		}
		require.NoError(t, validateDevinAnthropicImages(req))
	})

	t.Run("no images and string system pass", func(t *testing.T) {
		req := &apicompat.AnthropicRequest{
			System: rawJSON(t, `"you are helpful"`),
			Messages: []apicompat.AnthropicMessage{
				{Role: "user", Content: rawJSON(t, `[{"type":"text","text":"hello"}]`)},
			},
		}
		require.NoError(t, validateDevinAnthropicImages(req))
	})

	t.Run("string content passes", func(t *testing.T) {
		req := &apicompat.AnthropicRequest{
			Messages: []apicompat.AnthropicMessage{
				{Role: "user", Content: rawJSON(t, `"just a string"`)},
			},
		}
		require.NoError(t, validateDevinAnthropicImages(req))
	})
}
