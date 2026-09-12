# 执行提示词：账号健康熔断「分级治理」升级

> 交付方式：本文是给执行模型的完整任务卡。执行方在仓库 `/mnt/data/sub2api`（Wei-Shaw/sub2api 的 fork，Go 后端 + Vue 前端）内完成代码落地；监督方负责验收、合并与部署。执行方**不得**自行部署生产、不得直接向 `origin/main` 推送。

---

## 0. 角色与目标

你是代码执行方。目标：把仓库内已有但未启用、覆盖面窄的「OpenAI APIKey 账号健康熔断器」升级为**分级治理**体系：

- **L1 关注**：窗口内失败数达到关注阈值 → 记录与告警，不影响调度；
- **L2 预警**：继续恶化达到预警阈值 → 告警升级，仍不影响调度；
- **L3 禁用**：达到熔断阈值 → 临时禁用该账号（复用现有 `SetTempUnschedulable`），进入冷却；
- **恢复**：默认冷却到期自动恢复（现有能力）；可选「冷却期内定时探测、确认健康提前解除」（本期实现但**默认关闭**）。

分三个 Phase 交付（A 覆盖面与管理化、B 三级档位、C 探测式恢复），每个 Phase 独立成 commit、独立可验收。

## 1. 背景事故（需求来源，真实生产事件）

2026-09-11 18:35–18:51（北京时间），生产 `corealgos.com`：

- openai 分组「gpt-max（满血）」（group_id=37）内账号 22「Arc Codex」（上游 `https://www.lumoncode.com/v1`，type=apikey、credentials.pool_mode=true、优先级 1）持续返回上游错误：`Concurrency limit exceeded for account, please retry later`、`Too many pending requests, please retry later`、`Upstream service temporarily unavailable`（429/503），16 分钟内 10+ 次失败。
- 关键特征：错误发生在 **SSE 流已建立之后**。日志为 `openai.forward_failed` 且 `upstream_error_response_already_written=true`（handler/openai_gateway_handler.go:955 一带），无法换号 failover；访问日志状态码 200，但错误被运维监控记录。
- 该路径**不触发** 429 限流标记：账号 22 的 `rate_limited_at` 全程为空，调度器始终视其为健康，加上优先级 1 与粘性会话，请求反复落回坏账号。
- 现有熔断器当时未启用：生产 settings 表无 `openai_apikey_health_breaker_settings` 键，代码默认 `Enabled=false`。

## 2. 现状代码事实（已由监督方核实；行号可能漂移，以实际代码为准）

| 能力 | 位置 | 要点 |
|---|---|---|
| 健康熔断器本体 | `backend/internal/service/openai_apikey_health_breaker.go` | `isOpenAIAPIKeyHealthBreakerAccount` 现仅认 `platform=openai && type=apikey && IsPoolMode()`；`classifyOpenAIAPIKeyHealthFailure` 只统计可归因于账号的 429/5xx（排除凭证失败、请求级瞬态、同账号可重试等）；`ObserveOpenAIAPIKeyHealthFailure/Success`，成功会清零计数 |
| 触发动作 | 同上 | 达阈值 → `accountRepo.SetTempUnschedulable(account.ID, now+CooldownMinutes, reason)` + `tempUnschedCache.SetTempUnsched` + 结构化日志 `openai.apikey_health_breaker_tripped`；reason 为 JSON 序列化的 `TempUnschedState{UntilUnix, TriggeredAtUnix, StatusCode, MatchedKeyword, RuleIndex, ErrorMessage, TriggerCount, TriggerThreshold, TriggerWindowMinutes}` |
| 设置 | `backend/internal/service/setting_openai_apikey_health.go`、`settings_view.go`（`DefaultOpenAIAPIKeyHealthBreakerSettings`） | settings key=`openai_apikey_health_breaker_settings`（`domain_constants.go:582`）；默认 Enabled=false、Window=2、Threshold=10、Cooldown=5；归一化边界 Window 1–60、Threshold 1–10000、Cooldown 1–60；带 30s 进程内缓存 |
| 调用点（转发前失败） | `backend/internal/service/openai_account_scheduler.go`（`ReportOpenAIAccountScheduleResult` 内，约 2429/2450 行） | 失败经 `ObserveOpenAIAPIKeyHealthFailure` 进入熔断计数 |
| 调用点（流中失败） | `backend/internal/handler/openai_gateway_handler.go`（约 874/1439）、`backend/internal/handler/openai_chat_completions.go`（约 329） | 在 `!openAIForwardMayFailover(...)`（语义字节已写出、无法换号）时调用 `ObserveOpenAIAccountHealthFailure` —— **流中失败已接入计数，升级时不得破坏这条路径** |
| 底层临时排除体系 | `backend/internal/service/ratelimit_service.go` | `HandleTempUnschedulable` / `matchTempUnschedulableRules` / `triggerTempUnschedulable`（按状态码+响应关键词的每账号规则）、`ClearTempUnschedulable`、`tempUnschedCache` |
| 其他自动排除 | `settings_view.go` | 429 限流标记（rate_limited_at/rate_limit_reset_at，带上游 reset 时间）；过载冷却 `DefaultOverloadCooldownSettings`（默认开、10 分钟，overload_until） |
| 平台口径 | `backend/internal/service/openai_gateway_scheduling.go` `NormalizeOpenAICompatiblePlatform` | OpenAI 兼容平台集合：openai / deepseek / kimi / zhipu / grok / minimax / other |
| 按平台扩展先例 | `backend/internal/service/antigravity_internal500_penalty.go` | 平台专属惩罚/熔断的既有模式 |
| 探测先例 | `backend/internal/service/account_balance_probe.go`、channel_monitor probe 相关文件、`channel_monitor_ssrf.go` | 探测器与 SSRF 防护的既有模式 |
| 告警设施 | `backend/internal/service/ops_alert_evaluator_service.go` 等 | 运维告警/通知可复用 |

