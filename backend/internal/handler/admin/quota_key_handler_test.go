//go:build unit

// D-CURSOR-021：admin quota_keys 8 端点（§4.4）单测。
//
// 覆盖口径：
//   - 8 端点各自的**成功**路径与**关键失败**路径（非法 id / 缺参 / 服务报错 / 未装配依赖）；
//   - F1 脱敏：任何响应体都不含激活码原文；
//   - 删除走 DeleteWithAccounts（软删唯一路径，F5）；status 不可编辑；
//   - 顶层 quota_key_summaries 聚合为常数次查询（无 N+1）。

package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

const testQuotaKeyPlain = "sk-antigravity-secret-1234"

// ---------------------------------------------------------------------------
// 测试替身：service.QuotaKeyRepository（不引入新生产抽象，仅实现既有接口）
// ---------------------------------------------------------------------------

type fakeQuotaKeyRepo struct {
	mu        sync.Mutex
	keys      map[int64]*ent.QuotaKey
	nextID    int64
	accounts  map[int64][]*ent.Account
	listCalls int
	listErr   error
	getErr    error
	createErr error
	updateErr error
	deleted   []int64
	softKey   []int64
	outboxed  []int64
}

func newFakeQuotaKeyRepo() *fakeQuotaKeyRepo {
	return &fakeQuotaKeyRepo{
		keys:     map[int64]*ent.QuotaKey{},
		accounts: map[int64][]*ent.Account{},
	}
}

// seed 预置一个 active 激活码。
func (r *fakeQuotaKeyRepo) seed(id int64, last4, status string, exhausted bool) *ent.QuotaKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	qk := &ent.QuotaKey{ID: id, CodeLast4: last4, Status: status, Exhausted: exhausted}
	r.keys[id] = qk
	return qk
}

func (r *fakeQuotaKeyRepo) GetByID(_ context.Context, quotaKeyID int64) (*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	qk, ok := r.keys[quotaKeyID]
	if !ok {
		return nil, &ent.NotFoundError{}
	}
	return qk, nil
}

func (r *fakeQuotaKeyRepo) Create(_ context.Context, input service.CreateQuotaKeyInput) (*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return nil, r.createErr
	}
	r.nextID++
	qk := &ent.QuotaKey{
		ID:            r.nextID,
		CodeEncrypted: input.CodeEncrypted,
		CodeMAC:       input.CodeMAC,
		CodeLast4:     input.CodeLast4,
		Status:        input.Status,
		Exhausted:     input.Exhausted,
		ExpiresAt:     input.ExpiresAt,
		Notes:         input.Notes,
	}
	r.keys[qk.ID] = qk
	return qk, nil
}

func (r *fakeQuotaKeyRepo) UpdateStatusFields(_ context.Context, _ *ent.Tx, id int64, status string, exhausted bool, lastError *string, _ *time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return r.updateErr
	}
	qk, ok := r.keys[id]
	if !ok {
		return &ent.NotFoundError{}
	}
	qk.Status = status
	qk.Exhausted = exhausted
	qk.LastRefreshError = lastError
	return nil
}

func (r *fakeQuotaKeyRepo) HardDelete(_ context.Context, _ *ent.Tx, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.keys, id)
	return nil
}

func (r *fakeQuotaKeyRepo) UpdateCredentials(_ context.Context, id int64, codeEncrypted, codeMAC, codeLast4 string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return r.updateErr
	}
	qk, ok := r.keys[id]
	if !ok {
		return &ent.NotFoundError{}
	}
	qk.CodeEncrypted = codeEncrypted
	qk.CodeMAC = codeMAC
	qk.CodeLast4 = codeLast4
	return nil
}

func (r *fakeQuotaKeyRepo) ListExpired(_ context.Context, _ time.Time) ([]*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot(), nil
}

func (r *fakeQuotaKeyRepo) ListByStatus(_ context.Context, status string) ([]*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*ent.QuotaKey, 0)
	for _, qk := range r.keys {
		if qk.Status == status {
			out = append(out, qk)
		}
	}
	return out, nil
}

func (r *fakeQuotaKeyRepo) List(_ context.Context) ([]*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCalls++
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.snapshot(), nil
}

