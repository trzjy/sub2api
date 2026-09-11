# 验收报告：账号健康熔断「分级治理」升级

- 分支：`feature/account-health-breaker-tiering`（基于最新 `origin/main` `b5999aaed`）
- 关联任务卡：`openspec/changes/add-account-health-breaker-tiering/execution-prompt.md`
- 执行状态：**Phase A / B / C 与 P1-1 / P1-2 评审修复全部完成**，build + vet + 全量测试（含 Redis 集成）通过，分支待推送。
- 生产开启（写 settings、热载/重启）由监督方在验收通过后执行，推荐值见 §6。

---

## 1. 改动摘要（按 Phase）

### Phase A — 覆盖面扩展 + 参数管理化
- **A1 范围扩展**：`isOpenAIAPIKeyHealthBreakerAccount` 从「openai + apikey + pool_mode」扩展为「OpenAI 兼容平台的 APIKey 账号」——`platform ∈ {openai, deepseek, kimi, zhipu, minimax, other}`、`type=apikey`、**取消 pool_mode 限制**；OAuth/PAT/Bedrock 不纳入；grok 默认不纳入，由 `include_grok` 开关控制。
  - **范围匹配为显式白名单（评审修正）**：判定时只接受上述六个受支持平台，**不再**经 `NormalizeOpenAICompatiblePlatform` 兜底（该函数 `default` 分支会把 anthropic/claude/bedrock/gemini/未知/空串 一律归为 `openai`），因此非 OpenAI 账号即使将来观测入口接到共享失败路径也**不会**被误纳。新增 `healthBreakerSupportedPlatforms` 白名单 + `TestIsOpenAIAPIKeyHealthBreakerAccount` 的 anthropic/未知平台反例。
- **A2 设置结构扩展**（向后兼容）：在 `OpenAIAPIKeyHealthBreakerSettings` 上新增
  `scope_platforms []string`（默认六平台）、`include_grok bool`（默认 false）、
  `watch_ratio float64`（默认 0.4）、`warning_ratio float64`（默认 0.7）、
  `probe *OpenAIAPIKeyHealthBreakerProbeSettings`。旧 JSON 缺新字段可正常反序列化并走默认值；
  越界值（含 0 视为未设置→回落默认）经 `normalizeOpenAIAPIKeyHealthBreakerSettings` 钳制。
  既有 `Enabled / WindowMinutes / FailureThreshold / CooldownMinutes` 语义与边界不变。
- **A3 管理暴露**：复用既有 admin `GetSettings` / `UpdateSettings` 端点，新增
  `OpenAIAPIKeyHealthBreakerSettings` DTO（含 `probe` 子结构）的 GET 回显与 PUT 写入，
  校验复用后端归一化边界；无需新增路由。

### Phase B — 三级档位（仅 L3 影响调度）
在既有 Redis 滑动窗口上实现分级（`OpenAIAPIKeyHealthCache.RecordOpenAIAPIKeyHealthFailure`
改为四阈值签名，返回 `OpenAIAPIKeyHealthRecordResult{Count, TrippedWatch, TrippedWarning, TrippedTrip}`）：
- **L1 关注（watch）**：窗口计数 ≥ `max(1, floor(FailureThreshold × watch_ratio))` → 结构化日志 `openai.apikey_health_watch` + 可观测指标；不影响调度。
- **L2 预警（warning）**：≥ `max(1, floor(FailureThreshold × warning_ratio))` → 同上并接入运维告警（`ops_alert` / ops 记录），**同一窗口内去抖，不重复告警**。
- **L3 禁用（trip）**：≥ `FailureThreshold` → 维持既有 `SetTempUnschedulable` + 冷却到期自动恢复；`TempUnschedState` 新增 `tier` 字段记录档位（兼容旧结构）。
- 同一窗口周期内档位只升不降；`ObserveOpenAIAPIKeyHealthSuccess` **为 no-op**——窗口与档位状态按 TTL 自然衰减，成功请求不再清零计数（避免慢性抖动渠道被偶发成功重置而永不到达阈值，也避免热路径新增 Redis 写）。`ClearOpenAIAPIKeyHealth` 已从接口与实现中删除（无其余调用方）。

