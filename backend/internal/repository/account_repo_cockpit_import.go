// account_repo_cockpit_import.go —— cockpit 导入整文件原子提交仓储（方案 v16 §1.6，B1b）。
//
// 职责：在单一数据库事务内，把一批已构造好的账号终态载荷（platform/uid/credentials/extra）
// 原子写入 accounts 表，并返回 created / skipped_existing 计数。
//
// 并发计数确定性（v16 §1.6 钉死）：自有事务路径在事务起点取
// pg_advisory_xact_lock（固定键）串行化 cockpit 导入提交——消除先查后写竞态
// 导致的 created/skipped 误计（ent 批量 upsert 不暴露实际插入行数，先查后写
// 差值在并发下不可判定；这是唯一最小机制）。DO NOTHING 仍是约束兜底，写入语义不变。
//
// 冲突语义（方案 §1.6 钉死，零兜底）：
//   - 唯一约束由部分唯一索引 accounts_platform_uid_active (platform, uid) WHERE uid != '' 提供；
//   - INSERT 使用 ON CONFLICT (platform, uid) WHERE uid != '' DO NOTHING，
//     冲突目标含部分索引谓词（ent v0.14.5 支持 sql.ConflictWhere，见 account_create.go:2315）；
//   - 任何非目标索引约束冲突（name 唯一、FK 等）→ 整个事务回滚 + 显式错误返回，
//     禁止吞成 skipped_existing；目标索引冲突在 DO NOTHING 下不报错，若意外报错按非预期错误回滚；
//   - 任何错误 → 全部未写入（部分成功禁止）。
//
// 本文件不做 entry→payload 转换（§1.3 门控）；载荷由未来平台映射器产出。

package repository

import (
	"context"
	"errors"
	"fmt"

	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/predicate"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// cockpitImportKey 是 (platform, uid) 部分唯一索引的冲突键。
type cockpitImportKey struct {
	platform string
	uid      string
}

// cockpitImportAdvisoryLockKey 是 cockpit 导入提交串行化的事务级咨询锁固定键。
// 取值来源：FNV-1a 64-bit("sub2api/cockpit_import_commit") = 6799643624145489065
// （确定性派生、常量钉死，与迁移锁 migrationsAdvisoryLockID=694208311321144027 不冲突）。
const cockpitImportAdvisoryLockKey int64 = 6799643624145489065

type cockpitImportCommitRepository struct {
	client *dbent.Client
}

// NewCockpitImportCommitRepository 构造 cockpit 导入整文件原子提交仓储。
func NewCockpitImportCommitRepository(client *dbent.Client) service.CockpitImportCommitRepository {
	return &cockpitImportCommitRepository{client: client}
}

// CommitCockpitImport 在单一事务内原子提交整批账号载荷。
// 返回 created（实际新增）/ skipped_existing（存量已存在被 DO NOTHING 跳过的键数）。
// 任何错误 → 整体回滚，全部未写入。
func (r *cockpitImportCommitRepository) CommitCockpitImport(ctx context.Context, payloads []service.CockpitImportAccountPayload) (service.CockpitImportCommitResult, error) {
	result := service.CockpitImportCommitResult{}
	if len(payloads) == 0 {
		return result, nil
	}

	// 失败关闭：uid 为空不在部分唯一索引覆盖范围内，禁止写入（避免静默重复/绕过唯一约束）。
	for i := range payloads {
		if payloads[i].UID == "" {
			return result, fmt.Errorf("cockpit import payload[%d] has empty uid; partial unique index does not cover uid='' (fail close)", i)
		}
		if payloads[i].Platform == "" {
			return result, fmt.Errorf("cockpit import payload[%d] has empty platform (fail close)", i)
		}
	}

	// 去重：同一 (platform, uid) 在同一批次内只写入一次（保留末值；payload 已是终态字段）。
	unique := dedupCockpitPayloads(payloads)

	// 开启单一事务，整文件原子提交。
	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return result, fmt.Errorf("cockpit import begin tx: %w", err)
	}
	var txClient *dbent.Client
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		txClient = tx.Client()

		// v16 §1.6 并发计数确定性：自有事务路径在事务起点取
		// pg_advisory_xact_lock（固定键）串行化 cockpit 导入提交，消除先查后写
		// 竞态导致的 created/skipped 误计（ent 批量 upsert 不暴露实际插入行数，
		// 先查后写差值在并发下不可判定）。xact 变体随事务提交/回滚自动释放，无需解锁。
		// 经事务驱动 ExecContext 执行（tx.config.driver 为 *txDriver，链路
		// 直达底层 *sql.Tx.ExecContext），保证锁与后续语句同连接同事务。
		if _, lerr := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", cockpitImportAdvisoryLockKey); lerr != nil {
			return result, fmt.Errorf("cockpit import acquire advisory lock: %w", lerr)
		}
	} else {
		// 已处于调用方事务中，复用该事务。
		// 注：此 ErrTxStarted 分支当前无生产调用方（handler 直调本仓储），
		// 故不加咨询锁——调用方事务的生命周期与隔离边界不归本仓储管辖，
		// 串行化责任由未来引入该路径的调用方承担（届时须同步评审锁粒度）。
		txClient = r.client
	}

	// ① 存量命中计数：SELECT 已存在的 (platform, uid) 键。
	existing, qerr := queryExistingCockpitKeys(ctx, txClient, unique)
	if qerr != nil {
		return result, fmt.Errorf("cockpit import query existing: %w", qerr)
	}
	skipped := 0
	for i := range unique {
		if existing[cockpitImportKey{unique[i].Platform, unique[i].UID}] {
			skipped++
		}
	}

	// ② 整批 INSERT ... ON CONFLICT (platform, uid) WHERE uid != '' DO NOTHING。
	builders := make([]*dbent.AccountCreate, 0, len(unique))
	for i := range unique {
		p := unique[i]
		// 复用既有写路径加密/MAC 约定（A3-E2），与 accounts 表其余写入一致。
		prepared, perr := prepareCredentialsForStorage(p.Credentials)
		if perr != nil {
			return result, fmt.Errorf("cockpit import prepare credentials for %s/%s: %w", p.Platform, p.UID, perr)
		}
		b := txClient.Account.Create().
			SetName(p.Name).
			SetPlatform(p.Platform).
			SetType(p.AccountType).
			SetUID(p.UID).
			SetCredentials(prepared.storage).
			SetCredentialsMAC(prepared.mac).
			SetExtra(normalizeJSONMap(p.Extra))
		if prepared.apiKeyMAC != nil {
			b.SetNillableCredentialsAPIKeyMAC(prepared.apiKeyMAC)
		}
		builders = append(builders, b)
	}

	// 冲突目标必须含部分索引谓词 WHERE uid != ''（与迁移 262 索引一致）。
	// vendored ent v0.14.5 支持 sql.ConflictWhere，无需降级为无谓词宽泛 DO NOTHING 或裸 SQL。
	err = txClient.Account.CreateBulk(builders...).
		OnConflict(
			entsql.ConflictColumns(dbaccount.FieldPlatform, dbaccount.FieldUID),
			entsql.ConflictWhere(entsql.NEQ(dbaccount.FieldUID, "")),
			entsql.DoNothing(),
		).
		Exec(ctx)
	if err != nil {
		// 双路径约束判定（同 redeem_code_repo.go:379 惯例）：唯一约束或任意约束错误均回滚并显式失败，
		// 禁止吞成 skipped_existing。目标索引冲突在 DO NOTHING 下不报错；若意外报错即按非预期错误回滚。
		if cockpitImportIsConstraintConflict(err) {
			return result, fmt.Errorf("cockpit import constraint conflict (partial unique index or other constraint) for (platform,uid): %w", err)
		}
		return result, fmt.Errorf("cockpit import bulk insert failed: %w", err)
	}

	// ③ 写入后校验计数：在事务内重新统计已存在唯一键，确认实际落库数（DB 实证，非算术假设）。
	afterExisting, aerr := queryExistingCockpitKeys(ctx, txClient, unique)
	if aerr != nil {
		return result, fmt.Errorf("cockpit import query existing after: %w", aerr)
	}
	created := 0
	for i := range unique {
		k := cockpitImportKey{unique[i].Platform, unique[i].UID}
		if afterExisting[k] && !existing[k] {
			created++
		}
	}

	if tx != nil {
		if cerr := tx.Commit(); cerr != nil {
			return result, fmt.Errorf("cockpit import commit tx: %w", cerr)
		}
	}

	result.Created = created
	result.SkippedExisting = skipped
	return result, nil
}

