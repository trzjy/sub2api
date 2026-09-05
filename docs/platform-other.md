# 平台接入方案：通用 OpenAI 兼容上游平台 `other`

> 日期：2026-09-03 ｜ 分支：`feat/custom-other-platform` ｜ 状态：**方案修订版（已并入外部审查 must_fix，待复核收敛）**

## 1. 背景与目标

Sub2API 的平台标识是编译期硬编码，分散在十几处名单。现有 8 个具名平台 `anthropic/openai/gemini/antigravity/grok/kimi/zhipu/deepseek`。

运营者希望接 **任意 OpenAI 兼容上游**（以非主流第三方厂商为主；将来即便要接官方模型也走同一入口），并明确**不为每个厂商各建一个具名平台**（样板成本每厂商付一次，长尾不划算）。

用户决策：
1. 只新增**一个通用平台**，内部标识 `other`，展示名 Other；
2. minimax/qwen 等**不做任何官方预设**——需要时以 `other` 平台自填 base_url/api_key/model_mapping 添加；官方模型亦然；
3. `other` 能力与具名 OpenAI 兼容平台看齐（见 §3）；支持**用户×平台 USD 配额**；**Anthropic 客户端（Claude Code `/v1/messages`）兼容范围见 §3.7 如实声明**。

**阶段边界**：阶段一只提交方案文档与 `docs/.gitignore` 白名单（本分支即如此），不做任何代码/测试改动。§4.7 fixture、调度快照、URL 失败关闭、模型空 mapping、Anthropic 字段级验收等全部属**阶段二实现**，届时以 diff + 测试结果提供证据；不得把方案清单当作实现证据。

## 2. 语义与权威源

`other` = **OpenAI 兼容 + API-Key + 自定义 base_url 的按量上游**。平台标识只新增一个，第三方厂商共用此桶；厂商间区分靠「账号 base_url + model_mapping + 群组命名」。

平台「可枚举」权威点（新增 `other` 必须同步）：

| 权威面 | 位置 |
|---|---|
| 平台常量 | `backend/internal/domain/constants.go`、`backend/internal/service/domain_constants.go` |
| 配额白名单（单一权威源） | `service/domain_constants.go` `AllowedQuotaPlatforms` |
| 配额 ent 构建期校验 | `backend/ent/schema/user_platform_quota.go` `Validate` |
| 配额 DB CHECK | `backend/migrations/*`（与上同批） |
| **调度快照平台集合**（外审 1） | `backend/internal/service/scheduler_snapshot_service.go`（平台数组 / canonical bucket / 批量重建 / reopen·retire 全生命周期） |
| 路由 family | `backend/internal/server/routes/gateway.go` |
| 前端类型/目录/配色/徽章/i18n | `frontend/src/types/index.ts`、`constants/platforms.ts`、`utils/platformColors.ts`、`components/common/PlatformTypeBadge.vue`、i18n `en\|zh` |
| 账号/群组表 platform | 普通字符串列，不触发迁移 |

## 3. 架构裁决

### 3.1 OpenAI 兼容协议族，不并入 CN 专属语义
`account.IsOpenAICompatible()` 纳入 `other`；`NormalizeOpenAICompatiblePlatform` 保留 `other` 原值。CN 专属余额/额度探测、Coding-Plan 窗口限流、CN 前端面板保持 CN-only，避免对自定义上游误触发。

### 3.2 窄谓词
service 层 `UsesOpenAIProtocolSharedBaseURL(platform)` = `openai || IsCNProvider() || other`（**含 openai OAuth**；**排除 grok/composite**），替换 base/key/模型同步等公共能力点上的 `IsOpenAI()||IsCNProvider()`，不得回归 openai/grok。

### 3.3 base_url：无默认、失败关闭（外审 2）
`other` 不内置任何上游默认值。**账号创建与更新的后端口径要求 `credentials.base_url` 必填**；并在此基础上做**纵深防御**：所有经 `GetOpenAIBaseURL()`/`GetOpenAIFormatBaseURL()` 的 URL 构建入口（CC 管道 `openai_gateway_cc_pipeline.go`、Responses、模型 `/v1/models`、count_tokens、embeddings 等）对 `other` 平台**空 base_url 一律返回明确配置错误**，禁止落入通用 `https://api.openai.com` fallback——防止存量账号/直写 DB/部分更新路径把第三方 key 打到 OpenAI 官方（凭证外发 + 不可预期费用）。`GetOpenAIBaseURL` 平台默认 switch 对 `other` 返回 ""（不进 default=api.openai.com 兜底分支）。

