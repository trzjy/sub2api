# CF524 覆盖扩展方案 v1.0：OpenAIGateway 家族接入首字节护栏 + 流式心跳 + 观测

> 状态：v1.0 待方案审。2026-09-30 用户裁定立项：扩大 CF524 覆盖面至 OpenAIGateway
> 处理器家族，语义与主方案（docs/cf524-upstream-hang-mitigation-plan.md v8.3，已部署
> a099982c7）完全同源——D1 首字节护栏（硬边界 fail-closed）+ D2 流式心跳（可选优化，
> hb=0 合法）+ G5 观测三件套。本方案不引入第二套机制、第二套配置、第二套事件 schema。
> 生产实证（部署后 ~4h）：499@120.0s ×6、499@124.9s ×2、502@124.99s ×6+，全部走
> OpenAIGateway 家族（组件 handler.openai_gateway.*），guard 事件 0——防线未覆盖该家族。
> 证据：`~/.sub2api-acceptance/sub2api-cf524-deploy-20260930/99-final-report.md` §四。

## 1. 现状勘踏（代码坐实，2026-09-30 主会话）

### 1.1 路由与处理器家族
`routes/gateway.go:74` `isOpenAIResponsesCompatibleGatewayPlatform`：kimi/zhipu/deepseek/
minimax/codebuddy/other/openai/grok 全部路由至 `h.OpenAIGateway`。生产 524 主力流量
（deepseek-v4.1-flash、glm-5.3-flash 等）在此家族。

三个流式入口与换号循环（**循环都在 handler 层**）：

| 入口 | 位置 | 每账号尝试调用 | 写后禁 failover 判定 |
| --- | --- | --- | --- |
| ChatCompletions | handler/openai_chat_completions.go:24 | OpenAIGatewayService.ForwardAsChatCompletions（:259） | writerSizeBeforeForward（:252），消费于 :337/:386 |
| Responses | handler/openai_gateway_handler.go:388 | OpenAIGatewayService.Forward（:791） | OpenAICompactKeepaliveAdjustedWrittenSize（:780），消费于 :887/:956 |
| Messages | handler/openai_gateway_handler.go:1141 | OpenAIGatewayService.ForwardAsAnthropic（:1375 一带） | writerSizeBeforeForward（:1368），消费于 :1455 |

### 1.2 上游执行收口点
8 个直连 `doOpenAIUpstream` 调用（全部经此一个函数出网）：
cc_pipeline:243、cc_anthropic_native:120、cc:395、forward:1125（Responses/Messages 主体）、
messages_native:103、messages:442、passthrough:378、responses_native:125；raw 路径经
sendCCUpstreamRequest（→doOpenAIUpstream）。**guard 收口在 doOpenAIUpstream 一处即覆盖
全部 8+1 执行点。**

`doOpenAIUpstreamNoWatchdog`（plugin_transport.go:76）调用方：codebuddy_gateway_forward
（自有 forward，主方案 L221 已登记残余风险）、alpha_search、embeddings、count_tokens
（非流式/低危）。本方案不改 NoWatchdog 变体；codebuddy 维持残余风险登记不变。

### 1.3 既有机制（扩展必须共存的三个事实源）
1. **CN 首包 watchdog**（openai_plugin_transport.go:25-75）：kimi/deepseek/zhipu/minimax
   （isCNFamilyFastpathAccount，openai_account_runtime_block_fastpath.go:99）出站加 60s
   首包超时 → 内部超时错误 → 既有 504 冷却 + 换号。**每账号独立 60s、无共享预算**：
   两个账号先后悬挂 = 120s = CF 墙（生产 120.0s 簇的成因）；非 CN fastpath
   （openai/grok/codebuddy/other）无任何首包超时（124.9s/125s 簇成因）。
