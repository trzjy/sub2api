# REVIEW-PACKET 终审（闸②）：Vision 图片能力分流 — 实现审

审查类型：终审（审已提交 diff + 实现 vs 已批准方案 v3.1 的一致性）。
模型路由：走 codex-companion（gpt-5.6-sol）。

## 0. 外审须知

1. 共享合同 `/mnt/data/ai-tool-global-source/claude/authority-first-change-contract.md`。
2. 本 packet 自包含：方案定案摘要、diff 清单、验证证据、预闭环自审、已知缺口均在本文。
   方案正文 `docs/capability-routing-plan.md` v3.1（闸①过审版）仅供深读。
3. 禁叠补丁自查：请逐处检查改动是"既有调度管线的能力扩展"还是"为眼前问题打补丁"（合同 §5）。
4. 工作区混有其他任务改动（已分类剔除，见 §2 末注）；本 packet 只审 Vision 域差异。
5. 生产验收项（§5.1-5.9 类）不在本 diff——迁移未上生产、种子检测未执行，如实登记为
   部署后事项，勿因未部署判 block。

## 1. 已批准方案（闸① v3.1 定案，自包含摘要）

**问题**：上游 infer（inferaiapi.com）对 `deepseek-v4.1-flash` 在 chat_completions 协议下
静默丢图（HTTP 200 + 正常文本、无错误信号、照常计费）；星思云站(137) 同组同平台实测可读图
（随机验证码对照实验：真值 681170，infer 0/2，星思云 2/2 精确读出）。

**定案主路径**（用户裁定 v3：手动一键检测 + 同平台可配置分流 + 计费 B 案）：
- **§3.2 含图检测**：chat_completions `image_url` / responses `input_image` 解析，产出 `hasImageInput`。
- **§3.3 能力标记**：新表 `account_model_capabilities`，键 `(account_id, upstream_model, protocol)`，
  `source=detect/manual`；**无记录=unknown 放行**（保守，存量账号不被排除）。缓存+失效广播。
- **§3.4 选号过滤**：调度请求加 `RequireVision`；`known && !supported → vision_not_supported` 排除；
  unknown 放行；全部选号入口首选+failover 重入均生效。
- **§3.5 手动一键检测**：随机验证码图精确匹配，四态分类（detect_failed 不落库 / supported 落 true /
  unsupported 落 false / manual_review 不落 false 待人工）；管理员触发，无自动学习。
- **§3.6 计费 B 案**：按原始模型定价（用户账单不变），`account_id` 记实际承接账号，价差平台自担；
  带图请求跳过利润门；优先级链=手动覆盖 > 分流规则 > 检测标记 > unknown 放行，
  **规则只收窄候选池绝不豁免能力校验**；规则命中但目标全不可用 → 无可用账号错误（不静默回落）。
- **§3.7 同平台分流配置**：分组级 `vision_target_account_ids`，配置校验强制目标 ⊆ 该分组账号
  （跨组拒绝）；变更即时生效。
- **§3.8 观测与开关**：`vision_routed` / `vision_detect_result` 日志、`vision_not_supported`
  filter reason；全局 kill-switch `vision_routing_enabled`，关闭回 v1 行为，秒级生效。

## 2. 提交 diff 清单（A/B/C/D 三轮派发 + F1-F5 整改）

