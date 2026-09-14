-- 250_codebuddy_shadow_quota_dimension.sql
-- 扩展 quota_dimension 枚举：新增 'codebuddy'（CodeBuddy OAuth 母账号的影子维度）。
--
-- 背景：CodeBuddy 上游-影子账号方案把 CodeBuddy OAuth 母账号的"一母多影"做成
-- 独立 quota_dimension='codebuddy' 的影子账号（platform=目标分组平台、凭证透传母账号）。
-- 这样既能在调度热路径用纯函数 codeBuddyRPMGated 区分"codebuddy 影子"（按母账号 RPM
-- 聚合防 N 倍超售），又不与 'spark' 维度（OpenAI OAuth 母账号的 spark 影子）语义混淆。
--
-- 仅放宽 chk_accounts_quota_dimension 的枚举白名单，不改变默认值（'global'）与
-- chk_accounts_parent_dimension 业务约束（parent_account_id IS NOT NULL 且
-- quota_dimension <> 'global'——'codebuddy' 天然满足）。幂等：DROP/ADD 受存在性守卫保护。

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'chk_accounts_quota_dimension'
  ) THEN
    ALTER TABLE accounts DROP CONSTRAINT chk_accounts_quota_dimension;
  END IF;
  ALTER TABLE accounts ADD CONSTRAINT chk_accounts_quota_dimension
    CHECK (quota_dimension IN ('global', 'spark', 'codebuddy')) NOT VALID;
END $$;

ALTER TABLE accounts VALIDATE CONSTRAINT chk_accounts_quota_dimension;
