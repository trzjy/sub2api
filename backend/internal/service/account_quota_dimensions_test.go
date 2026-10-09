//go:build unit

package service

// 账号额度维度解析 + F6 调度前置维度门单元测试（方案 §2.2/§3.1；C3 卡）。
//
// 覆盖：
//   - §3.1 锁定八组合（逐条对表断言 + 不变式）；
//   - 逐来源年龄门（R3-F1/R14）：过期来源只降自己那批维度为 unknown，不连累他源；
//   - 存在性与状态分离（R2-F5）：能力字段真 + 数值键缺失 ⇒ 存在 + unknown；平台不支持 ⇒ 不存在；
//   - 挂载点回归：isAccountSchedulableForQuota 放行/跳过两路效果；
//   - C6 联合：TH 钱包 fresh-positive / fresh-zero / stale 三态经 resolver → 门判定。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---- 固定时钟与夹具（独立于其他卡测试文件，避免耦合） ----

var qdBase = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func qdRecentRFC3339(now time.Time) string {
	return now.Add(-1 * time.Minute).UTC().Format(time.RFC3339)
}

func qdStaleRFC3339(now time.Time) string {
	return now.Add(-(cnQuotaBalanceFreshnessThresholdMinutes + 5) * time.Minute).UTC().Format(time.RFC3339)
}

func qdKiraAccount() *Account {
	return &Account{
		ID: 9101, Platform: PlatformOther, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"api_key": "kira-test", "base_url": "https://kiraai.vn/api/v1"},
		Extra:       map[string]any{},
	}
}

func qdTokenHarborAccount() *Account {
	return &Account{
		ID: 9102, Platform: PlatformOther, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"api_key": "sk-th", "base_url": "https://tokenharbor.ai/v1"},
		Extra:       map[string]any{},
	}
}

func qdKimiPayGAccount() *Account {
	return &Account{
		ID: 9103, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{"api_key": "sk-kimi"},
		Extra:       map[string]any{},
	}
}

func qdKimiCodingAccount() *Account {
	return &Account{
		ID: 9104, Platform: PlatformKimi, Type: AccountTypeAPIKey, Status: StatusActive,
		Credentials: map[string]any{
			"api_key":      "sk-kimi-coding",
			"base_url":     "https://api.kimi.com/coding",
			"account_mode": AccountModeCoding,
		},
		Extra: map[string]any{},
	}
}

func qdAnthropicOAuthAccount() *Account {
	return &Account{
		ID: 9105, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"access_token": "t"},
		Extra:       map[string]any{},
	}
}

func qdFindDim(dims []QuotaDimension, kind QuotaDimensionKind, source string) *QuotaDimension {
	for i := range dims {
		if dims[i].Kind == kind && dims[i].Source == source {
			return &dims[i]
		}
	}
	return nil
}

// ---- §3.1 八组合 ----

