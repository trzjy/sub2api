//go:build unit

package service

// 统一字段表的「单一来源」约束（CARD-01 收口）。
//
// 背景：字段表建成时，表项是手写的 map，同时另有两份「顺序清单」
// （cursorExecNativeRejectOrder / cursorInteractionQueryOrder）。三份数据描述
// 同一批字段 —— 既有代码就曾因两份清单漂移漏掉过 namespace 内的 custom 子工具
// （custom 子工具被整个丢弃，codex 的 exec 从未进过 f4）。
//
// 现在的口径：表由「类别清单」派生，顺序清单本身就是表的唯一数据源。
// 本文件钉住这个约束：
//   - 清单里的每个字段都能在表里查到，且分类正确；
//   - 表里没有任何不在清单里的字段（表不会凭空多出字段）；
//   - 原生顺序清单与「拒绝形状的优先级顺序」同源（cursorNativeExecRejectField
//     取第一个命中的字段号，顺序变了拒绝形状就变了，所以必须同源）。
//
// 判据：往清单里加一个字段，表必须自动包含它；这正是「派生」与「手写」的区别。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCursorFieldTableDerivedFromSlices 验证表由类别清单派生。
func TestCursorFieldTableDerivedFromSlices(t *testing.T) {
	// 1) 每份清单里的字段都能在表里查到，且分类正确。
	for _, n := range cursorExecNativeRejectOrder {
		require.Equal(t, cursorFieldNative,
			cursorFieldClassOf(cursorDomainExec, n),
			"原生清单字段 %d 必须在表里登记为 Native", n)
	}
	for _, n := range cursorExecMetadataOrder {
		require.Equal(t, cursorFieldMetadata,
			cursorFieldClassOf(cursorDomainExec, n),
			"元数据清单字段 %d 必须在表里登记为 Metadata", n)
	}
	for _, n := range cursorExecHandlerOrder {
		require.Equal(t, cursorFieldHandler,
			cursorFieldClassOf(cursorDomainExec, n),
			"委托字段清单字段 %d 必须在表里登记为 Handler", n)
	}
	for _, n := range cursorInteractionQueryOrder {
		require.Equal(t, cursorFieldInteraction,
			cursorFieldClassOf(cursorDomainInteraction, n),
			"交互清单字段 %d 必须在表里登记为 Interaction", n)
	}

	// 2) 表的大小 = 各清单长度之和（表里不得凭空多出字段，也不得漏）。
	want := len(cursorExecNativeRejectOrder) +
		len(cursorExecMetadataOrder) +
		len(cursorExecHandlerOrder) +
		len(cursorInteractionQueryOrder)
	require.Len(t, cursorFieldClassTable, want,
		"表的大小必须等于各类别清单长度之和：多出来的字段说明表与清单已漂移")
}

// TestCursorFieldTableNoDuplicateEntries 验证表里没有重复登记。
// 域 + 字段号是复合键，同一键登记两次会让后写的覆盖先写的。
func TestCursorFieldTableNoDuplicateEntries(t *testing.T) {
	counts := map[cursorFieldKey]int{}
	for _, n := range cursorExecNativeRejectOrder {
		counts[cursorFieldKey{domain: cursorDomainExec, number: n}]++
	}
	for _, n := range cursorExecMetadataOrder {
		counts[cursorFieldKey{domain: cursorDomainExec, number: n}]++
	}
	for _, n := range cursorExecHandlerOrder {
		counts[cursorFieldKey{domain: cursorDomainExec, number: n}]++
	}
	for _, n := range cursorInteractionQueryOrder {
		counts[cursorFieldKey{domain: cursorDomainInteraction, number: n}]++
	}
	for k, c := range counts {
		require.Equal(t, 1, c,
			"域 %d 的字段 %d 被登记了 %d 次", k.domain, k.number, c)
	}
}

// TestCursorNativeRejectOrderIsSingleSource 验证「拒绝形状的优先级顺序」与
// 字段表同源：cursorNativeExecRejectField 按 cursorExecNativeRejectOrder 取
// 第一个命中的字段号，因此该顺序就是拒绝形状的优先级。
//
// 若这里改成维护第二份顺序清单，两处就会漂移 —— 拒绝的形状与表里登记的字段
// 对不上，而测试全绿（这正是要防的）。
func TestCursorNativeRejectOrderIsSingleSource(t *testing.T) {
	// 顺序清单的顺序即优先级：f2 优先于 f3，f3 优先于 f4 ……以此类推。
	require.Equal(t, cursorFldNativeRejectShellArgs, cursorExecNativeRejectOrder[0],
		"拒绝优先级第一位必须是 shell_args")
	require.Equal(t, cursorFldNativeRejectWriteArgs, cursorExecNativeRejectOrder[1],
		"拒绝优先级第二位必须是 write_args")

	// 构造只含 f3 的 exec 消息 → 取到的字段号必须是 f3（优先级生效）。
	parts := []cursorPBField{{Number: cursorFldNativeRejectWriteArgs}}
	got, ok := cursorNativeExecRejectField(parts)
	require.True(t, ok)
	require.Equal(t, cursorFldNativeRejectWriteArgs, got)

	// 同时含 f2 与 f3 → 必须按优先级取 f2（而不是 parts 的出现顺序）。
	parts = []cursorPBField{
		{Number: cursorFldNativeRejectWriteArgs},
		{Number: cursorFldNativeRejectShellArgs},
	}
	got, ok = cursorNativeExecRejectField(parts)
	require.True(t, ok)
	require.Equal(t, cursorFldNativeRejectShellArgs, got,
		"优先级顺序必须来自 cursorExecNativeRejectOrder，而不是字段出现顺序")
}
