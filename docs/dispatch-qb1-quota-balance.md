# 派发单 QB-1：CN 供应商余额周期探测扩展 + coding 快照阈值停调接线

## 背景
- 方案：`docs/account-quota-balance-circuit-break-plan.md` §3.1/§3.2（R19 外审通过）；
  确认包 `docs/account-quota-balance-confirmation-package.md` 已获用户裁定（阈值值 99
  属阶段 2 管理端配置，**不进本单代码**）。
- 本单是阶段 1 唯一编码单：两条缺陷的改动同落 `CNProviderBalanceCheckService` 一族
  （共享文件+测试对，按派发粒度规则合并为一单）。
- 探测人群（确认包②，用户裁定）：zhipu/minimax 平台 payg 账号 + kimi/deepseek 平台
  native 余额不可用的 payg 账号（生产 = #87/#135 型中转；native 可用的 inferaiapi 型
  维持原生路径、不进探测）。

## 改动范围
基线分支 main（6292202）。仅改下列文件：

1. `backend/internal/service/cn_provider_balance_check_service.go`
   a. **构造注入**：struct + `NewCNProviderBalanceCheckService` 增加
      `*RateLimitService`（供 `handleCNProviderInsufficientBalance` 与
      `ApplyAccountSchedulingThreshold`）。
   b. **runOnce collect()（:110-135）**：zhipu/minimax 平台 payg 账号
      （`!IsCodingPlan()`、非 Ollama、IsActive、**`account.Schedulable` 管理端开关
      为真**——临时停调账号不受此限须继续探测以支持充值恢复，现在 :131 被直接丢弃）
      收集进新 `probeTargets`；kimi/deepseek 维持现有 `paygTargets` 原生路径不变。
   c. **新增最小完成请求探测函数**（命名贴近既有风格，如 `probeOne`）：
      - 模型解析：`resolveWebTestModel(account, "", account.Platform)`；结果为空 →
        本轮跳过 + 日志（承态，下周期重试）。
      - 凭证/端点：`account.GetOpenAIProtocolAPIKey()` +
        `account.GetOpenAIBaseURL()`，出站 URL 过 `cnValidateProbeURL(cfg, …)`。
      - 请求：`POST {base}/chat/completions`，body
        `{"model":<解析模型>,"messages":[{"role":"user","content":"hi"}],"max_tokens":1}`；
        HTTP 执行复用本文件族既有姿态（同 CNProviderBalanceService/QuotaService 的
        client/超时/代理解析方式），不新建客户端机制。
      - **判定唯一入口** `cnProviderResponseIndicatesInsufficientBalance(body)`：
        * 命中 → `s.rateLimitSvc.handleCNProviderInsufficientBalance(ctx, account,
          extractUpstreamErrorMessage(body))`（停调 2×周期，含 `_balance_low` 快照标记）；
        * 未命中且为有效完成响应（HTTP 2xx）→ 镜像 `checkOne` :255 清除分支：仅当
          `TempUnschedulableUntil != nil && HasPrefix(reason, cnBalanceLowReasonPrefix)`
          时 `ClearTempUnschedulable`（前缀精确命中，不触碰他因停调）；
        * 其余（超时/连接失败/401/403/429 无文案/未识别错误体）→ 不停调不清除，
          既有观测日志，下周期重试。
   d. **native 失败转探测**：`paygTargets` 的 kimi/deepseek 账号 native
      `QueryBalance` 返回 err 或 `!Success` 时，同周期对该账号执行 c 探测
      （覆盖中转不实现 `/user/balance` 的人群；native 成功的账号绝不探测）。
   e. **probeQuota（:206-218）**：`QueryUsage` 成功返回后重载账号
      （`s.accountRepo.GetByID`），调
      `s.rateLimitSvc.ApplyAccountSchedulingThreshold(ctx, fresh)`——照抄
      `codebuddy_quota_check_service.go:111-121` 先例形态（含失败日志不阻断）。
2. `backend/internal/service/wire.go:397` 附近
   `ProvideCNProviderBalanceCheckService` 签名加 rateLimitService 参数。
3. `backend/cmd/server/wire_gen.go`：随签名变化重新生成（`go generate` 或 wire CLI；
   保证生成文件与 Provide 签名一致）。
4. `backend/internal/service/cn_provider_balance_check_service_test.go` 新增用例：
   - 探测 200+余额文案 → 停调，且**断言实际进入余额分支**（R18-F2）：reason 前缀
     `cn_balance_low` + 期限 ≈2× 检测周期 + 未写入配额窗口原因 + 后续有效探测清除该前缀；
   - 探测 429 无余额文案 / 401 / 超时 → 不停调不清除；
   - 先停调（前缀 reason）→ 探测 200 健康 → 前缀精确清除；他因 reason 不被清除；
   - zhipu/minimax payg 进探测队列、coding/ollama 不进；native 成功账号不探测、
     native 失败账号转探测；
   - probeQuota 落快照后：配置阈值触顶 → 停调；无阈值 → 不停调（既有语义保持）；
     触顶恢复（快照刷新降值）→ 停调到期不续停；
   - 模型解析为空 → 跳过 + 日志。

## 禁区
- 不改 `account_scheduling_threshold_eval.go`、`ratelimit_cn_providers.go`、
  `cn_provider_balance_service.go`、`cn_provider_quota_service.go` 的既有逻辑（只调用）。
- 不新增配置项/运行时开关/逐账号门禁/指纹/漂移守卫/状态簿记/告警/指标；
  不发明默认调度阈值；不改 Start/Stop/ticker 骨架与预算结构。
- 不动 `upstream_billing_probe`；不改停调原语与其 reason 枚举。
- 超出本单范围的发现一律 BLOCKED 顶回主会话，不自行扩大改动。

## 验证命令白名单（只允许跑以下命令）
- `cd /mnt/data/sub2api/backend && go build ./...`
- `cd /mnt/data/sub2api/backend && go vet ./internal/service/ ./cmd/server/`
- `cd /mnt/data/sub2api/backend && go test ./internal/service/ -run 'CNBalance|CNProvider|SchedulingThreshold|WalletProbe|Probe' -count=1`

## Done when
- 白名单三条命令全绿（以实际输出为证）。
- 测试覆盖改动范围第 4 条全部用例族，重叠用例含 R18-F2 四断言。
- `wire_gen.go` 与 Provide 签名一致（`go build ./...` 绿即证）。
- diff 不含禁区清单内文件的逻辑改动。

## 模型
- 主力 hy3；额度耗尽切 `custom-local:deepseek-v4.1-flash`，切换不需请示。
