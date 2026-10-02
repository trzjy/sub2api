package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// D-BE-002b §3.3 逐路径 handler 排除测试
//
// 七个安装点（handler 层已解析渠道映射 A→B 并装入请求 ctx）各一条 e2e/前置段测试：
//   - 构造分组渠道映射 A→B，分组下放置账号 X（extra.model_rate_limits 含 scope=B、
//     且 rate_limit_reset_at 为未来时间），以及对照账号 Y（无限制）。
//   - 用该分组 API Key 发客户端请求模型 A。
//   - 断言①：账号 X 不被选中（被模型级限流门按其映射名 B 排除）。
//   - 断言②：调度门判定身份 == 转发实际身份（ctx 中 ChannelMappedModel == B，
//     且上游桩收到的模型名 == B）。
//
// 主网关（1-4，*GatewayHandler）走前置段单测：捕获选号边界 ctx 并断言其
// ChannelMappedModel==B，且用导出的 GetModelRateLimitRemainingTimeWithContext（内部
// 走 modelRateLimitKeysForRequest 的 ctx 追加逻辑）证明 X 被排除、Y 可选。
// OpenAI 网关（5-7，*OpenAIGatewayHandler）走真实 e2e：真实调度器套用限流门排除 X，
// 上游桩记录被选中账号与转发模型名。
// ---------------------------------------------------------------------------

const (
	cmModelA = "deepseek-v4.1-flash"
	cmModelB = "deepseek-v4.1-flash-free"
)

// cmHTTPUpstream 记录被转发到的账号 ID 与请求体中的模型名。
type cmHTTPUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	models     []string
}

func (u *cmHTTPUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	// 模型名来源：绝大多数路径在请求体 "model" 字段；Gemini 网关把映射后的模型名
	// 拼进 URL 路径（/v1beta/models/{model}:{action}），请求体不含 model 字段，需从路径提取。
	model := gjson.GetBytes(body, "model").String()
	if model == "" && strings.Contains(req.URL.Path, "/v1beta/models/") {
		seg := strings.TrimPrefix(req.URL.Path, "/v1beta/models/")
		if i := strings.Index(seg, ":"); i > 0 {
			model = seg[:i]
		}
	}
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.models = append(u.models, model)
	u.mu.Unlock()
	// 按上游路径返回合法响应体，避免 200 后响应解析失败触发重试/挂起。
	var respBody string
	switch {
	case strings.Contains(req.URL.Path, "/responses"):
		respBody = `{"id":"resp_cm_ok","object":"response","model":"` + model + `","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`
	case strings.Contains(req.URL.Path, "/messages"):
		respBody = `{"id":"msg_cm_ok","type":"message","role":"assistant","model":"` + model + `","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	case strings.Contains(req.URL.Path, "/v1beta/models") || strings.Contains(req.URL.Path, "generateContent"):
		respBody = `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"modelVersion":"` + model + `"}`
	default:
		respBody = `{"id":"cm_ok","model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(respBody)),
	}, nil
}

// cmSchedulerCache 捕获选号边界 ctx（断言②身份恒等）。
type cmSchedulerCache struct {
	accounts []*service.Account
	mu       sync.Mutex
	ctxs     []context.Context
}