| 单元 | 内容 | 主要文件 | 验收状态 |
|---|---|---|---|
| A | §3.2 请求含图检测 `HasOpenAIInputImage` + context hint | image_input_detect.go（新）、openai_chat_completions.go、openai_gateway_handler.go | ✅ 定向测试过 |
| B | §3.3 能力表：ent schema + 迁移 259 + repo + 缓存/失效广播 + service | account_model_capability{,_cache,_repo}*.go（新）、259_account_model_capabilities.sql（新） | ✅ 定向测试过 |
| C | §3.4 调度 `RequireVision` + `vision_not_supported` 过滤 + 全选号入口传参 | openai_account_scheduler.go、openai_gateway_service.go、handler 入口 | ✅（F1 修复测试编译后） |
| D | §3.5/§3.7 检测 service + VisionRouting 同平台分流 + 同组校验（迁移 260） | vision_detect_service.go、vision_routing_service.go、vision_routing_repo.go（新）、260_add_group_vision_routing.sql（新） | ✅（随 F3 验收） |
| F1 | 修复 C 单测试编译阻塞（构造函数 23 调用点补参，纯机械） | openai_account_scheduler_test.go 等 | ✅ vet 三包过 |
| F3 | §3.5 admin API 接线（单检/批检/手动覆盖 PUT + 路由 + wire） | vision_capability_handler.go（新）、routes/admin.go、handler/wire.go、wire_gen.go | ✅ build 过、7 场景测试过（含跨组拒绝） |
| E | §3.7 前端 UI：分组路由页分流配置 + 账号页检测按钮/能力弹窗 | GroupsView.vue、AccountsView.vue、VisionCapabilityModal.vue（新）、i18n、types | ✅ vue-tsc 过、契约与后端注册一致 |
| F2 | §3.7 调度侧候选池收窄（v2 契约：复用 VisionRoutingService 直读，失败关闭） | openai_account_scheduler.go:2384-2534、vision_routing 测试（新） | ✅ 9 场景测试全过 |
| F4 | §3.6 利润门整改：带图文本请求跳过装门（WithOpenAIProfitControlSuppressed） | openai_account_scheduler.go:2369-2372、handler 入口 | ✅ ProfitControl 定向测试过 |
| F5 | §3.8 kill-switch：`vision_routing_enabled`（默认开）收口调度唯一入口 | setting_gateway_runtime.go:225-246、setting_{parse,service,update,view}.go、scheduler.go:2376-2382 | ✅ 3 个开关场景测试过 |

范围外未混入（工作区分类核验）：gateway_key_billing_test、grok_media、openai_alpha_search、
openai_embeddings、openai_live、gateway_models_pinned_test、openai_account_scheduler_compact_test
等改动属其他任务，不在本审范围；`当前任务.md`/`AGENTS.md` 为任务文档与协作配置。

## 3. 验证证据

- 收敛全量（共享契约变更，合同 §5 情形①③触发，一轮只跑一次）：
  `go build ./...` → **通过**；`go test ./... -count=1` → **exit 0，52 包全绿，零 FAIL 零 panic**
  （日志 /tmp/vision-converge-fulltest.log）。
- 定向：`-run TestOpenAIAccountScheduler_VisionRouting` → 9 场景全过（候选池收窄 /
  收窄不豁免 / 无候选报错 / 未配置回退 / 不带图无副作用 / 读错失败关闭 /
  kill-switch 关→回退 V1 / 开→保持 / 未设默认开）。
- 定向：service+handler `-run 'ProfitControl'` 及 handler `-run 'VisionCapability|HasImageInputHint'` → 全过。
- 类型检查：`go vet ./internal/service/ ./internal/handler/ ./internal/repository/` → 零输出。
- 前端：`vue-tsc --noEmit` → 过（E 单验收记录）。

## 4. 两项预闭环自审结果（供对抗性挑战）

**执行遗漏核对**：§3.2 含图检测（A）；§3.3 表/协议维度/unknown 放行/缓存失效（B）；
§3.4 RequireVision 全入口（C+F1）；§3.5 四态分类+单/批/覆盖 API（D+F3，四态常量
vision_detect_service.go:20-27 实装）；§3.6 B 案计费零改动+利润门跳过（F4）+优先级链
（F2 `TargetStillSubjectToCapabilityFilter` 实测收窄不豁免）；§3.7 同组校验（F3
`TestGroupVisionRoutingCrossGroupRejected` 实测拒绝）+候选池收窄（F2）；§3.8 kill-switch
（F5）+`vision_routed` 日志（scheduler.go:2392）+`vision_not_supported` reason
（scheduler.go:1938）。§4.8 种子数据与 §5 生产验收 → 部署后事项，见 §5 已知缺口。

