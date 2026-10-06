-- 248_widen_redeem_code_column.sql
-- 兑换码生成器输出 XXXX-XXXX-XXXX-XXXX 格式（32 位十六进制 + 3 个连字符 = 35 字符），
-- 超出 code 列 VARCHAR(32) 上限，导致生成兑换码/福利批次时
-- ent 校验失败：value is greater than the required length。
-- 放宽到 VARCHAR(64) 以容纳带连字符格式。

ALTER TABLE redeem_codes ALTER COLUMN code TYPE VARCHAR(64);
