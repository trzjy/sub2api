-- 294: 预置 Cursor 极速编程 Auto分组 与 Auto 动态调度承载账号
-- 幂等迁移：重复执行安全，不修改任何既有数据。

-- 1) 预置 Cursor 极速编程 Auto分组
INSERT INTO groups (name, description, platform, subscription_type, rate_multiplier, concurrency, status, model_allowlist, created_at, updated_at)
VALUES (
    'Auto分组',
    'Cursor 家族模型',
    'cursor',
    'standard',
    1.0,
    2,
    'active',
    '{"models": ["cursor-auto", "cursor-fast", "auto", "default"], "enabled": true}',
    NOW(),
    NOW()
)
ON CONFLICT (name) DO NOTHING;

-- 2) 预置 Cursor Auto 动态号池调度承载账号
INSERT INTO accounts (name, platform, type, status, credentials, extra, created_at, updated_at)
SELECT
    'Cursor Auto 动态调度枢纽',
    'cursor',
    'oauth',
    'active',
    '{"sub_type": "auto", "model_mapping": {"cursor-auto": "default", "cursor-fast": "default", "auto": "default", "default": "default"}}'::jsonb,
    '{}'::jsonb,
    NOW(),
    NOW()
WHERE NOT EXISTS (
    SELECT 1 FROM accounts WHERE name = 'Cursor Auto 动态调度枢纽'
);

-- 3) 挂载账号至 Auto分组
INSERT INTO account_groups (account_id, group_id, priority)
SELECT a.id, g.id, 1
FROM accounts a, groups g
WHERE a.name = 'Cursor Auto 动态调度枢纽'
  AND g.name = 'Auto分组'
  AND NOT EXISTS (
      SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ag.group_id = g.id
  );
