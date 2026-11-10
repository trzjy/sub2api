-- 302_quota_upstreams_key_partial_unique.sql
-- 修复：uq_quota_upstreams_key 未排除软删行，软删的上游条目永久占用 key 唯一坑位。
-- 改为部分唯一索引：仅对未软删行强制唯一。
--
-- 该索引不是 UNIQUE 约束（迁移 297 建的是普通 CREATE UNIQUE INDEX），故只需重建索引。
-- 运行时无 ON CONFLICT (key) 依赖：quota_upstream_repo 只有 Exists/Get/List/UpdateOneID，
-- 无 Create 路径；297 里的 ON CONFLICT (key) DO NOTHING 是建表种子，不受影响。

DROP INDEX IF EXISTS uq_quota_upstreams_key;

CREATE UNIQUE INDEX IF NOT EXISTS uq_quota_upstreams_key
    ON quota_upstreams (key)
    WHERE deleted_at IS NULL;
