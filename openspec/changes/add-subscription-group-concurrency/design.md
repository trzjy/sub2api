# 订阅分组并发限制 + 并发超限文案全局可配（设计文档）

> 状态：已实施（功能闭环完成；代码已落地并通过 build/vet/全量测试，3 处测试覆盖落差中 ① WS Responses 端到端、② 前端组件 spec 已补，仅 ③ 迁移真实 PG 实测因环境无数据库仍留作语法保证）　最后更新：2026-09-11
> 验收方式：GLM 对照本文档核对代码落地情况。
> 本版按第二轮复审定案：Live（语音）整体出范围；文案按维度区分。详见第二、五、六节。

## 一、要解决什么

现在系统限制并发只有一条路：用户全局并发（`users.concurrency`）。请求进来不分情形先去占这个用户名额。缺口有二：

1. **订阅分组不能单独限并发。** 每个订阅套餐对应一个独立分组，但分组表没有并发字段，做不到"用这个分组的人，每人各自最多 N 个并发"。
2. **并发超限返回英文死文案**，改不了中文，也显示不出具体上限值。

## 二、设计

### 1. 并发：两条路二选一，互不干扰

- **订阅请求** → 只看分组并发。分组设多少，用这个分组的人每人各自最多多少。
- **计量请求** → 只看用户全局并发，一字不改。
- **绝不取小、绝不叠加、绝不合并。** 订阅请求不再受用户全局并发约束。
- **分组并发填 0 = 不限**，直接放行，**不回流到用户并发**。
- 该并发限**每个人各自**，不是分组总并发。分组设 5，100 人各限 5，合计 500 正常。
- 同一用户买两张不同分组的卡，两卡并发各算各的。

判定标准与系统计费预检口径**逐字一致**：

```
是订阅模式 = 分组是订阅类型 且 该用户订阅有效
（group.IsSubscriptionType() && subscription != nil）
```

**范围：Live（语音）整体不做。** 详见第六节边界。

### 2. 文案：新增一个全局设置，按维度区分

- 新增设置项 `concurrency_limit_message`，管理端系统设置页可填。
- 支持两个变量：
  - `{limit}` → 当前适用的并发上限值。
  - `{scope}` → 维度词：**订阅用户撞上限渲染为"订阅"，计量用户撞上限渲染为"用户"。**
- **适用维度（关键决策）**：
  - **订阅用户**撞自己分组的并发上限 → 渲染文案，`{scope}` = 订阅。
  - **计量用户**撞自己的用户并发上限 → 渲染文案，`{scope}` = 用户。
  - **上游账号忙不过来（slotType = account）不是"用户撞上我们的并发上限"** → **保持原样英文，绝不使用本文案。**
- 示例：模板填 `当前{scope}并发上限 {limit}，已达上限值。`
  - 订阅分组上限 5 → `当前订阅并发上限 5，已达上限值。`
  - 计量用户上限 5 → `当前用户并发上限 5，已达上限值。`
- **留空回退现有英文，行为与改动前逐字节一致。**
- 覆盖出口见 D 部分清单（Gemini、WS Responses 属于用户/分组维度，纳入；Account/Live 出口不纳入）。

## 三、现状关键点（已核对到文件与行号）

