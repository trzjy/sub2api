# 派发单 P2-B：CF524 扩展——ChatCompletions 入口安装 + 防线统一（W2，依赖 P2-A 符号）

方案：`docs/cf524-openai-gateway-guard-extension-plan.md` v1.2（§3.1/§3.3/§3.4）。
前置：`internal/handler/openai_heartbeat_failover_guard.go`（P2-A 卡）已落盘，本卡消费
`cf524InstallUpstreamBudgetAndHeartbeatOpenAI` / `cf524OpenAIHeartbeatFailoverGuardStillClean`。
开工前先完整读派发单、方案 §3.1/§3.3、P2-A 两符号实现。
报告写 `/root/sub2api/.omc/reports/openai-p2b-cc-entry-report.md`，末行 `OPENAI_P2B_CC_ENTRY_REPORT_END`。

## 任务（变更严格限于 2 文件）

### 1. `backend/internal/handler/openai_chat_completions.go`
**① 安装点（§3.1）**：`ChatCompletions` 入口在**认证后、选号循环前**调用
`h.cf524InstallUpstreamBudgetAndHeartbeatOpenAI(c, requestStart, reqStream)`（参数名以现场
为准）。CC 链无 compact keepalive（状态 key 恒不存在）→ 正常安装，无需互斥分支。
**② 防线统一（§3.3，本方案最高风险点）**：`:337` 一带的裸 `Size() != before` 消费点改造为
调用 `cf524OpenAIHeartbeatFailoverGuardStillClean`：
- `writerSizeBeforeForward` 与 `hbCommittedBaseline` **同时点**捕获（踩坑 #1——
  baseline 取心跳 owner `CommittedBytes()`，owner 不存在时为 0；owner 获取方式对照 P2-A
  helper 的安装产物/`UpstreamHeartbeatFromContext`）；
- 比较走共享函数（内部双侧 normalizeSize）；CC 无 compact 口径冲突，调用方无需额外归一；
- 改造前后对**无心跳请求逐一等价**（回归用例钉死）。

### 2. 新文件 `backend/internal/handler/openai_chat_completions_install_guard_test.go`（定向测试，`-tags unit`，确认包内 build tag 惯例）
- **目标侧闭包断言（§3.4 矩阵 CC 侧）**：CC 入口发起的请求（流式/非流式）到达选号循环前
  请求 context **必有快照**（流式另有心跳 owner）；可用 gin test context + helper 装配组合论证。
- **防线等价回归**：非流式/无心跳请求的写后禁 failover 判定与改造前逐一等价
  （构造已写场景→判定 false；未写场景→true）。
- **基线同时点断言**（踩坑 #1）：hb owner 存在时 baseline 为捕获时点 CommittedBytes，
  前置心跳字节不重复计入。

## 硬约束
- 触碰禁区即打回：`gateway_handler.go`、`openai_gateway_handler.go`、`cf524_observation.go`、
  `upstream_heartbeat.go`、`openai_heartbeat_failover_guard.go`（P2-A 产物）。
- 零新增机制/配置/兜底分支；handler 选号循环零新增机器（护栏超时错误由既有
  UpstreamFailoverError 换号分类消费）。
- 禁止 git 写操作；禁止全量测试。

## 验证白名单
```
cd backend && GOFLAGS=-buildvcs=false go build ./...
GOFLAGS=-buildvcs=false go vet ./internal/handler/
GOFLAGS=-buildvcs=false go test -tags unit -count=1 -run '<定向>' ./internal/handler/
```

## Done when
build/vet 绿；定向单测绿；报告含变更文件清单+关键 diff+用例结果原文，末行
`OPENAI_P2B_CC_ENTRY_REPORT_END`。遇 429/503/520/524 保留已完成部分并声明现场。
