//go:build unit

package service

// 本文件是派发单 BATCHSEQ 的定向测试：交付批改用**批次序号 seq** 判定"新批"，
// 修复 call_id 跨轮复用时新批被吞（生产故障：交付批 call_count=0 → 上游无结果 →
// stream_error）。
//
// 覆盖 Done when 四点：
//  1. 同一 call_id 在第二批复用 → 第二批仍被正确计入 deliveredCallIDs（不被吞）；
//  2. 没有任何新封口批 → prepareDeliveredBatch 明确报错，且绝不产生空 pending；
//  3. 多批一次封口 → seq 判定仍一次性交付全部新批；
//  4. deliveredBatchSeq 只在 commit 成功后才推进（准备成功但未提交不推进；提交失败不推进）。
//
// 全部为纯内存组件测试：无 DB、无网络。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// cursorBatchSeqFixture 是"一个活跃会话 + 一份委托记录"的最小夹具。
type cursorBatchSeqFixture struct {
	svc         *CursorExecutionService
	record      *cursorSessionRecord
	delegations *cursorToolDelegations
}

func newCursorBatchSeqFixture(t *testing.T) *cursorBatchSeqFixture {
	t.Helper()
	svc := &CursorExecutionService{sessions: newCursorSessionStore()}
	record := svc.sessions.start(cursorSessionSpec{
		protocol:      cursorProtocolResponses,
		indexProtocol: "responses",
	})
	require.NotNil(t, record)
	svc.sessions.attachOwner(record, svc)
	return &cursorBatchSeqFixture{
		svc:         svc,
		record:      record,
		delegations: newLocalCursorToolDelegations(),
	}
}

// prepare 走生产同一入口（服务锁内整批可失败准备）。
func (f *cursorBatchSeqFixture) prepare(t *testing.T) (*cursorPreparedBatch, error) {
	t.Helper()
	return f.svc.prepareDeliveredBatch(
		f.record,
		cursorResumeProtocolOfSpec(f.record.spec),
		"gpt-4o",
		"",
		f.delegations,
		"",
	)
}

// commit 走生产同一入口（写出成功后的提交）。
func (f *cursorBatchSeqFixture) commit(t *testing.T, prepared *cursorPreparedBatch) {
	t.Helper()
	require.NoError(t, f.svc.commitDeliveredBatch(f.record, prepared, nil))
}

// deliveredSeq 读会话已交付到的批次序号水位。
func (f *cursorBatchSeqFixture) deliveredSeq() uint64 {
	f.record.mu.Lock()
	defer f.record.mu.Unlock()
	return f.record.deliveredBatchSeq
}

// noteNativeFsBatch 用原生文件工具通道建一个独立批（shellOnly，≥1 条委托即可封口），
// 与生产故障现场一致（read/write 走 ExecServerMessage 的独立批）。
func (f *cursorBatchSeqFixture) noteNativeFsBatch(t *testing.T, kind, callID string) {
	t.Helper()
	require.NoError(t, f.delegations.noteNativeFsDelegation(kind, callID,
		cursorToolCallCapture{ID: callID, Kind: kind, Name: kind, ArgsJSON: `{"path":"/tmp/a"}`}))
}

// nextBatchSeqs 返回当前全部已封口批的 seq（断言 seq 单调分配用）。
func (f *cursorBatchSeqFixture) sealedSeqs() []uint64 {
	var out []uint64
	f.delegations.mu.Lock()
	defer f.delegations.mu.Unlock()
	for _, b := range f.delegations.all {
		if b.sealed {
			out = append(out, b.seq)
		}
	}
	return out
}