**统一失败契约**：`other` + 空 base_url 一律返回明确配置错误（HTTP 400 + 稳定错误码），由**集中 helper** 判定，禁止各入口自行回退。**引用点清单是阶段二实现第 0 步的固定产物**：全仓扫描 `GetOpenAIBaseURL()`/`GetOpenAIFormatBaseURL()` 全部调用点，以及任何把空值显式替换为 `https://api.openai.com` 的 fallback 点，逐点加失败关闭——清单以扫描结果提交，不以「…等」省略，并做旧 fallback 残留扫描（见 §7/§8）。

### 3.4 协议：固定 chat_completions
`GetAPIProtocol` 对非 CN 恒返 `chat_completions` → `other` 天然锁 CC。账号层不接受将其配成 anthropic/adaptive。

### 3.5 模型可见性与空 mapping（外审 3）
- 模型列表后端事实源 = 组内账号 `model_mapping` 键并集；`other` **无平台静态默认模型目录**，也不得落入 `defaultModelIDsForPlatform` 的 Claude 默认分支——`other` 分组无 mapping 时公开模型列表**必须返回空**。
- **空 mapping 不可调度**：调度/支持性判定对 `other` 账号要求显式非空 model_mapping，否则明确拒绝（不把任意模型原样发往第三方上游）。创建建议先填 mapping（`/v1/models` 预览可回填）。
- 使「公开模型列表」与「调度准入」使用同一事实源，避免列表显示某模型但调度不认（或反之）。

### 3.6 分组与调度语义（如实声明）
- `other` 只作单平台分组（group.platform=other），**不作 composite 目标**。
- **混厂保证是操作指引，不是系统契约**：同一 `other` 分组允许多账号，调度按账号 `IsModelSupported`/mapping 过滤后做负载选择，**没有 alias→厂商唯一性约束**；两个账号对同一公开 alias 映射到不同上游时行为未定义。文档与 UI 明确指引运营**按厂商各建 `other` 分组**以获得确定语义。

### 3.7 Anthropic 客户端兼容：范围如实收窄（外审 4）
`/v1/messages` → OpenAI 网关 → `shouldForwardOpenAIResponsesViaRawChatCompletions`（非 CN API-Key 亦 true）→ 现成 Messages→ChatCompletions 转换器（`openai_gateway_messages_chat_fallback.go` → `pkg/apicompat`）。**兼容范围 = 转换器实际覆盖的 Anthropic 子集**：
- **支持**：基础 messages 文本/多轮、基础 tool_use/tool_result 续接、流式。
- **降级/丢弃（如实不承诺保真）**：上游无对应能力时 `thinking` 内容块、server-side 工具（如 web_search）会被转换器丢弃；请求可能返回 200 但语义缺失。不能以「返回 200」充当完整语义兼容。
- 本任务**不重写转换器**。实现阶段验收 = **字段级正/负例断言**（不是「记录观察」）：①支持子集正例：基础文本/多轮、`tool_use`/`tool_result` 续接、流式、`cache_control`、`count_tokens` 逐字段断言往返；②降级项负例断言预期行为：`thinking` 内容块被丢弃、server-side 工具被丢弃/明确拒绝——**断言明确行为，不允许「HTTP 200 + 静默丢语义」当通过**；③无能力项返回明确结果。上述断言列入 done_when。需要完整保真的场景应接支持原生 anthropic 协议的上游或独立具名平台。

### 3.8 调度快照生命周期（外审 1）
`other` 须纳入调度快照全生命周期，与既有平台一致：
- 快照平台集合/可调度平台数组纳入 `other`；
- canonical bucket、批量账号事件重建、分组变更/全量重建、reopen/retire 对 `other` 生效；
- 补齐对应快照测试。否则新增/修改 `other` 账号后可能沿用陈旧候选集（Redis/快照模式）。

