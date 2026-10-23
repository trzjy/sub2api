//go:build unit

// W2-D1：管理端展示通路的两层额度投影（方案 §25.6 消费闭包⑤⑥⑦ + §25.7-B 展示口径）。
//
// 本文件只验证**读通路与展示投影**：
//   - quota key 列表/详情/probe 响应带同一份已提交快照投影；
//   - 账号列表 lite/full 带同一份投影（同值），且按一次批量 summary 投影，不逐行查询；
//   - 账号页不出现伪造的供应商账号余额/百分比，只给「供应商账号额度未提供」注明。

package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const (
	handlerSnapshotSuccess = `{"total":400,"used":86,"status_code":200,` +
		`"checked_at":"2026-10-22T02:00:00Z","attempted_at":"2026-10-22T02:00:00Z","error_code":""}`
	handlerSnapshotFailed = `{"total":400,"used":86,"status_code":200,` +
		`"checked_at":"2026-10-22T02:00:00Z","attempted_at":"2026-10-22T02:05:00Z","error_code":"lookup_timeout"}`
)

// seedUsage 给内存替身里的激活码挂上 usage_snapshot（模拟 CR-A 已落库的列内容）。
func seedUsage(repo *fakeQuotaKeyRepo, id int64, raw string) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if qk, ok := repo.keys[id]; ok {
		qk.UsageSnapshot = jsontext.Value(raw)
	}
}

// ---------------------------------------------------------------------------
// quota key 列表 / 详情
// ---------------------------------------------------------------------------

// TestQuotaKeyHandler_List_UsageProjectionThreeStates 钉死列表三态：
// 成功态有数值、失败态保留成功值并标 query_failed、无快照态**不出现** usage 字段（不是 0）。
func TestQuotaKeyHandler_List_UsageProjectionThreeStates(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(1, "1111", "active", false)
	repo.seed(2, "2222", "active", false)
	repo.seed(3, "3333", "active", false)
	seedUsage(repo, 1, handlerSnapshotSuccess)
	seedUsage(repo, 2, handlerSnapshotFailed)
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 3)

	byID := map[float64]map[string]any{}
	for _, item := range payload.Data.Items {
		byID[item["id"].(float64)] = item
	}

	success, ok := byID[1]["usage"].(map[string]any)
	require.True(t, ok, "成功态必须带 usage 投影")
	require.Equal(t, float64(400), success["total"])
	require.Equal(t, float64(86), success["used"])
	require.Equal(t, float64(314), success["remaining"], "remaining 是 total-used 的算术剩余")
	require.Equal(t, false, success["query_failed"])
	require.Equal(t, service.QuotaKeyUsageLabel, success["label"], "标签必须是「所属激活码额度」")
	require.Equal(t, service.QuotaKeyUsageScope, success["scope"])

	failed, ok := byID[2]["usage"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, failed["query_failed"])
	require.Equal(t, "lookup_timeout", failed["error_code"])
	require.Equal(t, float64(400), failed["total"], "失败不得抹掉最近成功值")
	require.Equal(t, float64(86), failed["used"], "失败不得把 used 写成 0")
	require.NotNil(t, failed["attempted_at"])
	require.NotNil(t, failed["checked_at"])

	require.NotContains(t, byID[3], "usage", "无快照时不得出现 usage 字段（缺失不是 0）")
}

