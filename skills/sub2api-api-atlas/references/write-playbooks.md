# 写入剧本：五个高频域

只登记**下笔前必须知道**的东西：端点、请求体、覆盖语义、易踩的坑。
字段全集以源码 struct 为唯一事实源（每节给了 文件:行），本文件是摘要。

在写任何生产数据前：先只读核对目标集合，再写入，最后回读确认。
鉴权/幂等/step-up 的通用规则见 `conventions.md`。

---

## 一、账号 accounts

高频：改并发/优先级/倍率、改分组归属、改凭据、批量操作、状态开关。
共 88 条端点，完整清单见 `routes-admin.md` 的 `/admin/accounts` 段。

### 建号

`POST /api/v1/admin/accounts`（幂等 scope `admin.accounts.create`）

```jsonc
{
  "name": "string, 必填",
  "platform": "anthropic|openai|gemini|... 必填",
  "type": "oauth|setup-token|apikey|upstream|bedrock|service_account 必填",
  "credentials": { ... } // 必填，形状按平台/类型而定
  // 可选：notes, extra, proxy_id, concurrency, priority, rate_multiplier,
  //       load_factor, group_ids[], expires_at(unix 秒), auto_pause_on_expired,
  //       upstream_billing_probe_enabled, confirm_mixed_channel_risk,
  //       quota_key_id（cursor 平台必填）
}
```

struct：`backend/internal/handler/admin/account_handler.go:183`

建号前的三道守卫（服务层）：

- `other` 平台只允许 `type=apikey` 且 credentials 必须带非空 `base_url`。
- `cursor` 平台必须带 `quota_key_id`；非 cursor 平台携带归属会被拒绝。
  源码：`backend/internal/service/admin_account.go:798`。
- 普通创建入口传 `credentials.access_mode = "web"` 一律拒绝，web 登录链内部豁免。

### 改账号（最常用）

`PUT /api/v1/admin/accounts/:id`

struct：`account_handler.go:205`（`UpdateAccountRequest`，全部字段指针/可缺省）

只传要改的字段。语义差异**必须分清**：

| 字段类别 | 未携带时的行为 |
| --- | --- |
| 普通标量（concurrency、priority、rate_multiplier、name…） | 不改（指针 nil） |
| `status` | 不改；取值只能是 `active \| inactive \| error` |
| `group_ids` | 不改；传 `[]` = 清空全部分组（repo 层先删全部绑定再按传入重建） |
| `extra` | 不改；传 `{}` = 以空集为基础**替换**，但受管键（`quota_*` 系列、`grok` 计费快照、上游探测/用量快照等）会从原值补回，见下 |
| `credentials` | 不改；`{}` 与缺省同效（服务层判 `len>0` 才进入合并），**不能靠它清空凭据** |

`credentials` 的合并规则（`MergePreservingSensitiveCreds`，
`backend/internal/service/account_credentials_redact.go:50`）：

- incoming 覆盖 existing；
- **敏感键**（`api_key`、`cookie`、`access_token`、`refresh_token`、
  `base_url` 不含在内——见 conventions.md §7 清单）在 incoming 缺省时
  从 existing 补回；
- **非敏感键**（`base_url`、`model_mapping`、`balance_probe`、
  `access_mode`、`intercept_warmup_requests`…）在 incoming 缺省时 **丢失**。

所以：

- 只改 `concurrency`，不要带 `credentials` 字段，否则会触发上面这套合并逻辑。
- 要改某个非敏感凭据键，**先 `GET /:id` 拿全量 credentials，改完整体回传**；
  或者改用 `bulk-update`（走 JSONB 键级合并，见下）。
- `GET /:id` 回来的敏感键是掩码，别回填。

`quota_key_id` 三态（`account_handler.go:216` 附近注释）：
字段缺省 = 沿用；显式 `null` = 清空（仅非 cursor）；数值 = 设置。

