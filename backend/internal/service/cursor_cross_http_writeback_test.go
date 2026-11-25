//go:build unit

package service

// 本文件是 **§25.3 / §24.3 跨 HTTP 真实结果回写通道（W2-F）** 的组件级定向测试。
//
// 形状以证据 26/27/28 与第三方真实字段定义为蓝本：四层编码字节形状（恒 McpResult.f1
// success / is_error 走 McpSuccess.f2 / 结构化走 McpSuccess.f3 Struct）、整批预编码后零发送
// 原子性、单工具写回原生双工、同名并行两工具各自配对、传输失败关闭会话零重放、未知 ID
// 失败关闭、未装配回写通道即 intake_unavailable。
//
// 全部走**真实生产实现**：cursorAgentRunIntake（CursorSessionIntake 生产实现）+
// cursorEncodeMcpToolResult（§25.3 四层编码）。旧链语义（文本补历史 / 伪 McpError /
// half-close 冒充）在本文件中**只能出现在失败断言里**，绝不作为成功路径。
//
// 端到端（真实 Execute + 真实 ResumeCursorSession + 真实上游连接）见
// cursor_cross_http_fullchain_test.go。
//
// 纯内存组件测试：用 io.Pipe 作为原生双工 writer，从对端 reader 捕获真实回写字节并逐层解码。

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newWritebackDuplex 构造一条只承载"真实结果回写"的轻量双工（reader/writer 为 io.Pipe）。
//
// 测试只关心 send() 把字节写到 writer；reader 端由 captureDuplex 消费并解码。
func newWritebackDuplex(pr *io.PipeReader, pw *io.PipeWriter, delegations *cursorToolDelegations) *cursorAgentDuplex {
	return &cursorAgentDuplex{
		reader:      pr,
		writer:      pw,
		delegations: delegations,
		lg:          zap.NewNop(),
		reqID:       "writeback-test",
		model:       "cursor-test-upstream",
	}
}

