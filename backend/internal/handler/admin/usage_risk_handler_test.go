package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ===================== stub 实现 service.UsageRiskService =====================

type usageRiskServiceStub struct {
	// ListReports
	listCalls  int
	lastFilter service.UsageRiskListFilter
	listResult *service.UsageRiskReportList
	listErr    error

	// GetReport
	reportCalls  int
	lastReportID int64
	reportResult *service.UsageRiskReportDetail
	reportErr    error

	// UpdateStatus
	statusCalls        int
	lastStatusReportID int64
	lastStatus         string
	lastStatusAdminID  int64
	statusErr          error

	// GetRunStatus
	runStatusResult *service.UsageRiskRunStatus
	runStatusErr    error

	// GetRiskSummary
	summaryResult *service.UsageRiskSummary
	summaryErr    error
}

var _ service.UsageRiskService = (*usageRiskServiceStub)(nil)

func (s *usageRiskServiceStub) ListReports(_ context.Context, f service.UsageRiskListFilter) (*service.UsageRiskReportList, error) {
	s.listCalls++
	s.lastFilter = f
	return s.listResult, s.listErr
}

func (s *usageRiskServiceStub) GetReport(_ context.Context, reportID int64) (*service.UsageRiskReportDetail, error) {
	s.reportCalls++
	s.lastReportID = reportID
	return s.reportResult, s.reportErr
}

func (s *usageRiskServiceStub) UpdateStatus(_ context.Context, reportID int64, newStatus string, adminID int64) error {
	s.statusCalls++
	s.lastStatusReportID = reportID
	s.lastStatus = newStatus
	s.lastStatusAdminID = adminID
	return s.statusErr
}

func (s *usageRiskServiceStub) GetRunStatus(_ context.Context) (*service.UsageRiskRunStatus, error) {
	return s.runStatusResult, s.runStatusErr
}

func (s *usageRiskServiceStub) GetRiskSummary(_ context.Context) (*service.UsageRiskSummary, error) {
	return s.summaryResult, s.summaryErr
}

// ===================== 测试辅助 =====================

// usageRiskSettingRepoStubForAdmin 是 SettingRepository 的进程内桩（admin 测试包内自建，
// 复用 service 包内同名 stub 的技术细节）。
type usageRiskSettingRepoStubForAdmin struct {
	values map[string]string
}

