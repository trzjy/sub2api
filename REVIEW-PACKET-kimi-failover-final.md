# REVIEW-PACKET 终审（闸②）：Kimi 上游故障切换与熔断加固 — 实现审

审查类型：终审（审已提交 diff + 实现 vs 已批准方案的一致性）。
模型路由：走 codex-companion（gpt-5.6-sol）。

## 0. 外审须知

1. 共享合同 `/mnt/data/ai-tool-global-source/claude/authority-first-change-contract.md`。
2. **本 packet 自包含**（闸①第 3 轮第 1 条收敛项）：方案全文、diff 清单、验证证据
   均在本文，不必外跳（方案正文 `docs/kimi-upstream-failover-hardening-plan.md`
   v4 仅供深读）。
3. 禁叠补丁自查：请检查每处改动是"既有机制入口开放/参数调整"还是"为眼前问题
   打补丁"（合同 §5）。
4. P4（同源站整体切断）不在本 diff 内——维持独立阻断待用户裁定，勿审。

## 1. 已批准方案（闸①闭合版，摘要自包含）

背景：corealgos Kimi 号池（#52-#56，上游 infer 中转）故障期 CF 代答 520/522/524，
每跳 90~125s 串行 failover 跑不赢客户端 ~100s 首字节死线。根因（生产 DB 实证）：
健康熔断器本就覆盖 kimi（4 账号全被 L3 跳闸过），真问题是①参数与故障形态不匹配
（4次/5min 阈值被 90s+ 失败延迟摊薄）②跳闸后 5min 冷却+5 次探测全落故障期③
fastpath 单发瞬态冷却（含 500/502/503/504/520-524）被平台门禁挡住 kimi 吃不到。

用户需求 6 条 + 裁定累积（裁定 1-6，2026-09-26）：P2=60s；重复计费接受（本地不
重复计费）；恢复=复用既有探测；同账号重试默认 3→1；健康熔断器 2次/5min+15min
（纯配置，6 平台共享，派生 probe.max_attempts≥15）；流程裁定=闸①以消化记录闭合。
范围：显式清单 kimi/deepseek/zhipu/minimax（apikey+oauth）；other/OpenAI/
anthropic/gemini/codebuddy 不动；gemini 链上限语义保留。

定位声明：全部落点为项目作者既有机制的入口开放与参数调整，无新建平行机制。

## 2. 提交 diff 清单（四张派发卡 + 整改 + 机械收尾）

| 卡 | 内容 | 文件 | 验收状态 |
|---|---|---|---|
| 1 | P1a fastpath 门禁拆除 | openai_account_runtime_block_fastpath.go（新增 isCNFamilyFastpathAccount 谓词；瞬态冷却门禁/BlockAccountScheduling 写入/两处读取早退放开）+ 测试 4 个 | ✅ 4 测试实跑全绿 |
| 2+2b | P3a/P3b：maxAccountSwitches 旧链归零 + 重试默认 3→1 | failover_loop.go（MaxSwitches 删除、耗尽改候选集合自然判定、503 退避分支删除、maxSameAccountRetries 3→1、新增 NewFailoverStateCapped 供 gemini）、gateway_handler*.go、config.go、account.go、grok_media.go 等旧链清理 + 测试适配 | ✅ 残留扫描归零（gemini 变体/429 storm 独立机制除外）；cap 语义双断言测试绿 |
| 3 | P1c 探测恢复 | **零码改**（裁定：P1a 瞬态 TTL 10s/45s<探测间隔 60s 自然到期即恢复；长冷却 L3 marker 已在既有 probeRecoverableMarkers，生产实证探测已跑） | ✅ BLOCKED 判断正确，方案已回写 |
| 4 | P2 CN 首包 60s | 新增 openai_cn_first_byte_timeout.go（watchdog：独立 context+定时器；errOpenAICNFirstByteTimeout ≠ context.Canceled；body 首字节停表）；doOpenAIUpstream 唯一注入点（非 CN 直通）；chat/messages 接线；504 映射 NextAccountRetry+卡 1 冷却链；clientOutputStarted 门控（响应头已下发不透明切换）| ✅ 12 测试全绿（三场景+错误区分+非 CN 直通）|
| 机械收尾（主会话） | ①unit 套件先在断裂修复：payment_config_plans_validation_test.go ptrStr/ptrInt 重声明→改名 pcValPtr*（与生产 usage_risk_analysis_service.go 及 usage_risk_rules_test.go 冲突，先在问题）②repetition_breaker.go 两处陈旧注释更新 ③openai_models_list.go 参数名去旧链化（行为不变，<=0→默认 3）④TestFailoverStateCapSemantics 补 gemini 上限断言 | | ✅ |

