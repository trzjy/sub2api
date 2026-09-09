//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// TestCNExtraKey 验证 provider 维度的 Extra 快照键由前缀 + 后缀拼接。
func TestCNExtraKey(t *testing.T) {
	t.Parallel()
	require.Equal(t, "kimi_5h_used_percent", cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed))
	require.Equal(t, "zhipu_weekly_reset_at", cnExtraKey(PlatformZhipu, cnExtraSuffixWeeklyReset))
	require.Equal(t, "deepseek_balance", cnExtraKey(PlatformDeepseek, cnBalanceExtraSuffixBalance))
}

// TestCNParseF64 兼容 JSON 数值与字符串（cc-switch 与上游字段类型不一致）。
func TestCNParseF64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  any
		want float64
		ok   bool
	}{
		{"float64", float64(12.5), 12.5, true},
		{"int", 100, 100, true},
		{"numeric string", "33.3", 33.3, true},
		{"trim string", "  7  ", 7, true},
		{"non-numeric string", "abc", 0, false},
		{"nil", nil, 0, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := cnParseF64(tc.raw)
			require.Equal(t, tc.ok, ok)
			if ok {
				require.InDelta(t, tc.want, got, 1e-9)
			}
		})
	}
}

// TestCNMillisToRFC3339 秒级（<1e12）按秒、毫秒级按毫秒处理；非正返回空串。
func TestCNMillisToRFC3339(t *testing.T) {
	t.Parallel()
	// 1700000000 秒 = 1700000000000 毫秒
	want := time.UnixMilli(1700000000000).UTC().Format(time.RFC3339)
	require.Equal(t, want, cnMillisToRFC3339(1700000000))    // 秒级
	require.Equal(t, want, cnMillisToRFC3339(1700000000000)) // 毫秒级
	require.Equal(t, "", cnMillisToRFC3339(0))               // 非正
	require.Equal(t, "", cnMillisToRFC3339(-1))
}

// TestCnnormalizeResetTime 覆盖 ISO8601 字符串 / 数字（秒、毫秒）/ 非法输入。
func TestCnnormalizeResetTime(t *testing.T) {
	t.Parallel()
	// ISO8601 字符串归一化为 RFC3339（UTC）。
	require.Equal(t, "2026-08-14T10:00:00Z", cnNormalizeResetTime("2026-08-14T10:00:00Z"))
	// 毫秒级 float64。
	require.Equal(t,
		time.UnixMilli(1700000000000).UTC().Format(time.RFC3339),
		cnNormalizeResetTime(float64(1700000000000)))
	// 非法字符串。
	require.Equal(t, "", cnNormalizeResetTime("not-a-time"))
	require.Equal(t, "", cnNormalizeResetTime(""))
}

// TestParseKimiUsageTiers 验证 Kimi For Coding /usages 解析：
//   - 首个 limits[].detail → 5h 桶，utilization=(limit-remaining)/limit*100
//   - usage → weekly 桶
//   - 仅取首个 detail（多个 detail 时不应重复产出 5h）
func TestParseKimiUsageTiers(t *testing.T) {
	t.Parallel()
	body := []byte(`{
		"limits": [
			{"name": "5h", "detail": {"limit": 1000, "remaining": 600, "resetTime": "2026-08-14T15:00:00Z"}},
			{"name": "ignored-second-detail", "detail": {"limit": 999, "remaining": 0, "resetTime": "2026-08-14T20:00:00Z"}}
		],
		"usage": {"limit": 10000, "remaining": 4000, "resetTime": "2026-08-18T00:00:00Z"}
	}`)
	tiers := parseKimiUsageTiers(body)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 40.0, tiers[0].UsedPercent, 1e-9) // (1000-600)/1000*100
	require.Equal(t, "2026-08-14T15:00:00Z", tiers[0].ResetAt)
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 60.0, tiers[1].UsedPercent, 1e-9) // (10000-4000)/10000*100
	require.Equal(t, "2026-08-18T00:00:00Z", tiers[1].ResetAt)
}

// TestParseKimiUsageTiers_LimitZero 不应除零：limit=0 → utilization=0。
func TestParseKimiUsageTiers_LimitZero(t *testing.T) {
	t.Parallel()
	body := []byte(`{"limits":[{"detail":{"limit":0,"remaining":0,"resetTime":"2026-08-14T15:00:00Z"}}]}`)
	tiers := parseKimiUsageTiers(body)
	require.Len(t, tiers, 1)
	require.InDelta(t, 0.0, tiers[0].UsedPercent, 1e-9)
}

// TestParseZhipuTokenTiers_UnitClassification 显式 unit（3=5h / 6=weekly）优先分类，
// 不能被 reset 时间排序覆盖（周期末尾周窗口会更早重置）。
func TestParseZhipuTokenTiers_UnitClassification(t *testing.T) {
	t.Parallel()
	// weekly 的 nextResetTime 早于 5h（模拟周期末尾），但 unit 必须胜出。
	data := gjson.Parse(`{
		"limits": [
			{"type":"TOKENS_LIMIT","unit":6,"percentage":70,"nextResetTime":1700000000000},
			{"type":"TOKENS_LIMIT","unit":3,"percentage":20,"nextResetTime":1700000099999}
		]
	}`)
	tiers := parseZhipuTokenTiers(data)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 20.0, tiers[0].UsedPercent, 1e-9)
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 70.0, tiers[1].UsedPercent, 1e-9)
}

// TestParseZhipuTokenTiers_SingleTierOldPlan 老套餐仅回 1 条 → 降级为仅 5h。
func TestParseZhipuTokenTiers_SingleTierOldPlan(t *testing.T) {
	t.Parallel()
	data := gjson.Parse(`{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":15,"nextResetTime":1700000000000}]}`)
	tiers := parseZhipuTokenTiers(data)
	require.Len(t, tiers, 1)
	require.Equal(t, "5h", tiers[0].Window)
}

// TestParseZhipuTokenTiers_FallbackHeuristic unit 缺失时：无 reset 的条目优先归 5h，
// 其余按 reset 升序填入剩余槽位。
func TestParseZhipuTokenTiers_FallbackHeuristic(t *testing.T) {
	t.Parallel()
	// 无 unit：A 无 reset、B 有 reset。A 先填 5h，B 填 weekly。
	data := gjson.Parse(`{
		"limits": [
			{"type":"TOKENS_LIMIT","percentage":50,"nextResetTime":1700000000000},
			{"type":"TOKENS_LIMIT","percentage":10}
		]
	}`)
	tiers := parseZhipuTokenTiers(data)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 10.0, tiers[0].UsedPercent, 1e-9) // 无 reset 优先 5h
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 50.0, tiers[1].UsedPercent, 1e-9)
}

// TestParseZhipuTokenTiers_IgnoresNonTokenEntries 非 TOKENS_LIMIT/CREDIT_LIMIT 条目跳过。
func TestParseZhipuTokenTiers_IgnoresNonTokenEntries(t *testing.T) {
	t.Parallel()
	data := gjson.Parse(`{"limits":[{"type":"OTHER_LIMIT","unit":3,"percentage":99}]}`)
	require.Empty(t, parseZhipuTokenTiers(data))
}

// TestCNQuotaExtraUpdates 验证 tier 列表落 Extra 快照键的 provider 前缀与窗口映射。
func TestCNQuotaExtraUpdates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	tiers := []CNQuotaTier{
		{Window: "5h", UsedPercent: 40, ResetAt: "2026-08-14T15:00:00Z"},
		{Window: "weekly", UsedPercent: 60, ResetAt: "2026-08-18T00:00:00Z"},
		{Window: "monthly", UsedPercent: 25, ResetAt: "2026-09-14T15:59:59Z"},
	}
	updates := cnQuotaExtraUpdates(PlatformKimi, tiers, now)
	require.Equal(t, 40.0, updates["kimi_5h_used_percent"])
	require.Equal(t, "2026-08-14T15:00:00Z", updates["kimi_5h_reset_at"])
	require.Equal(t, 60.0, updates["kimi_weekly_used_percent"])
	require.Equal(t, "2026-08-18T00:00:00Z", updates["kimi_weekly_reset_at"])
	require.Equal(t, 25.0, updates["kimi_monthly_used_percent"])
	require.Equal(t, "2026-09-14T15:59:59Z", updates["kimi_monthly_reset_at"])
	require.Equal(t, now.Format(time.RFC3339), updates["kimi_usage_updated_at"])
}

// 外审 must_fix #2：上游 reset 从有效变为空时，必须清除 DB 中旧 reset 值，
// 否则前端继续显示过期倒计时、429 冷却仍按旧时间停调。
func TestCNQuotaExtraUpdates_ClearsStaleResetWhenResetEmpty(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	// 5h 档 reset 变空（上游返回 -1/缺失），weekly 档正常。
	tiers := []CNQuotaTier{
		{Window: "5h", UsedPercent: 45, ResetAt: ""},
		{Window: "weekly", UsedPercent: 60, ResetAt: "2026-08-18T00:00:00Z"},
	}
	updates := cnQuotaExtraUpdates(providerVolcano, tiers, now)
	require.Equal(t, 45.0, updates["volcano_5h_used_percent"])
	// 关键：5h reset 必须被写 nil（cleared），而不是保留旧值。
	require.Nil(t, updates["volcano_5h_reset_at"], "5h reset 空时必须以 nil 清除，不得残留")
	require.Equal(t, "2026-08-18T00:00:00Z", updates["volcano_weekly_reset_at"])
}

