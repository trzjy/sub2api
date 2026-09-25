# 异常调用分析（防中转商薅羊毛）设计方案

**Date:** 2026-09-24
**Status:** Final v7（外审 7 轮：6 轮 45 项发现全部采纳回修，第 7 轮 approve 无阻塞收敛，见 §12）
**Owner:** 主会话（方案制定）

## 1. 问题背景

不限量订阅（`subscription_type=subscription` 分组 + 日/周/月 USD 限额为 NULL，`backend/ent/schema/group.go:82-96`）存在被其他中转商批量转售薅羊毛的风险：中转商购买一份（或多份）不限量订阅，转售给多个下游客户，导致上游账号资源被远超个人合理用量地消耗。现有体系只有 RPM/并发/Key 金额窗限流等硬限制，没有任何针对调用行为模式的异常分析与识别能力（全库无 abnormal/anomaly/suspicious 相关实现；`security-ban-prevention-plan.md` 只覆盖上游防封，与本需求正交）。

本方案新增一个**纯离线分析**功能：定时聚合 `usage_logs`，按规则对订阅用户打风险分，产出"异常调用分析报告"，展示在管理仪表盘与专属分析页。

## 2. 用户裁定（2026-09-24，强制约束）

| # | 裁定 |
|---|------|
| 1 | **仅人工处置**：检测器不执行任何自动动作（不自动禁用 key、不自动封号、不自动调限额、不自动降级）。处置动作由管理员在现有用户管理入口手工执行，本功能只提供跳转链接。 |
| 2 | **告警展示在管理仪表盘**：管理 Dashboard 增加风险摘要卡片；不接推送渠道。 |
| 3 | **R2 离群对照基线 = 同分组订阅用户群**（按当日同 `group_id` 的订阅用户用量分位数），不用全站基线。 |

明确边界：本功能是"分析报告"，不是"风控执行器"。报告状态流转（确认/忽略/解决）属于工作台记账，不是处置动作。

## 3. 数据基础（现有，不改动）

- `usage_logs`（`backend/ent/schema/usage_log.go`）：`user_id`/`api_key_id`/`subscription_id`/`group_id`/`ip_address`/`user_agent`/`model`/全量 token/cost/`stream`/`duration_ms`/`created_at`，已有 `(user_id, created_at)` 等复合索引。只读消费，不写不改。
- 订阅实例 `user_subscriptions`：**部分唯一约束 `(user_id, group_id)`**（`user_subscription.go:105`，WHERE deleted_at IS NULL）——一个用户可同时持有**多个不同分组**的活跃订阅，但每分组至多一份。分析的事实粒度因此必须是 `user_id + group_id`（见 §5/§6）。
- 用户限额来源：`users.rpm_limit`、`users.concurrency`、`groups.rpm_limit`、用户×分组 override（三层语义以 `billing_cache_service.go` `checkRPM` 权威实现为准，见 R3a）。
- 聚合 job 模式参照 `dashboard_aggregation_service.go`（Redis leader 锁多副本去重 + 总任务 deadline）；rollup 表模式参照 `channel_monitor_daily_rollup.go`；时区口径参照 `internal/pkg/timezone`（应用配置时区）。

**不触碰网关热路径**：分析完全离线，`RecordUsage` 链路零改动。

## 4. 检测规则（R1–R8，纯规则，无 ML）

**分析范围与资格谓词**：
- 进入分析的 usage_logs 行必须 `subscription_id IS NOT NULL`（订阅计费事实）。按量计费行不进入任何分组级指标与 R2 基线；**唯一例外是 R3a 用户全局作用域**，统计该用户全部请求（含按量行），与网关用户全局限流计数器作用域一致。
- 默认仅分析**不限量订阅分组**（组 `daily/weekly/monthly_limit_usd` 全 NULL），聚焦裁定主诉；settings `usage_risk_unlimited_groups_only`（默认 true）可放开到全部订阅分组——限量组的组限额是组属性，组内订阅用户同限额，基线不被污染，保留分析价值。
- **门槛与粒度清单**（全部规则无遗漏）：
  - 分组级规则（按 `user+group` 当日资格行总量过 `min_daily_requests`（默认 10），命中标 `scope=group` 只落本组行）：R2、R3a 分组作用域、R3b、**R7**；
  - 用户级规则（按该用户当日资格行**全量**过同一门槛，命中以 `scope=user` 复制到该用户当日**每个**合格分组报告行）：R1、R4、R6、R8、**R5**；
  - R5 精确定义：对**非空 `ip_address`** 统计该 IP 当日关联的 distinct 订阅资格用户数（跨分组累计），distinct 订阅资格 `user_id` ≥3 且该 IP 资格请求总量 ≥100 次时，对所有关联用户的合格分组报告行计命中；空 IP 行一律排除；
  - Dashboard 按用户最高分去重（§6.2）。
- **入榜阈值**：低于 `usage_risk_listing_min_score`（默认 40，medium 下限）仅落库留档（审计可显式查询）；**所有消费端（规则映射、Dashboard open 计数/级别分布/Top N、分析页默认榜单、handler 查询）统一消费同一 settings 值**，禁止硬编码。