func TestQuotaDimensionGateEightCombinations(t *testing.T) {
	acct := QuotaDimensionScopeAccount

	t.Run("exhausted+unknown -> skip", func(t *testing.T) {
		dims := []QuotaDimension{
			{Scope: acct, Status: QuotaDimensionExhausted},
			{Scope: acct, Status: QuotaDimensionUnknown},
		}
		require.Equal(t, QuotaDimensionGateSkip, EvaluateAccountQuotaDimensionGate(dims))
	})

	t.Run("remaining+unknown -> allow", func(t *testing.T) {
		dims := []QuotaDimension{
			{Scope: acct, Status: QuotaDimensionRemaining},
			{Scope: acct, Status: QuotaDimensionUnknown},
		}
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims))
	})

	t.Run("all unknown -> allow (invariant: unknown never alone skips)", func(t *testing.T) {
		dims := []QuotaDimension{
			{Scope: acct, Status: QuotaDimensionUnknown},
			{Scope: acct, Status: QuotaDimensionUnknown},
		}
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims))
	})

	t.Run("empty list -> allow", func(t *testing.T) {
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(nil))
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate([]QuotaDimension{}))
	})

	t.Run("single model-scope exhausted -> allow (scope=model not in gate)", func(t *testing.T) {
		dims := []QuotaDimension{
			{Scope: acct, Status: QuotaDimensionRemaining},
			{Scope: QuotaDimensionScopeModel, Target: "glm-test", Kind: QuotaDimensionKindFree, Status: QuotaDimensionExhausted},
		}
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims))
	})

	t.Run("negative: account exhausted + model remaining -> still skip", func(t *testing.T) {
		dims := []QuotaDimension{
			{Scope: acct, Status: QuotaDimensionExhausted},
			{Scope: QuotaDimensionScopeModel, Target: "glm-test", Kind: QuotaDimensionKindFree, Status: QuotaDimensionRemaining},
		}
		require.Equal(t, QuotaDimensionGateSkip, EvaluateAccountQuotaDimensionGate(dims))
	})

	t.Run("kira free remaining + fresh VND<=0 -> skip (P1)", func(t *testing.T) {
		account := qdKiraAccount()
		account.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
			"used_percent": 10.0,
			"fetched_at":   qdBase.Format(time.RFC3339),
		}
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance)] = 0.0
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated)] = qdRecentRFC3339(qdBase)

		dims := ResolveAccountQuotaDimensions(account, qdBase)
		free := qdFindDim(dims, QuotaDimensionKindFree, kiraUsageSnapshotExtraKey)
		require.NotNil(t, free)
		require.True(t, free.HasRemaining(), "免费池有余量")
		require.True(t, free.ServableFalsified(), "新鲜 VND<=0 ⇒ 免费维度 servable=false")
		require.Equal(t, QuotaDimensionGateSkip, EvaluateAccountQuotaDimensionGate(dims))
	})

	t.Run("kira free valid + VND missing -> allow", func(t *testing.T) {
		account := qdKiraAccount()
		account.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
			"used_percent": 10.0,
			"fetched_at":   qdBase.Format(time.RFC3339),
		}
		// VND 键缺失 → paid 维度存在 + unknown（R2-F5）。
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		free := qdFindDim(dims, QuotaDimensionKindFree, kiraUsageSnapshotExtraKey)
		require.NotNil(t, free)
		require.True(t, free.HasRemaining())
		require.False(t, free.ServableFalsified(), "VND 缺失不得产生 servable=false")
		paid := qdFindDim(dims, QuotaDimensionKindPaid, quotaDimensionSourceKiraVND)
		require.NotNil(t, paid, "Kira 付费维度恒存在（键缺失=unknown）")
		require.Equal(t, QuotaDimensionUnknown, paid.Status)
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims))
	})
}

// ---- 逐来源年龄门 ----

