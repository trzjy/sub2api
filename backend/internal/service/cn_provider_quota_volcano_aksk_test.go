package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 回归：火山订阅用量必须走 AK/SK 管理面接口（GetAFPUsage / GetCodingPlanUsage）。
// 生产实测（2026-09-06）推理端点成功响应不携带任何 x-ratelimit-* 头，响应头探测
// 拿不到用量。以下 fixture 为生产真实密钥调用官方接口的原始响应摘录。

func TestParseVolcanoAFPUsageTiers(t *testing.T) {
	// 账号 29（火山agent，Medium 档）真实响应摘录。
	body := []byte(`{"ResponseMetadata":{"Action":"GetAFPUsage","Version":"2024-01-01"},"Result":{"PlanType":"medium",` +
		`"AFPFiveHour":{"Quota":10000,"Used":0.1879,"SubscribeTime":1788701412000,"ResetTime":1788719412000},` +
		`"AFPWeekly":{"Quota":35000,"Used":34979.4955,"SubscribeTime":1788105600000,"ResetTime":1788710400000},` +
		`"AFPMonthly":{"Quota":100000,"Used":34979.4955,"SubscribeTime":1788465386000,"ResetTime":1791129599000},` +
		`"AFPDaily":{"Quota":50000,"Used":0,"SubscribeTime":1788624000000,"ResetTime":1788710400000}}}`)

	tiers := parseVolcanoAFPUsageTiers(body)
	require.Len(t, tiers, 3)

	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 0.1879/10000*100, tiers[0].UsedPercent, 1e-9)
	require.Equal(t, "2026-09-06T18:30:12Z", tiers[0].ResetAt) // 锚点+5h（官方滚动窗口规则实测验证）
	require.False(t, tiers[0].UsedPercentUnknown)

	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 34979.4955/35000*100, tiers[1].UsedPercent, 1e-9)

	require.Equal(t, "monthly", tiers[2].Window)
	require.InDelta(t, 34.9794955, tiers[2].UsedPercent, 1e-6)
}

func TestParseVolcanoCodingUsageTiers(t *testing.T) {
	// 账号 30（火山coding）真实响应摘录。
	body := []byte(`{"ResponseMetadata":{"Action":"GetCodingPlanUsage","Version":"2024-01-01"},"Result":{"Status":"Running","UpdateTimestamp":1788706144,` +
		`"QuotaUsage":[{"Level":"session","Percent":0.003069,"ResetTimestamp":1788713292,"Cap":100,"RewardTotalPercent":0},` +
		`{"Level":"weekly","Percent":32.0221312,"ResetTimestamp":1788710400,"Cap":100,"RewardTotalPercent":0},` +
		`{"Level":"monthly","Percent":16.0110656,"ResetTimestamp":1791129599,"Cap":100,"RewardTotalPercent":0}],"HasReward":false}}`)

	tiers := parseVolcanoCodingUsageTiers(body)
	require.Len(t, tiers, 3)

	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 0.003069, tiers[0].UsedPercent, 1e-9)
	require.Equal(t, "2026-09-06T16:48:12Z", tiers[0].ResetAt)

	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 32.0221312, tiers[1].UsedPercent, 1e-9)

	require.Equal(t, "monthly", tiers[2].Window)
	require.InDelta(t, 16.0110656, tiers[2].UsedPercent, 1e-9)
}

func TestVolcanoUsageActionByBaseURL(t *testing.T) {
	require.Equal(t, "GetAFPUsage", volcanoUsageAction("https://ark.cn-beijing.volces.com/api/plan"))
	require.Equal(t, "GetCodingPlanUsage", volcanoUsageAction("https://ark.cn-beijing.volces.com/api/coding"))
	require.Empty(t, volcanoUsageAction("https://ark.cn-beijing.volces.com/api/other"))
}

func TestNormalizeVolcanoPlanBaseURL(t *testing.T) {
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/plan", normalizeVolcanoPlanBaseURL("https://ark.cn-beijing.volces.com/api/plan/v3"))
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/plan", normalizeVolcanoPlanBaseURL("https://ark.cn-beijing.volces.com/api/plan/"))
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/coding", normalizeVolcanoPlanBaseURL("https://ark.cn-beijing.volces.com/api/coding/v3/"))
}
