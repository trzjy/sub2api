# B4 实现方案：分包积分快照 + 到期紧迫度调度 + 旧总量展示退役

> 权威：`codebuddy-cockpit-fusion-plan.md` v16 §4.1（含 2026-09-29 addendum）、
> §4.2（冻结已完全解除）、§5 B4 验收行。本文只是派发执行层分解，零新增语义；
> 与权威冲突时以权威为准，发现权威缺口 BLOCKED 顶回主会话。

## 0. 现状基线（2026-09-29 主会话只读排查）

- **写入链**：`codebuddy_quota_service.go` `queryUsageForAccount` →
  `UpdateExtra` 5 键（used_percent/reset_at/updated_at/total/used）+
  失败 `persistError`。解析已按 2026-09-13 生产抓包真实 schema（PR-C1）。
- **used_percent 公式**：现值 = Σused/Σsize×100 —— 与 §4.1 钉死公式
  (Σtotal−Σremaining)/Σtotal×100 **数学等价**，方向正确；B4 改为从分包
  参与集合显式推导（异单位/无效分包剔除后求和），语义显式化非改向。
- **reset_at**：已用 CycleEndTime（`parseCodeBuddyCycleEnd`，
  `2006-01-02 15:04:05` @ FixedZone UTC+8），与 addendum 裁定一致。
- **错误键现状**：`codebuddy_quota_error`（backend 写 + frontend 读一致）。
  §4.1 SSOT 钉 `codebuddy_credit_error`、quota_error 废弃 → **B4 改名迁移**。
- **阈值消费**：`account_scheduling_threshold_eval.go`
  `codeBuddyThresholdCandidates` 读 used_percent/reset_at（保留，不动语义）。
- **调度插入点**：`gateway_scheduling.go` 同优先级候选排序
  （`filterByMinPriority` / `sortCandidatesForFallback` / 
  `shuffleWithinSortGroups`，:456-479、:741、:780-798）。
- **前端**：`utils/codebuddyCredit.ts` + `AccountUsageCell` + `types/index.ts`；
  错误键现读 `codebuddy_quota_error`。
- **周期任务**：`CodeBuddyQuotaCheckService`（wire.go，默认 30m）。
- **现存偏差（§4.1 已预判，B4 修正）**：`setBillingHeaders` 空 domain 发
  `X-No-Domain: 1`；§4.1 契约唯一例外 = `X-No-Authorization: 1`。

## 0.1 方案审消化记录（闸①，2026-09-29，gpt-5.6-sol，结论 block→回修）

外审证据：R1 `~/.codex-companion/2026-09-29T09-56-09-657Z-review/`（首次
09-33 断流失败关闭不计）；R2 直连 `~/.codex-companion/b4-plan-review/
direct-r2/`（companion 两轮故障后按用户授权改道直连 gpt-5.6-sol，
结论 block→回修，5 项发现）。R1 8 项**全部采纳**；R2 5 项（3 must_fix +
1 executor_cleanup + 1 residual_risks）**全部采纳**，回修映射：

### R5 消化（2026-09-29，直连 gpt-5.6-sol，block→回修）

外审证据：`~/.codex-companion/b4-plan-review/direct-r5/`。R4 #2 闭合；
剩 1 项 must_fix **采纳**：当前已接受事件的 SQL 比较状态与 version 写入
语义未钉死。回修：Card B 补可执行定义——当前事件时间 =
`GREATEST(COALESCE(packages_updated_at,0), COALESCE(last_attempt_at,0))`
（零新增字段，优先于外审建议的显式新字段方案，合同 §5）、version 一律
= attempt_version 成功提交不得另自增、五场景回归补两条交错断言。

### R4 消化（2026-09-29，直连 gpt-5.6-sol，block→机械回修）

外审证据：`~/.codex-companion/b4-plan-review/direct-r4/`。R3 #2/#4/#5 闭合；
R4 2 项均为残留旧表述与新契约的内部矛盾，**全部采纳（机械回修）**：

| # | 发现 | 回修 |
|---|---|---|
| 1 | Card B 残留"请求级预分配 (attempt_time, attempt_version) 三路径同键"旧句 | Card B 重写：抓取开始仅取 attempt_version；成功用 `(success_time, attempt_version)`（持久化于 packages_updated_at 作比较状态源）、失败用 `(attempt_time, attempt_version)`（持久化于 last_attempt_at），同一 DB 字典序裁决 |
| 2 | Card C 残留"有消费方登记归属"旧句（重新打开 R3 #3） | Card C 替换为与 §1 一致收敛门：迁移/证明非 freshness 后停写并删；无法收敛 = 清理与发布验收失败 |

