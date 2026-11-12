//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// A04-S：高级候选池消费来源门（selectByLoadBalance）的两条 Done when：
//  1. gate reason 非空 → 候选被剔除，且剔除原因按 filterStats 语义可诊断；
//  2. gate 返回 error → 同样**明确剔除并记录**（quota_key_state_unavailable），
//     既不静默当"无否决"放行，也不让单个账号的读取故障把整轮选号打失败。
//
// 与既有 P1-S 资格测试同口径：复用包内夹具（stub repo / 包级 provider 安装），
// 不连 DB、不引入 mock 框架。夹具与替身沿用 cursor_scheduling_eligibility_test.go
// 与 cursor_supply_source_usage_test.go 中的既有定义，不另建第二份来源门替身。

// cursorSupplySourceGateSnapshotErrStub 让**额度快照读取**失败（状态读取正常），
// 用于覆盖 cursorSupplySourceAvailabilityVetoReason 的第二条 error 分支——
// 只装状态替身（cursorQuotaKeyStateStub）只能触发第一条。
type cursorSupplySourceGateSnapshotErrStub struct {
	cursorQuotaKeyStateStub
}

func (s cursorSupplySourceGateSnapshotErrStub) CursorQuotaKeyUsageSnapshot(_ context.Context, _ int64) ([]byte, error) {
	return nil, errors.New("usage snapshot store unreachable")
}

// cursorSupplySourceGateAdvancedReq 构造高级候选池消费的请求（cursor 平台 + 挂组 + 模型）。
func cursorSupplySourceGateAdvancedReq() OpenAIAccountScheduleRequest {
	groupID := cursorSchedTestGroupID
	return OpenAIAccountScheduleRequest{
		GroupID:        &groupID,
		Platform:       PlatformCursor,
		RequestedModel: cursorSchedTestModel,
	}
}

// TestCursorSchedulingAdvancedPoolSourceGateReasonExcludes 钉死第一条：来源门给出明确
// 否决原因时，cursor 账号被剔出高级候选池，原因串进入可诊断的 filterStats。
func TestCursorSchedulingAdvancedPoolSourceGateReasonExcludes(t *testing.T) {
	ctx := context.Background()
	expiredKey := int64(9701)

	installCursorQuotaKeyStateForTest(t, cursorQuotaKeyStateStub{statuses: map[int64]string{
		expiredKey: domain.QuotaKeyStatusExpired,
	}})

	account := cursorSchedTestAccount(8701, &expiredKey, cursorSchedTestModel)
	_, scheduler := cursorSchedTestAdvancedFixture([]Account{account})

	_, _, _, _, err := scheduler.selectByLoadBalance(ctx, cursorSupplySourceGateAdvancedReq())
	require.ErrorIs(t, err, ErrNoAvailableAccounts, "来源门否决的账号不得进入高级候选池")
	require.Contains(t, err.Error(), "quota_key_expired=1", "否决原因须按 filterStats 语义记录")
}