| 事项 | 位置 |
|---|---|
| 并发值来源 | `apiKey.User.Concurrency`（`internal/server/middleware/api_key_auth.go:178-181`、`:276-279`） |
| 订阅模式判定现成 | `service/group.go:154`；预检口径 `billing_cache_service.go:751` |
| 分组可从请求上下文取 | `setGroupContext`（`api_key_auth.go:387-390`）；读取先例 `gateway_request_pricing.go:25`（注意该处注释：调度会覆盖 ctxkey.Group） |
| 分组加载全链路样板 | `rpm_limit`（migration `125_add_group_rpm_limit.sql`） |
| 认证快照版本号 | `api_key_auth_cache_impl.go:17`，当前 24，**必须升 25** |
| 现有并发槽位 key | `concurrency:user:{userID}`（`concurrency_cache.go:30`、:385） |
| 用户槽脚本计数 | `acquireScript`：`ZCARD(user) + ZCARD(live:user)`（`concurrency_cache.go:71-105`）——**含 live key** |
| 槽位清理机制 | `concurrency_cache.go:436`、`:1095`、`:1170`（按用户维度组织） |
| 活跃索引成员解析 | `readIndexLoads` 用 `strconv.ParseInt(member, 10, 64)`（`concurrency_cache.go:546-551`），**硬假设成员是整数** |
| `slotIndexSpec` | `concurrency_cache.go:428-432`，`slotKey func(int64) string` |
| 并发错误出口（英文默认） | `concurrency_error_response.go:17` |
| `ConcurrencyError` 现状 | 只有 `SlotType`、`IsTimeout`（`gateway_helper.go:146`），**无上限值** |
| slotType 取值 | `"user"` / `"account"`（`gateway_handler.go:246/:435` 等） |
| 全局设置样板 | `site_name`：`domain_constants.go:367` + 三处登记 + `GetSiteName`（`setting_features.go:291`） |
| 分组创建 RPMLimit 写入 | `admin_group.go:614`（创建）、`:1010-1011`（更新） |
| Live 入口（**出范围**） | `gateway.POST /live`、`/realtime/calls` → `h.OpenAIGateway.Live`（`routes/gateway.go:212`、`:379`） |
| WS Responses（**在范围**，文本流式） | `h.OpenAIGateway.ResponsesWebSocket`（`routes/gateway.go:231`、`:372`、`:385`） |

## 四、并发槽位获取的全部调用点（穷举，施工与验收清单）

按当前代码全量 grep，用户槽获取共 **15 处 handler 调用点，其中 14 处在范围（组 1~3）、1 处 Live 出范围不计**。

### 组 1：直接调 `AcquireUserSlotWithWait`（4 处）

- [ ] `gateway_handler.go:243`（Anthropic messages）
- [ ] `gateway_handler_chat_completions.go:125`（Anthropic chat）
- [ ] `gateway_handler_responses.go:134`（Anthropic responses）
- [ ] `gemini_v1beta_handler.go:312`（Gemini）

### 组 2：经共享包装 `acquireResponsesUserSlot`（1 处定义 + 7 处调用）

包装定义：`openai_gateway_handler.go:2010-2026`（内部 :2019 调 `AcquireUserSlotWithWait`）。
**判定移进包装内部**（包装有 gin ctx，可自取 group/subscription），7 个调用方只做"去掉传入的 concurrency 参数"的最小改动，不再各自判定：

- [ ] `openai_gateway_handler.go:588`
- [ ] `openai_gateway_handler.go:1226`
- [ ] `openai_chat_completions.go:131`
- [ ] `openai_embeddings.go:93`
- [ ] `openai_images.go:128`
- [ ] `openai_alpha_search.go:95`
- [ ] `grok_media.go:145`

### 组 3：WS Responses 的 try-acquire（3 处）—— **易漏**

- [ ] `openai_gateway_handler.go:2463`
- [ ] `openai_gateway_handler.go:2478`
- [ ] `openai_gateway_handler.go:2849`
（函数 `ResponsesWebSocket`，文本流式，**在范围内**。）

这 3 处调 `TryAcquireUserSlotForAPIKey(ctx, subject.UserID, subject.Concurrency, apiKey.ID)`。**它本身就是用户槽获取**（`gateway_helper.go:237-244`：内部先 `TryAcquireUserSlot`，再包 APIKey 统计），不是"APIKey 统计逻辑"。必须一并分流，否则 WS 链路订阅用户仍被 `users.concurrency` 卡住。
WS 用**不等待**的 try-acquire，因此 B 部分必须提供不等待变体 `TryAcquireUserGroupSlot`（不能只给带等待的版本）。

**顺序陷阱（必改，否则本组分流失效）**：`ResponsesWebSocket` 里 `subscription` 读取在 `:2492`，而槽位获取在 `:2463`、重取闭包在 `:2478`——**都在前面**。C.1 判定函数以 `subscription` 为入参，照现有顺序实施会在 `:2463` 拿到 nil → 回落 user 维度 → 订阅 WS 用户照样被 `users.concurrency` 卡住。实施时必须把 `:2492` 的 `GetSubscriptionFromContext` **前移到 `:2463` 之前**（纯 ctx 读，零成本）。HTTP 各入口的 subscription 一律在取槽前读取，只有 WS 这一处顺序相反。