### R3 消化（2026-09-29，直连 gpt-5.6-sol，block→回修）

外审证据：`~/.codex-companion/b4-plan-review/direct-r3/`。R2 之 #2/#3 闭合、
#5 主体闭合；R3 5 项**全部采纳**，回修映射：

| # | 发现 | 回修 |
|---|---|---|
| 1 | 统一排序键违反权威"成功时间优先"（:806-811 实证） | §1 重写：成功路径=成功时间、失败路径=尝试时间，同一 `(event_time, version)` 比较器；版本=DB 分配单调号、每次抓取开始取得、三路径复用，仅同时间戳 tiebreaker |
| 2 | "唯一参与资格谓词"与 §4.2 时间/新鲜度条件矛盾（两套参与集合） | §1 拆分命名：**基础快照有效性谓词**（字段有效+非 Status=3，用于 used_percent）与**紧迫度参与性谓词**（基础+168h 窗口+24h 新鲜，用于 urgency/D）；Card D 复用基础实现叠加条件 |
| 3 | 旧 updated_at 非零消费者分支未收敛到唯一 freshness | §1/Card C 钉死：无消费者→停写并删；有消费者→迁移或证明非 freshness 后停写并删；禁"登记归属"后共存 |
| 4 | 显示舍入规则未给具体值 | Card F 钉死：2 位小数 + ROUND_HALF_UP + 合计/单包同口径 + 舍入 spec 用例 |
| 5 | Card E 批次表达可能被误执行为迁移先行 | Card E 执行顺序钉死：迁移与切换不得早于 Card B 上线，同一发布闸门 |

### R2 消化（2026-09-29）

| # | 发现 | 回修 |
|---|---|---|
| 1 | 成功/失败比较器未定义为同一可执行序（"更新时间"vs"attempt_time"；version 取得时机未定义） | §1 新增**统一排序键**：请求级预分配 `(attempt_time, attempt_version)` 贯穿三路径，DB 单一元组裁决，禁止提交/到达时间；Card B 同步 |
| 2 | Card E 数据迁移在批 1，旧服务运行窗口会再写旧键 → 切换窗口分叉 | Card E 增**发布闸门**：迁移+切换单一发布动作，禁"迁移后旧版本继续运行"窗口；双键冲突失败阻断；五态幂等测试 |
| 3 | reset_at 语义未保留（阈值消费依赖） | §1 + Card B 字段矩阵钉死：成功事务继续写 reset_at，失败写不得修改；验收补 reset_at/阈值候选回归 |
| 4 | 旧 `updated_at` 键未纳入清理（并行 freshness 事实源风险） | §1 + Card C 清理清单纳入旧 `codebuddy_credit_updated_at` |
| 5 | 前端数值精度/合计规则未定义 | Card F 增精度约束：字符串承载 + 十进制安全求和 + 20,8 边界/求和/舍入测试 |

### R1 消化（2026-09-29）

| # | 发现 | 回修 |
|---|---|---|
| 1 | 单写者拓扑证明须前置（§4.1 前置阻断项，非收尾） | **已前置**：主会话派发前完成生产拓扑取证（§0.2），多实例则先出锁卡 |
| 2 | 双写链（Card A 暂留 UpdateExtra + Card B 新路径）违反旧链归零；缺 UpdateExtra 不足证明 | **重构**：Card A 只做纯解析+存储组装（零写路径），Card B 为唯一写路径变更（替换全部快照/错误/used_percent 写入入口+停写旧总量键，A/B 同任务边界合入，不可独立部署）；UpdateExtra 不足证据见 §0.2 |
| 3 | 失败写缺同一单调比较器 | Card B 明确：失败写与成功写同一字典序比较器，失败版本持久化（§1 version 键），真实 DB 并发测试证明旧失败不回退新状态 |
| 4 | 解析错误 vs 清除错误混淆 | Card B 明确：本次解析的错误结果随成功快照同事务写入；本次无错误才清除旧错误；同一比较器裁决 |
| 5 | used_percent 参与集合漏 Status=3 排除 | 基础快照有效性谓词（Card A 实现/D 复用）：字段有效 + 非 Status=3；耗尽包不稀释分母回归断言（R3 起谓词拆分见 §1） |
| 6 | 错误键双名兼容 = 红线越界（禁兜底/并行事实源） | Card E 重定义：唯一后端迁移所有者——原子/幂等迁移存量 + 删旧键 + 旧引用 grep 归零验证，然后一次切换全部读写端；**禁止任何双名读取** |
| 7 | DB 条件更新回归必须为完成门禁 | §4 验收改为：DB-backed 五场景回归 = 不可跳过完成门禁（dockerized postgres，命令+证据位置固定）；环境不可用 = B4 未完成 |
| 8 | (P2) 上线观测缺口 | 采纳：新增快照年龄/无效分包错误计数/条件更新拒绝计数 slog 结构化日志（复用既有日志路径，不建新观测机制）；无既有指标入口的部分登记为残余风险 |

