# 派发单 Vision-F4：带图请求跳过利润门（方案 §3.6 执行遗漏整改）

## 背景
方案：`docs/capability-routing-plan.md` v3.1 §3.6（已过闸①）明文：
「`RequireVision` 请求沿用生图惯例——图片类请求**不进利润门**……避免分流因
利润门被 veto」。消费端闭包审查（2026-09-27 主会话）发现实现未覆盖：
- handler 文本入口先行装门（`WithOpenAIRequestPricingContext` → 装门），
  调度器防御装门条件只看 `requiredImageCapability == ""`
  （`openai_account_scheduler.go:2373`），带图文本请求照常装门 →
  候选过滤（:1944）与抢槽后终检都会 veto 视觉分流目标账号。

**修法（主会话裁定）**：不改装门条件，复用既有「门范围外」显式标记机制
`service.WithOpenAIProfitControlSuppressed`——所有装门点（handler 首装、调度器
防御装门、WS turn 重装 `WithOpenAITurnPricingContext`）均已尊重该标记
（携带标记时只固定 pricingAt、不装门；veto 无门即放行）。带图请求在入口
打上标记即可全链跳过，与独立图片端点/Grok 媒体/count_tokens/live 同构。

## 改动范围
1. **枚举装门入口**：`grep -rn "WithOpenAIRequestPricingContext" internal/handler/
   --include="*.go"`（非测试），逐点判断该入口的请求是否可能进入
   `requireVision=true` 调度（以 A 单 `HasOpenAIInputImage` / 含图输入 hint 的
   可达性为准；embeddings / alpha_search 无图片输入，不动）。
2. 对**可能带图**的入口（预期至少：chat_completions :164、openai_gateway_handler
   :639 / :1260 / :2614，以实际枚举为准）：当请求含图输入时，在调用
   `WithOpenAIRequestPricingContext` **之前**将 ctx 包上
   `service.WithOpenAIProfitControlSuppressed(...)`（顺序：先标记后装门，
   装门函数读标记跳过装门但仍固定 pricingAt）。不含图请求路径零改动。
3. 不改 service 层（`openai_profit_control.go`、调度器装门/ veto 逻辑零改动——
   标记机制已全链生效）。
4. 新增/扩展测试（handler 层，放 `internal/handler/` 既有测试组织方式内）：
   - 含图请求入口：高倍率账号不被利润门 veto（断言 ctx 无门 / veto 放行）；
   - 不含图请求：利润门行为与现状一致（防回归）。

## 禁区
- 不得改 `openai_profit_control.go`、`openai_account_scheduler.go`、前端、service 层。
- 不得改 embeddings / alpha_search / count_tokens / grok_media 等无图或已跳门入口。
- 不得改计费/定价路径（标记只关准入过滤，不影响定价与计费——既有语义）。
- 禁止兜底分支（合同 §5）；边界外发现 BLOCKED 顶回。

## 验证命令白名单（只允许跑以下命令）
- `cd backend && go build ./...`
- `cd backend && go test ./internal/handler/ -run 'ProfitControl' -count=1`
- `cd backend && go test ./internal/handler/ -run 'Vision|ImageInput' -count=1`

## Done when
- 上述入口逐点落地（枚举结果写进交付说明），`go build ./...` 通过。
- 新增测试 PASS：含图跳门 + 不含图防回归两方向都有断言。
- `git status --short` 快照改动仅限所涉 handler 文件 + 测试文件。

## 模型
- 执行模型：custom-local:deepseek-v4.1-flash（hy3 本轮挂起，主会话已切）。