| 规则 | 指标定义 | 默认阈值 | 权重 |
|------|----------|----------|------|
| R1 全天候活跃 | 当日活跃小时数（有调用的小时数）；连续活跃天数（活跃小时 ≥ 阈值的天数，取自 rollup 历史桶） | 活跃 ≥20h 且连续 ≥3 天 | 25 |
| R2 用量离群 | 当日 `sum(actual_cost)` 对比**同 `(group_id)` peer 群**当日分位数。**peer 群 = 当日该分组满足资格谓词 + `min_daily_requests` 门槛的 (user, group) 全体（含被测用户自身；同一 `unlimited_groups_only` 口径、同一应用时区日界）**；分位数 = 该群日 cost 的线性插值 P95；evidence 记录 peer_count、筛选条件与算法 | > 群 P95 × 1.5；peer_count ≥10，不足显式 `insufficient_peers`（不兜底） | 20 |
| R3a RPM 贴线 | 按分钟分别计算**用户全局计数**（该用户全部请求，含按量行）与**用户-分组计数**（仅资格行）两个真实作用域；适用限额的选择**语义**复用 `checkRPM`（`billing_cache_service.go:781`：override 替代分组限、override=0 仅免除分组检查、用户限为全局天花板），但限额**取值为分析执行时的当前有效配置**（usage_logs 不含历史限额快照，历史分钟不按当时限额重算）；evidence 记录用户限/分组限/override/计算时间。**任一作用域的真实分钟计数达到其适用上限 ×0.9** 即该分钟命中；命中分钟数由阶段一直接产出：分组作用域在分钟聚合中产出；**用户全局作用域由阶段一按 `user+minute` 独立聚合成用户日级结果（每轮现算、不持久化、不进分组 rollup 行），避免多分组重复计数或按量专属小时丢失** | 命中分钟 ≥30/天 | 15/作用域（两作用域同时命中记两条 scope 命中、各计 15——两判据独立；实现固化于规则引擎常量表，权重不可配置） |
| R3b 占用率贴线 | `sum(duration_ms) / 当日时长`（并发占用的日志级代理指标），按 `user_id+group_id` 粒度 | ≥ 0.5 | 10 |
| R4 IP 离散 | 当日 distinct `ip_address`（精确值，仅对候选做阶段二明细查询） | ≥10 | 15 |
| R5 同 IP 聚簇 | 某关联 IP 当日承载 **distinct 订阅资格 `user_id` ≥3**（同分组或跨分组累计；非空 IP；**“互不相干”无数据来源、该措辞废除，正式定义即 distinct user_id**——共享 NAT 误报由保守阈值 + 人工复核兜住）且该 IP 总量 ≥100 次 | 命中即记 | 25 |
| R6 客户端指纹 | UA 不在已知编程客户端白名单（可配置子串表）的请求占比 | ≥0.8 | 10 |
| R7 缓存失效 | `sum(cache_read_tokens)/sum(input_tokens+cache_read_tokens)`，要求当日输入量 ≥500 万 token | ≤0.05 | 10 |
| R8 多 key 均摊 | 用户（跨其全部分组聚合）≥3 个 key 且每个 key 承载 ≥20% 请求量（规避单 key 限流的特征） | 命中即记 | 10 |

- 分值映射：总分 ≥100 critical；70–99 high；40–69 medium；其余仅记录不入榜。
- **设置域校验（单一校验器）**：settings 保存与启动加载共用同一校验器，逐键声明类型/合法范围/是否允许 0（比例类 ∈ (0,1]、倍数类 >1、计数类 ≥1、天数类 ≥1）；**每条规则另设 `usage_risk_<rule>_enabled` 布尔开关（废除“阈值=0 禁用”表述，避免与 R3a 的 `override=0`——那是网关限额语义——混淆）**；run 只消费启动时固化的完整策略快照（§5）。
- 每条命中规则产出 `{rule, detail, points}` 与证据快照；R2 样本不足时在报告中显式记录 `insufficient_peers`（R1 历史未覆盖同理记 `insufficient_history`），均为 0 分、不入榜、在 rule_hits 留 points=0 条目注明原因，不得静默通过或按经验补基线（零假设约束）。
- 规则粒度归属以上述清单为准：IP/UA/key/活跃小时是用户级特征，不按分组切分；token/费用类指标按分组事实行计算；日报告按 `user_id+group_id` 出行（§6.2）。

## 5. 架构与数据流

```
usage_logs（只读）
   │  每小时 job（leader 锁 + 硬截止 < 锁 TTL；资格谓词过滤；重算最近 26h 事务化删除重建
   │  + 保留期对账游标：有界批次回填桶并派生完整键集重评报告）
   ▼
user_usage_metrics_rollup（新表，user+group ×小时桶，UTC）
   │  规则引擎（小时桶 → 日级指标 → 打分）
   │  两阶段：候选集 = 资格谓词 + 门槛的完整超集（无分数粗筛），
   │  阶段二按固定批次（每批 ≤100）遍历该全集做精确明细查询
   ▼
usage_risk_reports（新表，user+group ×日报告，应用时区日界）
   │
   ├── 管理仪表盘风险摘要卡片（dashboard stats 响应新增 risk_summary 块）
   └── 异常调用分析页（列表 + 证据下钻 + 跳转用户管理）
```

- **事实粒度**：全链路（rollup、报告、R2 基线）统一 `user_id + group_id + 时间`，与 `user_subscriptions` 唯一约束对齐；用户同小时多分组流量各行独立，不混合。
- **资格谓词前置**：阶段一聚合 SQL 内联资格过滤（`subscription_id IS NOT NULL` + 分组范围开关），按量行仅进入 R3a 用户全局作用域的独立聚合。
- **两阶段查询**：阶段一只用桶级聚合（GROUP BY user, group, minute/hour，带时间界与 statement timeout），分钟粒度计数在阶段一完成（R3a 双作用域）；**候选集 = 当日满足资格谓词 + `min_daily_requests` 门槛的 (user, group) 完整超集（不做分数粗筛——保证 R4/R5/R8 单独命中者与低分留档行必然进入阶段二并落库）**；阶段二按 **每批 ≤100 名** 循环遍历该全集（无每日总量截断语义），单批失败记录后继续下一批，批次与覆盖完成状态写入 `usage_risk_runs`（§6.3）；未完成的候选由下一小时任务继续覆盖（幂等重算天然收敛）。
- **窗口自愈 = 删除重建 + 全保留期对账**（不是裸 upsert）：
  - 26h 近窗：每次运行在事务内先删后建小时桶（**窗口起点按应用时区墙钟下取整到整点桶边界，删除与聚合统一使用对齐后边界——实际覆盖 ≥26h；防非整点起点下首段日志归入早于起点的桶而被删除边界遗漏、重插同键冲突；下取整实现为墙钟分钟秒纳秒回退（非 `time.Date` 重构）——DST 回拨歧义墙钟下 `time.Date` 的 occurrence 选择不受保证（实测取第二个即未来时刻），回退法锁定 ts 同 occurrence 的整点，任何时区下不向未来对齐；无 DST 时区两法完全等价，与 `date_trunc('hour', AT TIME ZONE)` 墙钟整点桶口径一致**），upsert 日报告；
  - **对账游标（日粒度推进 + 批内偏移）**：历史区间（近窗之外、保留期内）按日推进，日内候选全集以 **每批 ≤100 键** 为提交粒度循环（无每日总量截断语义），批内进度持久化到 `usage_risk_runs`（游标 = 日期 + 批内偏移），崩溃/预算耗尽后从批内偏移续跑；**每轮在处理候选批次之前，先为对账预留预算并至少完成一个有界批次**，保证单调推进不被候选批次饿死。older 日（离开 26h 近窗的历史日）候选集冻结——迟到日志在日界后分钟级落库、该日进入历史区间前已静默 ≥26h——OFFSET 续跑无漂移源；近窗每轮从偏移 0 全量重算天然自愈。**容量残留（登记）**：单日预备工作（桶重建/日聚合/候选收集/RPM 现算）为整日粒度，若病态巨日的预备成本本身超过单轮总预算，该日将跨轮停滞并拖累当轮近窗——部署须保证单日预备量级显著小于 4 分钟总预算（见"多副本与硬截止"）；
  - **批次语义（派生完整键集）**：日内每批（≤100 键）先从 `usage_logs` 派生该批键的事实，**UPSERT 新增与已有报告**（迟到日志使从未成报的 user+group 达标、策略放宽新增合格分组，都在此路径成报——对账不是只验旧行），批 UPSERT 阶段不触发失效；**当日全部批走完（或当日无候选）后，以该日完整候选键集**将集合之外的旧报告写 `invalidated_at`（失效只可能以完整日键集触发，部分键集失效是禁区）；重新有效（迟到日志、时区重建、策略重评后事实重新成立）恢复 `invalidated_at = NULL`。两个方向都**只动有效性维度，status 人工状态与审计字段原样保留**；
  - **桶回填**：同一游标顺带重建该批对应的小时桶（新表上线为空、时区变更全窗口重建都走此路径）；R1 依赖历史桶判断连续天数，**历史覆盖完成前 run 状态一律 partial**（新鲜度条展示覆盖进度）；**R1 在所需回看期未覆盖时记 `insufficient_history`、0 分、不入榜**（其余规则照常计算打分），`history_covered` 翻转后由对账游标重评补上 R1 并更新报告。
