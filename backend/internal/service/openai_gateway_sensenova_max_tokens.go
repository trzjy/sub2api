package service

import (
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// sensenovaMaxTokensCap 是 sensenova（token.sensenova.cn）对输出 token 数的上游硬上限。
// 上游超限直接 400："field MaxTokens invalid, should be in [1, 65536]"。
// 该上限与模型、平台无关，只按实际上游 base_url 判定，不做模型过滤。
const sensenovaMaxTokensCap int64 = 65536

// clampSensenovaUpstreamMaxTokens 是 chat completions 出站的独立 token 钩子
// （forwardAsRawChatCompletions、/v1/responses 降级与 /v1/messages 降级三条路径共用）。
// 判定只看本次请求实际选用的上游 base_url（GetOpenAIBaseURL，与
// openAIChatCompletionsTargetURL 的取值一致）是否 token.sensenova.cn，
// 不读平台、不读账号名单、不读 extra 配置。只降不升：字段缺失、非数字、
// 值不超过上限时原样返回。
func clampSensenovaUpstreamMaxTokens(account *Account, body []byte) []byte {
	if account == nil || len(body) == 0 {
		return body
	}
	if !isSensenovaBaseURL(account.GetOpenAIBaseURL()) {
		return body
	}
	if !gjson.ValidBytes(body) {
		return body
	}
	clamped := false
	out := body
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		result := gjson.GetBytes(out, key)
		if !result.Exists() || result.Type != gjson.Number || result.Int() <= sensenovaMaxTokensCap {
			continue
		}
		updated, err := sjson.SetBytes(out, key, sensenovaMaxTokensCap)
		if err != nil {
			return body
		}
		out = updated
		clamped = true
	}
	if clamped {
		logger.L().Debug("openai chat_completions raw: clamped max_tokens for sensenova account",
			zap.Int64("account_id", account.ID),
			zap.Int64("cap", sensenovaMaxTokensCap),
		)
	}
	return out
}

// isSensenovaBaseURL 判定 raw 是否指向 sensenova 上游（token.sensenova.cn）。
// 写法与 isOllamaCloudBaseURL 对齐：TrimSpace、含 ?/# 直接拒绝、url.Parse、
// 要求 https scheme、拒绝 userinfo/opaque/query/fragment，hostname 精确等于
// token.sensenova.cn（EqualFold），禁止子串匹配；路径不参与判定（带尾斜杠命中）。
func isSensenovaBaseURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "?#") {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return false
	}
	return strings.EqualFold(parsed.Hostname(), "token.sensenova.cn")
}