2. **compact SSE keepalive**（openai_compact_sse_keepalive.go，#3887）：Responses handler
   入口 :454 对 body-signal compact 请求启动，包装 c.Writer，interval 注释行；排除字节用
   `OpenAICompactKeepaliveAdjustedWrittenSize`（:173，无语义字节时归一为 **-1** 哨兵）。
3. **OpenAI 家族既有写后禁 failover 防线**：CC/Messages 用裸 `c.Writer.Size()` 对比快照；
   Responses 用 compact 调整后尺寸。三种口径并存。

## 2. 方案语义（与主方案逐条同源，零新增机制）

- **D1 护栏**：请求入口一次捕获不可变 `RequestBudgetSnapshot`（复用
  `service.NewRequestBudgetSnapshot`，同两配置键、同 [30,90] fail-closed 边界、同
  TotalBudget 公式）；每账号尝试窗口 = `snapshot.AttemptWindow(now)`（剩余预算，**换号
  不重置**）；剩余 < 5s 最小可行窗口 → ShouldRetryNextAccount=false → 既有
  FailoverExhausted 路径。护栏取消边界仅覆盖响应头等待。
- **D2 心跳**：复用 `service.UpstreamHeartbeat`（同 `: keep-alive\n\n` 注释帧，SSE 注释层
  对 OpenAI/Anthropic 两种客户端解析器均透明）；请求级 owner 在 handler 入口安装，
  仅 clientStream 且 hb>0 时；每账号尝试前 Resume、响应头到达/错误/护栏超时后
  stop-and-wait（单一 writer 所有权，先停拍再交 writer）。
- **G5 观测**：复用 `cf524_observation.go` 全套（同三事件、同 outcome 枚举、同 request_id
  串联、同 `upstream_headers_received_ms` 判别器经既有完成日志）。平台字段用既有
  `platform` 参数（account.Platform）。**零新事件、零新 schema。**
- **配置**：同两键同源（config.go SetDefault guard=90/hb=15），零新增配置。
- **晚到契约**：沿用已授权变更——心跳启动后迟到 2xx/4xx/5xx → SSE 200 + error 帧。

## 3. 扩展设计

### 3.1 安装点（G2b 等价，3 处）
`ChatCompletions`/`Responses`/`Messages` 三入口在认证后、选号循环前调用新的
`h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, requestStart, reqStream)`（包装既有
`cf524InstallUpstreamBudgetAndHeartbeat` 语义；**安装条件与主方案逐字相同**：guardSeconds<=0
不安装、非流式只装快照、Flush 缺失失败关闭记 warn）。

**compact 互斥（本方案唯一新增判定，必须外审挑战）**：Responses 入口已启动 compact
keepalive（:454）的请求（`openAICompactClientWantsStream(c)` 且 compact 标记）**不再安装
心跳 owner**（快照仍装，护栏仍生效）——同一请求同一时刻只允许一个拍频机制写 writer，
避免双 ticker 数据竞争/字节交错。compact 请求的 keepalive 是已验收机制（#3887），
不替换、不吸收（旧链归零义务不触发：其职责是 compact unary 等待，与本方案 header-wait
窗口不重叠）。

### 3.2 消费点（收口于 doOpenAIUpstream 一处）
`doOpenAIUpstream` 重命名内层实现，新外层包装（新文件
`service/openai_upstream_guard.go`）按序执行：
1. 从 ctx 取快照与 hb owner（`RequestBudgetSnapshotFromContext` /
   `UpstreamHeartbeatFromContext`）；**无快照即原样透传内层实现**（未安装快照的调用方
   ——embeddings/count_tokens/alpha_search/全部测试桩——零行为变化，无兜底分支）。
2. `window := snapshot.AttemptWindow(now)` 构建护栏（同
   `newUpstreamFirstByteGuard`，reqCtx 写入请求 context）；剩余 <5s 不再发起新尝试、
   直接返回既有 FailoverExhausted 语义错误（由 handler 循环既有消费）。
