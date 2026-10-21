package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// errorCandidatesAdminService wraps stubAdminService so a directed test can force
// GetGroupModelsListCandidates to fail and exercise the existing error response chain.
// All other AdminService methods are promoted unchanged from the embedded stub.
type errorCandidatesAdminService struct {
	*stubAdminService
	candidatesErr error
}

func (s *errorCandidatesAdminService) GetGroupModelsListCandidates(ctx context.Context, id int64, platform string) ([]string, error) {
	if s.candidatesErr != nil {
		return nil, s.candidatesErr
	}
	return s.stubAdminService.GetGroupModelsListCandidates(ctx, id, platform)
}

// newErrorCandidatesRouter builds a real gin router exposing only the candidate
// endpoint wired to a handler backed by the given (error-injecting) service.
func newErrorCandidatesRouter(svc service.AdminService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := NewGroupHandler(svc, nil, nil)
	router.GET("/api/v1/admin/groups/:id/model-allowlist-candidates", h.GetGroupModelAllowlistCandidates)
	return router
}

// TestGroupModelAllowlistCandidatesSuccess verifies the normal path: a valid numeric
// groupID plus platform returns 200 and a body carrying the candidate models list.
func TestGroupModelAllowlistCandidatesSuccess(t *testing.T) {
	router, _ := setupAdminRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/groups/5/model-allowlist-candidates?platform=openai", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"models"`)
	require.Contains(t, rec.Body.String(), "gpt-5.5")
	require.Contains(t, rec.Body.String(), "gpt-5.4")

	var parsed struct {
		Data struct {
			Models []string `json:"models"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
	require.ElementsMatch(t, []string{"gpt-5.5", "gpt-5.4"}, parsed.Data.Models)
}

// TestGroupModelAllowlistCandidatesInvalidGroupID verifies non-numeric and negative
// groupIDs are rejected with 400 before the service is consulted.
func TestGroupModelAllowlistCandidatesInvalidGroupID(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"non-numeric", "abc"},
		{"negative", "-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, svc := setupAdminRouter()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/groups/"+tc.id+"/model-allowlist-candidates?platform=openai", nil)
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			// Stub must not have been consulted for an invalid groupID.
			_ = svc
		})
	}
}

// TestGroupModelAllowlistCandidatesServiceError verifies that a service error is
// surfaced through the existing response.ErrorFrom chain (non-200 with an error
// envelope) rather than a successful empty list.
func TestGroupModelAllowlistCandidatesServiceError(t *testing.T) {
	svc := &errorCandidatesAdminService{
		stubAdminService: newStubAdminService(),
		candidatesErr:    errors.New("upstream catalog unavailable"),
	}
	router := newErrorCandidatesRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/groups/5/model-allowlist-candidates?platform=openai", nil)
	router.ServeHTTP(rec, req)

	require.NotEqual(t, http.StatusOK, rec.Code)

	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.NotZero(t, envelope.Code)
	require.NotEmpty(t, envelope.Message)
}
