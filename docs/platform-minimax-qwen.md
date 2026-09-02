# 平台接入方案：MiniMax / Qwen（上游一等平台）

> 日期：2026-09-03 ｜ 分支：`feat/platform-minimax-qwen` ｜ 状态：**待外部审查（阶段一）**

## 1. 背景与目标

Sub2API 是 AI 网关：把管理员配置的上游账号聚合成 OpenAI/Anthropic 兼容端点分发给用户。当前平台白名单是编译期硬编码的 8 个：
`anthropic / openai / gemini / antigravity / grok / kimi / zhipu / deepseek`。

运营者希望把 **MiniMax** 与 **Qwen（阿里云百炼 / DashScope）** 也作为可添加、可分发的上游平台。两者的共同形态：**OpenAI 兼容 + 纯 API-Key + 按量付费**，无订阅账号、无 Coding Plan/滚动用量窗口、无官方余额查询端点。

产品决策（用户确认）：
1. minimax 与 qwen **一起接入**；
2. **支持「用户 × 平台 USD 配额」** 管控；
3. **兼容 Anthropic 系客户端**（Claude Code 打 `/v1/messages`）。

默认 OpenAI 兼容 base_url（仅作默认，账号可填自定义 base_url 覆盖；以官方文档为准）：
- Qwen：`https://dashscope.aliyuncs.com/compatible-mode/v1`
- MiniMax：大陆 `https://api.minimax.chat/v1` ／ 海外 `https://api.minimax.io/v1`

平台标识与展示：`minimax` / MiniMax；`qwen` / Qwen。

## 2. 权威源与同步义务

平台「可枚举」边界目前分散在以下各点，新增平台必须**保持同步**：

| 权威面 | 位置 | 备注 |
|---|---|---|
| 平台常量 | `backend/internal/domain/constants.go`、`backend/internal/service/domain_constants.go` | |
| 配额白名单 | `service/domain_constants.go` `AllowedQuotaPlatforms` | **单一权威源**（注释明示） |
| 配额 ent 构建期校验 | `backend/ent/schema/user_platform_quota.go` `Validate` | 需与上同步 |
| 配额 DB CHECK | `backend/migrations/*`（157/224/227 模式） | 需与上**同批**改，见 §6 事故史 |
| 前端类型 | `frontend/src/types/index.ts`（AccountPlatform/GroupPlatform） | |
| 前端目录 | `frontend/src/constants/platforms.ts` | |
| 前端配色/徽章/i18n | `platformColors.ts` / `PlatformTypeBadge.vue` / i18n en|zh | |

`accounts.platform` 与 `groups.platform` 是普通字符串列（无 ent 校验、无 DB CHECK），**不触发迁移**。

## 3. 架构裁决

### 3.1 走「OpenAI 兼容协议族」，不并入 CN 专属语义

新平台为按量 API-Key、无官方余额端点、无 Coding Plan/5h 窗口。若并入 `IsCNProvider()` 全局语义，CN 专属的余额探测、Coding-Plan 额度探测、滚动窗口限流会被误触发到无对应端点的上游（20+ 引用点、误触面过大）。

因此：**不**改 `IsCNProvider`、`AllowedSchedulingThresholdPlatforms`、CN 前端面板（kimi/zhipu/deepseek 专属），CN 专属增强一律保持 CN-only。

### 3.2 新增窄谓词：共享 OpenAI base/key 的平台族

`IsOpenAI() || IsCNProvider()` 判定（用于共享 base_url/api_key 的平台集合）缺新平台，需收敛为单一窄谓词：

- 自由函数 `UsesOpenAIProtocolSharedBaseURL(platform)`（放 `service/domain_constants.go`，语义：走共享 OpenAI 兼容 base_url/api_key 的平台）= `platform == openai || IsCNProvider(platform) || platform == minimax || platform == qwen`。
- 语义要点：**含 openai OAuth**（现门 `IsOpenAI()||IsCNProvider()` 本身含），**排除 grok**（grok 走独立 `GetGrokBaseURL`/`GetGrokAccessToken`）与 **composite**。替换下列点**不得**造成 openai/grok 现有行为回归。
- Account 方法同放 `account.go`。

### 3.3 Anthropic 客户端兼容：复用现成 Messages→ChatCompletions 转换

Claude Code 打 `/v1/messages` 时：路由 family 门（§4.2）把 qwen/minimax 判入 OpenAI 网关 → `OpenAIGateway.Messages` → `shouldForwardOpenAIResponsesViaRawChatCompletions` 对两平台**恒返回 true** → 现成 Messages→CC 转换把 anthropic 请求体翻译成 Chat Completions 打到上游 base_url。`GetAPIProtocol` 对非 CN 平台恒返回 `chat_completions`，天然把两平台锁死在 CC 协议——**不依赖上游 anthropic 端点，无需任何协议转换新代码**。

