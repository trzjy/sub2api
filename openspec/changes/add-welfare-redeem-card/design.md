# 福利兑换卡（批次发放 + 独立福利余额池 + 优先扣减）设计文档

> 状态：后端已实施（迁移 245 + 兑换/扣费/预检/清零/管理 API 落地，go build/vet/全量测试 51 包通过）；前端双 Tab 界面实施中　最后更新：2026-09-13
> 已知测试缺口：本地无 PG，「同批次第二张被唯一索引拦截」与「扣减顺序/清零」的库级集成测试未跑，需在有 DB 的环境补验（见 ACCEPTANCE.md）。
> 需求口径已由用户逐条拍板（见第二节），实施前不再变更；如需变更，本节同步更新后再动工。
> 验收方式：对照本文档与 ACCEPTANCE.md 核对代码落地情况。

## 一、要解决什么

运营场景：生成一批福利兑换卡发到群里给用户抢。一张卡可同时携带两类权益：

1. **订阅权益**：一个或多个订阅分组，每个分组各自选天卡 / 周卡 / 月卡时长；兑换后若用户已有同组订阅则顺延天数（复用现有 `AssignOrExtendSubscription`）。
2. **福利余额**：随机金额（如 1–3 元、2–5 元），独立于用户充值余额，兑换后 7 天内有效，过期自动清零；扣费时优先于充值余额。

现有 `redeem_codes` 体系（余额卡 / 并发卡 / 订阅卡）只支持"一码一权益一分组"，没有批次概念，也没有独立福利余额池，需要在此基础上扩展。

## 二、用户已拍板的口径（定案）

1. **卡的兑换有效期 = 生成后 24 小时内**（替代最初的"当天"口径，避免跨零点争议；以服务器时区为准）。
2. **批次之间互不影响**：同一用户可在 A、B 不同批次各兑一张；每张卡产生的福利余额独立记账、独立计时清零；扣减时**优先扣离清零时间最近的福利余额**。
3. **同批次每人限兑一张**；同一订阅分组重复获得时顺延天数。
4. **清零并发/在途请求不做强防护**：允许极端情况下把余额扣成负数，不为几分几毫增加设计复杂度。
5. **随机金额支持小数**（如 0.3 元也可出现），生成时在 [min, max] 区间均匀随机，保留 2 位小数。
6. **福利消费不计入返佣/分销报表**。

## 三、现状关键点（已核对到文件与行号）

- `backend/ent/schema/redeem_code.go:36-75`：`redeem_codes` 已有 `code/type/value/status/used_by/used_at/expires_at/group_id/validity_days`，硬删除策略。
- `backend/internal/domain/constants.go:68-71`：兑换类型枚举 `balance/concurrency/subscription/invitation`。
- `backend/internal/service/redeem_service.go:418-568`：`redeem()` 完整链路——限流（30 次失败封禁）、Redis 分布式锁、乐观锁 `WHERE status='unused'` 标记、事务内按类型发放、提交后失效缓存；**返佣钩子只在 `type == balance && value > 0` 时触发（:557），新增 welfare 类型天然不触发返佣**。
- `backend/internal/service/redeem_service.go:205`：`GenerateCodes` 批量生成 + 去重重试。
- `backend/internal/service/redeem_service.go:532`：订阅卡通过 `AssignOrExtendSubscription` 发放，同组自动顺延——福利卡订阅权益直接复用。
- `backend/internal/repository/usage_billing_repo.go:243` `deductUsageBillingBalance`：标准分组扣费的唯一 SQL 落点（同事务、先 `balance >= $1` 守卫、未命中再无条件扣并记 `BalanceOverdrafted`）——**福利优先扣减在此函数内扩展，不新增第二条扣费路径**。
- `backend/internal/service/billing_cache_service.go:879` `checkBalanceEligibility`：请求预检只看 `users.balance` 缓存；余额 ≤0（或低于最低保留金）直接拒绝。**用户充值余额为 0 但有福利余额时会被误拒，必须处理**（见第四节第 5 条）。
- 定时任务模板：`backend/internal/service/subscription_expiry_service.go` 等 7 个 ticker 模式任务，清零任务照此实现。
- 批量图片冻结链路（`reserveUsageBillingBatchImageBalance`，usage_billing_repo.go:275）与 `user_platform_quotas` 是独立资金路径，本期不动（见第五节）。

