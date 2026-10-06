package service

import (
	"context"
	"log/slog"
	"sort"
	"strings"
)

// 存量陈旧记录清理（派发单 D-QL-003D，方案 th-kira-quota-lifecycle §7）：
// 线上 207/208 等账号 extra.model_rate_limits_meta 内残留 tokenharbor_free_tier
// 前缀键（D2 探测链的写入事件元数据），读路径已由 003A/003B/003C/003E 退役，
// 本文件交付存量数据的一次性幂等清理。
//
// 不挂自动执行（不新增调度任务）：管理端一次性操作注册属扩范围。生产调用方式
// 见完工报告——主会话验收后手动执行一次即可（幂等，重复执行零写入）。

const (
	// staleTokenHarborFreeTierPrefix 是要清除的 meta 键前缀：D2 探测链对免费档
	// scope 写入的 model_rate_limits_meta 事件元数据键（scope 本身带模型名，
	// 均以 tokenharbor_free_tier 为前缀）。注意与保留键 tokenharbor_account_level_probe
	// （账号级占位 scope，无此前缀）区分。
	staleTokenHarborFreeTierPrefix = "tokenharbor_free_tier"

	// modelRateLimitsMetaExtraKey 是 extra 下独立于 model_rate_limits 的写入事件
	// 元数据桶键名（repository 层 modelRateLimitsMetaKey 同义；service 包独立定义
	// 以免跨包耦合，同 modelRateLimitsKey 常量先例）。
	modelRateLimitsMetaExtraKey = "model_rate_limits_meta"
)

// CleanStaleTokenHarborFreeTierExtra 幂等清理账号 extra.model_rate_limits_meta 内
// 以 tokenharbor_free_tier 为前缀的陈旧探测记录键（保留 tokenharbor_account_level_probe
// 等其他键），UpdateExtra 回写。
//
// 语义：
//   - 只删前缀命中键，绝不动 meta 桶内其他键，绝不动 extra 其他顶层键；
//   - 空/清理后为空的 meta 桶显式写空 map（显式化「已清理」而非缺失；
//     nil 会让 jsonb 合并语义视作"未更新"）；
//   - 无 meta / 无命中键 → 本账号 no-op（不写库），天然幂等：二次执行零写入；
//   - 清理不可逆、走生产数据：每账号先快照原 extra 到日志（脱敏，只记键名结构
//     与命中键计数，不落任何值内容）再删；
//   - 单账号失败（读/写/账号不存在）中止返回 error，已清理账号保持清理结果
//     （重跑即幂等续跑），返回值为本次实际写入的账号数。
func CleanStaleTokenHarborFreeTierExtra(ctx context.Context, repo AccountRepository, accountIDs []int64) (int, error) {
	cleaned := 0
	for _, accountID := range accountIDs {
		account, err := repo.GetByID(ctx, accountID)
		if err != nil {
			return cleaned, err
		}
		if account == nil || account.Extra == nil {
			continue
		}
		metaRaw, ok := account.Extra[modelRateLimitsMetaExtraKey].(map[string]any)
		if !ok || len(metaRaw) == 0 {
			continue
		}

		// 命中键集合（确定性排序便于快照日志比对）。
		var staleKeys []string
		for key := range metaRaw {
			if strings.HasPrefix(key, staleTokenHarborFreeTierPrefix) {
				staleKeys = append(staleKeys, key)
			}
		}
		if len(staleKeys) == 0 {
			continue
		}
		sort.Strings(staleKeys)

		// 删前脱敏快照：只记键名结构与命中键计数，不落任何值内容。
		snapshotKeys := make([]string, 0, len(metaRaw))
		for key := range metaRaw {
			snapshotKeys = append(snapshotKeys, key)
		}
		sort.Strings(snapshotKeys)
		slog.Info("clean stale tokenharbor_free_tier extra: pre-delete snapshot",
			slog.Int64("account_id", accountID),
			slog.String("extra_key", modelRateLimitsMetaExtraKey),
			slog.Any("meta_keys_before", snapshotKeys),
			slog.Any("stale_keys_to_delete", staleKeys),
			slog.Int("stale_key_count", len(staleKeys)),
		)

		cleanedMeta := make(map[string]any, len(metaRaw)-len(staleKeys))
		for key, value := range metaRaw {
			if strings.HasPrefix(key, staleTokenHarborFreeTierPrefix) {
				continue
			}
			cleanedMeta[key] = value
		}
		if err := repo.UpdateExtra(ctx, accountID, map[string]any{
			modelRateLimitsMetaExtraKey: cleanedMeta,
		}); err != nil {
			return cleaned, err
		}
		cleaned++
	}
	return cleaned, nil
}
