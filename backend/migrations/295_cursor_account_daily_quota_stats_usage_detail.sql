-- 295_cursor_account_daily_quota_stats_usage_detail.sql
--
-- 目的（用户 2026-11-06 指令）：
--   「把每个账号每天用量都给我记录下来，我们要知道每个账号的上限到底是多少」
--
-- 为什么补这些列（294 建表时漏了三件对"求上限"最要紧的事）：
--
-- 1. cache token 没记。生产样本里缓存 token 占比很大（367 号某次 14 万 io + 10 万 cache），
--    只统计 input/output 会显著低估真实消耗，求出来的"上限"必然偏小。
--
-- 2. 模型构成没记。claude-opus 与 claude-sonnet 的扣量倍率明显不同：358 号 3 次 opus 就
--    被挡，355 号 15 次 sonnet 才被挡。不按模型拆开，两类号的"上限"永远对不上。
--
-- 3. **最要紧的一条**：294 里的 capacity_* 取的是"当天全天用量"，而不是"被挡那一刻之前的
--    累计用量"。这是错的——如果账号在 15:38 被挡、之后当天又成功了 10 次（说明挡是假的或
--    已恢复），全天用量就会把挡后的量算进"容量"里，把上限抬高。本迁移新增
--    requests_after_block / tokens_after_block 专门度量"被挡之后还成功了多少"：
--    这个值 > 0 就是"挡板话术"的直接证据，也是判断上游到底有没有真额度的关键指标。
--
-- 口径更正（重要）：
--   capacity_* 不再声称是"实测日容量"。上游的额度口径尚未确认（三重排他证据表明
--   「今日额度已用完」不是余额判定），所以这些列只表示"上游说这句话时，该号当天已经
--   消耗了多少"，是求上限的**观测样本**，不是结论。列名保留不改，避免破坏已上线的读取。

ALTER TABLE cursor_account_daily_quota_stats
    ADD COLUMN IF NOT EXISTS cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cache_read_tokens     BIGINT NOT NULL DEFAULT 0,
    -- 按模型的当日请求数，形如 {"claude-opus-5-5": 3, "claude-sonnet-5": 15}。
    -- 用 jsonb 而不是固定列：模型名会变，加列每次都要迁移。
    ADD COLUMN IF NOT EXISTS model_requests        JSONB,
    -- 当天被上游以「今日额度已用完」挡回的次数（可能多次）。
    ADD COLUMN IF NOT EXISTS blocked_count         INT NOT NULL DEFAULT 0,
    -- 第一次被挡之后，该号当天还成功了多少请求 / 多少 token。
    -- > 0 即证明"额度未真正耗尽，只是上游换了句话挡回来"；NULL 表示当天没被挡。
    ADD COLUMN IF NOT EXISTS requests_after_block  BIGINT,
    ADD COLUMN IF NOT EXISTS tokens_after_block    BIGINT,
    -- 被挡时刻之前的累计用量（求上限的主样本）。
    ADD COLUMN IF NOT EXISTS capacity_cache_creation_tokens BIGINT,
    ADD COLUMN IF NOT EXISTS capacity_cache_read_tokens     BIGINT;

COMMENT ON COLUMN cursor_account_daily_quota_stats.capacity_requests IS
    '观测样本：上游说「今日额度已用完」时，该号当天（被挡之前）已消耗的请求数。不是确认的日容量。';
COMMENT ON COLUMN cursor_account_daily_quota_stats.blocked_count IS
    '当天被上游「今日额度已用完」挡回的次数';
COMMENT ON COLUMN cursor_account_daily_quota_stats.requests_after_block IS
    '第一次被挡之后当天仍成功的请求数；> 0 即证明额度并未真正耗尽';
COMMENT ON COLUMN cursor_account_daily_quota_stats.model_requests IS
    '当日按模型拆分的请求数，如 {"claude-opus-5-5":3}';

-- 求上限时要按"被挡过没"筛选，给个部分索引。
CREATE INDEX IF NOT EXISTS idx_cursor_ads_blocked
    ON cursor_account_daily_quota_stats (stat_date, account_id)
    WHERE blocked_count > 0;
