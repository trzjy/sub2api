//go:build unit

package service

// 本文件是派发单 B6 的定向单测：解析器批次归属计数必须按代际复位（外审 F1 整改）。
//
// 缺陷：解析器的 batchCounts（按 model_call_id 记录 started/internal/listedSeen）与
// pendingListed / lastModelCallID 从不随批次代际复位。续轮里同一 model_call_id 的新一代
// 在 noteToolRequestsListed 时看到 listedSeen==true 且 started+internal>=count，会把新
// listed 挂起为 pending → 新批永远拿不到 listed → checkSealableLocked 永不通过 → 180s 挂死。
//
// 改法：在 cursorSSEStream.next() 的工具交接检查点（s.delegated=true 且 s.done=true）对解析器
// 调用 resetBatchGeneration()，清空 batchCounts / pendingListed / lastModelCallID。后续帧属新一代
// model call，旧批计数不得跨代沿用。同一 HTTP 段内出现第二个 model_call_id 的批（未交接）不得复位。
//
// 复用 cursor_protocol_response_toolbatch_test.go 的帧构造辅助（tbStartedMsg / tbListedMsg），
// 二者同包且同为 unit 构建标签，可直接引用。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCursorListedGenerationCountsResetAcrossGenerations 覆盖 D1：同一解析器上，
// ① 第一代 started(ext a) + listed(1) → 批计数 started=1/listedSeen=true；
// ② 触发一次交接检查点（此处直接调用 next() 内部调用的复位方法，等价于 s.delegated=true 处
//
//	的代际复位）；
//
// ③ 第二代同 model_call_id started(ext b) + listed(1) → 断言该 listed 归属到批（不挂起）、
//
//	pendingListed==nil、批计数回到 started=1/listedSeen=true。
func TestCursorListedGenerationCountsResetAcrossGenerations(t *testing.T) {
	p := cursorNewStreamParser("m")

	// ① 第一代：started(ext a) + listed(1)。
	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("ext-a", "mc-g", true)))
	require.NoError(t, p.noteToolRequestsListed(tbListedMsg(1)))
	gen1 := p.batchCounts["mc-g"]
	require.NotNil(t, gen1, "第一代必须按 model_call_id 建批")
	require.Equal(t, 1, gen1.started, "第一代 started 必须计 1")
	require.True(t, gen1.listedSeen, "第一代 listed 必须记 listedSeen")
	require.Nil(t, p.pendingListed, "第一代 listed 不得挂起")

	// ② 触发一次交接检查点：等价于 next() 中 s.delegated=true 处对解析器的代际复位。
	p.resetBatchGeneration()

	// 复位后三项必须清空。
	require.Empty(t, p.batchCounts, "代际复位必须清空 batchCounts")
	require.Nil(t, p.pendingListed, "代际复位必须清空 pendingListed")
	require.Empty(t, p.lastModelCallID, "代际复位必须清空 lastModelCallID")

	// ③ 第二代同 model_call_id：started(ext b) + listed(1)。
	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("ext-b", "mc-g", true)))
	require.NoError(t, p.noteToolRequestsListed(tbListedMsg(1)))

	// 新 listed 必须归属到新批（不挂起）。
	require.Nil(t, p.pendingListed, "第二代 listed 不得挂起（必须归属到批）")
	gen2 := p.batchCounts["mc-g"]
	require.NotNil(t, gen2, "第二代必须重新按 model_call_id 建批")
	require.Equal(t, 1, gen2.started, "第二代 started 必须回到 1（不沿用第一代计数）")
	require.True(t, gen2.listedSeen, "第二代必须重新记 listedSeen")
}

// TestCursorListedGenerationNoResetWithinSameSegment 覆盖 D2：复位只在交接点发生。
// 同一 HTTP 段内出现第二个 model_call_id 的批（未交接）时，解析器**不得**做代际复位——
// 第一个批的归属计数必须原样保留，第二个批的 listed 才能正确归属到自己的批、不挂起。
func TestCursorListedGenerationNoResetWithinSameSegment(t *testing.T) {
	p := cursorNewStreamParser("m")

	// 第一个 model_call_id：started + listed（不触发交接，绝不复位）。
	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("a", "mc-1", true)))
	require.NoError(t, p.noteToolRequestsListed(tbListedMsg(1)))
	require.NotNil(t, p.batchCounts["mc-1"])
	require.True(t, p.batchCounts["mc-1"].listedSeen)

	// 同一段内第二个 model_call_id（未交接，绝不复位）：started + listed。
	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("b", "mc-2", true)))
	require.NoError(t, p.noteToolRequestsListed(tbListedMsg(1)))

	// 复位只在交接点发生：mc-1 的计数必须原样保留（未被清空）。
	bc1 := p.batchCounts["mc-1"]
	require.NotNil(t, bc1, "复位只发生在交接点；mc-1 计数必须保留")
	require.Equal(t, 1, bc1.started)
	require.True(t, bc1.listedSeen, "mc-1 的 listedSeen 不得被第二个批复位")
	require.Equal(t, "mc-2", p.lastModelCallID, "lastModelCallID 应反映最近 started（mc-2）")

	// mc-2 的 listed 必须归属到自己的批，不挂起。
	require.Nil(t, p.pendingListed, "第二个批的 listed 必须归属、不得挂起")
	bc2 := p.batchCounts["mc-2"]
	require.NotNil(t, bc2, "mc-2 必须按新 model_call_id 建批")
	require.Equal(t, 1, bc2.started)
	require.True(t, bc2.listedSeen, "mc-2 的 listed 必须归属到自己的批")
}

// TestCursorListedGenerationResetClearsInheritedListedSeen 覆盖 D1 的"挂死"根因：
// 模拟 bug 原始失效路径——第一代把 listedSeen 置 true；若不复位，第二代同 model_call_id 的
// listed 会被 noteToolRequestsListed 判为"已 listed 且计数已齐"而挂起；复位后必须能归属。
func TestCursorListedGenerationResetClearsInheritedListedSeen(t *testing.T) {
	p := cursorNewStreamParser("m")

	// 第一代：started + listed → listedSeen=true。
	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("a", "mc-g", true)))
	require.NoError(t, p.noteToolRequestsListed(tbListedMsg(1)))
	require.True(t, p.batchCounts["mc-g"].listedSeen)

	// 不复位时（bug 原始行为），第二代 listed 会被挂起：
	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("b", "mc-g", true)))
	require.NoError(t, p.noteToolRequestsListed(tbListedMsg(1)))
	require.NotNil(t, p.pendingListed, "未复位时第二代 listed 必须被挂起（复现 bug）")
	require.Equal(t, 1, *p.pendingListed)

	// 复位后新生的同 model_call_id 批必须重新开始，listed 能正常归属、不再挂起。
	p.resetBatchGeneration()
	require.Empty(t, p.batchCounts)
	require.Nil(t, p.pendingListed)

	require.NoError(t, p.noteToolCallStarted(tbStartedMsg("c", "mc-g", true)))
	require.NoError(t, p.noteToolRequestsListed(tbListedMsg(1)))
	require.Nil(t, p.pendingListed, "复位后第二代 listed 必须归属到批、不得挂起")
	require.True(t, p.batchCounts["mc-g"].listedSeen)
	require.Equal(t, 1, p.batchCounts["mc-g"].started)
}
