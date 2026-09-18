package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 平台归并重构 PR-2（docs/platform-merge-refactor-plan.md §5.8）：
// web-* 旧平台账号迁移的应用层 reconciler 写路径。三字段
// （platform / credentials["access_mode"] / extra["migrated_from_platform"]）
// 与 MAC、scheduler outbox 必须由本文件的 repository 层专用事务方法原子更新
// （锁定读取 → 幂等校验 → 写入 → outbox 入队 → 提交，单事务内完成），
// 禁止 service 层拼接多个普通 repo 调用模拟原子性（部分失败即三字段不一致，
// down 不可靠逆推）。

// migratedFromPlatformExtraKey 记录迁移前旧平台值，down 按它精确逆推。
const migratedFromPlatformExtraKey = "migrated_from_platform"

// accessModeCredentialKey 与 service 层 GetAccessMode 读取的键一致。
const accessModeCredentialKey = "access_mode"

// ListMigratedFromWebPlatformAccounts 返回已迁移（extra 带 migrated_from_platform 标记）
// 的账号，供 down 逆推与一致性核对。PR-4 旧链归零后不再按 platform 守卫（web-* 旧平台
// 已退役，down 严格按 migrated_from_platform 标记精确逆推，方案 §7）。
func (r *accountRepository) ListMigratedFromWebPlatformAccounts(ctx context.Context) ([]*service.Account, error) {
	ids, err := r.listMigrationCandidateIDs(ctx, `
		SELECT id
		FROM accounts
		WHERE extra ? $1
		  AND deleted_at IS NULL
		ORDER BY id
	`, migratedFromPlatformExtraKey)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return r.GetByIDs(ctx, ids)
}

func (r *accountRepository) listMigrationCandidateIDs(ctx context.Context, query string, args ...any) ([]int64, error) {
	rows, err := r.client.QueryContext(ctx, query, args...)
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
	return ids, rows.Err()
}

// MigrateAccountPlatform 把单个账号迁移到目标官方平台并显式化 access_mode=web
// （up，逐账号单事务）。PR-4 旧链归零后旧 web-* 平台已退役，本方法作为三字段原子写
// 原语保留（平台归并一致性 / 幂等 / 失败关闭的单测与集成验证仍依赖它）。
// 单事务内完成：platform → 官方值、credentials["access_mode"]="web"（经既有凭证
// 写路径重算 MAC）、extra["migrated_from_platform"]=旧平台值、scheduler outbox 入队。
// 幂等：已迁移（platform=官方值且标记存在）返回 (false, nil)；platform 与
// fromPlatform 不一致即失败关闭，绝不半写。
func (r *accountRepository) MigrateAccountPlatform(
	ctx context.Context,
	id int64,
	fromPlatform string,
	toPlatform string,
) (bool, error) {
	return r.migrateAccountPlatformRow(ctx, id, func(platform string, creds, extra map[string]any) (string, map[string]any, map[string]any, error) {
		if platform == toPlatform {
			if _, ok := extra[migratedFromPlatformExtraKey]; ok {
				// 已迁移：续跑幂等补完，不重复写。
				return "", nil, nil, nil
			}
			return "", nil, nil, fmt.Errorf("account %d migration inconsistent: platform already %q but %s missing",
				id, toPlatform, migratedFromPlatformExtraKey)
		}
		if platform != fromPlatform {
			return "", nil, nil, fmt.Errorf("account %d migration mismatch: expected platform %q, got %q",
				id, fromPlatform, platform)
		}
		if creds == nil {
			creds = map[string]any{}
		}
		creds[accessModeCredentialKey] = service.AccountAccessModeWeb
		extra = cloneExtraMap(extra)
		extra[migratedFromPlatformExtraKey] = fromPlatform
		return toPlatform, creds, extra, nil
	})
}

// RevertMigratedAccountPlatform 回滚单个已迁移账号（down，逐账号单事务）。
// 按 extra["migrated_from_platform"] 还原 platform，移除 access_mode 键与标记，
// 同一事务同一写路径（重算 MAC + scheduler outbox）。幂等：标记缺失即视为
// 已回滚，返回 (false, nil)。
func (r *accountRepository) RevertMigratedAccountPlatform(ctx context.Context, id int64) (bool, error) {
	return r.migrateAccountPlatformRow(ctx, id, func(platform string, creds, extra map[string]any) (string, map[string]any, map[string]any, error) {
		marker, ok := extra[migratedFromPlatformExtraKey].(string)
		if !ok || marker == "" {
			// 未迁移或已回滚：down 幂等 no-op。
			return "", nil, nil, nil
		}
		if creds == nil {
			creds = map[string]any{}
		}
		delete(creds, accessModeCredentialKey)
		extra = cloneExtraMap(extra)
		delete(extra, migratedFromPlatformExtraKey)
		return marker, creds, extra, nil
	})
}

