package admin

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// newQuotaKeyFilterRepo 构造 sqlite 内存实例上的真实 accountRepository：
// ent Where 是否真正生效只能在真实查询上验证，mock 仓储证明不了这一点。
func newQuotaKeyFilterRepo(t *testing.T) *ent.Client {
	t.Helper()
	db, err := sql.Open("sqlite", "file:account_handler_quota_key_filter?mode=memory&cache=shared&_pragma=foreign_keys(1)&_fk=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(ent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// seedQuotaKey 造一个激活码，返回其 id（accounts.quota_key_id 外键指向 quota_keys.id）。
func seedQuotaKey(t *testing.T, client *ent.Client, mac string) int64 {
	t.Helper()
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("ciphertext-" + mac).
		SetCodeMAC(mac).
		SetCodeLast4("1234").
		SetStatus("active").
		Save(t.Context())
	require.NoError(t, err)
	return qk.ID
}

// seedQuotaKeyAccount 造一个账号；quotaKeyID <= 0 表示不归属任何激活码。
func seedQuotaKeyAccount(t *testing.T, client *ent.Client, name string, quotaKeyID int64) {
	t.Helper()
	create := client.Account.Create().
		SetName(name).
		SetPlatform(service.PlatformCursor).
		SetType(service.AccountTypeOAuth).
		SetCredentials(map[string]any{}).
		SetExtra(map[string]any{}).
		SetConcurrency(3).
		SetPriority(50).
		SetRateMultiplier(1.0).
		SetStatus(service.StatusActive).
		SetSchedulable(true)
	if quotaKeyID > 0 {
		create = create.SetQuotaKeyID(quotaKeyID)
	}
	_, err := create.Save(t.Context())
	require.NoError(t, err)
}

// TestAccountRepoListWithFilters_QuotaKeyID 锁定「按激活码过滤账号列表」的语义：
//   - quotaKeyID > 0：只返回归属该激活码的账号；未归属激活码（NULL）的账号不返回；
//   - quotaKeyID == 0：不加任何激活码条件，行为与改动前完全一致（全部返回）。
func TestAccountRepoListWithFilters_QuotaKeyID(t *testing.T) {
	client := newQuotaKeyFilterRepo(t)
	repo := repository.NewAccountRepository(client, nil, nil)

	qk16 := seedQuotaKey(t, client, "mac-16")
	qk548 := seedQuotaKey(t, client, "mac-548")
	seedQuotaKeyAccount(t, client, "qk-16-a", qk16)
	seedQuotaKeyAccount(t, client, "qk-16-b", qk16)
	seedQuotaKeyAccount(t, client, "qk-548", qk548)
	seedQuotaKeyAccount(t, client, "no-quota-key", 0)

	params := pagination.PaginationParams{Page: 1, PageSize: 50}

	filtered, page, err := repo.ListWithFilters(t.Context(), params, "", "", "", "", 0, "", qk16)
	require.NoError(t, err)
	require.Equal(t, int64(2), page.Total, "只返回归属该激活码的账号")
	names := []string{filtered[0].Name, filtered[1].Name}
	require.ElementsMatch(t, []string{"qk-16-a", "qk-16-b"}, names,
		"未归属激活码的账号（quota_key_id IS NULL）不应被返回")

	filteredOther, pageOther, err := repo.ListWithFilters(t.Context(), params, "", "", "", "", 0, "", qk548)
	require.NoError(t, err)
	require.Equal(t, int64(1), pageOther.Total)
	require.Equal(t, "qk-548", filteredOther[0].Name)

	// quotaKeyID = 0（不过滤）：行为与本次改动前完全一致。
	unfiltered, pageUnfiltered, err := repo.ListWithFilters(t.Context(), params, "", "", "", "", 0, "", 0)
	require.NoError(t, err)
	require.Equal(t, int64(4), pageUnfiltered.Total, "不过滤时返回全部账号（含未归属激活码的账号）")
	require.Len(t, unfiltered, 4)
}

// TestAccountHandlerList_QuotaKeyIDFilter 锁定 HTTP 层：
//   - ?quota_key_id=16 → 列表查询按该激活码过滤（服务层收到的 id 为 16）；
//   - 不传 → 不过滤（0）；
//   - 非法值（0 / 负数 / 非数字）→ 400，绝不静默放宽成"不过滤"。
func TestAccountHandlerList_QuotaKeyIDFilter(t *testing.T) {
	router, adminSvc := setupAccountListRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&quota_key_id=16", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(16), adminSvc.lastListAccounts.quotaKeyID, "激活码过滤必须透传到服务层")

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(0), adminSvc.lastListAccounts.quotaKeyID, "不传参数时不过滤")

	for _, raw := range []string{"0", "-1", "abc", "1.5"} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?quota_key_id="+raw, nil)
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, "非法 quota_key_id=%s 必须 400", raw)
	}
}
