package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// stubFreshnessBounds 提供冻结上界（三维唯一公式的账号侧输入），用于验证管理端阈值
// 与公式同一计算结果（ub > 基线时阈值 = ub）。
type stubFreshnessBounds struct{ ub time.Duration }

func (s stubFreshnessBounds) AccountFreshnessUpperBound(int64, string) time.Duration { return s.ub }

// TestAccountHandler_FreshnessPassthroughNoSignal 管理端透传定向测试（D4 验收 6）：
// 无信号占位（无权威观测）展示 waiting_probe「等待主动复探」，**不得显示为正常倒计时**；
// 阈值取三维唯一公式（未注入冻结上界来源时 = 对应维度配置基线）；未注入告警服务时
// active_alert 缺省 false。
func TestAccountHandler_FreshnessPassthroughNoSignal(t *testing.T) {
	// 空 repo：唯一状态写入口读面返回空条目（等价于无权威观测）。
	rl := service.NewRateLimitService(&freshNilObsRepo{}, nil, nil, nil, nil)
	// 注入冻结上界来源（> 基线）以验证管理端阈值与三维唯一公式同一计算结果。
	rl.SetFreshnessBoundsProvider(stubFreshnessBounds{ub: 315 * time.Second})
	h := &AccountHandler{rateLimitService: rl}

	o, err := h.buildFreshnessObservation(context.Background(), 42, "gpt-4")
	require.NoError(t, err)
	require.Equal(t, "gpt-4", o.Scope)
	require.Equal(t, service.FreshnessDimAccountModel, o.Dimension)
	require.Equal(t, service.FreshnessDisplayWaitingProbe, o.DisplayState)
	require.NotEqual(t, service.FreshnessDisplayObserved, o.DisplayState, "无信号占位不得显示为已观测/倒计时")
	require.False(t, o.Stale)
	require.False(t, o.ActiveAlert)
	require.Equal(t, service.AccountModelFreshnessThreshold(130).Seconds(), o.EffectiveThresholdSeconds)
	require.Positive(t, o.EffectiveThresholdSeconds, "阈值不得退化为零值")
}

// TestAccountHandler_FreshnessPassthroughAccountLevelDimension 账号级（无模型键）维度
// 透传 dimension=account_level，阈值走账号级口径（与账号+模型同源公式）。
func TestAccountHandler_FreshnessPassthroughAccountLevelDimension(t *testing.T) {
	rl := service.NewRateLimitService(&freshNilObsRepo{}, nil, nil, nil, nil)
	rl.SetFreshnessBoundsProvider(stubFreshnessBounds{ub: 315 * time.Second})
	h := &AccountHandler{rateLimitService: rl}

	o, err := h.buildFreshnessObservation(context.Background(), 42, service.TokenHarborAccountLevelScope)
	require.NoError(t, err)
	require.Equal(t, service.FreshnessDimAccountLevel, o.Dimension)
	require.Equal(t, service.FreshnessDisplayWaitingProbe, o.DisplayState)
	// 账号级与账号+模型共用同一上界公式（同口径），管理端读同一计算结果。
	require.Equal(t, service.AccountLevelFreshnessThreshold(130).Seconds(), o.EffectiveThresholdSeconds)
	require.Equal(t, service.AccountModelFreshnessThreshold(130), service.AccountLevelFreshnessThreshold(130))
}

// freshFailObsRepo 是观测读取失败的窄仓储桩（E11）：嵌入 fakeAccountRepo 满足
// service.AccountRepository 接口（NewRateLimitService 的 accountRepo 入参面），同时显式
// 实现 modelRateLimitObservationRepository 三方法，readErr 非 nil 时 GetModelRateLimitEntry
// 返回读取错误，模拟 DB/存储故障。
type freshFailObsRepo struct {
	fakeAccountRepo
	readErr error
}

func (r *freshFailObsRepo) GetModelRateLimitEntry(_ context.Context, _ int64, _ string) (map[string]any, error) {
	if r.readErr != nil {
		return nil, r.readErr
	}
	return nil, nil
}
func (r *freshFailObsRepo) GetModelRateLimitMeta(context.Context, int64, string) (time.Time, int64, bool, error) {
	return time.Time{}, 0, false, nil
}
func (r *freshFailObsRepo) CommitModelRateLimitObservation(context.Context, int64, string, map[string]any, bool, time.Time, int64) error {
	return nil
}

