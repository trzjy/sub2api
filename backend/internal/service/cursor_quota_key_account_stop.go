package service

// 本文件实现「激活码额度耗尽 → 名下账号一起停调」的码级停调域（用户裁定 D9，2026-11-06）。
//
// 为什么要这一层：账号自己的日额度（08:00 重置）和激活码的共享额度池（每天 08:00 与 19:00
// 各恢复一次）是两套完全不同的恢复机制。原来的代码只有账号级停调，激活码额度见底时
// 只是调度侧逐个账号被来源门否决——号还挂在池子里、后台还显示"可用"，实际一个都跑不动。
//
// 设计要点：
//   - 码级停调原因串前缀固定为 cursor quota key exhausted，与账号级的
//     cursor account blocked by upstream（2026-11-06 由 cursor account daily quota exhausted
//     改名，因为后者是误报）严格区分；
//   - 恢复后按前缀精确清除，绝不误清账号级停调（两者到期时刻不同，混清会把还在冷却的号
//     放回池子，让它立刻再撞一次上游）；
//   - 停调目标 = 最近的码级恢复点（19:00 或次日 08:00），不是账号级的 08:05。

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

const (
	// cursorQuotaKeyExhaustedReasonPrefix 是码级停调的固定原因前缀（精确清除的锚点）。
	cursorQuotaKeyExhaustedReasonPrefix = "cursor quota key exhausted"

	// 激活码额度池每天恢复两次：早上 08:00 与晚上 19:00（用户 2026-11-06 提供的运行事实）。
	cursorQuotaKeyResetHourMorning = 8
	cursorQuotaKeyResetHourEvening = 19
)

// cursorQuotaKeyRecoveryUntil 计算"此刻码级额度耗尽"应该停到什么时候。
//
//	00:00–08:00 → 当天 08:00
//	08:00–19:00 → 当天 19:00
//	19:00–24:00 → 次日 08:00
//
// 直接停在恢复整点而不是加宽限：码级额度是整点恢复的确定事件，加宽限只会白白少用几分钟。
// band 只用于日志。
func cursorQuotaKeyRecoveryUntil(now time.Time) (until time.Time, band string) {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	morning := dayStart.Add(cursorQuotaKeyResetHourMorning * time.Hour)
	evening := dayStart.Add(cursorQuotaKeyResetHourEvening * time.Hour)

	switch {
	case now.Before(morning):
		return morning, "before-08:00"
	case now.Before(evening):
		return evening, "before-19:00"
	default:
		return morning.AddDate(0, 0, 1), "after-19:00"
	}
}

// quotaKeyAccountStopSink 是"码级停调"所需的窄面（实现为 *QuotaKeyService）。
//
// 单独声明成接口而不是给 QuotaKeyRepository 加方法：这两个动作只服务本文件的周期扫描，
// 不应把已有仓储窄替身（出站/执行器侧测试替身）逼着实现它们。
type quotaKeyAccountStopSink interface {
	// StopAccountsUnderExhaustedKey 把该码名下"当前没被别的理由停着"的账号一起停到 until，
	// 返回受影响行数。已因账号级额度耗尽停着的号不动（它的到期时刻可能更晚）。
	StopAccountsUnderExhaustedKey(ctx context.Context, quotaKeyID int64, until time.Time) (int64, error)
	// ClearStopsUnderRecoveredKey 精确清除该码名下"由码级原因停着"的账号停调，返回受影响行数。
	ClearStopsUnderRecoveredKey(ctx context.Context, quotaKeyID int64) (int64, error)
}

// StopAccountsUnderExhaustedKey 实现 quotaKeyAccountStopSink。
func (s *QuotaKeyService) StopAccountsUnderExhaustedKey(ctx context.Context, quotaKeyID int64, until time.Time) (int64, error) {
	if s == nil || s.client == nil {
		return 0, fmt.Errorf("quota_key_service: ent client unavailable for key-level account stop")
	}
	now := time.Now()
	reason := fmt.Sprintf("%s until %s", cursorQuotaKeyExhaustedReasonPrefix, until.Format("01-02 15:04"))
	n, err := s.client.Account.Update().
		Where(
			account.QuotaKeyID(quotaKeyID),
			account.DeletedAtIsNil(),
			// 只在"当前没停"或"当前就是码级原因停的"两种情况下写入：
			// 账号级额度耗尽的号到期时刻可能比码级更晚，不能被这条更早的停调覆盖掉。
			account.Or(
				account.TempUnschedulableUntilIsNil(),
				account.TempUnschedulableUntilLT(now),
				account.TempUnschedulableReasonHasPrefix(cursorQuotaKeyExhaustedReasonPrefix),
			),
		).
		SetTempUnschedulableUntil(until).
		SetTempUnschedulableReason(reason).
		Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("quota_key_service: stop accounts under quota key %d: %w", quotaKeyID, err)
	}
	return int64(n), nil
}

