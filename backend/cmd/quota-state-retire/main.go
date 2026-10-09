// quota-state-retire 是双额度统一机制（线七 SSOT，方案 §7.3 / R19-F6）的
// 可执行退役命令：把本方案拥有的账号状态键从库中清除，供回滚发布闸门
// （R16-F2）使用——回滚到不消费 http_403_recovery 的旧版本前，必须先令
// "持有新链状态键的账号数 = 0"。
//
// 本方案拥有的状态（且仅这些）：
//   - F3 403 content-policy 恢复链的状态 SSOT 键 extra.http_403_recovery（整键移除）；
//   - F1 额度生命周期链写入的 temp_unschedulable_until / temp_unschedulable_reason
//     （reason 以 cnQuotaExhaustedReasonPrefix 前缀开头时清除）。
//
// 其余一切状态（401 / breaker / transport / 手工停调 / 外部 CRS 同步 / 平台特定
// 暂停）都是他链状态，**退役不动**（方案 §7.2 Phase 2 边界）。
//
// schedulable 重算（R20-F1，硬要求）：退役**不得**把 schedulable 直接置真。对每个
// 目标账号，在同一原子条件 UPDATE 内按"剩余阻断（他链归属状态）"重算：
//   - 仍持有他链 error（status<>active 且当前 error 不由 F3 拥有）→ 维持冻结；
//   - 仍有非本方案 reason 的 temp_unschedulable_until 未过期 → 维持冻结；
//   - 仍被他链置 schedulable<>true（且非 F3 拥有）→ 维持冻结；
//   - 无剩余阻断才恢复可调度（schedulable=true）。
//
// "当前 error 是否由 F3 拥有"由 F3 键内 state_revision 是否等于账号全局
// sched_state_revision 判定（R18-F2）：任一他链替换过账号 error/调度阻断状态都会使
// 全局 revision 自增（R19-F2，各写入原语单语句维护），从而令 F3 所有权失配、退役
// 保守地维持冻结——这正是"F3 键与本方案外 error（如 401）并存"场景的正确行为。
//
// 幂等：重复执行零副作用。第一次执行后目标账号不再持有本方案键，第二次枚举为空集合、
// 零写入、正常退出 0。
//
// 用法：
//
//	go run ./cmd/quota-state-retire                 # dry-run：只统计不写库
//	go run ./cmd/quota-state-retire --execute       # 实际退役（回滚前执行）
//
// 原子性论证（逐账号）：每个目标账号的退役是**一条**条件 UPDATE——
//  1. WHERE 含代际/快照 CAS（F3 键 generation 未变、scheme temp 的 reason+until 未变），
//     故"退役执行中该账号被写入新 F3 状态"时该语句 0 行命中、不写、不丢新写入；
//  2. SET 内 status/error_message/temp_*/schedulable/extra 全部在一次行级写中生效，
//     Postgres 保证单语句原子，无需显式事务；
//  3. extra 移除 F3 键与自增 sched_state_revision 复用同一 jsonb_set 表达式（同点出生）。
//
// 真实并发语义（行锁竞争、jsonb 条件）超出注入式 unit 基建能力，按 C1-a-r3/C1-b①
// 先例在 integration 面补真实 Postgres 用例（见完工报告"integrative 面补验声明"）。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	// statusActive 与 domain.StatusActive 逐字一致（此处按字面值镜像，避免 cmd 依赖 domain 包）。
	statusActive = "active"

	// cnQuotaExhaustedReasonPrefix 是 F1（额度生命周期）停调 reason 的稳定前缀，
	// 与服务包 cn_quota_lifecycle_service.go:59 的 cnQuotaExhaustedReasonPrefix 逐字一致。
	// 该常量未导出且其文件属并行卡禁区（不修改），故此处按字面值镜像：
	// 上游若改名，本命令须同步。
	cnQuotaExhaustedReasonPrefix = "cn_quota_exhausted"

	// F3 SSOT 键名复用 repository 导出常量，避免与基座字面漂移。
	kF3  = repository.HTTP403RecoveryExtraKey
	kRev = repository.SchedStateRevisionExtraKey
)