// 外审 must_fix #2 变体：上游某档整档缺失（只返回 5h）时，weekly 档 used + reset
// 都必须被清除，避免上一轮 valid 的 weekly 值在 DB 中 stale 残留。
func TestCNQuotaExtraUpdates_ClearsMissingWindowKeys(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	tiers := []CNQuotaTier{
		{Window: "5h", UsedPercent: 45, ResetAt: "2026-08-14T15:00:00Z"},
		// weekly 档缺失。
	}
	updates := cnQuotaExtraUpdates(providerVolcano, tiers, now)
	require.Equal(t, 45.0, updates["volcano_5h_used_percent"])
	require.Equal(t, "2026-08-14T15:00:00Z", updates["volcano_5h_reset_at"])
	require.Nil(t, updates["volcano_weekly_used_percent"], "缺档 weekly used 必须以 nil 清除残留")
	require.Nil(t, updates["volcano_weekly_reset_at"], "缺档 weekly reset 必须以 nil 清除残留")
}

// 外审 must_fix #2 变体：上游某档整档缺失（只返回 5h/weekly）时，monthly 档 used + reset
// 都必须被清除，避免上一轮 valid 的 monthly 值在 DB 中 stale 残留。
func TestCNQuotaExtraUpdates_ClearsMissingMonthlyKeys(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	tiers := []CNQuotaTier{
		{Window: "5h", UsedPercent: 45, ResetAt: "2026-08-14T15:00:00Z"},
		{Window: "weekly", UsedPercent: 60, ResetAt: "2026-08-18T00:00:00Z"},
		// monthly 档缺失。
	}
	updates := cnQuotaExtraUpdates(providerVolcano, tiers, now)
	require.Nil(t, updates["volcano_monthly_used_percent"], "缺档 monthly used 必须以 nil 清除残留")
	require.Nil(t, updates["volcano_monthly_reset_at"], "缺档 monthly reset 必须以 nil 清除残留")
}

// TestVolcanoNextMonthlyReset 验证月重置为下个月同一天 23:59:59（Asia/Shanghai），
// 且日期溢出时回落到目标月最后一天。
func TestVolcanoNextMonthlyReset(t *testing.T) {
	t.Parallel()
	loc := volcanoPlanLoc

	// 普通日期：8 月 14 日 -> 9 月 14 日 23:59:59。
	fixed := time.Date(2026, 8, 14, 12, 0, 0, 0, loc)
	got, err := time.Parse(time.RFC3339, volcanoNextMonthlyResetAt(fixed))
	require.NoError(t, err)
	want := time.Date(2026, 9, 14, 23, 59, 59, 0, loc)
	require.True(t, got.Equal(want), "got %v want %v", got, want)

	// 月末溢出：1 月 31 日 -> 2 月 28 日 23:59:59。
	leap := time.Date(2026, 1, 31, 12, 0, 0, 0, loc)
	got, err = time.Parse(time.RFC3339, volcanoNextMonthlyResetAt(leap))
	require.NoError(t, err)
	want = time.Date(2026, 2, 28, 23, 59, 59, 0, loc)
	require.True(t, got.Equal(want), "got %v want %v", got, want)
}

// TestCNProviderResponseIndicatesInsufficientBalance 覆盖中英文余额不足文案与否定用例。
func TestCNProviderResponseIndicatesInsufficientBalance(t *testing.T) {
	t.Parallel()
	positive := []string{
		`{"error":{"message":"余额不足"}}`,
		`{"error":{"message":"Insufficient balance"}}`,
		`{"code":"insufficient_credit"}`,
		`"balance is not enough"`,
		`"no enough balance"`,
	}
	for _, body := range positive {
		require.True(t, cnProviderResponseIndicatesInsufficientBalance([]byte(body)), body)
	}
	negative := []string{
		`{"error":{"message":"rate limit exceeded"}}`,
		`{"error":{"message":"quota exhausted"}}`,
		``,
	}
	for _, body := range negative {
		require.False(t, cnProviderResponseIndicatesInsufficientBalance([]byte(body)), body)
	}
}

// TestCNBalanceLowReason 验证稳定前缀（供周期检测任务识别并清除）。
func TestCNBalanceLowReason(t *testing.T) {
	t.Parallel()
	require.Equal(t, "cn_balance_low: upstream said x",
		cnBalanceLowReason("upstream said x"))
	require.Equal(t, "cn_balance_low: 余额不足，账号临时停调",
		cnBalanceLowReason("   "))
	require.True(t, len(cnBalanceLowReason("")) > len(cnBalanceLowReasonPrefix))
}

// TestZhipuQuotaHost 按域名路由智谱额度端点主机（bigmodel.cn / z.ai / 默认国内站）。
func TestZhipuQuotaHost(t *testing.T) {
	t.Parallel()
	require.Equal(t, "https://open.bigmodel.cn", zhipuQuotaHost("https://open.bigmodel.cn/api/paas/v4"))
	require.Equal(t, "https://api.z.ai", zhipuQuotaHost("https://api.z.ai/api/paas/v4"))
	require.Equal(t, "https://open.bigmodel.cn", zhipuQuotaHost("https://custom.example.com")) // 默认国内站
	require.Equal(t, "https://open.bigmodel.cn/api/monitor/usage/quota/limit", zhipuQuotaURL("https://open.bigmodel.cn/api/paas/v4"))
}

// TestKimiQuotaURL 两种协议默认 base（coding/v1 与 coding）都归一到 /coding/v1/usages
// （cc-switch 固定端点；无 /v1 的 /coding/usages 实测 404）。
func TestKimiQuotaURL(t *testing.T) {
	t.Parallel()
	require.Equal(t, "https://api.kimi.com/coding/v1/usages", kimiQuotaURL("https://api.kimi.com/coding/v1"))
	require.Equal(t, "https://api.kimi.com/coding/v1/usages", kimiQuotaURL("https://api.kimi.com/coding"))
	require.Equal(t, "https://api.kimi.com/coding/v1/usages", kimiQuotaURL("https://api.kimi.com/coding/"))
	require.Equal(t, "https://api.kimi.com/coding/v1/usages", kimiQuotaURL("https://api.kimi.com/coding/v1/"))
}

func TestMiniMaxQuotaURL(t *testing.T) {
	t.Parallel()
	require.Equal(t, "https://api.minimax.io/v1/api/openplatform/coding_plan/remains",
		minimaxQuotaURL("https://api.minimax.io/v1"))
	require.Equal(t, "https://api.minimax.io/v1/api/openplatform/coding_plan/remains",
		minimaxQuotaURL("https://api.minimax.io/anthropic"))
	require.Equal(t, "https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains",
		minimaxQuotaURL("https://api.minimaxi.com/v1"))
	require.Equal(t, "https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains",
		minimaxQuotaURL("https://api.minimax.com/v1"))
	require.Equal(t, "https://api.minimaxi.com/v1/api/openplatform/coding_plan/remains",
		minimaxQuotaURL("https://custom.example.com"))
}

func TestParseMiniMaxUsageTiers(t *testing.T) {
	t.Parallel()
	endMs := int64(1_700_000_000_000)
	weeklyEndMs := endMs + 7*24*60*60*1000
	body := []byte(`{
		"base_resp": {"status_code": 0},
		"current_subscribe_title": "Max",
		"model_remains": [
			{"model_name": "video", "current_interval_remaining_percent": 10},
			{
				"model_name": "general",
				"current_interval_remaining_percent": 25,
				"end_time": 1700000000000,
				"current_weekly_status": 1,
				"current_weekly_remaining_percent": 40,
				"weekly_end_time": 1700604800000
			}
		]
	}`)
	tiers := parseMiniMaxUsageTiers(body)
	require.Len(t, tiers, 2)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 75, tiers[0].UsedPercent, 1e-9)
	require.Equal(t, time.UnixMilli(endMs).UTC().Format(time.RFC3339), tiers[0].ResetAt)
	require.Equal(t, "weekly", tiers[1].Window)
	require.InDelta(t, 60, tiers[1].UsedPercent, 1e-9)
	require.Equal(t, time.UnixMilli(weeklyEndMs).UTC().Format(time.RFC3339), tiers[1].ResetAt)

	noWeekly := parseMiniMaxUsageTiers([]byte(`{
		"model_remains": [{
			"model_name": "general",
			"current_interval_remaining_percent": 80,
			"end_time": 1700000000000,
			"current_weekly_status": 3,
			"current_weekly_remaining_percent": 10
		}]
	}`))
	require.Len(t, noWeekly, 1)
	require.Equal(t, "5h", noWeekly[0].Window)
	require.InDelta(t, 20, noWeekly[0].UsedPercent, 1e-9)

	require.Nil(t, parseMiniMaxUsageTiers([]byte(`{"model_remains":[{"model_name":"video","current_interval_remaining_percent":5}]}`)))
}

