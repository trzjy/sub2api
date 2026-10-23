//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// A04-G：来源门（§25.17.3「A04 唯一修改点」）单测。
//
// 只读包内既有夹具（stub repo / 包级 provider 安装），不连 DB、不引入 mock 框架。
// 解析一律复用 ParseQuotaKeyUsageSnapshot（不自建第二份解析）。
//
// 被测门只有一处判定：cursorSupplySourceAvailabilityVetoReason（及其 usage 子谓词
// cursorSupplySourceUsageVetoReason）。断言口径固定为二分离：
//   - 依赖未装配 / 读取失败 / 解析失败 → 明确 error；
//   - 合法但缺失 / 过期 / 耗尽 / 无有效归属码 → 非空 reason 且 err == nil。

// cursorQuotaKeySourceStub 同时实现 CursorQuotaKeyStateProvider 与
// CursorQuotaKeyUsageSnapshotProvider 两个窄读取接口，供来源门测试一次装齐。
type cursorQuotaKeySourceStub struct {
	statuses    map[int64]string
	statusErr   error
	snapshots   map[int64][]byte
	snapshotErr error
}

func (s cursorQuotaKeySourceStub) CursorQuotaKeyStatus(_ context.Context, id int64) (string, error) {
	if s.statusErr != nil {
		return "", s.statusErr
	}
	if st, ok := s.statuses[id]; ok {
		return st, nil
	}
	return "", errors.New("quota key not found")
}

func (s cursorQuotaKeySourceStub) CursorQuotaKeyUsageSnapshot(_ context.Context, id int64) ([]byte, error) {
	if s.snapshotErr != nil {
		return nil, s.snapshotErr
	}
	if raw, ok := s.snapshots[id]; ok {
		return raw, nil
	}
	return nil, nil
}

// installCursorQuotaKeySourceForTest 同时安装状态与额度快照两个窄读取器，并在测试结束时
// 还原包级实例，避免污染同包其它用例（包级单例是共享状态）。
func installCursorQuotaKeySourceForTest(t *testing.T, stub cursorQuotaKeySourceStub) {
	t.Helper()
	cursorQuotaKeyStateMu.Lock()
	prevState := cursorQuotaKeyStateInstance
	cursorQuotaKeyStateMu.Unlock()
	cursorQuotaKeyUsageSnapshotMu.Lock()
	prevUsage := cursorQuotaKeyUsageSnapshotInstance
	cursorQuotaKeyUsageSnapshotMu.Unlock()

	InstallCursorQuotaKeyStateProvider(stub)
	InstallCursorQuotaKeyUsageSnapshotProvider(stub)

	t.Cleanup(func() {
		cursorQuotaKeyStateMu.Lock()
		cursorQuotaKeyStateInstance = prevState
		cursorQuotaKeyStateMu.Unlock()
		cursorQuotaKeyUsageSnapshotMu.Lock()
		cursorQuotaKeyUsageSnapshotInstance = prevUsage
		cursorQuotaKeyUsageSnapshotMu.Unlock()
	})
}

// clearCursorQuotaKeySourceProvidersForTest 把两个窄读取器都置空，供"依赖未装配"用例
// 精确控制装配形态；测试结束还原。
func clearCursorQuotaKeySourceProvidersForTest(t *testing.T) {
	t.Helper()
	cursorQuotaKeyStateMu.Lock()
	prevState := cursorQuotaKeyStateInstance
	cursorQuotaKeyStateInstance = nil
	cursorQuotaKeyStateMu.Unlock()
	cursorQuotaKeyUsageSnapshotMu.Lock()
	prevUsage := cursorQuotaKeyUsageSnapshotInstance
	cursorQuotaKeyUsageSnapshotInstance = nil
	cursorQuotaKeyUsageSnapshotMu.Unlock()

	t.Cleanup(func() {
		cursorQuotaKeyStateMu.Lock()
		cursorQuotaKeyStateInstance = prevState
		cursorQuotaKeyStateMu.Unlock()
		cursorQuotaKeyUsageSnapshotMu.Lock()
		cursorQuotaKeyUsageSnapshotInstance = prevUsage
		cursorQuotaKeyUsageSnapshotMu.Unlock()
	})
}