**全局一致性**：平行路径——选号入口收口 `selectAccountWithSchedulerOnce` 单点，
`vision_routing_repo` 刻意避开 ent codegen（既定设计，拒绝 ent 补列=第二读路径）；
失效逻辑——能力表缓存失效广播沿用 error_passthrough 二级缓存模式（B 单实装）；
旧入口——RequireVision=false 时调度行为零改动（`RequireVisionFalseNoEffect` 实测）；
DTO——分组 `vision_routing` 随 group create/update DTO 走，不新增端点；前端契约字段/
端点与后端 admin.go:542-544 注册一致（E 单核对）。消费端 §3.6 闭包 10 项逐条登记于
`当前任务.md`（8 项代码实证通过 + 2 项整改后通过）。

## 5. 已知缺口与拒绝项留痕（主会话如实登记，请外审定性）

1. **`vision_detect_result` 结构化日志未落点**（§3.8 观测项之一；检测结果经 API 响应
   返回管理员，但无服务端日志）。主会话初判：机械观测缺口，不影响语义，可整改或降级。
2. **§6"检测流量记入 usage_logs"未实现**：检测由 VisionDetectService 直连上游
   （账号真实 key），不经网关计费链，故无 usage_logs 记录。用户账单零侵入已满足，
   但"平台侧检测流量统计"缺观测面。主会话初判：需用户裁定接受或补记录。
3. **生产验收未执行**（迁移 259/260 未上生产、种子检测未跑、多实例缓存一致性需生产
   验证、端到端 CodeBuddy 发图实测）——闸②通过后按部署流程执行并按
   docs/evidence-filing-standard.md 落盘。
4. 拒绝项留痕：v2 F2 整改中拒绝 ent 补列方案（同一数据第二条读路径，违反方案唯一）；
   拒绝跨平台分流（用户裁定 v3）；无自动学习（外审闸①已证不可靠，用户裁定手动检测）。

## 6. 请外审回答

1. 实现 vs 方案 v3.1 是否存在未登记的语义偏离（over-implementation / deviation /
   fake implementation 三查）？
2. §5 缺口 1/2 的定性是否同意主会话初判？不同意请给闭合路径。
3. F2 v2 直读无缓存的失败关闭语义（读错→本次选号失败关闭）在高频带图流量下是否
   可接受，还是必须加降级？（注意：合同 §5 禁兜底，降级路径需权威依据。）
4. 其他阻断项（如有），逐条给出可执行的整改要求。

## 7. 终审消化记录（2026-09-27 第 1 次终审，10 条发现，裁定留痕）

外审输出归档：docs/review-packets/vision-final-gate2-20260927-review-output.md。

- P1-1 Upsert 覆盖 manual（repo:74-80）→ **采纳整改 R1**。依据：方案 §3.8 手动覆盖优先级最高。
- P1-2 能力读取错误被当 unknown 放行（service:158-163）→ **采纳整改 R2**。依据：合同 §5 禁兜底；
  §3.4 unknown 放行前提是"确认无记录"，源不可读必须失败关闭。
- P1-3 kill-switch 无写入入口 → **采纳整改 R3**。依据：§3.8 开关定位=回滚手段，无控制面写入即死配置。
- P2-4 子串匹配偏离精确匹配（detect:216-220）→ **采纳整改 R4**。依据：§3.5 权威判定"精确匹配"。
- P2-5 空目标规则静默回落（routing:95-107）→ **采纳整改 R5**。依据：§3.6"规则命中但目标
  不可用→无可用账号错误，不静默回落"。
