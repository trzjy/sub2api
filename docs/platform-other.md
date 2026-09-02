# 平台接入方案：通用 OpenAI 兼容上游平台 `other`

> 日期：2026-09-03 ｜ 分支：`feat/custom-other-platform` ｜ 状态：**待外部审查（阶段一）**

## 1. 背景与目标

Sub2API 的平台标识是编译期硬编码，分散在十几处名单（调度/路由/base-URL 门控/模型/用量/配额/前端类型/配色/i18n/DB CHECK）。现有 8 个具名平台 `anthropic/openai/gemini/antigravity/grok/kimi/zhipu/deepseek`。

运营者希望接 **任意 OpenAI 兼容上游**（以非主流第三方厂商为主；将来即便有想接的官方模型，也走同一入口），并明确**不愿为每个厂商各建一个具名平台**（样板成本每厂商付一次，长尾不划算）。

用户决策：
1. **只新增一个通用平台**，内部标识 `other`，展示名 Other；
2. minimax/qwen 等厂商**不做任何官方预设**——需要时就以 `other` 平台、自填 base_url/api_key/model_mapping 的方式添加；官方模型同样走此第三方/自定义入口；
3. `other` 平台作为 OpenAI 兼容自定义上游，能力与具名 OpenAI 兼容平台看齐（见 §3）；**支持「用户 × 平台 USD 配额」**；**兼容 Anthropic 系客户端（Claude Code 打 `/v1/messages`）**。

## 2. 语义与权威源

`other` 的语义：**OpenAI 兼容 + API-Key + 按量付费的自定义上游**（base_url 由运营自填，指向任何 ChatCompletions 兼容端点）。平台标识只新增一个，所有第三方厂商共用此桶；厂商之间的区分靠「账号 base_url + 账号 model_mapping + 群组命名」。

平台「可枚举」权威点（新增 `other` 必须同步，除末项外均为本次改动）：

| 权威面 | 位置 |
|---|---|
| 平台常量 | `backend/internal/domain/constants.go`、`backend/internal/service/domain_constants.go` |
| 配额白名单（单一权威源） | `service/domain_constants.go` `AllowedQuotaPlatforms` |
| 配额 ent 构建期校验 | `backend/ent/schema/user_platform_quota.go` `Validate` |
| 配额 DB CHECK | `backend/migrations/*`（须与上同批） |
| 前端类型/目录/配色/徽章/i18n | `frontend/src/types/index.ts`、`constants/platforms.ts`、`utils/platformColors.ts`、`components/common/PlatformTypeBadge.vue`、i18n `en\|zh` |
| 账号/群组表 platform | 普通字符串列，不触发迁移 |

## 3. 架构裁决

- **走 OpenAI 兼容协议族**：`account.IsOpenAICompatible()` 纳入 `other`；`NormalizeOpenAICompatiblePlatform` 保留 `other` 原值；路由 family 门（§4.3）纳入，否则三主端点落错网关。
- **不并入 `IsCNProvider()` 全局语义**：CN 专属余额/额度探测、Coding-Plan 窗口限流、CN 前端面板均保持 CN-only，避免对无官方余额端点的自定义上游误触发。
- **新增窄谓词**（service 层）：把「走共享 OpenAI base_url/api_key 的平台族」统一 = `openai || IsCNProvider() || other`（**含 openai OAuth**——替换现门不得回归；**排除 grok/composite**）。用于 base/key/模型同步等公共能力点，替换 `IsOpenAI()||IsCNProvider()`。
- **无厂商默认 base_url**：`other` 平台不内置任何官方上游默认值；`GetOpenAIBaseURL` 对 `other` 仅返回账号 `credentials.base_url`，为空即空（**不回落 api.openai.com**），账号创建层强制 base_url 必填。
- **Anthropic 客户端兼容**：`/v1/messages` → OpenAI 网关 → `shouldForwardOpenAIResponsesViaRawChatCompletions`（非 CN API-Key 亦为 true）→ 现成 Messages→CC 转换；`GetAPIProtocol` 对非 CN 恒返 `chat_completions`，天然锁 CC 协议。不依赖上游 anthropic 端点。
- **无静态模型目录**：`other` 无平台默认模型清单；用户侧可见模型 = 组内账号 model_mapping 键并集。运营添加厂商时必须写 model_mapping（可用账号创建时的 `/v1/models` 预览回填）。
- **分组与调度**：`other` 只作单平台分组（group.platform=other），**不作 composite 目标**。同一 other 分组混多家厂商账号时按账号 base_url 与 model_mapping 承担各自模型；运营宜按厂商各建 other 分组（文档指引），避免模型冲突。