func (f *cmSchedulerCache) GetSnapshot(ctx context.Context, _ service.SchedulerBucket) ([]*service.Account, bool, error) {
	f.mu.Lock()
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	return f.accounts, true, nil
}
func (f *cmSchedulerCache) CaptureBucketWriteToken(context.Context, service.SchedulerBucket) (service.SchedulerBucketWriteToken, error) {
	return service.SchedulerBucketWriteToken{Bucket: service.SchedulerBucket{}, Epoch: 1}, nil
}
func (f *cmSchedulerCache) SetSnapshot(context.Context, service.SchedulerBucket, service.SchedulerBucketWriteToken, []service.Account) error {
	return nil
}
func (f *cmSchedulerCache) RetireBucket(context.Context, service.SchedulerBucket) error { return nil }
func (f *cmSchedulerCache) ReopenBucket(context.Context, service.SchedulerBucket) (service.SchedulerBucketWriteToken, error) {
	return service.SchedulerBucketWriteToken{Bucket: service.SchedulerBucket{}, Epoch: 1}, nil
}
func (f *cmSchedulerCache) TryAcquireGroupLifecycleLease(context.Context, int64, time.Duration) (service.SchedulerGroupLifecycleLease, bool, error) {
	return service.SchedulerGroupLifecycleLease{}, false, nil
}
func (f *cmSchedulerCache) ReleaseGroupLifecycleLease(context.Context, service.SchedulerGroupLifecycleLease) error {
	return nil
}
func (f *cmSchedulerCache) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	for _, a := range f.accounts {
		if a.ID == id {
			cp := *a
			return &cp, nil
		}
	}
	return nil, nil
}
func (f *cmSchedulerCache) SetAccount(context.Context, *service.Account) error        { return nil }
func (f *cmSchedulerCache) DeleteAccount(context.Context, int64) error                { return nil }
func (f *cmSchedulerCache) UpdateLastUsed(context.Context, map[int64]time.Time) error { return nil }
func (f *cmSchedulerCache) TryLockBucket(context.Context, service.SchedulerBucket, time.Duration) (bool, error) {
	return true, nil
}
func (f *cmSchedulerCache) UnlockBucket(context.Context, service.SchedulerBucket) error { return nil }
func (f *cmSchedulerCache) ListBuckets(context.Context) ([]service.SchedulerBucket, error) {
	return nil, nil
}
func (f *cmSchedulerCache) GetOutboxWatermark(context.Context) (int64, error) { return 0, nil }
func (f *cmSchedulerCache) SetOutboxWatermark(context.Context, int64) error   { return nil }

type cmGroupRepo struct {
	group *service.Group
}

func (f *cmGroupRepo) Create(context.Context, *service.Group) error { return nil }
func (f *cmGroupRepo) GetByID(context.Context, int64) (*service.Group, error) {
	return f.group, nil
}
func (f *cmGroupRepo) GetByIDLite(context.Context, int64) (*service.Group, error) {
	return f.group, nil
}
func (f *cmGroupRepo) Update(context.Context, *service.Group) error          { return nil }
func (f *cmGroupRepo) Delete(context.Context, int64) error                   { return nil }
func (f *cmGroupRepo) DeleteCascade(context.Context, int64) ([]int64, error) { return nil, nil }
func (f *cmGroupRepo) List(context.Context, pagination.PaginationParams) ([]service.Group, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (f *cmGroupRepo) ListWithFilters(context.Context, pagination.PaginationParams, string, string, string, *bool) ([]service.Group, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (f *cmGroupRepo) ListActive(context.Context) ([]service.Group, error) { return nil, nil }
func (f *cmGroupRepo) ListActiveByPlatform(context.Context, string) ([]service.Group, error) {
	return nil, nil
}
func (f *cmGroupRepo) ExistsByName(context.Context, string) (bool, error) { return false, nil }
func (f *cmGroupRepo) GetAccountCount(context.Context, int64) (int64, int64, error) {
	return 0, 0, nil
}
func (f *cmGroupRepo) DeleteAccountGroupsByGroupID(context.Context, int64) (int64, error) {
	return 0, nil
}
func (f *cmGroupRepo) GetAccountIDsByGroupIDs(context.Context, []int64) ([]int64, error) {
	return nil, nil
}
func (f *cmGroupRepo) BindAccountsToGroup(context.Context, int64, []int64) error { return nil }
func (f *cmGroupRepo) UpdateSortOrders(context.Context, []service.GroupSortOrderUpdate) error {
	return nil
}

type cmConcurrencyCache struct{}

func (f *cmConcurrencyCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	return true, nil
}
func (f *cmConcurrencyCache) ReleaseAccountSlot(context.Context, int64, string) error { return nil }
func (f *cmConcurrencyCache) GetAccountConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}
func (f *cmConcurrencyCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}
func (f *cmConcurrencyCache) DecrementAccountWaitCount(context.Context, int64) error { return nil }
func (f *cmConcurrencyCache) GetAccountWaitingCount(context.Context, int64) (int, error) {
	return 0, nil
}
func (f *cmConcurrencyCache) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	return true, nil
}
func (f *cmConcurrencyCache) ReleaseUserSlot(context.Context, int64, string) error   { return nil }
func (f *cmConcurrencyCache) GetUserConcurrency(context.Context, int64) (int, error) { return 0, nil }
func (f *cmConcurrencyCache) IncrementWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}
func (f *cmConcurrencyCache) DecrementWaitCount(context.Context, int64) error { return nil }
func (f *cmConcurrencyCache) AcquireUserGroupSlot(context.Context, int64, int64, int, string) (bool, error) {
	return true, nil
}
func (f *cmConcurrencyCache) ReleaseUserGroupSlot(context.Context, int64, int64, string) error {
	return nil
}
func (f *cmConcurrencyCache) GetUserGroupConcurrency(context.Context, int64, int64) (int, error) {
	return 0, nil
}
func (f *cmConcurrencyCache) IncrementUserGroupWaitCount(context.Context, int64, int64, int) (bool, error) {
	return true, nil
}
func (f *cmConcurrencyCache) DecrementUserGroupWaitCount(context.Context, int64, int64) error {
	return nil
}
func (f *cmConcurrencyCache) GetAccountsLoadBatch(context.Context, []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	return map[int64]*service.AccountLoadInfo{}, nil
}
func (f *cmConcurrencyCache) GetUsersLoadBatch(context.Context, []service.UserWithConcurrency) (map[int64]*service.UserLoadInfo, error) {
	return map[int64]*service.UserLoadInfo{}, nil
}
func (f *cmConcurrencyCache) GetAccountConcurrencyBatch(_ context.Context, accountIDs []int64) (map[int64]int, error) {
	result := make(map[int64]int, len(accountIDs))
	for _, id := range accountIDs {
		result[id] = 0
	}
	return result, nil
}
func (f *cmConcurrencyCache) CleanupExpiredAccountSlots(context.Context, int64) error { return nil }
func (f *cmConcurrencyCache) CleanupExpiredAccountSlotKeys(context.Context) error     { return nil }
func (f *cmConcurrencyCache) CleanupStaleProcessSlots(context.Context, string) error  { return nil }

