package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRepository_SetTempUnschedulable_NoRowsAffectedDoesNotWriteOutbox(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	until := time.Now().Add(10 * time.Minute)

	err := repo.SetTempUnschedulable(context.Background(), 42, until, "retry")
	require.NoError(t, err)
	require.Len(t, exec.execQueries, 1)
	require.Contains(t, exec.execQueries[0], "UPDATE accounts")
	require.NotContains(t, strings.Join(exec.execQueries, "\n"), "scheduler_outbox")
}

func TestAccountRepository_ShortenTempUnschedulableIfOwned_SQLShapeAndOutbox(t *testing.T) {
	// F4 受控缩短原语（派发单 C1-a-r3 / 方案 §4 F4 R12-F1 消化）的落库形态。
	// 单条条件 UPDATE，无读-改-写两段式：仅当 until > newUntil 且 reason 以
	// ownedReasonPrefix 前缀开头时把 until 缩短至 newUntil。
	const prefix = "cn quota exhausted:"
	newUntil := time.Now().Add(5 * time.Minute)

	t.Run("no rows affected does not write outbox", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		shortened, err := repo.ShortenTempUnschedulableIfOwned(context.Background(), 42, newUntil, prefix)

		require.NoError(t, err)
		require.False(t, shortened, "无匹配行时必须返回 false（no-op）")
		require.Len(t, exec.execQueries, 1, "no-op 不应触发 outbox")
		normalized := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, normalized, "UPDATE accounts")
		require.Contains(t, normalized, "temp_unschedulable_until > $1",
			"必须仅在现有 until 严格大于 newUntil 时缩短（禁止延长）")
		require.Contains(t, normalized, "temp_unschedulable_reason LIKE $3 || '%'",
			"必须用前缀 LIKE 守卫归属，避免把 reason 拉到内存过滤")
		require.Contains(t, normalized, "temp_unschedulable_until = $1",
			"缩短仅改写 until 字段")
		require.NotContains(t, normalized, "temp_unschedulable_reason =",
			"缩短原语不得改写 reason 字段")
		require.Contains(t, normalized, "deleted_at IS NULL")
		require.Len(t, exec.execArgs[0], 3)
		require.Equal(t, newUntil, exec.execArgs[0][0])
		require.Equal(t, int64(42), exec.execArgs[0][1])
		require.Equal(t, prefix, exec.execArgs[0][2])
		require.NotContains(t, strings.Join(exec.execQueries, "\n"), "scheduler_outbox")
	})

	t.Run("shorten applied writes outbox", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		shortened, err := repo.ShortenTempUnschedulableIfOwned(context.Background(), 42, newUntil, prefix)

		require.NoError(t, err)
		require.True(t, shortened, "命中条件时必须返回 true（已缩短）")
		require.Len(t, exec.execQueries, 2,
			"缩短命中后必须触发 scheduler_outbox 传播")
		normalizedUpdate := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, normalizedUpdate, "UPDATE accounts")
		require.Contains(t, normalizedUpdate, "temp_unschedulable_until > $1")
		require.Contains(t, normalizedUpdate, "temp_unschedulable_reason LIKE $3 || '%'")
		require.Contains(t, normalizeSQLWhitespace(exec.execQueries[1]), "INSERT INTO scheduler_outbox")
	})
}

func TestAccountRepository_ResetQuotaUsedAndClearRateLimitCooldown_NoRowsAffectedReturnsNotFoundWithoutOutbox(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	err := repo.ResetQuotaUsedAndClearRateLimitCooldown(context.Background(), 42)

	require.ErrorIs(t, err, service.ErrAccountNotFound)
	require.Len(t, exec.execQueries, 1)
	require.Contains(t, exec.execQueries[0], "UPDATE accounts")
	require.NotContains(t, strings.Join(exec.execQueries, "\n"), "scheduler_outbox")
}