## 五、改动清单

### A. 分组并发字段全链路贯通

- [ ] A.1 迁移 `backend/migrations/242_add_group_concurrency.sql`：`ALTER TABLE groups ADD COLUMN IF NOT EXISTS concurrency integer NOT NULL DEFAULT 0`（幂等）+ COMMENT。
- [ ] A.2 `ent/schema/group.go` 加 `field.Int("concurrency").Default(0)`，放 `rpm_limit` 旁。
- [ ] A.3 生成 ent 代码，确认 `Group` 结构体含 `Concurrency`。
- [ ] A.4 `service/group.go` 领域模型加 `Concurrency int`。
- [ ] A.5 `repository/group_repo.go` 创建/更新 setter 写入；`groupEntityToService` 投影补齐。
- [ ] A.6 `repository/api_key_repo.go` 分组投影补齐。
- [ ] A.7 `service/api_key_auth_cache.go` 的 `APIKeyAuthGroupSnapshot` 加 `Concurrency int`。
- [ ] A.8 `api_key_auth_cache_impl.go` 的 `snapshotFromAPIKey`、`snapshotToAPIKey` 两处搬运。
- [ ] A.9 **快照版本号 24 → 25**（`api_key_auth_cache_impl.go:17`）。注意：会让全部认证快照一次性失效，部署瞬间有一波认证缓存冷启动（既有惯例，见第七节）。
- [ ] A.10 `CreateGroupInput`（:235）加 `Concurrency int`；`UpdateGroupInput`（:315）加 `Concurrency *int`。
- [ ] A.11 `admin_group.go` 创建（:614 一带，照 `RPMLimit`）、更新（:1010-1011）写入。
- [ ] A.12 `handler/dto/types.go` 分组请求/响应补 `concurrency`，handler 映射补齐。
- [ ] A.13 单测：创建/更新带并发；快照 round-trip（升版本后旧快照失效）。

### B. 新增「用户+分组」并发槽位线

- [ ] B.1 `concurrency_cache.go`：
  - 前缀 `concurrency:grpuser:`，key 函数 `grpUserSlotKey(groupID, userID)`。
  - `AcquireUserGroupSlot` / `ReleaseUserGroupSlot` / `GetUserGroupConcurrency` 三方法。
  - 按「用户+分组」的等待计数 `IncrementUserGroupWaitCount` / `DecrementUserGroupWaitCount`。
  - **脚本设计明确**：走**单键计数**脚本——只 `ZCARD(grpuser)`（对照用户脚本的双键结构：`ZCARD(user) + ZCARD(live:user)`）。**不引入也不引用任何 live key**。理由：分组维度是全新独立的一条线；Live 整体出范围，不存在 `live:grpuser` key，也不做与 live 的接替。禁止照抄 `acquireScript` 里 `liveUserSlotKey` 那一行，避免把 live 语义混进分组维度。
- [ ] B.2 **改造 `slotIndexSpec` 与活跃索引成员解析（关键，不是简单"再挂一个"）**：
  - 现状 `readIndexLoads` 用 `strconv.ParseInt(member,10,64)`（:546-551）解析成员，非整数判为 stale 移除；`slotKey` 是 `func(int64) string`。**复合键 `{groupID}:{userID}` 会被当成损坏成员清出索引**，导致清理防线静默失效。
  - 必须把 `slotIndexSpec` 的成员解析与 key 构造**泛化为按 spec 处理成员串**（例如 spec 增加 member↔id 的编解码，或直接用字符串键），再为 grpuser 定义 spec。
  - 完成后核对：grpuser 槽位能被 reconcile 正常计数与回收，不被误清。