func TestGetCodingPlanProvider_MiniMax(t *testing.T) {
	t.Parallel()
	coding := &Account{Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"account_mode": AccountModeCoding,
		"base_url":     "https://api.minimaxi.com/v1",
	}}
	require.Equal(t, PlatformMiniMax, coding.GetCodingPlanProvider())
	intl := &Account{Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"account_mode": AccountModeCoding,
		"base_url":     "https://api.minimax.io/anthropic",
	}}
	require.Equal(t, PlatformMiniMax, intl.GetCodingPlanProvider())
	// 未配 base_url 时走国内站默认域名，仍能识别官方额度端点。
	require.Equal(t, PlatformMiniMax, (&Account{Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"account_mode": AccountModeCoding,
	}}).GetCodingPlanProvider())
	require.Empty(t, (&Account{Platform: PlatformMiniMax, Credentials: map[string]any{"account_mode": AccountModePayG}}).GetCodingPlanProvider())
	// 自定义中转不得把第三方 Key 发往官方额度端点。
	require.Empty(t, (&Account{Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"account_mode": AccountModeCoding,
		"base_url":     "https://relay.example.com/v1",
	}}).GetCodingPlanProvider())
}

// TestCNBalanceURL Kimi 固定端点；DeepSeek 基于 base_url 拼接。
func TestCNBalanceURL(t *testing.T) {
	t.Parallel()
	kimi := &Account{Platform: PlatformKimi}
	require.Equal(t, "https://api.moonshot.cn/v1/users/me/balance", cnBalanceURL(kimi))

	deepseek := &Account{
		Platform:    PlatformDeepseek,
		Credentials: map[string]any{"base_url": "https://api.deepseek.com"},
	}
	require.Equal(t, "https://api.deepseek.com/user/balance", cnBalanceURL(deepseek))
}

// TestCNProviderThresholdCandidates 从 Extra 快照读取 5h / weekly 候选。
func TestCNProviderThresholdCandidates(t *testing.T) {
	t.Parallel()
	account := &Account{
		Platform: PlatformKimi,
		Extra: map[string]any{
			"kimi_5h_used_percent":     90.0,
			"kimi_5h_reset_at":         "2026-08-14T15:00:00Z",
			"kimi_weekly_used_percent": 50.0,
			"kimi_weekly_reset_at":     "2026-08-18T00:00:00Z",
		},
	}
	cands := cnProviderThresholdCandidates(account, PlatformKimi)
	// 仅返回非 nil 候选（两窗口均存在 → 2 条）。
	var present []*accountSchedulingThresholdCandidate
	for _, c := range cands {
		if c != nil {
			present = append(present, c)
		}
	}
	require.Len(t, present, 2)

	// 缺少 used 键的窗口不产生候选。
	partial := &Account{
		Platform: PlatformKimi,
		Extra:    map[string]any{"kimi_5h_reset_at": "2026-08-14T15:00:00Z"}, // 无 used_percent
	}
	require.Empty(t, filterNil(cnProviderThresholdCandidates(partial, PlatformKimi)))

	// 空 Extra / nil account 安全返回。
	require.Empty(t, filterNil(cnProviderThresholdCandidates(&Account{Platform: PlatformKimi}, PlatformKimi)))
}

func filterNil(cands []*accountSchedulingThresholdCandidate) []*accountSchedulingThresholdCandidate {
	var out []*accountSchedulingThresholdCandidate
	for _, c := range cands {
		if c != nil {
			out = append(out, c)
		}
	}
	return out
}

// TestEvaluateAccountSchedulingThreshold_KimiCodingPlan 集成验证：kimi coding 账号
// 5h 用量超阈值且窗口未重置 → 主动停调至 5h 重置点。
func TestEvaluateAccountSchedulingThreshold_KimiCodingPlan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	account := &Account{
		Platform: PlatformKimi,
		Extra: map[string]any{
			"kimi_5h_used_percent":     90.0,
			"kimi_5h_reset_at":         reset.Format(time.RFC3339),
			"kimi_weekly_used_percent": 30.0,
			"kimi_weekly_reset_at":     now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
		},
	}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformKimi: 80}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, PlatformKimi, decision.Platform)
	require.Equal(t, "5h", decision.Window)
	require.InDelta(t, 90.0, decision.UsedPercent, 1e-9)
	require.NotNil(t, decision.Until)
	require.True(t, reset.Equal(*decision.Until))
}

func TestEvaluateAccountSchedulingThreshold_MiniMaxCodingPlan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	reset := now.Add(3 * time.Hour)
	account := &Account{
		Platform: PlatformMiniMax,
		Extra: map[string]any{
			"minimax_5h_used_percent":     90.0,
			"minimax_5h_reset_at":         reset.Format(time.RFC3339),
			"minimax_weekly_used_percent": 30.0,
			"minimax_weekly_reset_at":     now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
		},
	}
	decision := EvaluateAccountSchedulingThreshold(account, map[string]int{PlatformMiniMax: 80}, now)
	require.True(t, decision.ShouldPause)
	require.Equal(t, PlatformMiniMax, decision.Platform)
	require.Equal(t, "5h", decision.Window)
	require.InDelta(t, 90.0, decision.UsedPercent, 1e-9)
	require.NotNil(t, decision.Until)
	require.True(t, reset.Equal(*decision.Until))
}

// TestEvaluateAccountSchedulingThreshold_CNWindowResetSkipped 窗口已重置（reset<=now）
// 或用量低于阈值 → 不停调（candidateMatchesThreshold 要求 until.After(now)）。
func TestEvaluateAccountSchedulingThreshold_CNWindowResetSkipped(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	// 重置时间已过。
	expired := &Account{
		Platform: PlatformZhipu,
		Extra: map[string]any{
			"zhipu_5h_used_percent": 99.0,
			"zhipu_5h_reset_at":     now.Add(-1 * time.Hour).Format(time.RFC3339),
		},
	}
	require.False(t, EvaluateAccountSchedulingThreshold(expired, map[string]int{PlatformZhipu: 80}, now).ShouldPause)

	// 用量低于阈值。
	low := &Account{
		Platform: PlatformZhipu,
		Extra: map[string]any{
			"zhipu_5h_used_percent": 20.0,
			"zhipu_5h_reset_at":     now.Add(3 * time.Hour).Format(time.RFC3339),
		},
	}
	require.False(t, EvaluateAccountSchedulingThreshold(low, map[string]int{PlatformZhipu: 80}, now).ShouldPause)
}

// TestCNProviderQuotaSnapshotReset Coding Plan 429 冷却：取快照中最早的「仍在未来」窗口重置点。
func TestCNProviderQuotaSnapshotReset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	future5h := now.Add(2 * time.Hour)
	futureWeekly := now.Add(3 * 24 * time.Hour)
	pastWeekly := now.Add(-24 * time.Hour)

	// 5h 在未来、weekly 已过期 → 返回 5h。
	account := &Account{
		Platform:    PlatformKimi,
		Credentials: map[string]any{"account_mode": AccountModeCoding},
		Extra: map[string]any{
			"kimi_5h_reset_at":     future5h.Format(time.RFC3339),
			"kimi_weekly_reset_at": pastWeekly.Format(time.RFC3339),
		},
	}
	got := cnProviderQuotaSnapshotReset(account, now)
	require.NotNil(t, got)
	require.True(t, future5h.Equal(*got))

	// 两窗口均在未来 → 取较早者（429 多由 5h 窗口触发，避免冷却到 weekly 重置）。
	both := &Account{
		Platform:    PlatformKimi,
		Credentials: map[string]any{"account_mode": AccountModeCoding},
		Extra: map[string]any{
			"kimi_5h_reset_at":     future5h.Format(time.RFC3339),
			"kimi_weekly_reset_at": futureWeekly.Format(time.RFC3339),
		},
	}
	gotBoth := cnProviderQuotaSnapshotReset(both, now)
	require.NotNil(t, gotBoth)
	require.True(t, future5h.Equal(*gotBoth))

	// 两窗口均过期 → nil。
	expired := &Account{
		Platform:    PlatformKimi,
		Credentials: map[string]any{"account_mode": AccountModeCoding},
		Extra: map[string]any{
			"kimi_5h_reset_at":     pastWeekly.Format(time.RFC3339),
			"kimi_weekly_reset_at": pastWeekly.Format(time.RFC3339),
		},
	}
	require.Nil(t, cnProviderQuotaSnapshotReset(expired, now))

	// payg 账号（非 coding）→ nil（余额型走余额检测）。
	payg := &Account{
		Platform:    PlatformKimi,
		Credentials: map[string]any{"account_mode": AccountModePayG},
		Extra:       map[string]any{"kimi_5h_reset_at": future5h.Format(time.RFC3339)},
	}
	require.Nil(t, cnProviderQuotaSnapshotReset(payg, now))

	// 外审 must_fix #1：payg 火山订阅号（platform=deepseek，base_url 指向 volces）
	// 必须能读到 volcano_* 重置点，供 429 冷却使用真实 5h 窗口。旧实现按 coding
	// 门控（GetCodingPlanProvider）会返回 nil，导致冷却链路断开。
	volcanoPayG := &Account{
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"account_mode": AccountModePayG,
			"base_url":     "https://ark.cn-beijing.volces.com/api/plan",
		},
		Extra: map[string]any{
			"volcano_5h_reset_at": future5h.Format(time.RFC3339),
		},
	}
	gotVolcano := cnProviderQuotaSnapshotReset(volcanoPayG, now)
	require.NotNil(t, gotVolcano, "payg 火山账号 429 冷却必须读到 volcano_5h 重置点")
	require.True(t, future5h.Equal(*gotVolcano))
}