`extra` 的替换粒度（`backend/internal/service/admin_account.go:1119` 起）：
提供时以本次传入的 map 为基础重建，然后**从原值补回受管键**——
`quota_used` / `quota_daily_*` / `quota_weekly_*`、`grok` 计费快照、
上游计费探测与用量快照、`openai_auto_reset_credit` 状态等
（清单见 `admin_account.go:1139` 的保留循环）。余额/配额与运行态不会被
一次编辑冲掉；你自己写在 `extra` 里的业务键则按传入值落库。

### 批量改（键级合并，推荐给多账号）

`POST /api/v1/admin/accounts/bulk-update`

```jsonc
{
  "account_ids": [1, 2],          // 与 filters 二选一（都不给则 400）
  "filters": { "platform": "openai", "type": "oauth", "status": "active",
               "group": "45", "search": "", "privacy_mode": "" },
  "concurrency": 10,
  "priority": 1,
  "rate_multiplier": 0.2,
  "status": "active",
  "schedulable": true,
  "group_ids": [3, 4],
  "credentials": { "model_mapping": { "gpt-5": "gpt-5-2026" } }, // 键级合并，不丢其他键
  "extra": { "base_rpm": 60 }                                    // 键级合并
}
```

struct：`account_handler.go:250`。仓库层确实走 merge，见
`backend/internal/repository/account_repo.go:4374` 与实测
`TestBulkUpdate_MergeCredentials`（`account_repo_integration_test.go:1846`）。

这是改非敏感凭据键时最安全的批量路径——不会把没提到的键冲掉。

### 单字段凭据批量改

`POST /api/v1/admin/accounts/batch-update-credentials`

```jsonc
{ "account_ids": [1,2], "field": "account_uuid", "value": "..." }
```

`field` 白名单只有 `account_uuid` / `org_uuid` / `intercept_warmup_requests`
（struct 在 `account_handler.go:2709`）。想改别的键请用 bulk-update 或单账号 PUT。

### 状态与调度开关

| 目的 | 端点 | body |
| --- | --- | --- |
| 改 status | `PUT /admin/accounts/:id` | `{"status":"active\|inactive\|error"}` |
| 可调度开关 | `POST /admin/accounts/:id/schedulable` | `{"schedulable": true}` |
| 清错误 | `POST /admin/accounts/:id/clear-error` | 无 |
| 清限流 | `POST /admin/accounts/:id/clear-rate-limit` | 无 |
| 恢复状态机 | `POST /admin/accounts/:id/recover-state` | 无 |
| 重置配额 | `POST /admin/accounts/:id/reset-quota` | 无 |
| 临时不可调度 | `GET/DELETE /admin/accounts/:id/temp-unschedulable` | — |

停号时按需要两个都动。调度侧的实际过滤是**两组条件都要满足**：
`status == active` **且** `schedulable == true`（外加
`rate_limit_reset_at` 已过、`temp_unschedulable_until` 已过），
见 `backend/internal/repository/account_repo.go:2303`（`schedulableAccountsQuery`）
与 `:2318`（`ListSchedulableByGroupID`）。
所以：

- 只想临时不上量、但保留账号状态语义 → 只关 `schedulable`；
- 想让账号彻底退出调度、状态也标出来 → `status=inactive` 已经足够把账号
  排除在调度之外，`schedulable` 不必跟着改；
- 反过来，若只改 `status=active` 而 `schedulable` 仍为 false，账号照样不会被调度。

### 重新授权落凭据

`POST /admin/accounts/:id/apply-oauth-credentials`

```jsonc
{ "type": "oauth|setup-token", "credentials": {...}, "extra": {...} }
```

struct：`account_handler.go:2048`。为什么不用 `PUT /:id`：这个接口只接这三个
字段，且 `extra` 走键级合并，避免重新授权把 `base_rpm` / `quota_*` /
`privacy_mode` 等持久配置冲掉；内部还会顺带 ClearError + InvalidateToken。

### 只读辅助（写入前先查）

```
GET /admin/accounts?platform=openai&status=active&page_size=1000
GET /admin/accounts/:id
GET /admin/accounts/:id/freshness        # 状态新鲜度
GET /admin/accounts/:id/models
GET /admin/accounts/:id/usage
POST /admin/accounts/today-stats/batch   # {"account_ids":[1,2]}
POST /admin/accounts/usage/batch         # {"account_ids":[1,2],"force":false}
POST /admin/accounts/check-mixed-channel # {"platform":"openai","group_ids":[1],"account_id":null}
```