- [ ] B.3 `concurrency_service.go`：接口加方法；新增 `AcquireUserGroupSlot`，`maxConcurrency <= 0` 直接 no-op 放行（对齐 `AcquireUserSlot` :381-388）。
- [ ] B.4 `gateway_helper.go`：
  - `AcquireUserGroupSlotWithWait(...)`（复用等待+心跳，slotType = `"group"`）。
  - **`TryAcquireUserGroupSlot(...)` 不等待变体**（组 3 需要）。
  - 两个变体都**必须保留 APIKey 槽位统计包装**：提供 `TryAcquireUserGroupSlotForAPIKey`（对齐 `TryAcquireUserSlotForAPIKey` 的写法），否则订阅请求的 per-APIKey 统计会静默丢失。
- [ ] B.5 单测：同用户两分组独立计数；释放与 TTL 回收；并发 0 时 no-op 放行；复合键槽位不受伤；分组脚本不含 live key。

### C. 请求入口分流

- [ ] C.1 `gateway_helper.go` 加统一判定函数。**返回结构体而非裸值**，需带 groupID（构造 grpuser key 要用）：

```go
type concurrencyScope struct {
    Kind      string // "user"（计量）| "group"（订阅）
    Max       int    // 对应上限；0 = 不限
    GroupID   int64  // 订阅时有效
    ScopeWord string // 文案 {scope} 维度词："用户" | "订阅"
}
func resolveConcurrencyScope(c *gin.Context, subject middleware.AuthSubject, subscription *service.UserSubscription) concurrencyScope
```

  - 判定口径与 `CheckBillingEligibility` 一致（`group.IsSubscriptionType() && subscription != nil`）。
  - **分组来源写死为"认证时刻分组"**：从认证中间件写入的 `ctxkey.Group` 取，在调度覆盖之前取用（取槽位本就发生在调度前）。契约写进代码注释，避免从被 fallback/composite 覆盖的位置取。
  - **分组缺失或无效 → 回落计量（user）维度**（沿用 `IsGroupContextValid` 防御）。
- [ ] C.2 改造第四节全部调用点（组 1/2/3）。订阅模式错误 slotType 传 `"group"`，计量传 `"user"`。组 2 判定移进包装内部。
- [ ] C.2b **WS 顺序修正**：把 `openai_gateway_handler.go:2492` 的 `GetSubscriptionFromContext` 前移到 `:2463` 取槽之前（详见第四节顺序陷阱说明）。
- [ ] C.3 账号级并发获取与 APIKey 槽位跟踪保持不变，仍在用户/分组槽位之后执行（**注意**：组 3 的 `TryAcquireUserSlotForAPIKey` 属于用户槽获取而非纯统计，见第四节）。
- [ ] C.4 单测 `resolveConcurrencyScope`：订阅→group（Kind/Max/GroupID/ScopeWord 均正确）；计量→user；未加载订阅→user；非订阅分组→user；**分组缺失/无效→user**。
- [x] C.5 集成测试覆盖：Anthropic messages、OpenAI chat、responses、**Gemini**、**WS Responses**。至少钉住"订阅用户不被用户全局并发拦截"。【WS Responses 端到端分流已补：`TestOpenAIResponsesWebSocket_SubscriptionUserBypassesGlobalUserConcurrency`（订阅用户绕过用户全局并发放行）+ `TestOpenAIResponsesWebSocket_MeteringUserBlockedWhenGlobalConcurrencyExhausted`（计量用户走 user 线被拦）；详见第八节①】

### D. 并发超限文案（按维度，覆盖全部相关出口）

- [ ] D.1 `domain_constants.go` 加 `SettingKeyConcurrencyLimitMessage = "concurrency_limit_message"`。
- [ ] D.2 `setting_parse.go` 默认值表加该 key（默认 `""`），视图赋值处取值。
- [ ] D.3 `settings_view.go` 的 `SystemSettings` 加 `ConcurrencyLimitMessage string`。
- [ ] D.4 `setting_update.go` 写入白名单加该字段。
- [ ] D.5 `setting_features.go` 加 `GetConcurrencyLimitMessage(ctx) string`（照 `GetSiteName`，空/错误返回空串）。
- [ ] D.6 `handler/dto/settings.go` 加 `concurrency_limit_message`，admin 设置请求/响应 mapper 贯通。
- [x] D.6.1 边界（防越界，验收重点）：`concurrency_limit_message` 仅限 **admin** `SystemSettings`（及 admin API 的 `UpdateSettingsRequest`/`SystemSettings`），**不得**进入任何面向访客的公开设置——`service` 公开白名单（`setting_public.go`）、`dto.PublicSettings`、`service.PublicSettingsInjectionPayload`、前端 `PublicSettings` 类型均不得包含此字段。任何新增公开字段都须通过 `public_settings_injection_schema` 防泄漏守护测试（dto 与注入结构字段须一致）。
- [ ] D.7 `ConcurrencyError` 加 `Limit int`，构造超时错误（`gateway_helper.go:412` 唯一构造点）时填上限；`WaitQueueFullError` 不填。
- [ ] D.8 文案渲染为公共函数（现状 `concurrencyErrorResponse` 只覆盖 HTTP 系）：

