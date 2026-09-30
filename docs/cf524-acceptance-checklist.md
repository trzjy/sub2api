# CF524 验收清单（覆盖矩阵 + 生产验收唯一口径 + 回滚兼容）

> 权威方案：`docs/cf524-upstream-hang-mitigation-plan.md` **v8.2 §4「验收」**。
> 派发单：`.omc/plans/cf524-g5-observability.md`（v2）。
> 前置交付：G1a/G1b（护栏+错误构造器+预算快照）、G3（心跳组件）、G2a（挂点 1/2/4 接线+挂点 3 既有接线）、
> G2b（三消费 handler 入口安装点+防线例外）、G4（配置键）、G5（本卡：可观测事件 + 判别器字段）。
>
> 本文件是**生产验收唯一口径**：判定只认本文件 §2 的命令与判据，其余描述性结论不构成验收依据。

---

## 1. 覆盖矩阵：五挂点 × 63-victim 全形态

**列定义（全形态 = 11 形）**：kimi / deepseek / zhipu 三平台 × 三路径（挂点 1 `/v1/messages`、
挂点 3 `/v1/chat/completions`、挂点 4 `/v1/responses`）= 9 形；anthropic 两形态（OAuth 主路径挂点 1、
API-key 直通挂点 2）= 2 形。

**行定义（五挂点）**：① `gateway_forward.go`（Forward 主路径，含 retry 循环 + 共享执行器）
② `gateway_anthropic_passthrough.go`（Anthropic APIKey 直通）
③ `gateway_forward_as_chat_completions.go`（CC→Anthropic，非共享执行器直连 DoWithTLS）
④ `gateway_forward_as_responses.go`（/v1/responses 非 OpenAI 平台）
⑤ `openai_gateway_forward.go`（OpenAI 原生路径——**零变化反向行**，不发任何新事件）。

### 1.1 悬挂（victim）形态 → 行为与用例对照

| # | 挂点 | 覆盖 victim 形态 | 期望行为（方案 §4） | 引用单测用例名 | 卡 |
|---|---|---|---|---|---|
| 1 | 挂点 1 `gateway_forward.go` | anthropic OAuth（claude-sonnet-5）+ kimi/deepseek/zhipu 三平台 | 悬挂 > guard → 心跳先起（SSE 200+keep-alive）→ guard 触发 → stop-and-wait → 换号；剩余<5s → 耗尽 | `TestG2AMount1ClaudeSonnet5OAuthHangHeartbeatThenGuardFailover`、`TestG2AMount1ClaudeSonnet5OAuthHangExhaustedWhenRemainingBelow5s`、`TestG2AMount1KimiDeepSeekZhipuHangReplay`（kimi/deepseek/zhipu 三子用例） | G2a |
| 2 | 挂点 2 `gateway_anthropic_passthrough.go` | anthropic API-key 直通（claude-sonnet-5） | 同上（直通形态） | `TestG2AMount2ClaudeSonnet5APIKeyPassthroughHangHeartbeatThenGuardFailover`、`TestG2AMount2ClaudeSonnet5APIKeyPassthroughHangExhausted` | G2a |
| 3 | 挂点 3 `gateway_forward_as_chat_completions.go` | kimi/deepseek/zhipu × CC 路径 | guard 触发 → `*UpstreamFailoverError`；剩余≥5s 交既有 FailoverContinue 换号，<5s 耗尽 | `TestForwardAsChatCompletions_GuardTriggerReturnsUpstreamFailoverError`（`budget_high_allows_failover` / `budget_low_exhausts` 两子用例） | G1b |
| 4 | 挂点 4 `gateway_forward_as_responses.go` | kimi/deepseek/zhipu × responses 路径 | 同挂点 1（单 attempt 形态） | `TestG2AMount4KimiDeepSeekZhipuHangReplay`（kimi/deepseek/zhipu 三子用例） | G2a |
| 5 | 挂点 5 `openai_gateway_forward.go` | OpenAI 原生路径（**零触碰反向行**） | 零新事件、零入口预算、零心跳 writer；既有 first_output_timeout 语义原样 | `TestOpenAIPathNoSharedFirstByteErrorConstructor`、`TestOpenAIFirstOutputHeaderGuardAdapterHeaderArrivesNoTimeout`、`TestOpenAIFirstOutputHeaderGuardAdapterTimeoutCancelsContext`、`TestOpenAIFirstOutputHeaderGuardAdapterCloseIdempotent`、`TestOpenAIForwardFirstOutputTimeoutIncludesResponseHeaderWait` | G1b |

### 1.2 非悬挂（配套）形态 → 行为与用例对照