// SQL 片段（编译期常量拼接，全部为内部固定标识，无外部输入，无注入面）。
const (
	// f3ExistsExpr：账号是否持有 F3 恢复键（NULL extra 安全）。
	f3ExistsExpr = "COALESCE(extra ? '" + kF3 + "', false)"

	// errOwnedExpr：当前账号 error/调度阻断状态是否由 F3 独占拥有——
	// F3 键存在，且键内 state_revision 等于账号全局 sched_state_revision
	// （R18-F2：任一他链替换状态都会令全局 revision 自增从而失配）。
	errOwnedExpr = "(" + f3ExistsExpr +
		" AND COALESCE((extra->'" + kF3 + "'->>'state_revision')::bigint, -1) = COALESCE((extra->>'" + kRev + "')::bigint, 0))"

	// clearSchemeTempExpr：F1 链拥有 temp_unschedulable 的判据（reason 前缀匹配）。
	clearSchemeTempExpr = "(temp_unschedulable_reason LIKE '" + cnQuotaExhaustedReasonPrefix + "%')"

	// blockedExpr：移除本方案状态后，账号是否仍被**他链**阻断（R20-F1）。
	// 只消费持久化的 owner 状态（他链 error / 他链 temp_unschedulable / 他链 schedulable=false）；
	// 不折叠瞬时门（rate_limit_reset_at / overload_until / expires_at）——这些由调度器查询
	// （schedulableAccountsQuery）独立实施，折叠进 schedulable 列会在他链未复位时造成永久过冻。
	blockedExpr = "((" +
		"status <> '" + statusActive + "' AND NOT " + errOwnedExpr +
		") OR (temp_unschedulable_until > NOW() AND NOT " + clearSchemeTempExpr +
		") OR (schedulable IS NOT TRUE AND NOT " + errOwnedExpr +
		"))"

	// removeF3AndBumpRevExpr：移除 http_403_recovery 整键 + 单语句自增 sched_state_revision
	// （复用基座 http403RemoveRecoveryWithRevisionExpr 的等价表达式；不改代际计数键）。
	removeF3AndBumpRevExpr = "jsonb_set((COALESCE(extra, '{}'::jsonb) - '" + kF3 + "'), '{" + kRev +
		"}', to_jsonb(COALESCE((extra->>'" + kRev + "')::bigint, 0) + 1), true)"

	// candidateQuery 枚举目标账号：持有 F3 键 **或** 持有 F1 前缀 temp_unschedulable（deleted_at IS NULL）。
	// id 游标分页，避免一次性拉全表。
	candidateQuery = `SELECT id,
		` + f3ExistsExpr + ` AS has_f3,
		COALESCE((extra->'` + kF3 + `'->>'generation')::bigint, 0) AS f3_generation,
		COALESCE(temp_unschedulable_reason, '') AS reason,
		temp_unschedulable_until
	FROM accounts
	WHERE id > $1
	  AND deleted_at IS NULL
	  AND (` + f3ExistsExpr + ` OR temp_unschedulable_reason LIKE '` + cnQuotaExhaustedReasonPrefix + `%')
	ORDER BY id ASC
	LIMIT $2`

	// remainingF3Query：回滚闸门复核口径——仍持有 F3 键的账号数（退役后应为 0）。
	remainingF3Query = `SELECT COUNT(*) FROM accounts WHERE deleted_at IS NULL AND ` + f3ExistsExpr

	// enqueueOutboxQuery：状态变更后投递调度器刷新事件（与基座各写入原语一致，best-effort）。
	enqueueOutboxQuery = `INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload) SELECT $1, $2, NULL, NULL`
)

type options struct {
	dryRun    bool
	batchSize int
}

