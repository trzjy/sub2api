-- 把 Muse 平台（domain.PlatformMuse = "muse"，OpenCode Go 上游 Meta Muse Spark
-- Contributor）加入 user_platform_quotas.platform 的 CHECK 约束（与 224/236/237/244 同型）。
--
-- 背景：muse 进入 AllowedQuotaPlatforms（后续 muse 单，internal/service/domain_constants.go）
-- 后，注册时 GetDefaultPlatformQuotas 会为全部允许平台预填充默认配额行。若代码白名单加了
-- muse 而 DB CHECK 未放开，会精确复现 224 迁移注释记载的"注册路径 fail-open → 新用户零配额
-- 行（缺失 = 无限额）"事故。本迁移把约束与代码平台列表对齐。
-- DROP ... IF EXISTS 保证可重入；新约束是旧约束的超集，存量行瞬时校验通过。
--
-- 本单范围：muse-1 仅动 user_platform_quotas.platform CHECK 这一处（核心注册层）。
-- composite_model_routes.target_platform 与 channel_monitors/..._request_templates.provider
-- 两张 CHECK 在 muse-2/3/4/5 接线时再放开，本单不扩大改动。
--
-- 兼容性证明（外审-6 强制）：
--   ① 发布窗口内「新版本写入 muse、旧版本读取」不崩：
--      user_platform_quotas.platform 是带 CHECK 的普通 text 列，ent 的 Validate 仅在写入路径
--      执行、读取路径不触发；旧版本二进制读取 platform='muse' 的存量行只是把它当作未知平台
--      （不出现在旧版 AllPlatforms/AllowedQuotaPlatforms），不会 panic 或中断事务。
--      即"写新读旧"安全：新版本写入的 muse 配额行，旧版本能正常读出、仅不在 UI/调度中暴露。
--   ② 回滚不破坏已有 quota 数据：
--      本约束是 CHECK（非原生 PG enum 类型），回滚 = DROP 现有约束后重建不含 muse 的超集约束，
--      纯 DDL、不触碰任何存量数据行，已有 quota 行原样保留、瞬时通过校验。
--
-- 枚举/约束保留策略（不可逆性说明）：
--   原生 PG enum 的 ADD VALUE 属不可逆操作（旧快照无法回放）；本仓库 user_platform_quotas.platform
--   用的是 CHECK 约束而非 enum 类型，故约束本身可正反向重放、不具 enum 那种不可逆性。
--   唯一不可逆风险点是"数据语义"：一旦线上写入 platform='muse' 的配额行，若随后把 CHECK 回滚到
--   不含 muse，这些 muse 行在后续任何 UPDATE/INSERT 触碰时会因违反 CHECK 而失败。保留策略：
--   回滚约束前必须先把 platform='muse' 的配额行删除/改回既有平台；muse 平台退役应按此顺序操作，
--   不可直接重放不含 muse 的约束。本迁移对"未写入 muse 行"的库永远安全、可重入。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'minimax', 'other', 'codebuddy', 'muse'));
