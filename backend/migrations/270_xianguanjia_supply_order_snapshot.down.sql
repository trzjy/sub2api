-- 270_xianguanjia_supply_order_snapshot.down.sql
-- 270 号迁移（货源订单金额快照三列）的 down 脚本：仅做结构回滚。
--
-- ⚠️ 结构回滚丢弃 goods_name / unit_price / order_amount 三列（含已落库快照），
--    生产回滚以迁移前 pg_dump 备份恢复为唯一手段；本文件仅用于开发/测试环境结构清理。
-- 说明：迁移器按文件名字典序执行内嵌 *.sql（含本 down 文件，".down.sql" 排在
--      同名 ".sql" 之前），故 down 的 DROP 先于 up 的 ADD 执行，为无操作；
--      真正的回滚由运维按需手工执行本文件。

ALTER TABLE xianguanjia_supply_orders
    DROP COLUMN IF EXISTS goods_name,
    DROP COLUMN IF EXISTS unit_price,
    DROP COLUMN IF EXISTS order_amount;