func cursorSourceTestAccount(qkID int64) *Account {
	return &Account{
		ID:          qkID,
		Platform:    PlatformCursor,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		QuotaKeyID:  &qkID,
	}
}

// TestCursorSupplySourceAvailabilityGateMissingBranches 逐条钉死「缺失/错误」类分支：
// 无有效归属码、依赖未装配、来源不存在/已删、快照读失败、NULL/空快照、字段缺失、解析失败。
// 这些情形修复前全部"放行"（快照缺失不否决、provider 为 nil 不收紧），是本单的核心回归面。
func TestCursorSupplySourceAvailabilityGateMissingBranches(t *testing.T) {
	const qkID int64 = 42
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prevNow := cursorSupplySourceUsageNow
	cursorSupplySourceUsageNow = func() time.Time { return now }
	t.Cleanup(func() { cursorSupplySourceUsageNow = prevNow })

	freshCheckedAt := now.Add(-time.Minute).Format(time.RFC3339)

	t.Run("no valid quota key id vetoes with reason", func(t *testing.T) {
		// 无有效 QuotaKeyID（nil / ≤0）：来源不可判定即不可用，必须给出非空 reason。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: []byte(`{"total":400,"used":86}`)},
		})
		unbound := cursorSourceTestAccount(qkID)
		unbound.QuotaKeyID = nil
		reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), unbound)
		require.NoError(t, err)
		require.Equal(t, quotaKeySourceUnbound, reason, "未绑定来源不得放行")

		zero := cursorSourceTestAccount(qkID)
		zeroID := int64(0)
		zero.QuotaKeyID = &zeroID
		reason, err = cursorSupplySourceAvailabilityVetoReason(context.Background(), zero)
		require.NoError(t, err)
		require.Equal(t, quotaKeySourceUnbound, reason, "非法（≤0）来源 id 不得放行")
	})

	t.Run("state provider not wired returns explicit error", func(t *testing.T) {
		clearCursorQuotaKeySourceProvidersForTest(t)
		InstallCursorQuotaKeyUsageSnapshotProvider(cursorQuotaKeySourceStub{
			snapshots: map[int64][]byte{qkID: []byte(fmt.Sprintf(
				`{"total":400,"used":86,"status_code":200,"checked_at":%q,"error_code":""}`, freshCheckedAt))},
		})
		reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.Error(t, err, "状态读取依赖未装配是装配错误，必须显式失败")
		require.Contains(t, err.Error(), "state provider is not wired")
		require.Empty(t, reason)
	})

	t.Run("usage snapshot provider not wired returns explicit error", func(t *testing.T) {
		clearCursorQuotaKeySourceProvidersForTest(t)
		InstallCursorQuotaKeyStateProvider(cursorQuotaKeySourceStub{
			statuses: map[int64]string{qkID: domain.QuotaKeyStatusActive},
		})
		reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.Error(t, err, "快照读取依赖未装配是装配错误，必须显式失败")
		require.Contains(t, err.Error(), "usage snapshot provider is not wired")
		require.Empty(t, reason)
	})

	t.Run("source missing or deleted returns explicit error", func(t *testing.T) {
		// 来源不存在/已软删：读取器如实返回 error（not found），门必须上抛而不是当"无否决"。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{},
		})
		reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.Error(t, err)
		require.Contains(t, err.Error(), "state read failed")
		require.Empty(t, reason)
	})

	t.Run("source expired status vetoes with reason", func(t *testing.T) {
		// 来源存在但不可用（终态过期）：合法结论 → 非空 reason。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusExpired},
			snapshots: map[int64][]byte{qkID: []byte(`{"total":400,"used":86}`)},
		})
		reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.NoError(t, err)
		require.Equal(t, "quota_key_expired", reason)
	})

	t.Run("snapshot read failure returns explicit error", func(t *testing.T) {
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:    map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshotErr: errors.New("db unavailable"),
		})
		reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.Error(t, err)
		require.Contains(t, err.Error(), "usage snapshot read failed")
		require.Empty(t, reason)
	})

	t.Run("NULL and empty snapshots veto with reason", func(t *testing.T) {
		// NULL 快照 / 空字节 / "null" / 全字段缺失的空对象：都等价"从未观测"→ 否决。
		for name, raw := range map[string][]byte{
			"null value":    nil,
			"empty bytes":   []byte(""),
			"json null":     []byte("null"),
			"empty object":  []byte("{}"),
			"null counters": []byte(`{"total":null,"used":null,"error_code":""}`),
		} {
			t.Run(name, func(t *testing.T) {
				installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
					statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
					snapshots: map[int64][]byte{qkID: raw},
				})
				reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
				require.NoError(t, err)
				require.Equal(t, quotaKeyUsageUnobserved, reason, "缺失快照不得放行")
			})
		}
	})

	t.Run("missing total vetoes with reason", func(t *testing.T) {
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: []byte(fmt.Sprintf(
				`{"used":86,"status_code":200,"checked_at":%q,"error_code":""}`, freshCheckedAt))},
		})
		reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageIncomplete, reason, "total 缺失不得放行")
	})

	t.Run("missing used vetoes with reason", func(t *testing.T) {
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: []byte(fmt.Sprintf(
				`{"total":400,"status_code":200,"checked_at":%q,"error_code":""}`, freshCheckedAt))},
		})
		reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageIncomplete, reason, "used 缺失不得放行")
	})

	t.Run("parse failure returns explicit error", func(t *testing.T) {
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: []byte(`{not-valid-json`)},
		})
		reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.Error(t, err, "解析失败必须明确上抛 error")
		require.Contains(t, err.Error(), "parse failed")
		require.Empty(t, reason)
	})
}

