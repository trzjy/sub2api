# Tasks

## 1. 数据库与仓储

- [x] 1.1 新增 `backend/migrations/240_custom_model_pricing.sql`：`custom_model_pricing`（models JSONB、billing_mode、token 单价列、per_request_price、enabled、remark、created_by、时间戳）与 `custom_model_pricing_intervals`（对齐 channel_pricing_intervals，FK 指向 custom 表），幂等 DDL + 索引 + COMMENT
- [x] 1.2 新增 `internal/repository/custom_model_pricing_repo.go`：List/Create/Update/Delete/SetEnabled，区间批量读写复用 channel_repo_pricing.go 的既有 helper
- [x] 1.3 注册进 `internal/repository/wire.go` ProviderSet

## 2. 自定义价格服务与解析链接入

- [x] 2.1 新增 `internal/service/custom_model_pricing_service.go`：原子快照缓存 + 60s 刷新 + CRUD 后即时刷新；`Match(model)` 精确 + `*` 通配（仅 enabled），返回 Clone
- [x] 2.2 `ModelPricingResolver` 增加 `SetCustomPricingProvider`（接口注入，nil 安全），`Resolve` 在渠道价之后、远程表之前插入 custom 层（token 与按次/图片模式均支持）；custom 为空时行为与现状一致
- [x] 2.3 单测：通配/禁用/克隆语义、解析优先级（custom 覆盖全局、custom 为空不改行为）、按次模式

## 3. 价格管理服务与接口

- [x] 3.1 `PricingService` 增加 SyncStatus 快照（lastUpdated/localHash/modelCount/lastError/lastAttempt/syncing）与 `SyncNow(ctx)`（CAS 防并发，复用下载链）
- [x] 3.2 新增 `internal/service/pricing_admin_service.go`：Catalog（remote ∪ builtin ∪ custom 合并、来源分层、TokenPricingAbsent 标注、搜索/分页）、UncoveredScan（分组配置扫 + usage_logs 近 N 天扫，判定复用 GetModelPricing/custom Match；tokens>0 且 actual_cost=0 打对账标记）、Preview（resolver 解析 + 分组倍率试算）
- [x] 3.3 usage 扫描复用既有 `GetModelStatsWithFilters` 聚合查询（usage_logs 分区表，走 created_at 条件），未新增 SQL
- [x] 3.4 网关计费 ErrModelPricingUnavailable 路径挂进程内缺口记录（按模型去重、cap 256），状态接口一并返回
- [x] 3.5 新增 `internal/handler/admin/pricing_handler.go` + 路由 `registerPricingRoutes`（GET status/catalog/uncovered/preview、POST sync、custom CRUD），挂入 AdminHandlers 与 wire（wire_gen 手工接线 + cleanup）

## 4. 验证（后端）

- [x] 4.1 `go build ./...` 与 `go vet` 通过
- [x] 4.2 新增单测全绿；billing/pricing/resolver 回归与 service 全量套件通过
- [x] 4.3 custom 为空时 resolver 输出与原行为一致（billing 全量回归通过）

## 5. 前端

- [x] 5.1 `api/admin/pricing.ts`（类型 + 接口封装）并注册进 `api/admin/index.ts`
- [x] 5.2 `views/admin/PricingView.vue`：同步状态卡片（含手动同步 + 线上缺口）、价格目录（搜索/来源筛选/分页 + 试算）、未覆盖列表（双通道标记 + 一键补价）、自定义价格 CRUD 对话框
- [x] 5.3 路由 `/admin/pricing` + 侧栏菜单项 + zh/en i18n（`admin.pricing.*`、`nav.pricing`）
- [x] 5.4 `vue-tsc --noEmit` 通过、生产构建通过、locale 防冲突测试通过
