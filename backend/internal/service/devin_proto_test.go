//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// scannedField 是测试辅助：记录 protobuf 字段号、线类型与原始值。
type scannedField struct {
	num    int
	wire   int
	varint uint64
	data   []byte
	dbl    float64
}

// scanFields 用生产解码器扫描原始字节，供字段 tag 断言使用。
func scanFields(t *testing.T, data []byte) []scannedField {
	t.Helper()
	d := NewProtoDecoder(data)
	var out []scannedField
	for !d.Done() {
		num, wire := d.ReadTag()
		f := scannedField{num: num, wire: wire}
		switch wire {
		case 0:
			f.varint = d.ReadUint64()
		case 1:
			f.dbl = d.ReadDouble()
		case 2:
			f.data = append([]byte(nil), d.ReadBytes()...)
		default:
			d.Skip(wire)
		}
		out = append(out, f)
	}
	return out
}

func fieldNumbers(fields []scannedField) []int {
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.num)
	}
	return out
}

func fieldsOfNum(fields []scannedField, num int) []scannedField {
	var out []scannedField
	for _, f := range fields {
		if f.num == num {
			out = append(out, f)
		}
	}
	return out
}

// TestDevinProtoMetadataTags 断言 Metadata 各字段 tag 严格为 1/7/12/2/3/4/21。
func TestDevinProtoMetadataTags(t *testing.T) {
	meta := &Metadata{
		IDEName:          "chisel",
		IDEVersion:       "3.2.23",
		ExtensionName:    "chisel",
		ExtensionVersion: "1.48.2",
		APIKey:           "devin-session-token$abc",
		Locale:           "en",
		UserJWT:          "jwt-value",
	}
	outer := scanFields(t, encodeGetUserJwtRequest(meta))
	require.Len(t, outer, 1, "GetUserJwtRequest 应只有一个字段 1")
	require.Equal(t, 1, outer[0].num)
	require.Equal(t, 2, outer[0].wire)

	inner := scanFields(t, outer[0].data)
	require.Equal(t, []int{1, 7, 12, 2, 3, 4, 21}, fieldNumbers(inner),
		"Metadata 字段编号必须与权威表一致")
	for _, f := range inner {
		require.Equal(t, 2, f.wire, "Metadata 各字段均应为 length-delimited")
	}
	require.Equal(t, "chisel", string(inner[0].data))                                  // ideName
	require.Equal(t, "3.2.23", string(fieldsOfNum(inner, 7)[0].data))                  // ideVersion
	require.Equal(t, "1.48.2", string(fieldsOfNum(inner, 2)[0].data))                  // extensionVersion
	require.Equal(t, "devin-session-token$abc", string(fieldsOfNum(inner, 3)[0].data)) // apiKey
	require.Equal(t, "jwt-value", string(fieldsOfNum(inner, 21)[0].data))              // userJwt
}

// TestDevinProtoGetUserJwtResponse 断言响应 1/2 解码，且未知字段不中断。
func TestDevinProtoGetUserJwtResponse(t *testing.T) {
	e := &ProtoEncoder{}
	e.String(1, "jwt-1")
	e.String(2, "https://custom.example")
	e.Uint64(99, 1234) // 未知字段，必须被 Skip 且不中断
	e.String(100, "ignored")

	got, err := decodeGetUserJwtResponse(e.Bytes())
	require.NoError(t, err)
	require.Equal(t, "jwt-1", got.UserJWT)
	require.Equal(t, "https://custom.example", got.CustomAPIServerURL)
}

// TestDevinProtoGetChatMessageRequestFields 断言 request 的 7/13/16/20/21/22 字段。
func TestDevinProtoGetChatMessageRequestFields(t *testing.T) {
	req := &GetChatMessageRequest{
		Metadata:           &Metadata{IDEName: "chisel", APIKey: "tok", UserJWT: "jwt"},
		Prompt:             "system-prompt",
		ChatMessagePrompts: []ChatMessagePrompt{{MessageID: "m1", Source: ChatMessageSourceUser, Prompt: "hi"}},
		ChatModelUID:       "model-x",
		Configuration: &CompletionConfiguration{
			NumCompletions: 1, MaxTokens: 64000, MaxNewlines: 200,
			Temperature: 0.4, FirstTemperature: 0.4, TopK: 50, TopP: 1,
			StopPatterns: []string{"\n\nUSER:"}, FimEotProbThreshold: 1,
		},
		Tools:                    []ChatToolDefinition{{Name: "tool-a", JSONSchemaString: "{}"}},
		DisableParallelToolCalls: true,
		ToolChoice:               &ChatToolChoice{OptionName: "auto"},
		CascadeID:                "cascade-1",
		ExecutionID:              "exec-1",
	}
	top := scanFields(t, encodeGetChatMessageRequest(req))
	nums := fieldNumbers(top)
	for _, want := range []int{1, 2, 3, 7, 8, 10, 11, 12, 13, 16, 20, 21, 22} {
		require.Contains(t, nums, want, "request 缺少字段 %d", want)
	}

	require.Equal(t, uint64(5), fieldsOfNum(top, 7)[0].varint, "request type 7 必须为 5")
	require.Equal(t, uint64(1), fieldsOfNum(top, 20)[0].varint, "planner mode 20 必须为 1")
	require.Equal(t, "model-x", string(fieldsOfNum(top, 21)[0].data))
	require.Equal(t, "cascade-1", string(fieldsOfNum(top, 16)[0].data))
	require.Equal(t, "exec-1", string(fieldsOfNum(top, 22)[0].data))

	cacheOpts := scanFields(t, fieldsOfNum(top, 13)[0].data)
	require.Equal(t, []int{1}, fieldNumbers(cacheOpts), "promptCacheOptions 仅含字段 1")
	require.Equal(t, uint64(1), cacheOpts[0].varint, "promptCacheOptions.type 必须为 1")

	config := scanFields(t, fieldsOfNum(top, 8)[0].data)
	require.Equal(t, []int{1, 2, 3, 5, 6, 7, 8, 9, 11}, fieldNumbers(config),
		"CompletionConfiguration 字段编号必须为 1/2/3/5/6/7/8/9/11")
	require.InDelta(t, 0.4, fieldsOfNum(config, 5)[0].dbl, 1e-9)
	require.InDelta(t, 1.0, fieldsOfNum(config, 8)[0].dbl, 1e-9)
	require.InDelta(t, 1.0, fieldsOfNum(config, 11)[0].dbl, 1e-9)
}

