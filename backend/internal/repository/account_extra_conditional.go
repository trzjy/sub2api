package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 本文件实现 B4 Card B：CodeBuddy 积分快照**唯一条件更新写口**。
//
// 背景（§0.2）：accountRepository.UpdateExtra 实现为无条件 JSONB 合并
// （extra || $1，WHERE 仅 id+deleted_at）——无单调校验、无 version 比较、
// 无同事务错误清除联动，无法承载 §1 要求的"成功/失败同一 (event_time, version)
// 字典序条件更新"。故新增本文件的条件更新方法作为单一新入口，旧 UpdateExtra
// 对其余 40+ 调用方零改动。
//
// 统一排序键（§1 R3/R4/R5/R6/R7 逐轮收敛）：
//   * 成功路径比较时间 = 成功快照完成时刻（SuccessTime），持久化于
//     codebuddy_credit_packages_updated_at；
//   * 失败路径比较时间 = 抓取请求开始时刻（AttemptTime），持久化于
//     codebuddy_credit_last_attempt_at；
//   * 二者复用同一 tuple 比较器 (event_time, version)，由本文件的同一条原子
//     条件 UPDATE 裁决；version 仅作同时间戳 tiebreaker。
//
// canonical 读取转换（R6/R7 收敛，候选值与当前值同一表达式）：
//   * 时间 = (extra->>'key')::timestamptz，NULL/缺失 → '-infinity'::timestamptz；
//   * 版本 = COALESCE((extra->>'key')::bigint, 0)（0 = 尚无事件哨兵；
//     合法 attempt_version 从 sequence 起 ≥1）。
//   * 当前事件时间 = GREATEST(packages_updated_at 表达式, last_attempt_at 表达式)。
//
// 接受条件（tuple 语义，R6 #3 失败用相等+更大版本，不用严格 >）：
//   candidate_event_time > current_event_time
//   OR (candidate_event_time = current_event_time AND candidate_version > current_version)
//
// 原子性：条件判断、字段写入、version 写入位于**同一条**条件 UPDATE；只合并
// B4 负责的键（jsonb_build_object），保留其余 extra 内容（JSONB merge 天然保留）。
// RowsAffected=0 即条件更新被拒绝（调用方按失败路径处理，不得重试改写比较条件）。

// codebuddyAttemptVersionSequenceName 是 attempt_version 的 DB sequence 名
// （迁移 265 创建；§3 Card B R7/R8 钉死名称与起始值 1）。
const codebuddyAttemptVersionSequenceName = "seq_codebuddy_credit_attempt_version"

// codebuddyExtraTimeExpr 返回某 extra 时间键的 canonical timestamptz 表达式
// （NULL/缺失 → '-infinity'::timestamptz 哨兵）。候选值与当前已接受事件时间
// 必须用同一表达式（§1 canonical 转换）。
func codebuddyExtraTimeExpr(key string) string {
	return "COALESCE((extra ->> '" + key + "')::timestamptz, '-infinity'::timestamptz)"
}

// codebuddyExtraVersionExpr 返回某 extra 版本键的 canonical bigint 表达式
// （NULL/缺失 → 0 哨兵；合法 attempt_version ≥1）。
func codebuddyExtraVersionExpr(key string) string {
	return "COALESCE((extra ->> '" + key + "')::bigint, 0)"
}

// codebuddyCurrentEventTimeExpr 是**当前已接受事件**的比较状态：两个时间键的
// GREATEST（成功事务同事务写 packages_updated_at 与 last_attempt_at 均为
// success_time；被接受的失败满足 attempt_time >= 当前事件时间）。
func codebuddyCurrentEventTimeExpr() string {
	return "GREATEST(" +
		codebuddyExtraTimeExpr(service.CodeBuddyCreditPackagesUpdatedAtKey) + ", " +
		codebuddyExtraTimeExpr(service.CodeBuddyCreditLastAttemptAtKey) + ")"
}

// codebuddyTupleAcceptConditionSQL 构造统一 tuple 接受条件的 WHERE 谓词：
// candidate_event_time 用 $1（timestamptz）、candidate_version 用 $2（bigint）。
// 候选值与当前已接受事件值用同一 canonical 转换（R7 must_fix #1）。
func codebuddyTupleAcceptConditionSQL() string {
	current := codebuddyCurrentEventTimeExpr()
	currentVersion := codebuddyExtraVersionExpr(service.CodeBuddyCreditVersionKey)
	return "(" +
		"$1::timestamptz > " + current +
		" OR (" +
		"$1::timestamptz = " + current +
		" AND $2::bigint > " + currentVersion +
		"))"
}

// NextCodeBuddyCreditAttemptVersion 在抓取开始时取得 DB 分配的单调号。
// sequence 调用失败 → 返回 error，由调用方按"本次抓取失败关闭"处理（禁应用
// 本地计数器降级）。成功提交/失败提交阶段均不得再次调用递增（§1 R6 #1）。
func (r *accountRepository) NextCodeBuddyCreditAttemptVersion(ctx context.Context) (int64, error) {
	if r == nil || r.sql == nil {
		return 0, errors.New("codebuddy attempt version: sql executor not configured")
	}
	rows, err := r.sql.QueryContext(ctx, "SELECT nextval('"+codebuddyAttemptVersionSequenceName+"')")
	if err != nil {
		return 0, fmt.Errorf("codebuddy attempt version: nextval %s: %w", codebuddyAttemptVersionSequenceName, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("codebuddy attempt version: nextval %s: %w", codebuddyAttemptVersionSequenceName, err)
		}
		return 0, fmt.Errorf("codebuddy attempt version: nextval %s returned no row", codebuddyAttemptVersionSequenceName)
	}
	var version int64
	if err := rows.Scan(&version); err != nil {
		return 0, fmt.Errorf("codebuddy attempt version: scan nextval %s: %w", codebuddyAttemptVersionSequenceName, err)
	}
	return version, nil
}

