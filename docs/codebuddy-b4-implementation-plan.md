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
- **调度插入点（D-2 执行轮实证回写 2026-09-30，权威 §4.2"同分组候选排序加权项"语义不变）**：
  `openai_account_scheduler.go` `buildOpenAIAccountLoadPlan` 打分循环
  （codebuddy 平台候选唯一真实选号路径；`gateway_scheduling.go` 对
  codebuddy 候选**不可达**——路由/入口/Handler 三层证据见 D-2 BLOCKED
  记录：`routes/gateway.go:29-38` codebuddy 必须走 OpenAI 网关，否则出站
  缺指纹头 403）。接入形态 = 候选 score × `(1 + k × urgency_norm)`
  （权威公式 weight = base_weight × (1+k×norm)，base_weight=打分 score；
  k=0/未启用时乘 1 逐位一致，score 计算路径零改动）。
  ~~旧写法：`gateway_scheduling.go` 同优先级候选排序
  （`filterByMinPriority` / `sortCandidatesForFallback` /
  `shuffleWithinSortGroups`，:456-479、:741、:780-798）~~（事实错误，
  该链路不承载 codebuddy 选号）。
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

### R8 消化（2026-09-29，直连 gpt-5.6-sol，**结论 approve**，闸①通过；首轮 reasoning trace 失败关闭重试有效）

零 must_fix。4 项执行层补强**全部采纳**：

| # | 分类 | 发现 | 回修 |
|---|---|---|---|
| 1 | executor_cleanup | Card E/F 前端文件所有权重叠 | 前端文件唯一所有者 = Card F；E 纯后端迁移+发布闸门；错误键前端切换并入 F，与 E 同发布闸门 |
| 2 | executor_cleanup | Card B SQL 占位符形态 | Card B 附参数化 SQL 样例（jsonb_build_object 键集合/参数类型/RowsAffected=0=拒绝语义） |
| 3 | suggestions | sequence 名称/权限未落层 | 钉死 seq_codebuddy_credit_attempt_version、起始 1、owner/GRANT 随迁移文件钉死进验收证据 |
| 4 | residual_risks | 备份一致性验证委托发布单 | Card E 备份 manifest：账号集合/摘要/时间点/恢复后逐项核验，保留至发布验证完成 |

### R7 消化（2026-09-29，直连 gpt-5.6-sol，block→回修；首轮仅 reasoning trace 失败关闭，同路由重试有效）

外审证据：`~/.codex-companion/b4-plan-review/direct-r7/`。1 must_fix +
2 residual + 1 executor_cleanup + 1 suggestion **全部采纳**：

| # | 发现 | 回修 |
|---|---|---|
| 1 | 可执行 SQL 未落到 JSONB 存储结构（裸字段名/无类型转换/原子性未钉） | Card B 重写：完整键名 + `(extra->>'key')::timestamptz` / `::bigint` canonical 转换 + '-infinity'/0 哨兵 + tuple 接受条件 + 同一条原子条件 UPDATE（只合并 B4 键、保留其余 extra） |
| 2 | (residual) 微秒归一化与 attempt_time 采样点未钉死 | 统一 `time.Now().UTC().Truncate(time.Microsecond)` 应用侧截断；attempt_time = 抓取请求开始时刻采样（与 attempt_version 同点） |
| 3 | (residual) 恢复流程缺写入隔离 | Card E 恢复流程钉死：停写→校验备份→恢复→验证→再启动旧版本 |
| 4 | (cleanup) §0.1 R5 消化记录残留废弃表达式 | 标注"历史方案已修正，不得执行" |
| 5 | (suggestion) sequence 创建/权限/调用边界未挂卡 | Card B 补：sequence 由 B 迁移文件创建、repo 抓取开始调用、失败=本次抓取失败关闭（禁应用本地计数器） |

### R6 消化（2026-09-29，直连 gpt-5.6-sol，block→机械回修）

外审证据：`~/.codex-companion/b4-plan-review/direct-r6/`。GREATEST 零新增
字段方案被判定逻辑成立；3 项 must_fix + 2 项 residual_risks **全部采纳**：

| # | 发现 | 回修 |
|---|---|---|
| 1 | §1 "原子自增"与 Card B "写 attempt_version 不得自增"矛盾 | §1 统一：version 仅保存最近被接受事件的 attempt_version；attempt_version 由 DB sequence 抓取开始时取得，提交阶段不得递增 |
| 2 | COALESCE(...,0) 非法 timestamptz 表达式；首次 NULL version 未定义 | 改 '-infinity'::timestamptz 哨兵 + COALESCE(version,0)（attempt_version 从 ≥1 起）；接受条件写成显式 tuple SQL |
| 3 | 失败"严格 >"与同时间戳版本仲裁矛盾 | 失败接受条件改 >=（相等须版本更大）；同时间戳高版本失败可接受/低版本被拒列入回归断言 |
| 4 | (residual) 时钟来源/时区/精度未钉死 | Card B 补：应用进程生成 time.Now UTC、timestamptz 微秒；单实例下无跨实例偏差，多实例另立卡 |
| 5 | (residual) 迁移前向不可逆缺恢复边界 | Card E 补：迁移前 extra 快照备份+发布失败恢复路径；成功后声明前向修复边界（不可回滚点） |

