-- 249_fix_redeem_code_groups_id.sql
-- ent 实体 RedeemCodeGroup 携带隐式自增 id 主键，而 245 迁移建表为
-- (redeem_code_id, group_id) 复合主键、无 id 列。模型与真实表结构不一致，
-- ent 写入（CreateBulk 生成 RETURNING "id"）时报 pq: column "id" does not exist，
-- 福利批次创建在"写入分组勾选"一步必然失败。
-- 对齐为自增 id 主键，原复合主键语义降级为唯一约束保留（一卡一组一行）。

ALTER TABLE redeem_code_groups DROP CONSTRAINT IF EXISTS redeem_code_groups_pkey;

ALTER TABLE redeem_code_groups ADD COLUMN IF NOT EXISTS id BIGSERIAL;

CREATE UNIQUE INDEX IF NOT EXISTS redeem_code_groups_id_key ON redeem_code_groups (id);

CREATE UNIQUE INDEX IF NOT EXISTS redeem_code_groups_code_group_key
    ON redeem_code_groups (redeem_code_id, group_id);
