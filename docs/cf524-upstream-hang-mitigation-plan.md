# CF 524 上游悬挂治理方案（首字节护栏泛化 + 流式心跳保活）

> 状态：草案待外审（方案审）。2026-09-30 用户裁定：否决纯配置层方案（误伤风险），
> 按代码层方案走七步工作流。排查证据：`~/.sub2api-acceptance/sub2api-cf524-diagnosis-20260930/`。

## 0. 问题定义（证据收敛）

- 生产 48h：63 个请求被 Cloudflare 在 ~125.0s 掐断（应用侧 status=499，时延密集簇
  124.93–125.10s）；54 个 stream:true、9 个 stream:false；波及 20 个独立 IP；
  路径覆盖 /v1/chat/completions、/v1/messages、/v1/responses。
- 根因链：上游（第三方中转为主）收请求后长时间不返回响应头 → 网关无限等待 →
  CF 125s 断开 → 用户侧 524。
- 代码根因（http_upstream.go:940-955）：OpenAI profile 的 transport
  responseHeaderTimeout=0（配置未设，无护栏）；通用默认 300s 也 > CF 125s。
- 既有机制（本方案的复用基座）：`openai_first_output_timeout_seconds` 的
  headerGuard（openai_gateway_forward.go:1089-1140）——但生效条件钉死为
  `reqStream && account.Platform == PlatformOpenAI`，受害者平台
  （kimi/deepseek/zhipu/anthropic）全部不覆盖。
- failover 语义基座已存在：`UpstreamFailoverError.SafeToFailoverAfterWrite`
  （gateway_service.go:696，"仅写出 SSE 注释等非语义字节时仍可换号"）+
  handler 侧 maxOpenAIFirstOutputTimeoutSwitches=1 换号上限 +
  `handleStreamingAwareError(streamStarted=true)` 已支持写出后以 SSE error 帧
  传达错误。

## 1. 权威与边界

- 权威来源：用户 2026-09-30 裁定（代码层方案 2，禁止纯配置层方案 1）+ 本方案文档。
- 语义边界：本方案只约束"上游响应头等待"阶段的行为；上游首字节之后的一切
  （流式转发、聚合、计费）零改动。非流式客户端的 CF 125s 墙在 origin 侧不可
  突破（响应未聚合完成前无字节可发），只能收敛为"在墙内主动失败/failover"。
- 禁兜底（合同 §5）：护栏触发必须是显式错误 + failover/错误响应，禁止静默
  重试计数掩盖、禁止降级为无护栏。

## 2. 设计

### D1 通用首字节护栏（泛化 headerGuard）

- 把 `openAIFirstOutputHeaderGuard` 从 openai_gateway_forward.go 泛化为共享
  组件，应用到全部无护栏的客户端可见上游 Post 点（方案审 P1 闭合，2026-09-30
  代码定位完成，63-victim 全形态在列）：
  1. `gateway_forward.go:391`（Forward 主路径 /v1/messages，anthropic OAuth
     + 各平台，retry 循环内 DoWithTLS——claude-sonnet-5 受害形态在此）
  2. `gateway_anthropic_passthrough.go:107`（API-key 直通 DoWithTLS）
  3. `gateway_forward_as_chat_completions.go`（CC→Anthropic 链路，
     kimi/deepseek/zhipu chat_completions 受害形态）
  4. `gateway_forward_as_responses.go`（/v1/responses 非 OpenAI 平台）
  5. `openai_gateway_forward.go`（OpenAI 原生，既有 headerGuard——G1 迁移为
     共享组件后此处改调共享实现，语义零变化）
- 生效条件：上游响应头等待超过 guard 值 → 构造 failover 错误
  （模式复用 newOpenAIFirstOutputTimeoutError：Stage/Reason 按既有枚举，
  SafeToFailoverAfterWrite=true；stream 客户端换号续流，非流式客户端换号重试）。
- 换号上限复用 maxOpenAIFirstOutputTimeoutSwitches=1 语义（每种客户端形态各
  1 次额外机会）；耗尽后：stream → SSE error 帧（既有通道）；非流式 → 502
  JSON（在 CF 墙内主动失败，客户端可重试）。
- **配置键（新增，不改既有键语义）**：`gateway.upstream_first_byte_guard_seconds`，
  默认 90（< CF ~120s 双墙，从 48h 数据看 >110s 的悬挂 100% 被杀，90s 给
  failover 留出一轮余量）。**Validate() 只接受 [30,600]，0 与越界值一律失败
  关闭**（方案审 P2 采纳：护栏是墙内收敛机制，禁止 0=禁用回退到无护栏旧行为；
  误配置/热加载不得重新暴露 524 根因）。生产 config.yaml 部署时显式写 90。
- **不做 transport 层全局 300s 的下调**（那是方案 1 的误伤面）；护栏是请求级
  context deadline，仅作用于上述五个 forward 挂点。

### D2 流式心跳保活（早 flush + comment 帧，晚触发）

- 触发条件（全部满足才启动，保证健康路径零改动）：
  `clientStream == true` 且上游响应头等待已超过 `heartbeat_delay`（新键
  `gateway.upstream_heartbeat_delay_seconds`，默认 15，0=禁用，Validate
  [0]∪[5,60]）。
- 行为：向客户端写 SSE 200 响应头 + 周期性 `: keep-alive` 注释帧（间隔 15s）；
  一旦上游响应头到达，停止心跳并走既有流式转发（后续字节为真实语义流）。
- 错误语义承接：心跳已启动后再失败，一律走 `handleStreamingAwareError`
  streamStarted=true 通道（SSE error 帧），不再尝试改写 HTTP status——该通道
  已存在且有测试，非新增机制。