- P2-6 vision_detect_result 日志未落点（主会话缺口①）→ **采纳整改 R4**。外审否定降级初判，依据 §3.8 明文。
- P2-7 检测流量不入 usage_logs（主会话缺口②）→ **采纳整改 R4**。依据 §6 明文，属执行遗漏非待裁定项。
- P2-8 前端暴露未实现协议选项 → **采纳整改 R5**。依据 §6"先覆盖 chat_completions"实现边界。
- P2-9 直读失败关闭的容量验证 → 采纳为部署期残留项（外审明确不得为吞错加降级），入生产验收清单。
- P2-10 迁移/种子/多实例/端到端部署验收 → 已在清单（本 packet §5.3），部署期执行。

无拒绝项。整改 5 单并行派发（R1-R5，文件边界互斥，≤5 并发合规）。

## 8. 整改轮（R1-R5）落地摘要 —— 第 2 次终审送审范围

8 条采纳发现的落地与验证（主会话实证，非执行会话声明）：

| 发现 | 落地 | 验证 |
|---|---|---|
| P1-1 manual 保护 | repo Upsert 三处 `existing='manual' AND EXCLUDED='detect'` 条件更新；Override 路径不受限 | 3 新场景测试过（manual 保持/detect 刷新/Override 可覆盖） |
| P1-2 失败关闭 | `ModelSupportsVisionInput`→`(supported, known, err)`；调度 :1942 读错→`vision_capability_unavailable` 排除候选，选号整体失败关闭；unknown 放行语义保留 | 3 场景 + 端到端"唯一候选读错→选号 nil"测试过；既有 9 场景不回归 |
| P1-3 kill-switch 写入 | `vision_routing_enabled` 纳入 UpdateSettingsRequest/DTO/view（全链 `*bool`，未携带不覆盖）；非法值 400 | 5 场景测试过（service 3 + handler 2） |
| P2-4 精确匹配 | `visionDetectCodeMatches`：剔除非数字后纯数字串恰等于验证码 | 3 场景（解释文本 supported/附加数字 unsupported/错数字 unsupported） |
| P2-5 空规则拒绝 | `ErrVisionRoutingInvalidConfig`：模型模式空/目标列表空拒绝写入 | 3 场景测试过 |
| P2-6 detect 日志 | 四态 + 落库失败路径统一 `slog("vision_detect_result")`，不含 key/验证码/正文 | 测试断言过 |
| P2-7 usage 记录 | 注入 `UsageLogRepository`，三态（传输错/HTTP 错/200）写 `user_id=0,api_key_id=0` 哨兵行，cost=0；Create 失败如实上抛。生产接线经 `service.ProvideVisionDetectService` provider（wire regen-safe），非 setter 手补 | 4 场景（含早返回不记、Create 错误传播）+ wire 构建过 |
| P2-8 协议收敛 | 前端协议选项仅 chat_completions | vue-tsc 过 |

收敛验证（整改后全量，一轮）：`go build ./...` ✅；`go vet` 三包 ✅；
`go test ./... -count=1` exit 0（52 包，/tmp/vision-r-converge-fulltest.log）；
provider 接线后 service/handler/repository 三包定向全过；`vue-tsc --noEmit` ✅。

越界扫描：git status 对比派发前基线，新增恰为 R3 三文件 + 4 个测试文件，零越界；
全程无 git checkout/restore。

两项预闭环自审（整改轮）：执行遗漏——8 条逐项落地如上表，P2-7 生产接线补齐（否则
即 fake implementation）；全局一致性——Override/detect 写路径语义分明、settings 缓存
失效既有链覆盖新 key、能力表缓存失效未被破坏、settings DTO `*bool` 前后端一致
（前端零消费）、wire provider 单一构造路径。资金数值：哨兵行 cost=0 不进用户账单/订阅。

请外审：①逐条复核 8 项整改是否闭合对应发现；②整改是否引入新问题（含 P1-2 失败关闭
在部分候选读错时的可用性语义：读错候选排除而非整请求失败，全候选读错才整体失败——
是否符合"失败关闭"且不过度）；③有无新发现。

## 9. 第 2 次终审消化记录（2026-09-27，block：1×P1 + 4×P2，全部采纳，无拒绝项）

