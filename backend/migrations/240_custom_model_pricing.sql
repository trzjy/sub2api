-- 240: 全局自定义模型定价层（价格管理中心）
--
-- custom_model_pricing 是介于渠道价与远程同步表之间的全局价格覆盖层：
-- 分组价 → 渠道价 → 自定义价(本表) → 远程 LiteLLM 表 → 代码内置兜底。
-- 列结构对齐 channel_model_pricing 系列（去掉 channel_id），增加 enabled/remark/created_by。
-- 远程价格表同步只写 data/model_pricing.json 缓存文件，永不触碰本表，手改不会被覆盖。

CREATE TABLE IF NOT EXISTS custom_model_pricing (
    id                 BIGSERIAL      PRIMARY KEY,
    models             JSONB          NOT NULL DEFAULT '[]',
    billing_mode       VARCHAR(32)    NOT NULL DEFAULT 'token',
    input_price        NUMERIC(20,12),
    output_price       NUMERIC(20,12),
    cache_write_price  NUMERIC(20,12),
    cache_read_price   NUMERIC(20,12),
    fast_multiplier    NUMERIC(10,6),
    flex_multiplier    NUMERIC(10,6),
    image_input_price  NUMERIC(20,12),
    image_output_price NUMERIC(20,12),
    per_request_price  NUMERIC(20,12),
    enabled            BOOLEAN        NOT NULL DEFAULT TRUE,
    remark             TEXT           NOT NULL DEFAULT '',
    created_by         BIGINT,
    created_at         TIMESTAMPTZ    NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ    NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_custom_model_pricing_enabled ON custom_model_pricing (enabled);

CREATE TABLE IF NOT EXISTS custom_model_pricing_intervals (
    id                BIGSERIAL      PRIMARY KEY,
    pricing_id        BIGINT         NOT NULL REFERENCES custom_model_pricing(id) ON DELETE CASCADE,
    min_tokens        INT            NOT NULL DEFAULT 0,
    max_tokens        INT,
    tier_label        VARCHAR(50),
    input_price       NUMERIC(20,12),
    output_price      NUMERIC(20,12),
    cache_write_price NUMERIC(20,12),
    cache_read_price  NUMERIC(20,12),
    input_multiplier  NUMERIC(12,6),
    output_multiplier NUMERIC(12,6),
    cache_write_multiplier NUMERIC(12,6),
    cache_read_multiplier  NUMERIC(12,6),
    per_request_price NUMERIC(20,12),
    sort_order        INT            NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ    NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ    NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_custom_model_pricing_intervals_pricing_id
    ON custom_model_pricing_intervals (pricing_id);

COMMENT ON TABLE custom_model_pricing IS '全局自定义模型定价：价格管理中心维护，优先级介于渠道价与远程同步表之间';
COMMENT ON COLUMN custom_model_pricing.models IS '生效模型列表，JSON 数组，支持 * 后缀通配，如 ["gpt-5.6-sol","my-alias-*"]';
COMMENT ON COLUMN custom_model_pricing.billing_mode IS '计费模式：token / per_request / image / video';
COMMENT ON COLUMN custom_model_pricing.enabled IS '启用开关：禁用条目不参与查价但保留配置';
COMMENT ON COLUMN custom_model_pricing.remark IS '备注：如补价原因、官方调价公告链接';
COMMENT ON COLUMN custom_model_pricing.created_by IS '创建管理员 ID';
COMMENT ON TABLE custom_model_pricing_intervals IS '自定义定价区间：token 区间 / 按次分层 / 图片分辨率分层';
