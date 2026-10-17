package service

import (
	"errors"
	"fmt"
	"strings"
)

// geminiAIStudioActions 是允许出现在上游 URL 里的 action 集合，与 ForwardNative
// 的入站校验保持一致，同时避免 compat 侧把可变字符串直接拼进 path。
var geminiAIStudioActions = map[string]struct{}{
	"generateContent":       {},
	"streamGenerateContent": {},
	"countTokens":           {},
}

// buildGeminiAIStudioModelActionURL 组装 AI Studio 的
// /v1beta/models/{model}:{action} 上游 URL。
//
// model 是客户端可控的（native 路由取自 URL 片段，compat 路由取自请求体的 model
// 字段，之后可能再经渠道映射），因此必须先过路径片段护栏才能拼进 path，
// 见 upstream_path_guard.go。新增 AI Studio 端点请一律走本函数。
func buildGeminiAIStudioModelActionURL(baseURL, model, action string, stream bool) (string, error) {
	trimmedBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmedBase == "" {
		return "", errors.New("gemini base url is required")
	}
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return "", errors.New("gemini model is required")
	}
	if err := validateUpstreamPathSegment("gemini model", trimmedModel); err != nil {
		return "", err
	}
	trimmedAction := strings.TrimSpace(action)
	if _, ok := geminiAIStudioActions[trimmedAction]; !ok {
		return "", fmt.Errorf("unsupported gemini action: %s", trimmedAction)
	}

	// 版本感知拼接（同源 upstream_endpoint_url.go）：base 已含 /v1beta 时只追加相对路径，
	// 避免裸拼 "/v1beta/models" 造成 /v1beta/v1beta 双段 404。model 已过上方片段护栏、
	// action 已过白名单，二者均为合法路径片段。
	fullURL := buildVersionedEndpointURL(endpointFamilyGemini, trimmedBase, "/v1beta/models/"+trimmedModel+":"+trimmedAction)
	if stream {
		fullURL += "?alt=sse"
	}
	return fullURL, nil
}

// IsSafeGeminiModelPathSegment 供 handler 层在解析出 URL 里的模型名后立刻校验，
// 让客户端拿到明确的 400，而不是等到构造上游请求时才报错。
func IsSafeGeminiModelPathSegment(model string) bool {
	return isSafeUpstreamPathSegment(strings.TrimSpace(model))
}