```go
// kind: "user" | "group"（账号维度不调用本函数）
func renderConcurrencyLimitMessage(ctx context.Context, kind string, limit int) string
```

  调用方（handler 层）已知道 slotType；**仅在 slotType 为 `"user"` 或 `"group"` 时调用**；`"account"` 分支**整个不调用**，走原英文/原路径。
  - `{scope}`：kind = "group" → `订阅`；kind = "user" → `用户`。
  - `{limit}`：limit > 0 时替换为数字。
- [ ] D.9 单测：空配置回退英文（逐字节一致）；模板含 `{scope}`+`{limit}` 时两维度渲染正确；**account 维度不渲染本文案**；无上限（排队满）+ 含 `{limit}` 模板时回退英文，不出裸 `{limit}`。

**并发超限出口清单：**

| 出口 | 位置 | 维度 | 处置 |
|---|---|---|---|
| HTTP 系（Anthropic/OpenAI） | `concurrency_error_response.go:17` → 两个 `handleConcurrencyError` | user / group / account | user、group 调 D.8；account 不调 |
| Gemini | `gemini_v1beta_handler.go:313-316` `googleError(c, 429, err.Error())` | user / group | 改为调 D.8（**现状不经统一出口**） |
| WS Responses | `openai_gateway_handler.go:2470`、`:2485`、`:2854` 硬编码英文 | user / group | 改为调 D.8（**现状硬编码**） |
| ~~Live~~ | `openai_live.go:110`、`:183` | —— | **出范围，不改** |
| 账号忙 / 上游限流 | `handleConcurrencyError(..., "account", ...)` | account | **保持原样，不用本文案** |

**文案渲染规则（契约，写进代码注释）：**

1. `kind` 为 `"account"` 或非 user/group → 不调用本函数，保持原英文。
2. 设置值为空（trim 后）→ 用现有英文默认，行为与改动前逐字节一致。
3. 模板含 `{scope}` → 替换为 `订阅`（group）或 `用户`（user）。
4. 模板含 `{limit}` 且能拿到上限值 → 替换为数字；拿不到 → 整段回退英文，**绝不出现裸 `{limit}`**。
5. 只替换 `{scope}` 与 `{limit}`，其他花括号原样保留。

**已知限制**：`{scope}` 渲染出的"订阅"/"用户"是后端写死的中文。若管理员填英文模板（如 `Current {scope} concurrency limit {limit}`），`{scope}` 处仍出中文，形成中英混排。本设计为单模板、无多语言机制（`site_name` 同理），对本产品可接受。英文站点管理员应直接用中文维度词或回避 `{scope}`（如 `Concurrency limit {limit} reached`）。

### E. 前端

- [ ] E.1 `views/admin/GroupsView.vue` 分组表单加并发数字输入（0 = 不限），紧邻现有 RPM 项；提示语明示「仅订阅分组；限每个使用者各自并发，非分组总并发」。
- [ ] E.2 前端 API/类型补 `concurrency`。
- [ ] E.3 `views/admin/SettingsView.vue` 加文案输入框，placeholder 提示支持 `{scope}` 与 `{limit}`，并给示例 `当前{scope}并发上限 {limit}，已达上限值。`
- [ ] E.4 i18n：`i18n/locales/{zh,en}/admin/overview.ts` 的 `admin.groups` 块（紧邻 `rpmLimit`，zh 约 :840）加 3 条；`{zh,en}/admin/settings.ts` 加文案设置 3 条。
- [x] E.5 前端单测 + i18n 编译测试通过。【新增组件级 spec：`groupsViewConcurrencyField.spec.ts`（create/edit 表单 concurrency 输入接线 + i18n key 双语存在）、`settingsViewConcurrencyMessageField.spec.ts`（concurrency_limit_message 文本域接线 + i18n key 双语存在）；详见第八节②】