// TestQuotaKeyHandler_Get_UsageProjection 钉死详情端点：与列表同一份投影值。
func TestQuotaKeyHandler_Get_UsageProjection(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(7, "7777", "active", false)
	seedUsage(repo, 7, handlerSnapshotSuccess)
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys/7", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var payload struct {
		Data struct {
			ID    int64 `json:"id"`
			Usage struct {
				Total     *int   `json:"total"`
				Used      *int   `json:"used"`
				Remaining *int   `json:"remaining"`
				Label     string `json:"label"`
				Scope     string `json:"scope"`
			} `json:"usage"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, int64(7), payload.Data.ID)
	require.NotNil(t, payload.Data.Usage.Total)
	require.Equal(t, 400, *payload.Data.Usage.Total)
	require.NotNil(t, payload.Data.Usage.Used)
	require.Equal(t, 86, *payload.Data.Usage.Used)
	require.NotNil(t, payload.Data.Usage.Remaining)
	require.Equal(t, 314, *payload.Data.Usage.Remaining)
	require.Equal(t, service.QuotaKeyUsageLabel, payload.Data.Usage.Label)
	require.Equal(t, service.QuotaKeyUsageScope, payload.Data.Usage.Scope)
}

// TestQuotaKeyHandler_Get_NoSnapshotOmitsUsage 钉死详情缺失态：无快照不出现 usage（不补 0）。
func TestQuotaKeyHandler_Get_NoSnapshotOmitsUsage(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(8, "8888", "active", false)
	router := setupQuotaKeyRouter(newQuotaKeyService(repo, nil), nil)

	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys/8", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	var data map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["data"], &data))
	require.NotContains(t, data, "usage", "无快照时省略 usage，不得落成 0 值")
}

// ---------------------------------------------------------------------------
// probe
// ---------------------------------------------------------------------------

// newProbePostgresClient 返回连接**隔离 PostgreSQL** 的 ent client（懒初始化，幂等建表）。
//
// A07 测试装配修复（§25.17.6）：本用例此前自建 SQLite，却调用生产同 key advisory 事务
// （pg_try_advisory_xact_lock），必然报no such function并返回 500。advisory 事务语义
// 只能由真实 PostgreSQL 证明，故这里改用与生产同型的真实隔离库；生产代码不为 SQLite
// 增加任何兜底分支。
//
// DSN 取 QUOTA_TEST_DSN（默认既有 quota_test 库）；本轮执行指向本轮隔离库，
// 建库/清库只在该隔离库内进行，绝不使用生产库。
func newProbePostgresClient(t *testing.T) *ent.Client {
	t.Helper()
	probeTestOnce.Do(func() {
		dsn := os.Getenv("QUOTA_TEST_DSN")
		if dsn == "" {
			dsn = "postgres://quota_test:quota_test_pw@127.0.0.1:5432/quota_test?sslmode=disable"
		}
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			probeTestInitErr = fmt.Errorf("open sql: %w", err)
			return
		}
		if err := db.Ping(); err != nil {
			_ = db.Close()
			probeTestInitErr = fmt.Errorf("ping probe test db (set QUOTA_TEST_DSN to an isolated PostgreSQL): %w", err)
			return
		}
		drv := entsql.OpenDB(dialect.Postgres, db)
		client := ent.NewClient(ent.Driver(drv))
		if err := client.Schema.Create(context.Background()); err != nil {
			_ = db.Close()
			probeTestInitErr = fmt.Errorf("schema create: %w", err)
			return
		}
		probeTestClient = client
	})
	if probeTestInitErr != nil {
		t.Fatalf("probe test PostgreSQL init failed: %v", probeTestInitErr)
	}
	return probeTestClient
}

var (
	probeTestOnce    sync.Once
	probeTestClient  *ent.Client
	probeTestInitErr error
)

// newProbeQuotaKey 在隔离库里建一条本轮唯一的激活码。
//
// code 必须逐次唯一：quota_keys 的删除口径是软删（行保留），而 uq_quota_keys_code_mac
// 是库级唯一索引——用固定 code 的用例在第二次运行时会撞唯一约束。故code 带上运行标识。
// 这不影响被测语义（Probe 只按 MAC 复核来源身份，与 code 具体值无关）。
func newProbeQuotaKey(t *testing.T, svc *service.QuotaKeyService, client *ent.Client, tag string) *service.QuotaKeyStatus {
	t.Helper()
	code := fmt.Sprintf("JG-CF-PROBE-%s-%d", tag, time.Now().UnixNano())
	status, err := svc.Create(context.Background(), code, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		// 软删以免污染其它用例；唯一索引冲突由上面的逐次唯一 code 规避。
		_ = client.QuotaKey.DeleteOneID(status.ID).Exec(context.Background())
	})
	return status
}

// TestQuotaKeyHandler_Probe_UsageProjectionFromCommittedSnapshot 钉死 probe 返回带投影，
// 且投影取自**已提交**快照列。
//
// A07 装配修复后，本用例在真实隔离 PostgreSQL 上跑完整链路：httptest lookup 返回
// 400/37 → Probe 在同 key advisory 事务内写 usage_snapshot 并 commit → HTTP 投影必须
// 等于**数据库已提交的新快照**（400/37），而不是 lookup 之前的旧值、也不是内存里
// 尚未提交的值。commit 失败时 HTTP 不得 200。
func TestQuotaKeyHandler_Probe_UsageProjectionFromCommittedSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{` +
			`"group":"Pro","key_status_code":1,"quota_key_max_quota":400,` +
			`"quota_key_used_quota":37,"device_count":1,"device_max_count":3,` +
			`"mode":1,"expired_at":"2026-12-31T00:00:00Z"}}}`))
	}))
	defer srv.Close()

	gin.SetMode(gin.TestMode)
	client := newProbePostgresClient(t)
	ctx := context.Background()
	settingRepo := repository.NewSettingRepository(client)
	quotaKeyRepo := repository.NewQuotaKeyRepository(client)
	autoEntry := service.NewCursorAutoEntryService(settingRepo, quotaKeyRepo, service.NewCredcryptEncryptor(), client, &http.Client{Timeout: 5 * time.Second}, srv.URL)
	svc := service.NewQuotaKeyService(quotaKeyRepo, service.NewCredcryptEncryptor(), nil, client)
	handler := NewQuotaKeyHandler(svc, nil, autoEntry)

	router := gin.New()
	qk := router.Group("/api/v1/admin/quota_keys")
	qk.POST("/:id/probe_quota", handler.ProbeQuota)

	// 预置"旧的已提交快照"（400/86）：probe 的新观测必须真实落库并覆盖它。
	status := newProbeQuotaKey(t, svc, client, "SNAPSHOT")
	_, err := client.QuotaKey.UpdateOneID(status.ID).
		SetUsageSnapshot(jsontext.Value(handlerSnapshotSuccess)).
		Save(ctx)
	require.NoError(t, err)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/"+itoa(status.ID)+"/probe_quota", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 数据库里已提交的新快照 = 本次 lookup 的 400/37（不再是预置的 400/86）。
	committed, err := client.QuotaKey.Get(ctx, status.ID)
	require.NoError(t, err)
	require.NotContains(t, string(committed.UsageSnapshot), `"used":86`,
		"Probe 必须在真实 PostgreSQL 上覆盖旧快照")

	var payload struct {
		Data struct {
			Max   int `json:"max"`
			Used  int `json:"used"`
			Usage *struct {
				Total     *int   `json:"total"`
				Used      *int   `json:"used"`
				Remaining *int   `json:"remaining"`
				Label     string `json:"label"`
				Failed    bool   `json:"query_failed"`
			} `json:"usage"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	// 既有 probe 摘要字段形态不变。
	require.Equal(t, 400, payload.Data.Max)
	require.Equal(t, 37, payload.Data.Used)
	// 投影必须等于数据库已提交的新快照，而不是预置旧值。
	require.NotNil(t, payload.Data.Usage, "probe 响应必须带 usage 投影")
	require.NotNil(t, payload.Data.Usage.Total)
	require.Equal(t, 400, *payload.Data.Usage.Total)
	require.NotNil(t, payload.Data.Usage.Used)
	require.Equal(t, 37, *payload.Data.Usage.Used, "HTTP 投影必须等于数据库已提交的新快照")
	require.NotNil(t, payload.Data.Usage.Remaining)
	require.Equal(t, 363, *payload.Data.Usage.Remaining)
	require.Equal(t, service.QuotaKeyUsageLabel, payload.Data.Usage.Label)
	require.False(t, payload.Data.Usage.Failed)

	// 断言链直接读已提交列，而不是只信HTTP 自述。
	committedDoc, ok := service.ParseQuotaKeyUsageSnapshot(committed.UsageSnapshot)
	require.True(t, ok, "已提交快照必须可解析")
	require.NotNil(t, committedDoc.Used)
	require.Equal(t, 37, *committedDoc.Used)
	require.Equal(t, *committedDoc.Used, *payload.Data.Usage.Used)
}

// TestQuotaKeyHandler_Probe_CommitFailureMustNotReturnOK 钉死 commit 失败时 HTTP 不得 200，
// 且不得把 lookup 的内存值当成已提交事实（数据库仍是旧快照）。
//
// 故障注入点在同 key 事务的 commit：用一个包装仓储让事务提交失败（真实 PostgreSQL 拒绝
// 该提交），从而证明 Probe 只有在快照真正落库后才报告成功。
func TestQuotaKeyHandler_Probe_CommitFailureMustNotReturnOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{` +
			`"group":"Pro","key_status_code":1,"quota_key_max_quota":400,` +
			`"quota_key_used_quota":37,"device_count":1,"device_max_count":3,` +
			`"mode":1,"expired_at":"2026-12-31T00:00:00Z"}}}`))
	}))
	defer srv.Close()

	gin.SetMode(gin.TestMode)
	client := newProbePostgresClient(t)
	ctx := context.Background()
	settingRepo := repository.NewSettingRepository(client)
	baseRepo := repository.NewQuotaKeyRepository(client)
	autoEntry := service.NewCursorAutoEntryService(settingRepo, baseRepo, service.NewCredcryptEncryptor(), client, &http.Client{Timeout: 5 * time.Second}, srv.URL)
	svc := service.NewQuotaKeyService(baseRepo, service.NewCredcryptEncryptor(), nil, client)
	handler := NewQuotaKeyHandler(svc, nil, autoEntry)

	router := gin.New()
	qk := router.Group("/api/v1/admin/quota_keys")
	qk.POST("/:id/probe_quota", handler.ProbeQuota)

	status := newProbeQuotaKey(t, svc, client, "COMMITFAIL")
	_, err := client.QuotaKey.UpdateOneID(status.ID).
		SetUsageSnapshot(jsontext.Value(handlerSnapshotSuccess)).
		Save(ctx)
	require.NoError(t, err)

	// 注入点：让观测写入本身失败（等价于该事务无法提交）。用只读副本仓储替身，
	// 其 UpdateUsageSnapshot 明确报错 —— 事务随之回滚，commit 从未发生。
	failingRepo := &commitFailingQuotaKeyRepo{QuotaKeyRepository: baseRepo}
	failingAutoEntry := service.NewCursorAutoEntryService(settingRepo, failingRepo, service.NewCredcryptEncryptor(), client, &http.Client{Timeout: 5 * time.Second}, srv.URL)
	failingHandler := NewQuotaKeyHandler(svc, nil, failingAutoEntry)
	failingRouter := gin.New()
	failingRouter.POST("/api/v1/admin/quota_keys/:id/probe_quota", failingHandler.ProbeQuota)

	rec := doQuotaKeyJSON(failingRouter, http.MethodPost, "/api/v1/admin/quota_keys/"+itoa(status.ID)+"/probe_quota", "")
	require.NotEqual(t, http.StatusOK, rec.Code, "快照未提交时 probe 不得返回 200")

	// 数据库仍是旧快照：lookup 的 400/37 没有被当成已提交事实。
	row, err := client.QuotaKey.Get(ctx, status.ID)
	require.NoError(t, err)
	require.JSONEq(t, handlerSnapshotSuccess, string(row.UsageSnapshot))
}

