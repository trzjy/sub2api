-- 删除隐藏的组内优先级（2026-11-17 用户裁定：只保留后台可见的账号优先级）
ALTER TABLE account_groups DROP COLUMN IF EXISTS priority;
