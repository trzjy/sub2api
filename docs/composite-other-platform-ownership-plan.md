# 方案：composite 聚合分组接纳 other 平台账号的 model_mapping 归属（限时福利全模型整改收尾）

日期：2026-10-01（v3 追补接入点 7） · 状态：✅ 已完成——7 接入点全部落地上线（b8e01d343），五入口活体验收全绿 · 作者：主会话

## 用户裁定（权威来源）
1. （2026-10-01）账号按各自平台创建；**还没有平台的模型统一创建到 other 平台**；本批 qwen/deepseek
   福利账号纳入限时福利分组（id=94）。
2. （2026-09-30 15:29-15:48）限时福利分组不过滤任何模型；聚合分组内所有模型都应可用。
3. （2026-10-01）若昨日改造未完成，继续整改完毕。

## 现状与根因（已实证）
- **线上当前 = b8e01d343（谓词 + 接入点 1-7 全部落地，任务闭环）**。历史复测节点：98134f330
  （v1 前基线，qwen 400 根因=isConcreteRequestPlatform 白名单不含 other）；db456b3d1
  （接入点 1-4 落地，活体暴露接入点 5/6）；cd2ced8f0（接入点 5/6 落地，活体暴露接入点 7）。
- qwen3.8-flash（5 个 platform=other 账号，model_mapping={qwen3.8-flash: qwen3.8-flash:free}）
  历史过渡证据：98134f330 时代 400 "Model is not supported by composite groups"；
  db456b3d1 400 "…not supported by this OpenAI-compatible endpoint…"（接入点 5 拦截，
  已随 cd2ced8f0 闭合）；cd2ced8f0 /v1/messages 403 组开关（接入点 7，已随 b8e01d343 闭合）。
  **b8e01d343 终态活体（2026-10-01）**：五入口全 200 + /v1/models 双模型，验收口径 3 全过。
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

接入点（7 处，全部是"composite 对账号平台/归属的判定"语义；接入点 1-6 已落地部署）：
1. `backend/internal/service/gateway_service.go:1489` resolveCompositeModelOwnership——调度/门禁
   共用的归属层（整改主目标）。【已落地 db456b3d1】
2. `backend/internal/service/composite_route_resolver.go:74` resolver Matched 分支的平台守卫——
   ownership 命中后的二次校验，同步放行 other（否则 1 的产出仍被拒）。【已落地 db456b3d1】
3. `backend/internal/service/openai_codex_models_service.go:1045`——composite 分组 /v1/models
   目录扫描点 A。【已落地 db456b3d1】
4. `backend/internal/service/openai_codex_models_service.go:1200`——目录扫描点 B。【已落地
   db456b3d1】
5. `backend/internal/handler/openai_gateway_handler.go:336` openAICompatibleTextTargetAllowed
   ——OpenAI 兼容端点级 composite 目标平台 allowed 白名单（openai/grok/kimi/zhipu/deepseek/
   minimax），补 PlatformOther。**活体实证（2026-10-01，db456b3d1 部署后）**：归属层放行后
   该门禁 400 "Model is not supported by this OpenAI-compatible endpoint for composite
   groups"；other 定义即通用 OpenAI 兼容上游，chat/completions 是其原生协议，同类接入。
   覆盖 5 个调用点：chat_completions.go:82、openai_gateway_handler.go:476/:1221、
   openai_gateway_count_tokens.go:63/:240。【v2 已落地 cd2ced8f0】
   **五入口放行的实施前逐入口证据（外审 v2-R5 P1：转发层 other 适配分支全部已存在，
   本方案零新增转换路径，仅拆门禁）**：
   | 入口 | 既有 other 转发实现 | 证据 |
   |---|---|---|
   | /v1/chat/completions | 原生 CC 协议直转 | account.go:1494 GetAPIProtocol 对 other 默认 chat_completions；组 94 全 23 账号 DB 实证 api_protocol=chat_completions+自定义 base_url |
   | /v1/responses | Responses→CC 转发 | openai_gateway_forward.go:1452 shouldForwardOpenAIResponsesViaRawChatCompletions 显式 PlatformOther→true（注释：other 是通用 CC 自定义上游，一律按 CC 转发） |
   | /v1/messages | Anthropic→CC 转换+CC→Anthropic 回桥 | openai_gateway_messages.go:104 ForwardAsAnthropic 经同一谓词命中 forwardAnthropicViaRawChatCompletions |
   | /v1/responses/input_tokens | 本地估算 | openai_gateway_count_tokens.go:154 shouldEstimateOpenAIInputTokensLocally 显式 PlatformOther→true |
   | /v1/messages/count_tokens | 本地估算返回 {"input_tokens":N} | openai_gateway_count_tokens.go:272 ForwardCountTokensAsAnthropic 显式 IsCNProvider||PlatformOther→estimateAnthropicCountTokensLocally（注释点名 other） |
