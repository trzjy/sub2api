-- 232_xianyu_control_plane.sql
-- Xianyu 自动发货主程序统一控制面：数据模型与约束。
-- 该迁移是前向一次性迁移：创建 Worker 连接、闲鱼账号、商品池、商品映射、
-- 绑定规则表，并扩展 xianyu_order_claims 以支持发货状态闭环。

-- ---------------------------------------------------------------------------
-- 4.1 Worker 连接配置
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS xianyu_worker_configs (
    id                  BIGSERIAL PRIMARY KEY,
    base_url            VARCHAR(255) NOT NULL,
    api_token_encrypted TEXT NOT NULL,
    status              VARCHAR(16) NOT NULL DEFAULT 'disabled',
    health_status       VARCHAR(16) NOT NULL DEFAULT 'unknown',
    last_checked_at     TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 只允许一条 active Worker 配置；使用 PostgreSQL 部分唯一索引，不只依赖应用层检查。
CREATE UNIQUE INDEX IF NOT EXISTS uq_xianyu_worker_configs_active
    ON xianyu_worker_configs (status)
    WHERE status = 'active';

-- ---------------------------------------------------------------------------
-- 4.2 闲鱼账号
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS xianyu_accounts (
    id              BIGSERIAL PRIMARY KEY,
    worker_config_id BIGINT NOT NULL REFERENCES xianyu_worker_configs(id) ON DELETE RESTRICT,
    account_id      VARCHAR(80) NOT NULL,
    nickname        VARCHAR(120) NOT NULL DEFAULT '',
    status          VARCHAR(16) NOT NULL DEFAULT 'disabled',
    cookie_status   VARCHAR(16) NOT NULL DEFAULT 'unknown',
    task_status     VARCHAR(16) NOT NULL DEFAULT 'unknown',
    last_login_at   TIMESTAMPTZ,
    last_seen_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_xianyu_accounts_worker_account UNIQUE (worker_config_id, account_id)
);

CREATE INDEX IF NOT EXISTS idx_xianyu_accounts_worker
    ON xianyu_accounts (worker_config_id);

-- ---------------------------------------------------------------------------
-- 4.3 商品池
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS xianyu_item_pools (
    id                  BIGSERIAL PRIMARY KEY,
    name                VARCHAR(120) NOT NULL,
    slug                VARCHAR(80) NOT NULL,
    description         TEXT NOT NULL DEFAULT '',
    low_stock_threshold INT NOT NULL DEFAULT 0 CHECK (low_stock_threshold >= 0),
    status              VARCHAR(16) NOT NULL DEFAULT 'active',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_xianyu_item_pools_slug UNIQUE (slug)
);

-- ---------------------------------------------------------------------------
-- 4.4 商品映射与自动绑定
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS xianyu_products (
    id              BIGSERIAL PRIMARY KEY,
    account_pk      BIGINT NOT NULL REFERENCES xianyu_accounts(id) ON DELETE RESTRICT,
    account_id      VARCHAR(80) NOT NULL,
    item_id         VARCHAR(64) NOT NULL,
    title           VARCHAR(255) NOT NULL DEFAULT '',
    spec_name       VARCHAR(64) NOT NULL DEFAULT '',
    spec_value      VARCHAR(64) NOT NULL DEFAULT '',
    pool_id         BIGINT REFERENCES xianyu_item_pools(id) ON DELETE SET NULL,
    binding_status  VARCHAR(16) NOT NULL DEFAULT 'unmapped',
    binding_source  VARCHAR(24) NOT NULL DEFAULT 'auto_new',
    status          VARCHAR(16) NOT NULL DEFAULT 'active',
    last_seen_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_xianyu_products_identity UNIQUE (account_pk, item_id, spec_name, spec_value),
    CONSTRAINT chk_xianyu_products_binding_pool CHECK (
        (binding_status = 'mapped' AND pool_id IS NOT NULL)
        OR (binding_status = 'unmapped' AND pool_id IS NULL)
        OR (binding_status NOT IN ('mapped', 'unmapped'))
    )
);

CREATE INDEX IF NOT EXISTS idx_xianyu_products_account
    ON xianyu_products (account_pk, status);

CREATE TABLE IF NOT EXISTS xianyu_binding_rules (
    id          BIGSERIAL PRIMARY KEY,
    priority    INT NOT NULL DEFAULT 0,
    account_pk  BIGINT NOT NULL REFERENCES xianyu_accounts(id) ON DELETE CASCADE,
    match_type  VARCHAR(24) NOT NULL,
    keyword     VARCHAR(255) NOT NULL DEFAULT '',
    pool_id     BIGINT NOT NULL REFERENCES xianyu_item_pools(id) ON DELETE RESTRICT,
    status      VARCHAR(16) NOT NULL DEFAULT 'active',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_xianyu_binding_rules_keyword CHECK (
        (match_type = 'keyword' AND btrim(keyword) <> '')
        OR match_type = 'account_default'
    )
);

CREATE INDEX IF NOT EXISTS idx_xianyu_binding_rules_account
    ON xianyu_binding_rules (account_pk, status, priority, id);

-- ---------------------------------------------------------------------------
-- 4.5 发货记录扩展
-- ---------------------------------------------------------------------------
ALTER TABLE xianyu_order_claims
    ADD COLUMN IF NOT EXISTS product_id       BIGINT REFERENCES xianyu_products(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS pool_id          BIGINT REFERENCES xianyu_item_pools(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS binding_source   VARCHAR(24),
    ADD COLUMN IF NOT EXISTS delivery_status  VARCHAR(24) NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS delivery_error   TEXT,
    ADD COLUMN IF NOT EXISTS attempt_count    INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_attempt_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW();

CREATE INDEX IF NOT EXISTS idx_xianyu_order_claims_delivery_status
    ON xianyu_order_claims (delivery_status, last_attempt_at);

CREATE INDEX IF NOT EXISTS idx_xianyu_order_claims_pool
    ON xianyu_order_claims (pool_id);

CREATE INDEX IF NOT EXISTS idx_xianyu_order_claims_product
    ON xianyu_order_claims (product_id);
