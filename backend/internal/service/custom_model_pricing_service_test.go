package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- CustomModelPricingService.MatchCustomModelPricing ---

func TestCustomModelPricingMatch_ExactAndWildcard(t *testing.T) {
	in := 0.5
	out := 2.0
	svc := NewCustomModelPricingService(nil)
	svc.entries = []CustomModelPricing{
		{ID: 1, Models: []string{"gpt-5.6-sol"}, Enabled: true, InputPrice: &in, OutputPrice: &out},
		{ID: 2, Models: []string{"my-alias-*"}, Enabled: true},
		{ID: 3, Models: []string{"disabled-model"}, Enabled: false},
	}

	// 精确命中：大小写不敏感；返回 Clone，改写不污染缓存
	hit := svc.MatchCustomModelPricing("GPT-5.6-SOL")
	require.NotNil(t, hit)
	assert.Equal(t, int64(1), hit.ID)
	require.NotNil(t, hit.InputPrice)
	assert.Equal(t, in, *hit.InputPrice)
	hit.InputPrice = nil
	assert.NotNil(t, svc.MatchCustomModelPricing("gpt-5.6-sol").InputPrice)

	// 通配命中
	wild := svc.MatchCustomModelPricing("my-alias-pro")
	require.NotNil(t, wild)
	assert.Equal(t, int64(2), wild.ID)

	// 禁用条目不参与匹配
	assert.Nil(t, svc.MatchCustomModelPricing("disabled-model"))

	// 未命中
	assert.Nil(t, svc.MatchCustomModelPricing("claude-sonnet-4"))
	assert.Nil(t, svc.MatchCustomModelPricing(""))
}

// --- 解析链：custom 层生效与合并语义 ---

type staticCustomPricingProvider struct {
	entries []CustomModelPricing
}

func (p *staticCustomPricingProvider) MatchCustomModelPricing(model string) *ChannelModelPricing {
	for i := range p.entries {
		entry := &p.entries[i]
		for _, m := range entry.Models {
			if m == model {
				return entry.ToChannelModelPricing()
			}
		}
	}
	return nil
}

// newPricingTestBillingService 构造带全局价格表种子的 BillingService。
func newPricingTestBillingService(t *testing.T, models map[string]*LiteLLMModelPricing) *BillingService {
	t.Helper()
	pricing := NewPricingService(nil, nil)
	pricing.pricingData = models
	return NewBillingService(nil, pricing)
}

func TestResolver_CustomLayerMergesOverGlobal(t *testing.T) {
	customIn := 1e-5 // $10 / MTok，覆盖 gpt-5.6-sol 内置的 $5
	custom := &staticCustomPricingProvider{entries: []CustomModelPricing{
		{ID: 9, Models: []string{"gpt-5.6-sol"}, Enabled: true, BillingMode: BillingModeToken, InputPrice: &customIn},
	}}
	bs := newPricingTestBillingService(t, map[string]*LiteLLMModelPricing{
		"gpt-5.6-sol": {InputCostPerToken: 5e-6, OutputCostPerToken: 3e-5},
	})
	resolver := NewModelPricingResolver(nil, bs)
	resolver.SetCustomPricingProvider(custom)

	// custom 生效：input 覆盖为 $10/MTok，output 沿用全局价 $30/MTok（合并语义）
	resolved := resolver.Resolve(context.Background(), PricingInput{Model: "gpt-5.6-sol"})
	require.NotNil(t, resolved.BasePricing)
	assert.Equal(t, PricingSourceCustom, resolved.Source)
	assert.Equal(t, customIn, resolved.BasePricing.InputPricePerToken)
	assert.InDelta(t, 3e-5, resolved.BasePricing.OutputPricePerToken, 1e-12)

	// 未命中 custom 的模型回退全局表
	resolved2 := resolver.Resolve(context.Background(), PricingInput{Model: "gpt-5.6-terra"})
	require.NotNil(t, resolved2.BasePricing)
	assert.Equal(t, PricingSourceLiteLLM, resolved2.Source)
	assert.Equal(t, 2e-6, resolved2.BasePricing.InputPricePerToken)
}

func TestResolver_CustomLayerPerRequestMode(t *testing.T) {
	perCall := 0.01
	custom := &staticCustomPricingProvider{entries: []CustomModelPricing{
		{ID: 5, Models: []string{"my-video-model"}, Enabled: true, BillingMode: BillingModeVideo, PerRequestPrice: &perCall},
	}}
	bs := newPricingTestBillingService(t, map[string]*LiteLLMModelPricing{})
	resolver := NewModelPricingResolver(nil, bs)
	resolver.SetCustomPricingProvider(custom)

	resolved := resolver.Resolve(context.Background(), PricingInput{Model: "my-video-model"})
	assert.Equal(t, BillingModeVideo, resolved.Mode)
	assert.Equal(t, PricingSourceCustom, resolved.Source)
	assert.InDelta(t, perCall, resolved.DefaultPerRequestPrice, 1e-12)
}

func TestResolver_NilCustomProvider_UnchangedBehavior(t *testing.T) {
	bs := newPricingTestBillingService(t, map[string]*LiteLLMModelPricing{
		"gpt-5.6-sol": {InputCostPerToken: 5e-6, OutputCostPerToken: 3e-5},
	})
	resolver := NewModelPricingResolver(nil, bs)
	resolved := resolver.Resolve(context.Background(), PricingInput{Model: "gpt-5.6-sol"})
	require.NotNil(t, resolved.BasePricing)
	assert.Equal(t, PricingSourceLiteLLM, resolved.Source)
	assert.Equal(t, 5e-6, resolved.BasePricing.InputPricePerToken)
	assert.Equal(t, 3e-5, resolved.BasePricing.OutputPricePerToken)
}

