package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildGeminiAIStudioModelActionURL(t *testing.T) {
	const base = "https://generativelanguage.googleapis.com"

	got, err := buildGeminiAIStudioModelActionURL(base, "gemini-2.5-pro", "generateContent", false)
	require.NoError(t, err)
	require.Equal(t, base+"/v1beta/models/gemini-2.5-pro:generateContent", got)

	got, err = buildGeminiAIStudioModelActionURL(base+"/", " gemini-2.5-flash ", "streamGenerateContent", true)
	require.NoError(t, err)
	require.Equal(t, base+"/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", got)

	got, err = buildGeminiAIStudioModelActionURL(base, "gemini-2.5-pro", "countTokens", false)
	require.NoError(t, err)
	require.Equal(t, base+"/v1beta/models/gemini-2.5-pro:countTokens", got)
}

// TestBuildGeminiAIStudioModelActionURLVersionSegment 锁定 G1 收敛：base 自带 /v1beta
// 版本段时只追加相对路径，不得出现 /v1beta/v1beta 双段。
func TestBuildGeminiAIStudioModelActionURLVersionSegment(t *testing.T) {
	const host = "https://generativelanguage.googleapis.com"

	got, err := buildGeminiAIStudioModelActionURL(host+"/v1beta", "gemini-2.5-pro", "generateContent", false)
	require.NoError(t, err)
	require.Equal(t, host+"/v1beta/models/gemini-2.5-pro:generateContent", got)
	require.NotContains(t, got, "/v1beta/v1beta")

	got, err = buildGeminiAIStudioModelActionURL(host+"/v1beta/", "gemini-2.5-flash", "streamGenerateContent", true)
	require.NoError(t, err)
	require.Equal(t, host+"/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", got)
	require.NotContains(t, got, "/v1beta/v1beta")

	// 反向用例：非 /v1beta 版本段（误配 /v1）按 Gemini 固定判定不做剥离，形态暴露错配。
	got, err = buildGeminiAIStudioModelActionURL(host+"/v1", "gemini-2.5-pro", "countTokens", false)
	require.NoError(t, err)
	require.Equal(t, host+"/v1/v1beta/models/gemini-2.5-pro:countTokens", got)
}

// TestBuildGeminiAIStudioModelActionURLRejectsNonConformingModel 锁定不变式：
// 模型名来自客户端（native 路由的 URL 片段 / compat 路由的请求体），
// 只有合规的路径片段才允许拼进上游 URL。
func TestBuildGeminiAIStudioModelActionURLRejectsNonConformingModel(t *testing.T) {
	const base = "https://generativelanguage.googleapis.com"

	for _, model := range []string{
		"../../x/y",
		"..",
		".",
		"gemini-2.5-pro/../../x",
		`..\..\x`,
		"gemini-2.5-pro?a=b",
		"gemini-2.5-pro#frag",
		"gemini-2.5-pro%2f..",
		"gemini 2.5 pro",
		"gemini\x00pro",
		"gemini-2.5-pro@001",
		"gemini~pro",
		"models/gemini-2.5-pro",
		"...",
		"",
		"   ",
	} {
		t.Run("model_"+model, func(t *testing.T) {
			_, err := buildGeminiAIStudioModelActionURL(base, model, "generateContent", false)
			require.Error(t, err, "model %q must be rejected", model)
			require.False(t, IsSafeGeminiModelPathSegment(model))
		})
	}

	// action 只允许已知取值，避免未来把可变字符串拼进 path。
	_, err := buildGeminiAIStudioModelActionURL(base, "gemini-2.5-pro", "deleteModel", false)
	require.Error(t, err)

	_, err = buildGeminiAIStudioModelActionURL("", "gemini-2.5-pro", "generateContent", false)
	require.Error(t, err)
}
