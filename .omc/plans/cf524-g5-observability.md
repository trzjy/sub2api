# 派发单 G5：CF524 可观测事件 + 覆盖矩阵验收卡

权威方案：`docs/cf524-upstream-hang-mitigation-plan.md` **v8.2（以仓库最新版为准）**§4 验收。前置：G1-G3 全部交付。

## 硬约束
- 改动面：挂点 1-5 处各加事件埋点（最小 diff）；新增验收文档 `docs/cf524-acceptance-checklist.md`；禁止 git 写操作；禁止全量测试。
- 验证白名单：`go build ./...`、`go vet ./internal/service/`、`go test -tags unit -run <定向> ./internal/service/`。

## 任务
1. **两条稳定事件**（slog，全部挂点统一）：
   - `gateway_first_byte_guard_triggered`：字段 path/platform/stream/attempt/outcome/elapsed_ms/request_id；
   - `gateway_upstream_heartbeat_started`：字段 path/platform/heartbeat_delay_ms/request_id。
2. **观测关联契约（v4 硬性）**：`outcome ∈ {failover_success, failover_exhausted, client_gone, subsequent_upstream_error}` 枚举固化（最后一项=guard 后下一 attempt 以非 failover 错误终结，v7）；每次 guard 触发发一条 attempt 事件 + 一条终态关联事件（同 request_id）；**访问日志必含同一 request_id 为验收前置**（缺失即判观测链断裂而非"无命中"）；验收含一条可控超时请求的端到端日志串联实测（guard 触发 → attempt → 终态 → 访问日志，四点同 request_id；含"guard 后下一 attempt 立即错误"分支）。
3. `docs/cf524-acceptance-checklist.md`：五挂点 × 63-victim 全形态（kimi/deepseek/zhipu 三平台 × 三路径 + anthropic OAuth/直通两形态）覆盖矩阵，逐行引用 G1-G3 单测用例名；生产验收唯一口径（v3 双簇）：上线后 48h 窗，`docker logs sub2api | grep` 两事件名 + 应用访问日志 499 统计同排查命令，判定 = **124.9–125.1s 簇与 119.9–120.1s 簇的 499 命中数均为 0**（基线 63/48h + 6/48h）且每条 guard 事件可串联最终结果；**回滚兼容项**：部署前旧镜像+新 config 启动实测、回滚步骤含配置恢复与失败关闭验证，纳入部署证据。

## 单测（-tags unit，定向）
- 事件字段断言：guard 触发事件含全部字段且 attempt/outcome 枚举取值正确（三分支各一用例）；心跳事件字段正确；attempt 事件与终态关联事件同 request_id。

## Done when
go build + go vet 绿；定向单测绿；验收文档落盘且矩阵五行全有对应用例名。
