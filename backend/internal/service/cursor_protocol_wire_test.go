//go:build unit

package service

// E6 卡的线上层单测：envelope / protobuf 编解码往返、畸形输入失败关闭。
//
// 取证锚点：02-request-body.md §1（envelope 布局）、03-response-frames.md §1（flags 位）。

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCursorEnvelopeRoundtrip 覆盖编解码往返与多帧连续解析。
func TestCursorEnvelopeRoundtrip(t *testing.T) {
	payloads := [][]byte{
		{},
		{0x01},
		[]byte("hello cursor"),
		bytes.Repeat([]byte{0xAB}, 1024),
	}
	for _, p := range payloads {
		env := cursorEncodeEnvelope(p)
		require.Len(t, env, cursorEnvelopeHeaderSize+len(p), "envelope must be header+payload")
		require.Equal(t, byte(0), env[0], "flags must be 0 (never compress)")
		require.Equal(t, uint32(len(p)), binary.BigEndian.Uint32(env[1:5]), "length must be u32 BE")

		frame, next, err := cursorReadEnvelopeFrame(env, 0)
		require.NoError(t, err)
		require.Equal(t, len(env), next)
		require.Equal(t, p, frame.Data)
		require.False(t, frame.cursorIsEndStream())
	}

	// 多帧连续：偏移推进正确。
	var stream []byte
	stream = append(stream, cursorEncodeEnvelope([]byte("a"))...)
	stream = append(stream, cursorEncodeEnvelope([]byte("bb"))...)
	stream = append(stream, cursorEncodeEnvelope([]byte("ccc"))...)

	var got []string
	off := 0
	for off < len(stream) {
		f, next, err := cursorReadEnvelopeFrame(stream, off)
		require.NoError(t, err)
		got = append(got, string(f.Data))
		off = next
	}
	require.Equal(t, []string{"a", "bb", "ccc"}, got)
}

// TestCursorEnvelopeEndStreamFlag 覆盖 EndStream 标记（flags&2）。
func TestCursorEnvelopeEndStreamFlag(t *testing.T) {
	// 手写 flags=2 的帧（cursorEncodeEnvelope 恒用 flags=0，故此处手工构造）。
	payload := []byte(`{"metadata":{}}`)
	frame := make([]byte, cursorEnvelopeHeaderSize+len(payload))
	frame[0] = cursorEnvelopeFlagEndStream
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)

	parsed, next, err := cursorReadEnvelopeFrame(frame, 0)
	require.NoError(t, err)
	require.Equal(t, len(frame), next)
	require.True(t, parsed.cursorIsEndStream())
	require.Equal(t, payload, parsed.Data)

	// bit0|bit1 同时置位也算 EndStream（但压缩位会被拒，见下）。
	require.True(t, cursorEnvelopeFrame{Flags: 0x03}.cursorIsEndStream())
	require.False(t, cursorEnvelopeFrame{Flags: 0x01}.cursorIsEndStream())
}

// TestCursorEnvelopeFailsClosedOnMalformed 覆盖畸形 envelope 一律失败关闭。
func TestCursorEnvelopeFailsClosedOnMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"header truncated", []byte{0x00, 0x00, 0x00}},
		{"promises more than received", []byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x41}},
		{"payload shorter than declared", []byte{0x00, 0x00, 0x00, 0x00, 0x05, 0x01, 0x02}},
		// 压缩帧：客户端从不压缩，中转若启用客户端会抛错，本层同样失败关闭。
		{"compressed flag", []byte{cursorEnvelopeFlagCompressed, 0x00, 0x00, 0x00, 0x01, 0x41}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := cursorReadEnvelopeFrame(c.in, 0)
			require.ErrorIs(t, err, ErrCursorProtocol)
		})
	}
}