// cmChannelService 构造带渠道映射 A→B 的 ChannelService。
func cmChannelService(groupID int64, platform string, mapping map[string]string) *service.ChannelService {
	return service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
		channels: []service.Channel{{
			ID:           7701,
			Name:         "cm-e2e-channel",
			Status:       service.StatusActive,
			GroupIDs:     []int64{groupID},
			ModelMapping: map[string]map[string]string{platform: mapping},
		}},
		groupPlatforms: map[int64]string{groupID: platform},
	}, nil, nil, nil, nil)
}

// cmRateLimitedExtra 生成账号 extra：scope=modelB 的模型级限流，未过期。
func cmRateLimitedExtra(modelB, futureReset string) map[string]any {
	return map[string]any{
		"model_rate_limits": map[string]any{
			modelB: map[string]any{"rate_limit_reset_at": futureReset},
		},
	}
}

func cmFutureReset() string {
	return time.Now().Add(10 * time.Minute).Format(time.RFC3339)
}

// cmNewGatewayHandler 构造主网关 handler（点 1-4），捕获选号 ctx，并注入渠道映射。
func cmNewGatewayHandler(t *testing.T, group *service.Group, accounts []*service.Account, platform string, withUpstream *cmHTTPUpstream) (*GatewayHandler, *cmSchedulerCache) {
	t.Helper()
	sched := &cmSchedulerCache{accounts: accounts}
	snapshot := service.NewSchedulerSnapshotService(sched, nil, nil, nil, nil)
	channelSvc := cmChannelService(group.ID, platform, map[string]string{cmModelA: cmModelB})
	gwSvc := service.NewGatewayService(
		nil, &cmGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		snapshot, nil, nil, nil, nil, nil, withUpstream, nil, nil, nil, nil, nil, nil,
		nil, channelSvc, nil, nil, nil, nil,
	)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	concurrencySvc := service.NewConcurrencyService(&cmConcurrencyCache{})
	concurrencyHelper := NewConcurrencyHelper(concurrencySvc, SSEPingFormatClaude, 0)
	// Gemini 网关经 GatewayHandler.GeminiV1BetaModels → GeminiMessagesCompatService.
	// ForwardNative 转发到注入的 cmHTTPUpstream（捕获 model B），故需在此接入该服务。
	geminiCompatSvc := service.NewGeminiMessagesCompatService(
		nil, nil, nil, nil, nil, nil, withUpstream, nil, cfg,
	)
	h := &GatewayHandler{
		gatewayService:              gwSvc,
		billingCacheService:         billingCacheSvc,
		concurrencyHelper:           concurrencyHelper,
		maxAccountSwitchesGemini:    1,
		geminiCompatService: geminiCompatSvc,
	}
	return h, sched
}