// TestNormalizeOpenAICompatiblePlatform_SchedulerExactMatch 回归保护：
// grok 与国产供应商原样保留，其余归一为 openai —— 保证 kimi/zhipu/deepseek 分组请求
// 精确匹配同名账号（与 openai/grok 当前行为一致），不会错误并入 openai 池。
func TestNormalizeOpenAICompatiblePlatform_SchedulerExactMatch(t *testing.T) {
	t.Parallel()
	require.Equal(t, PlatformGrok, NormalizeOpenAICompatiblePlatform(PlatformGrok))
	require.Equal(t, PlatformKimi, NormalizeOpenAICompatiblePlatform(PlatformKimi))
	require.Equal(t, PlatformZhipu, NormalizeOpenAICompatiblePlatform(PlatformZhipu))
	require.Equal(t, PlatformDeepseek, NormalizeOpenAICompatiblePlatform(PlatformDeepseek))
	// 其他平台（含空、anthropic、未知）一律归一为 openai。
	require.Equal(t, PlatformOpenAI, NormalizeOpenAICompatiblePlatform(""))
	require.Equal(t, PlatformOpenAI, NormalizeOpenAICompatiblePlatform(PlatformAnthropic))
	require.Equal(t, PlatformOpenAI, NormalizeOpenAICompatiblePlatform("something-else"))
}

// TestGetOpenAIProtocolAPIKey_CNProviders 验证 OpenAI 协议族密钥读取覆盖国产供应商，
// 同时保持 IsOpenAIApiKey 的 openai-only 语义（调度倍率/WS 门控不受影响）。
func TestGetOpenAIProtocolAPIKey_CNProviders(t *testing.T) {
	t.Parallel()

	kimi := &Account{
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-kimi"},
	}
	require.Equal(t, "sk-kimi", kimi.GetOpenAIProtocolAPIKey())
	require.False(t, kimi.IsOpenAIApiKey(), "IsOpenAIApiKey stays openai-only for scheduling gates")

	// 非 APIKey 类型的 CN 账号不返回密钥
	notAPIKey := &Account{
		Platform:    PlatformDeepseek,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"api_key": "sk-leak"},
	}
	require.Equal(t, "", notAPIKey.GetOpenAIProtocolAPIKey())

	// openai 原生账号行为不变
	openai := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-openai"},
	}
	require.Equal(t, "sk-openai", openai.GetOpenAIProtocolAPIKey())
}

// TestBuildUpstreamModelsRequest_CNProviders 验证“同步上游支持的模型”对国产供应商可用：
// 密钥经 GetOpenAIProtocolAPIKey 读取，/models 端点拼接到账号 base_url（含默认值）。
func TestBuildUpstreamModelsRequest_CNProviders(t *testing.T) {
	t.Parallel()

	svc := &AccountTestService{cfg: &config.Config{}}
	cases := []struct {
		name     string
		platform string
		mode     string
		wantURL  string
	}{
		{"kimi default", PlatformKimi, "", "https://api.moonshot.cn/v1/models"},
		{"kimi coding", PlatformKimi, AccountModeCoding, "https://api.kimi.com/coding/v1/models"},
		{"zhipu default", PlatformZhipu, "", "https://open.bigmodel.cn/api/paas/v4/models"},
		{"zhipu coding", PlatformZhipu, AccountModeCoding, "https://open.bigmodel.cn/api/coding/paas/v4/models"},
		{"deepseek", PlatformDeepseek, "", "https://api.deepseek.com/v1/models"},
		{"minimax default", PlatformMiniMax, "", "https://api.minimaxi.com/v1/models"},
		{"minimax coding", PlatformMiniMax, AccountModeCoding, "https://api.minimaxi.com/v1/models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			creds := map[string]any{"api_key": "sk-test"}
			if tc.mode != "" {
				creds["account_mode"] = tc.mode
			}
			account := &Account{ID: 1, Platform: tc.platform, Type: AccountTypeAPIKey, Credentials: creds}
			req, err := svc.buildUpstreamModelsRequest(context.Background(), account)
			require.NoError(t, err)
			require.Equal(t, tc.wantURL, req.URL.String())
			require.Equal(t, "Bearer sk-test", req.Header.Get("Authorization"))
		})
	}
}

// TestGetAPIProtocol 验证协议凭证维度的平台校验矩阵：
// responses 仅 deepseek / kimi；缺失/非法值回退 chat_completions（与旧行为一致）。
func TestGetAPIProtocol(t *testing.T) {
	t.Parallel()

	mk := func(platform, protocol string) *Account {
		creds := map[string]any{"api_key": "sk-test"}
		if protocol != "" {
			creds["api_protocol"] = protocol
		}
		return &Account{Platform: platform, Type: AccountTypeAPIKey, Credentials: creds}
	}

	require.Equal(t, APIProtocolChatCompletions, mk(PlatformKimi, "").GetAPIProtocol(), "缺失回退默认")
	require.Equal(t, APIProtocolAnthropic, mk(PlatformZhipu, APIProtocolAnthropic).GetAPIProtocol())
	require.Equal(t, APIProtocolAnthropic, mk(PlatformKimi, APIProtocolAnthropic).GetAPIProtocol())
	require.Equal(t, APIProtocolAnthropic, mk(PlatformDeepseek, APIProtocolAnthropic).GetAPIProtocol())
	require.Equal(t, APIProtocolResponses, mk(PlatformDeepseek, APIProtocolResponses).GetAPIProtocol())
	require.Equal(t, APIProtocolResponses, mk(PlatformKimi, APIProtocolResponses).GetAPIProtocol())
	require.Equal(t, APIProtocolResponses, mk(PlatformMiniMax, APIProtocolResponses).GetAPIProtocol())
	require.Equal(t, APIProtocolAdaptive, mk(PlatformKimi, APIProtocolAdaptive).GetAPIProtocol())
	require.Equal(t, APIProtocolAdaptive, mk(PlatformMiniMax, APIProtocolAdaptive).GetAPIProtocol())
	require.Equal(t, APIProtocolAdaptive, mk(PlatformZhipu, APIProtocolAdaptive).GetAPIProtocol())
	require.Equal(t, APIProtocolAdaptive, mk(PlatformDeepseek, APIProtocolAdaptive).GetAPIProtocol())
	require.Equal(t, APIProtocolChatCompletions, mk(PlatformZhipu, APIProtocolResponses).GetAPIProtocol(), "zhipu 无 responses 端点")
	require.Equal(t, APIProtocolChatCompletions, mk(PlatformKimi, "bogus").GetAPIProtocol(), "非法值回退默认")
	require.Equal(t, APIProtocolChatCompletions, (&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}).GetAPIProtocol(), "openai 恒为默认（本次仅 other 开放协议覆盖）")

	// other 双协议：anthropic / chat_completions 生效；responses / adaptive 不支持（
	// 无原生 responses 端点、无厂商默认端点），缺失/非法回退 chat_completions。
	require.Equal(t, APIProtocolAnthropic, mk(PlatformOther, APIProtocolAnthropic).GetAPIProtocol())
	require.Equal(t, APIProtocolChatCompletions, mk(PlatformOther, APIProtocolChatCompletions).GetAPIProtocol())
	require.Equal(t, APIProtocolChatCompletions, mk(PlatformOther, "").GetAPIProtocol(), "other 缺失回退默认")
	require.Equal(t, APIProtocolChatCompletions, mk(PlatformOther, "bogus").GetAPIProtocol(), "other 非法值回退默认")
	require.Equal(t, APIProtocolChatCompletions, mk(PlatformOther, APIProtocolResponses).GetAPIProtocol(), "other 无 responses 端点")
	require.Equal(t, APIProtocolChatCompletions, mk(PlatformOther, APIProtocolAdaptive).GetAPIProtocol(), "other 无 adaptive 厂商默认端点")
	require.Equal(t, APIProtocolChatCompletions, mk(PlatformOpenAI, APIProtocolAnthropic).GetAPIProtocol(), "openai 不开放协议覆盖")
}

// TestGetOpenAIFormatBaseURLForAnthropicOther 锁定 anthropic 协议 other 的 OpenAI 格式
// base 必须失败关闭：凭证 base_url 指向 Anthropic 端点，不能拿来拼 /v1/embeddings 等
// OpenAI 路径（外审-2 同源：other 空 base 禁止回落官方 OpenAI）。
func TestGetOpenAIFormatBaseURLForAnthropicOther(t *testing.T) {
	t.Parallel()

	acct := &Account{
		Platform: PlatformOther,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "sk-lkeap",
			"base_url":     "https://api.lkeap.cloud.tencent.com/plan/anthropic",
			"api_protocol": APIProtocolAnthropic,
		},
	}
	require.Equal(t, "", acct.GetOpenAIFormatBaseURL())
	require.Equal(t, "https://api.lkeap.cloud.tencent.com/plan/anthropic", acct.GetAnthropicProtocolBaseURL())

	// chat_completions 协议 other 不受影响：OpenAI 格式 base = 凭证 base_url。
	chatAcct := &Account{
		Platform: PlatformOther,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-or",
			"base_url": "https://openrouter.ai/api/v1",
		},
	}
	require.Equal(t, "https://openrouter.ai/api/v1", chatAcct.GetOpenAIFormatBaseURL())
}

