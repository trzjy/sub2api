-- 删除已废弃的聚合直绑开关列（绑定即准入重构，2026-11-17 用户裁定）
ALTER TABLE groups DROP COLUMN IF EXISTS aggregate_codebuddy_enabled;