// cmDriveGateway 驱动主网关 handler 的一个入口方法，返回 gin 上下文。
func cmDriveGateway(t *testing.T, h *GatewayHandler, call func(*gin.Context), group *service.Group, groupID int64, path, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	c.Request = req
	apiKey := &service.APIKey{
		ID:      3001,
		UserID:  4001,
		GroupID: &groupID,
		Status:  service.StatusActive,
		User:    &service.User{ID: 4001, Concurrency: 10, Balance: 100, Status: service.StatusActive},
		Group:   group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	// Gemini 网关走 /v1beta/models/*modelAction 通配路由参数；直接调用 handler 时需
	// 在 gin context 注入该路由参数，否则 parseGeminiModelAction 因缺 path 返回 404。
	if strings.HasPrefix(path, "/v1beta/models") {
		rest := strings.TrimPrefix(path, "/v1beta/models")
		c.Params = gin.Params{{Key: "modelAction", Value: rest}}
	}
	call(c)
}

// 断言捕获的选号 ctx 中 ChannelMappedModel==B，且 X 被限流门排除、Y 可选。
func cmAssertGatewaySelection(t *testing.T, sched *cmSchedulerCache, accountX, accountY *service.Account) {
	t.Helper()
	sched.mu.Lock()
	ctxs := append([]context.Context(nil), sched.ctxs...)
	sched.mu.Unlock()
	require.NotEmpty(t, ctxs, "handler 应到达选号边界（捕获到 ctx）")
	ctx := ctxs[len(ctxs)-1]
	require.Equal(t, cmModelB, service.ChannelMappedModelFromContext(ctx),
		"选号边界 ctx 应携带渠道映射后的上游模型名 B（调度门判定身份）")
	// 断言①：X 被按映射名 B 排除（限流剩余时间 > 0）；Y 可选（== 0）。
	require.Greater(t, accountX.GetModelRateLimitRemainingTimeWithContext(ctx, cmModelA), time.Duration(0),
		"scope=B 的账号 X 应被模型级限流门按其映射名 B 排除")
	require.Equal(t, time.Duration(0), accountY.GetModelRateLimitRemainingTimeWithContext(ctx, cmModelA),
		"对照账号 Y 不应被限流门排除")
}

// cmOpenAIAccountRepo 捕获选号 ctx，并原样返回全部账号（让真实调度器套用限流门）。
type cmOpenAIAccountRepo struct {
	service.AccountRepository
	mu       sync.Mutex
	accounts []*service.Account
	ctxs     []context.Context
}

func (r *cmOpenAIAccountRepo) capture(ctx context.Context) {
	r.mu.Lock()
	r.ctxs = append(r.ctxs, ctx)
	r.mu.Unlock()
}

func (r *cmOpenAIAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, _ string) ([]service.Account, error) {
	r.capture(ctx)
	out := make([]service.Account, 0, len(r.accounts))
	for _, a := range r.accounts {
		out = append(out, *a)
	}
	return out, nil
}
func (r *cmOpenAIAccountRepo) ListSchedulableByPlatform(ctx context.Context, _ string) ([]service.Account, error) {
	r.capture(ctx)
	out := make([]service.Account, 0, len(r.accounts))
	for _, a := range r.accounts {
		out = append(out, *a)
	}
	return out, nil
}
func (r *cmOpenAIAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for _, a := range r.accounts {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, nil
}

// cmNewOpenAIGatewayHandler 构造 OpenAI 网关 handler（点 5-7）。
func cmNewOpenAIGatewayHandler(t *testing.T, group *service.Group, repo *cmOpenAIAccountRepo, platform string) (*OpenAIGatewayHandler, *cmHTTPUpstream) {
	t.Helper()
	channelSvc := cmChannelService(group.ID, platform, map[string]string{cmModelA: cmModelB})
	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	upstream := &cmHTTPUpstream{}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	gatewaySvc := service.NewOpenAIGatewayService(
		repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCacheSvc, upstream,
		&service.DeferredService{}, nil, nil, nil, channelSvc, nil, nil, nil, nil,
	)
	concurrencySvc := service.NewConcurrencyService(&concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	})
	concurrencyHelper := NewConcurrencyHelper(concurrencySvc, SSEPingFormatNone, time.Second)
	h := &OpenAIGatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   concurrencyHelper,
	}
	return h, upstream
}