## 4. 改动清单

### 4.1 常量与谓词（后端）

- `backend/internal/domain/constants.go`：`PlatformMinimax = "minimax"`、`PlatformQwen = "qwen"`。
- `backend/internal/service/domain_constants.go`：常量别名；`DefaultMinimaxBaseURL` / `DefaultQwenBaseURL`；`UsesOpenAIProtocolSharedBaseURL` 自由函数。
- `backend/internal/service/account.go`：`IsOpenAICompatible()`（L296）纳入两平台；新增谓词方法。

### 4.2 调度 / 路由 / 转发（漏一处 = 不可调度或走错网关）

- `openai_gateway_scheduling.go` `NormalizeOpenAICompatiblePlatform`（L293）：保留两平台原值（否则永不匹配）。
- `server/routes/gateway.go` `isOpenAIResponsesCompatibleGatewayPlatform`（L48）与 `countTokensHandler`（L58）：两平台入 OpenAI 网关族，count_tokens 走 `OpenAIGateway.CountTokens` 分支。
- `openai_gateway_forward.go` `shouldForwardOpenAIResponsesViaRawChatCompletions`（L1239）：两平台恒 `true`（否则 Extra 无 probe 标记时默认推入不存在的 `/v1/responses`）。

### 4.3 base / key 门控（否则静默回落 api.openai.com 或读不出 key）

- `account.go` `GetOpenAIBaseURL`（L1335 门 → 窄谓词；L1352 平台默认 switch 加两平台默认 base，保留兜底但两平台不再落 api.openai.com）。
- `account.go` `GetOpenAIProtocolAPIKey`（L1675）：放行两平台 apikey 读取（openai 仍走 `GetOpenAIApiKey`）。

### 4.4 模型 / 用量 / dispatch / count_tokens（正常可用的公共能力）

- `upstream_models.go`（L516/L636）`IsOpenAI()||IsCNProvider()` case → 窄谓词（`/v1/models` 同步）。
- `openai_gateway_count_tokens.go`：`shouldEstimateOpenAIInputTokensLocally`（L152）与 `ForwardCountTokensAsAnthropic` CN 分支（L274）加两平台（本地估算，避免打到不存在的 input_tokens 端点）。
- `openai_gateway_handler.go` `allowOpenAICompatibleMessagesDispatch`（L217）：豁免分支加两平台（否则 sanitize 置 false → 403）。
- `openai_messages_dispatch.go` `ResolveMessagesDispatchModel`（L85）：两平台返回 ""（group 级默认 gpt-5.x 映射不发给新上游，改写交给账号级 model_mapping）。
- `openai_gateway_usage.go` `filterCNProviderBillingModelCandidates`（L865）：门加两平台（避免未显式定价的 claude-* 走 Claude 价卡误计）。
- `handler/endpoint.go` `GetUpstreamEndpoint`（L318）：用窄谓词（ops/日志层端点到报）。

### 4.5 账号生命周期 / 分组

- `account_service.go` `TestCredentials`（L497）：两平台并入可用分支（否则管理端「测试」误报 unsupported）。
- `account_test_service.go`（L299）：两平台路由到通用 chat-completions 探活。
- `admin_account.go` `buildAccountForCreate` / `account_service.go` Create：**平台→type 校验**，两平台仅接受 `apikey`（小 helper 复用，后端权威化，不依赖前端）。
- `account_handler.go` `scheduleOpenAIResponsesProbe` + `openai_apikey_responses_probe.go` `ProbeOpenAIAPIKeyResponsesSupport`：两平台落标 `auto` + `supported=false`、**不发网络探测**。
- `group_handler.go` platform `oneof`（L101/L169）加 `minimax qwen`；`target_platform` oneof（L237）**不动**。

### 4.6 配额三件套（必须同批，见 §6）

- `service/domain_constants.go` `AllowedQuotaPlatforms` 追加 minimax/qwen。
- `ent/schema/user_platform_quota.go` `Validate` 追加 minimax/qwen。
- 新迁移 `backend/migrations/236_user_platform_quotas_add_minimax_qwen.sql`（DROP + ADD CHECK 全平台列表，仿 157/224 可重入模式）+ 同前缀迁移测试。

### 4.7 前端