### R5 消化（2026-09-29，直连 gpt-5.6-sol，block→回修）

外审证据：`~/.codex-companion/b4-plan-review/direct-r5/`。R4 #2 闭合；
剩 1 项 must_fix **采纳**：当前已接受事件的 SQL 比较状态与 version 写入
语义未钉死。回修：Card B 补可执行定义——当前事件时间推导（**历史方案
`GREATEST(COALESCE(packages_updated_at,0), COALESCE(last_attempt_at,0))`
已被 R6/R7 修正为 timestamptz 哨兵 + JSONB 提取口径，仅作演进留痕，
不得执行**）、version 一律 = attempt_version 成功提交不得另自增、
五场景回归补两条交错断言。

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
- `codebuddy_credit_version`（R6 回修 #1 全文统一）：**仅保存最近一次被
  接受事件的 `attempt_version`**；`attempt_version` 由 DB sequence（或
  等价 DB 原子分配机制）在**抓取开始时**取得，成功/失败提交阶段**均不得
  再次递增**。
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
- 文件：config 定义、新 `internal/service/codebuddy_urgency.go`（+test：
  纯函数可全场景单测；谓词**复用 Card A 基础谓词并叠加窗口/新鲜度条件**，
  不复制第二份）、**接入点 = `openai_account_scheduler.go`
  `buildOpenAIAccountLoadPlan`（D-2 实证回写，见上文"调度插入点"）**。
- 内容：配置项+校验（非法值启动与**热加载**均失败关闭，热加载用例必测）、
  参与集合三条件、urgency/D/norm 公式、排序加权接入（score ×
  (1+k×urgency_norm)，k=0 乘 1 逐位一致）、k=0 等价性、钉行为测试全清单
  （§4.2，含裁定配套两用例：Status=3 剔除断言耗尽包 total 不进 D、
  Asia/Shanghai 跨时区边界）。
- **进度注（2026-09-30）**：纯函数+config+全部单测已由 D-2 轮交付
  （245 用例 PASS）；接入被 D-2 BLOCKED 顶回（原方案接入点不可达），
  接入+死 helper 清理由 D-3 轮执行。

**Card E — 错误键原子迁移（唯一后端迁移所有者，含发布闸门）**
  **执行顺序钉死（R3 回修 #5）**：Card E 可与代码开发并行，但**数据库
  迁移与读写切换动作不得早于 Card B 新写入口上线**——二者必须同一发布
  闸门完成；禁止"先迁移、后等 Card B"的窗口形态。
- 文件：backend 存量迁移（指定唯一迁移文件+测试）。**前端文件所有权重叠
  裁定（R8 cleanup 采纳）**：前端错误键读写切换从本卡移出并入 Card F
  （前端文件唯一所有者 = F，避免双会话改同文件）；E = 纯后端迁移 +
  发布闸门 + 迁移验收。
- 内容：**原子/幂等迁移**存量 `codebuddy_quota_error` →
  `codebuddy_credit_error`（迁移失败即阻断），删除旧键；迁移后旧引用
  grep 归零验证；**然后一次切换全部读写端，禁止任何双名读取/兼容层**
  （红线：无双名共存期）。前端读侧切换由 Card F 执行（见 R8 权属裁定），
  F 的切换上线与 E 的后端切换同一发布闸门。
