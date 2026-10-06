package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// codeBuddyQuotaRepoStub 是 CodeBuddyQuotaService 测试用 AccountRepository 桩：
// 在 stubOpenAIAccountRepo（无 build tag 全量桩）之上仅覆盖 QueryUsage/FetchModels 实际调用的几个方法。
// 同时实现 Card B 的 CodeBuddyConditionalExtraWriter 窄接口，捕获快照/错误条件更新写入。
type codeBuddyQuotaRepoStub struct {
	stubOpenAIAccountRepo
	getByID        *Account
	updateExtra    int
	lastExtra      map[string]any
	nextVer        int64
	nextVerCalls   int
	nextVerErr     error
	snapshotWrites []CodeBuddyCreditSnapshotWrite
	snapshotErr    error
	snapshotReject bool
	errorWrites    []CodeBuddyCreditAttemptErrorWrite
}

func (r *codeBuddyQuotaRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.getByID != nil {
		return r.getByID, nil
	}
	return &Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Status:      StatusActive,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "t"},
	}, nil
}

func (r *codeBuddyQuotaRepoStub) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.updateExtra++
	r.lastExtra = updates
	return nil
}

// --- CodeBuddyConditionalExtraWriter 窄接口实现（Card B 唯一条件更新写口） ---

func (r *codeBuddyQuotaRepoStub) NextCodeBuddyCreditAttemptVersion(_ context.Context) (int64, error) {
	r.nextVerCalls++
	if r.nextVerErr != nil {
		return 0, r.nextVerErr
	}
	return r.nextVer, nil
}

func (r *codeBuddyQuotaRepoStub) WriteCodeBuddyCreditSnapshot(_ context.Context, _ int64, write CodeBuddyCreditSnapshotWrite) (bool, error) {
	r.snapshotWrites = append(r.snapshotWrites, write)
	if r.snapshotErr != nil {
		return false, r.snapshotErr
	}
	return !r.snapshotReject, nil
}

func (r *codeBuddyQuotaRepoStub) WriteCodeBuddyCreditAttemptError(_ context.Context, _ int64, write CodeBuddyCreditAttemptErrorWrite) (bool, error) {
	r.errorWrites = append(r.errorWrites, write)
	return true, nil
}

func healthyCodeBuddyQuotaAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Status:      StatusActive,
		Type:        AccountTypeOAuth,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"uid":           "u-1",
			"enterprise_id": "e-1",
			"domain":        "tencent.com",
		},
	}
}

// TestCodeBuddyQuotaService_QueryUsageParsesRealBillingSchema 用真实抓包结构（脱敏
// fixture：data.Response.Data.{TotalCount,TotalDosage,Accounts[]}，PascalCase）验证
// Card A 分包解析 + Card B 条件更新写路径：两个 Status=0 分包（500/1500）聚合，
// used_percent=0，最晚 CycleEndTime 为 reset_at；成功事务写分包快照 + 版本号，
// 且不再走旧 UpdateExtra 写口（updateExtra 必须保持 0）。
func TestCodeBuddyQuotaService_QueryUsageParsesRealBillingSchema(t *testing.T) {
	fixture, err := os.ReadFile("testdata/codebuddy_billing_get_user_resource.json")
	require.NoError(t, err)

	account := healthyCodeBuddyQuotaAccount(8801)
	// 真实响应不含 OAuth uid（仅有 AccountId/Uin/ResourceId/BindRecords[].BindObjectId），
	// 解析器按 uid 命不中任何账号时退回响应中的全部账号（该请求以 X-User-Id 认证）。
	account.Credentials["uid"] = "db237973-4482-49ff-9872-1bab5ab94b16"
	repo := &codeBuddyQuotaRepoStub{getByID: account, nextVer: 7}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(fixture)),
	}}

	svc := NewCodeBuddyQuotaService(repo, nil, upstream, &config.Config{})
	result, err := svc.QueryUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, 0.0, result.UsedPercent, "抓包中两个资源包 CapacityUsed 均为 0")
	require.Equal(t, 2000.0, result.TotalCredit, "500 + 1500")
	require.Equal(t, 0.0, result.UsedCredit)
	// 最晚 CycleEndTime = 2026-10-13 07:46:09 (UTC+8) → 2026-10-12T23:46:09Z
	require.Equal(t, "2026-10-12T23:46:09Z", result.ResetAt)

	// Card B 唯一条件更新写口：成功快照通过 WriteCodeBuddyCreditSnapshot 落库。
	require.Equal(t, 1, repo.nextVerCalls, "抓取开始必须取一次 attempt_version")
	require.Len(t, repo.snapshotWrites, 1, "成功快照应走条件更新写口")
	write := repo.snapshotWrites[0]
	require.Equal(t, int64(7), write.Version, "version 必须 = 抓取开始取得的 attempt_version")
	require.Len(t, write.Packages, 2, "两个分包都应进入快照")
	require.Equal(t, 0.0, write.UsedPercent)
	require.False(t, write.SuccessTime.IsZero(), "成功时刻必须采样")
	require.Equal(t, "2026-10-12T23:46:09Z", write.ResetAt.UTC().Format(time.RFC3339))
	require.Empty(t, write.ErrorMsg, "无解析错误时应清旧错误")
	// 旧 UpdateExtra 写口零调用（写路径全量切换，禁双写口）。
	require.Equal(t, 0, repo.updateExtra, "快照不得再走旧 UpdateExtra 写口")
}

