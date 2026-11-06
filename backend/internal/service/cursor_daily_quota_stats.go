package service

// 本文件是 Cursor 账号每日额度统计的读侧 DTO 与周期汇总入口（迁移见
// 294_cursor_account_daily_quota_stats.sql，读写在 repository/cursor_daily_quota_stats_repo.go）。
//
// 要回答的两个问题（用户裁定 D2，2026-11-06）：
//  ① 每个激活码每天 400 额度，被多少个账号烧光 → CursorQuotaKeyDailyBurn；
//  ② 每个账号每天到底有多少可用额度 → CursorAccountDailyQuotaStat.Capacity*（实测下界）。
//
// 为什么"实测"而不是"查余额"：上游给不出账号级真实余额（中转返回 mock / 无 token 401 /
// 带 token 也 401，三条路都验证过）。唯一可靠的事实是"这个号今天把自己用光了"，
// 那一刻它当天的用量就是它日容量的实测值；攒够多天就能收敛出可信区间。

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

// errCursorDailyStatsUnsupported 表示仓储实现未提供统计能力（装配错误，不静默返回空）。
var errCursorDailyStatsUnsupported = errors.New("quota_key_service: repository does not support cursor daily quota stats")

// CursorAccountDailyQuotaStat 是一账号一天的用量统计行（2026-11-06 从「日额度」改为「日消耗」）。
//
// 语义更正：上游的账号级额度口径尚未确认（「今日额度已用完」被证明不是余额判定），
// 所以本结构记录的是**可观测量**——这个号当天实际干了多少、被挡时已经干了多少、
// 被挡之后是否还能继续干。攒够多天就能反推上限，而不是靠猜。
type CursorAccountDailyQuotaStat struct {
	StatDate   time.Time `json:"stat_date"`
	AccountID  int64     `json:"account_id"`
	QuotaKeyID *int64    `json:"quota_key_id,omitempty"`

	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	ActualCost          float64 `json:"actual_cost"`
	// ModelRequests 当日按模型拆分的请求数，如 {"claude-opus-5-5":3}。
	// 必须按模型拆：opus 与 sonnet 的扣量倍率不同（358 号 3 次 opus 被挡，
	// 355 号 15 次 sonnet 才被挡），混在一起的"上限"永远对不上。
	ModelRequests map[string]int64 `json:"model_requests,omitempty"`

	// Exhausted 当天是否被上游以「今日额度已用完」挡过；ExhaustedAt 是第一次被挡的时刻；
	// BlockedCount 是当天被挡的总次数。
	Exhausted    bool       `json:"exhausted"`
	ExhaustedAt  *time.Time `json:"exhausted_at,omitempty"`
	BlockedCount int        `json:"blocked_count"`

	FirstRequestAt *time.Time `json:"first_request_at,omitempty"`
	LastRequestAt  *time.Time `json:"last_request_at,omitempty"`

	// RequestsAfterBlock / TokensAfterBlock 是第一次被挡之后当天仍然成功的请求数与 token。
	// **> 0 就是"额度并未真正耗尽"的直接铁证**，比任何推断都硬。
	RequestsAfterBlock *int64 `json:"requests_after_block,omitempty"`
	TokensAfterBlock   *int64 `json:"tokens_after_block,omitempty"`

	// Capacity* 是"上游说这句话时，该号当天已经消耗了多少"（被挡之前，不含被挡之后的量）。
	// 只当 Exhausted=true 时有值。它是求上限的观测样本，不是确认的日容量。
	CapacityRequests            *int64   `json:"capacity_requests,omitempty"`
	CapacityInputTokens         *int64   `json:"capacity_input_tokens,omitempty"`
	CapacityOutputTokens        *int64   `json:"capacity_output_tokens,omitempty"`
	CapacityCacheCreationTokens *int64   `json:"capacity_cache_creation_tokens,omitempty"`
	CapacityCacheReadTokens     *int64   `json:"capacity_cache_read_tokens,omitempty"`
	CapacityActualCost          *float64 `json:"capacity_actual_cost,omitempty"`

	QuotaKeyTotal *int `json:"quota_key_total,omitempty"`
	QuotaKeyUsed  *int `json:"quota_key_used,omitempty"`
}

