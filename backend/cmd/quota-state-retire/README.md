# quota-state-retire — 新链状态退役命令

执行双额度统一机制（线七 SSOT §7.3 / R19-F6）的**状态退役**：把本方案拥有的账号状态键
从库中清除，供**回滚发布闸门（R16-F2）**使用——回滚到不消费 `http_403_recovery` 的旧版本前，
必须先令「持有新链状态键的账号数 = 0」。

## 退役范围（仅本方案拥有的状态）

| 键 | 归属 | 处理 |
|---|---|---|
| `extra.http_403_recovery` | F3 403 content-policy 恢复链（状态 SSOT） | **整键移除** |
| `temp_unschedulable_until` / `temp_unschedulable_reason` | F1 额度生命周期链（reason 前缀 `cn_quota_exhausted`） | **仅当 reason 前缀匹配时清除** |

`extra.http_403_gen_counter`（代际计数键）**不删除**：R17-F3 要求计数键独立于恢复键、
跨恢复/转交保留以防代际复用；旧版本不消费该键，残留无害。

其余一切状态（401 / breaker / transport / 手工停调 / 外部 CRS 同步 / 平台特定暂停）都是
**他链状态，退役不动**（方案 §7.2 Phase 2 边界）。

## schedulable 重算（R20-F1，硬约束）

退役**绝不**把 `schedulable` 直接置真。对每个目标账号，在**同一条原子条件 UPDATE** 内按
「剩余阻断（他链归属状态）」重算：

- 他链 error（`status <> active` 且当前 error 不由 F3 拥有）→ **维持冻结**；
- 非本方案 reason 的 `temp_unschedulable_until` 未过期 → **维持冻结**；
- 他链置 `schedulable <> true`（且非 F3 拥有）→ **维持冻结**；
- 无剩余阻断才恢复可调度。

「当前 error 是否由 F3 拥有」由 F3 键内 `state_revision` 是否等于账号全局
`sched_state_revision` 判定（R18-F2）。任一他链替换过账号 error/调度阻断状态都会使全局
revision 自增（R19-F2，各写入原语单语句维护），从而令 F3 所有权失配、退役保守地维持冻结。

瞬时调度门（`rate_limit_reset_at` / `overload_until` / `expires_at`）**不**折叠进 `schedulable`
列——它们由调度器查询独立实施，折叠会在他链未复位时造成永久过冻。

## 原子性与并发

每个目标账号的退役是**一条**条件 UPDATE：

1. `WHERE` 含 CAS 守卫——F3 键 `generation` 未变、scheme temp 的 `reason+until` 未变。
   故「退役执行中该账号被写入新 F3 状态」时该语句 0 行命中、**不写、不丢新写入**；
2. `SET` 内 `status`/`error_message`/`temp_*`/`schedulable`/`extra` 在一次行级写中生效，
   Postgres 保证单语句原子，无需显式事务；
3. `extra` 移除 F3 键与自增 `sched_state_revision` 复用同一 `jsonb_set` 表达式（同点出生）。

**幂等**：第一次执行后目标账号不再持有本方案键；第二次枚举为空集合、零写入、退出 0。

## 幂等与共存

- 新链状态 + 401 error（无 F3 键）并存 → 退役清 F3 键，账号因 401 error 仍**维持冻结**；
- F3 键 + 非 F3 reason 的 `temp_unschedulable` 未过期 → 退役后 `until` **保留**、仍冻结；
- 退役执行中写入新 F3 状态 → CAS 失配、跳过、**不丢新写入**。

## 前置与注意

- 命令同时是方案 §7.2「本方案退役」的清理工具——不预留退役命令则回滚闸门不可执行。
- 建议在**回滚镜像部署前**执行；执行后按 runhook 复核 `remaining_f3_key_holders=0`。
- 命令会为受影响账号投递 `scheduler_outbox`（best-effort）以刷新运行中网关的调度快照；
  若命令与网关并发运行后仍观察到陈旧调度，重启网关/outbox 消费即可。

## 用法

```bash
# dry-run（默认）：只统计不写库
go run ./cmd/quota-state-retire

# 实际退役
go run ./cmd/quota-state-retire --execute
```

输出示例（`remaining_f3_key_holders=0` 即回滚闸门证据）：

```
mode=execute candidates=3 processed=3 skipped=0 f3_keys_removed=2 scheme_temp_keys_cleared=1 unfrozen=1 kept_frozen=2 remaining_f3_key_holders=0
```

退出码：0 正常；2 退役后仍检测到 `http_403_recovery` 持有者（闸门未满足，需排查并发写入后重跑）。

配套运维文档：`docs-local/f3-rollback-runbook.md`（不入镜像、不 commit）。
