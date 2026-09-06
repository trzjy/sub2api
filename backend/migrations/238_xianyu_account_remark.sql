-- 238_xianyu_account_remark.sql
-- 闲鱼账号投影补充运营备注：主程序侧可编辑的账号备注，用于区分多账号用途。
-- 该列由主程序管理，Worker 账号投影同步不覆盖（投影 UPSERT 不含 remark）。

ALTER TABLE xianyu_accounts
    ADD COLUMN IF NOT EXISTS remark VARCHAR(200) NOT NULL DEFAULT '';