func TestSupportsNativeCNResponses(t *testing.T) {
	t.Parallel()
	require.True(t, (&Account{Platform: PlatformDeepseek}).SupportsNativeCNResponses())
	require.True(t, (&Account{Platform: PlatformKimi}).SupportsNativeCNResponses())
	require.True(t, (&Account{Platform: PlatformKimi, Credentials: map[string]any{"account_mode": AccountModeCoding}}).SupportsNativeCNResponses())
	require.True(t, (&Account{Platform: PlatformMiniMax}).SupportsNativeCNResponses())
	require.False(t, (&Account{Platform: PlatformZhipu}).SupportsNativeCNResponses())
	require.False(t, (&Account{Platform: PlatformOpenAI}).SupportsNativeCNResponses())
}

func TestAdaptiveProtocolBaseURLs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		platform      string
		mode          string
		wantChat      string
		wantAnthropic string
		wantResponses string
	}{
		{"kimi payg", PlatformKimi, AccountModePayG, DefaultKimiPayGBaseURL, DefaultKimiPayGAnthropicBaseURL, DefaultKimiPayGBaseURL},
		{"kimi coding", PlatformKimi, AccountModeCoding, DefaultKimiCodingBaseURL, DefaultKimiCodingAnthropicBaseURL, DefaultKimiCodingBaseURL},
		{"zhipu payg", PlatformZhipu, AccountModePayG, DefaultZhipuPayGBaseURL, DefaultZhipuAnthropicBaseURL, DefaultZhipuPayGBaseURL},
		{"zhipu coding", PlatformZhipu, AccountModeCoding, DefaultZhipuCodingBaseURL, DefaultZhipuAnthropicBaseURL, DefaultZhipuCodingBaseURL},
		{"deepseek", PlatformDeepseek, AccountModePayG, DefaultDeepseekBaseURL, DefaultDeepseekAnthropicBaseURL, DefaultDeepseekBaseURL},
		{"minimax payg", PlatformMiniMax, AccountModePayG, DefaultMiniMaxBaseURL, DefaultMiniMaxAnthropicBaseURL, DefaultMiniMaxBaseURL},
		{"minimax coding", PlatformMiniMax, AccountModeCoding, DefaultMiniMaxBaseURL, DefaultMiniMaxAnthropicBaseURL, DefaultMiniMaxBaseURL},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Platform: tc.platform, Type: AccountTypeAPIKey, Credentials: map[string]any{
				"api_protocol": APIProtocolAdaptive,
				"account_mode": tc.mode,
			}}
			require.Equal(t, tc.wantChat, account.GetCNProtocolBaseURL(APIProtocolChatCompletions))
			require.Equal(t, tc.wantAnthropic, account.GetCNProtocolBaseURL(APIProtocolAnthropic))
			require.Equal(t, tc.wantResponses, account.GetCNProtocolBaseURL(APIProtocolResponses))
			require.Equal(t, tc.wantAnthropic, account.GetAnthropicProtocolBaseURL())
		})
	}
}

func TestAdaptiveProtocolBaseURLOverrides(t *testing.T) {
	t.Parallel()

	account := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_protocol": APIProtocolAdaptive,
		"base_url":     "https://legacy-chat.example.com",
		"api_base_urls": map[string]any{
			APIProtocolChatCompletions: "https://chat.example.com",
			APIProtocolAnthropic:       "https://anthropic.example.com",
			APIProtocolResponses:       "https://responses.example.com",
		},
	}}

	require.Equal(t, "https://chat.example.com", account.GetOpenAIBaseURL())
	require.Equal(t, "https://chat.example.com", account.GetCNProtocolBaseURL(APIProtocolChatCompletions))
	require.Equal(t, "https://anthropic.example.com", account.GetAnthropicProtocolBaseURL())
	require.Equal(t, "https://responses.example.com", account.GetCNProtocolBaseURL(APIProtocolResponses))
}

// TestAnthropicProtocolBaseURL 验证 Anthropic 协议默认端点与协议感知的
// OpenAI 格式 base 回退。
func TestAnthropicProtocolBaseURL(t *testing.T) {
	t.Parallel()

	// 默认端点（按供应商 × 模式）
	require.Equal(t, "https://api.moonshot.cn/anthropic", (&Account{
		Platform: PlatformKimi, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolAnthropic},
	}).GetAnthropicProtocolBaseURL())
	require.Equal(t, "https://api.kimi.com/coding", (&Account{
		Platform: PlatformKimi, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolAnthropic, "account_mode": AccountModeCoding},
	}).GetAnthropicProtocolBaseURL())
	require.Equal(t, "https://open.bigmodel.cn/api/anthropic", (&Account{
		Platform: PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolAnthropic},
	}).GetAnthropicProtocolBaseURL())
	require.Equal(t, "https://api.deepseek.com/anthropic", (&Account{
		Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolAnthropic},
	}).GetAnthropicProtocolBaseURL())
	require.Equal(t, "https://api.minimaxi.com/anthropic", (&Account{
		Platform: PlatformMiniMax, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolAnthropic},
	}).GetAnthropicProtocolBaseURL())

	// 凭证 base_url 覆盖默认值
	require.Equal(t, "https://custom.example.com/anthropic", (&Account{
		Platform: PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolAnthropic, "base_url": "https://custom.example.com/anthropic"},
	}).GetAnthropicProtocolBaseURL())

	// 非 Anthropic 协议返回空串
	require.Empty(t, (&Account{
		Platform: PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://open.bigmodel.cn/api/paas/v4"},
	}).GetAnthropicProtocolBaseURL())
}

// TestGetOpenAIFormatBaseURL_ProtocolAware anthropic 协议账号的凭证 base_url
// 指向 Anthropic 端点，OpenAI 格式路径（模型同步等）必须回退到 CC 默认 base。
func TestGetOpenAIFormatBaseURL_ProtocolAware(t *testing.T) {
	t.Parallel()

	zhipuAnthropic := &Account{
		Platform: PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_protocol": APIProtocolAnthropic,
			"base_url":     "https://open.bigmodel.cn/api/anthropic",
		},
	}
	require.Equal(t, "https://open.bigmodel.cn/api/paas/v4", zhipuAnthropic.GetOpenAIFormatBaseURL())

	kimiCodingAnthropic := &Account{
		Platform: PlatformKimi, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_protocol": APIProtocolAnthropic,
			"account_mode": AccountModeCoding,
			"base_url":     "https://api.kimi.com/coding",
		},
	}
	require.Equal(t, "https://api.kimi.com/coding/v1", kimiCodingAnthropic.GetOpenAIFormatBaseURL())

	// chat_completions 协议下行为不变（凭证 base_url 原样返回）
	ccAccount := &Account{
		Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://ds-relay.example.com"},
	}
	require.Equal(t, "https://ds-relay.example.com", ccAccount.GetOpenAIFormatBaseURL())
}

// TestBuildUpstreamModelsRequest_AnthropicProtocol 模型同步使用协议感知 base。
func TestBuildUpstreamModelsRequest_AnthropicProtocol(t *testing.T) {
	t.Parallel()
	svc := &AccountTestService{cfg: &config.Config{}}
	account := &Account{
		ID: 1, Platform: PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"api_protocol": APIProtocolAnthropic,
			"base_url":     "https://open.bigmodel.cn/api/anthropic",
		},
	}
	req, err := svc.buildUpstreamModelsRequest(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "https://open.bigmodel.cn/api/paas/v4/models", req.URL.String())
}

// TestBuildOpenAIResponsesURLForPlatform deepseek 官方端点为 /responses（无 /v1）。
func TestBuildOpenAIResponsesURLForPlatform(t *testing.T) {
	t.Parallel()
	require.Equal(t, "https://api.deepseek.com/responses", buildOpenAIResponsesURLForPlatform(PlatformDeepseek, "https://api.deepseek.com"))
	require.Equal(t, "https://relay.example.com/responses", buildOpenAIResponsesURLForPlatform(PlatformDeepseek, "https://relay.example.com"))
	require.Equal(t, "https://relay.example.com/v1/responses", buildOpenAIResponsesURLForPlatform(PlatformDeepseek, "https://relay.example.com/v1"))
	require.Equal(t, "https://api.openai.com/v1/responses", buildOpenAIResponsesURLForPlatform(PlatformOpenAI, "https://api.openai.com"))
	require.Equal(t, "https://open.bigmodel.cn/api/paas/v4/responses", buildOpenAIResponsesURLForPlatform(PlatformZhipu, "https://open.bigmodel.cn/api/paas/v4"))
	require.Equal(t, "https://api.moonshot.cn/v1/responses", buildOpenAIResponsesURLForPlatform(PlatformKimi, "https://api.moonshot.cn/v1"))
	require.Equal(t, "https://api.kimi.com/coding/v1/responses", buildOpenAIResponsesURLForPlatform(PlatformKimi, "https://api.kimi.com/coding/v1"))
}