## 3. 验证证据

- 定向：`go test -tags unit ./internal/service/ -run 'FirstByte|Timeout' -count=1` → ok 37.2s；
  handler `-run 'TestFailoverStateCapSemantics|Failover|Cancellation|Switch' -count=1` → ok 16.0s；
  service `-run 'PoolModeRetry|ModelsList'` → ok；CN fastpath 4 测试 → 全绿。
- 收敛全量：`go build ./...` → **通过**；`go test -tags unit ./... -count=1`
  被主机内存压力终止（系统级 kill，非测试失败）——**如实登记为环境限制未验项，
  待低负载窗口补跑**；已收集的定向证据见上两行（受影响三包 handler/service/config
  中，handler 与 service 已有过滤全绿证据，config 无单测）。
- 残留扫描：`maxAccountSwitches` 全仓 grep → 仅剩删除记录注释/方案文档/gemini
  变体/429 storm 独立机制（openAIOAuth429StormMaxAccountSwitches=1，项目作者既有
  429 风暴保护，非全局上限，未动）。

## 4. 两项预闭环自审结果（供对抗性挑战）

**执行遗漏核对**：需求 ①轮询不限个数→P3b 候选集合语义+cap 测试；②快速切换→
P3a 3→1+P1a 快速冷却+P2 60s；③恢复即启用→短冷却自然到期+长冷却既有探测
（P1c 零码改裁定）；④60s→常量+注入点；⑤同源站切断→P4 独立阻断未实施；⑥
根因→已查明更正（v3 记录）。全部落地或有明确阻断记录。

**全局一致性**：平行路径——model 级瞬态冷却内部无第二道平台门禁（已核
recordOpenAIAccountModelTransientFailure）；失效逻辑——CN 账号级块均有持久化
字段背书（fail-open 交互已核实）；旧入口——doOpenAIUpstream 唯一注入点，非 CN
直通原路径；DTO——无 API 契约变更；gemini 链——NewFailoverStateCapped 原样保留
上限语义（2b 整改项）。

## 5. 已知风险与拒绝项留痕

- gemini 越界（卡 2 执行会话删其上限）已在 2b 整改恢复；grok_media 本地 cap 删除
  判定为旧链合法延伸（grok 属 OpenAI 家族链）。
- 拒绝项（闸①留痕沿用）：剩余 deadline 预算扣减（§1 零假设）；P1b 指标仪表盘
  （§5 改动最小化）。
- 未验项：P1b 为生产配置操作（阈值/冷却/probe.max_attempts），**待部署窗口执行**，
  不在本 diff；生产实测证据按 docs/evidence-filing-standard.md 部署后落盘。

## 6. 请外审回答

