package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 请求规模分流的判定事实源（2026-11-15）。
//
// 背景：分流阈值（后台配置 request_size_routing_threshold）是硬线，但线画在一把
// 与上游不同源的尺子上 —— 判定用的是本地 tiktoken 估算（openai_gateway_count_tokens.go），
// 各上游真实分词器数出来的 token 数普遍更大。生产实测（11-12~11-15）：
//
//	账号                          估算/上游实际
//	984 讯飞星火 x2.6                  63.3%
//	goaichat 系列（330/335/373/376）    72~75%
//	424                               35.6%
//	442 LongCat-942                    94.0%
//	137 真 DeepSeek                    99.4%
//
// 后果（11-12 起 3 天带 session 的请求）：实际已越过 256k 但估算没越过的「漏判」463 次，
// 是「估算虚高误判」的 33 倍；漏判的长活落在 small_request_only 账号上（984 一家 463 单），
// 而客户感知到的就是「长任务细节丢了」。
//
// 本文件把判定收敛为一处：**同一会话上一单上游回报的真实输入 token 优先于本地估算**。
// 该值本来就随每个请求落库（usage_logs），只是此前没有回到判定路径上。
const (
	// requestSizeLastInputTTL 与粘性会话同寿命（openaiStickySessionTTL = 1h）：
	// 会话粘多久，「上一单真实值」就该记多久，两者必须同步滑动，否则出现
	// 「粘在同一个号上但预判值已过期」的假象。
	requestSizeLastInputTTL = time.Hour
)

type requestSizeSessionHashKey struct{}

// SessionInputTokenCache 是「会话上一单真实输入 token」的缓存能力。
//
// 刻意做成可选能力接口，不并进 GatewayCache 主接口：它是分流预判的加装件，
// 未装配（测试桩、降级缓存实现）时必须自然退化为既有行为（纯本地估算），
// 而不是让整个服务起不来。生产实现见 repository/gateway_cache.go。
type SessionInputTokenCache interface {
	// GetSessionInputTokens 返回会话上一单上游回报的真实输入 token；未记录返回 0。
	GetSessionInputTokens(ctx context.Context, groupID int64, sessionHash string) (int, error)
	// SetSessionInputTokens 记录会话上一单的真实输入 token 并刷新 TTL。
	SetSessionInputTokens(ctx context.Context, groupID int64, sessionHash string, tokens int, ttl time.Duration) error
}

// sessionInputTokenCache 取可选能力；未装配返回 nil（调用方按「没有上一单」处理）。
func (s *OpenAIGatewayService) sessionInputTokenCache() SessionInputTokenCache {
	if s == nil || s.cache == nil {
		return nil
	}
	c, _ := s.cache.(SessionInputTokenCache)
	return c
}

// WithRequestSizeSessionHash 把本请求的会话哈希装入 ctx。
// 目的：让 RecordUsage 在拿到上游真实 token 后，能把它记回该会话。
// 只在 handler 已经算出 sessionHash 的路径上装；未装则记不到，属失败开放。
func WithRequestSizeSessionHash(ctx context.Context, sessionHash string) context.Context {
	if ctx == nil || sessionHash == "" {
		return ctx
	}
	return context.WithValue(ctx, requestSizeSessionHashKey{}, sessionHash)
}

// RequestSizeSessionHash 读取 WithRequestSizeSessionHash 装入的会话哈希。
func RequestSizeSessionHash(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	hash, _ := ctx.Value(requestSizeSessionHashKey{}).(string)
	return hash
}

// resolveRequestSizeRouting 判定本请求属于大活池还是小活池。
//
// 入参：
//   - estimated：本地 tiktoken 估算的输入 token（既有唯一估算来源）
//   - lastRealTokens / hasLastReal：同一会话上一单上游回报的真实输入 token（含缓存读写）
//   - threshold：本轮判定用的大活阈值（由调用方按后台配置注入；
//     传 RequestSizeRoutingThresholdUnknown(0) 表示「阈值不可知」）
//
// 规则：
//  1. 上一单真实值是本轮的下界。上下文在同一会话内只会增长，本地估算又系统性偏低，
//     两者取大等于用「已经确认过的真实值」兜住估算的下限 —— 这是根因修复。
//  2. 阈值不可知（threshold<=0）时不判定大小：返回 (effective, false)。
//     调用方必须同时跳过按规模的账号排除（不得据此缩小候选池），
//     否则「不可知」会被误当成「小请求」，把只接大请求的账号全部踢出候选。
//
// 返回：用于对账落库的 effective 估算值，以及是否按大活处理。
func ResolveRequestSizeRouting(estimated, lastRealTokens int, hasLastReal bool, threshold int) (int, bool) {
	effective := estimated
	if hasLastReal && lastRealTokens > effective {
		effective = lastRealTokens
	}
	if threshold <= 0 {
		// 阈值不可知：不做大小判定，由调用方负责跳过规模排除。
		return effective, false
	}
	// 大活判定只看 effective（已含上一单真实值下界）。
	// 上下文在同一会话内只增不减，用真实值兜住估算下限后，阈值线附近的抖动
	// 不会再造成来回换号（换号 = prompt cache 全废），无需额外滞回下沿。
	return effective, effective > threshold
}

// rememberSessionInputTokens 把本单上游回报的真实输入 token 记到会话上。
// 失败只告警不阻断：这是预判用的旁路信息，读不到就退回纯本地估算（既有行为）。
func (s *OpenAIGatewayService) rememberSessionInputTokens(ctx context.Context, groupID *int64, sessionHash string, tokens int) {
	cache := s.sessionInputTokenCache()
	if cache == nil || sessionHash == "" || tokens <= 0 {
		return
	}
	if err := cache.SetSessionInputTokens(ctx, derefGroupID(groupID), sessionHash, tokens, requestSizeLastInputTTL); err != nil {
		logger.LegacyPrintf("service.openai_gateway",
			"[RequestSize] 会话输入 token 记录失败（该会话退回纯本地估算） session_hash_len=%d tokens=%d err=%v",
			len(sessionHash), tokens, err)
	}
}

// lastSessionInputTokens 读同一会话上一单的真实输入 token。未记录/读失败一律按「没有」处理。
func (s *OpenAIGatewayService) LastSessionInputTokens(ctx context.Context, groupID *int64, sessionHash string) (int, bool) {
	cache := s.sessionInputTokenCache()
	if cache == nil || sessionHash == "" {
		return 0, false
	}
	tokens, err := cache.GetSessionInputTokens(ctx, derefGroupID(groupID), sessionHash)
	if err != nil || tokens <= 0 {
		return 0, false
	}
	return tokens, true
}
