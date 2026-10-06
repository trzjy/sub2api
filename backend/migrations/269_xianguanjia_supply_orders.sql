-- 269_xianguanjia_supply_orders.sql
-- 闲管家「虚拟货源」方向的货源订单表（D6d 卡密订单接口）。
--
-- 背景（总单 /root/dispatch-D6.md / 派发单 D6D）：
--   买家付款后闲管家调「创建卡密订单」，我方同步从卡密池取卡并返回 card_items；
--   退款时闲管家调「订单退款通知」，我方作废对应订单的卡（delivered→expired）。
--   本表持久化「管家订单号 → 已发卡号」的映射，作为两头需求的基石：
--     1) 幂等：同一 manager_order_no 只允许存在一行（UNIQUE 约束），任何路径
--        不得对同一订单号二次发卡（资金红线）；
--     2) 查单：查询订单详情需按管家订单号回读状态与卡密（幂等查询）；
--     3) 退款：退款通知需按订单号定位到具体卡号，再精准作废（复用 D2 语义）。
--
-- 字段说明：
--   manager_order_no 管家订单号（唯一），幂等锚点；
--   goods_no         商品编号（来自卡密池商品维度，D6c 映射 goods_no=分组ID）；
--   quantity         本单成功发出的卡数量（幂等重发时据此回读，不重算）；
--   status           20=成功（官方 order_status=20），30=已退款；10 为内部占位态
--                    （已预留订单行、取卡中，仅存在于写入事务窗口，不回读为成功）；
--   card_nos         已发卡号数组（jsonb），幂等查单/退款作废的数据源；
--   refunded_at      退款通知到达时间；未退款为 NULL。
--
-- 本迁移仅随代码提交，不自动应用到生产库；启用前需 DBA 评审。可重入：CREATE TABLE IF NOT EXISTS。
--
-- ⚠️ 与 268_xianguanjia_supply.sql（D6a 货源凭证列，若采用同号迁移）同号不同名，
--    由集成者确认最终编号次序；本文件为 D6d 独立交付，不引用 D6a 的列。

CREATE TABLE IF NOT EXISTS xianguanjia_supply_orders (
    id               BIGSERIAL PRIMARY KEY,
    manager_order_no TEXT NOT NULL,
    goods_no         TEXT NOT NULL DEFAULT '',
    quantity         INT NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    status           INT NOT NULL DEFAULT 20,
    card_nos         JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    refunded_at      TIMESTAMPTZ NULL,
    CONSTRAINT uq_xianguanjia_supply_orders_manager_order_no UNIQUE (manager_order_no)
);

CREATE INDEX IF NOT EXISTS idx_xianguanjia_supply_orders_status
    ON xianguanjia_supply_orders (status);
