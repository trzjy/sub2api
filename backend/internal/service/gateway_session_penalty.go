package service

import (
	"context"
	"log/slog"
	"strings"
)

// PenalizeSessionAccountForCapacityShed clears the sticky session binding and
// down-weights the failed account via a deferred last_used refresh, so that the
// client's immediate retry in the same session is routed to a healthy account.
//
// 2026-10-20 实证：GPT 通道账号 244（官方 Plus OAuth）被 OpenAI 上游账号级间歇
// 降载（流内 response.failed / server_is_overloaded）。同负载换 IP 的 A/B 实验证明
// 降载跟账号不跟 IP；同组账号 231 零降载。中段流失败终止后客户端以同一 session_id
// 立刻重试，若仍钉在 244 则整会话硬失败。清粘性绑定 + LRU 降权可让立刻重试落到健康账号。
//
// 键口径必须复用 openai_sticky_compat.go 的 openAISessionCacheKey /
// openAILegacySessionCacheKey（primary + legacy 双 key），不得新造。
func (s *OpenAIGatewayService) PenalizeSessionAccountForCapacityShed(ctx context.Context, groupID *int64, sessionHash string, accountID int64) {
	// 安全 no-op：nil 服务 / 无效账号 / 空 sessionHash 时绝不触碰 cache。
	if s == nil || accountID <= 0 || strings.TrimSpace(sessionHash) == "" {
		return
	}

	if s.cache != nil {
		// primary 键：openai:<hash>
		if primary := s.openAISessionCacheKey(sessionHash); primary != "" {
			if err := s.cache.DeleteSessionAccountID(ctx, derefGroupID(groupID), primary); err != nil {
				slog.Warn("gateway.session_account_penalize_delete_failed",
					slog.Int64("account_id", accountID),
					slog.Any("group_id", groupID),
					slog.String("session_hash", shortSessionHash(sessionHash)),
					slog.String("reason", "capacity_shed"),
					slog.Any("err", err))
			}
		}
		// legacy 键：仅当 ctx 携带 legacy hash（粘性双写路径）时存在。
		if legacy := s.openAILegacySessionCacheKey(ctx, sessionHash); legacy != "" {
			if err := s.cache.DeleteSessionAccountID(ctx, derefGroupID(groupID), legacy); err != nil {
				slog.Warn("gateway.session_account_penalize_delete_failed",
					slog.Int64("account_id", accountID),
					slog.Any("group_id", groupID),
					slog.String("session_hash", shortSessionHash(sessionHash)),
					slog.String("reason", "capacity_shed"),
					slog.Any("err", err))
			}
		}
	}

	// 失败也刷新 recency，让 LRU 把该账号降权，下一次选号轮到健康账号。
	if s.deferredService != nil {
		s.deferredService.ScheduleLastUsedUpdate(accountID)
	}

	slog.Info("gateway.session_account_penalized",
		slog.Int64("account_id", accountID),
		slog.Any("group_id", groupID),
		slog.String("session_hash", shortSessionHash(sessionHash)),
		slog.String("reason", "capacity_shed"))
}