- failover 兼容：心跳字节均为 SSE 注释（非语义），换号合法性由既有
  SafeToFailoverAfterWrite 判定，无新机制。
- **writer 所有权（方案审 P2 采纳，G3 实现与验收硬约束）**：单一写入所有者
  ——心跳与真实流式转发不得并发持写 ResponseWriter；进入真实转发、failover
  或错误通道前必须同步 stop-and-wait（停止 tick、等待心跳 goroutine 退出后
  才交接 writer）。竞态测试必须覆盖：上游响应与 tick 同时到达、guard 超时与
  tick 同时到达、客户端断开时心跳退出三个交叠场景。
- **非流式客户端明确不受益**（无字节可提前写）：靠 D1 护栏在墙内主动失败。
  本方案不承诺消除非流式 524，只把"挂 125s 后 CF 杀"收敛为"≤90s 主动
  failover/报错"，该边界写入验收文档。

### D3 明确不做（防过度设计）

- 不改 transport 全局/Profile 级 responseHeaderTimeout 默认值。
- 不做心跳帧间隔自适应、不做多轮 failover 上限调整、不做 codebuddy 平台
  特化（codebuddy 走自家 forward，本期护栏仅挂通用链路；codebuddy 受益与否
  由 D1 挂点覆盖面决定，不单独开卡）。
- 不动计费：header 阶段超时 = 上游未产出语义输出，与既有
  first-output-timeout 的计费口径一致。

## 3. 派发拆分（文件边界，最大并发 ≤5）

| 卡 | 文件 | 内容 | 依赖 |
|---|---|---|---|
| G1 | internal/service/{openai_first_output_timeout.go, openai_gateway_forward.go, gateway_forward_as_chat_completions.go} | headerGuard 泛化为共享组件（**旧链归零（合同 §4，方案审 P2 采纳）：登记旧 openAIFirstOutputHeaderGuard 入口与全部调用点 → 迁移至共享实现 → 删除被替代旧类型/构造器 → 仓库级符号残留扫描 + 定向测试证明无残留**）+ ForwardAsChatCompletions 挂点 + 单测 | 方案审通过 |
| G2 | internal/service/{gateway_forward.go, gateway_anthropic_passthrough.go, gateway_forward_as_responses.go} | 覆盖矩阵 1/2/4 挂点接 D1 护栏 + failover 接线 + 单测 | G1 |
| G3 | internal/service/（心跳 writer 组件）+ 三路径接入 | D2 心跳（晚触发 + stop-and-wait writer 所有权 + SSE error 承接 + 三交叠竞态测试）+ 单测 | G1（共享组件） |
| G4 | internal/config/config.go + config.example.yaml + 生产 config.yaml 变更单 | 两个新键 + Validate（guard [30,600] 失败关闭；heartbeat [0]∪[5,60]）+ 文档注释 | 方案审通过 |
| G5 | docs/ + 验收卡 | 路径覆盖矩阵 × 63-victim 形态对照 + 生产观测（方案审 P2 采纳）：稳定事件名 `gateway_first_byte_guard_triggered` / `gateway_upstream_heartbeat_started`，字段 path/platform/stream/attempt/outcome/elapsed_ms；上线验收以「124.9–125.1s 的 499 簇消失 + guard 触发→换号成功或耗尽链路可判定」为准，观察窗口 48h | 全部 |

- 并行性：G1/G4 无共享状态可并行；G2/G3 依赖 G1 组件签名，G1 交付后并行；
  G5 收尾串行。
- 验证白名单（每卡）：`go build` + `go test -tags unit -run <定向>` + `go vet`；
  收敛后在算力机跑一轮 service+handler 包全量（高风险边界：调度/资金邻接 →
  全量触发，用户已允许算力机跑全量）。

## 4. 验收（Done when）

1. 63-victim 形态回放：对 kimi/deepseek/zhipu 平台的三路径，stream 客户端在
   上游悬挂时收到 SSE 200+心跳、guard 触发后换号或 SSE error 帧；非流式在
   ≤90s+一轮 failover 内收到 502 JSON（无 524 暴露）。
2. 健康路径零回归：上游正常响应（含 4xx/5xx 快速返回）时，响应头/状态码/
   错误透传与现状 bit 级一致（心跳未触发、护栏未触发）。
3. OpenAI 平台原生路径（既有 first-output-timeout）行为零变化。
4. 配置：guard 键 Validate 失败关闭（0 与 [30,600] 外一律拒绝启动/热加载，
   无旧行为回退路径）；heartbeat 键 0=禁用（纯优化，允许关闭）。
5. 单测覆盖：guard 触发/未触发、心跳启动/停止、failover 接线、SSE error 帧、
   非流式 502、Validate 边界。
6. 生产观测：guard 触发与心跳启动各有结构化 slog 计数（复用 §0.1 #8 风格）。

## 5. 残余风险（如实登记）

- 非流式客户端 CF 墙不可消除：>90s 真实慢上游（如深度推理非流式）会被护栏
  误伤一轮 failover——failover 后仍慢则客户端收到可重试错误而非 524。误伤率
  由 90s 阈值与 48h 数据（victim 中位悬挂 ≥124s）背书，上线后按 guard 触发
  计数复核。
- 第三方中转上游的真实 TTFB 分布无历史数据，90s 初值上线后按观测调整。
- Anthropic OAuth 直通路径（claude-sonnet-5 victim 形态）若挂点覆盖不到，
  G2 卡 BLOCKED 上报，不得静默跳过。