func (r *usageRiskSettingRepoStubForAdmin) Get(ctx context.Context, key string) (*service.Setting, error) {
	v, ok := r.values[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return &service.Setting{Key: key, Value: v}, nil
}

func (r *usageRiskSettingRepoStubForAdmin) GetValue(ctx context.Context, key string) (string, error) {
	v, ok := r.values[key]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (r *usageRiskSettingRepoStubForAdmin) Set(ctx context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func (r *usageRiskSettingRepoStubForAdmin) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := r.values[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (r *usageRiskSettingRepoStubForAdmin) SetMultiple(ctx context.Context, settings map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for k, v := range settings {
		r.values[k] = v
	}
	return nil
}

func (r *usageRiskSettingRepoStubForAdmin) GetAll(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out, nil
}

func (r *usageRiskSettingRepoStubForAdmin) Delete(ctx context.Context, key string) error {
	delete(r.values, key)
	return nil
}

func setupUsageRiskRouter(svc *usageRiskServiceStub, userID int64, setting *service.SettingService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if userID > 0 {
		router.Use(func(c *gin.Context) {
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID})
			c.Next()
		})
	}
	h := NewUsageRiskHandler(svc, setting)
	grp := router.Group("/api/v1/admin/usage-risk")
	{
		grp.GET("/reports", h.ListReports)
		grp.GET("/reports/:report_id", h.GetReport)
		grp.POST("/reports/:report_id/status", h.UpdateStatus)
		grp.GET("/run-status", h.GetRunStatus)
		grp.GET("/settings", h.GetSettings)
		grp.PUT("/settings", h.PutSettings)
	}
	return router
}

// 从响应体提取 data 字段。
func decodeData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	data, _ := envelope["data"].(map[string]any)
	return data
}

// ===================== 列表参数透传 / 默认过滤 =====================

func TestUsageRiskListReports_ParamPassthrough(t *testing.T) {
	svc := &usageRiskServiceStub{
		listResult: &service.UsageRiskReportList{
			Items:    []service.UsageRiskReportItem{},
			Total:    0,
			MinScore: 40,
		},
	}
	router := setupUsageRiskRouter(svc, 1, nil)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/usage-risk/reports?page=2&page_size=50&date=2026-09-20&level=high&user_id=7&group_id=3&rule=R1&include_low=true", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, svc.listCalls)
	require.Equal(t, 2, svc.lastFilter.Page)
	require.Equal(t, 50, svc.lastFilter.PageSize)
	require.Equal(t, "2026-09-20", svc.lastFilter.ReportDate)
	require.Equal(t, "high", svc.lastFilter.Level)
	require.Equal(t, int64(7), svc.lastFilter.UserID)
	require.Equal(t, int64(3), svc.lastFilter.GroupID)
	require.Equal(t, "R1", svc.lastFilter.Rule)
	require.True(t, svc.lastFilter.IncludeLow)

	data := decodeData(t, rec)
	require.Equal(t, float64(40), data["min_score"])
}

func TestUsageRiskListReports_Defaults(t *testing.T) {
	svc := &usageRiskServiceStub{
		listResult: &service.UsageRiskReportList{Items: []service.UsageRiskReportItem{}, Total: 0, MinScore: 40},
	}
	router := setupUsageRiskRouter(svc, 1, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/reports", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, svc.lastFilter.Page) // 默认页 1
	require.Equal(t, 20, svc.lastFilter.PageSize)
	require.False(t, svc.lastFilter.IncludeLow) // 默认不放低分
	require.Equal(t, int64(0), svc.lastFilter.UserID)
	require.Equal(t, int64(0), svc.lastFilter.GroupID)
}

func TestUsageRiskListReports_IncludeLowToggle(t *testing.T) {
	svc := &usageRiskServiceStub{
		listResult: &service.UsageRiskReportList{Items: []service.UsageRiskReportItem{}, Total: 0, MinScore: 40},
	}
	router := setupUsageRiskRouter(svc, 1, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/reports?include_low=false", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.False(t, svc.lastFilter.IncludeLow)

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/reports?include_low=true", nil)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	require.True(t, svc.lastFilter.IncludeLow)
}

// ===================== 列表筛选参数严格校验（R12-2） =====================

// 非法筛选参数 → 400，service 不被调用。
func TestUsageRiskListReports_InvalidFilterParams(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"user_id not a number", "user_id=abc"},
		{"group_id not a number", "group_id=xyz"},
		{"user_id zero", "user_id=0"},
		{"user_id negative", "user_id=-1"},
		{"group_id zero", "group_id=0"},
		{"group_id negative", "group_id=-1"},
		{"include_low not a bool", "include_low=maybe"},
		{"date not in YYYY-MM-DD", "date=2026-13-40"},
		{"level not in enum", "level=huge"},
		{"rule not in rule set", "rule=R9"},
		{"rule unknown format", "rule=NOPE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &usageRiskServiceStub{}
			router := setupUsageRiskRouter(svc, 1, nil)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/reports?"+tc.query, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code, tc.query)
			require.Equal(t, 0, svc.listCalls, "非法参数不应触发 service 查询")
		})
	}
}

// 合法边界值 → 行为不变，透传 stub 过滤条件。
func TestUsageRiskListReports_ValidBoundaryParams(t *testing.T) {
	svc := &usageRiskServiceStub{
		listResult: &service.UsageRiskReportList{Items: []service.UsageRiskReportItem{}, Total: 0, MinScore: 40},
	}
	router := setupUsageRiskRouter(svc, 1, nil)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/usage-risk/reports?date=2026-09-20&level=low&rule=R3a&user_id=7&group_id=3&include_low=false", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, svc.listCalls)
	require.Equal(t, "2026-09-20", svc.lastFilter.ReportDate)
	require.Equal(t, "low", svc.lastFilter.Level)
	require.Equal(t, "R3a", svc.lastFilter.Rule)
	require.Equal(t, int64(7), svc.lastFilter.UserID)
	require.Equal(t, int64(3), svc.lastFilter.GroupID)
	require.False(t, svc.lastFilter.IncludeLow)
}

