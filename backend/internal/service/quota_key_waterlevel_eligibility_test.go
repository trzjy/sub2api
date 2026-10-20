//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// WaterLevel 的账号夹具必须显式声明能力；空 mapping 在调度中不可服务。
func withCursorWaterModels() func(*ent.AccountCreate) {
	return func(c *ent.AccountCreate) {
		mapping := make(map[string]any)
		for _, model := range CursorStaticEntryModels() {
			mapping[model] = model
		}
		c.SetCredentials(map[string]any{"model_mapping": mapping})
	}
}

// wiredWaterAllCursor 构造依赖齐备（gateway/channel/resolver 全注入）的水位判定器，
// resolver 对任意 (group,model) 都判定为 Cursor 可服务（等价"依赖齐备且可路由"）。
// countsTowardWaterLevel 不消费 w.client，故此处无需真实 ent client。
func wiredWaterAllCursor() *quotaKeyModelWater {
	return &quotaKeyModelWater{
		gateway:  &GatewayService{},
		channel:  stubChannelServiceNoChannel(),
		resolver: stubResolverAllCursor(),
	}
}

func TestCursorWaterLevelEligibilityMatchesKnownSchedulingGates(t *testing.T) {
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	newAccount := func() *ent.Account {
		return &ent.Account{
			ID: 1, Platform: PlatformCursor, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: true, AutoPauseOnExpired: true,
			Credentials: map[string]any{"model_mapping": map[string]any{"grok-4.7": "grok-4.7"}},
			Edges: ent.AccountEdges{
				QuotaKey: &ent.QuotaKey{ID: 1, Status: domain.QuotaKeyStatusActive},
				Groups: []*ent.Group{{ID: 1, Platform: PlatformCursor, Status: StatusActive,
					ModelAllowlist: domain.GroupModelAllowlist{Enabled: true, Models: []string{"grok-4.7"}}}},
			},
		}
	}
	tests := []struct {
		name string
		edit func(*ent.Account)
		want bool
	}{
		{"active", func(a *ent.Account) {}, true},
		{"bare account", func(a *ent.Account) { a.Edges.Groups = nil }, false},
		{"empty mapping", func(a *ent.Account) { a.Credentials = nil }, false},
		{"disjoint mapping", func(a *ent.Account) {
			a.Credentials["model_mapping"] = map[string]any{"gpt-6-sol": "gpt-6-sol"}
		}, false},
		{"dynamic concrete mapping", func(a *ent.Account) {
			a.Credentials["model_mapping"] = map[string]any{"catalog-added-id": "wire-id"}
			a.Edges.Groups[0].ModelAllowlist.Models = []string{"catalog-added-id"}
		}, true},
		{"wildcard mapping", func(a *ent.Account) {
			a.Credentials["model_mapping"] = map[string]any{"grok-*": "grok-4.7"}
		}, true},
		{"wildcard group allowlist", func(a *ent.Account) {
			a.Edges.Groups[0].ModelAllowlist.Models = []string{"grok-*"}
		}, true},
		{"allow all group", func(a *ent.Account) { a.Edges.Groups[0].ModelAllowlist.Enabled = false }, true},
		{"anthropic bridge", func(a *ent.Account) { a.Edges.Groups[0].Platform = PlatformAnthropic }, true},
		{"gemini bridge", func(a *ent.Account) { a.Edges.Groups[0].Platform = PlatformGemini }, true},
		{"preserve composite boundary", func(a *ent.Account) { a.Edges.Groups[0].Platform = PlatformComposite }, true},
		{"openai platform mismatch", func(a *ent.Account) { a.Edges.Groups[0].Platform = PlatformOpenAI }, false},
		{"inactive group", func(a *ent.Account) { a.Edges.Groups[0].Status = "disabled" }, false},
		{"deleted group", func(a *ent.Account) { a.Edges.Groups[0].DeletedAt = &past }, false},
		{"expired account", func(a *ent.Account) { a.ExpiresAt = &past }, false},
		{"expiry auto pause disabled", func(a *ent.Account) { a.ExpiresAt = &past; a.AutoPauseOnExpired = false }, true},
		{"future account expiry", func(a *ent.Account) { a.ExpiresAt = &future }, true},
		{"disabled account", func(a *ent.Account) { a.Schedulable = false }, false},
		{"inactive account", func(a *ent.Account) { a.Status = "error" }, false},
		{"deleted account", func(a *ent.Account) { a.DeletedAt = &past }, false},
		{"other platform account", func(a *ent.Account) { a.Platform = PlatformOpenAI }, false},
		{"temporary window", func(a *ent.Account) { a.TempUnschedulableUntil = &future }, false},
		{"past temporary window", func(a *ent.Account) { a.TempUnschedulableUntil = &past }, true},
		{"overload window", func(a *ent.Account) { a.OverloadUntil = &future }, false},
		{"past overload window", func(a *ent.Account) { a.OverloadUntil = &past }, true},
		{"rate limit window", func(a *ent.Account) { a.RateLimitResetAt = &future }, false},
		{"past rate limit window", func(a *ent.Account) { a.RateLimitResetAt = &past }, true},
		{"only supported model is rate limited", func(a *ent.Account) {
			a.Extra = map[string]any{modelRateLimitsKey: map[string]any{"grok-4.7": map[string]any{"rate_limit_reset_at": future.Format(time.RFC3339)}}}
		}, false},
		{"supported model limit elapsed", func(a *ent.Account) {
			a.Extra = map[string]any{modelRateLimitsKey: map[string]any{"grok-4.7": map[string]any{"rate_limit_reset_at": past.Format(time.RFC3339)}}}
		}, true},
		{"another supported model remains available", func(a *ent.Account) {
			a.Credentials["model_mapping"].(map[string]any)["grok-4.6"] = "grok-4.6"
			a.Edges.Groups[0].ModelAllowlist.Models = []string{"grok-*"}
			a.Extra = map[string]any{modelRateLimitsKey: map[string]any{"grok-4.7": map[string]any{"rate_limit_reset_at": future.Format(time.RFC3339)}}}
		}, true},
		{"expired key status", func(a *ent.Account) { a.Edges.QuotaKey.Status = domain.QuotaKeyStatusExpired }, false},
		{"expired key before sweep", func(a *ent.Account) { a.Edges.QuotaKey.ExpiresAt = &past }, false},
		{"refresh exhausted marker preserves supply", func(a *ent.Account) { a.Edges.QuotaKey.Exhausted = true }, true},
		{"key disabled is not expired", func(a *ent.Account) { a.Edges.QuotaKey.Status = "disabled" }, true},
		{"concurrency use does not remove supply", func(a *ent.Account) { a.Concurrency = 3; a.LastUsedAt = &past }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := newAccount()
			tt.edit(row)
			got, err := wiredWaterAllCursor().countsTowardWaterLevel(context.Background(), row)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
	t.Run("missing key fails closed", func(t *testing.T) {
		row := newAccount()
		row.Edges.QuotaKey = nil
		got, err := wiredWaterAllCursor().countsTowardWaterLevel(context.Background(), row)
		require.ErrorContains(t, err, "quota key unavailable")
		require.False(t, got)
	})
}

func TestQuotaKeyModelWaterReloadsEligibilityAndCountsOnce(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	// §18.4 第11条：水位生产链路依赖齐备（gateway/channel/resolver 全注入）。此处注入
	// stub 依赖以等价生产 wire 装配，resolver 对任意模型判定为 Cursor 可服务。
	water := newRoutedWater(t, client, stubResolverAllCursor(), stubChannelServiceNoChannel(), &GatewayService{})
	base, threshold, err := water.WaterLevel(ctx, "grok-4.7")
	require.NoError(t, err)
	require.Equal(t, 10, threshold)
	key := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	g1 := mustCreateCursorGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{"grok-4.7"}})
	g2 := mustCreateGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{"grok-4.7"}})
	row := mustCreateCursorAccount(t, client, key.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, row.ID, g1.ID)
	mustLinkAccountToGroup(t, client, row.ID, g2.ID)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.AccountID(row.ID)).Exec(ctx)
		_, _ = client.Group.Delete().Where(entgroup.IDIn(g1.ID, g2.ID)).Exec(ctx)
		_ = client.Account.UpdateOneID(row.ID).SetDeletedAt(time.Now()).Exec(ctx)
	})
	check := func(want int) {
		t.Helper()
		got, _, err := water.WaterLevel(ctx, "gpt-6-sol")
		require.NoError(t, err)
		require.Equal(t, base+want, got)
	}
	check(1)
	require.NoError(t, client.QuotaKey.UpdateOneID(key.ID).SetExhausted(true).Exec(ctx))
	check(1)
	require.NoError(t, client.QuotaKey.UpdateOneID(key.ID).SetExpiresAt(time.Now().Add(-time.Hour)).Exec(ctx))
	check(0)
	require.NoError(t, client.QuotaKey.UpdateOneID(key.ID).ClearExpiresAt().Exec(ctx))
	check(1)
	require.NoError(t, client.Account.UpdateOneID(row.ID).SetAutoPauseOnExpired(true).SetExpiresAt(time.Now().Add(-time.Hour)).Exec(ctx))
	check(0)
	require.NoError(t, client.Account.UpdateOneID(row.ID).SetAutoPauseOnExpired(false).Exec(ctx))
	check(1)
	require.NoError(t, client.Account.UpdateOneID(row.ID).SetCredentials(map[string]any{}).Exec(ctx))
	check(0)
}