// commitFailingQuotaKeyRepo 让观测快照写入失败，其余能力沿用真实仓储（真实 PostgreSQL）。
//
// 注入点必须**留在真实同 key 事务内**：嵌入真实仓储保证它仍满足 service 的观测窄接口
// （否则 ProbeKeyQuota 会在 asObservationRepo 处失败关闭，测到的就不是 commit 语义）。
// 写入失败使该事务整体回滚、commit 从不发生——这正是"快照未提交"的可观测等价形态：
// HTTP 不得 200，且数据库仍是旧快照。
type commitFailingQuotaKeyRepo struct {
	service.QuotaKeyRepository
}

// quotaKeyTxRepo 是 service 侧的观测窄接口形状（方法集与 service 的窄接口逐字一致）。
// 这里在测试内重声明一次，用于把嵌入的真实仓储断言成事务能力并逐个转发。
type quotaKeyTxRepo interface {
	GetByIDInTx(ctx context.Context, tx *ent.Tx, id int64) (*ent.QuotaKey, error)
	UpdateUsageSnapshot(ctx context.Context, tx *ent.Tx, id int64, snapshot []byte, expiresAt **time.Time) error
	UpdateCredentialsAtomically(ctx context.Context, tx *ent.Tx, id int64, codeEncrypted, codeMAC, codeLast4 string, clearUsageSnapshot bool) error
}