> **第 1 轮终审消化记录（2026-09-26，第 1 次终审 block 7 条）**：#1 gemini 第三
> 调用点无上限=采纳（整改）；#2 冷却链空模型假闭环=采纳（模型必填贯穿+端到端
> 测试）；#3 watchdog 消费者未闭包=采纳（主路径 chat/messages/passthrough/
> responses/web 系列闭合，embeddings/alpha_search/codebuddy NoWatchdog 收窄恢复
> 既有行为）；#4 候选集合冻结=**拒绝**（过度设计：新机制且请求期间恢复账号可被
> 尝试恰服务于需求①，合同 §5；方案措辞已修正为"每账号至多一次+失败排除列表"）；
> #5 watchdog 取消/超时竞态=采纳（父取消优先+原子单胜者+3 组竞态测试）；
> #6/#7 GPT 定价变更（.gitignore + gpt-price-fx-*）=**越界归属**——该等工作流
> 在途文件先于本任务存在于工作区，不属本 diff，不捆绑提交，发现转记该工作流。
> 整改执行=派发单 5（报告 docs/dispatch/2026-09-26-kimi-failover/report-5.md）。

> **第 2 轮终审消化记录（2026-09-26，7 条）**：#1 native anthropic 两消费者
> （事发主路径）首字节前提交 200+超时正常 finalize=采纳（惰性头+头前切换/
> 头后流中断错误事件+4 两阶段测试+mutation 反证）；#2 fallback 读取助手先写
> 502 再转 failover=采纳（助手先返回错误、调用方优先映射）；#3 watchdog 拿到
> detach 后 context 无取消信号=采纳（doOpenAIUpstream 增加 clientCtx 参数，
> 原始客户端 context 参与唯一胜者裁决）；#4 profit_veto 旧退避测试=采纳（按
> 无退避语义重写）；#5 .gitignore hunk=裁定（另一工作流；本任务提交时仅窄
> 放行 kimi 方案文档）；#6 旧退避链残留（singleAccountBackoffDelay/
> profitVetoedAccountIDs/allExclusionsAreProfitVetoed）=采纳（已删，grep 零
> 匹配）；#7 watchdog EOF/Close 泄漏=采纳（显式 Close+release）。本轮无拒绝
> 项。整改执行=派发单 6（报告 report-6.md）；验证改**全包** -count=1
> （handler 32.5s / service 194.2s 双绿——第 1 轮过滤式验证盲区已修正）。

> **第 3 轮终审消化记录（2026-09-26，5 条）**：#1 CN 流式无 body 时默认 10s
> keepalive 先提交响应打穿换号窗口=采纳（firstByteSeen 标志，CC/Messages 心跳
> 在首字节前跳过、首字节后恢复，非 CN 不变）；#2 clientCtx 整体装回 request
> 丢 transport profile=采纳（wctx 改从 request.Context() 派生保留
> HTTPUpstreamProfileOpenAI，clientCtx 仅作独立取消信号）；#3 parentDone 检查
> 与 CAS 间窗口=采纳（三路纯 CAS 唯一胜者：取消/首字节→settled、定时器→
> timedOut，context.Canceled 不再转译内部超时）；#4 onTooLarge→nil 过度归并
> 破坏原 endpoint 错误契约=采纳（writeError 恢复，超限/解析/读取三类原契约
> 各自回写，仅内部超时原样返回走 failover）；#5 .gitignore=提交期裁定（gpt-
> price 两行属另一工作流在途改动，提交本任务时从暂存拆出；kimi 方案放行行
> 保留）。整改执行=派发单 7（report-7.md，含 6 次 mutation 反证）；全包
> -count=1 双绿（handler 29.3s / service 200.8s）。

