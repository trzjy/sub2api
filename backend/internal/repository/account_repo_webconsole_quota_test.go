package repository

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// newWebConsoleQuotaRepo 构造 sqlmock 驱动的 accountRepository（与
// newTokenHarborSessionRepo 同款：schedulerCache 传 nil，使 syncSchedulerAccountSnapshot
// 提前返回，避免测试里出现额外的 GetByID / cache 写入查询）。
func newWebConsoleQuotaRepo(t *testing.T) (*accountRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	return newAccountRepositoryWithSQL(client, db, nil), mock
}

// patchPayloadMatcher 解析 credentials UPDATE 的第一个参数（加密后 JSONB 字符串），
// 把解密前的明文结构交给 check 断言（passthrough 模式下 storage 即明文）。
type patchPayloadMatcher struct {
	t     *testing.T
	check func(t *testing.T, creds map[string]any)
}

func (m patchPayloadMatcher) Match(v driver.Value) bool {
	raw, ok := v.(string)
	if !ok {
		if b, isB := v.([]byte); isB {
			raw = string(b)
		} else {
			return false
		}
	}
	var creds map[string]any
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		m.t.Errorf("patch payload is not valid JSON: %v (raw=%s)", err, raw)
		return false
	}
	m.check(m.t, creds)
	return true
}

