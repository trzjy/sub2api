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

外审证据：`~/.codex-companion/2026-09-29T09-56-09-657Z-review/`
（首次 09-33 会话断流重连失败关闭无内容，不计证据；本轮重试同路由有效）。
8 项发现**全部采纳**，回修映射：

| # | 发现 | 回修 |
|---|---|---|
| 1 | 单写者拓扑证明须前置（§4.1 前置阻断项，非收尾） | **已前置**：主会话派发前完成生产拓扑取证（§0.2），多实例则先出锁卡 |
| 2 | 双写链（Card A 暂留 UpdateExtra + Card B 新路径）违反旧链归零；缺 UpdateExtra 不足证明 | **重构**：Card A 只做纯解析+存储组装（零写路径），Card B 为唯一写路径变更（替换全部快照/错误/used_percent 写入入口+停写旧总量键，A/B 同任务边界合入，不可独立部署）；UpdateExtra 不足证据见 §0.2 |
| 3 | 失败写缺同一单调比较器 | Card B 明确：失败写与成功写同一字典序比较器（attempt_time, version），失败版本持久化（§1 version 键），真实 DB 并发测试证明旧失败不回退新状态 |
| 4 | 解析错误 vs 清除错误混淆 | Card B 明确：本次解析的错误结果随成功快照同事务写入；本次无错误才清除旧错误；同一比较器裁决 |
| 5 | used_percent 参与集合漏 Status=3 排除 | 唯一参与资格谓词（Card A/D 共用）：字段有效 + 非 Status=3；used_percent 与 §4.2 同谓词；耗尽包不稀释分母回归断言 |
| 6 | 错误键双名兼容 = 红线越界（禁兜底/并行事实源） | Card E 重定义：唯一后端迁移所有者——原子/幂等迁移存量 + 删旧键 + 旧引用 grep 归零验证，然后一次切换全部读写端；**禁止任何双名读取** |
| 7 | DB 条件更新回归必须为完成门禁 | §4 验收改为：DB-backed 五场景回归 = 不可跳过完成门禁（dockerized postgres，命令+证据位置固定）；环境不可用 = B4 未完成（不是登记待补验） |
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
- **唯一参与资格谓词（Card A/D 共用，单一实现）**：分包参与求和/调度当且仅当
  ①字段有效（单位/数值校验通过）②非 Status=3（耗尽/过期）。used_percent 与
  §4.2 紧迫度同谓词——**Status=3 分包不得进入 used_percent 的分子或分母**
  （耗尽包 total 稀释分母会反向压低已用比例、破坏阈值语义），回归必测。
- `codebuddy_credit_packages_updated_at`（唯一 freshness 依据，失败绝不更新）。
- `codebuddy_credit_last_attempt_at`。
- `codebuddy_credit_version`（DB 条件更新原子自增的 monotonic 版本）。
- 错误键改名：`codebuddy_quota_error` → `codebuddy_credit_error`（写读两侧
  + 存量 extra 数据迁移 + 旧键归零）。
- `codebuddy_credit_used_percent` 保留：唯一写入者 = 分包快照成功提交事务；
  公式 = (Σtotal−Σremaining)/Σtotal×100（参与分包 = 唯一参与资格谓词）；
  Σtotal=0 → 不写入、保留旧值、计入错误标记（失败关闭）。
- 旧键 `codebuddy_credit_total`/`codebuddy_credit_used`：写入链退役；展示
  由分包求和派生（异单位不计入并标注）；清理前逐键 grep 消费方，证据落盘。

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
  **唯一参与资格谓词**（used_percent 与 §4.2 共用实现）、used_percent 从
  参与分包推导、Σtotal=0 边界、`setBillingHeaders` 空 domain 改
  `X-No-Authorization: 1`（唯一例外，三态请求 fixture 单测：cn 有 domain /
  intl 有 domain / 空 domain；断言头集合精确形态）。
- **本卡零写路径变更**（不触碰 UpdateExtra 调用、不改写库行为）——写路径
  全部由 Card B 单点替换，A/B 同任务边界合入，不可独立部署。
- 错误键名常量即用 `codebuddy_credit_error`（§1 契约；写切换由 B 执行）。

**Card D — §4.2 紧迫度加权调度**
- 文件：`internal/service/gateway_scheduling.go`（+test）、config 定义、
  新 `internal/service/codebuddy_urgency.go`（+test：纯函数可全场景单测；
  参与资格谓词**复用 Card A 实现**，不复制第二份）。
