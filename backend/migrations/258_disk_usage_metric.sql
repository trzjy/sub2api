-- Disk usage metric for ops monitoring (disk-guard plan 2026-09-26, item 3④)
--
-- 1) Add disk_usage_percent column to ops_system_metrics (columnar snapshot table,
--    same style as cpu_usage_percent / memory_usage_percent).
-- 2) Seed the "disk-usage-high" alert rule (>85%, notify_email=false — console-only
--    visibility per user decision; no external push).
--
-- This migration is idempotent.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

ALTER TABLE ops_system_metrics
    ADD COLUMN IF NOT EXISTS disk_usage_percent DOUBLE PRECISION;

COMMENT ON COLUMN ops_system_metrics.disk_usage_percent IS 'Root partition usage percent (gopsutil disk.Usage("/"), Used/Total*100).';

INSERT INTO ops_alert_rules (
    name, description, enabled, metric_type, operator, threshold,
    window_minutes, sustained_minutes, severity, notify_email, cooldown_minutes,
    created_at, updated_at
) VALUES (
    'disk-usage-high',
    '当磁盘使用率超过 85% 且持续 10 分钟时触发告警（防止磁盘打满导致全站故障；仅在控制台告警列表可见，不发邮件）',
    true, 'disk_usage_percent', '>', 85.0, 5, 10, 'P0', false, 30, NOW(), NOW()
) ON CONFLICT (name) DO NOTHING;
