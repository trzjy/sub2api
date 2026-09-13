-- 247_promo_intel.sql
-- 优惠情报中心：厂商官方优惠/公告资讯源的每日轮询 + LLM 结构化整理 + 管理台简报。
-- 1) promo_intel_sources 资讯源：厂商官方公告/更新日志/价格/活动页轮询配置；
--    last_extracted_hash 记录上次已成功进入整理层的内容指纹——指纹未变跳过抓取
--    后续流程，LLM 恢复可用后凭「内容指纹 ≠ 已整理指纹」自动补跑，不丢数据；
-- 2) promo_intel_items 情报条目：LLM 结构化提取结果（或 LLM 未配置/失败时降级的
--    「原文待整理」条目）。fingerprint 全局唯一去重：同一优惠被同/跨源重复发现时
--    刷新既有行（last_seen 随 updated_at 体现），绝不重复汇报。

CREATE TABLE IF NOT EXISTS promo_intel_sources (
    id                   BIGSERIAL PRIMARY KEY,
    name                 VARCHAR(100) NOT NULL,
    vendor               VARCHAR(50) NOT NULL DEFAULT 'other',
    category             VARCHAR(32) NOT NULL DEFAULT 'announcement',
    url                  TEXT NOT NULL DEFAULT '',
    fetch_interval_minutes INTEGER NOT NULL DEFAULT 1440,
    enabled              BOOLEAN NOT NULL DEFAULT TRUE,
    llm_extract          BOOLEAN NOT NULL DEFAULT TRUE,
    notes                TEXT NOT NULL DEFAULT '',
    last_fetched_at      TIMESTAMPTZ,
    last_extracted_hash  VARCHAR(64) NOT NULL DEFAULT '',
    last_status          VARCHAR(16) NOT NULL DEFAULT '',
    last_error           TEXT NOT NULL DEFAULT '',
    created_by           BIGINT NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 种子源幂等补种与重命名冲突防护都依赖 name 唯一。
CREATE UNIQUE INDEX IF NOT EXISTS idx_promo_intel_sources_name
    ON promo_intel_sources (name);
-- 轮询循环扫描：enabled + last_fetched_at 判到期。
CREATE INDEX IF NOT EXISTS idx_promo_intel_sources_enabled_last_fetched
    ON promo_intel_sources (enabled, last_fetched_at);

CREATE TABLE IF NOT EXISTS promo_intel_items (
    id                BIGSERIAL PRIMARY KEY,
    source_id         BIGINT NOT NULL REFERENCES promo_intel_sources(id) ON DELETE CASCADE,
    vendor            VARCHAR(50) NOT NULL DEFAULT 'other',
    category          VARCHAR(32) NOT NULL DEFAULT 'other',
    title             TEXT NOT NULL DEFAULT '',
    summary           TEXT NOT NULL DEFAULT '',
    details           TEXT NOT NULL DEFAULT '',
    discount_info     TEXT NOT NULL DEFAULT '',
    valid_until       TEXT NOT NULL DEFAULT '',
    url               TEXT NOT NULL DEFAULT '',
    relevance         VARCHAR(16) NOT NULL DEFAULT 'medium',
    status            VARCHAR(16) NOT NULL DEFAULT 'pending',
    fingerprint       VARCHAR(64) NOT NULL,
    raw_excerpt       TEXT NOT NULL DEFAULT '',
    extract_status    VARCHAR(16) NOT NULL DEFAULT 'llm',
    digest_date       DATE,
    source_fetched_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 同一优惠（同厂商同标题归一化）只报一次；再次发现走 Upsert 刷新。
CREATE UNIQUE INDEX IF NOT EXISTS idx_promo_intel_items_fingerprint
    ON promo_intel_items (fingerprint);
-- 收件箱主查询：状态 + 简报日期倒序；厂商/类型筛选次级。
CREATE INDEX IF NOT EXISTS idx_promo_intel_items_status_digest
    ON promo_intel_items (status, digest_date DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_promo_intel_items_vendor
    ON promo_intel_items (vendor);
CREATE INDEX IF NOT EXISTS idx_promo_intel_items_source
    ON promo_intel_items (source_id);

COMMENT ON COLUMN promo_intel_sources.last_extracted_hash IS '上次成功进入整理层的正文内容指纹（SHA256）；为空表示尚无成功整理记录，LLM 可用后会自动补跑';
COMMENT ON COLUMN promo_intel_items.extract_status IS 'llm=已结构化整理；pending=原文待整理（LLM 未配置或失败时的降级态，恢复后自动补跑）';
COMMENT ON COLUMN promo_intel_items.digest_date IS '首次发现日期（每日简报按此聚合）';
