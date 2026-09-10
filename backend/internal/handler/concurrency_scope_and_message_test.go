package handler

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newScopeTestContext 构造一个带有指定 group 的 gin.Context（group 由认证中间件写入
// ctxkey.Group，resolveConcurrencyScope 必须从此处读取"认证时刻分组"）。
func newScopeTestContext(group *service.Group) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	if group != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	}
	c.Request = req
	return c
}

func validSubscriptionGroup(concurrency int) *service.Group {
	return &service.Group{
		ID:              7,
		Name:            "sub-group",
		Platform:        "anthropic",
		Status:          service.StatusActive,
		Hydrated:        true,
		SubscriptionType: service.SubscriptionTypeSubscription,
		Concurrency:     concurrency,
	}
}

func validMeteringGroup() *service.Group {
	return &service.Group{
		ID:              9,
		Name:            "meter-group",
		Platform:        "anthropic",
		Status:          service.StatusActive,
		Hydrated:        true,
		SubscriptionType: service.SubscriptionTypeStandard,
		Concurrency:     0,
	}
}

func TestResolveConcurrencyScope(t *testing.T) {
	sub := &service.UserSubscription{}

	tests := []struct {
		name        string
		group       *service.Group
		subscription *service.UserSubscription
		wantKind    string
		wantMax     int
		wantGroupID int64
		wantWord    string
	}{
		{
			name:        "subscription group with subscription resolves to group scope",
			group:       validSubscriptionGroup(3),
			subscription: sub,
			wantKind:    "group",
			wantMax:     3,
			wantGroupID: 7,
			wantWord:    "订阅",
		},
		{
			name:        "metering group resolves to user scope even with subscription",
			group:       validMeteringGroup(),
			subscription: sub,
			wantKind:    "user",
			wantMax:     5,
			wantGroupID: 0,
			wantWord:    "用户",
		},
		{
			name:        "subscription group without subscription resolves to user scope",
			group:       validSubscriptionGroup(3),
			subscription: nil,
			wantKind:    "user",
			wantMax:     5,
			wantGroupID: 0,
			wantWord:    "用户",
		},
		{
			name:        "no group in context resolves to user scope",
			group:       nil,
			subscription: sub,
			wantKind:    "user",
			wantMax:     5,
			wantGroupID: 0,
			wantWord:    "用户",
		},
		{
			name: "invalid (non-hydrated) subscription group resolves to user scope",
			group: &service.Group{
				ID:               7,
				SubscriptionType: service.SubscriptionTypeSubscription,
				// 缺少 Hydrated/Platform/Status → IsGroupContextValid 为 false
			},
			subscription: sub,
			wantKind:    "user",
			wantMax:     5,
			wantGroupID: 0,
			wantWord:    "用户",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newScopeTestContext(tt.group)
			subject := middleware.AuthSubject{UserID: 1, Concurrency: tt.wantMax}
			scope := resolveConcurrencyScope(c, subject, tt.subscription)
			require.Equal(t, tt.wantKind, scope.Kind)
			require.Equal(t, tt.wantMax, scope.Max)
			require.Equal(t, tt.wantGroupID, scope.GroupID)
			require.Equal(t, tt.wantWord, scope.ScopeWord)
		})
	}
}

func TestRenderConcurrencyLimitMessage(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		kind string
		limit int
		want string
	}{
		{
			name:  "empty template falls back to english byte-for-byte (user)",
			tmpl:  "",
			kind:  "user",
			limit: 5,
			want:  "Concurrency limit exceeded for user, please retry later",
		},
		{
			name:  "whitespace-only template falls back to english (group)",
			tmpl:  "   ",
			kind:  "group",
			limit: 0,
			want:  "Concurrency limit exceeded for group, please retry later",
		},
		{
			name:  "user scope and limit both substituted",
			tmpl:  "当前{scope}并发上限为{limit}",
			kind:  "user",
			limit: 5,
			want:  "当前用户并发上限为5",
		},
		{
			name:  "group scope and limit both substituted",
			tmpl:  "当前{scope}并发上限为{limit}",
			kind:  "group",
			limit: 3,
			want:  "当前订阅并发上限为3",
		},
		{
			name:  "account dimension never renders custom template",
			tmpl:  "自定义{s Limit}",
			kind:  "account",
			limit: 5,
			want:  "Concurrency limit exceeded for account, please retry later",
		},
		{
			name:  "no limit with {limit} template falls back to english (user), no bare placeholder",
			tmpl:  "上限为{limit}",
			kind:  "user",
			limit: 0,
			want:  "Concurrency limit exceeded for user, please retry later",
		},
		{
			name:  "no limit with {limit} template falls back to english (group), no bare placeholder",
			tmpl:  "上限为{limit}",
			kind:  "group",
			limit: 0,
			want:  "Concurrency limit exceeded for group, please retry later",
		},
		{
			name:  "other braces preserved verbatim",
			tmpl:  "您已达到 {scope} 的 {limit} 限制 {keep}",
			kind:  "user",
			limit: 2,
			want:  "您已达到 用户 的 2 限制 {keep}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderConcurrencyLimitMessage(tt.tmpl, tt.kind, tt.limit)
			require.Equal(t, tt.want, got)
			// 任何情况下都不应出现裸 {limit} / 裸 {scope}
			require.NotContains(t, got, "{limit}")
			require.NotContains(t, got, "{scope}")
		})
	}
}

