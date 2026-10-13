-- Cursor 站点渠道接入（v3.3 §4.1 / C1）— quota_keys 表 + 索引 + CHECK
--
-- 本迁移与 ent/migrate/schema.go 的 QuotaKeysTable/QuotaKeysColumns 自动生成保持一致：
--   - 11 字段（id / code_encrypted / code_mac / code_last4 / status / exhausted /
--     last_refresh_error / last_refresh_at / expires_at / created_at / updated_at / notes）
--   - 5 索引（code_mac 唯一 / status / expires_at 部分 / exhausted 部分 / reconciler 复合部分）
--   - 1 CHECK 约束（status 枚举）
--
-- 设计说明：
--   - code_mac 部分唯一索引（IS NOT NULL）：NULL 不参与唯一约束，允许 code_mac 留空（实施期定形）
--   - expires_at / exhausted 部分索引：仅索引热路径字段，节约空间
--   - reconciler 复合部分索引（status = 'active'）：C9 reconciler 周期扫表热路径
--   - status CHECK：4 状态枚举（active / exhausted / expired / disabled）
--
-- 兼容性（重入 / 回滚）：
--   - 所有 IF NOT EXISTS / IF EXISTS 保护，迁移可重入
--   - 表为新增表，回滚 = DROP TABLE（无存量数据）
--   - ent generate 的 schema.go 是 source of truth，本文件与 ent 字段一一对应
--
-- C0 阻塞项：#⑦ cursor 存量 / #⑧ usage_logs FK 行为 与本迁移无依赖（C2 处理）

CREATE TABLE IF NOT EXISTS quota_keys (
    id                  BIGSERIAL PRIMARY KEY,
    code_encrypted      VARCHAR(2048) NOT NULL,
    code_mac            VARCHAR(64),
    code_last4          VARCHAR(8),
    status              VARCHAR(20) NOT NULL DEFAULT 'active',
    exhausted           BOOLEAN NOT NULL DEFAULT false,
    last_refresh_error  VARCHAR(2000),
    last_refresh_at     TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    notes               TEXT
);

-- status CHECK 约束（4 状态枚举）
ALTER TABLE quota_keys
    DROP CONSTRAINT IF EXISTS chk_quota_keys_status;
ALTER TABLE quota_keys
    ADD CONSTRAINT chk_quota_keys_status
    CHECK (status IN ('active', 'exhausted', 'expired', 'disabled'));

-- code_mac 唯一索引（NULL 不参与唯一约束）
CREATE UNIQUE INDEX IF NOT EXISTS uq_quota_keys_code_mac
    ON quota_keys(code_mac)
    WHERE code_mac IS NOT NULL;

-- status 普通索引
CREATE INDEX IF NOT EXISTS idx_quota_keys_status
    ON quota_keys(status);

-- expires_at 部分索引（仅非 NULL）
CREATE INDEX IF NOT EXISTS idx_quota_keys_expires_at
    ON quota_keys(expires_at)
    WHERE expires_at IS NOT NULL;

-- exhausted 部分索引（仅 true）
CREATE INDEX IF NOT EXISTS idx_quota_keys_exhausted
    ON quota_keys(exhausted)
    WHERE exhausted = true;

-- reconciler 复合部分索引（C9 周期扫表热路径，v3.3 §3.4）
CREATE INDEX IF NOT EXISTS idx_quota_keys_reconciler
    ON quota_keys(status, exhausted, expires_at)
    WHERE status = 'active';
