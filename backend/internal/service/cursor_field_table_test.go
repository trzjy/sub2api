//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCursorFieldTableClassifications 验证所有分类的准确性。
func TestCursorFieldTableClassifications(t *testing.T) {
	// ExecServerMessage 域：11 类原生工具字段。
	for _, n := range cursorExecNativeRejectOrder {
		require.Equal(t, cursorFieldNative,
			cursorFieldClassOf(cursorDomainExec, n),
			"exec 域字段 %d 应为 Native", n)
	}

	// ExecServerMessage 域：可忽略元数据。
	for _, n := range []int{
		cursorFldExecMetaID,
		cursorFldExecMetaExecID,
		cursorFldExecMetaSpanContext,
		cursorFldExecMetaAcceptHook,
		cursorFldExecMetaMachineID,
	} {
		require.Equal(t, cursorFieldMetadata,
			cursorFieldClassOf(cursorDomainExec, n),
			"exec 域字段 %d 应为 Metadata", n)
	}

	// ExecServerMessage 域：委托字段（handler）。
	for _, n := range []int{10, 36, 11} {
		require.Equal(t, cursorFieldHandler,
			cursorFieldClassOf(cursorDomainExec, n),
			"exec 域字段 %d 应为 Handler", n)
	}

	// InteractionQuery 域：10 类已知交互询问类型。
	for _, n := range cursorInteractionQueryOrder {
		require.Equal(t, cursorFieldInteraction,
			cursorFieldClassOf(cursorDomainInteraction, n),
			"interaction 域字段 %d 应为 Interaction", n)
	}
}

// TestCursorFieldTableUnknown 验证未知字段返回 unknown。
func TestCursorFieldTableUnknown(t *testing.T) {
	// exec 域未登记字段。
	require.Equal(t, cursorFieldUnknown, cursorFieldClassOf(cursorDomainExec, 999))
	// interaction 域未登记字段。
	require.Equal(t, cursorFieldUnknown, cursorFieldClassOf(cursorDomainInteraction, 999))
	// interaction 域不含 exec 元数据字段。
	require.Equal(t, cursorFieldUnknown,
		cursorFieldClassOf(cursorDomainInteraction, cursorFldExecMetaID))
	// 非法消息域。
	require.Equal(t, cursorFieldUnknown, cursorFieldClassOf(cursorMessageDomain(99), 2))
}

// TestCursorFieldTableDomainIsolation 验证相同字段号在不同域返回不同分类。
func TestCursorFieldTableDomainIsolation(t *testing.T) {
	// 字段 2：exec 域是 Native（shell_args），interaction 域是 Interaction（web_search）。
	require.Equal(t, cursorFieldNative, cursorFieldClassOf(cursorDomainExec, 2))
	require.Equal(t, cursorFieldInteraction, cursorFieldClassOf(cursorDomainInteraction, 2))

	// 字段 3：exec 域 Native（write_args），interaction 域 Interaction（ask_question）。
	require.Equal(t, cursorFieldNative, cursorFieldClassOf(cursorDomainExec, 3))
	require.Equal(t, cursorFieldInteraction, cursorFieldClassOf(cursorDomainInteraction, 3))

	// 字段 10：exec 域 Handler（request_context），interaction 域 Interaction（pr_management）。
	require.Equal(t, cursorFieldHandler, cursorFieldClassOf(cursorDomainExec, 10))
	require.Equal(t, cursorFieldInteraction, cursorFieldClassOf(cursorDomainInteraction, 10))

	// 字段 11：exec 域 Handler（mcp_args），interaction 域 Interaction（mcp_auth）。
	require.Equal(t, cursorFieldHandler, cursorFieldClassOf(cursorDomainExec, 11))
	require.Equal(t, cursorFieldInteraction, cursorFieldClassOf(cursorDomainInteraction, 11))
}

// TestCursorFieldTableOrderSlices 验证顺序切片包含预期的 11 与 10 个字段。
func TestCursorFieldTableOrderSlices(t *testing.T) {
	require.Len(t, cursorExecNativeRejectOrder, 11,
		"原生拒接顺序切片必须包含 11 个字段")
	require.Len(t, cursorInteractionQueryOrder, 10,
		"交互询问顺序切片必须包含 10 个字段")

	// 11 个原生字段号须与 ExecServerMessage 域登记一致，且字段号无重复。
	seen := map[int]bool{}
	for _, n := range cursorExecNativeRejectOrder {
		require.False(t, seen[n], "原生字段号 %d 重复", n)
		seen[n] = true
		require.Equal(t, cursorFieldNative, cursorFieldClassOf(cursorDomainExec, n),
			"顺序切片字段 %d 必须在 exec 域登记为 Native", n)
	}

	// 10 个交互字段号须与 InteractionQuery 域登记一致，且字段号无重复。
	iseen := map[int]bool{}
	for _, n := range cursorInteractionQueryOrder {
		require.False(t, iseen[n], "交互字段号 %d 重复", n)
		iseen[n] = true
		require.Equal(t, cursorFieldInteraction, cursorFieldClassOf(cursorDomainInteraction, n),
			"顺序切片字段 %d 必须在 interaction 域登记为 Interaction", n)
	}
}
