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

// qdFindModelDim 按 scope=model 且 target=模型ID 查找模型级投影维度。
func qdFindModelDim(dims []QuotaDimension, target string) *QuotaDimension {
	for i := range dims {
		if dims[i].Scope == QuotaDimensionScopeModel && dims[i].Target == target {
			return &dims[i]
		}
	}
	return nil
}

// qdTHModelLimitEntry 构造一条 model_rate_limits 条目（reason 复用同包权威前缀
// tokenHarborFreeTierReasonPrefix，不复制字面量）；resetAt 与 precise 控制是否命中
// ActiveTokenHarborFreeTierScopes 的未到期判定。
func qdTHModelLimitEntry(resetAt time.Time, precise bool) map[string]any {
	return map[string]any{
		"rate_limited_at":     resetAt.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		"rate_limit_reset_at": resetAt.UTC().Format(time.RFC3339),
		"reason":              tokenHarborFreeTierReasonPrefix + "_exhausted",
		"precise_reset":       precise,
	}
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

	// ServableNo 联动删除负例（用户 2026-10-10 §0.2 再裁定）：新鲜 VND≤0 不再把免费
	// 维度置 servable=false ⇒ 「免费 remaining + 付费 exhausted」经门**放行**
	// （该组合的冻结判据改由响应式 402 链与 Kira 确认探针 9 格判定表承担）。
	t.Run("kira free remaining + fresh VND<=0 -> allow (ServableNo linkage removed)", func(t *testing.T) {
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
		require.Equal(t, QuotaServableUnknown, free.Servable,
			"新鲜 VND<=0 不得再置 servable=false（联动已删除）")
		require.False(t, free.ServableFalsified(), "解析侧已无 servable=false 生产者")
		paid := qdFindDim(dims, QuotaDimensionKindPaid, quotaDimensionSourceKiraVND)
		require.NotNil(t, paid)
		require.True(t, paid.IsExhausted(), "付费维度仍如实为 exhausted（paid 事实不变）")
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims),
			"免费 remaining + 付费 exhausted ⇒ 放行（§3.1 门）")
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

	t.Run("kira usage stale + fresh zero VND -> free unknown, servable not falsified", func(t *testing.T) {
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
		// ServableNo 联动已删除：新鲜 VND≤0 也不再证伪免费维度（§0.2 再裁定）。
		require.Equal(t, QuotaServableUnknown, free.Servable)
		require.False(t, free.ServableFalsified())
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

// ---- th_usage_snapshot 用量维度删除 + 门数学中性（用户 2026-10-10 §0.2 再裁定） ----

// TestQuotaDimensionUsageDimensionRemoved 锁定 th_usage_snapshot 不再产出 free 维度
// （用量计数无额度分母 ⇒ 恒 unknown，纯展示噪声）。
func TestQuotaDimensionUsageDimensionRemoved(t *testing.T) {
	newTH := func() *Account {
		account := qdTokenHarborAccount()
		account.Extra[TokenHarborUsageSnapshotExtraKey] = map[string]any{
			"fetched_at": qdBase.Format(time.RFC3339),
			"windows": []any{map[string]any{
				"requests":   12.0,
				"tokens_in":  1000.0,
				"tokens_out": 200.0,
			}},
		}
		return account
	}

	t.Run("usage 快照存在 ⇒ 零 usage 维度产出", func(t *testing.T) {
		account := newTH()
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		for i := range dims {
			require.NotEqual(t, TokenHarborUsageSnapshotExtraKey, dims[i].Source,
				"th_usage_snapshot 不得再产出任何维度（无分母 ⇒ 恒 unknown ⇒ 噪声）")
		}
		require.Nil(t, qdFindDim(dims, QuotaDimensionKindFree, TokenHarborUsageSnapshotExtraKey))
	})

	t.Run("删除对门判定中性：有/无该维度逐账号等价", func(t *testing.T) {
		// 门数学中性证明：usage 维度恒为 unknown，而 §3.1 判定表只对 confirmed 计数
		// （unknown 既不加 confirmed 也不加 usableRemaining），故把该维度「加回」后
		// 判定结果必须逐账号完全等价。
		usageDim := QuotaDimension{
			Kind:     QuotaDimensionKindFree,
			Scope:    QuotaDimensionScopeAccount,
			Status:   QuotaDimensionUnknown,
			Servable: QuotaServableUnknown,
			Source:   TokenHarborUsageSnapshotExtraKey,
		}
		cases := []struct {
			name string
			acct func() *Account
		}{
			{"TH：用量快照存在（免费档未耗尽）", func() *Account {
				a := newTH()
				a.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
					"has_pass": false, "exhausted": false, "spend_after_allowance": false,
					"fetched_at": qdBase.Format(time.RFC3339),
				}
				return a
			}},
			{"TH：用量快照存在 + 免费档耗尽（付费维度 remaining）", func() *Account {
				a := newTH()
				a.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
					"has_pass": false, "exhausted": true, "spend_after_allowance": true,
					"fetched_at": qdBase.Format(time.RFC3339),
				}
				a.Extra[TokenHarborWalletBalanceExtraKey] = 50.0
				a.Extra[TokenHarborWalletBalanceUpdatedAtExtraKey] = qdRecentRFC3339(qdBase)
				return a
			}},
			{"TH：用量快照存在 + 全维度 unknown（快照过期）", func() *Account {
				a := newTH()
				a.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
					"has_pass": false, "exhausted": true, "spend_after_allowance": true,
					"fetched_at": qdStaleRFC3339(qdBase),
				}
				a.Extra[TokenHarborWalletBalanceExtraKey] = 50.0
				a.Extra[TokenHarborWalletBalanceUpdatedAtExtraKey] = qdStaleRFC3339(qdBase)
				return a
			}},
			{"TH：用量快照存在 + 付费维度 fresh-zero", func() *Account {
				a := newTH()
				a.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
					"has_pass": false, "exhausted": true, "spend_after_allowance": true,
					"fetched_at": qdBase.Format(time.RFC3339),
				}
				a.Extra[TokenHarborWalletBalanceExtraKey] = 0.0
				a.Extra[TokenHarborWalletBalanceUpdatedAtExtraKey] = qdRecentRFC3339(qdBase)
				return a
			}},
			{"Kira：用量维度不属于 Kira（控制组）", func() *Account {
				a := qdKiraAccount()
				a.Extra[kiraUsageSnapshotExtraKey] = map[string]any{
					"used_percent": 100.0, "fetched_at": qdBase.Format(time.RFC3339),
				}
				a.Extra[TokenHarborUsageSnapshotExtraKey] = map[string]any{"fetched_at": qdBase.Format(time.RFC3339)}
				return a
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				account := tc.acct()
				without := EvaluateAccountQuotaDimensionGate(ResolveAccountQuotaDimensions(account, qdBase))
				with := EvaluateAccountQuotaDimensionGate(
					append(ResolveAccountQuotaDimensions(account, qdBase), usageDim))
				require.Equal(t, without, with,
					"删除必须是门数学中性的：加回 usage 维度后判定结果必须等价")
			})
		}
	})
}

