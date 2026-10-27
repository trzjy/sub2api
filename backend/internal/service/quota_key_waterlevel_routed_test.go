//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// §18.4 W 路由/端点/渠道/额度门 —— 带真实依赖的水位（生产 wire 装配等价物，单测版）。
// ---------------------------------------------------------------------------

func newRoutedWater(t *testing.T, client *ent.Client, resolver *CompositeRouteResolver, channel *ChannelService, gateway *GatewayService) *quotaKeyModelWater {
	t.Helper()
	w := &quotaKeyModelWater{client: client, gateway: gateway, channel: channel, resolver: resolver}
	w.SetRoutingDependencies(gateway, channel, resolver)
	return w
}

// stubChannelServiceNoChannel 返回无渠道关联的 ChannelService（lookupGroupChannel → nil,nil）。
func stubChannelServiceNoChannel() *ChannelService {
	return newTestChannelService(&mockChannelRepository{
		listAllFn: func(ctx context.Context) ([]Channel, error) {
			return []Channel{}, nil
		},
		getGroupPlatformsFn: func(ctx context.Context, ids []int64) (map[int64]string, error) {
			return map[int64]string{}, nil
		},
	})
}

// stubChannelServiceError 返回加载渠道缓存即报错的 ChannelService（lookupGroupChannel → err）。
func stubChannelServiceError() *ChannelService {
	return newTestChannelService(&mockChannelRepository{
		listAllFn: func(ctx context.Context) ([]Channel, error) {
			return nil, errors.New("channel cache load failed")
		},
	})
}

// stubChannelServiceRestricting 返回限制模型的渠道（RestrictModels=true，且无模型定价）。
// 任意模型均不在定价列表 → checkRestricted 返回 true（排除）。
func stubChannelServiceRestricting(groupID int64) *ChannelService {
	return newTestChannelService(&mockChannelRepository{
		listAllFn: func(ctx context.Context) ([]Channel, error) {
			return []Channel{{
				ID:                 1,
				Name:               "restricted",
				Status:             StatusActive,
				RestrictModels:     true,
				GroupIDs:           []int64{groupID},
				BillingModelSource: BillingModelSourceUpstream,
			}}, nil
		},
		getGroupPlatformsFn: func(ctx context.Context, ids []int64) (map[int64]string, error) {
			return map[int64]string{}, nil
		},
	})
}

func stubChannelServiceOpen(groupID int64) *ChannelService {
	return newTestChannelService(&mockChannelRepository{
		listAllFn: func(ctx context.Context) ([]Channel, error) {
			return []Channel{{
				ID:                 1,
				Name:               "open",
				Status:             StatusActive,
				RestrictModels:     false,
				GroupIDs:           []int64{groupID},
				BillingModelSource: BillingModelSourceUpstream,
			}}, nil
		},
		getGroupPlatformsFn: func(ctx context.Context, ids []int64) (map[int64]string, error) {
			return map[int64]string{}, nil
		},
	})
}

// installRoutedWaterSourceForTest 为单个 key 一次装齐 A04-G 来源门依赖（状态 provider +
// 新鲜额度快照 provider）。
//
// countsTowardWaterLevel 在路由/渠道门之前先过 cursorSupplySourceAvailabilityVetoReasonString；
// 该门在「状态 provider 或快照 provider 任一未装配」时返回 error，被折叠成
// quota_key_state_unavailable 直接不计入水位。本文件的用例检验对象是路由/渠道/依赖缺失，
// 不是来源门，因此必须装齐来源读取器，否则断言会被来源门否决而不是被用例本来的门否决。
func installRoutedWaterSourceForTest(t *testing.T, keyID int64) {
	t.Helper()
	now := time.Now()
	prevNow := cursorSupplySourceUsageNow
	cursorSupplySourceUsageNow = func() time.Time { return now }
	t.Cleanup(func() { cursorSupplySourceUsageNow = prevNow })
	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{keyID: domain.QuotaKeyStatusActive},
		snapshots: map[int64][]byte{keyID: []byte(fmt.Sprintf(
			`{"total":400,"used":86,"status_code":200,"checked_at":%q,"error_code":""}`,
			now.Add(-time.Minute).Format(time.RFC3339),
		))},
	})
}

