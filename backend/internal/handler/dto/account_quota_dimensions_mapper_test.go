package dto

// 账号额度维度 DTO 投影 + balance_low 标记单测（方案 §3.4 / C5 卡）。
//
// 覆盖：
//   - 五类账号族（TH / Kira / coding plan / relay / 无快照）经 AccountFromServiceShallow
//     投影出正确的 quota_dimensions 条目数、枚举小写字符串与 observed_at 序列化形态；
//   - balance_low：extra 含 {platform}_balance_low 标记 → true；不含 → false（omitempty 省略）；
//   - 无快照账号 → 字段省略（与实现选择一致：空列表 + omitempty）。
//
// 说明：mapper 内部取 time.Now()，故夹具时间戳用相对 now 的近期值（10 分钟年龄门内）。
// 未导出的 service 键常数（kiraUsageSnapshotExtraKey / cnExtraKey / 后缀）在测试中按
// 线上键名原文构造，与 service 包写入端一致（service 包不可改，测试只读镜像键名）。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func dtoRecentRFC3339() string {
	return time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339)
}

// dtoTokenHarborAccount 构造 TH（tokenharbor.ai）账号：订阅快照 + 官方 free 口径。
func dtoTokenHarborAccount() *service.Account {
	return &service.Account{
		ID: 9201, Name: "th", Platform: service.PlatformOther, Type: service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Credentials: map[string]any{"api_key": "sk-th", "base_url": "https://tokenharbor.ai/v1"},
		Extra:       map[string]any{},
	}
}

// dtoKiraAccount 构造 Kira（kiraai.vn）账号：免费池快照 + VND 余额（platform 前缀键）。
func dtoKiraAccount() *service.Account {
	return &service.Account{
		ID: 9202, Name: "kira", Platform: service.PlatformOther, Type: service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Credentials: map[string]any{"api_key": "kira-key", "base_url": "https://kiraai.vn/api/v1"},
		Extra:       map[string]any{},
	}
}

// dtoKimiCodingAccount 构造 Coding Plan 账号（滚动窗口键 kimi_*）。
func dtoKimiCodingAccount() *service.Account {
	return &service.Account{
		ID: 9203, Name: "kimi-coding", Platform: service.PlatformKimi, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive,
		Credentials: map[string]any{
			"api_key":      "sk-kimi-coding",
			"base_url":     "https://api.kimi.com/coding",
			"account_mode": service.AccountModeCoding,
		},
		Extra: map[string]any{},
	}
}

// dtoKimiRelayAccount 构造 relay / payg 账号（{platform}_balance 键族）。
func dtoKimiRelayAccount() *service.Account {
	return &service.Account{
		ID: 9204, Name: "kimi-relay", Platform: service.PlatformKimi, Type: service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Credentials: map[string]any{"api_key": "sk-kimi-payg"},
		Extra:       map[string]any{},
	}
}

// dtoNoSnapshotAccount 构造无任何额度快照的账号（Anthropic OAuth）。
func dtoNoSnapshotAccount() *service.Account {
	return &service.Account{
		ID: 9205, Name: "anthropic-oauth", Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Credentials: map[string]any{"access_token": "t"},
		Extra:       map[string]any{},
	}
}