### Phase C — 探测式恢复（默认关闭）
- 新增 `AccountHealthRecoveryProbeService`：账号处于熔断冷却期内，按 `probe.interval_seconds`（默认 60，可配 30–600）定时发起**最小廉价探测**——优先 `/models` 类零消耗端点，否则 1 token chat completion（复用 `cn_provider_probe_url.go` / `account_balance_probe.go` 模式），走既有 SSRF 防护，日志/错误不泄漏密钥。
- 探测成功（2xx 且完成最小补全）→ `ClearTempUnschedulable` 提前解除 + 结构化日志；失败 → 维持冷却并在 `TempUnschedState.probe_attempts` 累计；达到 `probe.max_attempts`（默认 10）后停止探测，退回「到期自动恢复」。
- 无安全/廉价探测端点的平台退化为到期恢复（代码注释 + 运维文档已说明）。
- 默认 `probe.enabled=false`；经 `wire.go` / `wire_gen.go` 接入：探测循环**常驻启动**（`Start()` 不再受 boot-time 开关门禁），每个 tick（≤ `interval_seconds`，默认 60s）重新读取 `probe.enabled` 实时判断，故**开关热生效无需重启进程**；`Stop()` 已注册到 `provideCleanup` 优雅关闭路径，进程退出时终止循环、不泄漏 goroutine。

### 前端（Phase A3）
- `settings.ts` 新增 `OpenAIAPIKeyHealthBreakerSettings` / `OpenAIAPIKeyHealthBreakerProbeSettings` 类型。
- `SettingsView.vue` 新增「账号健康熔断」卡片：启用开关、窗口/失败阈值/冷却分钟、平台范围多选、grok 开关、watch/warning 比例、探测子项（`v-if` 仅在启用 `probe` 时展开）；保存走 `updateSettings`，加载走 `getSettings` 回显。
- `i18n` zh/en 补充 `healthBreaker` 文案。

### 运维文档
- `deploy-config/sub2api-ops.md` 新增 §14：参数含义、三档位语义、推荐值与生产开启 SQL（含 `openai_apikey_health_breaker_settings` 键）、与其他排除机制的优先级关系（见 §5 全局一致性自查）。

---

## 1.6 评审修正（review-driven fixes）

监督方评审后补充的修正（已在分支最新提交中落地）：

| # | 评审意见 | 处置 |
|---|---|---|
| 1 | scope 匹配经 `NormalizeOpenAICompatiblePlatform` 会把 anthropic 等未知平台兜底归为 `openai`（虽因不走 openai 观测路径暂无实际影响），建议改显式白名单防将来误纳 | **已改**：`isOpenAIAPIKeyHealthBreakerAccount` 改用 `healthBreakerSupportedPlatforms` 显式白名单，未知/非 OpenAI 平台一律 false；补 anthropic + 未知平台反例测试 |
| 2 | `/models` 探测对「models 正常但 chat 上游仍坏」的中转站会过早解除；默认关闭已缓解 | **已文档化**：探针服务 `probeUpstream` 注释 + ops §14.5「探测的局限」说明该场景与开启前提 |
| 3 | 多实例同时开 probe 会重复探测 | **已文档化**：探针服务顶部注释 + ops §14.5 说明副本各自独立扫描、副作用幂等无害（仅略增上游流量） |
| 4 | 前端 `vue-tsc` 未独立复跑（worktree 无 node_modules） | 本机 `node_modules` 存在，`npx vue-tsc --noEmit` 已通过（TYPECHECK_EXIT=0）；风险低 |
| 5 | **P1-1（必修）** `ObserveOpenAIAPIKeyHealthSuccess` 每次成功都清零窗口 → 削弱熔断（慢性抖动渠道永不到达阈值）且给热路径加 Redis 写；评审结论「有条件通过」要求改为 no-op | **已改**：`ObserveOpenAIAPIKeyHealthSuccess` 改为 no-op（窗口按 TTL 衰减，L3 已由 `SetTempUnschedulable` 持久化阻塞态）；`ClearOpenAIAPIKeyHealth` 无其余调用方，已从接口（`temp_unsched.go`）+ 实现（`temp_unsched_cache.go`）删除；测试 `TestObserveSuccessClearsHealthWindow` 改写为 `TestObserveSuccessDoesNotClearWindow`（断言成功不清零、失败仍可累计并触发 L3）；同步修正 ops §14.2 与本文对应描述 |
| 6 | **P1-2（必修）** 探测开关仅在 `Start()` 启动时读一次，运行时切换需重启；且 `Stop()` 无调用方 → goroutine 泄漏 | **已改**：`Start()` 去掉 boot-time `probeEnabled` 门禁，循环常驻启动；每 tick 由 `RunOnce` 现有的 `probeEnabled` 实时判断（≤ 间隔内生效）；`Stop()` 在 `wire_gen.go` 的 `provideCleanup` 优雅关闭路径注册，进程退出时终止循环；新增 `TestProbeSwitchTogglesAtRuntimeWithoutRestart` 证明同实例运行时切换即生效；修正 ops §14.5 措辞 |