6. `backend/internal/handler/gateway_handler.go:1313` compositeAvailableModels 平台循环
   ——composite /v1/models 清单枚举硬编码 9 个具体平台，补 PlatformOther
   （GetAvailableModels 纯字符串匹配+收集 model_mapping 键，other 原生支持）。
   【v2 已落地 cd2ced8f0】
7. `backend/internal/handler/openai_gateway_handler.go:311` allowOpenAICompatibleMessagesDispatch
   ——composite 分组 /v1/messages 与 /v1/messages/count_tokens 的派发豁免：现豁免
   target∈{grok∪CN 供应商}，other 落到组 AllowMessagesDispatch 开关（组 94=false）→403。
   补 `platform == PlatformOther` 进豁免（与 CN 同语义：other 走
   forwardAnthropicViaRawChatCompletions 既有转换链，openai_gateway_messages.go:104 证据）。
   **活体对照实证（2026-10-01，cd2ced8f0 部署后）**：qwen /v1/messages 403 组开关 vs
   deepseek 同入口 200（CN 豁免路径可用）——非组级统一开关，是第七处平台判定。
   覆盖 2 调用点：Messages(openai_gateway_handler.go:1185)、CountTokens
   (openai_gateway_count_tokens.go:199)。【v3 已落地 b8e01d343，实际行号 329】

## 禁区（不做）
- 不修改 `isConcreteRequestPlatform` 本体及其余消费点（admin_group.go:425 组来源校验、
  channel_service.go:360 计费渠道、model_plaza_service.go:180 模型广场、
  openai_codex_model_metadata.go:139 元数据、admin_group.go:200）——避免 other 泄漏进
  计费/广场/元数据面。
- 不修改显式路由 target 校验（admin_group.go:345）：other 模型一律经 model_mapping 归属路由，
  显式路由目标仍限具体平台（方案唯一，不新增第二路由机制）。
- **embeddings 端点不动**（openai_embeddings.go:76 compositeTargetPlatformAllowed 限
  PlatformOpenAI 单平台）。权威边界（外审 v2 P1 要求可核验，非假设）：
  ①组 94 全部 23 账号的 model_mapping 实证只有 deepseek-v4.1-flash 与 qwen3.8-flash 两个
  chat 模型（DB 查询 2026-10-01），无任何 embeddings 模型；②该端点对 composite 分组
  **现对包括 deepseek 在内的所有目标平台**都只放行 target=openai——福利 deepseek 模型
  今天同样被它拒绝，other 不构成差别对待，属既有统一能力面语义；③"所有模型可用"裁定
  的语境是福利 chat 模型入口。若未来组内出现 embeddings 映射需求，另行走变更。
- isResponsesWebSocketCompositePlatform（openai_gateway_handler.go:346）不动：WSv2 通道
  已有既定设计排除先例——CN 供应商（kimi/zhipu/deepseek）因 WSv2 ingress transport
  过滤同样被排除（代码注释 342-345），福利 deepseek 模型在 WSv2 上同样不可用；
  other 沿用同一排除面，非新设差别对待。
- 调度层零改动（纯字符串匹配，已实证可承载 other）。
- 不改 Group model_allowlist 等分组配置（已合规）。

## 边界语义（保持）
- 归属歧义：同一模型被多平台账号同时 claim → Ambiguous → 400（既有语义，不变）。
- 归属失败关闭：目录查询错误时 unknown alias 报错、可识别模型回落检测器（既有语义，不变）。
- other 空映射账号：不可归属任何模型（既有加固，天然安全）。

## 验收口径
1. 定向单测（v2 增补针对接入点 5/6）：
   - ① other+显式映射账号使门禁 `compositeTargetPlatformResolved` 放行且盖章平台=other、
     UpstreamModel 传播正确；② other 空映射账号不产生归属；
   - ③④ /v1/models 目录**扫描点 A 与 B 分别**构造定向用例（参数化或两用例），逐一断言
     other 显式映射收录、空映射排除——单一用例不允许同时覆盖两点；
   - ⑤ other 账号与具体平台账号对同一模型别名各自显式映射 → Ambiguous → 400；
   - ⑥ 表驱动负向：非 other 的未知平台即使配置有效 model_mapping，谓词 false、不产生
     归属、不进 A/B 目录扫描；
   - ⑦ **v2**：openAICompatibleTextTargetAllowed 对 decision.TargetPlatform=other 放行
     （handler 层单测）；⑧ **v2**：compositeAvailableModels 含 other 账号 model_mapping
     键、不含未知平台映射键；
   - ⑨ **v3**：allowOpenAICompatibleMessagesDispatch **表驱动**（外审 v3-R2：单点
     other=true/openai=false 锁不住集合，误写 `platform != PlatformOpenAI` 仍会绿）——
     composite 分组 AllowMessagesDispatch=false 时逐一断言：豁免集合
     {Grok, Kimi, Zhipu, Deepseek, MiniMax, Other} 全部 allow=true；其余具体平台
     {Anthropic, Gemini, OpenAI, Antigravity} 及未知平台（unknownplat）全部
     allow=false；     另须含一例负向回归：**非 composite 分组**（如 platform=other 的普通分组）+
     AllowMessagesDispatch=false + Other → allow=false（锁定豁免仅发生在 composite
     分组条件内，防布尔/括号漂移把豁免移出分支）（外审 v3-R3 P2）。
