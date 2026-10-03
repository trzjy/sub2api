-- 268_xianguanjia_supply_orders.down.sql
-- 268 号迁移（货源订单表）的 down 脚本：仅做结构回滚。
--
-- ⚠️ 结构回滚丢弃 xianguanjia_supply_orders 全部行（含已发卡号映射），
--    生产回滚以迁移前 pg_dump 备份恢复为唯一手段；本文件仅用于开发/测试环境结构清理。
-- 说明：迁移器按文件名字典序执行内嵌 *.sql（含本 down 文件，".down.sql" 排在
--      同名 ".sql" 之前），故 down 的 DROP 先于 up 的 CREATE 执行，为无操作；
--      真正的回滚由运维按需手工执行本文件。

DROP TABLE IF EXISTS xianguanjia_supply_orders;