// stubResolverAllCursor 返回对任意 (group, model) 都把所有权判定为 Cursor 的 resolver，
// 使水位 routed 口径对所有可调度模型放行（等价"依赖齐备且全部可路由到 Cursor"，测试用）。
func stubResolverAllCursor() *CompositeRouteResolver {
	r := NewCompositeRouteResolver(compositeRouteRepoStub{routes: nil})
	r.SetModelOwnershipResolver(func(ctx context.Context, groupID int64, model string) (CompositeModelOwnership, error) {
		return CompositeModelOwnership{Matched: true, TargetPlatform: PlatformCursor}, nil
	})
	return r
}

// ---------------------------------------------------------------------------
// 白盒：候选模型集必须保留中间字面 *（§18.4.1）。
// ---------------------------------------------------------------------------

func TestCursorCandidateModelsKeepsMidStringAsterisk(t *testing.T) {
	candidate := &Account{
		Platform: PlatformCursor, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{
			"gpt-*-high":    "gpt-5",    // 中间字面 *，必须保留
			"claude-opus-*": "claude-x", // 后缀 *，通配键，应跳过
			"grok-4.7":      "grok-4.7", // 精确键，保留
		}},
	}
	got := (&quotaKeyModelWater{}).cursorCandidateModels(candidate)
	joined := strings.Join(got, ",")
	require.Contains(t, joined, "gpt-*-high", "中间字面 * 不得被 HasSuffix 误丢")
	require.Contains(t, joined, "grok-4.7")
	require.NotContains(t, joined, "claude-opus-*", "后缀 * 通配键不应进精确候选集")
}

// ---------------------------------------------------------------------------
// 白盒：ResolveGeminiWithDefault 未匹配仅补 Gemini target，不伪造 Matched/ownership。
// ---------------------------------------------------------------------------

func TestResolveGeminiWithDefaultBehaviour(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{
		{ID: 1, GroupID: 1, PublicModel: "m1", MatchType: CompositeRouteMatchExact, Enabled: true, Endpoint: CompositeRouteEndpointGemini, TargetPlatform: PlatformGemini},
	}})
	ctx := context.Background()

	dec, def, err := resolver.ResolveGeminiWithDefault(ctx, 1, "m1")
	require.NoError(t, err)
	require.False(t, def, "已命中不应默认")
	require.True(t, dec.Matched)

	dec, def, err = resolver.ResolveGeminiWithDefault(ctx, 1, "unknown-model-x")
	require.NoError(t, err)
	require.True(t, def, "未匹配应默认补 Gemini")
	require.False(t, dec.Matched, "默认补全不得伪造 Matched")
	require.Equal(t, PlatformGemini, dec.TargetPlatform)

	// Resolve 报错时不应补全，直接传播错误。
	errResolver := NewCompositeRouteResolver(failingRouteRepo{})
	_, _, err = errResolver.ResolveGeminiWithDefault(ctx, 1, "m1")
	require.Error(t, err)
}

type failingRouteRepo struct{}

func (failingRouteRepo) ListByGroup(ctx context.Context, groupID int64, includeDisabled bool) ([]CompositeModelRoute, error) {
	return nil, errors.New("route repo boom")
}
func (failingRouteRepo) Create(ctx context.Context, route *CompositeModelRoute) error { return nil }
func (failingRouteRepo) Update(ctx context.Context, route *CompositeModelRoute) error { return nil }
func (failingRouteRepo) Delete(ctx context.Context, id int64) error                   { return nil }
func (failingRouteRepo) DeleteByGroup(ctx context.Context, groupID int64) error       { return nil }

// ---------------------------------------------------------------------------
// 白盒：ResolveOpenAIMessagesDispatchMappedModel 纯计算（§18.4 W 第8条）。
// ---------------------------------------------------------------------------

func TestResolveOpenAIMessagesDispatchMappedModelBehaviour(t *testing.T) {
	compGrok := &Group{Platform: PlatformComposite}
	require.Equal(t, "", ResolveOpenAIMessagesDispatchMappedModel(compGrok, PlatformGrok, "claude-opus-4"),
		"composite + grok 目标不应改写")
	require.Equal(t, "", ResolveOpenAIMessagesDispatchMappedModel(compGrok, PlatformCodeBuddy, "claude-opus-4"),
		"composite + codebuddy 目标不应改写")

	openaiG := &Group{Platform: PlatformOpenAI}
	require.Equal(t, defaultOpenAIMessagesDispatchOpusMappedModel,
		ResolveOpenAIMessagesDispatchMappedModel(openaiG, PlatformOpenAI, "claude-opus-4"),
		"openai 组 claude-opus 应改写为默认 opus 映射")
	require.Equal(t, "", ResolveOpenAIMessagesDispatchMappedModel(openaiG, PlatformOpenAI, "gpt-4"),
		"非 claude 家族不应改写")

	require.Equal(t, "", ResolveOpenAIMessagesDispatchMappedModel(nil, PlatformOpenAI, "x"))
}

