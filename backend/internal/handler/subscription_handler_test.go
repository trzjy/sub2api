//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// subStubRepo 是 UserSubscriptionRepository 的最小桩：只实现四个端点实际调用的方法，
// 其余方法由嵌入的接口提供（不会被触达）。
type subStubRepo struct {
	service.UserSubscriptionRepository
	byUser      []service.UserSubscription
	byUserActive []service.UserSubscription
	byID        map[int64]service.UserSubscription
}

func (r *subStubRepo) ListByUserID(_ context.Context, _ int64) ([]service.UserSubscription, error) {
	return r.byUser, nil
}

func (r *subStubRepo) ListActiveByUserID(_ context.Context, _ int64) ([]service.UserSubscription, error) {
	return r.byUserActive, nil
}

func (r *subStubRepo) GetByID(_ context.Context, id int64) (*service.UserSubscription, error) {
	sub, ok := r.byID[id]
	if !ok {
		return nil, nil
	}
	return &sub, nil
}

// newSubHandlerWith 用桩仓储构造 SubscriptionService 与 SubscriptionHandler。
func newSubHandlerWith(byUser, byUserActive []service.UserSubscription, byID map[int64]service.UserSubscription) *SubscriptionHandler {
	repo := &subStubRepo{byUser: byUser, byUserActive: byUserActive, byID: byID}
	svc := service.NewSubscriptionService(nil, repo, nil, nil, nil)
	return NewSubscriptionHandler(svc)
}

// runSubContext 注入 AuthSubject 并构造测试 gin 上下文。
func runSubContext() (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/subscriptions", nil)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
	return w, c
}

// 构造两条订阅：一条嵌套 codebuddy（内部供给平台）分组，一条 composite 分组。
func subFixtures() (byUser, byUserActive []service.UserSubscription, byID map[int64]service.UserSubscription) {
	codebuddySub := service.UserSubscription{
		ID:      11,
		UserID:  1,
		GroupID: 10,
		Group:   &service.Group{ID: 10, Name: "codebuddy-g", Platform: "codebuddy"},
	}
	compositeSub := service.UserSubscription{
		ID:      12,
		UserID:  1,
		GroupID: 20,
		Group:   &service.Group{ID: 20, Name: "composite-g", Platform: "composite"},
	}
	byUser = []service.UserSubscription{codebuddySub, compositeSub}
	byUserActive = []service.UserSubscription{codebuddySub, compositeSub}
	byID = map[int64]service.UserSubscription{11: codebuddySub, 12: compositeSub}
	return
}

// 解码响应信封的 data 字段。
func decodeData(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code)
	var envelope struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	require.NoError(t, json.Unmarshal(envelope.Data, out))
}

// extractPlatforms 从 UserSubscription 列表提取每条订阅的 Group.Platform（nil 时为 ""）。
func extractPlatforms(subs []dto.UserSubscription) []string {
	plats := make([]string, 0, len(subs))
	for _, s := range subs {
		if s.Group != nil {
			plats = append(plats, s.Group.Platform)
		} else {
			plats = append(plats, "")
		}
	}
	return plats
}

func TestSubscriptionListFiltersInternalSupplyPlatform(t *testing.T) {
	byUser, _, _ := subFixtures()
	h := newSubHandlerWith(byUser, nil, nil)
	w, c := runSubContext()
	h.List(c)
	var out []dto.UserSubscription
	decodeData(t, w, &out)
	require.NotContains(t, extractPlatforms(out), "codebuddy", "codebuddy subscription must be hidden")
	require.Contains(t, extractPlatforms(out), "composite", "composite subscription must be visible")
	require.Len(t, out, 1, "only the non-internal subscription should remain")
}

func TestSubscriptionGetActiveFiltersInternalSupplyPlatform(t *testing.T) {
	_, byUserActive, _ := subFixtures()
	h := newSubHandlerWith(nil, byUserActive, nil)
	w, c := runSubContext()
	h.GetActive(c)
	var out []dto.UserSubscription
	decodeData(t, w, &out)
	require.NotContains(t, extractPlatforms(out), "codebuddy", "codebuddy subscription must be hidden")
	require.Contains(t, extractPlatforms(out), "composite", "composite subscription must be visible")
	require.Len(t, out, 1, "only the non-internal subscription should remain")
}

func TestSubscriptionGetProgressFiltersInternalSupplyPlatform(t *testing.T) {
	_, byUserActive, byID := subFixtures()
	h := newSubHandlerWith(nil, byUserActive, byID)
	w, c := runSubContext()
	h.GetProgress(c)
	var out []SubscriptionProgressInfo
	decodeData(t, w, &out)
	plats := make([]string, 0, len(out))
	for _, info := range out {
		if info.Subscription != nil && info.Subscription.Group != nil {
			plats = append(plats, info.Subscription.Group.Platform)
		}
	}
	require.NotContains(t, plats, "codebuddy", "codebuddy subscription must be hidden")
	require.Contains(t, plats, "composite", "composite subscription must be visible")
	require.Len(t, out, 1, "only the non-internal subscription should remain")
}

func TestSubscriptionGetSummaryFiltersInternalSupplyPlatform(t *testing.T) {
	_, byUserActive, _ := subFixtures()
	h := newSubHandlerWith(nil, byUserActive, nil)
	w, c := runSubContext()
	h.GetSummary(c)
	var summary struct {
		ActiveCount   int                        `json:"active_count"`
		TotalUsedUSD  float64                    `json:"total_used_usd"`
		Subscriptions []SubscriptionSummaryItem  `json:"subscriptions"`
	}
	decodeData(t, w, &summary)
	groupIDs := make([]int64, 0, len(summary.Subscriptions))
	for _, item := range summary.Subscriptions {
		groupIDs = append(groupIDs, item.GroupID)
	}
	require.NotContains(t, groupIDs, int64(10), "codebuddy subscription (group 10) must be hidden")
	require.Contains(t, groupIDs, int64(20), "composite subscription (group 20) must be visible")
	require.Len(t, summary.Subscriptions, 1, "only the non-internal subscription should remain")
	// ActiveCount 沿用 ListActiveUserSubscriptions 的全量计数（本轮未进入 items 过滤点）。
	require.Equal(t, 2, summary.ActiveCount)
}