// TestCursorSupplySourceAvailabilityGateUsageRules 钉死「合法但不可用」的剩余规则：
// 观测失败、缺 checked_at、超 5 分钟、剩余 ≤0 一律否决；真实 0 保持为 0（不是缺失）；
// 新鲜且有剩余才放行；非 cursor 平台不产生任何 Cursor 否决。
func TestCursorSupplySourceAvailabilityGateUsageRules(t *testing.T) {
	const qkID int64 = 42
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prevNow := cursorSupplySourceUsageNow
	cursorSupplySourceUsageNow = func() time.Time { return now }
	t.Cleanup(func() { cursorSupplySourceUsageNow = prevNow })

	freshCheckedAt := now.Add(-time.Minute).Format(time.RFC3339)
	staleCheckedAt := now.Add(-6 * time.Minute).Format(time.RFC3339)

	makeSnap := func(total, used int, errorCode, checkedAt string) []byte {
		return []byte(fmt.Sprintf(
			`{"total":%d,"used":%d,"status_code":200,"checked_at":%q,"error_code":%q}`,
			total, used, checkedAt, errorCode,
		))
	}

	gate := func(t *testing.T, snapshots map[int64][]byte) (string, error) {
		t.Helper()
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: snapshots,
		})
		return cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
	}

	t.Run("observation failed vetoes", func(t *testing.T) {
		// 最近一次观测失败（error_code 非空）→ 否决，不沿用保留数值。
		reason, err := gate(t, map[int64][]byte{qkID: makeSnap(400, 86, "lookup_http_500", freshCheckedAt)})
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageObservationFailed, reason)
	})

	t.Run("missing checked_at vetoes", func(t *testing.T) {
		reason, err := gate(t, map[int64][]byte{qkID: []byte(`{"total":400,"used":86,"status_code":200,"error_code":""}`)})
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageStale, reason)
	})

	t.Run("stale checked_at over five minutes vetoes", func(t *testing.T) {
		reason, err := gate(t, map[int64][]byte{qkID: makeSnap(400, 86, "", staleCheckedAt)})
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageStale, reason)
	})

	t.Run("exhausted total-used<=0 vetoes", func(t *testing.T) {
		reason, err := gate(t, map[int64][]byte{qkID: makeSnap(100, 100, "", freshCheckedAt)})
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageExhausted, reason)
	})

	t.Run("real zero stays zero and is not treated as missing", func(t *testing.T) {
		// 真实 0 保持为 0：total=0/used=0 是完整快照（剩余 0 → 耗尽），
		// 不得被误判成"快照缺失 / 字段缺失"。
		reason, err := gate(t, map[int64][]byte{qkID: makeSnap(0, 0, "", freshCheckedAt)})
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageExhausted, reason)
		require.NotEqual(t, quotaKeyUsageUnobserved, reason, "真实 0 不是缺失")
		require.NotEqual(t, quotaKeyUsageIncomplete, reason, "真实 0 不是字段缺失")
	})

	t.Run("real zero total with real zero used boundary passes only above zero", func(t *testing.T) {
		// 边界：total-used==1 仍有剩余 → 放行（0 与 1 的判定必须分开）。
		reason, err := gate(t, map[int64][]byte{qkID: makeSnap(100, 99, "", freshCheckedAt)})
		require.NoError(t, err)
		require.Empty(t, reason)
	})

	t.Run("fresh snapshot with remaining passes", func(t *testing.T) {
		reason, err := gate(t, map[int64][]byte{qkID: makeSnap(400, 86, "", freshCheckedAt)})
		require.NoError(t, err)
		require.Empty(t, reason)
	})

	t.Run("non cursor account never vetoed by cursor gate", func(t *testing.T) {
		// 非 Cursor → 无 Cursor 否决，即便快照是耗尽/缺失形态。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: makeSnap(100, 100, "", freshCheckedAt)},
		})
		account := cursorSourceTestAccount(qkID)
		account.Platform = PlatformOpenAI
		reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), account)
		require.NoError(t, err)
		require.Empty(t, reason)

		reason, err = cursorSupplySourceAvailabilityVetoReason(context.Background(), nil)
		require.NoError(t, err)
		require.Empty(t, reason)
	})
}

