// Package repository 实现数据访问层（Repository Pattern）。
//
// 该包提供了与数据库交互的所有操作，包括 CRUD、复杂查询和批量操作。
// 采用 Repository 模式将数据访问逻辑与业务逻辑分离，便于测试和维护。
//
// 主要特性：
//   - 使用 Ent ORM 进行类型安全的数据库操作
//   - 对于复杂查询（如批量更新、聚合统计）使用原生 SQL
//   - 提供统一的错误翻译机制，将数据库错误转换为业务错误
//   - 支持软删除，所有查询自动过滤已删除记录
package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	dbpredicate "github.com/Wei-Shaw/sub2api/ent/predicate"
	dbproxy "github.com/Wei-Shaw/sub2api/ent/proxy"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	"github.com/lib/pq"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
)

// accountRepository 实现 service.AccountRepository 接口。
// 提供 AI API 账户的完整数据访问功能。
//
// 设计说明：
//   - client: Ent 客户端，用于类型安全的 ORM 操作
//   - sql: 原生 SQL 执行器，用于复杂查询和批量操作
//   - schedulerCache: 调度器缓存，用于在账号状态变更时同步快照
type accountRepository struct {
	client *dbent.Client // Ent ORM 客户端
	sql    sqlExecutor   // 原生 SQL 执行接口
	// schedulerCache 用于在账号状态变更时主动同步快照到缓存，
	// 确保粘性会话能及时感知账号不可用状态。
	// Used to proactively sync account snapshot to cache when status changes,
	// ensuring sticky sessions can promptly detect unavailable accounts.
	schedulerCache service.SchedulerCache

	// modelRateLimitWriteMu / modelRateLimitWriteLocks 是 per-account 模型级限流写锁
	// （E38 下沉）。业务 SET（SetModelRateLimit / SetModelRateLimitWithPreciseReset 经
	// commitModelRateLimitSet）与探测族写入口（ApplyModelRateLimitObservation 经
	// WithModelRateLimitAccountLock）共享该锁，保证「读 meta → 裁决 → 提交」整区间串行，
	// 防并发交错读同一 revision 后后写者覆盖较新条目与 meta。惰性 map 模式，与原 service 层
	// 原 service 层 per-account 写锁同款；锁不可重入，仅存在于 commitModelRateLimitSet 与
	// WithModelRateLimitAccountLock 两个外层边界（CommitModelRateLimitObservation 本身不加锁）。
	modelRateLimitWriteMu    sync.Mutex
	modelRateLimitWriteLocks map[int64]*sync.Mutex
}

var schedulerNeutralExtraKeyPrefixes = []string{
	"codex_primary_",
	"codex_secondary_",
	"codex_5h_",
	"codex_7d_",
	"codex_reset_credit_",
	"passive_usage_",
	"upstream_billing_probe",
	"upstream_billing_rate_sync",
	"ollama_cloud_usage",
}

var schedulerNeutralExtraKeys = map[string]struct{}{
	"codex_usage_updated_at":     {},
	"grok_billing_snapshot":      {},
	"session_window_utilization": {},
}

const postgresParameterBatchSize = 50000

const codexFingerprintSeedCanonicalPattern = "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"
const codexFingerprintNilSeed = "00000000-0000-0000-0000-000000000000"

// ---- F3 403 content-policy 恢复链 extra 键族（方案 §3.3 / R7-F3 / R15-F2 / R17-F3） ----
//
// HTTP403RecoveryExtraKey 是 F3 403 恢复链独占的状态 SSOT，经原子 UPDATE 写入/清理：
// 恢复 sweep、401/402/429 转交、CAS 清理、重启续探全部只消费该键。JSON 形态：
//
//	{
//	  "until":          "<RFC3339>",    // 官方恢复时间；无 until 冷却轮 = now + 既有 403 冷却常量
//	  "generation":     <int64>,        // 每次状态写入分配的持久单调代际令牌（不可复用）
//	  "reason":         "<string>",     // F3 状态原因（含 no-until 冷却轮标记）
//	  "owner":          "f3_lifecycle", // 链所有者，CAS 条件之一
//	  "state_revision": <int64>         // 写入时账号级 sched_state_revision 快照，CAS 条件之三
//	}
//
// state_revision 与全局 extra.sched_state_revision 在同一写入语句内同点出生（同一 +1 表达式），
// 故恒等；条件清理/转交以「键内 state_revision = 当前全局 sched_state_revision」为第三条 CAS
// 条件（R18-F2）：任何他链替换账号 error/调度阻断状态都会使全局 revision 自增，从而令该条件
// 失配、CAS 零写入——防止恢复探针清掉他链刚写入的状态。
//
// 字面值契约：internal/service 侧（C1-b②）按 tokenharbor 会话键先例逐字镜像；
// 生产代码不跨包 import（service 依赖 repository 会形成循环，见 domain_constants 先例）。
const (
	// HTTP403RecoveryExtraKey 是 F3 恢复链独占的状态键名。
	HTTP403RecoveryExtraKey = "http_403_recovery"
	// HTTP403GenerationCounterExtraKey 是独立持久代际计数键：**不随** http_403_recovery
	// 删除而删除（R17-F3）。2xx 恢复与 401/402/429 转交只删恢复记录、不删计数器，防止
	// "清理后重新初始化" 造成代际复用。
	HTTP403GenerationCounterExtraKey = "http_403_gen_counter"
	// HTTP403RecoveryOwnerF3 是 F3 链固定所有者标识，CAS 条件之一。
	HTTP403RecoveryOwnerF3 = "f3_lifecycle"
	// HTTP403RecoveryTempUnschedulableReasonPrefix 是 F3 写入 temp_unschedulable 时给
	// reason 加的所有权标记前缀（R15-F2 选择：reason 内带 F3 标记，与
	// ShortenTempUnschedulableIfOwned 的 reason 前缀所有权判据同型）。CAS 清理/转交仅在该
	// 前缀命中时清除 temp_unschedulable_until，非 F3 拥有的 until 不动。
	HTTP403RecoveryTempUnschedulableReasonPrefix = "http_403_recovery_f3: "
	// SchedStateRevisionExtraKey 是账号调度状态 revision 计数键（R19-F2）：全部会替换账号
	// error/调度阻断状态的持久层写入原语在单语句内自增，供跨链并发检测；语义与 per-F3 的
	// generation 不等同（revision = 账号级全局、generation = F3 链代际），不得合并。
	SchedStateRevisionExtraKey = "sched_state_revision"
)

// schedStateRevisionNextValueExpr 返回「读取旧行 extra 的 sched_state_revision 并 +1」的
// jsonb 值表达式（未落地）。供需要在**同一语句多处**写入同一新 revision 的场景复用（F3
// 恢复键内 state_revision 记录 + 全局 sched_state_revision 同点出生）：UPDATE 的 SET 表达式
// 统一以旧行取值，故两处重复该表达式得到同一新值。
func schedStateRevisionNextValueExpr() string {
	return "to_jsonb(COALESCE((extra->>'" + SchedStateRevisionExtraKey + "')::bigint, 0) + 1)"
}

// schedStateRevisionIncrementExpr 返回在单条 UPDATE 内自增 sched_state_revision 的
// jsonb 表达式（引用 extra 列，使 revision 与其余字段写入同一语句原子生效）。
func schedStateRevisionIncrementExpr() string {
	path := "'{" + SchedStateRevisionExtraKey + "}'"
	return "jsonb_set(COALESCE(extra, '{}'::jsonb), " + path + ", " +
		schedStateRevisionNextValueExpr() + ", true)"
}

// http403GenerationCounterIncrementExpr 返回在单条 UPDATE 内自增持久代际计数键的 jsonb
// 表达式；调用方以 RETURNING 读取自增后的新值（禁止读-改-写两步，R17-F3/R19-F1）。
func http403GenerationCounterIncrementExpr() string {
	path := "'{" + HTTP403GenerationCounterExtraKey + "}'"
	return "jsonb_set(COALESCE(extra, '{}'::jsonb), " + path +
		", to_jsonb(COALESCE((extra->>'" + HTTP403GenerationCounterExtraKey + "')::bigint, 0) + 1), true)"
}

// http403RemoveRecoveryWithRevisionExpr 返回「删除 http_403_recovery 键 + 自增
// sched_state_revision」的合并 extra 表达式（单语句原子）。仅移除 F3 拥有的恢复键，
// 不触碰独立代际计数键（R17-F3）。
func http403RemoveRecoveryWithRevisionExpr() string {
	return "jsonb_set((COALESCE(extra, '{}'::jsonb) - '" + HTTP403RecoveryExtraKey + "'), '{" +
		SchedStateRevisionExtraKey + "}', " + schedStateRevisionNextValueExpr() + ", true)"
}

// http403MarkRecoveryWithRevisionExpr 返回 F3 首次写入的合并 extra 表达式（单语句原子）：
// 合并恢复键负载 + 在恢复键内写入 state_revision 记录 + 自增全局 sched_state_revision。
// payloadPlaceholder 为携带 {until,generation,reason,owner} 的 jsonb 参数占位符（如 "$5"）。
//
// 键内 state_revision 与全局 sched_state_revision 复用**同一 +1 表达式**：UPDATE 的 SET
// 表达式统一以旧行取值，故二者在同一语句内同点出生、值恒等——正是 CAS 条件之三
// （键内 state_revision = 当前全局 sched_state_revision）的比较前提（R18-F2/R19-F2）。
func http403MarkRecoveryWithRevisionExpr(payloadPlaceholder string) string {
	recoveryRevisionPath := "'{" + HTTP403RecoveryExtraKey + "," + SchedStateRevisionExtraKey + "}'"
	globalPath := "'{" + SchedStateRevisionExtraKey + "}'"
	next := schedStateRevisionNextValueExpr()
	return "jsonb_set(jsonb_set(COALESCE(extra, '{}'::jsonb) || " + payloadPlaceholder + "::jsonb, " +
		recoveryRevisionPath + ", " + next + ", true), " + globalPath + ", " + next + ", true)"
}

func codexFingerprintSeedValidSQL(extraExpr string) string {
	value := "(" + extraExpr + " ->> 'codex_fingerprint_seed')"
	return "(" + value + " ~ '" + codexFingerprintSeedCanonicalPattern + "' AND " + value + " <> '" + codexFingerprintNilSeed + "')"
}

func ensureCodexFingerprintSeedSQL(extraExpr string) string {
	return "CASE WHEN platform = 'openai' AND type = 'oauth' THEN " +
		"jsonb_set(" + extraExpr + ", '{codex_fingerprint_seed}', " +
		"CASE WHEN " + codexFingerprintSeedValidSQL("extra") +
		" THEN to_jsonb(extra ->> 'codex_fingerprint_seed') ELSE to_jsonb(gen_random_uuid()::text) END, true) " +
		"ELSE " + extraExpr + " END"
}

func stripCodexFingerprintSeedFromExtraUpdate(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	if _, exists := extra["codex_fingerprint_seed"]; !exists {
		return extra
	}
	stripped := make(map[string]any, len(extra)-1)
	for key, value := range extra {
		if key == "codex_fingerprint_seed" {
			continue
		}
		stripped[key] = value
	}
	return stripped
}

// NewAccountRepository 创建账户仓储实例。
// 这是对外暴露的构造函数，返回接口类型以便于依赖注入。
func NewAccountRepository(client *dbent.Client, sqlDB *sql.DB, schedulerCache service.SchedulerCache) service.AccountRepository {
	return newAccountRepositoryWithSQL(client, sqlDB, schedulerCache)
}

// NewAdminAccountRepository exposes the account repository's atomic duplication capability
// as an explicit dependency of the admin service.
func NewAdminAccountRepository(client *dbent.Client, sqlDB *sql.DB, schedulerCache service.SchedulerCache) service.AdminAccountRepository {
	return newAccountRepositoryWithSQL(client, sqlDB, schedulerCache)
}

// newAccountRepositoryWithSQL 是内部构造函数，支持依赖注入 SQL 执行器。
// 这种设计便于单元测试时注入 mock 对象。
func newAccountRepositoryWithSQL(client *dbent.Client, sqlq sqlExecutor, schedulerCache service.SchedulerCache) *accountRepository {
	return &accountRepository{client: client, sql: sqlq, schedulerCache: schedulerCache}
}

func (r *accountRepository) Create(ctx context.Context, account *service.Account) error {
	if err := createAccountRecord(ctx, r.client, account); err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &account.ID, nil, buildSchedulerGroupPayload(account.GroupIDs)); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue account create failed: account=%d err=%v", account.ID, err)
	}
	return nil
}

func createAccountRecord(ctx context.Context, client *dbent.Client, account *service.Account) error {
	if account == nil {
		return service.ErrAccountNilInput
	}

	// 写路径加密（A3-E2）：敏感子键加密 + 维护 credentials_mac / credentials_api_key_mac。
	creds, err := prepareCredentialsForStorage(account.Credentials)
	if err != nil {
		return err
	}

	builder := client.Account.Create().
		SetName(account.Name).
		SetNillableNotes(account.Notes).
		SetPlatform(account.Platform).
		SetType(account.Type).
		SetCredentials(creds.storage).
		SetCredentialsMAC(creds.mac).
		SetNillableCredentialsAPIKeyMAC(creds.apiKeyMAC).
		SetExtra(normalizeJSONMap(account.Extra)).
		SetConcurrency(account.Concurrency).
		SetPriority(account.Priority).
		SetStatus(account.Status).
		SetErrorMessage(account.ErrorMessage).
		SetSchedulable(account.Schedulable).
		SetAutoPauseOnExpired(account.AutoPauseOnExpired)

	if account.RateMultiplier != nil {
		builder.SetRateMultiplier(*account.RateMultiplier)
	}
	if account.LoadFactor != nil {
		builder.SetLoadFactor(*account.LoadFactor)
	}

	if account.ProxyID != nil {
		builder.SetProxyID(*account.ProxyID)
	}
	if account.LastUsedAt != nil {
		builder.SetLastUsedAt(*account.LastUsedAt)
	}
	if account.ExpiresAt != nil {
		builder.SetExpiresAt(*account.ExpiresAt)
	}
	if account.RateLimitedAt != nil {
		builder.SetRateLimitedAt(*account.RateLimitedAt)
	}
	if account.RateLimitResetAt != nil {
		builder.SetRateLimitResetAt(*account.RateLimitResetAt)
	}
	if account.OverloadUntil != nil {
		builder.SetOverloadUntil(*account.OverloadUntil)
	}
	if account.SessionWindowStart != nil {
		builder.SetSessionWindowStart(*account.SessionWindowStart)
	}
	if account.SessionWindowEnd != nil {
		builder.SetSessionWindowEnd(*account.SessionWindowEnd)
	}
	if account.SessionWindowStatus != "" {
		builder.SetSessionWindowStatus(account.SessionWindowStatus)
	}

	builder.SetQuotaDimension(dbaccount.QuotaDimension(account.QuotaDimensionOrDefault()))
	if account.ParentAccountID != nil {
		builder.SetParentAccountID(*account.ParentAccountID)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}

	account.ID = created.ID
	account.CreatedAt = created.CreatedAt
	account.UpdatedAt = created.UpdatedAt
	return nil
}

// CreateWithAccountGroups atomically persists an account, its exact per-group priorities,
// and the scheduler outbox event used to publish the new routing snapshot.
func (r *accountRepository) CreateWithAccountGroups(ctx context.Context, account *service.Account, groups []service.AccountGroup) error {
	if account == nil {
		return service.ErrAccountNilInput
	}
	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return err
	}

	var txClient *dbent.Client
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		txClient = tx.Client()
	} else {
		// Reuse a caller-owned transaction when this repository is already transactional.
		txClient = r.client
	}
	groupIDs := make([]int64, 0, len(groups))
	for i := range groups {
		groupIDs = append(groupIDs, groups[i].GroupID)
	}
	if err := lockLiveGroups(ctx, txClient, groupIDs); err != nil {
		return err
	}

	if err := createAccountRecord(ctx, txClient, account); err != nil {
		return err
	}
	if len(groups) > 0 {
		builders := make([]*dbent.AccountGroupCreate, 0, len(groups))
		for i := range groups {
			groups[i].AccountID = account.ID
			builders = append(builders, txClient.AccountGroup.Create().
				SetAccountID(account.ID).
				SetGroupID(groups[i].GroupID).
				SetPriority(groups[i].Priority),
			)
		}
		if _, err := txClient.AccountGroup.CreateBulk(builders...).Save(ctx); err != nil {
			return err
		}
	}
	account.GroupIDs = groupIDs
	account.AccountGroups = append([]service.AccountGroup(nil), groups...)
	if err := enqueueSchedulerOutbox(ctx, txClient, service.SchedulerOutboxEventAccountChanged, &account.ID, nil, buildSchedulerGroupPayload(groupIDs)); err != nil {
		return err
	}

	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (r *accountRepository) GetByID(ctx context.Context, id int64) (*service.Account, error) {
	m, err := r.client.Account.Query().Where(dbaccount.IDEQ(id)).Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}

	accounts, err := r.accountsToService(ctx, []*dbent.Account{m})
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, service.ErrAccountNotFound
	}
	return &accounts[0], nil
}

func (r *accountRepository) GetByIDs(ctx context.Context, ids []int64) ([]*service.Account, error) {
	if len(ids) == 0 {
		return []*service.Account{}, nil
	}

	// De-duplicate while preserving order of first occurrence.
	uniqueIDs := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	if len(uniqueIDs) == 0 {
		return []*service.Account{}, nil
	}

	entAccounts, err := r.client.Account.
		Query().
		Where(dbaccount.IDIn(uniqueIDs...)).
		WithProxy().
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(entAccounts) == 0 {
		return []*service.Account{}, nil
	}

	accountIDs := make([]int64, 0, len(entAccounts))
	entByID := make(map[int64]*dbent.Account, len(entAccounts))
	for _, acc := range entAccounts {
		entByID[acc.ID] = acc
		accountIDs = append(accountIDs, acc.ID)
	}

	groupsByAccount, groupIDsByAccount, accountGroupsByAccount, err := r.loadAccountGroups(ctx, accountIDs)
	if err != nil {
		return nil, err
	}

	outByID := make(map[int64]*service.Account, len(entAccounts))
	for _, entAcc := range entAccounts {
		out, err := accountEntityToService(entAcc)
		if err != nil {
			return nil, err
		}
		if out == nil {
			continue
		}

		// Prefer the preloaded proxy edge when available.
		if entAcc.Edges.Proxy != nil {
			out.Proxy = proxyEntityToService(entAcc.Edges.Proxy)
		}

		if groups, ok := groupsByAccount[entAcc.ID]; ok {
			out.Groups = groups
		}
		if groupIDs, ok := groupIDsByAccount[entAcc.ID]; ok {
			out.GroupIDs = groupIDs
		}
		if ags, ok := accountGroupsByAccount[entAcc.ID]; ok {
			out.AccountGroups = ags
		}
		outByID[entAcc.ID] = out
	}

	// Preserve input order (first occurrence), and ignore missing IDs.
	out := make([]*service.Account, 0, len(uniqueIDs))
	for _, id := range uniqueIDs {
		if _, ok := entByID[id]; !ok {
			continue
		}
		if acc, ok := outByID[id]; ok && acc != nil {
			out = append(out, acc)
		}
	}

	return out, nil
}

// ExistsByID 检查指定 ID 的账号是否存在。
// 相比 GetByID，此方法性能更优，因为：
//   - 使用 Exist() 方法生成 SELECT EXISTS 查询，只返回布尔值
//   - 不加载完整的账号实体及其关联数据（Groups、Proxy 等）
//   - 适用于删除前的存在性检查等只需判断有无的场景
func (r *accountRepository) ExistsByID(ctx context.Context, id int64) (bool, error) {
	exists, err := r.client.Account.Query().Where(dbaccount.IDEQ(id)).Exist(ctx)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (r *accountRepository) GetByCRSAccountID(ctx context.Context, crsAccountID string) (*service.Account, error) {
	if crsAccountID == "" {
		return nil, nil
	}

	// 使用 sqljson.ValueEQ 生成 JSON 路径过滤，避免手写 SQL 片段导致语法兼容问题。
	// 排除 spark 影子账号(parent_account_id 非空):影子不持凭据,绝不能被 CRS 当作普通账号
	// 更新而覆盖 type/credentials/proxy。即便影子 Extra 被误写入 crs_account_id 也不会命中
	// (外审第7轮 P1)。
	m, err := r.client.Account.Query().
		Where(dbaccount.ParentAccountIDIsNil()).
		Where(func(s *entsql.Selector) {
			s.Where(sqljson.ValueEQ(dbaccount.FieldExtra, crsAccountID, sqljson.Path("crs_account_id")))
		}).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	accounts, err := r.accountsToService(ctx, []*dbent.Account{m})
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, nil
	}
	return &accounts[0], nil
}