func TestAccountRepository_GrokCredentialConditionalMutationsAreEligibleAndAtomicallyPropagated(t *testing.T) {
	proxyID := int64(77)
	snapshot := service.GrokCredentialMutationSnapshot{
		CredentialsJSON: `{"access_token":"access","refresh_token":"refresh","_token_version":123}`,
		ProxyID:         &proxyID,
	}

	t.Run("permanent", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		updated, err := repo.SetGrokCredentialErrorIfMatch(context.Background(), 42, snapshot, "revoked")

		require.NoError(t, err)
		require.False(t, updated)
		require.Len(t, exec.execQueries, 1)
		normalized := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, normalized, "WITH updated AS ( UPDATE accounts AS a")
		require.Contains(t, normalized, "a.schedulable IS TRUE")
		require.Contains(t, normalized, "a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW()")
		require.Contains(t, normalized, "a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW()")
		require.Contains(t, normalized, "a.overload_until IS NULL OR a.overload_until <= NOW()")
		require.Contains(t, normalized, "a.credentials_mac = $7")
		require.Contains(t, normalized, "a.proxy_id IS NOT DISTINCT FROM $8")
		require.Contains(t, normalized, "NOT EXISTS ( SELECT 1 FROM proxies p")
		require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
		require.Len(t, exec.execArgs[0], 10)
		expectedMac, macErr := credentialsMACOfJSONString(snapshot.CredentialsJSON)
		require.NoError(t, macErr)
		require.Equal(t, expectedMac, exec.execArgs[0][6])
		require.Equal(t, &proxyID, exec.execArgs[0][7])
		require.Equal(t, string(service.GrokCredentialReasonProxyInvalid), exec.execArgs[0][8])
		require.Equal(t, service.SchedulerOutboxEventAccountChanged, exec.execArgs[0][9])
	})

	t.Run("transient", func(t *testing.T) {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
		repo := newAccountRepositoryWithSQL(nil, exec, nil)

		updated, err := repo.SetGrokCredentialTempUnschedulableIfMatch(
			context.Background(), 42, snapshot, time.Now().Add(time.Minute), "temporary",
		)

		require.NoError(t, err)
		require.False(t, updated)
		require.Len(t, exec.execQueries, 1)
		normalized := normalizeSQLWhitespace(exec.execQueries[0])
		require.Contains(t, normalized, "WITH updated AS ( UPDATE accounts AS a")
		require.Contains(t, normalized, "a.schedulable IS TRUE")
		require.Contains(t, normalized, "a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW()")
		require.Contains(t, normalized, "a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW()")
		require.Contains(t, normalized, "a.overload_until IS NULL OR a.overload_until <= NOW()")
		require.Contains(t, normalized, "a.credentials_mac = $7")
		require.Contains(t, normalized, "a.proxy_id IS NOT DISTINCT FROM $8")
		require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
		require.Len(t, exec.execArgs[0], 9)
		expectedMacT, macErrT := credentialsMACOfJSONString(snapshot.CredentialsJSON)
		require.NoError(t, macErrT)
		require.Equal(t, expectedMacT, exec.execArgs[0][6])
		require.Equal(t, &proxyID, exec.execArgs[0][7])
		require.Equal(t, service.SchedulerOutboxEventAccountChanged, exec.execArgs[0][8])
	})
}

func TestAccountRepository_GrokCredentialCommitCarriesOutboxAcrossCallerCancellation(t *testing.T) {
	snapshot := service.GrokCredentialMutationSnapshot{CredentialsJSON: `{"access_token":"access","refresh_token":"refresh"}`}
	tests := []struct {
		name   string
		mutate func(context.Context, *accountRepository) (bool, error)
	}{
		{
			name: "permanent",
			mutate: func(ctx context.Context, repo *accountRepository) (bool, error) {
				return repo.SetGrokCredentialErrorIfMatch(ctx, 42, snapshot, string(service.GrokCredentialReasonRevoked))
			},
		},
		{
			name: "transient",
			mutate: func(ctx context.Context, repo *accountRepository) (bool, error) {
				return repo.SetGrokCredentialTempUnschedulableIfMatch(ctx, 42, snapshot, time.Now().Add(time.Minute), string(service.GrokCredentialReasonRefreshTransient))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			exec := &recordingSQLExecutor{result: rowsAffectedResult(1), afterExec: cancel}
			repo := newAccountRepositoryWithSQL(nil, exec, nil)

			updated, err := tt.mutate(ctx, repo)

			require.NoError(t, err)
			require.True(t, updated)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.Len(t, exec.execQueries, 1, "state update and scheduler outbox must share one atomic SQL statement")
			require.Contains(t, normalizeSQLWhitespace(exec.execQueries[0]), "INSERT INTO scheduler_outbox")
		})
	}
}

