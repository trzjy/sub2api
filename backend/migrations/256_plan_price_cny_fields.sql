-- 256_plan_price_cny_fields.sql
-- 新增 price_cny / original_price_cny 字段，存储管理员输入的人民币原值。
-- 管理后台回显直接读这两个字段，不再从 USD 换算回来。

ALTER TABLE subscription_plans
  ADD COLUMN IF NOT EXISTS price_cny numeric(20,4) DEFAULT 0,
  ADD COLUMN IF NOT EXISTS original_price_cny numeric(20,4) DEFAULT 0;

-- 回填已有数据：用当前汇率 7.15 从 USD 反算 CNY（仅一次，后续新数据由前端写入）
UPDATE subscription_plans
SET price_cny = ROUND(price * 7.15, 4),
    original_price_cny = ROUND(COALESCE(original_price, 0) * 7.15, 4)
WHERE price_cny = 0;