// TestNormalizeDeepSeekResponsesRequestBody 无状态适配：强制 store=false、
// 清除 previous_response_id；非原生 CN Responses 协议原样返回。
func TestNormalizeDeepSeekResponsesRequestBody(t *testing.T) {
	t.Parallel()

	deepseekResponses := &Account{
		Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolResponses},
	}
	body := []byte(`{"model":"deepseek-v4-pro","store":true,"previous_response_id":"resp_123","input":"hi"}`)
	normalized := normalizeDeepSeekResponsesRequestBody(deepseekResponses, body)
	require.False(t, gjson.GetBytes(normalized, "store").Bool())
	require.False(t, gjson.GetBytes(normalized, "previous_response_id").Exists())
	require.Equal(t, "deepseek-v4-pro", gjson.GetBytes(normalized, "model").String())

	deepseekAdaptive := &Account{
		Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolAdaptive},
	}
	adaptiveNormalized := normalizeDeepSeekResponsesRequestBody(deepseekAdaptive, body)
	require.False(t, gjson.GetBytes(adaptiveNormalized, "store").Bool())
	require.False(t, gjson.GetBytes(adaptiveNormalized, "previous_response_id").Exists())

	// 非 responses 协议（deepseek CC 账号）原样返回
	deepseekCC := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey}
	require.Equal(t, string(body), string(normalizeDeepSeekResponsesRequestBody(deepseekCC, body)))

	kimiResponses := &Account{
		Platform: PlatformKimi, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolResponses},
	}
	kimiNormalized := normalizeDeepSeekResponsesRequestBody(kimiResponses, body)
	require.False(t, gjson.GetBytes(kimiNormalized, "store").Bool())
	require.False(t, gjson.GetBytes(kimiNormalized, "previous_response_id").Exists())

	kimiCodingAdaptive := &Account{
		Platform: PlatformKimi, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolAdaptive, "account_mode": AccountModeCoding},
	}
	kimiCodingNormalized := normalizeDeepSeekResponsesRequestBody(kimiCodingAdaptive, body)
	require.False(t, gjson.GetBytes(kimiCodingNormalized, "store").Bool())
	require.False(t, gjson.GetBytes(kimiCodingNormalized, "previous_response_id").Exists())

	// openai 账号原样返回
	openai := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.Equal(t, string(body), string(normalizeDeepSeekResponsesRequestBody(openai, body)))
}

// TestGetAnthropicAPIKeyAuthScheme_CNProvider CN 账号可经 extra 覆写鉴权方案，
// 默认保持 x-api-key。
func TestGetAnthropicAPIKeyAuthScheme_CNProvider(t *testing.T) {
	t.Parallel()

	zhipu := &Account{
		Platform: PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolAnthropic},
	}
	require.Equal(t, AnthropicAPIKeyAuthSchemeXAPIKey, zhipu.GetAnthropicAPIKeyAuthScheme())

	zhipu.Extra = map[string]any{"anthropic_apikey_auth_scheme": "authorization_bearer"}
	require.Equal(t, AnthropicAPIKeyAuthSchemeAuthorizationBearer, zhipu.GetAnthropicAPIKeyAuthScheme())
}

// TestParseVolcanoHeaderTiers 火山从 OpenAI 兼容限流响应头解析 5h + 周 + 月窗口：
// x-ratelimit-reset-requests（绝对 Unix 秒）→ 5h 重置；limit/remaining → 5h 已用百分比；
// 周/月窗口按官方规则确定性计算，恒有 reset_at。
func TestParseVolcanoHeaderTiers(t *testing.T) {
	t.Parallel()
	resetUnix := time.Now().Add(3 * time.Hour).Unix()
	h := http.Header{}
	h.Set("x-ratelimit-reset-requests", strconv.FormatInt(resetUnix, 10))
	h.Set("x-ratelimit-limit-requests", "100")
	h.Set("x-ratelimit-remaining-requests", "80")
	tiers := parseVolcanoHeaderTiers(h)
	require.Len(t, tiers, 3)
	require.Equal(t, "5h", tiers[0].Window)
	require.InDelta(t, 20.0, tiers[0].UsedPercent, 1e-9)
	require.Equal(t, time.Unix(resetUnix, 0).UTC().Format(time.RFC3339), tiers[0].ResetAt)
	require.False(t, tiers[0].UsedPercentUnknown, "limit/remaining 齐备时 5h 用量已知")
	require.Equal(t, "weekly", tiers[1].Window)
	require.NotEmpty(t, tiers[1].ResetAt)
	require.Equal(t, "monthly", tiers[2].Window)
	require.NotEmpty(t, tiers[2].ResetAt)
}

// TestParseVolcanoHeaderTiersNo5hHeader 缺 5h 限流头时仍有确定性周/月窗口 tier（仍渲染倒计时）。
func TestParseVolcanoHeaderTiersNo5hHeader(t *testing.T) {
	t.Parallel()
	tiers := parseVolcanoHeaderTiers(http.Header{})
	require.Len(t, tiers, 2)
	require.Equal(t, "weekly", tiers[0].Window)
	require.NotEmpty(t, tiers[0].ResetAt)
	require.Equal(t, "monthly", tiers[1].Window)
	require.NotEmpty(t, tiers[1].ResetAt)
}

// TestParseVolcanoResetHeader 解析绝对 Unix 秒与相对秒数，拒空/非法/非未来。
func TestParseVolcanoResetHeader(t *testing.T) {
	t.Parallel()
	resetUnix := time.Now().Add(3 * time.Hour).Unix()
	s, ok := parseVolcanoResetHeader(strconv.FormatInt(resetUnix, 10))
	require.True(t, ok)
	require.Equal(t, time.Unix(resetUnix, 0).UTC().Format(time.RFC3339), s)

	// 相对秒数（小整数）→ now+秒，落在未来。
	rel := strconv.FormatInt(int64(time.Now().Add(2*time.Hour).Sub(time.Now()).Seconds())+1, 10)
	s2, ok2 := parseVolcanoResetHeader(rel)
	require.True(t, ok2)
	require.NotEmpty(t, s2)

	_, ok3 := parseVolcanoResetHeader("")
	require.False(t, ok3)
	_, ok4 := parseVolcanoResetHeader("not-a-number")
	require.False(t, ok4)
	_, ok5 := parseVolcanoResetHeader("0")
	require.False(t, ok5)
}

// TestVolcanoNextWeeklyReset 下一个周一 00:00（Asia/Shanghai），且必在未来。
func TestVolcanoNextWeeklyReset(t *testing.T) {
	t.Parallel()
	reset := volcanoNextWeeklyReset()
	require.NotEmpty(t, reset)
	parsed, err := time.Parse(time.RFC3339, reset)
	require.NoError(t, err)
	local := parsed.In(volcanoPlanLoc)
	require.Equal(t, time.Monday, local.Weekday())
	require.Equal(t, 0, local.Hour())
	require.Equal(t, 0, local.Minute())
	require.Equal(t, 0, local.Second())
	require.True(t, local.After(time.Now().In(volcanoPlanLoc)))
}

// TestVolcanoProbeModel 取 model_mapping 首个上游模型，缺映射回落默认模型。
func TestVolcanoProbeModel(t *testing.T) {
	t.Parallel()
	acc := &Account{
		Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-4o": "ark-model-x", "gpt-4o-mini": "ark-model-y"},
		},
	}
	require.Equal(t, "ark-model-x", volcanoProbeModel(acc))
	require.Equal(t, volcanoQuotaProbeDefaultModel, volcanoProbeModel(&Account{Platform: PlatformDeepseek}))
}

// TestVolcanoExtraUpdatesAndSnapshotReset volcano 快照写 volcano_ 前缀，且
// cnProviderQuotaSnapshotReset 按 GetCodingPlanProvider 取键（不能错读 deepseek_*）。
func TestVolcanoExtraUpdatesAndSnapshotReset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	tiers := []CNQuotaTier{
		{Window: "5h", UsedPercent: 30, ResetAt: "2026-09-04T11:00:00Z"},
		{Window: "weekly", UsedPercent: 5, ResetAt: "2026-09-07T08:00:00Z"},
	}
	updates := cnQuotaExtraUpdates(providerVolcano, tiers, now)
	require.Equal(t, 30.0, updates[cnExtraKey(providerVolcano, cnExtraSuffix5hUsed)])
	// tiers 未含 monthly，相关键应被清除为 nil。
	require.Nil(t, updates[cnExtraKey(providerVolcano, cnExtraSuffixMonthlyReset)])
	require.Nil(t, updates[cnExtraKey(providerVolcano, cnExtraSuffixMonthlyUsed)])
	reset := cnProviderQuotaSnapshotReset(&Account{
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"account_mode": AccountModeCoding,
			"base_url":     "https://ark.cn-beijing.volces.com/api/coding",
		},
		Extra: updates,
	}, now)
	require.NotNil(t, reset)
	require.Equal(t, "2026-09-04T11:00:00Z", reset.UTC().Format(time.RFC3339))
}