func TestAccountRepository_SetGrokOAuthErrorIfCredentialsUnchanged_RequiresActiveExactCredentialMatch(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	applied, err := repo.SetGrokOAuthErrorIfCredentialsUnchanged(
		context.Background(),
		42,
		map[string]any{"access_token": "observed", "_token_version": int64(7)},
		"missing refresh token",
	)

	require.NoError(t, err)
	require.False(t, applied)
	require.Len(t, exec.execQueries, 1, "the account mutation and conditional outbox insert must be one statement")
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "WITH updated AS")
	require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
	require.Contains(t, normalized, "FROM updated")
	require.Contains(t, normalized, "platform = $4")
	require.Contains(t, normalized, "type = $5")
	require.Contains(t, normalized, "status = $6")
	require.Contains(t, normalized, "credentials_mac = $7")
	require.Contains(t, normalized, "NULLIF(BTRIM(a.credentials->>'refresh_token'), '') IS NULL")
	require.Len(t, exec.execArgs, 1)
	require.Equal(t, service.StatusActive, exec.execArgs[0][5])
	expectedObservedMac, err := credentialsMACOf(map[string]any{"access_token": "observed", "_token_version": int64(7)})
	require.NoError(t, err)
	require.Equal(t, expectedObservedMac, exec.execArgs[0][6])
}

func TestAccountRepository_SetGrokOAuthErrorIfCredentialsUnchanged_AppliedWritesOutbox(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	applied, err := repo.SetGrokOAuthErrorIfCredentialsUnchanged(
		context.Background(),
		42,
		map[string]any{"access_token": "observed"},
		"missing refresh token",
	)

	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "WITH updated AS")
	require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
	require.Contains(t, normalized, "SELECT $8, updated.id, NULL, NULL FROM updated")
}

func TestAccountRepository_SetGrokOAuthRefreshErrorIfCredentialsUnchanged_UsesAttemptCredentialsAndProxy(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	proxyID := int64(17)

	applied, err := repo.SetGrokOAuthRefreshErrorIfCredentialsUnchanged(
		context.Background(),
		42,
		map[string]any{"refresh_token": "attempted", "_token_version": int64(7)},
		&proxyID,
		"revoked",
	)

	require.NoError(t, err)
	require.False(t, applied)
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "credentials_mac = $7")
	require.Contains(t, normalized, "proxy_id IS NOT DISTINCT FROM $8")
	require.NotContains(t, normalized, "credentials->>'refresh_token'",
		"background invalid_grant CAS must accept the attempted refresh token; only reconciliation requires it missing")
	require.Equal(t, &proxyID, exec.execArgs[0][7])
	require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
	require.Len(t, exec.execArgs[0], 9)
}

func TestAccountRepository_SetGrokOAuthRefreshTempUnschedulableIfCredentialsUnchanged_UsesAttemptCredentialsAndProxy(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	proxyID := int64(19)

	applied, err := repo.SetGrokOAuthRefreshTempUnschedulableIfCredentialsUnchanged(
		context.Background(),
		42,
		map[string]any{"refresh_token": "attempted", "_token_version": int64(8)},
		&proxyID,
		time.Now().Add(10*time.Minute),
		"retry exhausted",
	)

	require.NoError(t, err)
	require.False(t, applied)
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "credentials_mac = $7")
	require.Contains(t, normalized, "proxy_id IS NOT DISTINCT FROM $8")
	require.Contains(t, normalized, "a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until < $1")
	require.Len(t, exec.execArgs[0], 9)
	require.Equal(t, &proxyID, exec.execArgs[0][7])
	require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
}