// CursorQuotaKeyDailyBurn 是一码一天的烧号汇总。
//
// 2026-11-06「加全」扩维：原版只回答"几个号把额度烧光了"，但生产数据已证明"烧光了"
// 这个说法本身不可信（30 天内 5 次命中，全部是上游挡人、不是余额归零）。真正的判据是
// **被挡之后还能不能继续跑**：还能跑 → 就是假挡，这个号的额度根本没到顶。
// 所以把账号行上所有可观测维度都汇总到码这一层，让"一个码一天烧掉多少成本"一次看穿。
type CursorQuotaKeyDailyBurn struct {
	StatDate   time.Time `json:"stat_date"`
	QuotaKeyID int64     `json:"quota_key_id"`

	// —— 账号面：这个码当天手里几个号、几个在干活、几个被挡 ——
	TotalAccounts  int64 `json:"total_accounts"`  // 统计表里有行的号数（含当天零请求的）
	ActiveAccounts int64 `json:"active_accounts"` // 当天真跑过请求的号数
	// BlockedAccounts 是被上游以「今日额度已用完」挡过的号数。
	// 注意：它不代表"额度真的用完了"，只代表"上游这么说了一次"，详见 blocked_count 的注释。
	BlockedAccounts int64 `json:"blocked_accounts"`
	// FalseBlockAccounts 是"被挡之后当天仍然成功过"的号数 —— **> 0 就是假挡的直接证据**。
	// 这个维度原版缺失，是"400 分到底要几个号"算不准的主因：把假挡号当成真耗尽号，
	// 会得出"一个号只值几十次请求"的错误结论。
	FalseBlockAccounts int64 `json:"false_block_accounts"`
	// NeverBlockedAccounts 是全程没被挡过的号数，用来对照"挡人"与"真到顶"的比例。
	NeverBlockedAccounts int64 `json:"never_blocked_accounts"`

	// —— 用量面：当天全码合计（本站计费口径，非上游口径） ——
	TotalRequests            int64 `json:"total_requests"`
	TotalInputTokens         int64 `json:"total_input_tokens"`
	TotalOutputTokens        int64 `json:"total_output_tokens"`
	TotalCacheCreationTokens int64 `json:"total_cache_creation_tokens"`
	TotalCacheReadTokens     int64 `json:"total_cache_read_tokens"`
	// TotalTokens 是上面四项之和。单列出来是因为"看成本"的人只想看一个数。
	TotalTokens int64   `json:"total_tokens"`
	TotalCost   float64 `json:"total_cost"`
	// ModelRequests 是当天全码按模型合并后的请求数（多账号的同名模型相加）。
	// opus 与 sonnet 扣量倍率不同，只有拆开才知道"哪一个模型把这个码吃掉了"。
	ModelRequests map[string]int64 `json:"model_requests,omitempty"`

	// —— 被挡面 ——
	TotalBlockedCount       int64 `json:"total_blocked_count"`        // 全码当天被挡的总次数
	TotalRequestsAfterBlock int64 `json:"total_requests_after_block"` // 被挡之后仍然成功的请求合计
	TotalTokensAfterBlock   int64 `json:"total_tokens_after_block"`   // 被挡之后仍然成功的 token 合计

	// —— 上限观测面：当天被挡的号里，烧得最狠的那个烧到了多少 ——
	// 这是求"账号日上限"的实测下界样本：被挡时已经烧掉的量。多个码多天取最大，
	// 才能得到"上限至少有这么高"。没被挡过则为 NULL（不是 0）。
	MaxCapacityRequests *int64   `json:"max_capacity_requests,omitempty"`
	MaxCapacityTokens   *int64   `json:"max_capacity_tokens,omitempty"`
	MaxCapacityCost     *float64 `json:"max_capacity_cost,omitempty"`

	// —— 时间面：这个码当天的活跃区间 ——
	FirstRequestAt *time.Time `json:"first_request_at,omitempty"`
	LastRequestAt  *time.Time `json:"last_request_at,omitempty"`

	// —— 码面：激活码自己的额度快照 ——
	QuotaKeyTotal *int `json:"quota_key_total,omitempty"`
	QuotaKeyUsed  *int `json:"quota_key_used,omitempty"`
}

// cursorDailyQuotaStatsWriter 是"汇总某一天"的窄面（实现为 *repository.quotaKeyRepository）。
// 用类型断言而非扩宽 QuotaKeyRepository：既有测试替身不该被逼着实现统计能力。
type cursorDailyQuotaStatsWriter interface {
	UpsertCursorAccountDailyQuotaStats(ctx context.Context, statDate, dayStart, dayEnd time.Time) (int64, error)
}