// TestAcquireScopedUserSlotWithWait_GroupScopeBypassesUserConcurrency 验证订阅分组请求按
// group 维度占槽：即便用户全局并发（users.concurrency）已耗尽，订阅请求仍可通过分组并发线放行，
// 且不会去占用户全局并发槽位。这是设计的核心契约①。
func TestAcquireScopedUserSlotWithWait_GroupScopeBypassesUserConcurrency(t *testing.T) {
	var userAcquireCalls, groupAcquireCalls int32
	cache := &concurrencyCacheMock{
		// 用户全局并发已耗尽
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			atomic.AddInt32(&userAcquireCalls, 1)
			return false, nil
		},
		// 分组并发仍有余量
		acquireUserGroupSlotFn: func(ctx context.Context, groupID, userID int64, maxConcurrency int, requestID string) (bool, error) {
			atomic.AddInt32(&groupAcquireCalls, 1)
			return true, nil
		},
	}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)

	c := newScopeTestContext(validSubscriptionGroup(3))
	subject := middleware.AuthSubject{UserID: 1, Concurrency: 1}
	scope := resolveConcurrencyScope(c, subject, &service.UserSubscription{})
	require.Equal(t, "group", scope.Kind)

	release, err := helper.AcquireScopedUserSlotWithWait(c, scope, 1, false, nil)
	require.NoError(t, err)
	require.NotNil(t, release, "subscription request must be admitted via group concurrency despite user concurrency exhaustion")
	require.Equal(t, int32(1), atomic.LoadInt32(&groupAcquireCalls), "group slot should be acquired once")
	require.Equal(t, int32(0), atomic.LoadInt32(&userAcquireCalls), "user global slot must NOT be touched for subscription requests")

	release()
}

// TestAcquireScopedUserSlotWithWait_UserScopeDoesNotUseGroupLine 验证计量（非订阅）请求仍走
// 用户全局并发线：即便分组并发线有余量，计量请求也绝不会去占分组并发槽位（与订阅路径互不相干）。
// 与上一测试互为对照，共同证明两条并发线的分流正确性。
func TestAcquireScopedUserSlotWithWait_UserScopeDoesNotUseGroupLine(t *testing.T) {
	var userAcquireCalls, groupAcquireCalls int32
	cache := &concurrencyCacheMock{
		// 用户全局并发有余量
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			atomic.AddInt32(&userAcquireCalls, 1)
			return true, nil
		},
		// 分组并发线有余量，但计量请求不应触及
		acquireUserGroupSlotFn: func(ctx context.Context, groupID, userID int64, maxConcurrency int, requestID string) (bool, error) {
			atomic.AddInt32(&groupAcquireCalls, 1)
			return true, nil
		},
	}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)

	// 计量分组（非订阅）→ scope 为 user
	c := newScopeTestContext(validMeteringGroup())
	subject := middleware.AuthSubject{UserID: 1, Concurrency: 1}
	scope := resolveConcurrencyScope(c, subject, &service.UserSubscription{})
	require.Equal(t, "user", scope.Kind)

	release, err := helper.AcquireScopedUserSlotWithWait(c, scope, 1, false, nil)
	require.NoError(t, err)
	require.NotNil(t, release, "metering request should be admitted via user concurrency line")
	require.Equal(t, int32(1), atomic.LoadInt32(&userAcquireCalls), "user slot should be acquired once")
	require.Equal(t, int32(0), atomic.LoadInt32(&groupAcquireCalls), "group slot must NOT be used for metering requests")

	release()
}
