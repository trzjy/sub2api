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
