-- monreal-be：为 channel_monitor_probe_state 补 first_token_ms / total_ms 两列（B3 流式 TTFT）。
-- 根因：B3 引入流式探测计时（首 token 延迟 / 总时长），ProbeState 与 postgresProbeStore
-- 的 Get/Upsert 需持久化这两个可空整数字段。
-- 幂等：ADD COLUMN IF NOT EXISTS；仅 additive，不删不改既有列，与 272 文件头部注释
-- 与格式惯例一致（C2 系列 additive 迁移）。

ALTER TABLE channel_monitor_probe_state
	ADD COLUMN IF NOT EXISTS first_token_ms INT NULL,
	ADD COLUMN IF NOT EXISTS total_ms INT NULL;