// ---------------------------------------------------------------------------
// 白盒：账号额度维度门 Skip 判定（§18.4.9 不把 key 余额复制成账号额度）。
// ---------------------------------------------------------------------------

func TestEvaluateAccountQuotaDimensionGateSkipWhenConfirmedNoRemaining(t *testing.T) {
	exhausted := []QuotaDimension{{
		Kind:     QuotaDimensionKindSubscription,
		Scope:    QuotaDimensionScopeAccount,
		Status:   QuotaDimensionExhausted,
		Servable: QuotaServableUnknown,
	}}
	require.Equal(t, QuotaDimensionGateSkip, EvaluateAccountQuotaDimensionGate(exhausted))

	// 空列表/全 unknown → 放行（不单独决定跳过）。
	require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate(nil))
	require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate([]QuotaDimension{
		{Scope: QuotaDimensionScopeAccount, Status: QuotaDimensionUnknown},
	}))
	// 有可用余量 → 放行。
	require.Equal(t, QuotaDimensionGateAllow, EvaluateAccountQuotaDimensionGate([]QuotaDimension{
		{Kind: QuotaDimensionKindSubscription, Scope: QuotaDimensionScopeAccount, Status: QuotaDimensionRemaining, Servable: QuotaServableUnknown},
	}))
}

// ---------------------------------------------------------------------------
// 白盒：resolveMapping 必须透传渠道 BillingModelSource（三处 billing source 的输入）。
// ---------------------------------------------------------------------------

func TestResolveMappingBillingSourcePlumbed(t *testing.T) {
	for _, src := range []string{BillingModelSourceRequested, BillingModelSourceUpstream, BillingModelSourceChannelMapped, BillingModelSourceResponse} {
		lk := &channelLookup{channel: &Channel{ID: 1, BillingModelSource: src}, cache: &channelCache{}}
		cm := resolveMapping(lk, 1, "grok-4.7")
		require.Equal(t, src, cm.BillingModelSource)
	}
}

// ---------------------------------------------------------------------------
// 集成（真实 DB）：配置 A —— composite 组所有文本端点显式改向 OpenAI → 水位 1。
//
// 期望值修正（原为 0，写于 2026-10-20）：2026-10-21 起 OpenAI 兼容族已纳入 cursor 跨平台桥接
// （cursorNativeBridgePlatforms 含 PlatformOpenAI），"显式改向 OpenAI = cursor 不可服务"的
// 前提已消失。调度器会把这类请求派给映射了该模型的 cursor 账号，水位必须与之一致；
// 同口径正例见 cursor_o57a_integration_test.go 用例 (b)
// （openai 平台请求 + 映射该模型的 cursor 账号 → 准入通过，reason 为空）。
// ---------------------------------------------------------------------------

func TestCursorWaterLevelConfigAOpenAIExplicitTargetAdmitsCursorBridge(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	model := "grok-4.7"
	key := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 来源门（A04-G）先于路由/渠道/依赖门判定：装齐来源读取器，使断言回到本用例的检验对象。
	installRoutedWaterSourceForTest(t, key.ID)
	g := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{model}})
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: g.ID, PublicModel: model, MatchType: CompositeRouteMatchExact, Enabled: true, Endpoint: CompositeRouteEndpointChatCompletions, TargetPlatform: PlatformOpenAI},
		{ID: 2, GroupID: g.ID, PublicModel: model, MatchType: CompositeRouteMatchExact, Enabled: true, Endpoint: CompositeRouteEndpointMessages, TargetPlatform: PlatformOpenAI},
		{ID: 3, GroupID: g.ID, PublicModel: model, MatchType: CompositeRouteMatchExact, Enabled: true, Endpoint: CompositeRouteEndpointResponses, TargetPlatform: PlatformOpenAI},
		{ID: 4, GroupID: g.ID, PublicModel: model, MatchType: CompositeRouteMatchExact, Enabled: true, Endpoint: CompositeRouteEndpointGemini, TargetPlatform: PlatformOpenAI},
	}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: routes})
	w := newRoutedWater(t, client, resolver, stubChannelServiceNoChannel(), &GatewayService{})
	acc := mustCreateCursorAccount(t, client, key.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, acc.ID, g.ID)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.AccountID(acc.ID)).Exec(ctx)
		_, _ = client.Group.Delete().Where(entgroup.ID(g.ID)).Exec(ctx)
		_ = client.Account.UpdateOneID(acc.ID).SetDeletedAt(time.Now()).Exec(ctx)
	})
	current, threshold, err := w.WaterLevel(ctx, model)
	require.NoError(t, err)
	require.Equal(t, 10, threshold)
	require.Equal(t, 1, current, "配置A: 文本端点显式改向 OpenAI 后 cursor 仍可服务（桥接白名单已含 openai），应计入水位")
}