## 四、设计

### 1. 数据模型（迁移 245）

**`redeem_batches`（新表）**——批次元数据，管理端列表/导出用：

| 字段 | 类型 | 说明 |
|---|---|---|
| id | bigserial PK | |
| name | text | 批次名（运营填，如"国庆福利第一波"） |
| config | jsonb | 生成参数快照：分组+时长勾选、金额区间、数量 |
| code_count | int | 实际生成数量 |
| created_by | bigint | 管理员 ID |
| created_at | timestamptz | |

**`redeem_codes` 扩列**：

- `batch_id bigint NULL` → `redeem_batches.id`，仅福利卡有值。
- `type` 新增枚举值 `welfare`（domain 常量 `RedeemTypeWelfare = "welfare"`）。
- `value` 含义：福利卡为该卡的**实际随机金额**（生成时落定，导出时直接展示）。
- `expires_at` = 生成时刻 + 24h（口径 1）。
- `group_id`/`validity_days` 对福利卡**置空**，分组清单走新关联表（一卡多分组，单字段放不下）。

**`redeem_code_groups`（新表）**——福利卡的分组勾选快照：

| 字段 | 类型 | 说明 |
|---|---|---|
| redeem_code_id | bigint | FK → redeem_codes，级联删 |
| group_id | bigint | FK → groups |
| validity_days | int | 天卡=1 / 周卡=7 / 月卡=30（前端勾选映射） |

PK = (redeem_code_id, group_id)。

**`welfare_balances`（新表）**——独立福利余额池，一卡一条（口径 2 的"互不影响"由行隔离天然实现）：

| 字段 | 类型 | 说明 |
|---|---|---|
| id | bigserial PK | |
| user_id | bigint | FK → users |
| redeem_code_id | bigint | FK → redeem_codes（溯源到哪张卡） |
| batch_id | bigint | FK → redeem_batches |
| amount_initial | numeric(20,8) | 面值 |
| amount_remaining | numeric(20,8) | 剩余 |
| status | text | `active` / `exhausted` / `expired` |
| expires_at | timestamptz | 兑换时刻 + 168h（7 天） |
| created_at / updated_at | timestamptz | |

索引：`(user_id, status, expires_at)`（扣减与清零扫描共用）。

**"同批次限一张"约束（口径 3）**：`redeem_codes` 上加部分唯一索引
`UNIQUE (batch_id, used_by) WHERE batch_id IS NOT NULL AND used_by IS NOT NULL`。
数据库层兜底；并发抢兑时第二个事务在 `Use()` 更新处违反唯一约束，回滚并向用户返回友好错误 `本批次福利卡每人限兑一张`（需把该 unique violation 映射为专门错误码，不能返回 500）。

### 2. 生成（管理端）

复用 `GenerateCodes` 骨架，新增 `GenerateWelfareBatch(req)`：

- 入参：批次名、分组勾选清单 `[{group_id, validity_days}]`（≥0 组）、金额区间 `[min, max]`（≥0 区间可无，表示纯订阅卡；min/max 均 >0 表示含余额）、数量 N（上限沿用现有批量上限）。
- 校验：min ≤ max、min ≥ 0、N ≥ 1、至少勾一项权益（分组或余额区间二选一不能全空）、分组必须是订阅类型分组。
- 每张卡：`GenerateRandomCode()` 生成码；`value = round2(uniform(min, max))`（crypto/rand 派生，不用 math/rand 全局源）；`expires_at = now + 24h`；写 `redeem_code_groups` 明细。
- 事务：整批一个事务或分批事务均可，失败整批回滚（宁可重试不留半截批次）。
- 导出：现有批量导出扩展 welfare 类型，列含码、金额、批次名、过期时刻；批次列表页可再导出。

### 3. 兑换

`redeem()` 的类型分发新增 `case RedeemTypeWelfare`，全部在现有同一事务内：