// TestCursorBatchSeqReusedCallIDNotSwallowed 覆盖 ①：同一 call_id 跨批复用（生产实测
// 同一会话内 read 与 write 共用 call-3466d322-...）时，第二批仍必须计入交付集合。
func TestCursorBatchSeqReusedCallIDNotSwallowed(t *testing.T) {
	f := newCursorBatchSeqFixture(t)
	const callID = "call-3466d322-d84c-4b2c-98a3-08ef52318bbe"

	// 第一轮：read 批交付并提交。
	f.noteNativeFsBatch(t, cursorToolKindRead, callID)
	require.NoError(t, f.delegations.seal())
	first, err := f.prepare(t)
	require.NoError(t, err)
	require.Equal(t, []string{callID}, first.callIDs, "第一轮必须交付 read 调用")
	require.Equal(t, uint64(1), first.deliveredSeq, "第一轮交付的是 seq=1 的批")
	f.commit(t, first)
	require.Equal(t, uint64(1), f.deliveredSeq())
	require.NotNil(t, f.record.pending)

	// 第二轮：write 批复用同一个 call_id（真实客户端行为）。
	f.noteNativeFsBatch(t, cursorToolKindWrite, callID)
	require.NoError(t, f.delegations.seal())
	require.Equal(t, []uint64{1, 2}, f.sealedSeqs(), "复用 call_id 的新批必须是 seq=2 的新批")

	second, err := f.prepare(t)
	require.NoError(t, err, "同 call_id 的第二批不得被判为'已登记的历史批'而吞掉")
	// 该原 id 第一轮已交付过 → 第二轮必须换一个**新的**出站 id（客户端按 id 去重，
	// 复用同一 id 会丢弃第二条调用）。这一条同时证明新批没被吞：集合非空且长度恰为 1。
	require.Len(t, second.callIDs, 1, "第二批的 call_id 必须计入 deliveredCallIDs（修复前这里是空的，导致 stream_error）")
	require.NotEqual(t, callID, second.callIDs[0], "复用原 id 的第二批必须被改写为新的出站 id")
	// 新 id 必须能反查回原来的上游原 id（续轮回写依赖这条映射）；
	// 同时证明"历史批不重复计入"：集合里没有别的 id（第一轮的批不会二次出现）。
	original, ok := f.delegations.upstreamCallID(second.callIDs[0])
	require.True(t, ok, "改写后的新 id 必须能在本会话内反查到上游原 id")
	require.Equal(t, callID, original, "新 id 反查必须回到被复用的那个原 id")
	require.Equal(t, uint64(2), second.deliveredSeq, "第二批 seq 必须为 2")
	require.NotNil(t, f.record.pending, "第二批必须产生 pending（等待 write 结果）")
	require.Equal(t, second.callIDs, f.record.pending.callIDs)
	require.False(t, f.record.pending.delivered, "准备阶段只能是已准备未交付")

	f.commit(t, second)
	require.Equal(t, uint64(2), f.deliveredSeq(), "第二批提交后水位推进到 2")
}

// TestCursorBatchSeqNoNewBatchFailsWithoutEmptyPending 覆盖 ②：没有新封口批时
// prepareDeliveredBatch 必须明确失败关闭，绝不构造空 pending（空批交付 = 上游无结果）。
func TestCursorBatchSeqNoNewBatchFailsWithoutEmptyPending(t *testing.T) {
	f := newCursorBatchSeqFixture(t)
	const callID = "call-no-new-batch"

	f.noteNativeFsBatch(t, cursorToolKindRead, callID)
	require.NoError(t, f.delegations.seal())
	first, err := f.prepare(t)
	require.NoError(t, err)
	f.commit(t, first)

	// 会话状态与 pending 快照：本轮 prepare 失败后必须原样保留，不得被空批覆盖。
	f.record.mu.Lock()
	stateBefore, pendingBefore := f.record.state, f.record.pending
	f.record.mu.Unlock()
	require.NotNil(t, pendingBefore)

	// 没有任何新批（seq 全部 <= 水位）→ 明确错误。
	_, err = f.prepare(t)
	require.Error(t, err, "无新封口批时必须失败关闭")
	require.ErrorIs(t, err, ErrCursorBatchIncomplete)
	require.Contains(t, err.Error(), "batch checkpoint without new sealed batch")

	f.record.mu.Lock()
	stateAfter, pendingAfter := f.record.state, f.record.pending
	f.record.mu.Unlock()
	require.Equal(t, stateBefore, stateAfter, "失败不得改变会话状态")
	require.Same(t, pendingBefore, pendingAfter, "失败不得构造/覆盖 pending（绝不交付空批）")
	require.NotEqual(t, 0, len(pendingAfter.callIDs), "pending 必须是上一轮真实交付的批")
	require.Equal(t, uint64(1), f.deliveredSeq(), "失败不推进水位")
}

