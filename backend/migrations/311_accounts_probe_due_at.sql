-- 批1：账号级停调恢复闭环
--   1) accounts.probe_due_at：恢复探测的下次执行时刻（派生列 = extra 各分量 due 最小值）
--   2) 探测代际序列：每次登记一个新 gen，防 ABA（同 resetAt 的迟到探测清掉新事件）
--   3) 存量回填：quota 类（429/过载）与无归属 transport 类 temp 停调
--   4) 回滚快照表：记录每行迁移前的原值与迁移写入值
--
-- 幂等：全部 IF NOT EXISTS / 条件 UPDATE；重复执行零写入。

ALTER TABLE accounts ADD COLUMN IF NOT EXISTS probe_due_at timestamptz;

CREATE INDEX IF NOT EXISTS idx_accounts_probe_due_at
  ON accounts (probe_due_at)
  WHERE probe_due_at IS NOT NULL AND deleted_at IS NULL;

CREATE SEQUENCE IF NOT EXISTS accounts_recovery_probe_gen_seq;

CREATE TABLE IF NOT EXISTS accounts_probe_due_at_backfill_snapshot (
  account_id bigint PRIMARY KEY,
  prev_temp_until timestamptz,
  prev_temp_reason text,
  prev_rate_limit_reset_at timestamptz,
  prev_overload_until timestamptz,
  new_probe_due_at timestamptz NOT NULL,
  new_reason text,
  captured_at timestamptz NOT NULL DEFAULT NOW()
);

-- 1) quota 类：429（有/无回显）与过载。只写 probe_due_at 排程，不写 until/reason。
WITH candidates AS (
  SELECT a.id,
         a.rate_limit_reset_at,
         a.overload_until,
         COALESCE(a.extra, '{}'::jsonb) AS e,
         CASE
           WHEN a.rate_limit_reset_at > NOW() AND a.overload_until > NOW() THEN LEAST(a.rate_limit_reset_at, a.overload_until)
           WHEN a.rate_limit_reset_at > NOW() THEN a.rate_limit_reset_at
           ELSE a.overload_until
         END AS due,
         CASE
           WHEN a.overload_until > NOW() AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW()) THEN 'overload'
           ELSE 'noecho'
         END AS mode,
         CASE
           WHEN a.platform = 'antigravity'
             OR (a.platform = 'cursor' AND a.quota_key_id IS NOT NULL)
             OR (a.credentials ? 'balance_probe')
           THEN 'quota' ELSE 'actual'
         END AS kind
  FROM accounts a
  WHERE a.deleted_at IS NULL
    AND a.probe_due_at IS NULL
    AND (a.rate_limit_reset_at > NOW() OR a.overload_until > NOW())
    AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW())
    AND COALESCE(a.extra->>'cn_quota_lifecycle', '') = ''
    AND COALESCE(a.extra->>'http_403_recovery', '') = ''
), snap AS (
  INSERT INTO accounts_probe_due_at_backfill_snapshot
    (account_id, prev_temp_until, prev_temp_reason, prev_rate_limit_reset_at, prev_overload_until, new_probe_due_at, new_reason)
  SELECT a.id, a.temp_unschedulable_until, a.temp_unschedulable_reason, a.rate_limit_reset_at, a.overload_until, c.due, NULL
  FROM accounts a JOIN candidates c ON c.id = a.id
  ON CONFLICT (account_id) DO NOTHING
  RETURNING account_id
)
UPDATE accounts a
SET extra = jsonb_set(
      jsonb_set(c.e, '{account_recovery_probe,quota}',
        jsonb_build_object('gen', nextval('accounts_recovery_probe_gen_seq'),
                           'due', to_jsonb(c.due), 'mode', c.mode, 'kind', c.kind), true),
      '{sched_state_revision}',
      to_jsonb(COALESCE((c.e->>'sched_state_revision')::bigint, 0) + 1), true),
    probe_due_at = c.due,
    updated_at = NOW()
FROM candidates c
WHERE a.id = c.id;