func (r *accountRepository) ListCRSAccountIDs(ctx context.Context) (map[string]int64, error) {
	// parent_account_id IS NULL 排除 spark 影子账号:影子不是 CRS 账号,绝不能进 CRS 同步映射
	// (否则会被当普通账号更新而覆盖 type/credentials/proxy)(外审第7轮 P1)。
	rows, err := r.sql.QueryContext(ctx, `
		SELECT id, extra->>'crs_account_id'
		FROM accounts
		WHERE deleted_at IS NULL
			AND parent_account_id IS NULL
			AND extra->>'crs_account_id' IS NOT NULL
			AND extra->>'crs_account_id' != ''
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]int64)
	for rows.Next() {
		var id int64
		var crsID string
		if err := rows.Scan(&id, &crsID); err != nil {
			return nil, err
		}
		result[crsID] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *accountRepository) Update(ctx context.Context, account *service.Account) error {
	return r.updateAccount(ctx, account, nil, nil, account.RateMultiplier)
}

// UpdateWithAccountBillingSettings applies an admin account edit while
// preserving a concurrently probe-synchronized rate unless the request
// explicitly includes a manual rate.
func (r *accountRepository) UpdateWithAccountBillingSettings(
	ctx context.Context,
	account *service.Account,
	probeEnabled *bool,
	rateSyncEnabled *bool,
	rateMultiplier *float64,
) error {
	return r.updateAccount(ctx, account, probeEnabled, rateSyncEnabled, rateMultiplier)
}

func (r *accountRepository) updateAccount(
	ctx context.Context,
	account *service.Account,
	explicitProbeEnabled *bool,
	explicitRateSyncEnabled *bool,
	explicitRateMultiplier *float64,
) error {
	if account == nil {
		return nil
	}

	baseCtx := ctx
	contextTx := dbent.TxFromContext(ctx)
	client := r.client
	var tx *dbent.Tx
	if contextTx != nil {
		client = contextTx.Client()
	} else {
		var err error
		tx, err = r.client.Tx(ctx)
		if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
			return err
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}

	updated, err := r.updateLockedAccount(
		ctx,
		client,
		account,
		explicitProbeEnabled,
		explicitRateSyncEnabled,
		explicitRateMultiplier,
	)
	if err != nil {
		return translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}
	if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &account.ID, nil, buildSchedulerGroupPayload(account.GroupIDs)); err != nil {
		return err
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	account.UpdatedAt = updated.UpdatedAt
	// 普通账号编辑（如 model_mapping / credentials）也需要立即刷新单账号快照，
	// 否则网关在 outbox worker 延迟或异常时仍可能读到旧配置。
	if contextTx == nil {
		r.syncSchedulerAccountSnapshot(baseCtx, account.ID)
	}
	return nil
}

func (r *accountRepository) updateLockedAccount(
	ctx context.Context,
	client *dbent.Client,
	account *service.Account,
	explicitProbeEnabled *bool,
	explicitRateSyncEnabled *bool,
	explicitRateMultiplier *float64,
) (*dbent.Account, error) {
	extra, err := lockAndMergeAccountProbeExtra(ctx, client, account, explicitProbeEnabled, explicitRateSyncEnabled)
	if err != nil {
		return nil, err
	}
	account.Extra = extra

	schedulable := account.Schedulable
	if account.Status == service.StatusError {
		schedulable = false
	}

	// 写路径加密（A3-E2）：敏感子键加密 + 维护 credentials_mac / credentials_api_key_mac。
	creds, err := prepareCredentialsForStorage(account.Credentials)
	if err != nil {
		return nil, err
	}

	builder := client.Account.UpdateOneID(account.ID).
		SetName(account.Name).
		SetNillableNotes(account.Notes).
		SetPlatform(account.Platform).
		SetType(account.Type).
		SetCredentials(creds.storage).
		SetCredentialsMAC(creds.mac).
		SetNillableCredentialsAPIKeyMAC(creds.apiKeyMAC).
		SetExtra(extra).
		SetConcurrency(account.Concurrency).
		SetPriority(account.Priority).
		SetStatus(account.Status).
		SetErrorMessage(account.ErrorMessage).
		SetSchedulable(schedulable).
		SetAutoPauseOnExpired(account.AutoPauseOnExpired)

	if explicitRateMultiplier != nil {
		builder.SetRateMultiplier(*explicitRateMultiplier)
	}
	if account.LoadFactor != nil {
		builder.SetLoadFactor(*account.LoadFactor)
	} else {
		builder.ClearLoadFactor()
	}

	if account.ProxyID != nil {
		builder.SetProxyID(*account.ProxyID)
	} else {
		builder.ClearProxyID()
	}
	if account.LastUsedAt != nil {
		builder.SetLastUsedAt(*account.LastUsedAt)
	} else {
		builder.ClearLastUsedAt()
	}
	if account.ExpiresAt != nil {
		builder.SetExpiresAt(*account.ExpiresAt)
	} else {
		builder.ClearExpiresAt()
	}
	if account.RateLimitedAt != nil {
		builder.SetRateLimitedAt(*account.RateLimitedAt)
	} else {
		builder.ClearRateLimitedAt()
	}
	if account.RateLimitResetAt != nil {
		builder.SetRateLimitResetAt(*account.RateLimitResetAt)
	} else {
		builder.ClearRateLimitResetAt()
	}
	if account.OverloadUntil != nil {
		builder.SetOverloadUntil(*account.OverloadUntil)
	} else {
		builder.ClearOverloadUntil()
	}
	if account.SessionWindowStart != nil {
		builder.SetSessionWindowStart(*account.SessionWindowStart)
	} else {
		builder.ClearSessionWindowStart()
	}
	if account.SessionWindowEnd != nil {
		builder.SetSessionWindowEnd(*account.SessionWindowEnd)
	} else {
		builder.ClearSessionWindowEnd()
	}
	if account.SessionWindowStatus != "" {
		builder.SetSessionWindowStatus(account.SessionWindowStatus)
	} else {
		builder.ClearSessionWindowStatus()
	}
	if account.Notes == nil {
		builder.ClearNotes()
	}

	builder.SetQuotaDimension(dbaccount.QuotaDimension(account.QuotaDimensionOrDefault()))
	builder.SetNillableParentAccountID(account.ParentAccountID)

	return builder.Save(ctx)
}

func lockAndMergeAccountProbeExtra(
	ctx context.Context,
	client *dbent.Client,
	account *service.Account,
	explicitProbeEnabled *bool,
	explicitRateSyncEnabled *bool,
) (map[string]any, error) {
	// 凭证一致性判断改用指纹（A3-E2）：GCM nonce 随机导致密文间不可比。
	// mac 按明文计算；api_key 指纹与 base_url 分开传参，NULL 语义与原先
	// JSONB 缺键比较一致。
	incomingMac, err := credentialsMACOf(account.Credentials)
	if err != nil {
		return nil, err
	}
	incomingApiKeyMac, err := credentialsApiKeyMACOf(account.Credentials)
	if err != nil {
		return nil, err
	}
	var incomingBaseURL any
	if baseURL, ok := account.Credentials["base_url"].(string); ok {
		incomingBaseURL = baseURL
	}
	var proxyID any
	if account.ProxyID != nil {
		proxyID = *account.ProxyID
	}
	rows, err := client.QueryContext(ctx, `
		SELECT
			platform = $2
			AND type = $3
			AND credentials_mac IS NOT DISTINCT FROM $4
			AND proxy_id IS NOT DISTINCT FROM $5,
			COALESCE(
				platform IN (`+ollamaCloudUsagePlatformsSQL+`)
				AND $2 IN (`+ollamaCloudUsagePlatformsSQL+`)
				AND type = 'apikey'
				AND $3 = 'apikey'
				AND credentials_api_key_mac IS NOT DISTINCT FROM $6
				AND `+ollamaCloudBaseURLMatchesSQL("credentials ->> 'base_url'")+`
				AND `+ollamaCloudBaseURLMatchesSQL("$7::text")+`,
				false
			),
			proxy_id IS NOT DISTINCT FROM $5,
			extra -> 'upstream_billing_probe_enabled',
			extra -> 'upstream_billing_rate_sync_enabled',
			extra -> 'upstream_billing_probe',
			extra -> 'ollama_cloud_usage_session',
			extra -> 'ollama_cloud_usage_auto_refresh',
			extra -> 'ollama_cloud_usage_snapshot'
		FROM accounts
		WHERE id = $1 AND deleted_at IS NULL
		FOR NO KEY UPDATE
	`, account.ID, account.Platform, account.Type, incomingMac, proxyID, incomingApiKeyMac, incomingBaseURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrAccountNotFound
	}

	var (
		identityUnchanged            bool
		ollamaGroupIdentityUnchanged bool
		ollamaProxyIdentityUnchanged bool
		currentEnabled               []byte
		currentRateSyncEnabled       []byte
		currentSnapshot              []byte
		currentOllamaSession         []byte
		currentOllamaAutoRefresh     []byte
		currentOllamaSnapshot        []byte
	)
	if err := rows.Scan(
		&identityUnchanged,
		&ollamaGroupIdentityUnchanged,
		&ollamaProxyIdentityUnchanged,
		&currentEnabled,
		&currentRateSyncEnabled,
		&currentSnapshot,
		&currentOllamaSession,
		&currentOllamaAutoRefresh,
		&currentOllamaSnapshot,
	); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	extra := copyJSONMap(normalizeJSONMap(account.Extra))
	for _, key := range []string{
		service.UpstreamBillingProbeEnabledExtraKey,
		service.UpstreamBillingRateSyncEnabledExtraKey,
		service.UpstreamBillingProbeExtraKey,
		service.OllamaCloudUsageSessionExtraKey,
		service.OllamaCloudUsageAutoRefreshExtraKey,
		service.OllamaCloudUsageSnapshotExtraKey,
	} {
		delete(extra, key)
	}
	probeAccount := service.IsUpstreamBillingProbeIdentity(account.Platform, account.Type)
	// 网页逆向接入（web 接入模式账号）不是 API key 探活对象，失败关闭排除
	// （方案 §5.6，归并后 platform 已是官方值）。
	if probeAccount && account.IsWebAccessMode() {
		probeAccount = false
	}
	probeEnabled := false
	probeEnabledPresent := false
	if probeAccount {
		if enabled, ok, err := decodeAccountExtraJSON(currentEnabled); err != nil {
			return nil, err
		} else if value, isBool := enabled.(bool); ok && isBool {
			probeEnabled = value
			probeEnabledPresent = true
		}
		if explicitProbeEnabled != nil {
			probeEnabled = *explicitProbeEnabled
			probeEnabledPresent = true
		}
	}
	rateSyncEnabled := false
	rateSyncEnabledPresent := false
	if probeAccount {
		if enabled, ok, err := decodeAccountExtraJSON(currentRateSyncEnabled); err != nil {
			return nil, err
		} else if value, isBool := enabled.(bool); ok && isBool {
			rateSyncEnabled = value
			rateSyncEnabledPresent = true
		}
		if explicitRateSyncEnabled != nil {
			rateSyncEnabled = *explicitRateSyncEnabled
			rateSyncEnabledPresent = true
		}
		if explicitProbeEnabled != nil && !*explicitProbeEnabled {
			rateSyncEnabled = false
			rateSyncEnabledPresent = true
		}
		// 同步依赖探测，方向是单向的：探测关闭（或探测键缺失）一律把同步归零。
		// 不做反向推导——由 rate_sync=true 推出 probe=true 会让一条"同步开、探测键
		// 缺失"的僵尸记录在任意一次无关编辑时静默打开周期性外呼。需要同时打开两个
		// 开关的调用方（管理端编辑）自己显式传 explicitProbeEnabled=true。
		if !probeEnabled {
			rateSyncEnabled = false
		}
		if probeEnabledPresent {
			extra[service.UpstreamBillingProbeEnabledExtraKey] = probeEnabled
		}
		if rateSyncEnabledPresent {
			extra[service.UpstreamBillingRateSyncEnabledExtraKey] = rateSyncEnabled
		}
	}
	probeExplicitlyDisabled := probeEnabledPresent && !probeEnabled
	if identityUnchanged && !probeExplicitlyDisabled {
		if snapshot, ok, err := decodeAccountExtraJSON(currentSnapshot); err != nil {
			return nil, err
		} else if ok {
			extra[service.UpstreamBillingProbeExtraKey] = snapshot
		}
	}

	if service.IsOllamaCloudUsageAccount(account) && ollamaGroupIdentityUnchanged {
		for key, raw := range map[string][]byte{
			service.OllamaCloudUsageSessionExtraKey:     currentOllamaSession,
			service.OllamaCloudUsageAutoRefreshExtraKey: currentOllamaAutoRefresh,
		} {
			if value, ok, err := decodeAccountExtraJSON(raw); err != nil {
				return nil, err
			} else if ok {
				extra[key] = value
			}
		}
		if ollamaProxyIdentityUnchanged {
			if snapshot, ok, err := decodeAccountExtraJSON(currentOllamaSnapshot); err != nil {
				return nil, err
			} else if ok {
				extra[service.OllamaCloudUsageSnapshotExtraKey] = snapshot
			}
		}
	}
	return extra, nil
}

func decodeAccountExtraJSON(raw []byte) (any, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (r *accountRepository) UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error {
	// 写路径加密（A3-E2）：敏感子键加密 + 维护指纹列。CASE 里"凭证是否变化"
	// 的判断改用指纹等值（GCM nonce 随机，密文之间不可直接比较；api_key 的
	// 变化同样经 credentials_api_key_mac 判断）。指纹始终按明文计算。
	creds, err := prepareCredentialsForStorage(credentials)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(creds.storage)
	if err != nil {
		return err
	}
	baseCtx := ctx
	contextTx := dbent.TxFromContext(ctx)
	client := r.client
	var tx *dbent.Tx
	if contextTx != nil {
		client = contextTx.Client()
	} else if r.client != nil {
		var txErr error
		tx, txErr = r.client.Tx(ctx)
		if txErr != nil && !errors.Is(txErr, dbent.ErrTxStarted) {
			return txErr
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}
	result, err := client.ExecContext(ctx, `
		UPDATE accounts
		SET
			credentials = $1::jsonb,
			credentials_mac = $3,
			credentials_api_key_mac = $4,
			extra = CASE
				-- 凭证整体未变化 ⇒ Ollama 组身份必然未变化；顶层 DISTINCT 守卫防止
				-- 非 Ollama 账号的无变化持久化误清探测快照或重写 NULL extra。
				WHEN platform IN (`+ollamaCloudUsagePlatformsSQL+`)
					AND type = 'apikey'
					AND credentials_mac IS DISTINCT FROM $3
					AND (
						credentials_api_key_mac IS DISTINCT FROM $4
						OR NOT (
							`+ollamaCloudBaseURLMatchesSQL("credentials ->> 'base_url'")+`
							AND `+ollamaCloudBaseURLMatchesSQL("$1::jsonb ->> 'base_url'")+`
						)
					)
				THEN COALESCE(extra, '{}'::jsonb)
					- 'upstream_billing_probe'
					- 'ollama_cloud_usage_session'
					- 'ollama_cloud_usage_auto_refresh'
					- 'ollama_cloud_usage_snapshot'
				-- 上游倍率探测已放宽到全部 API-key 平台：凭证变化即视为探测
				-- 身份变化，丢弃 stale 快照。
				WHEN type = 'apikey'
					AND credentials_mac IS DISTINCT FROM $3
				THEN COALESCE(extra, '{}'::jsonb) - 'upstream_billing_probe'
				ELSE extra
			END,
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
	`, string(payload), id, creds.mac, creds.apiKeyMAC)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAccountNotFound
	}
	if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		return err
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if contextTx == nil {
		r.syncSchedulerAccountSnapshot(baseCtx, id)
	}
	return nil
}

func (r *accountRepository) Delete(ctx context.Context, id int64) error {
	groupIDs, err := r.loadAccountGroupIDs(ctx, id)
	if err != nil {
		return err
	}
	// 使用事务保证账号与关联分组的删除原子性
	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return err
	}

	var txClient *dbent.Client
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		txClient = tx.Client()
	} else {
		// 已处于外部事务中（ErrTxStarted），复用当前 client
		txClient = r.client
	}

	if _, err := txClient.AccountGroup.Delete().Where(dbaccountgroup.AccountIDEQ(id)).Exec(ctx); err != nil {
		return err
	}
	if _, err := txClient.ExecContext(ctx, "DELETE FROM scheduled_test_plans WHERE account_id = $1", id); err != nil {
		return err
	}
	if _, err := txClient.Account.Delete().Where(dbaccount.IDEQ(id)).Exec(ctx); err != nil {
		return err
	}

	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	r.deleteSchedulerAccountSnapshot(ctx, id)
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, buildSchedulerGroupPayload(groupIDs)); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue account delete failed: account=%d err=%v", id, err)
	}
	return nil
}

func (r *accountRepository) List(ctx context.Context, params pagination.PaginationParams) ([]service.Account, *pagination.PaginationResult, error) {
	return r.ListWithFilters(ctx, params, "", "", "", "", 0, "")
}

func (r *accountRepository) accountListFilteredQuery(platform, accountType, status, search string, groupID int64, privacyMode string) *dbent.AccountQuery {
	q := r.client.Account.Query()

	if platform != "" {
		q = q.Where(dbaccount.PlatformEQ(platform))
	}
	if accountType != "" {
		q = q.Where(dbaccount.TypeEQ(accountType))
	}
	if status != "" {
		switch status {
		case service.StatusActive:
			q = q.Where(
				dbaccount.StatusEQ(status),
				dbaccount.SchedulableEQ(true),
				dbaccount.Or(
					dbaccount.RateLimitResetAtIsNil(),
					dbaccount.RateLimitResetAtLTE(time.Now()),
				),
				dbpredicate.Account(func(s *entsql.Selector) {
					col := s.C("temp_unschedulable_until")
					s.Where(entsql.Or(
						entsql.IsNull(col),
						entsql.LTE(col, entsql.Expr("NOW()")),
					))
				}),
			)
		// available / temp_limited 与分组列表"可用/限流账号数"的统计口径
		// （group_repo.go 的 groupAccountAvailableSQL / groupAccountTemporarilyLimitedSQL）保持一致，
		// 用于分组页数字下钻到账号列表。
		case "available":
			now := time.Now()
			q = q.Where(
				dbaccount.StatusEQ(service.StatusActive),
				dbaccount.SchedulableEQ(true),
				notExpiredPredicate(now),
				dbaccount.Or(
					dbaccount.RateLimitResetAtIsNil(),
					dbaccount.RateLimitResetAtLTE(now),
				),
				dbaccount.Or(
					dbaccount.OverloadUntilIsNil(),
					dbaccount.OverloadUntilLTE(now),
				),
				tempUnschedulablePredicate(),
			)
		case "temp_limited":
			now := time.Now()
			q = q.Where(
				dbaccount.StatusEQ(service.StatusActive),
				dbaccount.SchedulableEQ(true),
				notExpiredPredicate(now),
				dbaccount.Or(
					dbaccount.RateLimitResetAtGT(now),
					dbaccount.OverloadUntilGT(now),
					dbaccount.TempUnschedulableUntilGT(now),
				),
			)
		case "rate_limited":
			q = q.Where(
				dbaccount.StatusEQ(service.StatusActive),
				dbaccount.RateLimitResetAtGT(time.Now()),
				dbpredicate.Account(func(s *entsql.Selector) {
					col := s.C("temp_unschedulable_until")
					s.Where(entsql.Or(
						entsql.IsNull(col),
						entsql.LTE(col, entsql.Expr("NOW()")),
					))
				}),
			)
		case "temp_unschedulable":
			q = q.Where(
				dbaccount.StatusEQ(service.StatusActive),
				dbpredicate.Account(func(s *entsql.Selector) {
					col := s.C("temp_unschedulable_until")
					s.Where(entsql.And(
						entsql.Not(entsql.IsNull(col)),
						entsql.GT(col, entsql.Expr("NOW()")),
					))
				}),
			)
		case "unschedulable":
			q = q.Where(
				dbaccount.StatusEQ(service.StatusActive),
				dbaccount.SchedulableEQ(false),
				dbaccount.Or(
					dbaccount.RateLimitResetAtIsNil(),
					dbaccount.RateLimitResetAtLTE(time.Now()),
				),
				dbpredicate.Account(func(s *entsql.Selector) {
					col := s.C("temp_unschedulable_until")
					s.Where(entsql.Or(
						entsql.IsNull(col),
						entsql.LTE(col, entsql.Expr("NOW()")),
					))
				}),
			)
		default:
			q = q.Where(dbaccount.StatusEQ(status))
		}
	}
	if search != "" {
		q = q.Where(dbaccount.NameContainsFold(search))
	}
	if groupID == service.AccountListGroupUngrouped {
		q = q.Where(dbaccount.Not(dbaccount.HasAccountGroups()))
	} else if groupID > 0 {
		q = q.Where(dbaccount.HasAccountGroupsWith(dbaccountgroup.GroupIDEQ(groupID)))
	}
	if privacyMode != "" {
		q = q.Where(dbpredicate.Account(func(s *entsql.Selector) {
			path := sqljson.Path("privacy_mode")
			switch privacyMode {
			case service.AccountPrivacyModeUnsetFilter:
				s.Where(entsql.Or(
					entsql.Not(sqljson.HasKey(dbaccount.FieldExtra, path)),
					sqljson.ValueEQ(dbaccount.FieldExtra, "", path),
				))
			default:
				s.Where(sqljson.ValueEQ(dbaccount.FieldExtra, privacyMode, path))
			}
		}))
	}

	return q
}

func (r *accountRepository) ListWithFilters(ctx context.Context, params pagination.PaginationParams, platform, accountType, status, search string, groupID int64, privacyMode string) ([]service.Account, *pagination.PaginationResult, error) {
	q := r.accountListFilteredQuery(platform, accountType, status, search, groupID, privacyMode)
	// Clone before Count so interceptor-appended predicates (SoftDeleteMixin's
	// deleted_at IS NULL) don't accumulate on the shared builder and pollute the
	// subsequent list query. Same pattern used in group_repo/promo_code_repo/user_repo
	// (P1-03 audit fix, commit 2588fa6a).
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	accountsQuery := q.
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range accountListOrder(params) {
		accountsQuery = accountsQuery.Order(order)
	}

	accounts, err := accountsQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	outAccounts, err := r.accountsToService(ctx, accounts)
	if err != nil {
		return nil, nil, err
	}
	return outAccounts, paginationResultFromTotal(int64(total), params), nil
}

func (r *accountRepository) ListAllWithFilters(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode string) ([]service.Account, error) {
	accounts, err := r.accountListFilteredQuery(platform, accountType, status, search, groupID, privacyMode).All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListOpsAccountsForStats(ctx context.Context, platformFilter string, groupIDFilter *int64) ([]service.Account, error) {
	if r == nil || r.client == nil {
		return []service.Account{}, nil
	}

	q := r.client.Account.Query()
	if platformFilter = strings.TrimSpace(platformFilter); platformFilter != "" {
		q = q.Where(dbaccount.PlatformEQ(platformFilter))
	}
	if groupIDFilter != nil && *groupIDFilter > 0 {
		q = q.Where(dbaccount.HasAccountGroupsWith(dbaccountgroup.GroupIDEQ(*groupIDFilter)))
	}

	accounts, err := q.
		Select(
			dbaccount.FieldID,
			dbaccount.FieldName,
			dbaccount.FieldPlatform,
			dbaccount.FieldConcurrency,
			dbaccount.FieldLoadFactor,
			dbaccount.FieldStatus,
			dbaccount.FieldErrorMessage,
			dbaccount.FieldSchedulable,
			dbaccount.FieldRateLimitResetAt,
			dbaccount.FieldOverloadUntil,
			dbaccount.FieldTempUnschedulableUntil,
		).
		Order(dbent.Asc(dbaccount.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func accountListOrder(params pagination.PaginationParams) []func(*entsql.Selector) {
	sortBy := strings.ToLower(strings.TrimSpace(params.SortBy))
	sortOrder := params.NormalizedSortOrder(pagination.SortOrderAsc)
	if sortBy == "upstream_billing_rate" {
		direction := "ASC"
		tieOrder := entsql.Asc
		if sortOrder == pagination.SortOrderDesc {
			direction = "DESC"
			tieOrder = entsql.Desc
		}
		return []func(*entsql.Selector){func(s *entsql.Selector) {
			extra := s.C(dbaccount.FieldExtra)
			expression := upstreamBillingRateSortExpression(extra)
			s.OrderExpr(entsql.Expr(expression + " " + direction + " NULLS LAST"))
			s.OrderBy(tieOrder(s.C(dbaccount.FieldID)))
		}}
	}

	field := dbaccount.FieldName
	defaultOrder := true
	switch sortBy {
	case "", "name":
		field = dbaccount.FieldName
	case "id":
		field = dbaccount.FieldID
		defaultOrder = false
	case "status":
		field = dbaccount.FieldStatus
		defaultOrder = false
	case "schedulable":
		field = dbaccount.FieldSchedulable
		defaultOrder = false
	case "priority":
		field = dbaccount.FieldPriority
		defaultOrder = false
	case "rate_multiplier":
		field = dbaccount.FieldRateMultiplier
		defaultOrder = false
	case "last_used_at":
		field = dbaccount.FieldLastUsedAt
		defaultOrder = false
	case "expires_at":
		field = dbaccount.FieldExpiresAt
		defaultOrder = false
	case "created_at":
		field = dbaccount.FieldCreatedAt
		defaultOrder = false
	}

	if sortOrder == pagination.SortOrderDesc {
		return []func(*entsql.Selector){dbent.Desc(field), dbent.Desc(dbaccount.FieldID)}
	}
	if defaultOrder {
		return []func(*entsql.Selector){dbent.Asc(dbaccount.FieldName), dbent.Asc(dbaccount.FieldID)}
	}
	return []func(*entsql.Selector){dbent.Asc(field), dbent.Asc(dbaccount.FieldID)}
}

func upstreamBillingRateSortExpression(extra string) string {
	status := extra + " #>> '{upstream_billing_probe,status}'"
	effectiveJSON := extra + " #> '{upstream_billing_probe,data,effective_rate_multiplier}'"
	effective := extra + " #>> '{upstream_billing_probe,data,effective_rate_multiplier}'"
	resolvedJSON := extra + " #> '{upstream_billing_probe,data,resolved_rate_multiplier}'"
	resolved := extra + " #>> '{upstream_billing_probe,data,resolved_rate_multiplier}'"
	peakEnabledJSON := extra + " #> '{upstream_billing_probe,data,peak_rate_enabled}'"
	peakEnabled := extra + " #>> '{upstream_billing_probe,data,peak_rate_enabled}'"
	peakStart := extra + " #>> '{upstream_billing_probe,data,peak_start}'"
	peakEnd := extra + " #>> '{upstream_billing_probe,data,peak_end}'"
	peakMultiplierJSON := extra + " #> '{upstream_billing_probe,data,peak_rate_multiplier}'"
	peakMultiplier := extra + " #>> '{upstream_billing_probe,data,peak_rate_multiplier}'"
	peakMultiplierValue := "(CASE WHEN jsonb_typeof(" + peakMultiplierJSON + ") = 'number' THEN (" + peakMultiplier + ")::numeric END)"
	billingScope := extra + " #>> '{upstream_billing_probe,data,billing_scope}'"
	timezone := extra + " #>> '{upstream_billing_probe,data,timezone}'"
	validClock := "'^([01][0-9]|2[0-3]):[0-5][0-9]$'"
	startMinute := "(CASE WHEN " + peakStart + " ~ " + validClock + " THEN split_part(" + peakStart + ", ':', 1)::numeric * 60 + split_part(" + peakStart + ", ':', 2)::numeric END)"
	endMinute := "(CASE WHEN " + peakEnd + " ~ " + validClock + " THEN split_part(" + peakEnd + ", ':', 1)::numeric * 60 + split_part(" + peakEnd + ", ':', 2)::numeric END)"
	localMinute := "(EXTRACT(HOUR FROM (CURRENT_TIMESTAMP AT TIME ZONE (" + timezone + "))) * 60 + EXTRACT(MINUTE FROM (CURRENT_TIMESTAMP AT TIME ZONE (" + timezone + "))))"
	validPeakWindow := peakStart + " ~ " + validClock + " AND " +
		peakEnd + " ~ " + validClock + " AND " +
		startMinute + " < " + endMinute
	validPeakConfig := validPeakWindow + " AND " + peakMultiplierValue + " >= 0 AND " +
		"EXISTS (SELECT 1 FROM pg_timezone_names WHERE name = " + timezone + ")"
	dynamicRate := "CASE WHEN " + peakEnabled + " = 'false' THEN (" + resolved + ")::numeric WHEN " + peakEnabled + " = 'true' AND " + validPeakConfig +
		" THEN (" + resolved + ")::numeric * CASE WHEN " + localMinute + " >= " + startMinute + " AND " + localMinute + " < " + endMinute +
		" THEN " + peakMultiplierValue + " ELSE 1 END ELSE NULL END"
	legacySnapshot := "jsonb_typeof(" + resolvedJSON + ") IS NULL AND jsonb_typeof(" + peakEnabledJSON + ") IS NULL"

	return "CASE WHEN " + status + " IN ('ok', 'failed') AND (jsonb_typeof(" + resolvedJSON + ") = 'number' OR jsonb_typeof(" + effectiveJSON + ") = 'number') THEN CASE WHEN jsonb_typeof(" +
		resolvedJSON + ") = 'number' AND jsonb_typeof(" + peakEnabledJSON + ") = 'boolean' THEN CASE WHEN " + billingScope + " = 'token' THEN " + dynamicRate + " ELSE NULL END WHEN " + legacySnapshot +
		" AND jsonb_typeof(" + effectiveJSON + ") = 'number' THEN (" + effective + ")::numeric END END"
}

func (r *accountRepository) ListByGroup(ctx context.Context, groupID int64) ([]service.Account, error) {
	accounts, err := r.queryAccountsByGroup(ctx, groupID, accountGroupQueryOptions{
		status: service.StatusActive,
	})
	if err != nil {
		return nil, err
	}
	return accounts, nil
}

func (r *accountRepository) ListActive(ctx context.Context) ([]service.Account, error) {
	accounts, err := r.client.Account.Query().
		Where(dbaccount.StatusEQ(service.StatusActive)).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListOAuthRefreshCandidatePage(ctx context.Context, options service.OAuthRefreshPageOptions) (*service.OAuthRefreshCandidatePage, error) {
	if r.sql == nil {
		return nil, errors.New("account repository SQL executor not configured")
	}
	if len(options.Platforms) == 0 {
		return nil, errors.New("oauth refresh candidate platforms cannot be empty")
	}
	if options.Limit <= 0 || options.Limit > 1000 {
		return nil, errors.New("oauth refresh candidate page limit must be between 1 and 1000")
	}

	// (cond) IS NOT TRUE 把 NULL 和 FALSE 都视为"可被刷新"。直接写
	// NOT (a AND b) 在 PG 三值逻辑下会把 a 或 b 为 NULL 的行（即绝大多数
	// 健康账号：temp_unschedulable_until=NULL）也排除，导致后台 token
	// 刷新工作器漏掉所有正常账号 → access_token 到期后请求开始 401。
	query := `
		SELECT id
		FROM accounts
		WHERE deleted_at IS NULL
			AND schedulable = TRUE
			AND platform = ANY($1)
			AND id > $2`
	if options.ActiveOnly {
		query += `
			AND status = 'active'`
	}
	if options.IncludeSetupToken {
		query += `
			AND type IN ('oauth', 'setup-token')`
	} else {
		query += `
			AND type = 'oauth'`
	}
	if options.RequireRefreshToken {
		query += `
			AND credentials ? 'refresh_token'
			AND btrim(credentials->>'refresh_token') <> ''`
	}
	if options.ExcludeRetryCooldown {
		query += `
			AND (
				temp_unschedulable_until > NOW()
				AND temp_unschedulable_reason LIKE 'token refresh retry exhausted:%'
			) IS NOT TRUE`
	}
	query += `
		ORDER BY id ASC
		LIMIT $3`

	rows, err := r.sql.QueryContext(ctx, query, pq.Array(options.Platforms), options.AfterID, options.Limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return &service.OAuthRefreshCandidatePage{Accounts: []service.Account{}}, nil
	}

	accounts, err := r.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	accountsByID := make(map[int64]*service.Account, len(accounts))
	for _, account := range accounts {
		if account != nil {
			accountsByID[account.ID] = account
		}
	}
	out := make([]service.Account, 0, len(accounts))
	for _, id := range ids {
		if account := accountsByID[id]; account != nil {
			out = append(out, *account)
		}
	}
	page := &service.OAuthRefreshCandidatePage{
		Accounts: out,
		HasMore:  len(ids) == options.Limit,
	}
	if len(ids) > 0 {
		page.NextAfterID = ids[len(ids)-1]
	}
	return page, nil
}

func (r *accountRepository) ListByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformEQ(platform),
			dbaccount.StatusEQ(service.StatusActive),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) UpdateLastUsed(ctx context.Context, id int64) error {
	now := time.Now()
	_, err := r.client.Account.Update().
		Where(dbaccount.IDEQ(id)).
		SetLastUsedAt(now).
		Save(ctx)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"last_used": map[string]int64{
			strconv.FormatInt(id, 10): now.Unix(),
		},
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountLastUsed, &id, nil, payload); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue last used failed: account=%d err=%v", id, err)
	}
	return nil
}

func (r *accountRepository) BatchUpdateLastUsed(ctx context.Context, updates map[int64]time.Time) error {
	if len(updates) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(updates))
	args := make([]any, 0, len(updates)*2+1)
	caseSQL := "UPDATE accounts SET last_used_at = CASE id"

	idx := 1
	for id, ts := range updates {
		caseSQL += " WHEN $" + itoa(idx) + " THEN $" + itoa(idx+1) + "::timestamptz"
		args = append(args, id, ts)
		ids = append(ids, id)
		idx += 2
	}

	caseSQL += " END, updated_at = NOW() WHERE id = ANY($" + itoa(idx) + ") AND deleted_at IS NULL"
	args = append(args, pq.Array(ids))

	_, err := r.sql.ExecContext(ctx, caseSQL, args...)
	if err != nil {
		return err
	}
	lastUsedPayload := make(map[string]int64, len(updates))
	for id, ts := range updates {
		lastUsedPayload[strconv.FormatInt(id, 10)] = ts.Unix()
	}
	payload := map[string]any{"last_used": lastUsedPayload}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountLastUsed, nil, nil, payload); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue batch last used failed: err=%v", err)
	}
	return nil
}

func (r *accountRepository) SetError(ctx context.Context, id int64, errorMsg string) error {
	// 单条原生 SQL（原为 ent Update）：除保持既有字段效果（status=error、
	// error_message、schedulable=false、updated_at）外，同语句内自增
	// sched_state_revision（R19-F2，全部会替换账号 error/调度阻断状态的持久层写入
	// 原语统一维护）。WHERE 仅按 id（与既有 ent Update().Where(IDEQ(id)) 语义一致，
	// 不额外引入 deleted_at 过滤），0 行时返回 ErrAccountNotFound（对齐 ent 的
	// NotFoundError 语义）。
	result, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET status = $1,
			error_message = $2,
			schedulable = FALSE,
			extra = `+schedStateRevisionIncrementExpr()+`,
			updated_at = NOW()
		WHERE id = $3
	`, service.StatusError, errorMsg, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAccountNotFound
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue set error failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

func (r *accountRepository) SetGrokCredentialErrorIfMatch(
	ctx context.Context,
	id int64,
	snapshot service.GrokCredentialMutationSnapshot,
	errorMsg string,
) (bool, error) {
	// 凭证一致性守卫改用指纹比对（A3-E2）。
	expectedMac, err := credentialsMACOfJSONString(snapshot.CredentialsJSON)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
		UPDATE accounts AS a
		SET status = $1,
			error_message = $2,
			schedulable = false,
			updated_at = NOW()
		WHERE a.id = $3
			AND a.deleted_at IS NULL
			AND a.status = $4
			AND a.platform = $5
			AND a.type = $6
			AND a.schedulable IS TRUE
			AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW())
			AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW())
			AND (a.overload_until IS NULL OR a.overload_until <= NOW())
			AND (a.auto_pause_on_expired IS NOT TRUE OR a.expires_at IS NULL OR a.expires_at > NOW())
			AND a.credentials_mac = $7
			AND a.proxy_id IS NOT DISTINCT FROM $8
			AND ($2 <> $9 OR (
				a.proxy_id IS NOT NULL AND NOT EXISTS (
					SELECT 1 FROM proxies p WHERE p.id = a.proxy_id AND p.deleted_at IS NULL
				)
			))
		RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $10, updated.id, NULL, NULL FROM updated
	`, service.StatusError, errorMsg, id, service.StatusActive, service.PlatformGrok, service.AccountTypeOAuth,
		expectedMac, snapshot.ProxyID, string(service.GrokCredentialReasonProxyInvalid),
		service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}

// SetGrokOAuthErrorIfCredentialsUnchanged atomically quarantines a structurally
// invalid Grok OAuth account only if it is still active and its complete JSONB
// credential document matches the state observed by reconciliation. Exact
// JSONB equality includes _token_version when present and prevents a concurrent
// reauthorization from being overwritten by a stale check-then-mutate path.
func (r *accountRepository) SetGrokOAuthErrorIfCredentialsUnchanged(
	ctx context.Context,
	id int64,
	expectedCredentials map[string]any,
	errorMsg string,
) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	// 凭证一致性守卫改用指纹比对（A3-E2）；refresh_token 为空的守卫不受加密
	// 影响：空字符串值不加密，密文存在 ⟺ 明文存在。
	expectedMac, err := credentialsMACOf(expectedCredentials)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
		UPDATE accounts AS a
		SET status = $1,
			error_message = $2,
			schedulable = FALSE,
			updated_at = NOW()
		WHERE a.id = $3
			AND a.deleted_at IS NULL
			AND a.platform = $4
			AND a.type = $5
			AND a.status = $6
			AND a.credentials_mac = $7
			AND NULLIF(BTRIM(a.credentials->>'refresh_token'), '') IS NULL
		RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $8, updated.id, NULL, NULL FROM updated
	`,
		service.StatusError,
		errorMsg,
		id,
		service.PlatformGrok,
		service.AccountTypeOAuth,
		service.StatusActive,
		expectedMac,
		service.SchedulerOutboxEventAccountChanged,
	)
	if err != nil {
		return false, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rowsAffected == 0 {
		return false, nil
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}

// UpdateGrokOAuthCredentialsIfUnchanged persists provider-issued replacement
// credentials only while the complete Grok OAuth credential document and
// proxy still match the fresh snapshot used by the upstream refresh call. The
// scheduler outbox insert is part of the same PostgreSQL statement, so a
// durable invalidation failure rolls the credential update back as well.
func (r *accountRepository) UpdateGrokOAuthCredentialsIfUnchanged(
	ctx context.Context,
	id int64,
	expectedCredentials map[string]any,
	expectedProxyID *int64,
	credentials map[string]any,
) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	// 写路径加密（A3-E2）：新凭证加密落库 + 维护指纹列；一致性守卫改用指纹比对。
	newCreds, err := prepareCredentialsForStorage(credentials)
	if err != nil {
		return false, err
	}
	credentialsJSON, err := json.Marshal(newCreds.storage)
	if err != nil {
		return false, err
	}
	expectedMac, err := credentialsMACOf(expectedCredentials)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
		UPDATE accounts AS a
		SET credentials = $1::jsonb,
			credentials_mac = $7,
			credentials_api_key_mac = $8,
			updated_at = NOW()
		WHERE a.id = $2
			AND a.deleted_at IS NULL
			AND a.platform = $3
			AND a.type = $4
			AND a.credentials_mac = $5
			AND a.proxy_id IS NOT DISTINCT FROM $6
		RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $9, updated.id, NULL, NULL FROM updated
	`,
		string(credentialsJSON),
		id,
		service.PlatformGrok,
		service.AccountTypeOAuth,
		expectedMac,
		expectedProxyID,
		newCreds.mac,
		newCreds.apiKeyMAC,
		service.SchedulerOutboxEventAccountChanged,
	)
	if err != nil {
		return false, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rowsAffected == 0 {
		return false, nil
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}

// SetGrokOAuthRefreshErrorIfCredentialsUnchanged is the background-refresh
// counterpart to reconciliation's stricter missing-refresh-token mutation. It
// matches the complete credential document used by the failed upstream attempt
// but deliberately does not require the refresh token to be absent.
func (r *accountRepository) SetGrokOAuthRefreshErrorIfCredentialsUnchanged(
	ctx context.Context,
	id int64,
	expectedCredentials map[string]any,
	expectedProxyID *int64,
	errorMsg string,
) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	// 凭证一致性守卫改用指纹比对（A3-E2）。
	expectedMac, err := credentialsMACOf(expectedCredentials)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
		UPDATE accounts AS a
		SET status = $1,
			error_message = $2,
			schedulable = FALSE,
			updated_at = NOW()
		WHERE a.id = $3
			AND a.deleted_at IS NULL
			AND a.platform = $4
			AND a.type = $5
			AND a.status = $6
			AND a.credentials_mac = $7
			AND a.proxy_id IS NOT DISTINCT FROM $8
		RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $9, updated.id, NULL, NULL FROM updated
	`,
		service.StatusError,
		errorMsg,
		id,
		service.PlatformGrok,
		service.AccountTypeOAuth,
		service.StatusActive,
		expectedMac,
		expectedProxyID,
		service.SchedulerOutboxEventAccountChanged,
	)
	if err != nil {
		return false, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rowsAffected == 0 {
		return false, nil
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}

// SetGrokOAuthRefreshTempUnschedulableIfCredentialsUnchanged applies a bounded
// transient refresh quarantine only while the active Grok OAuth credential
// document still matches the exact upstream attempt.
func (r *accountRepository) SetGrokOAuthRefreshTempUnschedulableIfCredentialsUnchanged(
	ctx context.Context,
	id int64,
	expectedCredentials map[string]any,
	expectedProxyID *int64,
	until time.Time,
	reason string,
) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	// 凭证一致性守卫改用指纹比对（A3-E2）。
	expectedMac, err := credentialsMACOf(expectedCredentials)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
		UPDATE accounts AS a
		SET temp_unschedulable_until = $1,
			temp_unschedulable_reason = $2,
			updated_at = NOW()
		WHERE a.id = $3
			AND a.deleted_at IS NULL
			AND a.platform = $4
			AND a.type = $5
			AND a.status = $6
			AND a.credentials_mac = $7
			AND a.proxy_id IS NOT DISTINCT FROM $8
			AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until < $1)
		RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $9, updated.id, NULL, NULL FROM updated
	`,
		until,
		reason,
		id,
		service.PlatformGrok,
		service.AccountTypeOAuth,
		service.StatusActive,
		expectedMac,
		expectedProxyID,
		service.SchedulerOutboxEventAccountChanged,
	)
	if err != nil {
		return false, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rowsAffected == 0 {
		return false, nil
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}

// syncSchedulerAccountSnapshot 在账号状态变更时主动同步快照到调度器缓存。
// 当账号被设置为错误、禁用、不可调度或临时不可调度时调用，
// 确保调度器和粘性会话逻辑能及时感知账号的最新状态，避免继续使用不可用账号。
//
// syncSchedulerAccountSnapshot proactively syncs account snapshot to scheduler cache
// when account status changes. Called when account is set to error, disabled,
// unschedulable, or temporarily unschedulable, ensuring scheduler and sticky session
// logic can promptly detect the latest account state and avoid using unavailable accounts.
func (r *accountRepository) syncSchedulerAccountSnapshot(ctx context.Context, accountID int64) {
	if r == nil || r.schedulerCache == nil || accountID <= 0 {
		return
	}
	account, err := r.GetByID(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("repository.account", "[Scheduler] sync account snapshot read failed: id=%d err=%v", accountID, err)
		return
	}
	if err := r.schedulerCache.SetAccount(ctx, account); err != nil {
		logger.LegacyPrintf("repository.account", "[Scheduler] sync account snapshot write failed: id=%d err=%v", accountID, err)
	}
}

func (r *accountRepository) syncSchedulerAccountSnapshotDetached(ctx context.Context, accountID int64) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	propagationCtx, cancel := context.WithTimeout(base, 2*time.Second)
	defer cancel()
	r.syncSchedulerAccountSnapshot(propagationCtx, accountID)
}

func (r *accountRepository) deleteSchedulerAccountSnapshot(ctx context.Context, accountID int64) {
	if r == nil || r.schedulerCache == nil || accountID <= 0 {
		return
	}
	if err := r.schedulerCache.DeleteAccount(ctx, accountID); err != nil {
		logger.LegacyPrintf("repository.account", "[Scheduler] delete account snapshot failed: id=%d err=%v", accountID, err)
	}
}

func (r *accountRepository) syncSchedulerAccountSnapshots(ctx context.Context, accountIDs []int64) {
	if r == nil || r.schedulerCache == nil || len(accountIDs) == 0 {
		return
	}

	uniqueIDs := make([]int64, 0, len(accountIDs))
	seen := make(map[int64]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		if id <= 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	if len(uniqueIDs) == 0 {
		return
	}

	accounts, err := r.GetByIDs(ctx, uniqueIDs)
	if err != nil {
		logger.LegacyPrintf("repository.account", "[Scheduler] batch sync account snapshot read failed: count=%d err=%v", len(uniqueIDs), err)
		return
	}

	for _, account := range accounts {
		if account == nil {
			continue
		}
		if err := r.schedulerCache.SetAccount(ctx, account); err != nil {
			logger.LegacyPrintf("repository.account", "[Scheduler] batch sync account snapshot write failed: id=%d err=%v", account.ID, err)
		}
	}
}

func (r *accountRepository) ClearError(ctx context.Context, id int64) error {
	// 单条原生 SQL（原为 ent Update）：字段效果与既有 ent Update 逐列等价（status=active、
	// error_message=''、updated_at=NOW()），并同语句内自增 sched_state_revision（R19-F2：
	// ClearError 会替换账号 error/调度阻断状态，属"全部写入点"）。WHERE 仅按 id（与既有
	// ent Update().Where(IDEQ(id)) 语义一致，不额外引入 deleted_at 过滤）；0 行不返回错误，
	// 保持既有 ent Update 语义（不返回 NotFound）。
	_, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET status = $1,
			error_message = '',
			extra = `+schedStateRevisionIncrementExpr()+`,
			updated_at = NOW()
		WHERE id = $2
	`, service.StatusActive, id)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue clear error failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

