-- Add OpenCode as a first-class platform (account types Zen / GO).
--
-- 1. user_platform_quotas.platform CHECK
-- 2. composite_model_routes.target_platform CHECK
-- 3. channel_monitors / channel_monitor_request_templates provider CHECK
--
-- Runs after 237_add_minimax_platform.sql. DROP ... IF EXISTS + 幂等守卫保证可重入。
--
-- ============================ 平台清单的依据（2026-10-16 修订） ============================
-- 本文件原先把 user_platform_quotas / composite_model_routes 的白名单写成上游的 10 平台
-- （anthropic/openai/gemini/antigravity/grok/kimi/zhipu/deepseek/minimax/opencode_go），
-- 漏掉 fork 侧自研平台 codebuddy 与 other。ADD CONSTRAINT 会对全表做校验，生产
-- user_platform_quotas 存量 2578 行中 codebuddy 232 + other 233 = 465 行不在白名单内，
-- 直接导致迁移失败、容器起不来（2026-10-16 部署 f53553f45 → 0b3862111 即因此回滚）。
--
-- 现改为与 288_platform_check_constraints_superset.sql **逐字符相同**的 16 平台超集，
-- 使 281 与 288 幂等且一致（288 再次执行只会 DROP 后加回同一份定义）。
--
-- 16 平台清单的推导（以代码为 SSOT，不以 288 为 SSOT）：
--   ① internal/domain/platforms.go 的 platformList（15 个具体平台，不含 other）
--      → anthropic, openai, gemini, antigravity, grok, kimi, zhipu, deepseek, minimax,
--        opencode_go, typesafe, command_code, cline, codebuddy, muse
--   ② internal/service/domain_constants.go 的 AllowedQuotaPlatforms（11 项，配额侧 SSOT）
--      → 相对 ① 多 other；这是 platformList 刻意不登记的唯一平台（见 platforms.go 头注：
--        other 不是「具体平台」，但配额链路显式放行）。
--   ③ 并集 = ① ∪ ② = 16 项，与 288 的列表完全一致（无差异）。
--   注：ent/schema/user_platform_quota.go 的构建期校验是 12 项（① 的子集 + other + muse），
--   比 DB 约束更窄；它是 ent 层的写入校验，不参与本迁移的表级 CHECK，无需对齐。
--
-- channel_monitors / channel_monitor_request_templates 的 provider 白名单用 12 项，
-- 与上面 16 项**故意不同**：provider 表达的是「可探测的协议族」而非平台注册表，
-- 取「生产现行白名单 ∪ admin handler oneof 白名单」的并集，既不缩窄生产已有能力，
-- 也不遗漏代码已放行的值：
--   生产现行（2026-10-16 实读 pg_constraint）：openai, anthropic, gemini, grok, antigravity,
--     kimi, zhipu, deepseek, minimax, codebuddy   ← 含 codebuddy，本文件原版会把它删掉
--   admin handler oneof（internal/handler/admin/channel_monitor_handler.go）：
--     openai, anthropic, gemini, grok, antigravity, kimi, zhipu, deepseek, minimax,
--     muse, opencode_go                            ← 含 muse/opencode_go，原版缺
--   并集 = 12 项（上面两者合并）。typesafe/command_code/cline 未纳入：渠道监控探针
--   （internal/service/channel_monitor_probe.go）未声明这三族的探测能力，纳入即放行
--   无实现支撑的 provider。
--
-- 可重入：DROP ... IF EXISTS 先删后加；channel_monitors / request_templates 分支改为
-- 「逐个成员核对，缺任一成员才重建」，因此上一轮留下的部分正确状态也能收敛到目标定义。
-- 与 288 无冲突：288 只重建同两张表的同名约束且用的是同一份 16 项列表。

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

DO $$
DECLARE
    monitor_constraint_def TEXT;
    template_constraint_def TEXT;
    required_member TEXT;
    monitor_needs_rebuild BOOLEAN;
    template_needs_rebuild BOOLEAN;
BEGIN
    -- 渠道监控可探测协议族白名单（= 生产现行 ∪ handler oneof）。
    monitor_needs_rebuild := FALSE;
    SELECT pg_get_constraintdef(c.oid)
      INTO monitor_constraint_def
      FROM pg_constraint c
      JOIN pg_class t ON t.oid = c.conrelid
     WHERE t.relname = 'channel_monitors'
       AND c.conname = 'channel_monitors_provider_check';

    IF monitor_constraint_def IS NULL THEN
        monitor_needs_rebuild := TRUE;
    ELSE
        FOREACH required_member IN ARRAY ARRAY[
            'openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi',
            'zhipu', 'deepseek', 'minimax', 'codebuddy', 'muse', 'opencode_go']
        LOOP
            IF position(required_member IN monitor_constraint_def) = 0 THEN
                monitor_needs_rebuild := TRUE;
                EXIT;
            END IF;
        END LOOP;
    END IF;

    IF monitor_needs_rebuild THEN
        ALTER TABLE channel_monitors
            DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;
        ALTER TABLE channel_monitors
            ADD CONSTRAINT channel_monitors_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok',
                                'antigravity', 'kimi', 'zhipu', 'deepseek', 'minimax',
                                'codebuddy', 'muse', 'opencode_go'));
    END IF;

    template_needs_rebuild := FALSE;
    SELECT pg_get_constraintdef(c.oid)
      INTO template_constraint_def
      FROM pg_constraint c
      JOIN pg_class t ON t.oid = c.conrelid
     WHERE t.relname = 'channel_monitor_request_templates'
       AND c.conname = 'channel_monitor_request_templates_provider_check';

    IF template_constraint_def IS NULL THEN
        template_needs_rebuild := TRUE;
    ELSE
        FOREACH required_member IN ARRAY ARRAY[
            'openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kimi',
            'zhipu', 'deepseek', 'minimax', 'codebuddy', 'muse', 'opencode_go']
        LOOP
            IF position(required_member IN template_constraint_def) = 0 THEN
                template_needs_rebuild := TRUE;
                EXIT;
            END IF;
        END LOOP;
    END IF;

    IF template_needs_rebuild THEN
        ALTER TABLE channel_monitor_request_templates
            DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;
        ALTER TABLE channel_monitor_request_templates
            ADD CONSTRAINT channel_monitor_request_templates_provider_check
            CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok',
                                'antigravity', 'kimi', 'zhipu', 'deepseek', 'minimax',
                                'codebuddy', 'muse', 'opencode_go'));
    END IF;
END $$;
