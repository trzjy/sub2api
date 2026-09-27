# 派发单 Vision-V1：WS 首帧含图 hint + kill-switch 设置源修正（第 5 次终审 2 条）

## 背景

方案 SSOT `docs/capability-routing-plan.md` v3.1（已过闸①）。第 5 次终审
（2026-09-28，输出 /home/zjy/.codex-companion/2026-09-27T17-41-02-141Z-review，
裁定留痕 REVIEW-PACKET-vision-routing-final.md §15）block，其中 2 条归本单：

- **P1 WS 首帧不写含图 hint**：`openai_gateway_handler.go` WS 桥首帧只把
  firstMessage 传给 `withOpenAIProfitSuppressedForImage`（:2639-2640），未调
  `SetOpenAIHasImageInputHint`；后续选号 `GetOpenAIHasImageInputHint(c)` 恒 false
  ⇒ 带图 WS 请求 RequireVision=false，绕过 §3.4 视觉过滤。chat_completions 入口
  （openai_chat_completions.go:107）已正确写 hint，WS 入口是漏网。
- **P2 kill-switch 读错设置实例**：`isVisionRoutingEnabled`
  （openai_account_scheduler.go:2544-2549）读 `s.rateLimitService.settingService`；
  构造函数注入的 `s.settingService`（openai_gateway_service.go:454/528/567）才是
  权威实例。rateLimitService 为 nil 或二者非同一实例时，配置的
  vision_routing_enabled=false 被误当默认开启，kill-switch 失效。

## 改动范围（仅允许改以下 4 个文件）

1. `backend/internal/handler/openai_gateway_handler.go`
2. `backend/internal/service/openai_account_scheduler.go`
3. WS 入口对应测试文件（现有含图/hint 相关测试所在文件；如需新建放
   `backend/internal/handler/openai_ws_vision_hint_test.go`）
4. `backend/internal/service/setting_vision_routing_update_test.go` 或 scheduler
   kill-switch 测试所在文件（如需新建放
   `backend/internal/service/openai_account_scheduler_vision_killswitch_test.go`）

除此之外一律禁改。

### 改动点 A：WS 首帧写含图 hint（P1）

- WS 桥首帧解析后、装门前，与 chat_completions 入口同构地写请求级 hint：
  `service.SetOpenAIHasImageInputHint(c, service.HasOpenAIInputImage(firstMessage))`。
  注意复用既有解析函数（`HasOpenAIInputImage`），不得新写第二套解析（平行路径禁令）。
- 同一连接内后续 turn / failover 重入选号时复用该请求级结果（既有
  `GetOpenAIHasImageInputHint(c)` 读取点不动——hint 是请求级/连接级常量，首帧判定
  一次即可；不得改成每 turn 重解析）。
- 与 `withOpenAIProfitSuppressedForImage` 的关系：两处都基于 firstMessage 判定，
  语义一致；如二者判定函数相同，可提取为局部变量一次判定两处使用（净减复杂度），
  但不得改变利润门既有行为。

### 改动点 B：kill-switch 权威设置源（P2）

- `isVisionRoutingEnabled` 改读 `s.settingService`（构造函数注入实例，
  openai_gateway_service.go:454），删除对 `s.rateLimitService.settingService` 的引用：
  ```go
  func (s *OpenAIGatewayService) isVisionRoutingEnabled(ctx context.Context) bool {
      if s == nil || s.settingService == nil {
          return true
      }
      return s.settingService.IsVisionRoutingEnabled(ctx)
  }
  ```
- 缺 settingService（仅测试/误配可达）默认开启语义保持不变。
- 同步检查 `openai_gateway_service.go` 内其他 `s.rateLimitService.settingService`
  引用：本单只改 vision 开关这一处；如发现其他读点属既有代码（非本任务引入），
  BLOCKED 顶回列出，不自行扩大。

## 禁区

- 禁改：vision_capability/group_handler/前端/迁移/wire*/setting_* 生产代码、
  能力缓存协议（account_model_capability*、vision_routing_repo）。
- 禁止 `git checkout --` / `git restore`（共享工作区）。
- 禁止新增解析函数、兜底、TTL；错误路径显式传播。
- 超范围发现一律 BLOCKED 顶回。
- 开工前后 `git status --short` 快照自证改动仅限上列文件。

## 验证命令白名单（/mnt/data/sub2api/backend 下执行）

- `go build ./internal/handler/ ./internal/service/`
- `go vet ./internal/handler/ ./internal/service/`
- `go test ./internal/service/ -run 'VisionRouting|VisionKillSwitch|KillSwitch' -count=1`
- `go test ./internal/handler/ -run 'Vision|HasImageInput|WebSocket' -count=1`
- `go build ./... && go vet ./...`

（全量测试由主会话收敛阶段统一执行，你不得跑 `go test ./...`。）

## Done when

1. 白名单命令全过，输出贴回。
2. 改动点 A/B 落地：WS 首帧写 hint（选号读到 true 时 RequireVision 生效）；
   kill-switch 读 s.settingService。
3. 新增/更新测试：首帧含 input_image ⇒ 选号参数 requireVision=true；
   vision_routing_enabled=false ⇒ 无视 hint 走 v1 行为（3 场景）。
4. `git status --short` 前后快照证明零越界。
5. 超范围问题 BLOCKED 顶回。

## 模型

- 主力 hy3；额度耗尽切 `custom-local:deepseek-v4.1-flash`（必须带 `custom-local:`
  前缀），切换不需请示。