// --- PricingService 线上缺口记录 ---

func TestPricingService_RecordPricingGap(t *testing.T) {
	svc := NewPricingService(nil, nil)
	svc.RecordPricingGap("model-a")
	svc.RecordPricingGap("model-a")
	svc.RecordPricingGap("model-b")

	gaps := svc.PricingGaps()
	require.Len(t, gaps, 2)
	byModel := map[string]PricingGapEntry{}
	for _, g := range gaps {
		byModel[g.Model] = g
	}
	assert.Equal(t, int64(2), byModel["model-a"].Count)
	assert.Equal(t, int64(1), byModel["model-b"].Count)
}

// --- PricingAdminService 目录合并与未覆盖扫描 ---

type fakeGroupRepoForPricing struct {
	GroupRepository
	groups []Group
}

func (f *fakeGroupRepoForPricing) ListActive(ctx context.Context) ([]Group, error) {
	return f.groups, nil
}

type fakeUsageRepoForPricing struct {
	UsageLogRepository
	stats []usagestats.ModelStat
}

func (f *fakeUsageRepoForPricing) GetModelStatsWithFilters(ctx context.Context, startTime, endTime time.Time, userID, apiKeyID, accountID, groupID int64, requestType *int16, stream *bool, billingType *int8) ([]usagestats.ModelStat, error) {
	return f.stats, nil
}

func TestPricingAdminService_GetCatalog_MergesSources(t *testing.T) {
	customPrice := 7e-6
	customSvc := NewCustomModelPricingService(nil)
	customSvc.entries = []CustomModelPricing{
		{ID: 3, Models: []string{"gpt-5.6-sol"}, Enabled: true, InputPrice: &customPrice},
	}
	svc := &PricingAdminService{
		pricing: newPricingTestBillingService(t, map[string]*LiteLLMModelPricing{
			"gpt-5.6-sol": {InputCostPerToken: 5e-6},
		}).pricingService,
		billing: newPricingTestBillingService(t, map[string]*LiteLLMModelPricing{
			"gpt-5.6-sol": {InputCostPerToken: 5e-6},
		}),
		custom: customSvc,
	}

	result := svc.GetCatalog("gpt-5.6", "", 1, 50)
	byModel := map[string]CatalogEntry{}
	for _, item := range result.Items {
		byModel[item.Model] = item
	}
	require.Contains(t, byModel, "gpt-5.6-sol")
	assert.Equal(t, PricingSourceCustom, byModel["gpt-5.6-sol"].Source)
	assert.InDelta(t, 7, byModel["gpt-5.6-sol"].InputPerMTok, 1e-9)
}

func TestPricingAdminService_ScanUncovered(t *testing.T) {
	in := 1e-5
	groups := []Group{
		{ID: 1, Name: "已覆盖分组", Platform: "openai", ModelPricing: []ChannelModelPricing{
			{Models: []string{"covered-by-group"}, BillingMode: BillingModeToken, InputPrice: &in},
		}},
		{ID: 2, Name: "裸分组", Platform: "openai", ModelsListConfig: GroupModelsListConfig{
			Enabled: true,
			Models:  []string{"brand-new-model"},
		}},
	}
	usageRepo := &fakeUsageRepoForPricing{stats: []usagestats.ModelStat{
		{Model: "brand-new-model", Requests: 10, TotalTokens: 1000, ActualCost: 0},
	}}
	svc := &PricingAdminService{
		pricing:      NewPricingService(nil, nil),
		billing:      newPricingTestBillingService(t, map[string]*LiteLLMModelPricing{}),
		custom:       NewCustomModelPricingService(nil),
		groupService: NewGroupService(&fakeGroupRepoForPricing{groups: groups}, nil),
		usageRepo:    usageRepo,
	}

	result, err := svc.ScanUncovered(context.Background(), 30)
	require.NoError(t, err)
	byModel := map[string]UncoveredEntry{}
	for _, item := range result.Items {
		byModel[item.Model] = item
	}
	assert.NotContains(t, byModel, "covered-by-group", "分组价覆盖的模型不应报未覆盖")
	require.Contains(t, byModel, "brand-new-model", "全局无价的模型应报未覆盖")
	assert.Contains(t, byModel["brand-new-model"].References, "裸分组")
	assert.Contains(t, byModel["brand-new-model"].References, "usage")
	assert.True(t, byModel["brand-new-model"].ZeroCostOnly, "有 token 流量但扣费为 0 应打对账标记")
}

// --- 同步状态快照 ---

func TestPricingService_SyncStatusSnapshot(t *testing.T) {
	svc := NewPricingService(nil, nil)
	svc.pricingData = map[string]*LiteLLMModelPricing{"a": {}, "b": {}}
	svc.lastUpdated = time.Now()
	svc.lastAttemptError = "boom"

	status := svc.SyncStatusSnapshot()
	assert.Equal(t, 2, status.ModelCount)
	assert.Equal(t, "boom", status.LastError)
	assert.False(t, status.Syncing)
	assert.False(t, status.SchedulerEnabled)
}
