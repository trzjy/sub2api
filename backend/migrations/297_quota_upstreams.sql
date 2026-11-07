-- cursor 号池上游注册表（CARD-KC-16）：多上游（ShareAI / kn-cy 等）共用一套号池，
-- 每条上游有稳定标识（不可变，引用目标）+ 可编辑显示名 + 类型 + 续费链接。
--
-- 背景：cursor 平台已有一个接通的号池上游 ShareAI（激活码 → 拉号 → cursor oauth 账号），
-- 现在要接第二个上游 kn-cy.com（第三方 OpenAI 兼容网关）。用户裁定（R2/R3）：不同上游要
-- 区分显示、带上名称与标识，且名称可编辑，粒度 = 全局上游条目。故引入 quota_upstreams 注册表，
-- quota_keys 通过 upstream_key（默认 shareai）引用其 key。
--
-- 兼容性（重入 / 回滚）：
--   - 全程 IF NOT EXISTS / IF EXISTS / ON CONFLICT DO NOTHING，迁移可重入（幂等空操作）。
--   - 末尾注释含回滚语句（仅在需要时手动执行）。
--   - 本文件是 ent/schema/quota_upstream.go 与 quota_key.go 的 SQL 落地，字段/类型/默认值逐字对应。
--   - quota_keys 增列也带 DEFAULT 'shareai'，存量行在不跑迁移时也语义正确（双保险）。

-- 1) 上游注册表：quota_upstreams
CREATE TABLE IF NOT EXISTS quota_upstreams (
    id           BIGSERIAL PRIMARY KEY,
    key          VARCHAR(64)  NOT NULL,
    name         VARCHAR(100) NOT NULL,
    type         VARCHAR(32)  NOT NULL,
    renewal_url  VARCHAR(500),
    notes        TEXT,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at   TIMESTAMPTZ
);

-- key 唯一索引（上游稳定标识，引用目标；key 非 NULL 故整体唯一）
CREATE UNIQUE INDEX IF NOT EXISTS uq_quota_upstreams_key
    ON quota_upstreams(key);

-- type 单索引：便于按上游类型查（决定用哪个拉号客户端）
CREATE INDEX IF NOT EXISTS idx_quota_upstreams_type
    ON quota_upstreams(type);

-- type CHECK：限定取值集合（shareai / kn_cy）
ALTER TABLE quota_upstreams
    DROP CONSTRAINT IF EXISTS chk_quota_upstreams_type;
ALTER TABLE quota_upstreams
    ADD CONSTRAINT chk_quota_upstreams_type
    CHECK (type IN ('shareai', 'kn_cy'));

-- 2) quota_keys 增上游标识列（默认 shareai；存量行一次性回填为 shareai）
ALTER TABLE quota_keys ADD COLUMN IF NOT EXISTS upstream_key VARCHAR(64) NOT NULL DEFAULT 'shareai';

-- upstream_key 普通索引（便于按上游查，非唯一）
CREATE INDEX IF NOT EXISTS idx_quota_keys_upstream_key
    ON quota_keys(upstream_key);

-- 3) 种子：插入两条上游条目（幂等：ON CONFLICT (key) DO NOTHING）
INSERT INTO quota_upstreams (key, name, type, renewal_url, created_at, updated_at) VALUES
  ('shareai', 'ShareAI 号池', 'shareai', '', now(), now()),
  ('kn_cy', 'kn-cy.com 网关', 'kn_cy', 'https://shop.kn-cy.com/products', now(), now())
ON CONFLICT (key) DO NOTHING;

-- 回滚（仅在需要时手动执行）：
-- BEGIN;
-- ALTER TABLE quota_keys DROP COLUMN IF EXISTS upstream_key;
-- DROP INDEX IF EXISTS idx_quota_upstreams_type;
-- DROP INDEX IF EXISTS uq_quota_upstreams_key;
-- DROP TABLE IF EXISTS quota_upstreams;
-- COMMIT;