// TestCursorSupplyAvailability_ExhaustedRejectsAccountAvailable 钉死「来源耗尽而账号自身
// 可用仍拒绝」：账号归属码状态为 active（账号自身可服务），仅来源真实剩余 ≤0，来源门必须
// 否决——来源可用性 AND 账号可用性，二者缺一不可。
func TestCursorSupplyAvailability_ExhaustedRejectsAccountAvailable(t *testing.T) {
	const qkID int64 = 42
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prevNow := cursorSupplySourceUsageNow
	cursorSupplySourceUsageNow = func() time.Time { return now }
	t.Cleanup(func() { cursorSupplySourceUsageNow = prevNow })

	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{qkID: domain.QuotaKeyStatusActive},
		snapshots: map[int64][]byte{qkID: []byte(fmt.Sprintf(
			`{"total":100,"used":100,"status_code":200,"checked_at":%q,"error_code":""}`,
			now.Add(-time.Minute).Format(time.RFC3339),
		))},
	})
	reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), cursorSourceTestAccount(qkID))
	require.NoError(t, err)
	require.Equal(t, quotaKeyUsageExhausted, reason, "来源耗尽必须否决，即便账号自身状态可用")
}

// TestCursorSupplyAvailability_SourceRecoveryDoesNotClearAccountTempPause 钉死「来源恢复
// 不清账号临停」：来源快照恢复为新鲜可用后，来源共同否决解除（reason 为空），但账号自身的
// 临时不可调度（TempUnschedulableUntil）仍独立生效——来源门只解来源这一维，绝不触碰账号
// 管理态。两个维度由各自判定负责，互不清算。
func TestCursorSupplyAvailability_SourceRecoveryDoesNotClearAccountTempPause(t *testing.T) {
	const qkID int64 = 42
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prevNow := cursorSupplySourceUsageNow
	cursorSupplySourceUsageNow = func() time.Time { return now }
	t.Cleanup(func() { cursorSupplySourceUsageNow = prevNow })

	account := cursorSourceTestAccount(qkID)
	// 临停必须在 IsSchedulable() 判定的真实时钟（time.Now）未来：来源门用冻结的
	// cursorSupplySourceUsageNow 场景时钟，而账号资格门用真实墙钟，若按场景时钟设临停
	// 会被真实时钟判定为已过期（跨日运行必挂）。
	future := time.Now().Add(time.Hour)
	account.TempUnschedulableUntil = &future // 账号自身临停（来源恢复后仍在冷却）

	installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
		statuses: map[int64]string{qkID: domain.QuotaKeyStatusActive},
		snapshots: map[int64][]byte{qkID: []byte(fmt.Sprintf(
			`{"total":400,"used":86,"status_code":200,"checked_at":%q,"error_code":""}`,
			now.Add(-time.Minute).Format(time.RFC3339),
		))},
	})
	// 来源已恢复 → 来源共同否决解除。
	reason, err := cursorSupplySourceAvailabilityVetoReason(context.Background(), account)
	require.NoError(t, err)
	require.Empty(t, reason, "来源恢复后来源门不得再否决")
	// 但账号自身临停仍独立生效（由账号资格门负责，不在此来源门内清除）。
	require.False(t, account.IsSchedulable(), "来源恢复不得清除账号自身临停冷却")
}