// accountPlatformRowMutation 是迁移/回滚共用的行级变更决策：输入锁定的当前
// 行数据，输出目标 platform、目标 credentials、目标 extra；跳过时返回空
// platform（调用方按 (false, nil) 处理），失败时返回错误并回滚。
type accountPlatformRowMutation func(platform string, creds, extra map[string]any) (string, map[string]any, map[string]any, error)

// migrateAccountPlatformRow 逐账号单事务的原子变更主体：锁定读取 → 行级
// 决策 → 凭证写路径（加密 + 重算 MAC）→ 三字段写入 → outbox 入队 → 提交。
func (r *accountRepository) migrateAccountPlatformRow(ctx context.Context, id int64, mutate accountPlatformRowMutation) (bool, error) {
	contextTx := dbent.TxFromContext(ctx)
	client := r.client
	var tx *dbent.Tx
	if contextTx != nil {
		client = contextTx.Client()
	} else {
		var err error
		tx, err = r.client.Tx(ctx)
		if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
			return false, err
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}

	// 锁定读取（FOR NO KEY UPDATE），与写入同一事务。
	rows, err := client.QueryContext(ctx, `
		SELECT platform, credentials, COALESCE(extra, '{}'::jsonb)
		FROM accounts
		WHERE id = $1 AND deleted_at IS NULL
		FOR NO KEY UPDATE
	`, id)
	if err != nil {
		return false, err
	}
	if !rows.Next() {
		closeErr := rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
		if closeErr != nil {
			return false, closeErr
		}
		return false, service.ErrAccountNotFound
	}
	var currentPlatform string
	var credsRaw, extraRaw []byte
	if err := rows.Scan(&currentPlatform, &credsRaw, &extraRaw); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	creds, err := decodeStoredCredentials(credsRaw)
	if err != nil {
		return false, err
	}
	extra, err := decodeStoredMap(extraRaw)
	if err != nil {
		return false, fmt.Errorf("decode extra for account %d: %w", id, err)
	}

	nextPlatform, nextCreds, nextExtra, err := mutate(currentPlatform, creds, extra)
	if err != nil {
		return false, err
	}
	if nextPlatform == "" {
		// 幂等跳过：三字段保持一致，无需写。
		return false, nil
	}

	// 既有凭证写路径（A3-E2）：敏感子键加密 + 维护 credentials_mac /
	// credentials_api_key_mac（MAC 按明文计算）。
	prepared, err := prepareCredentialsForStorage(nextCreds)
	if err != nil {
		return false, err
	}
	extraJSON, err := json.Marshal(nextExtra)
	if err != nil {
		return false, err
	}
	storageJSON, err := json.Marshal(prepared.storage)
	if err != nil {
		return false, err
	}
	result, err := client.ExecContext(ctx, `
		UPDATE accounts
		SET platform = $1,
			credentials = $2::jsonb,
			credentials_mac = $3,
			credentials_api_key_mac = $4,
			extra = $5::jsonb,
			updated_at = NOW()
		WHERE id = $6 AND deleted_at IS NULL
	`, nextPlatform, string(storageJSON), prepared.mac, prepared.apiKeyMAC, string(extraJSON), id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, service.ErrAccountNotFound
	}
	if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		return false, err
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return false, err
		}
	}
	if contextTx == nil && tx != nil {
		// 提交后立即刷新单账号快照，网关在 outbox worker 延迟或异常时
		// 不会读到旧平台/旧凭证（与 updateAccount 同口径）。
		r.syncSchedulerAccountSnapshot(context.Background(), id)
	}
	return true, nil
}

func decodeStoredCredentials(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	storage := map[string]any{}
	if err := json.Unmarshal(raw, &storage); err != nil {
		return nil, fmt.Errorf("decode credentials: %w", err)
	}
	plain, err := decryptCredentialsMap(storage)
	if err != nil {
		return nil, err
	}
	if plain == nil {
		plain = map[string]any{}
	}
	return plain, nil
}

func decodeStoredMap(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func cloneExtraMap(extra map[string]any) map[string]any {
	out := make(map[string]any, len(extra)+1)
	for key, value := range extra {
		out[key] = value
	}
	return out
}