func TestAccountFromServiceShallow_QuotaDimensionsFiveFamilies(t *testing.T) {
	t.Run("token harbor -> subscription exhausted + free remaining", func(t *testing.T) {
		fetched := dtoRecentRFC3339()
		account := dtoTokenHarborAccount()
		account.Extra[service.TokenHarborPassSnapshotExtraKey] = map[string]any{
			"has_pass":              true,
			"plan_exhausted":        true,
			"exhausted":             false,
			"spend_after_allowance": false,
			"fetched_at":            fetched,
		}

		got := AccountFromServiceShallow(account)
		require.NotNil(t, got)
		require.Len(t, got.QuotaDimensions, 2)

		subscription := got.QuotaDimensions[0]
		require.Equal(t, "subscription", subscription.Kind)
		require.Equal(t, "account", subscription.Scope)
		require.Equal(t, "", subscription.Target)
		require.Equal(t, "exhausted", subscription.Status)
		require.Equal(t, "unknown", subscription.Servable)
		require.Equal(t, service.TokenHarborPassSnapshotExtraKey, subscription.Source)
		require.Equal(t, fetched, subscription.ObservedAt)

		free := got.QuotaDimensions[1]
		require.Equal(t, "free", free.Kind)
		require.Equal(t, "remaining", free.Status)
		require.Equal(t, fetched, free.ObservedAt)
	})

	t.Run("kira -> free remaining (servable no) + paid exhausted", func(t *testing.T) {
		usedAt := dtoRecentRFC3339()
		account := dtoKiraAccount()
		account.Extra["kira_usage_snapshot"] = map[string]any{
			"used_percent": 10.0,
			"fetched_at":   usedAt,
		}
		account.Extra["other_balance"] = 0.0
		account.Extra["other_balance_updated_at"] = usedAt

		got := AccountFromServiceShallow(account)
		require.Len(t, got.QuotaDimensions, 2)

		free := got.QuotaDimensions[0]
		require.Equal(t, "free", free.Kind)
		require.Equal(t, "account", free.Scope)
		require.Equal(t, "remaining", free.Status)
		require.Equal(t, "no", free.Servable)
		require.Equal(t, "kira_usage_snapshot", free.Source)
		require.Equal(t, usedAt, free.ObservedAt)

		paid := got.QuotaDimensions[1]
		require.Equal(t, "paid", paid.Kind)
		require.Equal(t, "exhausted", paid.Status)
		require.Equal(t, "unknown", paid.Servable)
		require.Equal(t, "kira_vnd_balance", paid.Source)
		require.Equal(t, usedAt, paid.ObservedAt)
	})

	t.Run("coding plan -> single free window exhausted", func(t *testing.T) {
		updated := dtoRecentRFC3339()
		account := dtoKimiCodingAccount()
		account.Extra["kimi_5h_used_percent"] = 100.0
		account.Extra["kimi_usage_updated_at"] = updated

		got := AccountFromServiceShallow(account)
		require.Len(t, got.QuotaDimensions, 1)
		dim := got.QuotaDimensions[0]
		require.Equal(t, "free", dim.Kind)
		require.Equal(t, "account", dim.Scope)
		require.Equal(t, "exhausted", dim.Status)
		require.Equal(t, "unknown", dim.Servable)
		require.Equal(t, "kimi_usage_updated_at", dim.Source)
		require.Equal(t, updated, dim.ObservedAt)
	})

	t.Run("relay -> single paid exhausted", func(t *testing.T) {
		updated := dtoRecentRFC3339()
		account := dtoKimiRelayAccount()
		account.Extra["kimi_balance"] = 0.0
		account.Extra["kimi_balance_updated_at"] = updated

		got := AccountFromServiceShallow(account)
		require.Len(t, got.QuotaDimensions, 1)
		dim := got.QuotaDimensions[0]
		require.Equal(t, "paid", dim.Kind)
		require.Equal(t, "account", dim.Scope)
		require.Equal(t, "exhausted", dim.Status)
		require.Equal(t, "kimi_balance", dim.Source)
		require.Equal(t, updated, dim.ObservedAt)
	})

	t.Run("no snapshot -> empty list, field omitted in JSON", func(t *testing.T) {
		got := AccountFromServiceShallow(dtoNoSnapshotAccount())
		require.Empty(t, got.QuotaDimensions)

		raw, err := json.Marshal(got)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "quota_dimensions")
		require.NotContains(t, string(raw), "balance_low")
	})

	t.Run("nil account -> nil", func(t *testing.T) {
		require.Nil(t, AccountFromServiceShallow(nil))
	})
}

func TestAccountFromService_QuotaDimensionsFilled(t *testing.T) {
	usedAt := dtoRecentRFC3339()
	account := dtoKiraAccount()
	account.Extra["kira_usage_snapshot"] = map[string]any{
		"used_percent": 50.0,
		"fetched_at":   usedAt,
	}
	got := AccountFromService(account)
	require.NotNil(t, got)
	require.NotEmpty(t, got.QuotaDimensions)

	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"quota_dimensions"`)
	require.Contains(t, string(raw), `"observed_at"`)
	require.True(t, strings.Contains(string(raw), usedAt), "observed_at 应为 RFC3339 原样字符串")
}

func TestAccountFromServiceShallow_BalanceLowMarker(t *testing.T) {
	t.Run("marker true -> balance_low=true", func(t *testing.T) {
		account := dtoKimiRelayAccount()
		account.Extra["kimi_balance_low"] = true

		got := AccountFromServiceShallow(account)
		require.True(t, got.BalanceLow)

		raw, err := json.Marshal(got)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"balance_low":true`)
	})

	t.Run("marker absent -> false and omitted", func(t *testing.T) {
		got := AccountFromServiceShallow(dtoKimiRelayAccount())
		require.False(t, got.BalanceLow)

		raw, err := json.Marshal(got)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "balance_low")
	})

	t.Run("marker false (cleared) -> false and omitted", func(t *testing.T) {
		account := dtoKimiRelayAccount()
		account.Extra["kimi_balance_low"] = false

		got := AccountFromServiceShallow(account)
		require.False(t, got.BalanceLow)
	})

	t.Run("non-bool marker -> false", func(t *testing.T) {
		account := dtoKimiRelayAccount()
		account.Extra["kimi_balance_low"] = "true"

		got := AccountFromServiceShallow(account)
		require.False(t, got.BalanceLow)
	})

	t.Run("marker keyed by other platform not matched", func(t *testing.T) {
		account := dtoKimiRelayAccount()
		account.Extra["zhipu_balance_low"] = true

		got := AccountFromServiceShallow(account)
		require.False(t, got.BalanceLow)
	})
}
