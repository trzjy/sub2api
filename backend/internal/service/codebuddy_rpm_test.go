package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// codeBuddyRPMCacheStub 实现 RPMCache，供 CodeBuddy RPM 调度测试注入固定计数。
type codeBuddyRPMCacheStub struct {
	counts       map[int64]int
	increments   map[int64]int
	batchCalls   int
	incrementErr error
}

func (c *codeBuddyRPMCacheStub) IncrementRPM(_ context.Context, accountID int64) (int, error) {
	if c.incrementErr != nil {
		return 0, c.incrementErr
	}
	if c.increments == nil {
		c.increments = map[int64]int{}
	}
	c.increments[accountID]++
	return c.counts[accountID] + c.increments[accountID], nil
}

func (c *codeBuddyRPMCacheStub) GetRPM(_ context.Context, accountID int64) (int, error) {
	return c.counts[accountID], nil
}

func (c *codeBuddyRPMCacheStub) GetRPMBatch(_ context.Context, accountIDs []int64) (map[int64]int, error) {
	c.batchCalls++
	out := make(map[int64]int, len(accountIDs))
	for _, id := range accountIDs {
		out[id] = c.counts[id]
	}
	return out, nil
}

func codeBuddyRPMSettingService(values map[string]string) *SettingService {
	if values == nil {
		values = map[string]string{}
	}
	return NewSettingService(&antigravitySettingRepoStub{values: values}, &config.Config{})
}

func codeBuddyRPMTestAccount(id int64, extra map[string]any) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra:       extra,
		Credentials: map[string]any{"access_token": "at", "uid": "u"},
	}
}

// TestGetCodeBuddyDefaultRPM 覆盖平台默认 RPM 取值：未配置回落 10，显式配置生效，
// 显式配置 0 表示不启用平台默认。
func TestGetCodeBuddyDefaultRPM(t *testing.T) {
	ctx := context.Background()

	require.Equal(t, 10, codeBuddyRPMSettingService(nil).GetCodeBuddyDefaultRPM(ctx),
		"未配置时必须回落出厂默认 10")

	require.Equal(t, 25, codeBuddyRPMSettingService(map[string]string{
		SettingKeyCodeBuddyDefaultRPM: "25",
	}).GetCodeBuddyDefaultRPM(ctx))

	require.Equal(t, 0, codeBuddyRPMSettingService(map[string]string{
		SettingKeyCodeBuddyDefaultRPM: "0",
	}).GetCodeBuddyDefaultRPM(ctx), "显式 0 表示关闭平台默认")
}

// TestCodeBuddyRPMSchedulable_DefaultAndOverride 覆盖 B1 两条路径：
// 未配置账号命中平台默认；显式 base_rpm 覆盖默认。
func TestCodeBuddyRPMSchedulable_DefaultAndOverride(t *testing.T) {
	ctx := context.Background()

	t.Run("未配置账号命中平台默认10", func(t *testing.T) {
		svc := &OpenAIGatewayService{settingService: codeBuddyRPMSettingService(nil)}
		account := codeBuddyRPMTestAccount(1, nil)

		require.True(t, svc.codeBuddyRPMSchedulable(ctx, account, 5, false), "绿区应可调度")
		require.False(t, svc.codeBuddyRPMSchedulable(ctx, account, 50, false), "红区非粘性应被排除")
		require.True(t, svc.codeBuddyRPMSchedulable(ctx, account, 10, true), "黄区粘性应放行")
		require.False(t, svc.codeBuddyRPMSchedulable(ctx, account, 10, false), "黄区非粘性应被排除")
		// 计数不可用 → 失败开放
		require.True(t, svc.codeBuddyRPMSchedulable(ctx, account, -1, false))
	})

	t.Run("显式base_rpm覆盖平台默认", func(t *testing.T) {
		svc := &OpenAIGatewayService{settingService: codeBuddyRPMSettingService(nil)}
		account := codeBuddyRPMTestAccount(2, map[string]any{"base_rpm": 100})

		require.True(t, svc.codeBuddyRPMSchedulable(ctx, account, 50, false),
			"显式 100 时 count=50 属绿区（若误用默认10则会被排除）")
		require.Equal(t, 100, svc.codeBuddyEffectiveRPM(ctx, account))
	})

	t.Run("平台默认可调为0时关闭限流", func(t *testing.T) {
		svc := &OpenAIGatewayService{settingService: codeBuddyRPMSettingService(map[string]string{
			SettingKeyCodeBuddyDefaultRPM: "0",
		})}
		account := codeBuddyRPMTestAccount(3, nil)
		require.True(t, svc.codeBuddyRPMSchedulable(ctx, account, 9999, false))
	})

	t.Run("显式base_rpm优先于平台默认0", func(t *testing.T) {
		svc := &OpenAIGatewayService{settingService: codeBuddyRPMSettingService(map[string]string{
			SettingKeyCodeBuddyDefaultRPM: "0",
		})}
		account := codeBuddyRPMTestAccount(4, map[string]any{"base_rpm": 10})
		require.False(t, svc.codeBuddyRPMSchedulable(ctx, account, 50, false),
			"平台默认关闭不影响账号显式配置")
	})
}