func TestQuotaDimensionPerSourceAgeGate(t *testing.T) {
	t.Run("kira usage snapshot stale + exhausted -> free unknown (not confirmed)", func(t *testing.T) {
		account := qdKiraAccount()
		account.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
			"used_percent": 100.0, // 陈旧快照即使写着耗尽也不产生 confirmed
			"fetched_at":   qdStaleRFC3339(qdBase),
		}
		// B 来源（VND 余额）新鲜且为正：不因 A 来源陈旧被连累。
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance)] = 5000.0
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated)] = qdRecentRFC3339(qdBase)

		dims := ResolveAccountQuotaDimensions(account, qdBase)
		free := qdFindDim(dims, QuotaDimensionKindFree, kiraUsageSnapshotExtraKey)
		require.NotNil(t, free)
		require.False(t, free.Confirmed(), "过期来源维度不得产生 confirmed 值")
		require.Equal(t, QuotaDimensionUnknown, free.Status)
		paid := qdFindDim(dims, QuotaDimensionKindPaid, quotaDimensionSourceKiraVND)
		require.NotNil(t, paid)
		require.Equal(t, QuotaDimensionRemaining, paid.Status, "B 来源新鲜 confirmed 不被 A 来源过期连累")
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims))
	})

	t.Run("kira usage snapshot stale + remaining -> free unknown", func(t *testing.T) {
		account := qdKiraAccount()
		account.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
			"used_percent": 10.0,
			"fetched_at":   qdStaleRFC3339(qdBase),
		}
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		free := qdFindDim(dims, QuotaDimensionKindFree, kiraUsageSnapshotExtraKey)
		require.NotNil(t, free)
		require.False(t, free.Confirmed())
	})

	t.Run("kira usage stale + fresh zero VND -> free unknown, servable falsified by fresh VND", func(t *testing.T) {
		account := qdKiraAccount()
		account.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
			"used_percent": 10.0,
			"fetched_at":   qdStaleRFC3339(qdBase),
		}
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance)] = 0.0
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated)] = qdRecentRFC3339(qdBase)
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		free := qdFindDim(dims, QuotaDimensionKindFree, kiraUsageSnapshotExtraKey)
		require.NotNil(t, free)
		// VND 新鲜 ≤0 仍使免费维度 servable=false（付费耗尽是事实，与 A 源陈旧无冲突）。
		require.True(t, free.ServableFalsified())
		require.False(t, free.Confirmed(), "A 来源陈旧 ⇒ 免费维度无 confirmed，不得单独致跳过")
	})

	t.Run("relay balance stale -> paid unknown", func(t *testing.T) {
		account := qdKimiPayGAccount()
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance)] = 0.0
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated)] = qdStaleRFC3339(qdBase)
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		paid := qdFindDim(dims, QuotaDimensionKindPaid, cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance))
		require.NotNil(t, paid)
		require.Equal(t, QuotaDimensionUnknown, paid.Status)
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims))
	})

	t.Run("coding plan window stale -> free unknown", func(t *testing.T) {
		account := qdKimiCodingAccount()
		account.Extra[cnExtraKey(PlatformKimi, cnExtraSuffix5hUsed)] = 100.0
		account.Extra[cnExtraKey(PlatformKimi, cnExtraSuffixUsageUpdated)] = qdStaleRFC3339(qdBase)
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		free := qdFindDim(dims, QuotaDimensionKindFree, cnExtraKey(PlatformKimi, cnExtraSuffixUsageUpdated))
		require.NotNil(t, free)
		require.Equal(t, QuotaDimensionUnknown, free.Status)
	})

	t.Run("TH pass snapshot stale not dragging fresh wallet", func(t *testing.T) {
		account := qdTokenHarborAccount()
		account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
			"has_pass":              true,
			"plan_exhausted":        true,
			"spend_after_allowance": true,
			"fetched_at":            qdStaleRFC3339(qdBase),
		}
		account.Extra[TokenHarborWalletBalanceExtraKey] = 50.0
		account.Extra[TokenHarborWalletBalanceUpdatedAtExtraKey] = qdRecentRFC3339(qdBase)

		dims := ResolveAccountQuotaDimensions(account, qdBase)
		sub := qdFindDim(dims, QuotaDimensionKindSubscription, TokenHarborPassSnapshotExtraKey)
		require.NotNil(t, sub)
		require.False(t, sub.Confirmed(), "过期订阅快照不得产生 confirmed")
		paid := qdFindDim(dims, QuotaDimensionKindPaid, TokenHarborWalletBalanceExtraKey)
		require.NotNil(t, paid)
		require.Equal(t, QuotaDimensionRemaining, paid.Status)
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims))
	})
}

// ---- 存在性与状态分离（R2-F5） ----

func TestQuotaDimensionExistenceStatusSeparation(t *testing.T) {
	t.Run("TH capability true + missing wallet value -> exists + unknown", func(t *testing.T) {
		account := qdTokenHarborAccount()
		account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
			"has_pass":              false,
			"spend_after_allowance": true,
			"fetched_at":            qdBase.Format(time.RFC3339),
		}
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		paid := qdFindDim(dims, QuotaDimensionKindPaid, TokenHarborWalletBalanceExtraKey)
		require.NotNil(t, paid, "能力字段为真 ⇒ 维度存在")
		require.Equal(t, QuotaDimensionUnknown, paid.Status, "数值键缺失 ⇒ unknown")
	})

	t.Run("TH capability false -> paid dimension absent", func(t *testing.T) {
		account := qdTokenHarborAccount()
		account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
			"has_pass":              false,
			"spend_after_allowance": false,
			"fetched_at":            qdBase.Format(time.RFC3339),
		}
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		require.Nil(t, qdFindDim(dims, QuotaDimensionKindPaid, TokenHarborWalletBalanceExtraKey))
	})

	t.Run("unsupported platform family -> empty dimension list", func(t *testing.T) {
		require.Empty(t, ResolveAccountQuotaDimensions(qdAnthropicOAuthAccount(), qdBase))
		require.Empty(t, ResolveAccountQuotaDimensions(&Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{}}, qdBase))
	})

	t.Run("nil account / nil extra -> empty", func(t *testing.T) {
		require.Empty(t, ResolveAccountQuotaDimensions(nil, qdBase))
		require.Empty(t, ResolveAccountQuotaDimensions(&Account{Extra: nil}, qdBase))
	})
}

