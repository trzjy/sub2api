-- Cursor 站点渠道接入（v3.3 §4.2 / C2）— accounts.quota_key_id 归属激活码
--
-- 为 accounts 表追加 quota_key_id 外键列，绑定 C1 的 quota_keys 表，实现
--「账号强制归属激活码」。约束三件套：
--   - CHECK：platform='cursor' 时 quota_key_id 必填（非 cursor 允许 NULL）
--   - FK：quota_keys(id) ON DELETE RESTRICT（禁止删除仍被账号引用的激活码）
--   - 部分索引：仅热路径（未删除 + 可调度）加速「按激活码取账号」查询
--     列顺序 (quota_key_id, status, schedulable) 对齐调度热路径过滤
--
-- C0 阻塞已解除：生产实测 accounts WHERE platform='cursor' = 0（含软删），
-- 无需存量回填，CHECK 可直接加（不会与既有行冲突）。
--
-- 兼容性（重入 / 回滚）：
--   - 所有 IF NOT EXISTS / IF EXISTS 保护，迁移可重入
--   - 回滚见文件末尾注释（DROP 约束/索引/列）
--   - 与 ent generate 的 schema.go（accounts 表 QuotaKeyID 列 + 外键）保持一致

-- 1) 外键列（默认 NULL：非 cursor 平台允许无归属）
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS quota_key_id bigint DEFAULT NULL;

-- 2) CHECK：cursor 平台必须归属激活码；其余平台允许 NULL
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_cursor_quota_key_required;
ALTER TABLE accounts
    ADD CONSTRAINT accounts_cursor_quota_key_required
    CHECK (platform <> 'cursor' OR quota_key_id IS NOT NULL);

-- 3) FK：归一到 quota_keys(id)，删除激活码被引用时拒绝（RESTRICT）
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_quota_key_id_fk;
ALTER TABLE accounts
    ADD CONSTRAINT accounts_quota_key_id_fk
    FOREIGN KEY (quota_key_id) REFERENCES quota_keys(id) ON DELETE RESTRICT;

-- 4) 部分索引：仅未删除且可调度的账号，加速「按激活码拉号池」热路径
DROP INDEX IF EXISTS idx_accounts_quota_key_working;
CREATE INDEX idx_accounts_quota_key_working
    ON accounts(quota_key_id, status, schedulable) WHERE deleted_at IS NULL;

-- 回滚（仅在需要时手动执行）：
-- BEGIN;
-- ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_quota_key_id_fk;
-- ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_cursor_quota_key_required;
-- DROP INDEX IF EXISTS idx_accounts_quota_key_working;
-- ALTER TABLE accounts DROP COLUMN IF EXISTS quota_key_id;
-- COMMIT;