## 3. 改造需求

### Phase A — 覆盖面扩展 + 参数管理化（必做）

- **A1 范围扩展**：熔断器适用范围从「openai + apikey + pool_mode」扩展为「OpenAI 兼容平台的 APIKey 账号」：platform ∈ {openai, deepseek, kimi, zhipu, minimax, other}（与 `NormalizeOpenAICompatiblePlatform` 口径一致）、type=apikey，**取消 pool_mode 限制**。OAuth/PAT/Bedrock 账号不纳入。grok 默认**不**纳入（其媒体生成有独立判定），是否纳入做成设置项。
- **A2 设置结构扩展**：在现有设置 JSON 上加字段，**必须向后兼容**（旧 JSON 缺新字段可正常反序列化并走默认值；新字段建议指针/带默认逻辑）。建议字段（命名可自定，语义对齐即可）：
  - `scope_platforms []string`（默认上表六平台）
  - `include_grok bool`（默认 false）
  - `watch_ratio float`（默认 0.4）、`warning_ratio float`（默认 0.7）（Phase B 用）
  - Phase C 的 `probe` 子结构（见下）
  - 既有 `Enabled / WindowMinutes / FailureThreshold / CooldownMinutes` 语义与归一化边界保持不变。
- **A3 管理暴露**：为该设置增加管理端 GET/PUT（对齐仓库现有 admin settings 端点模式，自查 `backend/internal/handler` 与 `backend/internal/server/routes` 的既有实现），前端设置页新增「账号健康熔断」卡片（启用、窗口分钟、失败阈值、冷却分钟、平台范围、grok 开关、watch/warning 比例、probe 子项），参数校验复用后端归一化边界。

### Phase B — 三级档位（必做）

在现有滑动窗口计数上实现分级；**只有 L3 影响调度**：

- **L1 关注（watch）**：窗口计数 ≥ watch 阈值（默认 `FailureThreshold × watch_ratio` 向下取整、最小 1）→ 结构化日志（新事件名，如 `openai.apikey_health_watch`，含 account_id、count、threshold、window）+ 可观测指标；不影响调度。
- **L2 预警（warning）**：≥ warning 阈值（默认 0.7×）→ 同上，并接入运维告警（复用 `ops_alert_evaluator_service` 或现有通知服务；至少落一条可查询的告警记录，避免只打日志）。
- **L3 禁用（trip）**：≥ `FailureThreshold` → 维持现有 `SetTempUnschedulable` + 冷却到期自动恢复逻辑，reason 中的 `TempUnschedState` 结构保持兼容并补充档位信息。
- 同一窗口周期内档位只升不降；`ObserveOpenAIAPIKeyHealthSuccess` 清零计数时**必须同时清除档位状态**。
- 三档跳变都要有结构化日志；L1/L2 重复触发同一窗口内不重复告警（去抖），避免刷屏。

### Phase C — 探测式恢复（必做，默认关闭）