## 4. 改动清单

### 4.1 常量与谓词
- `backend/internal/domain/constants.go`：`PlatformOther = "other"`（附语义注释）。
- `backend/internal/service/domain_constants.go`：别名；`UsesOpenAIProtocolSharedBaseURL` 自由函数。
- `backend/internal/service/account.go`：`IsOpenAICompatible()` 纳入 other；新增谓词方法。

### 4.2 调度 / 路由 / 转发
- `openai_gateway_scheduling.go` `NormalizeOpenAICompatiblePlatform`：保留 other。
- `server/routes/gateway.go` `isOpenAIResponsesCompatibleGatewayPlatform` 与 `countTokensHandler`：other 入 OpenAI 网关族。
- `openai_gateway_forward.go` `shouldForwardOpenAIResponsesViaRawChatCompletions`：other 恒 true。
- **`scheduler_snapshot_service.go`：平台集合/bucket/重建/reopen/retire 纳入 other（外审 1）。**

### 4.3 base / key / URL（外审 2）
- `account.go` `GetOpenAIBaseURL` 门换窄谓词；平台 switch 对 other 返回 ""（不落 api.openai.com）。
- **所有 URL 构建入口对 other 空 base_url 失败关闭（阶段二第 0 步先产出全引用点清单）**：CC 管道 `openai_gateway_cc_pipeline.go`、Responses/chat `buildUpstreamRequest`、upstream_models `/v1/models`、count_tokens、embeddings 及扫描发现的一切入口；统一错误语义，禁止任何入口回落 `https://api.openai.com`。
- `account.go` `GetOpenAIProtocolAPIKey`：放行 other apikey 读取。

### 4.4 公共能力 / 模型（外审 3）
- `upstream_models.go`：case → 窄谓词。
- `openai_gateway_count_tokens.go`：other 入本地估算分支。
- `openai_gateway_handler.go` `allowOpenAICompatibleMessagesDispatch`：other 豁免。
- `openai_messages_dispatch.go` `ResolveMessagesDispatchModel`：other 返回 ""。
- `openai_gateway_usage.go` `filterCNProviderBillingModelCandidates`：other 入。
- `handler/endpoint.go` `GetUpstreamEndpoint`：窄谓词。
- **模型列表对 other 空返回空**：`defaultModelIDsForPlatform` / `gateway_service.go GetAvailableModels` 链路，other 不落 Claude 默认分支。
- **调度准入对 other 空 mapping 拒绝**（空 model_mapping 不可调度/明确 4xx）。

### 4.5 账号生命周期 / 分组
- `account_service.go` `TestCredentials`：other 并入可用分支。
- `account_test_service.go`：other → 通用 chat-completions 探活。
- `admin_account.go` `buildAccountForCreate` / `account_service.go` Create **与 Update**：平台→type 校验（other 仅 `apikey`）+ **base_url 必填**；model_mapping 建议非空（空 mapping 由 §4.4 调度拒绝兜底）。
- `account_handler.go` `scheduleOpenAIResponsesProbe` + `openai_apikey_responses_probe.go` `ProbeOpenAIAPIKeyResponsesSupport`：other 落标 auto+supported=false、不发网络探测。
- `group_handler.go` platform `oneof`：加 other；`target_platform` oneof 不动。

### 4.6 配额三件套（同批，见 §6）
- `service/domain_constants.go` `AllowedQuotaPlatforms` 追加 other。
- `ent/schema/user_platform_quota.go` `Validate` 追加 other。
- 新迁移 `backend/migrations/236_user_platform_quotas_add_other.sql`（DROP+ADD CHECK 含 other）+ 同前缀迁移测试。

### 4.7 测试与契约（外审完整性，**阶段二实现必需项**）
- **`backend/internal/server/api_contract_test.go`：`default_platform_quotas` 两处 fixture 补 `"other"` 键。**本方案阶段一仅提交 docs/.gitignore；该 fixture 连同全部代码改动属阶段二实现（§1 阶段边界）。

