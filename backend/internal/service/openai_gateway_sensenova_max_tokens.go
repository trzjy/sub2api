package service

import (
	"context"
	"net/http"
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

// SensenovaContextWindowCap 是商汤 token.sensenova.cn 官方物理上下文窗口上限（256K，262144 tokens）。
// 请求 input tokens 超过此值时上游无法产出完整响应或直接挂死/空流。
const SensenovaContextWindowCap int = 262144

// sensenovaAccountIDs 从候选账号集合中挑出上游 base_url 命中商汤（token.sensenova.cn）的账号 id。
// 纯函数：用构造 Account 即可单测，不连 DB；不改变任何既有选号/调度行为。
func sensenovaAccountIDs(accounts []Account) map[int64]struct{} {
	excluded := make(map[int64]struct{})
	for i := range accounts {
		if isSensenovaBaseURL(accounts[i].GetOpenAIBaseURL()) {
			excluded[accounts[i].ID] = struct{}{}
		}
	}
	return excluded
}

// SensenovaExcludedIDs 列举该分组可调度候选中商汤账号 id 集合（派发单 SENSENOVA-256K-01）。
//
// 语义：复用 OpenAIGatewayService.listSchedulableAccounts 的候选列举，返回
// GetOpenAIBaseURL() 命中 isSensenovaBaseURL 的账号 id。仅供 handler 侧选号前
// 大请求动态排除使用，不改任何既有选号/调度函数签名与行为；列举候选失败返回 error，
// 由调用方失败开放处理。
func (s *OpenAIGatewayService) SensenovaExcludedIDs(ctx context.Context, groupID *int64, platform string) (map[int64]struct{}, error) {
	accounts, err := s.listSchedulableAccounts(ctx, groupID, platform)
	if err != nil {
		return nil, err
	}
	return sensenovaAccountIDs(accounts), nil
}

// accountMaxInputAccountIDs 挑出声明了单请求估算 input 上限、且本次估算超过该上限的账号 id。
// 纯函数，可单测；未声明或声明非法的账号零行为变化。
func accountMaxInputAccountIDs(accounts []Account, estimated int) map[int64]struct{} {
	excluded := make(map[int64]struct{})
	for i := range accounts {
		if max, ok := AccountMaxInputTokensOf(&accounts[i]); ok && estimated > max {
			excluded[accounts[i].ID] = struct{}{}
		}
	}
	return excluded
}

// SizeExcludedIDs 返回本次请求应排除的账号 id 集合，合并两条按大小的规则：
//   - 商汤账号：估算 input > SensenovaContextWindowCap（既有 256K 物理上限语义）
//   - 任意账号：估算 input > 该账号 extra.max_input_tokens（账号自设上限）
//
// 复用 listSchedulableAccounts 的候选列举；列举失败返回 error，由调用方失败开放处理。
func (s *OpenAIGatewayService) SizeExcludedIDs(ctx context.Context, groupID *int64, platform string, estimated int) (map[int64]struct{}, error) {
	accounts, err := s.listSchedulableAccounts(ctx, groupID, platform)
	if err != nil {
		return nil, err
	}
	excluded := accountMaxInputAccountIDs(accounts, estimated)
	if estimated > SensenovaContextWindowCap {
		for id := range sensenovaAccountIDs(accounts) {
			excluded[id] = struct{}{}
		}
	}
	return excluded, nil
}

// SensenovaEmptyStreamReason 标记商汤上游返回合法空流（HTTP 200 但正文 0 字符）。
const SensenovaEmptyStreamReason GatewayFailureReason = "sensenova_empty_stream"

// failoverSensenovaEmptyStream 处理商汤上游返回合法空流的情形：请求被上游承接且
// 响应头正常（HTTP 200），但剥离 reasoning 后可见正文为 0 字符（outputStarted 仍为
// false）。这是商汤逼近 256K 物理上限时的典型表现（reasoning 被网关剥离后正文空）。
// 返回 NextAccountRetry 让 handler 自动把当前商汤账号并入 failedAccountIDs 并换号；
// 不走账号冷却（该账号对更小请求仍健康），Scope 设为请求级瞬时。
func (s *OpenAIGatewayService) failoverSensenovaEmptyStream(ctx context.Context, account *Account, upstreamModel string) *UpstreamFailoverError {
	return &UpstreamFailoverError{
		StatusCode:        http.StatusOK,
		NextAccountAction: NextAccountRetry,
		Scope:             GatewayFailureScopeRequest,
		Reason:            SensenovaEmptyStreamReason,
	}
}
