-- 264_migrate_codebuddy_quota_error_to_credit_error.sql
--
-- 独占所有者：B4 Card E（唯一后端迁移所有者）。方案
-- docs/codebuddy-b4-implementation-plan.md §1；权威
-- docs/codebuddy-cockpit-fusion-plan.md §4.1。
--
-- 目的：Extra 错误键 SSOT 由旧名 codebuddy_quota_error 改名为 codebuddy_credit_error。
-- 本迁移把 accounts.extra 中的存量旧键**原子**搬迁到新键，搬迁完成后旧键在库内归零
-- （红线：禁止任何双名共存期 / 兼容层）。
--
-- 执行语义（与迁移 runner 契约一致，internal/repository/migrations_runner.go）：
--   * 本文件是常规迁移（非 *_notx 后缀），runner 把整份内容在**同一个事务**内执行，
--     任一语句失败即整批回滚 → 原子，不存在"部分账号已搬迁"的中间态；
--     "中途失败重试"从完整初始态重新开始，不会叠加出分叉状态。
--   * 幂等：搬迁条件恒为「旧键存在且新键不存在」。重复执行时旧键已被删除，
--     UPDATE 不命中任何行，为空操作（重复执行 = 无副作用）。
--   * 失败阻断（红线：不静默覆盖）：同一账号两键同时存在时无法判定权威值，
--     本迁移在**任何写入之前**先探测并 RAISE EXCEPTION 中止整批事务；绝不静默
--     用任一侧覆盖另一侧。遇此形态须人工裁定后重跑（语义裁定登记于 B4 发布单）。
--
-- 影响面：只触碰 B4 负责的单个错误键，accounts.extra 中其余键（分包快照、阈值、
-- 更新时间等）原样保留。

DO $$
DECLARE
    v_legacy_only  bigint;
    v_new_only     bigint;
    v_conflict     bigint;
    v_conflict_ids text;
BEGIN
    -- 状态①「旧键仅有」：正常存量，待搬迁。
    SELECT count(*) INTO v_legacy_only
      FROM accounts
     WHERE extra ? 'codebuddy_quota_error'
       AND NOT (extra ? 'codebuddy_credit_error');

    -- 状态②「新键仅有」：已由新写入端写入，或本迁移重复执行后的稳态；不命中搬迁。
    SELECT count(*) INTO v_new_only
      FROM accounts
     WHERE extra ? 'codebuddy_credit_error'
       AND NOT (extra ? 'codebuddy_quota_error');

    -- 状态③「双键冲突」：两键同时存在 → 失败阻断。探测先于任何写入，保证零部分写入。
    SELECT count(*), coalesce(string_agg(id::text, ',' ORDER BY id), '')
      INTO v_conflict, v_conflict_ids
      FROM accounts
     WHERE extra ? 'codebuddy_quota_error'
       AND extra ? 'codebuddy_credit_error';

    IF v_conflict > 0 THEN
        RAISE EXCEPTION
            '264 blocked: % account(s) carry BOTH codebuddy_quota_error and codebuddy_credit_error in accounts.extra (account ids: %); resolve the dual-key conflict manually, then re-run',
            v_conflict, v_conflict_ids;
    END IF;

    RAISE NOTICE '264 scan: legacy_only=%, new_only=%, conflict=0', v_legacy_only, v_new_only;
END $$;

-- 状态①搬迁（旧键仅有 → 原值写入新键并从 extra 删除旧键；同行 SET 引用旧值，
-- 单条原子 UPDATE）+ 状态④幂等（重复执行时旧键已删，WHERE 不命中 → 空操作）。
-- 状态⑤原子性由 runner 的单事务保证（整份文件同事务，失败整批回滚）。
UPDATE accounts
   SET extra = (extra - 'codebuddy_quota_error')
               || jsonb_build_object('codebuddy_credit_error', extra -> 'codebuddy_quota_error')
 WHERE extra ? 'codebuddy_quota_error'
   AND NOT (extra ? 'codebuddy_credit_error');