`GET /admin/accounts` 默认 `sort_by=name`、`sort_order=asc`，`page_size` 上限 1000。
`group` 过滤支持特殊值 `ungrouped`。

### 删号

`DELETE /admin/accounts/:id`（软删，DB 侧 `deleted_at`）
或 `POST /admin/accounts/batch-delete` `{"account_ids":[...]}`。

---

## 二、分组 groups

### 建分组

`POST /api/v1/admin/groups`

```jsonc
{
  "name": "必填",
  "platform": "anthropic|openai|...|composite",   // 缺省落为 anthropic（NormalizeGroupPlatform）
  "rate_multiplier": 1.0,                          // 必填且 > 0（<=0 报错）
  "description": "",
  "i18n": { "zh-CN": { "name": "...", "description": "..." } },
  "is_exclusive": false,
  "subscription_type": "standard|subscription",     // 缺省 standard
  "daily_limit_usd": 10,      // 可写数字、字符串数字、或 null
  "weekly_limit_usd": null,
  "monthly_limit_usd": null,
  "concurrency": 2,           // 仅 subscription 类型生效，0 = 不限
  "rpm_limit": 0,
  "fallback_group_id": null,
  "model_allowlist": { "enabled": false, "models": [] },
  "model_routing": { "claude-opus-*": [1, 2] },   // anthropic 平台
  "copy_accounts_from_group_ids": []              // 建组后从源组复制账号绑定
}
```

struct：`backend/internal/handler/admin/group_handler.go:229`
（`CreateGroupRequest`）。字段很多（图片/视频/音频定价、峰谷倍率、
利润控制、推理强度映射…），全量见 struct。

三条容易踩的：

1. `rate_multiplier` 必须 > 0，缺省 0 会被拒（`admin_group.go:502`）。
2. **限额三态**：`daily_limit_usd` 等用自定义类型 `optionalLimitField`
   （`group_handler.go:54`）——
   - 字段缺省 → 不改；
   - 显式 `null` 或 `""` → `ToServiceInput` 返回 -1 → `normalizeLimit` 转成
     nil → 落库 NULL = **无限制**；
   - 数字 / 数字字符串 → 设为该值。

   `0` 的实际效果也是**不限制**，不是"限额为零"。判定链是
   `HasDailyLimit() = DailyLimitUSD != nil && *DailyLimitUSD > 0`（`group.go:178`），
   执行点在 `billing_cache_service.go:1088`（日）与 `subscription_service.go:1148`（进度展示）
   都先过 `HasDailyLimit()`。注意 `admin_group.go:936` 的注释写着
   "0 表示不允许用量"，与执行行为不一致——按代码不按注释。

   要真正卡死额度，写一个很小的正数（例如 `0.01`）。
3. 创建时不能开启 `codex_models_manifest_config`（要在编辑里配）。

### 改分组

`PUT /api/v1/admin/groups/:id`，struct `group_handler.go:311`（`UpdateGroupRequest`）。

- 大部分字段是指针：缺省 = 不改。
- `rate_multiplier` 提供时必须 > 0。
- `status` 只能是 `active | inactive`。
- `vision_routing` 是带三态的自定义类型（`visionRoutingField`，
  `group_handler.go:203`）：缺省不改，显式 `{}` 清空，非空替换。
- `reasoning_effort_mappings`：`nil` 不改，`[]` 清空，非空替换。
- `copy_accounts_from_group_ids` 是同步操作：**先清空当前分组账号绑定，
  再绑定源分组的账号**。别在只想微调时随手带上。

### 分组内其它写入