// dedupCockpitPayloads 按 (platform, uid) 去重，同键保留末值。
func dedupCockpitPayloads(payloads []service.CockpitImportAccountPayload) []service.CockpitImportAccountPayload {
	if len(payloads) == 0 {
		return nil
	}
	out := make([]service.CockpitImportAccountPayload, 0, len(payloads))
	idx := make(map[cockpitImportKey]int, len(payloads))
	for i := range payloads {
		k := cockpitImportKey{payloads[i].Platform, payloads[i].UID}
		if j, ok := idx[k]; ok {
			out[j] = payloads[i]
			continue
		}
		idx[k] = len(out)
		out = append(out, payloads[i])
	}
	return out
}

// queryExistingCockpitKeys 查询给定载荷中已存在于 accounts 的 (platform, uid) 键集合。
func queryExistingCockpitKeys(ctx context.Context, client *dbent.Client, payloads []service.CockpitImportAccountPayload) (map[cockpitImportKey]bool, error) {
	result := make(map[cockpitImportKey]bool, len(payloads))
	if len(payloads) == 0 {
		return result, nil
	}
	preds := make([]predicate.Account, 0, len(payloads))
	for i := range payloads {
		preds = append(preds, dbaccount.And(
			dbaccount.Platform(payloads[i].Platform),
			dbaccount.UID(payloads[i].UID),
		))
	}
	rows, err := client.Account.Query().
		Where(dbaccount.Or(preds...)).
		Select(dbaccount.FieldPlatform, dbaccount.FieldUID).
		All(ctx)
	if err != nil {
		return result, err
	}
	for _, a := range rows {
		result[cockpitImportKey{a.Platform, a.UID}] = true
	}
	return result, nil
}

// cockpitImportIsConstraintConflict 双路径判定：同 redeem_code_repo.go:379 惯例。
// ent 把唯一约束包装为 ConstraintError{wrap: pq 23505}，两种路径都判定。
func cockpitImportIsConstraintConflict(err error) bool {
	return isUniqueViolation(err) || dbent.IsConstraintError(err)
}
