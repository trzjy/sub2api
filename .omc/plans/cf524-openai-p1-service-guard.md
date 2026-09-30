# 派发单 P1：CF524 扩展——OpenAI 上游执行收口护栏 + 心跳消费 + 观测单点

方案：`docs/cf524-openai-gateway-guard-extension-plan.md` v1.2（§3.2 消费点、§4 踩坑
清单为硬约束，执行前必读）。本卡只做 **service 层**；handler 安装点属 P2，**禁止触碰
handler/ 目录任何文件**。

## 背景
OpenAIGateway 家族（kimi/zhipu/deepseek/minimax/codebuddy/other/openai/grok）零 CF524
布线，生产 524 特征持续。护栏收口点 = `doOpenAIUpstream`（全部 8+1 上游执行点唯一出网
口）。快照/心跳 owner 由 P2 在 handler 入口安装（本卡不装），本卡交付消费端。

## 任务
1. **`backend/internal/service/openai_plugin_transport.go`**：`doOpenAIUpstream` 现实现
   改名为 `doOpenAIUpstreamInner`（私有）；新 `doOpenAIUpstream` 外层包装（签名加
   `c *gin.Context` 首参）：
   - 从 `clientCtx` 取 `RequestBudgetSnapshotFromContext` / `UpstreamHeartbeatFromContext`；
     **无快照 → 原样透传内层实现**（零行为变化，这是无兜底契约：不是降级分支，
     是"未安装快照的调用方不在本方案覆盖面"）。
   - 有快照：`window := snapshot.AttemptWindow(time.Now())`；剩余 <
     `upstreamFirstByteMinViableWindow`（5s）→ 直接返回
     `newUpstreamFirstByteTimeoutError(remaining)`（既有构造，upstream_first_byte_error.go:29，
     **不向上游发请求**）；否则 `newUpstreamFirstByteGuard(upstreamCtx, window)` 构建
     护栏（对照 gateway_forward.go:405-409 同型），护栏 reqCtx 写入上游请求 context。
   - 心跳 owner 存在且请求为流式时 `hb.Resume()`（对照 gateway_forward.go:972 一带；
     失败仅记 warn 日志 `gateway.cf524_heartbeat_resume_failed`，含 path/request_id/
     account_id，零语义影响——心跳是可选优化，不终止请求）。
   - 调内层实现后三分支（对照 `cf524ExecuteUpstreamWithGuard` gateway_forward.go:990-1035
     逐条同型）：①护栏 `TimedOut()` → 心跳 `OnGuardDecided()`+`Stop()`（stop-and-wait）、
     关 resp.Body、返回 `newUpstreamFirstByteTimeoutError(snapshot.RemainingBudget(now))`；
     ②传输错误 → `guard.Stop()`+`hb.Stop()` 后原样上抛；③响应头到达 → `guard.Stop()`+
     `hb.OnUpstreamHeaderArrived()`+`hb.Stop()`（晚到契约三分支顺序）后返回 resp。
   - **护栏取消边界仅覆盖响应头等待**（收到响应头即解除；body 读取不在 deadline 内）。
2. **观测单点（G5，复用 cf524_observation.go 全套，零新事件零新 schema）**：包装内
   `obs := &cf524GuardObservation{c: c, ctx: <原始 clientCtx——必须是未包护栏 reqCtx 的
   原始 context，否则护栏超时被误判 client_gone>, platform: account.Platform, ...}`；
   三分支对应发射（attempt 事件/heartbeat started 幂等/headers 时刻）。**tracker 在
   gin context（c.Set），这就是需要 c 参数的原因**。
3. **8 个调用点机械 c 参穿透**（各一行，不改任何逻辑）：
   `openai_gateway_cc_pipeline.go:243`、`openai_gateway_chat_completions_anthropic_native.go:120`、
   `openai_gateway_chat_completions.go:395`、`openai_gateway_forward.go:1125`、
   `openai_gateway_messages_anthropic_native.go:103`、`openai_gateway_messages.go:442`、
   `openai_gateway_passthrough.go:378`、`openai_gateway_responses_anthropic_native.go:125`。
   （各文件调用方作用域内已有 `c`；确实无 c 的调用点如实登记并 BLOCKED 上报，禁止
   自行造 bridge。）
4. **定向测试**（`-tags unit`，新文件 `openai_upstream_guard_test.go`）：
   - 无快照透传等价：无快照时内层被调用、返回值/错误逐字段一致；
   - 有快照护栏超时：窗口耗尽返回 `*UpstreamFailoverError` 且
     `ShouldRetryNextAccount()` 与剩余预算关系正确（<5s → false）；**不依赖真实网络**
     （httptest 慢响应或注入）；
   - 剩余 <5s 不发请求即返回超时错误；
   - 心跳分支：owner 存在时 Resume 被调、header 到达后 stop-and-wait（对照
     upstream_heartbeat_test.go 既有测试手法）；
   - 观测：护栏超时发 `gateway_first_byte_guard_triggered`（phase=attempt）、响应头
     到达后 `upstream_headers_received_ms` 可解析；
   - 非目标调用方闭包断言（P1 侧）：embeddings/count_tokens/alpha_search 路径请求
     context 无快照 → 包装层透传（护栏零介入）。

## 硬约束（踩坑清单 §4 逐条对照，违者验收打回）
- 命名返回值（#4）：新包装函数 `(resp *http.Response, err error)`。
- obs.ctx 用原始请求 context（#5）；事件单点发射（#6），tracker/cf524_observation.go
  **零改动**。
- 零新增机制/配置/兜底分支（#8）；无快照透传不是 fallback，是覆盖面定义。
- CN watchdog 在内层实现内部零改动；护栏只在自己 fired 时介入（#14）。
- 禁止触碰：handler/ 全目录、`upstream_heartbeat.go`、`cf524_observation.go`、
  `codebuddy_gateway_forward.go`、`doOpenAIUpstreamNoWatchdog` 及其调用方。
- 禁止 git 写操作；禁止全量测试。
- 每处 Size/字节算术若涉及，对照 gin v1.9.1 response_writer 源码（#3）——本卡预期
  无 Size 算术（防线判定属 P2）。

## 验证白名单
```
cd backend && GOFLAGS=-buildvcs=false go build ./...
GOFLAGS=-buildvcs=false go vet ./internal/service/
GOFLAGS=-buildvcs=false go test -tags unit -count=1 -run '<定向>' ./internal/service/
```

## Done when
build/vet 绿；定向单测绿；报告写 `/root/sub2api/.omc/reports/openai-p1-report.md`
（变更文件清单+关键 diff 原文+用例结果原文+未触碰声明），末行
`OPENAI_P1_REPORT_END`。
