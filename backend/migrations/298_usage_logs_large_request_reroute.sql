-- usage_logs 新增对账两列（LRO-02，方案 §3.4）
--
-- 1) estimated_input_tokens：在账号选择之前本地估算的输入 token 数；
--    未做估算的请求（或历史行）保持默认值 0。
-- 2) large_request_reroute：本次请求是否因「仅大请求可用」规则被排除过至少一个账号。
--
-- 兼容性（重入 / 回滚）：
--   - 两列均带 DEFAULT，存量行在不跑迁移时语义也正确（双保险）。
--   - 全程 IF NOT EXISTS，迁移可重入（重复执行为幂等空操作）。
--   - 本文件是 ent/schema/usage_log.go 的 SQL 落地，字段/类型/默认值逐字对应。
--   - 末尾注释含回滚语句（仅在需要时手动执行）。

ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS estimated_input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS large_request_reroute BOOLEAN NOT NULL DEFAULT FALSE;

-- 回滚（仅在需要时手动执行）：
-- BEGIN;
-- ALTER TABLE usage_logs DROP COLUMN IF EXISTS large_request_reroute;
-- ALTER TABLE usage_logs DROP COLUMN IF EXISTS estimated_input_tokens;
-- COMMIT;