// TestIncrementAndPrefetchCodeBuddyRPM 覆盖计数递增与批量预取。
func TestIncrementAndPrefetchCodeBuddyRPM(t *testing.T) {
	ctx := context.Background()
	rpm := &codeBuddyRPMCacheStub{counts: map[int64]int{11: 3}}
	svc := &OpenAIGatewayService{
		settingService: codeBuddyRPMSettingService(nil),
		rpmCache:       rpm,
	}

	// 非 CodeBuddy 账号不计数
	svc.incrementCodeBuddyRPM(ctx, &Account{ID: 99, Platform: PlatformOpenAI, Type: AccountTypeOAuth})
	require.Empty(t, rpm.increments)

	// CodeBuddy 账号成功触达后计数
	svc.incrementCodeBuddyRPM(ctx, codeBuddyRPMTestAccount(11, nil))
	require.Equal(t, 1, rpm.increments[11])

	// 预取只覆盖 CodeBuddy OAuth 账号
	accounts := []Account{
		*codeBuddyRPMTestAccount(11, nil),
		{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Schedulable: true},
		*codeBuddyRPMTestAccount(13, nil),
	}
	counts := svc.prefetchCodeBuddyRPMCounts(ctx, accounts)
	require.Equal(t, 3, counts[11])
	require.Equal(t, 0, counts[13])
	_, hasOpenAI := counts[12]
	require.False(t, hasOpenAI, "非 CodeBuddy 账号不应进入 RPM 预取")
}

// TestSelectAccountWithScheduler_CodeBuddyRPMFiltersHotAccount 端到端：调度器在
// CodeBuddy 平台默认 RPM 下跳过红区账号；显式 base_rpm 覆盖时不再跳过。
func TestSelectAccountWithScheduler_CodeBuddyRPMFiltersHotAccount(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(90001)

	newService := func(accounts []Account, rpm *codeBuddyRPMCacheStub) *OpenAIGatewayService {
		cfg := &config.Config{}
		cfg.Gateway.Scheduling.LoadBatchEnabled = false
		return &OpenAIGatewayService{
			accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
			cache:              &schedulerTestGatewayCache{},
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			settingService:     codeBuddyRPMSettingService(nil), // 默认 10
			rpmCache:           rpm,
		}
	}

	t.Run("红区账号被平台默认RPM排除", func(t *testing.T) {
		hot := *codeBuddyRPMTestAccount(91001, nil)
		hot.Priority = 0 // 优先级最高，但 RPM 红区
		cool := *codeBuddyRPMTestAccount(91002, nil)
		cool.Priority = 5
		rpm := &codeBuddyRPMCacheStub{counts: map[int64]int{91001: 50}}

		selection, _, err := newService([]Account{hot, cool}, rpm).SelectAccountWithSchedulerForCapability(
			ctx, &groupID, "", "", "", nil,
			OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, PlatformCodeBuddy,
		)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, int64(91002), selection.Account.ID, "应跳过 RPM 红区的高优先级账号")
	})

	t.Run("显式base_rpm覆盖时不再跳过", func(t *testing.T) {
		hot := *codeBuddyRPMTestAccount(92001, map[string]any{"base_rpm": 100})
		hot.Priority = 0
		cool := *codeBuddyRPMTestAccount(92002, nil)
		cool.Priority = 5
		rpm := &codeBuddyRPMCacheStub{counts: map[int64]int{92001: 50}}

		selection, _, err := newService([]Account{hot, cool}, rpm).SelectAccountWithSchedulerForCapability(
			ctx, &groupID, "", "", "", nil,
			OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, PlatformCodeBuddy,
		)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, int64(92001), selection.Account.ID,
			"显式 base_rpm=100 时 count=50 属绿区，应按优先级选中")
	})
}
