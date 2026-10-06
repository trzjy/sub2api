package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestOpenAIProfitControlVetoSkipsGateForImageInput 验证带图请求入口在装门时
// 跳过利润门（capability-routing-plan §3.6）：withOpenAIProfitSuppressedForImage
// 对含图 body 打上 suppressed 标记后，WithOpenAIRequestPricingContext 只固定
// pricingAt 不装门，视觉分流目标账号（高倍率）不被 veto；不含图请求利润门行为
// 与现状一致（防回归）。
func TestOpenAIProfitControlVetoSkipsGateForImageInput(t *testing.T) {
	groupID := int64(9101)
	group := &service.Group{
		ID:                   groupID,
		Platform:             service.PlatformOpenAI,
		Status:               service.StatusActive,
		Hydrated:             true,
		RateMultiplier:       1.0,
		SubscriptionType:     service.SubscriptionTypeStandard,
		ProfitControlEnabled: true,
		ProfitMinMargin:      0.5,
	}
	base := context.WithValue(context.Background(), ctxkey.Group, group)

	h := &OpenAIGatewayHandler{gatewayService: &service.OpenAIGatewayService{}}
	expensive := profitControlImageInputAccount(1, 0.8)

	// mountPricingContext 复刻 handler 四个装门点的装配顺序：先按 body 判定含图
	// （含图打 suppressed 标记），再交 WithOpenAIRequestPricingContext 装门。
	mountPricingContext := func(body []byte) context.Context {
		ctx, pricingAt := h.gatewayService.WithOpenAIRequestPricingContext(
			h.withOpenAIProfitSuppressedForImage(base, body), &groupID)
		require.False(t, pricingAt.IsZero(), "跳门时仍须固定 pricingAt 供计费共用")
		return ctx
	}

	t.Run("image input skips gate and is not vetoed", func(t *testing.T) {
		bodies := map[string][]byte{
			"chat_completions image_url": []byte(`{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`),
			"responses input_image":      []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`),
			"anthropic image":            []byte(`{"model":"claude","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}`),
		}
		for name, body := range bodies {
			vetoed, reason := service.OpenAIProfitControlVeto(mountPricingContext(body), expensive)
			require.Falsef(t, vetoed, "%s: 带图请求不得被利润门 veto（reason=%s）", name, reason)
		}
	})

	t.Run("text only keeps current gate behavior", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
		vetoed, reason := service.OpenAIProfitControlVeto(mountPricingContext(body), expensive)
		require.True(t, vetoed, "不含图请求利润门行为须与现状一致")
		require.Equal(t, "profit_threshold", reason)
	})

	t.Run("qualifying account passes gate when text only", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
		vetoed, reason := service.OpenAIProfitControlVeto(mountPricingContext(body), profitControlImageInputAccount(2, 0.3))
		require.False(t, vetoed, "合格账号在利润门内照常放行（reason=%s）", reason)
	})
}

func profitControlImageInputAccount(id int64, rate float64) *service.Account {
	return &service.Account{
		ID:             id,
		Platform:       service.PlatformOpenAI,
		Type:           service.AccountTypeAPIKey,
		Status:         service.StatusActive,
		Schedulable:    true,
		Concurrency:    2,
		RateMultiplier: &rate,
	}
}
