-- 304_quota_keys_account_settings.sql
-- B 批（cursor-key-as-management-unit）：在 quota_keys（激活码）上增加账号设置三列，
-- 把并发、负载因子、计费倍率上移到激活码，账号跟随码（用户裁定 2026-11-10）。
--
-- 语义（与 accounts 同型，但本列为"码级权威值"）：
--   - concurrency   激活码并发上限（NULL 表示沿用账号列默认 3，不写假值）
--   - load_factor   负载因子（NULL 表示沿用在账号列，空）
--   - rate_multiplier 计费倍率（中性值 1.0，与账号默认一致；NOT NULL 保证存量/新行都有值）
--
-- 兼容性（重入 / 回滚）：
--   - 全程 IF NOT EXISTS，迁移可重复执行（幂等空操作）。
--   - 本文件是 ent/schema/quota_key.go 对应三列的 SQL 落地，类型与默认值逐字对应
--     （ent 中 rate_multiplier 为 field.Float + SchemaType(decimal(10,4)) + Default(1.0)
--      → DECIMAL(10,4) NOT NULL DEFAULT 1.0；与 accounts.rate_multiplier 等对齐；
--      concurrency / load_factor 为 Optional().Nillable() → 可空 INTEGER）。
--   - 末尾注释含回滚语句（仅在需要时手动执行）。
--   - 写入逻辑（拉号入库 / 人工建号 / 改绑时把码级权威值落到名下账号）不在本卡范围。

ALTER TABLE quota_keys ADD COLUMN IF NOT EXISTS concurrency INTEGER;
ALTER TABLE quota_keys ADD COLUMN IF NOT EXISTS load_factor INTEGER;
ALTER TABLE quota_keys ADD COLUMN IF NOT EXISTS rate_multiplier DECIMAL(10,4) NOT NULL DEFAULT 1.0;

-- 回滚（仅在需要时手动执行）：
-- BEGIN;
-- ALTER TABLE quota_keys DROP COLUMN IF EXISTS rate_multiplier;
-- ALTER TABLE quota_keys DROP COLUMN IF EXISTS load_factor;
-- ALTER TABLE quota_keys DROP COLUMN IF EXISTS concurrency;
-- COMMIT;
