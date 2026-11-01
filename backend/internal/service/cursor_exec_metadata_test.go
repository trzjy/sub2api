//go:build unit

package service

// ExecServerMessage 元数据字段（f1 id / f15 exec_id / f19 span_context / f55
// accept_hook_additional_contexts / f57 machine_id — 服务端方向已证）与本轮裁定派发顺序的
// 定向单测。
//
// 本轮裁定（派发单 CURSOR-A）：
//   - 派发顺序严格为 f10 → f36 → f11 → 11 类原生拒绝 → 已证 metadata → fail-close。
//   - 11 类原生 exec（f2/f3/f4/f5/f7/f8/f9/f14/f16/f20/f23）一律按协议拒绝，不执行、不配对、
//     不做 shell fallback；只有 f11 mcp_args 走客户端声明工具执行。
//   - 服务端 metadata 白名单只含 f1/f15/f19/f55/f57；f39/f45 不再被当作 metadata 忽略，
//     f45（pi_read_args）未实现必须走 unsupported fail-close。
//   - respondContext 不再伪造服务器 GOOS / "UTC" 环境。
//
// 注意：本文件自带所需的 schema 字面量，不跨文件依赖 cursor_native_fs_bridge_test.go
// （那个文件属于别的卡，会被删除）。

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/types/known/structpb"
)

// cursorExecMetaLocalReadSchema 是本卡自带的 read 工具 schema 字面量（不依赖被删夹具）。
const cursorExecMetaLocalReadSchema = `{"type":"object","properties":{"file_path":{"type":"string"},"offset":{"type":"number"},"limit":{"type":"number"}},"required":["file_path"]}`

// cursorExecNativeRejectFields 是 11 类已裁定原生 exec 字段号（与社区 pb 逐条核对）。
var cursorExecNativeRejectFields = []int{2, 3, 4, 5, 7, 8, 9, 14, 16, 20, 23}

// cursorExecMetaTestSpanContext 构造一段 span_context 子消息字节（内容无关紧要，
// 白名单只按字段号判定，不解析内容）。
func cursorExecMetaTestSpanContext() []byte {
	return cursorAppendStringField(nil, 1, "span-1")
}

// cursorExecMetaTestPayload 把 ExecServerMessage 内层字节包进 AgentServerMessage.f2 信封。
func cursorExecMetaTestPayload(execMsg []byte) []byte {
	return cursorAppendBytesField(nil, cursorFldServerExecMessage, execMsg)
}

// cursorExecRejectTestDuplex 构造用于 respond 派发的 duplex，并提供一个会排干写入帧的 pipe
// writer + observer 日志。reject / context / mcp_state 路径都会调用 d.send，因此必须提供真实
// writer（nil writer 会 panic）；所有写出的字节被收集到 buf，cleanup 负责关闭 pipe 并等待
// 排干完成（保证断言 buf 时已落盘全部字节）。
func cursorExecRejectTestDuplex(t *testing.T, defs ...[]byte) (*cursorAgentDuplex, *bytes.Buffer, *observer.ObservedLogs, func()) {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	pr, pw := io.Pipe()
	buf := &bytes.Buffer{}
	doneCh := make(chan struct{})
	go func() {
		_, _ = io.Copy(buf, pr)
		close(doneCh)
	}()
	d := &cursorAgentDuplex{
		reader:           pr,
		writer:           pw,
		delegations:      newLocalCursorToolDelegations(),
		declaredToolDefs: defs,
		lg:               zap.New(core),
	}
	cleanup := func() {
		_ = pw.Close()
		<-doneCh
	}
	return d, buf, logs, cleanup
}

// ---------------------------------------------------------------------------
// ① 纯元数据 [1,15,19,55] → 被忽略，不失败、不终止连接
// ---------------------------------------------------------------------------

func TestCursorExecMetaOnlyIgnored(t *testing.T) {
	d, _, logs, cleanup := cursorExecRejectTestDuplex(t)

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
	cleanup()
}

// ---------------------------------------------------------------------------
// ② 未知非元数据字段（f99）→ 仍失败关闭，日志含字段号
// ---------------------------------------------------------------------------

func TestCursorExecUnknownFieldStillFailsClosed(t *testing.T) {
	d, _, logs, cleanup := cursorExecRejectTestDuplex(t)

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
	cleanup()
}

// ---------------------------------------------------------------------------
// ③ 元数据 f57 machine_id 单独出现 → 被忽略
// ---------------------------------------------------------------------------