func (r *commitFailingQuotaKeyRepo) txDelegate() quotaKeyTxRepo {
	return r.QuotaKeyRepository.(quotaKeyTxRepo)
}

func (r *commitFailingQuotaKeyRepo) GetByIDInTx(ctx context.Context, tx *ent.Tx, id int64) (*ent.QuotaKey, error) {
	return r.txDelegate().GetByIDInTx(ctx, tx, id)
}

func (r *commitFailingQuotaKeyRepo) UpdateCredentialsAtomically(ctx context.Context, tx *ent.Tx, id int64, codeEncrypted, codeMAC, codeLast4 string, clearUsageSnapshot bool) error {
	return r.txDelegate().UpdateCredentialsAtomically(ctx, tx, id, codeEncrypted, codeMAC, codeLast4, clearUsageSnapshot)
}

// UpdateUsageSnapshot 是唯一注入点：观测快照写失败 → 事务整体回滚 → commit 从不发生。
func (r *commitFailingQuotaKeyRepo) UpdateUsageSnapshot(context.Context, *ent.Tx, int64, []byte, **time.Time) error {
	return errors.New("injected failure: usage snapshot transaction cannot commit")
}

// ---------------------------------------------------------------------------
// 账号列表：批量投影 + lite/full 同值 + §25.7-B 口径
// ---------------------------------------------------------------------------