- **策略版本快照**：任何风险配置变更（阈值、UA 白名单、分组范围开关、入榜线）生成新 `policy_version`；run 内持久化**策略快照 jsonb（含全部阈值与当前应用时区名）**，报告记录产出版本。版本/时区变更的处置：驱动对账游标按保留窗口全量重评；**时区变更**走"换日迁移"——旧 `report_date` 行失效（保留人工状态与审计字段），按新日界生成新行，桶按新墙钟重建；重评完成前，新鲜度条暴露"重评进度 = 游标位置/窗口终点"。
- **保留期协调（单事务原子）**：风险表与 `usage_logs` 同库——两清在现有 `dashboard_aggregation_service.maybeCleanupRetention` 序列中以**同一 DB 事务**执行（风险报告与 rollup 按风险截止点、源日志按 `usage_logs_days` 截止点，两个截止点各自明确计算），任一失败整体回滚、本轮不推进，下轮按各自截止点幂等重试（按年龄截止的删除天然幂等，无需额外水位）；顺序承诺不提供原子性，故以事务为保证。失败回滚场景有集成测试。
- **启动重验**：源保留期 `usage_logs_days` 是部署配置，事后缩短不会触发 settings 保存校验——服务启动/配置装载时再次校验 `usage_risk_retention_days ≤ usage_logs_days`，违反则**分析任务失败关闭**（拒绝启动分析 job，结构化日志 + 新鲜度条呈现错误态），不得静默运行。
- **多副本与硬截止**：Redis leader 锁（key `usage:risk:analysis:leader`，TTL 5m，参照 `dashboard_aggregation_service.go:21-28`）。**获取锁即建立硬截止 context**：总预算 4 分钟（严格小于锁 TTL），所有 SQL/事务的 statement timeout ≤ 剩余预算，截止即取消并回滚在途操作，锁释放仅凭所有权令牌执行——保证租期内不会有第二副本并发删除重建同一窗口；含锁租期跨越与截止取消的并发测试。
- **列级更新隔离（防并发丢状态）**：报告表禁止整行覆盖——重算路径只 UPDATE 派生列（score/level/rule_hits/evidence/policy_version/invalidated_at），状态接口只 UPDATE 状态审计列且带原状态条件（`WHERE status = :old`，并发冲突即拒绝重试）；audit 事件与状态变更**同事务**落账。
- **新鲜度可观测**：分析页头部展示最近一次 run 的状态、数据截止时间、连续 partial 次数与失败批次数（消费 `usage_risk_runs` 唯一事实源），任务侧输出结构化日志；不做推送渠道（裁定 #2）。
- **时区口径**：小时事实桶按**应用时区墙钟整点**切桶（桶起点以 UTC instant 存储，兼容 UTC+05:30/05:45 等半小时偏移与 30 分钟 DST 切换，保证桶不跨越本地日界）；`report_date`、活跃小时、连续天数等日界统一用仓库应用时区（`internal/pkg/timezone`，StartOfDay 语义）；应用时区配置变更时，重建保留窗口内的桶与日报；**DST 公式**：R1 活跃小时 = 本地日内 distinct 桶起点（UTC instant）计数（回拨日两个同名墙钟小时计 2 个桶）；R3b 分母 = `[StartOfDay, StartOfNextDay)` 的实际 elapsed 时长（23/23.5/24/24.5/25 小时日按实际值）；24h 热力图按当日实际小时数渲染。验收含半小时偏移时区与 Lord Howe 30 分钟 DST 用例。

## 6. 新增存储

### 6.1 `user_usage_metrics_rollup`（user+group ×小时桶，UTC）

| 字段 | 类型 | 说明 |
|------|------|------|
| user_id / group_id | int64 | 唯一键 (user_id, group_id, bucket_hour) |
| bucket_hour | timestamptz | 应用时区墙钟整点桶起点（以 UTC instant 存储，不跨本地日界） |
| request_count | int | 该分组作用域请求数 |
| input/output/cache_read_tokens_sum | int64 | |
| cost_usd_sum | decimal(20,10) | `sum(actual_cost)` |
| occupied_ms_sum | int64 | `sum(duration_ms)`，R3b |
| non_whitelisted_ua_count | int | R6 |
| computed_at | timestamptz | |

注：R3a 贴线分钟数（用户-分组作用域与用户全局作用域）均**不落本表**——由阶段一按 user+minute / user+group+minute 每轮独立现算（见 §5 两阶段查询），本表仅承载小时粒度事实。