// WriteCodeBuddyCreditSnapshot 提交成功事务的条件更新（单一原子 UPDATE）。
//
// 字段矩阵（R2 #3 逐项钉死）——成功事务写：分包快照 +
// codebuddy_credit_packages_updated_at(=SuccessTime) + reset_at + used_percent
// + codebuddy_credit_last_attempt_at(=SuccessTime) + version(=attempt_version，
// 禁提交期自增) + 本次解析错误结果（ErrorMsg 为空则清除旧错误）。
//
// 无错误时**删除**旧错误键（`- 'codebuddy_credit_error'`）；有错误时按原值写入。
// 两分支的接受条件由同一 tuple 谓词统一保护，且都在同一条 UPDATE 内原子完成。
func (r *accountRepository) WriteCodeBuddyCreditSnapshot(ctx context.Context, accountID int64, write service.CodeBuddyCreditSnapshotWrite) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("codebuddy credit snapshot: sql executor not configured")
	}
	packagesJSON, err := json.Marshal(write.Packages)
	if err != nil {
		return false, fmt.Errorf("codebuddy credit snapshot: marshal packages: %w", err)
	}

	// 成功事务写入的 B4 键集合（仅这些键；其余 extra 键原样保留）。
	successKeys := "jsonb_build_object(" +
		"'" + service.CodeBuddyCreditPackagesKey + "', $3::jsonb, " +
		"'" + service.CodeBuddyCreditPackagesUpdatedAtKey + "', to_jsonb($1::timestamptz), " +
		"'" + service.CodeBuddyCreditResetAtKey + "', to_jsonb($4::timestamptz), " +
		"'" + service.CodeBuddyCreditUsedPercentKey + "', to_jsonb($5::float8), " +
		"'" + service.CodeBuddyCreditLastAttemptAtKey + "', to_jsonb($1::timestamptz), " +
		"'" + service.CodeBuddyCreditVersionKey + "', to_jsonb($2::bigint)" +
		")"

	// 错误键处理：无错误 → 删除旧键；有错误 → 写入错误值。
	// 两分支均只合并 B4 键。
	extraExpr := "CASE WHEN $6::text = '' THEN (COALESCE(extra, '{}'::jsonb) - '" + service.CodeBuddyCreditErrorKey + "') || " + successKeys +
		" ELSE COALESCE(extra, '{}'::jsonb) || (" + successKeys +
		" || jsonb_build_object('" + service.CodeBuddyCreditErrorKey + "', to_jsonb($6::text))) END"

	query := "UPDATE accounts SET extra = " + extraExpr + ", updated_at = NOW() WHERE id = $7 AND deleted_at IS NULL AND " +
		codebuddyTupleAcceptConditionSQL()

	result, err := r.sql.ExecContext(ctx, query,
		write.SuccessTime, write.Version, string(packagesJSON), write.ResetAt, write.UsedPercent, write.ErrorMsg, accountID,
	)
	if err != nil {
		return false, fmt.Errorf("codebuddy credit snapshot: conditional update: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("codebuddy credit snapshot: rows affected: %w", err)
	}
	// RowsAffected=0 = 条件更新被拒绝（账号不存在/软删，或 tuple 条件不满足）。
	return affected > 0, nil
}

// WriteCodeBuddyCreditAttemptError 提交失败事务的条件更新（单一原子 UPDATE）。
//
// 字段矩阵（R2 #3）——失败事务**仅**更新 codebuddy_credit_last_attempt_at
// (=AttemptTime) + 错误标记 + version 持久化；**不得**修改 freshness/reset_at/
// 成功快照（不触碰 packages/packages_updated_at/used_percent/reset_at）。
func (r *accountRepository) WriteCodeBuddyCreditAttemptError(ctx context.Context, accountID int64, write service.CodeBuddyCreditAttemptErrorWrite) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("codebuddy credit attempt error: sql executor not configured")
	}

	failureKeys := "jsonb_build_object(" +
		"'" + service.CodeBuddyCreditLastAttemptAtKey + "', to_jsonb($1::timestamptz), " +
		"'" + service.CodeBuddyCreditVersionKey + "', to_jsonb($2::bigint), " +
		"'" + service.CodeBuddyCreditErrorKey + "', to_jsonb($3::text)" +
		")"

	query := "UPDATE accounts SET extra = COALESCE(extra, '{}'::jsonb) || " + failureKeys +
		", updated_at = NOW() WHERE id = $4 AND deleted_at IS NULL AND " +
		codebuddyTupleAcceptConditionSQL()

	result, err := r.sql.ExecContext(ctx, query,
		write.AttemptTime, write.Version, write.ErrorMsg, accountID,
	)
	if err != nil {
		return false, fmt.Errorf("codebuddy credit attempt error: conditional update: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("codebuddy credit attempt error: rows affected: %w", err)
	}
	return affected > 0, nil
}
