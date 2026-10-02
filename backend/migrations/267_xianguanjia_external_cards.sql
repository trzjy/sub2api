-- 闲管家（开放平台）对接：外部卡密映射表 + 推送幂等去重表。
--
-- 背景（派发单 C1）：闲管家推送 webhook 时携带订单卡密与外部售后/退款状态，
--   - xianyu_external_cards 用于把闲管家侧卡密映射落库（与既有 xianyu_order_claims 领取记录关联）；
--   - xianguanjia_push_receipts 用于推送幂等去重（C1-a）：同一 (order_no, refund_status,
--     order_status, modify_time) 组合重复推送时直接 2xx 返回，避免重复作废。
-- card_pwd_encrypted 为加密后卡密，避免明文落库（真实加密须接入仓库既有凭证加密）。
--
-- 本迁移仅随代码提交，不自动应用到生产库；启用前需 DBA 评审。
-- 可重入：使用 CREATE TABLE IF NOT EXISTS；主键冲突由 ON CONFLICT DO NOTHING / UPSERT 处理。

CREATE TABLE IF NOT EXISTS xianyu_external_cards (
    order_no           text NOT NULL,
    card_no            text NOT NULL,
    card_pwd_encrypted text NOT NULL,
    sold_type          text NOT NULL DEFAULT '',
    voided_at          timestamptz NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (order_no, card_no)
);

CREATE TABLE IF NOT EXISTS xianguanjia_push_receipts (
    order_no      text NOT NULL,
    refund_status text NOT NULL DEFAULT '',
    order_status  text NOT NULL DEFAULT '',
    modify_time   text NOT NULL DEFAULT '',
    received_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (order_no, refund_status, order_status, modify_time)
);
