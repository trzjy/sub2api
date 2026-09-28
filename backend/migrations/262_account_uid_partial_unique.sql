-- B1b（方案 v15 §1.6）：accounts.uid 列 + (platform, uid) 部分唯一索引。
--
-- uid 为平台侧账号唯一标识（如 Cockpit 导出的 account id），与 platform 共同构成
-- 部分唯一索引 (platform, uid) WHERE uid != ''。空字符串（''）表示未设置，不参与
-- 唯一约束，因此存量行加列后默认 '' 不会触发冲突。
--
-- 关键闸门（方案 §1.6 钉死）：建索引前必须先扫描存量 (platform, uid) 重复（uid != ''）。
-- 发现任一重复 → RAISE EXCEPTION 中止迁移并报告冲突键；存量人工合并后才能建索引。
-- 不得跳过闸门直接建索引，否则部分唯一索引会因重复行而创建失败。

-- ① 加列：NOT NULL DEFAULT '' 对存量行安全（默认 '' 不触发部分索引谓词）。
ALTER TABLE accounts ADD COLUMN uid text NOT NULL DEFAULT '';

-- ② 查重前置闸门：扫描存量重复键（uid != ''）。
--    发现重复即中止迁移，报告 platform/uid/重复计数，交由人工合并。
DO $$
DECLARE
    dup_rec record;
BEGIN
    FOR dup_rec IN
        SELECT platform, uid, count(*) AS cnt
        FROM accounts
        WHERE uid != ''
        GROUP BY platform, uid
        HAVING count(*) > 1
    LOOP
        RAISE EXCEPTION 'B1b migration blocked: duplicate (platform, uid) found in accounts where uid != ''''; platform=% uid=% count=% — merge duplicates before applying 262',
            dup_rec.platform, dup_rec.uid, dup_rec.cnt;
    END LOOP;
END $$;

-- ③ 部分唯一索引：仅对 uid != '' 的行强制 (platform, uid) 唯一。
--    命名沿用仓库约定 <table>_<columns>_active（与 users_email_unique_active 等对齐）。
CREATE UNIQUE INDEX IF NOT EXISTS accounts_platform_uid_active
    ON accounts (platform, uid)
    WHERE uid != '';
