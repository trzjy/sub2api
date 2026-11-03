//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAntigravityConnectionTestModel(t *testing.T) {
	t.Parallel()

	require.Equal(t, "claude-sonnet-4-6", antigravityConnectionTestModel(""))
	require.Equal(t, "gemini-3.1-pro-preview", antigravityConnectionTestModel("gemini-3.1-pro-preview"))
}

func TestAntigravityBuildClaudeTestRequest_RealInboundLink(t *testing.T) {
	t.Parallel()

	// 非图片模型（含 gemini 映射模型）应走真实 Claude 入站转换链路
	// （buildClaudeTestRequest -> TransformClaudeToGeminiWithOptions），
	// 产出 systemInstruction 身份提示词与 contents，而非原生 Gemini 手拼 JSON。
	for _, model := range []string{"gemini-3.8-flash", "claude-sonnet-4-6"} {
		model := model
		t.Run(model, func(t *testing.T) {
			t.Parallel()

			svc := &AntigravityGatewayService{}
			raw, err := svc.buildClaudeTestRequest(context.Background(), "proj-123", model, "")
			require.NoError(t, err)

			var v1Req map[string]any
			require.NoError(t, json.Unmarshal(raw, &v1Req))
			require.Equal(t, "proj-123", v1Req["project"])
			require.Equal(t, model, v1Req["model"])

			inner, ok := v1Req["request"].(map[string]any)
			require.True(t, ok, "expected inner Gemini request")

			// 内容：真实链路转换出的 contents 应存在且非空（含用户输入 "."）
			contents, ok := inner["contents"].([]any)
			require.True(t, ok, "expected contents from Claude->Gemini transform")
			require.NotEmpty(t, contents, "contents should not be empty")
			firstContent := contents[0].(map[string]any)
			require.Equal(t, "user", firstContent["role"])
			parts := firstContent["parts"].([]any)
			require.NotEmpty(t, parts)

			// 身份提示词：经 TransformClaudeToGeminiWithOptions 注入的 systemInstruction
			// 应含 Antigravity 身份补丁（"You are Antigravity"）。
			sysInst, ok := inner["systemInstruction"].(map[string]any)
			require.True(t, ok, "expected systemInstruction from transform (identity patch)")
			sysParts, ok := sysInst["parts"].([]any)
			require.True(t, ok)
			joined := ""
			for _, p := range sysParts {
				if pm, ok := p.(map[string]any); ok {
					if txt, ok := pm["text"].(string); ok {
						joined += txt
					}
				}
			}
			require.Contains(t, joined, "You are Antigravity",
				"systemInstruction must carry the Antigravity identity patch injected by the real inbound link")

			// toolConfig 由 TransformClaudeToGeminiWithOptions 统一注入 VALIDATED 模式
			toolCfg, ok := inner["toolConfig"].(map[string]any)
			require.True(t, ok, "expected toolConfig to be present")
			fcCfg, ok := toolCfg["functionCallingConfig"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "VALIDATED", fcCfg["mode"])
		})
	}
}

func TestAntigravityBuildGeminiTestRequestWithPrompt_ImageModel(t *testing.T) {
	t.Parallel()

	svc := &AntigravityGatewayService{}
	raw, err := svc.buildGeminiTestRequestWithPrompt("proj-123", "gemini-3.1-flash-image", "a cute cat")
	require.NoError(t, err)

	var v1Req map[string]any
	require.NoError(t, json.Unmarshal(raw, &v1Req))
	inner, ok := v1Req["request"].(map[string]any)
	require.True(t, ok)

	genCfg, ok := inner["generationConfig"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []any{"TEXT", "IMAGE"}, genCfg["responseModalities"])

	contents, ok := inner["contents"].([]any)
	require.True(t, ok)
	require.Len(t, contents, 1)
	firstContent := contents[0].(map[string]any)
	parts := firstContent["parts"].([]any)
	require.Equal(t, "a cute cat", parts[0].(map[string]any)["text"])
}

func TestExtractContentFromSSEResponse_ExtractsTextAndInlineImages(t *testing.T) {
	t.Parallel()

	sse := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Here is your image\"},{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":\"iVBORw0KGgo=\"}}]}}]}}\n\n"
	text, images := extractContentFromSSEResponse([]byte(sse))
	require.Equal(t, "Here is your image", text)
	require.Len(t, images, 1)
	require.Equal(t, "image/png", images[0].MimeType)
	require.Equal(t, "data:image/png;base64,iVBORw0KGgo=", images[0].ImageURL)
}

func TestAntigravityShouldUseGeminiTestBuilder_UsesMappedModel(t *testing.T) {
	t.Parallel()

	require.True(t, shouldUseAntigravityGeminiTestRequest("custom-alias", "gemini-2.5-flash"))
	require.True(t, shouldUseAntigravityGeminiTestRequest("gemini-3.1-pro", "gemini-pro-agent"))
	require.False(t, shouldUseAntigravityGeminiTestRequest("gemini-alias", "claude-sonnet-4-6"))
	require.False(t, shouldUseAntigravityGeminiTestRequest("claude-sonnet-4-6", "claude-sonnet-4-6"))
}

// TestAntigravityValidateTestConnectionResponse_EmptyCandidates 校验空响应拦截：
// 上游 200 但 SSE 仅有空 candidates 时，应返回包含「响应为空」的错误。
func TestAntigravityValidateTestConnectionResponse_EmptyCandidates(t *testing.T) {
	t.Parallel()

	cases := []string{
		// 空 candidates 数组
		"data: {\"response\":{\"candidates\":[]}}\n\n",
		// candidates 缺失
		"data: {\"response\":{}}\n\n",
		// 仅有结束标记 / 无 JSON 内容
		"data: [DONE]\n\n",
	}
	for _, sse := range cases {
		_, _, err := validateTestConnectionResponse([]byte(sse))
		require.Error(t, err, "expected error for empty SSE response: %q", sse)
		require.Contains(t, err.Error(), "响应为空",
			"empty-response error must carry the 响应为空 marker: %q", sse)
	}
}

// TestAntigravityValidateTestConnectionResponse_Valid 正向校验：含有效文本不应报错。
func TestAntigravityValidateTestConnectionResponse_Valid(t *testing.T) {
	t.Parallel()

	sse := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}}\n\n"
	text, images, err := validateTestConnectionResponse([]byte(sse))
	require.NoError(t, err)
	require.Equal(t, "hi", text)
	require.Empty(t, images)
}
