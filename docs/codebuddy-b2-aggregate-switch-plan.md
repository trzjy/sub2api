# B2 剩余批次方案：聚合直绑分组开关（方案 v16 §5 B2 收尾）

日期：2026-09-29。本文是 B2 批次的剩余范围方案，与 `codebuddy-cockpit-fusion-plan.md` v16
配套；冲突时以融合方案正文为准。

## 0. 现状盘点（已实现，不再重做）

B2 §2.1-1/2/3 已在早前波次落地（提交 45f20232a / 1047d7560 / 060a3d55b）：

- 候选池扩展：`expandPlatformsForAggregatePool`（backend/internal/repository/account_repo.go:3323，
  分组路径唯一纳入点）+ `account_repo_aggregate_pool_test.go` 4 用例。
- 调度放行：`codeBuddyPlatformMismatchAllowed`（openai_account_scheduler.go:372）+
  过滤接点 ：1650-1654。
- 逐模型目录闸门：预算/前4/确定性 tie-breaker/三态失败关闭/not_probed 计数全链 +
  `codebuddy_aggregate_gate_test.go` 13 用例。
- 快照 @agg 桶键与超集单链：`schedulerAggregationBucketPlatform` +
  `withAggregatedCodeBuddy`（分组路径跳过快照层补入，D-13c）。
- /responses、/v1/messages 入站桥接与 forwardCodeBuddy（B3a 范围，已存在）。

**唯一生产缺口**：聚合绑定开关不存在——候选池扩展当前无条件生效，无法表达
"该分组聚合绑定关闭 → codebuddy 账号不在候选池"（融合方案 §5 三维矩阵第 3 行）。
配套缺口：三维状态矩阵逐组合测试未全部钉住。

## 1. 改什么

### 1.1 分组级开关字段（存储 + 权威路径 + 迁移）

- ent schema `backend/ent/schema/group.go` 新增
  `aggregate_codebuddy_enabled`（bool，默认 false=关闭）。沿既有 bool 字段全链
  （样板：`allow_messages_dispatch`，ent/schema/group.go:239）：代码生成
  （`go generate ./ent`）→ service `Group` 结构体
  （backend/internal/service/group.go:18）→ group repo 读写映射 →
  `CreateGroupInput` / `UpdateGroupInput`（admin_service.go:324 起）→
  admin handler 请求 DTO（group_handler.go:279 创建 / :359 更新，`*bool`）→
  响应 DTO（dto/types.go Group + dto/mappers.go）。
- **迁移（复用既有版本化 SQL 路径）**：`backend/migrations/263_add_group_aggregate_codebuddy.sql`
  （`ALTER TABLE groups ADD COLUMN IF NOT EXISTS aggregate_codebuddy_enabled boolean
  NOT NULL DEFAULT false` + COMMENT，样板：242_add_group_concurrency.sql）+
  迁移 parity 测试（样板：242/260 同名 `_test.go`）。schema DDL 只经该版本化
  迁移框架执行，无数据回填；§5"禁止裸 DB 写入"指绕过迁移框架/权威路径直接改
  业务行，不适用于本迁移文件。
  **发布顺序与回滚边界**：先迁移后发布（新代码读该列，缺列启动失败——迁移是
  发布前置）；应用回滚时保留该列（旧代码忽略新列）；无安全降级路径（DROP 后
  新代码不可运行）→ 禁止 DROP，降级停止条件 = 仅应用回滚、列与数据保留。
- **部署期语义与切换前置证据（显式登记）**：存量聚合分组行落为 false = 部署后
  聚合分组候选池不含 codebuddy，直至逐分组开绑定；codebuddy 自有分组不经
  扩展函数、不受影响。"可观察影响为零"不是无条件断言，而是**切换前置证据项**
  （生产/预发执行，非零即阻断激活）：① 迁移前可调度（schedulable=true）
  codebuddy 账号数=0；② 受影响聚合分组清单与当前候选池取证；③ 生产
  runMode≠simple 取证。逐分组开绑定即恢复各分组直绑候选。
- 语义：true=该聚合族分组候选池并入 codebuddy（现行为）；false=并入关闭，
  该分组任何生产选号路径都看不到 codebuddy 账号。
- **校验**：仅聚合族平台可置 true；非聚合平台置 true 请求拒绝（400，错误码
  `AGGREGATE_BINDING_NOT_SUPPORTED`）。**平台分类权威 = 分组自身的
  `Platform` 字段**（分组级唯一平台权威，与 `NormalizeProfitControlConfig`
  消费同一字段；一个分组恒对应一个 Platform 字段值）：创建用归一化后的
  输入平台，更新用合并后的 `group.Platform`。禁止按请求目标平台或账号平台
  推断。创建、更新分别加测试。

### 1.2 唯一纳入点贯通（repo 层，开关读取与扩展同边界）

- `expandPlatformsForAggregatePool` 的开关**不由调用方传参**，而是在
  `queryAccountsByGroup`（account_repo.go:3341）内部读取：当
  `opts.platforms` 与聚合族相交时，同一查询边界内按 PK 读 groups 行取
  `aggregate_codebuddy_enabled`，据此决定是否并入 codebuddy；非聚合平台
  集合零额外读取，行为逐位不变。读取失败 → 查询报错失败关闭，不静默取默认。