func TestCursorExecMetaMachineIDOnlyIgnored(t *testing.T) {
	d, _, logs, cleanup := cursorExecRejectTestDuplex(t)

	execMsg := cursorAppendBytesField(nil, cursorFldExecMetaMachineID, []byte("machine-9"))

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err, "仅 machine_id 的元数据消息不得失败关闭")
	require.False(t, done)
	require.Equal(t, 1, phaseCount(logs, "exec_metadata_only"))
	require.Equal(t, 0, phaseCount(logs, "close"))
	cleanup()
}

// ---------------------------------------------------------------------------
// ④ 元数据与未知字段混排 → 仍失败关闭（白名单不得掩盖未知字段）
// ---------------------------------------------------------------------------

func TestCursorExecMetaMixedWithUnknownFailsClosed(t *testing.T) {
	d, _, logs, cleanup := cursorExecRejectTestDuplex(t)

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 5)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaSpanContext, cursorExecMetaTestSpanContext())
	execMsg = cursorAppendVarintField(execMsg, cursorFldExecMetaAcceptHook, 1)
	execMsg = cursorAppendBytesField(execMsg, 99, []byte("unknown-exec"))

	_, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
	require.Equal(t, 0, phaseCount(logs, "exec_metadata_only"))
	cleanup()
}

// ---------------------------------------------------------------------------
// ⑤ f45（pi_read_args，未实现）→ fail-close，不得按 metadata 忽略、不得空 McpResult
// ---------------------------------------------------------------------------

func TestCursorExecF45FailsClosed(t *testing.T) {
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 45)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-45"))
	execMsg = cursorAppendBytesField(execMsg, 45, []byte("pi-read-args"))

	_, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest, "f45 未实现必须走 unsupported fail-close")
	require.Equal(t, 0, phaseCount(logs, "exec_metadata_only"), "f45 不得按 metadata 忽略")
	require.Equal(t, 1, phaseCount(logs, "exec_unsupported_fields"))
	cleanup()
	require.Empty(t, buf.Len(), "fail-close 不得写出任何结果帧（含空 McpResult）")
}

// ---------------------------------------------------------------------------
// ⑥ f39（local_execution_time_ms，客户端方向）→ 不再被当作 metadata，fail-close
// ---------------------------------------------------------------------------

func TestCursorExecF39NotMetadata(t *testing.T) {
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 39)
	execMsg = cursorAppendBytesField(execMsg, 39, []byte("local-time"))

	_, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest, "f39 不再被当 metadata，必须 fail-close")
	require.Equal(t, 0, phaseCount(logs, "exec_metadata_only"))
	require.Equal(t, 1, phaseCount(logs, "exec_unsupported_fields"))
	cleanup()
	require.Empty(t, buf.Len(), "fail-close 不得写出任何结果帧")
}

// ---------------------------------------------------------------------------
// ⑦ 白名单谓词本身：只有五个服务端方向已证字段号返回 true；f39/f45/任何原生/执行字段均否
// ---------------------------------------------------------------------------

func TestCursorExecMetadataFieldWhitelist(t *testing.T) {
	allowed := []int{
		cursorFldExecMetaID,
		cursorFldExecMetaExecID,
		cursorFldExecMetaMachineID,
		cursorFldExecMetaSpanContext,
		cursorFldExecMetaAcceptHook,
	}
	for _, n := range allowed {
		require.True(t, cursorExecMetadataFieldOK(n), "f%d 必须在元数据白名单内", n)
	}
	// 已实现的 exec 子字段、11 类原生拒绝字段、f39/f45 客户端方向字段、任意未知字段都不得进白名单。
	for _, n := range []int{3, 4, 5, 7, 8, 9, 10, 11, 14, 16, 20, 23, 36, 39, 45, 99, 0, -1} {
		require.False(t, cursorExecMetadataFieldOK(n), "f%d 不得被当作可忽略元数据", n)
	}
	// 空 parts 不算"纯元数据"（空 ExecServerMessage 仍需按原路径处理）。
	require.False(t, cursorExecMetadataOnly(nil))
}

// ---------------------------------------------------------------------------
// ⑧ 11 类原生 exec 字段各自命中拒绝路径（不是委托、不是 unsupported）
// ---------------------------------------------------------------------------

