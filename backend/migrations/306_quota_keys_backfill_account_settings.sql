-- 306_quota_keys_backfill_account_settings.sql
-- 存量激活码设置回填迁移（派发单 c1p1-B1backfill，用户裁定 2026-11-10）。
--
-- 背景：304 将 concurrency/load_factor/rate_multiplier 上移到激活码（码级权威值），
-- 305 将 model_mapping 上移到激活码。本迁移对存量激活码按名下最新未软删 cursor 账号
-- 的当前值回填码级列；若同一码下主/备两个账号设置不一致，以最新未软删账号为准并记录
-- RAISE NOTICE 通知。
-- 同时把 quota_keys.expires_at 与名下未软删 cursor 账号对齐（双向：码级为空时从最新账号
-- 回填，码级非空时把账号对齐到码级）。
--
-- 幂等：仅在码级列仍为初始状态（NULL 或 rate_multiplier = 1.0）时回填，不覆盖之后手工
-- 设定的码级值。重复执行本迁移为安全空操作。
--
-- 兼容隔离 schema 测试与 search_path：全程不硬编码 schema，表名走 search_path
-- （不使用硬编码的 schema 名判定），因此可在非默认 schema 下跑同样的迁移。
--
-- 末尾注释含回滚说明。

DO $$
DECLARE
    q                  RECORD;
    acc                RECORD;
    acc_cnt            INTEGER;
    acc_distinct       INTEGER;
    resolved_expires   TIMESTAMPTZ;
BEGIN
    FOR q IN
        SELECT * FROM quota_keys WHERE deleted_at IS NULL
    LOOP
        -- 名下最新未软删 cursor 账号（主/备以最新为准）。
        SELECT * INTO acc
        FROM accounts a
        WHERE a.quota_key_id = q.id
          AND a.platform = 'cursor'
          AND a.deleted_at IS NULL
        ORDER BY a.created_at DESC, a.id DESC
        LIMIT 1;

        -- 无账号则跳过本码。
        IF acc IS NULL THEN
            CONTINUE;
        END IF;

        -- 冲突检测：≥2 个未软删 cursor 账号且四列（concurrency / load_factor /
        -- rate_multiplier / credentials->'model_mapping'）存在取值差异，则记录通知。
        SELECT count(*),
               count(DISTINCT (a.concurrency, a.load_factor, a.rate_multiplier,
                               a.credentials->'model_mapping'))
          INTO acc_cnt, acc_distinct
        FROM accounts a
        WHERE a.quota_key_id = q.id
          AND a.platform = 'cursor'
          AND a.deleted_at IS NULL;

        IF acc_cnt >= 2 AND acc_distinct > 1 THEN
            RAISE NOTICE 'quota_key % backfill conflict, adopting latest account % values',
                q.id, acc.id;
        END IF;

        -- 幂等回填码级列（仅在初始状态时回填，绝不覆盖手工设定的码级值）。
        UPDATE quota_keys
        SET concurrency = CASE
                WHEN quota_keys.concurrency IS NULL AND acc.concurrency > 0
                THEN acc.concurrency ELSE quota_keys.concurrency END,
            load_factor = CASE
                WHEN quota_keys.load_factor IS NULL
                     AND acc.load_factor IS NOT NULL AND acc.load_factor > 0
                THEN acc.load_factor ELSE quota_keys.load_factor END,
            rate_multiplier = CASE
                WHEN quota_keys.rate_multiplier = 1.0
                     AND acc.rate_multiplier IS NOT NULL
                     AND acc.rate_multiplier >= 0 AND acc.rate_multiplier <> 1.0
                THEN acc.rate_multiplier ELSE quota_keys.rate_multiplier END,
            model_mapping = CASE
                WHEN quota_keys.model_mapping IS NULL
                     AND jsonb_typeof(acc.credentials->'model_mapping') = 'object'
                     AND acc.credentials->'model_mapping' <> '{}'::jsonb
                THEN acc.credentials->'model_mapping' ELSE quota_keys.model_mapping END,
            expires_at = CASE
                WHEN quota_keys.expires_at IS NULL AND acc.expires_at IS NOT NULL
                THEN acc.expires_at ELSE quota_keys.expires_at END
        WHERE quota_keys.id = q.id;

        -- 解析码级最终到期时间（含刚回填出的值），用于对账名下账号。
        resolved_expires := q.expires_at;
        IF resolved_expires IS NULL THEN
            resolved_expires := acc.expires_at;
        END IF;

        -- 对齐到期时间：把该码名下未软删 cursor 账号的 expires_at 同步为码级值。
        IF resolved_expires IS NOT NULL THEN
            UPDATE accounts
            SET expires_at = resolved_expires
            WHERE quota_key_id = q.id
              AND platform = 'cursor'
              AND deleted_at IS NULL
              AND expires_at IS DISTINCT FROM resolved_expires;
        END IF;
    END LOOP;
END $$;

-- 回滚（仅在需要时手动执行；本迁移为纯回填、无结构性变更，以下仅给出“撤销回填效果”的
-- 一种思路，需按业务确认受影响范围后再执行，切勿在线上直接盲跑）：
-- BEGIN;
-- -- 把码级列重置为初始状态（等于撤销 306 的回填；注意这会丢掉人工在码级设定的覆盖值）。
-- UPDATE quota_keys
--    SET concurrency = NULL,
--        load_factor = NULL,
--        rate_multiplier = 1.0,
--        model_mapping = NULL,
--        expires_at = NULL
--  WHERE id IN (/* 仅限本次需要回滚的激活码 id */);
-- -- 账号 expires_at 是否回退取决于业务口径，通常不回退（保持已同步的账号到期时间）。
-- COMMIT;
