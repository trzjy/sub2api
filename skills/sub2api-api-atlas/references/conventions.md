# 约定：鉴权 / 响应 / 分页 / 幂等 / 错误

本文只登记**跨端点通用**的约定。单端点特有的规则写在该端点的 playbook 里。

---

## 1. 鉴权域

| 域 | 中间件 | 凭据 | 源码锚点 |
| --- | --- | --- | --- |
| 管理面 | `adminAuth` | `x-api-key: <admin api key>`，或 `Authorization: Bearer <管理员 JWT>` | `backend/internal/server/middleware/admin_auth.go:24` |
| 登录用户面 | `jwtAuth` | `Authorization: Bearer <jwt>` | `backend/internal/server/middleware/jwt_auth.go` |
| 网关 | `apiKeyAuth` | `Authorization: Bearer <用户 API Key>`，也接受 `x-api-key` / `x-goog-api-key` | `backend/internal/server/middleware/api_key_auth.go:71` |
| 模型广场 | `optionalJWTAuth` | 可匿名 | — |
| 内部服务间 | 自带令牌校验 | 不走用户/管理员鉴权 | `backend/internal/server/routes/common.go:37` |
| 货源模式 | 六段验签中间件 | 闲管家签名 | `backend/internal/server/routes/xgj_supply.go` |

管理面还叠了三层：`panelRateLimiter.Global()`（面板限流，管理员默认豁免）、
`auditLog`（变更类操作入审计）、`AdminComplianceGuard`（部署与运营合规确认）。
源码：`backend/internal/server/routes/admin.go:27`。

**管理端 WebSocket**（如 ops 实时流）无法设 Authorization 头，改走子协议：
`Sec-WebSocket-Protocol: sub2api-admin, jwt.<token>`，见 `admin_auth.go:35`。

鉴权失败返回体是中间件独立结构，与业务响应封装**不同**：

```json
{ "code": "INVALID_ADMIN_KEY", "message": "Invalid admin API key" }
```

`code` 是字符串。业务响应里的 `code` 是整数。写脚本判错时别混用。

常见鉴权错误码：`UNAUTHORIZED`、`INVALID_ADMIN_KEY`、
`STEP_UP_REQUIRED`（403，敏感操作需先做 TOTP step-up）。

## 2. 响应封装

业务响应统一走 `backend/internal/pkg/response/response.go`：

```json
{ "code": 0, "message": "success", "data": { ... } }
```

- 成功：`code = 0`；错误：`code = HTTP 状态码`。
- 可选字段 `reason`、`metadata`（结构化错误细节），见 `ErrorWithDetails`。
- 创建成功用 `Created`（HTTP 201），异步受理用 `Accepted`（HTTP 202）。
- 分页数据固定形状：

```json
{
  "code": 0,
  "data": {
    "items": [ ... ],
    "total": 123,
    "page": 1,
    "page_size": 20,
    "pages": 7
  }
}
```

`PaginatedWithResult`（response.go:148）是唯一分页出口；部分端点额外把同一份
items 塞进 `quota_key_summaries` 之类的别名键做兼容，以端点实际返回为准。

## 3. 分页与查询参数

`response.ParsePagination`（response.go:170）：

- `page` 默认 1。
- `page_size` 默认 20，上限 1000；也接受 `limit` 作同义参数。
- 超过 1000 静默截断为默认值，**不报错**——写脚本时别靠报错发现分页问题。

各域的附加过滤参数不一样，必须读 handler。已核对过的：

| 端点 | 可用过滤 |
| --- | --- |
| `GET /admin/accounts` | `platform`、`type`、`status`、`search`、`group`（支持 `ungrouped`）、`privacy_mode`、`sort_by`（默认 `name`）、`sort_order`（默认 `asc`）、`lite`、`include_scheduler_score` |
| `GET /admin/groups` | `platform`、`status`、`search`、`is_exclusive`、`sort_by`（默认 `sort_order`，注意这个默认值本身有点怪）、`sort_order` |
| `GET /admin/subscriptions` | `user_id`、`group_id`、`status`、`platform`、`sort_by`（默认 `created_at`）、`sort_order`（默认 `desc`） |
| `GET /admin/quota_keys` | 无分页参数（一次返回全量摘要） |

