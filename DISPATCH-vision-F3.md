# 派发单 Vision-F3：admin API 接线（D 单缺失的 handler/路由/wire）

## 背景
方案：`docs/capability-routing-plan.md` v3.1 §3.5/§3.7（已过闸①）。
D 单只落了 service/repo 层：`VisionDetectService`
（`backend/internal/service/vision_detect_service.go`）、`VisionRoutingService`
（`vision_routing_service.go`，含同组校验）、能力表 repo（`account_model_capability_repo.go`）
均无 handler 调用、无路由注册、无 wire 接线。本单把它们接到 admin API。

**API 契约（主会话已裁定，前端并行单按同一契约开发，字段名不得偏离）**：
- 检测（单账号）：`POST /api/admin/accounts/:id/vision-capability/detect`
  body `{"model": "...", "protocol": "chat_completions"}`
  → `{"status": "supported|unsupported|detect_failed|manual_review", "supports_vision": true|false|null}`
- 检测（批量）：`POST /api/admin/accounts/vision-capability/detect`
  body `{"account_ids": [..], "items": [{"model":"..","protocol":".."}]}`
  → `{"results": [{"account_id":..,"model":"..","protocol":"..","status":"..","supports_vision":..}]}`
- 手动覆盖：`PUT /api/admin/accounts/:id/vision-capability`
  body `{"model":"..","protocol":"..","supports_vision": true|false}`
  （落库 source=manual，优先级最高）
- 分组分流配置：**不新增端点**，在既有 group create/update API 的 DTO 上加
  `vision_routing`（map[模型名][]账号ID），同构 `model_routing` 现有字段路径。

## 改动范围
- 新文件 `backend/internal/handler/admin/vision_capability_handler.go`（+同名测试文件）：
  三个端点，调用既有 VisionDetectService / 能力表 repo/service，落库与缓存
  失效广播复用 B 单既有链路；detect_failed 不落库（方案 §3.5 两级规则已在
  service 实现，handler 只透传，不得重复实现判定逻辑）。
- `backend/internal/handler/admin/group_handler.go` + `backend/internal/handler/dto/types.go`
  + `mappers.go`：group 请求/响应 DTO 增加 `VisionRouting map[string][]int64`
  （json:`vision_routing`），同构 `ModelRouting`（group_handler.go:228/305 现有路径）；
  group create/update 链路写入前调用 VisionRoutingService 的同组校验
  （service 已实现，跨组拒绝），校验失败返回 4xx 明确错误。
- 路由注册：同构既有 admin account 路由的注册位置（自行检索仓内 admin 路由
  注册点，保持同一风格）。
- wire 接线：`backend/cmd/server/wire.go` provider 集 + 用 `~/go/bin/wire`
  重新生成 `wire_gen.go`（仓内既有生成物，照既有惯例）。
- admin 权限中间件与既有 admin 端点同构。

## 禁区
- 不得改 service 层语义（vision_*_service.go、能力表 repo、调度器、group.go）。
- 不得改前端。
- 禁止兜底分支（合同 §5）；边界外发现 BLOCKED 顶回。

## 验证命令白名单（只允许跑以下命令）
- `cd backend && go build ./...`
- `cd backend && go test ./internal/handler/admin/ -run 'Vision' -count=1`
- `cd backend && go test ./internal/handler/ -run 'TestGroup' -count=1`（group DTO 改动回归）

## Done when
- 三端点 + group DTO 字段 + 同组校验接线 + wire 构建全部落地，`go build ./...` 通过。
- 新 handler 测试 PASS（覆盖：检测成功落库、detect_failed 不落库、覆盖写 manual、
  分组跨组账号被拒）。
- `git status --short` 快照改动仅限上述文件 + wire 生成物。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
