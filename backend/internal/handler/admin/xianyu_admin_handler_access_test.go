package admin

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestXianyuAccessGrantsAdminRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewXianyuAdminHandler(nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/access", nil)
	ctx.Set(string(middleware.ContextKeyUserRole), string(middleware.ContextKeyUserRoleAdmin))

	handler.Access(ctx)

	require.Equal(t, 200, recorder.Code)
	var body struct {
		Data struct {
			CanManage bool `json:"can_manage"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.True(t, body.Data.CanManage)
}

func TestXianyuAccessRejectsNonAdminRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewXianyuAdminHandler(nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/access", nil)
	ctx.Set(string(middleware.ContextKeyUserRole), string(middleware.ContextKeyUserRoleUser))

	handler.Access(ctx)

	require.Equal(t, 200, recorder.Code)
	var body struct {
		Data struct {
			CanManage bool `json:"can_manage"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.False(t, body.Data.CanManage)
}
