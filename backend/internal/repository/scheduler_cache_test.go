package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFilterSchedulerCredentialsKeepsSubscriptionPlanType(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"plan_type":     "plus",
		"access_token":  "secret-access-token",
		"refresh_token": "secret-refresh-token",
	})

	require.Equal(t, "plus", filtered["plan_type"])
	require.NotContains(t, filtered, "access_token")
	require.NotContains(t, filtered, "refresh_token")
}

func TestSchedulerMetadataAccountKeepsOpenAISubscriptionIdentity(t *testing.T) {
	account := service.Account{
		ID:       24,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"plan_type":    "plus",
			"access_token": "secret-access-token",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)

	require.True(t, metadata.IsOpenAIChatGPTSubscription())
	require.Empty(t, metadata.GetCredential("access_token"))
}

func TestSchedulerMetadataAccountProjectsUpstreamBillingProbe(t *testing.T) {
	lastError := strings.Repeat("upstream diagnostic ", 512)
	probe := map[string]any{
		"status": "ok",
		"data": map[string]any{
			"billing_scope":             "token",
			"resolved_rate_multiplier":  0.03,
			"peak_rate_enabled":         true,
			"peak_start":                "09:00",
			"peak_end":                  "18:00",
			"peak_rate_multiplier":      2.0,
			"timezone":                  "Asia/Shanghai",
			"effective_rate_multiplier": 0.03,
			"remote_diagnostic":         lastError,
		},
		"received_at":   "2026-07-13T10:00:00Z",
		"fresh_until":   "2026-07-13T11:00:00Z",
		"next_probe_at": "2026-07-13T10:30:00Z",
		"http_status":   502,
		"last_error":    lastError,
	}
	account := service.Account{
		ID: 42,
		Extra: map[string]any{
			"upstream_billing_probe": probe,
			"unused_large_field":     "drop-me",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)
	fullPayload, metaPayload, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)

	filtered, ok := metadata.Extra["upstream_billing_probe"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "ok", filtered["status"])
	require.Equal(t, "2026-07-13T10:00:00Z", filtered["received_at"])
	require.Equal(t, "2026-07-13T11:00:00Z", filtered["fresh_until"])
	require.Equal(t, "2026-07-13T10:30:00Z", filtered["next_probe_at"])
	require.NotContains(t, filtered, "http_status")
	require.NotContains(t, filtered, "last_error")
	filteredData, ok := filtered["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "token", filteredData["billing_scope"])
	require.Equal(t, 0.03, filteredData["resolved_rate_multiplier"])
	require.Equal(t, true, filteredData["peak_rate_enabled"])
	require.Equal(t, "09:00", filteredData["peak_start"])
	require.Equal(t, "18:00", filteredData["peak_end"])
	require.Equal(t, 2.0, filteredData["peak_rate_multiplier"])
	require.Equal(t, "Asia/Shanghai", filteredData["timezone"])
	require.NotContains(t, filteredData, "effective_rate_multiplier")
	require.NotContains(t, filteredData, "remote_diagnostic")
	require.NotContains(t, metadata.Extra, "unused_large_field")
	require.Contains(t, string(fullPayload), lastError)
	require.NotContains(t, string(metaPayload), "last_error")
	require.Less(t, len(metaPayload)*4, len(fullPayload))
}

// cnQuotaPlatforms 是 CN 供应商平台枚举（余额键族按 account.Platform 加前缀，
// 见 service.cnExtraKey）。
var cnQuotaPlatforms = []string{
	service.PlatformKimi,
	service.PlatformDeepseek,
	service.PlatformZhipu,
	service.PlatformMiniMax,
}

// cnQuotaBalanceSuffixes 是 {platform}_balance* 余额键族后缀。
var cnQuotaBalanceSuffixes = []string{
	"balance",
	"balance_updated_at",
	"balance_available",
	"balance_unlimited",
	"balance_currency",
	"balance_low",
	"balances",
}

// cnCodingPlanUsageProviders 是 Coding Plan 滚动窗口用量键的 provider 枚举
// （cnQuotaExtraUpdates 按 provider 前缀落键；火山 volcano 只能靠凭据 base_url 判定）。
var cnCodingPlanUsageProviders = []string{
	service.PlatformKimi,
	service.PlatformDeepseek,
	service.PlatformZhipu,
	service.PlatformMiniMax,
	"volcano",
}

// cnCodingPlanUsageSuffixes 是 Coding Plan 滚动窗口用量键族后缀
// （对齐 service.cnExtraSuffix*：5h / weekly / monthly + 采集时间）。
var cnCodingPlanUsageSuffixes = []string{
	"5h_used_percent",
	"5h_reset_at",
	"weekly_used_percent",
	"weekly_reset_at",
	"monthly_used_percent",
	"monthly_reset_at",
	"usage_updated_at",
}

func TestFilterSchedulerExtraKeepsCNQuotaKeys(t *testing.T) {
	extra := map[string]any{
		"kira_usage_snapshot":       map[string]any{"used_percent": 42},
		"kira_probe_cooldown_until": "2026-10-10T12:00:00Z",
		"th_pass_snapshot":          map[string]any{"has_pass": true},
		"th_usage_snapshot":         map[string]any{"windows": map[string]any{}},
		"th_balance":                3.5,
		"th_balance_updated_at":     "2026-10-10T11:00:00Z",
		"th_balance_low":            false,
		"th_probe_backoff_until":    "2026-10-10T12:30:00Z",
		"cn_quota_lifecycle":        map[string]any{"state": "quota_exhausted"},
	}
	// CN 供应商 Coding Plan 滚动窗口用量键族（G2-R3）。
	for _, provider := range cnCodingPlanUsageProviders {
		for _, suffix := range cnCodingPlanUsageSuffixes {
			extra[provider+"_"+suffix] = "value-" + provider + "_" + suffix
		}
	}
	for _, platform := range cnQuotaPlatforms {
		for _, suffix := range cnQuotaBalanceSuffixes {
			extra[platform+"_"+suffix] = "value-" + platform + "_" + suffix
		}
	}

	filtered := filterSchedulerExtra(extra)

	require.Len(t, filtered, len(extra), "CN 额度键族必须整体进调度快照投影")
	for key, want := range extra {
		require.Equal(t, want, filtered[key], "CN 额度键 %s 必须进调度快照投影", key)
	}
}

func TestFilterSchedulerExtraDropsNonWhitelistedCNNeighborKeys(t *testing.T) {
	filtered := filterSchedulerExtra(map[string]any{
		"kira_usage_snapshot": map[string]any{"used_percent": 42},
		// 同前缀但非白名单键：会话态必须继续裁掉，其余近邻键保持现状不进投影。
		"th_session_cookie":       "secret-session-cookie",
		"th_session_login_at":     "2026-10-10T10:00:00Z",
		"th_probe_backoff_reason": "tokenharbor login rejected",
		"th_wallet_snapshot":      map[string]any{"value": 3.5},
		"kimi_balance_plan_name":  "pro",
		// G2-R3 只放行 5h/weekly/monthly/usage_updated_at 四档后缀；daily 档
		// 不在 cnQuotaExtraUpdates 落键集合内，必须继续裁掉。
		"kimi_daily_used_percent": 12.5,
		"volcano_daily_reset_at":  "2026-10-10T18:00:00Z",
	})

	require.Contains(t, filtered, "kira_usage_snapshot")
	for _, key := range []string{
		"th_session_cookie",
		"th_session_login_at",
		"th_probe_backoff_reason",
		"th_wallet_snapshot",
		"kimi_balance_plan_name",
		"kimi_daily_used_percent",
		"volcano_daily_reset_at",
	} {
		require.NotContains(t, filtered, key)
	}
}

// TestSchedulerMetadataAccountProjectsKiraPaidQuotaDimension 是白名单补键的端到端锁：
// 快照投影 → 账号重建 → ResolveAccountQuotaDimensions 必须能产出 Kira 付费维度
// （VND 余额键族此前被裁 ⇒ 无维度 ⇒ §3.1 F6 门 fail-open）。
//
// G2-R3 起 filterSchedulerCredentials 放行 base_url ⇒ 投影账号命中
// accountIsKiraBaseURL ⇒ 走 Kira 分支 ⇒ kira_usage_snapshot 被消费，维度列表为
// [free（每日免费池）, paid（VND 余额）]。免费维度端到端锁见
// TestSchedulerMetadataAccountProjectsKiraFreeQuotaDimension。
func TestSchedulerMetadataAccountProjectsKiraPaidQuotaDimension(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	account := service.Account{
		ID:       88,
		Platform: service.PlatformKimi,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://kiraai.vn/api/v1",
			"api_key":  "secret-api-key",
		},
		Extra: map[string]any{
			"kira_usage_snapshot": map[string]any{
				"fetched_at":   now.Format(time.RFC3339),
				"used_percent": 42,
			},
			"kimi_balance":            12.5,
			"kimi_balance_updated_at": now.Format(time.RFC3339),
		},
	}

	metadata := buildSchedulerMetadataAccount(account)
	dims := service.ResolveAccountQuotaDimensions(&metadata, now)

	require.Len(t, dims, 2)
	require.Equal(t, service.QuotaDimensionKindFree, dims[0].Kind)
	require.Equal(t, service.QuotaDimensionKindPaid, dims[1].Kind)
	require.Equal(t, service.QuotaDimensionScopeAccount, dims[1].Scope)
	require.Equal(t, service.QuotaDimensionRemaining, dims[1].Status)
	require.True(t, dims[1].Confirmed())
	require.Equal(t, service.QuotaDimensionGateAllow, service.EvaluateAccountQuotaDimensionGate(dims))
}

