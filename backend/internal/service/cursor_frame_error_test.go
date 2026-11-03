//go:build unit

package service

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCursorFrameClassifyEnvelope 信封层错误归 frame_envelope，且文案与改造前逐字一致。
func TestCursorFrameClassifyEnvelope(t *testing.T) {
	_, _, err := cursorReadEnvelopeFrame([]byte{0x00, 0x00}, 0)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCursorProtocol, "哨兵语义不得改变")

	class, index, ok := cursorFrameClassify(err)
	require.True(t, ok)
	require.Equal(t, cursorFrameEnvelope, class)
	require.Equal(t, -1, index, "非流路径不带帧序号")
	require.Equal(t, "cursor: malformed protocol frame (fail closed): truncated envelope header at offset 0",
		err.Error(), "错误文案必须与改造前逐字一致")
}

// TestCursorFrameClassifyProtobuf protobuf 字段层错误归 frame_protobuf。
func TestCursorFrameClassifyProtobuf(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want string
	}{
		{"unsupported wire type 1", []byte{0x09, 0, 0, 0, 0, 0, 0, 0, 0},
			"cursor: malformed protocol frame (fail closed): unsupported wire type 1 for field 1"},
		{"truncated varint", []byte{0x08, 0x80},
			"cursor: malformed protocol frame (fail closed): truncated varint at offset 1"},
		{"invalid field number", []byte{0x00, 0x00},
			"cursor: malformed protocol frame (fail closed): invalid field number 0 at offset 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cursorDecodeProtobuf(tc.in)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrCursorProtocol)
			class, _, ok := cursorFrameClassify(err)
			require.True(t, ok)
			require.Equal(t, cursorFrameProtobuf, class)
			require.Equal(t, tc.want, err.Error())
		})
	}
}

// TestCursorFrameClassifyMcpArgs mcp_args 解码层错误归 frame_mcp_args。
func TestCursorFrameClassifyMcpArgs(t *testing.T) {
	// kind 未设置：空 Value。
	_, err := cursorDecodeMcpValue(nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCursorProtocol)
	class, _, ok := cursorFrameClassify(err)
	require.True(t, ok)
	require.Equal(t, cursorFrameMcpArgs, class)
	require.Equal(t, "cursor: malformed protocol frame (fail closed): mcp value has no kind set", err.Error())
}

// TestCursorFrameClassifyRetype retype 结构化错误（既有 cursorRetypeError）也归 frame_mcp_args，
// 不再退化为 protocol_frame 兜底。
func TestCursorFrameClassifyRetype(t *testing.T) {
	err := cursorRetypeErrorf(cursorRetypeSchemaUnresolvable)
	require.ErrorIs(t, err, ErrCursorProtocol)
	class, index, ok := cursorFrameClassify(err)
	require.True(t, ok)
	require.Equal(t, cursorFrameMcpArgs, class)
	require.Equal(t, -1, index)
}

// TestCursorFrameClassifyEndStreamPayload EndStream payload 非 JSON 归独立类别。
func TestCursorFrameClassifyEndStreamPayload(t *testing.T) {
	_, _, err := cursorParseEndStream([]byte("not-json"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCursorProtocol)
	class, _, ok := cursorFrameClassify(err)
	require.True(t, ok)
	require.Equal(t, cursorFrameEndStreamPayload, class)
}

// TestCursorFrameClassifyWithCause 带底层原因的帧级错误：哨兵与 cause 都可 errors.Is 命中，
// 文案与改造前 `%w: …: %v` 渲染一致。
func TestCursorFrameClassifyWithCause(t *testing.T) {
	err := cursorFrameErrorWrapf(cursorFrameEnvelope, io.ErrUnexpectedEOF, "read envelope: %v", io.ErrUnexpectedEOF)
	require.ErrorIs(t, err, ErrCursorProtocol)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF, "底层原因必须仍可 Is 命中，不退化既有判定")
	require.Equal(t,
		"cursor: malformed protocol frame (fail closed): read envelope: unexpected EOF",
		err.Error())
}