// captureDuplex 启动一个并发 reader 把双工 writer 的真实回写字节累积到 buf。
//
// 返回的函数在 DeliverToolResults 返回后调用：关闭 reader 结束 goroutine 并交回完整字节
// （send 是阻塞写，DeliverToolResults 返回即代表所有 Write 已被 reader 消费）。
func captureDuplex(t *testing.T, d *cursorAgentDuplex) func() []byte {
	t.Helper()
	var mu sync.Mutex
	var buf []byte
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tmp := make([]byte, 8192)
		for {
			n, err := d.reader.Read(tmp)
			if n > 0 {
				mu.Lock()
				buf = append(buf, tmp[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return func() []byte {
		_ = d.reader.Close()
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return buf
	}
}

// splitWritebackEnvelopes 把一段回写字节流切成逐条 envelope，并剥掉外层 envelope 与
// send 包裹的 bytes_field(2, ExecClientMessage) 标签，交回**裸 ExecClientMessage** 字节。
//
// 生产写路径是 d.send(cursorFldClientMessage, execMsg) → cursorEncodeEnvelope(
// bytes_field(2, execMsg))：一条回写 = 一条 envelope，envelope.Data = bytes_field(2, execMsg)。
// decodeWritebackExec 期望裸 ExecClientMessage，故这里把外层 f2 标签剥掉再交回。
func splitWritebackEnvelopes(t *testing.T, wire []byte) [][]byte {
	t.Helper()
	var out [][]byte
	off := 0
	for off < len(wire) {
		frame, next, err := cursorReadEnvelopeFrame(wire, off)
		require.NoError(t, err, "回写字节必须是合法 envelope 流")
		// 剥掉 send 包裹的外层 bytes_field(2, ExecClientMessage) 标签。
		outer, derr := cursorDecodeProtobuf(frame.Data)
		require.NoError(t, derr, "envelope payload 必须是合法 protobuf")
		ecm, ok := cursorFindField(outer, cursorFldClientMessage)
		if !ok {
			// 控制帧（如 ExecClientControlMessage.f5 stream_close）不承载 ExecClientMessage：
			// 拒绝/原生文件回写路径会在结果帧之后补一条 stream_close 终止 exec 流，本辅助函数只回交
			// ExecClientMessage，控制帧按设计跳过（与 cursor_native_fs_bridge_test.go 同口径）。
			off = next
			continue
		}
		out = append(out, ecm.Bytes)
		off = next
	}
	return out
}

// cursorWritebackDecoded 是一条回写帧的解码结果。
type cursorWritebackDecoded struct {
	execID        uint64
	idField       []byte
	texts         []string
	structured    []byte // McpSuccess.f3 原始 Struct 字节
	hasErrorField bool   // McpResult.f2（McpError）是否存在——恒应为 false
	isErrorFlag   bool   // McpSuccess.f2 是否存在
	isErrorRaw    uint64
	isError       bool
}

// decodeWritebackExec 把一条 **ExecClientMessage 载荷**（envelope.Data）按 §25.3 四层编码解码。
//
// 字节级层次：ExecClientMessage{f1 exec_id, f15 id, f11 McpResult}
//
//	→ McpResult{f1 success}（f2 是另一类 McpError，不得承载工具结果）
//	→ McpSuccess{f1 content(repeated ContentItem), f2 bool is_error, f3 Struct structured_content}
//	→ ContentItem{f1 text=McpTextContent} → McpTextContent{f1 text(string)}
func decodeWritebackExec(t *testing.T, execPayload []byte) cursorWritebackDecoded {
	t.Helper()
	fields, err := cursorDecodeProtobuf(execPayload)
	require.NoError(t, err)
	out := cursorWritebackDecoded{execID: cursorVarintField(fields, cursorFldClientMsgID)}
	if id, ok := cursorFindField(fields, cursorFldClientMsgExecID); ok {
		out.idField = append([]byte(nil), id.Bytes...)
	}
	mcpResult, ok := cursorFindField(fields, cursorFldClientMsgMcpResult)
	require.True(t, ok, "ExecClientMessage 必须携带 f11 = McpResult")

	mcpFields, err := cursorDecodeProtobuf(mcpResult.Bytes)
	require.NoError(t, err)
	succ, hasSucc := cursorFindField(mcpFields, cursorFldMcpResultSuccess)
	_, hasErrField := cursorFindField(mcpFields, cursorFldMcpResultError)
	require.True(t, hasSucc, "结果载体必须恒为 McpResult.f1 success")
	require.False(t, hasErrField, "McpResult.f2(McpError) 不得承载工具执行结果")
	out.hasErrorField = hasErrField

	inner, err := cursorDecodeProtobuf(succ.Bytes)
	require.NoError(t, err)
	_, out.isErrorFlag = cursorFindField(inner, cursorFldMcpSuccessIsError)
	out.isErrorRaw = cursorVarintField(inner, cursorFldMcpSuccessIsError)
	out.isError = out.isErrorRaw != 0
	for _, f := range inner {
		switch f.Number {
		case cursorFldMcpSuccessContent:
			itemFields, ierr := cursorDecodeProtobuf(f.Bytes)
			require.NoError(t, ierr)
			tf, ok := cursorFindField(itemFields, cursorFldContentItemText)
			require.True(t, ok, "ContentItem 必须携带 f1 text")
			textFields, terr := cursorDecodeProtobuf(tf.Bytes)
			require.NoError(t, terr)
			textF, ok := cursorFindField(textFields, cursorFldTextContentText)
			require.True(t, ok, "McpTextContent 必须携带 f1 text")
			out.texts = append(out.texts, string(textF.Bytes))
		case cursorFldMcpSuccessStructuredContent:
			out.structured = append([]byte(nil), f.Bytes...)
		}
	}
	return out
}

// decodeWritebackStream 把一段回写字节流全部解码（同名并行两工具会写多条）。
//
// 采集到的字节是**信封流**：Envelope.Data = {f2 = ExecClientMessage}（d.send(2, msg) 的形态），
// ExecClientMessage 内层 f11 才是 McpResult。故这里先剥信封那一层，再按四层解码。
func decodeWritebackStream(t *testing.T, wire []byte) []cursorWritebackDecoded {
	t.Helper()
	var out []cursorWritebackDecoded
	// splitWritebackEnvelopes 已剥掉 envelope 与外层 f2 标签，交回裸 ExecClientMessage；
	// 这里直接按四层解码，不要再二次剥 f2（否则会把 McpResult 当成 ExecClientMessage 误传）。
	for _, data := range splitWritebackEnvelopes(t, wire) {
		out = append(out, decodeWritebackExec(t, data))
	}
	return out
}

// TestCursorWritebackFourLayerEncoding 固定 §25.3 四层编码字节形状（成功 / is_error / 结构化）。
//
// 真实字段落点：ExecClientMessage{f1 id, f15 id, f11 McpResult}、结果载体恒为
// McpResult.f1 success、失败由 McpSuccess.f2 bool is_error 表达、结构化内容是
// McpSuccess.f3 的 protobuf Struct。
func TestCursorWritebackFourLayerEncoding(t *testing.T) {
	// 成功 + 文本 + 结构化。
	okPayload, err := cursorEncodeMcpToolResult(7, []byte("exec-id-7"), "call_1",
		[]string{"sunny, 25C"}, nil, []byte(`{"temp":25}`), false)
	require.NoError(t, err)
	ok := decodeWritebackExec(t, okPayload)
	require.False(t, ok.hasErrorField, "McpResult.f2 必须不存在")
	require.False(t, ok.isError, "成功结果不得走错误分支")
	require.False(t, ok.isErrorFlag, "成功结果不得写 McpSuccess.f2")
	require.EqualValues(t, 7, ok.execID, "必须原样带回原 exec_id")
	require.Equal(t, []byte("exec-id-7"), ok.idField, "必须原样带回原 f15 id 字节")
	require.Equal(t, []string{"sunny, 25C"}, ok.texts)
	require.JSONEq(t, `{"temp":25}`, cursorTestStructJSON(t, ok.structured))

	// is_error：结果载体仍是 McpResult.f1 success，失败由 McpSuccess.f2=true 表达。
	errPayload, err := cursorEncodeMcpToolResult(9, []byte("exec-id-9"), "call_2",
		[]string{"boom"}, nil, nil, true)
	require.NoError(t, err)
	errDecoded := decodeWritebackExec(t, errPayload)
	require.False(t, errDecoded.hasErrorField, "错误不得走 McpResult.f2")
	require.True(t, errDecoded.isErrorFlag, "McpSuccess.f2 必须存在")
	require.EqualValues(t, 1, errDecoded.isErrorRaw, "McpSuccess.f2 必须是 bool true")
	require.True(t, errDecoded.isError, "is_error 结果必须保留错误语义")
	require.Equal(t, []string{"boom"}, errDecoded.texts)
	require.Empty(t, errDecoded.structured, "错误结果不携带 structured_content")

	// 多文本项：每条文本独立成一条 ContentItem（repeated），顺序保留。
	multiPayload, err := cursorEncodeMcpToolResult(3, nil, "call_3",
		[]string{"line-a", "line-b"}, nil, nil, false)
	require.NoError(t, err)
	multi := decodeWritebackExec(t, multiPayload)
	require.False(t, multi.isError)
	require.Equal(t, []string{"line-a", "line-b"}, multi.texts)
	require.Empty(t, multi.idField, "无 f15 原值时不得凭空造一个 id")
}

// TestCursorWritebackSingleToolDeliversToDuplex 覆盖单工具真实结果经生产通道写回原生双工。
//
// 真实字节必须落到 duplex.writer（被 reader 端捕获），并按四层编码解出正确文本 + 结构化。
func TestCursorWritebackSingleToolDeliversToDuplex(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	delegations := newCursorToolDelegations()
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "call_1", Name: "get_weather", ExecID: 7, IDField: []byte{0xaa, 0xbb}}))

	pr, pw := io.Pipe()
	d := newWritebackDuplex(pr, pw, delegations)
	require.NoError(t, svc.installIntake(record.id, &cursorAgentRunIntake{
		svc: svc, session: record, duplex: d, delegations: delegations,
	}))

	stop := captureDuplex(t, d)
	err := record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_1", Text: []string{"sunny"}, Structured: []byte(`{"ok":true}`)}})
	require.NoError(t, err)
	wire := stop()
	require.NotEmpty(t, wire, "真实结果必须写回原生双工")

	frames := decodeWritebackStream(t, wire)
	require.Len(t, frames, 1)
	require.False(t, frames[0].isError)
	require.Equal(t, []string{"sunny"}, frames[0].texts)
	require.JSONEq(t, `{"ok":true}`, cursorTestStructJSON(t, frames[0].structured))
	require.EqualValues(t, 7, frames[0].execID)
	require.Equal(t, []byte{0xaa, 0xbb}, frames[0].idField)
}

// TestCursorWritebackIsErrorPreserved 覆盖 is_error 语义：回写不得把本机失败写成成功。
func TestCursorWritebackIsErrorPreserved(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	delegations := newCursorToolDelegations()
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "call_err", Name: "flaky_tool", ExecID: 11, IDField: []byte{0x01}}))

	pr, pw := io.Pipe()
	d := newWritebackDuplex(pr, pw, delegations)
	require.NoError(t, svc.installIntake(record.id, &cursorAgentRunIntake{
		svc: svc, session: record, duplex: d, delegations: delegations,
	}))

	stop := captureDuplex(t, d)
	err := record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_err", Text: []string{"tool failed: timeout"}, IsError: true}})
	require.NoError(t, err)
	wire := stop()

	frames := decodeWritebackStream(t, wire)
	require.Len(t, frames, 1)
	require.False(t, frames[0].hasErrorField, "错误不得走 McpResult.f2（McpError）")
	require.True(t, frames[0].isErrorFlag, "McpSuccess.f2 必须存在")
	require.EqualValues(t, 1, frames[0].isErrorRaw, "McpSuccess.f2 必须是 bool true")
	require.True(t, frames[0].isError, "is_error 必须保留错误语义（McpSuccess.f2=true）")
	require.Equal(t, []string{"tool failed: timeout"}, frames[0].texts)
}

