-- 240_xianyu_pool_worker_card.sql
-- 卡券按池自动创建：每个库存池在 Worker 侧有一张专属的 API 发货卡券，
-- worker_card_id 记录该卡券 ID（主程序建池/绑定时自动供给，无需手工配置）。

ALTER TABLE xianyu_item_pools
    ADD COLUMN IF NOT EXISTS worker_card_id BIGINT;