// (a) Patch 只覆盖 patch 键且保留其他键。
func TestPatchAccountCredentialsMergesOnlyPatchKeys(t *testing.T) {
	repo, mock := newWebConsoleQuotaRepo(t)

	existing := `{"api_key":"sk-old","console_username":"alice","other":"keep"}`
	patch := map[string]any{
		"console_storage_state": "STATE",
		"console_password":      "pw-new",
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT credentials FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"credentials"}).AddRow([]byte(existing)))
	mock.ExpectExec(`(?s)UPDATE accounts SET credentials = \$1::jsonb.*WHERE id = \$4 AND deleted_at IS NULL`).
		WithArgs(
			patchPayloadMatcher{t: t, check: func(t *testing.T, creds map[string]any) {
				require.Equal(t, "sk-old", creds["api_key"], "未 patch 的键必须保留")
				require.Equal(t, "alice", creds["console_username"], "未 patch 的键必须保留")
				require.Equal(t, "keep", creds["other"], "未 patch 的键必须保留")
				require.Equal(t, "STATE", creds["console_storage_state"], "patch 键必须写入")
				require.Equal(t, "pw-new", creds["console_password"], "patch 键必须写入")
			}},
			sqlmock.AnyArg(), // credentials_mac
			sqlmock.AnyArg(), // credentials_api_key_mac
			int64(42),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := repo.PatchAccountCredentials(context.Background(), 42, patch, "")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// (b) expectedUsername 不匹配（库内 console_username 已是别的非空值）时跳过
// console_storage_state 写入，但其余 patch 键照常写入。
func TestPatchAccountCredentialsSkipsStorageStateOnUsernameMismatch(t *testing.T) {
	repo, mock := newWebConsoleQuotaRepo(t)

	existing := `{"console_username":"alice","console_storage_state":"OLD","other":"x"}`
	patch := map[string]any{
		"console_storage_state": "NEW",
		"console_password":      "pw",
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT credentials FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"credentials"}).AddRow([]byte(existing)))
	mock.ExpectExec(`(?s)UPDATE accounts SET credentials = \$1::jsonb.*WHERE id = \$4 AND deleted_at IS NULL`).
		WithArgs(
			patchPayloadMatcher{t: t, check: func(t *testing.T, creds map[string]any) {
				require.Equal(t, "OLD", creds["console_storage_state"], "username 不匹配时必须跳过 storage_state 写入，保留库内值")
				require.Equal(t, "pw", creds["console_password"], "其余 patch 键照常写入")
				require.Equal(t, "alice", creds["console_username"])
				require.Equal(t, "x", creds["other"])
			}},
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			int64(7),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := repo.PatchAccountCredentials(context.Background(), 7, patch, "bob")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// (b+ 反向) expectedUsername 与库内一致时不跳过 storage_state 写入。
func TestPatchAccountCredentialsWritesStorageStateOnUsernameMatch(t *testing.T) {
	repo, mock := newWebConsoleQuotaRepo(t)

	existing := `{"console_username":"alice","console_storage_state":"OLD"}`
	patch := map[string]any{"console_storage_state": "NEW"}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT credentials FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(int64(8)).
		WillReturnRows(sqlmock.NewRows([]string{"credentials"}).AddRow([]byte(existing)))
	mock.ExpectExec(`(?s)UPDATE accounts SET credentials = \$1::jsonb.*WHERE id = \$4 AND deleted_at IS NULL`).
		WithArgs(
			patchPayloadMatcher{t: t, check: func(t *testing.T, creds map[string]any) {
				require.Equal(t, "NEW", creds["console_storage_state"], "username 匹配时必须写入 storage_state")
			}},
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			int64(8),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := repo.PatchAccountCredentials(context.Background(), 8, patch, "alice")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 凭据变化不涉及调度状态：Patch 成功时不发送 scheduler outbox（仅 BEGIN/SELECT/UPDATE/COMMIT）。
func TestPatchAccountCredentialsSendsNoOutbox(t *testing.T) {
	repo, mock := newWebConsoleQuotaRepo(t)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT credentials FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE")).
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"credentials"}).AddRow([]byte(`{"a":"1"}`)))
	mock.ExpectExec(`(?s)UPDATE accounts SET credentials = \$1::jsonb.*WHERE id = \$4 AND deleted_at IS NULL`).
		WithArgs(patchPayloadMatcher{t: t, check: func(t *testing.T, creds map[string]any) {
			require.Equal(t, "1", creds["a"])
			require.Equal(t, "2", creds["b"])
		}}, sqlmock.AnyArg(), sqlmock.AnyArg(), int64(9)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := repo.PatchAccountCredentials(context.Background(), 9, map[string]any{"b": "2"}, "")
	require.NoError(t, err)
	// 没有任何 INSERT INTO scheduler_outbox 期望：若方法误发 outbox，ExpectationsWereMet 会失败。
	require.NoError(t, mock.ExpectationsWereMet())
}

// (c) 他因活跃停调（未过期且来源不同）零写入，applied=false，绝不覆盖、不写 outbox。
func TestSetTempUnschedulableIfOwnedOrUnpausedSkipsForeignActive(t *testing.T) {
	repo, mock := newWebConsoleQuotaRepo(t)

	until := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	reason := `{"source":"web_console_quota","msg":"exhausted"}`
	// 模拟 WHERE 失配（他因来源、未过期）：UPDATE 影响 0 行。
	mock.ExpectExec(`(?s)UPDATE accounts SET temp_unschedulable_until = \$1.*WHERE id = \$3 AND deleted_at IS NULL`).
		WithArgs(sqlmock.AnyArg(), reason, int64(10), `%`+"\"source\":\"web_console_quota\""+`%`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	applied, err := repo.SetTempUnschedulableIfOwnedOrUnpaused(context.Background(), 10, until, reason, "web_console_quota")
	require.NoError(t, err)
	require.False(t, applied, "他因活跃停调必须 applied=false")
	// 影响 0 行 → 不发 outbox、不同步快照（cache 为 nil 也无 DB 调用）。
	require.NoError(t, mock.ExpectationsWereMet())
}

// (d) 同源延长 / 缩短均 applied=true（WHERE 命中，影响 1 行）。
func TestSetTempUnschedulableIfOwnedOrUnpausedAppliesForOwnSource(t *testing.T) {
	repo, mock := newWebConsoleQuotaRepo(t)

	until := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	reason := `{"source":"web_console_quota","msg":"exhausted"}`

	for _, id := range []int64{11, 12} {
		mock.ExpectExec(`(?s)UPDATE accounts SET temp_unschedulable_until = \$1.*WHERE id = \$3 AND deleted_at IS NULL`).
			WithArgs(sqlmock.AnyArg(), reason, id, `%`+"\"source\":\"web_console_quota\""+`%`).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}

	// 同源延长。
	applied, err := repo.SetTempUnschedulableIfOwnedOrUnpaused(context.Background(), 11, until, reason, "web_console_quota")
	require.NoError(t, err)
	require.True(t, applied)

	// 同源缩短（until 提前）同样命中。
	shorter := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	applied, err = repo.SetTempUnschedulableIfOwnedOrUnpaused(context.Background(), 12, shorter, reason, "web_console_quota")
	require.NoError(t, err)
	require.True(t, applied)

	require.NoError(t, mock.ExpectationsWereMet())
}

// (e) Clear...IfOwned 在 until 或来源不匹配时 cleared=false（影响 0 行）。
func TestClearTempUnschedulableIfOwnedMismatchClearedFalse(t *testing.T) {
	repo, mock := newWebConsoleQuotaRepo(t)

	until := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	source := "web_console_quota"

	// until 不匹配 / 来源不匹配：两者都令 WHERE 失配 → 0 行。
	// 第 1 次用 web_console_quota 来源 + 命中 until；第 2 次用 other_source
	// 来源（pattern 由其派生）。两次均预期 0 行 → cleared=false。
	mock.ExpectExec(`(?s)UPDATE accounts SET temp_unschedulable_until = NULL.*WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs(int64(20), until, `%`+"\"source\":\""+source+"\""+`%`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)UPDATE accounts SET temp_unschedulable_until = NULL.*WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs(int64(20), time.Time{}, `%`+"\"source\":\"other_source\""+`%`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	cleared, err := repo.ClearTempUnschedulableIfOwned(context.Background(), 20, until, source)
	require.NoError(t, err)
	require.False(t, cleared, "until 不匹配必须 cleared=false")

	cleared, err = repo.ClearTempUnschedulableIfOwned(context.Background(), 20, time.Time{}, "other_source")
	require.NoError(t, err)
	require.False(t, cleared, "来源不匹配必须 cleared=false")

	require.NoError(t, mock.ExpectationsWereMet())
}

// (e+ 反向) until 与来源均匹配 → cleared=true 并写 outbox。
func TestClearTempUnschedulableIfOwnedMatchesClearedTrue(t *testing.T) {
	repo, mock := newWebConsoleQuotaRepo(t)

	until := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`(?s)UPDATE accounts SET temp_unschedulable_until = NULL.*WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs(int64(21), until, `%`+"\"source\":\"web_console_quota\""+`%`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WillReturnResult(sqlmock.NewResult(1, 1))

	cleared, err := repo.ClearTempUnschedulableIfOwned(context.Background(), 21, until, "web_console_quota")
	require.NoError(t, err)
	require.True(t, cleared)
	require.NoError(t, mock.ExpectationsWereMet())
}

// (f) Set / Clear 成功时均发 outbox（applied/cleared==true 分支）。
func TestSetAndClearSendOutboxOnSuccess(t *testing.T) {
	repo, mock := newWebConsoleQuotaRepo(t)

	reason := `{"source":"web_console_quota","msg":"x"}`
	until := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)

	// Set 成功。
	mock.ExpectExec(`(?s)UPDATE accounts SET temp_unschedulable_until = \$1.*WHERE id = \$3 AND deleted_at IS NULL`).
		WithArgs(sqlmock.AnyArg(), reason, int64(30), `%`+"\"source\":\"web_console_quota\""+`%`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WillReturnResult(sqlmock.NewResult(1, 1))

	applied, err := repo.SetTempUnschedulableIfOwnedOrUnpaused(context.Background(), 30, until, reason, "web_console_quota")
	require.NoError(t, err)
	require.True(t, applied)

	// Clear 成功。
	mock.ExpectExec(`(?s)UPDATE accounts SET temp_unschedulable_until = NULL.*WHERE id = \$1 AND deleted_at IS NULL`).
		WithArgs(int64(30), until, `%`+"\"source\":\"web_console_quota\""+`%`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WillReturnResult(sqlmock.NewResult(1, 1))

	cleared, err := repo.ClearTempUnschedulableIfOwned(context.Background(), 30, until, "web_console_quota")
	require.NoError(t, err)
	require.True(t, cleared)

	require.NoError(t, mock.ExpectationsWereMet())
}