### 4.8 前端
- `types/index.ts`：`AccountPlatform`/`GroupPlatform` 加 `'other'`。
- `constants/platforms.ts`：CONCRETE 加 `{ value:'other', label:'Other' }`；composite 目标专用列表过滤 other（替换 `GroupsView.vue` L4766）。
- `utils/platformColors.ts`：联合 + 色表 + 守卫 + label。
- `PlatformTypeBadge.vue`（default 误标 Gemini，必加 case）、`PlatformIcon.vue`。
- `CreateAccountModal.vue`：other 瓦片（type=apikey、base_url 必填、引导先填 model_mapping、不渲染 OAuth/CN 面板）；`EditAccountModal.vue` 通用段。
- i18n（en/zh 成对）：`admin/accounts.ts` platforms、`admin/overview.ts` groups.platforms。`dashboard.ts` 不加。
- 配额/用量 UI：后端驱动，仅补标签/配色。

## 5. 范围红线（不做）
- 厂商 base_url/模型**预设**、具名展示、品牌图标：一律不做。
- `AllowedSchedulingThresholdPlatforms`、CN 余额/额度探测、`account_mode` coding、CN 前端面板：不进。
- composite 目标全套（`isConcreteRequestPlatform`、`target_platform` oneof、聚合/默认模型、`composite_model_routes` DB CHECK）不动。
- channel_monitor provider 不动。
- 上游账单探测 `IsUpstreamBillingProbeIdentity` 不加。
- **不重写 Anthropic Messages→CC 转换器**；其既有降级语义如实记录，不扩为保真。
- README 不列平台总表。

## 6. 影响分析与事故史
- 配额 DB CHECK 同步事故（migration 224 注释）：三件套必须同批，否则注册 `BulkInsertInitial` 单条多行 INSERT 被整条中止 + fail-open 吞错 → 新用户配额行全丢。
- `api_contract_test.go` `default_platform_quotas` fixture 由 `AllowedQuotaPlatforms` 动态生成 → 补 other 键（§4.7）。
- 存量 `default_platform_quotas` 缺 other = 不限（null）。
- 402/403/429 走 openai 通用分支；other 无余额端点不做自动恢复。
- 混厂同分组无 alias 唯一性约束 → 操作指引按厂商分组（§3.6）。
- 凭证外发风险：base_url 空 → OpenAI fallback 全面封锁（§3.3）。

## 7. 验证矩阵
- 后端：`go build ./...`、`go vet ./internal/...`、`go test ./... && golangci-lint run ./...`（backend/Makefile）。
- 定向测试覆盖：窄谓词（grok/composite 不含）；调度快照桶对 other（外审 1 回归）；**空 base_url 在各 URL 构建入口失败关闭、不回落 api.openai.com**（外审 2 回归，覆盖 CC/模型/count_tokens/embeddings）；**other 空 mapping → 公开模型列表空 + 调度拒绝**（外审 3）；Normalize/dispatch/count_tokens/billing；迁移与契约 fixture（含 api_contract_test 补键）；配额 handler。
- Anthropic 客户端：**字段级正/负例端到端契约测试**（§3.7）——基础文本/多轮、tool_use/tool_result 续接、流式、cache、count_tokens 逐字段断言；thinking / server-side tools 的丢弃与拒绝行为显式断言；不允许以 200 + 静默丢语义通过（外审 4）。
- 前端：typecheck、lint:check、定向 vitest（platforms.spec.ts、SettingsView.spec.ts）。
- 端到端（需真实第三方 OpenAI 兼容端点 Key；无 key 时单测+冒烟为主并标注待真实验收）。

## 8. done_when
- 阶段一：本方案修订 commit 经 Codex 只读外审，**must_fix（外审 1-4 与完整性项）全部收敛**。
- 阶段二：实现 diff（方案+实现+测试）经 Codex 复核，must_fix 关闭。
- §7 可执行项全过；真实端点链路标注「待真实验收」；Anthropic 兼容按实测子集报告，不以 200 冒充语义兼容。
- §5 红线无泄漏；残留扫描：other 在 §2 各权威点（含调度快照）齐备、无 CN 专属误触发、无 composite/channel_monitor 泄漏、无厂商预设残留、空 base_url 无 OpenAI fallback 残留。