// TestCursorSchedulingAdvancedPoolSourceGateErrorExcludesAndRecords 钉死第二条：来源门
// 读取失败（key 状态不可读 / 额度快照不可读）时，账号被明确剔除并记为
// quota_key_state_unavailable——不是静默放行，也不是整轮失败。
func TestCursorSchedulingAdvancedPoolSourceGateErrorExcludesAndRecords(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		quota int64
		// install 装配本子场景的来源门替身（包级单例，测试结束自动还原）。
		install func(t *testing.T, quotaKeyID int64)
	}{
		{
			name:  "key state read error",
			quota: 9801,
			install: func(t *testing.T, _ int64) {
				installCursorQuotaKeyStateForTest(t, cursorQuotaKeyStateStub{err: errors.New("quota key store unreachable")})
			},
		},
		{
			name:  "usage snapshot read error",
			quota: 9802,
			install: func(t *testing.T, quotaKeyID int64) {
				// 状态读取正常（key 未过期），失败发生在额度快照维度。
				installCursorQuotaKeyStateForTest(t, cursorQuotaKeyStateStub{statuses: map[int64]string{
					quotaKeyID: domain.QuotaKeyStatusActive,
				}})
				prevSnapshot := currentCursorQuotaKeyUsageSnapshotProvider()
				InstallCursorQuotaKeyUsageSnapshotProvider(cursorSupplySourceGateSnapshotErrStub{})
				t.Cleanup(func() { InstallCursorQuotaKeyUsageSnapshotProvider(prevSnapshot) })
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.install(t, tc.quota)

			account := cursorSchedTestAccount(8801, &tc.quota, cursorSchedTestModel)
			_, scheduler := cursorSchedTestAdvancedFixture([]Account{account})

			_, _, _, _, err := scheduler.selectByLoadBalance(ctx, cursorSupplySourceGateAdvancedReq())
			// 失败关闭：读不到来源状态就不进池，但错误仍是"无可用账号"，
			// 而不是把来源读取故障原样抛给调用方把整轮选号打失败。
			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Contains(t, err.Error(), "quota_key_state_unavailable=1",
				"gate error 须记明确排除原因，不得静默放行")
		})
	}
}

// TestCursorSupplySourceGate_APIKeyAllowed 钉死 CARD-KC-10：第三方 Cursor APIKey 账号
// （OpenAI 兼容反代，platform=cursor && type=api_key）不依赖 ShareAI 激活码号池，
// 因此来源门必须放行——不得因其无 QuotaKeyID 返回 quota_key_source_unbound。
//
// 反面同样钉死：号池 OAuth 账号（type≠api_key）未绑码时仍必须被否决，放行条件不得外溢。
func TestCursorSupplySourceGate_APIKeyAllowed(t *testing.T) {
	ctx := context.Background()

	// 未装配任何 provider：APIKey 账号本就无需来源依赖，放行不受装配形态影响。
	clearCursorQuotaKeySourceProvidersForTest(t)

	apiKeyAccount := &Account{
		ID:          9901,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-kn-cy-secret"},
	}
	require.True(t, apiKeyAccount.IsCursorOpenAICompatibleAccount(),
		"前置：platform=cursor && type=api_key 判定为 OpenAI 兼容反代账号")

	reason, err := cursorSupplySourceAvailabilityVetoReason(ctx, apiKeyAccount)
	require.NoError(t, err, "APIKey 类型 Cursor 账号不得因来源依赖未装配而报错")
	require.Empty(t, reason, "APIKey 类型 Cursor 账号必须整体放行来源门")

	reason, err = cursorSupplySourceUsageVetoReason(ctx, apiKeyAccount)
	require.NoError(t, err)
	require.Empty(t, reason, "用量子谓词对 APIKey 类型 Cursor 账号同样必须放行")
	require.NotEqual(t, quotaKeySourceUnbound, reason, "不得返回 quota_key_source_unbound")

	// 兼容投影（既有调度路径消费的 string 形态）同样是空串。
	require.Empty(t, cursorSupplySourceAvailabilityVetoReasonString(ctx, apiKeyAccount))

	// 反证：号池 OAuth 账号未绑码时仍被否决（§25.17.3 不放宽 OAuth 账号的来源检查）。
	poolAccount := &Account{
		ID:          9902,
		Platform:    PlatformCursor,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
	}
	require.False(t, poolAccount.IsCursorOpenAICompatibleAccount())

	reason, err = cursorSupplySourceAvailabilityVetoReason(ctx, poolAccount)
	require.NoError(t, err)
	require.Equal(t, quotaKeySourceUnbound, reason, "号池 OAuth 账号未绑码仍须否决")

	reason, err = cursorSupplySourceUsageVetoReason(ctx, poolAccount)
	require.NoError(t, err)
	require.Equal(t, quotaKeySourceUnbound, reason, "用量子谓词对号池 OAuth 账号同样不放宽")
}