3. 心跳 Resume（owner 存在且 clientStream；失败仅 warn，同主方案）。
4. 调内层实现。
5. 三分支与主方案 cf524ExecuteUpstreamWithGuard 逐条对应：护栏超时 → 停表+心跳
   stop-and-wait → 返回护栏超时错误（handler 层映射进既有 UpstreamFailoverError 换号
   分类，**handler 循环零新增机器**）；传输错误 → 停表+停拍上抛；响应头到达 → 停表 +
   `OnUpstreamHeaderArrived`+`Stop`（晚到契约三分支顺序）→ 返回 resp。
6. G5 观测单点：包装函数内发 attempt/heartbeat/headers 三类（obs 结构复用，
   `ctx` 必须是**未被护栏包裹的原始请求 context**）。

**CN watchdog 共存**：watchdog 在内层实现内部、语义不变（60s 首包 + 504 冷却分类）；
护栏在外层，窗口取剩余预算。二者取消竞争时先到先裁决：watchdog 先 fired → 走既有
内部超时分类（护栏 TimedOut()=false 不介入）；护栏先 fired → reqCtx 取消使 Do 返回，
watchdog 未 fired 不分类。**两机制不改一行，只定义先后裁决顺序。**

### 3.3 写后禁 failover 防线统一（G5c/G5d 口径移植，本方案最高风险点）
三入口既有防线判定改为与 gateway_handler.go `heartbeatFailoverGuardStillClean` 完全
同一函数（提出到 service 层共享或包内可见的同型实现，**判定逻辑唯一**）：
- 基线增量严格相等：捕获 `writerSizeBeforeForward` **同时点**捕获
  `hbCommittedBaseline`（owner 存在时 `CommittedBytes()`，否则 0）；
- 双侧 `normalizeSize(<0→0)`（gin noWritten 哨兵，G5d）；
- 无心跳分支退化 `size == before`（与现状逐一等价，无第二分支）。
- **Responses 路径的 `OpenAICompactKeepaliveAdjustedWrittenSize` 口径冲突必须显式处理**：
  该函数无语义字节时返回 -1（与 G5d 归一为 0 的约定不同源）。统一规则：调用方先把
  其返回值过同一 `normalizeSize(<0→0)` 再进比较（-1 与 0 在"无语义字节"语义上等价，
  归一后比较结果不变）；compact keepalive 字节继续由该函数扣除，心跳字节由
  CommittedBytes 基线增量扣除，**两个扣除来源在比较中各自独立、不得合并计数**。
- CC :337、Messages :1455 的裸 `Size() != before` 消费点同批改造；改造前后对无心跳
  请求逐一等价（回归用例钉死）。

### 3.4 观测与验收
- 事件、字段、判别器与主方案完全一致；验收卡沿用
  docs/cf524-acceptance-checklist.md §2 **修正后**命令（正则容忍冒号后空格，
  407fe204c）；新增 OpenAIGateway 家族挂载矩阵章节（3 入口 × 形态）。
- 生产判收不变：墙簇 499 两簇（124.9-125.1s / 119.9-120.1s）= 0；每条 guard 事件
  request_id 可追终态。

## 4. 踩坑清单（历次外审实证，本方案硬约束——执行/复审逐条对照）