- **该设计使接口签名不变**：`ListSchedulableByGroupIDAndPlatform(s)` 及全部
  调用点零改动、零参数穿透——单一纳入点自带权威读取，不存在调用方旧值窗口。
  **消费者闭包清单（contract §3，逐一登记）**——经 `queryAccountsByGroup`
  的全部生产调用点，开关读取在其查询边界内自动生效：
  | 调用点 | 路径 | 判定 |
  |---|---|---|
  | scheduler_snapshot_service.go:1527 | 快照分组构建（mixed） | 受治理（聚合族时） |
  | scheduler_snapshot_service.go:1549 | 快照分组构建 | 受治理（聚合族时） |
  | openai_gateway_scheduling.go:1733 | 快照未命中直查回退 | 受治理（聚合族时） |
  | gateway_scheduling.go:1047/1091 | anthropic/gemini 直查回退 | 已证明不受影响（平台非聚合族，扩展 no-op 且零额外读取） |
  | admin_account.go:50 | 管理端 PlatformOpenAI 列表 | 已证明不受影响（非聚合族） |
  | batch_image_public.go:974 | 图片平台查询 | 已证明不受影响（非聚合族） |
  | gemini_messages_compat_service.go:460 | gemini messages 兼容 | 已证明不受影响（非聚合族） |
  验证：repo 定向测试断言开关两侧聚合分组行为；非聚合族分组既有 4 用例
  保持绿即证不受影响侧。
- **线性化点（钉死）**：开关状态 = 候选池查询事务内读到的 groups 行；
  翻转后发起的选号查询即见新值，翻转前已开始的查询按其事务快照完成
  （与仓库既有配置读取语义一致，不为本开关单独收紧）。
- 快照层：分组路径（groupID>0）纳入点在 repo（D-13c 单链），自动随 repo
  生效；**标准模式 groupID=0（未分组桶）→ 失败关闭，不补入 codebuddy**
  （无分组即无绑定授权；`withAggregatedCodeBuddy` 未分组分支小改）；
  repo 侧 `ListSchedulableUngroupedByPlatform(s)` 本就无纳入点，不动。
- **simple 模式行为不变**（个人版无分组概念，绑定无法表达；拒绝为其新增
  失败关闭/开关语义——无权威依据，过度设计）。闭合方式：**切换前置取证
  条款**（融合方案 §5"部署拓扑前置取证"已有项）：生产部署 runMode 取证
  ≠ simple，simple 模式部署不在 B2 激活拓扑，登记于切换清单。

### 1.3 快照失效（零新机制）

开关翻转 = 分组配置更新，`groupRepo.Update` 已在事务内 enqueue
`SchedulerOutboxEventGroupChanged`（group_repo.go:244）→
`handleGroupEvent`（scheduler_snapshot_service.go:724）重建该分组全部桶 →
重建时按新开关值执行 1.2。**不新增任何失效/版本机制**（融合方案 §5：
复用既有机制；contract §5：新机制引入前须证明既有机制不足）。

**收敛语义（既有机制已含串行化与栅栏保证，取证钉死）**：
- **桶写入者枚举**：全部重建收敛于 `rebuildBuckets` 单漏斗——读未命中懒构建
  （:298）、账号批量事件（:685）、账号/分组事件 rebuildByGroupIDs（:860/:913）、
  分组生命周期（:744）；快照写入统一经 `cache.SetSnapshot(ctx, bucket, token, …)`。
- **栅栏**：`SchedulerBucketWriteToken{Bucket, Epoch}` 把每个写入者限制在
  一个桶 epoch（scheduler_cache.go:25）；翻转触发的分组生命周期重建
  Retire/Reopen 提升 epoch，旧 epoch 的在途发布被栅栏拒绝。
- **租约**：分组生命周期重建由分组级租约串行化（`TryAcquireGroupLifecycleLease`，
  :782），并发重建失败重试、事件不标记 seen。
- 每次重建在执行时刻实时读权威（账号与开关同边界，1.2），因此乱序/延迟/
  并发事件最终桶状态恒收敛到权威值。epoch 内的在途瞬态窗口与仓库既有
  `schedulable`/配置翻转语义一致，不为本开关单独收紧；切换完成判定
  （重载+核验）闭合残余窗口。
- 不新增任何排序/版本/失效机制。
**运行期观测**：切换完成/关回验证的证据 = 既有 outbox lag 告警 + 重建
失败日志 + 候选池查询取证；不新建监控机制。

## 2. 依据什么权威

- 融合方案 v16 §5 B2 条款：聚合开关语义（分组级配置、既有权威路径、翻转+快照
  失效、禁裸配置写入、不新建版本协议）、三维状态矩阵（钉死行为依据）、
  B2 激活=独立生产切换门槛。
- contract §2 方案唯一 / §5 禁兜底与最小改动 / §3 影响分析（本文件 §1.2 的
  全调用点清点即消费者闭包）。