// TestCursorWritebackBatchEncodingFailureZeroSends 覆盖整批原子性：两条结果中第二条
// structured_content 非法 → 整批编码失败、零发送（reader 捕获字节为空），会话失败关闭。
// 不新增生产测试钩子：用现有 io.Pipe reader 的写入捕获做零发送断言。
func TestCursorWritebackBatchEncodingFailureZeroSends(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	delegations := newCursorToolDelegations()
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "c1", Name: "get_weather", ExecID: 1, IDField: []byte{0x01}}))
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "c2", Name: "get_weather", ExecID: 2, IDField: []byte{0x02}}))

	pr, pw := io.Pipe()
	d := newWritebackDuplex(pr, pw, delegations)
	require.NoError(t, svc.installIntake(record.id, &cursorAgentRunIntake{
		svc: svc, session: record, duplex: d, delegations: delegations,
	}))

	stop := captureDuplex(t, d)
	err := record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{
			{CallID: "c1", Text: []string{"first result is valid"}},
			{CallID: "c2", Text: []string{"second has bad struct"}, Structured: []byte(`{"broken":`)},
		})
	require.Error(t, err, "第二条编码失败必须返回错误")
	require.ErrorContains(t, err, "encode tool result", "错误必须来自编码阶段")

	wire := stop()
	require.Empty(t, wire, "整批编码失败必须零发送（第一条也不得写出）")

	record.mu.Lock()
	state := record.state
	record.mu.Unlock()
	require.Equal(t, cursorSessionClosed, state, "编码失败必须失败关闭会话")
}

