-- Migration: 259_account_model_capabilities
-- 图片能力分流方案（docs/capability-routing-plan.md §3.3）能力标记表。
--
-- 能力是 (账号, 上游模型, 协议) 三元组属性：同一账号不同模型、同一模型不同协议
-- 的能力不同（已实测：infer 在 chat_completions 下不解析图片、返回 200+正常文本，
-- 网关拿不到错误信号；星思云站三种协议均可读图）。因此必须独立表存储，且协议维度不可省。
--
-- 独立于 accounts.credentials：避免与凭据密文混存、写放大与并发冲突（外审重要1）。
--
-- 幂等（IF NOT EXISTS），不触碰任何已有表。

-- 1) 能力标记表
CREATE TABLE IF NOT EXISTS account_model_capabilities (
    id              BIGSERIAL    PRIMARY KEY,
    account_id      BIGINT       NOT NULL,
    upstream_model  VARCHAR(200) NOT NULL,
    protocol        VARCHAR(50)  NOT NULL,
    supports_vision BOOLEAN      NOT NULL DEFAULT FALSE,
    source          VARCHAR(20)  NOT NULL DEFAULT 'detect',
    detected_at     TIMESTAMPTZ  NULL,
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- 三元组唯一：同一账号同一模型同一协议只保留一条标记，Upsert 依此收敛。
    CONSTRAINT account_model_capabilities_unique
        UNIQUE (account_id, upstream_model, protocol),
    -- 来源可追溯：detect=一键检测落库 / manual=人工覆盖
    CONSTRAINT account_model_capabilities_source_check
        CHECK (source IN ('detect', 'manual')),
    -- 协议维度枚举
    CONSTRAINT account_model_capabilities_protocol_check
        CHECK (protocol IN ('chat_completions', 'responses', 'anthropic')),
    -- 上游模型名不允许空白
    CONSTRAINT account_model_capabilities_upstream_model_check
        CHECK (btrim(upstream_model) <> '')
);

-- 账号删除时级联清理能力标记（避免孤儿标记参与调度过滤）
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'account_model_capabilities_account_id_fkey'
    ) THEN
        ALTER TABLE account_model_capabilities
            ADD CONSTRAINT account_model_capabilities_account_id_fkey
            FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE;
    END IF;
END$$;

-- ListByAccount 热路径：按账号取全量标记
CREATE INDEX IF NOT EXISTS idx_account_model_capabilities_account
    ON account_model_capabilities (account_id);

-- 运维视图：按能力结论筛选（如批量收敛 non_vision）
CREATE INDEX IF NOT EXISTS idx_account_model_capabilities_supports_vision
    ON account_model_capabilities (supports_vision);

COMMENT ON TABLE account_model_capabilities IS
    'Per (account, upstream_model, protocol) upstream capability marks; no record = unknown (read path returns known=false)';
COMMENT ON COLUMN account_model_capabilities.protocol IS
    'Protocol dimension: chat_completions / responses / anthropic. Required to avoid cross-protocol contamination.';
COMMENT ON COLUMN account_model_capabilities.source IS
    'detect = one-click captcha detection; manual = administrator override.';
COMMENT ON COLUMN account_model_capabilities.detected_at IS
    'Last successful detection time; NULL for pure manual overrides.';