### 0.2 UpdateExtra 不足证据（合同 §5 新机制前置证明）

`accountRepository.UpdateExtra`（account_repo.go:2729）实现为 JSONB 合并
`extra || $1`（last-write-wins）：**无条件谓词**（WHERE 仅 id+deleted_at）、
**无单调校验**、**无 version 原子自增**、**无同事务错误清除联动**。§4.1
要求的"成功写仅单调通过时原子提交+同事务清错误、失败写同比较器"无法经
扩展该函数语义达成（调用方 40+ 处共用，改语义即全局行为变更）。故 Card B
新增 **codebuddy 快照专用条件更新 repo 方法**（单一新入口，旧 UpdateExtra
对其余调用方零改动），非通用新机制。

### 0.3 拓扑取证（单写者前置证明，派发前完成）

主会话生产取证：docker-compose 副本数 + CodeBuddyQuotaCheckService 实例拓扑
+ 既有调度锁/租约适用性，证据落盘 `~/.sub2api-acceptance/sub2api-b4-probe-
20260929/04-topology-evidence.md`。结论单实例 → 放行批 1；多实例 → 先出
锁接入卡（复用既有租约机制）再实施快照写入。

## 1. 存储契约（§4.1 权威，此处照录执行口径）

Extra 新键：
- `codebuddy_credit_packages` = `[{id,name,unit,remaining,total,expires_at,status}]`
  - id=AccountId；name=PackageName；unit=CapacityUnit；remaining=
    CapacityRemain（解析源 `CapacityRemainPrecise`，NUMERIC(20,8) 口径）；
    total=CapacitySize（源 `CapacitySizePrecise`）；expires_at=CycleEndTime
    （Asia/Shanghai 解析→UTC RFC3339）；status=Status 原值。
  - 校验失败关闭：unit≠`credits`、负值、remaining>total、**未知 Status**
    → 整包剔除 + 计入 `codebuddy_credit_error`（保留原始值供排查）。
  - `TotalDosage` 禁消费；`ExpiredTime` 不消费。
- **两个谓词拆分命名（R3 回修 #2，消除两套"参与集合"歧义）**：
  - **基础快照有效性谓词**：字段有效（单位/数值校验通过）+ 非 Status=3。
    使用方 = used_percent 分子分母、快照派生值。**不含**时间窗口/新鲜度。
  - **紧迫度参与性谓词** = 基础有效性 + `now < expires_at ≤ now+168h`
    （§2 条件②）+ 快照 ≤24h 新鲜（条件③）。使用方 = §4.2 urgency 与 D。
  - Card A 实现基础谓词；Card D 复用基础谓词并叠加窗口/新鲜度条件，
    **不复制第二份基础实现**；回归测试分别引用两个名字。
- `codebuddy_credit_packages_updated_at`（唯一 freshness 依据，失败绝不更新）。
- `codebuddy_credit_last_attempt_at`。
- `codebuddy_credit_version`（DB 条件更新原子自增的 monotonic 版本）。
- **统一排序键（R3 回修 #1，权威 :806-811 照录）**：**成功时间为主排序键**
  的全序——成功路径比较时间 = 该次成功快照的**成功时间**（snapshot 完成
  时刻），失败路径比较时间 = 该次尝试的**尝试时间**；二者用**同一字典序
  比较器 `(event_time, version)`**，由 DB 条件更新统一裁决；版本号 = **DB
  分配的单调版本号**（每次抓取开始时取得，成功/失败路径复用同一版本），
  **仅作同一时间戳下的 tiebreaker（同时间戳按版本号大者胜）**，绝不以版本
  号新旧否定更晚的成功时间（先发后至的请求按成功时间正常提交）。禁止
  到达顺序覆盖；禁止"请求开始时刻贯穿三路径"（违反权威成功时间语义）。