- 账号处于熔断冷却期内，按 `probe.interval_seconds`（默认 60，可配 30–600）定时探测；探测成功（拿到 2xx 且完成一次最小补全）→ `ClearTempUnschedulable` 提前解除 + 结构化日志；失败 → 冷却维持（可选顺延），达到 `probe.max_attempts`（默认 10）后停止探测，退回「到期自动恢复」。
- 探测请求**必须**复用账号真实转发链路的最小廉价请求（优先 `/models` 类零消耗端点，否则 1 token 的 chat completion；参考 `cn_provider_probe_url.go`、`account_balance_probe.go` 模式），走现有 SSRF 防护，日志与错误信息中不得泄漏密钥。
- 默认 `probe.enabled=false`，仅管理员显式开启。某平台若无安全/廉价的探测端点，允许该平台探测退化为「到期恢复」，在代码注释与运维文档中写明。

## 4. 边界（禁止事项）

1. 不改动调度排序、粘性会话、failover 主流程的语义；不触碰 `openAIForwardMayFailover` 的判定。
2. 熔断器**关闭时行为与 main 完全一致**（这是回归安全的硬性要求，需有对比测试）。
3. 不引入新的第三方依赖；Redis 结构沿用现有 `openAIAPIKeyHealth` 计数器模式扩展。
4. 不修改 `deploy/`（上游模板）；运维文档更新写在 `deploy-config/sub2api-ops.md`。
5. 不部署生产、不合并 main（见第 6 节工作流）。
6. 遵守仓库 `AGENTS.md` 的全局协作与评审协议；**任何代码改动之后先前评审证据作废，须重跑**。
7. 遇本卡与实际代码冲突（函数不存在/行为不符/行号漂移），以实际代码为准，做保守选择，并在验收报告中标注差异。

## 5. 验收标准（Done when）

1. `cd backend && go build ./... && go vet ./...` 通过；`go test ./internal/service/... ./internal/handler/...` 全绿。
2. 新增单元测试覆盖：
   - 范围判定矩阵（openai/deepseek/kimi/zhipu/minimax/other/grok × apikey/OAuth × pool/非 pool）；
   - 三档位升级路径与成功清零（含 L1/L2 去抖）；
   - 设置反序列化向后兼容（旧 JSON、缺字段、越界值归一化）；
   - Phase C：探测成功提前解除 / 失败续期 / 达 max_attempts 停止（探测器用 mock）；
   - **开关关闭 = 现状行为**的回归对比测试。
3. 前端设置卡片能保存并回显（API 层测试必须；UI 层提供验证说明或截图）。
4. `deploy-config/sub2api-ops.md` 新增小节：参数含义、推荐值、生产开启步骤（含示例 settings JSON 与写入 SQL）、与 429 限流标记 / 过载冷却 / 每账号关键词规则 / 渠道监控 v2 的关系与优先级说明（谁先谁后、是否会重复禁用）。
5. 交付验收报告，含：改动摘要（按 Phase）、commit 列表、测试输出证据（原样粘贴）、**执行遗漏自查**（对照本卡逐条说明完成/未完成）、**全局一致性自查**（是否与既有排除机制产生重复或冲突规则、是否影响其他平台）。

## 6. 工作流与交付

1. 分支：`feature/account-health-breaker-tiering`，基于最新 `origin/main`。
2. 小步提交；每个 Phase 至少一个独立 commit，Conventional Commits 风格（对齐 `git log` 现有风格，如 `feat(gateway): ...`）。
3. 完成后推送分支到 origin，输出验收报告；**禁止**合并 main、**禁止**部署。
4. 生产开启（写 settings、重启/热载）由监督方在验收通过后执行；交付物需附推荐的 settings JSON，例如：

```json
{
  "enabled": true,
  "window_minutes": 5,
  "failure_threshold": 4,
  "cooldown_minutes": 5,
  "watch_ratio": 0.4,
  "warning_ratio": 0.7,
  "scope_platforms": ["openai", "deepseek", "kimi", "zhipu", "minimax", "other"],
  "include_grok": false,
  "probe": { "enabled": false, "interval_seconds": 60, "max_attempts": 10 }
}
```

（字段名以最终实现为准，需与 A2 兼容性约定一致；默认阈值 10 次/2 分钟对慢性故障不敏感，生产推荐值按上例，理由写入运维文档。）

## 7. 监督方验收流程（供执行方理解，无需执行）

监督方将按以下顺序验收：本地构建与全量测试复跑 → Phase 逐条对照本卡（含边界第 2 条的关闭等价性）→ 用生产事故场景做桌面推演（账号 22 在 18:35–18:51 的失败序列在推荐参数下应于 ~18:39 前后进入 L2、~18:39–18:44 进入 L3）→ 评审通过后方合并 main 并部署。