- 内容：配置项+校验（非法值启动与**热加载**均失败关闭，热加载用例必测）、
  参与集合三条件、urgency/D/norm 公式、排序加权接入（同优先级组内，接入点
  前先盘点该 helper 全部调用点，登记无旁路）、k=0 等价性、钉行为测试全清单
  （§4.2，含裁定配套两用例：Status=3 剔除断言耗尽包 total 不进 D、
  Asia/Shanghai 跨时区边界）。

**Card E — 错误键原子迁移（唯一后端迁移所有者）**
- 文件：backend 存量迁移（指定唯一迁移文件+测试）、
  `frontend/src/utils/codebuddyCredit.ts`、`AccountUsageCell` 相关、
  `types/index.ts`、相关 spec。
- 内容：**原子/幂等迁移**存量 `codebuddy_quota_error` →
  `codebuddy_credit_error`（迁移失败即阻断），删除旧键；迁移后旧引用
  grep 归零验证；**然后一次切换全部读写端，禁止任何双名读取/兼容层**
  （红线：无双名共存期）。前端读侧切换与本卡同任务边界完成。

### 批 2（依赖批 1 产物；A+B 同任务边界合入）

**Card B — 唯一条件更新入口 + 五场景 DB 门禁**
- 文件：`internal/repository/`（新增 `account_extra_conditional.go` + test）、
  quota service 写路径接入（替换 `queryUsageForAccount` 全部写入口）。
- 内容：快照专用条件更新 repo 方法（§0.2 已证 UpdateExtra 不足；单一新
  入口）：成功写仅当 `(更新时间, version)` 字典序单调通过时原子提交，
  **同事务写本次解析错误结果（本次无错误才清除旧错误）**、version 原子
  自增、停写旧 total/used 键；**失败写走同一字典序比较器**
  （attempt_time, version），失败版本持久化，旧失败请求不得回退新状态。
  - **DB-backed 五场景回归 = 完成门禁（不可跳过）**：dockerized postgres
    （本地 docker，`go test -tags integration`），五场景 = 延迟失败/并发
    成功失败/同戳乱序/失败后成功清错误/先发后至跨版本清除过期错误；
    命令与证据位置固定于验收清单；**环境不可用 = B4 未完成**，不得以
    "待补验"登记替代。集成测试并验证 SQL 谓词/事务原子性/隔离行为。
- 部署顺序：与 Card A 同变更合入；存量数据迁移（Card E）先行。

**Card F — 前端分包展示 + 旧总量展示退役**
- 文件：`utils/codebuddyCredit.ts`（+spec）、`AccountUsageCell.vue`、
  i18n zh/en。
- 内容：分包卡片渲染（名称/剩余/总量/到期/错误标记/更新时间）、合计=
  Σ 派生（异单位不计入并标注）、旧 total/used 展示退役。

### 批 3（收尾，主会话）

**Card C — 旧键清理（grep 消费方证据 + 删除）**：依赖 F；逐键
`codebuddy_credit_total/used` 及旧 `codebuddy_quota_error` 残留 grep，
无消费方删除、有消费方登记归属。
**运维取证**：~~跨实例单写者证明~~（已前置 §0.3）；三态 domain fixture
断言核验；凭证脱敏 Bearer 零命中 grep 断言（扩展至全部 B4 证据与日志）；
**上线观测**（§0.1 #8）：快照年龄/无效分包错误计数/条件更新拒绝计数
结构化 slog；无既有指标入口的部分登记残余风险。

## 4. 验收（§5 B4 行对照）

脱敏 fixture 落盘✅（探针轮）；规范单位钉死✅；**跨实例单写者证明（已前置
§0.3，派发前完成）**；三态 domain 请求 fixture；快照 UI 卡片；
**DB-backed 五场景全序回归 = 完成门禁（不可跳过，环境不可用 = B4 未完成）**；
used_percent 单写者证明+旧键清理后阈值回归；公式方向回归（remaining→0 时
used_percent→100；耗尽包不稀释分母断言）；k 非法值失败关闭（含热加载）；
k=0 等价性；错误键原子迁移+旧引用归零；旧总量展示退役+无消费旧键清理
grep 证据；上线观测（快照年龄/错误计数/条件更新拒绝，slog）。
每卡默认验证 = 定向测试（**必带 `-tags unit`**）+ `go build`/`vue-tsc`；
收敛后全量一轮（高风险边界：调度/资金口径 → 全量触发）。

## 5. 红线提醒（派发单逐卡带）

- 执行会话禁 git 写操作；边界外发现 BLOCKED 顶回。
- 禁兜底/降级：校验失败一律失败关闭，不静默截断钳位。
- 凭证/fixture 脱敏：Bearer/X-User-Id/X-Tenant-Id 零出现。
- 契约字段名以 §4.1 为准，不得自创键名。
