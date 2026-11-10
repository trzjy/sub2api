-- 300_accounts_platform_uid_partial_unique.sql
-- 修复：accounts_platform_uid_active 未排除软删行，软删账号永久占用 (platform, uid) 唯一坑位，
-- 同一账号删掉后无法重建。与迁移 299（激活码同一缺陷）同源。
-- 改为部分唯一索引：仅对未软删行强制唯一。
--
-- 注意：本迁移与 account_repo_cockpit_import.go 的 ConflictWhere 必须逐字一致，
-- 否则 cockpit 批量导入的 ON CONFLICT 推断会失败（PG 要求谓词匹配）。

DROP INDEX IF EXISTS accounts_platform_uid_active;

CREATE UNIQUE INDEX IF NOT EXISTS accounts_platform_uid_active
    ON accounts (platform, uid)
    WHERE uid != '' AND deleted_at IS NULL;