func (r *accountRepository) AddToGroup(ctx context.Context, accountID, groupID int64, priority int) error {
	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return err
	}
	client := r.client
	if tx != nil {
		defer func() { _ = tx.Rollback() }()
		client = tx.Client()
	}
	if err := lockLiveGroups(ctx, client, []int64{groupID}); err != nil {
		return err
	}
	_, err = client.AccountGroup.Create().
		SetAccountID(accountID).
		SetGroupID(groupID).
		SetPriority(priority).
		Save(ctx)
	if err != nil {
		return err
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	payload := buildSchedulerGroupPayload([]int64{groupID})
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountGroupsChanged, &accountID, nil, payload); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue add to group failed: account=%d group=%d err=%v", accountID, groupID, err)
	}
	return nil
}

func (r *accountRepository) RemoveFromGroup(ctx context.Context, accountID, groupID int64) error {
	_, err := r.client.AccountGroup.Delete().
		Where(
			dbaccountgroup.AccountIDEQ(accountID),
			dbaccountgroup.GroupIDEQ(groupID),
		).
		Exec(ctx)
	if err != nil {
		return err
	}
	payload := buildSchedulerGroupPayload([]int64{groupID})
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountGroupsChanged, &accountID, nil, payload); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue remove from group failed: account=%d group=%d err=%v", accountID, groupID, err)
	}
	return nil
}