// TestAccountHandler_GetAccountFreshness_ReadErrorFailsClosed 定向测试（E11 验收）：
// 任一 scope 观测读取失败（DB 故障）时整体失败关闭，返回明确错误响应（500），
// **不得**把读取故障序列化为 waiting_probe/stale=false 的正常空态——管理员必须能区分
// 「无观测」与「不可读取」。
func TestAccountHandler_GetAccountFreshness_ReadErrorFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminSvc := newStubAdminService()
	adminSvc.getAccountResult = &service.Account{
		ID: 42,
		Extra: map[string]any{
			service.ModelRateLimitsKey: map[string]any{
				"gpt-4": map[string]any{},
				"gpt-5": map[string]any{},
			},
		},
	}
	repo := &freshFailObsRepo{readErr: errors.New("observation read failed: db down")}
	rl := service.NewRateLimitService(repo, nil, nil, nil, nil)
	h := &AccountHandler{adminService: adminSvc, rateLimitService: rl}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/42/freshness", nil)
	c.Params = []gin.Param{{Key: "id", Value: "42"}}

	h.GetAccountFreshness(c)

	require.Equal(t, http.StatusInternalServerError, w.Code, "观测读取失败必须整体失败关闭，不得返回正常空态")
	require.NotEqual(t, http.StatusOK, w.Code)

	var body struct {
		Code int `json:"code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, http.StatusInternalServerError, body.Code, "错误响应 code 应反映 500")
}

// TestAccountHandler_GetAccountFreshness_SuccessStructure 定向测试（E11 验收）：
// 正常路径响应结构不变：observations 数组逐条保留 scope/dimension/display_state/
// effective_threshold_seconds/stale/active_alert；无权威观测仍展示 waiting_probe，
// 读取错误不得污染正常路径。
func TestAccountHandler_GetAccountFreshness_SuccessStructure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminSvc := newStubAdminService()
	adminSvc.getAccountResult = &service.Account{
		ID: 42,
		Extra: map[string]any{
			service.ModelRateLimitsKey: map[string]any{
				"gpt-4":                              map[string]any{},
				service.TokenHarborAccountLevelScope: map[string]any{},
			},
		},
	}
	rl := service.NewRateLimitService(&freshNilObsRepo{}, nil, nil, nil, nil)
	rl.SetFreshnessBoundsProvider(stubFreshnessBounds{ub: 315 * time.Second})
	h := &AccountHandler{adminService: adminSvc, rateLimitService: rl}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/42/freshness", nil)
	c.Params = []gin.Param{{Key: "id", Value: "42"}}

	h.GetAccountFreshness(c)

	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Code int `json:"code"`
		Data struct {
			AccountID    int64 `json:"account_id"`
			Observations []struct {
				Scope                     string  `json:"scope"`
				Dimension                 string  `json:"dimension"`
				EffectiveThresholdSeconds float64 `json:"effective_threshold_seconds"`
				Stale                     bool    `json:"stale"`
				ActiveAlert               bool    `json:"active_alert"`
				DisplayState              string  `json:"display_state"`
			} `json:"observations"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 0, body.Code)
	require.Equal(t, int64(42), body.Data.AccountID)
	require.Len(t, body.Data.Observations, 2, "两个 scope 均应透传")
	for _, obs := range body.Data.Observations {
		require.Equal(t, service.FreshnessDisplayWaitingProbe, obs.DisplayState, "无权威观测仍为 waiting_probe 占位")
		require.False(t, obs.Stale)
		require.False(t, obs.ActiveAlert)
		require.Positive(t, obs.EffectiveThresholdSeconds, "阈值不得退化为零值")
	}
}

// freshnessAlertStoreStub 是告警查询存储的窄桩（E15）：实现 service.FreshnessAlertStore，
// 可控返回（activeHit 命中 / readErr 读取失败），Create/Update 在管理端读取路径不会用到。
type freshnessAlertStoreStub struct {
	readErr error
	active  *service.OpsAlertEvent
}

