package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
)

// 凭证存量迁移（docs/security-ban-prevention-plan.md A3-E3）：
// 把 accounts.credentials 中明文形态的敏感子键批量加密（enc:v1: 前缀），
// 并为所有行回填 credentials_mac / credentials_api_key_mac 指纹列（迁移 246
// 增加）。任务幂等：已加密且指纹一致的行跳过，可安全重跑。
//
// 以独立命令（cmd/credmigrate）执行，不经 cleanup 体系自动挂载——加密密钥
// 的启用与存量重写应由运维显式操作。主密钥未配置时调用方必须拒绝执行。

// CredentialMigrationStats 是一次存量迁移的统计结果。
type CredentialMigrationStats struct {
	// Scanned 扫描的总行数。
	Scanned int
	// EncryptedRows 本次发生敏感子键加密的行数。
	EncryptedRows int
	// MACBackfillRows 凭证未变化、仅回填指纹的行数。
	MACBackfillRows int
	// Unchanged 无需任何写入的行数。
	Unchanged int
}

type legacyCredentialRow struct {
	id          int64
	credentials []byte
	mac         sql.NullString
	apiKeyMAC   sql.NullString
}

// MigrateLegacyCredentials 扫描全部未删除账号，加密明文敏感子键并回填指纹。
// batchSize 是每批读取的行数（<=0 时取 500）。progress 每批回调一次，可为 nil。
func MigrateLegacyCredentials(
	ctx context.Context,
	db sqlExecutor,
	batchSize int,
	includeDeleted bool,
	progress func(stats *CredentialMigrationStats),
) (*CredentialMigrationStats, error) {
	if db == nil {
		return nil, fmt.Errorf("credential migration: db is nil")
	}
	if !credcrypt.Enabled() {
		return nil, fmt.Errorf("credential migration: %s is not configured; refusing to run", credcrypt.EnvKey)
	}
	if batchSize <= 0 {
		batchSize = 500
	}
	stats := &CredentialMigrationStats{}
	lastID := int64(0)
	// includeDeleted=true 时一并处理软删除行：它们虽不参与调度，但 DB 泄漏/备份场景
	// 下仍会暴露明文凭证（reachable 的敏感键），必须与存活行同等收敛。
	deletedFilter := "deleted_at IS NULL"
	if includeDeleted {
		deletedFilter = "TRUE"
	}
	for {
		rows, err := db.QueryContext(ctx, `
			SELECT id, credentials, credentials_mac, credentials_api_key_mac
			FROM accounts
			WHERE `+deletedFilter+` AND id > $1
			ORDER BY id
			LIMIT $2
		`, lastID, batchSize)
		if err != nil {
			return nil, fmt.Errorf("credential migration: query batch: %w", err)
		}
		batch := make([]legacyCredentialRow, 0, batchSize)
		for rows.Next() {
			var row legacyCredentialRow
			if err := rows.Scan(&row.id, &row.credentials, &row.mac, &row.apiKeyMAC); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("credential migration: scan: %w", err)
			}
			batch = append(batch, row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("credential migration: iterate: %w", err)
		}
		_ = rows.Close()
		if len(batch) == 0 {
			break
		}

		for _, row := range batch {
			lastID = row.id
			stats.Scanned++
			changed, err := migrateOneLegacyCredentialRow(ctx, db, row)
			if err != nil {
				return nil, fmt.Errorf("credential migration: account %d: %w", row.id, err)
			}
			switch changed {
			case migrationRowEncrypted:
				stats.EncryptedRows++
			case migrationRowMACBackfilled:
				stats.MACBackfillRows++
			default:
				stats.Unchanged++
			}
		}
		if progress != nil {
			progress(stats)
		}
		if len(batch) < batchSize {
			break
		}
	}
	return stats, nil
}

type migrationRowChange int

const (
	migrationRowUnchanged migrationRowChange = iota
	migrationRowEncrypted
	migrationRowMACBackfilled
)