两调用点（Messages:1185、CountTokens:199）均直呼该 helper
     无额外分支绕行（grep 实证）。
   - 接入点 5 的 5 个调用点同闭包证明：grep 实证 5 处均为
     `openAICompatibleTextTargetAllowed(c, apiKey, reqModel, h.coreGatewayService)`
     的无条件同一调用（chat_completions.go:82、openai_gateway_handler.go:476/:1221、
     openai_gateway_count_tokens.go:63/:240，前置均为 ensureCompositeTargetPlatform），
     helper 单测（⑦）统一覆盖 + 门禁后路径由 v1 活体背书（deepseek 经同一
     decision→调度链 200，调度层零改动）；qwen chat 路径由活体验收最终闭合。
   既有 composite 用例全绿。
2. 类型检查 `go build ./...` + `go vet ./internal/handler/ ./internal/service/`
   （SSOT 唯一命令口径。预存在 vet 漂移实证在 internal/server/routes/
   gateway_models_pinned_test.go:60（NewOpenAIGatewayHandler 少参，历史遗留，登记
   遗留项非本单范围），不阻断两目标包；`./internal/...` 与 `./...` 均会扫到该包，
   禁用）。
3. 活体验证（主会话执行，部署后，qwen3.8-flash 均经分组 94 用户 key）：
   - POST /v1/chat/completions → 200；
   - POST /v1/messages（anthropic 格式入口）→ 200【v3：接入点 7 落地后】；
   - POST /v1/responses → 200（若上游业务错误则失败即停如实上报，不得静默降级口径）；
   - count_tokens 两入口（/v1/responses/input_tokens、/v1/messages/count_tokens）→
     **正向断言**：HTTP 200 且响应体含数值型 input_tokens 字段（token-count 响应结构）；
     门禁文案负向匹配不作为通过依据（外审 v2-R5：防"换一种失败也判过"）
     【v3：/v1/messages/count_tokens 依赖接入点 7】；
   - /v1/models 列表含 qwen3.8-flash 与 deepseek-v4.1-flash。
   五入口与接入点 5 的 5 个调用点一一对应（ChatCompletions/Responses/Messages/
   ResponsesInputTokens/CountTokens，路由注册 gateway.go:85-262）。

## 派发
v3 接入点 7 为单行豁免条件追加+测试，与既有谓词/列表共享语义，单卡派发。
db456b3d1（接入点 1-4）、cd2ced8f0（接入点 5/6）已部署上线。

## 实施记录
- db456b3d1（2026-10-01）：接入点 1-4 + 8 测试函数，算力机 rycpu2 实现并独立复跑
  （build/vet/定向测试绿），GitHub 中心化部署 sub2api:db456b3d1-w 上线，断言全绿。
  活体：deepseek 200 ✅；qwen 400（接入点 5 拦截）+ /v1/models 缺 qwen（接入点 6 未
  枚举）→ 证明 4 点非闭包，扩点依据落本文件。
- cd2ced8f0（2026-10-01）：接入点 5/6 + ⑦⑧ 测试，同流程上线。活体：chat 200 ✅、
  responses 200 ✅、/v1/responses/input_tokens 200 {"input_tokens":2} ✅、/v1/models
  双模型 ✅；/v1/messages 403 组开关 vs deepseek 同入口 200（对照）→ 暴露接入点 7。
- b8e01d343（2026-10-01）：接入点 7（实际行号 329 单行豁免追加）+ ⑨ 表驱动测试
  （TestAllowOpenAICompatibleMessagesDispatch_CompositeOtherExemptionTable /
  _OtherExemptionScopedToComposite；负向用例漂移说明：:322 直连 other 分组豁免为 v2
  已落地禁区代码，以 openai 非 composite 分组等价表达同一语义）。v3 方案审连续两轮
  no blocking findings 后实施；算力机 rycpu2 实现并独立复跑（build/vet/定向测试绿，
  双端 md5 d704c194/dceac9dd 一致），GitHub 部署 sub2api:b8e01d343-w 上线，断言全绿。
  **终态活体验收（qwen3.8-flash 经分组 94 key）**：chat/completions 200 ✅、
  /v1/messages 200 ✅（接入点 7 闭合）、/v1/responses 200 ✅、
  /v1/responses/input_tokens 200 {"input_tokens":2} ✅、/v1/messages/count_tokens
  200 {"input_tokens":7} ✅（接入点 7 闭合）、/v1/models 含 qwen3.8-flash 与
  deepseek-v4.1-flash ✅——验收口径 3 全过，方案闭环。
