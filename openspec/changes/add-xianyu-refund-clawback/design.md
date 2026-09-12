# 闲鱼退款成功追回兑换码（refund clawback）

## 背景与问题

闲鱼买家退款成功后，已发货的兑换码在站点没有任何自动处置：

- 发货链路：Worker 领码（`POST /internal/xianyu/redeem-codes/claim`）后码置为
  `delivered`，发货≠核销，买家在站点兑换前一直可正常兑换。
- Worker 自带「退款订单注销」机制（`refund_cancel_service.py`）只向账号级
  `refund_cancel_url` 通用回调 POST `{delivery_content, link_url}`，站点没有接收端，
  且 payload 缺 order_no，无法定位码。
- 结果：退款成功后 `delivered` 码仍可兑换（白嫖）；`used` 码已发出的权益也无法追回。

## 业务决策（用户拍板）

1. 仅「退款成功」（worker 侧 status=refunded，disputeStatus=5）触发处置；
   「退款中」不动作。
2. 处置规则：
   - 码 `delivered`（未兑换）→ 直接作废（status→expired，与管理端「作废」语义一致）。
   - 码 `used`（已兑换）→ 按码类型追回权益：
     - subscription：扣减该码发放的订阅天数（复用负数天码逻辑：剩余不足则取消订阅）；
     - balance：扣减该码面值余额（下限 0，与负数余额码一致）；
     - concurrency：扣减该码面值并发数（下限 0）；
     - invitation：无实质权益，不追回。
   - 无 claim 记录（非池卡订单）→ 记录后忽略。

## 方案

### 协议

Worker（scheduler 退款同步 `_fetch_refund_orders_impl`）在同步到 `refunded` 订单后，
向主程序上报：

```
POST {SUB2API_INTERNAL_BASE_URL}/api/v1/internal/xianyu/refund-events
X-Internal-Token: <SUB2API_INTERNAL_TOKEN>
{"order_no": "...", "account_id": "...", "status": "refunded"}
```

- 与 delivery-results 相同的通道与鉴权；未配置 base_url/token 时静默跳过（单机模式）。
- Worker 侧以 `xy_orders.refund_reported` 布尔列去重；上报成功才置位。
  站点侧同样幂等（见下），双保险。

### 站点侧（v2：单事务原子处置，取代 v1 租约状态机）

首轮实现采用「CAS 认领 processing → 追回 → Complete/Fail」两阶段 + 10 分钟 stale
接管，外部评审（codex）指出两个 HIGH：崩溃后 stale 接管可重复扣减权益（追回原语
无幂等键）；processing 重入返回空 action 的 2xx 会让 Worker 永久停报。v2 改为
**全处置在单个数据库事务内原子完成**，两个问题从结构上消除：

- 迁移 243：`xianyu_order_claims` 增加 `refund_action` / `refund_detail` /
  `refund_handled_at`（幂等去重锚点：非 NULL 即已处置）+ 未处置部分索引。
- 新仓储 `XianyuRefundEventRepository.ProcessRefundAtomically`（ent 事务 + 事务内
  裸 SQL，复用 `clientFromContext` / `config.ExecContext` 既有机制）：
  1. CAS 认领：`UPDATE claims WHERE order_no=$1 AND account_id=$2 AND
     refund_handled_at IS NULL RETURNING redeem_code_id`（account_id 绑定校验，
     防共享内网 token 下跨账号处置）；
     落空时区分：无领取记录（no_claim）/ 账号不匹配（409）/ 已处置（幂等回放 action）。
  2. 码快照 `SELECT ... FOR UPDATE`，与买家兑换（`StatusIn(unused,delivered)`
     乐观锁）行锁互斥，先提交者胜出；
  3. 分支处置：delivered/unused → 原子作废(expired)；expired/disabled → 记录忽略；
     used → 回调 `ClawbackXianyuRedeemCodeTx` 在**同一事务**内追回；
  4. 写入处置终态，随事务一并提交。任一步失败整体回滚——claim 审计列与码状态
     不残留中间态，Worker 可安全重报；不存在永久 processing，无需租约/stale 接管。
- `RedeemService.ClawbackXianyuRedeemCodeTx`：调用方事务内追回（订阅扣天复用
  reduceOrCancelSubscription，0 天按 30 映射与兑换一致；订阅已不存在视为无追回；
  余额/并发复用 GREATEST(...,0) 原语）；提交后 `InvalidateAfterClawback`
  失效核销用户缓存。
- `XianyuDeliveryService.ProcessRefundEvent`：校验（order_no/account_id/仅
  refunded）→ 原子处置 → 提交后缓存失效；no_claim 映射为 200 action 回显。
- 新端点 `POST /api/v1/internal/xianyu/refund-events`（X-Internal-Token + 16KB
  限长），payload `{order_no, account_id, status}`，account_id 必填并参与绑定校验。

### 已知取舍

- v2 无两阶段状态机：追回与终态同事务提交，消除重复扣减与停报窗口；代价是
  in-flight 期间的重复上报请求会阻塞在 claim 行锁上直到首个事务提交（秒级，可接受）。
- worker 每轮退款同步只上报 disputeStatus=5 列表中的订单；退款成功后该列表保留一段
  时间，期间天然重试。列表退出后的补偿依赖 worker 的 `refund_reported=0` 全量兜底
  扫描（每轮同步对本账号未上报的 refunded 订单重报）。

## 不做什么

- 「退款中」不做任何处置（含冻结）。
- 已兑换订阅码只扣天数，不尝试回收已产生的用量/流量。
- 不改管理端 UI（处置结果暂以 claim 行审计列 + Worker 日志呈现，可后续下钻展示）。