### F. 验证

- [ ] F.1 `cd backend && go build ./... && go vet ./...` 通过。
- [ ] F.2 相关包 `go test` 全绿。
- [ ] F.3 迁移重复执行不报错。【落差见第八节③：本环境无真实 PG 实测，仅由 `IF NOT EXISTS` 语法保证幂等】
- [x] F.4 关键契约各有测试证据：①订阅分组并发 0 不受用户并发限制（含 WS Responses 链路）②同用户两分组独立计数 ③槽位可回收不泄漏（复合键不被误清）④文案空配置零变化 ⑤订阅/计量两维度文案变量正确 ⑥account 维度不受本文案影响。【①"含 WS Responses 链路"已由 WS handler 端到端用例钉住，详见第八节①】
- [ ] F.5 前端 `npx vitest run` + 类型检查通过。

## 六、边界（防越界，验收重点）

- **Live（语音）整体出范围，完全不动。** 入口 `gateway.POST /live`、`codexDirect.POST /realtime/calls`（`routes/gateway.go:212`、`:379`）以及 `openai_live.go`、`service/openai_live.go`、`acquireLiveLeaseScript`、`live:*` 系列 key 一律不碰。订阅分组并发**不覆盖** Live；Live 保持原用户并发语义。这是明确决策，不是遗漏。
- 不动 `users.concurrency` 的**计量模式**语义、账号级并发、图片进程级并发、RPM 语义。
- 不给 `subscription_plans` / `user_subscriptions` 加字段。
- 订阅与计量两条路之间**不得出现任何取小/叠加/回落的代码**。
- 文案只作用于 user/group 两个维度；**account（上游账号忙/上游限流）保持原样，绝不套用户文案**。
- **可观测性后果（已知并接受）**：订阅请求不再计入 `concurrency:user:` 槽位，admin 现有用户负载视图将不含订阅流量；当前无分组维度负载视图，运营侧看不到分组并发水位。本期不做新视图，明示，后续可另立需求。

## 七、风险与上线

- **头号风险：新槽位线的清理（B.2）。** 复合键会踩 `readIndexLoads` 的 int64 硬假设，必须泛化 `slotIndexSpec` 成员处理，否则"挂了清理却静默失效"。
- **次风险：易漏链路。** WS Responses（组 3）是核心契约最易失效处，第四节已穷举，逐条核对。
- **文案维度边界。** account 出口必须显式排除，否则账号繁忙会渲染出错误语义（第二、五节已定死）。
- **快照版本升级冷启动。** 24→25 会让全部认证快照一次性失效，部署瞬间有一波认证缓存重建（既有惯例，可接受，此处明示）。
- 迁移幂等；上线后分组并发默认 0、文案默认空，**全部现有请求行为不变**。
- 先部署后端，再部署前端。无需数据回填。

## 八、闭环结论与已知落差（测试覆盖）

设计清单 A.1–F.5 已逐项对照 diff 与测试证据，功能代码全部落地。原 3 处测试覆盖落差中，①、② 已于本轮补齐，③ 因环境无数据库仍保留语法保证。

① **WS Responses 端到端分流（C.5 / F.4①）——已补。**
新增 WS handler 端到端用例（扩展 `runOpenAIResponsesWebSocketUsageLogCase` harness 支持注入订阅上下文与自定义并发缓存）：
- `TestOpenAIResponsesWebSocket_SubscriptionUserBypassesGlobalUserConcurrency`：订阅分组（SubscriptionType=subscription、Concurrency=3、Hydrated）用户，在**用户全局并发槽耗尽**、**分组并发槽可用**时，仍正常建立连接并收到 `response.completed` → 证明 WS 路径命中 group 维度、绕过用户全局并发。
- `TestOpenAIResponsesWebSocket_MeteringUserBlockedWhenGlobalConcurrencyExhausted`：计量（无订阅）用户，用户全局并发槽耗尽时被 `StatusTryAgainLater` 关闭 → 证明计量走 user 维度。
核心契约"订阅用户不被用户全局并发拦截"现已有 handler 级端到端证据（此前仅有 helper 级 mock `TestAcquireScopedUserSlotWithWait_GroupScopeBypassesUserConcurrency`）。