// TestFilterSchedulerCredentialsKeepsBaseURLAndDropsSecrets 锁住 G2-R3 的凭据投影边界：
// base_url（上游 URL，非机密）必须进投影——它是 Kira/TH/火山分支判定的唯一事实源；
// token / session / cookie / 口令类敏感字段必须继续裁掉。
//
// 注：api_key 在本卡之前就已在白名单内（hydrateSelectedAccount 用快照账号做转发，
// 需要它），不在本卡放行范围、也不在本卡收紧范围，故不纳入本负例。
func TestFilterSchedulerCredentialsKeepsBaseURLAndDropsSecrets(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"base_url":      "https://kiraai.vn/api/v1",
		"access_token":  "secret-access-token",
		"refresh_token": "secret-refresh-token",
		"session_id":    "secret-session-id",
		"cookie":        "secret-cookie",
		"kira_jwt":      "secret-kira-jwt",
		"kira_password": "secret-kira-password",
	})

	require.Equal(t, "https://kiraai.vn/api/v1", filtered["base_url"])
	for _, key := range []string{"access_token", "refresh_token", "session_id", "cookie", "kira_jwt", "kira_password"} {
		require.NotContains(t, filtered, key, "凭据敏感字段 %s 必须继续裁掉", key)
	}
}

// TestSchedulerMetadataAccountProjectsKiraFreeQuotaDimension 闭合 G2-R2 登记缺口：
// base_url 放行后投影账号命中 Kira 分支，kira_usage_snapshot 被消费并产出
// kind=free 的账号级维度（此前无人消费 ⇒ 免费维度在主路径为空）。
func TestSchedulerMetadataAccountProjectsKiraFreeQuotaDimension(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	account := service.Account{
		ID:       88,
		Platform: service.PlatformKimi,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://kiraai.vn/api/v1",
			"api_key":  "secret-api-key",
		},
		Extra: map[string]any{
			"kira_usage_snapshot": map[string]any{
				"fetched_at":   now.Format(time.RFC3339),
				"used_percent": 10,
			},
		},
	}

	metadata := buildSchedulerMetadataAccount(account)
	require.Equal(t, "https://kiraai.vn/api/v1", metadata.GetBaseURL(),
		"base_url 必须进投影，否则 Kira/TH/火山分支判定失效")

	dims := service.ResolveAccountQuotaDimensions(&metadata, now)

	var free *service.QuotaDimension
	for i := range dims {
		if dims[i].Kind == service.QuotaDimensionKindFree {
			free = &dims[i]
			break
		}
	}
	require.NotNil(t, free, "投影账号必须产出 Kira 免费维度")
	require.Equal(t, service.QuotaDimensionScopeAccount, free.Scope)
	require.Equal(t, service.QuotaDimensionRemaining, free.Status)
	require.True(t, free.Confirmed())
	require.Equal(t, "kira_usage_snapshot", free.Source)
	require.Equal(t, service.QuotaDimensionGateAllow, service.EvaluateAccountQuotaDimensionGate(dims))
}