// ---- C3-r2：模型级投影 + 门隔离序列化（方案 §2.1 硬要求） ----

// TestQuotaDimensionModelScopeProjection 锁定解析函数产出 scope=model 投影的契约。
func TestQuotaDimensionModelScopeProjection(t *testing.T) {
	const modelID = "glm-test"

	t.Run("命中条目产出（target=模型ID、status=exhausted）", func(t *testing.T) {
		account := qdTokenHarborAccount()
		account.Extra[modelRateLimitsKey] = map[string]any{
			modelID: qdTHModelLimitEntry(qdBase.Add(time.Hour), true), // 未到期 → 命中
		}
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		md := qdFindModelDim(dims, modelID)
		require.NotNil(t, md, "命中前缀且未到期的条目应产出模型级维度")
		require.Equal(t, QuotaDimensionScopeModel, md.Scope)
		require.Equal(t, QuotaDimensionKindFree, md.Kind)
		require.Equal(t, modelID, md.Target)
		require.Equal(t, QuotaDimensionExhausted, md.Status, "模型级免费档命中即 exhausted（confirmed）")
		require.Equal(t, QuotaServableUnknown, md.Servable)
		require.Equal(t, modelRateLimitsKey, md.Source, "Source=model_rate_limits 键名")
		require.True(t, md.ObservedAt.IsZero(), "权威导出未暴露时间戳 ⇒ 零值")
	})

	t.Run("到期条目不产出", func(t *testing.T) {
		account := qdTokenHarborAccount()
		account.Extra[modelRateLimitsKey] = map[string]any{
			modelID: qdTHModelLimitEntry(qdBase.Add(-time.Hour), true), // 已到期 → 剔除
		}
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		require.Nil(t, qdFindModelDim(dims, modelID), "到期条目不产出任何维度")
	})

	t.Run("无命中条目 → 零模型级投影", func(t *testing.T) {
		account := qdTokenHarborAccount()
		account.Extra[modelRateLimitsKey] = map[string]any{
			modelID: qdTHModelLimitEntry(qdBase.Add(time.Hour), true),
		}
		// 前缀不符 → ActiveTokenHarborFreeTierScopes 不返回该 scope。
		entry := account.Extra[modelRateLimitsKey].(map[string]any)[modelID].(map[string]any)
		entry["reason"] = "some_other_reason"
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		require.Nil(t, qdFindModelDim(dims, modelID))
	})

	t.Run("非 TH 账号不产出", func(t *testing.T) {
		account := qdKiraAccount() // 非 TH 账号
		account.Extra[modelRateLimitsKey] = map[string]any{
			modelID: qdTHModelLimitEntry(qdBase.Add(time.Hour), true),
		}
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		require.Nil(t, qdFindModelDim(dims, modelID), "非 TH 账号零模型级投影")
	})

	t.Run("TH 账号无 model_rate_limits → 零模型级投影（现状行为）", func(t *testing.T) {
		dims := ResolveAccountQuotaDimensions(qdTokenHarborAccount(), qdBase)
		for i := range dims {
			require.NotEqual(t, QuotaDimensionScopeModel, dims[i].Scope)
		}
	})
}

