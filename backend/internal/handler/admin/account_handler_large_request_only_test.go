//go:build unit

package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupRequestSizeScopeRouter() (*gin.Engine, *stubAdminService) {
	gin.SetMode(gin.TestMode)
	adminSvc := newStubAdminService()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	router := gin.New()
	router.POST("/api/v1/admin/accounts/:id/request-size-scope", handler.SetRequestSizeScope)
	return router, adminSvc
}

func postRequestSizeScope(t *testing.T, router *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestSetRequestSizeScopeLargeOnly(t *testing.T) {
	router, adminSvc := setupRequestSizeScopeRouter()

	recorder := postRequestSizeScope(t, router, "/api/v1/admin/accounts/3/request-size-scope", `{"scope":"large_only"}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, adminSvc.setLargeRequestOnlyCalls)
	require.Equal(t, int64(3), adminSvc.lastSetLargeRequestOnly.id)
	require.Equal(t, service.RequestSizeScopeLargeOnly, adminSvc.lastSetLargeRequestOnly.scope)

	var body struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, int64(3), body.Data.ID)
}

func TestSetRequestSizeScopeSmallOnly(t *testing.T) {
	router, adminSvc := setupRequestSizeScopeRouter()

	recorder := postRequestSizeScope(t, router, "/api/v1/admin/accounts/3/request-size-scope", `{"scope":"small_only"}`)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, adminSvc.setLargeRequestOnlyCalls)
	require.Equal(t, service.RequestSizeScopeSmallOnly, adminSvc.lastSetLargeRequestOnly.scope)
}

func TestSetRequestSizeScopeAny(t *testing.T) {
	router, adminSvc := setupRequestSizeScopeRouter()

	recorder := postRequestSizeScope(t, router, "/api/v1/admin/accounts/3/request-size-scope", `{"scope":"any"}`)

	// "any" 是合法取值（清除限制），必须真的调到服务方法。
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, adminSvc.setLargeRequestOnlyCalls)
	require.Equal(t, int64(3), adminSvc.lastSetLargeRequestOnly.id)
	require.Equal(t, service.RequestSizeScopeAny, adminSvc.lastSetLargeRequestOnly.scope)
}

func TestSetRequestSizeScopePoolExhaustedReturns400(t *testing.T) {
	router, adminSvc := setupRequestSizeScopeRouter()
	adminSvc.setLargeRequestOnlyErr = infraerrors.Newf(http.StatusBadRequest,
		"REQUEST_SIZE_SCOPE_POOL_EXHAUSTED",
		"group %d platform %s would have no schedulable account left for small requests", 2, "openai")

	recorder := postRequestSizeScope(t, router, "/api/v1/admin/accounts/3/request-size-scope", `{"scope":"large_only"}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var body struct {
		Code    int    `json:"code"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "REQUEST_SIZE_SCOPE_POOL_EXHAUSTED", body.Reason)
	require.Equal(t, http.StatusBadRequest, body.Code)
	require.Contains(t, body.Message, "no schedulable account left")
}

func TestSetRequestSizeScopeRequiresScope(t *testing.T) {
	router, adminSvc := setupRequestSizeScopeRouter()

	recorder := postRequestSizeScope(t, router, "/api/v1/admin/accounts/3/request-size-scope", `{}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, 0, adminSvc.setLargeRequestOnlyCalls)
}

func TestSetRequestSizeScopeRejectsUnknownScope(t *testing.T) {
	router, adminSvc := setupRequestSizeScopeRouter()

	recorder := postRequestSizeScope(t, router, "/api/v1/admin/accounts/3/request-size-scope", `{"scope":"medium_only"}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, 0, adminSvc.setLargeRequestOnlyCalls)
}

func TestSetRequestSizeScopeRejectsInvalidID(t *testing.T) {
	router, adminSvc := setupRequestSizeScopeRouter()

	recorder := postRequestSizeScope(t, router, "/api/v1/admin/accounts/not-an-id/request-size-scope", `{"scope":"large_only"}`)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, 0, adminSvc.setLargeRequestOnlyCalls)
}