// TestSchedulerMetadataAccountProjectsTokenHarborSubscriptionAndFreeDimensions 锁住
// TH 端到端：base_url 放行后投影账号命中 accountIsTokenHarborBaseURL，产出
// subscription（has_pass）+ free（官方 free-tier 口径）+ paid（钱包，能力字段为真）
// 三个账号级维度。
func TestSchedulerMetadataAccountProjectsTokenHarborSubscriptionAndFreeDimensions(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	account := service.Account{
		ID:       99,
		Platform: service.PlatformKimi,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://tokenharbor.ai/v1",
			"api_key":  "secret-api-key",
		},
		Extra: map[string]any{
			service.TokenHarborPassSnapshotExtraKey: map[string]any{
				"has_pass":              true,
				"pass_name":             "pro",
				"spend_after_allowance": true,
				"plan_exhausted":        false,
				"exhausted":             false,
				"fetched_at":            now.Format(time.RFC3339),
			},
			service.TokenHarborWalletBalanceExtraKey:          3.5,
			service.TokenHarborWalletBalanceUpdatedAtExtraKey: now.Format(time.RFC3339),
		},
	}

	metadata := buildSchedulerMetadataAccount(account)
	dims := service.ResolveAccountQuotaDimensions(&metadata, now)

	kinds := make(map[service.QuotaDimensionKind]service.QuotaDimensionStatus)
	for _, d := range dims {
		kinds[d.Kind] = d.Status
	}
	require.Equal(t, service.QuotaDimensionRemaining, kinds[service.QuotaDimensionKindSubscription])
	require.Equal(t, service.QuotaDimensionRemaining, kinds[service.QuotaDimensionKindFree])
	require.Equal(t, service.QuotaDimensionRemaining, kinds[service.QuotaDimensionKindPaid])
	require.Equal(t, service.QuotaDimensionGateAllow, service.EvaluateAccountQuotaDimensionGate(dims))
}