> **第 4 轮终审消化记录（2026-09-26，5 条）**：#1 native anthropic keepalive
> ping 第三处路径（CC/Messages 已修）=采纳（同模式 FirstByteSeen 抑制+回归
> 测试）；#2 watchdog 已定胜者被 parentDone 覆盖=采纳（错误分类只读原子终态，
> Fired() 先行，"超时先赢、客户端后取消"确定性测试）；#3 旧 interval ticker
> （合法配置 30..59s）在 watchdog 前抢跑=采纳（首字节前 tick 不裁决，由 60s
> 边界统一负责；首字节后 interval 语义不变）；#4 failover_loop 注释"候选集合
> 即为边界"与动态调度矛盾=采纳（改为"失败账号至多一试+排除集耗尽判定"）；
> #5 .gitignore gpt-price 两行=提交期裁定（另一工作流，提交时拆出）。
> 主会话验收补正：interval 测试 :613 断言与流中断返回契约错位（生产行为正确：
> 504 failover+响应头未提交+零 usage；该函数各类流中断均返回非 nil result 带
> 出 usage）——机械断言修正，非生产行为变更。整改执行=派发单 8（report-8.md）；
> 全包 -count=1 双绿（handler 31.9s / service 210.3s）。

**第 5 轮请外审回答（复审收敛性）**：

> **第 5 轮终审消化记录（2026-09-26，3 条 must_fix）**：#1 grok_media eligibility
> 分支"未加排除集→无限循环"=**拒绝**（事实前提错误：`grok_media.go:283` 就在该
> 分支内写入 `failedAccountIDs`；调度器主过滤 `openai_account_scheduler.go:1453`/
> 粘性 `:513`/加权回落 `:1285` 三处消费排除集；唯一候选排除后必经
> `ErrNoAvailableAccounts` → `grok_media.go:226-231` 返回 503 终止。grok_media.go
> 列入整改禁区零改动）；#2 watchdog 标记被包装链遮蔽+Forward keepalive 无抑制=
> 采纳（载体接口 `cnFirstByteTimeoutBodyCarrier` 穿透 openAIRequestContextReadCloser
> 与 responsesClientToolStreamBody 两层包装；response_handling keepalive case 补
> FirstByteSeen 抑制）；#3 responses native 空流 EOF 头不提交=采纳（finalize 后
> `!clientDisconnected` 时 `writeStreamHeaders()+Flush`，与 CC native 收尾同语义）。
> 整改执行=派发单 9（report-9.md；hy3 额度 429 切 custom-local:deepseek-v4.1-flash，
> 中转 502 一次后接手盘点半成品续做）。**主会话验收补强**（合同 §3 平行路径闭合，
> report-9 §6）：第 4 轮已裁定的 interval tick 首字节前抢跑在 response_handling
> `:896` 存在同构残口——同模式抑制+回归测试+mutation 反证（摘除→FAIL/恢复→PASS）。
> 定向全绿（CNFirstByteTimeout 16.8s；Responses/NativeAnthropic 4.5s）；全包
> -count=1 双绿由主会话重跑取证。

**第 6 轮请外审回答（收敛裁定轮）**：
1. 第 5 轮 2 条整改（载体接口穿透+空流头提交+Forward keepalive/interval 抑制）
   是否闭合？载体接口设计是否符合"既有机制入口开放"而非新机制？
2. #1 的拒绝依据是否成立（排除集已在分支内写入且调度器三处消费）？
3. 若无 must_fix 级实质缺陷，请明确给出可收口结论；机械问题（注释、文案、
   风格）按合同 §6 本地修复，不触发重审。