func cmDriveOpenAIGateway(t *testing.T, h *OpenAIGatewayHandler, call func(*gin.Context), group *service.Group, groupID int64, path, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	c.Request = req
	apiKey := &service.APIKey{
		ID:      3101,
		UserID:  4101,
		GroupID: &groupID,
		Status:  service.StatusActive,
		User:    &service.User{ID: 4101, Concurrency: 10, Balance: 100, Status: service.StatusActive},
		Group:   group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
	call(c)
}

// ===================== 安装点 1：gateway_handler.go Messages =====================
func TestChannelMappedGatewayMessagesExcludesRateLimitedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(5101)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	future := cmFutureReset()
	accountX := &service.Account{
		ID: 5111, Name: "gw-msg-rate-limited", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:   map[string]any{"intercept_warmup_requests": true, "access_token": "tok-x"},
		Extra:         cmRateLimitedExtra(cmModelB, future),
		AccountGroups: []service.AccountGroup{{AccountID: 5111, GroupID: groupID}},
	}
	accountY := &service.Account{
		ID: 5112, Name: "gw-msg-healthy", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:   map[string]any{"intercept_warmup_requests": true, "access_token": "tok-y"},
		AccountGroups: []service.AccountGroup{{AccountID: 5112, GroupID: groupID}},
	}
	h, sched := cmNewGatewayHandler(t, group, []*service.Account{accountX, accountY}, service.PlatformAnthropic, &cmHTTPUpstream{})
	body := `{"model":"deepseek-v4.1-flash","max_tokens":10,"messages":[{"role":"user","content":"Warmup"}]}`
	cmDriveGateway(t, h, h.Messages, group, groupID, "/v1/messages", body)
	cmAssertGatewaySelection(t, sched, accountX, accountY)
}