// TestSchedulerMetadataAccountProjectsCodingPlanQuotaDimensions 锁住 coding-plan
// 端到端：火山（volcano）订阅号只能靠凭据 base_url 判定 provider（platform 常存为
// kimi/deepseek），base_url 放行 ⇒ resolveCNQuotaProvider 解析出 volcano ⇒
// 5h / weekly / monthly 三档滚动窗口维度全部可解析。
func TestSchedulerMetadataAccountProjectsCodingPlanQuotaDimensions(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	account := service.Account{
		ID:       77,
		Platform: service.PlatformKimi,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://ark.cn-beijing.volces.com/api/coding/v1",
			"api_key":  "secret-api-key",
		},
		Extra: map[string]any{
			"volcano_usage_updated_at":     now.Format(time.RFC3339),
			"volcano_5h_used_percent":      10,
			"volcano_5h_reset_at":          now.Add(5 * time.Hour).Format(time.RFC3339),
			"volcano_weekly_used_percent":  100,
			"volcano_weekly_reset_at":      now.Add(48 * time.Hour).Format(time.RFC3339),
			"volcano_monthly_used_percent": 30,
			"volcano_monthly_reset_at":     now.Add(240 * time.Hour).Format(time.RFC3339),
		},
	}

	metadata := buildSchedulerMetadataAccount(account)
	dims := service.ResolveAccountQuotaDimensions(&metadata, now)

	require.Len(t, dims, 3, "5h / weekly / monthly 三档维度必须全部可解析")
	for _, d := range dims {
		require.Equal(t, service.QuotaDimensionKindFree, d.Kind)
		require.Equal(t, service.QuotaDimensionScopeAccount, d.Scope)
		require.True(t, d.Confirmed())
		require.Equal(t, "volcano_usage_updated_at", d.Source)
	}
	require.Equal(t, service.QuotaDimensionRemaining, dims[0].Status)
	require.Equal(t, service.QuotaDimensionExhausted, dims[1].Status)
	require.Equal(t, service.QuotaDimensionRemaining, dims[2].Status)
	require.Equal(t, service.QuotaDimensionGateAllow, service.EvaluateAccountQuotaDimensionGate(dims))
}

func TestSchedulerMetadataAccountDropsInvalidUpstreamBillingProbe(t *testing.T) {
	for _, probe := range []any{
		"invalid",
		map[string]any{},
		map[string]any{"status": ""},
	} {
		metadata := buildSchedulerMetadataAccount(service.Account{
			Extra: map[string]any{service.UpstreamBillingProbeExtraKey: probe},
		})

		require.NotContains(t, metadata.Extra, service.UpstreamBillingProbeExtraKey)
	}
}
