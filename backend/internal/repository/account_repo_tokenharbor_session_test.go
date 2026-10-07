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
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// newTokenHarborSessionRepo 构造 sqlmock 驱动的 accountRepository（UpdateExtra
// 对非调度中性键走 tx + outbox，故需要 ent client 包同一 mock db）。
func newTokenHarborSessionRepo(t *testing.T) (*accountRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	return newAccountRepositoryWithSQL(client, db, nil), mock
}

func TestStoreTokenHarborSessionMergesEncryptedCookieAtomically(t *testing.T) {
	repo, mock := newTokenHarborSessionRepo(t)
	loginAt := time.Unix(1730000000, 0)

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = COALESCE\(extra, '{}'::jsonb\) \|\| \$1::jsonb.*WHERE id = \$2 AND deleted_at IS NULL`).
		WithArgs(`{"th_session_cookie":"sess-cookie","th_session_login_at":1730000000}`, int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err := repo.StoreTokenHarborSession(context.Background(), 42, "sess-cookie", loginAt)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// encryptedTHCookiePayload 校验 UpdateExtra payload：cookie 为 enc:v1 密文且能
// 解回 wantPlain，login_at 为 wantUnix。
type encryptedTHCookiePayload struct {
	wantPlain string
	wantUnix  int64
}

func (m encryptedTHCookiePayload) Match(value driver.Value) bool {
	raw, ok := value.(string)
	if !ok {
		if bytes, isBytes := value.([]byte); isBytes {
			raw = string(bytes)
		} else {
			return false
		}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return false
	}
	cookie, ok := payload["th_session_cookie"].(string)
	if !ok || !credcrypt.IsEncrypted(cookie) {
		return false
	}
	plain, err := credcrypt.Decrypt(cookie)
	if err != nil || plain != m.wantPlain {
		return false
	}
	loginAt, ok := payload["th_session_login_at"].(float64)
	return ok && int64(loginAt) == m.wantUnix
}

// 加密启用时 cookie 以 enc:v1 密文落库；密文可经同密钥解回明文。
func TestStoreTokenHarborSessionEncryptsCookieWhenCipherEnabled(t *testing.T) {
	withTestCredCipher(t, func() {
		repo, mock := newTokenHarborSessionRepo(t)

		mock.ExpectBegin()
		mock.ExpectExec(`(?s)UPDATE accounts SET extra.*WHERE id = \$2 AND deleted_at IS NULL`).
			WithArgs(encryptedTHCookiePayload{wantPlain: "sess-cookie", wantUnix: 1730000000}, int64(42)).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()

		err := repo.StoreTokenHarborSession(context.Background(), 42, "sess-cookie", time.Unix(1730000000, 0))
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// 加密启用时 Load 解回明文（写读闭环）。
func TestLoadTokenHarborSessionDecryptsEncryptedCookie(t *testing.T) {
	withTestCredCipher(t, func() {
		repo, mock := newTokenHarborSessionRepo(t)

		encrypted, err := encryptCredentialsValue("sess-cookie")
		require.NoError(t, err)
		require.True(t, credcrypt.IsEncrypted(encrypted))

		mock.ExpectQuery(regexp.QuoteMeta("SELECT extra->>'th_session_cookie', extra->>'th_session_login_at' FROM accounts WHERE id = $1 AND deleted_at IS NULL")).
			WithArgs(int64(42)).
			WillReturnRows(sqlmock.NewRows([]string{"cookie", "login_at"}).AddRow(encrypted, "1730000000"))

		cookie, loginAt, err := repo.LoadTokenHarborSession(context.Background(), 42)
		require.NoError(t, err)
		require.Equal(t, "sess-cookie", cookie)
		require.Equal(t, time.Unix(1730000000, 0), loginAt)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func TestLoadTokenHarborSessionHitMissAndNull(t *testing.T) {
	repo, mock := newTokenHarborSessionRepo(t)
	query := regexp.QuoteMeta("SELECT extra->>'th_session_cookie', extra->>'th_session_login_at' FROM accounts WHERE id = $1 AND deleted_at IS NULL")

	// 命中：明文 cookie + unix 秒 login_at。
	mock.ExpectQuery(query).WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"cookie", "login_at"}).AddRow("sess-cookie", "1730000000"))
	// 键缺失/JSON null（->> 返回 NULL）→ 无会话。
	mock.ExpectQuery(query).WithArgs(int64(43)).
		WillReturnRows(sqlmock.NewRows([]string{"cookie", "login_at"}).AddRow(nil, nil))
	// 账号不存在（零行）→ 无会话。
	mock.ExpectQuery(query).WithArgs(int64(44)).
		WillReturnRows(sqlmock.NewRows([]string{"cookie", "login_at"}))
	// login_at 形态损坏 → 错误上抛（fail loud，由 service 侧 WARN 兜底）。
	mock.ExpectQuery(query).WithArgs(int64(45)).
		WillReturnRows(sqlmock.NewRows([]string{"cookie", "login_at"}).AddRow("sess-cookie", "not-a-number"))
	// 密文但进程无密钥 → 错误上抛，绝不把密文当明文放行。
	mock.ExpectQuery(query).WithArgs(int64(46)).
		WillReturnRows(sqlmock.NewRows([]string{"cookie", "login_at"}).AddRow("enc:v1:AAAA", "1730000000"))

	cookie, loginAt, err := repo.LoadTokenHarborSession(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, "sess-cookie", cookie)
	require.Equal(t, time.Unix(1730000000, 0), loginAt)

	cookie, loginAt, err = repo.LoadTokenHarborSession(context.Background(), 43)
	require.NoError(t, err)
	require.Empty(t, cookie)
	require.True(t, loginAt.IsZero())

	cookie, loginAt, err = repo.LoadTokenHarborSession(context.Background(), 44)
	require.NoError(t, err)
	require.Empty(t, cookie)
	require.True(t, loginAt.IsZero())

	_, _, err = repo.LoadTokenHarborSession(context.Background(), 45)
	require.Error(t, err)
	require.Contains(t, err.Error(), "login_at")

	_, _, err = repo.LoadTokenHarborSession(context.Background(), 46)
	require.Error(t, err, "encrypted value without key must fail loud")

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClearTokenHarborSessionSetsBothKeysNull(t *testing.T) {
	repo, mock := newTokenHarborSessionRepo(t)

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE accounts SET extra = COALESCE\(extra, '{}'::jsonb\) \|\| \$1::jsonb.*WHERE id = \$2 AND deleted_at IS NULL`).
		WithArgs(`{"th_session_cookie":null,"th_session_login_at":null}`, int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err := repo.ClearTokenHarborSession(context.Background(), 42)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
