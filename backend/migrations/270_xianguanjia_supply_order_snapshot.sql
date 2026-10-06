-- 270_xianguanjia_supply_order_snapshot.sql
-- 货源订单金额快照三列（D6E-02R #3 外审 addendum 整改）。
--
-- 背景（外审 addendum gpt-5.6-sol 快照 2026-10-04T04-15-33Z，P1 第 1 条 must_fix）：
--   原 270 曾以「非空约束 + 零值默认（空串与 0）」给既有订单行伪造空商品名/零金额快照，
--   已审定的查单/退款只读快照路径会把历史订单迁成 refund_amount=0 伪成功，
--   违背「杜绝漂移、绝不为 0」锁定语义。本整改废除零默认，改为在仍可改写的
--   270 内从权威数据源确定性回填既有行；无法推导时令迁移失败关闭，回填校验
--   后 SET NOT NULL，不留空值/零值默认。
--
-- 整改语义（本文件相对 269 的既有订单行做一次性回填）：
--   1) 回填权威源 = 商品目录同源表达式（supply_catalog.go supplyGoodsSelect）：
--        goods_no    = "groups".id 十进制串（订单行已有，仅用于 JOIN 定位）；
--        goods_name  = groups.name；
--        unit_price  = COALESCE((SELECT sp.price_cny FROM subscription_plans sp
--                      WHERE sp.group_id = g.id ORDER BY sp.sort_order, sp.id LIMIT 1), 0)；
--        order_amount= unit_price × quantity（快照金额，退款 refund_amount 同源，绝不为 0）。
--   2) 两级失败关闭（任一级命中即 RAISE EXCEPTION，迁移失败关闭，绝不静默放行）：
--        a. 回填前置校验：既有行 goods_no 非纯数字 → 无法定位商品源，迁移失败；
--        b. 回填后置校验：回填后仍有 goods_name 为空 或 unit_price/order_amount
--           为 NULL 或 <=0 的行 → 商品缺失、单价为零、或零/负金额快照均属无法推导
--           真实历史单价/金额，禁止以 0 快照放行（这正是本整改要堵的资金口子）。
--           快照金额绝不为 0：order_amount<=0（含 NULL）一律拒迁。
--   3) SET NOT NULL 无默认：三列先可空加列、回填校验通过后统一加 NOT NULL，
--      新写入路径 InsertCreating 恒携带三值，不受影响。
--
-- 字段说明（快照用途，查单/退款只读，不再实时聚合）：
--   goods_name   下单时商品名（TEXT），查单对外 goods_name 直接取自快照；
--   unit_price   单价（BIGINT 分），下单时商品单价快照；
--   order_amount 订单金额（BIGINT 分）= unit_price × quantity，退款 refund_amount
--               亦取此快照（不再实时聚合，绝不为 0）。
--
-- ⚠️ 268/269 迁移已应用且不可变，本文件为独立新增，一字不动它们。
-- ⚠️ 270 尚未在任何环境应用，可自由改写本文件。

-- 1) 三列先可空加列（不带 DEFAULT、不带 NOT NULL），可重入 ADD COLUMN IF NOT EXISTS。
ALTER TABLE xianguanjia_supply_orders
    ADD COLUMN IF NOT EXISTS goods_name   TEXT,
    ADD COLUMN IF NOT EXISTS unit_price    BIGINT,
    ADD COLUMN IF NOT EXISTS order_amount  BIGINT;

-- 2) 回填前置校验：既有行 goods_no 非纯数字 → 无法定位商品源，迁移失败关闭。
DO $$
DECLARE
    v_cnt    INTEGER;
    v_sample TEXT;
BEGIN
    SELECT count(*) INTO v_cnt
    FROM xianguanjia_supply_orders
    WHERE goods_no IS NULL OR goods_no !~ '^[0-9]+$';

    IF v_cnt > 0 THEN
        SELECT goods_no INTO v_sample
        FROM xianguanjia_supply_orders
        WHERE goods_no IS NULL OR goods_no !~ '^[0-9]+$'
        LIMIT 1;
        RAISE EXCEPTION '270 迁移失败关闭（前置校验）：存在 % 行 goods_no 非纯数字或为空（样例 goods_no=%），无法从商品目录推导商品源', v_cnt, v_sample;
    END IF;
END $$;

-- 3) 回填 UPDATE（仅填 goods_name IS NULL 的既有行），JOIN 条件与 GetGoods 同语义：
--    g.deleted_at IS NULL AND g.id = o.goods_no::bigint。
UPDATE xianguanjia_supply_orders o
SET goods_name = g.name,
    unit_price = COALESCE((
        SELECT sp.price_cny
        FROM subscription_plans sp
        WHERE sp.group_id = g.id
        ORDER BY sp.sort_order, sp.id
        LIMIT 1
    ), 0),
    order_amount = COALESCE((
        SELECT sp.price_cny
        FROM subscription_plans sp
        WHERE sp.group_id = g.id
        ORDER BY sp.sort_order, sp.id
        LIMIT 1
    ), 0) * o.quantity
FROM "groups" g
WHERE g.deleted_at IS NULL
  AND g.id = o.goods_no::bigint
  AND o.goods_name IS NULL;

-- 4) 回填后置校验：仍有 goods_name 为空、unit_price/order_amount 为 NULL 或 <=0 的
--    未回填行 → 失败关闭。商品缺失、单价为零、或零/负金额快照均属无法推导真实历史
--    单价/金额，快照金额绝不为 0，零/负金额订单禁止放行，不得以 0 快照放行。
DO $$
DECLARE
    v_cnt    INTEGER;
    v_sample TEXT;
BEGIN
    SELECT count(*) INTO v_cnt
    FROM xianguanjia_supply_orders
    WHERE goods_name IS NULL OR unit_price IS NULL OR unit_price <= 0
        OR order_amount IS NULL OR order_amount <= 0;

    IF v_cnt > 0 THEN
        SELECT goods_no INTO v_sample
        FROM xianguanjia_supply_orders
        WHERE goods_name IS NULL OR unit_price IS NULL OR unit_price <= 0
            OR order_amount IS NULL OR order_amount <= 0
        LIMIT 1;
        RAISE EXCEPTION '270 迁移失败关闭（后置校验）：回填后仍有 % 行 goods_name 为空、或 unit_price/order_amount 为 NULL 或 <=0（样例 goods_no=%），快照金额绝不为 0，零/负金额订单禁止放行，无法推导真实历史单价/金额，禁止以 0 快照放行', v_cnt, v_sample;
    END IF;
END $$;

-- 5) 三列 SET NOT NULL（无默认值）；新写入路径 InsertCreating 恒携带三值，不受影响。
ALTER TABLE xianguanjia_supply_orders
    ALTER COLUMN goods_name  SET NOT NULL,
    ALTER COLUMN unit_price   SET NOT NULL,
    ALTER COLUMN order_amount SET NOT NULL;