| # | 坑（出处） | 硬约束 |
| --- | --- | --- |
| 1 | 防线基线重复计入前置心跳字节（G5c P1） | 基线与 writerSizeBeforeForward **同时点**捕获；增量严格相等 |
| 2 | gin -1 哨兵翻转在快照外（G5d P1） | 双侧 normalizeSize(<0→0)；涉及 Size 算术处逐处核对翻转时序 |
| 3 | 哨兵"自动抵消"想当然（G5c 回归引入） | 任何 Size 结论必须对照 gin v1.9.1 response_writer 源码，禁口头推断 |
| 4 | Forward 返回值未命名导致 defer 读不到 err（轮1 驳回项反证） | 新增包装函数一律命名返回值 |
| 5 | 观测 ctx 误用护栏 reqCtx → client_gone 误判（主方案设计约束） | obs.ctx 用原始请求 context |
| 6 | 事件多点发射/测试导出污染（G5b） | 单点发射（3.2 包装函数内）；tracker gin-context 域、不导出；seam 测试走组合论证 |
| 7 | 方案与代码口径漂移 3 处（v8.3 回写） | 任何语义修同一边界内先回写本方案再改码；外审对照以方案为准 |
| 8 | 外审建议夹带新兜底机制（轮1 abort 建议） | 心跳可选（hb=0 合法）、护栏硬边界 fail-closed、零降级分支；复审建议暗含兜底一律拒绝并记录依据 |
| 9 | 外审 packet 缺实现 diff 致审无效（轮1 首审作废） | 提交后审用 `--scope task-snapshot --task-snapshot-file <全量 diff>`；审前核对 packet 含全部差异 |
| 10 | 复审者事实错误（轮1 finding3） | 采纳前逐条对照源码验证；驳回须给闭合依据 |
| 11 | 验收采集正则假 0（407fe204c） | 采集命令先对真实生产日志格式核验再进验收卡 |
| 12 | compact keepalive 归一 -1 与 G5d 归一 0 约定不同源 | 3.3 统一规则：消费方先归一再比较，两个扣除来源独立 |
| 13 | 双拍频机制同 writer 数据竞争风险 | compact 互斥（3.1）：同请求同时刻仅一个拍频机制 |
| 14 | 双取消机制（watchdog/护栏）错误分类互相覆盖（对照 plugin_transport 现注释） | 3.2 先后裁决顺序钉死；护栏 TimedOut 只在自身 fired 时介入 |
| 15 | 执行环境：幻影依赖/执行中断/DONE 标记误报 | 派发单含验证白名单；验收查 import↔声明对照；监控只认当前卡标记；429/520 断点续派须场景声明 |
| 16 | 全量测试滥用/漏跑 | 每卡定向测试+build+vet 白名单；收敛对账（共享文件并行后）/高风险边界验收各跑一次全量 |

## 5. 文件边界与派发拆分预案（执行期出正式派发单）

预计边界（执行派发按文件+测试对，≤5 并行）：
- P1 service 层：`openai_upstream_guard.go`（新，包装+护栏+心跳消费+观测单点）+
  `openai_plugin_transport.go`（内层改名）+ `upstream_first_byte_guard.go`（如需导出
  判定共享）——**单卡串行先行**（P2/P3 依赖其符号）；
- P2 三个 handler 安装点+防线统一：`openai_chat_completions.go`、
  `openai_gateway_handler.go`（Responses+Messages 两入口同文件，同卡）、共享判定函数
  落点——P2a（CC）/P2b（gateway_handler 双入口）可并行（无共享文件）；
- P3 观测复用接线：`cf524_observation.go` 如需 platform 泛化微调 + middleware 判别器
  （预计零改动，核实即闭环）；
- P4 测试对：每卡自带定向测试（含哨兵/基线/互斥/等价回归），e2e seam 用组合论证。

不在边界内（零改动）：`upstream_heartbeat.go`、`cf524_observation.go`（如无需泛化）、
`codebuddy_gateway_forward.go`、`openai_gateway_passthrough.go`（其 doOpenAIUpstream
调用经收口自动获得护栏——需在 P1 验证 passthrough 心跳 owner 缺失时仅护栏生效、
行为兼容）、openai compact keepalive、消费机器 FailoverState。

## 6. 验收与回滚
- 回滚兼容：新配置零新增（同两键），旧镜像回退无配置障碍（同主方案实测结论）。
- 行为兼容硬指标：未装快照的既有调用方与测试桩零行为变化（P1 卡回归用例钉死）；
  三入口非流式/无心跳请求防线判定与现状逐一等价。
- 48h 生产观察窗口径同主方案（墙簇两簇=0），扩展上线后重开窗口。
