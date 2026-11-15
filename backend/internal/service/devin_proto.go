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

// ─── 枚举常量（proto3 数值，与参考实现一致）────────────────────────────────

const (
	ChatMessageSourceUnspecified  uint32 = 0
	ChatMessageSourceUser         uint32 = 1
	ChatMessageSourceSystem       uint32 = 2
	ChatMessageSourceUnknown      uint32 = 3
	ChatMessageSourceTool         uint32 = 4
	ChatMessageSourceSystemPrompt uint32 = 5

	StopReasonUnspecified  int32 = 0
	StopReasonMaxTokens    int32 = 3
	StopReasonFunctionCall int32 = 10

	ChatMessageRequestTypeCascade    uint32 = 5
	ConversationalPlannerModeDefault uint32 = 1
	CacheControlTypeEphemeral        uint32 = 1
)

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
	if start > len(d.buf) {
		start = len(d.buf)
	}
	if d.pos > len(d.buf) {
		d.pos = len(d.buf)
	}
	return string(d.buf[start:d.pos])
}

func (d *ProtoDecoder) ReadBytes() []byte {
	length := int(d.varint())
	start := d.pos
	d.pos += length
	if start > len(d.buf) {
		start = len(d.buf)
	}
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
		d.pos += int(d.varint())
	case 5: // 32-bit
		d.pos += 4
	}
	if d.pos > len(d.buf) {
		d.pos = len(d.buf)
	}
}

func (d *ProtoDecoder) Done() bool {
	return d.pos >= len(d.buf)
}

// ─── Metadata ────────────────────────────────────────────────────────────────

// Metadata 承载 Devin API 认证与客户端指纹信息。
type Metadata struct {
	IDEName          string // field 1
	IDEVersion       string // field 7
	ExtensionName    string // field 12
	ExtensionVersion string // field 2
	APIKey           string // field 3
	Locale           string // field 4
	UserJWT          string // field 21
}

func encodeMetadata(e *ProtoEncoder, m *Metadata) {
	if m == nil {
		return
	}
	e.String(1, m.IDEName)
	e.String(7, m.IDEVersion)
	e.String(12, m.ExtensionName)
	e.String(2, m.ExtensionVersion)
	e.String(3, m.APIKey)
	e.String(4, m.Locale)
	e.String(21, m.UserJWT)
}

// ─── GetUserJwt ──────────────────────────────────────────────────────────────

func encodeGetUserJwtRequest(m *Metadata) []byte {
	e := &ProtoEncoder{}
	e.Message(1, func(sub *ProtoEncoder) {
		encodeMetadata(sub, m)
	})
	return e.Bytes()
}