- 错误键改名：`codebuddy_quota_error` → `codebuddy_credit_error`（写读两侧
  + 存量 extra 数据迁移 + 旧键归零）。
- `codebuddy_credit_used_percent` 保留：唯一写入者 = 分包快照成功提交事务；
  公式 = (Σtotal−Σremaining)/Σtotal×100（参与分包 = **基础快照有效性谓词**）；
  Σtotal=0 → 不写入、保留旧值、计入错误标记（失败关闭）。
- `codebuddy_credit_reset_at` 语义保留（R2 回修 #3）：新条件更新成功事务
  **继续写 reset_at**（源 = CycleEndTime），阈值消费
  （`codeBuddyThresholdCandidates`）不感知变更；失败写**不得**修改
  reset_at/freshness/成功快照。验收补：新快照后 reset_at 与阈值候选回归。
- 旧键 `codebuddy_credit_total`/`codebuddy_credit_used`：写入链退役；展示
  由分包求和派生（异单位不计入并标注）；清理前逐键 grep 消费方，证据落盘。
- 旧 `codebuddy_credit_updated_at` 键（R3 回修 #3，确定性收敛，权威 :802
  唯一 freshness）：Card C 清理清单逐键 grep，两条路径二选一、不允许共存——
  无消费者 → 停止写入并删除；有消费者 → 逐个迁移到
  `codebuddy_credit_packages_updated_at`（或证明该消费不表示 freshness）后
  停止写入并删除旧键。**禁止仅"登记归属"后旧键继续带消费者共存**。

## 2. 调度契约（§4.2 照录执行口径）

- 配置 `gate.codebuddy.urgency_boost_enabled`（默认 false）+ `urgency_boost_k`
  （默认 0，强制 [0,1]，非法值启动/热加载失败关闭，无静默兜底）。
- 参与分包集合 = ①字段有效且非 Status=3；②`now < expires_at ≤ now+168h`
  （Asia/Shanghai 字面量→UTC）；③所属快照 ≤24h 新鲜。
- `urgency = Σremaining(参与)`；`D = Σtotal(参与)`；`urgency_norm = urgency/D`
  （D=0 → 0）截断 [0,1]；`weight = base_weight × (1 + k × urgency_norm)`。
- 作用点 = 同分组候选排序加权项；不改阈值暂停/429 冷却/并发/RPM 语义；
  k=0 与现行为逐位一致。

## 3. 卡拆分与批次（并发 ≤5，文件+测试对为单位；含 §0.1 回修重构）

### 批 1（三卡并行）

**Card A — 分包解析 + 存储组装（纯函数，零写路径）**
- 文件：`internal/service/codebuddy_quota_service.go`（+test 文件；
  testdata 增脱敏精简 fixture）。
- 内容：解析分包条目（§1 契约）、校验失败关闭、storage 组装、
  **基础快照有效性谓词**（used_percent 与 §4.2 共用其实现；紧迫度窗口
  叠加属 Card D）、used_percent 从
  参与分包推导、Σtotal=0 边界、`setBillingHeaders` 空 domain 改
  `X-No-Authorization: 1`（唯一例外，三态请求 fixture 单测：cn 有 domain /
  intl 有 domain / 空 domain；断言头集合精确形态）。
- **本卡零写路径变更**（不触碰 UpdateExtra 调用、不改写库行为）——写路径
  全部由 Card B 单点替换，A/B 同任务边界合入，不可独立部署。
- 错误键名常量即用 `codebuddy_credit_error`（§1 契约；写切换由 B 执行）。

**Card D — §4.2 紧迫度加权调度**
- 文件：`internal/service/gateway_scheduling.go`（+test）、config 定义、
  新 `internal/service/codebuddy_urgency.go`（+test：纯函数可全场景单测；
  谓词**复用 Card A 基础谓词并叠加窗口/新鲜度条件**，不复制第二份）。
