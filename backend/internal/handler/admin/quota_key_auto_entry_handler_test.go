//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// setupAutoEntryRouter 以 sqlite 内存库 + 真实 setting/quota_key 仓储装配含 D2-a 新端点的路由。
// 与 registerQuotaKeyRoutes 同序：auto_entry_config 静态路径先于 :id 注册，避免被 :id 通配吞掉。
// 返回 router、autoEntry 服务与底层 ent client（供测试在同一实例上播种/断言数据）。
func setupAutoEntryRouter(t *testing.T, httpDoer service.HTTPDoer, lookupBaseURL string) (*gin.Engine, *service.CursorAutoEntryService, *ent.Client) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	client := newSQLiteEntClient(t)
	settingRepo := repository.NewSettingRepository(client)
	quotaKeyRepo := repository.NewQuotaKeyRepository(client)
	autoEntry := service.NewCursorAutoEntryService(settingRepo, quotaKeyRepo, service.NewCredcryptEncryptor(), client, httpDoer, lookupBaseURL)
	h := NewQuotaKeyHandler(nil, nil, autoEntry)

	router := gin.New()
	qk := router.Group("/api/v1/admin/quota_keys")
	qk.GET("/auto_entry_config", h.GetAutoEntryConfig)
	qk.PUT("/auto_entry_config", h.UpdateAutoEntryConfig)
	qk.POST("/:id/probe_quota", h.ProbeQuota)
	return router, autoEntry, client
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestAutoEntryConfig_GetDefault_WhenMissing(t *testing.T) {
	router, _, _ := setupAutoEntryRouter(t, nil, "")
	rec := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys/auto_entry_config", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Data struct {
			Vendors       map[string]bool `json:"vendors"`
			EntryDefaults struct {
				Priority       int     `json:"priority"`
				RateMultiplier float64 `json:"rate_multiplier"`
			} `json:"entry_defaults"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, 1, body.Data.EntryDefaults.Priority)
	require.Equal(t, 1.0, body.Data.EntryDefaults.RateMultiplier)
	require.True(t, body.Data.Vendors[service.CursorVendorOpenAI])
}

func TestAutoEntryConfig_UpdateAndGetRoundTrip(t *testing.T) {
	router, _, _ := setupAutoEntryRouter(t, nil, "")
	body := `{"vendors":{"openai":true,"anthropic":false},"models":[{"id":"gpt-4.1","vendor":"openai","probe_status":""}],"entry_defaults":{"priority":1,"rate_multiplier":1.0}}`
	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/auto_entry_config", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec2 := doQuotaKeyJSON(router, http.MethodGet, "/api/v1/admin/quota_keys/auto_entry_config", "")
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())
	var getBody struct {
		Data struct {
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &getBody))
	require.Len(t, getBody.Data.Models, 1)
	require.Equal(t, "gpt-4.1", getBody.Data.Models[0].ID)
}

func TestAutoEntryConfig_UpdateRejectsDisabledVendorModel(t *testing.T) {
	router, _, _ := setupAutoEntryRouter(t, nil, "")
	body := `{"vendors":{"openai":false},"models":[{"id":"gpt-4.1","vendor":"openai"}],"entry_defaults":{"priority":1,"rate_multiplier":1.0}}`
	rec := doQuotaKeyJSON(router, http.MethodPut, "/api/v1/admin/quota_keys/auto_entry_config", body)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestProbeQuota_WritesSnapshotToAccounts(t *testing.T) {
	var gotKey, gotDeviceID, gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("key")
		gotDeviceID = r.Header.Get("device-id")
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"key_info":{` +
			`"group":"Pro","key_status_code":1,"quota_key_max_quota":400,` +
			`"quota_key_used_quota":37,"device_count":1,"device_max_count":3,` +
			`"mode":1,"expired_at":"2026-12-31T00:00:00Z"}}}`))
	}))
	defer srv.Close()

	router, _, client := setupAutoEntryRouter(t, &http.Client{Timeout: 5 * time.Second}, srv.URL)

	// 在 sqlite 中建一个激活码（明文加密落库）与两个账号。
	qkSvc := service.NewQuotaKeyService(repository.NewQuotaKeyRepository(client), service.NewCredcryptEncryptor(), nil, client)
	st, err := qkSvc.Create(context.Background(), "JG-CF-PROBE-TEST", nil, nil)
	require.NoError(t, err)
	qkID := st.ID
	for i := 0; i < 2; i++ {
		_, err := client.Account.Create().
			SetName("acc").
			SetPlatform("cursor").
			SetType("cursor").
			SetQuotaKeyID(qkID).
			SetConcurrency(3).
			SetPriority(1).
			SetRateMultiplier(1.0).
			SetExtra(map[string]any{}).
			Save(context.Background())
		require.NoError(t, err)
	}

	rec := doQuotaKeyJSON(router, http.MethodPost, "/api/v1/admin/quota_keys/"+itoa(qkID)+"/probe_quota", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Data struct {
			Max             int `json:"max"`
			Used            int `json:"used"`
			StatusCode      int `json:"status_code"`
			AccountsWritten int `json:"accounts_written"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 400, resp.Data.Max)
	require.Equal(t, 37, resp.Data.Used)
	require.Equal(t, 2, resp.Data.AccountsWritten)

	// 断言真实请求头：解密后的明文 code 作为 key、DJB2 device-id、浏览器 UA。
	require.Equal(t, "/lookup", gotPath)
	require.Equal(t, "JG-CF-PROBE-TEST", gotKey)
	require.Len(t, gotDeviceID, 8)
	require.NotEmpty(t, gotUA, "lookup 请求必须携带浏览器 UA")

	// 断言 snapshot 已写入两个账号的 extra。
	accounts, err := client.Account.Query().Where(account.QuotaKeyID(qkID)).All(context.Background())
	require.NoError(t, err)
	require.Len(t, accounts, 2)
	for _, a := range accounts {
		snap, ok := a.Extra[service.CursorKeyQuotaSnapshotExtraKey].(map[string]any)
		require.True(t, ok, "extra 中应有 key_quota_snapshot")
		require.Equal(t, float64(37), snap["used"])
		require.Equal(t, float64(400), snap["max"])
	}
}