- 用户裁定（2026-09-29）：B2 剩余范围 = 聚合开关 + 三维矩阵测试，按规范工作流推进。

## 3. 做到什么算完成（验收清单）

1. `aggregate_codebuddy_enabled` 全链可读写：创建/更新/查询分组 API 带该字段；
   非聚合平台置 true 返回 400。
   **迁移（外审 F2）**：263 迁移在旧库升级后既有行落 false（parity 测试 +
   有 DB 环境升级验证登记遗留项）；无 DB 环境下以迁移测试 + 迁移文件评审为证，
   生产升级验证列入切换前置取证。
2. 开关关闭时，聚合族分组的候选池不含 codebuddy 账号（repo 单点定向测试；
   候选池唯一纳入点即单点）；**标准模式未分组桶失败关闭**：不补入 codebuddy
   （service 定向测试）；simple 模式补入行为不变（既有用例保持绿）。
3. 开关翻转后：GroupChanged 事件触发该分组桶重建，重建结果反映新开关
   （service 定向测试：开→关重建后快照无 codebuddy；关→开反之）。
   **收敛（融合方案 §5 钉死的并发启停必测，一个用例覆盖）**：并发启停 +
   连续翻转下最终桶状态等于权威值（租约串行化 + 重建实时读权威，既有机制
   保证，测试钉住行为而非复刻机制）。
4. 三维状态矩阵逐组合用例补齐（融合方案 §5 矩阵表 5 行）：
   - schedulable=false × 任意开关 × 新请求：不可选（既有
     `TestAggregatePool_SchedulableFalseCodeBuddyNotVisible` 为开关开侧，
     补开关关侧）；
   - schedulable=false × 已建立/已分配：立即停止（外审 F3：沿既有账号状态
     权威链逐一登记并测试**实际发送路径**各消费点——快照已加载候选、选号
     结果/租约、转发执行前校验、续租、重试换号不再选中；开关关闭与账号禁用
     同时发生时执行账号禁用语义。既有用例核对后补缺，登记消费点清单）；
   - 绑定关 × 新请求：走原生路径（1.2 新用例）；
   - 绑定关 × 已建立请求：放行完成、不中途改道（快照已加载集合不被翻转回收，
     定向用例）；
   - 重试换号按新请求重新过两维过滤（既有 failover 用例核对，缺则补）。
5. 非聚合分组、codebuddy 自有分组、groupID=0/simple 路径行为逐位不变
   （既有 4 repo 用例 + 快照层用例保持绿）。
6. 定向验证：`go build ./...` +
   repository/service 两包定向测试；合并前全量 `go test ./... -count=1` 一次
   （调度契约变更，contract §5 风险触发③）。
7. 显式不做（防漂移）：不改 forwardCodeBuddy/目录闸门/预算语义；不动
   `isCodeBuddyShadowAccount`；不实现解冻操作本身（既有管理端账号状态链，
   属生产切换动作）；前端管理 UI 不在本次（激活经 admin API 完成）。
8. 遗留项如实登记：生产库升级验证与部署期候选池取证需在有 DB 的生产/预发
   环境执行（切换前置取证清单），无 DB 环境不宣称已验。

## 4. 派发拆分（依赖链决定串行，两卡）

- **卡 B2-A**：§1.1 字段全链（含 263 迁移 + parity 测试）+ §1.2 repo 边界
  开关读取与扩展 + 验收 1/2 对应测试（文件：ent/schema/group.go + 生成代码、
  group.go 服务结构、group_repo 映射、admin_group.go、group_handler.go、
  dto 两文件、account_repo.go（queryAccountsByGroup/expandPlatformsForAggregatePool
  + groups 读取）、account_repo_aggregate_pool_test.go）。**接口签名与调用点
  零改动**。
- **卡 B2-B**（依赖 B2-A 合入）：§1.2 未分组失败关闭（withAggregatedCodeBuddy
  小改）+ §1.3 翻转/收敛用例（租约串行化 + 实时读权威；含融合方案 §5 钉死的
  并发启停用例）+ 三维矩阵补缺（含 F3 发送路径消费点登记）（文件：
  scheduler_snapshot_service.go 未分组分支及其测试、
  codebuddy_aggregate_gate_test.go 补充、既有矩阵用例核对补齐）+ 验收
  2(未分组)/3/4/5。
- 执行路由：`codebuddy --model hy3` 主力，额度耗尽切
  `custom-local:deepseek-v4.1-flash`（不中断、不请示）。

## 5. 禁区（两卡通用）

- 禁止触碰：codebuddy_gateway_forward.go、openai_account_scheduler.go 目录闸门
  与预算段、spark 相关、payment 链、cockpit 导入链。
- 禁止新增缓存/版本/失效机制（§1.3 既有租约链闭合）。
- schema 变更只经 263 版本化迁移文件（§1.1）；"禁止裸 DB 写入"指绕过迁移
  框架/权威路径直接改业务行；字段默认 false，无数据回填。
- 执行会话禁止任何 git 写操作；发现超出派发单范围的问题一律 BLOCKED 顶回。
