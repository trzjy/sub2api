-- Migration: 313_seed_request_size_routing_threshold
-- 请求规模分流阈值的唯一事实源收口。
--
-- 背景（根因）：
--   大/小请求的分界阈值此前有两份事实：
--     ① 代码常量 service.LargeRequestOnlyInputThreshold = 256000（兜底默认值）
--     ② settings 表 request_size_routing_threshold（运行时实际生效值）
--   两份一旦不一致（例如线上把配置调成 128000，代码仍留 256000），在配置读取
--   失败的瞬间就会把分界悄悄换成代码常量那个数：12.8 万~25.6 万之间的请求被判成
--   「小请求」，落进只接小请求的账号，大活细节丢失（11-12 事故形态）。
--
-- 本迁移做两件事：
--   1. 删除代码里的硬编码常量，阈值只剩 settings 表一个来源；
--      读不到阈值就「不做规模分流」（两类账号都留在候选池），不再编默认数。
--   2. 给全新部署种一行 request_size_routing_threshold = 128000，
--      保证「没有这一行」不会导致规模分流整个失效。
--
-- 取值说明：128000 是当前线上生效值（settings.request_size_routing_threshold，
-- 2026-11-22 调整）。种子的作用是「全新部署有合法初值」，不是「覆盖线上配置」，
-- 因此用 ON CONFLICT (key) DO NOTHING：已存在就原样保留。

INSERT INTO settings (key, value, updated_at)
VALUES ('request_size_routing_threshold', '128000', NOW())
ON CONFLICT (key) DO NOTHING;

-- 回滚说明：本迁移只种一行配置，不改表结构。
-- 回滚时如需恢复旧行为，由人工决定是否删除该行：
--   DELETE FROM settings WHERE key = 'request_size_routing_threshold';
-- （删除后该环境将退化为「阈值不可知 → 不做规模分流」，不再有 256000 兜底。）