// ===================== 安装点 2：gateway_handler_chat_completions.go =====================
func TestChannelMappedGatewayChatCompletionsExcludesRateLimitedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(5201)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive}
	future := cmFutureReset()
	accountX := &service.Account{
		ID: 5211, Name: "gw-cc-rate-limited", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:   map[string]any{"intercept_warmup_requests": true, "access_token": "tok-x"},
		Extra:         cmRateLimitedExtra(cmModelB, future),
		AccountGroups: []service.AccountGroup{{AccountID: 5211, GroupID: groupID}},
	}
	accountY := &service.Account{
		ID: 5212, Name: "gw-cc-healthy", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:   map[string]any{"intercept_warmup_requests": true, "access_token": "tok-y"},
		AccountGroups: []service.AccountGroup{{AccountID: 5212, GroupID: groupID}},
	}
	h, sched := cmNewGatewayHandler(t, group, []*service.Account{accountX, accountY}, service.PlatformOpenAI, &cmHTTPUpstream{})
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"Warmup"}],"stream":false}`
	cmDriveGateway(t, h, h.ChatCompletions, group, groupID, "/v1/chat/completions", body)
	cmAssertGatewaySelection(t, sched, accountX, accountY)
}

// ===================== 安装点 3：gateway_handler_responses.go =====================
func TestChannelMappedGatewayResponsesExcludesRateLimitedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(5301)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	future := cmFutureReset()
	accountX := &service.Account{
		ID: 5311, Name: "gw-resp-rate-limited", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:   map[string]any{"intercept_warmup_requests": true, "access_token": "tok-x"},
		Extra:         cmRateLimitedExtra(cmModelB, future),
		AccountGroups: []service.AccountGroup{{AccountID: 5311, GroupID: groupID}},
	}
	accountY := &service.Account{
		ID: 5312, Name: "gw-resp-healthy", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:   map[string]any{"intercept_warmup_requests": true, "access_token": "tok-y"},
		AccountGroups: []service.AccountGroup{{AccountID: 5312, GroupID: groupID}},
	}
	h, sched := cmNewGatewayHandler(t, group, []*service.Account{accountX, accountY}, service.PlatformAnthropic, &cmHTTPUpstream{})
	body := `{"model":"deepseek-v4.1-flash","input":"Warmup","stream":false}`
	cmDriveGateway(t, h, h.Responses, group, groupID, "/v1/responses", body)
	cmAssertGatewaySelection(t, sched, accountX, accountY)
}

// ===================== 安装点 4：gemini_v1beta_handler.go（经 GatewayHandler.GeminiV1BetaModels） =====================
func TestChannelMappedGeminiExcludesRateLimitedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(5401)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformGemini, Status: service.StatusActive}
	future := cmFutureReset()
	accountX := &service.Account{
		ID: 5411, Name: "gem-rate-limited", Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:   map[string]any{"api_key": "sk-x", "base_url": "https://unused.invalid"},
		Extra:         cmRateLimitedExtra(cmModelB, future),
		AccountGroups: []service.AccountGroup{{AccountID: 5411, GroupID: groupID}},
	}
	accountY := &service.Account{
		ID: 5412, Name: "gem-healthy", Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:   map[string]any{"api_key": "sk-y", "base_url": "https://unused.invalid"},
		AccountGroups: []service.AccountGroup{{AccountID: 5412, GroupID: groupID}},
	}
	upstream := &cmHTTPUpstream{}
	h, sched := cmNewGatewayHandler(t, group, []*service.Account{accountX, accountY}, service.PlatformGemini, upstream)
	// gemini 模型名取自 URL 路径参数 model。
	body := `{"contents":[{"parts":[{"text":"hello"}]}]}`
	cmDriveGateway(t, h, h.GeminiV1BetaModels, group, groupID, "/v1beta/models/deepseek-v4.1-flash:generateContent", body)
	cmAssertGatewaySelection(t, sched, accountX, accountY)
	// 断言②补充：转发的上游桩收到的模型名应为 B（调度门判定身份 == 转发实际身份）。
	upstream.mu.Lock()
	models := append([]string(nil), upstream.models...)
	upstream.mu.Unlock()
	require.Contains(t, models, cmModelB, "gemini 转发到上游的模型名应为渠道映射后的 B")
}

// ===================== 安装点 5：openai_gateway_handler.go Responses（真实 e2e） =====================
func TestChannelMappedOpenAIResponsesExcludesRateLimitedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(5501)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive}
	future := cmFutureReset()
	accountX := &service.Account{
		ID: 5511, Name: "oa-resp-rate-limited", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1,
		Credentials:   map[string]any{"api_key": "sk-x", "base_url": "https://unused.invalid"},
		Extra:         cmRateLimitedExtra(cmModelB, future),
		AccountGroups: []service.AccountGroup{{AccountID: 5511, GroupID: groupID}},
	}
	accountY := &service.Account{
		ID: 5512, Name: "oa-resp-healthy", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 2,
		Credentials:   map[string]any{"api_key": "sk-y", "base_url": "https://unused.invalid"},
		AccountGroups: []service.AccountGroup{{AccountID: 5512, GroupID: groupID}},
	}
	repo := &cmOpenAIAccountRepo{accounts: []*service.Account{accountX, accountY}}
	h, upstream := cmNewOpenAIGatewayHandler(t, group, repo, service.PlatformOpenAI)
	body := `{"model":"deepseek-v4.1-flash","input":"hello","stream":false}`
	cmDriveOpenAIGateway(t, h, h.Responses, group, groupID, "/openai/v1/responses", body)
	// 断言②：调度门判定身份 == 转发实际身份（上游收到 B）。
	upstream.mu.Lock()
	models := append([]string(nil), upstream.models...)
	ids := append([]int64(nil), upstream.accountIDs...)
	upstream.mu.Unlock()
	require.Contains(t, models, cmModelB, "OpenAI Responses 转发到上游的模型名应为渠道映射后的 B")
	// 断言①：被限流门按其映射名 B 排除的账号 X 不应被选中，落到了 Y。
	require.NotContains(t, ids, int64(5511), "scope=B 的账号 X 应被模型级限流门排除")
	require.Contains(t, ids, int64(5512), "请求应落到未被限流的账号 Y")
}

// ===================== 安装点 6：openai_gateway_handler.go Messages（Anthropic 兼容，真实 e2e） =====================
func TestChannelMappedOpenAIMessagesExcludesRateLimitedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(5601)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowMessagesDispatch: true}
	future := cmFutureReset()
	accountX := &service.Account{
		ID: 5611, Name: "oa-msg-rate-limited", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1,
		Credentials:   map[string]any{"api_key": "sk-x", "base_url": "https://unused.invalid"},
		Extra:         cmRateLimitedExtra(cmModelB, future),
		AccountGroups: []service.AccountGroup{{AccountID: 5611, GroupID: groupID}},
	}
	accountY := &service.Account{
		ID: 5612, Name: "oa-msg-healthy", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 2,
		Credentials:   map[string]any{"api_key": "sk-y", "base_url": "https://unused.invalid"},
		AccountGroups: []service.AccountGroup{{AccountID: 5612, GroupID: groupID}},
	}
	repo := &cmOpenAIAccountRepo{accounts: []*service.Account{accountX, accountY}}
	h, upstream := cmNewOpenAIGatewayHandler(t, group, repo, service.PlatformOpenAI)
	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}],"stream":false}`
	cmDriveOpenAIGateway(t, h, h.Messages, group, groupID, "/openai/v1/messages", body)
	upstream.mu.Lock()
	models := append([]string(nil), upstream.models...)
	ids := append([]int64(nil), upstream.accountIDs...)
	upstream.mu.Unlock()
	require.Contains(t, models, cmModelB, "OpenAI Messages 转发到上游的模型名应为渠道映射后的 B")
	require.NotContains(t, ids, int64(5611), "scope=B 的账号 X 应被模型级限流门排除")
	require.Contains(t, ids, int64(5612), "请求应落到未被限流的账号 Y")
}