// TestCursorWaterLevelConsumesSameSourceGate 钉死「水位与调度消费同一来源门」：来源耗尽的
// 账号在水位计数路径上不计入（与调度候选粗筛走同一 cursorSupplySourceAvailabilityVetoReason
// 谓词），来源未被否决的同一账号则计入——两路径共享唯一来源门，无第二实现。
func TestCursorWaterLevelConsumesSameSourceGate(t *testing.T) {
	const qkID int64 = 1
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prevNow := cursorSupplySourceUsageNow
	cursorSupplySourceUsageNow = func() time.Time { return now }
	t.Cleanup(func() { cursorSupplySourceUsageNow = prevNow })

	row := func() *ent.Account {
		keyID := qkID
		return &ent.Account{
			ID:          qkID,
			Platform:    PlatformCursor,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Credentials: map[string]any{"model_mapping": map[string]any{"grok-4.7": "grok-4.7"}},
			QuotaKeyID:  &keyID,
			Edges: ent.AccountEdges{
				QuotaKey: &ent.QuotaKey{ID: qkID, Status: domain.QuotaKeyStatusActive},
				Groups: []*ent.Group{{ID: 1, Platform: PlatformCursor, Status: StatusActive,
					ModelAllowlist: domain.GroupModelAllowlist{Enabled: true, Models: []string{"grok-4.7"}}}},
			},
		}
	}

	t.Run("exhausted source not counted", func(t *testing.T) {
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: []byte(fmt.Sprintf(
				`{"total":100,"used":100,"status_code":200,"checked_at":%q,"error_code":""}`,
				now.Add(-time.Minute).Format(time.RFC3339),
			))},
		})
		// 来源否决在 routing 之前短路：无需依赖齐备即可断言不计入。
		got, err := cursorAccountCountsTowardWaterLevel(context.Background(), row())
		require.NoError(t, err)
		require.False(t, got, "来源耗尽的账号不计入水位（与调度同谓词）")
	})

	t.Run("fresh source counted", func(t *testing.T) {
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses: map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: []byte(fmt.Sprintf(
				`{"total":400,"used":86,"status_code":200,"checked_at":%q,"error_code":""}`,
				now.Add(-time.Minute).Format(time.RFC3339),
			))},
		})
		// 来源新鲜：走完整 routing（依赖齐备），与调度候选粗筛同谓词地计入。
		got, err := wiredWaterAllCursor().countsTowardWaterLevel(context.Background(), row())
		require.NoError(t, err)
		require.True(t, got, "来源新鲜可用的账号计入水位（与调度同谓词）")
	})
}