| 目的 | 端点 | body |
| --- | --- | --- |
| 排序 | `PUT /admin/groups/sort-order` | `{"updates":[{"id":1,"sort_order":0}]}` 形态见 handler |
| 用户专属倍率 | `PUT /admin/groups/:id/rate-multipliers` | `{"entries":[{"user_id":1,"rate_multiplier":0.5}]}` |
| 清倍率 | `DELETE /admin/groups/:id/rate-multipliers` | — |
| 用户 RPM 覆盖 | `PUT /admin/groups/:id/rpm-overrides` | `{"entries":[{"user_id":1,"rpm_override":60}]}` |
| 清 RPM 覆盖 | `DELETE /admin/groups/:id/rpm-overrides` | — |
| 复制分组 | `POST /admin/groups/:id/duplicate` | 无 body，支持 `Idempotency-Key` |
| 组合路由 | `POST /admin/groups/:id/composite-routes` | 见下 |
| 组合路由预演 | `POST /admin/groups/:id/composite-routes/preview` | 见下 |

两个 entries 结构：`service.GroupRateMultiplierInput`（`user_group_rate.go:18`）
与 `GroupRPMOverrideInput`（`user_group_rate.go:25`，`rpm_override` 为 `*int`，
传 `null` 表示清该条覆盖）。

组合路由（仅 composite 平台分组）：

```jsonc
POST /admin/groups/:id/composite-routes
{
  "public_model": "必填",                 // 对外暴露的模型名
  "match_type": "exact|prefix",           // 缺省 exact
  "target_platform": "必填",              // 具体平台（不含 composite）
  "upstream_model": "",                   // 映射到上游的模型名
  "endpoint": "any|messages|count_tokens|responses|chat_completions|embeddings|images|gemini",
  "priority": 0,
  "enabled": true,
  "notes": ""
}
```

struct：`group_handler.go:393`。预演请求体只要 `{"model":"...","endpoint":"any"}`。
**改路由前先预演**（preview 不改数据，返回解析结果），确认命中顺序再写。

### 只读辅助

```
GET /admin/groups?platform=composite&page_size=1000
GET /admin/groups/all                       # 不分页，前端下拉用
GET /admin/groups/:id
GET /admin/groups/usage-summary
GET /admin/groups/capacity-summary
GET /admin/groups/:id/model-allowlist-candidates
GET /admin/groups/:id/composite-routes
```

---

## 三、订阅 subscriptions

### 分配（单用户）

`POST /api/v1/admin/subscriptions/assign`

```jsonc
{ "user_id": 123, "group_id": 45, "validity_days": 30, "notes": "" }
```

struct：`backend/internal/handler/admin/subscription_handler.go:43`

- `group_id` 必须是 `subscription_type = subscription` 的分组，
  否则报 `ErrGroupNotSubscriptionType`（`subscription_service.go:512`）。
- `validity_days <= 0` 时**默认 30 天**（不是报错，
  `normalizeAssignValidityDays`，`subscription_service.go:589`）；上限 36500 天。
- **`assign` 不是"延期"**（`subscription_service.go:506`
  `assignSubscriptionWithReuse`）：
  - 用户在该分组**没有**订阅 → 新建；
  - 已有订阅且**已过期/暂停** → 按 `validity_days` 重开新周期；
  - 已有订阅且**仍在有效期内** → 直接返回现有订阅（幂等成功），**不累加天数**；
    但若 `validity_days` / `notes` 与现有订阅隐含的值不一致（口径是按
    `starts_at + validity_days` 反算到期日），会返回
    `ErrSubscriptionAssignConflict`（`subscription_service.go:564`
    `detectAssignSemanticConflict`）。
- 想给有效订阅**加天数**，用 `POST /subscriptions/:id/extend` 或
  `bulk-action` 的 `extend`，别用 `assign`。

### 批量分配

`POST /api/v1/admin/subscriptions/bulk-assign`

```jsonc
{ "user_ids": [1,2], "group_id": 45, "validity_days": 30, "notes": "" }
```

`user_ids` 限 1..100。struct `subscription_handler.go:51`。