// cursorDailyQuotaStatsReader 是读测窄面（列表 + 按码汇总）。
type cursorDailyQuotaStatsReader interface {
	ListCursorAccountDailyQuotaStats(ctx context.Context, from, to time.Time, quotaKeyID *int64) ([]CursorAccountDailyQuotaStat, error)
	SummarizeCursorQuotaKeyDailyBurn(ctx context.Context, from, to time.Time, quotaKeyID *int64) ([]CursorQuotaKeyDailyBurn, error)
}

// cursorDailyStatsRollupInterval 是统计汇总的最小间隔：每小时一次。
//
// 为什么不每轮（5 分钟）都跑：汇总要扫当天的 usage_logs 与 ops_error_logs，是重查询，
// 5 分钟一次对"看日账"没有任何收益。每小时一次足够——统计是给人看趋势的，不是给调度用的。
const cursorDailyStatsRollupInterval = time.Hour

// rollupCursorDailyQuotaStats 周期把"昨天 + 今天"的账号用量事实汇总进统计表。
//
// 每天都重算昨天：跨零点的尾巴（23:59 发起、00:00 落库）会被今天的重算补上；
// 同时也兜住"进程重启错过整点"的情况。UPSERT 幂等，重算不会累加。
func (r *QuotaKeyReconciler) rollupCursorDailyQuotaStats(ctx context.Context) {
	if r == nil || r.repo == nil {
		return
	}
	writer, ok := r.repo.(cursorDailyQuotaStatsWriter)
	if !ok {
		return
	}
	now := timezone.Now()
	if !r.lastDailyStatsRollupAt.IsZero() && now.Sub(r.lastDailyStatsRollupAt) < cursorDailyStatsRollupInterval {
		return
	}
	firstRun := r.lastDailyStatsRollupAt.IsZero()
	r.lastDailyStatsRollupAt = now

	// 首次运行补最近 7 天：不然上线第一天只有"今天+昨天"两行，
	// 老板想看"这周每个码烧了几个号"就得等一周。回填是 UPSERT，重复跑不会翻倍。
	offsets := []int{-1, 0}
	if firstRun {
		offsets = []int{-7, -6, -5, -4, -3, -2, -1, 0}
	}

	for _, dayOffset := range offsets {
		day := now.AddDate(0, 0, dayOffset)
		dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, now.Location())
		dayEnd := dayStart.AddDate(0, 0, 1)
		n, err := writer.UpsertCursorAccountDailyQuotaStats(ctx, dayStart, dayStart, dayEnd)
		if err != nil {
			log.Printf("[CursorDailyQuotaStats] rollup %s failed: %v", dayStart.Format("2006-01-02"), err)
			continue
		}
		log.Printf("[CursorDailyQuotaStats] rollup %s done rows=%d", dayStart.Format("2006-01-02"), n)
	}
}

// ListCursorAccountDailyQuotaStats 供后台查询账号日统计（from/to 为日期，按天闭区间）。
func (s *QuotaKeyService) ListCursorAccountDailyQuotaStats(ctx context.Context, from, to time.Time, quotaKeyID *int64) ([]CursorAccountDailyQuotaStat, error) {
	if s == nil || s.repo == nil {
		return nil, errQuotaKeyUsageRepositoryUnavailable
	}
	reader, ok := s.repo.(cursorDailyQuotaStatsReader)
	if !ok {
		return nil, errCursorDailyStatsUnsupported
	}
	return reader.ListCursorAccountDailyQuotaStats(ctx, from, to, quotaKeyID)
}

// SummarizeCursorQuotaKeyDailyBurn 供后台查"每码每天烧几个号、烧了多少"。
//
// quotaKeyID 非 nil 时只汇总那一个码：与 ListCursorAccountDailyQuotaStats 的口径保持一致，
// 否则同一个请求里的两张表会因为一个过滤一个不过滤而对不上。
func (s *QuotaKeyService) SummarizeCursorQuotaKeyDailyBurn(ctx context.Context, from, to time.Time, quotaKeyID *int64) ([]CursorQuotaKeyDailyBurn, error) {
	if s == nil || s.repo == nil {
		return nil, errQuotaKeyUsageRepositoryUnavailable
	}
	reader, ok := s.repo.(cursorDailyQuotaStatsReader)
	if !ok {
		return nil, errCursorDailyStatsUnsupported
	}
	return reader.SummarizeCursorQuotaKeyDailyBurn(ctx, from, to, quotaKeyID)
}
