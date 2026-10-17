package service

import (
	"net/url"
	"strings"
)

// endpointURLFamily 标识上游 API 的版本段风格，决定版本段如何判定与剥离。
//
// 三族判定差异理由：
//   - OpenAI / Anthropic 共用通用判定（openAIBaseURLHasVersionSuffix）：base path 末段为
//     任一 v+数字[.n][alpha|beta|preview] 即视为版本段（含 /v1、/v4、/v1beta 等）。
//     判定函数零新增（直接复用既有），对 base 带 /v2、/v4 等假想 anthropic 中转形态同样防双段。
//   - Gemini 固定以 /v1beta 结尾才算版本段。原因：gemini 端点必须携带 /v1beta；若用通用判定，
//     当用户误配 /v1 base（聚合平台风格）时会剥掉 endpoint 的 /v1beta 产生 /v1/models（错误路径）。
//     固定 /v1beta 判定下误配 base 产生 /v1/v1beta/models，形态暴露错配，正确性不劣于任何替代。
type endpointURLFamily int

const (
	endpointFamilyOpenAI endpointURLFamily = iota
	endpointFamilyAnthropic
	endpointFamilyGemini
)

// familyVersionSegment 返回该族 endpoint 需剥离的版本段前缀。
// openai / anthropic 族为 "/v1"，gemini 族固定为 "/v1beta"。
func familyVersionSegment(family endpointURLFamily) string {
	if family == endpointFamilyGemini {
		return "/v1beta"
	}
	return "/v1"
}

// familyBaseHasVersionSuffix 判定 base path（已 TrimRight("/")）是否已带本族版本段。
//   - openai / anthropic 族复用既有 openAIBaseURLHasVersionSuffix（通用 v+数字 判定）；
//   - gemini 族固定以 "/v1beta" 结尾才算版本段。
func familyBaseHasVersionSuffix(family endpointURLFamily, path string) bool {
	switch family {
	case endpointFamilyOpenAI, endpointFamilyAnthropic:
		return openAIBaseURLHasVersionSuffix(path)
	case endpointFamilyGemini:
		return strings.HasSuffix(path, "/v1beta")
	default:
		return false
	}
}

// buildVersionedEndpointURL 通用版本段感知 URL 拼接原语。
//
// 逻辑与既有 buildOpenAIEndpointURL 逐字节同构，仅两处按族参数化：
//  1. relative 以本族版本段（openai/anthropic="/v1"，gemini="/v1beta"）剥离 endpoint 前缀；
//  2. base 是否已带版本段由 familyBaseHasVersionSuffix 按族判定。
//
// 继承既有防重逻辑：base path 已以 endpoint 或 relative 结尾时不追加（覆盖 gemini
// /v1beta/models 两级场景与手写版行为对齐）。
func buildVersionedEndpointURL(family endpointURLFamily, base string, endpoint string) string {
	normalized := strings.TrimSpace(base)
	endpoint = "/" + strings.TrimLeft(strings.TrimSpace(endpoint), "/")
	relative := strings.TrimPrefix(endpoint, familyVersionSegment(family))

	parsed, err := url.Parse(normalized)
	if err != nil {
		return strings.TrimRight(normalized, "/") + endpoint
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, endpoint) && !strings.HasSuffix(path, relative) {
		if familyBaseHasVersionSuffix(family, path) {
			path += relative
		} else {
			path += endpoint
		}
	}
	parsed.Path = path
	parsed.RawPath = ""
	parsed.Fragment = ""
	return parsed.String()
}