保留期默认 90 天（`usage_risk_retention_days`，约束见 §6.4），经 §5 保留期协调路径分批物理删。

### 6.2 `usage_risk_reports`（user+group ×日报告，应用时区日界）

| 字段 | 类型 | 说明 |
|------|------|------|
| report_id | bigint | **主键**（identity），API 路由与前端链接唯一引用 |
| user_id / group_id | int64 | 唯一键 (user_id, group_id, report_date)；同用户多分组各占一行 |
| report_date | date | 应用时区日界 |
| policy_version | int | 产出本报告的策略版本（§5 策略版本） |
| score | int | 规则加权和 |
| level | varchar(20) | low/medium/high/critical，CHECK 约束固定取值 |
| rule_hits | jsonb | `[{rule, scope(user/group), detail, points}]` |
| evidence | jsonb | 阶段二明细：ip_top（含关联用户数）≤50、ua_top ≤20、key 分布 ≤20、24h 热力、R2 群分位上下文（peer_count/筛选条件/算法）；**单行 evidence ≤256KB，超限截断并显式标记 truncated**；详情接口按需读取 |
| status | varchar(20) | **仅人工流转**四态 + CHECK；**状态机**：open→acknowledged→resolved，open/acknowledged→dismissed（终态）；open 统计只计 `status='open'`；重建/对账**永不写此字段** |
| invalidated_at | timestamptz | **仅系统写入**的对账失效时间，NULL=有效；失效与人工状态两维度独立表达，互不覆盖 |
| status_updated_by / status_updated_at | | 管理员操作留痕 |

同日重跑按 §5 列级更新隔离只写派生列；管理员已设置的状态不被覆盖（dismissed 后同日分数升高仅更新分数不动状态）。**仪表盘 open 计数口径** = `invalidated_at IS NULL AND status='open' AND score ≥ usage_risk_listing_min_score`；**分析列表默认展示全部有效状态行**（open/acknowledged/resolved/dismissed，仍限 `invalidated_at IS NULL`，低分留档行仅审计查询显式请求）——保证 acknowledged→resolved 流转在列表可达、报告不因默认过滤从管理视野消失（R5-1 已接受任务卡口径回写，2026-09-25 实施期外审第 6 轮 #8）。仪表盘 `risk_summary` 聚合时按 user_id 跨分组取**有效分组报告行的 `MAX(score)`** 展示（scope=user 命中分已含在各行分数内，不得再次累加）；命中明细取最高分行（或按 rule 去重合并），明细以分组行为准。

### 6.3 `usage_risk_runs`（运行覆盖台账）

| 字段 | 类型 | 说明 |
|------|------|------|
| run_id | bigint | **主键**（identity），“最近一次 run”与崩溃收敛按 run_id 定位 |
| run_at / window_start / window_end | timestamptz | 本次重算窗口（run_at 唯一索引） |
| candidates_total / batches_done / batches_failed | int | 候选总数与批次完成/失败计数 |
| failed_batches | jsonb | 失败批次的候选标识，供下一轮优先重试 |
| budget_exhausted | bool | 预算耗尽标志 |
| recon_cursor_date | date | 对账游标当前推进到的日期（保留期起点方向推进） |
| recon_batch_offset | int | 批内偏移（崩溃/预算耗尽后续跑位置） |
| recon_batches_done | int | 本次运行完成对账的有界批次数 |
| consecutive_partials | int | 连续 partial 运行次数（新鲜度观测用） |
| policy_version | int | 本轮使用的策略版本 |
| policy_snapshot | jsonb | 策略快照：全部阈值 + 应用时区名（时区/配置变更检测依据） |
| history_covered | bool | 保留窗口历史桶回填/重评是否完成（false 期间 status 一律 partial） |
| finished_at | timestamptz | 运行结束时间（running→partial 收敛时由收敛方写入） |
| failure_stage | varchar(30) | 失败/中断阶段（aggregate/candidates/reconcile/cleanup 等） |
| status | varchar(20) | running / completed / partial；**新 run 取得 leader 锁后，先把超期限（> 硬截止预算）的遗留 running 行原子收敛为 partial，并断言写入 finished_at 与 failure_stage**，防崩溃后永久 running |

### 6.4 证据与报告保留期（绑定源日志保留权威）

`dashboard_aggregation.retention.usage_logs_days`（viper 默认 90，`config.go:2516`，必须为正）驱动的 `CleanupUsageLogs`（`dashboard_aggregation_service.go:364-368`）是源日志寿命的唯一保留权威——**此前"usage_logs 无自动 TTL"的表述有误，特此纠正**。因此：

- 风险数据保留期 `usage_risk_retention_days`（默认 90）**必须 ≤ `usage_logs_days`**，settings 保存时校验，违反即拒绝保存（失败关闭，不静默钳制）；
- `usage_risk_reports`（含 evidence）与 rollup 经 §5 **保留期协调路径**清理：与源日志清理在**同一 DB 事务**内执行（两个截止点各自按配置计算），任一失败整体回滚——不变量由事务原子性保证，而非执行顺序；
- **双重校验**：settings 保存时 + 服务启动/配置装载时（防运维事后缩短 `usage_logs_days` 绕过保存校验）均失败关闭；
- 保留期绑定校验、清理顺序与失败场景均纳入验收测试。

## 7. 后端组件