## 4. 幂等

写操作里需要防重复的部分走 `Idempotency-Key` 请求头，服务端按
「scope + 请求体」去重，TTL 由各端点定（`DefaultWriteIdempotencyTTL`）。

设了幂等 scope 的写端点（源码实测，`internal/handler/admin/*.go` 里的 scope 字符串）：

```
admin.accounts.create / batch_create / import_data / import_codex_session
admin.accounts.duplicate / web-register / web-register-email-code
admin.groups.duplicate
admin.channel_monitors.duplicate
admin.redeem_codes.generate / create_and_redeem / welfare_batches.generate
admin.subscriptions.extend / bulk-action
admin.users.balance.update
admin.proxies.create
admin.system.update / rollback / restart
admin.usage.cleanup_tasks.create
admin.xianyu.pool_stock
```

（`admin.quota_updated` / `admin.quota_window_reset` 是 slog 日志标签，不是幂等 scope，
别混进来。）

另外几个不走 helper 但支持该头：`POST /admin/users/:id/withdraw`（直接读 header）、
`POST /admin/data-management/backups`（也接受 body 里的 `idempotency_key`，
header 名是 `X-Idempotency-Key`，见 `data_management_handler.go:187`）、
`POST /admin/accounts/:id/claude/reset-credits/redeem`（无 body，靠该头标识一次授权）。

**幂等 store 不可用时默认 fail-close**（拒绝写入并报
`idempotency store unavailable`），个别端点 fail-open，以源码
`executeAdminIdempotentJSON*` 的 mode 参数为准。

不需要幂等的普通 PUT 天然幂等，别硬塞幂等头——头里的 key 与请求体绑定，
换 body 复用同一 key 会命中旧结果。

## 5. step-up 2FA

敏感操作额外挂 `stepUpAuth`，未在有效期内完成 TOTP step-up 时返回
403 `STEP_UP_REQUIRED`，需先 `POST /api/v1/user/totp/step-up` 换授权。

挂 step-up 的端点（`admin.go` 内 `gin.HandlerFunc(stepUpAuth)`）：

```
POST /admin/accounts/platform-migration
GET  /admin/accounts/data          （导出，含凭据）
GET  /admin/proxies/data
POST /admin/data-management/s3/profiles
PUT  /admin/data-management/s3/profiles/:profile_id
POST /admin/data-management/s3/profiles/:profile_id/activate
POST /admin/data-management/backups
PUT  /admin/backups/s3-config
PUT  /admin/backups/image-storage
POST /admin/backups
GET  /admin/backups/:id/download-url
POST /admin/backups/:id/restore
POST /admin/plugins/upload
POST /admin/plugins/:id/enable
POST /admin/plugins/:id/disable
DELETE /admin/plugins/:id
PUT  /admin/plugins/:id/config
POST /admin/plugins/:id/test
```

**管理员 API Key 在这类端点上直接被拒**：step-up 开启时，
`x-api-key` 认证的请求返回 403 `STEP_UP_ADMIN_API_KEY_FORBIDDEN`
（"Admin API key cannot access this endpoint"），必须用**已过 TOTP step-up 的
管理员 JWT 会话**。源码：`backend/internal/server/middleware/step_up.go:105`。

step-up 关闭时这些端点直接放行。所以写脚本前先 `GET /admin/settings`
看 `step_up_enabled`；为 true 时备份/导出/插件类端点不能用 API Key 跑。

其余 step-up 错误码：`STEP_UP_TOTP_NOT_ENABLED`（管理员没开 TOTP）、
`STEP_UP_REQUIRED`（会话内近段时间未验证）、`STEP_UP_UNAVAILABLE`（校验服务故障，fail-closed）。

顺带一提：关闭 step-up 开关本身也走强制门控（`EnforceStepUpAlways`，
`setting_handler_update.go:568` 附近），属于防自锁设计。

