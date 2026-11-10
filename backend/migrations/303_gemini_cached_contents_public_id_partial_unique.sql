-- 303_gemini_cached_contents_public_id_partial_unique.sql
-- 修复：idx_gemini_cached_contents_public_id 未排除软删行，软删的缓存条目永久占用 public_id 唯一坑位。
-- 改为部分唯一索引：仅对未软删行强制唯一。
--
-- 本表无 ent schema（纯 raw SQL 仓储），且无 ON CONFLICT 依赖该索引，
-- 因此只需重建索引，无需同步任何 Go 侧声明。

DROP INDEX IF EXISTS idx_gemini_cached_contents_public_id;

CREATE UNIQUE INDEX IF NOT EXISTS idx_gemini_cached_contents_public_id
    ON gemini_cached_contents (public_id)
    WHERE deleted_at IS NULL;
