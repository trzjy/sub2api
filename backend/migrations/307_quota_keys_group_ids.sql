-- 307_quota_keys_group_ids.sql
-- 分组绑定（group_ids）上移到激活码管理（用户裁定 2026-11-11，模块 A1）。
-- 语义约定：
--   NULL = 未显式指定（拉号沿用模型推导挂组）；
--   []   = 显式解绑全部分组；
--   [1,2] = 码级权威分组。

ALTER TABLE quota_keys ADD COLUMN IF NOT EXISTS group_ids JSONB;

-- 回滚（仅在需要时手动执行）：
-- ALTER TABLE quota_keys DROP COLUMN IF EXISTS group_ids;