// TestVolcanoRealRequestProbe 火山走真实推理端点最小请求（Bearer ark 密钥 + POST JSON），
// 不复用已删除的 SigV4 AK/SK 管理 API 签名。验证探测模型与鉴权头构造。
func TestVolcanoRealRequestProbe(t *testing.T) {
	t.Parallel()
	// 探测模型取首个 mapping 值，回退默认公开模型。
	acc := &Account{
		Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "ark-test-key",
			"base_url":      "https://ark.cn-beijing.volces.com/api/coding",
			"model_mapping": map[string]any{"gpt-4o": "doubao-seed-2.1"},
		},
	}
	require.Equal(t, "doubao-seed-2.1", volcanoProbeModel(acc))

	// 火山分支应走 POST + Bearer，而非 AK/SK 签名头；此处仅校验模型/鉴权取值逻辑。
	require.NotEmpty(t, acc.GetCNAPIKey())
	require.True(t, isVolcanoBaseURL(acc.GetOpenAIBaseURL()))
}

// 外审 P2：limit 有效但 remaining 缺失/畸形时不得算成 100% 满额（否则健康账号被误暂停）；
// used 保持 0（未知）。
func TestParseVolcanoHeaderTiers_RemainingMissingNotFull(t *testing.T) {
	t.Parallel()
	resetUnix := time.Now().Add(3 * time.Hour).Unix()
	h := http.Header{}
	h.Set("x-ratelimit-reset-requests", strconv.FormatInt(resetUnix, 10))
	h.Set("x-ratelimit-limit-requests", "100")
	// 故意不设置 remaining。
	tiers := parseVolcanoHeaderTiers(h)
	require.Len(t, tiers, 3)
	require.Equal(t, "5h", tiers[0].Window)
	require.Equal(t, 0.0, tiers[0].UsedPercent, "remaining 缺失不得算成 100% 满额")
	require.True(t, tiers[0].UsedPercentUnknown, "remaining 缺失时 5h 用量未知，前端显示破折号而非假 0%")
	require.Equal(t, "weekly", tiers[1].Window)
	require.True(t, tiers[1].UsedPercentUnknown, "周用量上游不可得必须标记 Unknown")
	require.Equal(t, "monthly", tiers[2].Window)
	require.True(t, tiers[2].UsedPercentUnknown, "月用量上游不可得必须标记 Unknown")
}

func TestParseVolcanoHeaderTiers_RemainingMalformedNotFull(t *testing.T) {
	t.Parallel()
	resetUnix := time.Now().Add(3 * time.Hour).Unix()
	h := http.Header{}
	h.Set("x-ratelimit-reset-requests", strconv.FormatInt(resetUnix, 10))
	h.Set("x-ratelimit-limit-requests", "100")
	h.Set("x-ratelimit-remaining-requests", "not-a-number")
	tiers := parseVolcanoHeaderTiers(h)
	require.Len(t, tiers, 3)
	require.Equal(t, 0.0, tiers[0].UsedPercent)
	require.True(t, tiers[0].UsedPercentUnknown, "remaining 畸形时 5h 用量未知")
}

// F1：5h 重置头存在但 limit/remaining 完全缺失时，用量必须标记 Unknown（前端显示"—"），
// 不得渲染为假 0%。
func TestParseVolcanoHeaderTiers_5hUnknownWhenLimitMissing(t *testing.T) {
	t.Parallel()
	resetUnix := time.Now().Add(3 * time.Hour).Unix()
	h := http.Header{}
	h.Set("x-ratelimit-reset-requests", strconv.FormatInt(resetUnix, 10))
	// 不设置 limit / remaining。
	tiers := parseVolcanoHeaderTiers(h)
	require.Len(t, tiers, 3)
	require.Equal(t, "5h", tiers[0].Window)
	require.Equal(t, 0.0, tiers[0].UsedPercent)
	require.True(t, tiers[0].UsedPercentUnknown, "limit/remaining 全缺时 5h 用量未知")
}

// F2：火山账号收到 429（已限流）时，x-ratelimit-* 头同样携带重置时间；queryUsageForAccount
// 的 429 分支即调用 parseVolcanoHeaderTiers + cnQuotaExtraUpdates 落 5h 重置快照。此处验证
// 该数据通路确实能产出 volcano_5h_reset_at（已限流账号仍应显示倒计时）。
func TestVolcano429HeaderTiersPersist5hReset(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("x-ratelimit-reset-requests", strconv.FormatInt(time.Now().Add(3*time.Hour).Unix(), 10))
	h.Set("x-ratelimit-limit-requests", "100")
	h.Set("x-ratelimit-remaining-requests", "70")
	tiers := parseVolcanoHeaderTiers(h)
	require.Len(t, tiers, 3)
	require.Equal(t, "5h", tiers[0].Window)
	require.False(t, tiers[0].UsedPercentUnknown)
	updates := cnQuotaExtraUpdates(providerVolcano, tiers, time.Now())
	_, has5hReset := updates[cnExtraKey(providerVolcano, cnExtraSuffix5hReset)]
	require.True(t, has5hReset, "429 限流头须能落 5h 重置快照")
}

// F3：重置头兼容 Go duration、RFC3339、毫秒 epoch；过去时间被拒。
func TestParseVolcanoResetHeader_CompatibleFormats(t *testing.T) {
	t.Parallel()
	// 相对 duration "6m0s" → now+6min，落在未来。
	s, ok := parseVolcanoResetHeader("6m0s")
	require.True(t, ok)
	parsed, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	require.True(t, parsed.After(time.Now()))

	// RFC3339 绝对时间（未来）。
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	s2, ok2 := parseVolcanoResetHeader(future)
	require.True(t, ok2)
	require.Equal(t, future, s2)

	// 毫秒 epoch（>=1e12）→ 正确归一为秒级 epoch。
	ms := strconv.FormatInt(time.Now().Add(90*time.Minute).UnixMilli(), 10)
	s3, ok3 := parseVolcanoResetHeader(ms)
	require.True(t, ok3)
	secParsed, err := time.Parse(time.RFC3339, s3)
	require.NoError(t, err)
	require.InDelta(t, time.Now().Add(90*time.Minute).Unix(), secParsed.Unix(), 5)

	// 过去绝对时间被拒。
	past := strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10)
	_, ok4 := parseVolcanoResetHeader(past)
	require.False(t, ok4)
}

// F4：naive（无时区）续期时间按 Asia/Shanghai 落地，而非默认 UTC（否则 24h 旧记录被误判约 8h 旧）。
func TestParseWorkerTime_ShanghaiNaive(t *testing.T) {
	t.Parallel()
	// 北京时间 2026-09-04 10:00:00 应为 UTC 2026-09-04 02:00:00。
	ts, ok := parseWorkerTime("2026-09-04T10:00:00")
	require.True(t, ok)
	require.Equal(t, 2026, ts.Year())
	require.Equal(t, time.September, ts.Month())
	require.Equal(t, 4, ts.Day())
	require.Equal(t, 10, ts.Hour(), "Shanghai 落地后本地小时应为 10")
	require.Equal(t, 0, ts.Minute())
	require.Equal(t, 2, ts.UTC().Hour(), "naive 时间须按 Asia/Shanghai 解释，UTC 应为 02:00 而非 10:00")

	// RFC3339 带时区仍正确。
	ts2, ok2 := parseWorkerTime("2026-09-04T10:00:00+08:00")
	require.True(t, ok2)
	require.Equal(t, 10, ts2.Hour())
	require.Equal(t, 2, ts2.UTC().Hour())

	// 空串返回失败。
	_, ok3 := parseWorkerTime("")
	require.False(t, ok3)
}

// 外审 P1：周用量 Unknown 时只落 reset_at，不得写假 0 的 weekly_used_percent（覆盖旧值/
// 误导“未用”），前端据此渲染倒计时并显示“未知”。
func TestCNQuotaExtraUpdates_VolcanoWeeklyUnknown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	tiers := []CNQuotaTier{
		{Window: "5h", UsedPercent: 30, ResetAt: "2026-09-04T11:00:00Z"},
		{Window: "weekly", UsedPercent: 0, ResetAt: "2026-09-07T08:00:00Z", UsedPercentUnknown: true},
	}
	updates := cnQuotaExtraUpdates(providerVolcano, tiers, now)
	require.Equal(t, 30.0, updates[cnExtraKey(providerVolcano, cnExtraSuffix5hUsed)])
	require.Equal(t, "2026-09-04T11:00:00Z", updates[cnExtraKey(providerVolcano, cnExtraSuffix5hReset)])
	// 关键：周 reset 必须落库（前端渲染倒计时），weekly_used_percent 必须清空（不得假 0）。
	require.Equal(t, "2026-09-07T08:00:00Z", updates[cnExtraKey(providerVolcano, cnExtraSuffixWeeklyReset)])
	require.Nil(t, updates[cnExtraKey(providerVolcano, cnExtraSuffixWeeklyUsed)], "周用量 Unknown 不得写假 0")
}

