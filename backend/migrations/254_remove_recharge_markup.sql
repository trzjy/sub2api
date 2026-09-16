-- 254_remove_recharge_markup.sql
-- 移除充值加价系数 RECHARGE_MARKUP 设置。
-- 充值到账改为：实付金额按汇率折算 USD，充值多少折合多少，不再乘加价系数。
-- migrated 0.14 markup 的站点到账行为将按 1.0（汇率平价）生效。

DELETE FROM settings WHERE key = 'RECHARGE_MARKUP';