// 缺省参数 → 不进入查询条件（默认行为不变）。
func TestUsageRiskListReports_MissingParamsNoFilter(t *testing.T) {
	svc := &usageRiskServiceStub{
		listResult: &service.UsageRiskReportList{Items: []service.UsageRiskReportItem{}, Total: 0, MinScore: 40},
	}
	router := setupUsageRiskRouter(svc, 1, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/reports", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, svc.lastFilter.ReportDate)
	require.Empty(t, svc.lastFilter.Level)
	require.Empty(t, svc.lastFilter.Rule)
	require.Equal(t, int64(0), svc.lastFilter.UserID)
	require.Equal(t, int64(0), svc.lastFilter.GroupID)
}

// ===================== :report_id 寻址 =====================

func TestUsageRiskGetReport_Found(t *testing.T) {
	svc := &usageRiskServiceStub{
		reportResult: &service.UsageRiskReportDetail{
			UsageRiskReportItem: service.UsageRiskReportItem{ReportID: 42, Score: 88},
			Evidence:            json.RawMessage(`{"active_hours":20}`),
			PolicyVersion:       "1",
		},
	}
	router := setupUsageRiskRouter(svc, 1, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/reports/42", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(42), svc.lastReportID)
	data := decodeData(t, rec)
	require.Equal(t, float64(42), data["report_id"])
	require.Equal(t, float64(88), data["score"])
	require.Equal(t, "1", data["policy_version"])
	require.NotNil(t, data["evidence"])
	require.Equal(t, "1", data["policy_version"], "H/F1：policy_version 须序列化为 hex 字符串而非 JSON number")
}

func TestUsageRiskGetReport_InvalidID(t *testing.T) {
	svc := &usageRiskServiceStub{}
	router := setupUsageRiskRouter(svc, 1, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/reports/notanumber", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUsageRiskGetReport_NotFound(t *testing.T) {
	svc := &usageRiskServiceStub{reportErr: service.ErrUsageRiskReportNotFound}
	router := setupUsageRiskRouter(svc, 1, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/reports/99", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// ===================== 状态流转：合法 / 非法目标 / 非法转移 / 不存在 =====================

func TestUsageRiskUpdateStatus_Legal(t *testing.T) {
	svc := &usageRiskServiceStub{}
	router := setupUsageRiskRouter(svc, 5, nil)

	body, _ := json.Marshal(map[string]string{"status": "acknowledged"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/usage-risk/reports/42/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(42), svc.lastStatusReportID)
	require.Equal(t, "acknowledged", svc.lastStatus)
	require.Equal(t, int64(5), svc.lastStatusAdminID) // 操作者身份已传入
}

func TestUsageRiskUpdateStatus_IllegalTarget(t *testing.T) {
	svc := &usageRiskServiceStub{}
	router := setupUsageRiskRouter(svc, 5, nil)

	// "open" 不是合法的目标终态（仅 acknowledged/dismissed/resolved）。
	body, _ := json.Marshal(map[string]string{"status": "open"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/usage-risk/reports/42/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, 0, svc.statusCalls) // 未调用 service
}

func TestUsageRiskUpdateStatus_IllegalTransition(t *testing.T) {
	svc := &usageRiskServiceStub{statusErr: service.ErrUsageRiskIllegalTransition}
	router := setupUsageRiskRouter(svc, 5, nil)

	body, _ := json.Marshal(map[string]string{"status": "resolved"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/usage-risk/reports/42/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUsageRiskUpdateStatus_NotFound(t *testing.T) {
	svc := &usageRiskServiceStub{statusErr: service.ErrUsageRiskReportNotFound}
	router := setupUsageRiskRouter(svc, 5, nil)

	body, _ := json.Marshal(map[string]string{"status": "dismissed"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/usage-risk/reports/42/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

// ===================== run-status 结构 =====================

func TestUsageRiskGetRunStatus_Structure(t *testing.T) {
	svc := &usageRiskServiceStub{
		runStatusResult: &service.UsageRiskRunStatus{
			Status:              "partial",
			ConsecutivePartials: 3,
			FailedBatches:       2,
			HistoryCovered:      false,
			ReconProgress:       "2026-09-20/2026-09-01",
		},
	}
	router := setupUsageRiskRouter(svc, 1, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/run-status", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	data := decodeData(t, rec)
	require.Equal(t, "partial", data["status"])
	require.Equal(t, float64(3), data["consecutive_partials"])
	require.Equal(t, float64(2), data["failed_batches"])
	require.Equal(t, false, data["history_covered"])
	require.Equal(t, "2026-09-20/2026-09-01", data["recon_progress"])
	require.Contains(t, data, "window_end")
}

// ===================== 鉴权：非 admin（未认证）拒绝 =====================

func TestUsageRiskListReports_Unauthorized(t *testing.T) {
	svc := &usageRiskServiceStub{}
	router := setupUsageRiskRouter(svc, 0, nil) // 无 subject

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/reports", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestUsageRiskUpdateStatus_Unauthorized(t *testing.T) {
	svc := &usageRiskServiceStub{}
	router := setupUsageRiskRouter(svc, 0, nil)

	body, _ := json.Marshal(map[string]string{"status": "acknowledged"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/usage-risk/reports/42/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// ===================== dashboard risk_summary 响应结构 =====================

func TestDashboardRiskSummary_ServiceResultReturned(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &DashboardHandler{}
	svc := &usageRiskServiceStub{
		summaryResult: &service.UsageRiskSummary{
			OpenTotal: 5,
			ByLevel:   map[string]int{"medium": 2, "high": 2, "critical": 1},
			Top: []service.UsageRiskTopUser{
				{
					UserID:   7,
					Username: "alice",
					MaxScore: 95, // 用户跨分组最高分，不应被重复累加
					Level:    "critical",
					TopRules: []string{"R1", "R5"},
				},
			},
		},
	}
	h.SetUsageRiskService(svc)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	summary := h.buildRiskSummary(c)
	require.NotNil(t, summary)
	require.Equal(t, 5, summary.OpenTotal)
	require.Equal(t, 95, summary.Top[0].MaxScore) // MAX(score)，非重复累加
	require.Equal(t, []string{"R1", "R5"}, summary.Top[0].TopRules)
	require.Empty(t, summary.Error)
}

func TestDashboardRiskSummary_NilServiceZeroValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &DashboardHandler{} // 未注入 usageRiskService

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	summary := h.buildRiskSummary(c)
	require.NotNil(t, summary)
	require.Equal(t, 0, summary.OpenTotal)
	require.NotNil(t, summary.ByLevel) // 空 map 而非 null
	require.NotNil(t, summary.Top)     // 空 slice 而非 null
	require.Empty(t, summary.Error)
}

func TestDashboardRiskSummary_ErrorState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &DashboardHandler{}
	svc := &usageRiskServiceStub{summaryErr: service.ErrUsageRiskReportNotFound}
	h.SetUsageRiskService(svc)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	summary := h.buildRiskSummary(c)
	require.NotNil(t, summary)
	require.NotEmpty(t, summary.Error) // 失败关闭呈现明确 error 态，不伪造零值掩盖
}

// ===================== settings GET / PUT（唯一 admin 读写入口，U2 收口） =====================

func TestUsageRiskGetSettings_ReturnsEffectiveValues(t *testing.T) {
	repo := &usageRiskSettingRepoStubForAdmin{values: map[string]string{
		service.SettingKeyUsageRiskListingMinScore: "60",
		service.SettingKeyUsageRiskRetentionDays:   "90",
	}}
	setting := service.NewSettingService(repo, &config.Config{Default: config.DefaultConfig{UserConcurrency: 5}})
	svc := &usageRiskServiceStub{}
	router := setupUsageRiskRouter(svc, 1, setting)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/settings", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	data := decodeData(t, rec)
	settingsRaw, ok := data["settings"]
	require.True(t, ok, "settings 字段应存在")
	settingsJSON, _ := json.Marshal(settingsRaw)
	var settings map[string]string
	require.NoError(t, json.Unmarshal(settingsJSON, &settings))
	// 存储覆盖值优先（入榜线被存为 60，覆盖默认 40）。
	require.Equal(t, "60", settings[service.SettingKeyUsageRiskListingMinScore])
	// 未被存储覆盖的键返回默认兜底（usage_risk_enabled 默认 true）。
	require.Equal(t, "true", settings[service.SettingKeyUsageRiskEnabled])
}

func TestUsageRiskPutSettings_ValidPersisted(t *testing.T) {
	repo := &usageRiskSettingRepoStubForAdmin{values: map[string]string{}}
	setting := service.NewSettingService(repo, &config.Config{Default: config.DefaultConfig{UserConcurrency: 5}})
	svc := &usageRiskServiceStub{}
	router := setupUsageRiskRouter(svc, 1, setting)

	body, _ := json.Marshal(map[string]string{
		service.SettingKeyUsageRiskListingMinScore: "60",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/usage-risk/settings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	stored, err := repo.Get(context.Background(), service.SettingKeyUsageRiskListingMinScore)
	require.NoError(t, err)
	require.Equal(t, "60", stored.Value)
}

func TestUsageRiskPutSettings_InvalidValueRejected(t *testing.T) {
	repo := &usageRiskSettingRepoStubForAdmin{values: map[string]string{}}
	setting := service.NewSettingService(repo, &config.Config{Default: config.DefaultConfig{UserConcurrency: 5}})
	svc := &usageRiskServiceStub{}
	router := setupUsageRiskRouter(svc, 1, setting)

	// 比例类越界：r3b_occupancy 必须 ∈ (0,1]，0.5 合法但这里故意传 2 触发校验。
	body, _ := json.Marshal(map[string]string{
		service.SettingKeyUsageRiskR3BOccupancy: "2",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/usage-risk/settings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code) // 域校验器拒绝，不落库
	_, err := repo.Get(context.Background(), service.SettingKeyUsageRiskR3BOccupancy)
	require.Error(t, err, "非法值不应落库")
}

func TestUsageRiskPutSettings_RetentionBindingRejected(t *testing.T) {
	repo := &usageRiskSettingRepoStubForAdmin{values: map[string]string{}}
	setting := service.NewSettingService(repo, &config.Config{
		Default: config.DefaultConfig{UserConcurrency: 5},
		DashboardAgg: config.DashboardAggregationConfig{
			Retention: config.DashboardAggregationRetentionConfig{UsageLogsDays: 30},
		},
	})
	svc := &usageRiskServiceStub{}
	router := setupUsageRiskRouter(svc, 1, setting)

	// 风险保留期 90 > 源日志保留 30，绑定校验拒绝保存（方案 §6.4）。
	body, _ := json.Marshal(map[string]string{
		service.SettingKeyUsageRiskRetentionDays: "90",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/usage-risk/settings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	_, err := repo.Get(context.Background(), service.SettingKeyUsageRiskRetentionDays)
	require.Error(t, err, "绑定校验拒绝时不应落库")
}

func TestUsageRiskSettings_Unauthorized(t *testing.T) {
	svc := &usageRiskServiceStub{}
	// 未认证（userID=0）且 setting 服务有效：应 401 而非 500。handler 内先做服务可用性
	// 检查再做 adminID 检查；生产路径 adminAuth 中间件在前，此处验证纵深防御分支。
	setting := service.NewSettingService(&usageRiskSettingRepoStubForAdmin{values: map[string]string{}}, &config.Config{Default: config.DefaultConfig{UserConcurrency: 5}})
	router := setupUsageRiskRouter(svc, 0, setting)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage-risk/settings", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