// ---------------------------------------------------------------------------
// 集成：组级渠道读取失败 ≠ 无渠道 → 整次 WaterLevel 错误（不返回部分数）。
// ---------------------------------------------------------------------------

func TestCursorWaterLevelChannelLookupErrorFailsWhole(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	model := "grok-4.7"
	key := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 来源门（A04-G）先于路由/渠道/依赖门判定：装齐来源读取器，使断言回到本用例的检验对象。
	installRoutedWaterSourceForTest(t, key.ID)
	g := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{model}})
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: g.ID, PublicModel: model, MatchType: CompositeRouteMatchExact, Enabled: true, Endpoint: CompositeRouteEndpointChatCompletions, TargetPlatform: PlatformCursor},
	}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: routes})
	w := newRoutedWater(t, client, resolver, stubChannelServiceError(), &GatewayService{})
	acc := mustCreateCursorAccount(t, client, key.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, acc.ID, g.ID)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.AccountID(acc.ID)).Exec(ctx)
		_, _ = client.Group.Delete().Where(entgroup.ID(g.ID)).Exec(ctx)
		_ = client.Account.UpdateOneID(acc.ID).SetDeletedAt(time.Now()).Exec(ctx)
	})
	current, _, err := w.WaterLevel(ctx, model)
	require.Error(t, err)
	require.Contains(t, err.Error(), "lookup group channel")
	require.Equal(t, 0, current)
}

// ---------------------------------------------------------------------------
// 集成：gemini 端点未匹配 → 默认补 Gemini（Cursor 桥接可服务）→ 计入水位。
// ---------------------------------------------------------------------------

func TestCursorWaterLevelGeminiUnmatchedDefaultsToGemini(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	model := "zzz-unknown-9" // detector 不识别 → 路由/ownership/detector 均不命中
	key := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 来源门（A04-G）先于路由/渠道/依赖门判定：装齐来源读取器，使断言回到本用例的检验对象。
	installRoutedWaterSourceForTest(t, key.ID)
	g := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{model}})
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{}})
	w := newRoutedWater(t, client, resolver, stubChannelServiceNoChannel(), &GatewayService{})
	acc := mustCreateCursorAccount(t, client, key.ID, func(c *ent.AccountCreate) {
		c.SetCredentials(map[string]any{"model_mapping": map[string]any{model: model}})
	})
	mustLinkAccountToGroup(t, client, acc.ID, g.ID)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.AccountID(acc.ID)).Exec(ctx)
		_, _ = client.Group.Delete().Where(entgroup.ID(g.ID)).Exec(ctx)
		_ = client.Account.UpdateOneID(acc.ID).SetDeletedAt(time.Now()).Exec(ctx)
	})
	current, _, err := w.WaterLevel(ctx, model)
	require.NoError(t, err)
	require.Equal(t, 1, current, "gemini 端点未匹配但默认补 Gemini，应计入水位")
}

// ---------------------------------------------------------------------------
// 集成：真实路由命中 Cursor 可服务端点 → 计入水位（正向，验证门在应放行时放行）。
// ---------------------------------------------------------------------------