外审输出归档：docs/review-packets/vision-final-gate2-r2-20260927-*。

- P1-A 能力查询未接生产调度器（默认恒 unknown，§3.4 生产形同虚设）→ **采纳整改 S1**：
  ProvideOpenAIGatewayService 增参绑定 ModelSupportsVisionInput + 接线测试。
- P1-B 代理解析失败携 key 直连 → **采纳整改 S2**：解析失败即终止检测，禁止无代理回退。
- P2-C 缓存失效错误被吞 + 陈旧回填 → **采纳整改 S3**：失效失败清本地、禁 Redis 回填、
  错误传播至写路径。
- P2-D 体读失败缺 usage/日志 → **采纳整改 S2（同文件合并）**：发出后失败统一终结路径
  先记 usage 再记日志。
- P2-E ≤0 目标 ID 绕过同组校验 → **采纳整改 S4**：非正 ID 判配置错误，校验覆盖每个元素。

整改 4 单并行派发（S1-S4，文件边界互斥）。S 轮验收 + 收敛全量后第 3 次终审。

## 10. S 轮（第 2 次终审整改）落地摘要 —— 第 3 次终审送审范围

| 发现 | 落地 | 验证 |
|---|---|---|
| P1-A 接线缺失 | `ProvideOpenAIGatewayService`（wire.go:33）增参 `capability`，构造时 `SetOpenAIAccountVisionCapabilityLookup(capability.ModelSupportsVisionInput)`（:90）——provider 承载，wire regen-safe；wire_gen 调用点同步 | 接线测试 `TestOpenAIGatewayService_VisionCapabilityWiring`：经 provider 构造的服务，known=false 被排除 + 读错 `vision_capability_unavailable` 失败关闭；cleanup 复位全局 lookup |
| P1-B 代理失败直连 | parseProxyURL 失败 → 终止检测返回 detect_failed（附原因），logDetectResult 已写，请求未发出不记 usage；无代理回退路径已消除 | `TestVisionDetectProxyParseFailureStops` 过 |
| P2-C 缓存失效吞错 | invalidateAndNotifyAccount 返回 error；失败→clearLocalAccount+suspectRedisAccounts 窗口（跳过 Redis 回填、本地回填仅 DB 权威源、不回写 Redis）；写路径 `invalidate vision capability cache: %w` 透传 | 场景①失效失败传播+绕过陈旧缓存 ②广播失败不静默 ③正常路径零回归，全过 |
| P2-D 体读失败缺观测 | 请求发出后失败统一终结路径：记 usage → 记 vision_detect_result → 返回 detect_failed；发出前校验失败不记 usage 边界保持 | `TestVisionDetectResponseBodyReadFailure` 过 |
| P2-E 非法目标 ID | 写入校验 `id <= 0` → ErrVisionRoutingInvalidConfig；同组校验覆盖每个元素（跳过逻辑已删） | 0/负数/正 ID 三场景测试过 |

收敛验证：`go build ./...` ✅、`go vet` 三包 ✅、`go test ./... -count=1` 全量一轮
（/tmp/vision-s-converge-fulltest.log）。越界扫描：S 基线外仅 4 张派发单+1 个接线
测试文件，零越界。白名单缺陷留痕：R 轮 S3 卡 `-run 'AccountModelCapability'` 正则
与实际测试函数名不匹配，执行会话规范顶回，主会话已确认并以更正正则复跑（执行会话
无静默偏差）。

已知约束登记（非阻断）：S1 lookup 为包级全局，生产单次构造语义正确，测试 cleanup
复位；未来若出现多实例构造需改为实例字段。

请外审：①五项整改是否闭合；②S3 的 suspectRedis 窗口语义（失效失败后该账号读路径
DB 直读、本地回填、不回写 Redis 直至下次成功失效/广播）是否满足 §3.3 且无兜底；
③有无新发现。

## 11. 第 3 次终审消化记录（2026-09-27，block：2×P1 + 2×P2，全部采纳，无拒绝项）

