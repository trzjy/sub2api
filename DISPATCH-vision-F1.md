# 派发单 Vision-F1：修复 service 包测试编译阻塞（机械补参）

## 背景
方案：`docs/capability-routing-plan.md` v3.1（已过闸①）。C 单给
`SelectAccountWithSchedulerForCapability` 加参后未同步既有测试调用点，
`go test ./internal/service/` 编译失败，阻塞整个包的测试运行。
本单是纯机械修复，为后续所有 service 包验证解堵。

## 改动范围
只允许改以下 6 个测试文件（只补调用参数，一个字符的语义都不改）：
- `backend/internal/service/openai_account_scheduler_test.go`（约 16 处，含 1350/1567 行附近）
- `backend/internal/service/openai_guardian_affinity_test.go`（2 处）
- `backend/internal/service/openai_account_scheduler_compact_test.go`（4 处）
- `backend/internal/service/codebuddy_rpm_test.go`（2 处）
- `backend/internal/service/openai_profit_control_pricing_test.go`（2 处）
- `backend/internal/service/taska_platform_merge_scheduler_test.go`（7 处）

修法：读 `backend/internal/service/openai_account_scheduler.go:2221` 的当前
生产签名，把每个测试调用点的实参逐位对齐到该签名。新增的 `requireVision`
一律传 `false`（既有测试场景均不含图，不得改变任何测试的语义与断言）。
注意 vet 报错显示存在参数错位（bool 位置收到 `PlatformOpenAI`），必须按
生产签名逐位核对，不是简单在末尾补一个参。

## 禁区
- 不得改任何生产代码（`openai_account_scheduler.go` 等）。
- 不得改测试断言、测试名、期望值。
- 不触碰其他文件；发现越界问题 BLOCKED 顶回主会话。

## 验证命令白名单（只允许跑以下命令）
- `cd backend && go vet ./internal/service/`
- `cd backend && go test ./internal/service/ -run 'TestSelectAccount|TestAccountScheduler|TestGuardian' -count=1`

## Done when
- `go vet ./internal/service/` 零输出（编译通过）。
- 上述定向测试全部 PASS。
- 开工前后 `git status --short` 快照对比，仅上述 6 文件新增改动。

## 模型
- 主力 hy3；额度耗尽切 custom-local:deepseek-v4.1-flash，切换不需请示。