| 场景 | 期望行为 | 引用单测用例名 | 卡 |
|---|---|---|---|
| 换号交接连续性 | Resume 首帧即时，交接空窗 < delay | `TestG2AMount1ClaudeSonnet5OAuthFailoverHandoffResumesWithoutDelay`、`TestUpstreamHeartbeatResumeHandoffGap` | G2a/G3 |
| 非流式墙内预算 | 入口已耗扣除 + 次轮窗口被剩余截断，总耗时 ≤ 总预算 | `TestG2AMount1NonStreamBudgetClampsSecondAttemptWindow` | G2a |
| 请求体可重放 | 换号前后 wireBody 字节一致（三挂点） | `TestG2AMount1ReplayWireBodyConsistent`、`TestG2AMount2ReplayWireBodyConsistent`、`TestG2AMount4ReplayWireBodyConsistent`、`TestForwardAsChatCompletions_RequestReplayWireBodyConsistent` | G2a/G1b |
| 客户端已断开 | 失败关闭、不重试 | `TestG2AMount1ClientDisconnectedFailsClosedNoRetry` | G2a |
| heartbeat=0 流式 | 双超时 ≤ guard+25s（墙内），零心跳帧 | `TestG2AMount1HeartbeatZeroStreamDoubleTimeoutWithinGuardPlus25s` | G2a |
| 响应头先到 / body 超预算 | 完整送达，护栏未取消 body 读取 context | `TestG2AMount1HeaderFirstBodyBeyondBudgetDelivered`、`TestUpstreamFirstByteGuardHeaderFirstBodyBeyondBudget` | G2a/G1a |
| 无快照 / 健康路径零回归 | 走既有旧路径；零心跳、零 guard 触发、状态码逐字透传 | `TestG2AMount1NoSnapshotLegacyPathSuccess`、`TestG2AMount1HealthyPathZeroHeartbeatZeroGuardTrigger`、`TestG2AMount2HealthyPathZeroHeartbeat`、`TestG2AMount4HealthyPathZeroHeartbeat` | G2a |
| 护栏线性化（三边界） | 截止点前/恰好/后各唯一胜出者 | `TestUpstreamFirstByteGuardLinearizationBeforeDeadline`、`TestUpstreamFirstByteGuardLinearizationAtDeadline`、`TestUpstreamFirstByteGuardLinearizationAfterDeadline` | G1a |
| 预算纯函数 / 不可变时间轴 | 总预算三分支 + 换号不重置窗口 | `TestUpstreamFirstByteBudgetPureFunction`、`TestUpstreamFirstByteImmutableTimeline` | G1a |
| 错误构造器 | `SafeToFailoverAfterWrite=true`；剩余预算两分支 | `TestUpstreamFirstByteTimeoutErrorSafeToFailover`、`TestUpstreamFirstByteTimeoutErrorRetryBranch` | G1a |
| 心跳 Flush 契约 / 失败关闭 | 初始头与每帧 Flush；无 Flusher 拒绝启用 | `TestUpstreamHeartbeatFlushContract`、`TestUpstreamHeartbeatNoFlusherFailsClosed`、`TestUpstreamHeartbeatLateStopZeroWrite` | G3 |
| 心跳优先级唯一 | 响应事件 vs tick、guard 终态 vs tick 各唯一胜者 | `TestUpstreamHeartbeatPriorityResponseWins`、`TestUpstreamHeartbeatPriorityTickWins`、`TestUpstreamHeartbeatPriorityGuardTerminal`、`TestUpstreamHeartbeatLateHandoffNoInterleave` | G3 |
| 三交叠竞态 | 上游响应/tick 同至、Stop/tick 同至、客户端断开退出 | `TestUpstreamHeartbeatRaceUpstreamVsTick`、`TestUpstreamHeartbeatRaceStopVsTick`、`TestUpstreamHeartbeatClientDisconnectExits` | G3 |
| 入口安装点与防线例外 | 快照+owner 已装；认证失败零副作用；防线例外两分支 | `TestCF524G2b_InstallPoint_SnapshotAndHeartbeatInstalledAtHandlerEntry`、`TestCF524G2b_InstallPoint_AuthFailure_NoInstallNoSideEffects`、`TestCF524G2b_HeartbeatFailoverGuardStillClean_Branches`、`TestCF524G2b_HeartbeatCommittedExhaustedEmitsSSEError` | G2b |
| **可观测事件与关联** | attempt 事件字段齐全、outcome 枚举三分支、终态关联同 request_id、心跳事件字段 | `TestCF524ObservationGuardEventAttemptFieldsAndFailoverSuccess`、`TestCF524ObservationGuardEventFailoverExhausted`、`TestCF524ObservationGuardEventClientGone`、`TestCF524ObservationTerminalSubsequentUpstreamError`、`TestCF524ObservationTerminalFailoverExhaustedFromError`、`TestCF524ObservationTerminalFailoverSuccessOnNilError`、`TestCF524ObservationResolvePendingAtRequestExit`、`TestCF524ObservationHeartbeatEventFieldsAndOnce` | G5 |
| **判别器字段 + 单点埋点** | 响应头到达记时刻（first-wins）；挂点 1/3 悬挂发事件、健康路径零事件；OpenAI 零事件 | `TestCF524ObservationUpstreamHeadersReceivedDiscriminator`、`TestCF524ObservationMount1HangEmitsGuardAndHeartbeatEvents`、`TestCF524ObservationMount1HealthyPathZeroEvents`、`TestCF524ObservationMount3HangEmitsGuardEvent`、`TestCF524ObservationOpenAIPathZeroEvents`、`TestCF524ObservationNilSafety` | G5 |

