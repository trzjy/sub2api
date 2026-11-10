-- 301_api_keys_key_partial_unique.sql
-- 修复：api_keys.key 的唯一性来自列约束 api_keys_key_key，不排除软删行，
-- 软删的 API Key 永久占用唯一坑位，同一 Key 删掉后无法重建。
-- PG 的 UNIQUE 约束不支持 WHERE 谓词，故改为部分唯一索引。
--
-- 注意：本迁移与 ent/schema/api_key.go 的 Indexes() 声明必须一致，
-- 并依赖 api_key_repo.go 的 translatePersistenceError 继续把唯一冲突翻成 ErrAPIKeyExists。

ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_key_key;
DROP INDEX IF EXISTS api_keys_key_key;
-- 迁移 011 已刻意删除 001 建的重复 btree 索引 idx_api_keys_key（它与旧唯一索引重复）；
-- 这里只做残留清理，不再重建：活跃行按 key 的等值查询由下面的部分唯一索引覆盖
-- （查询恒带 deleted_at IS NULL），模糊检索走 idx_api_keys_key_trgm。
DROP INDEX IF EXISTS idx_api_keys_key;

CREATE UNIQUE INDEX IF NOT EXISTS api_keys_key_active
    ON api_keys (key)
    WHERE deleted_at IS NULL;
