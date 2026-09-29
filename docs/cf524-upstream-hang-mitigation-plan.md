# CF 524 上游悬挂治理方案 v2（首字节护栏 + 流式心跳保活）

> 状态：方案审 delta 复核 7 项全采纳回修，待派发。2026-09-30 用户裁定：采用方案 A
> （心跳+护栏+failover，含流式晚到错误统一 SSE error 帧的契约变更授权）。
> 排查证据：`~/.sub2api-acceptance/sub2api-cf524-diagnosis-20260930/`。
> v1→v2：方案审 R1 5 项 + delta 复核 7 项全部采纳落地。

## 0. 问题定义（证据收敛）

- 生产 48h：63 个请求被 Cloudflare 在 ~125.0s 掐断（应用侧 status=499，时延密集簇
  124.93–125.10s，另有 120.0s 簇 6 条）；54 个 stream:true、9 个 stream:false；
  20 个独立 IP。账号分布：138(kimi-k3) 37 次/59%，其余 kimi/zhipu/deepseek 各
  1-4 次，anthropic 133（claude-sonnet-5）1 次。
- 根因链：上游（第三方中转为主）收请求后长时间不返回响应头 → 网关无限等待 →
  CF 125s 断开 → 用户侧 524。
- 代码根因（http_upstream.go:940-955）：OpenAI profile transport
  responseHeaderTimeout=0（配置未设）；通用默认 300s 也 > CF 墙。
- 既有机制基座：`openAIFirstOutputHeaderGuard`（openai_gateway_forward.go:1089，
  生效条件钉死 reqStream && PlatformOpenAI）；`UpstreamFailoverError.
  SafeToFailoverAfterWrite`（gateway_service.go:696）；handler 侧
  maxOpenAIFirstOutputTimeoutSwitches=1；`handleStreamingAwareError(streamStarted)`
  已支持写出后以 SSE error 帧传达错误。

## 1. 权威与边界

- 用户裁定链：2026-09-30 否决纯配置方案（误伤）→ 选定代码方案 → delta 复核后
  **明确选定方案 A**：保留流式心跳；授权其契约变更——心跳启动后上游晚到
  2xx/4xx/5xx 一律以 SSE 200 + 语义帧/error 帧传达，不再保持原始状态码
  （合同 §1 用户决定 > 其他，本行即授权记录）。
- 语义边界：只约束"上游响应头等待"阶段；上游首字节之后的一切（流式转发、
  聚合、计费）零改动。非流式客户端的 CF 墙 origin 侧不可突破，收敛为
  "墙内主动 failover/报错"。
- 禁兜底（合同 §5）：护栏触发必须显式错误 + failover/错误响应；guard 键无
  禁用路径（0 与越界一律失败关闭），不得回退无护栏旧行为。

## 2. 设计

### D1 通用首字节护栏（泛化 headerGuard，含 CF 墙内总预算）

- 把 `openAIFirstOutputHeaderGuard` 泛化为共享组件，应用到全部无护栏的客户端
  可见上游 Post 点（五挂点，方案审 P1 闭合）：
  1. `gateway_forward.go:391`（Forward 主路径 /v1/messages，anthropic OAuth
     + 各平台，retry 循环内 DoWithTLS——claude-sonnet-5 受害形态在此）
  2. `gateway_anthropic_passthrough.go:107`（API-key 直通 DoWithTLS）
  3. `gateway_forward_as_chat_completions.go`（CC→Anthropic 链路）
  4. `gateway_forward_as_responses.go`（/v1/responses 非 OpenAI 平台）
  5. `openai_gateway_forward.go`（OpenAI 原生既有 headerGuard——G1 迁移共享
     组件后改调共享实现，语义零变化）
- **请求级总预算（delta 复核 P1-1/-2 闭合）**：进入 forward 层时设绝对截止：
  - 流式客户端（有心跳，CF 墙被心跳中和）：绝对截止 = 进入 + 2×guard + 10s
    写回余量（默认 190s）；
  - 非流式客户端（无心跳，受 CF 墙约束）：绝对截止 = 进入 + guard + 25s
    （默认 115s < 120s 双墙）；每次尝试的 guard 窗口 = min(guard, 剩余预算)，
    换号不重置绝对截止。