func (r *accountRepository) GetGroups(ctx context.Context, accountID int64) ([]service.Group, error) {
	groups, err := r.client.Group.Query().
		Where(
			dbgroup.HasAccountsWith(dbaccount.IDEQ(accountID)),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}

	outGroups := make([]service.Group, 0, len(groups))
	for i := range groups {
		outGroups = append(outGroups, *groupEntityToService(groups[i]))
	}
	return outGroups, nil
}

func (r *accountRepository) BindGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	existingGroupIDs, err := r.loadAccountGroupIDs(ctx, accountID)
	if err != nil {
		return err
	}
	// 使用事务保证删除旧绑定与创建新绑定的原子性
	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return err
	}

	var txClient *dbent.Client
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		txClient = tx.Client()
	} else {
		// 已处于外部事务中（ErrTxStarted），复用当前 client
		txClient = r.client
	}
	if err := lockLiveGroups(ctx, txClient, groupIDs); err != nil {
		return err
	}

	if _, err := txClient.AccountGroup.Delete().Where(dbaccountgroup.AccountIDEQ(accountID)).Exec(ctx); err != nil {
		return err
	}

	if len(groupIDs) == 0 {
		if tx != nil {
			return tx.Commit()
		}
		return nil
	}

	builders := make([]*dbent.AccountGroupCreate, 0, len(groupIDs))
	for i, groupID := range groupIDs {
		builders = append(builders, txClient.AccountGroup.Create().
			SetAccountID(accountID).
			SetGroupID(groupID).
			SetPriority(i+1),
		)
	}

	if _, err := txClient.AccountGroup.CreateBulk(builders...).Save(ctx); err != nil {
		return err
	}

	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	payload := buildSchedulerGroupPayload(mergeGroupIDs(existingGroupIDs, groupIDs))
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountGroupsChanged, &accountID, nil, payload); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue bind groups failed: account=%d err=%v", accountID, err)
	}
	return nil
}

func (r *accountRepository) ListSchedulable(ctx context.Context) ([]service.Account, error) {
	accounts, err := r.schedulableAccountsQuery(time.Now()).All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListSchedulableAccountLoads(ctx context.Context) ([]service.AccountWithConcurrency, error) {
	accounts, err := r.schedulableAccountsQuery(time.Now()).
		Select(
			dbaccount.FieldID,
			dbaccount.FieldConcurrency,
			dbaccount.FieldLoadFactor,
		).
		All(ctx)
	if err != nil {
		return nil, err
	}

	loads := make([]service.AccountWithConcurrency, 0, len(accounts))
	for _, account := range accounts {
		projection := service.Account{
			ID:          account.ID,
			Concurrency: account.Concurrency,
			LoadFactor:  account.LoadFactor,
		}
		loads = append(loads, service.AccountWithConcurrency{
			ID:             account.ID,
			MaxConcurrency: projection.EffectiveLoadFactor(),
		})
	}
	return loads, nil
}

func (r *accountRepository) schedulableAccountsQuery(now time.Time) *dbent.AccountQuery {
	return r.client.Account.Query().
		Where(
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.SchedulableEQ(true),
			tempUnschedulablePredicate(),
			notExpiredPredicate(now),
			dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
			dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
		).
		Order(dbent.Asc(dbaccount.FieldPriority))
}

func (r *accountRepository) ListSchedulableByGroupID(ctx context.Context, groupID int64) ([]service.Account, error) {
	return r.queryAccountsByGroup(ctx, groupID, accountGroupQueryOptions{
		status:      service.StatusActive,
		schedulable: true,
	})
}

func (r *accountRepository) ListSchedulableCapacityByGroupIDs(ctx context.Context, groupIDs []int64) ([]service.GroupAccountCapacityRow, error) {
	groupIDs = uniquePositiveInt64s(groupIDs)
	if len(groupIDs) == 0 {
		return []service.GroupAccountCapacityRow{}, nil
	}
	if r.sql == nil {
		rows := make([]service.GroupAccountCapacityRow, 0)
		for _, groupID := range groupIDs {
			accounts, err := r.ListSchedulableByGroupID(ctx, groupID)
			if err != nil {
				return nil, err
			}
			for i := range accounts {
				acc := &accounts[i]
				rows = append(rows, service.GroupAccountCapacityRow{
					GroupID:             groupID,
					AccountID:           acc.ID,
					Concurrency:         acc.Concurrency,
					Extra:               copyJSONMap(acc.Extra),
					SessionWindowStart:  acc.SessionWindowStart,
					SessionWindowEnd:    acc.SessionWindowEnd,
					SessionWindowStatus: acc.SessionWindowStatus,
				})
			}
		}
		return rows, nil
	}

	rows, err := r.sql.QueryContext(ctx, `
		SELECT
			ag.group_id,
			a.id AS account_id,
			a.concurrency,
			COALESCE(a.extra, '{}'::jsonb)::text AS extra,
			a.session_window_start,
			a.session_window_end,
			COALESCE(a.session_window_status, '') AS session_window_status
		FROM account_groups ag
		JOIN accounts a ON a.id = ag.account_id
		WHERE ag.group_id = ANY($1)
			AND a.deleted_at IS NULL
			AND a.status = $2
			AND a.schedulable = TRUE
			AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= $3)
			AND (a.expires_at IS NULL OR a.expires_at > $3 OR a.auto_pause_on_expired = FALSE)
			AND (a.overload_until IS NULL OR a.overload_until <= $3)
			AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= $3)
		ORDER BY ag.group_id ASC, ag.priority ASC, a.priority ASC, a.id ASC
	`, pq.Array(groupIDs), service.StatusActive, time.Now())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]service.GroupAccountCapacityRow, 0)
	for rows.Next() {
		var row service.GroupAccountCapacityRow
		var extraRaw string
		if err := rows.Scan(
			&row.GroupID,
			&row.AccountID,
			&row.Concurrency,
			&extraRaw,
			&row.SessionWindowStart,
			&row.SessionWindowEnd,
			&row.SessionWindowStatus,
		); err != nil {
			return nil, err
		}
		if extraRaw != "" && extraRaw != "null" {
			var extra map[string]any
			if err := json.Unmarshal([]byte(extraRaw), &extra); err != nil {
				return nil, err
			}
			row.Extra = extra
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *accountRepository) ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	now := time.Now()
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformEQ(platform),
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.SchedulableEQ(true),
			tempUnschedulablePredicate(),
			notExpiredPredicate(now),
			dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
			dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]service.Account, error) {
	// 单平台查询复用多平台逻辑，保持过滤条件与排序策略一致。
	return r.queryAccountsByGroup(ctx, groupID, accountGroupQueryOptions{
		status:      service.StatusActive,
		schedulable: true,
		platforms:   []string{platform},
	})
}