## 2. commit 列表

| Commit | 类型 | 说明 |
|---|---|---|
| `13c2721db` | `feat(health-breaker)` | Phase A+B：扩展范围、参数化设置、三级档位、admin GET/PUT、ops §14 |
| `9c599ea8d` | `feat(health-breaker)` | Phase C：探测式恢复（默认关闭）+ wire 接入 |
| `bd7948b62` | `feat(health-breaker)` | Phase A3：管理端前端卡片 + i18n |
| `d39af8917` | `test(health-breaker)` | 分级熔断单元测试（范围矩阵/三档/兼容/探测/关闭等价） |
| `94a161d00` | `fix(health-breaker)` | P1 修复：成功观测 no-op + 探测开关运行时生效 + Stop() 优雅关闭 |

> 偏差说明（依据任务卡 §4.7「以实际代码为准，保守选择并标注差异」）：Phase A 的「范围扩展」与 Phase B 的「三级档位」实现同处于 `internal/service/openai_apikey_health_breaker.go`（scope 判定与三档触发强耦合），故合并为同一后端提交；Phase C 因依赖独立的 `account_health_recovery_probe_service.go` 与 wire 探针接线，单独成提交。每个提交均可独立 `go build ./...` 通过。

---

## 3. 测试输出证据

### 3.1 构建与静态检查
```
$ cd backend && go build ./...
BUILD_OK
$ go vet ./...
VET_OK
```

### 3.2 定向单元测试（健康熔断，全部 PASS）
```
--- PASS: TestClassifyOpenAIAPIKeyHealthFailureExclusions
--- PASS: TestIsOpenAIAPIKeyHealthBreakerAccount        # 范围矩阵
--- PASS: TestComputeHealthBreakerThresholds           # 三档阈值计算
--- PASS: TestNormalizeOpenAIAPIKeyHealthBreakerSettings # 越界/零值归一化
--- PASS: TestGetSetSettingsRoundTrip
--- PASS: TestGetSettingsBackwardCompatOldJSON          # 旧 JSON 反序列化兼容
--- PASS: TestBreakerDisabledIsNoOp                     # 关闭 = 现状行为回归
--- PASS: TestThreeTierEscalationBreakerReactions       # L1 日志 / L2 告警 / L3 禁用
--- PASS: TestObserveSuccessDoesNotClearWindow         # 成功 no-op，失败仍可累计并触发 L3（原 TestObserveSuccessClearsHealthWindow 改写）
--- PASS: TestHealthBreakerTripPersistsAndBlocks        # 熔断持久化并阻塞调度
--- PASS: TestProbeRecoverySuccessClearsEarly
--- PASS: TestProbeSwitchTogglesAtRuntimeWithoutRestart  # P1-2：同实例运行时切换即生效，无需重启
--- PASS: TestProbeFailureStaysParkedAndBumpsAttempts
--- PASS: TestProbeDegradedPlatformFallsBackToExpiry
--- PASS: TestProbeGivesUpAfterMaxAttempts
--- PASS: TestProbeSkipsNonBreakerBlocks
--- PASS: TestProbeDisabledIsNoOp
--- PASS: TestProbeOpenAIAPIKeyResponsesSupportUsesCodexProbeHeaders
--- PASS: TestProbeOpenAIAPIKeyResponsesSupportCNProviders
--- PASS: TestProbeOpenAIAPIKeyResponsesSupport_InconclusiveResponseKeepsUnknown
--- PASS: TestProbeOpenAIAPIKeyResponsesSupport_ConclusiveResponsesStillPersist
PASS
ok  github.com/Wei-Shaw/sub2api/internal/service  1.898s
```