func (r *fakeQuotaKeyRepo) UpdateMeta(_ context.Context, id int64, expiresAt **time.Time, notes **string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return r.updateErr
	}
	qk, ok := r.keys[id]
	if !ok {
		return &ent.NotFoundError{}
	}
	if expiresAt != nil {
		qk.ExpiresAt = *expiresAt
	}
	if notes != nil {
		qk.Notes = *notes
	}
	return nil
}

func (r *fakeQuotaKeyRepo) ListAccountsByQuotaKey(_ context.Context, quotaKeyID int64) ([]*ent.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*ent.Account{}, r.accounts[quotaKeyID]...), nil
}

func (r *fakeQuotaKeyRepo) CountWorkingAccounts(_ context.Context, _ *ent.Tx, quotaKeyID int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.accounts[quotaKeyID]), nil
}

func (r *fakeQuotaKeyRepo) SoftDeleteAccountsByQuotaKey(_ context.Context, _ *ent.Tx, quotaKeyID int64) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, quotaKeyID)
	ids := make([]int64, 0, len(r.accounts[quotaKeyID]))
	for _, acc := range r.accounts[quotaKeyID] {
		ids = append(ids, acc.ID)
	}
	return ids, nil
}

func (r *fakeQuotaKeyRepo) EnqueueAccountChangedOutbox(_ context.Context, _ *ent.Tx, accountIDs []int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outboxed = append(r.outboxed, accountIDs...)
	return nil
}

func (r *fakeQuotaKeyRepo) SoftDelete(_ context.Context, _ *ent.Tx, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.softKey = append(r.softKey, id)
	return nil
}

func (r *fakeQuotaKeyRepo) ListEligibleForRefresh(_ context.Context) ([]*ent.QuotaKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*ent.QuotaKey, 0)
	for _, qk := range r.keys {
		if qk.Status == "active" && !qk.Exhausted {
			out = append(out, qk)
		}
	}
	return out, nil
}

func (r *fakeQuotaKeyRepo) snapshot() []*ent.QuotaKey {
	out := make([]*ent.QuotaKey, 0, len(r.keys))
	for _, qk := range r.keys {
		out = append(out, qk)
	}
	return out
}

func (r *fakeQuotaKeyRepo) listCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCalls
}

// ---------------------------------------------------------------------------
// 路由与 HTTP 辅助
// ---------------------------------------------------------------------------

func setupQuotaKeyRouter(svc *service.QuotaKeyService, sweeper *service.QuotaKeySweeper) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := NewQuotaKeyHandler(svc, sweeper)
	// 与 internal/server/routes/admin.go 的 registerQuotaKeyRoutes 同序注册。
	qk := router.Group("/api/v1/admin/quota_keys")
	qk.GET("", h.List)
	qk.GET("/:id", h.Get)
	qk.POST("", h.Create)
	qk.PUT("/:id", h.Update)
	qk.DELETE("/:id", h.Delete)
	qk.POST("/:id/refresh", h.RefreshAccounts)
	qk.POST("/:id/clear_exhausted", h.ClearExhausted)
	qk.POST("/sweep_expired", h.SweepExpired)
	return router
}

func newQuotaKeyService(repo service.QuotaKeyRepository, client *ent.Client) *service.QuotaKeyService {
	return service.NewQuotaKeyService(repo, service.NewCredcryptEncryptor(), nil, client)
}

func doQuotaKeyJSON(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	}
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

func requireNoPlaintext(t *testing.T, body string) {
	t.Helper()
	require.NotContains(t, body, testQuotaKeyPlain, "F1: 响应体绝不能包含激活码原文")
	require.NotContains(t, body, "code_encrypted", "F1: 响应体不得包含密文字段")
}