- 内容：配置项+校验（非法值启动与**热加载**均失败关闭，热加载用例必测）、
  参与集合三条件、urgency/D/norm 公式、排序加权接入（同优先级组内，接入点
  前先盘点该 helper 全部调用点，登记无旁路）、k=0 等价性、钉行为测试全清单
  （§4.2，含裁定配套两用例：Status=3 剔除断言耗尽包 total 不进 D、
  Asia/Shanghai 跨时区边界）。

**Card E — 错误键原子迁移（唯一后端迁移所有者，含发布闸门）**
  **执行顺序钉死（R3 回修 #5）**：Card E 可与代码开发并行，但**数据库
  迁移与读写切换动作不得早于 Card B 新写入口上线**——二者必须同一发布
  闸门完成；禁止"先迁移、后等 Card B"的窗口形态。
- 文件：backend 存量迁移（指定唯一迁移文件+测试）、
  `frontend/src/utils/codebuddyCredit.ts`、`AccountUsageCell` 相关、
  `types/index.ts`、相关 spec。
- 内容：**原子/幂等迁移**存量 `codebuddy_quota_error` →
  `codebuddy_credit_error`（迁移失败即阻断），删除旧键；迁移后旧引用
  grep 归零验证；**然后一次切换全部读写端，禁止任何双名读取/兼容层**
  （红线：无双名共存期）。前端读侧切换与本卡同任务边界完成。
- **发布闸门（R2 回修 #2）**：迁移 + 切换 = 单一发布动作——旧写入进程
  停写/隔离后执行事务迁移，再启用只读写新键版本；**禁止"迁移完成后旧
  版本继续运行"窗口**（旧服务会在迁移后再写旧键 → 状态分叉）。
  双键冲突策略：**失败阻断**，不静默覆盖；幂等性测试覆盖五态 = 旧键仅
  有/新键仅有/双键冲突/重复执行/中途失败重试。

### 批 2（依赖批 1 产物；A+B 同任务边界合入）

**Card B — 唯一条件更新入口 + 五场景 DB 门禁**
- 文件：`internal/repository/`（新增 `account_extra_conditional.go` + test）、
  quota service 写路径接入（替换 `queryUsageForAccount` 全部写入口）。
- 内容：快照专用条件更新 repo 方法（§0.2 已证 UpdateExtra 不足；单一新
  入口）：排序键按 §1 统一排序键定义——抓取开始时仅取得并复用
  `attempt_version`（DB 分配单调号）；**成功条件更新用
  `(success_time, attempt_version)`**（success_time = 该次成功快照完成
  时刻，成功事务持久化于 `packages_updated_at`）；**失败条件更新用
  `(attempt_time, attempt_version)`**（接受时持久化于
  `last_attempt_at`）；同一 DB 字典序条件更新裁决。
  - **当前已接受事件比较状态的可执行定义（R5 回修 #1，零新增字段）**：
    当前事件时间 = `GREATEST(COALESCE(packages_updated_at,0),
    COALESCE(last_attempt_at,0))`，当前版本 = `codebuddy_credit_version`
    （其值 = 最近一次被接受事件的 attempt_version）。成立前提（由比较器
    单调性保证，回归断言）：成功事务同事务更新 packages_updated_at 与
    last_attempt_at = success_time；被接受的失败写 last_attempt_at =
    attempt_time > 当前事件时间。NULL 视为时间零值；同时间戳按版本号
    大者胜（tiebreaker）。
  - **version 写入语义（R5 回修 #1）**：`codebuddy_credit_version` 一律
    写入本次抓取开始取得的 `attempt_version`；**成功提交不得另行自增
    生成新版本**（自增会破坏比较键中 attempt_version 语义）。
  - **字段矩阵（R2 回修 #3，逐项钉死）**：
  - 成功事务：分包快照 + `packages_updated_at` + `reset_at` + `used_percent`
    + `last_attempt_at`(=success_time) + version(=attempt_version，不另自增)
    + 本次解析错误结果；
  - 本次无错误才清除旧错误；
  - 失败事务：同一排序键下仅更新错误/尝试字段（`last_attempt_at` + 错误
    标记 + 失败 version 持久化），**不得**修改 freshness/reset_at/成功快照；
  - `updated_at` 旧键停止写入（清理由 Card C）。