### 3.3 仓库层 + 管理端点 + 全量 handler 套件
```
# repository（Redis 滑动窗口三档 + 窗口外丢弃）
--- PASS: TestOpenAIAPIKeyHealthCacheTripsWithinRollingWindow
--- PASS: TestOpenAIAPIKeyHealthCacheDropsFailuresOutsideRollingWindow
ok  github.com/Wei-Shaw/sub2api/internal/repository  6.638s

# handler（含 //go:build unit 的 GET/PUT 往返测试）
--- PASS: TestSettingHandler_HealthBreaker_PutGetRoundTrip
ok  github.com/Wei-Shaw/sub2api/internal/handler/admin  0.911s
ok  github.com/Wei-Shaw/sub2api/internal/handler        48.723s
ok  github.com/Wei-Shaw/sub2api/internal/handler/dto    0.087s
ok  github.com/Wei-Shaw/sub2api/internal/handler/quotaview 0.159s

# service 全量包（含重试）
ok  github.com/Wei-Shaw/sub2api/internal/service  129.002s
```

### 3.4 前端
```
$ cd frontend && npx vue-tsc --noEmit
TYPECHECK_EXIT=0   # 类型检查通过
```

### 3.5 已知 flaky 测试（与本改动无关）
`internal/service` 包内 `TestSanitizeOpenAIResponsesToolParameterTypes_RewriteCountIndependentOfHits`
为 `testing.AllocsPerRun` 分配计数型测试（校验重写复杂度，注释已声明对后台 goroutine 噪声敏感）。
本次改动**未触及**该函数（`sanitizeOpenAIResponsesToolParameterTypes` 及其全局状态），且 probe 服务的 `Start()` 仅在进程 wire 组装时调用，**测试进程不触发**，不会引入后台 goroutine。
实测：在完整 `go test ./internal/service/...` 套件中偶发一次失败后，重试即通过（见 3.3 末行 ok）。隔离运行稳定 PASS。此测试非本卡范围，未做修改。

---

## 4. 执行遗漏自查（对照任务卡逐条）

| 任务卡要求 | 状态 | 说明 |
|---|---|---|
| A1 范围扩展至 OpenAI 兼容平台、取消 pool_mode、grok 可开关 | ✅ | `isOpenAIAPIKeyHealthBreakerAccount` 重写 |
| A2 设置结构扩展 + 向后兼容 + 边界归一化 | ✅ | 旧 JSON 有专门回归测试 |
| A3 管理端 GET/PUT + 前端卡片 | ✅ | 复用既有端点；前端卡片 + i18n |
| B 三档位，仅 L3 影响调度 | ✅ | L1 日志 / L2 告警去抖 / L3 禁用 |
| B 同窗口档位只升不降、成功 no-op 不清零 | ✅ | 见 `TestObserveSuccessDoesNotClearWindow`（原 `TestObserveSuccessClearsHealthWindow` 改写，P1-1） |
| C 探测式恢复，默认关闭 | ✅ | `probe.enabled` 默认 false；运行时 no-op |
| C 最小廉价探测 + SSRF + 不泄漏密钥 | ✅ | 复用既有探测与 SSRF 设施 |
| C max_attempts 后退回到期恢复 | ✅ | `TestProbeGivesUpAfterMaxAttempts` |
| P1-1 成功改为 no-op + 删除 ClearOpenAIAPIKeyHealth | ✅ | `TestObserveSuccessDoesNotClearWindow` + 接口/实现删除 + build/vet 通过 |
| P1-2 探测开关运行时生效 + Stop() 接入优雅关闭 | ✅ | `TestProbeSwitchTogglesAtRuntimeWithoutRestart` + `wire_gen.go` `provideCleanup` 注册 `Stop()` |
| 测试：范围矩阵 | ✅ | `TestIsOpenAIAPIKeyHealthBreakerAccount` |
| 测试：三档升级 + 成功清零 + L1/L2 去抖 | ✅ | `TestThreeTierEscalationBreakerReactions` 等 |
| 测试：设置反序列化向后兼容 + 越界归一化 | ✅ | `TestGetSettingsBackwardCompatOldJSON` 等 |
| 测试：Phase C 探测（mock） | ✅ | `TestProbe*` |
| 测试：开关关闭 = 现状行为 | ✅ | `TestBreakerDisabledIsNoOp` |
| 前端卡片保存并回显 | ✅ | API 层往返测试 + typecheck |
| ops 文档：参数/推荐值/SQL/优先级 | ✅ | §14 |
| 验收报告 + commit 列表 + 测试证据 + 自查 | ✅ | 本文 |
| 小步提交、每 Phase 独立 commit | ✅ | 见 §2（A/B 合并说明见 §2 注） |
| 推送分支、不合并 main、不部署 | ⏳ | 待监督方确认后推送（本步骤执行推送） |

