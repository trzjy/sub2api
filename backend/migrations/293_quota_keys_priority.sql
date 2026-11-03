-- quota_keys 新增 priority：激活码优先级（数值越小越优先），与 accounts.priority 同一语义。
--
-- 用户裁定（2026-11-03）：激活码页可给每个码设置优先级，该码名下账号的优先级跟随码的
-- 优先级——日限额激活码取小值（优先消耗），月总限额激活码取大值（兜底补充）。
--
-- 口径：
--   · 拉号入库时把本列写入 accounts.priority：默认 1 与既有入库常量 CursorEntryPriority 相同，
--     故存量行为不变（现有 50 个 cursor 账号均为 priority=1）；
--   · 修改本列时在同一事务内刷新该码名下未删除账号的 accounts.priority，使"码的优先级"
--     与"码下账号的优先级"始终一致；
--   · 本列不参与额度/到期/耗尽任何判定，只决定调度与补池的顺序。
--
-- 可重入：ADD COLUMN IF NOT EXISTS，重复执行幂等。
-- 可逆：ALTER TABLE quota_keys DROP COLUMN IF EXISTS priority;（纯 DDL，不触存量数据）。
--
-- 存量行一律取默认值 1（不预填其它值：任何其它值都会立刻改变现有调度顺序）。

ALTER TABLE quota_keys
    ADD COLUMN IF NOT EXISTS priority INTEGER NOT NULL DEFAULT 1;

-- 还原 SQL：
--   ALTER TABLE quota_keys DROP COLUMN IF EXISTS priority;
