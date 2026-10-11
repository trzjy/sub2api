-- 261_usage_logs_platform_side_rows_nullable.sql
-- Vision 检测（方案 docs/capability-routing-plan.md §6）按 SSOT 口径在 usage_logs
-- 记录平台侧检测流量：user_id / api_key_id 为 NULL（不归属任何用户 / key，
-- 不进入用户账单）。原实现写入 0 哨兵，与生产 NOT NULL + FK 约束冲突导致
-- 检测 API 500（usage_logs_user_id_fkey）。
--
-- 放开两列 NOT NULL；FK 保留（PostgreSQL 外键对 NULL 天然放行，无需重建约束）。
-- 唯一索引 idx_usage_logs_request_id_api_key_unique (request_id, api_key_id)
-- 默认 NULLS DISTINCT，检测行（两列均为 NULL）互不冲突、也不与业务行冲突。
--
-- 幂等：ALTER ... DROP NOT NULL 重复执行无副作用。
ALTER TABLE usage_logs ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE usage_logs ALTER COLUMN api_key_id DROP NOT NULL;
