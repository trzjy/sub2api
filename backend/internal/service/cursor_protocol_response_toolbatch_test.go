//go:build unit

package service

// 本文件是派发单 B1 的定向单测（方案 §3 卡 B1）：cursorStreamParser 的工具批次解析与
// tool_requests_listed 归属。覆盖 Done when D1 / D2 / D3。
//
// 构造方式：全部用例都用**真实 protobuf 编码**（cursorAppend* 辅助），参照派发单 B1
// 事实依据的真实帧结构（started.f1=call_id / f2=ToolCall{f15|f44} / f3=model_call_id；
// listed 内层只有 f1=call_count，无 model_call_id）。不引入任何二进制夹具文件。
//
// 注：既有测试（cursor_execution_batch_test.go / cursor_cross_http_fullchain_test.go）
// 仍用旧合成帧布局（call_id 放在 ToolCall.f1、ToolCall 放在 started.f15）。本解析器对
// 两种布局都兼容（见 noteToolCallStarted 的双模解析），故本卡不改动那些禁区文件也能
// 保证其不回归（D4）。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 帧构造（started / listed 的 message 层字节；noteToolCallStarted /
// noteToolRequestsListed 直接消费这两层字节）
// ---------------------------------------------------------------------------

// tbStartedMsg 构造 tool_call_started 消息字节（真实帧结构）。
//
//	f1 (string) call_id        —— 对外委托 ID
//	f2 (msg)    ToolCall
//	   f15 (msg) mcp_tool_call   → 对外（external=true）
//	   f44 (msg) get_mcp_tools  → 内部元工具（external=false）
//	f3 (string) model_call_id  —— 本批归属键
func tbStartedMsg(callID, modelCallID string, external bool) []byte {
	var tcInner []byte
	if external {
		// ToolCall 内层含 f15（mcp_tool_call 分支）→ 对外委托。
		tcInner = cursorAppendBytesField(nil, cursorFldToolCallStartedToolCall, []byte{0x00})
	} else {
		// ToolCall 内层含 f44（get_mcp_tools 分支）→ 内部元工具。
		tcInner = cursorAppendBytesField(nil, cursorFldToolCallGetMcpTools, []byte{0x00})
	}
	msg := cursorAppendStringField(nil, cursorFldToolCallID, callID)                     // f1 call_id
	msg = cursorAppendBytesField(msg, cursorFldToolCallStartedToolCallMsg, tcInner)      // f2 ToolCall
	msg = cursorAppendStringField(msg, cursorFldToolCallStartedModelCallID, modelCallID) // f3 model_call_id
	return msg
}

// tbListedMsg 构造 tool_requests_listed 消息字节（内层只有 f1=call_count）。
func tbListedMsg(count uint64) []byte {
	return cursorAppendVarintField(nil, cursorFldToolRequestsListedCallCount, count)
}

// tbBatchByModelCallID 在委托记录里按 model_call_id 找批次（同包可访问未导出字段）。
func tbBatchByModelCallID(d *cursorToolDelegations, id string) *cursorToolBatch {
	if d == nil {
		return nil
	}
	for _, b := range d.all {
		if b.modelCallID == id {
			return b
		}
	}
	return nil
}

// tbBatchHasStarted 报告批次的对外 started 列表是否含给定 callID。
func tbBatchHasStarted(b *cursorToolBatch, callID string) bool {
	if b == nil {
		return false
	}
	for _, c := range b.started {
		if c == callID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// D1：解析正确 —— started.f1→call_id、f3→model_call_id；f15→external、f44→internal
// ---------------------------------------------------------------------------

func TestCursorToolBatchStartedParsesCallIDAndModelCallID(t *testing.T) {
	p := cursorNewStreamParser("m")

	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("call-e1dc3bb3-abc", "mc-0-se5x", true)))

	// ① f1→call_id：对外 started 列表含该 call_id。
	require.Equal(t, "mc-0-se5x", p.lastModelCallID, "f3 必须解析为 model_call_id")
	b := tbBatchByModelCallID(p.delegations, "mc-0-se5x")
	require.NotNil(t, b, "必须按 model_call_id 开批")
	require.True(t, tbBatchHasStarted(b, "call-e1dc3bb3-abc"), "f1 必须解析为 call_id 并登记为对外")
	require.Equal(t, 0, b.internalStarted, "对外帧不得计入 internal")
	bc := p.batchCounts["mc-0-se5x"]
	require.NotNil(t, bc)
	require.Equal(t, 1, bc.started)
	require.Equal(t, 0, bc.internal)
}

func TestCursorToolBatchStartedExternalVsInternal(t *testing.T) {
	// 对外：ToolCall 含 f15 → external=true，call_id 进入对外 started 列表。
	pExt := cursorNewStreamParser("m")
	require.NoError(t, pExt.noteToolCallStarted(tbStartedMsg("call-ext", "mc-ext", true)))
	bExt := tbBatchByModelCallID(pExt.delegations, "mc-ext")
	require.True(t, tbBatchHasStarted(bExt, "call-ext"))
	require.Equal(t, 0, bExt.internalStarted)

	// 内部：ToolCall 含 f44 → external=false，即便带了 call_id 也只计入 internal。
	pInt := cursorNewStreamParser("m")
	require.NoError(t, pInt.noteToolCallStarted(tbStartedMsg("call-int", "mc-int", false)))
	bInt := tbBatchByModelCallID(pInt.delegations, "mc-int")
	require.False(t, tbBatchHasStarted(bInt, "call-int"), "内部元工具不得进入对外 started 列表")
	require.Equal(t, 1, bInt.internalStarted, "内部元工具必须计入 internal")
	require.Equal(t, 1, pInt.batchCounts["mc-int"].internal)
}

