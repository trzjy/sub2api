package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 账号级停调恢复探测（批1）的仓储原语。
//
// 唯一不变量：accounts.probe_due_at == extra.account_recovery_probe 各分量 due 的最小值
// （无分量为 NULL）。所有写路径都在**同一条 UPDATE** 内先算出写后的 extra，再由它派生
// 列值，禁止对列做 LEAST 增量维护。

const recoveryProbeExtraKey = service.AccountRecoveryProbeExtraKey

const (
	recoveryProbeComponentQuota     = "quota"
	recoveryProbeComponentTransport = "transport"
)

// probeExtraDueExpr 返回「从 jsonb 表达式取两分量 due 最小值」的 SQL 片段。
func probeExtraDueExpr(extraExpr string) string {
	return "COALESCE((" + extraExpr + " #>> '{account_recovery_probe,quota,due}')::timestamptz, (" +
		extraExpr + " #>> '{account_recovery_probe,transport,due}')::timestamptz)"
}

// recoveryWriteGuardSQL 是通用恢复写入的来源守卫：仅在「无停调 / 停调已过期 /
// 停调已归通用恢复所有」时落笔，绝不覆盖 CN/F3/web_console 的活跃停调。
const recoveryWriteGuardSQL = `deleted_at IS NULL
			AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until <= NOW()
			     OR temp_unschedulable_reason LIKE '%"source":"recovery_probe"%')`

// SetQuotaProbeSchedule 为额度类停调（429/过载）登记一次恢复探测。
// 不写 temp_unschedulable_until / reason：额度停调由 rate_limit_reset_at /
// overload_until 表达，写 until 会让影子账号被 global 429 连坐。
func (r *accountRepository) SetQuotaProbeSchedule(ctx context.Context, accountID int64, due time.Time, mode, kind string) (bool, error) {
	if r.sql == nil {
		return false, fmt.Errorf("account repository SQL executor is not configured")
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH base AS (
			SELECT id, COALESCE(extra, '{}'::jsonb) AS e
			FROM accounts
			WHERE id = $1 AND `+recoveryWriteGuardSQL+`
			FOR UPDATE
		), next AS (
			-- 注意：jsonb_set 的 create_missing 不会创建多级路径中的中间对象，
			-- 因此先在根层写入 account_recovery_probe（用 || 合并保留另一分量），
			-- 再写入本分量。嵌套多级 set 在没有父对象时会静默不生效。
			SELECT id, jsonb_set(e, '{account_recovery_probe}',
				COALESCE(e->'account_recovery_probe', '{}'::jsonb) || jsonb_build_object('quota',
					jsonb_build_object(
						'gen', nextval('accounts_recovery_probe_gen_seq'),
						'due', to_jsonb($2::timestamptz),
						'mode', $3::text,
						'kind', $4::text)), true) AS e2
			FROM base
		), fin AS (
			SELECT id, jsonb_set(e2, '{sched_state_revision}',
				to_jsonb(COALESCE((e2->>'sched_state_revision')::bigint, 0) + 1), true) AS e3
			FROM next
		)
		UPDATE accounts AS a
		SET extra = fin.e3,
			probe_due_at = `+probeExtraDueExpr("fin.e3")+`,
			updated_at = NOW()
		FROM fin
		WHERE a.id = fin.id
	`, accountID, due, mode, kind)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	r.afterRecoveryStateChange(ctx, accountID)
	return true, nil
}

// SetTransportRecoveryStop 为共享凭据/传输类停调（503、5xx/502）登记停调与恢复探测。
// 写 until（单调只增）+ 结构化 reason（source=recovery_probe）+ transport 分量。
func (r *accountRepository) SetTransportRecoveryStop(ctx context.Context, accountID int64, until, due time.Time, mode, reasonJSON string) (bool, error) {
	if r.sql == nil {
		return false, fmt.Errorf("account repository SQL executor is not configured")
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH base AS (
			SELECT id, COALESCE(extra, '{}'::jsonb) AS e, temp_unschedulable_until
			FROM accounts
			WHERE id = $1 AND `+recoveryWriteGuardSQL+`
			FOR UPDATE
		), next AS (
			SELECT id, jsonb_set(e, '{account_recovery_probe}',
				COALESCE(e->'account_recovery_probe', '{}'::jsonb) || jsonb_build_object('transport',
					jsonb_build_object(
						'gen', nextval('accounts_recovery_probe_gen_seq'),
						'due', to_jsonb($2::timestamptz),
						'mode', $3::text)), true) AS e2,
				CASE WHEN temp_unschedulable_until IS NULL OR temp_unschedulable_until < $4::timestamptz
					THEN $4::timestamptz ELSE temp_unschedulable_until END AS u2
			FROM base
		), fin AS (
			SELECT id, jsonb_set(e2, '{sched_state_revision}',
				to_jsonb(COALESCE((e2->>'sched_state_revision')::bigint, 0) + 1), true) AS e3, u2
			FROM next
		)
		UPDATE accounts AS a
		SET extra = fin.e3,
			temp_unschedulable_until = fin.u2,
			temp_unschedulable_reason = $5,
			probe_due_at = `+probeExtraDueExpr("fin.e3")+`,
			updated_at = NOW()
		FROM fin
		WHERE a.id = fin.id
	`, accountID, due, mode, until, reasonJSON)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	r.afterRecoveryStateChange(ctx, accountID)
	return true, nil
}

