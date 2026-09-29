# 派发单 G3：流式心跳保活组件（方案 A）——仅组件交付，不做挂点接入

权威方案：`docs/cf524-upstream-hang-mitigation-plan.md` **v8.2（以仓库最新版为准）** §2 D2。前置：G1 已交付共享护栏组件。**用户已授权契约变更**：心跳启动后上游晚到 2xx/4xx/5xx 一律 SSE 200 + 语义帧/error 帧。

## 硬约束
- **只新建 `internal/service/upstream_heartbeat.go` + `upstream_heartbeat_test.go`，不做任何挂点接入**（挂点 1-4 文件唯一所有者 = G2，本卡禁改 `gateway_forward*.go`/`gateway_anthropic_passthrough.go`）。禁止 git 写操作；禁止全量测试。
- 验证白名单：`go build ./...`、`go vet ./internal/service/`、`go test -tags unit -run <定向> ./internal/service/`、定向 `-race`。

## 任务（组件级，供 G2 接线）
1. **心跳 writer 组件**：接口为"客户端流式 + gin ResponseWriter + delay 配置"；`Start` 后 delay 到点写 SSE 200 响应头 + 15s 间隔 `: keep-alive` 注释帧；`Stop` 语义 = 上游响应头到达即停。**计时起点 = 请求入口单调时间（v6，覆盖认证/排队前置期）**；owner 为请求级（不随 attempt 重建）；**delay 值取请求入口配置快照（v7，热加载不影响进行中请求）**。
2. **Flush 契约（v3 新增，硬性）**：初始响应头与每个心跳帧写入后必须显式 `Flush()`（http.Flusher；gin ResponseWriter 支持）；组件检测不到 Flusher 能力时**失败关闭**——返回错误拒绝启用心跳并上抛，不静默降级为无心跳。
3. **单一 writer 所有权（硬约束）**：组件提供同步 stop-and-wait（停 tick、等 goroutine 退出、返回后调用方才可写 ResponseWriter）；心跳与真实转发/failover/错误通道不得并发持写。
4. **状态机原语 + 唯一线性化优先级（v4 硬性）**：组件内部用原子状态实现，优先级固定——① 上游响应头事件 > 心跳启动（临界点晚到 4xx/5xx：响应事件先到则保留原始状态码语义、心跳不再启动；tick 先到则按晚到契约交 error 帧通道）；② guard 终态（调用方已声明换号/耗尽）> tick 写出（终态后 tick 一律丢弃、零写出）；③ 任何交接前 stop-and-wait。组件暴露供 G2 编排三分支顺序的状态查询/交接原语——组件负责"停得干净 + 优先级唯一"，顺序编排在 G2。
5. **已提交字节基线查询（v5 新增，供 G2 防线例外）**：组件暴露"心跳已提交的累计字节数/是否已启动"查询，G2 消费 handler 以 `Size() > max(writerSizeBeforeForward, heartbeatBaseSize)` 判定语义写出；并暴露 `IsCommitted()`（心跳已提交 ⇒ streamStarted，v6）。
6. **换号后即时恢复原语（v6）**：`Resume()`——stop-and-wait 后下一 attempt 调用即恢复 tick 并**立刻 Flush 一帧**（不重等 delay，交接空窗 < delay）。
7. heartbeat_delay 读 `cfg.Gateway.UpstreamHeartbeatDelaySeconds`（G4 并行交付，字段名写死）。

## 单测（-tags unit，定向，含 -race）
- 晚触发：delay 前 Stop → 零字节写出；悬挂 ≥delay → 客户端按时间轴**实际收到**初始头与连续 keep-alive 帧（Flush 契约验证，不满足即失败）。
- Flush 失败关闭：无 Flusher 的 ResponseWriter → Start 返回错误、零写出。
- 晚到交接：Stop 后调用方写语义流/error 帧，无帧交错、无 data race。
- **交接连续性（v6）**：heartbeat=60、首轮 guard=90 超时、次轮续挂 → 时间轴断言交接空窗 <60s（Resume 首帧即时）。
- **优先级断言（v4）**：响应事件 vs tick 同至 → 断言胜出方唯一、败方零写出（临界 4xx/5xx 两序各一用例：响应先到→无 SSE 200 提交；tick 先到→晚到契约 error 帧）；guard 终态 vs tick 同至 → 终态后零 tick 写出。
- 三交叠竞态：上游响应与 tick 同至、Stop 与 tick 同至、客户端断开（ctx cancel）时 goroutine 退出（-race 干净）。

## Done when
go build + go vet 绿；定向单测（含 -race）绿；报告含各用例名与结果原文；零挂点文件改动（git diff 证明）。