func (r *accountRepository) ListSchedulableByPlatforms(ctx context.Context, platforms []string) ([]service.Account, error) {
	if len(platforms) == 0 {
		return nil, nil
	}
	// 仅返回可调度的活跃账号，并过滤处于过载/限流窗口的账号。
	// 代理与分组信息统一在 accountsToService 中批量加载，避免 N+1 查询。
	now := time.Now()
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformIn(platforms...),
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.SchedulableEQ(true),
			tempUnschedulablePredicate(),
			notExpiredPredicate(now),
			dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
			dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	now := time.Now()
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformEQ(platform),
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.SchedulableEQ(true),
			dbaccount.Not(dbaccount.HasAccountGroups()),
			tempUnschedulablePredicate(),
			notExpiredPredicate(now),
			dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
			dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListSchedulableUngroupedByPlatforms(ctx context.Context, platforms []string) ([]service.Account, error) {
	if len(platforms) == 0 {
		return nil, nil
	}
	now := time.Now()
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformIn(platforms...),
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.SchedulableEQ(true),
			dbaccount.Not(dbaccount.HasAccountGroups()),
			tempUnschedulablePredicate(),
			notExpiredPredicate(now),
			dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
			dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, groupID int64, platforms []string) ([]service.Account, error) {
	if len(platforms) == 0 {
		return nil, nil
	}
	// 复用按分组查询逻辑，保证分组优先级 + 账号优先级的排序与筛选一致。
	return r.queryAccountsByGroup(ctx, groupID, accountGroupQueryOptions{
		status:      service.StatusActive,
		schedulable: true,
		platforms:   platforms,
	})
}

// ListModelAvailabilityCandidates returns the persistently configured account
// pool used to decide whether a model is supported. Unlike scheduling queries,
// it intentionally ignores transient runtime state (rate limits, overload,
// temporary unschedulability, and expiry windows).
func (r *accountRepository) ListModelAvailabilityCandidates(
	ctx context.Context,
	groupID *int64,
	platforms []string,
	includeGrouped bool,
) ([]service.Account, error) {
	if len(platforms) == 0 {
		return []service.Account{}, nil
	}
	if groupID != nil {
		return r.queryAccountsByGroup(ctx, *groupID, accountGroupQueryOptions{
			status:               service.StatusActive,
			schedulable:          true,
			ignoreTransientState: true,
			platforms:            platforms,
		})
	}

	preds := []dbpredicate.Account{
		dbaccount.StatusEQ(service.StatusActive),
		dbaccount.SchedulableEQ(true),
		dbaccount.PlatformIn(platforms...),
	}
	if !includeGrouped {
		preds = append(preds, dbaccount.Not(dbaccount.HasAccountGroups()))
	}
	accounts, err := r.client.Account.Query().
		Where(preds...).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	now := time.Now()
	_, err := r.client.Account.Update().
		Where(dbaccount.IDEQ(id)).
		SetRateLimitedAt(now).
		SetRateLimitResetAt(resetAt).
		Save(ctx)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue rate limit failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

// SetRateLimitedIfLater atomically extends an account-level rate limit. Grok
// requests may finish concurrently, so an older response must not overwrite a
// later reset boundary observed by another request or instance.
func (r *accountRepository) SetRateLimitedIfLater(ctx context.Context, id int64, resetAt time.Time) error {
	now := time.Now()
	updated, err := r.client.Account.Update().
		Where(
			dbaccount.IDEQ(id),
			dbaccount.Or(
				dbaccount.RateLimitResetAtIsNil(),
				dbaccount.RateLimitResetAtLT(resetAt),
			),
		).
		SetRateLimitedAt(now).
		SetRateLimitResetAt(resetAt).
		Save(ctx)
	if err != nil {
		return err
	}
	if updated == 0 {
		// This instance may not have observed the later value written elsewhere.
		// Refresh its local scheduler snapshot even though no outbox event is needed.
		r.syncSchedulerAccountSnapshot(ctx, id)
		return nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue extended rate limit failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

// ClearRateLimitIfObserved clears exactly the Grok rate-limit generation seen
// by a successful request. Matching both timestamps prevents a stale success
// from erasing a later clear/re-arm generation with an equal or shorter reset.
func (r *accountRepository) ClearRateLimitIfObserved(ctx context.Context, id int64, observedLimitedAt, observedResetAt time.Time) (bool, error) {
	updated, err := r.client.Account.Update().
		Where(
			dbaccount.IDEQ(id),
			dbaccount.PlatformEQ(service.PlatformGrok),
			dbaccount.TypeEQ(service.AccountTypeOAuth),
			dbaccount.RateLimitedAtEQ(observedLimitedAt),
			dbaccount.RateLimitResetAtEQ(observedResetAt),
		).
		ClearRateLimitedAt().
		ClearRateLimitResetAt().
		Save(ctx)
	if err != nil {
		return false, err
	}
	if updated == 0 {
		r.syncSchedulerAccountSnapshot(ctx, id)
		return false, nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue observed rate-limit clear failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return true, nil
}

// SetModelRateLimit 经共享收敛写 helper commitModelRateLimitSet 提交模型级限流初始 SET：
// 经 commitModelRateLimitSet 在 per-account 写锁下参与 `(事件时间, tie_breaker)` 事件裁决，单语句
// 原子提交条目+meta（与探测链写入口共用同一裁决基线与提交模式，E37 收敛）。不再经 writeModelRateLimit
// 直写 model_rate_limits 桶（绕过 meta 的旧双路径已移除）。不写 precise_reset 键（默认无精确恢复
// 信号），12 处调用方条目载荷与现状逐键一致（rate_limited_at/rate_limit_reset_at/reason，
// 无 precise_reset）。签名不变，调用方零改动。per-account 写锁已下沉到仓库层（E38）。
func (r *accountRepository) SetModelRateLimit(ctx context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	var rsn string
	if len(reason) > 0 {
		rsn = reason[0]
	}
	return r.commitModelRateLimitSet(ctx, id, scope, resetAt, rsn, nil)
}

// SetModelRateLimitWithPreciseReset 与 SetModelRateLimit 同构（同一条 jsonb_set 写入），
// 额外持久化精确恢复信号标记 precise_reset：true = 上游给出了可解析的恢复时刻
// （rate_limit_reset_at 是真实信号，前端可显示倒计时）；false = 无上游时间信号
// （D5 哨兵分支，rate_limit_reset_at 只是"持续受限"占位值，恢复时刻未知，须由主动
// 复探确认）。该标记是唯一判别依据，前端不得用 reset_at 距今时长等启发式推断。
//
// 该路径是模型级限流的初始 SET（业务链 429 首次写限流）。按 E8 #4，初始 SET 也
// 必须参与 `(事件时间, tie_breaker)` 事件裁决：经 commitModelRateLimitSet 在 per-account 写锁保护下
// 先读 meta、按事件裁决语义比较（严格更旧整体 no-op），再经 CommitModelRateLimitObservation
// 单语句原子提交条目+meta（last_event_at=写入时刻、revision 按现行 rev+1 规则）。per-account 写锁已
// 下沉到仓库层（E38），写锁只存在于 commitModelRateLimitSet 与 WithModelRateLimitAccountLock 两处。
// 不新增第二条提交路径，与探测链写入口共享同一裁决基线与提交模式。
func (r *accountRepository) SetModelRateLimitWithPreciseReset(ctx context.Context, id int64, scope string, resetAt time.Time, preciseReset bool, reason string) error {
	return r.commitModelRateLimitSet(ctx, id, scope, resetAt, reason, map[string]any{"precise_reset": preciseReset})
}

// commitModelRateLimitSet 是 SetModelRateLimit 与 SetModelRateLimitWithPreciseReset 共享的
// 收敛写 helper：把「读 meta -> 严格更旧整体 no-op -> 单语句原子提交条目+meta」的事件裁决逻辑
// 收敛到唯一实现（E37 收敛，消除绕过 meta 的第二条直写路径）。extra 为附加键（precise_reset
// 等，可 nil），在 modelRateLimitPayload 构造条目后并入。行为与原 E8 #4 收敛路径完全一致。
// modelRateLimitWriteLock 返回指定账号的 per-account 模型级限流写锁（惰性 map 模式，
// 与原 service 层 per-account 写锁同款）。全局 modelRateLimitWriteMu 仅保护 map 的
// 读写，per-account 锁本体用于串行化该账号的「读 meta → 裁决 → 提交」区间。
func (r *accountRepository) modelRateLimitWriteLock(accountID int64) *sync.Mutex {
	r.modelRateLimitWriteMu.Lock()
	// 惰性初始化：手写构造的测试桩可能未走 newAccountRepositoryWithSQL，锁表为 nil 时先建表，
	// 保证任何写路径都不会因 nil map 写入而 panic。
	if r.modelRateLimitWriteLocks == nil {
		r.modelRateLimitWriteLocks = make(map[int64]*sync.Mutex)
	}
	l, ok := r.modelRateLimitWriteLocks[accountID]
	if !ok {
		l = &sync.Mutex{}
		r.modelRateLimitWriteLocks[accountID] = l
	}
	r.modelRateLimitWriteMu.Unlock()
	return l
}

// WithModelRateLimitAccountLock 是 per-account 模型级限流写锁的窄面：fn 在持锁期间执行，
// 用于包裹 service 层多步读改写（ApplyModelRateLimitObservation 全区间）。真仓库实现=持锁
// 执行 fn；测试替身桩直接执行 fn（E30/E33 惯例）。锁不可重入——CommitModelRateLimitObservation
// 本身绝不在内部加锁，锁只存在于此边界与 commitModelRateLimitSet 两个外层边界。
func (r *accountRepository) WithModelRateLimitAccountLock(ctx context.Context, accountID int64, fn func(ctx context.Context) error) error {
	lock := r.modelRateLimitWriteLock(accountID)
	lock.Lock()
	defer lock.Unlock()
	return fn(ctx)
}

func (r *accountRepository) commitModelRateLimitSet(ctx context.Context, id int64, scope string, resetAt time.Time, reason string, extra map[string]any) error {
	if scope == "" {
		return nil
	}
	// per-account 写锁下沉（E38）：读 meta → 严格更旧整体 no-op → 单语句原子提交条目+meta
	// 的整个区间持锁，防并发交错读同一 revision 后后写者覆盖较新条目与 meta。锁只在此外层边界
	// 与 WithModelRateLimitAccountLock（探测族写入口）存在，CommitModelRateLimitObservation 内部不再加锁。
	return r.WithModelRateLimitAccountLock(ctx, id, func(ctx context.Context) error {
		// 与写入口同款裁决：仅当本次 SET 严格不旧于既有 meta 事件才应用；否则整体 no-op
		// （防延迟的旧 SET 覆盖较新的探测/业务状态，事件裁决基线对全部状态写入者共享）。
		lastEventAt, rev, has, err := r.GetModelRateLimitMeta(ctx, id, scope)
		if err != nil {
			return err
		}
		now := time.Now()
		if has && now.Before(lastEventAt) {
			return nil
		}
		payload := modelRateLimitPayload(resetAt, reason)
		for k, v := range extra {
			payload[k] = v
		}
		return r.CommitModelRateLimitObservation(ctx, id, scope, payload, false, now, rev+1)
	})
}

// modelRateLimitPayload 构造模型级限流条目的写入载荷（rate_limited_at 取当前时刻，
// rate_limit_reset_at 取调用方给定的恢复时刻；reason 为空则不写该键）。
func modelRateLimitPayload(resetAt time.Time, reason ...string) map[string]any {
	now := time.Now().UTC()
	payload := map[string]any{
		"rate_limited_at":     now.Format(time.RFC3339),
		"rate_limit_reset_at": resetAt.UTC().Format(time.RFC3339),
	}
	if len(reason) > 0 {
		if value := strings.TrimSpace(reason[0]); value != "" {
			payload["reason"] = value
		}
	}
	return payload
}

func (r *accountRepository) SetOverloaded(ctx context.Context, id int64, until time.Time) error {
	_, err := r.client.Account.Update().
		Where(dbaccount.IDEQ(id)).
		SetOverloadUntil(until).
		Save(ctx)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue overload failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

func (r *accountRepository) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	result, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET temp_unschedulable_until = $1,
			temp_unschedulable_reason = $2,
			extra = `+schedStateRevisionIncrementExpr()+`,
			updated_at = NOW()
		WHERE id = $3
			AND deleted_at IS NULL
			AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until < $1)
	`, until, reason, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected <= 0 {
		return nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue temp unschedulable failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

// ShortenTempUnschedulableIfOwned F4 受控缩短原语（派发单 C1-a-r3 / 方案 §4 F4
// R12-F1 消化）：单条条件 UPDATE，无读-改-写两段式。仅当
// temp_unschedulable_until > newUntil 且该 until 的 reason 以 ownedReasonPrefix
// 前缀开头（lifecycle 域拥有）时，把 until 缩短至 newUntil；返回是否实际缩短。
//
// 硬约束：
//   - 非前缀拥有的 reason 一律不动（负例）；
//   - newUntil >= 现有 until 时 no-op（不得延长——延长仍走 SetTempUnschedulable
//     的 GREATEST 守卫：temp_unschedulable_until IS NULL OR < $1）；
//   - 不改 reason 字段、不清除其他字段。
//
// LIKE 前缀条件用 ownedReasonPrefix||'%' 表达，禁止把 reason 全表拉到内存过滤。
func (r *accountRepository) ShortenTempUnschedulableIfOwned(ctx context.Context, accountID int64, newUntil time.Time, ownedReasonPrefix string) (bool, error) {
	result, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET temp_unschedulable_until = $1,
			updated_at = NOW()
		WHERE id = $2
			AND deleted_at IS NULL
			AND temp_unschedulable_until > $1
			AND temp_unschedulable_reason LIKE $3 || '%'
	`, newUntil, accountID, ownedReasonPrefix)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected <= 0 {
		return false, nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue shorten temp unschedulable failed: account=%d err=%v", accountID, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, accountID)
	return true, nil
}

func (r *accountRepository) SetGrokCredentialTempUnschedulableIfMatch(
	ctx context.Context,
	id int64,
	snapshot service.GrokCredentialMutationSnapshot,
	until time.Time,
	reason string,
) (bool, error) {
	// 凭证一致性守卫改用指纹比对（A3-E2）。
	expectedMac, err := credentialsMACOfJSONString(snapshot.CredentialsJSON)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
		UPDATE accounts AS a
		SET temp_unschedulable_until = CASE
				WHEN a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until < $1 THEN $1
				ELSE a.temp_unschedulable_until
			END,
			temp_unschedulable_reason = $2,
			updated_at = NOW()
		WHERE a.id = $3
			AND a.deleted_at IS NULL
			AND a.status = $4
			AND a.platform = $5
			AND a.type = $6
			AND a.schedulable IS TRUE
			AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW())
			AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW())
			AND (a.overload_until IS NULL OR a.overload_until <= NOW())
			AND (a.auto_pause_on_expired IS NOT TRUE OR a.expires_at IS NULL OR a.expires_at > NOW())
			AND a.credentials_mac = $7
			AND a.proxy_id IS NOT DISTINCT FROM $8
		RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $9, updated.id, NULL, NULL FROM updated
	`, until, reason, id, service.StatusActive, service.PlatformGrok, service.AccountTypeOAuth,
		expectedMac, snapshot.ProxyID, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}

func (r *accountRepository) ClearTempUnschedulable(ctx context.Context, id int64) error {
	_, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET temp_unschedulable_until = NULL,
			temp_unschedulable_reason = NULL,
			extra = `+schedStateRevisionIncrementExpr()+`,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
	`, id)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue clear temp unschedulable failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

// SetTempUnschedulableReason updates only the temp-unschedulable reason string for
// an account that is still inside its cooldown window (e.g. to bump probe attempt
// counters). It does not extend the cooldown; the scheduling state is unchanged so
// no scheduler outbox event is emitted.
func (r *accountRepository) SetTempUnschedulableReason(ctx context.Context, id int64, reason string) error {
	_, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET temp_unschedulable_reason = $2,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
			AND temp_unschedulable_until IS NOT NULL
			AND temp_unschedulable_until > NOW()
	`, id, reason)
	if err != nil {
		return err
	}
	return nil
}

// ListTempUnschedulableAccounts returns accounts currently inside a temp-unschedulable
// cooldown, ordered by soonest expiry and capped at limit. It powers the optional
// health-breaker probe-based recovery sweep.
func (r *accountRepository) ListTempUnschedulableAccounts(ctx context.Context, now time.Time, limit int) ([]*service.Account, error) {
	if limit <= 0 {
		limit = 200
	}
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.TempUnschedulableUntilNotNil(),
			dbaccount.TempUnschedulableUntilGT(now),
			dbaccount.DeletedAtIsNil(),
		).
		Order(dbent.Asc(dbaccount.FieldTempUnschedulableUntil)).
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

// ============================================================================
// F3 403 content-policy 恢复链存储层原语（派发单 C1-b① / 方案 §3.3）。
// 全部定义在 accountRepository 具体类型上（不加入 AccountRepository 大接口，承
// C1-a-r3 ShortenTempUnschedulableIfOwned 先例），服务侧经窄面接口 + 类型断言消费
// （C1-b②）。均为单语句/单事务原子边界，禁止读-改-写两步。
// ============================================================================

// AllocHTTP403Generation 原子分配 F3 代际令牌：单语句 `UPDATE ... RETURNING` 自增独立
// 持久计数键 http_403_gen_counter 并返回新值（R17-F3/R19-F1，禁止读-改-写）。计数键独立
// 于 http_403_recovery：2xx 恢复与转交清理只删恢复记录、不删计数器，防代际复用。
func (r *accountRepository) AllocHTTP403Generation(ctx context.Context, accountID int64) (int64, error) {
	if r.sql == nil {
		return 0, errors.New("account repository SQL executor is not configured")
	}
	var generation int64
	err := scanSingleRow(ctx, r.sql, `
		UPDATE accounts
		SET extra = `+http403GenerationCounterIncrementExpr()+`,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
		RETURNING (extra->>'`+HTTP403GenerationCounterExtraKey+`')::bigint
	`, []any{accountID}, &generation)
	if err != nil {
		return 0, err
	}
	return generation, nil
}

// Mark403PausedWithRecovery 原子完成 F3 首次写入（R15-F1）：在**单条** UPDATE 内复刻 403
// 三振升级链 SetError 的全部字段效果（status=error、error_message=errorMsg、
// schedulable=false、updated_at），可选写 temp_unschedulable_until（含 F3 所有权标记
// reason 前缀，R15-F2），并合并 http_403_recovery 状态键 + 自增 sched_state_revision。
// 禁止"先冻结后补键"的两步写（防进程在部分写入后崩溃形成"已冻结但无 sweep 候选"）。
//
// 字段效果与 SetError 逐列一致（status / error_message / schedulable）；tempUnschedulable
// 为 nil 时不触碰既有 temp_unschedulable_until（COALESCE 保留旧值）。
func (r *accountRepository) Mark403PausedWithRecovery(
	ctx context.Context,
	accountID int64,
	until time.Time,
	generation int64,
	reason string,
	errorMsg string,
	tempUnschedulable *time.Time,
) error {
	if r.sql == nil {
		return errors.New("account repository SQL executor is not configured")
	}
	payload, err := json.Marshal(map[string]any{
		"until":      until.UTC().Format(time.RFC3339),
		"generation": generation,
		"reason":     reason,
		"owner":      HTTP403RecoveryOwnerF3,
	})
	if err != nil {
		return err
	}
	var tempUntil any
	var tempUntilReason any
	if tempUnschedulable != nil {
		tempUntil = tempUnschedulable.UTC()
		tempUntilReason = HTTP403RecoveryTempUnschedulableReasonPrefix + reason
	}
	result, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET status = $1,
			error_message = $2,
			schedulable = FALSE,
			temp_unschedulable_until = COALESCE($3, temp_unschedulable_until),
			temp_unschedulable_reason = COALESCE($4, temp_unschedulable_reason),
			extra = `+http403MarkRecoveryWithRevisionExpr("$5")+`,
			updated_at = NOW()
		WHERE id = $6
			AND deleted_at IS NULL
	`, service.StatusError, errorMsg, tempUntil, tempUntilReason, string(payload), accountID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAccountNotFound
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue 403 paused recovery failed: account=%d err=%v", accountID, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, accountID)
	return nil
}

// ClearHTTP403RecoveryIfOwned CAS 条件清理（方案 §3.3 条件清理，R1-F4/R2-F3/R6-F2/R12-F1/R18-F2）：
// 仅当 http_403_recovery.generation 与传入值匹配**且** owner=F3**且** 键内 state_revision 与当前
// 全局 sched_state_revision 相等时，单条 UPDATE 内：删除 http_403_recovery 键、清除 error 元数据、
// 恢复调度（复刻既有 ClearError/SetSchedulable(true) 字段效果：status=active、error_message=”、
// schedulable=TRUE），并**按所有权标记条件**清除 F3 拥有的 temp_unschedulable_until（reason 前缀
// 命中才清，R15-F2；非 F3 拥有的 until 不动），同语句自增 sched_state_revision。失配（任一条件
// 不满足）→ 返回 false 且零写入。第三条 revision 条件使任何他链状态替换（其写入点自增全局
// revision）令本次 CAS 失配，防止恢复探针清掉他链刚写入的 error/调度阻断状态。
func (r *accountRepository) ClearHTTP403RecoveryIfOwned(ctx context.Context, accountID int64, generation int64) (bool, error) {
	if r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	result, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET status = $3,
			error_message = '',
			schedulable = TRUE,
			temp_unschedulable_until = CASE
				WHEN temp_unschedulable_reason LIKE $4 || '%' THEN NULL
				ELSE temp_unschedulable_until END,
			temp_unschedulable_reason = CASE
				WHEN temp_unschedulable_reason LIKE $4 || '%' THEN NULL
				ELSE temp_unschedulable_reason END,
			extra = `+http403RemoveRecoveryWithRevisionExpr()+`,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'generation' = $2
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'owner' = $5
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'state_revision' = extra->>'`+SchedStateRevisionExtraKey+`'
	`, accountID, strconv.FormatInt(generation, 10), service.StatusActive,
		HTTP403RecoveryTempUnschedulableReasonPrefix, HTTP403RecoveryOwnerF3)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected <= 0 {
		return false, nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue clear 403 recovery failed: account=%d err=%v", accountID, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, accountID)
	return true, nil
}

// HTTP403TransitionTarget 已迁移至 service 包（service.HTTP403TransitionTarget）：类型归属
// service 可避免 service 反向 import repository 形成依赖环（repository 已 import service）。
// 字段语义与 CAS 行为不变（见 service.HTTP403TransitionTarget 文档）。

// TransitionHTTP403RecoveryTo 原子转交（R7-F3）：同一单条 UPDATE 内写入新链暂停状态效果 +
// 移除 http_403_recovery 元数据（旧 403 状态不得残留阻塞后续恢复）+ 自增 sched_state_revision；
// CAS 条件与 ClearHTTP403RecoveryIfOwned 相同（generation + owner=F3 + 键内 state_revision =
// 当前全局 sched_state_revision，R18-F2），失配返回 false 且零写入。target 类型为
// service.HTTP403TransitionTarget（已从本包迁出，避免 import 环）。
//
// 字段写入与 target 语义严格对应（C1-b②-r2）：status/error_message 直写
// （error_message = $4 无条件赋值，故 target.ErrorMessage 为 "" 即清空 error_message 列，
// 旧 403 残留文本被擦除，不写转交文本进 error 字段）；schedulable 直写；temp_unschedulable_until/
// reason 仅当 target 对应指针非 nil 时写入，否则若该 until 的 reason 以 F3 所有权前缀开头则
// 清除（R15-F2 所有权感知），否则保留原值。
func (r *accountRepository) TransitionHTTP403RecoveryTo(ctx context.Context, accountID int64, generation int64, target service.HTTP403TransitionTarget) (bool, error) {
	if r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	result, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET status = $3,
			error_message = $4,
			schedulable = $5,
			temp_unschedulable_until = CASE
				WHEN $6::timestamptz IS NOT NULL THEN $6::timestamptz
				WHEN temp_unschedulable_reason LIKE $9 || '%' THEN NULL
				ELSE temp_unschedulable_until END,
			temp_unschedulable_reason = CASE
				WHEN $7::text IS NOT NULL THEN $7::text
				WHEN temp_unschedulable_reason LIKE $9 || '%' THEN NULL
				ELSE temp_unschedulable_reason END,
			extra = `+http403RemoveRecoveryWithRevisionExpr()+`,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'generation' = $2
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'owner' = $8
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'state_revision' = extra->>'`+SchedStateRevisionExtraKey+`'
	`, accountID, strconv.FormatInt(generation, 10), target.Status, target.ErrorMessage,
		target.Schedulable, target.TempUnschedulableUntil, target.TempUnschedulableReason,
		HTTP403RecoveryOwnerF3, HTTP403RecoveryTempUnschedulableReasonPrefix)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected <= 0 {
		return false, nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue transition 403 recovery failed: account=%d err=%v", accountID, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, accountID)
	return true, nil
}

// ListHTTP403RecoveryDueAccounts F3 候选查询（方案 §4 F3 行 / R7-F2）：返回
// http_403_recovery 非空且 until <= now 且未删除的账号，按 until 升序，limit 默认 200
// （对齐 ListTempUnschedulableAccounts 惯例）。**不受 schedulable / HasError / 快照新鲜度
// 过滤影响**——持有 F3 持久化状态的账号必须被 sweep 候选覆盖（D4 证据基线）。映射复用
// accountsToService，返回值顺序与 SQL 的 until 升序一致。
func (r *accountRepository) ListHTTP403RecoveryDueAccounts(ctx context.Context, now time.Time, limit int) ([]*service.Account, error) {
	if limit <= 0 {
		limit = 200
	}
	if r.sql == nil {
		return nil, errors.New("account repository SQL executor is not configured")
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT id
		FROM accounts
		WHERE deleted_at IS NULL
			AND jsonb_typeof(extra->'`+HTTP403RecoveryExtraKey+`') = 'object'
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'until' IS NOT NULL
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'until' ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2})$'
			AND (extra->'`+HTTP403RecoveryExtraKey+`'->>'until')::timestamptz <= $1
		ORDER BY (extra->'`+HTTP403RecoveryExtraKey+`'->>'until')::timestamptz ASC, id ASC
		LIMIT $2
	`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []*service.Account{}, nil
	}

	entities, err := r.client.Account.Query().
		Where(dbaccount.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	mapped, err := r.accountsToService(ctx, entities)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]service.Account, len(mapped))
	for _, account := range mapped {
		byID[account.ID] = account
	}
	out := make([]*service.Account, 0, len(ids))
	for _, id := range ids {
		if account, ok := byID[id]; ok {
			acc := account
			out = append(out, &acc)
		}
	}
	return out, nil
}

// UpdateHTTP403RecoveryUntil 同键改 until/reason（CAS 三条件同 ①：generation + owner=F3
// + 键内 state_revision = 当前全局 sched_state_revision）。命中则改写键内 until/reason 并
// 同步键内 state_revision 与全局自增同点出生（承 ① r2 表达式助手先例）；失配零写入，
// 返回 false。用于「仍 403 带 until 续等」与「仍 403 无 until 冷却轮」（派发单 C1-b② 1d）。
func (r *accountRepository) UpdateHTTP403RecoveryUntil(ctx context.Context, accountID int64, generation int64, newUntil time.Time, reason string) (bool, error) {
	if r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	newUntilUTC := newUntil.UTC()
	// 同键改写 until/reason（保留 generation/owner），并将键内 state_revision 与全局
	// sched_state_revision 同点自增 +1（复用 ① r2 表达式助手先例）。表达式纯 Go 拼接构建，
	// 避免 SQL 字面量中混入 Go 连接符（否则会被解析为多字符 rune 字面量）。
	recoveryPath := "'{" + HTTP403RecoveryExtraKey + "}'"
	globalPath := "'{" + SchedStateRevisionExtraKey + "}'"
	newRecoveryKeyValueExpr := "jsonb_set(jsonb_set(jsonb_set(COALESCE(extra->'" + HTTP403RecoveryExtraKey +
		"', '{}'::jsonb), '{until}', to_jsonb($3::text), true), '{reason}', to_jsonb($4::text), true), " +
		"'{state_revision}', " + schedStateRevisionNextValueExpr() + ", true)"
	newExtraExpr := "jsonb_set(jsonb_set(COALESCE(extra, '{}'::jsonb), " + recoveryPath + ", " +
		newRecoveryKeyValueExpr + ", true), " + globalPath + ", " + schedStateRevisionNextValueExpr() + ", true)"
	result, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET extra = `+newExtraExpr+`,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'generation' = $2
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'owner' = $5
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'state_revision' = extra->>'`+SchedStateRevisionExtraKey+`'
	`, accountID, strconv.FormatInt(generation, 10), newUntilUTC.Format(time.RFC3339), reason, HTTP403RecoveryOwnerF3)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected <= 0 {
		return false, nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue update 403 recovery until failed: account=%d err=%v", accountID, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, accountID)
	return true, nil
}

// RemoveHTTP403RecoveryRecord stale 清理（R17-F2 / 派发单 C1-b② 1b）：单条件 UPDATE 仅当
// 键存在且 generation 匹配时删除 http_403_recovery 键（**只删键，不动 status / error_message
// / schedulable / temp_unschedulable**——调用时机 = 已核验易主，状态归他链管），同语句内
// 自增 sched_state_revision；失配（键不存在或 generation 不匹配）零写入，返回 false。
func (r *accountRepository) RemoveHTTP403RecoveryRecord(ctx context.Context, accountID int64, generation int64) (bool, error) {
	if r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	result, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET extra = `+http403RemoveRecoveryWithRevisionExpr()+`,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
			AND extra->'`+HTTP403RecoveryExtraKey+`'->>'generation' = $2
	`, accountID, strconv.FormatInt(generation, 10))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected <= 0 {
		return false, nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue remove 403 recovery record failed: account=%d err=%v", accountID, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, accountID)
	return true, nil
}

// ListLegacyHTTP403ErrorAccounts 存量候选（R8-F1 / 派发单 C1-b② 3c）：status='error' 且
// error_message 以 'Access forbidden (403):' 起、且 extra->'http_403_recovery' 为空、且
// deleted_at 为空的账号，按 updated_at 升序，limit 默认 200。映射复用 accountsToService。
// 候选查询排除已持有 http_403_recovery 键的账号（幂等；写入后离开候选集）。
func (r *accountRepository) ListLegacyHTTP403ErrorAccounts(ctx context.Context, limit int) ([]*service.Account, error) {
	if limit <= 0 {
		limit = 200
	}
	if r.sql == nil {
		return nil, errors.New("account repository SQL executor is not configured")
	}
	const prefix = "Access forbidden (403):"
	rows, err := r.sql.QueryContext(ctx, `
		SELECT id
		FROM accounts
		WHERE deleted_at IS NULL
			AND status = 'error'
			AND error_message LIKE $1
			AND extra->'`+HTTP403RecoveryExtraKey+`' IS NULL
		ORDER BY updated_at ASC, id ASC
		LIMIT $2
	`, prefix+"%", limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []*service.Account{}, nil
	}

	entities, err := r.client.Account.Query().
		Where(dbaccount.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	mapped, err := r.accountsToService(ctx, entities)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]service.Account, len(mapped))
	for _, account := range mapped {
		byID[account.ID] = account
	}
	out := make([]*service.Account, 0, len(ids))
	for _, id := range ids {
		if account, ok := byID[id]; ok {
			acc := account
			out = append(out, &acc)
		}
	}
	return out, nil
}

func (r *accountRepository) ClearRateLimit(ctx context.Context, id int64) error {
	_, err := r.client.Account.Update().
		Where(dbaccount.IDEQ(id)).
		ClearRateLimitedAt().
		ClearRateLimitResetAt().
		ClearOverloadUntil().
		Save(ctx)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue clear rate limit failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

func (r *accountRepository) ClearAntigravityQuotaScopes(ctx context.Context, id int64) error {
	client := clientFromContext(ctx, r.client)
	result, err := client.ExecContext(
		ctx,
		"UPDATE accounts SET extra = COALESCE(extra, '{}'::jsonb) - 'antigravity_quota_scopes', updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL",
		id,
	)
	if err != nil {
		return err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAccountNotFound
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue clear quota scopes failed: account=%d err=%v", id, err)
	}
	return nil
}

func (r *accountRepository) ClearModelRateLimits(ctx context.Context, id int64) error {
	client := clientFromContext(ctx, r.client)
	result, err := client.ExecContext(
		ctx,
		"UPDATE accounts SET extra = COALESCE(extra, '{}'::jsonb) - 'model_rate_limits', updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL",
		id,
	)
	if err != nil {
		return err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAccountNotFound
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue clear model rate limit failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

// modelRateLimitsMetaKey 是 extra 下独立于 model_rate_limits 的写入事件元数据桶：
// 记录每个 scope 的最后一次权威写入事件时间与单调递增 revision，用于探测链写入入口
// 的事件时间原子应用与同刻精度碰撞的 tie-breaker（即使 model_rate_limits[scope] 被
// 成功探测清除，元数据仍保留，旧事件不得覆盖新状态）。
const modelRateLimitsMetaKey = "model_rate_limits_meta"

// DeleteModelRateLimitsMetaKeys 按 keys 从 extra.model_rate_limits_meta 桶内原子删除
// 命中键（D-QL-007 F2：清理函数改为仓库层 jsonb 原子按键删除）。
//
// SQL 形态与 CommitModelRateLimitObservation 的既有 jsonb_set 同构：单条 UPDATE 内
// `extra->'model_rate_limits_meta' - $keys::text[]`（jsonb `-` 数组删除）只重写
// model_rate_limits_meta 子对象本身，桶外其他 extra 顶层键与桶内未命中键均不受影响，
// 不覆盖并发写入者（如 CommitModelRateLimitObservation 经 jsonb_set 对同一桶按 scope
// 键的写入）——消除旧「读整桶→内存删键→UpdateExtra 整桶回写」对同一桶其他键的
// 覆盖窗口。
//
//   - 单条 UPDATE + RowsAffected 校验（0 行 = 账号不存在 → ErrAccountNotFound）；
//   - meta 桶为 JSONB 字面量 null / 非对象（数组、标量）/ 缺失时按空对象处理
//     （jsonb_typeof 归一，D-QL-009 F1）：'{}'::jsonb - keys 仍为空对象回写，
//     不会因对 scalar 执行 jsonb `-` 删除而报 "cannot delete from scalar"
//     中止批量清理；
//   - updated_at = NOW() 与既有写路径同口径；
//   - 返回值：keys 为空时不执行 SQL、直接返回 (0, nil)（调用方 no-op 语义）。
func (r *accountRepository) DeleteModelRateLimitsMetaKeys(ctx context.Context, id int64, keys []string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	client := clientFromContext(ctx, r.client)
	result, err := client.ExecContext(ctx, `
		UPDATE accounts SET
			extra = jsonb_set(
				COALESCE(extra, '{}'::jsonb),
				ARRAY['model_rate_limits_meta'],
				COALESCE(
					CASE WHEN jsonb_typeof(extra->'model_rate_limits_meta') = 'object'
					     THEN extra->'model_rate_limits_meta' END,
					'{}'::jsonb
				) - $1::text[],
				true
			),
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
	`, pq.StringArray(keys), id)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected == 0 {
		return 0, service.ErrAccountNotFound
	}
	return affected, nil
}

// modelRateLimitsKey 是 accounts.extra 下模型级限流桶的 jsonb 键名（与 service 包同名
// 常量同义；repository 包内独立定义以免跨包耦合）。
const modelRateLimitsKey = "model_rate_limits"

// GetModelRateLimitEntry 读取 extra->'model_rate_limits'->scope 的单个限流条目
// （整个嵌套对象），不存在时返回 (nil, nil)。
func (r *accountRepository) GetModelRateLimitEntry(ctx context.Context, id int64, scope string) (map[string]any, error) {
	rows, err := r.sql.QueryContext(ctx, `
		SELECT extra->'model_rate_limits'->$1
		FROM accounts WHERE id = $2 AND deleted_at IS NULL
	`, scope, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, err
	}
	return entry, nil
}

// CommitModelRateLimitObservation 在同一数据库写入（单条 UPDATE 语句）内提交
// model_rate_limits[scope] 条目（clear=true 时清除该 scope）与
// model_rate_limits_meta[scope] 元数据（last_event_at/revision）：
//   - 单语句内两处 jsonb_set 对同一行生效，任一环节失败则整条语句失败、两者均不变，
//     消除「状态条目已变而裁决基线仍旧值」的中间态（E1 原子性）；
//   - meta 与状态条目保持分桶，clear 成功清除条目时 meta 仍保留（事件裁决基线不丢）。
//
// 调用方负责在进程内 per-account 锁下完成读-改-写，保证事件时间原子应用。
func (r *accountRepository) CommitModelRateLimitObservation(ctx context.Context, id int64, scope string, entry map[string]any, clear bool, lastEventAt time.Time, revision int64) error {
	var raw []byte = []byte("{}")
	var err error
	if !clear {
		raw, err = json.Marshal(entry)
		if err != nil {
			return err
		}
	}
	meta := map[string]any{
		// RFC3339Nano（与 E2 迁移记录同口径）：裁决比较用的是带纳秒的 EventTime，meta
		// 秒级写入会让同秒内的旧事件被误判为新。纳秒无损写入/读取后裁决基线不再丢失精度。
		"last_event_at": lastEventAt.UTC().Format(time.RFC3339Nano),
		"revision":      revision,
	}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// commitObservation 把「单条 UPDATE（状态条目+meta）+ RowsAffected 校验 + outbox 写入」
	// 收进同一事务边界：事务内（恢复路径的既有 txCtx）直接复用该事务；非事务路径下由
	// WithObservationTx 新开 r.client.Tx 后同样参与——任一步失败整体回滚（状态+meta+outbox
	// 同生共死），不再「先独立提交状态、再写 outbox 失败却已持久化」。
	commitObservation := func(txCtx context.Context) error {
		client := clientFromContext(txCtx, r.client)
		// 一次 UPDATE：状态条目（clear=true 时从 model_rate_limits 桶删除 scope，否则写入
		// entry）与 model_rate_limits_meta[scope] 元数据在同一语句内对同一行提交，
		// 保证原子性（任一环节失败整条失败，两者均不变）。
		result, err := client.ExecContext(txCtx, `
			UPDATE accounts SET
				extra = jsonb_set(
					jsonb_set(
						COALESCE(extra, '{}'::jsonb),
						ARRAY['model_rate_limits'],
						CASE WHEN $3::boolean
							THEN COALESCE(extra->'model_rate_limits', '{}'::jsonb) - $1
							ELSE jsonb_set(COALESCE(extra->'model_rate_limits', '{}'::jsonb), ARRAY[$1]::text[], $4::jsonb, true)
						END,
						true
					),
					ARRAY['model_rate_limits_meta'],
					jsonb_set(COALESCE(extra->'model_rate_limits_meta', '{}'::jsonb), ARRAY[$1]::text[], $5::jsonb, true),
					true
				),
				updated_at = NOW()
			WHERE id = $2 AND deleted_at IS NULL
		`, scope, id, clear, raw, metaRaw)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return service.ErrAccountNotFound
		}
		// outbox 写入经事务感知执行器：事务内（恢复路径 / 本次新开事务）参与同一 ent 事务，
		// 随 UPDATE 一并提交；失败时整体回滚（状态+meta 一并撤销）并返回错误——不再「状态已
		// 提交却无事件」。错误明确返回，沿调用链传播到写入口，保持可观测。
		if err := enqueueSchedulerOutbox(txCtx, txAwareSQLExecutor(txCtx, r.sql, r.client), service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
			return err
		}
		return nil
	}

	if dbent.TxFromContext(ctx) != nil {
		// 事务内路径（恢复链 WithObservationTx 调用方）：UPDATE + outbox 已在既有事务内，
		// 随告警关闭失败整体回滚（状态不提交、事件丢弃）。快照同步不在提交前做（事务内读不到
		// 未提交状态，预览会拿到旧快照），由 WithObservationTx 成功返回处经导出窄面
		// SyncSchedulerAccountSnapshot 补做。
		return commitObservation(ctx)
	}

	// 非事务路径：复用 WithObservationTx ——ctx 无事务时新开 r.client.Tx，将 UPDATE（状态+meta）、
	// RowsAffected 校验、outbox 写入收进同一事务。任一环节失败整体回滚（同生共死），不再前一写
	// 独立提交（旧路径：UPDATE 先 autocommit，outbox 再走裸连接，outbox 失败已持久化却返回错误）。
	if err := r.WithObservationTx(ctx, commitObservation); err != nil {
		return err
	}
	// 快照同步仅在事务提交成功后（且仅非事务路径）执行：不预览未提交状态——提交前同步会读到
	// 旧快照，导致权威状态与缓存分裂。提交后的同步由 ctx（无事务）正常读取已提交行。
	// 同步失败仅可观测（E47）：业务结果已确认提交，不得被重新分类为提交失败（与
	// ratelimit_service.go 提交后同步约定同口径）；错误经结构化日志暴露，不 return。
	if err := r.SyncSchedulerAccountSnapshot(ctx, id); err != nil {
		logger.LegacyPrintf("repository.account", "[Scheduler] sync account snapshot after commit failed: id=%d err=%v", id, err)
	}
	return nil
}

// SyncSchedulerAccountSnapshot 暴露快照同步的薄导出，供两处补做（事务内不预览未提交状态，
// 故不在提交前同步）：
//   - 写入口恢复路径：在 WithObservationTx 成功返回后补做；
//   - CommitModelRateLimitObservation 非事务路径：在 WithObservationTx 提交成功后补做。
//
// 同步失败返回明确错误以便上报告警：状态已提交，返回错误不影响原子性，但必须可观测（不静默吞掉）。
// 单一实现，薄包既有 syncSchedulerAccountSnapshot 的读取/写入，仅补返回错误。
func (r *accountRepository) SyncSchedulerAccountSnapshot(ctx context.Context, accountID int64) error {
	if r == nil || accountID <= 0 {
		return nil
	}
	account, err := r.GetByID(ctx, accountID)
	if err != nil {
		return err
	}
	if r.schedulerCache == nil {
		return nil
	}
	if err := r.schedulerCache.SetAccount(ctx, account); err != nil {
		return err
	}
	return nil
}

// WithObservationTx 在写入口恢复路径内开启（或参与已有）ent 事务，将状态提交与同维告警关闭
// 收进同一事务：fn 内任一错误都整体回滚（状态不提交、告警不变）。若 ctx 已携带事务则直接参与，
// 否则开启新事务并在 fn 返回 nil 时提交。沿 MigrateSettingAtomically / RollbackSettingAtomically
// 的 TxFromContext 窄面惯例（E20 #2）。
func (r *accountRepository) WithObservationTx(ctx context.Context, fn func(txCtx context.Context) error) error {
	if r == nil || r.client == nil {
		return fmt.Errorf("nil account repository client")
	}
	if dbent.TxFromContext(ctx) != nil {
		return fn(ctx)
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	if err := fn(txCtx); err != nil {
		return err
	}
	return tx.Commit()
}

// GetModelRateLimitMeta 读取 extra->'model_rate_limits_meta'->scope 的写入事件元数据
// （{last_event_at, revision}），不存在时 ok=false。
func (r *accountRepository) GetModelRateLimitMeta(ctx context.Context, id int64, scope string) (lastEventAt time.Time, revision int64, ok bool, err error) {
	rows, qerr := r.sql.QueryContext(ctx, `
		SELECT extra->'model_rate_limits_meta'->$1
		FROM accounts WHERE id = $2 AND deleted_at IS NULL
	`, scope, id)
	if qerr != nil {
		return time.Time{}, 0, false, qerr
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if rerr := rows.Err(); rerr != nil {
			return time.Time{}, 0, false, rerr
		}
		return time.Time{}, 0, false, nil
	}
	var raw []byte
	if serr := rows.Scan(&raw); serr != nil {
		return time.Time{}, 0, false, serr
	}
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, 0, false, nil
	}
	var meta struct {
		LastEventAt string `json:"last_event_at"`
		Revision    int64  `json:"revision"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return time.Time{}, 0, false, err
	}
	if meta.LastEventAt == "" {
		return time.Time{}, 0, false, nil
	}
	// 解析兼容 Nano：历史秒级（RFC3339）与当前纳秒级（RFC3339Nano）均能无损还原。
	t, perr := time.Parse(time.RFC3339Nano, meta.LastEventAt)
	if perr != nil {
		// E12 #4：meta 损坏（last_event_at 非法）不得静默当作"无 meta"——否则调用方
		// 会以 rev=0 为基线把旧事件当作新事件应用（重置 revision），覆盖较新的权威状态。
		// 返回明确错误，调用方（ApplyModelRateLimitObservation /
		// SetModelRateLimitWithPreciseReset 的裁决读取）沿错误中止本次读改写。
		// 空 meta / "null" / 空 last_event_at 的"正常不存在"路径在上方保持不变。
		return time.Time{}, 0, false, fmt.Errorf("parse model_rate_limits_meta.last_event_at %q: %w", meta.LastEventAt, perr)
	}
	return t, meta.Revision, true, nil
}