## 4. 改动清单

### 4.1 常量与谓词（后端）
- `backend/internal/domain/constants.go`：`PlatformOther = "other"`（附语义注释）。
- `backend/internal/service/domain_constants.go`：别名；`UsesOpenAIProtocolSharedBaseURL` 自由函数（放 IsCNProvider 旁）。
- `backend/internal/service/account.go`：`IsOpenAICompatible()`（L296）纳入 other；新增谓词方法。

### 4.2 调度 / 路由 / 转发
- `openai_gateway_scheduling.go` `NormalizeOpenAICompatiblePlatform`（L293）：保留 `other` 原值。
- `server/routes/gateway.go` `isOpenAIResponsesCompatibleGatewayPlatform`（L48）与 `countTokensHandler`（L58）：other 入 OpenAI 网关族。
- `openai_gateway_forward.go` `shouldForwardOpenAIResponsesViaRawChatCompletions`（L1239）：other 恒 `true`。

### 4.3 base / key 门控
- `account.go` `GetOpenAIBaseURL`（L1335 门 → 窄谓词）：other 仅读账号 base_url，**空即空**（新增回归：不回落 api.openai.com）。可在 default 分支加 case 返回 "" 而非 openai.com。
- `account.go` `GetOpenAIProtocolAPIKey`（L1675）：放行 other apikey 读取。

### 4.4 公共能力
- `upstream_models.go`（L516/L636）case → 窄谓词（/v1/models）。
- `openai_gateway_count_tokens.go`（L152/L274）：other 入本地估算分支。
- `openai_gateway_handler.go` `allowOpenAICompatibleMessagesDispatch`（L217）：other 豁免。
- `openai_messages_dispatch.go` `ResolveMessagesDispatchModel`（L85）：other 返回 ""（改写交给账号 model_mapping）。
- `openai_gateway_usage.go` `filterCNProviderBillingModelCandidates`（L865）：other 入（防未定价 claude-* 误计高价）。
- `handler/endpoint.go` `GetUpstreamEndpoint`（L318）：窄谓词。

### 4.5 账号生命周期 / 分组
- `account_service.go` `TestCredentials`（L497）：other 并入可用分支。
- `account_test_service.go`（L299）：other → 通用 chat-completions 探活。
- `admin_account.go` `buildAccountForCreate` / `account_service.go` Create：**平台→type 校验**：other 仅 `apikey`，且 `credentials.base_url` **必填**（后端权威化）。
- `account_handler.go` `scheduleOpenAIResponsesProbe` + `openai_apikey_responses_probe.go` `ProbeOpenAIAPIKeyResponsesSupport`：other 落标 `auto`+`supported=false`、不发网络探测。
- `group_handler.go` platform `oneof`（L101/L169）加 `other`；`target_platform` oneof（L237）不动。

### 4.6 配额三件套（同批，见 §6）
- `service/domain_constants.go` `AllowedQuotaPlatforms` 追加 `other`。
- `ent/schema/user_platform_quota.go` `Validate` 追加 `other`。
- 新迁移 `backend/migrations/236_user_platform_quotas_add_other.sql`（DROP+ADD CHECK 全平台列表含 other，仿 157/224 可重入）+ 同前缀迁移测试。

