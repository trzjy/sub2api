//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// aggregateFamilyTestAccount 构造一个带同名 model_mapping 的账号夹具，
// 使 GetMappedModel 映射后的上游模型与请求模型一致。
func aggregateFamilyTestAccount(platform, model string) *Account {
	return &Account{
		Platform: platform,
		Credentials: map[string]any{
			"model_mapping": map[string]any{model: model},
		},
	}
}

// TestAggregatePlatformFamilyAllowed 钉死聚合平台按模型家族放行的纯函数口径：
// 请求模型（优先映射后的上游模型）家族必须与分组平台一致；无模型名失败关闭。
func TestAggregatePlatformFamilyAllowed(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		req     OpenAIAccountScheduleRequest
		want    bool
	}{
		{
			name:    "devin account serves openai family group with same-name mapping",
			account: aggregateFamilyTestAccount(PlatformDevin, "gpt-5-6-sol-none"),
			req: OpenAIAccountScheduleRequest{
				Platform:       PlatformOpenAI,
				RequestedModel: "gpt-5-6-sol-none",
			},
			want: true,
		},
		{
			name:    "devin account serves anthropic family group",
			account: aggregateFamilyTestAccount(PlatformDevin, "claude-sonnet-5-low"),
			req: OpenAIAccountScheduleRequest{
				Platform:       PlatformAnthropic,
				RequestedModel: "claude-sonnet-5-low",
			},
			want: true,
		},
		{
			name:    "devin account rejected when family mismatches group platform",
			account: aggregateFamilyTestAccount(PlatformDevin, "claude-sonnet-5-low"),
			req: OpenAIAccountScheduleRequest{
				Platform:       PlatformOpenAI,
				RequestedModel: "claude-sonnet-5-low",
			},
			want: false,
		},
		{
			name:    "empty requested model fails closed",
			account: aggregateFamilyTestAccount(PlatformDevin, "gpt-5-6-sol-none"),
			req: OpenAIAccountScheduleRequest{
				Platform:       PlatformOpenAI,
				RequestedModel: "   ",
			},
			want: false,
		},
		{
			name:    "non-aggregate account is not affected",
			account: aggregateFamilyTestAccount(PlatformOpenAI, "gpt-5-6-sol-none"),
			req: OpenAIAccountScheduleRequest{
				Platform:       PlatformOpenAI,
				RequestedModel: "gpt-5-6-sol-none",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, aggregatePlatformFamilyAllowed(tt.account, tt.req))
		})
	}
}
