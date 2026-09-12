-- 245_welfare_redeem_cards.sql
-- 福利兑换卡：批次发放 + 一卡多分组订阅 + 独立福利余额池。
-- 1) redeem_batches 批次元数据（生成参数快照 + 统计锚点）；
-- 2) redeem_codes 增加 batch_id，welfare 类型卡归属批次；
--    部分唯一索引 (batch_id, used_by) 兜底「同批次每人限兑一张」；
-- 3) redeem_code_groups 一卡多分组勾选快照（每组独立 validity_days：天1/周7/月30）；
-- 4) welfare_balances 独立福利余额池，一卡一行，批次间互不影响；
--    扣减优先于 users.balance，按 expires_at 最近优先；到期由定时任务清零，
--    清零只动本表，绝不触碰 users.balance。

CREATE TABLE IF NOT EXISTS redeem_batches (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL DEFAULT '',
    config      JSONB NOT NULL DEFAULT '{}'::jsonb,
    code_count  INTEGER NOT NULL DEFAULT 0,
    created_by  BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE redeem_codes
    ADD COLUMN IF NOT EXISTS batch_id BIGINT REFERENCES redeem_batches(id);

CREATE INDEX IF NOT EXISTS idx_redeem_codes_batch_id ON redeem_codes (batch_id);

-- 同批次每人限兑一张：used_by 写入时即受约束（非延迟），并发抢兑第二张学唯一冲突即失败。
CREATE UNIQUE INDEX IF NOT EXISTS idx_redeem_codes_batch_one_per_user
    ON redeem_codes (batch_id, used_by)
    WHERE batch_id IS NOT NULL AND used_by IS NOT NULL;

CREATE TABLE IF NOT EXISTS redeem_code_groups (
    redeem_code_id BIGINT NOT NULL REFERENCES redeem_codes(id) ON DELETE CASCADE,
    group_id       BIGINT NOT NULL REFERENCES groups(id),
    validity_days  INTEGER NOT NULL DEFAULT 30,
    PRIMARY KEY (redeem_code_id, group_id)
);

CREATE TABLE IF NOT EXISTS welfare_balances (
    id               BIGSERIAL PRIMARY KEY,
    user_id          BIGINT NOT NULL REFERENCES users(id),
    redeem_code_id   BIGINT NOT NULL REFERENCES redeem_codes(id),
    batch_id         BIGINT NOT NULL REFERENCES redeem_batches(id),
    amount_initial   NUMERIC(20,8) NOT NULL DEFAULT 0,
    amount_remaining NUMERIC(20,8) NOT NULL DEFAULT 0,
    status           TEXT NOT NULL DEFAULT 'active',
    expires_at       TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 扣减（user 维度按过期时间正序扫可用行）与清零扫描（status + expires_at）共用。
CREATE INDEX IF NOT EXISTS idx_welfare_balances_user_active_expiry
    ON welfare_balances (user_id, status, expires_at);
CREATE INDEX IF NOT EXISTS idx_welfare_balances_expiry_sweep
    ON welfare_balances (status, expires_at);