func TestCursorWaterLevelCursorRoutedEndpointCounts(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	model := "grok-4.7"
	key := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 来源门（A04-G）先于路由/渠道/依赖门判定：装齐来源读取器，使断言回到本用例的检验对象。
	installRoutedWaterSourceForTest(t, key.ID)
	g := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{model}})
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: g.ID, PublicModel: model, MatchType: CompositeRouteMatchExact, Enabled: true, Endpoint: CompositeRouteEndpointChatCompletions, TargetPlatform: PlatformCursor},
	}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: routes})
	w := newRoutedWater(t, client, resolver, stubChannelServiceNoChannel(), &GatewayService{})
	acc := mustCreateCursorAccount(t, client, key.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, acc.ID, g.ID)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.AccountID(acc.ID)).Exec(ctx)
		_, _ = client.Group.Delete().Where(entgroup.ID(g.ID)).Exec(ctx)
		_ = client.Account.UpdateOneID(acc.ID).SetDeletedAt(time.Now()).Exec(ctx)
	})
	current, _, err := w.WaterLevel(ctx, model)
	require.NoError(t, err)
	require.Equal(t, 1, current, "真实路由命中 Cursor 可服务端点应计入水位")
}

// ---------------------------------------------------------------------------
// 集成：同一账号挂入两个 Cursor 可服务组 → 只计一次（去重）。
// ---------------------------------------------------------------------------

func TestCursorWaterLevelSameAccountDedupedAcrossGroups(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	model := "grok-4.7"
	key := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 来源门（A04-G）先于路由/渠道/依赖门判定：装齐来源读取器，使断言回到本用例的检验对象。
	installRoutedWaterSourceForTest(t, key.ID)
	g1 := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{model}})
	g2 := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{model}})
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: g1.ID, PublicModel: model, MatchType: CompositeRouteMatchExact, Enabled: true, Endpoint: CompositeRouteEndpointChatCompletions, TargetPlatform: PlatformCursor},
	}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: routes})
	w := newRoutedWater(t, client, resolver, stubChannelServiceNoChannel(), &GatewayService{})
	acc := mustCreateCursorAccount(t, client, key.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, acc.ID, g1.ID)
	mustLinkAccountToGroup(t, client, acc.ID, g2.ID)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.AccountID(acc.ID)).Exec(ctx)
		_, _ = client.Group.Delete().Where(entgroup.IDIn(g1.ID, g2.ID)).Exec(ctx)
		_ = client.Account.UpdateOneID(acc.ID).SetDeletedAt(time.Now()).Exec(ctx)
	})
	current, _, err := w.WaterLevel(ctx, model)
	require.NoError(t, err)
	require.Equal(t, 1, current, "同一账号跨多组只计一次")
}

// ---------------------------------------------------------------------------
// 集成：其它平台账号不纳入 Cursor 水位（不扩围）。
// ---------------------------------------------------------------------------

func TestCursorWaterLevelOtherPlatformNotExpanded(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	model := "grok-4.7"
	key := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 来源门（A04-G）先于路由/渠道/依赖门判定：装齐来源读取器，使断言回到本用例的检验对象。
	installRoutedWaterSourceForTest(t, key.ID)
	g := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{model}})
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: g.ID, PublicModel: model, MatchType: CompositeRouteMatchExact, Enabled: true, Endpoint: CompositeRouteEndpointChatCompletions, TargetPlatform: PlatformCursor},
	}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: routes})
	w := newRoutedWater(t, client, resolver, stubChannelServiceNoChannel(), &GatewayService{})
	cursorAcc := mustCreateCursorAccount(t, client, key.ID, withCursorWaterModels())
	openaiAcc, err := client.Account.Create().
		SetName(fmt.Sprintf("other-plat-acc-%d", time.Now().UnixNano())).
		SetPlatform(domain.PlatformOpenAI).
		SetType("oauth").
		SetCredentials(map[string]any{"model_mapping": map[string]any{model: model}}).
		SetQuotaKeyID(key.ID).
		SetSchedulable(true).
		SetStatus(domain.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	mustLinkAccountToGroup(t, client, cursorAcc.ID, g.ID)
	mustLinkAccountToGroup(t, client, openaiAcc.ID, g.ID)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.AccountIDIn(cursorAcc.ID, openaiAcc.ID)).Exec(ctx)
		_, _ = client.Group.Delete().Where(entgroup.ID(g.ID)).Exec(ctx)
		_ = client.Account.Update().Where(account.IDIn(cursorAcc.ID, openaiAcc.ID)).SetDeletedAt(time.Now()).Exec(ctx)
	})
	current, _, err := w.WaterLevel(ctx, model)
	require.NoError(t, err)
	require.Equal(t, 1, current, "仅 Cursor 账号计入，OpenAI 账号不扩围")
}

// ---------------------------------------------------------------------------
// 集成：组级渠道限制模型 → 账号被排除（覆盖三处 billing source 的渠道限制分支）。
// ---------------------------------------------------------------------------