- `types/index.ts`：`AccountPlatform`/`GroupPlatform` 加 `'qwen'|'minimax'`。
- `constants/platforms.ts`：CONCRETE 加两项；**新增 composite 目标专用列表（过滤 minimax/qwen）** 并替换 `GroupsView.vue` 的 composite 目标下拉（L4766），避免 UI 出现后端会拒绝的目标。
- `utils/platformColors.ts`：平台联合 + 全色表 + 守卫 + label（TS 强类型强制补齐）。
- `components/common/PlatformTypeBadge.vue`（default 误标 Gemini，必加 case）、`PlatformIcon.vue`（品牌图标）。
- `components/account/CreateAccountModal.vue`：平台瓦片加 minimax/qwen（**锁定 `type=apikey`**，不渲染 OAuth/CN 面板）；base_url 留空用后端默认。`EditAccountModal.vue` 走通用 apikey/base_url 段。
- i18n（en/zh 必须成对）：`admin/accounts.ts` platforms、`admin/overview.ts` groups.platforms。`dashboard.ts`（渠道监控/quickstart）**不加**（渠道监控与 quickstart 未启用两平台）。
- 配额/用量 UI（`UserPlatformQuotaModal`/`Cell`）：由后端名单驱动，自动出现，仅需标签配色。

## 5. 范围边界（明示不做）

- `AllowedSchedulingThresholdPlatforms`、CN 余额/额度探测、`account_mode` coding、CN 前端面板：**不进**（语义不适合）。
- **composite 目标**：`isConcreteRequestPlatform`、`target_platform` oneof、composite 聚合与默认模型遍历、`composite_model_routes` DB CHECK 均**不动**（两平台不承担 composite 目标）。
- **channel_monitor**：ent/handler/迁移不动（渠道监控不对两平台开放）。
- **上游账单探测** `IsUpstreamBillingProbeIdentity`：不加（两平台官方域被 `upstreamBillingProbeOfficialAPIDomains` 短路为 unsupported）。
- README 无平台总表，本任务不加；`docs/` 下方案文档即运营入口。

## 6. 影响分析与事故史（为什么配额要三件套同批）

历史教训（`backend/migrations/224_user_platform_quotas_add_cn_providers.sql` 注释记载）：曾只加 `service.AllowedQuotaPlatforms` 而未同步 DB CHECK，导致注册 `BulkInsertInitial` 的单条多行 INSERT 因任一 `platform` 违约被**整条中止**，且被 fail-open 吞错 → **新用户全部平台配额行静默丢失**。

因此本任务 `AllowedQuotaPlatforms` 改动必须与 ent `Validate`、DB CHECK 迁移放**同一次改动**；`api_contract_test.go` 的 `default_platform_quotas` 整串 JSON fixture（默认平台配额由 `AllowedQuotaPlatforms` 动态生成）同步补 minimax/qwen 键。

其他影响消费方核对结论：
- 用量/账单记录用原始 platform 字符串，无白名单拒绝路径 → 新平台计量天然可用，仅展示侧需前端标签。
- `default_platform_quotas` 存量值缺两平台 key = 不限（null）；运营可后续单独写限额。
- 402/403/429：两平台走 openai 通用分支（无官方余额端点，不进 CN 自动恢复机制）；若未来要 402 自动恢复需另建机制（范围外）。
- url_allowlist（部署）：`deploy/config.example.yaml` `url_allowlist.upstream_hosts` 补 `dashscope.aliyuncs.com`、`api.minimax.chat`（默认 disabled，仅启用时影响）。

## 7. 验证矩阵

后端：`go build ./...`、`go vet ./internal/...`、`go test ./... && golangci-lint run ./...`（按 `backend/Makefile`）；定向含迁移/契约/调度/配额/handler 单测。
前端：`pnpm --dir frontend run typecheck`、`lint:check`、定向 `vitest run platforms.spec.ts SettingsView.spec.ts` 等。
端到端（需真实厂商 Key，无 key 时以单测 + 冒烟为主并在报告标注待真实验收）：建 minimax/qwen apikey 账号 + model_mapping → 建同平台分组 → 绑定 → 分别用 OpenAI 兼容客户端与 Claude Code（`/v1/messages`、`/v1/messages/count_tokens`）调用 → 核对 usage 平台归属与计费模型。

## 8. 风险与规避

- **名单漏同步**：任一路由/调度/门控漏一处即「不可调度」或「静默回落 api.openai.com」。规避：本清单穷尽（§4.2–4.5 逐点），并以单测锁定（窄谓词/转换/dispatch/count_tokens/billing/默认 base 回归）。
- **配额丢行事故**：三件套同批 + 迁移测试断言字面量（§6）。
- **前端误标**：`PlatformTypeBadge` default 误标 Gemini——必加 case，测试锁定。

## 9. done_when（本任务）

- 阶段一：本节所属方案 commit 经 Codex 只读外审，当前边界 must_fix 修订收敛。
- 阶段二：实现 diff（方案 + 实现 + 测试）经 Codex 复核，must_fix 关闭。
- 验证矩阵（§7）中可在本环境执行的项全部通过；真实厂商 Key 链路标注「待真实验收」不冒充完成。
- 范围红线（§5）无遗漏兑现；残留扫描（新平台名单在 §2 各权威点齐备、无 CN 专属误触发、无 composite/channel_monitor 泄漏）通过。
