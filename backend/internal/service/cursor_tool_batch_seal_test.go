//go:build unit

package service

// 本文件是 B2 卡（派发单 B2）的定向测试：cursorToolDelegations 按 call_id 归批
// + 封口条件以 listed 为准。
//
// 覆盖：
//  D1 归批：2 个开放批各含不同 call_id 的 started；委派按 call_id 落到正确批次；
//           无匹配 / 多匹配时按 batchFor("") 语义失败关闭。
//  D2 封口条件：
//      ① 批次无 listed → sealable() 返回 ErrCursorBatchIncomplete（不得 nil）；
//      ② listed=2、started=1 → 不封口；
//      ③ listed=2、started=2 且两条都有委派 → 封口成功（nil）；
//      ④ listed=1、started=1（对外）+ internal=1 → 不封口（计数不等）。
//  D3 重复 ID 仍失败关闭；空 ID 仍失败关闭。
//
// 全部是纯内存组件测试：无 DB、无网络、无 DB 事务。复用同包的 cursorBatchDelegation
// 构造委托捕获，避免重复符号。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCursorB2DelegationBatchesByCallID 覆盖 D1：按 call_id 归批与失败关闭语义。
func TestCursorB2DelegationBatchesByCallID(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolCallStarted("mc-A", "call_A", true))
	require.NoError(t, d.noteToolCallStarted("mc-B", "call_B", true))

	// 委派按 call_id 落到正确批次（started 列表硬链接，不靠 model_call_id）。
	require.NoError(t, d.noteDelegation("", cursorBatchDelegation("call_A", "get_weather")))
	require.NoError(t, d.noteDelegation("", cursorBatchDelegation("call_B", "get_weather")))

	calls := d.snapshot()
	require.Len(t, calls, 2, "两条委派必须各归各批")
	require.Equal(t, "call_A", calls[0].ID, "call_A 必须落在 mc-A 批")
	require.Equal(t, "call_B", calls[1].ID, "call_B 必须落在 mc-B 批")

	// 无匹配：call_id 不在任何 started → 回退 batchFor("")，多开放批 → 歧义失败关闭。
	err := d.noteDelegation("", cursorBatchDelegation("call_X", "get_weather"))
	require.ErrorIs(t, err, ErrCursorBatchAmbiguous, "无匹配 call_id 必须按 batchFor(\"\") 语义失败关闭")

	// 多匹配：同一 call_id 落在两个批次的 started → 同样回退 batchFor("") 歧义失败关闭。
	d2 := newCursorToolDelegations()
	require.NoError(t, d2.noteToolCallStarted("mc-A", "call_shared", true))
	require.NoError(t, d2.noteToolCallStarted("mc-B", "call_shared", true))
	err2 := d2.noteDelegation("", cursorBatchDelegation("call_shared", "get_weather"))
	require.ErrorIs(t, err2, ErrCursorBatchAmbiguous, "多匹配 call_id 必须按 batchFor(\"\") 语义失败关闭")
}

// TestCursorB2SealRequiresListed 覆盖 D2①：无 listed 的批次不得交接（含空开放批）。
func TestCursorB2SealRequiresListed(t *testing.T) {
	d := newCursorToolDelegations()
	// 有 started 且有委派，但从未收到 listed：缺 listed 时交接会丢工具，必须 fail-closed。
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_1", true))
	require.NoError(t, d.noteDelegation("", cursorBatchDelegation("call_1", "get_weather")))

	err := d.sealable()
	require.ErrorIs(t, err, ErrCursorBatchIncomplete, "无 listed 的批次不得交接")
	require.Contains(t, err.Error(), "listed=absent")
}

// TestCursorB2SealListedAboveStarted 覆盖 D2②：listed=2、started=1 → 不封口。
func TestCursorB2SealListedAboveStarted(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolRequestsListed("mc-1", 2))
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_1", true))
	require.NoError(t, d.noteDelegation("", cursorBatchDelegation("call_1", "get_weather")))

	err := d.sealable()
	require.ErrorIs(t, err, ErrCursorBatchIncomplete, "listed=2 但 started=1 必须等齐")
	require.Contains(t, err.Error(), "listed=2")
}

// TestCursorB2SealBothMatchSeals 覆盖 D3③（D2③）：listed=2、started=2 且都有委派 → 封口。
func TestCursorB2SealBothMatchSeals(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolRequestsListed("mc-1", 2))
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_1", true))
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_2", true))
	require.NoError(t, d.noteDelegation("", cursorBatchDelegation("call_1", "get_weather")))
	require.NoError(t, d.noteDelegation("", cursorBatchDelegation("call_2", "get_weather")))

	require.NoError(t, d.sealable(), "listed=2 started=2 且都有委派必须可封口")
	require.NoError(t, d.seal(), "seal 必须成功")
	require.Len(t, d.snapshot(), 2)
}

// TestCursorB2SealInternalPlusExternalNotEqualListed 覆盖 D2④：
// listed=1、started=1（对外）+ internal=1 → 计数不等，不封口。
func TestCursorB2SealInternalPlusExternalNotEqualListed(t *testing.T) {
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolRequestsListed("mc-1", 1))
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_1", true)) // 对外
	require.NoError(t, d.noteToolCallStarted("mc-1", "", false))      // 内部元工具
	require.NoError(t, d.noteDelegation("", cursorBatchDelegation("call_1", "get_weather")))

	err := d.sealable()
	require.ErrorIs(t, err, ErrCursorBatchIncomplete, "对外+内部计数不等 listed 时必须不封口")
	require.Contains(t, err.Error(), "listed=1")
}

// TestCursorB2DuplicateAndEmptyDelegationFailClosed 覆盖 D3：
// 重复 ID 仍失败关闭；空 ID 仍失败关闭。
func TestCursorB2DuplicateAndEmptyDelegationFailClosed(t *testing.T) {
	// 重复 ID。
	d := newCursorToolDelegations()
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_dup", true))
	require.NoError(t, d.noteDelegation("", cursorBatchDelegation("call_dup", "get_weather")))
	err := d.noteDelegation("", cursorBatchDelegation("call_dup", "get_weather"))
	require.ErrorIs(t, err, ErrCursorProtocol, "重复委托 ID 必须按冲突拒绝")
	require.Contains(t, err.Error(), "duplicate tool delegation")

	// 空 ID。
	d2 := newCursorToolDelegations()
	err2 := d2.noteDelegation("", cursorToolCallCapture{ID: "", Name: "get_weather", ArgsJSON: `{}`})
	require.ErrorIs(t, err2, ErrCursorProtocol, "无 tool_call_id 的委托必须失败关闭")
	require.Contains(t, err2.Error(), "mcp_args without tool_call_id")
}