// TestDevinProtoGetChatMessageResponseDecode 断言 3/5/6/7/9 解码，未知字段不中断。
func TestDevinProtoGetChatMessageResponseDecode(t *testing.T) {
	e2 := &ProtoEncoder{}
	e2.String(1, "msg-1")
	e2.String(3, "hello delta")
	e2.Uint64(5, uint64(StopReasonFunctionCall))
	e2.Message(6, func(sub *ProtoEncoder) {
		sub.String(1, "call-1")
		sub.String(2, "echo")
		sub.String(3, `{"x":1}`)
		sub.Bool(6, true)
		sub.Uint64(77, 999)
	})
	e2.Message(7, func(sub *ProtoEncoder) {
		sub.Uint64(2, 100)
		sub.Uint64(3, 200)
		sub.Uint64(4, 30)
		sub.Uint64(5, 40)
		sub.String(7, "msg-7")
		sub.String(9, "model-9")
		sub.String(99, "unknown")
	})
	e2.Bool(8, true)
	e2.String(9, "thinking delta")
	e2.String(10, "sig")
	e2.Bool(11, true)
	e2.Uint64(14, 7)
	e2.String(15, "out-1")
	e2.String(17, "req-1")
	e2.String(21, "sig-type")
	e2.String(23, "actual-model")
	e2.Uint64(500, 1) // 顶层未知字段

	got, err := decodeGetChatMessageResponse(e2.Bytes())
	require.NoError(t, err)
	require.Equal(t, "msg-1", got.MessageID)
	require.Equal(t, "hello delta", got.DeltaText)
	require.Equal(t, int32(10), got.StopReason)
	require.True(t, got.Redact)
	require.Equal(t, "thinking delta", got.DeltaThinking)
	require.Equal(t, "sig", got.DeltaSignature)
	require.True(t, got.ThinkingRedacted)
	require.Equal(t, int64(7), got.CreditCost)
	require.Equal(t, "out-1", got.OutputID)
	require.Equal(t, "req-1", got.RequestID)
	require.Equal(t, "sig-type", got.DeltaSignatureType)
	require.Equal(t, "actual-model", got.ActualModelUID)

	require.Len(t, got.DeltaToolCalls, 1)
	require.Equal(t, "call-1", got.DeltaToolCalls[0].ID)
	require.Equal(t, "echo", got.DeltaToolCalls[0].Name)
	require.Equal(t, `{"x":1}`, got.DeltaToolCalls[0].ArgumentsJSON)
	require.True(t, got.DeltaToolCalls[0].IsCustomToolCall)
}

// TestDevinProtoUsageDecode 断言 ModelUsageStats 字段 2–5 解码（含 7/9）。
func TestDevinProtoUsageDecode(t *testing.T) {
	e := &ProtoEncoder{}
	e.Message(7, func(sub *ProtoEncoder) {
		// 直接内联，避免依赖未导出的原始字节写入。
		sub.Uint64(2, 11)
		sub.Uint64(3, 22)
		sub.Uint64(4, 33)
		sub.Uint64(5, 44)
		sub.String(7, "usage-msg")
		sub.String(9, "usage-model")
		sub.Uint64(66, 1)
	})

	got, err := decodeGetChatMessageResponse(e.Bytes())
	require.NoError(t, err)
	require.NotNil(t, got.Usage)
	require.Equal(t, int64(11), got.Usage.InputTokens)
	require.Equal(t, int64(22), got.Usage.OutputTokens)
	require.Equal(t, int64(33), got.Usage.CacheWriteTokens)
	require.Equal(t, int64(44), got.Usage.CacheReadTokens)
	require.Equal(t, "usage-msg", got.Usage.MessageID)
	require.Equal(t, "usage-model", got.Usage.ModelUID)
}