// TestCursorBatchSeqMultipleNewBatchesDeliveredAtOnce 覆盖 ③：一次封口多个新批时，
// seq 判定必须一次性交付全部新批（并集），水位推进到最大 seq。
func TestCursorBatchSeqMultipleNewBatchesDeliveredAtOnce(t *testing.T) {
	f := newCursorBatchSeqFixture(t)

	f.noteNativeFsBatch(t, cursorToolKindRead, "call-A")
	f.noteNativeFsBatch(t, cursorToolKindWrite, "call-B")
	f.noteNativeFsBatch(t, cursorToolKindGrep, "call-C")
	require.NoError(t, f.delegations.seal())
	require.Equal(t, []uint64{1, 2, 3}, f.sealedSeqs())

	prepared, err := f.prepare(t)
	require.NoError(t, err)
	require.Equal(t, []string{"call-A", "call-B", "call-C"}, prepared.callIDs,
		"三批一次封口必须一次交付全部新批（按 seq 顺序保序）")
	require.Equal(t, uint64(3), prepared.deliveredSeq, "水位取本次交付批的最大 seq")
	f.commit(t, prepared)
	require.Equal(t, uint64(3), f.deliveredSeq())

	// 全部交付后再 prepare 仍无新批。
	_, err = f.prepare(t)
	require.ErrorIs(t, err, ErrCursorBatchIncomplete)

	// 之后又有一批（复用 call-A 的 ID）→ 只交付这一批，不重复历史批。
	// 该原 id 已在 seq=1..3 那批交付过 → 新批必须换新出站 id，且能反查回 call-A。
	f.noteNativeFsBatch(t, cursorToolKindDelete, "call-A")
	require.NoError(t, f.delegations.seal())
	next, err := f.prepare(t)
	require.NoError(t, err)
	require.Len(t, next.callIDs, 1, "只交付 seq=4 的新批，历史批不得重复计入（长度必须恰为 1）")
	require.NotEqual(t, "call-A", next.callIDs[0],
		"复用原 id 的新批必须被改写为新的出站 id（客户端按 id 去重）")
	original, ok := f.delegations.upstreamCallID(next.callIDs[0])
	require.True(t, ok, "改写后的新 id 必须能反查到上游原 id")
	require.Equal(t, "call-A", original, "新 id 必须反查回被复用的 call-A，而不是历史批里的别的 id")
	require.Equal(t, uint64(4), next.deliveredSeq)
}

// TestCursorBatchSeqDeliveredWatermarkAdvancesOnlyOnCommit 覆盖 ④：deliveredBatchSeq
// 只在 commit 成功后推进——准备成功未提交不推进，提交失败不推进。
func TestCursorBatchSeqDeliveredWatermarkAdvancesOnlyOnCommit(t *testing.T) {
	f := newCursorBatchSeqFixture(t)
	const callID = "call-watermark"

	// 准备成功但尚未提交 → 水位必须仍为 0。
	f.noteNativeFsBatch(t, cursorToolKindRead, callID)
	require.NoError(t, f.delegations.seal())
	prepared, err := f.prepare(t)
	require.NoError(t, err)
	require.Equal(t, uint64(1), prepared.deliveredSeq)
	require.Equal(t, uint64(0), f.deliveredSeq(), "准备阶段绝不推进水位")

	// 提交失败（会话已被 finalizer 接管）→ 水位仍不推进。
	f.record.mu.Lock()
	f.record.finishing = true
	f.record.mu.Unlock()
	require.ErrorIs(t, f.svc.commitDeliveredBatchState(f.record, prepared), ErrCursorResumeNotWaiting)
	require.Equal(t, uint64(0), f.deliveredSeq(), "提交失败绝不推进水位")

	// 解除失败条件后正常提交 → 水位推进。
	f.record.mu.Lock()
	f.record.finishing = false
	f.record.mu.Unlock()
	f.commit(t, prepared)
	require.Equal(t, uint64(1), f.deliveredSeq(), "只有提交成功才推进水位")
}

// TestCursorBatchSeqAllocatedMonotonically 覆盖"seq 由 newBatchLocked 单调分配"这一
// 修复前提：三处建批点（batchFor 具名 / batchFor 匿名 / noteStandaloneDelegation）共用
// 同一个计数器，从 1 开始、不重复、不复用。
func TestCursorBatchSeqAllocatedMonotonically(t *testing.T) {
	d := newCursorToolDelegations()

	require.NoError(t, d.noteToolRequestsListed("mc-1", 1))
	require.NoError(t, d.noteToolCallStarted("mc-1", "call_1", true))
	require.NoError(t, d.noteDelegation("mc-1", cursorBatchDelegation("call_1", "get_weather")))
	require.NoError(t, d.seal())

	require.NoError(t, d.noteNativeFsDelegation(cursorToolKindRead, "call_2",
		cursorToolCallCapture{ID: "call_2", Kind: cursorToolKindRead, Name: "read"}))
	require.NoError(t, d.seal())

	// 无 model_call_id 的匿名批（batchFor("") 新建分支）。
	require.NoError(t, d.noteToolRequestsListed("", 1))
	require.NoError(t, d.noteToolCallStarted("", "call_3", true))
	require.NoError(t, d.noteDelegation("", cursorBatchDelegation("call_3", "get_weather")))
	require.NoError(t, d.seal())

	d.mu.Lock()
	seqs := make([]uint64, 0, len(d.all))
	for _, b := range d.all {
		seqs = append(seqs, b.seq)
	}
	d.mu.Unlock()
	require.Equal(t, []uint64{1, 2, 3}, seqs, "三处建批点必须共用同一计数器，seq 从 1 单调递增")
}