逐用户调用同一个 `assignSubscriptionWithReuse`（`subscription_service.go:471`），
所以上面三条语义（无订阅新建 / 过期重开 / 有效期内复用且不累加）逐个生效。
返回 `success_count` / `failed_count` / `reused_count` / `created_count` 与逐用户
`statuses`（`created` / `reused` / `failed`），单条失败不中断整批。
批量里有用户已持有有效订阅是**正常情况**，计入 `reused_count` 而非失败。

### 批量动作（改期 / 重置额度 / 作废 / 恢复）

`POST /api/v1/admin/subscriptions/bulk-action`（幂等 scope `admin.subscriptions.bulk-action`）

```jsonc
{
  "subscription_ids": [1,2,3],   // 1..100
  "action": "extend|reset_quota|revoke|restore",
  "days": 30,                     // action=extend 必填，正延期负缩短，非 0
  "daily": true,                  // action=reset_quota 至少一个为 true
  "weekly": false,
  "monthly": false
}
```

struct：`backend/internal/service/subscription_bulk_action.go:15`，
校验在同文件 `:25`。重复 ID 只执行一次；单条失败不中断，结果里逐条返回。

### 单条动作

| 目的 | 端点 | body |
| --- | --- | --- |
| 延期/缩短 | `POST /admin/subscriptions/:id/extend` | `{"days": 30}`（负=缩短，幂等 scope 存在） |
| 重置窗口 | `POST /admin/subscriptions/:id/reset-quota` | `{"daily":true,"weekly":false,"monthly":false}` |
| 作废 | `POST /admin/subscriptions/:id/revoke` 或 `DELETE /admin/subscriptions/:id` | 无 |
| 恢复 | `POST /admin/subscriptions/:id/restore` | 无 |

`days` 限制 -36500..36500。

### 只读辅助

```
GET /admin/subscriptions?user_id=123&group_id=45&status=active
GET /admin/subscriptions/:id
GET /admin/subscriptions/:id/progress
GET /admin/users/:id/subscriptions
GET /admin/groups/:id/subscriptions
```

---

## 四、激活码 quota_keys

这是 cursor 平台的"拉号额度"键，不是用户兑换码（兑换码见 `redeem-codes`）。

### 建/改

```jsonc
POST /api/v1/admin/quota_keys
{ "code": "明文激活码", "expires_at": "2026-12-31T00:00:00Z", "notes": "" }
```

struct：`backend/internal/handler/admin/quota_key_handler.go:147`。
`code` 只是进站时用一次，落库即加密；**响应里永远不回明文**。

`PUT /admin/quota_keys/:id`（struct `:254`）：

```jsonc
{
  "code": "...",                 // 可选，改码
  "expires_at": "..." | null,    // 三态：缺省不改 / null 清空 / 值设置
  "notes": "..." | null
}
```

`status` **刻意不可编辑**（请求体不声明该字段），想改状态走下面两个动作端点。

### 状态动作

| 目的 | 端点 | 说明 |
| --- | --- | --- |
| 解除 exhausted | `POST /admin/quota_keys/:id/clear_exhausted` | 只清标记，不拉号 |
| 重新拉号 | `POST /admin/quota_keys/:id/refresh` | 按 key 重新拉账号 |
| 探测额度 | `POST /admin/quota_keys/:id/probe_quota` | 装配未接线时 503 |
| 到期清扫 | `POST /admin/quota_keys/sweep_expired` | 返回 `{"swept": n}` 本轮改写条数 |

状态值：`active`（正常可拉号）、`exhausted`（拉号失败占位，可手动解除）。

### 入库配置（cursor 自动入库）

`GET|PUT /admin/quota_keys/auto_entry_config`

```jsonc
{
  "vendors": { "cursor": true },
  "models": [ { "id": "gpt-5", "vendor": "cursor", "probe_status": "" } ],
  "entry_defaults": { "concurrency": 3, "priority": 0, "rate_multiplier": 1.0 }
}
```

struct：`quota_key_handler.go:411`。`concurrency` 为 `*int`，`null` = 沿用列默认 3。

### 只读

`GET /admin/quota_keys`（**一次返回全量摘要，无分页参数**）、`GET /admin/quota_keys/:id`。
列表里每条带 `account_count` / `working_count` 与额度快照。

---