// ---------------------------------------------------------------------------
// D2：listed 归属三种真实顺序（合成帧构造，参考抓包区间）
// ---------------------------------------------------------------------------

func TestCursorToolBatchListedBeforeStartedAttributedToNewBatch(t *testing.T) {
	// ① listed 先于 started：挂起 pending，下一个 started 消费并归到新批。
	p := cursorNewStreamParser("m")
	require.NoError(t, p.noteToolRequestsListed(tbListedMsg(2)))
	require.NotNil(t, p.pendingListed, "无 started 上下文时 listed 必须挂起")
	require.Equal(t, 2, *p.pendingListed)
	require.True(t, p.delegations == nil || len(p.delegations.all) == 0, "挂起阶段不得建批")

	// 下一个 started（新批 mc-X）消费 pending。
	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("c1", "mc-X", true)))
	require.Nil(t, p.pendingListed, "started 必须消费挂起的 listed")
	b := tbBatchByModelCallID(p.delegations, "mc-X")
	require.NotNil(t, b)
	require.Equal(t, 2, b.listed, "listed 必须归到新批")
	require.True(t, b.listedSeen, "归属后必须标记 listedSeen")
}

func TestCursorToolBatchListedWithFirstStartedSameBatch(t *testing.T) {
	// ② listed 与首个 started 同批到达：started 先到，listed 紧随归当前批。
	p := cursorNewStreamParser("m")
	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("c1", "mc-X", true)))
	require.NoError(t, p.noteToolRequestsListed(tbListedMsg(1)))
	require.Nil(t, p.pendingListed, "同批到达不应挂起")
	b := tbBatchByModelCallID(p.delegations, "mc-X")
	require.NotNil(t, b)
	require.Equal(t, 1, b.listed)
	require.True(t, b.listedSeen)
}

func TestCursorToolBatchListedAfterMultipleStartedAttributedToCurrentBatch(t *testing.T) {
	// ③ listed 在多条 started 之后到达且计数更大：归当前批。
	p := cursorNewStreamParser("m")
	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("c1", "mc-X", true)))
	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("c2", "mc-X", true)))
	require.Equal(t, 2, p.batchCounts["mc-X"].started, "两条对外 started 必须计数")
	require.NoError(t, p.noteToolRequestsListed(tbListedMsg(3)))
	require.Nil(t, p.pendingListed)
	b := tbBatchByModelCallID(p.delegations, "mc-X")
	require.NotNil(t, b)
	require.Equal(t, 3, b.listed, "更大计数必须归当前批")
	require.True(t, b.listedSeen)
}

// ---------------------------------------------------------------------------
// D3：无 listed 上下文时不报错、不建批（pending 保留）
// ---------------------------------------------------------------------------

func TestCursorToolBatchListedNoStartedContextKeepsPending(t *testing.T) {
	p := cursorNewStreamParser("m")
	// 没有任何 started 上下文时收到 listed：不应报错，不应建批，pending 保留。
	err := p.noteToolRequestsListed(tbListedMsg(2))
	require.NoError(t, err, "无 started 上下文时 listed 不得报错")
	require.NotNil(t, p.pendingListed, "pending 必须保留")
	require.Equal(t, 2, *p.pendingListed)
	require.True(t, p.delegations == nil || len(p.delegations.all) == 0,
		"无上下文时不得建批（不建批、不报歧义、不失败）")
}

// ---------------------------------------------------------------------------
// 4.3 纪律回归：三处校验仍失败关闭（缺失 / wire 非 varint / 超 uint32）
// ---------------------------------------------------------------------------

func TestCursorToolBatchListedMissingCallCountFailsClosed(t *testing.T) {
	p := cursorNewStreamParser("m")
	// f27 内层为空（无 call_count）。
	empty := cursorAppendBytesField(nil, cursorFldUpdateToolRequestsListed, nil)
	err := p.noteToolRequestsListed(empty)
	require.ErrorIs(t, err, ErrCursorProtocol)
	require.Contains(t, err.Error(), "call_count")
}

func TestCursorToolBatchListedWrongWireFailsClosed(t *testing.T) {
	p := cursorNewStreamParser("m")
	// call_count 用 bytes wire（非 varint）→ 失败关闭。
	bad := cursorAppendBytesField(nil, cursorFldToolRequestsListedCallCount, []byte{0x01, 0x02})
	err := p.noteToolRequestsListed(bad)
	require.ErrorIs(t, err, ErrCursorProtocol)
	require.Contains(t, err.Error(), "wire type")
}

func TestCursorToolBatchListedOverflowUint32FailsClosed(t *testing.T) {
	p := cursorNewStreamParser("m")
	// f1 call_count 用 varint 但值超 uint32（2^32）→ 失败关闭。
	overflow := cursorAppendVarintField(nil, cursorFldToolRequestsListedCallCount, uint64(1)<<32)
	err := p.noteToolRequestsListed(overflow)
	require.ErrorIs(t, err, ErrCursorProtocol)
	require.Contains(t, err.Error(), "overflows uint32")
}
