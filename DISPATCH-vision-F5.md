# 派发单 Vision-F5：`vision_routing_enabled` 灰度开关（方案 §3.8 执行遗漏整改）

## 背景
方案：`docs/capability-routing-plan.md` v3.1 §3.8（已过闸①）明文：
「全局 kill-switch（设置项 `vision_routing_enabled`），关闭后 `RequireVision`
恒为 false，行为回到 v1 现状（普通路由），秒级生效（复用 error_passthrough 的
设置缓存刷新机制）」。消费端闭包审查（2026-09-27 主会话）确认未实现。

**主会话裁定**：
- 开关收口在**调度器唯一入口** `selectAccountWithScheduler`
  （`openai_account_scheduler.go` :2361 起的函数签名带 `requireVision bool`）：
  入口处查设置，关闭 → 将 `requireVision` 置 false 后走既有流程。单点收口
  同时覆盖 §3.4 能力过滤与 §3.7 候选池收窄，与「回到 v1 现状」语义一致。
- **默认值：未配置 = 开启**。开关定位是回滚手段（方案归在"灰度与回滚"），
  非灰度门禁；未配置时功能生效。
- 不在 handler 层判断（handler 派发单 F4 正在并行改同域文件，避开）。

## 改动范围
1. `backend/internal/service/openai_account_scheduler.go`：
   `selectAccountWithScheduler` 入口处（装门逻辑之后、选号开始前）：
   `requireVision && !s.isVisionRoutingEnabled(ctx)` → `requireVision = false`。
   读取走既有 SettingService 缓存链路（先检索仓内设置项的既有读取模式，
   同构跟随，不新造缓存机制）。
2. 设置项注册：按仓内设置项既有注册/默认值组织方式新增
   `vision_routing_enabled`（bool，默认 true）。先检索 error_passthrough 等
   既有设置项从 schema/常量到 handler 的全链定义位置，同构跟随。
3. 新增测试（service 层）：开关关闭 → 带图请求走普通路由（已知不支持视觉的
   账号不再被排除、vision_routing 配置不生效）；开关开启/未配置 → 现状行为。

## 禁区
- 不得改 handler、前端、`openai_profit_control.go`、`vision_routing_service.go`。
- F4 正在并行改 handler 文件，**绝对不得触碰 internal/handler/ 下任何文件**。
- 不得改既有测试文件的既有用例逻辑（只允许因设置项注册需要的最小机械扩展）。
- 禁止兜底分支（合同 §5）：设置读取失败按既有设置链路的失败语义处理
  （同构 error_passthrough 模式），不自创降级。
- 边界外发现 BLOCKED 顶回。

## 验证命令白名单（只允许跑以下命令）
- `cd backend && go build ./...`
- `cd backend && go test ./internal/service/ -run 'VisionRouting' -count=1`
- `cd backend && go test ./internal/service/ -run 'Setting' -count=1`

## Done when
- 开关收口 + 设置项注册 + 双向测试全部落地，`go build ./...` 通过。
- 新测试 PASS：关闭回 v1 / 开启现状 两方向都有断言。
- `git status --short` 快照改动仅限：`openai_account_scheduler.go`、设置项
  注册所涉文件、新/扩展测试文件。

## 模型
- 执行模型：custom-local:deepseek-v4.1-flash（hy3 本轮挂起，主会话已切）。