func TestAccountRepository_UpdateGrokOAuthCredentialsIfUnchanged_UsesExactAttemptStateAndAtomicOutbox(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	proxyID := int64(29)

	applied, err := repo.UpdateGrokOAuthCredentialsIfUnchanged(
		context.Background(),
		42,
		map[string]any{"refresh_token": "attempted", "_token_version": int64(9)},
		&proxyID,
		map[string]any{"refresh_token": "rotated", "_token_version": int64(10)},
	)

	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "WITH updated AS")
	require.Contains(t, normalized, "credentials = $1::jsonb")
	require.Contains(t, normalized, "credentials_mac = $5")
	require.Contains(t, normalized, "credentials_mac = $7")
	require.Contains(t, normalized, "credentials_api_key_mac = $8")
	require.Contains(t, normalized, "proxy_id IS NOT DISTINCT FROM $6")
	require.Contains(t, normalized, "INSERT INTO scheduler_outbox")
	require.Len(t, exec.execArgs[0], 9)
	require.Equal(t, &proxyID, exec.execArgs[0][5])
	expectedMacUpdate, err := credentialsMACOf(map[string]any{"refresh_token": "attempted", "_token_version": int64(9)})
	require.NoError(t, err)
	require.Equal(t, expectedMacUpdate, exec.execArgs[0][4])
}

func TestAccountRepository_ListOAuthRefreshCandidatePage_SQLFilter(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var capturedSQL string
	var capturedArgs []any
	mock.ExpectQuery("SELECT id").
		WillReturnRows(sqlmock.NewRows([]string{"id"})).
		WillDelayFor(0)

	repo := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db, captured: &capturedSQL, args: &capturedArgs}, nil)

	page, err := repo.ListOAuthRefreshCandidatePage(context.Background(), service.OAuthRefreshPageOptions{
		Platforms:            []string{service.PlatformAnthropic, service.PlatformOpenAI, service.PlatformGemini, service.PlatformAntigravity, service.PlatformGrok},
		AfterID:              100,
		Limit:                200,
		ActiveOnly:           true,
		IncludeSetupToken:    true,
		RequireRefreshToken:  true,
		ExcludeRetryCooldown: true,
	})
	require.NoError(t, err)
	require.Empty(t, page.Accounts)

	normalized := normalizeSQLWhitespace(capturedSQL)
	require.Contains(t, normalized, "deleted_at IS NULL")
	require.NotContains(t, normalized, "schedulable = TRUE",
		"paused (schedulable=false, status=active) accounts must remain OAuth refresh candidates: excluding them lets their stored access_token expire and the usage-window probe then reports a false 'needs re-auth'; permanent rejection is already covered by status = 'active' and refresh failures are bounded by the retry-cooldown exclusion")
	require.Contains(t, normalized, "status = 'active'")
	// setup-token 的 access_token 同为 8h 短期令牌，必须与 oauth 一起纳入后台刷新候选
	require.Contains(t, normalized, "type IN ('oauth', 'setup-token')")
	require.Contains(t, normalized, "platform = ANY($1)")
	require.NotContains(t, normalized, "platform IN ('anthropic'",
		"candidate platforms must come from the refresher registry instead of a second hard-coded list")
	require.Contains(t, normalized, "credentials ? 'refresh_token'")
	require.Contains(t, normalized, "btrim(credentials->>'refresh_token') <> ''")
	require.Contains(t, normalized, "temp_unschedulable_until > NOW()")
	require.Contains(t, normalized, "temp_unschedulable_reason LIKE 'token refresh retry exhausted:%'")
	require.Contains(t, normalized, "IS NOT TRUE",
		"must use IS NOT TRUE so accounts with NULL temp_unschedulable_until are not silently excluded by PG 3-valued logic")
	require.NotContains(t, normalized, "AND NOT (",
		"plain NOT (...) excludes NULL temp_unschedulable_until rows (the common healthy case)")
	require.Contains(t, normalized, "id > $2")
	require.Contains(t, normalized, "ORDER BY id ASC")
	require.Contains(t, normalized, "LIMIT $3")
	require.NotContains(t, normalized, "credentials->>'expires_at'")
	require.Len(t, capturedArgs, 3)
	require.Equal(t, int64(100), capturedArgs[1])
	require.Equal(t, 200, capturedArgs[2])
	valuer, ok := capturedArgs[0].(interface{ Value() (driver.Value, error) })
	require.True(t, ok)
	platforms, err := valuer.Value()
	require.NoError(t, err)
	require.Contains(t, platforms, service.PlatformGrok)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRepository_ListOAuthRefreshCandidatePage_ReconciliationExcludesAPIKeys(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	var capturedSQL string
	mock.ExpectQuery("SELECT id").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	repo := newAccountRepositoryWithSQL(nil, captureQuerySQL{db: db, captured: &capturedSQL}, nil)

	page, err := repo.ListOAuthRefreshCandidatePage(context.Background(), service.OAuthRefreshPageOptions{
		Platforms: []string{service.PlatformGrok},
		AfterID:   0,
		Limit:     50,
	})
	require.NoError(t, err)
	require.Empty(t, page.Accounts)

	normalized := normalizeSQLWhitespace(capturedSQL)
	require.Contains(t, normalized, "type = 'oauth'")
	require.NotContains(t, normalized, "type IN ('oauth', 'setup-token')")
	require.NotContains(t, normalized, "type = 'api-key'")
	require.NotContains(t, normalized, "credentials ? 'refresh_token'",
		"reconciliation must be able to find structurally invalid OAuth rows")
	require.Contains(t, normalized, "ORDER BY id ASC")
	require.NoError(t, mock.ExpectationsWereMet())
}