// ---- 挂载点回归 ----

func TestIsAccountSchedulableForQuotaDimensionsMount(t *testing.T) {
	svc := &GatewayService{}

	t.Run("allow: no quota snapshot account", func(t *testing.T) {
		require.True(t, svc.isAccountSchedulableForQuota(qdAnthropicOAuthAccount()))
	})

	t.Run("allow: kira free remaining + fresh positive VND", func(t *testing.T) {
		account := qdKiraAccount()
		account.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
			"used_percent": 10.0,
			"fetched_at":   time.Now().UTC().Format(time.RFC3339),
		}
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance)] = 1000.0
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated)] = time.Now().UTC().Format(time.RFC3339)
		require.True(t, svc.isAccountSchedulableForQuota(account))
	})

	t.Run("skip: kira free exhausted (fresh)", func(t *testing.T) {
		account := qdKiraAccount()
		account.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
			"used_percent": 100.0,
			"fetched_at":   time.Now().UTC().Format(time.RFC3339),
		}
		require.False(t, svc.isAccountSchedulableForQuota(account))
	})

	t.Run("skip: relay balance fresh zero", func(t *testing.T) {
		account := qdKimiPayGAccount()
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixBalance)] = 0.0
		account.Extra[cnExtraKey(account.Platform, cnBalanceExtraSuffixUpdated)] = time.Now().UTC().Format(time.RFC3339)
		require.False(t, svc.isAccountSchedulableForQuota(account))
	})
}

// ---- C6 联合：TH 钱包三态 ----

func TestQuotaDimensionGateC6TokenHarborWalletThreeStates(t *testing.T) {
	// 免费档已耗尽（scope=account confirmed exhausted），只考察钱包三态对门的影响。
	newAccount := func(wallet any, updatedAt any) *Account {
		account := qdTokenHarborAccount()
		account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
			"has_pass":              false,
			"exhausted":             true,
			"spend_after_allowance": true,
			"fetched_at":            qdBase.Format(time.RFC3339),
		}
		if wallet != nil {
			account.Extra[TokenHarborWalletBalanceExtraKey] = wallet
		}
		if updatedAt != nil {
			account.Extra[TokenHarborWalletBalanceUpdatedAtExtraKey] = updatedAt
		}
		return account
	}

	t.Run("fresh-positive -> allow (paid dimension has remaining)", func(t *testing.T) {
		account := newAccount(50.0, qdRecentRFC3339(qdBase))
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		paid := qdFindDim(dims, QuotaDimensionKindPaid, TokenHarborWalletBalanceExtraKey)
		require.NotNil(t, paid)
		require.Equal(t, QuotaDimensionRemaining, paid.Status)
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims))
	})

	t.Run("fresh-zero -> skip", func(t *testing.T) {
		account := newAccount(0.0, qdRecentRFC3339(qdBase))
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		paid := qdFindDim(dims, QuotaDimensionKindPaid, TokenHarborWalletBalanceExtraKey)
		require.NotNil(t, paid)
		require.Equal(t, QuotaDimensionExhausted, paid.Status)
		require.Equal(t, QuotaDimensionGateSkip, EvaluateAccountQuotaDimensionGate(dims))
	})

	t.Run("stale -> skip (unknown follows confirmed exhausted, never alone)", func(t *testing.T) {
		account := newAccount(50.0, qdStaleRFC3339(qdBase))
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		paid := qdFindDim(dims, QuotaDimensionKindPaid, TokenHarborWalletBalanceExtraKey)
		require.NotNil(t, paid)
		require.Equal(t, QuotaDimensionUnknown, paid.Status)
		require.Equal(t, QuotaDimensionGateSkip, EvaluateAccountQuotaDimensionGate(dims))
	})
}
