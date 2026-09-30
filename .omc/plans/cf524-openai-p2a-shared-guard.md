# 派发单 P2-A：CF524 扩展——OpenAI handler 共享判定函数 + 安装 helper（W1，先行卡）

方案：`docs/cf524-openai-gateway-guard-extension-plan.md` v1.2（§3.1/§3.3/§4）。
P2 后续卡（CC 入口 / gateway_handler 双入口）编译期依赖本卡符号，本卡必须最先完成。
报告写 `/root/sub2api/.omc/reports/openai-p2a-shared-guard-report.md`，末行 `OPENAI_P2A_SHARED_GUARD_REPORT_END`。

## 任务

### 1. 新文件 `backend/internal/handler/openai_heartbeat_failover_guard.go`，含两个符号：

**① `cf524OpenAIHeartbeatFailoverGuardStillClean`**（handler 包内可见，同型实现）：
先完整读 `gateway_handler.go:2140` 的 `heartbeatFailoverGuardStillClean`，语义逐字同型移植
（判定逻辑唯一：OpenAI 家族两入口都调它；`gateway_handler.go` 本体与 ：2140 函数零改动）：
- 基线增量严格相等：`writerSizeBeforeForward` 与 `hbCommittedBaseline` **同时点**捕获由调用方
  保证（踩坑 #1）；本函数只做比较。
- 双侧 `normalizeSize(<0→0)`（gin noWritten 哨兵，踩坑 #2/#3：任何 Size 结论必须对照
  gin v1.9.1 response_writer 源码，禁口头推断）。
- 无心跳分支（baseline=0）退化为 `size == before`，与现状逐一等价，无第二分支。

**② `cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c *gin.Context, requestStart time.Time, reqStream bool)`**：
包装既有 `cf524InstallUpstreamBudgetAndHeartbeat`（先 grep 找到现实现，读主方案 G2b 等价语义）。
安装条件**与主方案逐字相同**：
- `guardSeconds<=0` 不安装；
- 非流式（`reqStream==false`）只装快照、不装心跳 owner；
- Flush 缺失 → 失败关闭 + 记 warn。
**compact 互斥（方案 §3.1 唯一新增判定）**：互斥判定**只消费 compact keepalive 的真实安装
状态**——从 gin context key `openai_compact_sse_keepalive`（`openai_compact_sse_keepalive.go:12`）
读；已安装 → 本 helper 不再装心跳 owner（快照照装）；key 不存在/no-op → 正常安装恰一个
owner。**禁止在 helper 里重复推导 `openAICompactClientWantsStream`/compact 标记**。
调用顺序不变量（keepalive start 先于 helper）由调用方卡保证，本卡只消费状态 key。

### 2. 新文件 `backend/internal/handler/openai_heartbeat_failover_guard_test.go`（定向测试，`-tags unit`，先确认该包测试文件的 build tag 惯例——service 层测试带 `//go:build unit`，不带 tags 会静默空跑）：
- guard 判定表驱动：写入差→false；写入差+baseline 增量相等→true；双侧 -1 哨兵归一；
  无心跳分支退化等价。
- helper 安装条件：guardSeconds<=0 不安装；非流式只装快照；Flush 缺失失败关闭；
  keepalive key 已装 → 无心跳 owner；key 不存在 → 恰一个心跳 owner。

## 硬约束
- 变更严格限于：上述两个新文件。禁止触碰 `gateway_handler.go`、`openai_chat_completions.go`、
  `openai_gateway_handler.go`、`cf524_observation.go`、`upstream_heartbeat.go`。
- 零新增配置/机制；失败关闭无兜底（踩坑 #8）。
- 禁止 git 写操作；禁止全量测试。

## 验证白名单
```
cd backend && GOFLAGS=-buildvcs=false go build ./...
GOFLAGS=-buildvcs=false go vet ./internal/handler/
GOFLAGS=-buildvcs=false go test -tags unit -count=1 -run '<定向>' ./internal/handler/
```

## Done when
build/vet 绿；定向单测绿；报告含变更文件清单+关键 diff+用例结果原文，末行
`OPENAI_P2A_SHARED_GUARD_REPORT_END`。遇 429/503/520/524 保留已完成部分并声明现场。