| 组件 | 位置（对齐现有分层） |
|------|----------------------|
| 聚合 + 规则引擎 service | `backend/internal/service/usage_risk_analysis_service.go`（leader 锁、硬截止预算、26h 删除重建、两阶段分批、有界批次对账与桶回填、策略版本/时区变更检测、启动保留期重验） |
| repository | `backend/internal/repository/usage_risk_repo.go`（分钟/小时聚合查询、rollup 删除重建、报告**列级隔离更新**/对账失效、候选明细查询、运行台账、清理；索引：报告列表 (report_date DESC, score DESC) WHERE invalidated_at IS NULL、(user_id, report_date)、Dashboard 聚合部分索引、runs(run_at)、runs(status)；迁移验收验证查询计划） |
| admin handler | `backend/internal/handler/admin/usage_risk_handler.go` |
| 路由 | `backend/internal/server/routes/admin.go`：`GET /admin/usage-risk/reports`、`GET /admin/usage-risk/reports/:report_id`、`POST /admin/usage-risk/reports/:report_id/status`（仅状态记账，按 §6.2 状态机校验转移合法性）|
| 仪表盘集成 | `dashboard_handler.go` GetStats 响应新增 `risk_summary`（open 计数、各级别计数、Top N 报告，user 级聚合口径见 §6.2） |
| 阈值配置 | 现有 settings 体系（`setting_service.go`）：键前缀 `usage_risk_*`，含全局开关（默认开，纯分析零用户面影响）、`usage_risk_unlimited_groups_only`（默认 true，见 §4）、§4 全部阈值（**共用域校验器：settings 保存与启动加载走同一逐键校验——类型/范围/是否允许 0**）、逐规则 `usage_risk_<rule>_enabled` 开关、`usage_risk_listing_min_score`（默认 40，全端统一消费）、UA 白名单表、保留期（默认 90 天，**保存时 + 启动时校验 ≤ `dashboard_aggregation.retention.usage_logs_days`，见 §6.4**）；**任何键变更生成新策略版本并驱动保留窗口重评（§5）** |
| 迁移 | `backend/migrations/` 新增三张表 SQL（对齐现有编号递增）+ ent schema/codegen；`dashboard_aggregation_service.go` 的 `maybeCleanupRetention` 插入风险表清理调用（先于 `CleanupUsageLogs`，§5） |

报告状态变更写审计事件（现有 audit_log 体系，新增 `admin.usage_risk.status` 事件类）。

## 8. 前端

1. **管理仪表盘**（`frontend/src/views/admin/` Dashboard 视图）：新增"异常调用风险"卡片——open 报告数、级别分布、Top 5（用户/分数/命中规则），点击跳转分析页。
2. **异常调用分析页**（新视图，路由 `/admin/usage-analysis`）：
   - 页头新鲜度条：最近一次 run 状态、数据截止时间、连续 partial 次数、失败批次数（消费 `usage_risk_runs`）；
   - 列表：按日期/级别/用户/规则/分组筛选，按分数排序，默认榜单过滤 `score ≥ usage_risk_listing_min_score`（可显式查低分留档行）；
   - 证据下钻：24h 活跃热力、IP 明细（含同 IP 关联用户数）、UA 分布、key 用量分布、R2 群分位上下文、命中规则明细；
   - 操作：状态流转（按 §6.2 状态机：确认/忽略/解决）+ **跳转现有用户管理页**链接（裁定 #1：处置只走现有入口，本页不新建任何处置接口）；页面与 evidence 详情 RBAC 沿用现有用户管理同级管理员权限。

## 9. 明确不做（Out of Scope）

- 任何自动处置（禁用/封号/调限额/降级/熔断）——裁定 #1。`invalidated_at` 是数据对账标记，非处置。
- 推送通知渠道——裁定 #2。
- 在线/实时检测、请求路径拦截、ML 模型、IP ASN 数据库判断（可作二期候选）。
- 不修改 `usage_logs`/计费/网关任何现有代码路径。

## 10. 验收清单

- [ ] 三张新表迁移 + ent codegen 通过；`usage_logs` 零改动（diff 证明）。
- [ ] 聚合 service 单测：26h 窗口删除重建幂等（同输入两次运行结果一致）、leader 锁互斥、**硬截止（总预算 4 分钟 < 锁 TTL 5 分钟）：在途 SQL 随截止取消回滚、锁释放凭所有权令牌**、锁租期跨越并发用例、预算耗尽续跑。
- [ ] 事实粒度：**同用户多分组订阅**在同小时产生独立桶行与独立报告行，R2 基线互不污染。
- [ ] 资格谓词：按量行（`subscription_id IS NULL`）不进分组级指标与 R2 基线，但计入 R3a 用户全局作用域；`unlimited_groups_only=true` 时限量组不产生报告。
- [ ] 规则引擎单测：R1–R8 逐条命中/不命中/阈值边界；R2 群样本不足显式 `insufficient_peers`；分组级/用户级两套门槛独立判定；**R5 语义（非空 IP、distinct 订阅用户跨组累计、关联用户全部计命中）与空 IP 排除、跨组共享 IP、门槛边界用例**；**用户级命中以 scope=user 附着到该用户当日全部合格分组报告行、Dashboard 按用户最高分去重**；**R3a 双作用域（override 替代分组限、override=0 免除分组检查）限额选择语义与 checkRPM 一致，取值为分析时当前配置且 evidence 记录限额快照与计算时间；用户全局作用域按 user+minute 独立现算——含"多分组同分钟不重复计数"与"仅按量小时贴线分钟不丢失"用例**。
- [ ] 入榜阈值：低于 `usage_risk_listing_min_score` 不进 Dashboard open 计数/级别分布/Top N/默认榜单，但落库可审计查询；**含非默认阈值（如 25/60）用例，断言所有消费端一致**。
- [ ] 覆盖语义：候选 >100 时分批遍历全部、台账记录 batches/failed/budget_exhausted、失败批次下轮重试。
- [ ] 对账游标（日粒度+批内偏移）：模拟删除 26h 之外的历史源日志后，游标推进到该日时派生集合外的报告写 `invalidated_at`；批内偏移持久化、崩溃续跑；**连续 partial 场景下每轮预留对账预算、游标单调推进（不被候选批次饿死）；单批键数 ≤100 有上界；pending R1 全窗口重放可跨轮续跑（翻转轮回卷一次，后续轮从断点续跑不重置）**。
- [ ] 对账新增成报：迟到日志使从未成报的 user+group 达标、策略放宽新增合格分组时，对账批次从源事实派生完整键集并 UPSERT 新报告（非只验旧行）。
- [ ] 历史桶回填：新表上线后游标回填保留窗口小时桶，R1 连续天数在回填完成后可命中；覆盖完成前 run 一律 partial（history_covered=false）。
- [ ] 失效↔恢复双向：迟到日志/重评后事实重新成立时 `invalidated_at` 恢复 NULL，人工 status（四态全状态机转移矩阵断言：非法转移拒绝）与审计字段全程不变。
- [ ] 策略版本与时区：配置变更生成新版本并驱动保留窗口重评；报告与 run 记录版本与策略快照；**时区名持久化于快照，变更触发换日迁移——旧行失效（人工状态保留）+ 新日界新行 + 全窗口桶重建**；重评进度在新鲜度条可见。
- [ ] 台账自愈：进程在写 running 后崩溃，新 run 取锁后将超期限 running 行原子收敛为 partial，**断言 finished_at 与 failure_stage 已写入**。
- [ ] 时区与桶对齐：小时桶按应用时区墙钟整点切（含 UTC+05:30/05:45 与 Lord Howe 30 分钟 DST 用例），桶不跨本地日界；时区配置变更触发保留窗口重建。
- [ ] 对账（近窗）：模拟 usage cleanup 删除窗口内源日志后，下一轮将不再合格的报告写 `invalidated_at` 且不进 Dashboard open 计数；**人工 status 与审计字段在对账/重建全程不变**（含已 dismissed/resolved 的报告被失效的用例）。
- [ ] 保留期绑定：`usage_risk_retention_days > usage_logs_days` 时 settings 保存被拒绝；**服务启动时同样拒绝并使分析任务进入错误态（源保留期事后缩短用例）**；清理原子性集成测试：两清同一事务，人为使源日志删除失败 → 风险表删除一并回滚。
- [ ] handler 单测：报告列表/详情/状态流转（非法状态拒绝；对账接口外的任何入口不可写 `invalidated_at`）、dashboard `risk_summary` 响应结构（用户摘要 = MAX(score)，scope=user 分不重复累加用例）。
- [ ] 候选集完备：R4/R5/R8 单独命中（桶级粗分为 0）的用户必然进入阶段二并落库（含低分留档行）；无分数粗筛路径。
- [ ] R1 历史语义：回看期未覆盖时 `insufficient_history`、0 分不入榜；`history_covered` 翻转后重评补分。
- [ ] DST 公式：23/23.5/24/24.5/25 小时日的活跃小时与 R3b 分母断言；热力图按实际小时数渲染。
- [ ] R2 peer 口径：peer 群含被测用户、同资格谓词同门槛；evidence 记 peer_count/筛选条件/算法。
- [ ] 设置校验器：负值/超范围/比例越界在保存与启动两处均拒绝；`*_enabled` 开关与阈值解耦。
- [ ] 并发隔离：重算 UPDATE 派生列与管理员状态变更并发执行，人工状态与审计字段零丢失（列级 SQL 断言）。
- [ ] 路由与索引：`report_id` 主键寻址；列表/Dashboard/对账查询计划走索引（迁移验收）。
- [ ] evidence 边界：top-K 截断与 256KB 上限标记；详情按需读取；RBAC 与用户管理同级。
- [ ] 前端：仪表盘卡片 + 分析页渲染与筛选；处置仅跳转链接（无新建处置 API）。
- [ ] 定向测试 + 类型检查通过；DB 集成测试（rollup/报告 repo 层）在无 DB 派发环境下登记待补验。
- [ ] 全局一致性：无第二处限制/扣费逻辑引入；报告状态流转有审计留痕；阈值 settings 默认值与文档一致。

