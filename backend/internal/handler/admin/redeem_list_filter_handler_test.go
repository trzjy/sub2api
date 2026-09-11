package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupRedeemListRouter() (*gin.Engine, *stubAdminService) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminSvc := newStubAdminService()

	h := NewRedeemHandler(adminSvc, nil)
	router.GET("/api/v1/admin/redeem-codes", h.List)
	router.GET("/api/v1/admin/redeem-codes/values", h.ListValues)
	return router, adminSvc
}

func TestRedeemListValueFilter(t *testing.T) {
	t.Run("value 参数解析为 float 指针并透传", func(t *testing.T) {
		router, adminSvc := setupRedeemListRouter()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/redeem-codes?type=balance&value=10", nil)
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)

		require.Equal(t, 1, adminSvc.lastListRedeemCodes.calls)
		require.NotNil(t, adminSvc.lastListRedeemCodes.value)
		require.InDelta(t, 10.0, *adminSvc.lastListRedeemCodes.value, 1e-9)
		require.Equal(t, "balance", adminSvc.lastListRedeemCodes.codeType)
	})

	t.Run("缺省 value 时透传 nil", func(t *testing.T) {
		router, adminSvc := setupRedeemListRouter()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/redeem-codes", nil)
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)

		require.Nil(t, adminSvc.lastListRedeemCodes.value)
	})

	t.Run("非法 value 返回 400", func(t *testing.T) {
		router, _ := setupRedeemListRouter()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/redeem-codes?value=abc", nil)
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("负数 value 返回 400", func(t *testing.T) {
		router, _ := setupRedeemListRouter()

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/redeem-codes?value=-1", nil)
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

func TestRedeemListValues(t *testing.T) {
	router, _ := setupRedeemListRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/redeem-codes/values?type=balance", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Data struct {
			Values []float64 `json:"values"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(t, body.Data.Values)
	require.Empty(t, body.Data.Values)
}