// TestCursorWritebackParallelSameNameTwoTools 覆盖证据 28：同名并行两工具各自独立配对、各自回写。
//
// 两条委托同名（get_weather）但 call_id 与 exec 关联不同；两条真实结果必须分别落到
// 各自的 exec 关联（exec_id + f15 原值逐条对应，绝不按名字猜配对）。
func TestCursorWritebackParallelSameNameTwoTools(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	delegations := newCursorToolDelegations()
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "c1", Name: "get_weather", ExecID: 20, IDField: []byte{0xa1}}))
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "c2", Name: "get_weather", ExecID: 21, IDField: []byte{0xa2}}))

	pr, pw := io.Pipe()
	d := newWritebackDuplex(pr, pw, delegations)
	require.NoError(t, svc.installIntake(record.id, &cursorAgentRunIntake{
		svc: svc, session: record, duplex: d, delegations: delegations,
	}))

	stop := captureDuplex(t, d)
	err := record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{
			{CallID: "c1", Text: []string{"beijing: sunny"}},
			{CallID: "c2", Text: []string{"shanghai: rainy"}},
		})
	require.NoError(t, err)
	wire := stop()

	frames := decodeWritebackStream(t, wire)
	require.Len(t, frames, 2, "同名并行两工具必须各自回写一条结果")
	require.False(t, frames[0].isError)
	require.False(t, frames[1].isError)
	// 逐条对应原 exec 关联：c1→exec 20/0xa1，c2→exec 21/0xa2。
	require.EqualValues(t, 20, frames[0].execID)
	require.Equal(t, []byte{0xa1}, frames[0].idField)
	require.Equal(t, []string{"beijing: sunny"}, frames[0].texts)
	require.EqualValues(t, 21, frames[1].execID)
	require.Equal(t, []byte{0xa2}, frames[1].idField)
	require.Equal(t, []string{"shanghai: rainy"}, frames[1].texts)
}