func (s *freshnessAlertStoreStub) CreateAlertEvent(context.Context, *service.OpsAlertEvent) (*service.OpsAlertEvent, error) {
	return nil, nil
}
func (s *freshnessAlertStoreStub) GetActiveFreshnessAlert(_ context.Context, _ map[string]any) (*service.OpsAlertEvent, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.active, nil
}
func (s *freshnessAlertStoreStub) UpdateAlertEventStatus(context.Context, int64, string, *time.Time) error {
	return nil
}

// ListActiveFreshnessAlerts 满足 FreshnessAlertStore 接口（E45 接口扩面）；该 stub 用于管理端
// 透传测试，孤儿清扫不参与，返回空集合即可。
func (s *freshnessAlertStoreStub) ListActiveFreshnessAlerts(context.Context) ([]*service.OpsAlertEvent, error) {
	return nil, nil
}

// ResolveFreshnessAlertOnRecovery 满足 FreshnessAlertStore 接口（E47 接口扩面）；该 stub 用于管理端
// 透传测试，孤儿清扫不参与，未门禁恢复关闭窄面静默 no-op 即可。
func (s *freshnessAlertStoreStub) ResolveFreshnessAlertOnRecovery(context.Context, map[string]any) error {
	return nil
}

// freshCorruptObsRepo 是观测条目权威数据损坏的窄仓储桩（E16）：GetModelRateLimitEntry 返回
// 含非法 observed_at 的条目，模拟权威数据损坏（非读取故障、非无观测）。
type freshCorruptObsRepo struct {
	fakeAccountRepo
}

func (r *freshCorruptObsRepo) GetModelRateLimitEntry(_ context.Context, _ int64, scope string) (map[string]any, error) {
	return map[string]any{service.FreshnessObservedAtKey: "corrupted-not-rfc3339"}, nil
}
func (r *freshCorruptObsRepo) GetModelRateLimitMeta(context.Context, int64, string) (time.Time, int64, bool, error) {
	return time.Time{}, 0, false, nil
}
func (r *freshCorruptObsRepo) CommitModelRateLimitObservation(context.Context, int64, string, map[string]any, bool, time.Time, int64) error {
	return nil
}
func (r *freshCorruptObsRepo) WithModelRateLimitAccountLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// freshNilObsRepo 是观测窄面「无观测」语义的桩（E33）：嵌入 fakeAccountRepo 满足
// service.AccountRepository 接口，同时显式实现 modelRateLimitObservationRepository 三方法，
// Get 侧返回无条目（nil）语义——与用例「无权威观测」一致。用于补齐 E30 #2 后 admin 新鲜度
// 用例对窄面的显式实现要求（E30 白名单未含 admin 包导致的漏网）。
type freshNilObsRepo struct {
	fakeAccountRepo
}