type captureQuerySQL struct {
	db       *sql.DB
	captured *string
	args     *[]any
}

func (c captureQuerySQL) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return c.db.ExecContext(ctx, query, args...)
}

func (c captureQuerySQL) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if c.captured != nil {
		*c.captured = query
	}
	if c.args != nil {
		*c.args = append([]any(nil), args...)
	}
	return c.db.QueryContext(ctx, query, args...)
}

func normalizeSQLWhitespace(sql string) string {
	return strings.Join(regexp.MustCompile(`\s+`).Split(strings.TrimSpace(sql), -1), " ")
}

type rowsAffectedResult int64

func (r rowsAffectedResult) LastInsertId() (int64, error) { return 0, nil }
func (r rowsAffectedResult) RowsAffected() (int64, error) { return int64(r), nil }

type recordingSQLExecutor struct {
	result      sql.Result
	err         error
	afterExec   func()
	execQueries []string
	execArgs    [][]any
}

func (e *recordingSQLExecutor) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	e.execQueries = append(e.execQueries, query)
	e.execArgs = append(e.execArgs, append([]any(nil), args...))
	if e.err != nil {
		return nil, e.err
	}
	if e.afterExec != nil {
		e.afterExec()
	}
	return e.result, nil
}

func (e *recordingSQLExecutor) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return nil, sql.ErrNoRows
}

