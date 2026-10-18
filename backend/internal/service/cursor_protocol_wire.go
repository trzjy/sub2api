package service

// 本文件实现 Cursor（Connect RPC v1）协议的**线上层编解码**：5 字节 envelope 与
// 手写 protobuf varint / len-delimited 编解码（E6 卡，方案
// docs-local/cursor-platform-fix-plan.md §1.4 / §1.5）。
//
// 传输形态（证据 02-request-body.md / 03-response-frames.md，活体复核 07/08）：
//
//	+---------+------------------+-------------------+
//	| flags   | length (u32, BE) | payload           |
//	| u8      |                  | length 字节        |
//	+---------+------------------+-------------------+
//	 0         1..4               5 .. 5+len-1
//
// flags 位（客户端 @14763950 / @14778311 实测）：
//   bit0 (1) = payload 已 gzip 压缩；bit1 (2) = EndStream 终态帧。
//
// **为什么手写而不引 protoc-gen-go**：只用到十来个字段、且生成物不入库，
// 手写 60 行编解码比维护一条 protoc 工具链更省；字段号全部来自已取证的 schema
// （见本文件各处注释），未经取证的字段一律不写。
//
// **失败关闭**：本文件对畸形输入一律返回错误，绝不启发式猜测。未知 wire type
// 直接报错（不跳字节），因为错位解析出的字段号会污染上层语义。

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// envelope
// ---------------------------------------------------------------------------

const (
	// cursorEnvelopeHeaderSize 是 envelope 头长度（flags 1 + length 4）。
	cursorEnvelopeHeaderSize = 5

	// cursorEnvelopeFlagCompressed 是 bit0：payload 已压缩。
	//
	// ⚠️ Cursor 客户端声明 compressMinBytes=MAX_SAFE_INTEGER 且 sendCompression=nil，
	// 即**从不压缩也不接受压缩**（证据 03 §1）。本层不做 gunzip——收到压缩帧说明
	// 上游行为与客户端预期不符，按协议错误失败关闭，绝不猜。
	cursorEnvelopeFlagCompressed = 0x01

	// cursorEnvelopeFlagEndStream 是 bit1：EndStream 终态帧（**强制存在**）。
	cursorEnvelopeFlagEndStream = 0x02
)

// Cursor envelope / protobuf 层的失败关闭错误。
var (
	// ErrCursorProtocol 表示帧结构不合法（截断、未知 wire type、压缩帧等）。
	ErrCursorProtocol = errors.New("cursor: malformed protocol frame (fail closed)")

	// ErrCursorMissingEndStream 表示流已结束但**没有任何终态标记**。
	//
	// Connect 侧该标记是 EndStreamResponse（客户端 @14784500：缺失即
	// `protocol error: missing EndStreamResponse`）；AgentRun 侧的常规终态是
	// InteractionUpdate.turn_ended（证据 33/63 均在流尾给出）。两者皆无 ⇒
	// 这是一条被取消或截断的流，**不复用「流正常结束」路径**，直接失败关闭：
	// 它拿不到真实 usage，按成功收尾就等于回写一条 0 usage 的成功响应并误导计费。
	ErrCursorMissingEndStream = errors.New("cursor: missing EndStreamResponse (protocol error)")
)

// cursorEnvelopeFrame 是一帧 envelope 的解析结果。
type cursorEnvelopeFrame struct {
	Flags byte
	// Data 是 payload 的**切片引用**（不复制），仅在解析期间有效。
	Data []byte
}

// cursorIsEndStream 报告该帧是否为 EndStream 终态帧。
func (f cursorEnvelopeFrame) cursorIsEndStream() bool {
	return f.Flags&cursorEnvelopeFlagEndStream != 0
}

