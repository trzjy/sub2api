//go:build unit

package service

// ExecServerMessage 元数据字段（f1 id / f15 exec_id / f19 span_context / f55
// accept_hook_additional_contexts / f57 machine_id / f39 / f45）的定向单测。
//
// 生产事实（bundle 原文 + agent-host createExecInstance）：每条 exec 消息都恒定携带
// spanContext 与 acceptHookAdditionalContexts，因此存在"只有元数据、没有任何 exec 子字段"
// 的消息（[1,15,19,55] / [1,15,57]）。此类消息不得失败关闭整条会话。
//
// 覆盖：
//   ① 只有元数据 [1,15,19,55] → 忽略，respond 返回 (false, nil)，连接不终止；
//   ② 元数据 + f7 read_args → 正常走读分支，元数据不影响既有行为；
//   ③ 未知非元数据字段（f99）→ 仍失败关闭，日志含字段号；
//   ④ 元数据 f57 machine_id 单独出现 → 被忽略；
//   ⑤ 元数据与未知字段混排 → 仍失败关闭（元数据不得成为未知字段的挡箭牌）。

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// cursorExecMetaTestSpanContext 构造一段 span_context 子消息字节（内容无关紧要，
// 白名单只按字段号判定，不解析内容）。
func cursorExecMetaTestSpanContext() []byte {
	return cursorAppendStringField(nil, 1, "span-1")
}

// cursorExecMetaTestPayload 把 ExecServerMessage 内层字节包进 AgentServerMessage.f2 信封。
func cursorExecMetaTestPayload(execMsg []byte) []byte {
	return cursorAppendBytesField(nil, cursorFldServerExecMessage, execMsg)
}

// cursorExecMetaTestDuplex 构造只用于 respond 派发的 duplex（无 pipe），并返回 observer 日志。
// 注意：reader/writer 为 nil，任何误入 close 的路径都会 panic，因此"未 panic + 日志无 close"
// 即证明连接未被终止。
func cursorExecMetaTestDuplex(defs ...[]byte) (*cursorAgentDuplex, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	return &cursorAgentDuplex{
		delegations:      newLocalCursorToolDelegations(),
		declaredToolDefs: defs,
		lg:               zap.New(core),
	}, logs
}

// ---------------------------------------------------------------------------
// ① 只有元数据 [1,15,19,55] → 被忽略，不失败、不终止连接
// ---------------------------------------------------------------------------

func TestCursorExecMetaOnlyIgnored(t *testing.T) {
	d, logs := cursorExecMetaTestDuplex()

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 42)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-42"))
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaSpanContext, cursorExecMetaTestSpanContext())
	execMsg = cursorAppendVarintField(execMsg, cursorFldExecMetaAcceptHook, 0)

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err, "纯元数据消息不得失败关闭整条会话")
	require.False(t, done, "元数据消息不是终态")
	require.Empty(t, d.delegations.snapshot(), "元数据消息不得登记任何委托")
	require.Equal(t, 1, phaseCount(logs, "exec_metadata_only"), "必须记一条 exec_metadata_only 调试日志")
	require.Equal(t, 0, phaseCount(logs, "exec_unsupported_fields"), "不得记 unsupported 取证日志")
	require.Equal(t, 0, phaseCount(logs, "close"), "连接不得被终止")
}

// ---------------------------------------------------------------------------
// ② 元数据 + f7 read_args → 正常走读分支（元数据不干扰既有派发）
// ---------------------------------------------------------------------------

func TestCursorExecMetaWithReadArgs(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "read_file", cursorNativeFsTestReadSchema)}
	d, logs := cursorExecMetaTestDuplex(defs...)

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 77)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-77"))
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaSpanContext, cursorExecMetaTestSpanContext())
	execMsg = cursorAppendVarintField(execMsg, cursorFldExecMetaAcceptHook, 1)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaMachineID, []byte("machine-1"))

	read := cursorAppendStringField(nil, 1, "/repo/a.go")
	read = cursorAppendStringField(read, 2, "call_r")
	execMsg = cursorAppendBytesField(execMsg, cursorFldServerMsgReadArgs, read)

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err, "带元数据的 read_args 必须正常命中 read 分支")
	require.False(t, done)

	snap := d.delegations.snapshot()
	require.Len(t, snap, 1, "必须登记一条原生文件委托")
	require.Equal(t, cursorToolKindRead, snap[0].Kind)
	require.Equal(t, "read_file", snap[0].Name)
	require.EqualValues(t, 77, snap[0].ExecID)
	require.Equal(t, []byte("exec-77"), snap[0].IDField)
	require.Contains(t, snap[0].ArgsJSON, "/repo/a.go")
	require.Equal(t, 0, phaseCount(logs, "exec_metadata_only"), "有 exec 子字段时不得走元数据忽略路径")
}

