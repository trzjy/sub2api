-- D-CURSOR-014: 为 quota_keys 补充软删除能力（deleted_at）。
--
-- 与 accounts 表保持一致的软删口径（v3.4 §3.5）：账号/激活码"删除" = 号池摘除 +
-- 前端不显示 + 停止调度，数据库行与全部历史记录保留，禁止物理 DELETE。
-- SoftDeleteMixin 的查询拦截器会自动过滤 deleted_at 非 NULL 的行，故普通列表/号池
-- 查询天然排除已软删记录。

ALTER TABLE quota_keys ADD COLUMN IF NOT EXISTS deleted_at timestamptz NULL;

-- 软删后的回收站/审计扫描常按 deleted_at 过滤，加索引加速；部分索引仅覆盖已删除行以减小体积。
CREATE INDEX IF NOT EXISTS idx_quota_keys_deleted_at ON quota_keys (deleted_at);
