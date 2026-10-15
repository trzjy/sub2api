-- 288 取代 278_add_typesafe_platform.sql + 279_drop_platform_check_constraints.sql
-- （门二 B：不吸收 279 破坏性 DROP，约束超集由应用层 domain.IsConcretePlatform 与本约束双保险）。
--
-- 语义 = 生产现行约束的「超集」：不销毁既有平台白名单，仅把它扩到与代码平台清单
-- （internal/domain/platforms.go 的 platformList）一致，使数据库 CHECK 与应用层校验同向。
-- 279 的破坏性 DROP 在门二 B 裁定下不吸收；本迁移保持约束存在，新增平台改代码清单即可，
-- 无需再写「DROP 旧约束」式迁移，避免 279 那种把生产白名单整体撤掉导致的写路径失守。
--
-- 现行成员集推导链（执行器无生产库访问权，依树内可读来源并运算并集）：
--   ① 266_user_platform_quotas_add_muse.sql      → anthropic, openai, gemini, antigravity,
--                                                    grok, kimi, zhipu, deepseek, minimax,
--                                                    other, codebuddy, muse
--   ② 272_opencode_go_platform.sql（门一/门二推导中的 238 同义文件，树内即 272）
--                                                  → 在 ① 基础上补 opencode_go
--   ③ 278_add_typesafe_platform.sql              → 在 ② 基础上补 typesafe
--   ④ internal/domain/platforms.go 的 platformList（ConcretePlatformIDs）
--                                                  → 在 ③ 基础上补 command_code, cline
--                                                  （其余成员已被 ①~③ 覆盖；muse 已在 ①）
--   并集（逐个列出，muse 必在集内）：
--     anthropic, openai, gemini, antigravity, grok, kimi, zhipu, deepseek, minimax,
--     other, codebuddy, muse, opencode_go, typesafe, command_code, cline
--
-- 可重入：DROP ... IF EXISTS 先删后加，重复执行幂等。
-- 可逆（还原 SQL 见文件尾注释）：回滚 = 在不含新增成员的超集上重建约束即可，纯 DDL 不触存量数据。
--   注意语义不可逆点（与 266 注释一致）：一旦线上写入了交集外成员（如 command_code/cline）的
--   配额行，回滚约束到不含该成员会令后续触碰该行的写失败；退役平台应按「先删/改行、再缩约束」顺序。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'minimax', 'other', 'codebuddy', 'muse',
                        'opencode_go', 'typesafe', 'command_code', 'cline'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                               'kimi', 'zhipu', 'deepseek', 'minimax', 'other', 'codebuddy', 'muse',
                               'opencode_go', 'typesafe', 'command_code', 'cline'));

-- 还原 SQL（如需回滚到本迁移生效前、且库内尚未写入 command_code/cline 等新增成员行时）：
--   ALTER TABLE user_platform_quotas
--       DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;
--   ALTER TABLE user_platform_quotas
--       ADD CONSTRAINT user_platform_quotas_platform_check
--       CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
--                           'kimi', 'zhipu', 'deepseek', 'minimax', 'other', 'codebuddy', 'muse',
--                           'opencode_go', 'typesafe'));
--   ALTER TABLE composite_model_routes
--       DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;
--   ALTER TABLE composite_model_routes
--       ADD CONSTRAINT composite_model_routes_target_platform_check
--       CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
--                                  'kimi', 'zhipu', 'deepseek', 'minimax', 'other', 'codebuddy', 'muse',
--                                  'opencode_go', 'typesafe'));
