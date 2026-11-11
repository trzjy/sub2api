-- 308_quota_keys_backfill_group_ids.sql
-- 存量激活码分组绑定回填迁移（派发单 QKG-A2，方案 2026-11-11 §3 模块 A2）。
--
-- 背景：304/305 把激活码的码级权威字段（并发、负载、倍率、模型映射、到期）上移，
-- 306 回填了这些码级字段。本迁移进一步把“存量激活码的分组绑定”固化到码级列
-- quota_keys.group_ids，避免继续依赖静态清单推导导致漂移。
--
-- 取值规则：对每条未软删且尚未回填（group_ids IS NULL）的激活码，取名下最新未软删
-- cursor 账号；读该账号在 account_groups 的分组，按 priority ASC, group_id ASC 聚合成
-- BIGINT[] 写入码级列。无账号或无任何分组绑定则保持 group_ids 为 NULL（CONTINUE）。
--
-- 冲突处理：若同一激活码下多个未软删 cursor 账号的分组集合不一致，以最新账号为准，
-- 记录 RAISE NOTICE，并把同码其余未软删账号的 account_groups 同步替换为最新账号的分组
-- 集合（先删后建，priority = 序号），保证同码下账号与码级分组一致。
--
-- 幂等：仅在 group_ids IS NULL 时回填，已回填的码（group_ids 非空）被 FOR 循环过滤跳过；
-- 重复执行本迁移为安全空操作。
--
-- 兼容隔离 schema / search_path：全程不硬编码 schema 名，表名走 search_path（不使用
-- 硬编码的 schema 名判定），因此可在非默认 schema 下跑同样的迁移。
--
-- 末尾注释含撤销回填效果的回滚 SQL。

DO $$
DECLARE
    q             RECORD;
    latest_acc    RECORD;
    other_acc     RECORD;
    grp           BIGINT;
    groups_arr    BIGINT[];
    acc_cnt       INTEGER;
    distinct_cnt  INTEGER;
    idx           INTEGER;
BEGIN
    FOR q IN
        SELECT * FROM quota_keys WHERE deleted_at IS NULL AND group_ids IS NULL
    LOOP
        -- 名下最新未软删 cursor 账号（以最新为准）。
        SELECT * INTO latest_acc
        FROM accounts a
        WHERE a.quota_key_id = q.id
          AND a.platform = 'cursor'
          AND a.deleted_at IS NULL
        ORDER BY a.created_at DESC, a.id DESC
        LIMIT 1;

        -- 无账号则保持 group_ids 为 NULL，跳过本码。
        IF latest_acc IS NULL THEN
            CONTINUE;
        END IF;

        -- 取最新账号在 account_groups 的分组，按 priority ASC, group_id ASC 聚合成 BIGINT[]。
        groups_arr := ARRAY(
            SELECT ag.group_id
            FROM account_groups ag
            WHERE ag.account_id = latest_acc.id
            ORDER BY ag.priority ASC, ag.group_id ASC
        );

        -- 最新账号无任何分组绑定 → 保持 group_ids 为 NULL，跳过本码。
        IF array_length(groups_arr, 1) IS NULL THEN
            CONTINUE;
        END IF;

        -- 冲突检测：统计同码名下未软删 cursor 账号各自分组集合（按 priority, group_id 排序的
        -- 有序数组）的不同取值个数；≥2 个账号且集合不一致则记为冲突。
        SELECT count(*) INTO distinct_cnt
        FROM (
            SELECT DISTINCT
                (SELECT array_agg(ag.group_id ORDER BY ag.priority ASC, ag.group_id ASC)
                   FROM account_groups ag
                  WHERE ag.account_id = a.id) AS grps
            FROM accounts a
            WHERE a.quota_key_id = q.id
              AND a.platform = 'cursor'
              AND a.deleted_at IS NULL
        ) s;

        SELECT count(*) INTO acc_cnt
        FROM accounts a
        WHERE a.quota_key_id = q.id
          AND a.platform = 'cursor'
          AND a.deleted_at IS NULL;

        IF acc_cnt >= 2 AND distinct_cnt > 1 THEN
            RAISE NOTICE 'quota_key % group backfill conflict, adopting latest account % groups',
                q.id, latest_acc.id;

            -- 以最新账号为准，把同码其余未软删 cursor 账号的 account_groups 替换为最新账号的
            -- 分组集合（先删后建，priority = 序号）。
            FOR other_acc IN
                SELECT * FROM accounts a
                WHERE a.quota_key_id = q.id
                  AND a.platform = 'cursor'
                  AND a.deleted_at IS NULL
                  AND a.id <> latest_acc.id
            LOOP
                DELETE FROM account_groups WHERE account_id = other_acc.id;
                idx := 1;
                FOREACH grp IN ARRAY groups_arr
                LOOP
                    INSERT INTO account_groups (account_id, group_id, priority)
                    VALUES (other_acc.id, grp, idx);
                    idx := idx + 1;
                END LOOP;
            END LOOP;
        END IF;

        -- 固化分组绑定到码级列（仅当 group_ids IS NULL 时进入本循环，绝不覆盖已回填值）。
        UPDATE quota_keys
        SET group_ids = to_jsonb(groups_arr)
        WHERE id = q.id;
    END LOOP;
END $$;

-- 回滚（仅在需要时手动执行；本迁移为纯回填、无结构性变更。以下仅给出“撤销回填效果”的
-- 一种思路，需按业务确认受影响范围后再执行，切勿在线上直接盲跑）：
-- BEGIN;
-- -- 把回填出的 group_ids 重置回 NULL（等于撤销本迁移；注意这会丢掉固化到码级的分组绑定，
-- -- 且并不会回退上面“冲突同步”对 account_groups 的改写——如需一并撤销，需另行按审计还原）。
-- UPDATE quota_keys
--    SET group_ids = NULL
-- WHERE id IN (/* 仅限本次需要回滚的激活码 id */);
-- COMMIT;
