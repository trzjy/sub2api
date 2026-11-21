-- 批1 热修：修复 311 与首版写路径未创建嵌套 jsonb 对象导致的“孤儿 probe_due_at”。
--
-- 背景：Postgres 的 jsonb_set(..., create_missing=true) **不会**创建多级路径中的
-- 中间对象；对 extra 里尚无 account_recovery_probe 根键的行，`{account_recovery_probe,quota}`
-- 的写入静默无效，只留下 probe_due_at（账号被停调但恢复通道读不到分量 → 永不探测/永不恢复）。
--
-- 本迁移为孤儿行重建分量；修复后的写路径改为“先在根层合并 account_recovery_probe，
-- 再写分量”（见 account_recovery_probe_repo.go）。
-- 幂等：只处理 extra 中缺失 account_recovery_probe 的行。

UPDATE accounts a
SET extra = jsonb_set(
      COALESCE(a.extra, '{}'::jsonb),
      '{account_recovery_probe}',
      CASE
        WHEN COALESCE(a.temp_unschedulable_reason, '') LIKE '%"source":"recovery_probe"%' THEN
          jsonb_build_object(
            'transport', jsonb_build_object(
              'gen', nextval('accounts_recovery_probe_gen_seq'),
              'due', to_jsonb(a.probe_due_at),
              'mode', COALESCE(NULLIF(a.temp_unschedulable_reason::jsonb->>'recovery_mode', ''), 'transient')))
        ELSE
          jsonb_build_object(
            'quota', jsonb_build_object(
              'gen', nextval('accounts_recovery_probe_gen_seq'),
              'due', to_jsonb(a.probe_due_at),
              'mode', CASE
                        WHEN a.overload_until > NOW() AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW()) THEN 'overload'
                        ELSE 'noecho'
                      END,
              'kind', CASE
                        WHEN a.platform = 'antigravity'
                          OR (a.platform = 'cursor' AND a.quota_key_id IS NOT NULL)
                          OR (a.credentials ? 'balance_probe')
                        THEN 'quota' ELSE 'actual'
                      END))
      END,
      true),
    updated_at = NOW()
WHERE a.deleted_at IS NULL
  AND a.probe_due_at IS NOT NULL
  AND (a.extra IS NULL OR NOT (a.extra ? 'account_recovery_probe'));

-- 断言：不应再有孤儿行（有 probe_due_at 却无分量）。
DO $$
DECLARE
  orphan bigint;
BEGIN
  SELECT count(*) INTO orphan
  FROM accounts
  WHERE deleted_at IS NULL
    AND probe_due_at IS NOT NULL
    AND (extra IS NULL OR NOT (extra ? 'account_recovery_probe'));
  IF orphan > 0 THEN
    RAISE EXCEPTION 'batch1 repair: % orphan probe_due_at rows remain', orphan;
  END IF;
END $$;
