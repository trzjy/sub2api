-- 260_add_group_vision_routing.sql
-- 图片能力分流方案（docs/capability-routing-plan.md §3.7）：
-- 分组模型路由配置增加"视觉分流目标账号列表"（vision_target_account_ids）。
--
-- 采用平行字段 VisionRouting（与现有 ModelRouting 同构、互不影响）：
--   {"model_pattern": [account_id1, account_id2], ...}
-- 例如: {"deepseek-v4.1-flash": [137]}
-- 语义：带图请求（RequireVision=true）且模型命中该配置 → 候选池限定为目标账号
-- （规则只收窄候选池，绝不豁免能力校验）。同组约束由 service 层配置写入时校验
-- （vision_target ⊆ 该分组账号），DB 层仅保证列存在。
--
-- 幂等（IF NOT EXISTS），不触碰任何既有列/表。

ALTER TABLE groups
ADD COLUMN IF NOT EXISTS vision_routing JSONB DEFAULT '{}';

COMMENT ON COLUMN groups.vision_routing IS
    '视觉能力分流目标账号（同平台，后台可配置）：{"model_pattern": [account_id1, account_id2], ...}；仅收窄带图请求候选池，不豁免能力校验；配置写入时强制 vision_target ⊆ 该分组账号（方案 3.7 I-3）';