- **DB-backed 五场景回归 = 完成门禁（不可跳过）**：dockerized postgres
  （本地 docker，`go test -tags integration`），五场景 = 延迟失败/并发
  成功失败/同戳乱序（断言双请求各持不同 version）/失败后成功清错误/
  先发后至跨版本清除过期错误；**补交错断言（R5 回修 #1）：失败已接受后
  较早成功到达（被拒，不回退）与成功已接受后较早失败到达（被拒，不覆盖
  成功时间），覆盖 GREATEST 比较状态推导的正确性**；补验收：新快照后 reset_at 与阈值候选回归
  断言；命令与证据位置固定于验收清单；**环境不可用 = B4 未完成**，不得以
  "待补验"登记替代。集成测试并验证 SQL 谓词/事务原子性/隔离行为。
- 部署顺序：与 Card A 同变更合入；见 Card E 发布闸门。

**Card F — 前端分包展示 + 旧总量展示退役**
- 文件：`utils/codebuddyCredit.ts`（+spec）、`AccountUsageCell.vue`、
  i18n zh/en。
- 内容：分包卡片渲染（名称/剩余/总量/到期/错误标记/更新时间）、合计=
  Σ 派生（异单位不计入并标注）、旧 total/used 展示退役。
- **数值精度约束（R2 回修 #5 + R3 回修 #4）**：API/类型层用**字符串**承载
  NUMERIC(20,8) 值（禁 JS number 直接解析）；合计求和用十进制安全方式
  （字符串逐位或等价 Decimal）。**显示口径钉死**：展示保留 2 位小数、
 舍入模式 ROUND_HALF_UP（decimal.js 等价），合计与单包展示同一口径；
  spec 期望值含 20,8 边界值、多分包求和、异单位排除、舍入用例
  （如 0.005→0.01、1.005→1.01）。

### 批 3（收尾，主会话）

**Card C — 旧键清理（grep 消费方证据 + 删除）**：依赖 F；逐键
`codebuddy_credit_total/used`、旧 `codebuddy_credit_updated_at`（R2 回修
#4 纳入）及旧 `codebuddy_quota_error` 残留 grep；收敛门（R4 回修 #2，与
§1 一致）：无消费者 → 停止写入并删除；有消费者 → 逐个迁移到
`codebuddy_credit_packages_updated_at` 或记录可验证的"非 freshness"证明，
完成迁移/证明后停止旧键写入并删除旧键；**任一消费者无法完成收敛 → 清理
与发布验收失败，不得以登记归属替代删除**。
**运维取证**：~~跨实例单写者证明~~（已前置 §0.3）；三态 domain fixture
断言核验；凭证脱敏 Bearer 零命中 grep 断言（扩展至全部 B4 证据与日志）；
**上线观测**（§0.1 #8）：快照年龄/无效分包错误计数/条件更新拒绝计数
结构化 slog；无既有指标入口的部分登记残余风险。

## 4. 验收（§5 B4 行对照）

脱敏 fixture 落盘✅（探针轮）；规范单位钉死✅；**跨实例单写者证明（已前置
§0.3，派发前完成）**；三态 domain 请求 fixture；快照 UI 卡片；
**DB-backed 五场景全序回归 = 完成门禁（不可跳过，环境不可用 = B4 未完成；
含同戳双请求各持不同 version 断言 + 新快照后 reset_at/阈值候选回归）**；
used_percent 单写者证明+旧键清理后阈值回归；公式方向回归（remaining→0 时
used_percent→100；耗尽包不稀释分母断言）；k 非法值失败关闭（含热加载）；
k=0 等价性；错误键原子迁移+发布闸门+旧引用归零；旧总量展示退役+旧键
（total/used/updated_at/quota_error）清理 grep 证据；Card F 精度测试
（20,8 边界/多包求和/异单位排除/显示舍入）；上线观测（快照年龄/错误计数/
条件更新拒绝，slog）。
每卡默认验证 = 定向测试（**必带 `-tags unit`**）+ `go build`/`vue-tsc`；
收敛后全量一轮（高风险边界：调度/资金口径 → 全量触发）。

## 5. 红线提醒（派发单逐卡带）

- 执行会话禁 git 写操作；边界外发现 BLOCKED 顶回。
- 禁兜底/降级：校验失败一律失败关闭，不静默截断钳位。
- 凭证/fixture 脱敏：Bearer/X-User-Id/X-Tenant-Id 零出现。
- 契约字段名以 §4.1 为准，不得自创键名。
