-- Add per-group concurrency limit for subscription groups.
-- concurrency: 订阅分组各自并发上限（0 = 不限制）。
-- 订阅模式下，使用本分组的用户各自最多 concurrency 个并发，与用户全局 users.concurrency 互不干扰。
-- 计量（余额）模式仍只看 users.concurrency，不受影响。
ALTER TABLE groups ADD COLUMN IF NOT EXISTS concurrency integer NOT NULL DEFAULT 0;

COMMENT ON COLUMN groups.concurrency IS '订阅分组并发上限；0 表示不限制；订阅模式下每个使用者各自并发上限，与用户全局并发互不干扰。';
