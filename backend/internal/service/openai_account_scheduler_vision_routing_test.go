package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// fakeVisionRoutingRepository 是测试替身：Get 按注入配置返回，可注入读取错误并
// 统计读取次数，用于验证「每次选号请求最多读一次」。
type fakeVisionRoutingRepository struct {
	routing   map[string][]int64
	getErr    error
	getCalls  int
	groupSeen []int64
}

func (r *fakeVisionRoutingRepository) Get(ctx context.Context, groupID int64) (map[string][]int64, error) {
	r.getCalls++
	r.groupSeen = append(r.groupSeen, groupID)
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.routing, nil
}

func (r *fakeVisionRoutingRepository) GetByGroupIDs(_ context.Context, _ []int64) (map[int64]map[string][]int64, error) {
	panic("unexpected call to GetByGroupIDs")
}

func (r *fakeVisionRoutingRepository) Set(context.Context, int64, map[string][]int64) error {
	panic("unexpected call to Set")
}

// newVisionRoutingTestService 装配一个启用高级调度器的 OpenAIGatewayService，
// 带注入的视觉分流服务与账号候选池（docs/capability-routing-plan.md §3.7）。
func newVisionRoutingTestService(accounts []Account, repo *fakeVisionRoutingRepository) *OpenAIGatewayService {
	cfg := newSchedulerTestSubscriptionPriorityConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	if repo != nil {
		svc.visionRouting = NewVisionRoutingService(repo, nil)
	}
	return svc
}

// visionRoutingTestAccounts 返回两个同组 OpenAI APIKey 账号：target 优先级低、
// other 优先级高（若收窄失效会优先选中 other）。
func visionRoutingTestAccounts(groupID int64) (target, other Account) {
	target = Account{
		ID: 21651, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 100,
		AccountGroups: []AccountGroup{{AccountID: 21651, GroupID: groupID, Priority: 100}},
		GroupIDs:      []int64{groupID},
	}
	other = Account{
		ID: 21652, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1,
		AccountGroups: []AccountGroup{{AccountID: 21652, GroupID: groupID, Priority: 1}},
		GroupIDs:      []int64{groupID},
	}
	return target, other
}

func selectVisionRoutingForTest(
	t *testing.T,
	svc *OpenAIGatewayService,
	groupID int64,
	model string,
	requireVision bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	t.Helper()
	return svc.SelectAccountWithSchedulerForCapability(
		context.Background(),
		&groupID,
		"",
		"",
		model,
		nil,
		OpenAIUpstreamTransportAny,
		OpenAIEndpointCapabilityChatCompletions,
		false,
		false,
		false,
		requireVision,
	)
}

// 场景 1：命中限定——带图请求命中配置时，候选池收窄为目标账号，即使非目标账号
// 优先级更高也不得入选。
func TestOpenAIAccountScheduler_VisionRouting_TargetRestrictsCandidatePool(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
		return false, false, nil // unknown：按 §3.4 保守放行
	})

	groupID := int64(10131)
	target, other := visionRoutingTestAccounts(groupID)
	repo := &fakeVisionRoutingRepository{routing: map[string][]int64{"gpt-5.1": {target.ID}}}
	svc := newVisionRoutingTestService([]Account{other, target}, repo)

	selection, _, err := selectVisionRoutingForTest(t, svc, groupID, "gpt-5.1", true)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, target.ID, selection.Account.ID, "候选池必须收窄到视觉分流目标账号")
	require.Equal(t, 1, repo.getCalls, "每次选号请求最多读取一次视觉分流配置")
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

// 场景 2：目标池内仍被能力过滤排除——收窄只收窄候选池，不豁免 §3.4 能力过滤。
func TestOpenAIAccountScheduler_VisionRouting_TargetStillSubjectToCapabilityFilter(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	// 目标账号在能力表中被标为已知不支持视觉。
	setTestVisionCapabilityLookup(t, func(_ context.Context, accountID int64, _ string, _ string) (bool, bool, error) {
		require.Equal(t, int64(21651), accountID)
		return false, true, nil
	})

	groupID := int64(10132)
	target, other := visionRoutingTestAccounts(groupID)
	repo := &fakeVisionRoutingRepository{routing: map[string][]int64{"gpt-5.1": {target.ID}}}
	svc := newVisionRoutingTestService([]Account{other, target}, repo)

	selection, _, err := selectVisionRoutingForTest(t, svc, groupID, "gpt-5.1", true)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, selection)
	require.ErrorContains(t, err, "vision_not_supported", "收窄后仍须执行 §3.4 能力过滤，绝不豁免")
}

