-- C1: 渠道监控 probe_state 表 + 旧 platforms[].models 清单归零
-- 幂等：所有 DDL 带 IF NOT EXISTS；models 清空用 jsonb_set 重写（保留 platform/enabled）。
-- 注意：迁移 197（seed popular models）只读，本迁移不改动它；197 若曾向
-- platforms[].models 注入数据，本迁移在 197 之后运行将其清空，属 C1 预期行为。

-- 1) probe_state 表：记录每个 (platform, model, group) 三元组的最近探测结果。
CREATE TABLE IF NOT EXISTS channel_monitor_probe_state (
    platform       TEXT        NOT NULL,
    model          TEXT        NOT NULL,
    group_id       BIGINT      NOT NULL,
    last_probe_at  TIMESTAMPTZ,
    last_ok        BOOLEAN,
    last_detail    TEXT,
    last_account_id BIGINT,
    PRIMARY KEY (platform, model, group_id)
);

-- 2) 清空 channel_monitor_v2_config.platforms 各条目的 models 为 []，
--    保留 platform / enabled 等其它键（不删列、不删表）。
UPDATE channel_monitor_v2_config
SET platforms = (
    SELECT COALESCE(
        jsonb_agg(jsonb_set(p, '{models}', '[]'::jsonb)),
        '[]'::jsonb
    )
    FROM jsonb_array_elements(platforms::jsonb) AS p
)
WHERE id = 1;