// ClearStopsUnderRecoveredKey 实现 quotaKeyAccountStopSink。
//
// 按"码级原因前缀"精确清除该码名下被码级停调了的账号，与写入时并不对称：
//   - 这里【不】加 TempUnschedulableUntilGT 限制。码级额度恢复后，那些 until 已经过期的
//     账号（例如停到 19:00、现在 19:15）也必须被清掉 reason——否则后台会一直显示
//     "cursor quota key exhausted until 19:00" 的旧停调文本，直到下一个恢复点才会被
//     重新判定。一旦恢复判定来了却因为 until 已过期而清不掉，就会永久卡住；
//   - 前缀 cursor quota key exhausted 是全仓唯一的常量、且全仓仅本文件（StopAccountsUnderExhaustedKey）
//     会写入它，所以按前缀清不会误清账号级停调（账号级用的是
//     cursor account blocked by upstream，二者原因串严格区分）；
//   - 写入（StopAccountsUnderExhaustedKey）需要回避"账号级到期更晚"的号以不被覆盖，
//     而清除只需要认前缀、不挑 until——"写入/清除条件不完全对称"是有意的，不是遗漏。
func (s *QuotaKeyService) ClearStopsUnderRecoveredKey(ctx context.Context, quotaKeyID int64) (int64, error) {
	if s == nil || s.client == nil {
		return 0, fmt.Errorf("quota_key_service: ent client unavailable for key-level account stop")
	}
	n, err := s.client.Account.Update().
		Where(
			account.QuotaKeyID(quotaKeyID),
			account.DeletedAtIsNil(),
			account.TempUnschedulableReasonHasPrefix(cursorQuotaKeyExhaustedReasonPrefix),
		).
		ClearTempUnschedulableUntil().
		ClearTempUnschedulableReason().
		Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("quota_key_service: clear key-level stops under quota key %d: %w", quotaKeyID, err)
	}
	return int64(n), nil
}

// SetAccountStopSink 注入码级停调窄面（wire 在构造 reconc 时注入 *QuotaKeyService）。
func (r *QuotaKeyReconciler) SetAccountStopSink(sink quotaKeyAccountStopSink) {
	if r == nil {
		return
	}
	r.accountStopSink = sink
}

// cursorDailyQuotaStopCounter / CountAccountsStoppedForDailyQuota / sampleDailyQuotaRecovery
// 以及 07:55–08:40 的恢复采样窗口，已于 2026-11-06 一并删除。
//
// 原用途：每轮统计"还有多少号因当日额度耗尽停着"，在 07:55–08:40 之间每分钟记一条日志，
// 用掉到 0 的时刻反推上游真实重置点（用户裁定 D14「把时间搞准」）。
//
// 删除理由：被采样的那个前提本身不成立——「今日额度已用完」不是余额判定（见
// cursorQuotaExhaustedCooldown 的三重排他证据），账号级停调现在只有 5 分钟，
// 采样窗口里根本不会有账号停着，继续记只会产出恒为 0 的噪音。
// 真正需要测量的东西已改由 cursor_account_daily_quota_stats 承担（记每个号每天的
// 实际用量与被挡时刻，攒够了再反推口径）。

// sweepQuotaKeyLevelStops 每轮把激活码额度状态同步到它名下账号的停调状态。
//
// 三条硬规则：
//  1. 只认新鲜快照。快照过期时什么都不做——既不停也不清，保持上一轮结论，
//     避免刷新滞后导致账号在"停/放"之间来回弹；
//  2. 只认确定的"额度见底"（观测成功 + 剩余 < 调度下限）。观测失败（密钥无效等）
//     不在这里处理：那不是额度问题，账号停掉也修不好，交给后台显示 key_invalid；
//  3. 恢复后按前缀精确清除，绝不把账号级停调一起清掉。
func (r *QuotaKeyReconciler) sweepQuotaKeyLevelStops(ctx context.Context) {
	if r == nil || r.repo == nil || r.accountStopSink == nil {
		return
	}
	keys, err := r.repo.List(ctx)
	if err != nil {
		log.Printf("[QuotaKeyReconcile] list quota keys for key-level stop sweep failed: %v", err)
		return
	}
	now := timezone.Now()
	for _, qk := range keys {
		if qk == nil || qk.DeletedAt != nil {
			continue
		}
		if r.isStopping() || ctx.Err() != nil {
			return
		}
		snapshot, ok := ParseQuotaKeyUsageSnapshot(qk.UsageSnapshot)
		if !ok || snapshot.ErrorCode != "" || snapshot.Total == nil || snapshot.Used == nil {
			continue // 未观测 / 观测失败 / 字段不全：不下结论，保持现状
		}
		if snapshot.CheckedAt == nil || now.Sub(*snapshot.CheckedAt) > cursorSupplySourceUsageFreshnessWindow {
			continue // 快照过期：不基于旧数据动账号
		}
		if (*snapshot.Total - *snapshot.Used) < cursorQuotaKeyRemainingFloor {
			until, band := cursorQuotaKeyRecoveryUntil(now)
			n, serr := r.accountStopSink.StopAccountsUnderExhaustedKey(ctx, qk.ID, until)
			if serr != nil {
				log.Printf("[QuotaKeyReconcile] key-level stop for quota key %d failed: %v", qk.ID, serr)
				continue
			}
			if n > 0 {
				log.Printf("[QuotaKeyReconcile] key %d quota exhausted (remaining=%d) -> stopped %d account(s) until %s (%s)",
					qk.ID, *snapshot.Total-*snapshot.Used, n, until.Format("15:04"), band)
			}
			continue
		}
		// 额度已恢复到下限之上：把码级原因停着的号精确放回池子。
		n, cerr := r.accountStopSink.ClearStopsUnderRecoveredKey(ctx, qk.ID)
		if cerr != nil {
			log.Printf("[QuotaKeyReconcile] key-level stop clear for quota key %d failed: %v", qk.ID, cerr)
			continue
		}
		if n > 0 {
			log.Printf("[QuotaKeyReconcile] key %d quota recovered (remaining=%d) -> released %d account(s)",
				qk.ID, *snapshot.Total-*snapshot.Used, n)
		}
	}
}