func TestCursorWaterLevelChannelRestrictsModelExcluded(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	model := "grok-4.7"
	key := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 来源门（A04-G）先于路由/渠道/依赖门判定：装齐来源读取器，使断言回到本用例的检验对象。
	installRoutedWaterSourceForTest(t, key.ID)
	g := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{model}})
	routes := []CompositeModelRoute{
		{ID: 1, GroupID: g.ID, PublicModel: model, MatchType: CompositeRouteMatchExact, Enabled: true, Endpoint: CompositeRouteEndpointChatCompletions, TargetPlatform: PlatformCursor},
	}
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: routes})

	// 限制模型（无定价）→ 排除。
	wRestricted := newRoutedWater(t, client, resolver, stubChannelServiceRestricting(g.ID), &GatewayService{})
	acc := mustCreateCursorAccount(t, client, key.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, acc.ID, g.ID)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.AccountID(acc.ID)).Exec(ctx)
		_, _ = client.Group.Delete().Where(entgroup.ID(g.ID)).Exec(ctx)
		_ = client.Account.UpdateOneID(acc.ID).SetDeletedAt(time.Now()).Exec(ctx)
	})
	curRestricted, _, err := wRestricted.WaterLevel(ctx, model)
	require.NoError(t, err)
	require.Equal(t, 0, curRestricted, "渠道限制模型 → 账号被排除")

	// 不限制（RestrictModels=false）→ 计入。
	wOpen := newRoutedWater(t, client, resolver, stubChannelServiceOpen(g.ID), &GatewayService{})
	curOpen, _, err := wOpen.WaterLevel(ctx, model)
	require.NoError(t, err)
	require.Equal(t, 1, curOpen, "渠道未限制 → 账号计入")
}

// ---------------------------------------------------------------------------
// 缺陷回修（D-CRT-01W-FIX）：依赖缺失 → 整次 WaterLevel 返回错误，而非静默退回
// 粗交集计数；依赖齐备 → 同一配置按 routed 口径得到既有结果（防止误伤）。
// ---------------------------------------------------------------------------

func TestCursorWaterLevelMissingRoutingDepsFailsClosed(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	model := "grok-4.7"
	key := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	// 来源门（A04-G）先于路由/渠道/依赖门判定：装齐来源读取器，使断言回到本用例的检验对象。
	installRoutedWaterSourceForTest(t, key.ID)
	g := mustCreateCompositeGroup(t, client, domain.GroupModelAllowlist{Enabled: true, Models: []string{model}})
	// 该账号 + 组 + 白名单若走粗交集口径会不计为 1（组白名单允许 model、账号支持 model、
	// 账号可调度）—— 正是 §18.2 W 行认定的计数 bug 本体，依赖缺失时绝不可退回。
	acc := mustCreateCursorAccount(t, client, key.ID, withCursorWaterModels())
	mustLinkAccountToGroup(t, client, acc.ID, g.ID)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.AccountID(acc.ID)).Exec(ctx)
		_, _ = client.Group.Delete().Where(entgroup.ID(g.ID)).Exec(ctx)
		_ = client.Account.UpdateOneID(acc.ID).SetDeletedAt(time.Now()).Exec(ctx)
	})

	// 依赖缺失：未注入 gateway/channel/resolver → 必须显式失败，绝不退回粗交集。
	// 用 NewQuotaKeyModelWater（生产构造，不含路由依赖）等价"装配漏注入"场景。
	waterMissing := NewQuotaKeyModelWater(client, nil)
	curMissing, _, errMissing := waterMissing.WaterLevel(ctx, model)
	require.Error(t, errMissing, "依赖缺失必须整次失败，不得静默退回粗交集计数")
	require.Contains(t, errMissing.Error(), "routing dependency not wired")
	require.Equal(t, 0, curMissing, "依赖缺失时不得作为成功计数返回（旧粗交集会返回 1）")

	// 依赖齐备：同一配置按 routed 口径得到既有结果（resolver 把 model 路由到 Cursor → 计 1）。
	waterWired := newRoutedWater(t, client, stubResolverAllCursor(), stubChannelServiceNoChannel(), &GatewayService{})
	curWired, _, errWired := waterWired.WaterLevel(ctx, model)
	require.NoError(t, errWired)
	require.Equal(t, 1, curWired, "依赖齐备时同一配置应仍按 routed 口径计入（防止误伤）")
}