// TestQuotaDimensionGateModelScopeIsolationSerialization 锁定"含 exhausted 模型级维度的
// 账号 → 门判定仍由账号级维度单独决定"（方案 §2.1 隔离序列化硬要求）。
func TestQuotaDimensionGateModelScopeIsolationSerialization(t *testing.T) {
	const modelID = "glm-test"

	t.Run("账号级全 unknown + 模型级 exhausted -> 放行", func(t *testing.T) {
		account := qdTokenHarborAccount()
		account.Extra[modelRateLimitsKey] = map[string]any{
			modelID: qdTHModelLimitEntry(qdBase.Add(time.Hour), true), // 模型级 exhausted
		}
		// 账号级快照过期 ⇒ 免费维度 unknown（无 confirmed）。
		account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
			"has_pass":              false,
			"spend_after_allowance": false,
			"fetched_at":            qdStaleRFC3339(qdBase),
		}
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		require.NotNil(t, qdFindModelDim(dims, modelID), "模型级 exhausted 维度应存在")
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims),
			"账号级全 unknown ⇒ 放行，模型级 exhausted 不改变结果")
	})

	t.Run("账号级 remaining + 模型级 exhausted -> 放行", func(t *testing.T) {
		account := qdTokenHarborAccount()
		account.Extra[modelRateLimitsKey] = map[string]any{
			modelID: qdTHModelLimitEntry(qdBase.Add(time.Hour), true), // 模型级 exhausted
		}
		// 账号级免费维度新鲜且有剩余（exhausted=false）。
		account.Extra[TokenHarborPassSnapshotExtraKey] = map[string]any{
			"has_pass":              false,
			"exhausted":             false,
			"spend_after_allowance": false,
			"fetched_at":            qdBase.Format(time.RFC3339),
		}
		dims := ResolveAccountQuotaDimensions(account, qdBase)
		require.NotNil(t, qdFindModelDim(dims, modelID))
		require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(dims),
			"账号级有剩余 ⇒ 放行，模型级 exhausted 不干扰")
	})

	t.Run("账号级 exhausted + 模型级 remaining -> 跳过（模型级 remaining 不救账号级）", func(t *testing.T) {
		// 解析函数只产出模型级 exhausted 投影，故模型级 remaining 以构造维度呈现隔离语义。
		dims := []QuotaDimension{
			{Scope: QuotaDimensionScopeAccount, Status: QuotaDimensionExhausted},
			{Scope: QuotaDimensionScopeModel, Target: modelID, Kind: QuotaDimensionKindFree, Status: QuotaDimensionRemaining},
		}
		require.Equal(t, QuotaDimensionGateSkip, EvaluateAccountQuotaDimensionGate(dims),
			"账号级 exhausted 单独决定跳过；模型级 remaining 不救账号级")
	})
}
