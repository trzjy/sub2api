-- 294_cursor_account_daily_quota_stats.sql
--
-- 目的（用户裁定 D2 的统计设计，2026-11-06）：
--   老板要能准确回答两个问题——
--     ① 每个激活码每天那 400 额度，到底被多少个账号烧光？
--     ② 每个账号每天到底有多少可用额度？
--
-- 为什么必须落表而不是临时写 SQL：
--   上游给不出账号级真实余额（三条路都验证过：中转返回 mock、无 token 401、带 token 也 401），
--   所以"账号每天多少额度"只能靠实测反推——一个账号当天把自己用到"今日额度已用完"，
--   那它当天的用量就是它日容量的实测值（下界，且随天数收敛）。
--   这个"当天用光"的事实必须当时记下来，否则一周后 usage_logs 被保留期清掉就永久丢失。
--
-- 粒度：一账号一天一行（stat_date + account_id 唯一）。
-- 幂等：回填/重跑直接 UPSERT，不会重复累加。
CREATE TABLE IF NOT EXISTS cursor_account_daily_quota_stats (
    -- stat_date 是**本地时区**（Asia/Shanghai）的自然日，不是 UTC 日。
    -- 上游额度按"每日早上 8 点"重置，但分组统计按自然日更符合人看账本的习惯。
    stat_date DATE NOT NULL,
    account_id BIGINT NOT NULL,
    -- 归属激活码：账号可能被改绑，这里记的是统计时刻的归属，用于按码汇总。
    quota_key_id BIGINT,

    -- 当天该账号经计费链路记录的请求数与用量（本站口径，不是上游口径）。
    requests BIGINT NOT NULL DEFAULT 0,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    actual_cost NUMERIC(18, 8) NOT NULL DEFAULT 0,

    -- 当天是否撞过"今日额度已用完"；exhausted_at 是当天第一次撞上的时刻。
    exhausted BOOLEAN NOT NULL DEFAULT false,
    exhausted_at TIMESTAMPTZ,

    first_request_at TIMESTAMPTZ,
    last_request_at TIMESTAMPTZ,

    -- 实测日容量：只有 exhausted = true 才有值（当天用光 ⇒ 当天用量就是容量实测值）。
    -- 没耗尽的那天不填，避免把"没用完"误当成"只有这么多"。
    capacity_requests BIGINT,
    capacity_input_tokens BIGINT,
    capacity_output_tokens BIGINT,
    capacity_actual_cost NUMERIC(18, 8),

    -- 统计时刻该激活码的额度快照（total/used），用于把码的 400 分摊到账号上。
    -- 上游没给值时为 NULL（缺失不是 0）。
    quota_key_total INT,
    quota_key_used INT,

    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (stat_date, account_id)
);

-- 按码 + 日期查"这个码今天烧了几个号"。
CREATE INDEX IF NOT EXISTS idx_cursor_ads_key_date
    ON cursor_account_daily_quota_stats (quota_key_id, stat_date);

-- 按账号看历史趋势（这个号每天多少额度、是不是每天都能用满）。
CREATE INDEX IF NOT EXISTS idx_cursor_ads_account_date
    ON cursor_account_daily_quota_stats (account_id, stat_date);

COMMENT ON TABLE cursor_account_daily_quota_stats IS
    'Cursor 账号每日额度实测统计（一账号一天一行）：用量、是否耗尽、实测日容量，用于回答"每码每天 400 被几个号烧光"与"每个号每天多少额度"';
COMMENT ON COLUMN cursor_account_daily_quota_stats.capacity_requests IS
    '实测日容量（请求数）：仅当当天 exhausted=true 时有值';
