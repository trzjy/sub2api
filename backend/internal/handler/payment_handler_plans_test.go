//go:build unit

package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

// stubSettingRepo 是 SettingRepository 的只读桩，所有键返回空值。
// GetCheckoutInfo 会经 GetPaymentConfig 读取支付设置，这里用桩避免引入真实
// 设置仓储与迁移；全部返回默认零值即可让端点正常组装响应。
type stubSettingRepo struct{}

func (stubSettingRepo) Get(ctx context.Context, key string) (*service.Setting, error) {
	return nil, nil
}
func (stubSettingRepo) GetValue(ctx context.Context, key string) (string, error) { return "", nil }
func (stubSettingRepo) Set(ctx context.Context, key, value string) error         { return nil }
func (stubSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (stubSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	return nil
}
func (stubSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (stubSettingRepo) Delete(ctx context.Context, key string) error { return nil }

// plansFixture 在内存 SQLite 中建立若干分组与在售订阅计划，覆盖：
//   - 内部供给平台分组（codebuddy / muse / cursor）→ 应被过滤；
//   - composite 分组 → 应保留且 group_platform == "composite"；
//   - 普通平台分组（anthropic）→ 应保留不受影响。
type plansFixture struct {
	client      *dbent.Client
	configSvc   *service.PaymentConfigService
	handler     *PaymentHandler
	internalGrp map[string]int64 // platform -> group id
	compositeID int64
	normalID    int64
}

func newPlansFixture(t *testing.T) *plansFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, err := sql.Open("sqlite", "file:payment_handler_plans?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)

	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()

	// 内部供给平台分组
	internalGrp := map[string]int64{}
	for _, platform := range []string{domain.PlatformCodeBuddy, domain.PlatformMuse, domain.PlatformCursor} {
		g, cErr := client.Group.Create().
			SetName("grp-" + platform).
			SetPlatform(platform).
			Save(ctx)
		require.NoError(t, cErr)
		internalGrp[platform] = g.ID
		_, pErr := client.SubscriptionPlan.Create().
			SetGroupID(g.ID).
			SetName("plan-" + platform).
			SetDescription("").
			SetPrice(10).
			SetPriceCny(0).
			SetOriginalPriceCny(0).
			SetCurrency("USD").
			SetValidityDays(30).
			SetValidityUnit("day").
			SetFeatures("").
			SetProductName("").
			SetForSale(true).
			SetSortOrder(1).
			Save(ctx)
		require.NoError(t, pErr)
	}

	// composite 分组（正向：应保留）
	composite, cErr := client.Group.Create().
		SetName("grp-composite").
		SetPlatform(domain.PlatformComposite).
		Save(ctx)
	require.NoError(t, cErr)
	_, pErr := client.SubscriptionPlan.Create().
		SetGroupID(composite.ID).
		SetName("plan-composite").
		SetDescription("").
		SetPrice(20).
		SetPriceCny(0).
		SetOriginalPriceCny(0).
		SetCurrency("USD").
		SetValidityDays(30).
		SetValidityUnit("day").
		SetFeatures("").
		SetProductName("").
		SetForSale(true).
		SetSortOrder(2).
		Save(ctx)
	require.NoError(t, pErr)

	// 普通平台分组（正向：不受影响）
	normal, nErr := client.Group.Create().
		SetName("grp-anthropic").
		SetPlatform(domain.PlatformAnthropic).
		Save(ctx)
	require.NoError(t, nErr)
	_, pErr = client.SubscriptionPlan.Create().
		SetGroupID(normal.ID).
		SetName("plan-anthropic").
		SetDescription("").
		SetPrice(30).
		SetPriceCny(0).
		SetOriginalPriceCny(0).
		SetCurrency("USD").
		SetValidityDays(30).
		SetValidityUnit("day").
		SetFeatures("").
		SetProductName("").
		SetForSale(true).
		SetSortOrder(3).
		Save(ctx)
	require.NoError(t, pErr)

	configSvc := service.NewPaymentConfigService(client, stubSettingRepo{}, nil)
	handler := NewPaymentHandler(nil, configSvc)

	return &plansFixture{
		client:      client,
		configSvc:   configSvc,
		handler:     handler,
		internalGrp: internalGrp,
		compositeID: composite.ID,
		normalID:    normal.ID,
	}
}

// decodePlansResponse 解析 {"code":0,"data":[...]} 中的 plan 数组。
func decodePlansResponse(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var resp struct {
		Code int             `json:"code"`
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Equal(t, 0, resp.Code)
	return resp.Data
}

// decodeCheckoutPlans 解析 checkout-info 响应中的 data.plans 数组。
func decodeCheckoutPlans(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Plans []map[string]any `json:"plans"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.Equal(t, 0, resp.Code)
	return resp.Data.Plans
}

func runGetPlans(t *testing.T, h *PaymentHandler) []map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/payment/plans", nil)
	h.GetPlans(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)
	return decodePlansResponse(t, recorder.Body.Bytes())
}

func runGetCheckoutInfo(t *testing.T, h *PaymentHandler) []map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/payment/checkout-info", nil)
	h.GetCheckoutInfo(ctx)
	require.Equal(t, http.StatusOK, recorder.Code)
	return decodeCheckoutPlans(t, recorder.Body.Bytes())
}

// findPlanByGroupPlatform 返回首个匹配 group_platform 的计划，未找到则 nil。
func findPlanByGroupPlatform(plans []map[string]any, platform string) map[string]any {
	for _, p := range plans {
		if p["group_platform"] == platform {
			return p
		}
	}
	return nil
}

// TestGetPlansFiltersInternalSupplyPlatforms 验证 /plans 端点（F5）：
// 命中内部供给平台分组的在售计划被过滤；composite 与普通平台计划保留。
func TestGetPlansFiltersInternalSupplyPlatforms(t *testing.T) {
	f := newPlansFixture(t)
	plans := runGetPlans(t, f.handler)

	// 负向：内部供给平台不应出现在响应中（group_platform 不暴露内部平台名）。
	for _, p := range plans {
		gp, _ := p["group_platform"].(string)
		require.False(t, domain.IsInternalSupplyPlatform(gp),
			"内部供给平台计划不应出现在 /plans 响应中: %s", gp)
	}

	// 正向：composite 分组计划保留且 group_platform == "composite"。
	compositePlan := findPlanByGroupPlatform(plans, domain.PlatformComposite)
	require.NotNil(t, compositePlan, "/plans 应保留 composite 分组计划")
	require.Equal(t, "composite", compositePlan["group_platform"])

	// 正向：普通平台分组计划不受影响。
	normalPlan := findPlanByGroupPlatform(plans, domain.PlatformAnthropic)
	require.NotNil(t, normalPlan, "/plans 应保留普通平台分组计划")
	require.Equal(t, domain.PlatformAnthropic, normalPlan["group_platform"])
}

// TestGetCheckoutInfoFiltersInternalSupplyPlatforms 验证 /checkout-info 端点（F5）：
// 与 /plans 平行路径一致，内部供给平台计划在 checkout-info 中同样被过滤。
func TestGetCheckoutInfoFiltersInternalSupplyPlatforms(t *testing.T) {
	f := newPlansFixture(t)
	plans := runGetCheckoutInfo(t, f.handler)

	// 负向：内部供给平台不应出现在响应中（group_platform 不暴露内部平台名）。
	for _, p := range plans {
		gp, _ := p["group_platform"].(string)
		require.False(t, domain.IsInternalSupplyPlatform(gp),
			"内部供给平台计划不应出现在 /checkout-info 响应中: %s", gp)
	}

	// 正向：composite 分组计划保留且 group_platform == "composite"。
	compositePlan := findPlanByGroupPlatform(plans, domain.PlatformComposite)
	require.NotNil(t, compositePlan, "/checkout-info 应保留 composite 分组计划")
	require.Equal(t, "composite", compositePlan["group_platform"])

	// 正向：普通平台分组计划不受影响。
	normalPlan := findPlanByGroupPlatform(plans, domain.PlatformAnthropic)
	require.NotNil(t, normalPlan, "/checkout-info 应保留普通平台分组计划")
	require.Equal(t, domain.PlatformAnthropic, normalPlan["group_platform"])
}
