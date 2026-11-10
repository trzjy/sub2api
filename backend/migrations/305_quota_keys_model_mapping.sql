-- 305_quota_keys_model_mapping.sql
-- 将账号模型限制（model_mapping：白名单恒等映射与模型映射共用 JSONB map[string]string）
-- 上移到激活码管理（用户裁定 2026-11-10）。
-- NULL = 尚未在码级覆盖（拉号入库沿用上游实供/静态清单默认）；
-- 非 NULL = 码级权威模型映射，修改码时同事务同步名下账号，拉号/人工建号/改绑时继承。

ALTER TABLE quota_keys ADD COLUMN IF NOT EXISTS model_mapping JSONB;

-- 回滚（仅在需要时手动执行）：
-- BEGIN;
-- ALTER TABLE quota_keys DROP COLUMN IF EXISTS model_mapping;
-- COMMIT;