// TestCursorSupplySourceGate_CompatibleAccountWithQuotaKeyHonorsTerminalStatus 钉死
// QKG-FIX-1：第三方 Cursor APIKey 账号（OpenAI 兼容反代）**一旦绑定了激活码**，就必须先过
// 停供终态门——disabled 与 expired 同属停供终态，不得因账号类型被整体豁免。
//
// 背景：kn_cy 上游拉出的 type=apikey 账号同样写入 QuotaKeyID；修复前本函数第一行对
// IsCursorOpenAICompatibleAccount() 直接放行，早于任何码状态判定，导致 admin 停用/到期对
// 这批账号不生效。
//
// 三条断言同时钉住「收紧」与「不放宽」：
//  1. 绑 disabled 码 → 否决（quota_key_expired）；
//  2. 绑 active 码 → 放行（且不因缺号池快照被 usage 子谓词否决）；
//  3. 未绑码的兼容账号 → 维持原放行意图。
func TestCursorSupplySourceGate_CompatibleAccountWithQuotaKeyHonorsTerminalStatus(t *testing.T) {
	ctx := context.Background()

	compatAccount := func(id, quotaKeyID int64) *Account {
		acc := &Account{
			ID:          id,
			Platform:    PlatformCursor,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Credentials: map[string]any{
				"api_key":  "sk-kn-cy-secret",
				"base_url": "https://kn-cy.example/v1",
			},
		}
		if quotaKeyID > 0 {
			acc.QuotaKeyID = &quotaKeyID
		}
		return acc
	}

	t.Run("disabled key vetoes compatible account", func(t *testing.T) {
		const qkID int64 = 9911
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{qkID: domain.QuotaKeyStatusDisabled},
		})
		account := compatAccount(9911, qkID)
		require.True(t, account.IsCursorOpenAICompatibleAccount(), "前置：type=api_key 判定为兼容反代账号")

		reason, err := cursorSupplySourceAvailabilityVetoReason(ctx, account)
		require.NoError(t, err)
		require.Equal(t, quotaKeyExpired, reason, "绑 disabled 码的兼容型账号必须被否决")
		require.Equal(t, quotaKeyExpired, cursorSupplySourceAvailabilityVetoReasonString(ctx, account),
			"既有调度路径的 string 投影同样否决")
	})

	t.Run("expired key vetoes compatible account", func(t *testing.T) {
		const qkID int64 = 9912
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{qkID: domain.QuotaKeyStatusExpired},
		})
		account := compatAccount(9912, qkID)

		reason, err := cursorSupplySourceAvailabilityVetoReason(ctx, account)
		require.NoError(t, err)
		require.Equal(t, quotaKeyExpired, reason, "绑 expired 码的兼容型账号必须被否决")
	})

	t.Run("active key passes compatible account without pool snapshot", func(t *testing.T) {
		const qkID int64 = 9913
		// 只装状态读取、不装额度快照：兼容型账号的额度不走 ShareAI 号池快照，
		// 放行不得依赖快照装配。
		clearCursorQuotaKeySourceProvidersForTest(t)
		InstallCursorQuotaKeyStateProvider(cursorQuotaKeySourceStub{
			statuses: map[int64]string{qkID: domain.QuotaKeyStatusActive},
		})
		account := compatAccount(9913, qkID)

		reason, err := cursorSupplySourceAvailabilityVetoReason(ctx, account)
		require.NoError(t, err)
		require.Empty(t, reason, "绑 active 码的兼容型账号必须放行")
	})

	t.Run("unbound compatible account still passes", func(t *testing.T) {
		// 未绑码的兼容账号没有可判的码状态：维持既有放行意图（不得退化为
		// quota_key_source_unbound）。
		clearCursorQuotaKeySourceProvidersForTest(t)
		account := compatAccount(9914, 0)

		reason, err := cursorSupplySourceAvailabilityVetoReason(ctx, account)
		require.NoError(t, err)
		require.Empty(t, reason, "未绑码的兼容型账号维持放行")
		require.NotEqual(t, quotaKeySourceUnbound, reason)
	})
}
