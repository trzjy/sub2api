# 派发单 P2-C：CF524 扩展——gateway_handler 双入口安装 + compact 互斥 + 防线统一（W2，依赖 P2-A 符号）

方案：`docs/cf524-openai-gateway-guard-extension-plan.md` v1.2（§3.1/§3.3/§3.4）。
前置：`internal/handler/openai_heartbeat_failover_guard.go`（P2-A 卡）已落盘。
开工前先完整读派发单、方案 §3.1/§3.3、P2-A 两符号实现、`openai_compact_sse_keepalive.go`。
报告写 `/root/sub2api/.omc/reports/openai-p2c-gw-entries-report.md`，末行
`OPENAI_P2C_GW_ENTRIES_REPORT_END`。

## 任务（变更严格限于 2 文件）

### 1. `backend/internal/handler/openai_gateway_handler.go`（Responses+Messages 两入口，同卡）

**① 安装点 ×2（§3.1）**：
- **Responses 入口**：在 `service.StartOpenAICompactSSEKeepalive`（**:454**）**之后**、选号
  循环之前（仍在认证后）调用安装 helper。**调用顺序是钉死的不变量，不是实现期分支**：
  helper 执行时状态 key `openai_compact_sse_keepalive` 必已写入（含 :454 no-op 退出情形：
  no-op 不写 key 即事实源为"未安装"）。
- **Messages 入口**：认证后、选号循环前同样安装（该链无 compact keepalive，key 恒不存在
  → 正常安装）。
- 互斥判定不在本卡实现——helper（P2-A）内部只消费状态 key，本卡保证顺序。

**② 防线统一（§3.3，本方案最高风险点）**：
- `:1455` 裸 `c.Writer.Size() != writerSizeBeforeForward` 消费点改造为
  `cf524OpenAIHeartbeatFailoverGuardStillClean`；`:1368` 的 `writerSizeBeforeForward` 捕获
  **同时点**捕获 `hbCommittedBaseline`（踩坑 #1；owner 不存在时 0）。
- **Responses 路径 keepalive 口径冲突显式处理（踩坑 #12）**：`:780` 的
  `service.OpenAICompactKeepaliveAdjustedWrittenSize(c)` 无语义字节时返回 -1，与 G5d
  归一 0 约定不同源——调用方先把它过同一 `normalizeSize(<0→0)` 再进共享函数比较；
  compact keepalive 字节继续由该函数扣除、心跳字节由 CommittedBytes 基线增量扣除，
  **两个扣除来源各自独立、不得合并计数**。
- 改造前后对无心跳请求逐一等价（回归用例钉死）。

### 2. 新文件 `backend/internal/handler/openai_gateway_handler_install_guard_test.go`（定向测试，`-tags unit`，确认包内 build tag 惯例）
- **调用顺序不变量断言**（§3.1）：Responses 链 keepalive start 先于安装 helper
  （组合论证/探针断言执行序）。
- **单 owner 断言**：keepalive 已安装 → 最终同请求 0 个本方案心跳 owner；未安装/no-op →
  恰 1 个。
- **目标侧闭包断言（§3.4 矩阵 Responses/Messages 侧）**：两入口请求到达选号循环前 context
  必有快照（流式另有 owner）。
- **-1/0 归一回归**（踩坑 #12）：`OpenAICompactKeepaliveAdjustedWrittenSize` 返回 -1 场景
  经归一后与 0 等价，比较结果不变；两扣除来源独立不合并。
- **防线等价回归**：非流式/无心跳请求判定与改造前逐一等价。

## 硬约束
- 触碰禁区即打回：`gateway_handler.go`、`openai_chat_completions.go`、`cf524_observation.go`、
  `upstream_heartbeat.go`、`openai_heartbeat_failover_guard.go`（P2-A 产物）、compact keepalive
  本体（openai_compact_sse_keepalive.go，已验收机制不替换不吸收）。
- 零新增机制/配置/兜底分支；handler 换号循环零新增机器。
- 禁止 git 写操作；禁止全量测试。

## 验证白名单
```
cd backend && GOFLAGS=-buildvcs=false go build ./...
GOFLAGS=-buildvcs=false go vet ./internal/handler/
GOFLAGS=-buildvcs=false go test -tags unit -count=1 -run '<定向>' ./internal/handler/
```

## Done when
build/vet 绿；定向单测绿；报告含变更文件清单+关键 diff+用例结果原文，末行
`OPENAI_P2C_GW_ENTRIES_REPORT_END`。遇 429/503/520/524 保留已完成部分并声明现场。