// SetModelRateLimitMeta 已移除：其独立写入是 E1 原子性缺陷的根源（状态条目与 meta
// 分两次数据库写）。meta 写入现已并入 CommitModelRateLimitObservation 的单条 UPDATE。
// 既有双桶设计保留（状态条目与 meta 分桶；CommitModelRateLimitObservation 在 clear=true
// 时仅删 model_rate_limits[scope] 条目而保留 model_rate_limits_meta[scope] 裁决基线），
// 全部状态写入者（SetModelRateLimit / SetModelRateLimitWithPreciseReset / 探测链写入口）
// 均经同一收敛写链提交，提交路径合并且原子化（E37 收敛）。

func (r *accountRepository) UpdateSessionWindow(ctx context.Context, id int64, start, end *time.Time, status string) error {
	builder := r.client.Account.Update().
		Where(dbaccount.IDEQ(id)).
		SetSessionWindowStatus(status)
	if start != nil {
		builder.SetSessionWindowStart(*start)
	}
	if end != nil {
		builder.SetSessionWindowEnd(*end)
	}
	_, err := builder.Save(ctx)
	if err != nil {
		return err
	}
	// 触发调度器缓存更新（仅当窗口时间有变化时）
	if start != nil || end != nil {
		if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
			logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue session window update failed: account=%d err=%v", id, err)
		}
	}
	return nil
}

func (r *accountRepository) UpdateSessionWindowEnd(ctx context.Context, id int64, end time.Time) error {
	_, err := r.client.Account.Update().
		Where(dbaccount.IDEQ(id)).
		SetSessionWindowEnd(end).
		Save(ctx)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue session window end update failed: account=%d err=%v", id, err)
	}
	return nil
}

func (r *accountRepository) SetSchedulable(ctx context.Context, id int64, schedulable bool) error {
	// 单条原生 SQL（原为 ent Update）：字段效果逐列等价（schedulable=$1、updated_at=NOW()），
	// 并同语句内自增 sched_state_revision（R19-F2：SetSchedulable 会替换账号调度阻断状态，
	// 属"全部写入点"）。WHERE 仅按 id（与既有 ent Update().Where(IDEQ(id)) 语义一致）；
	// 0 行不返回错误，保持既有 ent Update 语义（不返回 NotFound）。
	_, err := r.sql.ExecContext(ctx, `
		UPDATE accounts
		SET schedulable = $1,
			extra = `+schedStateRevisionIncrementExpr()+`,
			updated_at = NOW()
		WHERE id = $2
	`, schedulable, id)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue schedulable change failed: account=%d err=%v", id, err)
	}
	if !schedulable {
		r.syncSchedulerAccountSnapshot(ctx, id)
	}
	return nil
}

func (r *accountRepository) AutoPauseExpiredAccounts(ctx context.Context, now time.Time) (int64, error) {
	rows, err := r.sql.QueryContext(ctx, `
		UPDATE accounts
		SET schedulable = FALSE,
			updated_at = NOW()
		WHERE deleted_at IS NULL
			AND schedulable = TRUE
			AND auto_pause_on_expired = TRUE
			AND expires_at IS NOT NULL
			AND expires_at <= $1
		RETURNING id
	`, now)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = rows.Close()
	}()

	accountIDs := make([]int64, 0)
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return 0, err
		}
		accountIDs = append(accountIDs, accountID)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(accountIDs) > 0 {
		// 只刷新本次暂停的账号及其所属分组，避免少量账号到期触发所有调度桶重建。
		payload := map[string]any{"account_ids": accountIDs}
		if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountBulkChanged, nil, nil, payload); err != nil {
			logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue auto pause account changes failed: err=%v", err)
		}
	}
	return int64(len(accountIDs)), nil
}

func (r *accountRepository) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	updates = stripCodexFingerprintSeedFromExtraUpdate(updates)
	if len(updates) == 0 {
		return nil
	}

	// 使用 JSONB 合并操作实现原子更新，避免读-改-写的并发丢失更新问题
	payload, err := json.Marshal(updates)
	if err != nil {
		return err
	}

	clearProbeSnapshot := upstreamBillingProbeExplicitlyDisabled(updates) || upstreamBillingProbeSnapshotClearRequested(updates)
	durableSchedulerChange := shouldEnqueueSchedulerOutboxForExtraUpdates(updates) || clearProbeSnapshot
	baseCtx := ctx
	contextTx := dbent.TxFromContext(ctx)
	client := clientFromContext(ctx, r.client)
	var tx *dbent.Tx
	if durableSchedulerChange && contextTx == nil {
		var txErr error
		tx, txErr = r.client.Tx(ctx)
		if txErr != nil && !errors.Is(txErr, dbent.ErrTxStarted) {
			return txErr
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}
	extraExpression := "COALESCE(extra, '{}'::jsonb) || $1::jsonb"
	if clearProbeSnapshot {
		extraExpression = "(" + extraExpression + ") - 'upstream_billing_probe'"
	}
	if service.ShouldEnsureCodexFingerprintSeedForExtraUpdates(updates) {
		extraExpression = ensureCodexFingerprintSeedSQL(extraExpression)
	}
	result, err := client.ExecContext(
		ctx,
		"UPDATE accounts SET extra = "+extraExpression+", updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL",
		string(payload), id,
	)

	if err != nil {
		return err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAccountNotFound
	}
	if durableSchedulerChange {
		if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
			return err
		}
		if tx != nil {
			if err := tx.Commit(); err != nil {
				return err
			}
		}
		if contextTx == nil {
			r.syncSchedulerAccountSnapshot(baseCtx, id)
		}
	} else {
		// 观测型 extra 字段不需要触发 bucket 重建，但仍同步单账号快照，
		// 让 sticky session / GetAccount 命中缓存时也能读到最新数据，
		// 同时避免缓存局部 patch 覆盖掉并发写入的其它账号字段。
		if dbent.TxFromContext(ctx) == nil {
			r.syncSchedulerAccountSnapshot(ctx, id)
		}
	}
	return nil
}

// TokenHarbor 会话持久化 extra 键名（字面值契约，与 internal/service 侧定义逐字一致）。
const (
	tokenHarborSessionCookieExtraKey  = "th_session_cookie"
	tokenHarborSessionLoginAtExtraKey = "th_session_login_at"
)

// StoreTokenHarborSession 持久化 TH dashboard 会话（enc:v1 加密落 extra，
// 原子合并——UpdateExtra 本身是 COALESCE||$1 jsonb 合并，无读改写竞争）。
func (r *accountRepository) StoreTokenHarborSession(ctx context.Context, id int64, cookie string, loginAt time.Time) error {
	encrypted, err := encryptCredentialsValue(cookie)
	if err != nil {
		return fmt.Errorf("encrypt tokenharbor session cookie: %w", err)
	}
	return r.UpdateExtra(ctx, id, map[string]any{
		tokenHarborSessionCookieExtraKey:  encrypted,
		tokenHarborSessionLoginAtExtraKey: loginAt.Unix(),
	})
}

// LoadTokenHarborSession 读取持久化会话；无会话（账号不存在/键缺失/JSON null）
// 返回 ("", zero, nil)。密文解密失败按错误上抛（fail loud），不把密文当明文放行。
func (r *accountRepository) LoadTokenHarborSession(ctx context.Context, id int64) (cookie string, loginAt time.Time, err error) {
	if r.sql == nil {
		return "", time.Time{}, errors.New("account repository SQL executor not configured")
	}
	rows, err := r.sql.QueryContext(ctx,
		"SELECT extra->>'"+tokenHarborSessionCookieExtraKey+"', extra->>'"+tokenHarborSessionLoginAtExtraKey+
			"' FROM accounts WHERE id = $1 AND deleted_at IS NULL", id)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if scanErr := rows.Err(); scanErr != nil {
			return "", time.Time{}, scanErr
		}
		return "", time.Time{}, nil
	}
	var rawCookie, rawLoginAt sql.NullString
	if err := rows.Scan(&rawCookie, &rawLoginAt); err != nil {
		return "", time.Time{}, err
	}
	if !rawCookie.Valid || rawCookie.String == "" {
		return "", time.Time{}, nil
	}
	cookie = rawCookie.String
	if credcrypt.IsEncrypted(cookie) {
		plain, decErr := credcrypt.Decrypt(cookie)
		if decErr != nil {
			return "", time.Time{}, fmt.Errorf("decrypt tokenharbor session cookie: %w", decErr)
		}
		cookie = plain
	}
	if rawLoginAt.Valid && rawLoginAt.String != "" {
		unix, parseErr := strconv.ParseInt(rawLoginAt.String, 10, 64)
		if parseErr != nil {
			return "", time.Time{}, fmt.Errorf("parse tokenharbor session login_at %q: %w", rawLoginAt.String, parseErr)
		}
		loginAt = time.Unix(unix, 0)
	}
	return cookie, loginAt, nil
}

// ClearTokenHarborSession 置 null 两键（th_session_cookie/th_session_login_at）。
// UpdateExtra 的 updates map 值为 nil 时 JSONB 合并后是 JSON null——读取方把
// null/缺失/过期统一视为无会话。
func (r *accountRepository) ClearTokenHarborSession(ctx context.Context, id int64) error {
	return r.UpdateExtra(ctx, id, map[string]any{
		tokenHarborSessionCookieExtraKey:  nil,
		tokenHarborSessionLoginAtExtraKey: nil,
	})
}