### 4.7 前端
- `types/index.ts`：`AccountPlatform`/`GroupPlatform` 加 `'other'`。
- `constants/platforms.ts`：CONCRETE 加 `{ value:'other', label:'Other' }`；**composite 目标专用列表过滤 other** 并替换 `GroupsView.vue` composite 目标下拉（L4766）。
- `utils/platformColors.ts`：平台联合 + 色表 + 守卫 + label。
- `PlatformTypeBadge.vue`（default 误标 Gemini，必加 case）、`PlatformIcon.vue`（通用/自定义图标）。
- `CreateAccountModal.vue`：other 瓦片（type=apikey，base_url 必填提示，不渲染 OAuth/CN 面板）；`EditAccountModal.vue` 通用 apikey/base_url 段。
- i18n（en/zh 成对）：`admin/accounts.ts` platforms、`admin/overview.ts` groups.platforms。`dashboard.ts` 渠道监控/quickstart 不加。
- 配额/用量 UI：后端名单驱动自动出现，仅补标签/配色。

## 5. 范围红线（不做）

- minimax/qwen 及其它厂商的 base_url/模型**预设**、具名展示、品牌图标：一律不做（首次真实使用由运营在 other 下自填，见文档 §1 决策）。
- `AllowedSchedulingThresholdPlatforms`、CN 余额/额度探测、`account_mode` coding、CN 前端面板：不进（语义不适合）。
- composite 目标：`isConcreteRequestPlatform`、`target_platform` oneof、composite 聚合/默认模型、`composite_model_routes` DB CHECK 均不动。
- channel_monitor provider：不动（渠道监控不对 other 开放）。
- 上游账单探测 `IsUpstreamBillingProbeIdentity`：不加。
- README 不列平台总表；本任务仅 docs 文档。

## 6. 影响分析与事故史

- 配额 DB CHECK 同步事故（`backend/migrations/224_...sql` 注释）：只加 service 名单不加 DB CHECK → 注册 `BulkInsertInitial` 单条多行 INSERT 被整条中止 + fail-open 吞错 → 新用户配额行全丢。故 `AllowedQuotaPlatforms` 与 ent `Validate`、DB CHECK 迁移**同批**。
- `api_contract_test.go` `default_platform_quotas` 整串 JSON fixture 由 `AllowedQuotaPlatforms` 动态生成，需补 `"other"` 键。
- 用量/账单用原始 platform 字符串，无白名单拒绝 → other 计量天然可用。
- 存量 `default_platform_quotas` 缺 other = 不限（null）。
- 402/403/429 走 openai 通用分支；other 无余额端点，不做自动恢复。
- 调度语义待实现时确认：other 分组内多账号按 model_mapping/base_url 匹配（文档指引按厂商建 other 分组）。

## 7. 验证矩阵

- 后端：`go build ./...`、`go vet ./internal/...`、`go test ./... && golangci-lint run ./...`（backend/Makefile）；定向迁移/契约/调度/配额/handler 单测。
- 前端：`pnpm --dir frontend run typecheck`、`lint:check`、定向 vitest（platforms.spec.ts、SettingsView.spec.ts）。
- 端到端（需真实第三方 OpenAI 兼容端点 Key；无 key 以单测+冒烟为主并标注待真实验收）：建 other apikey 账号（base_url 指向某第三方兼容端点）+ model_mapping → 建 other 分组 → 绑定 → 分别以 OpenAI 兼容客户端与 Claude Code（/v1/messages、/v1/messages/count_tokens）调用 → 核对 usage 平台归属与计费。

## 8. done_when

- 阶段一：本方案 commit 经 Codex 只读外审，当前边界 must_fix 修订收敛。
- 阶段二：实现 diff（方案+实现+测试）经 Codex 复核，must_fix 关闭。
- §7 可在本环境执行的项全部通过；真实端点链路标注「待真实验收」，不冒充完成。
- §5 范围红线无泄漏；残留扫描：other 在 §2 各权威点齐备、无 CN 专属误触发、无 composite/channel_monitor 泄漏、无其它厂商预设残留。
