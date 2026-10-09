package admin

// DTO 集成测试（方案 §3.4 / C5 卡硬要求）：种子账号带已知 extra → 经 admin account
// handler List 的真实 HTTP 响应 JSON，断言 quota_dimensions / balance_low 字段在位且值正确。
//
// 复用既有 handler 测试基建（setupAccountListRouter + stubAdminService），不新建基建。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountHandlerListIncludesQuotaDimensionsAndBalanceLow(t *testing.T) {
	router, adminSvc := setupAccountListRouter()
	now := time.Now().UTC()
	fetched := now.Add(-1 * time.Minute).Format(time.RFC3339)

	adminSvc.accounts = []service.Account{{
		ID: 701, Name: "kira-relay", Platform: service.PlatformOther, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 4,
		Credentials: map[string]any{"api_key": "kira-key", "base_url": "https://kiraai.vn/api/v1"},
		Extra: map[string]any{
			"kira_usage_snapshot":      map[string]any{"used_percent": 10.0, "fetched_at": fetched},
			"other_balance":            0.0,
			"other_balance_updated_at": fetched,
			"other_balance_low":        true,
		},
		CreatedAt: now, UpdatedAt: now,
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var payload struct {
		Data struct {
			Items []struct {
				ID            int64 `json:"id"`
				BalanceLow    bool  `json:"balance_low"`
				QuotaDimsJSON []struct {
					Kind       string `json:"kind"`
					Scope      string `json:"scope"`
					Target     string `json:"target"`
					Status     string `json:"status"`
					Servable   string `json:"servable"`
					Source     string `json:"source"`
					ObservedAt string `json:"observed_at"`
				} `json:"quota_dimensions"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)

	item := payload.Data.Items[0]
	require.Equal(t, int64(701), item.ID)
	require.True(t, item.BalanceLow, "balance_low 标记应经 handler 响应暴露")

	require.Len(t, item.QuotaDimsJSON, 2)
	free := item.QuotaDimsJSON[0]
	require.Equal(t, "free", free.Kind)
	require.Equal(t, "account", free.Scope)
	require.Equal(t, "remaining", free.Status)
	require.Equal(t, "no", free.Servable)
	require.Equal(t, "kira_usage_snapshot", free.Source)
	require.Equal(t, fetched, free.ObservedAt)

	paid := item.QuotaDimsJSON[1]
	require.Equal(t, "paid", paid.Kind)
	require.Equal(t, "account", paid.Scope)
	require.Equal(t, "exhausted", paid.Status)
	require.Equal(t, "kira_vnd_balance", paid.Source)
	require.Equal(t, fetched, paid.ObservedAt)
}

func TestAccountHandlerListOmitsQuotaDimensionsForSnapshotlessAccount(t *testing.T) {
	router, adminSvc := setupAccountListRouter()
	now := time.Now().UTC()
	adminSvc.accounts = []service.Account{{
		ID: 702, Name: "anthropic-oauth", Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, Concurrency: 4,
		Credentials: map[string]any{"access_token": "t"},
		Extra:       map[string]any{},
		CreatedAt:   now, UpdatedAt: now,
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)
	require.NotContains(t, payload.Data.Items[0], "quota_dimensions")
	require.NotContains(t, payload.Data.Items[0], "balance_low")
}

// TestAccountHandlerListLiteIncludesQuotaDimensions 覆盖 lite=1 紧凑 DTO 必须携带与完整
// DTO 同字段名（quota_dimensions / balance_low）、同语义的额度状态（方案 §3.4 / C5）。
func TestAccountHandlerListLiteIncludesQuotaDimensions(t *testing.T) {
	router, adminSvc := setupAccountListRouter()
	now := time.Now().UTC()
	fetched := now.Add(-1 * time.Minute).Format(time.RFC3339)

	adminSvc.accounts = []service.Account{{
		ID: 701, Name: "kira-relay", Platform: service.PlatformOther, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 4,
		Credentials: map[string]any{"api_key": "kira-key", "base_url": "https://kiraai.vn/api/v1"},
		Extra: map[string]any{
			"kira_usage_snapshot":      map[string]any{"used_percent": 10.0, "fetched_at": fetched},
			"other_balance":            0.0,
			"other_balance_updated_at": fetched,
			"other_balance_low":        true,
		},
		CreatedAt: now, UpdatedAt: now,
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEmpty(t, rec.Header().Get("ETag"))

	var payload struct {
		Data struct {
			Items []struct {
				ID            int64 `json:"id"`
				BalanceLow    bool  `json:"balance_low"`
				QuotaDimsJSON []struct {
					Kind       string `json:"kind"`
					Scope      string `json:"scope"`
					Target     string `json:"target"`
					Status     string `json:"status"`
					Servable   string `json:"servable"`
					Source     string `json:"source"`
					ObservedAt string `json:"observed_at"`
				} `json:"quota_dimensions"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)

	item := payload.Data.Items[0]
	require.Equal(t, int64(701), item.ID)
	require.True(t, item.BalanceLow, "lite=1 须暴露 balance_low 标记")

	require.Len(t, item.QuotaDimsJSON, 2, "lite=1 须暴露与完整 DTO 同形的 quota_dimensions")
	free := item.QuotaDimsJSON[0]
	require.Equal(t, "free", free.Kind)
	require.Equal(t, "account", free.Scope)
	require.Equal(t, "remaining", free.Status)
	require.Equal(t, "no", free.Servable)
	require.Equal(t, "kira_usage_snapshot", free.Source)
	require.Equal(t, fetched, free.ObservedAt)

	paid := item.QuotaDimsJSON[1]
	require.Equal(t, "paid", paid.Kind)
	require.Equal(t, "account", paid.Scope)
	require.Equal(t, "exhausted", paid.Status)
	require.Equal(t, "kira_vnd_balance", paid.Source)
	require.Equal(t, fetched, paid.ObservedAt)
}

// TestAccountHandlerListLiteOmitsQuotaDimensions 覆盖 lite=1 在快照缺失账号上仍保持
// omitempty 契约：quota_dimensions / balance_low 缺省时不应出现在紧凑 DTO 中（与既有
// 完整 DTO 行为一致，不改变 snapshotless 语义）。
func TestAccountHandlerListLiteOmitsQuotaDimensions(t *testing.T) {
	router, adminSvc := setupAccountListRouter()
	now := time.Now().UTC()
	adminSvc.accounts = []service.Account{{
		ID: 702, Name: "anthropic-oauth", Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, Concurrency: 4,
		Credentials: map[string]any{"access_token": "t"},
		Extra:       map[string]any{},
		CreatedAt:   now, UpdatedAt: now,
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?page=1&page_size=20&lite=1", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)
	require.NotContains(t, payload.Data.Items[0], "quota_dimensions")
	require.NotContains(t, payload.Data.Items[0], "balance_low")
}