// TestCursorWritebackTransportFailureClosesSession 覆盖发送中传输失败 → 关闭会话、零重放。
//
// 上游写入失败即失败关闭（finalizer），禁止重连重放结果。reader 端关闭使 writer.Write
// 立刻失败；验证：DeliverToolResults 返回错误、会话进入 closed 且不可再写（零重放）。
func TestCursorWritebackTransportFailureClosesSession(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	delegations := newCursorToolDelegations()
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "call_1", Name: "get_weather", ExecID: 7, IDField: []byte{0xaa, 0xbb}}))

	pr, pw := io.Pipe()
	// 关闭 reader 端：writer.Write 立刻返回 ErrClosedPipe，模拟传输失败。
	require.NoError(t, pr.Close())
	d := newWritebackDuplex(pr, pw, delegations)
	require.NoError(t, svc.installIntake(record.id, &cursorAgentRunIntake{
		svc: svc, session: record, duplex: d, delegations: delegations,
	}))

	err := record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_1", Text: []string{"sunny"}}})
	require.Error(t, err, "传输失败必须返回错误")
	require.ErrorContains(t, err, io.ErrClosedPipe.Error())

	// 会话被 finalizer 关闭（不双写、不重放）。
	record.mu.Lock()
	state := record.state
	record.mu.Unlock()
	require.Equal(t, cursorSessionClosed, state, "传输失败必须关闭会话（finalizer）")

	// 再次提交同一批结果：写通道已关闭，仍然失败（没有任何重放路径）。
	require.Error(t, record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_1", Text: []string{"sunny"}}}))
}

// TestCursorWritebackUnknownCallIDFailsClosed 覆盖提交的结果 ID 无任何已登记关联 → 失败关闭、零上游写入。
func TestCursorWritebackUnknownCallIDFailsClosed(t *testing.T) {
	svc := newCursorLifecycleTestService()
	record := startCursorLifecycleTestSession(t, svc)
	delegations := newCursorToolDelegations()
	// 只登记 call_known，故意提交 call_unknown。
	require.NoError(t, delegations.noteDelegation("",
		cursorToolCallCapture{ID: "call_known", Name: "get_weather", ExecID: 5}))

	pr, pw := io.Pipe()
	d := newWritebackDuplex(pr, pw, delegations)
	require.NoError(t, svc.installIntake(record.id, &cursorAgentRunIntake{
		svc: svc, session: record, duplex: d, delegations: delegations,
	}))

	stop := captureDuplex(t, d)
	err := record.intake.DeliverToolResults(context.Background(), record.id, cursorSessionLevelBatchID,
		[]CursorToolResult{{CallID: "call_unknown", Text: []string{"x"}}})
	require.ErrorIs(t, err, ErrCursorResumeUnknownAssociation)
	wire := stop()
	require.Empty(t, wire, "未知 ID 必须零上游写入（不伪造结果）")

	record.mu.Lock()
	state := record.state
	record.mu.Unlock()
	require.Equal(t, cursorSessionClosed, state, "未知 ID 必须失败关闭")
}

// TestCursorWritebackUninstalledFailsClosed 覆盖回写通道未装配 → intake_unavailable（失败关闭，零写入）。
func TestCursorWritebackUninstalledFailsClosed(t *testing.T) {
	it := &cursorAgentRunIntake{} // svc/session/duplex 全空：未装配
	err := it.DeliverToolResults(context.Background(), "sid", "bid",
		[]CursorToolResult{{CallID: "c", Text: []string{"x"}}})
	require.ErrorIs(t, err, ErrCursorResumeIntakeUnavailable)
}
