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
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
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

// TestQuotaKeyHandler_Probe_UsageProjectionFromCommittedSnapshot 钉死 probe 返回带投影，
// 且投影取自**已提交**快照列（本单只读：probe 不把 lookup 结果冒充已提交事实）。
func TestQuotaKeyHandler_Probe_UsageProjectionFromCommittedSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{` +
			`"group":"Pro","key_status_code":1,"quota_key_max_quota":400,` +
			`"quota_key_used_quota":37,"device_count":1,"device_max_count":3,` +
			`"mode":1,"expired_at":"2026-12-31T00:00:00Z"}}}`))
	}))
	defer srv.Close()

	gin.SetMode(gin.TestMode)
	client := newSQLiteEntClient(t)
	settingRepo := repository.NewSettingRepository(client)
	quotaKeyRepo := repository.NewQuotaKeyRepository(client)
	autoEntry := service.NewCursorAutoEntryService(settingRepo, quotaKeyRepo, service.NewCredcryptEncryptor(), client, &http.Client{Timeout: 5 * time.Second}, srv.URL)
	svc := service.NewQuotaKeyService(quotaKeyRepo, service.NewCredcryptEncryptor(), nil, client)
	handler := NewQuotaKeyHandler(svc, nil, autoEntry)

	router := gin.New()
	qk := router.Group("/api/v1/admin/quota_keys")
	qk.POST("/:id/probe_quota", handler.ProbeQuota)

	ctx := context.Background()
	status, err := svc.Create(ctx, "JG-CF-PROBE-SNAPSHOT", nil, nil)
	require.NoError(t, err)

	// 预置"已提交"快照（写入者由后续单落地，本单只读）。
	_, err = client.QuotaKey.UpdateOneID(status.ID).
		SetUsageSnapshot(jsontext.Value(handlerSnapshotSuccess)).
		Save(ctx)
	require.NoError(t, err)

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/"+itoa(status.ID)+"/probe_quota", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

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
	// 新增投影来自已提交快照（400/86），不是本次 lookup 的 400/37。
	require.NotNil(t, payload.Data.Usage, "probe 响应必须带 usage 投影")
	require.NotNil(t, payload.Data.Usage.Total)
	require.Equal(t, 400, *payload.Data.Usage.Total)
	require.NotNil(t, payload.Data.Usage.Used)
	require.Equal(t, 86, *payload.Data.Usage.Used)
	require.NotNil(t, payload.Data.Usage.Remaining)
	require.Equal(t, 314, *payload.Data.Usage.Remaining)
	require.Equal(t, service.QuotaKeyUsageLabel, payload.Data.Usage.Label)
	require.False(t, payload.Data.Usage.Failed)

	// 只读验证：probe 未改写已提交快照列（写入者归后续单）。
	row, err := client.QuotaKey.Get(ctx, status.ID)
	require.NoError(t, err)
	require.JSONEq(t, handlerSnapshotSuccess, string(row.UsageSnapshot))
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
