# 派发单 G4：CF524 两个配置键 + Validate 失败关闭 + 交叉校验

权威方案：`docs/cf524-upstream-hang-mitigation-plan.md` **v8.2（以仓库最新版为准）** §2 D1/D2。先读方案再动手；与方案冲突处停下报告。

## 硬约束
- 只改 `internal/config/config.go`、`internal/config/config_test.go`（如无则新建）、`config.example.yaml`；生产 config.yaml 变更以"变更单"形式写在完成报告里（不直接改生产文件）。
- 禁止 git 写操作；禁止跑全量测试。
- 验证白名单：`go build ./...`、`go vet ./internal/config/`、`go test -tags unit -run <定向> ./internal/config/`。

## 任务
1. 新增 `Gateway.UpstreamFirstByteGuardSeconds int`（默认 90，键 `upstream_first_byte_guard_seconds`）与 `Gateway.UpstreamHeartbeatDelaySeconds int`（默认 15，键 `upstream_heartbeat_delay_seconds`），默认值在未配置时生效。
2. Validate() 失败关闭：
   - guard：**只接受 [30,90]**；0、负数、>90 一律返回错误拒绝启动/热加载（无任何"0=旧行为"回退）。
   - heartbeat：接受 0（禁用，纯优化可关闭）或 [5,60]；其余拒绝。
   - **交叉校验（v3 新增）**：heartbeat > 0 时必须 heartbeat < guard，否则拒绝启动（guard 先触发会使非零心跳永不生效）。
   - **热加载原子性（v4 新增）**：候选配置**整体校验为唯一提交边界**——非法候选一律整体拒绝、**零部分生效**（两键均维持旧值），记录明确拒绝事件；不存在先应用一键再校验另一键的半更新状态。
3. `config.example.yaml` 补两键注释（90 与 CF ~125s 墙关系、heartbeat 0=禁用、交叉约束）。
4. 生产 config.yaml 变更单（报告内）：`gateway:` 段加 `upstream_first_byte_guard_seconds: 90` 与 `upstream_heartbeat_delay_seconds: 15`。

## 单测（-tags unit，定向）
- Validate 边界：guard 29/30/90/91/0 各断言；heartbeat 0/4/5/60/61 各断言。
- 交叉校验：guard=30+heartbeat=60 拒绝；guard=90+heartbeat=15 通过；heartbeat=0 任意合法 guard 通过。
- **热加载原子性三测试**：非法 guard、非法 heartbeat、交叉校验失败 → 各自断言两键均维持旧值且拒绝事件产生。
- 默认值：空配置下两键分别=90/15。

## Done when
go build + go vet 绿；定向单测绿；报告含生产 config.yaml 变更单原文。
