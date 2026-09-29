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
- `codebuddy_credit_packages_updated_at`（唯一 freshness 依据，失败绝不更新）。
- `codebuddy_credit_last_attempt_at`。
- `codebuddy_credit_version`（DB 条件更新原子自增的 monotonic 版本）。
- 错误键改名：`codebuddy_quota_error` → `codebuddy_credit_error`（写读两侧
  + 存量 extra 数据迁移 + 旧键归零）。
- `codebuddy_credit_used_percent` 保留：唯一写入者 = 分包快照成功提交事务；
  公式 = (Σtotal−Σremaining)/Σtotal×100（参与分包 = unit 有效分包）；
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

## 3. 卡拆分与批次（并发 ≤5，文件+测试对为单位）

### 批 1（三卡并行）

**Card A — 分包解析 + 快照写入 + used_percent 单写者**
- 文件：`internal/service/codebuddy_quota_service.go`（+test 文件；
  testdata 增脱敏精简 fixture）。
- 内容：解析分包条目（§1 契约）、校验失败关闭、storage 组装、
  used_percent 从参与分包推导、Σtotal=0 边界、`setBillingHeaders` 空
  domain 改 `X-No-Authorization: 1`（唯一例外，含三态请求 fixture 单测：
  cn 有 domain / intl 有 domain / 空 domain）。
- 写路径暂保持现有 UpdateExtra（Card B 接管条件更新）；错误键本卡即改名
  `codebuddy_credit_error`（写侧）。

**Card D — §4.2 紧迫度加权调度**
- 文件：`internal/service/gateway_scheduling.go`（+test）、config 定义、
  新 `internal/service/codebuddy_urgency.go`（+test：纯函数可全场景单测）。
- 内容：配置项+校验、参与集合三条件、urgency/D/norm 公式、排序加权接入
  （同优先级组内）、k=0 等价性、钉行为测试全清单（§4.2，含裁定配套两用例：
  Status=3 剔除断言耗尽包 total 不进 D、Asia/Shanghai 跨时区边界）。

**Card E — 错误键改名迁移（读侧+存量数据）**
- 文件：`frontend/src/utils/codebuddyCredit.ts`、`AccountUsageCell` 相关、
  `types/index.ts`、相关 spec；backend 存量迁移（启动迁移或 repo 读路径
  双名兼容一版后清理——**不允许双名长期共存**，迁移+旧键删除同卡完成）。
- 与 Card A 写侧改名对齐（A、E 同名契约，改动交界在测试 fixture）。

### 批 2（依赖批 1 产物）

**Card B — 条件更新比较器 + 五场景回归**
- 文件：`internal/repository/account_repo.go`（或新增
  `account_extra_conditional.go` + test）、quota service 写路径接入。
- 内容：DB 条件更新（成功写仅在 (updated_at,version) 单调通过时原子提交
  且同事务清错误；失败写仅覆盖错误字段+last_attempt_at）+ §4.1 五场景
  回归（延迟失败/并发成功失败/同戳乱序/失败后成功清错误/先发后至跨版本
  清除过期错误）。集成测试登记"需 DB 环境补验"项按既有惯例。

**Card F — 前端分包展示 + 旧总量展示退役**
- 文件：`utils/codebuddyCredit.ts`（+spec）、`AccountUsageCell.vue`、
  i18n zh/en。
- 内容：分包卡片渲染（名称/剩余/总量/到期/错误标记/更新时间）、合计=
  Σ 派生（异单位不计入并标注）、旧 total/used 展示退役。

### 批 3（收尾，主会话）

**Card C — 旧键清理（grep 消费方证据 + 删除）**：依赖 F；逐键
`codebuddy_credit_total/used` 及旧 `codebuddy_quota_error` 残留 grep，
无消费方删除、有消费方登记归属。
**运维取证**：跨实例单写者证明（生产部署形态副本数 + QuotaCheckService
拓扑证据落盘证据目录）；三态 domain fixture 断言核验；凭证脱敏 Bearer
零命中 grep 断言。

## 4. 验收（§5 B4 行对照）

脱敏 fixture 落盘✅（探针轮）；规范单位钉死✅；跨实例单写者证明（收尾）；
三态 domain 请求 fixture；快照 UI 卡片；五场景全序回归；used_percent
单写者证明+旧键清理后阈值回归；公式方向回归（remaining→0 时 used_percent→100）；
k 非法值失败关闭；k=0 等价性；旧总量展示退役+无消费旧键清理 grep 证据。
每卡默认验证 = 定向测试（**必带 `-tags unit`**）+ `go build`/`vue-tsc`；
收敛后全量一轮（高风险边界：调度/资金口径 → 全量触发）。

## 5. 红线提醒（派发单逐卡带）

- 执行会话禁 git 写操作；边界外发现 BLOCKED 顶回。
- 禁兜底/降级：校验失败一律失败关闭，不静默截断钳位。
- 凭证/fixture 脱敏：Bearer/X-User-Id/X-Tenant-Id 零出现。
- 契约字段名以 §4.1 为准，不得自创键名。
