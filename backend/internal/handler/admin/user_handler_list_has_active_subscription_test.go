package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// hasActiveSubscriptionFilterStub 捕获传入 ListUsers 的 filters，其余 AdminService 方法走 baseline stub。
type hasActiveSubscriptionFilterStub struct {
	service.AdminService
	captured service.UserListFilters
}

func (s *hasActiveSubscriptionFilterStub) ListUsers(_ context.Context, _, _ int, filters service.UserListFilters, _, _ string) ([]service.User, int64, error) {
	s.captured = filters
	return []service.User{}, 0, nil
}

// TestAdminUserList_ParsesHasActiveSubscription 校验 has_active_subscription 查询参数解析：
// "true"/"1"→指向 true 的指针、"false"/"0"→指向 false 的指针、
// 非法值/空/缺省→nil（不过滤，向后兼容）。
func TestAdminUserList_ParsesHasActiveSubscription(t *testing.T) {
	gin.SetMode(gin.TestMode)

	wantTrue := true
	wantFalse := false

	cases := []struct {
		name  string
		query string
		want  *bool
	}{
		{"true filters to users with active subscription", "?has_active_subscription=true", &wantTrue},
		{"false filters to users without active subscription", "?has_active_subscription=false", &wantFalse},
		{"1 treated as true", "?has_active_subscription=1", &wantTrue},
		{"0 treated as false", "?has_active_subscription=0", &wantFalse},
		{"invalid value ignored", "?has_active_subscription=abc", nil},
		{"missing means no filter", "", nil},
		{"empty means no filter", "?has_active_subscription=", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &hasActiveSubscriptionFilterStub{AdminService: newStubAdminService()}
			r := gin.New()
			h := NewUserHandler(stub, nil, nil, nil, nil, nil, nil)
			r.GET("/admin/users", h.List)

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodGet, "/admin/users"+tc.query, nil)
			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			if tc.want == nil {
				require.Nil(t, stub.captured.HasActiveSubscription)
			} else {
				require.NotNil(t, stub.captured.HasActiveSubscription)
				require.Equal(t, *tc.want, *stub.captured.HasActiveSubscription)
			}
		})
	}
}