1. `Use()` 乐观锁标记（含上述唯一索引兜底，违反即返回"限兑一张"错误）。
2. 逐行读 `redeem_code_groups`，每组调 `AssignOrExtendSubscription`（同组顺延，口径 3，复用 :532 的调用形态）。
3. `value > 0` 时插入 `welfare_balances` 一行：`amount_initial = amount_remaining = value`，`expires_at = now + 168h`，`status = active`。
4. 提交后失效缓存（auth cache + 余额相关缓存，复用现有 `invalidateRedeemCaches` 模式）。
5. **不触发返佣钩子**（:557 的条件已天然排除，保持不扩条件，口径 6）。

注意：`redeem()` 前置 switch（:458）要把 `welfare` 加进合法类型，否则走 `unsupportedRedeemTypeError`。

### 4. 扣费优先级（唯一改动的扣费落点）

改 `deductUsageBillingBalance`（usage_billing_repo.go:243），在**同一事务**内先福利后充值：

```
remaining = amount
-- 1. 锁出该用户全部可用福利余额行，离清零最近的先扣（口径 2）
SELECT id, amount_remaining FROM welfare_balances
WHERE user_id = $u AND status = 'active' AND expires_at > NOW() AND amount_remaining > 0
ORDER BY expires_at ASC, id ASC
FOR UPDATE
-- 2. 逐行扣：deduct = min(row.remaining, remaining)；扣到 0 的行置 status='exhausted'
-- 3. remaining > 0 的部分走现有 users.balance 两段式扣减（守卫 + 透支兜底）原样不动
```

- 过期但清零任务还没扫到的行，靠 `expires_at > NOW()` 条件在扣减侧自然跳过——**清零不及时不会造成误扣**（这是口径 4"允许简单"下仍必须保留的一条防线，成本为零）。
- 按口径 4，福利行扣减不做"够不够"预检以外的任何防负保护；充值余额侧沿用现有透支语义。
- 返回值扩展：福利扣了多少（用于 usage log 备注/对账），其余结果结构不变。
- `billing_type` / 幂等指纹（`buildUsageBillingFingerprint`）不变——同一 request_id 重试仍幂等。

### 5. 请求预检放行（容易被漏的必需项）

`checkBalanceEligibility`（billing_cache_service.go:879）改造：

- 现有缓存余额 ≥ 阈值 → 直接放行（绝大多数请求零额外开销）。
- 缓存余额低于阈值 → **降级直查 DB**：`SELECT COALESCE(SUM(amount_remaining),0) FROM welfare_balances WHERE user_id=$1 AND status='active' AND expires_at > NOW()`；`paid + welfare ≥ 阈值` 则放行。
- 不引入新的福利余额缓存层（避免新增一整套失效钩子）；只有"充值余额不足"的少数请求多一次简单 SUM 查询，符合口径 4 的简单原则。
- `balanceBelowEligibilityThreshold` 语义不变，最低保留金判断用合并余额。

### 6. 清零任务

新增 `welfare_balance_expiry_service.go`，照 `subscription_expiry_service.go` 的 ticker 模式：

- 周期：每分钟一轮（与现有 expiry 任务同量级）。
- 每轮：`UPDATE welfare_balances SET status='expired', amount_remaining=0, updated_at=NOW() WHERE status='active' AND expires_at <= NOW() AND amount_remaining > 0`；已 `exhausted` 的跳过；记录清零总额到日志/运营指标。
- 清零只动 `welfare_balances`，**绝不碰 `users.balance`**（充值余额不受影响是硬需求，SQL 里就没有 users 表）。
- 失败下一轮重试，天然幂等。

### 7. 报表与返佣隔离（口径 6）

- 返佣：兑换侧已天然排除（见四.3）；需回归确认 `redeem_service.go:557` 条件不被后续改动波及，加测试锁定。
- 用量/收入报表：现有收入口径基于支付订单与 `users.balance` 变动，福利余额扣减不经过这两处，天然不计入；管理端"余额流水/兑换统计"若按 `redeem_codes.type` 聚合，需把 `welfare` 单独成列或过滤，避免福利面值被算进"充值总额"。
- 导出/统计 SQL 中所有 `type IN (...)` 的清单都要过一遍，明确 welfare 进哪列。

### 8. 管理端与用户端界面

**入口位置（定案）：不新增侧栏入口，并入现有 `/admin/redeem` 兑换码管理页，页内分两个 Tab：**