外审输出归档：docs/review-packets/vision-final-gate2-r3-20260927-review-output.md。

- P1-A ProxyID 已配但 Proxy 关系缺失→携 key 无代理直连（S2 的第二条漏网路径）→
  **采纳整改 T1**：关系缺失即终止，仅明确无代理配置才直连。
- P1-B 并发回源把旧能力装回本地缓存（代际竞态）→ **采纳整改 T2**：账号级代际校验，
  失效后旧快照不得胜出；禁止以放宽过滤规避。
- P2-C 畸形 200 落库负能力 → **采纳整改 T1**：结构校验，畸形/error payload/缺 message
  → detect_failed（保留 usage+日志，不写能力标记）。
- P2-D 早期 detect_failed 终态缺日志 → **采纳整改 T1**：全部终态统一 logDetectResult。

整改 2 单并行派发（T1/T2，文件边界互斥）。T 轮验收 + 收敛全量后第 4 次终审。
收敛趋势：第 1 轮 10 条 → 第 2 轮 5 条 → 第 3 轮 4 条，逐轮深入（实现→边界→并发），
非循环震荡；每轮均为新实质差异，重送合法。

## 12. T 轮（第 3 次终审整改）落地摘要 —— 第 4 次终审送审范围

| 发现 | 落地 | 验证 |
|---|---|---|
| P1-A Proxy 关系缺失直连 | `ProxyID != nil && Proxy == nil` → 构造 client 前终止，detect_failed+日志，不记 usage；仅 ProxyID 为空才直连 | `TestVisionDetectProxyRelationMissingStops` 过（doer 未触达断言） |
| P1-B 回源旧值竞态 | 账号级代际计数（accountGen+genMu）：DB 写提交后、回源刷新前 bumpGeneration；回源前记快照，本地安装前复核，变了丢弃 | 确定性竞态测试（旧值 false 未毒化缓存、再读得 true）+ `-race` 并发 + 零回归，全过 |
| P2-C 畸形 200 落负能力 | 200 先结构校验：坏 JSON/error payload/缺 choices·message → detect_failed（usage+日志保留，不写能力标记） | `TestVisionDetectMalformed200Responses` + `TestVisionDetectValidNoCodeStillUnsupported`（零回归）过 |
| P2-D 早期终态缺日志 | 全部四态终态统一 logDetectResult（缺 key/缺 base URL 等已补） | `TestVisionDetectMissingKeyHasLog` 过 |

过程留痕：T1 跑白名单时撞 T2 中间态编译错（refreshAccountCache 签名改半），执行会话
规范 BLOCKED 顶回未越界修补；主会话裁定等 T2 完成后重跑，T1 白名单 14 测试复跑全过。
越界扫描：T 基线 diff 为空（两单仅编辑既有未跟踪文件），零越界。

收敛验证：`go build ./...` ✅、`go vet` 三包 ✅、`go test ./... -count=1` 全量
（/tmp/vision-t-converge-fulltest.log）。前端 T 轮零改动，vue-tsc 证据沿用 R5/R 轮。

请外审：①四项整改是否闭合；②T2 代际校验时序论证（写提交后 bump→回源快照→安装前
复核）是否闭环、有无残余竞态窗口；③有无新发现。

## 13. 第 4 次终审裁定（2026-09-27，block，3 条：2×P1 + 1×P2）

外审输出归档 docs/review-packets/vision-final-gate2-r4-20260927/。逐条裁定（全部采纳，无拒绝项）：

- **P1-E 跨实例失效通知不传播代际与可疑状态** → 采纳整改 U。实证吻合：
  `invalidateAndNotifyAccount` 在 Redis 失效失败时仍无条件广播（service :348-357），
  订阅端（service :85-87 + repo cache :136-140）只清本地不清代际、不标 suspect；
  失效失败+广播成功的组合下，其他实例清本地后回读仍存陈旧值的 Redis 并回填 local+Redis，
  代际未推进导致并发回源旧值可重新安装。违反 §3.3 多实例即时生效与失败关闭。
