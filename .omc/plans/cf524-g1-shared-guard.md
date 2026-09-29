# 派发单 G1：首字节护栏泛化为共享组件（含旧链归零 + 平台无关错误构造器）

权威方案：`docs/cf524-upstream-hang-mitigation-plan.md` **v8.2（以仓库最新版为准）**§2 D1。先读方案再动手；与方案冲突处停下报告，不得自行改语义。

## 硬约束
- 只允许改/建本单列出的文件：新增 `internal/service/upstream_first_byte_guard.go`、`internal/service/upstream_first_byte_error.go`；改 `openai_gateway_forward.go`、`gateway_forward_as_chat_completions.go` + 对应 `_test.go`；禁止 git 写操作；禁止全量测试。
- **`internal/handler/openai_gateway_handler.go` 零改动**（其识别/计数/耗尽机器归 OpenAI 循环所有，不在本卡范围）。
- 禁止兜底/降级/回退路径（合同 §5）：guard 无禁用值路径。
- 验证白名单：`go build ./...`、`go vet ./internal/service/`、`go test -tags unit -run <定向> ./internal/service/`（**必须带 -tags unit**）。

## 任务
1. **泛化共享组件（watchdog 模式，v7）**：把 `openai_gateway_forward.go` 中的 `openAIFirstOutputHeaderGuard`（~1084-1145 行）泛化为平台无关的首字节护栏组件（`upstream_first_byte_guard.go`）。**护栏 = 可取消 watchdog，不是 context deadline**：attempt 起定时器（窗口=min(guard,剩余)）；头到→停表零取消；窗口耗尽→取消上游请求 context + failover 错误；**护栏 context 不进 body 读取路径**（body 继承父 context）。OpenAI 路径改调共享底层原语并以适配器包回**既有行为**（既有 newOpenAIFirstOutputTimeoutError/openai 超时配置/handler 换号计数全保留），**行为零变化**；入口预算/心跳/新错误构造器不进 OpenAI 路径（反向测试钉死）。
2. **平台无关错误构造器**（`upstream_first_byte_error.go`）：`newUpstreamFirstByteTimeoutError`——产出 `UpstreamFailoverError`（SafeToFailoverAfterWrite=true）。**换号语义（v5 更正，零新计数器）**：`ShouldRetryNextAccount` 由剩余预算判定——剩余 ≥5s → true（既有 FailoverState 循环换号，主链无固定切换上限是已接受设计，**禁止**新建切换上限计数器）；剩余 <5s → false（既有 FailoverExhausted → handleFailoverExhausted(streamStarted)：stream→SSE error 帧，非流式→JSON 错误）。**禁止改任何 handler 文件**（消费机器已存在，实证见方案 v5 头注）。
3. **请求级预算快照类型 + helper（v8.2）**：`upstream_first_byte_guard.go` 内交付 `RequestBudgetSnapshot`{guard 值, heartbeat delay 值, 绝对截止, 入口单调起点} 与 context 读/写 helper；**创建入口在 G2 的 handler 层安装点**（G1 不装）。挂点只经 helper 消费快照，禁止 forward/attempt 层重初始化计时。
4. **请求入口计时总预算（仅四个新挂点；方案 D1 v7）**：预算起点 = 请求到达网关的单调时间——唯一入口中间件创建请求级单调起点写入请求上下文（v7 唯一传播路径），五个挂点只消费、禁止 forward 层重新初始化；**入口一次捕获不可变快照 {guard, heartbeat delay, 绝对截止}**（热加载只影响后续请求，跨热加载边界测试），每轮尝试仅从快照截止取剩余窗口（禁止循环内重建完整 guard 窗口）；有心跳流式（heartbeat>0）总预算 2×guard+10s；**非流式或 heartbeat=0 流式一律 guard+25s（墙内，v6）**；每次尝试窗口 = min(guard, 剩余)；换号不重置。guard 值读 `cfg.Gateway.UpstreamFirstByteGuardSeconds`（G4 并行交付，字段名以此为准写死）。
4. **请求体可重放契约（零新机制）**：换号重放复用既有物化机制——`buildUpstreamRequest`（gateway_upstream_request.go:21）每 attempt 从同一 body `[]byte` 切片重建；挂点 2/3/4 的 build 函数同型核实后照用，核实结论写入报告。
5. **挂点 3 接入**：`gateway_forward_as_chat_completions.go` 上游 Post 接护栏 + 错误构造器 + 换号预算 + 耗尽分支。
6. **旧链归零（合同 §4）**：登记旧 `openAIFirstOutputHeaderGuard` 入口与全部调用点 → 迁移共享实现 → 删除被替代的旧类型/旧构造器 → 完成后 `grep -rn "openAIFirstOutputHeaderGuard" --include="*.go"` 残留扫描（应零命中或等价新名），结果写入报告。

## 单测（-tags unit，定向）
- 共享组件等价性：OpenAI 路径现有 headerGuard 用例改指共享实现后仍绿。
- 总预算：入口已耗时间被扣除（前置延迟 >5s 用例仍墙内返回）；**不可变截止时间轴用例**：入口前置延迟 + 首轮超时 + 换号 + 次轮再超时 → 最终响应 ≤ 对应总预算（证明次轮窗口被剩余截断、非完整 guard 重建）。
- **重放一致性**：换号前后 wireBody 字节完全一致（buildUpstreamRequest 返回值直接断言）。
- **watchdog 边界（v7）**：响应头先到、body 传输超预算 → 完整送达且护栏未取消上游 context。
- **头 vs 超时线性化三边界（v7）**：恰好截止点前/恰好截止点/恰好截止点后，各断言唯一胜出者。
- **OpenAI 零变化反向测试（v7）**：OpenAI 路径不带入口预算/心跳/新错误构造器。
- 错误构造器：SafeToFailoverAfterWrite=true；**剩余预算两分支**：剩余 ≥5s → ShouldRetryNextAccount=true；剩余 <5s → false（既有 FailoverExhausted 路径，测试断言错误字段而非新计数器）。
- 挂点 3：guard 触发 → failover 错误；额度耗尽 → SSE error 帧 / 502 JSON。
- 残留扫描结果贴入报告。

## Done when
go build + go vet 绿；定向单测绿；旧链符号扫描归零；报告含上述证据原文。
