//go:build unit

package service

// 本文件定向覆盖「会话级 rejection tombstone」（派发单 CURSOR-B2）。
//
// 根因：原 noteRejectedCall 只扫描「当前已 started 的批」做标记。拒绝先到、started
// 后到时标记落空 → 该批永远封不了口 → 180s 总期限挂死。本卡在 cursorToolDelegations
// 上加会话级墓碑：noteRejectedCall 无条件登记，noteToolCallStarted 在持锁下原子消费
// （消费即删，不跨批/跨会话复用），把 call 记入所属批的 rejected 集合，于是封口谓词
// 跳过它。
//
// 覆盖 5 类场景 + 无法归属失败关闭：
//  1. 拒绝先到 → started 后到：登记墓碑后 started 到达，该批能封口；
//  2. started 先到 → 拒绝后到：同样能封口；
//  3. 墓碑不跨会话复用：新会话同一 call_id 不命中旧会话墓碑；
//  4. 墓碑不跨批复用：同一 call_id 被一个批消费后，另一个批不得再消费；
//  5. 过期墓碑 → 失败关闭（错误含 call_id）；
//  6. 无法归属的墓碑（无 model_call_id + 多开放批）→ 失败关闭（错误含 call_id）。
//
// 全部纯内存组件测试：无 DB、无网络。直接驱动 delegations 方法构造场景，不依赖其它卡
// 尚未改动的封口逻辑。

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCursorRejectionTombstoneRejectBeforeStart 覆盖「拒绝先到、started 后到」：
// 登记墓碑后再登记 started，随后该批能封口（不再因缺 mcp 委托而失败关闭）。
func TestCursorRejectionTombstoneRejectBeforeStart(t *testing.T) {
	d := newCursorToolDelegations()

	// 本批声明 1 条；该 call 被本地拒绝（客户端无工具，不会有 mcp 委托）。
	require.NoError(t, d.noteToolRequestsListed("mc-1", 1))
	// 拒绝先到：此刻还没有 started 批，旧逻辑会落空；新逻辑无条件登记墓碑。
	d.noteRejectedCall("call_rej")
	// started 后到：原子消费墓碑并记入所属批 rejected（消费即删，不跨批/会话复用）。
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_rej", true))
	// 该批能封口（不再因缺 mcp 委托而失败关闭）。
	require.NoError(t, d.seal(), "拒绝先到+started后到：墓碑消费后该批必须能封口")
}

// TestCursorRejectionTombstoneStartBeforeReject 覆盖「started 先到、拒绝后到」：同样能封口。
func TestCursorRejectionTombstoneStartBeforeReject(t *testing.T) {
	d := newCursorToolDelegations()

	require.NoError(t, d.noteToolRequestsListed("mc-1", 1))
	// started 先到。
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_rej", true))
	// 拒绝后到：此时 call_rej 已在 started 列表，就地标记 rejected（同时登记墓碑）。
	d.noteRejectedCall("call_rej")
	require.NoError(t, d.seal(), "started先到+拒绝后到：该批必须能封口")
}

// TestCursorRejectionTombstoneNoCrossSession 覆盖「墓碑不跨会话复用」：
// 新会话里同一 call_id 不命中旧会话的墓碑（墓碑随会话 delegations 释放）。
func TestCursorRejectionTombstoneNoCrossSession(t *testing.T) {
	d1 := newCursorToolDelegations()
	d1.noteRejectedCall("call_shared") // 会话1 登记墓碑

	// 会话2（全新 delegations 实例）对同一 call_id 不应命中会话1的墓碑。
	d2 := newCursorToolDelegations()
	require.NoError(t, d2.noteToolRequestsListed("mc-1", 1))
	require.NoError(t, d2.noteToolCallStarted("mc-1", "call_shared", true))
	// 因为没命中墓碑，call_shared 没有进入 rejected，也无委托 → 封口应当失败。
	err := d2.seal()
	require.ErrorIs(t, err, ErrCursorBatchIncomplete,
		"新会话不得命中旧会话墓碑：缺委托且无 rejected 必须失败关闭")
	require.Contains(t, err.Error(), "call_shared")
}

