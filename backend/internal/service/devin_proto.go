package service

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// Devin / Codeium Cascade API 的最小 Protobuf 编解码器。
// 基于 devin-gateway (github.com/CaiJingLong/devin-gateway) 的 TypeScript 实现移植。
//
// 只实现网关实际使用的消息形状与字段编号。Proto3 语义：零值标量字段不编码；
// 字符串/字节默认为空串；repeated 字段用重复标签发送（消息/字符串类型不用 packed 编码）。

// ProtoEncoder 最小 Protobuf 编码器
type ProtoEncoder struct {
	buf []byte
}

func (e *ProtoEncoder) varint(n uint64) {
	for n > 0x7f {
		e.buf = append(e.buf, byte(n&0x7f)|0x80)
		n >>= 7
	}
	e.buf = append(e.buf, byte(n))
}

func (e *ProtoEncoder) tag(field int, wire int) {
	e.varint(uint64((field << 3) | wire))
}

func (e *ProtoEncoder) String(field int, value string) {
	if value == "" {
		return
	}
	e.tag(field, 2)
	e.varint(uint64(len(value)))
	e.buf = append(e.buf, []byte(value)...)
}

func (e *ProtoEncoder) Uint32(field int, value uint32) {
	if value == 0 {
		return
	}
	e.tag(field, 0)
	e.varint(uint64(value))
}

func (e *ProtoEncoder) Uint64(field int, value uint64) {
	if value == 0 {
		return
	}
	e.tag(field, 0)
	e.varint(value)
}

func (e *ProtoEncoder) Bool(field int, value bool) {
	if !value {
		return
	}
	e.tag(field, 0)
	e.varint(1)
}

func (e *ProtoEncoder) Double(field int, value float64) {
	if value == 0 {
		return
	}
	e.tag(field, 1)
	bits := math.Float64bits(value)
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, bits)
	e.buf = append(e.buf, buf...)
}

func (e *ProtoEncoder) Message(field int, encode func(*ProtoEncoder)) {
	sub := &ProtoEncoder{}
	encode(sub)
	e.tag(field, 2)
	e.varint(uint64(len(sub.buf)))
	e.buf = append(e.buf, sub.buf...)
}

func (e *ProtoEncoder) RepeatedMessage(field int, values []interface{}, encode func(*ProtoEncoder, interface{})) {
	for _, v := range values {
		e.Message(field, func(sub *ProtoEncoder) {
			encode(sub, v)
		})
	}
}

func (e *ProtoEncoder) RepeatedString(field int, values []string) {
	for _, v := range values {
		e.String(field, v)
	}
}

func (e *ProtoEncoder) Bytes() []byte {
	return e.buf
}

// ProtoDecoder 最小 Protobuf 解码器
type ProtoDecoder struct {
	buf []byte
	pos int
}

func NewProtoDecoder(data []byte) *ProtoDecoder {
	return &ProtoDecoder{buf: data, pos: 0}
}

func (d *ProtoDecoder) varint() uint64 {
	var result uint64
	var shift uint
	for d.pos < len(d.buf) {
		b := d.buf[d.pos]
		d.pos++
		result |= uint64(b&0x7f) << shift
		if (b & 0x80) == 0 {
			break
		}
		shift += 7
	}
	return result
}

func (d *ProtoDecoder) ReadTag() (field int, wire int) {
	tag := d.varint()
	return int(tag >> 3), int(tag & 0x07)
}

func (d *ProtoDecoder) ReadString() string {
	length := int(d.varint())
	start := d.pos
	d.pos += length
	if d.pos > len(d.buf) {
		d.pos = len(d.buf)
	}
	return string(d.buf[start:d.pos])
}

func (d *ProtoDecoder) ReadBytes() []byte {
	length := int(d.varint())
	start := d.pos
	d.pos += length
	if d.pos > len(d.buf) {
		d.pos = len(d.buf)
	}
	return d.buf[start:d.pos]
}

func (d *ProtoDecoder) ReadDouble() float64 {
	if d.pos+8 > len(d.buf) {
		d.pos = len(d.buf)
		return 0
	}
	bits := binary.LittleEndian.Uint64(d.buf[d.pos : d.pos+8])
	d.pos += 8
	return math.Float64frombits(bits)
}

func (d *ProtoDecoder) ReadUint32() uint32 {
	return uint32(d.varint())
}

func (d *ProtoDecoder) ReadUint64() uint64 {
	return d.varint()
}

func (d *ProtoDecoder) ReadBool() bool {
	return d.varint() != 0
}

func (d *ProtoDecoder) Skip(wire int) {
	switch wire {
	case 0: // varint
		d.varint()
	case 1: // 64-bit
		d.pos += 8
	case 2: // length-delimited
		length := int(d.varint())
		d.pos += length
	case 5: // 32-bit
		d.pos += 4
	}
}

func (d *ProtoDecoder) Done() bool {
	return d.pos >= len(d.buf)
}

// ─── Metadata ────────────────────────────────────────────────────────────────

// DevinMetadata 承载 Devin API 认证与客户端信息
type DevinMetadata struct {
	IDEName          string
	IDEVersion       string
	ExtensionName    string
	ExtensionVersion string
	APIKey           string
	Locale           string
	UserJWT          string
}

func encodeDevinMetadata(e *ProtoEncoder, m *DevinMetadata) {
	e.String(1, m.IDEName)
	e.String(2, m.IDEVersion)
	e.String(3, m.ExtensionName)
	e.String(4, m.ExtensionVersion)
	e.String(6, m.APIKey)
	e.String(13, m.Locale)
	e.String(17, m.UserJWT)
}

// ─── GetUserJwt ──────────────────────────────────────────────────────────────

func encodeGetUserJwtRequest(m *DevinMetadata) []byte {
	e := &ProtoEncoder{}
	e.Message(1, func(sub *ProtoEncoder) {
		encodeDevinMetadata(sub, m)
	})
	return e.Bytes()
}

// GetUserJwtResponse 是 Devin 认证响应
type GetUserJwtResponse struct {
	UserJWT            string
	CustomAPIServerURL string
}

func decodeGetUserJwtResponse(data []byte) (*GetUserJwtResponse, error) {
	d := NewProtoDecoder(data)
	res := &GetUserJwtResponse{}
	for !d.Done() {
		field, wire := d.ReadTag()
		if wire != 2 {
			d.Skip(wire)
			continue
		}
		switch field {
		case 1:
			res.UserJWT = d.ReadString()
		case 2:
			res.CustomAPIServerURL = d.ReadString()
		default:
			d.Skip(wire)
		}
	}
	return res, nil
}

// tryGunzipResponse 尝试 gzip 解压响应（Devin 可能返回压缩响应）
func tryGunzipResponse(data []byte) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gzip decompress: %w", err)
	}
	defer gr.Close()
	uncompressed, err := io.ReadAll(gr)
	if err != nil {
		return nil, fmt.Errorf("read gzip: %w", err)
	}
	return uncompressed, nil
}
