# Tasks

## 1. 数据库与仓储

- [x] 1.1 新增 `backend/migrations/247_promo_intel.sql`：`promo_intel_sources`（name 唯一、vendor、category、url、fetch_interval_minutes、enabled、llm_extract、last_fetched_at、last_extracted_hash、last_status、last_error、created_by、时间戳）与 `promo_intel_items`（source_id FK CASCADE、vendor、category、title、summary、details、discount_info、valid_until、url、relevance、status、fingerprint 唯一、raw_excerpt、extract_status、digest_date、时间戳），幂等 DDL + 索引 + COMMENT
- [x] 1.2 新增 `backend/ent/schema/promo_intel_source.go`、`promo_intel_item.go` 并重跑 ent 代码生成
- [x] 1.3 新增 `internal/repository/promo_intel_repo.go`：源 CRUD/ListDue/MarkFetched、情报 UpsertByFingerprint/List/UpdateStatus/CountByDigestDate；注册进 `internal/repository/wire.go`
- [x] 1.4 迁移测试 `247_promo_intel_test.go`

## 2. 服务层：轮询与整理

- [x] 2.1 新增 `internal/service/promo_intel_types.go`：POJO、列表参数、类型/厂商/相关性/状态常量与校验
- [x] 2.2 新增 `internal/service/promo_intel_fetch.go`：HTTP 抓取（httpclient 池、超时/限长）+ 正文提取（剥 script/style/标签、实体解码、空白折叠）+ SHA256 指纹
- [x] 2.3 新增 `internal/service/promo_intel_extract.go`：LLM 整理（可配置 OpenAI 兼容端点 /chat/completions，业务定制 prompt，JSON 宽容解析）；未配置/失败降级「原文待整理」，恢复后按内容指纹自动补跑
- [x] 2.4 新增 `internal/service/promo_intel_service.go`：源 CRUD、Findings/Upsert 编排、Start/Stop 单循环（leader lock 防多实例、并发有界）、立即抓取、种子源幂等内置（仅验证通过的清单）
- [x] 2.5 单测：正文提取、指纹去重（未变跳过 LLM）、LLM JSON 解析（含代码围栏/非法 JSON 降级）、到期调度、种子幂等

## 3. 设置与配置

- [x] 3.1 `internal/config/config.go` 新增 `PromoIntelConfig`（enabled、scan_interval_seconds、fetch_timeout_seconds、fetch_max_bytes、llm_timeout_seconds、llm_max_text_chars、worker_concurrency、seed_defaults）+ viper 默认值
- [x] 3.2 `SettingService` 新增 `GetPromoIntelRuntime`：promo_intel_enabled / promo_intel_llm_base_url / promo_intel_llm_api_key / promo_intel_llm_model（fail-open 默认启用；密钥回读脱敏）

## 4. 管理端接口

- [x] 4.1 新增 `internal/handler/admin/promo_intel_handler.go`：源 CRUD + 立即抓取、情报列表/状态分诊、每日简报聚合、设置读写（密钥脱敏）+ 连通性测试
- [x] 4.2 路由 `registerPromoIntelRoutes`（挂 `/admin/promo-intel/*`，特性开关守卫）；注册进 AdminHandlers、handler/wire.go、cmd/server/wire.go cleanup 与 wire_gen 手工接线

## 5. 前端

- [x] 5.1 `api/admin/promoIntel.ts`（类型 + 接口封装）并注册进 `api/admin/index.ts`
- [x] 5.2 `views/admin/PromoIntelView.vue`：情报列表（筛选/分诊/原文跳转）+ 每日简报卡（按日聚合）+ 资讯源管理（CRUD/立即抓取）+ 整理模型设置（含测试按钮）
- [x] 5.3 路由 `/admin/promo-intel` + 侧栏菜单 + zh/en i18n（`admin.promoIntel.*`、`nav.promoIntel`）
- [x] 5.4 `vue-tsc --noEmit` 与生产构建通过

## 6. 种子源与验证

- [x] 6.1 四圈层候选源收集与逐个验证（可达 + SSR 正文可提取），仅通过的进入内置清单（含「仅公众号/无法抓取」标注清单）
- [x] 6.2 种子源以静态清单 + `seed_defaults` 开关落地，服务启动幂等补种
- [x] 6.3 `go build ./...`、`go vet` 通过；`go test -tags=unit ./...` 全仓全绿（顺带修复了分支上既有的测试编译破损与 settings 契约过期期望）；`vue-tsc`、`pnpm build`、i18n 完整性测试全绿