> 矩阵五行（§1.1 行 1-5）= 五挂点；行 1-4 为落点行，行 5 为零变化反向行。
> 63-victim 全形态（11 形）在行 1-4 内全部有对应用例名；行 5 由四项 OpenAI 反向用例钉死。

---

## 2. 生产验收唯一口径（上线后 48h 窗）

### 2.1 采集命令（同排查命令）

```bash
# 两条稳定事件计数（slog → 统一日志管线）
docker logs sub2api 2>&1 | grep -c 'gateway_first_byte_guard_triggered'
docker logs sub2api 2>&1 | grep -c 'gateway_upstream_heartbeat_started'

# 墙簇 499 计数（既有访问日志：component=http.access，status_code=499，latency_ms 分簇）
docker logs sub2api 2>&1 \
  | grep '"http request completed"' \
  | grep '"status_code":499' \
  | grep -E '"latency_ms":(12[45][0-9]{3}|119[0-9]{3}|120[0-9]{3}|121[0-9]{3})'
```

### 2.2 判别器（零新事件流）

四个非 OpenAI 挂点收到上游响应头时记录时刻，经**既有请求级完成/访问日志**新增字段
`upstream_headers_received_ms`（request_id 本在该日志链中）输出。墙簇 499 据此分两类：

- **主验收（必须为零）**：**无** `upstream_headers_received_ms` 字段的墙簇 499
  = header-wait 悬挂未被护栏拦截而撞墙；基线 63/48h + 6/48h 全部属此类。
- **body-hang 残余指标（单列报告，不判失败）**：**有**该字段的墙簇 499
  = 响应头已到、body 阶段悬挂（方案 §5 已登记未治理）。

### 2.3 判定（唯一）

- **124.9–125.1s 簇与 119.9–120.1s 簇的 499 命中数均为 0**（基线 63/48h + 6/48h），且命中的 499 中
  **无** `upstream_headers_received_ms` 字段；
- 每条 `gateway_first_byte_guard_triggered` attempt 事件能用 **同一 request_id** 串联到一条终态关联
  事件（`phase=terminal`）与访问日志（`http request completed` 同 request_id）；
  **访问日志缺同一 request_id 即判观测链断裂**（而非"无命中"）；
- 上线验收须含**一条可控超时请求的端到端日志串联实测**：guard 触发 → attempt → 终态 → 访问日志，
  四点同 request_id；且覆盖"guard 后下一 attempt 立即错误"分支（终态 `outcome=subsequent_upstream_error`）。

### 2.4 事件 schema（固化）

| 事件 | 字段 |
|---|---|
| `gateway_first_byte_guard_triggered` | `path` / `platform` / `stream` / `attempt` / `outcome` / `elapsed_ms` / `request_id` / `phase`（`attempt`\|`terminal`） |
| `gateway_upstream_heartbeat_started` | `path` / `platform` / `heartbeat_delay_ms` / `request_id` |

`outcome ∈ {failover_success, failover_exhausted, client_gone, subsequent_upstream_error}`（最后一项 =
guard 后下一 attempt 以非 failover 错误终结）。每次 guard 触发发一条 attempt 事件 + 一条终态关联事件
（同 request_id）。

---

## 3. 回滚兼容（纳入部署证据）

### 3.1 部署前实测（旧镜像 + 新 config）

- 用**上一版本镜像**（不含 CF524 代码）+ **新 `config.yaml`**（含 `gateway.upstream_first_byte_guard_seconds`
  / `gateway.upstream_heartbeat_delay_seconds`）实测启动：证明旧二进制容忍新键（viper 未知键行为以实测为准），
  进程正常起、健康检查通过、普通网关请求正常。

### 3.2 回滚步骤

1. 停止当前容器；保留现场日志（`docker logs` 归档）。
2. 恢复 `config.yaml` 为回滚前快照（移除/还原两个 CF524 键）；确认配置加载成功。
3. 回滚镜像到上一版本；启动并验证：
   - 健康检查通过；
   - 配置失败关闭验证：故意写入非法 `guard`（如 `0` 或 `120`）或非法 `heartbeat`（如 `90` ≥ guard）
     时，进程**拒绝启动/热加载整体拒绝**（零部分生效），当前生效配置不变；
   - 普通网关请求（`/v1/messages`）端到端正常，无 CF524 事件。

### 3.3 部署证据落盘

按 `docs/evidence-filing-standard.md` 落盘至 `/home/zjy/.sub2api-acceptance/<项目>-<日期>/`：
`00-deploy/`（镜像 digest、config.yaml、启动日志、健康检查）、
`99-final-report.md`（48h 窗 §2.3 判定原文 + 事件/499 计数原始输出 + 回滚演练记录）。
**证据未落盘 = 任务未完成。**

---

## 4. 完成口径

- `go build ./...` + `go vet ./internal/service/ ./internal/handler/` 绿；
- 定向单测（`-tags unit`）绿，§1 矩阵行 1-5 全部有对应用例名且通过；
- 本文件落盘且矩阵五行全有对应用例名；
- 生产验收（§2）与回滚兼容（§3）随部署证据落盘后的实测结论关闭。