// 场景 3：池空返回错误——目标集合非空但没有任何候选命中时，返回无可用账号错误
// （不静默回落 unknown 放行）。
func TestOpenAIAccountScheduler_VisionRouting_NoTargetCandidateReturnsError(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
		return false, false, nil
	})

	groupID := int64(10133)
	target, other := visionRoutingTestAccounts(groupID)
	// 目标 ID 不存在于候选池。
	repo := &fakeVisionRoutingRepository{routing: map[string][]int64{"gpt-5.1": {999999}}}
	svc := newVisionRoutingTestService([]Account{other, target}, repo)

	selection, _, err := selectVisionRoutingForTest(t, svc, groupID, "gpt-5.1", true)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, selection)
	require.ErrorContains(t, err, "vision_routing_excluded")
}

// 场景 4：未配置命中回落既有过滤——分组未配置视觉分流时零收窄，行为与既有调度一致。
func TestOpenAIAccountScheduler_VisionRouting_UnconfiguredFallsBackToExistingFilter(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
		return false, false, nil
	})

	groupID := int64(10134)
	target, other := visionRoutingTestAccounts(groupID)
	repo := &fakeVisionRoutingRepository{routing: map[string][]int64{}}
	svc := newVisionRoutingTestService([]Account{other, target}, repo)

	selection, _, err := selectVisionRoutingForTest(t, svc, groupID, "gpt-5.1", true)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, other.ID, selection.Account.ID, "未配置视觉分流时按既有优先级正常选号")
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

// 场景 5：RequireVision=false 零影响——非带图请求零读取、零收窄。
func TestOpenAIAccountScheduler_VisionRouting_RequireVisionFalseNoEffect(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
		return false, false, nil
	})

	groupID := int64(10135)
	target, other := visionRoutingTestAccounts(groupID)
	repo := &fakeVisionRoutingRepository{routing: map[string][]int64{"gpt-5.1": {target.ID}}}
	svc := newVisionRoutingTestService([]Account{other, target}, repo)

	selection, _, err := selectVisionRoutingForTest(t, svc, groupID, "gpt-5.1", false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, other.ID, selection.Account.ID, "RequireVision=false 不得触发收窄")
	require.Equal(t, 0, repo.getCalls, "RequireVision=false 时零读取")
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

// 场景 6：读取错误失败关闭——视觉分流配置读取失败时终止选号，禁止静默当作未配置
// （合同 §5）。
func TestOpenAIAccountScheduler_VisionRouting_ReadErrorFailsClosed(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
		return false, false, nil
	})

	groupID := int64(10136)
	target, other := visionRoutingTestAccounts(groupID)
	repo := &fakeVisionRoutingRepository{getErr: errors.New("vision routing backend unavailable")}
	svc := newVisionRoutingTestService([]Account{other, target}, repo)

	selection, _, err := selectVisionRoutingForTest(t, svc, groupID, "gpt-5.1", true)
	require.Error(t, err)
	require.Nil(t, selection)
	require.NotErrorIs(t, err, ErrNoAvailableAccounts, "读取失败必须是明确错误，而非静默当作未配置")
	require.ErrorContains(t, err, "load vision routing targets")
}

// newVisionRoutingTestServiceWithKillSwitch 与 newVisionRoutingTestService 同构，
// 额外把视觉路由 kill-switch（vision_routing_enabled）按 killSwitch 写入设置仓储；
// killSwitch 为空表示未配置（默认开启）。
// kill-switch 读取走 svc.settingService（P2 修复点：构造函数注入的权威实例），
// 因此同一个 SettingService 实例必须同时挂到 svc.settingService 与
// rateLimitService.settingService（后者仍供高级调度器 settingRepo 读取），
// 否则既有场景会因 svc.settingService 为 nil 而恒返回默认开启、行为变化。
func newVisionRoutingTestServiceWithKillSwitch(
	accounts []Account,
	repo *fakeVisionRoutingRepository,
	killSwitch string,
) *OpenAIGatewayService {
	cfg := newSchedulerTestSubscriptionPriorityConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	settingRepo := &openAIAdvancedSchedulerSettingRepoStub{
		values: map[string]string{openAIAdvancedSchedulerSettingKey: "true"},
	}
	if killSwitch != "" {
		settingRepo.values[SettingKeyVisionRoutingEnabled] = killSwitch
	}
	settingService := NewSettingService(settingRepo, &config.Config{})
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		settingService:     settingService,
		rateLimitService:   &RateLimitService{settingService: settingService},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	if repo != nil {
		svc.visionRouting = NewVisionRoutingService(repo, nil)
	}
	return svc
}