// TestClearTempUnschedulableIfGeneration_RowsAffectedWritesOutbox 锁定仓储层条件清除原语
// （AC-G2R5 must_fix / AC-G2R6 子秒精度）：代际令牌命中（rows>0）时清除停调（until/reason 置
// NULL、自增 sched_state_revision），并如 ClearTempUnschedulable 同款写 scheduler_outbox 快照
// 事件，返回 true。SQL 形态必须含 `temp_unschedulable_until = $2` 条件守卫（timestamptz 全精度
// 等值，微秒级，不再 EXTRACT(EPOCH)::bigint 截断）——用截止时间做不可变代际令牌，单语句原子，
// 杜绝读-改-写两步的并发覆盖（见 account_repo.go ClearTempUnschedulableIfGeneration）。
func TestClearTempUnschedulableIfGeneration_RowsAffectedWritesOutbox(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	until := time.Date(2024, 5, 29, 12, 0, 0, 123456000, time.UTC)
	cleared, err := repo.ClearTempUnschedulableIfGeneration(context.Background(), 42, until)
	require.NoError(t, err)
	require.True(t, cleared, "命中条件时必须返回 true")

	require.Len(t, exec.execQueries, 2, "命中后应发 UPDATE + scheduler_outbox（与 ClearTempUnschedulable 同款）")
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "UPDATE accounts")
	require.Contains(t, normalized, "temp_unschedulable_until = NULL")
	require.Contains(t, normalized, "temp_unschedulable_reason = NULL")
	require.Contains(t, normalized, "WHERE id = $1 AND deleted_at IS NULL AND temp_unschedulable_until IS NOT NULL AND temp_unschedulable_until = $2",
		"必须用 until 截止时间做代际令牌条件守卫（timestamptz 全精度等值），避免读-改-写两步的并发覆盖")
	require.NotContains(t, normalized, "EXTRACT(EPOCH", "AC-G2R6：禁止 EXTRACT(EPOCH) 截断，必须全精度等值")
	require.Equal(t, int64(42), exec.execArgs[0][0])
	require.Equal(t, until, exec.execArgs[0][1])
	require.Contains(t, strings.Join(exec.execQueries, "\n"), "scheduler_outbox")
}

// TestClearTempUnschedulableIfGeneration_NoRowsAffectedDoesNotClearOrWriteOutbox 锁定代际失配
// （until 已被他人改写为更晚值，rows==0）时零写入、不写 outbox、返回 false——探测方据此保留新
// 停调、不清除他链刚写入的状态（见 account_health_recovery_probe_service.go 恢复分支）。
func TestClearTempUnschedulableIfGeneration_NoRowsAffectedDoesNotClearOrWriteOutbox(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	until := time.Date(2024, 5, 29, 12, 0, 0, 0, time.UTC)
	cleared, err := repo.ClearTempUnschedulableIfGeneration(context.Background(), 42, until)
	require.NoError(t, err)
	require.False(t, cleared, "代际失配（rows==0）时必须返回 false（no-op，保留新停调）")
	require.Len(t, exec.execQueries, 1, "未命中不得触发 outbox")
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "UPDATE accounts")
	require.Contains(t, normalized, "temp_unschedulable_until = $2")
	require.NotContains(t, normalized, "EXTRACT(EPOCH", "AC-G2R6：禁止 EXTRACT(EPOCH) 截断，必须全精度等值")
	require.NotContains(t, strings.Join(exec.execQueries, "\n"), "scheduler_outbox")
}

// TestClearTempUnschedulableIfGeneration_SubSecondPrecision 锁定 AC-G2R6 子秒精度修复：
// 代际令牌 until 携带微秒时，必须原样（timestamptz 全精度，微秒级）传入 SQL 的 $2，禁止
// EXTRACT(EPOCH)::bigint 截断到秒——否则「同一秒内的两个不同 until」会被误判为命中（同秒错清）。
func TestClearTempUnschedulableIfGeneration_SubSecondPrecision(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	// 带微秒的 until：123456000ns = 123.456ms。
	until := time.Date(2024, 5, 29, 12, 0, 0, 123456000, time.UTC)
	cleared, err := repo.ClearTempUnschedulableIfGeneration(context.Background(), 42, until)
	require.NoError(t, err)
	require.True(t, cleared, "命中条件时必须返回 true")

	require.Len(t, exec.execQueries, 2, "命中后应发 UPDATE + scheduler_outbox")
	// 全精度 time.Time 必须原样传入（微秒不得截断）：若被截断到秒，此处 time.Time 等值会失败。
	require.Equal(t, until, exec.execArgs[0][1], "全精度 time.Time 必须原样传入（微秒不得截断）")
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "temp_unschedulable_until = $2", "必须用全精度等值比较")
	require.NotContains(t, normalized, "EXTRACT(EPOCH", "AC-G2R6：禁止 EXTRACT(EPOCH) 截断")
}

