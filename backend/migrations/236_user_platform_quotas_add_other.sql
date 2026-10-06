-- 把通用 OpenAI 兼容上游平台 other 加入 user_platform_quotas.platform 的 CHECK 约束。
--
-- 背景：other（internal/service/domain_constants.go PlatformOther）进入 AllowedQuotaPlatforms
-- 后，注册时 GetDefaultPlatformQuotas 会为全部允许平台预填充默认配额行，但 224 号迁移的
-- CHECK 仍只允许 8 平台。BulkInsertInitial 是单条多行 INSERT，任一违约行会中止整条语句 →
-- 注册路径 fail-open 吞错 → 新用户拿到零条配额记录（含原有 8 平台，缺失配额行 = 无限额）。
-- 与 157/224 头注释记载的 grok、kimi/zhipu/deepseek 同型事故一致。
--
-- 修复：把约束与代码平台列表（PlatformOther）对齐。
-- DROP ... IF EXISTS 保证可重入；新约束是旧约束的超集，存量行瞬时校验通过。
ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'other'));