// 场景 7：kill-switch 关闭 → 回到 v1 现状。设置 vision_routing_enabled=false 后，
// 带图请求不得再触发 §3.4 能力过滤（已知不支持视觉的账号不再被排除），也不得触发
// §3.7 候选池收窄（视觉分流配置零读取、零生效），按既有优先级普通选号。
func TestOpenAIAccountScheduler_VisionRouting_KillSwitchDisabledFallsBackToV1(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	// 两个账号都被标记为「已知不支持视觉」：开关开启时带图请求必被 §3.4 排除。
	setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
		return false, true, nil
	})

	groupID := int64(10137)
	target, other := visionRoutingTestAccounts(groupID)
	// 视觉分流配置把 target 作为唯一目标；kill-switch 关闭后该配置必须整体失效。
	repo := &fakeVisionRoutingRepository{routing: map[string][]int64{"gpt-5.1": {target.ID}}}
	svc := newVisionRoutingTestServiceWithKillSwitch([]Account{other, target}, repo, "false")

	selection, _, err := selectVisionRoutingForTest(t, svc, groupID, "gpt-5.1", true)
	require.NoError(t, err, "kill-switch 关闭后带图请求不得再因视觉能力被排除")
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, other.ID, selection.Account.ID, "kill-switch 关闭后回到 v1 普通路由，按既有优先级选号")
	require.Equal(t, 0, repo.getCalls, "kill-switch 关闭后视觉分流配置零读取")
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

// 场景 8：kill-switch 显式开启 → 现状行为（收窄照常生效）。
func TestOpenAIAccountScheduler_VisionRouting_KillSwitchEnabledKeepsCurrentBehavior(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
		return false, false, nil // unknown：按 §3.4 保守放行
	})

	groupID := int64(10138)
	target, other := visionRoutingTestAccounts(groupID)
	repo := &fakeVisionRoutingRepository{routing: map[string][]int64{"gpt-5.1": {target.ID}}}
	svc := newVisionRoutingTestServiceWithKillSwitch([]Account{other, target}, repo, "true")

	selection, _, err := selectVisionRoutingForTest(t, svc, groupID, "gpt-5.1", true)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, target.ID, selection.Account.ID, "kill-switch 显式开启时视觉分流收窄照常生效")
	require.Equal(t, 1, repo.getCalls, "每次选号请求最多读取一次视觉分流配置")
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

// 场景 9：kill-switch 未配置 → 默认开启（开关定位是回滚手段，非灰度门禁）。
func TestOpenAIAccountScheduler_VisionRouting_KillSwitchUnsetDefaultsToEnabled(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
		return false, false, nil
	})

	groupID := int64(10139)
	target, other := visionRoutingTestAccounts(groupID)
	repo := &fakeVisionRoutingRepository{routing: map[string][]int64{"gpt-5.1": {target.ID}}}
	svc := newVisionRoutingTestServiceWithKillSwitch([]Account{other, target}, repo, "") // 未配置

		selection, _, err := selectVisionRoutingForTest(t, svc, groupID, "gpt-5.1", true)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, target.ID, selection.Account.ID, "未配置 kill-switch 时视觉分流照常生效（默认开启）")
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

// TestOpenAIAccountScheduler_VisionCapability_ReadErrorBehavior 覆盖派发单 Vision-R2（终审 P1-2）：
// §3.4 能力源读取失败时显式失败关闭，禁止静默降级为 unknown 放行（合同 §5）。
func TestOpenAIAccountScheduler_VisionCapability_ReadErrorBehavior(t *testing.T) {
	ctx := context.Background()
	account := &Account{ID: 21660, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{openaiModelTransient: newOpenAIAccountModelTransientState(128)}}

	reqBase := OpenAIAccountScheduleRequest{
		RequestedModel:     "gpt-5.1",
		RequiredTransport:  OpenAIUpstreamTransportAny,
		RequiredCapability: OpenAIEndpointCapabilityChatCompletions,
	}

	// 场景 ①：能力源读取失败 + RequireVision=true → 选号失败关闭（不静默放行）。
	t.Run("read error + RequireVision=true fails closed", func(t *testing.T) {
		setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
			return false, false, errors.New("capability source unavailable")
		})
		req := reqBase
		req.RequireVision = true
		compatible, reason := scheduler.isAccountRequestCompatibleReason(ctx, account, req)
		require.False(t, compatible)
		require.Equal(t, "vision_capability_unavailable", reason)
	})

	// 场景 ②：确认无记录（unknown）→ 仍放行（既有语义不回归）。
	t.Run("confirmed no record (unknown) still allowed", func(t *testing.T) {
		setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
			return false, false, nil // unknown
		})
		req := reqBase
		req.RequireVision = true
		compatible, reason := scheduler.isAccountRequestCompatibleReason(ctx, account, req)
		require.True(t, compatible)
		require.Empty(t, reason)
	})

	// 场景 ③：读取失败 + RequireVision=false → 不读能力表，无影响。
	t.Run("read error + RequireVision=false no effect", func(t *testing.T) {
		setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
			return false, false, errors.New("capability source unavailable")
		})
		req := reqBase
		req.RequireVision = false
		compatible, reason := scheduler.isAccountRequestCompatibleReason(ctx, account, req)
		require.True(t, compatible)
		require.Empty(t, reason)
	})
}