func (r *freshNilObsRepo) GetModelRateLimitEntry(_ context.Context, _ int64, _ string) (map[string]any, error) {
	return nil, nil
}
func (r *freshNilObsRepo) GetModelRateLimitMeta(context.Context, int64, string) (time.Time, int64, bool, error) {
	return time.Time{}, 0, false, nil
}
func (r *freshNilObsRepo) CommitModelRateLimitObservation(context.Context, int64, string, map[string]any, bool, time.Time, int64) error {
	return nil
}
func (r *freshNilObsRepo) WithModelRateLimitAccountLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// TestAccountHandler_GetAccountFreshness_InvalidObservedAtFailsClosed 定向测试（E16 验收）：
// observed_at 存在但格式非法（权威数据损坏）时，管理端必须失败关闭返回 500，
// **不得**降级序列化为 waiting_probe/observed 正常态——损坏不得显示为健康。
func TestAccountHandler_GetAccountFreshness_InvalidObservedAtFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminSvc := newStubAdminService()
	adminSvc.getAccountResult = &service.Account{
		ID: 42,
		Extra: map[string]any{
			service.ModelRateLimitsKey: map[string]any{
				"gpt-4": map[string]any{},
			},
		},
	}
	repo := &freshCorruptObsRepo{}
	rl := service.NewRateLimitService(repo, nil, nil, nil, nil)
	h := &AccountHandler{adminService: adminSvc, rateLimitService: rl}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/42/freshness", nil)
	c.Params = []gin.Param{{Key: "id", Value: "42"}}

	h.GetAccountFreshness(c)

	require.Equal(t, http.StatusInternalServerError, w.Code, "非法 observed_at 必须失败关闭，不得降级为正常态")
	require.NotEqual(t, http.StatusOK, w.Code)

	var body struct {
		Code int `json:"code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, http.StatusInternalServerError, body.Code, "错误响应 code 应反映 500")
}

// TestAccountHandler_GetAccountFreshness_AlertReadErrorFailsClosed 定向测试（E15 验收）：
// 告警查询读取失败（Ops/DB 故障）时整体失败关闭，返回明确错误响应（500），
// **不得**把告警读取故障序列化为 active_alert=false 的正常无告警态——管理员必须能区分
// 「无告警」与「不可读取」。
func TestAccountHandler_GetAccountFreshness_AlertReadErrorFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminSvc := newStubAdminService()
	adminSvc.getAccountResult = &service.Account{
		ID: 42,
		Extra: map[string]any{
			service.ModelRateLimitsKey: map[string]any{
				"gpt-4": map[string]any{},
			},
		},
	}
	rl := service.NewRateLimitService(nil, nil, nil, nil, nil)
	alertSvc := service.NewFreshnessAlertService(&freshnessAlertStoreStub{
		readErr: errors.New("alert read failed: ops db down"),
	}, nil, nil, nil)
	h := &AccountHandler{adminService: adminSvc, rateLimitService: rl, freshnessAlertService: alertSvc}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/42/freshness", nil)
	c.Params = []gin.Param{{Key: "id", Value: "42"}}

	h.GetAccountFreshness(c)

	require.Equal(t, http.StatusInternalServerError, w.Code, "告警读取失败必须整体失败关闭，不得序列化为正常无告警态")
	require.NotEqual(t, http.StatusOK, w.Code)

	var body struct {
		Code int `json:"code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, http.StatusInternalServerError, body.Code, "错误响应 code 应反映 500")
}

// TestAccountHandler_FreshnessPassthroughNilAlertService 定向测试（E15 验收）：
// freshnessAlertService == nil（未接线）是「无告警服务」的合法构造语义，不是读取失败——
// 保持 active_alert=false 正常态响应 200。
func TestAccountHandler_FreshnessPassthroughNilAlertService(t *testing.T) {
	rl := service.NewRateLimitService(&freshNilObsRepo{}, nil, nil, nil, nil)
	rl.SetFreshnessBoundsProvider(stubFreshnessBounds{ub: 315 * time.Second})
	h := &AccountHandler{rateLimitService: rl}

	o, err := h.buildFreshnessObservation(context.Background(), 42, "gpt-4")
	require.NoError(t, err, "未注入告警服务时不得报错")
	require.False(t, o.ActiveAlert, "nil 服务=无告警服务，active_alert 保持 false")
	require.Equal(t, service.FreshnessDisplayWaitingProbe, o.DisplayState, "读取路径不被告警服务状态污染")
}

// TestAccountHandler_FreshnessAlertHit 定向测试（E15 验收）：
// 正常命中：告警服务查询返回 firing 告警时 active_alert=true。
func TestAccountHandler_FreshnessAlertHit(t *testing.T) {
	rl := service.NewRateLimitService(&freshNilObsRepo{}, nil, nil, nil, nil)
	rl.SetFreshnessBoundsProvider(stubFreshnessBounds{ub: 315 * time.Second})
	alertSvc := service.NewFreshnessAlertService(&freshnessAlertStoreStub{
		active: &service.OpsAlertEvent{
			ID:     1,
			Status: service.OpsAlertStatusFiring,
		},
	}, nil, nil, nil)
	h := &AccountHandler{rateLimitService: rl, freshnessAlertService: alertSvc}

	o, err := h.buildFreshnessObservation(context.Background(), 42, "gpt-4")
	require.NoError(t, err)
	require.True(t, o.ActiveAlert, "firing 告警命中必须透传 active_alert=true")
}