## 6. 平台与状态枚举

平台清单唯一来源是 `backend/internal/domain/platforms.go`，
`group_platform` / `concrete_platform` 两个 binding 校验器都从它派生
（`backend/internal/handler/admin/platform_validation.go`）。当前具体平台：

```
anthropic, openai, gemini, antigravity, grok, kimi, zhipu, deepseek,
minimax, opencode_go, typesafe, command_code, cline, codebuddy, muse, cursor
```

分组平台额外接受 `composite`。`other` 是通用 OpenAI 兼容自定义上游，
**不在** `IsConcretePlatform` 清单内（刻意排除，见 platforms.go 头部注释）。

账号/分组状态用 `domain` 常量：`active`、`disabled`、`error`、
`unused`、`used`、`expired`。注意账号更新接口的 `status` 只接受
`active | inactive | error`（handler binding），与 DB 层的 `disabled` 不是一套，
落值前先看具体端点。

`is_subscription_type` 相关：`subscription_type` 只接受
`standard`（按余额）与 `subscription`（按限额）。

## 7. 凭据脱敏与合并

账号 `credentials` 里有一批**敏感键**，读接口一律脱敏回传，写接口收到缺省
时**保留原值**而不是清空。清单在
`backend/internal/service/account_credentials_redact.go:5`：

```
access_token, refresh_token, id_token, agent_private_key,
login_email, login_password, login_refresh_token,
api_key, session_key, cookie,
access_key, secret_key,
password, sso_token, sso, sso-rw, clearTextPassword,
aws_secret_access_key, aws_session_token,
service_account_json, service_account, private_key,
th_password, kira_jwt, kira_password,
balance_probe_access_token
```

合并函数 `MergePreservingSensitiveCreds`（同文件:50）：incoming 覆盖 existing，
敏感键在 incoming 缺省时从 existing 补回。

**关键差异**：

| 路径 | credentials 语义 | extra 语义 |
| --- | --- | --- |
| `PUT /admin/accounts/:id` | 敏感键缺省保留，非敏感键整体以 incoming 为准（合并后的 map 直接落库） | 提供即整体替换；`{}` 表示显式清空 |
| `POST /admin/accounts/bulk-update` | **JSONB 键级合并**（repository 层 merge，实测 `TestBulkUpdate_MergeCredentials`） | JSONB 键级合并 |
| `POST /admin/accounts/:id/apply-oauth-credentials` | 只接 `type`/`credentials`/`extra` 三字段，避免误带 concurrency 等 | 走 `UpdateAccountExtra`，键级合并，绝不整体覆盖 |
| `POST /admin/accounts/batch-update-credentials` | 只改**一个**白名单字段（`account_uuid` / `org_uuid` / `intercept_warmup_requests`） | 不动 |

由此推出两条实战结论：

1. 改非敏感 credentials 键（`base_url`、`model_mapping`、`balance_probe` 等）
   用 `PUT /:id` 时，**没带上的键会丢**。要么先 `GET` 全量再回传，要么改用
   `bulk-update`（它走键级合并）。
2. 敏感键本来就脱敏，`GET` 回来的是掩码，直接回传会把掩码写回去——所以
   `PUT` 才设计了"敏感键缺省保留"。**不要试图把脱敏值回填**。

## 8. 常见错误码

业务错误 `code` 字段是整数（HTTP 状态码），`message` 是给人和脚本看的文本。
需要按原因分支时读 `reason`（`ErrorWithDetails`）。

高频：

| HTTP | 含义 | 常见原因 |
| --- | --- | --- |
| 400 | 参数错误 | binding 校验失败，`message` 以 `Invalid request:` 开头 |
| 401 | 未认证 | 缺 header、key 无效、JWT 过期 |
| 403 | 无权限 / step-up | `STEP_UP_REQUIRED` |
| 404 | 不存在或未注册 | 端点整组未接线时也是 404 |
| 409 | 冲突 | 登录会话重复、唯一约束 |
| 503 | 依赖未装配 | 如 `Cursor auto-entry service unavailable` |