// TestCursorRejectionTombstoneNoCrossBatch 覆盖「墓碑不跨批复用」：
// 同一 call_id 的墓碑被一个批消费后，另一个批不得再消费它。
func TestCursorRejectionTombstoneNoCrossBatch(t *testing.T) {
	d := newCursorToolDelegations()
	d.noteRejectedCall("call_rej") // 登记墓碑

	// 批1 消费墓碑（started 到达）。
	require.NoError(t, d.noteToolRequestsListed("mc-1", 1))
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_rej", true))

	// 另一个批（mc-2）的同一 call_id 到达：墓碑已被批1消费，不得再命中。
	require.NoError(t, d.noteToolRequestsListed("mc-2", 1))
	require.NoError(t, d.noteToolCallStarted("mc-2", "call_rej", true))

	// 批1 能封口（rejected 在批1），批2 缺委托且无 rejected → 整体封口失败，
	// 且错误指向 call_rej，证明批2 没消费墓碑（不跨批复用）。
	err := d.seal()
	require.ErrorIs(t, err, ErrCursorBatchIncomplete,
		"墓碑已被批1消费，批2 不得再命中：批2 必须失败关闭")
	require.Contains(t, err.Error(), "call_rej")
}

// TestCursorRejectionTombstoneExpiredFailsClosed 覆盖「过期墓碑 → 失败关闭」：
// 墓碑登记时刻距今超过本会话 180s 总期限即失败关闭（错误含 call_id），绝不静默忽略。
//
// 通过临时把 TTL 收紧到 1ns 构造过期（生产语义恒为 180s），避免 180s 真睡。
func TestCursorRejectionTombstoneExpiredFailsClosed(t *testing.T) {
	orig := cursorRejectionTombstoneTTL
	cursorRejectionTombstoneTTL = time.Nanosecond
	defer func() { cursorRejectionTombstoneTTL = orig }()

	d := newCursorToolDelegations()
	d.noteRejectedCall("call_rej") // 登记墓碑
	require.NoError(t, d.noteToolRequestsListed("mc-1", 1))
	// 两次调用之间必然流逝 > 1ns，墓碑即过期。
	err := d.noteToolCallStarted("mc-1", "call_rej", true)
	require.ErrorIs(t, err, ErrCursorBatchIncomplete,
		"过期墓碑在 started 到达时必须失败关闭")
	require.Contains(t, err.Error(), "call_rej", "错误信息必须能看出 call_id")
	require.Contains(t, err.Error(), "expired", "错误信息必须标明过期")
}

// TestCursorRejectionTombstoneUnattributableFailsClosed 覆盖「无法归属的墓碑 → 失败关闭」：
// started 到达时该墓碑既无匹配的开放批 started、也无法归到唯一可承载批（多个无 ID 开放批）
// → 失败关闭（错误含 call_id），绝不静默忽略。
func TestCursorRejectionTombstoneUnattributableFailsClosed(t *testing.T) {
	d := newCursorToolDelegations()
	// 两个携带 started 的开放批（不同 model_call_id），制造批量归属含糊。
	require.NoError(t, d.noteToolCallStarted("mc-A", "call_a", true))
	require.NoError(t, d.noteToolCallStarted("mc-B", "call_b", true))
	// 登记墓碑。
	d.noteRejectedCall("call_rej")
	// 无 model_call_id 的 started 到达：batchFor("") 因 ≥2 开放批含糊失败，
	// 墓碑无法归属 → 失败关闭。
	err := d.noteToolCallStarted("", "call_rej", true)
	require.ErrorIs(t, err, ErrCursorBatchAmbiguous,
		"无法归属的墓碑必须失败关闭")
	require.Contains(t, err.Error(), "call_rej", "错误信息必须能看出 call_id")
}
