package repository

// 本文件实现 Cursor 账号每日额度实测统计的读写（迁移见 294_cursor_account_daily_quota_stats.sql）。
//
// 为什么单独一个文件：它是"统计"而不是"调度状态"，写入口只有周期汇总一处、读入口只有后台
// 查询一处，跟激活码的拉号/停调状态机没有任何耦合。混进 quota_key_repository 只会让那个
// 已经很大的文件更难改。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// cursorDailyStatsUpsertSQL 把某一天的事实汇总进统计表（幂等 UPSERT）。
//
// 口径说明（重要，改之前先读）：
//   - 只统计 platform='cursor' 的账号；账号即使已软删也照统计——它当天确实烧过额度，
//     删账号不能抹掉历史账；
//   - requests / tokens / actual_cost 来自 usage_logs（本站计费链路，不是上游口径）,
//     token 现在含 cache_creation / cache_read：生产样本里缓存占比很大，漏掉会显著低估消耗；
//   - model_requests 按模型拆分请求数（jsonb），因为 opus 与 sonnet 的扣量倍率不同，
//     不拆开的"上限"永远对不上；
//   - exhausted 只认 ops_error_logs 里出现「今日额度已用完」的记录。不用 accounts.extra
//     里的标记，因为那是"最后一次"的覆盖值，没有历史；
//   - **capacity_\* 取的是"被挡那一刻之前"的累计用量，不是当天全天用量**。全天用量会把
//     被挡之后仍然成功的请求算进来，把上限抬高（2026-11-06 修正）；
//   - requests_after_block / tokens_after_block 度量"被挡之后当天还成功了多少"：
//     > 0 就是"额度并未真正耗尽、上游只是换了句话挡回来"的直接证据；
//   - quota_key_total / quota_key_used 取 quota_keys.usage_snapshot 当前的 total/used，
//     用来把码的额度分摊到账号上。上游没给值时保持 NULL（缺失不是 0）。
const cursorDailyStatsUpsertSQL = `
WITH ex AS (
    SELECT account_id,
           min(created_at) AS first_block_at,
           count(*)        AS blocked_count
      FROM ops_error_logs
     WHERE created_at >= $2 AND created_at < $3
       AND (error_message LIKE '%今日额度已用完%'
            OR upstream_error_message LIKE '%今日额度已用完%')
     GROUP BY account_id
),
us AS (
    SELECT account_id,
           count(*)                       AS requests,
           sum(input_tokens)              AS input_tokens,
           sum(output_tokens)             AS output_tokens,
           sum(cache_creation_tokens)     AS cache_creation_tokens,
           sum(cache_read_tokens)         AS cache_read_tokens,
           sum(actual_cost)               AS actual_cost,
           min(created_at)                AS first_at,
           max(created_at)                AS last_at
      FROM usage_logs
     WHERE created_at >= $2 AND created_at < $3
     GROUP BY account_id
),
ub AS (
    SELECT ul.account_id,
           count(*)                       AS requests,
           sum(ul.input_tokens)           AS input_tokens,
           sum(ul.output_tokens)          AS output_tokens,
           sum(ul.cache_creation_tokens)  AS cache_creation_tokens,
           sum(ul.cache_read_tokens)      AS cache_read_tokens,
           sum(ul.actual_cost)            AS actual_cost
      FROM usage_logs ul
      JOIN ex ON ex.account_id = ul.account_id
     WHERE ul.created_at >= $2 AND ul.created_at < $3
       AND ul.created_at < ex.first_block_at
     GROUP BY ul.account_id
),
ua AS (
    SELECT ul.account_id,
           count(*) AS requests,
           sum(ul.input_tokens + ul.output_tokens
               + ul.cache_creation_tokens + ul.cache_read_tokens) AS tokens
      FROM usage_logs ul
      JOIN ex ON ex.account_id = ul.account_id
     WHERE ul.created_at >= $2 AND ul.created_at < $3
       AND ul.created_at >= ex.first_block_at
     GROUP BY ul.account_id
),
mo AS (
    SELECT account_id, jsonb_object_agg(model, cnt) AS model_requests
      FROM (
        SELECT account_id, model, count(*) AS cnt
          FROM usage_logs
         WHERE created_at >= $2 AND created_at < $3
         GROUP BY account_id, model
      ) t
     GROUP BY account_id
)
INSERT INTO cursor_account_daily_quota_stats (
    stat_date, account_id, quota_key_id,
    requests, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
    actual_cost, model_requests,
    exhausted, exhausted_at, blocked_count,
    first_request_at, last_request_at,
    requests_after_block, tokens_after_block,
    capacity_requests, capacity_input_tokens, capacity_output_tokens,
    capacity_cache_creation_tokens, capacity_cache_read_tokens, capacity_actual_cost,
    quota_key_total, quota_key_used, updated_at
)
SELECT
    $1::date,
    a.id,
    a.quota_key_id,
    COALESCE(us.requests, 0),
    COALESCE(us.input_tokens, 0),
    COALESCE(us.output_tokens, 0),
    COALESCE(us.cache_creation_tokens, 0),
    COALESCE(us.cache_read_tokens, 0),
    COALESCE(us.actual_cost, 0),
    mo.model_requests,
    (ex.first_block_at IS NOT NULL),
    ex.first_block_at,
    COALESCE(ex.blocked_count, 0),
    us.first_at,
    us.last_at,
    -- 被挡之后当天仍成功的量：被挡了就一定填（0 表示"之后一次都没成功"），
    -- 没被挡才留 NULL。二者必须能区分——0 是"挡了之后确实没再成功过"这个事实，
    -- 而 NULL 是"根本没发生过被挡"，混成同一个值会让统计口径失真。
    CASE WHEN ex.first_block_at IS NOT NULL THEN COALESCE(ua.requests, 0) END,
    CASE WHEN ex.first_block_at IS NOT NULL THEN COALESCE(ua.tokens, 0) END,
    CASE WHEN ex.first_block_at IS NOT NULL THEN COALESCE(ub.requests, 0) END,
    CASE WHEN ex.first_block_at IS NOT NULL THEN COALESCE(ub.input_tokens, 0) END,
    CASE WHEN ex.first_block_at IS NOT NULL THEN COALESCE(ub.output_tokens, 0) END,
    CASE WHEN ex.first_block_at IS NOT NULL THEN COALESCE(ub.cache_creation_tokens, 0) END,
    CASE WHEN ex.first_block_at IS NOT NULL THEN COALESCE(ub.cache_read_tokens, 0) END,
    CASE WHEN ex.first_block_at IS NOT NULL THEN COALESCE(ub.actual_cost, 0) END,
    (q.usage_snapshot->>'total')::int,
    (q.usage_snapshot->>'used')::int,
    NOW()
FROM accounts a
LEFT JOIN us ON us.account_id = a.id
LEFT JOIN ex ON ex.account_id = a.id
LEFT JOIN ub ON ub.account_id = a.id
LEFT JOIN ua ON ua.account_id = a.id
LEFT JOIN mo ON mo.account_id = a.id
LEFT JOIN quota_keys q ON q.id = a.quota_key_id
WHERE a.platform = 'cursor'
ON CONFLICT (stat_date, account_id) DO UPDATE SET
    quota_key_id                    = EXCLUDED.quota_key_id,
    requests                        = EXCLUDED.requests,
    input_tokens                    = EXCLUDED.input_tokens,
    output_tokens                   = EXCLUDED.output_tokens,
    cache_creation_tokens           = EXCLUDED.cache_creation_tokens,
    cache_read_tokens               = EXCLUDED.cache_read_tokens,
    actual_cost                     = EXCLUDED.actual_cost,
    model_requests                  = EXCLUDED.model_requests,
    exhausted                       = EXCLUDED.exhausted,
    exhausted_at                    = EXCLUDED.exhausted_at,
    blocked_count                   = EXCLUDED.blocked_count,
    first_request_at                = EXCLUDED.first_request_at,
    last_request_at                 = EXCLUDED.last_request_at,
    requests_after_block            = EXCLUDED.requests_after_block,
    tokens_after_block              = EXCLUDED.tokens_after_block,
    capacity_requests               = EXCLUDED.capacity_requests,
    capacity_input_tokens           = EXCLUDED.capacity_input_tokens,
    capacity_output_tokens          = EXCLUDED.capacity_output_tokens,
    capacity_cache_creation_tokens  = EXCLUDED.capacity_cache_creation_tokens,
    capacity_cache_read_tokens      = EXCLUDED.capacity_cache_read_tokens,
    capacity_actual_cost            = EXCLUDED.capacity_actual_cost,
    quota_key_total                 = EXCLUDED.quota_key_total,
    quota_key_used                  = EXCLUDED.quota_key_used,
    updated_at                      = NOW()
`

