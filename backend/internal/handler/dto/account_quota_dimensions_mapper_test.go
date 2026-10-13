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
		require.Equal(t, "unknown", free.Servable) // §0.2 再裁定：Kira 免费维度无 servable 生产者，恒 unknown
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

func TestAccountFromServiceShallow_BalanceProbeCapableProjection(t *testing.T) {
	// 后端选路事实源 RelayProbeTargetURL 的布尔投影（方案乙 / BAL-GATE-D5）：
	// 前端是否显示余额探测单元格的唯一能力源。三态：
	//   1) probe enabled + 显式 URL → true；
	//   2) enabled 但无 URL 且官方 base_url → false（配错账号整格不出现）；
	//   3) 无 probe 且 kimi/deepseek 平台 → false（官方端点账号显示走前端平台规则）。

	t.Run("probe enabled + explicit URL -> true", func(t *testing.T) {
		account := dtoKimiRelayAccount()
		account.Credentials["balance_probe"] = map[string]any{
			"enabled": true,
			"url":     "https://probe.example.com/api/usage",
		}

		got := AccountFromServiceShallow(account)
		require.True(t, got.BalanceProbeCapable)

		raw, err := json.Marshal(got)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"balance_probe_capable":true`)
	})

	t.Run("enabled but no URL + official base_url -> false", func(t *testing.T) {
		account := dtoKimiRelayAccount()
		account.Credentials["base_url"] = "https://api.moonshot.cn/v1"
		account.Credentials["balance_probe"] = map[string]any{
			"enabled": true,
			// 无显式 url：按 base_url + /v1/usage 推导，但官方域名账号不适用 → 不可探测。
		}

		got := AccountFromServiceShallow(account)
		require.False(t, got.BalanceProbeCapable)

		raw, err := json.Marshal(got)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "balance_probe_capable")
	})

	t.Run("no probe + kimi platform -> false", func(t *testing.T) {
		account := dtoKimiRelayAccount() // 无 balance_probe 凭据

		got := AccountFromServiceShallow(account)
		require.False(t, got.BalanceProbeCapable)
	})

	t.Run("no probe + deepseek platform -> false", func(t *testing.T) {
		account := &service.Account{
			ID:          9301,
			Name:       "deepseek-payg",
			Platform:   service.PlatformDeepseek,
			Type:       service.AccountTypeAPIKey,
			Status:     service.StatusActive,
			Credentials: map[string]any{"api_key": "sk-ds"},
			Extra:       map[string]any{},
		}

		got := AccountFromServiceShallow(account)
		require.False(t, got.BalanceProbeCapable)
	})
}

func TestAccountListItemFromAccount_BalanceProbeCapableProjection(t *testing.T) {
	// lite 列表 DTO 透传完整 DTO 已算出的 BalanceProbeCapable（BAL-GATE-D6）：
	// 列表页实际走 AccountListItemFromAccount（mappers.go:543），此前未透传该字段，
	// 导致真实列表接口永远发不出 balance_probe_capable:true，前端能力显示断链。
	// 这里仅做值透传，禁止再次调用 RelayProbeTargetURL 或重推导。

	t.Run("probe enabled + explicit URL -> lite JSON contains true", func(t *testing.T) {
		svc := dtoKimiRelayAccount()
		svc.Credentials["balance_probe"] = map[string]any{
			"enabled": true,
			"url":     "https://probe.example.com/api/usage",
		}
		full := AccountFromServiceShallow(svc)
		require.True(t, full.BalanceProbeCapable)

		item := AccountListItemFromAccount(full)
		require.NotNil(t, item)
		require.True(t, item.BalanceProbeCapable)

		raw, err := json.Marshal(item)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"balance_probe_capable":true`)
	})

	t.Run("capability false -> lite JSON omits field", func(t *testing.T) {
		svc := dtoKimiRelayAccount() // 无 balance_probe 凭据 → false
		full := AccountFromServiceShallow(svc)
		require.False(t, full.BalanceProbeCapable)

		item := AccountListItemFromAccount(full)
		require.NotNil(t, item)
		require.False(t, item.BalanceProbeCapable)

		raw, err := json.Marshal(item)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "balance_probe_capable")
	})

	t.Run("consistent with full DTO projection on same account", func(t *testing.T) {
		// 同一账号：完整 DTO 与 lite DTO 投影结果一致（皆 true）。
		svc := dtoKimiRelayAccount()
		svc.Credentials["balance_probe"] = map[string]any{
			"enabled": true,
			"url":     "https://probe.example.com/api/usage",
		}
		full := AccountFromServiceShallow(svc)
		require.True(t, full.BalanceProbeCapable)

		item := AccountListItemFromAccount(full)
		require.Equal(t, full.BalanceProbeCapable, item.BalanceProbeCapable)
		require.True(t, item.BalanceProbeCapable)

		fullRaw, err := json.Marshal(full)
		require.NoError(t, err)
		itemRaw, err := json.Marshal(item)
		require.NoError(t, err)
		require.Contains(t, string(fullRaw), `"balance_probe_capable":true`)
		require.Contains(t, string(itemRaw), `"balance_probe_capable":true`)

		// 反例：无能力账号两 mapper 皆缺省（一致）。
		svc2 := dtoKimiRelayAccount()
		full2 := AccountFromServiceShallow(svc2)
		item2 := AccountListItemFromAccount(full2)
		require.Equal(t, full2.BalanceProbeCapable, item2.BalanceProbeCapable)
		require.False(t, item2.BalanceProbeCapable)

		full2Raw, err := json.Marshal(full2)
		require.NoError(t, err)
		item2Raw, err := json.Marshal(item2)
		require.NoError(t, err)
		require.NotContains(t, string(full2Raw), "balance_probe_capable")
		require.NotContains(t, string(item2Raw), "balance_probe_capable")
	})
}
