-- C2-FIX2：为 channel_monitor_probe_state 补 next_due 列（跨卡 schema 不一致修复）。
-- 根因：C2 的 postgresProbeStore（SELECT/INSERT）引用 next_due，但 C1 的迁移 271
-- 建表时未建该列，导致探测 worker 每 tick 报 "column next_due does not exist"。
-- 幂等：ADD COLUMN IF NOT EXISTS；仅 additive，不删不改既有列。

ALTER TABLE channel_monitor_probe_state ADD COLUMN IF NOT EXISTS next_due TIMESTAMPTZ;