// GetUserJwtResponse 是 Devin 认证响应。
type GetUserJwtResponse struct {
	UserJWT            string // field 1
	CustomAPIServerURL string // field 2
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

// tryGunzipResponse 尝试 gzip 解压响应（Devin 可能返回压缩响应）。
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

// ─── ImageData ───────────────────────────────────────────────────────────────

// ImageData 是 Devin 图片输入（v1 网关不使用，仅保留编解码完整性）。
type ImageData struct {
	Base64Data string // field 1
	MimeType   string // field 2
}

func encodeImageData(e *ProtoEncoder, img *ImageData) {
	if img == nil {
		return
	}
	e.String(1, img.Base64Data)
	e.String(2, img.MimeType)
}

// ─── ChatToolCall ────────────────────────────────────────────────────────────

// ChatToolCall 是 Devin 工具调用（请求与响应共用形状）。
type ChatToolCall struct {
	ID               string // field 1
	Name             string // field 2
	ArgumentsJSON    string // field 3
	InvalidJSONStr   string // field 4
	InvalidJSONErr   string // field 5
	IsCustomToolCall bool   // field 6
}

func encodeChatToolCall(e *ProtoEncoder, tc *ChatToolCall) {
	if tc == nil {
		return
	}
	e.String(1, tc.ID)
	e.String(2, tc.Name)
	e.String(3, tc.ArgumentsJSON)
	e.String(4, tc.InvalidJSONStr)
	e.String(5, tc.InvalidJSONErr)
	e.Bool(6, tc.IsCustomToolCall)
}

func decodeChatToolCall(d *ProtoDecoder) ChatToolCall {
	tc := ChatToolCall{}
	for !d.Done() {
		field, wire := d.ReadTag()
		switch field {
		case 1:
			tc.ID = d.ReadString()
		case 2:
			tc.Name = d.ReadString()
		case 3:
			tc.ArgumentsJSON = d.ReadString()
		case 4:
			tc.InvalidJSONStr = d.ReadString()
		case 5:
			tc.InvalidJSONErr = d.ReadString()
		case 6:
			tc.IsCustomToolCall = d.ReadBool()
		default:
			d.Skip(wire)
		}
	}
	return tc
}

// ─── ChatMessagePrompt ───────────────────────────────────────────────────────

// ChatMessagePrompt 是 Devin 会话中的单条消息。
type ChatMessagePrompt struct {
	MessageID         string         // field 1
	Source            uint32         // field 2 (ChatMessageSource)
	Prompt            string         // field 3
	ToolCalls         []ChatToolCall // field 6 (repeated message)
	ToolCallId        string         // field 7
	ToolResultIsError bool           // field 9
	Images            []ImageData    // field 10 (repeated message)
	Thinking          string         // field 11
	Signature         string         // field 12
	SignatureType     string         // field 18
}

func encodeChatMessagePrompt(e *ProtoEncoder, p *ChatMessagePrompt) {
	if p == nil {
		return
	}
	e.String(1, p.MessageID)
	e.Uint32(2, p.Source)
	e.String(3, p.Prompt)
	for i := range p.ToolCalls {
		tc := &p.ToolCalls[i]
		e.Message(6, func(sub *ProtoEncoder) { encodeChatToolCall(sub, tc) })
	}
	e.String(7, p.ToolCallId)
	e.Bool(9, p.ToolResultIsError)
	for i := range p.Images {
		img := &p.Images[i]
		e.Message(10, func(sub *ProtoEncoder) { encodeImageData(sub, img) })
	}
	e.String(11, p.Thinking)
	e.String(12, p.Signature)
	e.String(18, p.SignatureType)
}

// ─── ChatToolDefinition ──────────────────────────────────────────────────────

// ChatToolDefinition 是 Devin 工具定义。
type ChatToolDefinition struct {
	Name             string // field 1
	Description      string // field 2
	JSONSchemaString string // field 3
	Strict           bool   // field 12
}

func encodeChatToolDefinition(e *ProtoEncoder, t *ChatToolDefinition) {
	if t == nil {
		return
	}
	e.String(1, t.Name)
	e.String(2, t.Description)
	e.String(3, t.JSONSchemaString)
	e.Bool(12, t.Strict)
}

// ─── ChatToolChoice ──────────────────────────────────────────────────────────

// ChatToolChoice 是 Devin 工具选择策略。
type ChatToolChoice struct {
	OptionName string // field 1 ("auto"/"none"/"any"/...)
	ToolName   string // field 2
}

func encodeChatToolChoice(e *ProtoEncoder, c *ChatToolChoice) {
	if c == nil {
		return
	}
	e.String(1, c.OptionName)
	e.String(2, c.ToolName)
}

// ─── CompletionConfiguration ─────────────────────────────────────────────────

// CompletionConfiguration 是 Devin 生成配置。
type CompletionConfiguration struct {
	NumCompletions      uint64   // field 1
	MaxTokens           uint64   // field 2
	MaxNewlines         uint64   // field 3
	Temperature         float64  // field 5
	FirstTemperature    float64  // field 6
	TopK                uint64   // field 7
	TopP                float64  // field 8
	StopPatterns        []string // field 9 (repeated string)
	FimEotProbThreshold float64  // field 11
}

func encodeCompletionConfiguration(e *ProtoEncoder, c *CompletionConfiguration) {
	if c == nil {
		return
	}
	e.Uint64(1, c.NumCompletions)
	e.Uint64(2, c.MaxTokens)
	e.Uint64(3, c.MaxNewlines)
	e.Double(5, c.Temperature)
	e.Double(6, c.FirstTemperature)
	e.Uint64(7, c.TopK)
	e.Double(8, c.TopP)
	e.RepeatedString(9, c.StopPatterns)
	e.Double(11, c.FimEotProbThreshold)
}

// ─── GetChatMessageRequest ───────────────────────────────────────────────────

// GetChatMessageRequest 是 Devin GetChatMessage 请求。
// 字段 7/13/20 为协议固定值，由编码器写入，不暴露为结构体字段。
type GetChatMessageRequest struct {
	Metadata                 *Metadata                // field 1
	Prompt                   string                   // field 2 (system prompt)
	ChatMessagePrompts       []ChatMessagePrompt      // field 3
	ChatModelUID             string                   // field 21
	Configuration            *CompletionConfiguration // field 8
	Tools                    []ChatToolDefinition     // field 10
	DisableParallelToolCalls bool                     // field 11
	ToolChoice               *ChatToolChoice          // field 12
	CascadeID                string                   // field 16
	ExecutionID              string                   // field 22
}

func encodeGetChatMessageRequest(req *GetChatMessageRequest) []byte {
	e := &ProtoEncoder{}
	if req == nil {
		return e.Bytes()
	}
	if req.Metadata != nil {
		e.Message(1, func(sub *ProtoEncoder) { encodeMetadata(sub, req.Metadata) })
	}
	e.String(2, req.Prompt)
	for i := range req.ChatMessagePrompts {
		p := &req.ChatMessagePrompts[i]
		e.Message(3, func(sub *ProtoEncoder) { encodeChatMessagePrompt(sub, p) })
	}
	e.String(21, req.ChatModelUID)
	e.Uint32(7, ChatMessageRequestTypeCascade)
	if req.Configuration != nil {
		e.Message(8, func(sub *ProtoEncoder) { encodeCompletionConfiguration(sub, req.Configuration) })
	}
	for i := range req.Tools {
		t := &req.Tools[i]
		e.Message(10, func(sub *ProtoEncoder) { encodeChatToolDefinition(sub, t) })
	}
	e.Bool(11, req.DisableParallelToolCalls)
	if req.ToolChoice != nil {
		e.Message(12, func(sub *ProtoEncoder) { encodeChatToolChoice(sub, req.ToolChoice) })
	}
	// field 13: promptCacheOptions { 1: EPHEMERAL(1) }
	e.Message(13, func(sub *ProtoEncoder) {
		sub.Uint32(1, CacheControlTypeEphemeral)
	})
	e.String(16, req.CascadeID)
	e.Uint32(20, ConversationalPlannerModeDefault)
	e.String(22, req.ExecutionID)
	return e.Bytes()
}

// ─── GetChatMessageResponse ──────────────────────────────────────────────────

// ModelUsageStats 是响应中的用量统计。
type ModelUsageStats struct {
	InputTokens      int64  // field 2
	OutputTokens     int64  // field 3
	CacheWriteTokens int64  // field 4
	CacheReadTokens  int64  // field 5
	MessageID        string // field 7
	ModelUID         string // field 9
}

func decodeModelUsageStats(d *ProtoDecoder) *ModelUsageStats {
	s := &ModelUsageStats{}
	for !d.Done() {
		field, wire := d.ReadTag()
		switch field {
		case 2:
			s.InputTokens = int64(d.ReadUint64())
		case 3:
			s.OutputTokens = int64(d.ReadUint64())
		case 4:
			s.CacheWriteTokens = int64(d.ReadUint64())
		case 5:
			s.CacheReadTokens = int64(d.ReadUint64())
		case 7:
			s.MessageID = d.ReadString()
		case 9:
			s.ModelUID = d.ReadString()
		default:
			d.Skip(wire)
		}
	}
	return s
}

// GetChatMessageResponse 是 Devin GetChatMessage 的单个流式响应帧解码结果。
type GetChatMessageResponse struct {
	MessageID          string           // field 1
	DeltaText          string           // field 3
	StopReason         int32            // field 5 (varint)
	DeltaToolCalls     []ChatToolCall   // field 6 (repeated message)
	Usage              *ModelUsageStats // field 7 (message)
	Redact             bool             // field 8
	DeltaThinking      string           // field 9
	DeltaSignature     string           // field 10
	ThinkingRedacted   bool             // field 11
	CreditCost         int64            // field 14 (varint)
	OutputID           string           // field 15
	RequestID          string           // field 17
	DeltaSignatureType string           // field 21
	ActualModelUID     string           // field 23
}

func decodeGetChatMessageResponse(data []byte) (*GetChatMessageResponse, error) {
	d := NewProtoDecoder(data)
	res := &GetChatMessageResponse{}
	for !d.Done() {
		field, wire := d.ReadTag()
		switch field {
		case 1:
			res.MessageID = d.ReadString()
		case 3:
			res.DeltaText = d.ReadString()
		case 5:
			res.StopReason = int32(d.ReadUint64())
		case 6:
			if wire == 2 {
				res.DeltaToolCalls = append(res.DeltaToolCalls,
					decodeChatToolCall(NewProtoDecoder(d.ReadBytes())))
			} else {
				d.Skip(wire)
			}
		case 7:
			if wire == 2 {
				res.Usage = decodeModelUsageStats(NewProtoDecoder(d.ReadBytes()))
			} else {
				d.Skip(wire)
			}
		case 8:
			res.Redact = d.ReadBool()
		case 9:
			res.DeltaThinking = d.ReadString()
		case 10:
			res.DeltaSignature = d.ReadString()
		case 11:
			res.ThinkingRedacted = d.ReadBool()
		case 14:
			res.CreditCost = int64(d.ReadUint64())
		case 15:
			res.OutputID = d.ReadString()
		case 17:
			res.RequestID = d.ReadString()
		case 21:
			res.DeltaSignatureType = d.ReadString()
		case 23:
			res.ActualModelUID = d.ReadString()
		default:
			d.Skip(wire)
		}
	}
	return res, nil
}
