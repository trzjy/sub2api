package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUpstreamEndpointURL 是通用原语 buildVersionedEndpointURL 的版本段矩阵单测。
// 覆盖三族 × base 形态矩阵、双段硬断言，以及方案审补充的三组边界用例。
//
// 风格对齐 openai_endpoint_url_test.go（table-driven，子测试名 kebab）。
func TestUpstreamEndpointURL(t *testing.T) {
	tests := []struct {
		name     string
		family   endpointURLFamily
		base     string
		endpoint string
		want     string
	}{
		// ---- OpenAI 族（版本段 /v1）----
		{name: "openai-bare", family: endpointFamilyOpenAI, base: "https://upstream.example", endpoint: "/v1/chat/completions", want: "https://upstream.example/v1/chat/completions"},
		{name: "openai-trailing-slash", family: endpointFamilyOpenAI, base: "https://upstream.example/", endpoint: "/v1/chat/completions", want: "https://upstream.example/v1/chat/completions"},
		{name: "openai-with-v1", family: endpointFamilyOpenAI, base: "https://upstream.example/v1", endpoint: "/v1/chat/completions", want: "https://upstream.example/v1/chat/completions"},
		{name: "openai-with-v4", family: endpointFamilyOpenAI, base: "https://upstream.example/v4", endpoint: "/v1/chat/completions", want: "https://upstream.example/v4/chat/completions"},
		{name: "openai-with-v1beta", family: endpointFamilyOpenAI, base: "https://upstream.example/v1beta", endpoint: "/v1/chat/completions", want: "https://upstream.example/v1beta/chat/completions"},
		{name: "openai-with-subpath-api", family: endpointFamilyOpenAI, base: "https://upstream.example/api", endpoint: "/v1/chat/completions", want: "https://upstream.example/api/v1/chat/completions"},
		{name: "openai-with-subpath-openai-v2", family: endpointFamilyOpenAI, base: "https://upstream.example/openai/v2", endpoint: "/v1/chat/completions", want: "https://upstream.example/openai/v2/chat/completions"},
		{name: "openai-already-complete", family: endpointFamilyOpenAI, base: "https://upstream.example/v1/chat/completions", endpoint: "/v1/chat/completions", want: "https://upstream.example/v1/chat/completions"},
		{name: "openai-parse-fallback", family: endpointFamilyOpenAI, base: "ht tp://upstream.example", endpoint: "/v1/chat/completions", want: "ht tp://upstream.example/v1/chat/completions"},

		// ---- Anthropic 族（版本段 /v1，沿用通用判定）----
		{name: "anthropic-bare", family: endpointFamilyAnthropic, base: "https://upstream.example", endpoint: "/v1/messages", want: "https://upstream.example/v1/messages"},
		{name: "anthropic-trailing-slash", family: endpointFamilyAnthropic, base: "https://upstream.example/", endpoint: "/v1/messages", want: "https://upstream.example/v1/messages"},
		{name: "anthropic-with-v1", family: endpointFamilyAnthropic, base: "https://upstream.example/v1", endpoint: "/v1/messages", want: "https://upstream.example/v1/messages"},
		{name: "anthropic-with-v4", family: endpointFamilyAnthropic, base: "https://upstream.example/v4", endpoint: "/v1/messages", want: "https://upstream.example/v4/messages"},
		{name: "anthropic-with-v1beta", family: endpointFamilyAnthropic, base: "https://upstream.example/v1beta", endpoint: "/v1/messages", want: "https://upstream.example/v1beta/messages"},
		{name: "anthropic-with-subpath-api", family: endpointFamilyAnthropic, base: "https://upstream.example/api", endpoint: "/v1/messages", want: "https://upstream.example/api/v1/messages"},
		{name: "anthropic-with-subpath-openai-v2", family: endpointFamilyAnthropic, base: "https://upstream.example/openai/v2", endpoint: "/v1/messages", want: "https://upstream.example/openai/v2/messages"},
		{name: "anthropic-already-complete", family: endpointFamilyAnthropic, base: "https://upstream.example/v1/messages", endpoint: "/v1/messages", want: "https://upstream.example/v1/messages"},
		{name: "anthropic-parse-fallback", family: endpointFamilyAnthropic, base: "ht tp://upstream.example", endpoint: "/v1/messages", want: "ht tp://upstream.example/v1/messages"},

		// ---- Gemini 族（版本段固定 /v1beta）----
		{name: "gemini-bare", family: endpointFamilyGemini, base: "https://generativelanguage.googleapis.com", endpoint: "/v1beta/models", want: "https://generativelanguage.googleapis.com/v1beta/models"},
		{name: "gemini-trailing-slash", family: endpointFamilyGemini, base: "https://generativelanguage.googleapis.com/", endpoint: "/v1beta/models", want: "https://generativelanguage.googleapis.com/v1beta/models"},
		// 误配 /v1 base 时固定判定产生 /v1/v1beta/models，形态暴露错配（方案 §4.1 锁定的预期）。
		{name: "gemini-with-v1", family: endpointFamilyGemini, base: "https://generativelanguage.googleapis.com/v1", endpoint: "/v1beta/models", want: "https://generativelanguage.googleapis.com/v1/v1beta/models"},
		{name: "gemini-with-v4", family: endpointFamilyGemini, base: "https://generativelanguage.googleapis.com/v4", endpoint: "/v1beta/models", want: "https://generativelanguage.googleapis.com/v4/v1beta/models"},
		{name: "gemini-with-v1beta", family: endpointFamilyGemini, base: "https://generativelanguage.googleapis.com/v1beta", endpoint: "/v1beta/models", want: "https://generativelanguage.googleapis.com/v1beta/models"},
		{name: "gemini-with-subpath-api", family: endpointFamilyGemini, base: "https://generativelanguage.googleapis.com/api", endpoint: "/v1beta/models", want: "https://generativelanguage.googleapis.com/api/v1beta/models"},
		{name: "gemini-with-subpath-openai-v2", family: endpointFamilyGemini, base: "https://generativelanguage.googleapis.com/openai/v2", endpoint: "/v1beta/models", want: "https://generativelanguage.googleapis.com/openai/v2/v1beta/models"},
		{name: "gemini-already-complete", family: endpointFamilyGemini, base: "https://generativelanguage.googleapis.com/v1beta/models", endpoint: "/v1beta/models", want: "https://generativelanguage.googleapis.com/v1beta/models"},
		{name: "gemini-parse-fallback", family: endpointFamilyGemini, base: "ht tp://generativelanguage.googleapis.com", endpoint: "/v1beta/models", want: "ht tp://generativelanguage.googleapis.com/v1beta/models"},

		// ---- 方案审补充①：query-bearing base（非受支持输入形态）----
		// 断言：query 保留、endpoint 追加到 path（继承既有 openai helper 语义）。
		// 契约明示：query-bearing base 非受支持输入形态，anthropic 调用方 + "?beta=true"
		// 字符串拼接在此形态下结构未定义。
		{name: "query-bearing-openai", family: endpointFamilyOpenAI, base: "https://upstream.example/v1?redirect=/", endpoint: "/v1/chat/completions", want: "https://upstream.example/v1/chat/completions?redirect=/"},
		{name: "query-bearing-anthropic", family: endpointFamilyAnthropic, base: "https://upstream.example/v1?redirect=/", endpoint: "/v1/messages", want: "https://upstream.example/v1/messages?redirect=/"},
		{name: "query-bearing-gemini", family: endpointFamilyGemini, base: "https://upstream.example/v1?redirect=/", endpoint: "/v1beta/models", want: "https://upstream.example/v1/v1beta/models?redirect=/"},

		// ---- 方案审补充②：relative suffix 误命中边界 ----
		// 继承既有 HasSuffix helper 语义：命中即不追加，勿凭直觉改逻辑。
		{name: "relative-suffix-gemini", family: endpointFamilyGemini, base: "https://host/some-prefix/models", endpoint: "/v1beta/models", want: "https://host/some-prefix/models"},
		{name: "relative-suffix-openai", family: endpointFamilyOpenAI, base: "https://host/some-prefix/chat/completions", endpoint: "/v1/chat/completions", want: "https://host/some-prefix/chat/completions"},

		// ---- 方案审补充③：base 尾段带额外子路径 ----
		{name: "base-extra-subpath-gemini", family: endpointFamilyGemini, base: "https://host/v1beta/other/models", endpoint: "/v1beta/models", want: "https://host/v1beta/other/models"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildVersionedEndpointURL(tt.family, tt.base, tt.endpoint)

			// 双段硬断言：本族结果不得出现版本段自我重复。
			if tt.family == endpointFamilyGemini {
				require.NotContains(t, got, "/v1beta/v1beta", "gemini 族不得出现 /v1beta/v1beta 双段")
			} else {
				require.NotContains(t, got, "/v1/v1", "openai/anthropic 族不得出现 /v1/v1 双段")
			}

			require.Equal(t, tt.want, got)
		})
	}
}

// TestBuildGeminiModelsURLPrimitiveEquivalence 锁定 G3 收敛：buildGeminiModelsURL 经原语产出的
// 三形态（裸域 / 带 /v1beta / 已含完整端点）与手写版逐字节一致。
func TestBuildGeminiModelsURLPrimitiveEquivalence(t *testing.T) {
	require.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models", buildGeminiModelsURL("https://generativelanguage.googleapis.com"))
	require.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models", buildGeminiModelsURL("https://generativelanguage.googleapis.com/v1beta"))
	require.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models", buildGeminiModelsURL("https://generativelanguage.googleapis.com/v1beta/models"))
}