// TestClearTempUnschedulableIfGeneration_GenerationMismatchSameSecondRetainsNewBlock 锁定
// AC-G2R6 子秒精度修复的「同秒不同 until 必失配」语义：本代令牌 until=12:00:00.123456；他因
// 同窗口写入了不同 until（12:00:00.654321）。仓储层以全精度 `= $2` 等值比较，必失配、rows==0、
// 返回 false，账号保持新停调（不清）。若仍用 EXTRACT(EPOCH)::bigint 截断到秒，二者会被误判为
// 命中（同秒错清），正是本修复要消除的漏洞。
func TestClearTempUnschedulableIfGeneration_GenerationMismatchSameSecondRetainsNewBlock(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	// 本代令牌（微秒级）；他因同窗口写入的不同 until 由 SQL 的 `= $2` 全精度比较判失配。
	until := time.Date(2024, 5, 29, 12, 0, 0, 123456000, time.UTC)
	cleared, err := repo.ClearTempUnschedulableIfGeneration(context.Background(), 42, until)
	require.NoError(t, err)
	require.False(t, cleared, "同秒不同微秒的 until 代际失配必须返回 false，保留新停调")
	require.Len(t, exec.execQueries, 1, "未命中不得触发 outbox")
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "temp_unschedulable_until = $2",
		"必须用全精度等值比较，使同秒不同微秒的 until 失配")
	require.NotContains(t, normalized, "EXTRACT(EPOCH", "AC-G2R6：禁止 EXTRACT(EPOCH) 截断，必须全精度等值")
	require.NotContains(t, strings.Join(exec.execQueries, "\n"), "scheduler_outbox")
}

// TestBumpTempUnschedulableReasonIfGeneration_Success 锁定 attempt 回写代际守卫原语
// （AC-G2R6 must_fix）：代际命中（until 仍等于令牌且停调窗口仍有效）时改写 reason 返回 true；
// SQL 形态须含 `temp_unschedulable_until = $2`、`temp_unschedulable_until > NOW()`、
// `deleted_at IS NULL`、`temp_unschedulable_reason = $3`，且全精度等值（无 EXTRACT(EPOCH)）。
func TestBumpTempUnschedulableReasonIfGeneration_Success(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	until := time.Date(2024, 5, 29, 12, 0, 0, 123456000, time.UTC)
	const newReason = `{"probe_attempts":2}`
	bumped, err := repo.BumpTempUnschedulableReasonIfGeneration(context.Background(), 42, until, newReason)
	require.NoError(t, err)
	require.True(t, bumped, "代际命中且停调窗口有效时必须返回 true")

	require.Len(t, exec.execQueries, 1, "reason 改写不改调度状态，不写 scheduler_outbox")
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "UPDATE accounts")
	require.Contains(t, normalized, "temp_unschedulable_reason = $3")
	require.Contains(t, normalized, "temp_unschedulable_until = $2")
	require.Contains(t, normalized, "temp_unschedulable_until > NOW()")
	require.Contains(t, normalized, "deleted_at IS NULL")
	require.NotContains(t, normalized, "EXTRACT(EPOCH", "AC-G2R6：全精度等值，禁止截断")
	require.Equal(t, until, exec.execArgs[0][1], "全精度 time.Time 必须原样传入（微秒不得截断）")
	require.Equal(t, newReason, exec.execArgs[0][2])
}

// TestBumpTempUnschedulableReasonIfGeneration_TakenOver 锁定代际失配（已被接管，必写更晚
// until）→ rows==0、返回 false，调用方据此终止本轮补探、保留接管者 reason。
func TestBumpTempUnschedulableReasonIfGeneration_TakenOver(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	until := time.Date(2024, 5, 29, 12, 0, 0, 0, time.UTC)
	bumped, err := repo.BumpTempUnschedulableReasonIfGeneration(context.Background(), 42, until, `{"probe_attempts":2}`)
	require.NoError(t, err)
	require.False(t, bumped, "代际失配（已被接管）必须返回 false，调用方据此终止本轮补探")
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, normalized, "temp_unschedulable_until = $2")
	require.Contains(t, normalized, "temp_unschedulable_until > NOW()")
	require.NotContains(t, normalized, "EXTRACT(EPOCH", "AC-G2R6：全精度等值，禁止截断")
}
