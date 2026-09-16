-- 255_plan_price_precision.sql
-- 提升 subscription_plans 价格存储精度：decimal(20,2) → decimal(20,4)
-- 原因：CNY→USD→CNY 双向换算时 2 位小数舍入导致 ±0.01 抖动（如 12 CNY 存为 1.68 USD，回显 12.01 CNY）
-- 4 位小数可保证主流汇率下双向换算稳定在 ±0.00 范围内。

ALTER TABLE subscription_plans
  ALTER COLUMN price TYPE numeric(20,4) USING price::numeric(20,4),
  ALTER COLUMN original_price TYPE numeric(20,4) USING original_price::numeric(20,4);