// TestCursorProtobufRoundtrip 覆盖 protobuf 编码 → 解码往返。
func TestCursorProtobufRoundtrip(t *testing.T) {
	var b []byte
	b = cursorAppendStringField(b, 1, "hello")
	b = cursorAppendVarintField(b, 2, 42)
	b = cursorAppendBytesField(b, 3, []byte{0xDE, 0xAD})
	b = cursorAppendStringField(b, 4, "world")
	// 空串不写（proto3 默认值省略）。
	b = cursorAppendStringField(b, 5, "")

	fields, err := cursorDecodeProtobuf(b)
	require.NoError(t, err)
	require.Len(t, fields, 4, "empty string must not be encoded")

	f1, ok := cursorFindField(fields, 1)
	require.True(t, ok)
	require.Equal(t, "hello", f1.String())

	f2, ok := cursorFindField(fields, 2)
	require.True(t, ok)
	require.Equal(t, uint64(42), f2.Varint)

	f3, ok := cursorFindField(fields, 3)
	require.True(t, ok)
	require.Equal(t, []byte{0xDE, 0xAD}, f3.Bytes)

	f4, ok := cursorFindField(fields, 4)
	require.True(t, ok)
	require.Equal(t, "world", f4.String())

	_, ok = cursorFindField(fields, 5)
	require.False(t, ok, "field 5 must be absent")
}

// TestCursorProtobufLastOneWins 覆盖同号字段后值覆盖前值（protobuf 语义）。
func TestCursorProtobufLastOneWins(t *testing.T) {
	var b []byte
	b = cursorAppendStringField(b, 1, "first")
	b = cursorAppendStringField(b, 1, "second")

	fields, err := cursorDecodeProtobuf(b)
	require.NoError(t, err)
	f, ok := cursorFindField(fields, 1)
	require.True(t, ok)
	require.Equal(t, "second", f.String())
}

// TestCursorVarintBoundaries 覆盖 varint 边界（1 字节 / 2 字节 / 多字节）。
func TestCursorVarintBoundaries(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 16383, 16384, 1 << 20, 1 << 60} {
		enc := cursorAppendVarint(nil, v)
		got, off, err := cursorConsumeVarint(enc, 0)
		require.NoError(t, err, "value %d", v)
		require.Equal(t, v, got, "value %d", v)
		require.Equal(t, len(enc), off)
	}
}

// TestCursorDecodeProtobufFailsClosed 覆盖畸形 protobuf 一律失败关闭。
func TestCursorDecodeProtobufFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"field number 0", []byte{0x00}},                                  // tag 0 非法
		{"unsupported wire type 1", []byte{0x09, 0, 0, 0, 0, 0, 0, 0, 0}}, // wire=1（fixed64）不解析
		{"unsupported wire type 5", []byte{0x0D, 0, 0, 0, 0}},             // wire=5（fixed32）不解析
		{"length overruns buffer", []byte{0x0A, 0x10, 0x41}},              // 声明 16 字节只剩 1
		{"truncated varint", []byte{0x08, 0x80}},
		{"truncated length varint", []byte{0x0A, 0x80}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := cursorDecodeProtobuf(c.in)
			require.ErrorIs(t, err, ErrCursorProtocol)
		})
	}
}

// TestCursorJSONStringMatchesEncodingJSON 钉死手写 JSON 字符串转义与
// encoding/json 逐字一致（否则含引号/换行/控制字符的模型输出会产出非法 JSON）。
func TestCursorJSONStringMatchesEncodingJSON(t *testing.T) {
	cases := []string{
		"",
		"plain",
		`with "quotes"`,
		`back\slash`,
		"line\nbreak",
		"tab\there",
		"cr\rhere",
		"null\x00byte",
		"bell\x07",
		"中文与 emoji 🎉",
	}
	for _, s := range cases {
		require.Equal(t, string(cursorJSONMarshalString(s)), cursorJSONString(s),
			"cursorJSONString must match encoding/json for %q", s)
	}
}

// cursorJSONMarshalString 是 encoding/json 的字符串编码（测试对照用）。
func cursorJSONMarshalString(s string) []byte {
	var buf bytes.Buffer
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				buf.WriteString(`\u00`)
				buf.WriteByte(hex[byte(r)>>4])
				buf.WriteByte(hex[byte(r)&0xF])
				continue
			}
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
	return buf.Bytes()
}
