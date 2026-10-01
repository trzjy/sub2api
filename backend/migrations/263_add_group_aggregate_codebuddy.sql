-- 263_add_group_aggregate_codebuddy.sql
-- 聚合直绑分组开关（方案 docs-local/codebuddy-b2-aggregate-switch-plan.md §1.1）：
-- 分组级聚合绑定开关。true=该聚合族分组候选池并入 codebuddy；false=关闭（默认），
-- 该分组任何生产选号路径都看不到 codebuddy 账号。
-- 仅聚合族平台（deepseek/zhipu/kimi/minimax/other/codebuddy）可置 true，否则
-- 创建/更新被拒（400 AGGREGATE_BINDING_NOT_SUPPORTED）。
-- 默认 false=关闭：部署后存量聚合分组行落 false，候选池不含 codebuddy，直至逐分组开绑定。
-- 幂等（IF NOT EXISTS），无数据回填。禁止 DROP（降级停止条件=仅应用回滚、列与数据保留）。

ALTER TABLE groups
ADD COLUMN IF NOT EXISTS aggregate_codebuddy_enabled BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN groups.aggregate_codebuddy_enabled IS
    '聚合直绑开关：true=聚合族分组候选池并入 codebuddy；false=关闭（默认），该分组不可见 codebuddy 账号';
