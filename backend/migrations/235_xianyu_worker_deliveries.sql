-- 235_xianyu_worker_deliveries.sql
-- Worker 自动发货订单级记录：主程序按 order_no 幂等关联 Worker 发货结果。
-- 只记录订单级汇总（数量/状态/错误），不复制 Worker 卡券内容或卡券模型。
-- Worker 保持本地库存与逐份发货实现；两边只通过订单号、数量和结果回传做幂等关联。

CREATE TABLE IF NOT EXISTS xianyu_worker_deliveries (
    order_no        VARCHAR(64) PRIMARY KEY,
    delivery_kind   VARCHAR(24) NOT NULL DEFAULT 'auto',   -- auto/manual/redelivery
    quantity        INT         NOT NULL DEFAULT 1,        -- 订单数量
    quantity_sent   INT         NOT NULL DEFAULT 0,        -- 已实际发送数量（订单级汇总）
    delivery_status VARCHAR(24) NOT NULL DEFAULT 'pending',-- pending/sent/failed/legacy_unverified
    delivery_error  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_xianyu_worker_deliveries_status
    ON xianyu_worker_deliveries (delivery_status, updated_at);