// RescheduleRecoveryProbeComponent 覆盖指定分量的 due（不是取更早），再由写后的
// extra 重算 probe_due_at。代际 gen 必须匹配，否则零写入（他因接管或新事件）。
func (r *accountRepository) RescheduleRecoveryProbeComponent(ctx context.Context, accountID int64, component string, expectedGen int64, newDue time.Time) (bool, error) {
	if r.sql == nil {
		return false, fmt.Errorf("account repository SQL executor is not configured")
	}
	if component != recoveryProbeComponentQuota && component != recoveryProbeComponentTransport {
		return false, fmt.Errorf("unknown recovery probe component %q", component)
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH base AS (
			SELECT id, COALESCE(extra, '{}'::jsonb) AS e
			FROM accounts
			WHERE id = $1
				AND (COALESCE(extra, '{}'::jsonb) #>> ARRAY['account_recovery_probe', $2, 'gen']) = $3::text
			FOR UPDATE
		), next AS (
			SELECT id, jsonb_set(e, '{account_recovery_probe}',
				COALESCE(e->'account_recovery_probe', '{}'::jsonb) || jsonb_build_object($2,
					COALESCE(e->'account_recovery_probe'->$2, '{}'::jsonb) || jsonb_build_object('due', to_jsonb($4::timestamptz))),
				true) AS e2
			FROM base
		), fin AS (
			SELECT id, jsonb_set(e2, '{sched_state_revision}',
				to_jsonb(COALESCE((e2->>'sched_state_revision')::bigint, 0) + 1), true) AS e3
			FROM next
		)
		UPDATE accounts AS a
		SET extra = fin.e3,
			probe_due_at = `+probeExtraDueExpr("fin.e3")+`,
			updated_at = NOW()
		FROM fin
		WHERE a.id = fin.id
	`, accountID, component, fmt.Sprintf("%d", expectedGen), newDue)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	r.afterRecoveryStateChange(ctx, accountID)
	return true, nil
}

// ClearRecoveryProbeComponent 在代际匹配时清除指定分量；transport 分量清除时
// 一并清掉仅属 recovery_probe 的 until/reason。列由写后的 extra 重算。
func (r *accountRepository) ClearRecoveryProbeComponent(ctx context.Context, accountID int64, component string, expectedGen int64) (bool, error) {
	if r.sql == nil {
		return false, fmt.Errorf("account repository SQL executor is not configured")
	}
	if component != recoveryProbeComponentQuota && component != recoveryProbeComponentTransport {
		return false, fmt.Errorf("unknown recovery probe component %q", component)
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH base AS (
			SELECT id,
				COALESCE(extra, '{}'::jsonb) AS e,
				temp_unschedulable_until,
				temp_unschedulable_reason
			FROM accounts
			WHERE id = $1
				AND (COALESCE(extra, '{}'::jsonb) #>> ARRAY['account_recovery_probe', $2, 'gen']) = $3::text
			FOR UPDATE
		), next AS (
			SELECT id,
				CASE WHEN (e #- ARRAY['account_recovery_probe', $2])->'account_recovery_probe' = '{}'::jsonb
					THEN (e #- ARRAY['account_recovery_probe', $2]) - 'account_recovery_probe'
					ELSE e #- ARRAY['account_recovery_probe', $2] END AS e2,
				CASE WHEN $2 = 'transport' AND temp_unschedulable_reason LIKE '%"source":"recovery_probe"%' THEN NULL ELSE temp_unschedulable_until END AS u2,
				CASE WHEN $2 = 'transport' AND temp_unschedulable_reason LIKE '%"source":"recovery_probe"%' THEN NULL ELSE temp_unschedulable_reason END AS r2
			FROM base
		), fin AS (
			SELECT id, jsonb_set(e2, '{sched_state_revision}',
				to_jsonb(COALESCE((e2->>'sched_state_revision')::bigint, 0) + 1), true) AS e3, u2, r2
			FROM next
		)
		UPDATE accounts AS a
		SET extra = fin.e3,
			temp_unschedulable_until = fin.u2,
			temp_unschedulable_reason = fin.r2,
			probe_due_at = `+probeExtraDueExpr("fin.e3")+`,
			updated_at = NOW()
		FROM fin
		WHERE a.id = fin.id
	`, accountID, component, fmt.Sprintf("%d", expectedGen))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	r.afterRecoveryStateChange(ctx, accountID)
	return true, nil
}

// ListProbeDueAccounts 返回探测时刻已到的账号（按 probe_due_at 升序、有界）。
func (r *accountRepository) ListProbeDueAccounts(ctx context.Context, now time.Time, limit int) ([]*service.Account, error) {
	if limit <= 0 {
		limit = 200
	}
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.ProbeDueAtNotNil(),
			dbaccount.ProbeDueAtLTE(now),
			dbaccount.SchedulableEQ(true),
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.DeletedAtIsNil(),
		).
		Order(dbent.Asc(dbaccount.FieldProbeDueAt)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	mapped, err := r.accountsToService(ctx, accounts)
	if err != nil {
		return nil, err
	}
	out := make([]*service.Account, 0, len(mapped))
	for i := range mapped {
		out = append(out, &mapped[i])
	}
	return out, nil
}

// ListModelRateLimitProbeDue 返回模型级限流条目探测时刻已到的 (accountID, scope) 候选
// （按 probe_due_at 升序、有界）。单条 SQL 同时带出 meta 的 last_event_at 原文作为事件
// 版本基线（空串 = 无 meta）；归属过滤由调用方在 Go 侧用 isModelProbeOwned 做一次。
func (r *accountRepository) ListModelRateLimitProbeDue(ctx context.Context, now time.Time, limit int) ([]service.ModelProbeTask, error) {
	if r.sql == nil {
		return nil, fmt.Errorf("account repository SQL executor is not configured")
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT a.id, e.key, e.value,
			COALESCE(a.extra->'model_rate_limits_meta'->e.key->>'last_event_at', '') AS meta_last_event_at
		FROM accounts a
		CROSS JOIN LATERAL jsonb_each(COALESCE(a.extra->'model_rate_limits', '{}'::jsonb)) AS e(key, value)
		WHERE a.deleted_at IS NULL AND a.schedulable AND a.status = 'active'
			AND e.value->>'probe_due_at' IS NOT NULL
			AND (e.value->>'probe_due_at')::timestamptz <= $1
		ORDER BY (e.value->>'probe_due_at')::timestamptz
		LIMIT $2
	`, now, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]service.ModelProbeTask, 0)
	for rows.Next() {
		var (
			accountID int64
			scope     string
			raw       []byte
			metaAt    string
		)
		if err := rows.Scan(&accountID, &scope, &raw, &metaAt); err != nil {
			return nil, err
		}
		var entry map[string]any
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, err
		}
		out = append(out, service.ModelProbeTask{
			AccountID:       accountID,
			Scope:           scope,
			Entry:           entry,
			MetaLastEventAt: metaAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// modelProbePatchGuardCTE 是 PatchModelRateLimitEntry 三分支共用的来源守卫：仅当目标行
// 未删除、且目标 scope 条目的 probe_gen 仍等于 expectedGen（FOR UPDATE 锁定读）时，
// 才在数据库当前行上落笔。任一支均只动目标 scope，不写 meta、不碰兄弟 scope。
const modelProbePatchGuardCTE = `WITH cur AS (
		SELECT id FROM accounts
		WHERE id = $1 AND deleted_at IS NULL
			AND (extra->'model_rate_limits'->$2::text->>'probe_gen')::bigint = $3
		FOR UPDATE
	)
	UPDATE accounts AS a
	SET extra = %s, updated_at = NOW()
	FROM cur WHERE a.id = cur.id`

// PatchModelRateLimitEntry 在代际匹配时对单个 (accountID, scope) 条目做最小改动：
//   - reschedule：合并 probe_due_at/probe_gen（失败重排，gen 取新值）；
//   - observe：合并 {$4: $5}（ObserveKey/Observe，观测写回）；
//   - delete-scope：从 model_rate_limits 桶移除该 scope（不清除 meta）。
//
// 零写入返回 (false, nil)；成功落笔后复用批 1 的调度副作用并返回 (true, nil)。
func (r *accountRepository) PatchModelRateLimitEntry(ctx context.Context, accountID int64, scope string, expectedGen int64, patch service.ModelProbePatch) (bool, error) {
	if r.sql == nil {
		return false, fmt.Errorf("account repository SQL executor is not configured")
	}

	var extraExpr string
	var args []any
	switch patch.Op {
	case service.ModelProbePatchReschedule:
		extraExpr = `jsonb_set(
				COALESCE(a.extra, '{}'::jsonb),
				ARRAY['model_rate_limits'],
				jsonb_set(
					COALESCE(a.extra->'model_rate_limits', '{}'::jsonb),
					ARRAY[$2]::text[],
					COALESCE(a.extra->'model_rate_limits'->$2::text, '{}'::jsonb) || jsonb_build_object(
						'probe_due_at', to_jsonb($4::timestamptz),
						'probe_gen', nextval('accounts_recovery_probe_gen_seq')
					),
					true
				),
				true
			)`
		args = []any{accountID, scope, expectedGen, patch.Due}
	case service.ModelProbePatchObserve:
		extraExpr = `jsonb_set(
				COALESCE(a.extra, '{}'::jsonb),
				ARRAY['model_rate_limits'],
				jsonb_set(
					COALESCE(a.extra->'model_rate_limits', '{}'::jsonb),
					ARRAY[$2]::text[],
					COALESCE(a.extra->'model_rate_limits'->$2::text, '{}'::jsonb) || jsonb_build_object($4::text, $5::text),
					true
				),
				true
			)`
		args = []any{accountID, scope, expectedGen, patch.ObserveKey, patch.Observe}
	case service.ModelProbePatchDeleteScope:
		extraExpr = `jsonb_set(
				COALESCE(a.extra, '{}'::jsonb),
				ARRAY['model_rate_limits'],
				COALESCE(a.extra->'model_rate_limits', '{}'::jsonb) - $2::text,
				true
			)`
		args = []any{accountID, scope, expectedGen}
	default:
		return false, fmt.Errorf("unknown model probe patch op %q", patch.Op)
	}

	result, err := r.sql.ExecContext(ctx, fmt.Sprintf(modelProbePatchGuardCTE, extraExpr), args...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil
	}
	r.afterRecoveryStateChange(ctx, accountID)
	return true, nil
}

// afterRecoveryStateChange 复用既有调度副作用：outbox + 单账号快照同步。
func (r *accountRepository) afterRecoveryStateChange(ctx context.Context, accountID int64) {
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue recovery probe state failed: account=%d err=%v", accountID, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, accountID)
}

// recoveryProbeReasonJSON 构造 transport 类停调的结构化 reason。
func recoveryProbeReasonJSON(mode string, statusCode int, message string, now time.Time) string {
	state := service.TempUnschedState{
		TriggeredAtUnix: now.Unix(),
		StatusCode:      statusCode,
		MatchedKeyword:  "account_recovery_generic",
		ErrorMessage:    message,
		Source:          "recovery_probe",
		RecoveryMode:    mode,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	return string(raw)
}