func setupAccountQuotaUsageRouter(t *testing.T, repo *fakeQuotaKeyRepo) (*gin.Engine, *stubAdminService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminSvc := newStubAdminService()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler.SetQuotaKeyService(newQuotaKeyService(repo, nil))
	router.GET("/api/v1/admin/accounts", handler.List)
	return router, adminSvc
}

func cursorAccountFixture(id int64, quotaKeyID int64, now time.Time) service.Account {
	return service.Account{
		ID: id, Name: "cursor-account", Platform: service.PlatformCursor, Type: "cursor",
		Credentials: map[string]any{"api_key": "sk-cursor-test"},
		Extra:       map[string]any{}, Status: service.StatusActive, Schedulable: true,
		Concurrency: 3, QuotaKeyID: &quotaKeyID, CreatedAt: now, UpdatedAt: now,
	}
}

// TestAccountHandlerListQuotaKeyUsageProjectionLiteFullSameValue 钉死：
//   - 账号 lite/full 行都带「所属激活码额度」投影，且两者取值完全相同（§25.8 验收矩阵）；
//   - 投影按一次批量 summary 得到（4 个账号、2 个激活码 → 只 1 次仓储查询，不逐行查询）；
//   - §25.7-B 口径：账号页只给「供应商账号额度未提供」，不出现余额数值/百分比字段。
func TestAccountHandlerListQuotaKeyUsageProjectionLiteFullSameValue(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(7, "7777", "active", false)
	repo.seed(9, "9999", "active", false)
	seedUsage(repo, 7, handlerSnapshotSuccess)
	seedUsage(repo, 9, handlerSnapshotFailed)

	router, adminSvc := setupAccountQuotaUsageRouter(t, repo)
	now := time.Now().UTC()
	adminSvc.accounts = []service.Account{
		cursorAccountFixture(101, 7, now),
		cursorAccountFixture(102, 7, now), // 同码多账号：必须共用同一份来源快照
		cursorAccountFixture(103, 9, now),
		{ID: 104, Name: "openai-account", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Concurrency: 3, CreatedAt: now, UpdatedAt: now},
	}

	fullItems := listAccountsItems(t, router, "/api/v1/admin/accounts?page=1&page_size=20")
	// 批量：4 行 2 码、整页一次请求只允许一次仓储查询（禁 N+1、禁逐行网络查询）。
	require.Equal(t, 1, repo.listCallCount(), "账号列表的额度投影只允许一次批量查询")
	liteItems := listAccountsItems(t, router, "/api/v1/admin/accounts?page=1&page_size=20&lite=1")
	require.Equal(t, 2, repo.listCallCount(), "lite 请求同样只加一次批量查询（与行数无关）")
	require.Len(t, fullItems, 4)
	require.Len(t, liteItems, 4)

	fullByID := itemsByID(fullItems)
	liteByID := itemsByID(liteItems)

	successUsage, ok := fullByID[float64(101)]["quota_key_usage"].(map[string]any)
	require.True(t, ok, "full 行必须带 quota_key_usage")
	require.Equal(t, float64(400), successUsage["total"])
	require.Equal(t, float64(86), successUsage["used"])
	require.Equal(t, float64(314), successUsage["remaining"])
	require.Equal(t, service.QuotaKeyUsageLabel, successUsage["label"])
	require.Equal(t, service.QuotaKeyUsageScope, successUsage["scope"])
	require.Equal(t, false, successUsage["query_failed"])

	// 同码多账号：共享同一份来源快照（值完全一致）。
	require.Equal(t, fullByID[float64(101)]["quota_key_usage"], fullByID[float64(102)]["quota_key_usage"])

	// 失败态：保留上次成功值 + 查询失败标记。
	failedUsage, ok := fullByID[float64(103)]["quota_key_usage"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, failedUsage["query_failed"])
	require.Equal(t, "lookup_timeout", failedUsage["error_code"])
	require.Equal(t, float64(400), failedUsage["total"])
	require.Equal(t, float64(86), failedUsage["used"])

	// lite 与 full 同值（§25.8）。
	for _, id := range []float64{101, 102, 103} {
		require.Equal(t, fullByID[id]["quota_key_usage"], liteByID[id]["quota_key_usage"],
			"lite/full 的 quota_key_usage 必须同值")
		require.Equal(t, fullByID[id]["vendor_account_quota"], liteByID[id]["vendor_account_quota"],
			"lite/full 的 vendor_account_quota 必须同值")
	}

	// §25.7-B：只给"未提供"注明，绝不伪造供应商账号余额或百分比。
	for _, id := range []float64{101, 102, 103} {
		vendor, ok := fullByID[id]["vendor_account_quota"].(map[string]any)
		require.True(t, ok, "cursor 账号必须带供应商账号额度注明")
		require.Equal(t, false, vendor["provided"])
		require.Equal(t, service.VendorAccountQuotaUnavailableNote, vendor["note"])
		require.Len(t, vendor, 2, "注明结构只允许 provided/note，不得出现余额或百分比字段")
		requireNoFabricatedBalance(t, fullByID[id])
	}

	// 非 Cursor 账号：不写这两维字段（其它平台展示不受影响）。
	require.NotContains(t, fullByID[float64(104)], "quota_key_usage")
	require.NotContains(t, fullByID[float64(104)], "vendor_account_quota")
	require.NotContains(t, liteByID[float64(104)], "vendor_account_quota")
}

