# 派发单 Vision-E：前端 UI（分流配置 + 检测按钮）

## 背景
方案：`docs/capability-routing-plan.md` v3.1 §3.5/§3.7（已过闸①）。
后端并行开发中，以下 API 契约由主会话裁定，**前后端按同一契约开发**，
字段名不得偏离；后端联调留收敛阶段。

**API 契约**：
- 检测（单账号）：`POST /api/admin/accounts/:id/vision-capability/detect`
  body `{"model": "...", "protocol": "chat_completions"}`
  → `{"status": "supported|unsupported|detect_failed|manual_review", "supports_vision": true|false|null}`
- 检测（批量）：`POST /api/admin/accounts/vision-capability/detect`
  body `{"account_ids": [..], "items": [{"model":"..","protocol":".."}]}`
  → `{"results": [{"account_id":..,"model":"..","protocol":"..","status":"..","supports_vision":..}]}`
- 手动覆盖：`PUT /api/admin/accounts/:id/vision-capability`
  body `{"model":"..","protocol":"..","supports_vision": true|false}`
- 分组配置：既有 group create/update API 新增字段
  `vision_routing: Record<string, number[]>`（模型名 → 本组账号 ID 列表），
  与 `model_routing` 同层同形。

## 改动范围
- `frontend/src/views/admin/GroupsView.vue`（及其子组件，如需抽组件放
  `frontend/src/components/admin/group/`）：模型路由配置区为每个模型增加
  "视觉分流目标账号"多选，**账号选项仅限该分组已有账号**（方案 §3.7 同组
  约束的前端镜像；后端仍会校验），提交随 group 保存走 `vision_routing` 字段。
- `frontend/src/views/admin/AccountsView.vue` 或 `frontend/src/components/admin/account/`
  下账号操作区：新增"检测视觉能力"操作（选择模型+协议 → 调单账号检测 API →
  四态结果展示：支持/不支持/检测失败/需人工裁决；manual_review 态提供
  "标记为支持/标记为不支持"按钮走手动覆盖 API）。
- API client：按仓内既有 admin API client 组织方式新增调用（先看现有
  admin 账号/分组请求写在哪，同构跟随）。
- i18n：`frontend/src/i18n/locales/zh/admin/`（及 en 如仓内惯例要求双语）
  增加相应文案 key。

## 禁区
- 不动 backend 任何文件。
- 不重构既有组件/不改既有逻辑；只做增量。
- 边界外发现 BLOCKED 顶回。

## 验证命令白名单（只允许跑以下命令）
- `cd frontend && npm run typecheck 2>/dev/null || npx vue-tsc --noEmit`（以 package.json 既有 scripts 为准，选类型检查命令）
- `cd frontend && npm run build`（如耗时过长可只跑 typecheck 并注明）

## Done when
- 分组页可配置每模型视觉分流目标账号（限本组账号），随保存提交 vision_routing。
- 账号页可触发检测并展示四态结果，manual_review 可手动覆盖。
- 类型检查通过。
- `git status --short` 快照改动仅限 frontend/ 下相关文件。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
