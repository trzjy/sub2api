# 验收与评审记录

## v2：外部评审修复（codex changes-requested → 重构）

首轮实现（两阶段租约状态机）经 codex 异构评审拒收，3 个 issue 全部采纳：

1. HIGH 租约无 owner fencing，stale 接管可重复扣减权益 → **P1 结构性修复**：
   弃用两阶段状态机，处置（认领+码快照+作废/追回+终态）收敛进单个数据库事务
   （ent 事务 + 事务内裸 SQL，复用 `clientFromContext`/`config.ExecContext` 机制）。
   追回与终态同事务提交，失败整体回滚，无 stale 接管 → 无重复扣减路径。
2. HIGH processing 重入返回空 action 的 2xx，Worker 永久停报 → **P2 结构性修复**：
   不再有 processing 中间态；已处置订单幂等回放真实 action（2xx）；失败回滚到
   未处置（非 2xx），Worker `refund_reported` 不置位、兜底扫描下轮重报。
   Complete 0 行静默成功的问题随接口删除一并消失（0 行即报错）。
3. MEDIUM account_id 契约字段被静默忽略 → **P3**：payload account_id 必填
   （缺失 400），并与 claim.account_id 绑定校验（不匹配 409
   XIANYU_REFUND_ACCOUNT_MISMATCH）。

评审还要求覆盖 stale 接管/并发重入/去重组合测试：stale 接管已随状态机删除；
并发互斥改由「claim 行锁 + 码行 FOR UPDATE + 双方条件更新」保证（同事务提交），
集成测试覆盖追回失败整体回滚 + 重试成功、幂等回放、账号不匹配、无 claim。

## 变更清单

站点（backend/）：
- `migrations/243_xianyu_refund_handling.sql`：claim 行新增 refund_action/
  refund_detail/refund_handled_at + 未处置部分索引（refund_handled_at IS NULL）。
- `internal/repository/xianyu_refund_repo.go`：`ProcessRefundAtomically`——单事务内
  CAS 认领（account_id 绑定）+ 码快照 FOR UPDATE + delivered/unused 原子作废 +
  used 码回调追回 + 终态写入，全或无。
- `internal/service/xianyu_refund.go`：接口 + `ProcessRefundEvent` 编排
  （仅 status=refunded；no_claim 映射；提交后缓存失效）。
- `internal/service/redeem_clawback.go`：`RedeemService.ClawbackXianyuRedeemCodeTx`
  （调用方事务内追回）+ `InvalidateAfterClawback`（提交后缓存失效）。
- `internal/handler/xianyu_delivery_handler.go` + `server/routes/common.go`：
  `POST /api/v1/internal/xianyu/refund-events`。
- wire：ProvideXianyuDeliveryService 增两依赖；repository ProviderSet 注册新仓储
  （ent client 依赖）；`wire.Bind(XianyuRedeemClawback → *RedeemService)`；
  wire_gen.go 手工同步（wire 工具被存量 provideCleanup 未声明变量阻断，与历史做法一致）。

Worker（deploy-config/xianyu-auto-reply-src/）：
- `common/services/sub2api_refund_event_client.py`：上报客户端（配置检测/重试退避，
  复用 SUB2API_INTERNAL_BASE_URL/TOKEN）。
- `common/services/refund_cancel_service.py`：`report_refunded_orders_to_sub2api` 兜底
  批量上报（仅配置了回调才执行；refund_reported 标记去重；单轮限 20 条）。
- `common/services/order_service.py::_fetch_refund_orders_impl`：同步尾部挂钩。
- `common/models/xy_order.py` + `common/db/init_database.py`：xy_orders.refund_reported 列。

## 测试证据（隔离 worktree：HEAD + 仅本变更文件，排除并行会话在途改动）

- `go build ./...`、`go vet ./internal/... ./cmd/...` 全过。
- `go test ./internal/service/ ./internal/handler/ ./internal/repository/` 全绿
  （124s/44s/4s）。
- 新增单测：service 8 个（输入校验×5/无 claim/幂等回放/账号不匹配/clawback 提交后
  缓存失效/voided 不失效/错误传播）+ clawback 守卫 1 个 + handler 端点 1 个
  （401/400×2/200 no_claim/409 mismatch/200 voided）。
- 新增集成测试（testcontainers Postgres，跑通 243 迁移）5 个：
  delivered 认领即作废+终态+重复回放；**真实余额追回单事务提交**（balance 20→8 与
  claim 终态同事务）；追回失败整体回滚（claim 未处置、码不变，可重报）；
  账号不匹配拒绝且不动码；无 claim。
- Worker：`pytest backend-web/tests` 49 通过（含新增 3 个：契约/未配置跳过/单机不触 DB）。
- 首轮 codex 评审确认的核验项：`$2::interval`（v2 已移除该用法）、NUMERIC→float64
  扫描、迁移索引、wire_gen 参数顺序。

## Execution Omission Review（执行遗漏自查）

- 用户决策 1「退款成功才处置」→ 服务端仅接受 status=refunded，其余 400。✓
- 用户决策 2「delivered 作废；used 订阅扣天/余额扣值」→ 全覆盖；另含并发类型（码库
  支持的类型族一致性，非遗漏）。✓
- 漏项扫描：worker 兜底扫描覆盖「上报时站点不可用」场景（flag 未置位下轮重报）；
  站点幂等覆盖重复上报；追回失败整体回滚可重试。UI 展示 refund_* 审计列属设计文档
  明确的 v1 非目标。

## Global Consistency Review（全局一致性自查）

- SSOT：退款处置状态唯一收敛在 xianyu_order_claims.refund_*（refund_handled_at 为
  幂等锚点）；worker 的 refund_reported 仅为上报去重标记，不承载站点语义。
- 与管理端人工「作废」语义一致（均置 expired），未引入第二种作废状态。
- 未触碰既有 refund_cancel_url 通用注销机制（保留上游行为）；delivery-results
  协议未改动。
- 工作区存在另一并行会话的在途改动（对账/auto-deliveries 功能，涉及
  xianyu_order_claim_repo.go、xianyu_worker_client.go 等），与本次变更文件不重叠；
  本变更的验证在隔离 worktree（HEAD+仅本变更文件）完成，并已交叉检查无文件冲突。
- AGENTS.md 协议块未复制/未手改。
