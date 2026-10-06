-- 082_clear_xianyu_pool_notes.up.sql
-- 库存池统一口径方案（inventory-pool-unified-spec-counting，2026-09-17 批准）数据迁移（up）。
-- 统一口径：兑换码本身就是唯一库存事实，池归属由码自身 (group_id, validity_days, type='subscription')
-- 三个字段决定，不再依赖 redeem_codes.notes 上的 'xianyu_pool=<slug>' 历史标记。
-- 旧标记共 304 行，本迁移将其清空（旧链归零），使这些码归位为按规格自动纳管的普通可兑换订阅码，
-- 从而被对应 (分组 + 天数) 的库存池按规格统计剩余/已用/禁用，并被发货取码按规格取到。
-- 参考方案文档：docs/inventory-pool-unified-spec-counting-plan.md 第 4 节（数据迁移：旧链归零）。
-- 幂等：仅对仍带标记的码清空 notes，重复执行安全（第二次执行影响行数为 0）。

UPDATE redeem_codes
SET notes = ''
WHERE notes LIKE 'xianyu_pool=%';
