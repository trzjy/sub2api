-- 253_currency_unification.down.sql
-- 253 号迁移的 down 脚本：仅做结构回滚（结构回滚按方案 §4-5）。
--
-- ⚠️ 数据回滚不承诺无损（price 除法取整有厘级损耗），且 forward 迁移删除了
--    SUBSCRIPTION_USD_TO_CNY_RATE / BALANCE_RECHARGE_MULTIPLIER 原值。
--    生产回滚以迁移前 pg_dump 备份恢复为唯一手段；本文件仅用于开发/测试环境
--    结构清理，不恢复 plan 价格与旧设置 key。

-- 1. 删除 payment_orders.currency 列
ALTER TABLE payment_orders DROP COLUMN IF EXISTS currency;

-- 2. 删除 FX_RATES / RECHARGE_MARKUP 设置（旧 key 数据无法恢复，见文件头说明）
DELETE FROM settings WHERE key IN ('FX_RATES', 'RECHARGE_MARKUP');