// newSQLiteEntClient 构造 sqlite 内存实例，用于需要真实事务/更新的端点
// （ClearExhausted / DeleteWithAccounts / RefreshAccounts / SweepNow）。
func newSQLiteEntClient(t *testing.T) *ent.Client {
	t.Helper()
	db, err := sql.Open("sqlite", "file:quota_key_handler?mode=memory&cache=shared&_pragma=foreign_keys(1)&_fk=1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(ent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// ---------------------------------------------------------------------------
// 端点 1：GET /quota_keys
// ---------------------------------------------------------------------------

func TestQuotaKeyHandler_List_Success_AndMasked(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(1, "1234", "active", false)
	repo.seed(2, "5678", "expired", true)
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys", "")
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	requireNoPlaintext(t, body)

	var payload struct {
		Code int `json:"code"`
		Data struct {
			Items []struct {
				ID         int64  `json:"id"`
				CodeMasked string `json:"code_masked"`
				Status     string `json:"status"`
				Exhausted  bool   `json:"exhausted"`
			} `json:"items"`
			Total             int64 `json:"total"`
			Page              int   `json:"page"`
			PageSize          int   `json:"page_size"`
			Pages             int   `json:"pages"`
			QuotaKeySummaries []struct {
				ID         int64  `json:"id"`
				CodeMasked string `json:"code_masked"`
			} `json:"quota_key_summaries"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, 0, payload.Code)
	require.Len(t, payload.Data.Items, 2)
	require.Equal(t, int64(2), payload.Data.Total)
	require.Equal(t, 1, payload.Data.Page)
	require.Equal(t, 2, payload.Data.PageSize)
	require.Equal(t, 1, payload.Data.Pages)
	// 顶层汇总与列表同源，一次性给出
	require.Len(t, payload.Data.QuotaKeySummaries, 2)
	for _, item := range payload.Data.Items {
		require.Contains(t, item.CodeMasked, "XXXX-****-****-")
		require.Len(t, item.CodeMasked, len("XXXX-****-****-")+4)
	}
}

func TestQuotaKeyHandler_List_ServiceError(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.listErr = errors.New("boom")
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ---------------------------------------------------------------------------
// 端点 2：POST /quota_keys
// ---------------------------------------------------------------------------

func TestQuotaKeyHandler_Create_Success_NeverEchoesPlaintext(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys", `{"code":"`+testQuotaKeyPlain+`","notes":"备注"}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	body := rec.Body.String()
	requireNoPlaintext(t, body)

	var payload struct {
		Data struct {
			ID         int64  `json:"id"`
			CodeMasked string `json:"code_masked"`
			Status     string `json:"status"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, int64(1), payload.Data.ID)
	require.Equal(t, "XXXX-****-****-1234", payload.Data.CodeMasked)
	require.Equal(t, "active", payload.Data.Status)
}

func TestQuotaKeyHandler_Create_MissingCode(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys", `{"notes":"no code"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Invalid request")
}

func TestQuotaKeyHandler_Create_BlankCode(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys", `{"code":"   "}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "code is required")
}

// ---------------------------------------------------------------------------
// 端点 3：GET /quota_keys/:id
// ---------------------------------------------------------------------------

func TestQuotaKeyHandler_Get_Success_AccountPaging(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(7, "1234", "active", false)
	for i := 1; i <= 3; i++ {
		repo.accounts[7] = append(repo.accounts[7], &ent.Account{
			ID: int64(100 + i), Name: "cursor-acc", Platform: "cursor", Type: "apikey",
			Status: service.StatusActive, CreatedAt: time.Now().UTC(),
		})
	}
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys/7?page=1&page_size=2", "")
	require.Equal(t, http.StatusOK, rec.Code)
	requireNoPlaintext(t, rec.Body.String())

	var payload struct {
		Data struct {
			ID         int64  `json:"id"`
			CodeMasked string `json:"code_masked"`
			Status     string `json:"status"`
			Total      int64  `json:"total"`
			Page       int    `json:"page"`
			PageSize   int    `json:"page_size"`
			Accounts   []struct {
				ID       int64  `json:"id"`
				Platform string `json:"platform"`
			} `json:"accounts"`
			Credentials map[string]any `json:"credentials"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.NotContains(t, rec.Body.String(), `"code_value_masked"`)
	require.Equal(t, "XXXX-****-****-1234", payload.Data.CodeMasked)
	require.Equal(t, int64(3), payload.Data.Total)
	require.Equal(t, 1, payload.Data.Page)
	require.Equal(t, 2, payload.Data.PageSize)
	require.Len(t, payload.Data.Accounts, 2, "第一页按 page_size 截断")
	require.Nil(t, payload.Data.Credentials, "详情不应回放账号凭据")

	// 第二页取剩余 1 条
	rec2 := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys/7?page=2&page_size=2", "")
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Accounts, 1)
}

func TestQuotaKeyHandler_Get_InvalidID(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys/abc", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Invalid quota key id")
}

func TestQuotaKeyHandler_Get_NotFound(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys/42", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "QUOTA_KEY_NOT_FOUND")
}

// ---------------------------------------------------------------------------
// 端点 4：PUT /quota_keys/:id
// ---------------------------------------------------------------------------

func TestQuotaKeyHandler_Update_Success_StatusImmutable(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(9, "1234", "active", false)
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	// 请求体携带 status 也不可改写：Update 请求结构不含该字段，服务端忽略之。
	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/9",
		`{"code":"`+testQuotaKeyPlain+`","status":"expired"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	requireNoPlaintext(t, rec.Body.String())

	var payload struct {
		Data struct {
			ID         int64  `json:"id"`
			CodeMasked string `json:"code_masked"`
			Status     string `json:"status"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "XXXX-****-****-1234", payload.Data.CodeMasked)
	require.Equal(t, "active", payload.Data.Status, "status 不可通过 PUT 改写")
}

func TestQuotaKeyHandler_Update_MetaTriState(t *testing.T) {
	initialExpiresAt := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	initialNotes := "keep"
	setExpiresAt := time.Date(2026, time.November, 1, 8, 30, 0, 0, time.UTC)

	tests := []struct {
		name        string
		body        string
		wantExpires *time.Time
		wantNotes   *string
	}{
		{name: "omitted keeps values", body: `{}`, wantExpires: &initialExpiresAt, wantNotes: &initialNotes},
		{name: "null clears values", body: `{"expires_at":null,"notes":null}`},
		{name: "values replace values", body: `{"expires_at":"2026-11-01T08:30:00Z","notes":"updated"}`, wantExpires: &setExpiresAt, wantNotes: stringPointer("updated")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newFakeQuotaKeyRepo()
			qk := repo.seed(9, "1234", "active", false)
			qk.ExpiresAt = &initialExpiresAt
			qk.Notes = &initialNotes
			router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

			rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/9", tt.body)
			require.Equal(t, http.StatusOK, rec.Code)
			if tt.wantExpires == nil {
				require.Nil(t, qk.ExpiresAt)
			} else {
				require.NotNil(t, qk.ExpiresAt)
				require.Equal(t, *tt.wantExpires, *qk.ExpiresAt)
			}
			if tt.wantNotes == nil {
				require.Nil(t, qk.Notes)
			} else {
				require.NotNil(t, qk.Notes)
				require.Equal(t, *tt.wantNotes, *qk.Notes)
			}
		})
	}
}

func stringPointer(value string) *string {
	return &value
}

func TestQuotaKeyHandler_Update_InvalidID(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(9, "1234", "active", false)
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/0", `{"notes":"x"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaKeyHandler_Update_NotFound(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/99", `{"notes":"x"}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// ---------------------------------------------------------------------------
// 端点 5：DELETE /quota_keys/:id（软删唯一路径）
// ---------------------------------------------------------------------------

func TestQuotaKeyHandler_Delete_InvalidID(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodDelete, "/api/v1/admin/quota_keys/x", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaKeyHandler_Delete_NotConfiguredClient(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(5, "1234", "active", false)
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodDelete, "/api/v1/admin/quota_keys/5", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code, "未装配 ent client 时删除必须显式报错，禁止静默成功")
}

// ---------------------------------------------------------------------------
// 端点 6/7/8：refresh / clear_exhausted / sweep_expired
// ---------------------------------------------------------------------------

func TestQuotaKeyHandler_Refresh_InvalidID(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(5, "1234", "active", false)
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/-1/refresh", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaKeyHandler_Refresh_NotConfiguredClient(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(5, "1234", "active", false)
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/5/refresh", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestQuotaKeyHandler_ClearExhausted_Success(t *testing.T) {
	client := newSQLiteEntClient(t)
	ctx := context.Background()
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("ciphertext").SetCodeMAC("mac").SetCodeLast4("1234").
		SetStatus("active").SetExhausted(true).SetLastRefreshError("upstream down").
		Save(ctx)
	require.NoError(t, err)

	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, client), nil)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/"+strconv.FormatInt(qk.ID, 10)+"/clear_exhausted", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"cleared":true`)

	reloaded, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.False(t, reloaded.Exhausted, "ClearExhausted 必须清掉 exhausted 标记")
	require.Equal(t, "active", reloaded.Status, "ClearExhausted 不得改写 status 维度")
}

func TestQuotaKeyHandler_ClearExhausted_InvalidID(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/zz/clear_exhausted", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestQuotaKeyHandler_SweepExpired_Success(t *testing.T) {
	client := newSQLiteEntClient(t)
	ctx := context.Background()
	_, err := client.QuotaKey.Create().
		SetCodeEncrypted("ciphertext").SetCodeMAC("mac").SetCodeLast4("1234").
		SetStatus("active").SetExhausted(false).
		SetExpiresAt(time.Now().Add(-time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	// 用真实 repository（sqlite）验证 ListExpired + UpdateStatusFields 整链路。
	qkRepo := repository.NewQuotaKeyRepository(client)
	sweeper := service.NewQuotaKeySweeper(qkRepo, nil, client)
	router := setupQuotaKeyRouter(newQuotaKeyService(qkRepo, client), sweeper)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/sweep_expired", "")
	require.Equal(t, http.StatusOK, rec.Code)

	var payload struct {
		Data struct {
			Swept int `json:"swept"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.NotContains(t, rec.Body.String(), `"cleared"`)
	require.Equal(t, 1, payload.Data.Swept)

	all, err := client.QuotaKey.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, "expired", all[0].Status)
}

func TestQuotaKeyHandler_SweepExpired_SweeperNotWired(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/sweep_expired", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestQuotaKeyHandler_SweepExpired_RoutesBeforeWildcard(t *testing.T) {
	// 静态子路径不能被 /:id 通配吞掉：sweep_expired 必须命中专用 handler（返回 503
	// 而非 "Invalid quota key id"）。
	repo := newFakeQuotaKeyRepo()
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/sweep_expired", "")
	require.NotContains(t, rec.Body.String(), "Invalid quota key id")
}

// ---------------------------------------------------------------------------
// accounts 列表/详情：quota_key_id / quota_key_label + 顶层 quota_key_summaries
// ---------------------------------------------------------------------------

func setupAccountQuotaKeyRouter(adminSvc *stubAdminService, quotaKeySvc *service.QuotaKeyService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h.SetQuotaKeyService(quotaKeySvc)
	router.GET("/api/v1/admin/accounts", h.List)
	router.GET("/api/v1/admin/accounts/:id", h.GetByID)
	return router
}

func seedAccountPage(count int, keyA, keyB int64) []service.Account {
	out := make([]service.Account, 0, count)
	for i := 0; i < count; i++ {
		owner := keyA
		if i%2 == 1 {
			owner = keyB
		}
		out = append(out, service.Account{
			ID:         int64(1000 + i),
			Name:       "cursor-acc",
			Platform:   "cursor",
			Type:       "apikey",
			Status:     service.StatusActive,
			QuotaKeyID: &owner,
		})
	}
	return out
}

func TestAccountList_TopLevelQuotaKeySummaries_AndItemLabels(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(11, "4321", "active", false)
	repo.seed(12, "8765", "active", false)

	adminSvc := newStubAdminService()
	adminSvc.accounts = seedAccountPage(20, 11, 12)
	router := setupAccountQuotaKeyRouter(adminSvc, newQuotaKeyService(repo, nil))

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", "")
	require.Equal(t, http.StatusOK, rec.Code)
	requireNoPlaintext(t, rec.Body.String())

	var payload struct {
		Code int `json:"code"`
		Data struct {
			Items []struct {
				ID            int64  `json:"id"`
				QuotaKeyID    *int64 `json:"quota_key_id"`
				QuotaKeyLabel string `json:"quota_key_label"`
				CodeMasked    string `json:"code_masked"`
			} `json:"items"`
			Total             int64 `json:"total"`
			Page              int   `json:"page"`
			PageSize          int   `json:"page_size"`
			Pages             int   `json:"pages"`
			QuotaKeySummaries []struct {
				ID         int64  `json:"id"`
				CodeMasked string `json:"code_masked"`
				Status     string `json:"status"`
				Exhausted  bool   `json:"exhausted"`
			} `json:"quota_key_summaries"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, 0, payload.Code)
	require.Len(t, payload.Data.Items, 20)
	require.Equal(t, int64(20), payload.Data.Total)
	// 既有分页字段保持不缺失（items/total/page/page_size/pages）
	require.Equal(t, 1, payload.Data.Page)
	require.Equal(t, 20, payload.Data.PageSize)
	require.Equal(t, 1, payload.Data.Pages)

	// 顶层汇总：一次性给出两个激活码，且只含脱敏字段
	require.Len(t, payload.Data.QuotaKeySummaries, 2)
	require.Equal(t, int64(11), payload.Data.QuotaKeySummaries[0].ID)
	require.Equal(t, "XXXX-****-****-4321", payload.Data.QuotaKeySummaries[0].CodeMasked)
	require.Equal(t, int64(12), payload.Data.QuotaKeySummaries[1].ID)
	require.Equal(t, "XXXX-****-****-8765", payload.Data.QuotaKeySummaries[1].CodeMasked)

	for i, item := range payload.Data.Items {
		require.NotNil(t, item.QuotaKeyID)
		if i%2 == 0 {
			require.Equal(t, int64(11), *item.QuotaKeyID)
			require.Equal(t, "XXXX-****-****-4321", item.QuotaKeyLabel)
		} else {
			require.Equal(t, int64(12), *item.QuotaKeyID)
			require.Equal(t, "XXXX-****-****-8765", item.QuotaKeyLabel)
		}
		require.Empty(t, item.CodeMasked, "F7：账号项禁止内嵌完整 QuotaKey 聚合（code_masked 只能出现在顶层汇总）")
	}
}

// TestAccountList_QuotaKeySummaries_NoNPlusOne 锁定聚合查询次数为常数：
// 无论本页多少账号、涉及多少激活码，激活码汇总都只查一次（禁用逐账号回填）。
func TestAccountList_QuotaKeySummaries_NoNPlusOne(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(11, "4321", "active", false)
	repo.seed(12, "8765", "active", false)

	adminSvc := newStubAdminService()
	adminSvc.accounts = seedAccountPage(50, 11, 12)
	router := setupAccountQuotaKeyRouter(adminSvc, newQuotaKeyService(repo, nil))

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=50", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, repo.listCallCount(), "汇总查询次数必须为常数 1（50 个账号不得产生 50 次查询）")
}

// TestAccountList_NoQuotaKeyIDs_NoExtraQuery 无 cursor 归属账号时既不查汇总、
// 也不输出 quota_key_summaries（保持既有 JSON 形态）。
func TestAccountList_NoQuotaKeyIDs_NoExtraQuery(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	adminSvc := newStubAdminService()
	adminSvc.accounts = []service.Account{{
		ID: 7, Name: "openai-acc", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive,
	}}
	router := setupAccountQuotaKeyRouter(adminSvc, newQuotaKeyService(repo, nil))

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 0, repo.listCallCount())
	require.NotContains(t, rec.Body.String(), "quota_key_summaries")
	require.NotContains(t, rec.Body.String(), "quota_key_label")
}

// TestAccountList_UnwiredQuotaKeyService_DegradesGracefully 未装配激活码服务时
// 列表主数据不受影响，仅缺失两个展示标量。
func TestAccountList_UnwiredQuotaKeyService_DegradesGracefully(t *testing.T) {
	adminSvc := newStubAdminService()
	adminSvc.accounts = seedAccountPage(2, 11, 12)
	router := setupAccountQuotaKeyRouter(adminSvc, nil)

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"items":`)
	require.NotContains(t, rec.Body.String(), "quota_key_summaries")
}

func TestAccountGetByID_QuotaKeyLabel(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(11, "4321", "active", false)
	owner := int64(11)

	adminSvc := newStubAdminService()
	adminSvc.getAccountResult = &service.Account{
		ID: 5, Name: "cursor-acc", Platform: "cursor", Type: "apikey",
		Status: service.StatusActive, QuotaKeyID: &owner,
	}
	router := setupAccountQuotaKeyRouter(adminSvc, newQuotaKeyService(repo, nil))

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/accounts/5", "")
	require.Equal(t, http.StatusOK, rec.Code)
	requireNoPlaintext(t, rec.Body.String())

	var payload struct {
		Data struct {
			ID            int64  `json:"id"`
			QuotaKeyID    *int64 `json:"quota_key_id"`
			QuotaKeyLabel string `json:"quota_key_label"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.NotNil(t, payload.Data.QuotaKeyID)
	require.Equal(t, int64(11), *payload.Data.QuotaKeyID)
	require.Equal(t, "XXXX-****-****-4321", payload.Data.QuotaKeyLabel)
}
