-- 291 在 288 建立的 user_platform_quotas 平台 CHECK 超集（16 项）基础上，补入
-- cursor 这一项，使其与 service.AllowedQuotaPlatforms（= domain.ConcretePlatformIDs()
-- ∪ other，17 项）同集合。这样后端配额白名单放开到 17 项后，前端目录里 cursor 账号
-- 的配额行能写库不炸。
--
-- 推导链（执行器无生产库访问权，依树内可读来源并运算）：
--   ① 288_platform_check_constraints_superset.sql 的 user_platform_quotas CHECK
--      成员集（16 项）：anthropic, openai, gemini, antigravity, grok, kimi, zhipu,
--      deepseek, minimax, other, codebuddy, muse, opencode_go, typesafe, command_code, cline
--   ② service.AllowedQuotaPlatforms（17 项）= ① 的 16 项（不含 cursor）∪ cursor
--   仅 cursor 一项为 288 相对并集所缺，故本迁移只补 cursor。
--
-- 刻意不碰 composite_model_routes 的 CHECK（裁定 3：那是路由目标白名单，不在本次需求闭包内）。
--
-- 可重入：DROP ... IF EXISTS 先删后加，重复执行幂等。
-- 可逆（还原 SQL 见文件尾注释）：回滚 = 在不含 cursor 的超集上重建约束即可，纯 DDL 不触存量数据。
--   注意语义不可逆点（与 266/288 注释一致）：一旦线上写入了 cursor 的配额行，回滚约束到不含
--   cursor 会令后续触碰该行的写失败；退役平台应按「先删/改行、再缩约束」顺序。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'minimax', 'other', 'codebuddy', 'muse',
                        'opencode_go', 'typesafe', 'command_code', 'cline', 'cursor'));

-- 还原 SQL（如需回滚到本迁移生效前、且库内尚未写入 cursor 配额行时）：
--   ALTER TABLE user_platform_quotas
--       DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;
--   ALTER TABLE user_platform_quotas
--       ADD CONSTRAINT user_platform_quotas_platform_check
--       CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
--                           'kimi', 'zhipu', 'deepseek', 'minimax', 'other', 'codebuddy', 'muse',
--                           'opencode_go', 'typesafe', 'command_code', 'cline'));