// TestOpenAIAccountScheduler_VisionCapability_ReadErrorFailsSelectionClosed 端到端验证：
// 唯一候选账号能力源读取失败时，选号整体失败关闭，不得静默选中疑似不支持视觉的账号
// （合同 §5；方案 §3.4）。
func TestOpenAIAccountScheduler_VisionCapability_ReadErrorFailsSelectionClosed(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
		return false, false, errors.New("capability source unavailable")
	})

	groupID := int64(10140)
	target, _ := visionRoutingTestAccounts(groupID)
	// 不接 §3.7 视觉分流层，仅验证 §3.4 能力读取失败关闭。
	svc := newVisionRoutingTestService([]Account{target}, nil)

	selection, _, err := selectVisionRoutingForTest(t, svc, groupID, "gpt-5.1", true)
	require.Error(t, err, "读取失败必须终止选号，不得静默选中疑似不支持视觉的账号")
	require.Nil(t, selection)
}

// TestOpenAIGatewayService_VisionKillSwitchReadsInjectedSettingService 回归（P2 修复点）：
// isVisionRoutingEnabled 必须读构造函数注入的权威实例 s.settingService，绝非
// rateLimitService 上旁挂的 settingService。旧实现读旁路实例，配置 false 时误判默认开启。
// 每个子测试用独立的新 SettingService 实例，避免进程内缓存（visionRoutingEnabledCache，
// 60s TTL）串扰；不加任何新兜底。
func TestOpenAIGatewayService_VisionKillSwitchReadsInjectedSettingService(t *testing.T) {
	ctx := context.Background()
	setTestVisionCapabilityLookup(t, func(context.Context, int64, string, string) (bool, bool, error) {
		return false, false, nil
	})

	visionKillSwitchFalseSettingRepo := func() *openAIAdvancedSchedulerSettingRepoStub {
		return &openAIAdvancedSchedulerSettingRepoStub{
			values: map[string]string{
				openAIAdvancedSchedulerSettingKey: "true",
				SettingKeyVisionRoutingEnabled:     "false",
			},
		}
	}

	// 反证 a：vision_routing_enabled=false 只写入 svc.settingService（权威实例），
	// rateLimitService 为 nil —— 开关必须返回 false（权威源被读取）。
	// 旧实现此处因读旁路实例（nil）返回 true，kill-switch 失效。
	t.Run("authority false read via svc.settingService with nil rateLimitService", func(t *testing.T) {
		authority := NewSettingService(visionKillSwitchFalseSettingRepo(), &config.Config{})
		svc := &OpenAIGatewayService{
			settingService:     authority,
			rateLimitService:   nil, // 旁路未装配：旧实现读旁路会误判 true
			accountRepo:        schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: nil}},
			cache:              &schedulerTestGatewayCache{},
			cfg:                newSchedulerTestSubscriptionPriorityConfig(),
			concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
		}
		require.False(t, svc.isVisionRoutingEnabled(ctx), "false 写入权威实例且 rateLimitService 为 nil 时必须关闭")
	})

	// 反证 b（对照）：vision_routing_enabled=false 只写入旁路
	// rateLimitService.settingService，而 svc.settingService（权威）为 nil ——
	// 开关必须返回 true（默认开启），证明不再误读旁路实例。
	t.Run("sidecar false ignored when svc.settingService is nil", func(t *testing.T) {
		sidecar := NewSettingService(visionKillSwitchFalseSettingRepo(), &config.Config{})
		svc := &OpenAIGatewayService{
			settingService:     nil, // 权威未装配：默认开启语义保持不变
			rateLimitService:   &RateLimitService{settingService: sidecar},
			accountRepo:        schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: nil}},
			cache:              &schedulerTestGatewayCache{},
			cfg:                newSchedulerTestSubscriptionPriorityConfig(),
			concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
		}
		require.True(t, svc.isVisionRoutingEnabled(ctx), "false 只写入旁路且权威为 nil 时必须保持默认开启")
	})
}