② **前端新字段组件级单测（E.5）——已补。**
新增两个组件级 spec（沿用本仓库 GroupsView 既有的"读源码断言"风格，并额外校验 i18n key 在 zh/en 双语存在）：
- `groupsViewConcurrencyField.spec.ts`：create/edit 表单 `concurrency` 输入（`v-model.number`、`type="number" min="0"`）接线、label/placeholder/hint 的 i18n key、`concurrency: 0` 默认值、编辑回填 `editForm.concurrency = group.concurrency ?? 0`。
- `settingsViewConcurrencyMessageField.spec.ts`：`concurrency_limit_message` 文本域接线、label/placeholder/hint 的 i18n key、默认值与保存载荷。
i18n 编译/完整性测试、类型贯通（`Group`/`CreateGroupRequest`/`UpdateGroupRequest`/`SystemSettings`/`PublicSettings` 已含 `concurrency` 与 `concurrency_limit_message`）此前已通过。

③ **迁移幂等未对真实 PG 实测（F.3）——仍保留。**
由 `ALTER TABLE groups ADD COLUMN IF NOT EXISTS ...` 语法保证幂等，本环境无数据库实测。

### 全局一致性审查结论
- 订阅/计量两线之间无任何取小/叠加/回落代码（`resolveConcurrencyScope` 二选一，`gateway_helper.go` 无 min 逻辑）。
- 旧链归零：组 2 调用方已全部去掉 concurrency 入参，无并行旧路径残留。
- account 维度、users.concurrency 计量语义、账号级并发均未越界（account 出口不调 `renderConcurrencyLimitMessage`）。
- 快照 v24→v25 升级与旧快照拒绝测试（`TestAPIKeyService_RejectsV24SnapshotBeforeGroupConcurrency` / `TestAPIKeyService_RoundTripsV25GroupConcurrency`）配套。

### 收敛轮次
- 第一轮静态核对：A.8、C.4 取证不足，补查后确认落地。
- 第二轮：确认无新增问题，收敛于零新增。

### 验证证据（绿灯）
- `go build ./...` / `go vet ./...` 通过（仅余 2 条既有 `binding:` 警告，与本次无关）。
- backend `internal/handler`、`internal/service`、`internal/repository`（非集成）全量测试通过；Docker 集成测试 `TestConcurrencyCacheSuite`（含 B.5 全部 7 个 grpuser 用例）通过。
- D.9 / C.4 / C.5 / A.13 单元测试通过；新增 WS handler 端到端用例 `TestOpenAIResponsesWebSocket_SubscriptionUserBypassesGlobalUserConcurrency` 与 `TestOpenAIResponsesWebSocket_MeteringUserBlockedWhenGlobalConcurrencyExhausted`（C.5 / F.4①）。
- 前端 `npx vitest run`：270 文件 / 2060 用例通过；`vue-tsc` 类型检查通过；新增组件级 spec `groupsViewConcurrencyField.spec.ts`、`settingsViewConcurrencyMessageField.spec.ts`（E.5）。
- 修复一处 WS 回归：C.2b WS 路径对 `h.settingService` 补 nil 守卫（与既有 SSE/HTTP 路径一致），恢复 ~22 个 `TestOpenAIResponsesWebSocket*`。
- 修复一处越界：实施时把 `concurrency_limit_message` 误加进访客侧公开设置三处（`setting_public.go` 公开白名单、`dto.PublicSettings`、前端 `PublicSettings` 类型），被 `public_settings_injection_schema` 防泄漏守护测试拦下。已撤回上述三处，仅保留 admin `SystemSettings`；D.6.1 已补"不得进公开设置"的边界规则。撤回后 dto 包测试恢复绿灯，零功能影响（公开侧本无消费者）。