- **发布闸门（R2 回修 #2）**：迁移 + 切换 = 单一发布动作——旧写入进程
  停写/隔离后执行事务迁移，再启用只读写新键版本；**禁止"迁移完成后旧
  版本继续运行"窗口**（旧服务会在迁移后再写旧键 → 状态分叉）。
  双键冲突策略：**失败阻断**，不静默覆盖；幂等性测试覆盖五态 = 旧键仅
  有/新键仅有/双键冲突/重复执行/中途失败重试。
  - **恢复边界（R6 residual #5 + R7 residual 采纳）**：迁移执行前对受
    影响账号 extra 做快照备份（保留期与清理触发条件在发布单登记）；
    **发布失败恢复流程 = 停止新旧全部写入 → 校验备份 → 恢复 → 验证键
    状态 → 再启动旧版本**（恢复期间写入隔离，防止恢复快照覆盖恢复窗口
    内新键写入）；**备份 manifest（R8 residual 采纳）**：备份登记受影响
    账号集合/数量或校验摘要/备份时间点，恢复后逐项核验键状态，保留至
    发布验证完成后按登记条件清理；发布成功后进入前向修复边界（旧键
    已删，回滚旧版本不再支持——发布单明确声明该不可回滚点）。

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
  - **当前已接受事件比较状态的可执行定义（R5/R6/R7 逐轮收敛，零新增
    字段，JSONB 口径）**：全部键位于 `accounts.extra` JSONB，完整键名 =
    `codebuddy_credit_packages_updated_at` / `codebuddy_credit_last_attempt_at`
    / `codebuddy_credit_version`。**读取**（同一 canonical 转换用于候选值
    与当前值）：时间 = `(extra->>'key')::timestamptz`（NULL/缺失 →
    `'-infinity'::timestamptz`）；版本 = `COALESCE((extra->>'key')::bigint,
    0)`（0 = 尚无事件哨兵；合法 attempt_version 从 sequence 起 ≥1）。
    当前事件时间 = `GREATEST(上述两个时间表达式)`。**接受条件 = tuple
    语义**：`candidate_event_time > current_event_time OR
    (candidate_event_time = current_event_time AND candidate_version >
    current_version)`。**写入原子性**：条件判断、快照/错误字段写入、
    version 写入位于**同一条原子条件 UPDATE**（`UPDATE ... SET extra =
    extra || <新键值合并> WHERE <tuple 接受条件>`），只合并 B4 负责的
    JSONB 键、保留其他 extra 内容（JSONB merge 语义天然保留）。成立
    前提（由比较器单调性保证，回归断言）：成功事务同事务更新
    packages_updated_at 与 last_attempt_at 均为 success_time；被接受的
    失败满足 `attempt_time >= 当前事件时间`（**相等时须版本更大**，R6
    回修 #3：不写严格 `>`，同时间戳高版本失败可接受、低版本被拒）。
  - **version 写入语义（R5 回修 #1）**：`codebuddy_credit_version` 一律
    写入本次抓取开始取得的 `attempt_version`；**成功提交不得另行自增
    生成新版本**（自增会破坏比较键中 attempt_version 语义）。
  - **时间来源与精度（R6 residual #4 + R7 residual 采纳）**：success_time
    / attempt_time 由**应用进程生成**（`time.Now().UTC().Truncate(
    time.Microsecond)`，应用侧统一截断后传入 repo，比较值与写入值同一
    实例——禁驱动侧二次转换差异），存储 `timestamptz` 微秒精度；
    **attempt_time 采样点 = 抓取请求开始时刻**（与 attempt_version 同点
    取得，绑定于请求生命周期）；单实例拓扑下无跨实例时钟偏差问题
    （多实例扩展时另立卡）。
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
  成功时间），覆盖 GREATEST 比较状态推导的正确性**；**补 R6 断言：首次
  无快照无失败记录时首个成功与首个失败均能写入（NULL version=0 哨兵 +
  '-infinity' 哨兵路径）、同时间戳高版本失败可接受/低版本失败被拒**；补验收：新快照后 reset_at 与阈值候选回归
  断言；命令与证据位置固定于验收清单；**环境不可用 = B4 未完成**，不得以
  "待补验"登记替代。集成测试并验证 SQL 谓词/事务原子性/隔离行为。
- **参数化 SQL 样例（R8 cleanup 采纳）**：执行卡附样例钉死构造方式——
  写入键集合用 `jsonb_build_object(...)`（仅 B4 键）、候选时间/版本参数
  以 `timestamptz`/`bigint` 传入、`RowsAffected = 0` 即条件更新被拒绝
  （调用方按失败路径处理，不得重试改写比较条件）；不改变单一条件更新
  入口。
- **DB sequence 归属（R7 suggestion 采纳）**：`attempt_version` 的
  sequence 由 Card B 的迁移文件创建（名称与起始值在卡内钉死），repo 层
  抓取开始时调用取得；**sequence 调用失败 → 本次抓取失败关闭**（不得
  退化为应用本地计数器）。**命名与权限（R8 suggestion 采纳）**：sequence
  名 `seq_codebuddy_credit_attempt_version`、起始 1（≥1 语义）、owner =
  应用迁移执行角色，GRANT USAGE 予应用运行角色（名称随部署在迁移文件
  中钉死并进验收证据）。
- 部署顺序：与 Card A 同变更合入；见 Card E 发布闸门。

**Card F — 前端分包展示 + 旧总量展示退役**
- 文件：`utils/codebuddyCredit.ts`（+spec）、`AccountUsageCell.vue`、
  i18n zh/en。
- 内容：分包卡片渲染（名称/剩余/总量/到期/错误标记/更新时间）、合计=
  Σ 派生（异单位不计入并标注）、旧 total/used 展示退役、**错误键前端
  读写切换（R8 cleanup：前端文件唯一所有者，读 `codebuddy_credit_error`
  废弃 `codebuddy_quota_error`，与 Card E 后端切换同一发布闸门）**。
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