// ---------------------------------------------------------------------------
// ③ 未知非元数据字段（f99）→ 仍失败关闭，日志含字段号
// ---------------------------------------------------------------------------

func TestCursorExecUnknownFieldStillFailsClosed(t *testing.T) {
	d, logs := cursorExecMetaTestDuplex()

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 99)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-99"))
	execMsg = cursorAppendBytesField(execMsg, 99, []byte("unknown-exec"))

	_, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest, "未知非元数据字段必须失败关闭")

	entries := logs.FilterField(zap.String("phase", "exec_unsupported_fields")).All()
	require.Len(t, entries, 1, "必须保留 exec_unsupported_fields 取证日志")
	nums := fmt.Sprint(entries[0].ContextMap()["field_numbers"])
	require.Contains(t, nums, "99", "取证日志必须含未知字段号")
	require.Equal(t, 0, phaseCount(logs, "exec_metadata_only"), "含未知字段时不得按元数据忽略")
}

// ---------------------------------------------------------------------------
// ④ 元数据 f57 machine_id 单独出现 → 被忽略
// ---------------------------------------------------------------------------

func TestCursorExecMetaMachineIDOnlyIgnored(t *testing.T) {
	d, logs := cursorExecMetaTestDuplex()

	execMsg := cursorAppendBytesField(nil, cursorFldExecMetaMachineID, []byte("machine-9"))

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err, "仅 machine_id 的元数据消息不得失败关闭")
	require.False(t, done)
	require.Equal(t, 1, phaseCount(logs, "exec_metadata_only"))
	require.Equal(t, 0, phaseCount(logs, "close"))
}

// ---------------------------------------------------------------------------
// ⑤ 元数据与未知字段混排 → 仍失败关闭（白名单不得掩盖未知字段）
// ---------------------------------------------------------------------------

func TestCursorExecMetaMixedWithUnknownFailsClosed(t *testing.T) {
	d, logs := cursorExecMetaTestDuplex()

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 5)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaSpanContext, cursorExecMetaTestSpanContext())
	execMsg = cursorAppendVarintField(execMsg, cursorFldExecMetaAcceptHook, 1)
	execMsg = cursorAppendBytesField(execMsg, 99, []byte("unknown-exec"))

	_, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
	require.Equal(t, 0, phaseCount(logs, "exec_metadata_only"))
}

// ---------------------------------------------------------------------------
// 白名单谓词本身：只有七个元数据字段号返回 true
// ---------------------------------------------------------------------------

func TestCursorExecMetadataFieldWhitelist(t *testing.T) {
	allowed := []int{
		cursorFldExecMetaID,
		cursorFldExecMetaExecID,
		cursorFldExecMetaMachineID,
		cursorFldExecMetaSpanContext,
		cursorFldExecMetaAcceptHook,
		cursorFldExecMetaLocalExecTimeMs,
		cursorFldExecMetaHookAddlContext,
	}
	for _, n := range allowed {
		require.True(t, cursorExecMetadataFieldOK(n), "f%d 必须在元数据白名单内", n)
	}
	// 已实现的 exec 子字段与任意未知字段都不得进白名单。
	for _, n := range []int{3, 4, 5, 7, 8, 10, 11, 14, 36, 99, 0, -1} {
		require.False(t, cursorExecMetadataFieldOK(n), "f%d 不得被当作可忽略元数据", n)
	}
	// 空 parts 不算"纯元数据"（空 ExecServerMessage 仍需按原路径处理）。
	require.False(t, cursorExecMetadataOnly(nil))
}