func TestCursorExecNativeRejectAllElevenFields(t *testing.T) {
	for _, field := range cursorExecNativeRejectFields {
		field := field
		t.Run(fmt.Sprintf("f%d", field), func(t *testing.T) {
			defs := [][]byte{cursorTestBuildToolDef(t, "bash",
				`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}
			d, buf, logs, cleanup := cursorExecRejectTestDuplex(t, defs...)

			execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 1)
			execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-1"))
			execMsg = cursorAppendBytesField(execMsg, field, nil)

			done, err := d.respond(cursorExecMetaTestPayload(execMsg))
			require.NoError(t, err, "原生 exec 必须按拒绝处理，不得失败关闭")
			require.False(t, done)
			cleanup()

			require.NotEmpty(t, buf.Len(), "拒绝帧必须写出")
			ents := logs.FilterField(zap.String("phase", "native_exec_rejected")).All()
			require.Len(t, ents, 1, "必须记一条 native_exec_rejected")
			require.EqualValues(t, field, ents[0].ContextMap()["field"], "拒绝日志的 field 必须命中正确字段号")
			require.Equal(t, 0, phaseCount(logs, "exec_unsupported_fields"), "原生 exec 不得走 unsupported")
			require.Equal(t, 0, phaseCount(logs, "exec_metadata_only"), "原生 exec 不得被当作 metadata")
			require.Empty(t, d.delegations.snapshot(), "拒绝不得登记委托（不是 fs/shell 委托）")
		})
	}
}

// ---------------------------------------------------------------------------
// ⑨ f10 / f36 / f11 三者互不串线
// ---------------------------------------------------------------------------

func TestCursorExecDispatchNoCrossTalkF10(t *testing.T) {
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t)

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 1)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-1"))
	execMsg = cursorAppendBytesField(execMsg, 10, nil) // f10 request_context

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err)
	require.False(t, done)
	cleanup()

	require.Equal(t, 1, phaseCount(logs, "context_write_before"), "f10 必须走 context 路径")
	require.Equal(t, 0, phaseCount(logs, "mcp_state_reply"), "f10 不得触发 mcp_state 路径")
	require.Equal(t, 0, phaseCount(logs, "mcp_args_captured"), "f10 不得触发 mcp_args 路径")
	require.NotEmpty(t, buf.Len(), "context 回包必须写出")
	// respondContext 不得伪造服务器 GOOS / "UTC" 环境。
	require.NotContains(t, buf.String(), "UTC", "context 回包不得含伪造的时区 UTC")
	require.NotContains(t, buf.String(), runtime.GOOS, "context 回包不得含伪造的服务器 GOOS")
}

func TestCursorExecDispatchNoCrossTalkF36(t *testing.T) {
	defs := [][]byte{cursorTestBuildToolDef(t, "bash",
		`{"type":"object","properties":{"command":{"type":"string"}}}`)}
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t, defs...)

	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 1)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-1"))
	execMsg = cursorAppendBytesField(execMsg, 36, nil) // f36 mcp_state

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err)
	require.False(t, done)
	cleanup()

	require.Equal(t, 1, phaseCount(logs, "mcp_state_reply"), "f36 必须走 mcp_state 路径")
	require.Equal(t, 0, phaseCount(logs, "context_write_before"), "f36 不得触发 context 路径")
	require.Equal(t, 0, phaseCount(logs, "mcp_args_captured"), "f36 不得触发 mcp_args 路径")
	require.NotEmpty(t, buf.Len(), "mcp_state 回包必须写出")
}

func TestCursorExecDispatchNoCrossTalkF11(t *testing.T) {
	schema := `{"type":"object","properties":{"x":{"type":"string"}}}`
	defs := [][]byte{cursorTestBuildToolDef(t, "my_tool", schema)}
	d, buf, logs, cleanup := cursorExecRejectTestDuplex(t, defs...)

	mcpArgs := cursorTestBuildMcpArgsOfficial(t, "call-1", "my_tool",
		map[string]*structpb.Value{"x": cursorTestVal(t, "hello")})
	execMsg := cursorAppendVarintField(nil, cursorFldExecMetaID, 1)
	execMsg = cursorAppendBytesField(execMsg, cursorFldExecMetaExecID, []byte("exec-1"))
	execMsg = cursorAppendBytesField(execMsg, 11, mcpArgs) // f11 mcp_args

	done, err := d.respond(cursorExecMetaTestPayload(execMsg))
	require.NoError(t, err)
	require.False(t, done)
	cleanup()

	require.Equal(t, 1, phaseCount(logs, "mcp_args_captured"), "f11 必须走 mcp_args 捕获路径")
	require.Equal(t, 0, phaseCount(logs, "context_write_before"), "f11 不得触发 context 路径")
	require.Equal(t, 0, phaseCount(logs, "mcp_state_reply"), "f11 不得触发 mcp_state 路径")
	// f11 只捕获委托、绝不回 McpResult（不写任何结果帧），故 buf 应为空。
	require.Empty(t, buf.Len(), "f11 不得写出结果帧（只捕获委托）")
	snap := d.delegations.snapshot()
	require.Len(t, snap, 1, "f11 必须登记一条委托")
	require.Equal(t, "my_tool", snap[0].Name)
}