-- 2) transport 类：无归属的存量 temp 停调。改写为结构化 reason（source=recovery_probe，
--    migrated 标记在 extra），并立即排一次探测。
WITH candidates AS (
  SELECT a.id,
         a.temp_unschedulable_until,
         a.temp_unschedulable_reason,
         COALESCE(a.extra, '{}'::jsonb) AS e,
         jsonb_build_object(
           'matched_keyword', 'account_recovery_generic',
           'source', 'recovery_probe',
           'recovery_mode', 'transient',
           'triggered_at_unix', EXTRACT(EPOCH FROM NOW())::bigint,
           'error_message', LEFT(COALESCE(a.temp_unschedulable_reason, 'migrated legacy temp-unschedulable reason'), 512)
         )::text AS new_reason
  FROM accounts a
  WHERE a.deleted_at IS NULL
    AND a.probe_due_at IS NULL
    AND a.temp_unschedulable_until > NOW()
    AND COALESCE(a.extra->>'cn_quota_lifecycle', '') = ''
    AND COALESCE(a.extra->>'http_403_recovery', '') = ''
    AND COALESCE(a.temp_unschedulable_reason, '') NOT LIKE 'http_403_recovery_f3: %'
    AND COALESCE(a.temp_unschedulable_reason, '') NOT LIKE '%"source":"web_console_quota"%'
    AND COALESCE(a.temp_unschedulable_reason, '') NOT LIKE '%openai_apikey_health_breaker%'
    AND COALESCE(a.temp_unschedulable_reason, '') NOT LIKE '%pool_mode_401_escalation%'
    AND COALESCE(a.temp_unschedulable_reason, '') NOT LIKE '%oauth401%'
    AND COALESCE(a.temp_unschedulable_reason, '') NOT LIKE '%devin_upstream_quota_exhausted%'
    AND COALESCE(a.temp_unschedulable_reason, '') NOT LIKE '%cursor%'
), snap AS (
  INSERT INTO accounts_probe_due_at_backfill_snapshot
    (account_id, prev_temp_until, prev_temp_reason, prev_rate_limit_reset_at, prev_overload_until, new_probe_due_at, new_reason)
  SELECT a.id, a.temp_unschedulable_until, a.temp_unschedulable_reason, a.rate_limit_reset_at, a.overload_until, NOW(), c.new_reason
  FROM accounts a JOIN candidates c ON c.id = a.id
  ON CONFLICT (account_id) DO NOTHING
  RETURNING account_id
)
UPDATE accounts a
SET extra = jsonb_set(
      jsonb_set(
        jsonb_set(c.e, '{account_recovery_probe,transport}',
          jsonb_build_object('gen', nextval('accounts_recovery_probe_gen_seq'),
                             'due', to_jsonb(NOW()), 'mode', 'transient'), true),
        '{account_recovery_probe,migrated}', 'true'::jsonb, true),
      '{sched_state_revision}',
      to_jsonb(COALESCE((c.e->>'sched_state_revision')::bigint, 0) + 1), true),
    temp_unschedulable_reason = c.new_reason,
    probe_due_at = NOW(),
    updated_at = NOW()
FROM candidates c
WHERE a.id = c.id;

-- 3) 校验：关键不变式失败即失败关闭（迁移中止）。
DO $$
DECLARE
  orphan_recovery bigint;
  candidate_rows bigint;
BEGIN
  -- 关键：被 migration 写成 transport 行的账号必须有 probe_due_at（否则该账号会被
  -- 通用通道遗漏，且旧 marker 通道也不再识别它 ⇒ 永久停调）。
  SELECT count(*) INTO orphan_recovery
  FROM accounts
  WHERE deleted_at IS NULL
    AND temp_unschedulable_reason LIKE '%"source":"recovery_probe"%'
    AND probe_due_at IS NULL;
  IF orphan_recovery > 0 THEN
    RAISE EXCEPTION 'batch1 migration: % recovery_probe rows missing probe_due_at', orphan_recovery;
  END IF;

  -- 报告型检查：仍有无归属的有效 temp 停调未回填（可能命中排除词表，属预期），
  -- 只提示不阻断。
  SELECT count(*) INTO candidate_rows
  FROM accounts
  WHERE deleted_at IS NULL
    AND temp_unschedulable_until > NOW()
    AND probe_due_at IS NULL
    AND COALESCE(extra->>'cn_quota_lifecycle', '') = ''
    AND COALESCE(extra->>'http_403_recovery', '') = ''
    AND COALESCE(temp_unschedulable_reason, '') NOT LIKE '%"source":"web_console_quota"%'
    AND COALESCE(temp_unschedulable_reason, '') NOT LIKE '%openai_apikey_health_breaker%'
    AND COALESCE(temp_unschedulable_reason, '') NOT LIKE '%pool_mode_401_escalation%'
    AND COALESCE(temp_unschedulable_reason, '') NOT LIKE '%oauth401%'
    AND COALESCE(temp_unschedulable_reason, '') NOT LIKE '%devin_upstream_quota_exhausted%'
    AND COALESCE(temp_unschedulable_reason, '') NOT LIKE '%cursor%'
    AND COALESCE(temp_unschedulable_reason, '') NOT LIKE 'http_403_recovery_f3: %';
  IF candidate_rows > 0 THEN
    RAISE NOTICE 'batch1 migration: % unowned active temp stops left without probe_due_at (see report)', candidate_rows;
  END IF;
END $$;
