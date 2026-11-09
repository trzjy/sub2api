-- 298_quota_keys_code_mac_partial_unique.sql
-- 修复：uq_quota_keys_code_mac 未排除软删行，导致被删除的激活码永久占用唯一坑位，
-- 同一张卡删掉后无法重建（生产实证：重建报 duplicate key value violates unique constraint）。
-- 改为部分唯一索引：仅对未软删行（deleted_at IS NULL）保持 code_mac 唯一。
-- 与 016_soft_delete_partial_unique_indexes.sql 的既有模式一致。

DROP INDEX IF EXISTS uq_quota_keys_code_mac;

CREATE UNIQUE INDEX IF NOT EXISTS uq_quota_keys_code_mac
    ON quota_keys(code_mac)
    WHERE code_mac IS NOT NULL AND deleted_at IS NULL;