func migrateOneLegacyCredentialRow(ctx context.Context, db sqlExecutor, row legacyCredentialRow) (migrationRowChange, error) {
	var stored map[string]any
	if len(row.credentials) > 0 {
		if err := json.Unmarshal(row.credentials, &stored); err != nil {
			return migrationRowUnchanged, fmt.Errorf("unmarshal credentials: %w", err)
		}
	}
	// 先经读路径解密：指纹必须按明文计算，已加密行的密文直接算指纹会失配
	// （导致误改写）。解密同时兼容 legacy 明文行（原样返回）。
	credentials, err := decryptCredentialsMap(stored)
	if err != nil {
		return migrationRowUnchanged, err
	}
	if credentials == nil {
		credentials = map[string]any{}
	}

	// 目标形态：敏感子键加密 + 指纹回填。prepare 始终按明文计算指纹。
	prepared, err := prepareCredentialsForStorage(credentials)
	if err != nil {
		return migrationRowUnchanged, err
	}
	encryptedNow := false
	for _, key := range service.SensitiveCredentialKeys {
		if str, ok := stored[key].(string); ok && str != "" && !credcrypt.IsEncrypted(str) {
			encryptedNow = true
			break
		}
	}

	storedMAC := ""
	if row.mac.Valid {
		storedMAC = row.mac.String
	}
	storedAPIKeyMAC := ""
	if row.apiKeyMAC.Valid {
		storedAPIKeyMAC = row.apiKeyMAC.String
	}
	macMissing := prepared.mac != storedMAC || pointerValueOrEmpty(prepared.apiKeyMAC) != storedAPIKeyMAC

	if !encryptedNow && !macMissing {
		return migrationRowUnchanged, nil
	}

	payload, err := json.Marshal(prepared.storage)
	if err != nil {
		return migrationRowUnchanged, err
	}
	var apiKeyMAC any
	if prepared.apiKeyMAC != nil {
		apiKeyMAC = *prepared.apiKeyMAC
	}
	// 仅当确有加密发生时才重写 credentials；否则保持列不动，只补指纹，
	// 最小化对并发读写的扰动。
	if encryptedNow {
		result, err := db.ExecContext(ctx, `
			UPDATE accounts
			SET credentials = $1::jsonb,
				credentials_mac = $2,
				credentials_api_key_mac = $3,
				updated_at = NOW()
			WHERE id = $4 AND credentials IS NOT DISTINCT FROM CASE WHEN $5 = '' THEN '{}'::jsonb ELSE $5::jsonb END
		`, string(payload), prepared.mac, apiKeyMAC, row.id, string(row.credentials))
		if err != nil {
			return migrationRowUnchanged, err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return migrationRowUnchanged, fmt.Errorf("check update result: %w", err)
		} else if affected != 1 {
			return migrationRowUnchanged, fmt.Errorf("concurrent credentials change; refusing to overwrite")
		}
		return migrationRowEncrypted, nil
	}
	result, err := db.ExecContext(ctx, `
		UPDATE accounts
		SET credentials_mac = $1,
			credentials_api_key_mac = $2
		WHERE id = $3 AND credentials IS NOT DISTINCT FROM CASE WHEN $4 = '' THEN '{}'::jsonb ELSE $4::jsonb END
	`, prepared.mac, apiKeyMAC, row.id, string(row.credentials))
	if err != nil {
		return migrationRowUnchanged, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return migrationRowUnchanged, fmt.Errorf("check update result: %w", err)
	} else if affected != 1 {
		return migrationRowUnchanged, fmt.Errorf("concurrent credentials change; refusing to overwrite")
	}
	return migrationRowMACBackfilled, nil
}

func pointerValueOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// CountLegacyCredentials 是 dry-run 统计：按与 MigrateLegacyCredentials 相同的
// 判定扫描全表，报告各分类行数，但不执行任何写入。同样要求密钥已配置。
func CountLegacyCredentials(ctx context.Context, db sqlExecutor, includeDeleted bool) (*CredentialMigrationStats, error) {
	if db == nil {
		return nil, fmt.Errorf("credential migration: db is nil")
	}
	if !credcrypt.Enabled() {
		return nil, fmt.Errorf("credential migration: %s is not configured; refusing to run", credcrypt.EnvKey)
	}
	stats := &CredentialMigrationStats{}
	lastID := int64(0)
	deletedFilter := "deleted_at IS NULL"
	if includeDeleted {
		deletedFilter = "TRUE"
	}
	for {
		rows, err := db.QueryContext(ctx, `
			SELECT id, credentials, credentials_mac, credentials_api_key_mac
			FROM accounts
			WHERE `+deletedFilter+` AND id > $1
			ORDER BY id
			LIMIT 500
		`, lastID)
		if err != nil {
			return nil, fmt.Errorf("credential migration: query batch: %w", err)
		}
		batch := make([]legacyCredentialRow, 0, 500)
		for rows.Next() {
			var row legacyCredentialRow
			if err := rows.Scan(&row.id, &row.credentials, &row.mac, &row.apiKeyMAC); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("credential migration: scan: %w", err)
			}
			batch = append(batch, row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("credential migration: iterate: %w", err)
		}
		_ = rows.Close()
		if len(batch) == 0 {
			break
		}
		for _, row := range batch {
			lastID = row.id
			stats.Scanned++
			var stored map[string]any
			if len(row.credentials) > 0 {
				if err := json.Unmarshal(row.credentials, &stored); err != nil {
					return nil, fmt.Errorf("credential migration: account %d: unmarshal credentials: %w", row.id, err)
				}
			}
			credentials, err := decryptCredentialsMap(stored)
			if err != nil {
				return nil, fmt.Errorf("credential migration: account %d: %w", row.id, err)
			}
			if credentials == nil {
				credentials = map[string]any{}
			}
			prepared, err := prepareCredentialsForStorage(credentials)
			if err != nil {
				return nil, fmt.Errorf("credential migration: account %d: %w", row.id, err)
			}
			needEncrypt := false
			for _, key := range service.SensitiveCredentialKeys {
				if str, ok := credentials[key].(string); ok && str != "" && !credcrypt.IsEncrypted(str) {
					needEncrypt = true
					break
				}
			}
			macMissing := prepared.mac != nullStringValue(row.mac) ||
				pointerValueOrEmpty(prepared.apiKeyMAC) != nullStringValue(row.apiKeyMAC)
			switch {
			case needEncrypt:
				stats.EncryptedRows++
			case macMissing:
				stats.MACBackfillRows++
			default:
				stats.Unchanged++
			}
		}
		if len(batch) < 500 {
			break
		}
	}
	return stats, nil
}

func nullStringValue(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}
