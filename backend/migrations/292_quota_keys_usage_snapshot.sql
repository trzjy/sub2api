-- quota_keys 新增 usage_snapshot：来源（供应商 lookup）额度的最近观测快照（方案 §25.6 / §25.14-C）。
--
-- 背景（唯一事实源）：来源额度是供应商 lookup 给的合并 total/used，不是本站账单余额；
-- 本列是它在 quota_keys 上的唯一落点。禁止借 notes、settings 或账号
-- extra.key_quota_snapshot 承载来源额度归属（账号快照是旧复制链，按 §25.6 停止写入，
-- 且不得从旧快照导入为本列的新鲜事实）。
--
-- 快照内容口径（本迁移只建存储结构，不写业务逻辑、不回填数据）：
--   · 最近一次成功观测：total / used / status_code / checked_at；
--   · 最近一次观测的 attempted_at 与 error_code（成功时 error_code 缺失）；
--   · 查询失败不写 0、不抹掉最近成功数值——无成功值时数值字段缺失而非 0；
--   · 期限仍只写既有 expires_at，不复制进快照；exhausted / last_refresh_error 属拉号维度
--     事实，与来源额度互不覆盖。
--
-- 可空 / 向前兼容：存量行一律 NULL = 尚未观测；旧镜像不认识该列，读到 NULL 不受影响，
-- 不需要回滚即兼容（发布后由正式 lookup 填入）。
--
-- 可重入：ADD COLUMN IF NOT EXISTS，重复执行幂等。
-- 可逆：纯 DDL、不触存量数据，删除列即完全还原。
--
-- 还原 SQL：
--   ALTER TABLE quota_keys DROP COLUMN IF EXISTS usage_snapshot;

ALTER TABLE quota_keys
    ADD COLUMN IF NOT EXISTS usage_snapshot JSONB NULL;