> **第 6 轮终审消化记录（2026-09-26，1 must_fix + 1 residual_risk）**：
> #1 native anthropic 流路径 `clientOutputStarted` 本地标志与真实下游提交状态
> 脱节=**采纳（机制描述修正后按实质采纳）**：外审称"扫描上游 body 之前沿用旧
> 逻辑提交 200/SSE 响应头"与事实不符——函数开头 `writeAnthropicPassthroughResponseHeaders`
> 只写 header map 不提交（gin Writer 首次 Write 才提交）；真实缺口为空行分支
> `writeStreamLine("")` 写出 `"\n"` 即提交响应头而本地标志只认非空行不置位，
> 与外层换号门 `c.Writer.Written()` 脱节。修复：CN 超时判定改用同一状态源
> `openAIStreamClientOutputStarted(c, clientOutputStarted)`（与 response_handling
> `:455` 既有模式一致，非新机制）；回归测试
> `TestOpenAICNFirstByteTimeout_NativeAnthropicBlankLineCommitUsesRealState`
> 确定性模拟 expire 先于 firstByte 赢得 CAS 的边界竞态（先等 watchdog 到期再吐
> 空行），mutation 反证：恢复旧判定→FAIL（返回 UpstreamFailoverError 504），
> 恢复修复→PASS。#2 models list fallback 固定 3 次预算=按外审第二选项处置：
> 方案文档 P3 节记录为有意边界（辅助探测路径，非平台请求 failover 链），不改码。
>
> **验收自审发现并闭合 card-2 旧链缺口（阻塞级）**：重跑 `go build ./...` 暴露
> card-2（P3b 切换上限旧链归零）consumer closure 未闭合——config 字段已删但
> `openai_gateway_handler.go`（:50 字段/构造器/Responses/:622、messages/:1255、
> WS ingress/:2547 三内联循环的上限门与日志字段）与 `openai_chat_completions.go`
> （:153/:365/:378）仍引用已删字段，handler 包无法编译（此前验收构建只覆盖
> service 包）。处置=card-2 已批准语义的闭合：上限门整体删除，换号仅记录排除集
> +switchCount++（429 风暴守卫与日志继续消费），耗尽回归调度无候选判定；gemini
> 链与 429 风暴守卫（禁区）原样保留。残留扫描（gemini 变体除外）仅余注释与
> 禁区常量。定向：handler `Failover|Cancellation|Switch` ok 15.9s +
> CNFirstByteTimeout ok 18.8s。全包收敛重跑取证：`go build ./...` 通过；
> service `-tags unit -count=1` ok 213.1s；handler `-count=1` ok 29.4s
> （`-skip` 两个非本任务测试：未跟踪外来 WIP image-input-hint 的 handler 测试，
> service 助手已落盘而 handler 接线未实现，属并发会话半成品，不代实施不改其
> 文件，不计入本任务验收口径——report-9 §7.3 有披露）。
>
> **第 7 轮请外审回答（收口确认轮）**：第 6 轮 must_fix 整改（真实提交状态判定
> +回归测试+mutation 反证）、card-2 旧链缺口闭合、models list 有意边界记录是否
> 闭合？若无 must_fix 级实质缺陷，请明确给出可收口结论（用户裁定：机械问题本地
> 修复不触发重审）。

> **第 7 轮终审消化记录（2026-09-26，1 条，范围外→拒绝，收口轮）**：
> 唯一 must_fix 指向 `openai_has_image_input_hint_test.go`（"两入口未写图片输入
> hint"）——该测试与 `service/image_input_detect.go(+test)` 同属**未跟踪外来
> WIP**（并发会话半成品，本任务九卡范围外，report-9 §7.3 已披露，收敛验证已
> `-skip` 取证）。**拒绝留痕**（合同 §6）：①范围权威——`git status ??` 客观
> 证明非本任务 diff，属主为并发会话；②合同 §2 方案唯一 + §5 改动最小化——
> 代为实施无方案/无派发单/无用户裁定的外部特性构成 over-implementation（复审
> 三项完整性之阻断项），AGENTS.md"超出范围的发现一律 BLOCKED 顶回"；③验收
> 口径——本任务九卡 Done-when 与用户 6 条需求均不含该测试，其失败不影响验收。
> reviewer 对树状态的描述属实（service 助手已落盘、handler 接线缺失），已在
> §7.3 如实登记，特性完整性由其属主会话闭合。
>
> **防循环裁定（合同 §6）**：kimi 范围 diff 在第 7 轮零发现；上轮后无实质修改
> 不得再送审——**第 7 轮为收口轮，终审循环闭合**。七轮轨迹：7→7→5→5→3→
> 1+1→0（kimi 范围）。
