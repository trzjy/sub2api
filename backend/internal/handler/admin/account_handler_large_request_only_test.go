//go:build unit

package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupLargeRequestOnlyRouter() (*gin.Engine, *stubAdminService) {
	gin.SetMode(gin.TestMode)
	adminSvc := newStubAdminService()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	router := gin.New()
	router.POST("/api/v1/admin/accounts/:id/large-request-only", handler.SetLargeRequestOnly)
	return router, adminSvc
}

func TestSetLargeRequestOnlyEnabled(t *testing.T) {
	router, adminSvc := setupLargeRequestOnlyRouter()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/3/large-request-only", bytes.NewBufferString(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, adminSvc.setLargeRequestOnlyCalls)
	require.Equal(t, int64(3), adminSvc.lastSetLargeRequestOnly.id)
	require.True(t, adminSvc.lastSetLargeRequestOnly.enabled)

	var body struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, int64(3), body.Data.ID)
}

func TestSetLargeRequestOnlyDisabled(t *testing.T) {
	router, adminSvc := setupLargeRequestOnlyRouter()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/3/large-request-only", bytes.NewBufferString(`{"enabled":false}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	// binding:"required" 对 *bool 只保证非 nil，false 合法：必须真的调到服务方法。
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, adminSvc.setLargeRequestOnlyCalls)
	require.Equal(t, int64(3), adminSvc.lastSetLargeRequestOnly.id)
	require.False(t, adminSvc.lastSetLargeRequestOnly.enabled)
}

func TestSetLargeRequestOnlyPoolExhaustedReturns400(t *testing.T) {
	router, adminSvc := setupLargeRequestOnlyRouter()
	adminSvc.setLargeRequestOnlyErr = infraerrors.Newf(http.StatusBadRequest,
		"LARGE_REQUEST_ONLY_POOL_EXHAUSTED",
		"group %d platform %s would have no schedulable account left for small requests", 2, "openai")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/3/large-request-only", bytes.NewBufferString(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var body struct {
		Code    int    `json:"code"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "LARGE_REQUEST_ONLY_POOL_EXHAUSTED", body.Reason)
	require.Equal(t, http.StatusBadRequest, body.Code)
	require.Contains(t, body.Message, "no schedulable account left")
}

func TestSetLargeRequestOnlyRequiresEnabled(t *testing.T) {
	router, _ := setupLargeRequestOnlyRouter()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/3/large-request-only", bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestSetLargeRequestOnlyRejectsInvalidID(t *testing.T) {
	router, _ := setupLargeRequestOnlyRouter()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/not-an-id/large-request-only", bytes.NewBufferString(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}