// cursorEncodeEnvelope 把 payload 封装为 envelope。
//
// flags 恒为 0：本层从不压缩（见 cursorEnvelopeFlagCompressed 的说明）。
func cursorEncodeEnvelope(payload []byte) []byte {
	out := make([]byte, cursorEnvelopeHeaderSize+len(payload))
	out[0] = 0
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// cursorReadEnvelopeFrame 从 b 的偏移 off 处解析一帧。
//
// 返回帧与其**下一帧的起始偏移**。任何长度不自洽都返回 ErrCursorProtocol——
// 尤其"声明长度 > 实收字节"（客户端文案 `promised {N} bytes, received {M}`）
// 与尾部半帧（`incomplete envelope`）都归为协议错误。
func cursorReadEnvelopeFrame(b []byte, off int) (cursorEnvelopeFrame, int, error) {
	if off < 0 || off+cursorEnvelopeHeaderSize > len(b) {
		return cursorEnvelopeFrame{}, 0, fmt.Errorf("%w: truncated envelope header at offset %d", ErrCursorProtocol, off)
	}
	flags := b[off]
	if flags&cursorEnvelopeFlagCompressed != 0 {
		// 不解压缩：客户端从不压缩，中转若启用压缩客户端会直接抛错（证据 03 §1）。
		return cursorEnvelopeFrame{}, 0, fmt.Errorf(
			"%w: compressed envelope (flags=0x%02x) but client never negotiates compression", ErrCursorProtocol, flags)
	}
	n := binary.BigEndian.Uint32(b[off+1 : off+5])
	// uint32 → int 在 32 位平台可能溢出，先按 uint64 比对再做转换。
	if uint64(n) > uint64(len(b)-off-cursorEnvelopeHeaderSize) {
		return cursorEnvelopeFrame{}, 0, fmt.Errorf(
			"%w: promised %d bytes, received %d", ErrCursorProtocol, n, len(b)-off-cursorEnvelopeHeaderSize)
	}
	start := off + cursorEnvelopeHeaderSize
	return cursorEnvelopeFrame{Flags: flags, Data: b[start : start+int(n)]}, start + int(n), nil
}

// ---------------------------------------------------------------------------
// protobuf 编码
// ---------------------------------------------------------------------------

// protobuf wire type（本层只用到 0 与 2）。
const (
	cursorWireVarint = 0
	cursorWireBytes  = 2
)

// cursorAppendVarint 追加一个 varint。
func cursorAppendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// cursorAppendTag 追加字段标签（field<<3 | wire）。
func cursorAppendTag(b []byte, field, wire int) []byte {
	return cursorAppendVarint(b, uint64(field)<<3|uint64(wire))
}

// cursorAppendBytesField 追加一个 len-delimited 字段（string / bytes / message 共用）。
func cursorAppendBytesField(b []byte, field int, payload []byte) []byte {
	b = cursorAppendTag(b, field, cursorWireBytes)
	b = cursorAppendVarint(b, uint64(len(payload)))
	return append(b, payload...)
}

// cursorAppendStringField 追加一个 string 字段。空串**不写**（proto3 默认值省略，
// 与官方客户端编码一致——避免服务端把"显式空串"与"未设置"当两回事）。
func cursorAppendStringField(b []byte, field int, s string) []byte {
	if s == "" {
		return b
	}
	return cursorAppendBytesField(b, field, []byte(s))
}

// cursorAppendVarintField 追加一个 varint 字段（enum / bool / int32）。
func cursorAppendVarintField(b []byte, field int, v uint64) []byte {
	b = cursorAppendTag(b, field, cursorWireVarint)
	return cursorAppendVarint(b, v)
}

// ---------------------------------------------------------------------------
// protobuf 解码
// ---------------------------------------------------------------------------

// cursorPBField 是一个已解析的 protobuf 字段。
//
// 注意语义：**同号字段重复出现时后值覆盖前值**（protobuf 的 last-one-wins 规则）；
// repeated 字段由调用方按需累积，本层的对话响应解析不依赖 repeated 语义。
type cursorPBField struct {
	Number int
	Wire   int
	// Bytes 仅在 Wire == 2 时有意义，且**引用输入缓冲**（不复制）。
	Bytes []byte
	// Varint 仅在 Wire == 0 时有意义。
	Varint uint64
}

// String 便捷取值（仅 Wire==2 有效）。
func (f cursorPBField) String() string { return string(f.Bytes) }

// cursorDecodeProtobuf 解析一层 protobuf 字段序列。
//
// 失败关闭：未知 wire type / 长度越界 / varint 溢出 / trailing garbage 一律报错。
// 绝不做"尽力而为"的启发式解析——错位解析出的字段号会污染上层语义。
func cursorDecodeProtobuf(b []byte) ([]cursorPBField, error) {
	var out []cursorPBField
	i := 0
	for i < len(b) {
		key, n, err := cursorConsumeVarint(b, i)
		if err != nil {
			return nil, err
		}
		i = n
		field := int(key >> 3)
		wire := int(key & 0x7)
		if field <= 0 {
			return nil, fmt.Errorf("%w: invalid field number %d at offset %d", ErrCursorProtocol, field, i)
		}
		switch wire {
		case cursorWireVarint:
			v, n, err := cursorConsumeVarint(b, i)
			if err != nil {
				return nil, err
			}
			i = n
			out = append(out, cursorPBField{Number: field, Wire: wire, Varint: v})
		case cursorWireBytes:
			ln, n, err := cursorConsumeVarint(b, i)
			if err != nil {
				return nil, err
			}
			i = n
			if ln > uint64(len(b)-i) {
				return nil, fmt.Errorf(
					"%w: field %d promises %d bytes, only %d remain", ErrCursorProtocol, field, ln, len(b)-i)
			}
			out = append(out, cursorPBField{Number: field, Wire: wire, Bytes: b[i : i+int(ln)]})
			i += int(ln)
		default:
			// 固定 32/64 位 wire type（5/1）本协议不使用。直接失败，不跳 4/8 字节
			// 继续解——那会让后续字段全部错位。
			return nil, fmt.Errorf("%w: unsupported wire type %d for field %d", ErrCursorProtocol, wire, field)
		}
	}
	return out, nil
}

// cursorConsumeVarint 从 b[off] 解一个 varint，返回值与新偏移。
func cursorConsumeVarint(b []byte, off int) (uint64, int, error) {
	var v uint64
	var shift uint
	for i := off; i < len(b); i++ {
		c := b[i]
		if shift >= 64 {
			return 0, 0, fmt.Errorf("%w: varint overflow at offset %d", ErrCursorProtocol, off)
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, i + 1, nil
		}
		shift += 7
		if shift > 63 && i+1 < len(b) {
			// 10 字节 varint 的高位只允许 1 位；再多即溢出。
			return 0, 0, fmt.Errorf("%w: varint too long at offset %d", ErrCursorProtocol, off)
		}
	}
	return 0, 0, fmt.Errorf("%w: truncated varint at offset %d", ErrCursorProtocol, off)
}

// cursorFindField 返回指定字段号的最后一次出现（protobuf last-one-wins）。
func cursorFindField(fields []cursorPBField, number int) (cursorPBField, bool) {
	for i := len(fields) - 1; i >= 0; i-- {
		if fields[i].Number == number {
			return fields[i], true
		}
	}
	return cursorPBField{}, false
}