// TestParseCodeBuddyCreditUsage_ComputesPercentFromCapacityCounters 验证容量计数器
// 的聚合口径（sum(used)/sum(size)）与最晚 CycleEndTime 取值为重置时间。
func TestParseCodeBuddyCreditUsage_ComputesPercentFromCapacityCounters(t *testing.T) {
	body := []byte(`{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":2000,"Accounts":[
		{"AccountId":1,"CapacitySize":500,"CapacityUsed":100,"CapacityRemain":400,"CycleEndTime":"2026-09-30 23:59:59","Status":0},
		{"AccountId":2,"CapacitySize":1500,"CapacityUsed":300,"CapacityRemain":1200,"CycleEndTime":"2026-10-13 07:46:09","Status":0}
	]}}}}`)

	usage, ok := parseCodeBuddyCreditUsage(body, "unmatched-uid")
	require.True(t, ok)
	require.Equal(t, 2000.0, usage.TotalCredit)
	require.Equal(t, 400.0, usage.UsedCredit)
	require.InDelta(t, 20.0, usage.UsedPercent, 1e-9)
	require.True(t, usage.HasResetAt)
	// 取最晚的未来周期结束时间（聚合用量需全部资源包滚动后才归零）。
	require.Equal(t, "2026-10-12T23:46:09Z", usage.ResetAt.UTC().Format(time.RFC3339))
}

// TestParseCodeBuddyCreditUsage_PreciseStringFallback 验证整型计数器缺失时回退到
// *Precise 数值字符串字段（真实报文中二者并存）。
func TestParseCodeBuddyCreditUsage_PreciseStringFallback(t *testing.T) {
	body := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"AccountId":1,"CapacitySizePrecise":"500","CapacityUsedPrecise":"50","CycleCapacitySizePrecise":"500","CycleCapacityUsedPrecise":"50","CycleEndTime":"2026-09-30 23:59:59"}
	]}}}}`)

	usage, ok := parseCodeBuddyCreditUsage(body, "")
	require.True(t, ok)
	require.Equal(t, 500.0, usage.TotalCredit)
	require.Equal(t, 50.0, usage.UsedCredit)
	require.InDelta(t, 10.0, usage.UsedPercent, 1e-9)
}

// TestParseCodeBuddyCreditUsage_NoResetFieldWithoutCycleEndTime 验证：CycleEndTime
// 缺失时不写重置时间（HasResetAt=false，由调用方回退默认窗口），不做猜测。
func TestParseCodeBuddyCreditUsage_NoResetFieldWithoutCycleEndTime(t *testing.T) {
	body := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
		{"AccountId":1,"CapacitySize":500,"CapacityUsed":100,"DeductionEndTime":2049579968000,"ExpiredTime":""}
	]}}}}`)

	usage, ok := parseCodeBuddyCreditUsage(body, "")
	require.True(t, ok)
	require.False(t, usage.HasResetAt, "无 CycleEndTime 时不得用 DeductionEndTime 冒充重置时间")
	require.True(t, usage.ResetAt.IsZero())
}

// TestParseCodeBuddyCreditUsage_RejectsUnexpectedSchema 验证旧 camelCase 结构
// （PR3 假定的 schema）不再被当作有效解析结果。
func TestParseCodeBuddyCreditUsage_RejectsUnexpectedSchema(t *testing.T) {
	_, ok := parseCodeBuddyCreditUsage([]byte(`{"code":0,"data":{"creditUsedPercent":92}}`), "u")
	require.False(t, ok)
}