// ===================== 安装点 7：openai_gateway_handler.go ResponsesWebSocket（真实 WS e2e） =====================
func TestChannelMappedOpenAIWSResponsesExcludesRateLimitedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(5701)
	modelB := cmModelB
	future := cmFutureReset()

	upstreamModelCh := make(chan string, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
		msgType, payload, readErr := conn.Read(readCtx)
		cancelRead()
		if readErr != nil {
			return
		}
		if msgType == coderws.MessageText || msgType == coderws.MessageBinary {
			upstreamModelCh <- gjson.GetBytes(payload, "model").String()
		}
		response := `{"type":"response.completed","response":{"id":"resp_cm_ws","model":` + `"` + gjson.GetBytes(payload, "model").String() + `"` + `,"usage":{"input_tokens":1,"output_tokens":1}}}`
		writeCtx, cancelWrite := context.WithTimeout(r.Context(), 3*time.Second)
		_ = conn.Write(writeCtx, coderws.MessageText, []byte(response))
		cancelWrite()
	}))
	defer upstreamServer.Close()

	accountX := &service.Account{
		ID: 5711, Name: "oa-ws-rate-limited", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1,
		Credentials: map[string]any{"api_key": "sk-x", "base_url": "https://unused.invalid"},
		Extra:       cmRateLimitedExtra(modelB, future),
	}
	accountY := &service.Account{
		ID: 5712, Name: "oa-ws-healthy", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 2,
		Credentials: map[string]any{"api_key": "sk-y", "base_url": upstreamServer.URL},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
		},
	}
	repo := &cmOpenAIAccountRepo{accounts: []*service.Account{accountX, accountY}}

	channelSvc := cmChannelService(groupID, service.PlatformOpenAI, map[string]string{cmModelA: cmModelB})
	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	gatewaySvc := service.NewOpenAIGatewayService(
		repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCacheSvc, nil,
		&service.DeferredService{}, nil, nil, nil, channelSvc, nil, nil, nil, nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := &OpenAIGatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}

	apiKey := &service.APIKey{
		ID:      3701,
		GroupID: &groupID,
		User:    &service.User{ID: 4701, Status: service.StatusActive},
		Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	handlerServer := httptest.NewServer(router)
	defer handlerServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(handlerServer.URL, "http")+"/openai/v1/responses", nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"deepseek-v4.1-flash","stream":false}`))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
	_, event, err := clientConn.Read(readCtx)
	cancelRead()
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())

	select {
	case m := <-upstreamModelCh:
		require.Equal(t, modelB, m, "OpenAI WS 转发到上游的模型名应为渠道映射后的 B（断言②身份恒等）")
	case <-time.After(3 * time.Second):
		t.Fatal("上游桩未收到请求")
	}
	// 断言①：账号 X 被按映射名 B 排除（真实调度器未将其选入）。
	repo.mu.Lock()
	ctxs := append([]context.Context(nil), repo.ctxs...)
	repo.mu.Unlock()
	require.NotEmpty(t, ctxs, "WS 路径应到达选号边界")
	lastCtx := ctxs[len(ctxs)-1]
	require.Equal(t, modelB, service.ChannelMappedModelFromContext(lastCtx),
		"WS 选号边界 ctx 应携带渠道映射后的上游模型名 B")
	require.Greater(t, accountX.GetModelRateLimitRemainingTimeWithContext(lastCtx, cmModelA), time.Duration(0),
		"scope=B 的账号 X 应被模型级限流门按其映射名 B 排除")
	require.Equal(t, time.Duration(0), accountY.GetModelRateLimitRemainingTimeWithContext(lastCtx, cmModelA),
		"对照账号 Y 不应被限流门排除")
}

// ===================== 回归：无渠道映射 / scope 不匹配时不应误伤 =====================
func TestChannelMappedNoMappingDoesNotExcludeAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(5801)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	// 无渠道映射：ResolveChannelMappingAndRestrict 返回 Mapped=false，ctx 中不注入 mapped 名。
	// 账号限流 scope 是另一个名字（与 A/B 都不匹配），证明限流门不误伤。
	account := &service.Account{
		ID: 5811, Name: "gw-nomap", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:   map[string]any{"intercept_warmup_requests": true, "access_token": "tok"},
		Extra:         cmRateLimitedExtra("other-model", cmFutureReset()),
		AccountGroups: []service.AccountGroup{{AccountID: 5811, GroupID: groupID}},
	}
	// 故意不注入渠道映射（nil channelService）：NewGatewayService 第 channelService 参数传 nil。
	sched := &cmSchedulerCache{accounts: []*service.Account{account}}
	snapshot := service.NewSchedulerSnapshotService(sched, nil, nil, nil, nil)
	gwSvc := service.NewGatewayService(
		nil, &cmGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, nil,
		snapshot, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil,
	)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	concurrencySvc := service.NewConcurrencyService(&cmConcurrencyCache{})
	concurrencyHelper := NewConcurrencyHelper(concurrencySvc, SSEPingFormatClaude, 0)
	h := &GatewayHandler{
		gatewayService:           gwSvc,
		billingCacheService:      billingCacheSvc,
		concurrencyHelper:        concurrencyHelper,
		maxAccountSwitchesGemini: 1,
	}
	body := `{"model":"deepseek-v4.1-flash","max_tokens":10,"messages":[{"role":"user","content":"Warmup"}]}`
	cmDriveGateway(t, h, h.Messages, group, groupID, "/v1/messages", body)

	sched.mu.Lock()
	ctxs := append([]context.Context(nil), sched.ctxs...)
	sched.mu.Unlock()
	require.NotEmpty(t, ctxs)
	ctx := ctxs[len(ctxs)-1]
	require.Equal(t, "", service.ChannelMappedModelFromContext(ctx),
		"无渠道映射时 ctx 不应注入 mapped 模型名")
	// 限流 scope 与请求模型不匹配 → 账号不应被限流门排除（不误伤）。
	require.Equal(t, time.Duration(0), account.GetModelRateLimitRemainingTimeWithContext(ctx, cmModelA),
		"scope 不匹配时该账号不应被限流门排除（回归不误伤）")
}
