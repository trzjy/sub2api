# 修复任务卡：账号健康熔断验收 P1 修复（增量）

> 前置：分支 `feature/account-health-breaker-tiering` 的首轮验收已完成，结论为「有条件通过」。本卡只包含两项 P1 修复与可选 P2 项。**除本卡列出的内容外不得做任何其他改动**。仍遵守原任务卡（`execution-prompt.md`）的全部边界与交付约定：不合并 main、不部署、改动后重跑评审证据。
>
> 工作区注意：主工作区 `/mnt/data/sub2api` 已在 feature 分支上，其中 `frontend/src/utils/embedded-url.ts`、`frontend/src/utils/__tests__/embedded-url.spec.ts`、`frontend/src/views/user/__tests__/CustomPageView.spec.ts` 是**他任务的未提交改动，保持原样，严禁提交或还原**。

---

## P1-1 成功请求不得清零失败窗口（语义回归 + 热路径开销）

**问题**（验收定性）：`ObserveOpenAIAPIKeyHealthSuccess`（`backend/internal/service/openai_apikey_health_breaker.go`）现在对每个成功请求执行 settings 读取 + `ClearOpenAIAPIKeyHealth`（Redis DEL 清空失败窗口与档位 key）。两个危害：

1. 时好时坏的慢性故障渠道（本机制的核心治理对象）每个成功请求都清零计数，熔断退化为「需要连续 N 次失败才触发」，几乎永远够不到阈值；
2. 给每个在范围账号的成功请求增加一次 Redis 写，违背原设计明确注释（"must not add a Redis round trip to the hot path"）。

**修复要求**：

1. `ObserveOpenAIAPIKeyHealthSuccess` 恢复为 **no-op**（保留 nil 守卫直接返回，附注释说明：滚动窗口按时间自然衰减，档位 key 自带窗口 TTL 自动过期、L3 路径已清全部状态，成功路径无需参与，避免热路径 Redis 写）。
2. `OpenAIAPIKeyHealthCache.ClearOpenAIAPIKeyHealth` 若无其他调用方，从接口与实现中删除（先 `grep` 确认）；保留则说明理由。
3. `TestObserveSuccessClearsHealthWindow` 改写为 `TestObserveSuccessDoesNotClearWindow`：成功观测后窗口计数保留（后续失败仍能跨成功累计并触发档位）。
4. `deploy-config/sub2api-ops.md` §14.2 删除「成功…同时清零失败计数与档位状态」表述，改为「窗口按时间自然衰减，成功请求不重置窗口、不产生 Redis 写」。
5. `ACCEPTANCE.md` 中相应声明同步修正。

## P1-2 探测器生命周期：开关运行时生效 + 接入优雅关闭

**问题**（验收定性）：

1. `ProvideAccountHealthRecoveryProbeService` 只在进程启动时读一次 `probe.enabled` 决定是否 `Start()`——后台打开开关后循环永不启动，必须重启；而 ops 文档 §14.3 写「无需重启」（对熔断参数成立、对 probe 开关不成立，误导）。
2. `Stop()` 无任何调用点：优雅关闭时探测 goroutine 泄漏（对比 `apiKeyBalanceProbeCheck.Stop()`、`upstreamBillingProbe.Stop()` 都接入了 wire cleanup）。

**修复要求**：

1. 探测循环**常驻启动**（去掉 Start 处的 boot-time `probeEnabled` 门禁；每个 tick 内由 `RunOnce` 现有的 `probeEnabled` 实时判断——该逻辑已存在）。验收标准：**开启/关闭 `probe.enabled` 后 ≤60 秒内生效，无需重启**（写一个模拟设置的测试证明）。
2. 探测间隔（`probe.interval_seconds`）允许两种实现之一：每 tick 重读设置动态调整；或文档明示「间隔修改需重启」。倾向前者（设置缓存 30s，成本可忽略）。
3. 在 wire cleanup（`backend/cmd/server/wire.go` 及 `wire_gen.go`，与 `apiKeyBalanceProbeCheck.Stop()` 同一 cleanup 区）注册 `Stop()`。
4. 修正 ops 文档 §14.3 的生效时间表述，与实际行为一致。

## P2（可选，强烈建议一并做，工作量很小）

1. **scope 匹配改为显式白名单**：`isOpenAIAPIKeyHealthBreakerAccount` 先经 `NormalizeOpenAICompatiblePlatform(account.Platform)` 会把 anthropic 等平台归一为 openai 而进入范围（当前这些账号不经过 openai 观测路径，无行为影响，但语义误导且有将来误纳风险）。改为：对 `account.Platform` 原值判断（属于 openai/deepseek/kimi/zhipu/minimax/other/grok 集合），仅对 scope_platforms 配置项做归一化。
2. **文档补 /models 探测局限**：§14 注明部分中转站 `/models` 不走重上游，探测成功不等价于 chat 链路完全恢复；因此 probe 默认关闭，开启前自行评估。

## 验收标准（增量复审用）

1. `cd backend && go build ./... && go vet ./...` 通过；`go test ./internal/service/ ./internal/handler/... ./internal/repository/ -count=1` 全绿（原样粘贴输出）。
2. 新增/改写测试：成功不清窗（含"失败→成功→失败仍累计"序列）、probe 开关运行时生效（≤60s）、probe 优雅关闭 Stop 被调用。
3. P2-1 若做：scope 矩阵测试补 anthropic/claude 平台用例（应不在范围）。
4. 文档与 `ACCEPTANCE.md` 增补「验收修复」小节：问题、修法、diff 摘要、证据。
5. 在同一分支追加 commit（Conventional Commits，如 `fix(health-breaker): ...`），推送 origin；**不合并 main、不部署**。

## 监督方增量复审范围（告知）

只审本次修复 diff + 重跑测试证据；通过后由监督方合并 main、部署并写生产 settings。
