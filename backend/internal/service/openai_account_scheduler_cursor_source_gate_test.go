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