## 11. 风险与开放项

- 大流量站点分钟/小时桶聚合的 DB 压力：已用时间界 + statement timeout + 4 分钟总预算 + 分批阶段二约束；上线后观察单次运行耗时与台账，超预算再评估读副本/分片窗口（不在本期）。
- R3a 分钟计数为离线重算，不与网关在线限流计数器共享存储（Redis 窗口），二者限额选择语义同源（checkRPM）但取值为分析时当前配置、数据独立：报告中注明指标为"日志重算口径 + 分析时限额快照"。
- R5 同 IP 聚簇仅统计订阅用户，避免家庭/公司共享出口 NAT 误报个人用户；阈值默认保守。

## 12. 外审记录

- 2026-09-24 Codex（gpt-5.6-sol）方案审：结论 block，7 项发现（4×P1、3×P2）**全部采纳**，逐条回修如下：
  1. P1 分组事实粒度 → §3/§5/§6 唯一键改为 `(user_id, group_id, 时间)`，R2 基线按分组行。
  2. P1 R3a 计算口径 → §4 R3a 重定义为分钟级双作用域计数，严格复用 checkRPM 限额选择语义；阶段一新增分钟粒度产出字段。
  3. P1 每日 100 名截断 → §5 阶段二改为批次上限语义 + `usage_risk_runs` 覆盖台账 + 失败批次重试。
  4. P1 upsert 伪自愈 → §5 删除重建 + 报告对账失效（`invalidated`，保留人工状态）；§10 增对账用例。
  5. P2 任务期限 vs 锁租期 → §5 总预算 4 分钟 < TTL 5 分钟，预算耗尽续跑；§10 增并发用例。
  6. P2 时区 → §5/§6.2 小时桶 UTC、日界应用时区、时区变更重建、DST 用例。
  7. P2 证据保留期 → §6.4 报告与证据 90 天清理，纳入验收。
- 无拒绝项：所有发现均有代码权威依据（user_subscription.go:105、billing_cache_service.go:781、dashboard_aggregation_service.go:21-28、internal/pkg/timezone），且修复均未越过裁定 #1 边界。
- 2026-09-24 复审第 2 轮（gpt-5.6-sol）：结论 block，1 项 P1 **采纳**——单一 `status` 字段无法同时表达人工处置状态与系统对账失效，重建会覆盖人工审计记录。回修：状态模型拆分为 `status`（仅人工流转，重建/对账永不写）+ `invalidated_at`（仅系统写入，NULL=有效）（§5/§6.2/§9/§10 已同步）。
- 2026-09-24 复审第 3 轮（gpt-5.6-sol，首次提交因 Codex 沙箱工具路由瞬态故障失败关闭、同路由重试成功）：结论 block，7 项发现全部采纳——
  1. P1 资格谓词缺失 → §4 新增分析范围与资格谓词（订阅计费事实 + `unlimited_groups_only` 默认 true + R3a 全局作用域例外）。"仅不限量组"部分采纳：用户主诉原文为"主要是防止不限量订阅"（非排他），限量组同组限额是组属性、基线不污染，故以默认开关聚焦不限量、可配置放开，不硬编码排除。
  2. P1 历史对账缺口 → §5 新增 90 天对账游标（分批推进、进度入台账），§6.3 增游标字段。
  3. P1 用户级规则附着语义 → §4 定义两套门槛与 scope=user 复制附着、Dashboard 用户级去重；§6.2 rule_hits 增 scope。
  4. P1 低分入榜 → §4/§6.2/§8/§10 统一 `score ≥ 40` 过滤口径，低分留档可审计。
  5. P1 在途 SQL 越过锁租期 → §5 硬截止 context + statement timeout ≤ 剩余预算 + 截止取消回滚 + 所有权令牌释放。
  6. P2 must_fix R3a 历史限额不可重建 → §4 R3a 明确"分析时当前有效限额"口径 + evidence 限额快照，删除历史等价表述。
  7. P2 residual 新鲜度不可见 → §5/§8 分析页新鲜度条（消费 usage_risk_runs）+ 结构化日志，不引入推送（裁定 #2）。
