package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// targetShareAI 是 ShareAI 上游每码目标账号数（1 主 1 热备）。
// 直接引用方案文档裁定值，避免硬编码漂移。
const targetShareAI = service.CursorAutoPullAccountsPerKey // = 2

// targetKNCy 是 kn_cy 上游每码目标账号数（仅 1）。
const targetKNCy = 1

// quotaKey 是 quota_keys 表中未软删的激活码键。
type quotaKey struct {
	id          int64
	upstreamKey string
}

// retentionRow 是单个候选收敛账号（含最近成功时间，可能为 nil）。
type retentionRow struct {
	id          int64
	name        string
	createdAt   time.Time
	lastSuccess *time.Time
}

func main() {
	apply := flag.Bool("apply", false,
		"apply deletions to the database (default: dry-run, prints the retention plan only)")
	flag.Parse()

	cfg, err := config.LoadForBootstrap()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	client, db, err := repository.InitEnt(cfg)
	if err != nil {
		log.Fatalf("initialize database: %v", err)
	}
	defer func() { _ = client.Close() }()

	// nil scheduler cache：Delete 内部对 nil 安全（仅跳过缓存清理）。
	repo := repository.NewAccountRepository(client, db, nil)

	ctx := context.Background()

	keys, err := loadQuotaKeys(ctx, db)
	if err != nil {
		log.Fatalf("load quota keys: %v", err)
	}

	unknown := false
	for _, k := range keys {
		target, ok := targetForUpstream(k.upstreamKey)
		if !ok {
			fmt.Printf("key_id=%d upstream_key=%q status=unknown_upstream action=skip\n", k.id, k.upstreamKey)
			unknown = true
			continue
		}
		if err := processKey(ctx, db, repo, k.id, target, *apply); err != nil {
			log.Fatalf("process key %d failed: %v", k.id, err)
		}
	}

	// 遇到未知上游：失败关闭（不猜），即使其它键已正常处理也以非 0 退出。
	if unknown {
		fmt.Println("unknown_upstream encountered; fail closed")
		os.Exit(1)
	}
}

// targetForUpstream 把 upstream_key 映射到每码目标账号数。
// 返回 ok=false 表示未知/空上游：调用方必须失败关闭，不猜测目标值。
func targetForUpstream(upstreamKey string) (int64, bool) {
	switch upstreamKey {
	case "shareai":
		return targetShareAI, true
	case "kn_cy":
		return targetKNCy, true
	default:
		return 0, false
	}
}

// loadQuotaKeys 读取未软删的激活码键（id, upstream_key）。
func loadQuotaKeys(ctx context.Context, db *sql.DB) ([]quotaKey, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, upstream_key
		FROM quota_keys
		WHERE deleted_at IS NULL
		ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var keys []quotaKey
	for rows.Next() {
		var k quotaKey
		if err := rows.Scan(&k.id, &k.upstreamKey); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

// loadAccounts 读取该激活码下未软删的 cursor 平台账号（id, name, created_at）。
func loadAccounts(ctx context.Context, db *sql.DB, quotaKeyID int64) ([]retentionRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, created_at
		FROM accounts
		WHERE platform = 'cursor'
		  AND deleted_at IS NULL
		  AND quota_key_id = $1
		ORDER BY id ASC`, quotaKeyID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var accounts []retentionRow
	for rows.Next() {
		var r retentionRow
		if err := rows.Scan(&r.id, &r.name, &r.createdAt); err != nil {
			return nil, err
		}
		accounts = append(accounts, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return accounts, nil
}

// loadLastSuccess 取每个账号的最近一次成功时间（usage_logs 的最大 created_at）。
// 无记录时映射为 nil（NULLS LAST 排末尾）。
func loadLastSuccess(ctx context.Context, db *sql.DB, ids []int64) (map[int64]*time.Time, error) {
	result := make(map[int64]*time.Time, len(ids))
	if len(ids) == 0 {
		return result, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT account_id, max(created_at)
		FROM usage_logs
		WHERE account_id = ANY($1)
		GROUP BY account_id`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id int64
		var nt sql.NullTime
		if err := rows.Scan(&id, &nt); err != nil {
			return nil, err
		}
		if nt.Valid {
			t := nt.Time
			result[id] = &t
		} else {
			result[id] = nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// processKey 对单个激活码执行收敛：选出保留集、打印计划，--apply 时逐条删除。
func processKey(ctx context.Context, db *sql.DB, repo service.AccountRepository, quotaKeyID, target int64, apply bool) error {
	accounts, err := loadAccounts(ctx, db, quotaKeyID)
	if err != nil {
		return fmt.Errorf("load accounts: %w", err)
	}

	ids := make([]int64, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.id)
	}
	lastSuccess, err := loadLastSuccess(ctx, db, ids)
	if err != nil {
		return fmt.Errorf("load last success: %w", err)
	}
	for i := range accounts {
		accounts[i].lastSuccess = lastSuccess[accounts[i].id]
	}

	keep, drop := selectRetention(accounts, int(target))

	// 表头 + 每行计划（keep 在前，drop 在后，组内均已排序）。
	fmt.Println("key_id | account_id | name | last_success | action(keep/drop)")
	for _, r := range keep {
		fmt.Printf("%d | %d | %s | %s | keep\n", quotaKeyID, r.id, r.name, formatLastSuccess(r.lastSuccess))
	}
	for _, r := range drop {
		fmt.Printf("%d | %d | %s | %s | drop\n", quotaKeyID, r.id, r.name, formatLastSuccess(r.lastSuccess))
	}
	fmt.Printf("key_id=%d before=%d keep=%d drop=%d\n", quotaKeyID, len(accounts), len(keep), len(drop))

	if apply {
		for _, r := range drop {
			if err := repo.Delete(ctx, r.id); err != nil {
				fmt.Printf("deleted id=%d err=%v\n", r.id, err)
				os.Exit(1)
			}
			fmt.Printf("deleted id=%d err=<nil>\n", r.id)
		}
	}
	return nil
}

// formatLastSuccess 把最近成功时间格式化为 RFC3339；nil 输出空串。
func formatLastSuccess(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// selectRetention 是收敛排序的纯函数：
// 排序键 = last_success DESC NULLS LAST → created_at DESC → id DESC；
// 前 target 个保留，其余删除。target 超出账号数时全部保留。
func selectRetention(rows []retentionRow, target int) (keep, drop []retentionRow) {
	sorted := make([]retentionRow, len(rows))
	copy(sorted, rows)

	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		// last_success DESC，NULLS LAST。
		switch {
		case a.lastSuccess != nil && b.lastSuccess != nil:
			if !a.lastSuccess.Equal(*b.lastSuccess) {
				return a.lastSuccess.After(*b.lastSuccess)
			}
		case a.lastSuccess != nil:
			return true // a 非 NULL 排在 b(NULL) 之前
		case b.lastSuccess != nil:
			return false // b 非 NULL 排在 a(NULL) 之前
		}
		// created_at DESC。
		if !a.createdAt.Equal(b.createdAt) {
			return a.createdAt.After(b.createdAt)
		}
		// id DESC。
		return a.id > b.id
	})

	if target < 0 {
		target = 0
	}
	if target > len(sorted) {
		target = len(sorted)
	}
	return sorted[:target], sorted[target:]
}
