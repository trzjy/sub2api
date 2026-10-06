-- 239_xianyu_pool_redeem_cards.sql
-- 库存池接入真实兑换卡：
-- 1) 池新增发码规格（code_type/group_id/validity_days），补货直接生成可在站点兑换的订阅码；
-- 2) 下线 xianyu_delivery 发货凭证类型——该类型码领取即核销、用户无法兑换，存量数据
--    （含其订单领取记录）均为测试期产物，随迁移一并清理。

ALTER TABLE xianyu_item_pools
    ADD COLUMN IF NOT EXISTS code_type TEXT NOT NULL DEFAULT 'subscription',
    ADD COLUMN IF NOT EXISTS group_id BIGINT REFERENCES "groups"("id"),
    ADD COLUMN IF NOT EXISTS validity_days INTEGER;

DELETE FROM xianyu_order_claims
WHERE redeem_code_id IN (SELECT id FROM redeem_codes WHERE type = 'xianyu_delivery');

DELETE FROM redeem_codes WHERE type = 'xianyu_delivery';
