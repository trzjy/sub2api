-- 252_xianyu_xianguanjia.sql
-- 闲管家开放平台对接：配置表（单条活跃记录）。
--
-- 对接模型（官方 open.goofish.pro 文档）：闲鱼账号由商家在闲管家后台预先绑定
-- 并授权；开放平台按 app_id/app_secret（+ mch_id/mch_secret 货源商户）签名鉴权，
-- 提供查询已授权店铺、商品、订单、推送通知等能力。主程序通过该配置构建客户端。

CREATE TABLE IF NOT EXISTS xianyu_xianguanjia_config (
    id                   BIGSERIAL PRIMARY KEY,
    base_url             VARCHAR(255)  NOT NULL DEFAULT 'https://open.goofish.pro',
    app_id               VARCHAR(120)  NOT NULL DEFAULT '',
    app_secret_encrypted TEXT          NOT NULL DEFAULT '',
    mch_id               VARCHAR(120)  NOT NULL DEFAULT '',
    mch_secret_encrypted TEXT          NOT NULL DEFAULT '',
    push_url             VARCHAR(512)  NOT NULL DEFAULT '',
    status               VARCHAR(16)   NOT NULL DEFAULT 'disabled',
    health_status        VARCHAR(16)   NOT NULL DEFAULT 'unknown',
    last_checked_at      TIMESTAMPTZ,
    created_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

-- 只允许一条 active 配置（与 xianyu_worker_configs 语义对齐）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_xianyu_xianguanjia_config_active
    ON xianyu_xianguanjia_config (status)
    WHERE status = 'active';