## 五、系统设置 settings

### 全量设置

`GET|PUT /admin/settings`（struct `setting_handler_update.go:24`，
`UpdateSettingsRequest`，字段很多：注册开关、SMTP、Turnstile、OAuth、
限额、审计保留天数…）

**部分更新语义**（这点最容易踩）：

`PUT /admin/settings` 用**请求体里实际出现的键**决定写哪些 key
（`omittedSettingKeys`，`setting_handler_update.go:483`）。因此：

- 只发你要改的键 → 其余键保持库里现值。**推荐这种做法**。
- 发全量 → 每个键都按请求值落库；漏掉的键因为是"没发"所以也会保持——
  但如果你把 `GET` 的结果改一处再整体回传，就等于用旧快照覆盖并发的修改。

所以：**只发要改的键，不要全量回传**。

两个安全开关（`session_binding_enabled`、`step_up_enabled`、
`passkey_enabled` 等）在 struct 里是指针类型，缺省 = 保持现值，
防止老客户端全量保存时静默重置。

开启 step-up 有自锁风险保护：需操作者本人已启用 TOTP 才能开；
关闭 step-up 本身也强制走 step-up 校验（`EnforceStepUpAlways`）。

### 细分设置（推荐，字段少、不易误伤）

| 端点 | body |
| --- | --- |
| `PUT /admin/settings/overload-cooldown` | `{"enabled":true,"cooldown_minutes":5}` |
| `PUT /admin/settings/rate-limit-429-cooldown` | `{"enabled":true,"cooldown_seconds":60}` |
| `PUT /admin/settings/panel-rate-limit` | `{"enabled":true,"user_rpm":60,"heavy_rpm":10,"exempt_admin":true,"public_ip_rpm":120}` |
| `PUT /admin/settings/stream-timeout` | `{"enabled":true,"action":"...","temp_unsched_minutes":5,"threshold_count":3,"threshold_window_minutes":10}` |
| `PUT /admin/settings/rectifier` | `{"enabled":true,"thinking_signature_enabled":true,"thinking_budget_enabled":true,"apikey_signature_enabled":false,"apikey_signature_patterns":[]}` |
| `PUT /admin/settings/beta-policy` | `{"rules":[...]}` |
| `PUT /admin/settings/web-search-emulation` | 见 handler |
| `PUT /admin/settings/openai-images-oauth-unavailable-cooldown` | `{"cooldown_minutes":30}` |
| `PUT /admin/settings/:.../email-templates/:event/:locale` | 邮件模板，`events`/`locale` 走路径 |

这些端点都是**整体覆盖**该子配置（不是部分更新），改之前先 `GET` 同路径。

### 管理员 API Key

```
GET    /admin/settings/admin-api-key            # 查看（脱敏）
POST   /admin/settings/admin-api-key/regenerate # 重新生成
DELETE /admin/settings/admin-api-key            # 删除
```

重新生成后旧 key 立即失效——先确认脚本里换好新值再删旧的。

---

## 附：兑换码 redeem-codes（不在五域内，附上常用三个）

```jsonc
POST /admin/redeem-codes/generate          // 幂等
{ "count": 1, "type": "balance|concurrency|subscription|invitation",
  "value": 10, "group_id": null, "validity_days": 0,
  "expires_at": null, "expires_in_days": 30 }

POST /admin/redeem-codes/create-and-redeem // 幂等
{ "code": "订单号", "type": "balance", "value": 10, "user_id": 123,
  "group_id": null, "validity_days": 0, "notes": "" }

POST /admin/redeem-codes/batch-update
{ "ids": [1,2],
  "fields": { "status": "unused", "notes": "", "group_id": null,
              "value": 10, "type": "balance", "expires_at": "..." } }
```

struct：`backend/internal/handler/admin/redeem_handler.go:38`、`:50`；
批量体在 `backend/internal/handler/dto/types.go:735`（注意是 `ids` + `fields`
两层，字段不写在顶层）。`expires_at` 与 `expires_in_days` **互斥**，
同时给会 400。`type=subscription` 时 `group_id` 必填。