- 2026-09-24 复审第 4 轮（gpt-5.6-sol）：结论 block，8 项发现全部采纳——
  1. P1 保留期冲突 → 核实属实（`CleanupUsageLogs` 于 `dashboard_aggregation_service.go:364-368` 自动删源日志，`usage_logs_days` viper 默认 90，`config.go:2516`）；§6.4 重写为绑定源保留权威 + 保存时失败关闭校验，并纠正此前"无自动 TTL"的错误表述。
  2. P1 用户全局 RPM 粒度 → `rpm_line_minutes_global` 移出分组 rollup，改为阶段一 user+minute 独立现算（每轮重算、不持久化），§4/§6.1 同步。
  3. P1 R5/R7 语义缺失 → §4 门槛与粒度清单补全 R5（用户级、非空 IP、distinct 订阅用户跨组累计）与 R7（分组级），补空 IP/跨组共享/门槛边界用例。
  4. P1 对账游标饿死 → §5 每轮为对账预留固定预算（至少一个日期）且先于候选批次执行；增单调推进用例。
  5. P1 失效后无法恢复 → §5 双向对账：重新有效恢复 `invalidated_at=NULL`，人工状态不变；增三态恢复用例。
  6. P2 半小时时区桶错位 → §5/§6.1 桶按应用时区墙钟整点切、UTC instant 存储、时区变更重建；增 UTC+05:30/05:45 与 Lord Howe DST 用例。
  7. P2 配置变更无版本 → §5 策略版本机制：变更生成版本、驱动保留窗口重评、版本入报告与 run、进度可见。
  8. P2 崩溃 running 台账 → §6.3 新 run 取锁后原子收敛超期限 running 行为 partial（含失败阶段与结束时间）；增恢复用例。
- 2026-09-24 复审第 5 轮（gpt-5.6-sol）：结论 block，10 项发现全部采纳——
  1. P1 对账漏新增成报 → §5 批次语义改为"派生完整键集，先 UPSERT 新增/已有，再失效集合外旧行"；§10 增新增成报用例。
  2. P1 R1 历史桶缺失 → §5 桶回填（游标顺带重建保留窗口小时桶，覆盖完成前一律 partial，history_covered 字段）；§10 增回填用例。
  3. P1 游标批次无上界 → §5 游标细化为 `日期+键区间+批内偏移` 有界可恢复批次；§6.3 增键区间/偏移字段；§10 增卡死与单调推进用例。
  4. P1 独立清理破坏寿命不变量 → §5/§6.4 唯一保留协调路径：现有 `maybeCleanupRetention` 先清风险表、成功后再删同截止点源日志，任一步失败两清不推进；§10 增顺序与失败集成测试。
  5. P1 启动绕过保留期校验 → §5/§6.4 服务启动/配置装载时再校验，违反则分析任务失败关闭（错误态可见）；§10 增源保留期事后缩短用例。
  6. P2 Dashboard 重复累计 scope=user 分 → §6.2 用户摘要改为有效分组行 MAX(score)，明细取最高分行；§10 增去重断言。
  7. P2 入榜线硬编码 → §4/§6.2/§7/§8 统一消费 `usage_risk_listing_min_score`（默认 40）；§10 增非默认阈值用例。
  8. P1 台账缺承诺字段 → §6.3 增 finished_at/failure_stage/policy_snapshot/history_covered；收敛更新断言两字段。
  9. P1 时区未持久化/换日未定义 → §5 策略快照含时区名；变更走换日迁移（旧行失效保人工状态+新日界新行+全窗口桶重建）；§10 增用例。
  10. P2 executor_cleanup score/level 混行 → §6.2 拆两列并加 CHECK 约束。
- 2026-09-25 复审第 6 轮：**路由变更**——corealgos 网关 Codex 沙箱文件读取工具故障（两次提交 + 独立探针确认，按合同失败关闭），用户裁定临时改道 api.shaulaapi.com 直连上游 `gpt-5.6-sol`（与前 5 轮同型保持评审延续性；审查提示词复刻 companion 契约；证据 /home/zjy/.codex-companion/usage-risk-plan-direct-reviews/r6/response.json）。结论 block，12 项发现（10×must_fix、1×executor_cleanup、1×residual）全部采纳——
  1. must_fix 清理顺序非原子 → §5/§6.4 两清改同一 DB 事务（同库可原子），按年龄截止幂等重试，无需水位。
  2. must_fix `/reports/:id` 无单列主键 → §6.2 增 `report_id` identity 主键 + 复合唯一键，§7 路由全部 `:report_id`。
  3. must_fix 状态枚举矛盾 → §6.2 冻结四态 + 状态机（open→acknowledged→resolved；open/acknowledged→dismissed），open 统计只计 open；§10 用例同步。
  4. must_fix 候选集不完备 → §5 候选集 = 资格谓词 + 门槛的完整超集，废除分数粗筛；§10 增完备性用例。
  5. must_fix 历史未覆盖 R1 语义 → §5 `insufficient_history`、0 分不入榜，覆盖后重评补分。
  6. must_fix DST 公式 → §5 活跃小时按 distinct 桶 instant、R3b 分母按实际 elapsed；§10 增 23/25h 日用例。
  7. must_fix R2 peer 口径歧义 → §4 R2 peer 群精确定义（含被测用户、同谓词同门槛），evidence 记录 peer_count/筛选/算法。
  8. must_fix R5 “互不相干”无数据来源 → §4 正式定义 distinct user_id ≥3，措辞废除。
  9. must_fix 设置域校验 → §4 单一校验器（保存+启动）、逐键范围、`*_enabled` 开关替代“阈值=0 禁用”，与 R3a override=0 解耦。
  10. must_fix 重算/状态并发覆盖 → §5 列级更新隔离 + 原状态条件 + audit 同事务。
  11. executor_cleanup 台账主键/索引 → §6.3 run_id 主键；§7 repository 补报告/台账索引与查询计划验收。
  12. residual evidence 敏感/大小 → §6.2/§8 top-K 与 256KB 上限、详情按需读取、RBAC 同用户管理。