// TestLoadQuotaKeySummariesBatchQueryNotPerRow 钉死 loadQuotaKeySummaries 自身：
// 一次调用只查一次仓储，且每行拿到的额度都来自同一批 summary（行数不影响查询次数）。
func TestLoadQuotaKeySummariesBatchQueryNotPerRow(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(7, "7777", "active", false)
	seedUsage(repo, 7, handlerSnapshotSuccess)

	gin.SetMode(gin.TestMode)
	handler := NewAccountHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler.SetQuotaKeyService(newQuotaKeyService(repo, nil))

	ids := []int64{7, 7, 7, 7}
	summaries := handler.loadQuotaKeySummaries(context.Background(), ids)
	require.Equal(t, 1, repo.listCallCount(), "批量 summary 只允许一次仓储查询")
	require.Len(t, summaries, 1)

	summary, ok := summaries[7]
	require.True(t, ok)
	require.NotNil(t, summary.Usage)
	require.NotNil(t, summary.Usage.Total)
	require.Equal(t, 400, *summary.Usage.Total)
	require.Equal(t, "XXXX-****-****-7777", summary.CodeMasked)
}

// TestApplyQuotaKeyLabelProjectionIsPureMemory 钉死 applyQuotaKeyLabel/applyQuotaKeyUsageFields
// 是纯内存投影：不查库、不写伪造数值；无 summary 时只保留"未提供"注明。
func TestApplyQuotaKeyLabelProjectionIsPureMemory(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	repo.seed(7, "7777", "active", false)
	seedUsage(repo, 7, handlerSnapshotSuccess)

	summaries := map[int64]quotaKeySummaryView{
		7: {
			QuotaKey: dto.QuotaKey{ID: 7, CodeMasked: "XXXX-****-****-7777", Status: "active"},
			Usage:    service.BuildQuotaKeyUsageProjection([]byte(handlerSnapshotSuccess)),
		},
	}
	quotaKeyID := int64(7)

	account := &dto.Account{}
	applyQuotaKeyLabel(account, &quotaKeyID, summaries)
	require.NotNil(t, account.QuotaKeyID)
	require.Equal(t, int64(7), *account.QuotaKeyID)
	require.NotNil(t, account.QuotaKeyLabel)
	require.Equal(t, "XXXX-****-****-7777", *account.QuotaKeyLabel, "标签只用脱敏末4位")

	var fields quotaKeyUsageFields
	applyQuotaKeyUsageFields(&fields, &quotaKeyID, summaries)
	require.NotNil(t, fields.QuotaKeyUsage)
	require.NotNil(t, fields.QuotaKeyUsage.Total)
	require.Equal(t, 400, *fields.QuotaKeyUsage.Total)
	require.NotNil(t, fields.VendorAccountQuota)
	require.False(t, fields.VendorAccountQuota.Provided)
	require.Equal(t, service.VendorAccountQuotaUnavailableNote, fields.VendorAccountQuota.Note)

	// 降级：summary 取不到时仍给"未提供"注明，且不出现任何额度数值（缺失不是 0）。
	var degraded quotaKeyUsageFields
	applyQuotaKeyUsageFields(&degraded, &quotaKeyID, nil)
	require.Nil(t, degraded.QuotaKeyUsage)
	require.NotNil(t, degraded.VendorAccountQuota)
	require.False(t, degraded.VendorAccountQuota.Provided)

	// 无归属激活码 → 两维都不写。
	var untouched quotaKeyUsageFields
	applyQuotaKeyUsageFields(&untouched, nil, summaries)
	require.Nil(t, untouched.QuotaKeyUsage)
	require.Nil(t, untouched.VendorAccountQuota)

	// 仓库查询次数不因这些纯内存投影增加。
	require.Equal(t, 0, repo.listCallCount(), "纯内存投影不得触发任何仓储查询")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func listAccountsItems(t *testing.T, router *gin.Engine, path string) []map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	return payload.Data.Items
}

func itemsByID(items []map[string]any) map[float64]map[string]any {
	out := make(map[float64]map[string]any, len(items))
	for _, item := range items {
		id, ok := item["id"].(float64)
		if !ok {
			continue
		}
		out[id] = item
	}
	return out
}

// requireNoFabricatedBalance 逐层检查账号行：任何 key 都不得表达"供应商余额/百分比"，
// 任何数值也不得当成本站额度（§25.7-B：不得显示伪造余额或百分比）。
func requireNoFabricatedBalance(t *testing.T, item map[string]any) {
	t.Helper()
	for key, value := range item {
		switch key {
		case "quota_key_usage", "vendor_account_quota":
			// 这两维是明确口径的展示字段：quota_key_usage 带"所属激活码额度"标签，
			// vendor_account_quota 只有 provided=false + note。
			continue
		}
		lower := strings.ToLower(key)
		require.NotContains(t, lower, "percent", "账号行不得出现百分比字段: %s", key)
		require.NotContains(t, lower, "vendor_balance", "账号行不得出现供应商余额字段: %s", key)
		if nested, ok := value.(map[string]any); ok {
			requireNoFabricatedBalance(t, nested)
		}
	}
}
