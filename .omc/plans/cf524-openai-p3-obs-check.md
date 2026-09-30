# 派发单 P3：CF524 扩展——观测基建 OpenAI 家族泛化核实（W1，与 P2-A 并行）

方案：`docs/cf524-openai-gateway-guard-extension-plan.md` v1.2（§3.4/§5 P3 条目）。
**预计零改动，核实即闭环**。报告写 `/root/sub2api/.omc/reports/openai-p3-obs-check-report.md`，
末行 `OPENAI_P3_OBS_CHECK_REPORT_END`。

## 任务（只读核实为主）

1. **platform 字段泛化核实**：通读 `backend/internal/service/cf524_observation.go` 全文 +
   P1 新增的 `doOpenAIUpstreamWithGuard`（`openai_plugin_transport.go:77-190`）消费路径 +
   主方案 `gateway_forward.go` 的 `cf524ExecuteUpstreamWithGuard` 消费路径，逐项断言：
   - `platform` 参数取 `account.Platform`，OpenAI OAuth 家族的平台枚举值（读 account
     模型定义确认实际取值）能无损进入既有三事件 schema；
   - outcome 枚举、request_id 串联、`upstream_headers_received_ms` 判别器（经既有完成
     日志）对 OpenAI 路径**无需任何修改**即成立。
2. **判别器核实**：`UpstreamHeadersReceivedMs`（P1 测试已验可解析）与既有完成日志消费端
   （grep 判别器的日志读取方）对照，确认 OpenAI 家族完成日志会携带该字段。
3. **结论分支**：
   - 零改动成立 → 报告写明"核实即闭环"，逐条给证据（文件:行号）；
   - 发现必须改动才能支持 OpenAI → **最小修改** `cf524_observation.go` 并在报告登记
     修改理由+diff，禁止顺手重构；无法最小化闭合 → BLOCKED 上报，不得自造机制。

## 硬约束
- 首选零改动；改动仅限 `cf524_observation.go` 单文件且最小化。
- 零新事件/零新 schema（方案 §3 G5 硬约束）。
- 禁止 git 写操作；禁止全量测试。

## 验证白名单
```
cd backend && GOFLAGS=-buildvcs=false go build ./...
GOFLAGS=-buildvcs=false go vet ./internal/service/
```
（若发生了修改：追加 `GOFLAGS=-buildvcs=false go test -tags unit -count=1 -run 'TestCf524' ./internal/service/`）

## Done when
核实结论逐条有 文件:行号 证据；报告末行 `OPENAI_P3_OBS_CHECK_REPORT_END`。