// P1（重审）：5h 用量 Unknown（limit/remaining 缺失/畸形）时，volcano_5h_used_percent
// 必须清空（不得写假 0 误导“未用”），但 volcano_5h_reset_at 仍需落库以渲染倒计时。
// 此前仅周档处理 Unknown，导致 5h 档把 0 持久化为 0% 假象。
func TestCNQuotaExtraUpdates_Volcano5hUnknownPersist(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	tiers := []CNQuotaTier{
		{Window: "5h", UsedPercent: 0, ResetAt: "2026-09-04T11:00:00Z", UsedPercentUnknown: true},
		{Window: "weekly", UsedPercent: 0, ResetAt: "2026-09-07T08:00:00Z", UsedPercentUnknown: true},
	}
	updates := cnQuotaExtraUpdates(providerVolcano, tiers, now)
	// 5h reset 必须落库（前端渲染 5h 倒计时），5h_used_percent 必须清空（不得假 0）。
	require.Equal(t, "2026-09-04T11:00:00Z", updates[cnExtraKey(providerVolcano, cnExtraSuffix5hReset)])
	require.Nil(t, updates[cnExtraKey(providerVolcano, cnExtraSuffix5hUsed)], "5h 用量 Unknown 不得写假 0")
	// 周档同步正确。
	require.Equal(t, "2026-09-07T08:00:00Z", updates[cnExtraKey(providerVolcano, cnExtraSuffixWeeklyReset)])
	require.Nil(t, updates[cnExtraKey(providerVolcano, cnExtraSuffixWeeklyUsed)])
}

// 外审 P1：火山探测按账号 API 协议构造请求——Anthropic→/v1/messages + anthropic-version +
// x-api-key；OpenAI→/v3/chat/completions + Bearer。确保 Anthropic 协议火山账号也能命中正确
// 端点拿到响应头，而非永远 404/端点错误导致快照不更新。
func TestBuildVolcanoProbeRequest_ProtocolBranches(t *testing.T) {
	t.Parallel()
	profile, ok := parseVolcanoPlanProfile("https://ark.cn-beijing.volces.com/api/coding")
	require.True(t, ok)

	anthropicAcc := &Account{
		Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "ark-test", "api_protocol": APIProtocolAnthropic},
	}
	reqA, err := buildVolcanoProbeRequest(context.Background(), anthropicAcc, profile, "doubao-x", "ark-test")
	require.NoError(t, err)
	require.Equal(t, profile.anthropicMessagesURL(), reqA.URL.String())
	require.Equal(t, "2023-06-01", reqA.Header.Get("anthropic-version"))
	require.Equal(t, "ark-test", reqA.Header.Get("x-api-key"))
	require.Empty(t, reqA.Header.Get("Authorization"), "Anthropic 火山默认走 x-api-key，不应带 Authorization")

	openaiAcc := &Account{
		Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "ark-test"},
	}
	reqO, err := buildVolcanoProbeRequest(context.Background(), openaiAcc, profile, "doubao-x", "ark-test")
	require.NoError(t, err)
	require.Equal(t, profile.openAIChatCompletionsURL(), reqO.URL.String())
	require.Equal(t, "Bearer ark-test", reqO.Header.Get("Authorization"))
	require.Empty(t, reqO.Header.Get("anthropic-version"))
}

// 外审 P2：用量未知档位序列化时 used_percent 必须为 null（而非 0），前端据此显示“未知/—”，
// 与持久化快照（同样以 null 表示未知）保持一致；已知用量仍为数值。
func TestCNQuotaTier_MarshalUnknownNull(t *testing.T) {
	t.Parallel()
	unknown := CNQuotaTier{Window: "weekly", UsedPercent: 0, ResetAt: "2026-09-07T08:00:00Z", UsedPercentUnknown: true}
	b, err := json.Marshal(unknown)
	require.NoError(t, err)
	require.Contains(t, string(b), `"used_percent":null`)
	require.Contains(t, string(b), `"used_percent_unknown":true`)

	known := CNQuotaTier{Window: "5h", UsedPercent: 42.5, ResetAt: "2026-09-04T11:00:00Z"}
	b2, err := json.Marshal(known)
	require.NoError(t, err)
	require.Contains(t, string(b2), `"used_percent":42.5`)
	require.NotContains(t, string(b2), "null")
}

// TestVolcanoGetCodingPlanProvider coding 模式 + 火山 base_url 识别为 volcano 供应商。
func TestVolcanoGetCodingPlanProvider(t *testing.T) {
	t.Parallel()
	for _, baseURL := range []string{
		"https://ark.cn-beijing.volces.com/api/coding",
		"https://ark.cn-beijing.volces.com/api/plan",
	} {
		a := &Account{
			Platform:    PlatformDeepseek,
			Type:        AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": baseURL, "account_mode": AccountModeCoding},
		}
		require.Equal(t, providerVolcano, a.GetCodingPlanProvider())
	}
	// 非 coding 模式不识别。
	payg := &Account{
		Platform:    PlatformDeepseek,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://ark.cn-beijing.volces.com/api/plan", "account_mode": AccountModePayG},
	}
	require.Equal(t, "", payg.GetCodingPlanProvider())
}

// TestResolveCNQuotaProvider_VolcanoPayG 火山订阅号账号即使保存为 payg（deepseek
// 创建/编辑界面默认模式），也按 base_url 识别为 volcano 供应商；普通 CN payg 账号
// 与非火山 deepseek 仍返回空（保持 coding-only 语义）。
func TestResolveCNQuotaProvider_VolcanoPayG(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		account  *Account
		expected string
	}{
		{"volcano coding base_url", &Account{
			Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": "https://ark.cn-beijing.volces.com/api/coding/v3", "account_mode": AccountModeCoding},
		}, providerVolcano},
		{"volcano agent plan base_url", &Account{
			Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": "https://ark.cn-beijing.volces.com/api/plan", "account_mode": AccountModePayG},
		}, providerVolcano},
		// 火山地址仅存在于 api_base_urls.chat_completions（adaptive 协议），同样识别。
		{"volcano via api_base_urls.chat_completions", &Account{
			Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
			Credentials: map[string]any{
				"account_mode": AccountModePayG,
				"api_protocol": "adaptive",
				"api_base_urls": map[string]any{
					"chat_completions": "https://ark.cn-beijing.volces.com/api/coding/v3",
					"anthropic":        "https://ark.cn-beijing.volces.com/api/coding",
				},
			},
		}, providerVolcano},
		{"plain deepseek coding", &Account{
			Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": "https://api.deepseek.com", "account_mode": AccountModeCoding},
		}, ""},
		{"plain deepseek payg", &Account{
			Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": "https://api.deepseek.com", "account_mode": AccountModePayG},
		}, ""},
		{"kimi payg", &Account{
			Platform: PlatformKimi, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": "https://api.moonshot.cn/v1", "account_mode": AccountModePayG},
		}, ""},
		{"zhipu coding", &Account{
			Platform: PlatformZhipu, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": "https://open.bigmodel.cn/api/coding/paas/v4", "account_mode": AccountModeCoding},
		}, PlatformZhipu},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, resolveCNQuotaProvider(tc.account))
		})
	}
}

// TestResolveCNQuotaProvider_VolcanoPreferBaseURLOverPlatform 锁定用户反馈根因：
// 火山订阅号账号的 platform 常被误存为 kimi，且 api_protocol=adaptive 时
// GetOpenAIBaseURL() 优先取 api_base_urls.chat_completions（指向 api.kimi.com）。
// 若按 GetOpenAIBaseURL 判定会被误判为 Kimi，探测发往 Kimi /usages 返回 0% 周用量
// 且缺失 5h/月档。凭据 base_url（= ark.cn-beijing.volces.com）才是事实源，必须识别为
// providerVolcano，与 platform 误存无关。
func TestResolveCNQuotaProvider_VolcanoPreferBaseURLOverPlatform(t *testing.T) {
	t.Parallel()
	acc := &Account{
		Platform: PlatformKimi, // 误存：实际是火山订阅号
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "ark-x",
			"base_url":     "https://ark.cn-beijing.volces.com/api/coding",
			"api_protocol": APIProtocolAdaptive,
			"api_base_urls": map[string]any{
				"chat_completions": "https://api.kimi.com/coding/v1", // 会让 GetOpenAIBaseURL 误判
				"anthropic":        "https://api.kimi.com/coding/anthropic",
			},
		},
	}
	// 事实校验：GetOpenAIBaseURL 确实会被 adaptive 覆盖成 kimi 端点（即旧判定会误判）。
	require.Equal(t, "https://api.kimi.com/coding/v1", acc.GetOpenAIBaseURL(),
		"前置条件：adaptive 账号 GetOpenAIBaseURL 须返回 kimi 端点，方能验证火山优先修复")
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/coding", acc.GetBaseURL(),
		"前置条件：凭据原始 base_url 须是火山地址")

	// 核心断言：无论 platform 误存与 GetOpenAIBaseURL 指向 kimi，仍识别为火山。
	require.Equal(t, providerVolcano, resolveCNQuotaProvider(acc),
		"platform=kimi 误存 + base_url=火山 的账号必须识别为 volcano，不得发往 kimi 探测")
}

// TestValidateCodingPlanAccount_VolcanoPayGAllowed 校验 payg 火山订阅号账号可通过
// Coding Plan 探测校验；普通 deepseek payg 仍返回 CN_QUOTA_NOT_CODING_PLAN。
func TestValidateCodingPlanAccount_VolcanoPayGAllowed(t *testing.T) {
	t.Parallel()

	volcano := &Account{
		Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://ark.cn-beijing.volces.com/api/coding/v3", "account_mode": AccountModePayG},
	}
	require.NoError(t, validateCodingPlanAccount(volcano), "payg 火山订阅号应放行 Coding Plan 探测")

	plain := &Account{
		Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://api.deepseek.com", "account_mode": AccountModePayG},
	}
	err := validateCodingPlanAccount(plain)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CN_QUOTA_NOT_CODING_PLAN")
}