// ListDueBalanceProbeAccounts returns enabled, active API-key accounts whose
// persisted probe snapshot is stale or absent. SQL filtering avoids hydrating
// the full account pool each runner tick; stale accounts progress in bounded
// batches on every configured probe interval.
func (r *accountRepository) ListDueBalanceProbeAccounts(ctx context.Context, now time.Time, limit int) ([]service.Account, error) {
	if limit <= 0 {
		return []service.Account{}, nil
	}
	if r.sql == nil {
		return nil, errors.New("account repository SQL executor not configured")
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT id
		FROM accounts
		WHERE deleted_at IS NULL
			AND status = 'active'
			AND type = 'apikey'
			AND credentials @> '{"balance_probe": {"enabled": true}}'::jsonb
			AND (
				extra #>> '{balance_probe_snapshot,fetched_at}' IS NULL
				OR (extra #>> '{balance_probe_snapshot,fetched_at}')::timestamptz <= $1
			)
		ORDER BY extra #>> '{balance_probe_snapshot,fetched_at}' ASC NULLS FIRST, id ASC
		LIMIT $2
	`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []service.Account{}, nil
	}
	accounts, err := r.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]service.Account, 0, len(accounts))
	for _, account := range accounts {
		if account != nil {
			out = append(out, *account)
		}
	}
	return out, nil
}

// UpdateUpstreamBillingProbeSnapshot stores a probe result only while the
// network identity used by that probe is still current.
func (r *accountRepository) UpdateUpstreamBillingProbeSnapshot(
	ctx context.Context,
	account *service.Account,
	snapshot *service.UpstreamBillingProbeSnapshot,
	rateMultiplier *float64,
) error {
	if account == nil || snapshot == nil {
		return service.ErrAccountNilInput
	}
	if snapshot.Status != service.UpstreamBillingProbeStatusOK {
		rateMultiplier = nil
	}
	if dbent.TxFromContext(ctx) == nil {
		tx, err := r.client.Tx(ctx)
		if errors.Is(err, dbent.ErrTxStarted) {
			return r.updateUpstreamBillingProbeSnapshotInTx(ctx, account, snapshot, rateMultiplier)
		}
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		if err := r.updateUpstreamBillingProbeSnapshotInTx(dbent.NewTxContext(ctx, tx), account, snapshot, rateMultiplier); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		// The durable outbox event is committed with the snapshot. This direct
		// cache write only reduces visibility latency on the current instance.
		r.syncSchedulerAccountSnapshot(ctx, account.ID)
		return nil
	}
	return r.updateUpstreamBillingProbeSnapshotInTx(ctx, account, snapshot, rateMultiplier)
}

func (r *accountRepository) updateUpstreamBillingProbeSnapshotInTx(
	ctx context.Context,
	account *service.Account,
	snapshot *service.UpstreamBillingProbeSnapshot,
	rateMultiplier *float64,
) error {
	payload, err := json.Marshal(map[string]any{service.UpstreamBillingProbeExtraKey: snapshot})
	if err != nil {
		return err
	}
	var expectedSnapshot any
	if account.Extra != nil {
		expectedSnapshot = account.Extra[service.UpstreamBillingProbeExtraKey]
	}
	expectedSnapshotJSON, err := json.Marshal(expectedSnapshot)
	if err != nil {
		return err
	}
	var expectedEnabled any
	if account.Extra != nil {
		expectedEnabled = account.Extra[service.UpstreamBillingProbeEnabledExtraKey]
	}
	expectedEnabledJSON, err := json.Marshal(expectedEnabled)
	if err != nil {
		return err
	}
	var expectedRateSyncEnabled any
	if account.Extra != nil {
		expectedRateSyncEnabled = account.Extra[service.UpstreamBillingRateSyncEnabledExtraKey]
	}
	expectedRateSyncEnabledJSON, err := json.Marshal(expectedRateSyncEnabled)
	if err != nil {
		return err
	}
	client := clientFromContext(ctx, r.client)
	proxyMatches, err := lockAndMatchProbeProxyIdentity(ctx, client, account)
	if err != nil {
		return err
	}
	if !proxyMatches {
		return service.ErrUpstreamBillingProbeIdentityChanged
	}
	var proxyID any
	if account.ProxyID != nil {
		proxyID = *account.ProxyID
	}
	// 凭证一致性守卫改用指纹比对（A3-E2）。
	credentialsMac, err := credentialsMACOf(account.Credentials)
	if err != nil {
		return err
	}
	result, err := client.ExecContext(ctx, `
		UPDATE accounts
		SET
			extra = COALESCE(extra, '{}'::jsonb) || $1::jsonb,
			rate_multiplier = CASE
				WHEN $10::numeric IS NOT NULL
					AND extra @> '{"upstream_billing_probe_enabled": true}'::jsonb
					AND extra @> '{"upstream_billing_rate_sync_enabled": true}'::jsonb
				THEN $10::numeric
				ELSE rate_multiplier
			END,
			updated_at = NOW()
		WHERE id = $2
			AND platform = $3
			AND type = $4
			AND credentials_mac = $5
			AND proxy_id IS NOT DISTINCT FROM $6
			AND COALESCE(extra -> 'upstream_billing_probe', 'null'::jsonb) = $7::jsonb
			AND COALESCE(extra -> 'upstream_billing_probe_enabled', 'null'::jsonb) = $8::jsonb
			AND COALESCE(extra -> 'upstream_billing_rate_sync_enabled', 'null'::jsonb) = $9::jsonb
			AND deleted_at IS NULL
	`, string(payload), account.ID, account.Platform, account.Type, credentialsMac, proxyID, string(expectedSnapshotJSON), string(expectedEnabledJSON), string(expectedRateSyncEnabledJSON), rateMultiplier)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrUpstreamBillingProbeIdentityChanged
	}
	return enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &account.ID, nil, nil)
}

func lockAndMatchProbeProxyIdentity(ctx context.Context, client *dbent.Client, account *service.Account) (bool, error) {
	if account.ProxyID == nil {
		return true, nil
	}
	rows, err := client.QueryContext(ctx, `
		SELECT protocol, host, port, COALESCE(username, ''), COALESCE(password, ''), status
		FROM proxies
		WHERE id = $1 AND deleted_at IS NULL
		FOR SHARE
	`, *account.ProxyID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, err
		}
		return account.Proxy == nil, nil
	}
	if account.Proxy == nil || account.Proxy.ID != *account.ProxyID {
		return false, nil
	}
	var current proxyProbeIdentity
	if err := rows.Scan(&current.protocol, &current.host, &current.port, &current.username, &current.password, &current.status); err != nil {
		return false, err
	}
	return current == proxyProbeIdentityFromService(account.Proxy), rows.Err()
}

func shouldEnqueueSchedulerOutboxForExtraUpdates(updates map[string]any) bool {
	if len(updates) == 0 {
		return false
	}
	for key := range updates {
		if isSchedulerNeutralExtraKey(key) {
			continue
		}
		return true
	}
	return false
}

func isSchedulerNeutralExtraKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	if _, ok := schedulerNeutralExtraKeys[key]; ok {
		return true
	}
	for _, prefix := range schedulerNeutralExtraKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func upstreamBillingProbeExplicitlyDisabled(extra map[string]any) bool {
	enabled, ok := extra[service.UpstreamBillingProbeEnabledExtraKey].(bool)
	return ok && !enabled
}

func upstreamBillingProbeSnapshotClearRequested(extra map[string]any) bool {
	value, ok := extra[service.UpstreamBillingProbeExtraKey]
	return ok && value == nil
}

func ollamaCloudUsageSnapshotClearRequested(extra map[string]any) bool {
	value, ok := extra[service.OllamaCloudUsageSnapshotExtraKey]
	return ok && value == nil
}

func (r *accountRepository) BulkUpdate(ctx context.Context, ids []int64, updates service.AccountBulkUpdate) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	updates.Extra = stripCodexFingerprintSeedFromExtraUpdate(updates.Extra)

	setClauses := make([]string, 0, 8)
	args := make([]any, 0, 8)

	idx := 1
	ollamaProxyIdentityChanged := ""
	if updates.Name != nil {
		setClauses = append(setClauses, "name = $"+itoa(idx))
		args = append(args, *updates.Name)
		idx++
	}
	if updates.ProxyID != nil {
		// 0 表示清除代理（前端发送 0 而不是 null 来表达清除意图）
		if *updates.ProxyID == 0 {
			setClauses = append(setClauses, "proxy_id = NULL")
			ollamaProxyIdentityChanged = "proxy_id IS NOT NULL"
		} else {
			proxyPlaceholder := "$" + itoa(idx)
			setClauses = append(setClauses, "proxy_id = "+proxyPlaceholder)
			ollamaProxyIdentityChanged = "proxy_id IS DISTINCT FROM " + proxyPlaceholder
			args = append(args, *updates.ProxyID)
			idx++
		}
	}
	if updates.Concurrency != nil {
		setClauses = append(setClauses, "concurrency = $"+itoa(idx))
		args = append(args, *updates.Concurrency)
		idx++
	}
	if updates.Priority != nil {
		setClauses = append(setClauses, "priority = $"+itoa(idx))
		args = append(args, *updates.Priority)
		idx++
	}
	if updates.RateMultiplier != nil {
		setClauses = append(setClauses, "rate_multiplier = $"+itoa(idx))
		args = append(args, *updates.RateMultiplier)
		idx++
	}
	if updates.LoadFactor != nil {
		if *updates.LoadFactor <= 0 {
			setClauses = append(setClauses, "load_factor = NULL")
		} else {
			setClauses = append(setClauses, "load_factor = $"+itoa(idx))
			args = append(args, *updates.LoadFactor)
			idx++
		}
	}
	if updates.Status != nil {
		setClauses = append(setClauses, "status = $"+itoa(idx))
		args = append(args, *updates.Status)
		idx++
	}
	if updates.Schedulable != nil {
		setClauses = append(setClauses, "schedulable = $"+itoa(idx))
		args = append(args, *updates.Schedulable)
		idx++
	}
	if updates.ProbeEnabled != nil {
		if updates.Extra == nil {
			updates.Extra = make(map[string]any)
		}
		updates.Extra[service.UpstreamBillingProbeEnabledExtraKey] = *updates.ProbeEnabled
	}
	// JSONB 需要合并而非覆盖，使用 raw SQL 保持旧行为。
	credentialPlaceholder := ""
	apiKeyMacPlaceholder := ""
	if len(updates.Credentials) > 0 {
		// 写路径加密（A3-E2）：payload 敏感子键按键加密后参与 SQL 侧 JSONB 合并，
		// 逐键加密与合并语义兼容。合并后的整份文档指纹无法在本条 SQL 内计算，
		// credentials_mac 置 NULL（安全侧失效，CAS 守卫视为不匹配），由下一次
		// 整体写或 E3 存量迁移回填；api_key 指纹可由 payload 直接计算，照常维护。
		bulkCreds, err := prepareCredentialsForStorage(updates.Credentials)
		if err != nil {
			return 0, err
		}
		payload, err := json.Marshal(bulkCreds.storage)
		if err != nil {
			return 0, err
		}
		credentialPlaceholder = "$" + itoa(idx)
		setClauses = append(setClauses, "credentials = COALESCE(credentials, '{}'::jsonb) || "+credentialPlaceholder+"::jsonb")
		args = append(args, payload)
		idx++
		setClauses = append(setClauses, "credentials_mac = NULL")
		if _, ok := updates.Credentials["api_key"]; ok {
			apiKeyMacPlaceholder = "$" + itoa(idx)
			setClauses = append(setClauses, "credentials_api_key_mac = "+apiKeyMacPlaceholder)
			args = append(args, bulkCreds.apiKeyMAC)
			idx++
		}
	}

	ollamaGroupIdentityChanges := make([]string, 0, 2)
	if _, ok := updates.Credentials["api_key"]; ok {
		// 守卫改为指纹比对（A3-E2）：存量行 api_key 指纹为 NULL 时判"变化"，
		// 属灰度窗口期的安全侧误报，E3 迁移回填后消失。
		ollamaGroupIdentityChanges = append(ollamaGroupIdentityChanges, "credentials_api_key_mac IS DISTINCT FROM "+apiKeyMacPlaceholder)
	}
	if _, ok := updates.Credentials["base_url"]; ok {
		ollamaGroupIdentityChanges = append(ollamaGroupIdentityChanges,
			"NOT ("+ollamaCloudBaseURLMatchesSQL("credentials ->> 'base_url'")+
				" AND "+ollamaCloudBaseURLMatchesSQL(credentialPlaceholder+"::jsonb ->> 'base_url'")+")")
	}

	if len(updates.Extra) > 0 || len(ollamaGroupIdentityChanges) > 0 || ollamaProxyIdentityChanged != "" || updates.EnsureCodexFingerprintSeed {
		extraExpression := "COALESCE(extra, '{}'::jsonb)"
		if len(updates.Extra) > 0 {
			payload, err := json.Marshal(updates.Extra)
			if err != nil {
				return 0, err
			}
			extraExpression += " || $" + itoa(idx) + "::jsonb"
			args = append(args, payload)
			idx++
			if upstreamBillingProbeExplicitlyDisabled(updates.Extra) || upstreamBillingProbeSnapshotClearRequested(updates.Extra) {
				extraExpression = "(" + extraExpression + ") - 'upstream_billing_probe'"
			}
			if ollamaCloudUsageSnapshotClearRequested(updates.Extra) {
				extraExpression = "(" + extraExpression + ") - 'ollama_cloud_usage_snapshot'"
			}
		}
		eligibleAccount := "platform IN (" + ollamaCloudUsagePlatformsSQL + ") AND type = 'apikey'"
		groupIdentityChanged := ""
		if len(ollamaGroupIdentityChanges) > 0 {
			groupIdentityChanged = "(" + eligibleAccount + " AND (" + joinClauses(ollamaGroupIdentityChanges, " OR ") + "))"
		}
		snapshotIdentityChanged := groupIdentityChanged
		if ollamaProxyIdentityChanged != "" {
			proxyChanged := "(" + eligibleAccount + " AND " + ollamaProxyIdentityChanged + ")"
			if snapshotIdentityChanged == "" {
				snapshotIdentityChanged = proxyChanged
			} else {
				snapshotIdentityChanged = "(" + snapshotIdentityChanged + " OR " + proxyChanged + ")"
			}
		}
		if groupIdentityChanged != "" {
			extraExpression = "CASE" +
				" WHEN " + groupIdentityChanged + " THEN (" + extraExpression + ") - 'ollama_cloud_usage_session' - 'ollama_cloud_usage_auto_refresh' - 'ollama_cloud_usage_snapshot'" +
				" WHEN " + snapshotIdentityChanged + " THEN (" + extraExpression + ") - 'ollama_cloud_usage_snapshot'" +
				" ELSE " + extraExpression + " END"
		} else if snapshotIdentityChanged != "" {
			extraExpression = "CASE WHEN " + snapshotIdentityChanged + " THEN (" + extraExpression + ") - 'ollama_cloud_usage_snapshot' ELSE " + extraExpression + " END"
		}
		if updates.EnsureCodexFingerprintSeed {
			extraExpression = ensureCodexFingerprintSeedSQL(extraExpression)
		}
		setClauses = append(setClauses, "extra = "+extraExpression)
	}

	if len(setClauses) == 0 {
		return 0, nil
	}

	setClauses = append(setClauses, "updated_at = NOW()")

	whereClause := " WHERE id = ANY($" + itoa(idx) + ") AND deleted_at IS NULL"
	args = append(args, pq.Array(ids))
	idx++
	if updates.ProbeEnabled != nil {
		whereClause += " AND type = $" + itoa(idx)
		args = append(args, service.AccountTypeAPIKey)
	}
	query := "UPDATE accounts SET " + joinClauses(setClauses, ", ") + whereClause

	baseCtx := ctx
	contextTx := dbent.TxFromContext(ctx)
	exec := r.sql
	var tx *dbent.Tx
	if contextTx != nil {
		exec = contextTx.Client()
	} else if r.client != nil {
		var txErr error
		tx, txErr = r.client.Tx(ctx)
		if txErr != nil && !errors.Is(txErr, dbent.ErrTxStarted) {
			return 0, txErr
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			exec = tx.Client()
		}
	}

	result, err := exec.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if updates.ProbeEnabled != nil {
		expectedRows := int64(0)
		seenIDs := make(map[int64]struct{}, len(ids))
		for _, id := range ids {
			if _, seen := seenIDs[id]; seen {
				continue
			}
			seenIDs[id] = struct{}{}
			expectedRows++
		}
		if rows != expectedRows {
			return 0, service.ErrUpstreamBillingProbeAccountInvalid
		}
	}
	if rows > 0 {
		payload := map[string]any{"account_ids": ids}
		if err := enqueueSchedulerOutbox(ctx, exec, service.SchedulerOutboxEventAccountBulkChanged, nil, nil, payload); err != nil {
			return 0, err
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
	}
	if rows > 0 && contextTx == nil {
		shouldSync := false
		if updates.Status != nil && (*updates.Status == service.StatusError || *updates.Status == service.StatusDisabled) {
			shouldSync = true
		}
		if updates.Schedulable != nil && !*updates.Schedulable {
			shouldSync = true
		}
		if shouldSync {
			r.syncSchedulerAccountSnapshots(baseCtx, ids)
		}
	}
	return rows, nil
}

type accountGroupQueryOptions struct {
	status               string
	schedulable          bool
	ignoreTransientState bool
	platforms            []string // 允许的多个平台，空切片表示不进行平台过滤
}

// aggregatePlatformFamily 是国产 OpenAI 兼容平台族（方案 §2.1-1 唯一事实源）。
// 分组 platform 属于该族时，候选池额外纳入 platform=codebuddy 的可调度账号。
// 不在此集合内的平台（openai/anthropic/gemini/grok 等）行为逐位不变。
var aggregatePlatformFamily = map[string]struct{}{
	service.PlatformDeepseek:  {},
	service.PlatformZhipu:     {},
	service.PlatformKimi:      {},
	service.PlatformMiniMax:   {},
	service.PlatformOther:     {},
	service.PlatformCodeBuddy: {},
}

// expandPlatformsForAggregatePool 对聚合族分组，在原有平台集合基础上并入
// codebuddy 平台，使候选池额外包含可调度 codebuddy 账号；非聚合平台集合逐位
// 不变。若 codebuddy 已在集合中则不重复并入（codebuddy 自有分组行为不变）。
// enabled=false 时不并入 codebuddy（聚合直绑开关关闭——方案 §1.2）；
// 该函数仅扩展"平台集合"，不涉及 schedulable/状态/瞬态过滤，后者由
// queryAccountsByGroup 既有谓词保证（schedulable=false 的 codebuddy 任何分组都
// 不可见）。
func expandPlatformsForAggregatePool(platforms []string, enabled bool) []string {
	if len(platforms) == 0 {
		return platforms
	}
	if !enabled {
		return platforms
	}
	needCodeBuddy := false
	hasCodeBuddy := false
	for _, p := range platforms {
		if _, ok := aggregatePlatformFamily[p]; ok {
			needCodeBuddy = true
		}
		if p == service.PlatformCodeBuddy {
			hasCodeBuddy = true
		}
	}
	if !needCodeBuddy || hasCodeBuddy {
		return platforms
	}
	out := make([]string, 0, len(platforms)+1)
	out = append(out, platforms...)
	out = append(out, service.PlatformCodeBuddy)
	return out
}

// intersectsAggregateFamily 判断给定平台集合是否与聚合族相交。
// 仅当相交时 queryAccountsByGroup 才需要读取 groups 行的 aggregate_codebuddy_enabled
// 开关；非聚合族平台集合零额外读取、行为逐位不变（方案 §1.2）。
func intersectsAggregateFamily(platforms []string) bool {
	for _, p := range platforms {
		if _, ok := aggregatePlatformFamily[p]; ok {
			return true
		}
	}
	return false
}

func (r *accountRepository) queryAccountsByGroup(ctx context.Context, groupID int64, opts accountGroupQueryOptions) ([]service.Account, error) {
	q := r.client.AccountGroup.Query().
		Where(dbaccountgroup.GroupIDEQ(groupID))

	// 通过 account_groups 中间表查询账号，并按需叠加状态/平台/调度能力过滤。
	preds := make([]dbpredicate.Account, 0, 6)
	preds = append(preds, dbaccount.DeletedAtIsNil())
	if opts.status != "" {
		preds = append(preds, dbaccount.StatusEQ(opts.status))
	}
	if len(opts.platforms) > 0 {
		platforms := opts.platforms
		// 方案 §1.2：当 platforms 与聚合族相交时，同一查询边界内按 PK 读取 groups 行
		// 取 aggregate_codebuddy_enabled，据此决定是否并入 codebuddy；非聚合族平台集合
		// 零额外读取、行为逐位不变。读取失败 → 查询报错失败关闭，不静默取默认。
		// 未分组（groupID<=0，如标准模式未分组桶）无绑定授权，不补入 codebuddy。
		if groupID > 0 && intersectsAggregateFamily(opts.platforms) {
			grp, gerr := r.client.Group.Get(ctx, groupID)
			if gerr != nil {
				return nil, fmt.Errorf("queryAccountsByGroup: read aggregate_codebuddy_enabled for group %d: %w", groupID, gerr)
			}
			platforms = expandPlatformsForAggregatePool(opts.platforms, grp.AggregateCodebuddyEnabled)
		}
		preds = append(preds, dbaccount.PlatformIn(platforms...))
	}
	if opts.schedulable {
		preds = append(preds, dbaccount.SchedulableEQ(true))
		if !opts.ignoreTransientState {
			now := time.Now()
			preds = append(preds,
				tempUnschedulablePredicate(),
				notExpiredPredicate(now),
				dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
				dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
			)
		}
	}

	if len(preds) > 0 {
		q = q.Where(dbaccountgroup.HasAccountWith(preds...))
	}

	groups, err := q.
		Order(
			dbaccountgroup.ByPriority(),
			dbaccountgroup.ByAccountField(dbaccount.FieldPriority),
		).
		WithAccount().
		All(ctx)
	if err != nil {
		return nil, err
	}

	orderedIDs := make([]int64, 0, len(groups))
	accountMap := make(map[int64]*dbent.Account, len(groups))
	for _, ag := range groups {
		if ag.Edges.Account == nil {
			continue
		}
		if _, exists := accountMap[ag.AccountID]; exists {
			continue
		}
		accountMap[ag.AccountID] = ag.Edges.Account
		orderedIDs = append(orderedIDs, ag.AccountID)
	}

	accounts := make([]*dbent.Account, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		if acc, ok := accountMap[id]; ok {
			accounts = append(accounts, acc)
		}
	}

	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) accountsToService(ctx context.Context, accounts []*dbent.Account) ([]service.Account, error) {
	if len(accounts) == 0 {
		return []service.Account{}, nil
	}

	accountIDs := make([]int64, 0, len(accounts))
	proxyIDs := make([]int64, 0, len(accounts))
	for _, acc := range accounts {
		accountIDs = append(accountIDs, acc.ID)
		if acc.ProxyID != nil {
			proxyIDs = append(proxyIDs, *acc.ProxyID)
		}
		if acc.ProxyFallbackOriginID != nil {
			proxyIDs = append(proxyIDs, *acc.ProxyFallbackOriginID)
		}
	}

	proxyMap, err := r.loadProxies(ctx, proxyIDs)
	if err != nil {
		return nil, err
	}
	groupsByAccount, groupIDsByAccount, accountGroupsByAccount, err := r.loadAccountGroups(ctx, accountIDs)
	if err != nil {
		return nil, err
	}

	outAccounts := make([]service.Account, 0, len(accounts))
	for _, acc := range accounts {
		out, err := accountEntityToService(acc)
		if err != nil {
			return nil, err
		}
		if out == nil {
			continue
		}
		if acc.ProxyID != nil {
			if proxy, ok := proxyMap[*acc.ProxyID]; ok {
				out.Proxy = proxy
			}
		}
		out.ProxyFallbackOriginID = acc.ProxyFallbackOriginID
		if acc.ProxyFallbackOriginID != nil {
			if op, ok := proxyMap[*acc.ProxyFallbackOriginID]; ok && op != nil {
				n := op.Name
				out.ProxyFallbackOriginName = &n
			}
		}
		if groups, ok := groupsByAccount[acc.ID]; ok {
			out.Groups = groups
		}
		if groupIDs, ok := groupIDsByAccount[acc.ID]; ok {
			out.GroupIDs = groupIDs
		}
		if ags, ok := accountGroupsByAccount[acc.ID]; ok {
			out.AccountGroups = ags
		}
		outAccounts = append(outAccounts, *out)
	}

	return outAccounts, nil
}

func tempUnschedulablePredicate() dbpredicate.Account {
	return dbpredicate.Account(func(s *entsql.Selector) {
		col := s.C("temp_unschedulable_until")
		s.Where(entsql.Or(
			entsql.IsNull(col),
			entsql.LTE(col, entsql.Expr("NOW()")),
		))
	})
}

func notExpiredPredicate(now time.Time) dbpredicate.Account {
	return dbaccount.Or(
		dbaccount.ExpiresAtIsNil(),
		dbaccount.ExpiresAtGT(now),
		dbaccount.AutoPauseOnExpiredEQ(false),
	)
}

func (r *accountRepository) loadProxies(ctx context.Context, proxyIDs []int64) (map[int64]*service.Proxy, error) {
	proxyMap := make(map[int64]*service.Proxy)
	proxyIDs = uniquePositiveInt64s(proxyIDs)
	if len(proxyIDs) == 0 {
		return proxyMap, nil
	}

	for start := 0; start < len(proxyIDs); start += postgresParameterBatchSize {
		end := start + postgresParameterBatchSize
		if end > len(proxyIDs) {
			end = len(proxyIDs)
		}
		proxies, err := r.client.Proxy.Query().Where(dbproxy.IDIn(proxyIDs[start:end]...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range proxies {
			proxyMap[p.ID] = proxyEntityToService(p)
		}
	}
	return proxyMap, nil
}

func (r *accountRepository) loadAccountGroups(ctx context.Context, accountIDs []int64) (map[int64][]*service.Group, map[int64][]int64, map[int64][]service.AccountGroup, error) {
	groupsByAccount := make(map[int64][]*service.Group)
	groupIDsByAccount := make(map[int64][]int64)
	accountGroupsByAccount := make(map[int64][]service.AccountGroup)

	accountIDs = uniquePositiveInt64s(accountIDs)
	if len(accountIDs) == 0 {
		return groupsByAccount, groupIDsByAccount, accountGroupsByAccount, nil
	}

	for start := 0; start < len(accountIDs); start += postgresParameterBatchSize {
		end := start + postgresParameterBatchSize
		if end > len(accountIDs) {
			end = len(accountIDs)
		}
		entries, err := r.client.AccountGroup.Query().
			Where(dbaccountgroup.AccountIDIn(accountIDs[start:end]...)).
			Order(dbaccountgroup.ByAccountID(), dbaccountgroup.ByPriority()).
			All(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		groupIDs := make([]int64, 0, len(entries))
		for _, ag := range entries {
			groupIDs = append(groupIDs, ag.GroupID)
		}
		groupMap, err := r.loadGroups(ctx, groupIDs)
		if err != nil {
			return nil, nil, nil, err
		}

		for _, ag := range entries {
			groupSvc := groupMap[ag.GroupID]
			agSvc := service.AccountGroup{
				AccountID: ag.AccountID,
				GroupID:   ag.GroupID,
				Priority:  ag.Priority,
				CreatedAt: ag.CreatedAt,
				Group:     groupSvc,
			}
			accountGroupsByAccount[ag.AccountID] = append(accountGroupsByAccount[ag.AccountID], agSvc)
			groupIDsByAccount[ag.AccountID] = append(groupIDsByAccount[ag.AccountID], ag.GroupID)
			if groupSvc != nil {
				groupsByAccount[ag.AccountID] = append(groupsByAccount[ag.AccountID], groupSvc)
			}
		}
	}

	return groupsByAccount, groupIDsByAccount, accountGroupsByAccount, nil
}

func (r *accountRepository) loadGroups(ctx context.Context, groupIDs []int64) (map[int64]*service.Group, error) {
	groupMap := make(map[int64]*service.Group)
	groupIDs = uniquePositiveInt64s(groupIDs)
	if len(groupIDs) == 0 {
		return groupMap, nil
	}

	for start := 0; start < len(groupIDs); start += postgresParameterBatchSize {
		end := start + postgresParameterBatchSize
		if end > len(groupIDs) {
			end = len(groupIDs)
		}
		groups, err := r.client.Group.Query().Where(dbgroup.IDIn(groupIDs[start:end]...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			groupMap[g.ID] = groupEntityToService(g)
		}
	}
	return groupMap, nil
}

func uniquePositiveInt64s(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (r *accountRepository) loadAccountGroupIDs(ctx context.Context, accountID int64) ([]int64, error) {
	entries, err := r.client.AccountGroup.
		Query().
		Where(dbaccountgroup.AccountIDEQ(accountID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.GroupID)
	}
	return ids, nil
}

func mergeGroupIDs(a []int64, b []int64) []int64 {
	seen := make(map[int64]struct{}, len(a)+len(b))
	out := make([]int64, 0, len(a)+len(b))
	for _, id := range a {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range b {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// buildSchedulerGroupPayload 构造 EventAccountChanged / EventAccountGroupsChanged
// 事件的 payload。空 groupIDs 必须返回 untyped nil（any 而非 map[string]any(nil)），
// 否则 enqueueSchedulerOutbox 的 "payload != nil" 接口判空会被 typed-nil 欺骗，
// 把 payload marshal 成 "null" 写入 dedup_key 哈希，破坏与其他 nil-payload 调用的去重一致性。
func buildSchedulerGroupPayload(groupIDs []int64) any {
	if len(groupIDs) == 0 {
		return nil
	}
	return map[string]any{"group_ids": groupIDs}
}

func accountEntityToService(m *dbent.Account) (*service.Account, error) {
	if m == nil {
		return nil, nil
	}

	rateMultiplier := m.RateMultiplier

	// 读路径解密（A3-E2）：敏感子键遇 enc:v1: 前缀解密、无前缀原样返回。
	// 解密失败（密钥缺失/密文损坏）上抛，绝不把密文当明文放行。
	credentials, err := decryptCredentialsMap(m.Credentials)
	if err != nil {
		return nil, fmt.Errorf("account %d: %w", m.ID, err)
	}

	return &service.Account{
		ID:                      m.ID,
		Name:                    m.Name,
		Notes:                   m.Notes,
		Platform:                m.Platform,
		Type:                    m.Type,
		Credentials:             credentials,
		Extra:                   copyJSONMap(m.Extra),
		ProxyID:                 m.ProxyID,
		ProxyFallbackOriginID:   m.ProxyFallbackOriginID,
		Concurrency:             m.Concurrency,
		Priority:                m.Priority,
		RateMultiplier:          &rateMultiplier,
		LoadFactor:              m.LoadFactor,
		Status:                  m.Status,
		ErrorMessage:            derefString(m.ErrorMessage),
		LastUsedAt:              m.LastUsedAt,
		ExpiresAt:               m.ExpiresAt,
		AutoPauseOnExpired:      m.AutoPauseOnExpired,
		CreatedAt:               m.CreatedAt,
		UpdatedAt:               m.UpdatedAt,
		Schedulable:             m.Schedulable,
		RateLimitedAt:           m.RateLimitedAt,
		RateLimitResetAt:        m.RateLimitResetAt,
		OverloadUntil:           m.OverloadUntil,
		TempUnschedulableUntil:  m.TempUnschedulableUntil,
		TempUnschedulableReason: derefString(m.TempUnschedulableReason),
		SessionWindowStart:      m.SessionWindowStart,
		SessionWindowEnd:        m.SessionWindowEnd,
		SessionWindowStatus:     derefString(m.SessionWindowStatus),
		ParentAccountID:         m.ParentAccountID,
		QuotaDimension:          string(m.QuotaDimension),
	}, nil
}

func normalizeJSONMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	return in
}

func copyJSONMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func joinClauses(clauses []string, sep string) string {
	if len(clauses) == 0 {
		return ""
	}
	out := clauses[0]
	for i := 1; i < len(clauses); i++ {
		out += sep + clauses[i]
	}
	return out
}

func itoa(v int) string {
	return strconv.Itoa(v)
}

// FindByExtraField 根据 extra 字段中的键值对查找账号。
// 使用 PostgreSQL JSONB @> 操作符进行高效查询（需要 GIN 索引支持）。
//
// FindByExtraField finds accounts by key-value pairs in the extra field.
// Uses PostgreSQL JSONB @> operator for efficient queries (requires GIN index).
func (r *accountRepository) FindByExtraField(ctx context.Context, key string, value any) ([]service.Account, error) {
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.DeletedAtIsNil(),
			func(s *entsql.Selector) {
				path := sqljson.Path(key)
				switch v := value.(type) {
				case string:
					preds := []*entsql.Predicate{sqljson.ValueEQ(dbaccount.FieldExtra, v, path)}
					if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
						preds = append(preds, sqljson.ValueEQ(dbaccount.FieldExtra, parsed, path))
					}
					if len(preds) == 1 {
						s.Where(preds[0])
					} else {
						s.Where(entsql.Or(preds...))
					}
				case int:
					s.Where(entsql.Or(
						sqljson.ValueEQ(dbaccount.FieldExtra, v, path),
						sqljson.ValueEQ(dbaccount.FieldExtra, strconv.Itoa(v), path),
					))
				case int64:
					s.Where(entsql.Or(
						sqljson.ValueEQ(dbaccount.FieldExtra, v, path),
						sqljson.ValueEQ(dbaccount.FieldExtra, strconv.FormatInt(v, 10), path),
					))
				case json.Number:
					if parsed, err := v.Int64(); err == nil {
						s.Where(entsql.Or(
							sqljson.ValueEQ(dbaccount.FieldExtra, parsed, path),
							sqljson.ValueEQ(dbaccount.FieldExtra, v.String(), path),
						))
					} else {
						s.Where(sqljson.ValueEQ(dbaccount.FieldExtra, v.String(), path))
					}
				default:
					s.Where(sqljson.ValueEQ(dbaccount.FieldExtra, value, path))
				}
			},
		).
		All(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}

	return r.accountsToService(ctx, accounts)
}

// ListDueUpstreamBillingProbeAccounts bounds result hydration and network work
// to limit. PostgreSQL must still filter and order all enabled candidates;
// MATERIALIZED avoids repeating the defensive timestamp parse expression.
// Go writes next_probe_at via RFC3339Nano (up to 9 fractional digits) while
// jsonpath datetime() parses at most microseconds, so fractions beyond 6
// digits are trimmed first — mirroring ListDueOllamaCloudUsageAccounts.
// Without this, every nanosecond timestamp is treated as malformed and the
// fail-open ordering pins the cycle to the lowest account IDs, starving the
// rest of the pool.
func (r *accountRepository) ListDueUpstreamBillingProbeAccounts(ctx context.Context, now time.Time, limit int) ([]service.Account, error) {
	if limit <= 0 {
		return []service.Account{}, nil
	}
	if r.sql == nil {
		return nil, errors.New("account repository SQL executor not configured")
	}

	rows, err := r.sql.QueryContext(ctx, `
		WITH candidates AS (
			SELECT
				id,
				extra #>> '{upstream_billing_probe,status}' AS probe_status,
				extra #>> '{upstream_billing_probe,next_probe_at}' AS next_probe_at
			FROM accounts
			WHERE deleted_at IS NULL
				AND status = 'active'
				AND type = 'apikey'
				AND extra @> '{"upstream_billing_probe_enabled": true}'::jsonb
		), parsed AS MATERIALIZED (
			SELECT
				id,
				probe_status,
				next_probe_at,
				next_probe_at ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$' AS rfc3339_shape,
				jsonb_path_query_first_tz(
					jsonb_build_object(
						'value',
						replace(regexp_replace(regexp_replace(
							next_probe_at,
							'(\.[0-9]{6})[0-9]+(Z|[+-][0-9]{2}:[0-9]{2})$',
							'\1\2'
						), 'Z$', '+00:00'), 'T', ' ')
					),
					'$.value.datetime()',
					'{}'::jsonb,
					true
				) #>> '{}' AS parsed_next_probe_at
			FROM candidates
		), normalized AS (
			SELECT
				id,
				probe_status,
				next_probe_at,
				parsed_next_probe_at,
				rfc3339_shape AND parsed_next_probe_at IS NOT NULL AS valid_next_probe_at
			FROM parsed
		)
		SELECT id
		FROM normalized
		WHERE probe_status NOT IN ('ok', 'unsupported', 'failed')
			OR probe_status IS NULL
			OR next_probe_at IS NULL
			OR NOT valid_next_probe_at
			OR CASE WHEN valid_next_probe_at THEN parsed_next_probe_at::timestamptz <= $1 ELSE FALSE END
		ORDER BY
			CASE
				WHEN probe_status NOT IN ('ok', 'unsupported', 'failed')
					OR probe_status IS NULL
					OR next_probe_at IS NULL
					OR NOT valid_next_probe_at
				THEN 0
				ELSE 1
			END ASC,
			CASE WHEN valid_next_probe_at THEN parsed_next_probe_at::timestamptz END ASC NULLS FIRST,
			id ASC
		LIMIT $2
	`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []service.Account{}, nil
	}

	accounts, err := r.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]service.Account, 0, len(accounts))
	for _, account := range accounts {
		if account != nil {
			out = append(out, *account)
		}
	}
	return out, nil
}

// nowUTC is a SQL expression to generate a UTC RFC3339 timestamp string.
const nowUTC = `to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`

// dailyExpiredExpr is a SQL expression that evaluates to TRUE when daily quota period has expired.
// Supports both rolling (24h from start) and fixed (pre-computed reset_at) modes.
const dailyExpiredExpr = `(
	CASE WHEN COALESCE(extra->>'quota_daily_reset_mode', 'rolling') = 'fixed'
	THEN NOW() >= COALESCE((extra->>'quota_daily_reset_at')::timestamptz, '1970-01-01'::timestamptz)
	ELSE COALESCE((extra->>'quota_daily_start')::timestamptz, '1970-01-01'::timestamptz)
		+ '24 hours'::interval <= NOW()
	END
)`

// weeklyExpiredExpr is a SQL expression that evaluates to TRUE when weekly quota period has expired.
const weeklyExpiredExpr = `(
	CASE WHEN COALESCE(extra->>'quota_weekly_reset_mode', 'rolling') = 'fixed'
	THEN NOW() >= COALESCE((extra->>'quota_weekly_reset_at')::timestamptz, '1970-01-01'::timestamptz)
	ELSE COALESCE((extra->>'quota_weekly_start')::timestamptz, '1970-01-01'::timestamptz)
		+ '168 hours'::interval <= NOW()
	END
)`

// nextDailyResetAtExpr is a SQL expression to compute the next daily reset_at when a reset occurs.
// For fixed mode: computes the next future reset time based on NOW(), timezone, and configured hour.
// This correctly handles long-inactive accounts by jumping directly to the next valid reset point.
const nextDailyResetAtExpr = `(
	CASE WHEN COALESCE(extra->>'quota_daily_reset_mode', 'rolling') = 'fixed'
	THEN to_char((
		-- Compute today's reset point in the configured timezone, then pick next future one
		CASE WHEN NOW() >= (
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_daily_reset_hour')::int, 0) || ' hours')::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		-- NOW() is at or past today's reset point → next reset is tomorrow
		THEN (
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_daily_reset_hour')::int, 0) || ' hours')::interval
			+ '1 day'::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		-- NOW() is before today's reset point → next reset is today
		ELSE (
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_daily_reset_hour')::int, 0) || ' hours')::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		END
	) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	ELSE NULL END
)`

// nextWeeklyResetAtExpr is a SQL expression to compute the next weekly reset_at when a reset occurs.
// For fixed mode: computes the next future reset time based on NOW(), timezone, configured day and hour.
// This correctly handles long-inactive accounts by jumping directly to the next valid reset point.
const nextWeeklyResetAtExpr = `(
	CASE WHEN COALESCE(extra->>'quota_weekly_reset_mode', 'rolling') = 'fixed'
	THEN to_char((
		-- Compute this week's reset point in the configured timezone
		-- Step 1: get today's date at reset hour in configured tz
		-- Step 2: compute days forward to target weekday
		-- Step 3: if same day but past reset hour, advance 7 days
		CASE
		WHEN (
			-- days_forward = (target_day - current_day + 7) % 7
			(COALESCE((extra->>'quota_weekly_reset_day')::int, 1)
			 - EXTRACT(DOW FROM NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))::int
			 + 7) % 7
		) = 0 AND NOW() >= (
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_weekly_reset_hour')::int, 0) || ' hours')::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		-- Same weekday and past reset hour → next week
		THEN (
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_weekly_reset_hour')::int, 0) || ' hours')::interval
			+ '7 days'::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		ELSE (
			-- Advance to target weekday this week (or next if days_forward > 0)
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_weekly_reset_hour')::int, 0) || ' hours')::interval
			+ ((
				(COALESCE((extra->>'quota_weekly_reset_day')::int, 1)
				 - EXTRACT(DOW FROM NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))::int
				 + 7) % 7
			) || ' days')::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		END
	) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	ELSE NULL END
)`

// IncrementQuotaUsed 原子递增账号的配额用量（总/日/周三个维度）
// 日/周额度在周期过期时自动重置为 0 再递增。
// 支持滚动窗口（rolling）和固定时间（fixed）两种重置模式。
func (r *accountRepository) IncrementQuotaUsed(ctx context.Context, id int64, amount float64) error {
	rows, err := r.sql.QueryContext(ctx,
		`UPDATE accounts SET extra = (
			COALESCE(extra, '{}'::jsonb)
			-- 总额度：始终递增
			|| jsonb_build_object('quota_used', COALESCE((extra->>'quota_used')::numeric, 0) + $1)
			-- 日额度：仅在 quota_daily_limit > 0 时处理
			|| CASE WHEN COALESCE((extra->>'quota_daily_limit')::numeric, 0) > 0 THEN
				jsonb_build_object(
					'quota_daily_used',
					CASE WHEN `+dailyExpiredExpr+`
					THEN $1
					ELSE COALESCE((extra->>'quota_daily_used')::numeric, 0) + $1 END,
					'quota_daily_start',
					CASE WHEN `+dailyExpiredExpr+`
					THEN `+nowUTC+`
					ELSE COALESCE(extra->>'quota_daily_start', `+nowUTC+`) END
				)
				-- 固定模式重置时更新下次重置时间
				|| CASE WHEN `+dailyExpiredExpr+` AND `+nextDailyResetAtExpr+` IS NOT NULL
				   THEN jsonb_build_object('quota_daily_reset_at', `+nextDailyResetAtExpr+`)
				   ELSE '{}'::jsonb END
			ELSE '{}'::jsonb END
			-- 周额度：仅在 quota_weekly_limit > 0 时处理
			|| CASE WHEN COALESCE((extra->>'quota_weekly_limit')::numeric, 0) > 0 THEN
				jsonb_build_object(
					'quota_weekly_used',
					CASE WHEN `+weeklyExpiredExpr+`
					THEN $1
					ELSE COALESCE((extra->>'quota_weekly_used')::numeric, 0) + $1 END,
					'quota_weekly_start',
					CASE WHEN `+weeklyExpiredExpr+`
					THEN `+nowUTC+`
					ELSE COALESCE(extra->>'quota_weekly_start', `+nowUTC+`) END
				)
				-- 固定模式重置时更新下次重置时间
				|| CASE WHEN `+weeklyExpiredExpr+` AND `+nextWeeklyResetAtExpr+` IS NOT NULL
				   THEN jsonb_build_object('quota_weekly_reset_at', `+nextWeeklyResetAtExpr+`)
				   ELSE '{}'::jsonb END
			ELSE '{}'::jsonb END
		), updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
		RETURNING
			COALESCE((extra->>'quota_used')::numeric, 0),
			COALESCE((extra->>'quota_limit')::numeric, 0)`,
		amount, id)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var newUsed, limit float64
	if rows.Next() {
		if err := rows.Scan(&newUsed, &limit); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// 任一维度配额刚超限时触发调度快照刷新
	if limit > 0 && newUsed >= limit && (newUsed-amount) < limit {
		if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
			logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue quota exceeded failed: account=%d err=%v", id, err)
		}
	}
	return nil
}

// ResetQuotaUsedAndClearRateLimitCooldown resets all quota dimensions and the
// account-level cooldown in one statement. Other scheduler blocking state is preserved.
func (r *accountRepository) ResetQuotaUsedAndClearRateLimitCooldown(ctx context.Context, id int64) error {
	result, err := r.sql.ExecContext(ctx,
		`UPDATE accounts SET extra = (
			COALESCE(extra, '{}'::jsonb)
			|| '{"quota_used": 0, "quota_daily_used": 0, "quota_weekly_used": 0}'::jsonb
		) - 'quota_daily_start' - 'quota_weekly_start' - 'quota_daily_reset_at' - 'quota_weekly_reset_at',
		rate_limited_at = NULL, rate_limit_reset_at = NULL, updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`,
		id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAccountNotFound
	}
	// 重置配额后触发调度快照刷新，使账号重新参与调度
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue quota reset failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

// RevertProxyFallback 将账号的 proxy_id 切回 proxy_fallback_origin_id，并清空 origin 字段。
// 仅当 proxy_fallback_origin_id IS NOT NULL 时执行更新；
// 若影响行数为 0，则返回 ErrAccountNotInFallback（账号存在但不在 fallback 状态）。
func (r *accountRepository) RevertProxyFallback(ctx context.Context, accountID int64) error {
	res, err := r.sql.ExecContext(ctx, `
		UPDATE accounts SET proxy_id=proxy_fallback_origin_id, proxy_fallback_origin_id=NULL, updated_at=NOW()
		WHERE id=$1 AND proxy_fallback_origin_id IS NOT NULL AND deleted_at IS NULL`, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return service.ErrAccountNotInFallback
	}
	if err := enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] revert fallback enqueue failed: account=%d err=%v", accountID, err)
	}
	return nil
}

// ListShadowsByParent 返回指定父账号的影子账号；覆盖 quota_dimension='spark'（OpenAI OAuth
// 母账号的 spark 影子）与 quota_dimension='codebuddy'（CodeBuddy OAuth 母账号的一母多影）。
// 同时过滤 parent_account_id 与「dimension 为 spark 或 codebuddy」，防止其它 linked 维度被误伤。
// ⚠️ 新增影子维度（非空且非 spark/codebuddy）时：须更新此函数的 Or 分支，否则会静默漏掉新维度。
// 软删除行由 SoftDeleteMixin 拦截器自动排除，无需手写 deleted_at IS NULL。
func (r *accountRepository) ListShadowsByParent(ctx context.Context, parentID int64) ([]*service.Account, error) {
	rows, err := r.client.Account.Query().
		Where(
			dbaccount.ParentAccountIDEQ(parentID),
			dbaccount.Or(
				dbaccount.QuotaDimensionEQ(dbaccount.QuotaDimensionSpark),
				dbaccount.QuotaDimensionEQ(dbaccount.QuotaDimensionCodebuddy),
			),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*service.Account, 0, len(rows))
	ids := make([]int64, 0, len(rows))
	for _, m := range rows {
		account, err := accountEntityToService(m)
		if err != nil {
			return nil, err
		}
		ids = append(ids, account.ID)
		out = append(out, account)
	}
	// 影子行补分组绑定（accountEntityToService 不加载 edge）；向导需要 group_ids 做已建分组比对。
	_, groupIDsByAccount, _, err := r.loadAccountGroups(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, account := range out {
		if groupIDs, ok := groupIDsByAccount[account.ID]; ok {
			account.GroupIDs = groupIDs
		}
	}
	return out, nil
}