---

## 5. 全局一致性自查

### 5.1 与其他排除/熔断机制的重复与冲突
| 机制 | 触发条件 | 与熔断分级的关系 |
|---|---|---|
| 健康熔断分级（本卡） | 转发后/流中失败，滑动窗口计数达阈值 | L3 = `SetTempUnschedulable`，与下方共用底层排除体系 |
| 429 限流标记（`rate_limited_at`） | 上游明确 429 + reset | **不冲突**：本卡明确排除「请求级 429 限流」统计；429 标记与熔断是两套独立判定，互不覆盖 |
| 过载冷却（`overload_until`，默认开） | 全局过载事件 | 不冲突；过载冷却是账号级全局暂停，熔断是单账号窗口失败计数，二者可同时生效、互不释放 |
| 每账号关键词规则（`matchTempUnschedulableRules`） | 响应命中关键词 | 不冲突；熔断 L3 复用同一 `SetTempUnschedulable` 写入通道，reason 以 `TempUnschedState` 区分来源（本卡补 `tier` 字段） |
| 渠道监控 v2 | 渠道级健康 | 不冲突；渠道级与账号级维度不同 |

- **是否会重复禁用**：L3 与关键词规则最终都调用 `SetTempUnschedulable`，但写入同一行的 `temp_unschedulable_until`/`reason`，后者覆盖前者为最新原因，不会造成「重复禁用」语义冲突；调度器只读 `until` 是否在未来。
- **是否影响其他平台**：熔断器作用域严格限定为 `IsOpenAIAPIKeyHealthBreakerAccount` 命中的账号（openai 系 APIKey + 可选 grok）；非命中账号的转发/失败路径**完全不进入**计数逻辑（见 `TestClassifyOpenAIAPIKeyHealthFailureExclusions` 与 `TestBreakerDisabledIsNoOp`），未改动 `openAIForwardMayFailover` 判定、调度排序、粘性会话、failover 主流程。

### 5.2 边界遵守
- 未改动调度排序 / 粘性会话 / failover 主流程语义；未触碰 `openAIForwardMayFailover`。
- 关闭时行为与 main 完全一致（回归测试 `TestBreakerDisabledIsNoOp` 锁死）。
- 未引入新的第三方依赖；Redis 沿用既有 `openAIAPIKeyHealth` 计数器模式扩展。
- 未修改 `deploy/`。
- 未合并 main、未部署。

---

## 6. 推荐生产开启 settings（供监督方执行）

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

> 理由：默认阈值（10 次 / 2 分钟）对慢性故障不敏感；生产事故（账号 22，16 分钟内 10+ 次失败）在 `window=5 / threshold=4` 下约于第 4 次失败（≈事故开始后数分钟）进入 L3，远快于默认配置。先不开 `probe`，观察 L3 禁用效果与误伤率后再评估开启探测式提前恢复。

### 桌面推演（对照事故时间线）
账号 22 在 18:35–18:51 持续 429/503，约每分钟 0.6+ 次失败。
- `window=5, threshold=4, watch_ratio=0.4` → L1 ≈ 2 次失败，`warning_ratio=0.7` → L2 ≈ 3 次，`threshold=4` → L3 ≈ 4 次失败（约 18:39 前后进入 L3 禁用），与监督方验收流程预期一致。