// TestCursorStreamErrClassFrameSplit 钉死 error_class 取值契约：
// 帧级错误输出 `protocol_frame:<class>`，裸哨兵仍输出 `protocol_frame`。
func TestCursorStreamErrClassFrameSplit(t *testing.T) {
	_, _, envErr := cursorReadEnvelopeFrame([]byte{0x00, 0x00}, 0)
	require.Equal(t, "protocol_frame:frame_envelope", cursorStreamErrClass(envErr))

	_, pbErr := cursorDecodeProtobuf([]byte{0x09, 0, 0, 0, 0, 0, 0, 0, 0})
	require.Equal(t, "protocol_frame:frame_protobuf", cursorStreamErrClass(pbErr))

	// 裸哨兵（无帧级信息）保持粗粒度，与改造前一致。
	require.Equal(t, "protocol_frame", cursorStreamErrClass(ErrCursorProtocol))
	require.Equal(t, "protocol_frame", cursorStreamErrClass(fmt.Errorf("wrapped: %w", ErrCursorProtocol)))
}

// TestCursorAnnotateFrame 帧序号只补一次，且不改类别与文案。
func TestCursorAnnotateFrame(t *testing.T) {
	err := cursorFrameErrorf(cursorFrameMessage, "received extra EndStreamResponse")
	annotated := cursorAnnotateFrame(err, 7)
	require.ErrorIs(t, annotated, ErrCursorProtocol)

	class, index, ok := cursorFrameClassify(annotated)
	require.True(t, ok)
	require.Equal(t, cursorFrameMessage, class)
	require.Equal(t, 7, index)
	require.Equal(t, err.Error(), annotated.Error(), "补帧序号不得改文案")

	// 已带序号时再标注不覆盖。
	class, index, ok = cursorFrameClassify(cursorAnnotateFrame(annotated, 99))
	require.True(t, ok)
	require.Equal(t, cursorFrameMessage, class)
	require.Equal(t, 7, index, "已有帧序号不得被后来的标注覆盖")

	// 非帧级错误原样返回。
	plain := errors.New("plain")
	require.Same(t, plain, cursorAnnotateFrame(plain, 3))
	require.Nil(t, cursorAnnotateFrame(nil, 3))
}

// TestCursorFrameErrFields 渲染 zap 字段：只出枚举与整数，无帧内容。
func TestCursorFrameErrFields(t *testing.T) {
	require.Nil(t, cursorFrameErrFields(nil))
	require.Nil(t, cursorFrameErrFields(errors.New("plain")))

	fields := cursorFrameErrFields(cursorAnnotateFrame(
		cursorFrameErrorf(cursorFrameDuplex, "malformed KV set"), 3))
	require.Len(t, fields, 2)
	require.Equal(t, "protocol_frame_class", fields[0].Key)
	require.Equal(t, "frame_duplex", fields[0].String)
	require.Equal(t, "protocol_frame_index", fields[1].Key)
	require.EqualValues(t, 3, fields[1].Integer)

	// 无序号时不输出 index 字段。
	fields = cursorFrameErrFields(cursorFrameErrorf(cursorFrameMcpArgs, "mcp value has no kind set"))
	require.Len(t, fields, 1)
	require.Equal(t, "protocol_frame_class", fields[0].Key)
}

// TestCursorFrameErrClassFamilyCoverage 钉死"每一类都有出口"：全部帧级构造点产出的
// error_class 都不得再退化回裸 protocol_frame。
func TestCursorFrameErrClassFamilyCoverage(t *testing.T) {
	for _, tc := range []struct {
		class cursorFrameClass
		want  string
	}{
		{cursorFrameEnvelope, "protocol_frame:frame_envelope"},
		{cursorFrameProtobuf, "protocol_frame:frame_protobuf"},
		{cursorFrameMessage, "protocol_frame:frame_message"},
		{cursorFrameEndStreamPayload, "protocol_frame:frame_endstream_payload"},
		{cursorFrameMcpArgs, "protocol_frame:frame_mcp_args"},
		{cursorFrameDuplex, "protocol_frame:frame_duplex"},
		{cursorFrameDelegation, "protocol_frame:frame_delegation"},
		{cursorFrameInlineToolCall, "protocol_frame:frame_inline_tool_call"},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			err := cursorFrameErrorf(tc.class, "probe")
			require.ErrorIs(t, err, ErrCursorProtocol)
			require.Equal(t, tc.want, cursorStreamErrClass(err))
		})
	}
}

// TestCursorUserFacingErrorUnchanged 用户可见错误映射仍是 CUR-EXEC-03：拆帧分类不得
// 影响对外错误码（用户不需要知道是第几帧）。
func TestCursorUserFacingErrorUnchanged(t *testing.T) {
	_, _, envErr := cursorReadEnvelopeFrame([]byte{0x00, 0x00}, 0)
	status, code, _, ok := CursorUserFacingError(t.Context(), envErr)
	require.True(t, ok)
	require.Equal(t, 502, status)
	require.Equal(t, "CUR-EXEC-03", code)
}
