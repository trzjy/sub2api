//go:build unit

package service

// 本文件是派发单 B7（外审 F2/F3 整改）的定向测试：委派与交付集合双向一致。
//
// 覆盖派发单 §3 的三处整改与 Done when D1/D2/D3：
//   D1  一条没有对应 started 的委派不得并入已声明 started 的批次，封口必须失败关闭；
//       去掉该 orphan 委派则正常封口；
//   D2  call_id 为空且恰好一个开放批 → 仍走 batchFor("")，行为与改前一致；
//   D3  两个已封口批 A/B 经 prepareDeliveredBatch 后 pending.callIDs 同时含 A、B 的
//       call_id，且统一索引登记了两批的 call_id（交付集合 = 登记集合）。
//
// 全部为纯内存组件测试：无 DB、无网络。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCursorDelegationSetF3OrphanDelegationFailsClosed 覆盖 D1 + §3.1/§3.2：
// 一条没有对应 started 的委派不得并入已声明 started 的批次，封口必须失败关闭（能指认该 call_id）。
func TestCursorDelegationSetF3OrphanDelegationFailsClosed(t *testing.T) {
	// §3.1 路径：started(a) + listed(1) + delegation(b) 即失败关闭（b 无对应 started）。
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolCallStarted("mc-1", "a", true))
	require.NoError(t, d.noteToolRequestsListed("mc-1", 1))
	err := d.noteDelegation("mc-1", cursorToolCallCapture{ID: "b", Name: "get_weather", ArgsJSON: `{"city":"X"}`})
	require.Error(t, err, "orphan delegation b（无对应 started）必须失败关闭")
	require.ErrorIs(t, err, ErrCursorBatchIncomplete, "单开放批带 started 但未命中 → incomplete 类错误")
	require.Contains(t, err.Error(), "b", "报错文案必须能指认 b 无 started")

	// 去掉 delegation(b) → 正常封口（started(a) + listed(1) + delegation(a)）。
	d2 := newCursorToolDelegations()
	require.NoError(t, d2.noteToolCallStarted("mc-1", "a", true))
	require.NoError(t, d2.noteToolRequestsListed("mc-1", 1))
	require.NoError(t, d2.noteDelegation("mc-1", cursorBatchDelegation("a", "get_weather")))
	require.NoError(t, d2.seal(), "去掉 orphan delegation 后必须正常封口")

	// §3.2 路径：即便直接构造"started(a) + delegation(b) + delegation(a)"的坏批次，
	// 封口反向检查也必须失败关闭并指认 b（delivery set 超出 listed）。
	d3 := newCursorToolDelegations()
	d3.mu.Lock()
	d3.all = append(d3.all, &cursorToolBatch{
		modelCallID: "mc-1",
		listed:      1,
		listedSeen:  true,
		started:     []string{"a"},
		delegations: []cursorToolCallCapture{
			cursorToolCallCapture{ID: "b", Name: "get_weather", ArgsJSON: `{"city":"X"}`},
			cursorToolCallCapture{ID: "a", Name: "get_weather", ArgsJSON: `{"city":"Y"}`},
		},
	})
	d3.mu.Unlock()
	sealErr := d3.sealable()
	require.Error(t, sealErr, "封口反向检查必须拒绝 delegation(b)（无对应 started）")
	require.ErrorIs(t, sealErr, ErrCursorBatchIncomplete)
	require.Contains(t, sealErr.Error(), "b", "反向检查报错文案必须指认 b")
}

// TestCursorDelegationSetEmptyCallIDUsesBatchFor 覆盖 D2 + §3.1：
// call_id 为空且恰好一个开放批 → 仍走 batchFor("")，行为与改前一致（不得失败关闭、不得猜归属）。
func TestCursorDelegationSetEmptyCallIDUsesBatchFor(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolCallStarted("mc-1", "a", true)) // 恰好一个开放批

	batch, err := d.batchForDelegation("")
	require.NoError(t, err, "call_id 为空必须仍走 batchFor(\"\")，行为与改前一致")
	require.NotNil(t, batch)
	require.Equal(t, []string{"a"}, batch.started, "必须落到那个唯一开放批，行为不变")
}

// TestCursorDelegationSetDeliveredEqualsRegistered 覆盖 D3 + §3.3：
// 两个各自 started+delegation+listed 的已封口批 A、B，经 prepareDeliveredBatch 后，
// pending.callIDs 同时含 A、B 的 call_id（并集），且统一索引登记了两批（交付集合 = 登记集合）。
func TestCursorDelegationSetDeliveredEqualsRegistered(t *testing.T) {
	svc := &CursorExecutionService{sessions: newCursorSessionStore()}
	record := svc.sessions.start(cursorSessionSpec{
		protocol:      cursorProtocolResponses,
		indexProtocol: "responses",
	})
	require.NotNil(t, record)

	// 构造两个已封口批 A、B（各自 started+delegation+listed）。
	d := newLocalCursorToolDelegations()
	require.NoError(t, d.noteToolRequestsListed("mc-A", 1))
	require.NoError(t, d.noteToolCallStarted("mc-A", "call_A", true))
	require.NoError(t, d.noteDelegation("mc-A", cursorToolCallCapture{ID: "call_A", Name: "get_weather", ArgsJSON: `{"city":"Beijing"}`}))
	require.NoError(t, d.noteToolRequestsListed("mc-B", 1))
	require.NoError(t, d.noteToolCallStarted("mc-B", "call_B", true))
	require.NoError(t, d.noteDelegation("mc-B", cursorToolCallCapture{ID: "call_B", Name: "get_weather", ArgsJSON: `{"city":"Shanghai"}`}))
	require.NoError(t, d.seal())

	prepared, err := svc.prepareDeliveredBatch(record, cursorResumeProtocolOfSpec(record.spec), "gpt-4o", "", d, "")
	require.NoError(t, err)
	require.NotNil(t, prepared)

	// pending.callIDs 必须同时含 A、B 的 call_id（并集，而非仅最后一批）。
	require.Contains(t, record.pending.callIDs, "call_A", "交付集合必须含批 A 的 call_id")
	require.Contains(t, record.pending.callIDs, "call_B", "交付集合必须含批 B 的 call_id")

	// 统一索引登记了两批的 call_id：两条均可定位，且同归最后一批的 ModelCallID（mc-B）。
	entryA, locErr := svc.sessions.index.Locate(cursorSessionIndexKeyForSpec(record.spec, CursorIDKindCall, "call_A"))
	require.NoError(t, locErr, "批 A 的 call_id 必须已登记进统一索引")
	entryB, locErr := svc.sessions.index.Locate(cursorSessionIndexKeyForSpec(record.spec, CursorIDKindCall, "call_B"))
	require.NoError(t, locErr, "批 B 的 call_id 必须已登记进统一索引")
	require.Equal(t, "mc-B", entryA.BatchID, "索引登记必须取最后一批的 ModelCallID")
	require.Equal(t, "mc-B", entryB.BatchID)
}