- **Tab「福利批次」（默认首个，运营高频）**：批次列表——批次名、生成时间、数量、已兑/未兑统计、福利余额已清零总额；行内操作「查看卡密」（跳到 Tab 2 并按批次过滤）、「复制未兑换卡密」、「导出 CSV」、「新建福利批次」。
- **Tab「兑换码」**：现有码表原样保留，列增加"批次"，类型筛选增加 `welfare`。
- 新建批次用弹窗表单承载：批次名（默认填"福利批次-YYYYMMDD-HHmm"）、分组多选（每组随行下拉天/周/月卡）、金额区间 min/max（可空 = 纯订阅卡）、数量。提交中禁用按钮防双击重复建批；生成成功后弹窗直接给出「复制全部卡密」。

**发放便捷性（群发文案场景，逐项落）：**

1. 「复制未兑换卡密」一键复制为每行一个码的纯文本，直接粘进微信群；不夹带金额（金额是随机惊喜，运营要金额明细走 CSV 导出）。
2. 批次列表常驻已兑/未兑计数，发完后一眼看出抢了多少、剩多少，剩的可再复制补发。
3. 用户侧零新入口：现有 `/redeem` 兑换页不变，粘贴码即可；兑换成功提示明确写出"获得福利余额 X 元（7 天内有效）+ 分组 Y 顺延 Z 天"。
4. 个人余额页显示"福利余额"汇总行（剩余总额 + 最近一笔的清零时间），只读。
5. 管理端批次配置快照存 `redeem_batches.config`，事后能复盘"这批发了什么"，不靠记忆。

## 五、边界与不做的事

1. **批量图片冻结链路（hold/capture/release）不接入福利余额**：该链路有独立的冻结/结算状态机，接入成本高、福利金额小，冻结仍只走 `users.balance`；用户充值余额不足时即使有福利余额也无法发起批量图片任务。如需覆盖另立任务。
2. **`user_platform_quotas` 不接入**：平台配额是另一本账，与福利余额互不感知。
3. **不做清零瞬间的强并发防护**（口径 4）：扣减侧 `expires_at > NOW()` 过滤 + 清零任务幂等扫尾即为全部防线；允许极端窗口内出现分毫级负值/多扣，不补偿、不回填。
4. **福利余额不可提现、不可转让、不参与任何返佣**；管理员手动调整余额的现有入口（`ApplyRedeemBalanceAdjustment` 等）不动福利池。
5. **福利卡不支持负值/回收**：本期不做 welfare 类型的 clawback；发错了走删除未兑换码 + 过期自然失效，已兑换的福利余额由管理员手工 UPDATE 处理（写进运维手册即可）。
6. **时区**：24h/168h 均为相对时长，不涉及时区换算；所有比较用 DB `NOW()`。

## 六、迁移与兼容

- 迁移 245：建 3 张新表 + `redeem_codes.batch_id` 列 + 部分唯一索引。存量 `redeem_codes` 行 `batch_id` 全 NULL，唯一索引不影响旧数据。
- ent schema 同步更新；`RedeemTypeWelfare` 常量进 domain。
- 管理端旧兑换码列表/导出接口的返回结构只增不改，前端旧版本可继续工作。
- 回滚：drop 新表新列即可；扣费函数改动需随代码回滚一起 revert（代码与迁移同批次发布）。

## 七、测试要点

1. 生成：区间边界（min=max、含两位小数随机落点分布抽样）、分组必填校验、整批失败回滚、码去重重试。
2. 兑换：同批次第二张被唯一索引拦截且返回专用错误码（并发双开场景）；不同批次各兑一张成功；同组订阅顺延天数正确；24h 过期卡拒兑。
3. 扣费：福利优先、跨多行按 expires_at 顺序扣、行扣尽置 exhausted、剩余走充值余额、过期行被跳过、request_id 重试幂等不双扣。
4. 预检：充值 0 + 福利充足 → 放行；两者皆空 → 拒绝；阈值边界。
5. 清零：到期行清零置 expired、未到期不动、充值余额分文不动、任务重复跑幂等。
6. 报表：welfare 兑换不触发返佣（锁定 :557 条件的回归测试）、统计导出分类正确。
7. 迁移：245 在真实 PG 上 up/down 各跑一遍（项目已有迁移 _test.go 惯例，参照 244）。