- **P1-F 代际复核与本地安装非原子 + 失配仍返回旧快照** → 采纳整改 U。实证吻合：
  `maybeSetLocalAccount` check（genMu）与 install（localCacheMu）两段锁间有窗口，
  bump 可插入致旧值毒化；且 `refreshAccountCache` 失配时把陈旧 caps 返回当前请求，
  继续按旧 supports_vision 选号。整改：check+install 同锁原子化；失配限次重读
  （重记快照→重读 Redis/DB），耗尽即显式报错失败关闭，不返回旧快照。
- **P2-E manual_review 分类过宽** → 采纳整改 U。与方案 §3.5 权威口径冲突：
  方案两级规则明文"200 但响应不含验证码（空/普通文本拒绝）→ 落 false"，
  manual_review 仅限"明确策略/政策拒绝"（能力正常但策略拒绝，判 false 是假阴性）。
  现实现通用能力性措辞（cannot identify/无法识别 + 图片关键词）即判 manual_review
  （vision_detect_service.go isVisionRefusalResponse），"I cannot identify the digits
  in the image" 被误判人工复核 → unknown 放行，不支持账号滞留候选池。整改：
  manual_review 收窄至明确的政策/安全/拒绝性措辞（won't/refuse/policy/拒绝/政策类），
  能力性措辞（cannot/can't/unable/无法/看不到类）归 unsupported 落库 false。

收敛趋势：10 → 5 → 4 → 3 条，持续收窄且逐层深入（实现→边界→并发→多实例一致性），
每轮均为新实质差异，重送合法（合同 §6 防循环）。

## 14. U 轮（第 4 次终审整改）——派发范围

三发现共享同一子系统（能力缓存协议 + 检测分类测试文件与接口签名耦合
`vision_detect_service_test.go` stub :81-82 / mock :139,150），按派发粒度规则
①（测试文件被多方共享）合并为**单卡 Vision-U**，不做同文件并行避免 T1/T2 式撞车。

改动域：
- `backend/internal/service/account_model_capability_service.go`（接口签名扩展 +
  订阅回调协议 + 原子安装 + 失配重读失败关闭）
- `backend/internal/repository/account_model_capability_cache.go`（pub/sub payload
  JSON 化携带 suspect；兼容旧纯数字 payload 按保守 suspect 处理）
- `backend/internal/service/account_model_capability_service_test.go`、
  `backend/internal/repository/account_model_capability_cache_test.go`（协议/竞态测试）
- `backend/internal/service/vision_detect_service.go`（isVisionRefusalResponse 收窄）
- `backend/internal/service/vision_detect_service_test.go`（stub 签名同步 + 分类测试改写）

验收后收敛（vet + 全量 52 包一轮一次）→ 第 5 次终审。

## 15. 第 5 次终审裁定（2026-09-28，block，5×P1 + 1×P2 must_fix + 1×executor_cleanup）

外审输出归档：/home/zjy/.codex-companion/2026-09-27T17-41-02-141Z-review，
snapshot docs/review-packets/vision-final-gate2-r5-20260928/task-snapshot.md。
主会话逐条取证核实后裁定：

- **P1-1 WS 首帧含图不写 hint（openai_gateway_handler.go:2639-2640）** → 采纳。
  实证吻合：WS 桥首帧只把 firstMessage 传给 `withOpenAIProfitSuppressedForImage`
  （利润门跳过），未调 `SetOpenAIHasImageInputHint`；后续选号
  `GetOpenAIHasImageInputHint(c)` 恒 false ⇒ RequireVision=false，带图 WS 请求
  绕过 §3.4 过滤。违反"全部选号入口生效"（§3.4）。