// UpsertCursorAccountDailyQuotaStats 汇总某一天（本地自然日）的数据进统计表，返回处理行数。
// dayStart/dayEnd 由调用方按本地时区算好传入，避免在 SQL 里猜时区。
func (r *quotaKeyRepository) UpsertCursorAccountDailyQuotaStats(ctx context.Context, statDate time.Time, dayStart, dayEnd time.Time) (int64, error) {
	if r == nil || r.client == nil {
		return 0, fmt.Errorf("cursor daily quota stats: ent client unavailable")
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return 0, fmt.Errorf("cursor daily quota stats: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, cursorDailyStatsUpsertSQL, statDate.Format("2006-01-02"), dayStart, dayEnd)
	if err != nil {
		return 0, fmt.Errorf("cursor daily quota stats: upsert %s: %w", statDate.Format("2006-01-02"), err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("cursor daily quota stats: commit %s: %w", statDate.Format("2006-01-02"), err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// cursorDailyStatsSelectSQL 是读取统计表的唯一查询（后台接口与手工排查共用同一口径）。
//
// quotaKeyID 传 nil 表示不限激活码。按日期升序、账号升序，保证同样的数据每次查出来顺序一致。
const cursorDailyStatsSelectSQL = `
SELECT stat_date, account_id, quota_key_id,
       requests, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
       actual_cost, model_requests,
       exhausted, exhausted_at, blocked_count,
       first_request_at, last_request_at,
       requests_after_block, tokens_after_block,
       capacity_requests, capacity_input_tokens, capacity_output_tokens,
       capacity_cache_creation_tokens, capacity_cache_read_tokens, capacity_actual_cost,
       quota_key_total, quota_key_used
  FROM cursor_account_daily_quota_stats
 WHERE stat_date >= $1::date AND stat_date <= $2::date
   AND ($3::bigint IS NULL OR quota_key_id = $3::bigint)
 ORDER BY stat_date ASC, account_id ASC`

// ListCursorAccountDailyQuotaStats 读取区间内的账号日统计。
func (r *quotaKeyRepository) ListCursorAccountDailyQuotaStats(ctx context.Context, from, to time.Time, quotaKeyID *int64) ([]service.CursorAccountDailyQuotaStat, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("cursor daily quota stats: ent client unavailable")
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("cursor daily quota stats: begin read tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var keyArg any
	if quotaKeyID != nil {
		keyArg = *quotaKeyID
	}
	rows, err := tx.QueryContext(ctx, cursorDailyStatsSelectSQL, from.Format("2006-01-02"), to.Format("2006-01-02"), keyArg)
	if err != nil {
		return nil, fmt.Errorf("cursor daily quota stats: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]service.CursorAccountDailyQuotaStat, 0, 256)
	for rows.Next() {
		var (
			stat           service.CursorAccountDailyQuotaStat
			statDate       time.Time
			quotaKeyIDV    sql.NullInt64
			actualCost     float64
			modelJSON      []byte
			exhaustedAt    sql.NullTime
			firstAt        sql.NullTime
			lastAt         sql.NullTime
			reqAfterBlock  sql.NullInt64
			tokAfterBlock  sql.NullInt64
			capRequests    sql.NullInt64
			capInTokens    sql.NullInt64
			capOutTokens   sql.NullInt64
			capCacheCreate sql.NullInt64
			capCacheRead   sql.NullInt64
			capCost        sql.NullFloat64
			keyTotal       sql.NullInt64
			keyUsed        sql.NullInt64
		)
		if err := rows.Scan(
			&statDate, &stat.AccountID, &quotaKeyIDV,
			&stat.Requests, &stat.InputTokens, &stat.OutputTokens,
			&stat.CacheCreationTokens, &stat.CacheReadTokens,
			&actualCost, &modelJSON,
			&stat.Exhausted, &exhaustedAt, &stat.BlockedCount,
			&firstAt, &lastAt,
			&reqAfterBlock, &tokAfterBlock,
			&capRequests, &capInTokens, &capOutTokens,
			&capCacheCreate, &capCacheRead, &capCost,
			&keyTotal, &keyUsed,
		); err != nil {
			return nil, fmt.Errorf("cursor daily quota stats: scan: %w", err)
		}
		stat.StatDate = statDate
		stat.ActualCost = actualCost
		if len(modelJSON) > 0 {
			var breakdown map[string]int64
			if err := json.Unmarshal(modelJSON, &breakdown); err == nil && len(breakdown) > 0 {
				stat.ModelRequests = breakdown
			}
		}
		if quotaKeyIDV.Valid {
			v := quotaKeyIDV.Int64
			stat.QuotaKeyID = &v
		}
		if exhaustedAt.Valid {
			v := exhaustedAt.Time
			stat.ExhaustedAt = &v
		}
		if firstAt.Valid {
			v := firstAt.Time
			stat.FirstRequestAt = &v
		}
		if lastAt.Valid {
			v := lastAt.Time
			stat.LastRequestAt = &v
		}
		if reqAfterBlock.Valid {
			v := reqAfterBlock.Int64
			stat.RequestsAfterBlock = &v
		}
		if tokAfterBlock.Valid {
			v := tokAfterBlock.Int64
			stat.TokensAfterBlock = &v
		}
		if capRequests.Valid {
			v := capRequests.Int64
			stat.CapacityRequests = &v
		}
		if capInTokens.Valid {
			v := capInTokens.Int64
			stat.CapacityInputTokens = &v
		}
		if capOutTokens.Valid {
			v := capOutTokens.Int64
			stat.CapacityOutputTokens = &v
		}
		if capCacheCreate.Valid {
			v := capCacheCreate.Int64
			stat.CapacityCacheCreationTokens = &v
		}
		if capCacheRead.Valid {
			v := capCacheRead.Int64
			stat.CapacityCacheReadTokens = &v
		}
		if capCost.Valid {
			v := capCost.Float64
			stat.CapacityActualCost = &v
		}
		if keyTotal.Valid {
			v := int(keyTotal.Int64)
			stat.QuotaKeyTotal = &v
		}
		if keyUsed.Valid {
			v := int(keyUsed.Int64)
			stat.QuotaKeyUsed = &v
		}
		out = append(out, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cursor daily quota stats: rows: %w", err)
	}
	return out, nil
}

// cursorKeyBurnSQL 按「激活码 + 自然日」汇总：手里几个号、几个在干活、几个被挡、烧了多少。
//
// 这就是用户要的核心答案：「每个激活码每天 400 额度，被多少个账号消耗掉」。
//
// 2026-11-06「加全」：原版只 sum 了 requests / cost 和"几个号 exhausted"，漏了三个关键维度——
//   - false_block_accounts：被挡之后仍然成功的号数。这是"假挡"的直接证据，缺了它就会把
//     假挡号当成真耗尽号，于是"一个号值多少请求"永远算不准；
//   - token 分解（含 cache creation / read）：生产样本里缓存占比很大，漏掉会显著低估消耗；
//   - model_requests：opus 与 sonnet 扣量倍率不同，不拆开看不出哪个模型把码吃掉了。
//
// 另外容量观测取 max 而不是 sum：求的是"单个号最多能烧到多少"的下界，不是全码总量。
const cursorKeyBurnSQL = `
WITH base AS (
    SELECT stat_date, quota_key_id, requests,
           input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
           actual_cost, model_requests,
           COALESCE(blocked_count, 0) AS blocked_count,
           requests_after_block, tokens_after_block,
           capacity_requests, capacity_actual_cost,
           -- 被挡时的 token 总量：四个分量都缺失时保持 NULL（没被挡过 ≠ 烧了 0）
           CASE WHEN capacity_input_tokens IS NULL
                     AND capacity_output_tokens IS NULL
                     AND capacity_cache_creation_tokens IS NULL
                     AND capacity_cache_read_tokens IS NULL
                THEN NULL
                ELSE COALESCE(capacity_input_tokens, 0)
                   + COALESCE(capacity_output_tokens, 0)
                   + COALESCE(capacity_cache_creation_tokens, 0)
                   + COALESCE(capacity_cache_read_tokens, 0)
           END AS capacity_tokens,
           first_request_at, last_request_at, quota_key_total, quota_key_used
      FROM cursor_account_daily_quota_stats
     WHERE stat_date >= $1::date AND stat_date <= $2::date
       AND quota_key_id IS NOT NULL
       AND ($3::bigint IS NULL OR quota_key_id = $3::bigint)
),
agg AS (
    SELECT stat_date, quota_key_id,
           count(*)                                                     AS total_accounts,
           count(*) FILTER (WHERE requests > 0)                         AS active_accounts,
           count(*) FILTER (WHERE blocked_count > 0)                    AS blocked_accounts,
           count(*) FILTER (WHERE blocked_count > 0
                              AND COALESCE(requests_after_block, 0) > 0) AS false_block_accounts,
           count(*) FILTER (WHERE blocked_count = 0)                    AS never_blocked_accounts,
           COALESCE(sum(requests), 0)                                   AS total_requests,
           COALESCE(sum(input_tokens), 0)                               AS total_input_tokens,
           COALESCE(sum(output_tokens), 0)                              AS total_output_tokens,
           COALESCE(sum(cache_creation_tokens), 0)                      AS total_cache_creation_tokens,
           COALESCE(sum(cache_read_tokens), 0)                          AS total_cache_read_tokens,
           COALESCE(sum(actual_cost), 0)                                AS total_cost,
           COALESCE(sum(blocked_count), 0)                              AS total_blocked_count,
           COALESCE(sum(requests_after_block), 0)                       AS total_requests_after_block,
           COALESCE(sum(tokens_after_block), 0)                         AS total_tokens_after_block,
           max(capacity_requests)                                       AS max_capacity_requests,
           max(capacity_tokens)                                         AS max_capacity_tokens,
           max(capacity_actual_cost)                                    AS max_capacity_cost,
           min(first_request_at)                                        AS first_request_at,
           max(last_request_at)                                         AS last_request_at,
           max(quota_key_total)                                         AS key_total,
           max(quota_key_used)                                          AS key_used
      FROM base
     GROUP BY stat_date, quota_key_id
),
-- 模型分布要先摊平再合并：每个号各有自己的 map，同名模型的计数必须相加。
md AS (
    SELECT b.stat_date, b.quota_key_id, m.key, sum(m.value::bigint) AS cnt
      FROM base b
      CROSS JOIN LATERAL jsonb_each_text(COALESCE(b.model_requests, '{}'::jsonb)) AS m(key, value)
     GROUP BY b.stat_date, b.quota_key_id, m.key
),
mdagg AS (
    SELECT stat_date, quota_key_id, jsonb_object_agg(key, cnt) AS model_requests
      FROM md
     GROUP BY stat_date, quota_key_id
)
SELECT a.stat_date,
       a.quota_key_id,
       a.total_accounts,
       a.active_accounts,
       a.blocked_accounts,
       a.false_block_accounts,
       a.never_blocked_accounts,
       a.total_requests,
       a.total_input_tokens,
       a.total_output_tokens,
       a.total_cache_creation_tokens,
       a.total_cache_read_tokens,
       a.total_input_tokens + a.total_output_tokens
         + a.total_cache_creation_tokens + a.total_cache_read_tokens AS total_tokens,
       a.total_cost,
       mdagg.model_requests,
       a.total_blocked_count,
       a.total_requests_after_block,
       a.total_tokens_after_block,
       a.max_capacity_requests,
       a.max_capacity_tokens,
       a.max_capacity_cost,
       a.first_request_at,
       a.last_request_at,
       a.key_total,
       a.key_used
  FROM agg a
  LEFT JOIN mdagg ON mdagg.stat_date = a.stat_date AND mdagg.quota_key_id = a.quota_key_id
 ORDER BY a.stat_date ASC, a.quota_key_id ASC`

// SummarizeCursorQuotaKeyDailyBurn 按码+日汇总烧号情况。
//
// quotaKeyID 非 nil 时只汇总该码，与账号明细表的过滤保持同一口径。
func (r *quotaKeyRepository) SummarizeCursorQuotaKeyDailyBurn(ctx context.Context, from, to time.Time, quotaKeyID *int64) ([]service.CursorQuotaKeyDailyBurn, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("cursor daily quota stats: ent client unavailable")
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("cursor daily quota stats: begin summary tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 显式传 nil 接口值：让驱动发 NULL，避免把 *int64 零值当成"过滤 id=0"。
	var keyFilter any
	if quotaKeyID != nil && *quotaKeyID > 0 {
		keyFilter = *quotaKeyID
	}

	rows, err := tx.QueryContext(ctx, cursorKeyBurnSQL,
		from.Format("2006-01-02"), to.Format("2006-01-02"), keyFilter)
	if err != nil {
		return nil, fmt.Errorf("cursor daily quota stats: summary query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]service.CursorQuotaKeyDailyBurn, 0, 64)
	for rows.Next() {
		var (
			row      service.CursorQuotaKeyDailyBurn
			statDate time.Time
			modelRaw []byte
			keyTotal sql.NullInt64
			keyUsed  sql.NullInt64
			capReq   sql.NullInt64
			capTok   sql.NullInt64
			capCost  sql.NullFloat64
			firstAt  sql.NullTime
			lastAt   sql.NullTime
		)
		if err := rows.Scan(
			&statDate,
			&row.QuotaKeyID,
			&row.TotalAccounts,
			&row.ActiveAccounts,
			&row.BlockedAccounts,
			&row.FalseBlockAccounts,
			&row.NeverBlockedAccounts,
			&row.TotalRequests,
			&row.TotalInputTokens,
			&row.TotalOutputTokens,
			&row.TotalCacheCreationTokens,
			&row.TotalCacheReadTokens,
			&row.TotalTokens,
			&row.TotalCost,
			&modelRaw,
			&row.TotalBlockedCount,
			&row.TotalRequestsAfterBlock,
			&row.TotalTokensAfterBlock,
			&capReq,
			&capTok,
			&capCost,
			&firstAt,
			&lastAt,
			&keyTotal,
			&keyUsed,
		); err != nil {
			return nil, fmt.Errorf("cursor daily quota stats: summary scan: %w", err)
		}
		row.StatDate = statDate
		// 模型分布是附加信息，解析失败不该让整张表打不开：保持 nil，其余字段照给。
		if len(modelRaw) > 0 {
			var models map[string]int64
			if err := json.Unmarshal(modelRaw, &models); err == nil {
				row.ModelRequests = models
			}
		}
		if capReq.Valid {
			v := capReq.Int64
			row.MaxCapacityRequests = &v
		}
		if capTok.Valid {
			v := capTok.Int64
			row.MaxCapacityTokens = &v
		}
		if capCost.Valid {
			v := capCost.Float64
			row.MaxCapacityCost = &v
		}
		if firstAt.Valid {
			v := firstAt.Time
			row.FirstRequestAt = &v
		}
		if lastAt.Valid {
			v := lastAt.Time
			row.LastRequestAt = &v
		}
		if keyTotal.Valid {
			v := int(keyTotal.Int64)
			row.QuotaKeyTotal = &v
		}
		if keyUsed.Valid {
			v := int(keyUsed.Int64)
			row.QuotaKeyUsed = &v
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cursor daily quota stats: summary rows: %w", err)
	}
	return out, nil
}