- 生效条件：上游响应头等待超过当次窗口 → failover 错误（复用
  newOpenAIFirstOutputTimeoutError 模式，SafeToFailoverAfterWrite=true）。
- 换号上限：沿用 maxOpenAIFirstOutputTimeoutSwitches=1；耗尽后：stream →
  SSE error 帧；非流式 → 502 JSON。
- **配置键**：`gateway.upstream_first_byte_guard_seconds`，默认 90。
  **Validate() 只接受 [30,90]**（上限 90 = 非流式预算 115s 留足换号+写回
  余量，且任何合法值都在 CF 墙内收敛；0 与越界失败关闭，无旧行为回退）。
  生产 config.yaml 显式写 90。
- 不做 transport 层全局/Profile 超时下调；护栏是请求级 context deadline，
  仅作用于上述五挂点。

### D2 流式心跳保活（方案 A，授权契约变更）

- **适用范围（delta 复核 P1-3 闭合）**：仅限 D1 新挂的四个非 OpenAI 平台
  路径（挂点 1-4）；OpenAI 原生路径（挂点 5）明确不创建心跳 writer，
  行为零变化（反向测试钉死）。
- 触发条件：`clientStream == true` 且上游响应头等待超过 heartbeat_delay。
- 行为：写 SSE 200 响应头 + 周期 `: keep-alive` 注释帧（15s 间隔）；上游
  响应头到达即停。
- **晚到响应契约（用户已授权）**：心跳启动后，上游晚到 2xx 正常转语义流；
  晚到 4xx/5xx 一律 SSE error 帧（经既有 handleStreamingAwareError(true)
  通道）。验收按晚到 2xx/4xx/5xx 三分支分别测试。
- **状态机唯一顺序（delta 复核 P1-5 闭合）**：
  1. guard 超时且换号额度未耗尽 → 置 SafeToFailoverAfterWrite → 同步停止
     心跳（stop-and-wait）→ 换号；
  2. 换号额度耗尽 → handleStreamingAwareError(true)（SSE error 帧）；
  3. 非 failover 类晚到错误 → 直接 SSE error 帧。
  分支 1→2 与分支 3 分别有测试钉死顺序。
- **writer 所有权（单一所有者，硬约束）**：心跳与真实流式转发不得并发持写
  ResponseWriter；进入真实转发、failover 或错误通道前必须同步 stop-and-wait
  （停 tick、等心跳 goroutine 退出后才交接）。竞态测试覆盖：上游响应与
  tick 同至、guard 超时与 tick 同至、客户端断开时心跳退出。
- 配置键：`gateway.upstream_heartbeat_delay_seconds`，默认 15，0=禁用
  （纯优化可关闭，与护栏的不可禁用区分），Validate [0]∪[5,60]。

### D3 明确不做（防过度设计）

- 不改 transport 全局/Profile 级 responseHeaderTimeout 默认值。
- 不做心跳间隔自适应、不调换号上限、不为 codebuddy 平台单开特化
  （codebuddy 走自家 forward，不在五挂点覆盖面则如实登记为残余风险）。
- 不动计费：header 阶段超时 = 上游未产出语义输出，与既有 first-output-timeout
  计费口径一致。

## 3. 派发拆分（文件边界，最大并发 ≤5）

