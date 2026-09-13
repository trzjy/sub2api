package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// stubCustomPricingRepo 是 CustomModelPricingRepository 的最小内存实现（仅测试用）。
type stubCustomPricingRepo struct {
	entries []CustomModelPricing
}

func (r *stubCustomPricingRepo) List(_ context.Context) ([]CustomModelPricing, error) {
	return r.entries, nil
}

func (r *stubCustomPricingRepo) GetByID(_ context.Context, id int64) (*CustomModelPricing, error) {
	for i := range r.entries {
		if r.entries[i].ID == id {
			return &r.entries[i], nil
		}
	}
	return nil, nil
}

func (r *stubCustomPricingRepo) Create(_ context.Context, entry *CustomModelPricing) error {
	r.entries = append(r.entries, *entry)
	return nil
}

func (r *stubCustomPricingRepo) Update(_ context.Context, entry *CustomModelPricing) error {
	for i := range r.entries {
		if r.entries[i].ID == entry.ID {
			r.entries[i] = *entry
		}
	}
	return nil
}

func (r *stubCustomPricingRepo) Delete(_ context.Context, id int64) error {
	out := r.entries[:0]
	for _, e := range r.entries {
		if e.ID != id {
			out = append(out, e)
		}
	}
	r.entries = out
	return nil
}

// TestNewPricingAdminService_WiresCustomPricingIntoResolver 钉住 F11 回归：
// 生产装配（NewPricingAdminService 同时持有 custom 层与全局 resolver）必须把自定义
// 定价层注入 resolver，否则价格管理中心配的自定义价对网关计费完全无效
// （官方目录无该模型 → pricing not found → 成本记 0）。
func TestNewPricingAdminService_WiresCustomPricingIntoResolver(t *testing.T) {
	ctx := context.Background()

	// 官方目录里没有 hy3（Tencent 混元自有模型），只能靠自定义层定价。
	inputPrice := 1.400560224e-07  // ¥1/MTok ÷7.14
	outputPrice := 5.602240896e-07 // ¥4/MTok ÷7.14
	repo := &stubCustomPricingRepo{entries: []CustomModelPricing{{
		ID:          8,
		Models:      []string{"hy3"},
		Enabled:     true,
		BillingMode: BillingModeToken,
		InputPrice:  &inputPrice,
		OutputPrice: &outputPrice,
	}}}
	custom := NewCustomModelPricingService(repo)
	require.NoError(t, custom.refresh())

	bs := newPricingTestBillingService(t, map[string]*LiteLLMModelPricing{})
	resolver := NewModelPricingResolver(nil, bs)

	// 接线前：官方目录无 hy3 → 解析不到任何价格（这正是线上 cost=0 的成因）。
	require.Nil(t, resolver.Resolve(ctx, PricingInput{Model: "hy3"}).BasePricing)

	// 生产装配点：NewPricingAdminService 负责把 custom 层注入 resolver。
	_ = NewPricingAdminService(nil, bs, custom, resolver, nil, nil, nil)

	resolved := resolver.Resolve(ctx, PricingInput{Model: "hy3"})
	require.NotNil(t, resolved.BasePricing, "接线后 hy3 必须解析出价格")
	require.Equal(t, PricingSourceCustom, resolved.Source)
	require.InDelta(t, inputPrice, resolved.BasePricing.InputPricePerToken, 1e-18)
	require.InDelta(t, outputPrice, resolved.BasePricing.OutputPricePerToken, 1e-18)
}