// TestParseCodeBuddyCreditDecimal_SinglePath 验证 decimal 字符串单一口径：合法字符串
// 解析并量化到 NUMERIC(20,8)；被 float64 兜底接受过的形态（非字符串 / 非定点
// 字符串）一律失败关闭，不做 float64/量化降级。
func TestParseCodeBuddyCreditDecimal_SingleDecimalPath(t *testing.T) {
	// 合法定点字符串解析成功并按 NUMERIC(20,8) 量化。
	for _, tc := range []struct{ in, want string }{
		{"207.49000063", "207.49000063"},
		{" 500 ", "500"},
		{"0.123456789", "0.12345679"}, // >8 位小数被截断量化
	} {
		d, err := parseCodeBuddyCreditDecimal(tc.in)
		require.NoError(t, err, "合法字符串 %q 必须解析成功", tc.in)
		require.Equal(t, tc.want, d.String(), "NUMERIC(20,8) 量化口径 %q", tc.in)
	}

	// 非法字符串（float64 兜底此前能接住）→ 失败关闭。
	for _, in := range []string{"", "  ", "not-a-number", "12abc"} {
		_, err := parseCodeBuddyCreditDecimal(in)
		require.Error(t, err, "非法字符串 %q 必须失败关闭，不得回退 float64", in)
	}
}

// codeBuddyModelsRecorder 构造一个返回动态模型列表 JSON 的 HTTP 桩（每次调用返回全新响应体，
// 避免响应体被单次读取后耗尽导致复用失败）。
func codeBuddyModelsRecorder(body string) *httpUpstreamRecorder {
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}}
}

// TestCodeBuddyQuotaService_SupportedEffortsFromModelList 验证动态模型列表拉取与
// SupportedEffortsForModel 的缓存命中 / 缓存未命中惰性拉取两条路径。
func TestCodeBuddyQuotaService_SupportedEffortsFromModelList(t *testing.T) {
	account := healthyCodeBuddyQuotaAccount(8802)
	repo := &codeBuddyQuotaRepoStub{getByID: account}

	modelsBody := `{"code":0,"data":{"models":[{"id":"codebuddy-model-a","name":"Model A","supportedEfforts":["low","medium","high"]}]}}`

	// 1) 显式 FetchModels 后命中进程内缓存。
	svc := NewCodeBuddyQuotaService(repo, nil, codeBuddyModelsRecorder(modelsBody), &config.Config{})
	models, err := svc.FetchModels(context.Background(), account.ID)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "codebuddy-model-a", models[0].ID)
	require.Equal(t, []string{"low", "medium", "high"}, svc.SupportedEffortsForModel(context.Background(), account, "codebuddy-model-a"))

	// 2) 独立实例（独立缓存）在缓存未命中时惰性拉取模型列表。
	svc2 := NewCodeBuddyQuotaService(repo, nil, codeBuddyModelsRecorder(modelsBody), &config.Config{})
	require.Equal(t, []string{"low", "medium", "high"}, svc2.SupportedEffortsForModel(context.Background(), account, "codebuddy-model-a"))
}

func TestCodeBuddyQuotaService_IntlStaticModels(t *testing.T) {
	account := &Account{ID: 8803, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "at", "site": "intl"}}
	repo := &codeBuddyQuotaRepoStub{getByID: account}
	upstream := &httpUpstreamRecorder{}
	svc := &CodeBuddyQuotaService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg: &config.Config{Gateway: config.GatewayConfig{CodeBuddy: config.GatewayCodeBuddyConfig{
			StaticModelsIntl: " deepseek-v3, GLM-4, deepseek-v3, GLM-4, , ",
		}}},
	}

	models, err := svc.FetchModels(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, []CodeBuddyModel{{ID: "deepseek-v3"}, {ID: "GLM-4"}}, models)
	require.Empty(t, upstream.requests, "configured intl static models must not call upstream")
	require.Equal(t, models, svc.cachedModels(account.ID), "static models must populate the process cache")
}

func TestCodeBuddyQuotaService_IntlStaticModelsEmptyPreservesError(t *testing.T) {
	account := &Account{ID: 8804, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "at", "site": "intl"}}
	repo := &codeBuddyQuotaRepoStub{getByID: account}
	upstream := &httpUpstreamRecorder{}
	svc := &CodeBuddyQuotaService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg:          &config.Config{},
	}

	models, err := svc.FetchModels(context.Background(), account.ID)
	require.Error(t, err)
	require.Nil(t, models)
	require.Empty(t, upstream.requests, "empty static config must preserve no-upstream intl fallback")
}

func TestCodeBuddyQuotaService_FetchModelsInitializesNilCache(t *testing.T) {
	account := healthyCodeBuddyQuotaAccount(8805)
	repo := &codeBuddyQuotaRepoStub{getByID: account}
	upstream := codeBuddyModelsRecorder(`{"code":0,"data":{"models":[{"id":"model-a"}]}}`)
	svc := &CodeBuddyQuotaService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}

	models, err := svc.FetchModels(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, []CodeBuddyModel{{ID: "model-a"}}, models)
	require.Equal(t, models, svc.cachedModels(account.ID))
}
