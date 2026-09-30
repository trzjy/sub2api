# 派发单 W2-FIX：CF524 CC 安装护栏测试接收者修正（验收整改，单文件）

方案：`docs/cf524-openai-gateway-guard-extension-plan.md` v1.2（§3.1）。本单是 P2-B/W2 验收整改：
生产代码已闭环（安装 helper 已迁至 `*OpenAIGatewayHandler` 接收者，openai_gateway_handler.go:3746；
CC 入口本体 `func (h *OpenAIGatewayHandler) ChatCompletions`，openai_chat_completions.go:24；
`go build ./...` 与 `go vet ./internal/handler/` 均绿），仅剩 P2-B 测试文件两处旧接收者引用导致
定向单测编译失败：

```
openai_chat_completions_install_guard_test.go:31:55: *GatewayHandler has no method cf524InstallUpstreamBudgetAndHeartbeatOpenAI
openai_chat_completions_install_guard_test.go:46:54: 同上
```

报告写 `.omc/reports/openai-w2-fix-cc-test-receiver-report.md`，末行 `OPENAI_W2_FIX_CC_TEST_RECEIVER_REPORT_END`。

## 改动范围（严格限于 1 文件）
- `backend/internal/handler/openai_chat_completions_install_guard_test.go`：
  - :31 `(&GatewayHandler{cfg: cf524TestCfg(30, 15)}).cf524InstallUpstreamBudgetAndHeartbeatOpenAI(...)`
  - :46 `(&GatewayHandler{cfg: cf524TestCfg(30, 1)}).cf524InstallUpstreamBudgetAndHeartbeatOpenAI(...)`
  - 两处接收者改为 `&OpenAIGatewayHandler{cfg: ...}`（同文件包内 GW 测试
    `openai_gateway_handler_install_guard_test.go:37` 即此写法，照抄其构造形式）。
  - 测试语义零变化：断言、用例名、字段检查不动，只改构造的接收者类型。

## 禁区
- 除上述 1 文件外零改动；生产代码文件（含 openai_gateway_handler.go、
  openai_heartbeat_failover_guard.go、openai_chat_completions.go）零触碰。
- 禁止 git 写操作；禁止全量测试。

## 验证命令白名单（只允许跑以下命令）
```
cd backend && GOFLAGS=-buildvcs=false go build ./...
GOFLAGS=-buildvcs=false go vet ./internal/handler/
GOFLAGS=-buildvcs=false go test -tags unit -count=1 -run 'CF524' ./internal/handler/
```
（`-tags unit` 必须带；不带会静默空跑。）

## Done when
- build/vet 绿；`-run 'CF524'` 定向单测绿（含 P2-A/P2-B/P2-C 全部 CF524 前缀用例）。
- 报告含修改后两处构造原文 + 定向测试输出原文（须可见用例真实执行），末行
  `OPENAI_W2_FIX_CC_TEST_RECEIVER_REPORT_END`。

## 模型
- 主力 hy3；额度耗尽先 `~/.codebuddy-rotate/poll.py` 轮询账号池换号，全账号不可用再切
  custom-local:deepseek-v4.1-flash；切换不需请示。
