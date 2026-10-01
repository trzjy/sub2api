# 方案：composite 聚合分组接纳 other 平台账号的 model_mapping 归属（限时福利全模型整改收尾）

日期：2026-10-01 · 状态：待方案审外审 · 作者：主会话

## 用户裁定（权威来源）
1. （2026-10-01）账号按各自平台创建；**还没有平台的模型统一创建到 other 平台**；本批 qwen/deepseek
   福利账号纳入限时福利分组（id=94）。
2. （2026-09-30 15:29-15:48）限时福利分组不过滤任何模型；聚合分组内所有模型都应可用。
3. （2026-10-01）若昨日改造未完成，继续整改完毕。

## 现状与根因（已实证）
- 线上已部署 98134f330（cgate 三级解析）。deepseek-v4.1-flash（18 个 platform=deepseek 账号）
  经 ownership 层放行，活体 200 ✅。
- qwen3.8-flash（5 个 platform=other 账号，model_mapping={qwen3.8-flash: qwen3.8-flash:free}）
  400 "Model is not supported by composite groups" ❌。
- 根因：composite 三级解析的 ownership 归属层与模型目录扫描点用 `isConcreteRequestPlatform`
  过滤账号平台，白名单（anthropic/openai/gemini/antigravity/grok/kimi/zhipu/deepseek/minimax）
  不含 other → other 平台账号被跳过 → 三级解析全不命中。
- 分组层已合规：group 94 platform=composite、model_allowlist.enabled=false（不过滤）、
  绑定=23 个福利账号。

## 语义依据（other 平台账号已是既设计的安全类型）
- `backend/internal/service/account.go:867`：other 无内置模型目录，模型**完全来自账号
  model_mapping，空映射 = 无可服务模型**（既有外审-3 加固）。
- 调度层（`gateway_scheduling.go` selectAccountForModelWithPlatform）对平台做纯字符串匹配 +
  `isModelSupportedByAccountWithContext` 校验，other 可原生走通；forward 时由既有
  model_mapping 机制转换为上游模型名（deepseek 路径已同机制实证）。

## 改动清单（唯一方案）
新增窄域谓词（不动 `isConcreteRequestPlatform` 全局语义），仅用于"账号经显式 model_mapping
归属模型"的扫描点：

```go
// isCompositeOwnableAccountPlatform：composite 归属/模型目录扫描可采信的账号平台。
// = isConcreteRequestPlatform ∪ {other}。other 无内置目录，仅凭显式 model_mapping 归属
// （用户裁定 2026-10-01：无平台模型统一归 other，聚合分组必须可用）。
func isCompositeOwnableAccountPlatform(platform string) bool
```

接入点（4 处，全部是"扫描账号 model_mapping 判归属/目录"的语义）：
1. `backend/internal/service/gateway_service.go:1489` resolveCompositeModelOwnership——调度/门禁
   共用的归属层（整改主目标）。
2. `backend/internal/service/composite_route_resolver.go:74` resolver Matched 分支的平台守卫——
   ownership 命中后的二次校验，同步放行 other（否则 1 的产出仍被拒）。
3. `backend/internal/service/openai_codex_models_service.go:1045`——composite 分组 /v1/models
   目录扫描点 A。
4. `backend/internal/service/openai_codex_models_service.go:1200`——目录扫描点 B（qwen3.8-flash
   必须出现在分组模型列表，才算"支持所有模型"闭环）。

## 禁区（不做）
- 不修改 `isConcreteRequestPlatform` 本体及其余消费点（admin_group.go:425 组来源校验、
  channel_service.go:360 计费渠道、model_plaza_service.go:180 模型广场、
  openai_codex_model_metadata.go:139 元数据）——避免 other 泄漏进计费/广场/元数据面。
- 不修改显式路由 target 校验（admin_group.go:345）：other 模型一律经 model_mapping 归属路由，
  显式路由目标仍限具体平台（方案唯一，不新增第二路由机制）。
- 调度层零改动（纯字符串匹配，已实证可承载 other）。
- 不改 Group model_allowlist 等分组配置（已合规）。

## 边界语义（保持）
- 归属歧义：同一模型被多平台账号同时 claim → Ambiguous → 400（既有语义，不变）。
- 归属失败关闭：目录查询错误时 unknown alias 报错、可识别模型回落检测器（既有语义，不变）。
- other 空映射账号：不可归属任何模型（既有加固，天然安全）。

## 验收口径
1. 定向单测：新增用例 ≥4——① other+显式映射账号使门禁 `compositeTargetPlatformResolved` 放行且
   盖章平台=other、UpstreamModel 传播正确；② other 空映射账号不产生归属；③④ composite 分组
   /v1/models 目录**扫描点 A 与 B 分别**构造定向用例（参数化或两用例，强制各自经过
   openai_codex_models_service.go:1045 与 :1200 分支），逐一断言 other 显式映射收录、空映射
   排除——单一用例不允许同时覆盖两点（外审 2026-10-01 must_fix：防止仅一路径切换时测试仍绿）；
   ⑤ other 账号与具体平台账号对**同一模型别名**各自显式映射时，ownership 判 Ambiguous 且
   门禁 400（不得按遍历顺序择一）（外审 2026-10-01 复验 must_fix：歧义语义回归）；
   ⑥ 表驱动负向用例：非 other 的未知平台账号（如 "unknownplat"）即使配置有效
   model_mapping，谓词返回 false、不产生 ownership、不进入 A/B 两目录扫描结果
   （外审 2026-10-01 复验 must_fix：锁定谓词严格等于白名单∪{other}，排除"非空即放行"
   类实现漂移）。
   既有 composite 用例全绿。
2. 类型检查 `go build ./...` + `go vet`。
3. 活体验证（主会话执行）：部署后 qwen3.8-flash 经分组 94 用户 key 返回 200；
   /v1/models 列表含 qwen3.8-flash 与 deepseek-v4.1-flash。

## 派发
单一职责改动簇：谓词 + 4 接入点 + 测试共享同一新符号，存在共享状态，按派发粒度规则
合并为一张派发单（拆分会引入符号定义顺序依赖，无法并行）。