| 卡 | 文件 | 内容 | 依赖 |
|---|---|---|---|
| G1 | internal/service/{openai_first_output_timeout.go, openai_gateway_forward.go, gateway_forward_as_chat_completions.go} | headerGuard 泛化为共享组件（**旧链归零（合同 §4）：登记旧 openAIFirstOutputHeaderGuard 入口与全部调用点 → 迁移共享实现 → 删除被替代旧类型/构造器 → 仓库级符号残留扫描 + 定向测试证明无残留**）+ 挂点 3 + 请求级总预算（流式 2×guard+10s / 非流式 guard+25s）+ 单测 | 方案 v2 定稿 |
| G2 | internal/service/{gateway_forward.go, gateway_anthropic_passthrough.go, gateway_forward_as_responses.go} | 挂点 1/2/4 接护栏 + failover 接线 + 单测（含 claude-sonnet-5 两形态回放用例） | G1 |
| G3 | internal/service/（心跳 writer 组件）+ 挂点 1-4 接入 | D2 心跳（晚触发 + stop-and-wait 单一 writer 所有权 + 状态机三分支顺序 + 三交叠竞态 + 晚到 2xx/4xx/5xx 三分支 + OpenAI 路径反向测试）+ 单测 | G1 |
| G4 | internal/config/config.go + config.example.yaml + 生产 config.yaml 变更单 | 两个新键 + Validate（guard [30,90] 失败关闭无禁用路径；heartbeat [0]∪[5,60]）+ 文档注释 | 方案 v2 定稿 |
| G5 | docs/ + 验收卡 | 覆盖矩阵（五挂点 × 63-victim 全形态含 anthropic 两形态）+ 生产观测：稳定事件 `gateway_first_byte_guard_triggered` / `gateway_upstream_heartbeat_started`，字段 path/platform/stream/attempt/outcome/elapsed_ms，request_id 可串联 guard→换号→最终结果 | 全部 |

- 并行性：G1/G4 无共享文件 → 立即并行；G2/G3 依赖 G1 组件签名，G1 交付后
  并行；G5 收尾串行。
- 验证白名单（每卡）：`go build` + `go test -tags unit -run <定向>` + `go vet`；
  收敛后算力机 service+handler 包全量一轮（高风险边界 → 全量触发）。

## 4. 验收（Done when，单一完成口径）

1. **63-victim 全形态回放**：kimi/deepseek/zhipu 三平台 × 三路径 + **anthropic
   两形态（OAuth 主路径 gateway_forward.go:391 / API-key 直通
   gateway_anthropic_passthrough.go:107）**：stream 客户端收到 SSE 200+心跳、
   guard 触发后按状态机换号或 SSE error 帧；非流式在墙内预算（guard+25s）
   收到 502 JSON。无执行期 BLOCKED 例外（delta 复核 P1-6 删除逃生口）。
2. 健康路径零回归：上游正常响应（含 4xx/5xx 快速返回）时响应头/状态码/错误
   透传与现状 bit 级一致（心跳未触发、护栏未触发）。
3. OpenAI 原生路径行为零变化（含"OpenAI 请求不创建心跳 writer"反向测试）。
4. 晚到三分支：心跳启动后晚到 2xx→语义流、4xx/5xx→SSE error 帧，各有测试。
5. 状态机顺序：guard→换号（心跳 stop-and-wait 先于换号）→耗尽→error 帧，
   分支测试钉死；三交叠竞态测试通过。
6. 配置：guard [30,90] 外（含 0）拒绝启动/热加载；heartbeat [0]∪[5,60]。
7. **生产验收唯一口径（delta 复核 P2 闭合，替换 v1 的"slog 存在即完成"）**：
   上线后 48h 观察窗，`docker logs sub2api | grep` 两事件名 + 应用访问日志
   499 簇统计（与排查同命令），判定条件 = **124.9–125.1s 的 499 簇命中数为 0**
   （基线：63/48h）且每条 guard 触发事件能用 request_id 串联到换号成功或
   耗尽的最终结果。生产执行为上线后动作，随部署证据落盘。

## 5. 残余风险（如实登记）

- 非流式客户端 CF 墙不可消除：>guard 的真实慢上游被误伤一轮 failover，耗尽后
  客户端收到可重试错误而非 524。误伤率由 90s 阈值与 48h 数据（victim 中位
  悬挂 ≥124s）背书，上线后按 guard 触发计数复核。
- 第三方中转真实 TTFB 分布无历史数据，90s 初值上线后按观测调整（只能下调
  不了了之是不允许的：调整走配置热加载，仍受 [30,90] 约束）。
- codebuddy 平台不在五挂点覆盖面内；若其 forward 存在同形态悬挂，属残余风险
  如实登记，不在本方案范围（防过度设计）。