- **P1-2 vision_routing 无法显式清空（group_handler.go:557-568）** → 采纳。
  实证吻合：`applyVisionRouting` 对 `len(routing)==0` 直接 return nil（注释自称
  "避免无谓写"），编辑分组删空规则提交后旧规则残留并继续被调度读取。且前端
  `convertRoutingRulesToApiFormat` 空规则产出 `null`、非空产出 map，语义模糊。
- **P1-3 视觉配置校验与分组保存非原子（group_handler.go:761-767、916）** → 采纳。
  实证吻合：校验写入发生在 group Create/Update 成功之后，校验失败返回 4xx 但
  分组/其余字段已落库 ⇒ 部分提交。
- **P1-4 批量检测前后端契约不一致（accounts.ts:1214-1223 vs vision_capability_handler.go:57-62）**
  → 采纳。实证吻合：F3/E 派发单均写死 `{"account_ids":[..],"items":[..]}` 契约，
  后端按契约实现；E 单前端实现偏离为 `{items:[{account_id,...}]}`（E 单验收只核对
  "端点一致"，漏核 body 形状——主会话 E 单验收失职）。现前端批量 API 无人调用
  （全前端仅 VisionCapabilityModal 单检测/覆盖两处调用），但契约必须归一。
  裁定以**后端（=两份派发单原文契约）**为准：前端 `detectVisionCapabilityBatch`
  改为发送 `account_ids`（去重）+ items（model/protocol 去重）。
- **P1-5 分组读取链未装载 VisionRouting（mappers.go:159 / group_service read path）**
  → 采纳。实证吻合：`groups.vision_routing` 列（迁移 260）仅由 visionRoutingRepo
  （写路径 + 调度读路径 :2570）访问；GroupService.GetByID/List/ListActive → ent
  读取链不装载，`GroupFromServiceAdmin` 透传恒 nil ⇒ 管理员编辑页看不到已保存
  规则（前端 GroupsView.vue:6526 从 `group.vision_routing` 读）。违反 §3.7
  "配置即时生效/可视化"的闭环。
- **P2 kill-switch 读错 settingService 实例（openai_account_scheduler.go:2544-2549）**
  → 采纳。实证吻合：`isVisionRoutingEnabled` 读
  `s.rateLimitService.settingService`，而构造函数注入的 `s.settingService`
  才是权威实例（openai_gateway_service.go:454/528/567）；rateLimitService 为 nil
  或其 settingService 非同一实例时，配置的 false 被误当默认开启，kill-switch 失效。
- **executor_cleanup generationMatches 死代码** → **已闭合**（主会话终审提交前
  自查发现并删除，2026-09-28：build/vet/定向测试过；外审读的是删除前快照）。

收敛趋势：10 → 5 → 4 → 3 → 7 条。条数回升但层级持续下移（缓存协议 → 入口接线 /
DTO 装配 / API 契约层），无一条与已整改项同根因复发，重送合法（合同 §6）。

主会话 E 单验收失职留痕：F3/E 派发单契约一致，E 单实现偏离 body 形状，验收时
只核对了端点路径未核对请求体，特此登记。

## 16. V 轮（第 5 次终审整改）——派发范围

6 条未决发现（P1-1..P1-5 + P2）按改动域拆分：
- **单卡 V1（WS 入口 + kill-switch，2 条）**：openai_gateway_handler.go
  （首帧 SetOpenAIHasImageInputHint + turn/failover 重入复用请求级结果）、
  openai_account_scheduler.go（isVisionRoutingEnabled 改读 s.settingService）。
- **单卡 V2（分组配置读写闭环，3 条）**：group_handler.go（applyVisionRouting
  区分"未携带"与"显式空"；校验前置/同事务消除部分提交）、分组读取链 VisionRouting
  水合（GroupService 读路径经 VisionRoutingService/Repo 装载）、前端
  accounts.ts 批量契约对齐后端 account_ids 形状。
- 测试随卡：WS hint 测试、kill-switch 场景测试、分组清空/部分提交/读取水合测试、
  前端 vue-tsc。

主会话机械项：无（generationMatches 已闭）。