- 2026-09-25 复审第 7 轮（同一直连路由，证据 /home/zjy/.codex-companion/usage-risk-plan-direct-reviews/r7/response.json）：结论 **approve——无阻塞发现（no blocking findings）**。审查逐面确认第 6 轮 12 项采纳均已落地（候选完整超集、列级更新隔离、单事务清理、report_id/状态机、DST 公式、R2 peer 口径、设置校验器、evidence 边界、run 台账与索引）；非阻塞提示：实现期严格按 §6.3/§7 建索引且迁移与 Ent 生成代码一致（executor_cleanup，已有验收）、高流量 DB 压力由时间界/批次预算/超时/续跑控制（residual，§5/§11 已覆盖）、四条"单一权威路径"（候选超集/R3a 全局分钟聚合/派生键集对账/列级隔离）不得在实现期新增旁路（suggestions，已作为实现约束记录于本节）。**方案收敛定稿，进入实施阶段。**
- 2026-09-25 实施期外审第 4 轮（最后一轮，review-r7/，gpt-5.6-sol companion 路由）：结论 block 9 项（6×P1+3×P2），主会话逐条直读裁定拒绝 0 项；用户三项裁定落地为本节 SSOT 修订——
  1. 六项实现实锤（RPM 聚合 CTE 列缺失×2 / finalize ctx 十秒超时泄漏 / pending R1 每轮重置游标 / 近窗日 retry 后双跑 / CompleteRun 漏 candidates_total）派卡修复（R8-1/R8-2），不改方案。
  2. 键区间维度废除：游标由"日期+键区间+批内偏移"三元修订为 **"日期+批内偏移"二元**（§5 对账游标、批次语义、§10 用例同步）；§6.3 删 `recon_cursor_key_range`；依据：older 日候选集冻结使 OFFSET 续跑无漂移源、键区间在实现中从未参与查询（死链）；容量残留（单日预备整日粒度、病态巨日可跨轮停滞）如实登记于 §5。
  3. `rpm_line_minutes_group` 死列废除：§6.2 删该列，R3a 双作用域贴线分钟均由阶段一每轮现算不落表（原仅用户全局作用域现算，本修订后口径统一）。
- 2026-09-25 实施期外审第 5-6 轮（review-r8/、review-r9/，主会话按用户授权自裁延续窄口径验证审）：第 5 轮 block 2 项（ent schema 死列失配 / retry 丢弃批内偏移）全采纳修复（R9-1/R9-2，处置按本方案 :171"三张表 SQL + ent schema/codegen"交付物收窄）；第 6 轮 block 8 项（5×P1+3×P2）逐条直读预核采纳 8/拒绝 0，用户四裁定：六项后端实锤+前端 pending 消费同批派卡修（R10），本节 SSOT 回写两处——§5 近窗窗口**按应用时区整点桶边界对齐**（起点下取整，删除与聚合统一边界，防非整点起点重插同键冲突，#1 P0 必炸修法依据）；**仪表盘 open 计数**与**分析列表全有效状态展示**口径分离（#8，R5-1 已接受任务卡口径回写）。其余六项（#2 用户级门槛候选合并 / #3 partial 收尾 nil JSONB / #4 版本缓存早推进 / #5 近窗失败日跨轮偏移 / #6 失效报告过滤 / #7 前端 pending）为代码修复不改方案语义（#2/#5 依本节既有"用户级全量过门槛""近窗每轮从偏移 0 全量重算"原文，属实现与方案既有语义的偏差修复）。
- 2026-09-25 实施期外审第 7 轮（review-r10/，窄口径验证审）：block 1 项（P1 DST 回拨重叠小时对齐到未来——`time.Date` 在回拨歧义墙钟下实测取第二个 occurrence 使 alignedStart 晚于 windowStart，DELETE/INSERT 双遗漏窗口开头段，违反 §5 "≥26h 覆盖"），主会话 /tmp/dstcheck 实证预核坐实（Go time.Date 歧义取第二个=未来；墙钟回退法锁定同 occurrence 整点；上海等价零行为变化），采纳并 R11 修复：对齐改墙钟分钟秒纳秒回退（抽纯函数 alignWindowStartToHour 参数化 loc，生产单调用点不变），§5 :83 回写 DST occurrence 语义。用户两裁定：立即修 + 修后外审⑧窄口径（只裁 DST 修复增量）。**Residual risk（登记）**：半小时 DST 回拨点（Lord Howe 类）若恰好落在 26h 窗口首小时内，回退对齐起点早于窗口首段的桶键下界一个桶（回拨点半桶），DELETE 下界不含该桶——潜在重插唯一约束冲突；`time.Date` 老实现同样存在（非本修复引入），reviewer 未发现，扩大 DELETE 下界属 §5 语义变更须用户裁定，暂登记不静默修改。
- 2026-09-25 实施期外审第 8 轮（review-r11/，窄口径验证审）：**R11 DST 修复零发现通过（R11 闭环）**；另 block 6 项（全 P2，范围外旧代码），逐条直读预核采纳 6/拒绝 0（ADJUDICATION-round8.md）：#1 清理事务路径分区探测/枚举绕 Tx 借池连接（MaxOpenConns=1 必死锁）；#2 列表筛选参数静默忽略（user_id=abc 退化全量、无效日期 500；user_id 项与登记在案⑤⑦重复一并闭环）；#3 min_daily_requests 默认值权威倒置（U2 轮"实现取 10→回写方案"，数值未经用户裁定；reviewer"方案无值"表述有误——系过时代码注释误导）；#4 R5 命中 IP map 遍历抖动（detail 展示不确定，命中/分数不受影响）；#5 UA 白名单 SQL LIKE 通配语义+空哨兵 vs 规则侧字面匹配（两路径事实分叉）；#6 evidence 上限校验不含 original_bytes 且未超限也设置（违反 §6.2 :125 与注释"超限才有"，边界可越 256KB）。用户三裁定：①同批派卡修（R12）；②**min_daily_requests 默认值 10 追认**（权威链补正：用户裁定→方案 §4 →实现；R12 同步修正 policy 代码过时注释）；③修后窄口径外审⑨。
