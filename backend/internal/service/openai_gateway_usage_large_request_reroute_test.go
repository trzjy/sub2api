//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRecordUsageLargeRequestRoutingFieldsPersist 覆盖 LRO-09：选号侧的两个对账入参
// （RoutingEstimatedInputTokens / LargeRequestReroute）必须原样落到 UsageLog 的对账
// 字段上，供事后按「只接大请求」规则核对路由与估算是否合理。
func TestRecordUsageLargeRequestRoutingFieldsPersist(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, subRepo, nil)

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "resp_lro_routing_fields",
			Usage:     OpenAIUsage{InputTokens: 1500, OutputTokens: 200},
			Model:     "gpt-5.1",
			Duration:  time.Second,
		},
		APIKey:                      &APIKey{ID: 601},
		User:                        &User{ID: 602},
		Account:                     &Account{ID: 603, Platform: PlatformOpenAI},
		RoutingEstimatedInputTokens: 98765,
		LargeRequestReroute:         true,
	})
	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)

	require.Equal(t, 98765, usageRepo.lastLog.EstimatedInputTokens,
		"选号前本地估算须原样落到 UsageLog.EstimatedInputTokens")
	require.True(t, usageRepo.lastLog.LargeRequestReroute,
		"重路由标记须原样落到 UsageLog.LargeRequestReroute")
	// 反向确认：这两个字段不是计费字段，InputTokens 仍取上游口径。
	require.Equal(t, 1500, usageRepo.lastLog.InputTokens)

	// 未携带这两个入参时落零值，不得凭空产生对账数据。
	require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "resp_lro_routing_fields_absent",
			Usage:     OpenAIUsage{InputTokens: 1500, OutputTokens: 200},
			Model:     "gpt-5.1",
			Duration:  time.Second,
		},
		APIKey:  &APIKey{ID: 601},
		User:    &User{ID: 602},
		Account: &Account{ID: 603, Platform: PlatformOpenAI},
	}))
	require.NotNil(t, usageRepo.lastLog)
	require.Zero(t, usageRepo.lastLog.EstimatedInputTokens)
	require.False(t, usageRepo.lastLog.LargeRequestReroute)
}

// TestRecordUsageLargeRequestRoutingEstimateDoesNotAffectBilling 保证 RoutingEstimatedInputTokens
// 与 EstimatedInputTokens 的语义隔离：只有后者（Cursor 反代重算）能覆盖计费 input_tokens，
// 前者即便数值更大也只进对账字段，绝不改动计费口径与扣费金额。
func TestRecordUsageLargeRequestRoutingEstimateDoesNotAffectBilling(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, subRepo, nil)

	const (
		upstreamInputTokens = 5000
		billingEstimate     = 2000
		routingEstimate     = 9999
	)

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "resp_lro_routing_isolation",
			Usage:     OpenAIUsage{InputTokens: upstreamInputTokens, OutputTokens: 300},
			Model:     "gpt-5.1",
			Duration:  time.Second,
		},
		APIKey: &APIKey{ID: 701},
		User:   &User{ID: 702},
		// Cursor APIKey 账号：EstimatedInputTokens > 0 时会覆盖计费 input_tokens。
		Account:                     &Account{ID: 703, Platform: PlatformCursor, Type: AccountTypeAPIKey},
		EstimatedInputTokens:        billingEstimate,
		RoutingEstimatedInputTokens: routingEstimate,
	})
	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)

	// 计费口径只认 EstimatedInputTokens：既没有被上游虚高的 5000 污染，也没有被
	// RoutingEstimatedInputTokens 的 9999 劫持。
	require.Equal(t, billingEstimate, usageRepo.lastLog.InputTokens,
		"计费 input_tokens 只能由 EstimatedInputTokens 覆盖")
	// 对账字段独立承载选号前估算值。
	require.Equal(t, routingEstimate, usageRepo.lastLog.EstimatedInputTokens)

	expectedCost, calcErr := svc.billingService.CalculateCost("gpt-5.1", UsageTokens{
		InputTokens:  billingEstimate,
		OutputTokens: 300,
	}, 1.1)
	require.NoError(t, calcErr)
	require.InDelta(t, expectedCost.TotalCost, usageRepo.lastLog.TotalCost, 1e-12)
	require.InDelta(t, expectedCost.ActualCost, usageRepo.lastLog.ActualCost, 1e-12)
	require.Equal(t, 1, userRepo.deductCalls)
	require.InDelta(t, expectedCost.ActualCost, userRepo.lastAmount, 1e-12)

	// 反证：若误用 RoutingEstimatedInputTokens（9999）计费，金额必然更高。
	rerouteCost, calcErr := svc.billingService.CalculateCost("gpt-5.1", UsageTokens{
		InputTokens:  routingEstimate,
		OutputTokens: 300,
	}, 1.1)
	require.NoError(t, calcErr)
	require.Greater(t, rerouteCost.ActualCost, usageRepo.lastLog.ActualCost,
		"RoutingEstimatedInputTokens 不得参与计费")
}
