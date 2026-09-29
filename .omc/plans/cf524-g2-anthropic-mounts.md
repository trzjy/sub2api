# 派发单 G2：挂点 1/2/4 接护栏+心跳 + 消费 handler 防线例外——挂点文件唯一所有者

权威方案：`docs/cf524-upstream-hang-mitigation-plan.md` **v8.2（以仓库最新版为准）** §2 D1/D2。前置：G1（共享护栏组件 + 错误构造器）与 G3（心跳组件，含已提交字节基线查询）已交付，以其实际签名为准。

## 硬约束
- 只改：`internal/service/{gateway_forward.go, gateway_anthropic_passthrough.go, gateway_forward_as_responses.go}` + `internal/handler/{gateway_handler.go, gateway_handler_chat_completions.go, gateway_handler_responses.go}`（三消费 handler **只允许改"写后禁 failover"防线的 Size 判定一处**）+ 对应 `_test.go`；禁止 git 写操作；禁止全量测试。
- 验证白名单：`go build ./...`、`go vet ./internal/service/ ./internal/handler/`、`go test -tags unit -run <定向> ./internal/service/ ./internal/handler/`。

## 任务
1. **挂点 1**：`gateway_forward.go` Forward 主路径 retry 循环内 DoWithTLS（~391 行）接共享护栏 + 请求入口计时总预算（流式 2×guard+10s / 非流式 guard+25s；第二次尝试窗口=min(guard, 剩余)）。覆盖 anthropic OAuth + kimi/deepseek/zhipu 各平台。
2. **挂点 2**：`gateway_anthropic_passthrough.go` DoWithTLS（~107 行）同上。
3. **挂点 4**：`gateway_forward_as_responses.go` 非 OpenAI 平台 Post 同上。
4. **心跳接入（G3 组件）**：三挂点在 `clientStream==true` 时接 G3 心跳组件；交接前同步 stop-and-wait。
5. **入口安装点唯一所有者（v8.2，硬性）**：在三个消费 handler 的**认证/鉴权完成后、并发槽位等待与账号选择前**（各一处，唯一顺序）用 G1 快照类型创建请求级预算快照并安装请求级心跳 owner；forward/attempt 层禁止重初始化计时；认证失败路径零字节语义不破坏；前置延迟+换号+跨热加载边界测试为交付门槛。
6. **消费 handler 防线例外（v5，硬性）**：三消费 handler 既有防线 `c.Writer.Size() != writerSizeBeforeForward → 禁止 failover` 会把心跳注释帧误判为语义写出——判定改为 `Size() > max(writerSizeBeforeForward, heartbeatBaseSize)`（heartbeatBaseSize 来自 G3 组件基线查询；心跳未启动时取值与现状完全一致）。除该一处判定外消费机器（errors.As + fs.HandleFailoverError + handleFailoverExhausted）零改动。
7. **统一触发语义（v5）**：guard 超时 → G1 错误构造器（SafeToFailoverAfterWrite=true）；剩余 ≥5s → ShouldRetryNextAccount=true → 既有 FailoverContinue 换号（换号前心跳 stop-and-wait）；剩余 <5s → ShouldRetryNextAccount=false → 既有 FailoverExhausted → handleFailoverExhausted(streamStarted)：stream→SSE error 帧，非流式→JSON 错误。**心跳已提交 ⇒ 一律视为 streamStarted=true（v6，防 JSON 写进已提交 SSE 200）**。**禁止新建任何切换上限计数器**（主链无固定上限是已接受设计）。**无任何 BLOCKED/跳过例外**。
8. **OpenAI 原生路径零触碰**（openai_gateway_forward.go 属 G1 所有；openai_gateway_handler.go / openai_chat_completions.go 不在本卡范围；本卡不改）。

## 单测（-tags unit，定向）
- **claude-sonnet-5 两形态回放**（生产受害形态）：OAuth 主路径挂点 1、API-key 直通挂点 2，上游悬挂 >guard → 心跳先起、guard 触发后停心跳换号；换号成功返回；额度耗尽 → error 帧/502。
- kimi/deepseek/zhipu 三平台 × 挂点 1/4 悬挂回放（63-victim 形态）。
- 非流式墙内预算：第二次尝试窗口被剩余预算截断（**不可变截止时间**：入口前置延迟+首轮超时+换号+次轮再超时 ≤ 总预算的时间轴断言）；入口已耗时间被扣除。
- **重放一致性（v4）**：换号前后 wireBody 字节一致（三挂点各一用例）；客户端已断开 → 失败关闭不重试。
- 非 OpenAI 路径换号分支与耗尽分支顺序断言（心跳 stop-and-wait 先于换号；剩余<5s → 耗尽）。
- **防线例外两分支（v5）**：心跳已启动 → guard 超时后仍 FailoverContinue 换号（注释帧不判语义写出）；真实语义流已写出 → 仍禁止 failover（现状防线不回退）。
- **v6 四项**：响应头先到 body 超预算完整送达；心跳已提交+直接耗尽 → SSE error 帧；heartbeat=0 流式双超时 ≤ guard+25s；交接空窗 < delay（次轮首帧即时续写）。
- 健康路径零回归：上游 <delay 正常返回时零心跳帧、护栏零触发、状态码/错误透传与现状一致。

## Done when
go build + go vet 绿；定向单测绿；报告含 claude-sonnet-5 两形态用例名与结果原文。
