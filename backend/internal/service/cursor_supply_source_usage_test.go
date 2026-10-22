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

// W2-B2：来源门五规则 + 调度/水位同谓词的单元测试。
//
// 只读包内既有夹具（stub repo / 包级 provider 安装），不连 DB、不引入 mock 框架。
// 解析一律复用 ParseQuotaKeyUsageSnapshot（不自建第二份解析）。

// cursorQuotaKeySourceStub 同时实现 CursorQuotaKeyStateProvider 与
// CursorQuotaKeyUsageSnapshotProvider 两个窄读取接口，供来源门测试一次装齐。
type cursorQuotaKeySourceStub struct {
	statuses  map[int64]string
	statusErr error
	snapshots map[int64][]byte
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

func TestCursorSupplySourceUsageVetoReason_FiveRules(t *testing.T) {
	const qkID int64 = 42
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prevNow := cursorSupplySourceUsageNow
	cursorSupplySourceUsageNow = func() time.Time { return now }
	t.Cleanup(func() { cursorSupplySourceUsageNow = prevNow })

	freshCheckedAt := now.Add(-time.Minute).Format(time.RFC3339)
	staleCheckedAt := now.Add(-6 * time.Minute).Format(time.RFC3339)

	// 规则④/⑤ 的快照构造：total/used/checked_at/error_code 均可调。
	makeSnap := func(total, used int, errorCode, checkedAt string) []byte {
		return []byte(fmt.Sprintf(
			`{"total":%d,"used":%d,"status_code":200,"checked_at":%q,"error_code":%q}`,
			total, used, checkedAt, errorCode,
		))
	}

	t.Run("missing snapshot does not veto", func(t *testing.T) {
		// 规则②：快照缺失 → 不因快照否决（走既有来源过期/缺失规则）。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{},
		})
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.NoError(t, err)
		require.Empty(t, reason, "缺失快照不得否决（来源门不代替缺失规则）")
	})

	t.Run("observation failed vetoes (fail closed)", func(t *testing.T) {
		// 规则①：最近一次观测失败（error_code 非空）→ 否决，不沿用保留数值。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: makeSnap(400, 86, "lookup_http_500", freshCheckedAt)},
		})
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageObservationFailed, reason)
	})

	t.Run("stale checked_at vetoes", func(t *testing.T) {
		// 规则③：checked_at 距今 > 5 分钟 → 否决（等待 reconciler/管理员刷新）。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: makeSnap(400, 86, "", staleCheckedAt)},
		})
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageStale, reason)
	})

	t.Run("missing checked_at vetoes (treated stale)", func(t *testing.T) {
		// 规则③：成功值存在但 checked_at 缺失 → 无法判定新鲜度，失败关闭为否决。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: []byte(`{"total":400,"used":86,"status_code":200,"error_code":""}`)},
		})
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageStale, reason)
	})

	t.Run("exhausted total-used<=0 vetoes", func(t *testing.T) {
		// 规则④：真实剩余 total-used ≤ 0 → 否决。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: makeSnap(100, 100, "", freshCheckedAt)},
		})
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.NoError(t, err)
		require.Equal(t, quotaKeyUsageExhausted, reason)
	})

	t.Run("exhausted boundary total-used==1 passes", func(t *testing.T) {
		// 规则④ 边界：total-used==1 仍有剩余 → 不否决。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: makeSnap(100, 99, "", freshCheckedAt)},
		})
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.NoError(t, err)
		require.Empty(t, reason)
	})

	t.Run("parse failure returns explicit error", func(t *testing.T) {
		// 规则⑤：坏 JSON → 明确 error（不是空 reason），由调用方失败关闭。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: []byte(`{not-valid-json`)},
		})
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.Error(t, err, "解析失败必须明确上抛 error")
		require.Empty(t, reason)
	})

	t.Run("fresh snapshot passes", func(t *testing.T) {
		// 快照新鲜（<5min）且未耗尽 → 放行。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			statuses:  map[int64]string{qkID: domain.QuotaKeyStatusActive},
			snapshots: map[int64][]byte{qkID: makeSnap(400, 86, "", freshCheckedAt)},
		})
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), cursorSourceTestAccount(qkID))
		require.NoError(t, err)
		require.Empty(t, reason)
	})
}

func TestCursorSupplySourceUsageVetoReason_OutOfScope(t *testing.T) {
	const qkID int64 = 42

	t.Run("non-cursor account not vetoed", func(t *testing.T) {
		account := cursorSourceTestAccount(qkID)
		account.Platform = PlatformOpenAI
		// 即使装了来源读取器也不应影响非 cursor 账号。
		installCursorQuotaKeySourceForTest(t, cursorQuotaKeySourceStub{
			snapshots: map[int64][]byte{qkID: []byte(`{"total":0,"used":0,"error_code":""}`)},
		})
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), account)
		require.NoError(t, err)
		require.Empty(t, reason)
	})

	t.Run("unbound account not vetoed", func(t *testing.T) {
		account := cursorSourceTestAccount(qkID)
		account.QuotaKeyID = nil
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), account)
		require.NoError(t, err)
		require.Empty(t, reason)
	})

	t.Run("no snapshot provider installed degrades to no veto", func(t *testing.T) {
		// 来源读取器未装配（缩编/单元场景）按既有降级口径不收紧，与
		// codeBuddyQuotaInstance 为 nil 时一致。
		cursorQuotaKeyUsageSnapshotMu.Lock()
		prev := cursorQuotaKeyUsageSnapshotInstance
		cursorQuotaKeyUsageSnapshotInstance = nil
		cursorQuotaKeyUsageSnapshotMu.Unlock()
		t.Cleanup(func() {
			cursorQuotaKeyUsageSnapshotMu.Lock()
			cursorQuotaKeyUsageSnapshotInstance = prev
			cursorQuotaKeyUsageSnapshotMu.Unlock()
		})
		reason, err := cursorSupplySourceUsageVetoReason(context.Background(), cursorSourceTestAccount(qkID))
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
	future := now.Add(time.Hour)
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