// summary 是退役命令的输出统计，供回滚 runbook 的闸门复核。
type summary struct {
	Candidates            int64 // 枚举到的目标账号数
	Processed             int64 // 实际命中并写入的账号数
	Skipped               int64 // 因并发 CAS 失配（键已变）而零写入的账号数
	F3KeysRemoved         int64 // 移除的 http_403_recovery 键数
	SchemeTempKeysCleared int64 // 清除的 F1 前缀 temp_unschedulable 键数
	Unfrozen              int64 // 重算后恢复可调度的账号数
	KeptFrozen            int64 // 重算后仍维持冻结的账号数
	RemainingF3           int64 // 执行后仍持有 F3 键的账号数（闸门证据，期望 0）
}

func (s summary) KeysRemoved() int64 { return s.F3KeysRemoved + s.SchemeTempKeysCleared }

// candidate 是枚举到的单个目标账号快照（含 CAS 所需的代际/until 值）。
type candidate struct {
	id           int64
	hasF3        bool
	f3Generation int64
	schemeTemp   bool
	tempReason   string
	tempUntil    sql.NullTime
}

func main() {
	execute := flag.Bool("execute", false, "实际执行退役写入（默认 dry-run，只统计不写库）")
	batchSize := flag.Int("batch-size", 200, "枚举分页大小（1-5000）")
	timeout := flag.Duration("timeout", 30*time.Minute, "整体执行超时")
	flag.Parse()

	if *batchSize < 1 || *batchSize > 5000 {
		log.Fatal("--batch-size must be between 1 and 5000")
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：加载配置失败：%v\n", err)
		os.Exit(1)
	}
	db, err := sql.Open("postgres", cfg.Database.DSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：连接数据库失败：%v\n", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "错误：数据库不可达：%v\n", err)
		os.Exit(1)
	}

	sum, err := run(ctx, db, options{dryRun: !*execute, batchSize: *batchSize})
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：退役失败：%v\n", err)
		os.Exit(1)
	}

	mode := "execute"
	if !*execute {
		mode = "dry-run"
	}
	fmt.Printf("mode=%s candidates=%d processed=%d skipped=%d f3_keys_removed=%d scheme_temp_keys_cleared=%d unfrozen=%d kept_frozen=%d remaining_f3_key_holders=%d\n",
		mode, sum.Candidates, sum.Processed, sum.Skipped,
		sum.F3KeysRemoved, sum.SchemeTempKeysCleared, sum.Unfrozen, sum.KeptFrozen, sum.RemainingF3)
	if !*execute {
		fmt.Println("dry-run 模式：仅统计，未写库。加 --execute 实际退役。")
	} else if sum.RemainingF3 != 0 {
		fmt.Fprintf(os.Stderr, "警告：退役后仍有 %d 个账号持有 http_403_recovery 键，回滚发布闸门未满足（需排查并发写入后重跑）。\n", sum.RemainingF3)
		os.Exit(2)
	}
}

// run 执行退役主流程。dryRun=true 时只枚举统计，不写库。
func run(ctx context.Context, db *sql.DB, opts options) (summary, error) {
	var sum summary
	cursor := int64(0)
	for {
		cands, err := listCandidates(ctx, db, cursor, opts.batchSize)
		if err != nil {
			return sum, err
		}
		if len(cands) == 0 {
			break
		}
		for _, c := range cands {
			cursor = c.id
			if !c.hasF3 && !c.schemeTemp {
				// 防御：枚举 WHERE 已保证至少一类命中。
				continue
			}
			sum.Candidates++
			if opts.dryRun {
				if c.hasF3 {
					sum.F3KeysRemoved++
				}
				if c.schemeTemp {
					sum.SchemeTempKeysCleared++
				}
				continue
			}
			schedulable, applied, err := retireOne(ctx, db, c)
			if err != nil {
				return sum, err
			}
			if !applied {
				// 并发 CAS 失配（键在枚举后被改写）→ 零写入、不丢新状态。
				sum.Skipped++
				continue
			}
			sum.Processed++
			if c.hasF3 {
				sum.F3KeysRemoved++
			}
			if c.schemeTemp {
				sum.SchemeTempKeysCleared++
			}
			if schedulable {
				sum.Unfrozen++
			} else {
				sum.KeptFrozen++
			}
			// 调度器刷新事件（best-effort；失败不阻断退役，出库重建快照兜底）。
			if _, err := db.ExecContext(ctx, enqueueOutboxQuery, service.SchedulerOutboxEventAccountChanged, c.id); err != nil {
				log.Printf("[quota-state-retire] enqueue scheduler outbox failed: account=%d err=%v", c.id, err)
			}
		}
	}

	if err := db.QueryRowContext(ctx, remainingF3Query).Scan(&sum.RemainingF3); err != nil {
		return sum, fmt.Errorf("count remaining http_403_recovery holders: %w", err)
	}
	return sum, nil
}

