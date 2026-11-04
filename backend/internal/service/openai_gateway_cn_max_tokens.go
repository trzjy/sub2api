package service

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 国产模型上游的 max_tokens 上限夹取（2026-11-04 用户裁定）。
//
// 背景：客户端经常把 max_tokens 传成远超上游上限的值（例如 999999 或 0），上游直接回
// 400 `field MaxTokens invalid, should be in [1, 65536]`，整轮请求作废。生产实测（ops_error_logs，
// 近 7 天）这类 400 共 62 次，全部来自 deepseek-v4.1-flash 系列。
//
// 处置：发上游前把越界值夹到 [1, 65536]，让请求继续跑，而不是把一次可以跑通的生成
// 变成 400。这与 clampOllamaCloudUpstreamMaxTokens / clampSensenovaUpstreamMaxTokens
// 同一范式，只是判定条件从"具体域名"放宽到"国产厂商平台"——这些账号大量走第三方中转，
// 按域名枚举既覆盖不全也维护不动。
//
// 上限取 65536 是这些上游的通用上限（与上游报错话术里的区间一致）。若某个模型上限更小，
// 夹到 65536 与不夹的结果相同（都会被拒），因此本改动不会让任何请求变得更差。
const cnUpstreamMaxTokensCap = 65536

// clampCNUpstreamMaxTokens 把国产厂商账号请求里越界的 max_tokens / max_completion_tokens
// 夹到合法区间。非国产平台、非法 JSON、字段缺失或已是合法值时不改写（返回原字节）。
func clampCNUpstreamMaxTokens(account *Account, body []byte) []byte {
	if account == nil || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	switch account.Platform {
	case PlatformDeepseek, PlatformKimi, PlatformZhipu, PlatformMiniMax:
	default:
		return body
	}
	// 已有更精确口径的上游一律让位，绝不覆盖：
	//   - Ollama Cloud 有自己的账号级上限（extra.ollama_max_tokens_cap，默认 65535）；
	//   - 商汤 token.sensenova.cn 有独立夹取函数；
	//   - 官方 api.deepseek.com 支持更大上下文，夹到 65536 会平白截断合法请求。
	baseURL := account.GetOpenAIBaseURL()
	if isOllamaCloudBaseURL(baseURL) || isSensenovaBaseURL(baseURL) || isDeepSeekOfficialHost(baseURL) {
		return body
	}
	out := body
	clamped := false
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		value := gjson.GetBytes(out, key)
		if !value.Exists() || value.Type != gjson.Number {
			continue
		}
		target := value.Int()
		switch {
		case target > cnUpstreamMaxTokensCap:
			target = cnUpstreamMaxTokensCap
		case target < 1:
			target = 1
		default:
			continue
		}
		updated, err := sjson.SetBytes(out, key, target)
		if err != nil {
			return body
		}
		out = updated
		clamped = true
	}
	if !clamped {
		return body
	}
	logger.L().Debug("openai chat_completions raw: clamped max_tokens for CN provider",
		zap.Int64("account_id", account.ID),
		zap.String("platform", account.Platform),
		zap.Int64("cap", cnUpstreamMaxTokensCap),
	)
	return out
}
