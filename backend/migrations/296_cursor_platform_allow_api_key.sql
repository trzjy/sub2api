-- Cursor 站点渠道接入：放宽 accounts 表中 cursor 平台的 quota_key_id 检查约束
--
-- 历史迁移 274 的约束为
--   CHECK (platform <> 'cursor' OR quota_key_id IS NOT NULL)
-- 即 platform='cursor' 时强制 quota_key_id 必填。
--
-- 为接入第三方 OpenAI 兼容网关上游（kn-cy 等）作为 Cursor 备用渠道，需要允许
-- platform = 'cursor' 且 type = 'api_key' 的账号 quota_key_id 为 NULL：
--   - type = 'api_key'：第三方网关账号（kn-cy 等），允许 quota_key_id 为 NULL；
--   - type <> 'api_key'（ShareAI 号池 OAuth 账号）：继续强制 quota_key_id IS NOT NULL。
--
-- 历史迁移文件不可变，本迁移 296 通过 DROP + ADD 同名约束替换 274 的检查条件，
-- 保持约束名 accounts_cursor_quota_key_required 不变，避免与 274 的 DROP/ADD 冲突。
--
-- 兼容性（重入 / 回滚）：
--   - 全程 IF EXISTS / 同名约束，迁移可重入（重复执行为幂等空操作）。
--   - 回滚见文件末尾注释。
--   - 与 274 共享同一个约束名，故 274 与 296 各自重入均安全。

ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_cursor_quota_key_required;
ALTER TABLE accounts
    ADD CONSTRAINT accounts_cursor_quota_key_required
    CHECK (platform <> 'cursor' OR quota_key_id IS NOT NULL OR type = 'api_key');

-- 回滚（仅在需要时手动执行）：
-- BEGIN;
-- ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_cursor_quota_key_required;
-- ALTER TABLE accounts
--     ADD CONSTRAINT accounts_cursor_quota_key_required
--     CHECK (platform <> 'cursor' OR quota_key_id IS NOT NULL);
-- COMMIT;