// listCandidates 按 id 游标分页枚举目标账号。
func listCandidates(ctx context.Context, db *sql.DB, cursor int64, limit int) ([]candidate, error) {
	rows, err := db.QueryContext(ctx, candidateQuery, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]candidate, 0, limit)
	for rows.Next() {
		var (
			c      candidate
			reason string
		)
		if err := rows.Scan(&c.id, &c.hasF3, &c.f3Generation, &reason, &c.tempUntil); err != nil {
			return nil, err
		}
		c.tempReason = reason
		c.schemeTemp = isSchemeReason(reason)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// isSchemeReason 判定 temp_unschedulable_reason 是否为本方案（F1 链）拥有。
func isSchemeReason(reason string) bool {
	return strings.HasPrefix(reason, cnQuotaExhaustedReasonPrefix)
}

// retireOne 对单个账号执行原子条件 UPDATE 并返回重算后的 schedulable。
// 返回 applied=false 表示 CAS 失配（键在枚举后被改写）→ 零写入。
func retireOne(ctx context.Context, db *sql.DB, c candidate) (schedulable bool, applied bool, err error) {
	query, args := retireUpdateSQL(c)
	row := db.QueryRowContext(ctx, query, args...)
	if scanErr := row.Scan(&schedulable); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, scanErr
	}
	return schedulable, true, nil
}

// retireUpdateSQL 构造单账号退役的条件 UPDATE（含 CAS + schedulable 重算），见文件头原子性论证。
func retireUpdateSQL(c candidate) (string, []any) {
	args := []any{c.id}
	n := 1
	where := []string{"id = $1", "deleted_at IS NULL"}
	if c.hasF3 {
		n++
		where = append(where, fmt.Sprintf("extra ? '%s' AND (extra->'%s'->>'generation')::bigint = $%d", kF3, kF3, n))
		args = append(args, c.f3Generation)
	}
	if c.schemeTemp {
		n++
		where = append(where, fmt.Sprintf("temp_unschedulable_reason = $%d", n))
		args = append(args, c.tempReason)
		n++
		where = append(where, fmt.Sprintf("temp_unschedulable_until IS NOT DISTINCT FROM $%d", n))
		if c.tempUntil.Valid {
			args = append(args, c.tempUntil.Time)
		} else {
			args = append(args, nil)
		}
	}

	query := "UPDATE accounts SET " +
		"status = CASE WHEN " + errOwnedExpr + " THEN '" + statusActive + "' ELSE status END, " +
		"error_message = CASE WHEN " + errOwnedExpr + " THEN '' ELSE error_message END, " +
		"schedulable = CASE WHEN " + blockedExpr + " THEN FALSE ELSE TRUE END, " +
		"temp_unschedulable_until = CASE WHEN " + clearSchemeTempExpr + " THEN NULL ELSE temp_unschedulable_until END, " +
		"temp_unschedulable_reason = CASE WHEN " + clearSchemeTempExpr + " THEN NULL ELSE temp_unschedulable_reason END, " +
		"extra = " + removeF3AndBumpRevExpr + ", " +
		"updated_at = NOW() " +
		"WHERE " + strings.Join(where, " AND ") + " " +
		"RETURNING schedulable"
	return query, args
}
