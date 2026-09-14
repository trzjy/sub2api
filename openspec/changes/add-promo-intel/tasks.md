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


## 7. 上线后迭代（2026-09-14，用户反馈驱动；全部已部署并验收）

- [x] 7.1 整理模型改「本系统中转网关」自选：source(self/external) + protocol(openai/anthropic) + 管理员 API Key 下拉（仅管理员本人，带分组/可用模型）+ 模型候选；Anthropic /v1/messages 原生支持（ea6045e3e、dbc956a67、b331522fe）
- [x] 7.2 连通性测试失败明文化：模型白名单拒绝附被拒模型名+可用清单、端点不可达分类，不再兜底 internal error（ac8e7707d、82a73afef）
- [x] 7.3 每日简报「今日速读」：LLM 汇总当日新增 + 近 7 天仍有效（pending+high），当日缓存 + refresh 强制重生成 + 无 LLM 文本兜底；前端速读卡 + 重新生成按钮（2692b375c、973037249、ed0e24cc0）
- [x] 7.4 上线热修：到期扫描 SQL ::timestamptz、摘录 rune 截断 + NUL 剔除、补跑通道（0ffb37b5e、b2bfa9353、1070f7b39）
- [x] 7.5 生产实测与验收证据落盘：/home/zjy/.sub2api-acceptance/promo-intel-20260914/（00-deploy + 10-live-api + 20-db-state + 99-final-report，脱敏）

## Non-goals 增补（演进后）
- 原第 3 条「LLM 整理层：可配置独立 OpenAI 兼容端点」已演进为：默认 self（本系统中转网关 + 管理员已有 Key + 任意已有模型，双协议）；external 自定义端点降级为高级选项。
- 已知限制（记录不阻塞）：速读切日已改北京时间（c4f761bde，原为 UTC）；速读缓存按当日条目数失效；「有用」条目不参与速读聚合；promo_intel.server_port 为 wire 传入 serverBaseURL 缺失时的兜底（当前恒有值，实际未生效）。
- [x] 7.6 简报切日改北京时间（用户要求）：digest_date 归属与默认今天统一东八区；切日边界回归测试；顺带修复 main 上 TestParentHealthyForShadow 双定义编译阻断（c4f761bde）
