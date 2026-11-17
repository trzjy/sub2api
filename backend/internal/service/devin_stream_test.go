//go:build unit

package service

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// chunkReader 每次 Read 至多返回 max 字节，用于强制切帧分片。
type chunkReader struct {
	data []byte
	max  int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.max
	if n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func makeDevinFrame(t *testing.T, flags byte, payload []byte) []byte {
	t.Helper()
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func gzipDevinBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(data)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

// TestDevinStreamReaderFragmentedFrames 两帧分多次小块写入仍能正确切帧。
func TestDevinStreamReaderFragmentedFrames(t *testing.T) {
	f1 := makeDevinFrame(t, 0, []byte("hello"))
	f2 := makeDevinFrame(t, 0, []byte("world"))
	all := append(append([]byte(nil), f1...), f2...)

	r := NewDevinStreamReader(&chunkReader{data: all, max: 3})
	payload1, end1, err := r.Next()
	require.NoError(t, err)
	require.False(t, end1)
	require.Equal(t, "hello", string(payload1))

	payload2, end2, err := r.Next()
	require.NoError(t, err)
	require.False(t, end2)
	require.Equal(t, "world", string(payload2))

	_, _, err = r.Next()
	require.ErrorIs(t, err, io.EOF)
}

// TestDevinStreamReaderGzipFrame 压缩帧解压后仍是可解码的 protobuf 响应。
func TestDevinStreamReaderGzipFrame(t *testing.T) {
	resp := &ProtoEncoder{}
	resp.String(3, "compressed delta")
	resp.Uint64(5, 3)
	raw := resp.Bytes()

	frame := makeDevinFrame(t, ConnectCompressedFlag, gzipDevinBytes(t, raw))
	r := NewDevinStreamReader(bytes.NewReader(frame))
	payload, end, err := r.Next()
	require.NoError(t, err)
	require.False(t, end)
	require.Equal(t, raw, payload)

	msg, err := decodeGetChatMessageResponse(payload)
	require.NoError(t, err)
	require.Equal(t, "compressed delta", msg.DeltaText)
	require.Equal(t, int32(3), msg.StopReason)
}

// TestDevinStreamReaderOversizedFrame 超长帧头必须直接报错。
func TestDevinStreamReaderOversizedFrame(t *testing.T) {
	header := make([]byte, 5)
	header[0] = 0
	binary.BigEndian.PutUint32(header[1:5], uint32(MaxFramePayload)+1)

	r := NewDevinStreamReader(bytes.NewReader(header))
	_, _, err := r.Next()
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds")
}

// TestDevinStreamReaderEndStream END_STREAM flag 识别（含压缩 trailer）。
func TestDevinStreamReaderEndStream(t *testing.T) {
	trailer := []byte(`{"error":{"code":"permission_denied","message":"denied"}}`)
	frame := makeDevinFrame(t, ConnectEndStreamFlag, trailer)
	r := NewDevinStreamReader(bytes.NewReader(frame))
	payload, end, err := r.Next()
	require.NoError(t, err)
	require.True(t, end)
	require.Equal(t, string(trailer), string(payload))

	compressed := makeDevinFrame(t, ConnectEndStreamFlag|ConnectCompressedFlag, gzipDevinBytes(t, trailer))
	r2 := NewDevinStreamReader(bytes.NewReader(compressed))
	payload2, end2, err := r2.Next()
	require.NoError(t, err)
	require.True(t, end2)
	require.Equal(t, string(trailer), string(payload2))
}

// TestDevinStreamReaderIncompleteFrame 残留半帧在 EOF 时报错而非静默成功。
func TestDevinStreamReaderIncompleteFrame(t *testing.T) {
	partial := []byte{0, 0, 0, 0, 10, 'a', 'b'}
	r := NewDevinStreamReader(bytes.NewReader(partial))
	_, _, err := r.Next()
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

// TestDevinStreamNewDevinChatFrame 帧头 flags=0x01，gunzip 后可解码且固定字段/默认值正确。
func TestDevinStreamNewDevinChatFrame(t *testing.T) {
	maxTokens := 100
	params := &DevinChatParams{
		ModelUID:     "model-z",
		SystemPrompt: "system-z",
		CascadeID:    "cascade-z",
		Prompts:      []ChatMessagePrompt{{MessageID: "m1", Source: ChatMessageSourceUser, Prompt: "hi"}},
		MaxTokens:    &maxTokens,
	}
	frame, baseURL, err := NewDevinChatFrame(params, "jwt-z", "https://custom.example/")
	require.NoError(t, err)
	require.Equal(t, "https://custom.example", baseURL)
	require.Greater(t, len(frame), 5)
	require.Equal(t, byte(ConnectCompressedFlag), frame[0])
	require.Equal(t, len(frame)-5, int(binary.BigEndian.Uint32(frame[1:5])))

	raw, err := tryGunzipResponse(frame[5:])
	require.NoError(t, err)
	top := scanFields(t, raw)
	require.Equal(t, uint64(5), fieldsOfNum(top, 7)[0].varint, "request type 7=5")
	require.Equal(t, "cascade-z", string(fieldsOfNum(top, 16)[0].data))
	require.Equal(t, "model-z", string(fieldsOfNum(top, 21)[0].data))
	require.Len(t, fieldsOfNum(top, 22), 1, "executionId 必须存在")
	require.NotEmpty(t, fieldsOfNum(top, 22)[0].data)

	cacheOpts := scanFields(t, fieldsOfNum(top, 13)[0].data)
	require.Equal(t, uint64(1), cacheOpts[0].varint)

	config := scanFields(t, fieldsOfNum(top, 8)[0].data)
	require.Equal(t, uint64(100), fieldsOfNum(config, 2)[0].varint)
	require.Equal(t, uint64(200), fieldsOfNum(config, 3)[0].varint)
	require.Equal(t, uint64(50), fieldsOfNum(config, 7)[0].varint)
	require.InDelta(t, 0.4, fieldsOfNum(config, 5)[0].dbl, 1e-9, "temperature 默认 0.4")
	require.InDelta(t, 1.0, fieldsOfNum(config, 8)[0].dbl, 1e-9, "topP 默认 1")
	require.InDelta(t, 1.0, fieldsOfNum(config, 11)[0].dbl, 1e-9, "fimEot 默认 1")
	require.Len(t, fieldsOfNum(config, 9), 4, "默认 4 条 stopPatterns")

	// temperature 显式 0 → 0.01
	zero := 0.0
	params.Temperature = &zero
	frame2, _, err := NewDevinChatFrame(params, "jwt-z", "")
	require.NoError(t, err)
	raw2, err := tryGunzipResponse(frame2[5:])
	require.NoError(t, err)
	config2 := scanFields(t, fieldsOfNum(scanFields(t, raw2), 8)[0].data)
	require.InDelta(t, 0.01, fieldsOfNum(config2, 5)[0].dbl, 1e-9, "temperature=0 应钳到 0.01")
}

// eofWithDataReader 模拟一次 Read 同时返回完整数据与 io.EOF 的上游。
type eofWithDataReader struct {
	data []byte
	done bool
}

func (r *eofWithDataReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.data)
	return n, io.EOF
}

// TestDevinStreamReaderFullFrameWithEOF 覆盖 (n>0, io.EOF)：一次读到完整帧+EOF 仍须成功切帧。
func TestDevinStreamReaderFullFrameWithEOF(t *testing.T) {
	frame := makeDevinFrame(t, ConnectCompressedFlag, gzipDevinBytes(t, []byte("complete-with-eof")))

	r := NewDevinStreamReader(&eofWithDataReader{data: frame})
	payload, end, err := r.Next()
	require.NoError(t, err)
	require.False(t, end)
	require.Equal(t, "complete-with-eof", string(payload))

	// 帧消费完毕后，下一次 Next 应正常返回 io.EOF。
	_, _, err = r.Next()
	require.ErrorIs(t, err, io.EOF)
}

// TestDevinStreamReaderEOFWithResidual 覆盖 (n>0, io.EOF) 后残留半帧仍按 UnexpectedEOF 处理。
func TestDevinStreamReaderEOFWithResidual(t *testing.T) {
	partial := []byte{0, 0, 0, 0, 10, 'a', 'b'}
	r := NewDevinStreamReader(&eofWithDataReader{data: partial})
	_, _, err := r.Next()
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

// TestDevinChatFrameEncodesImage 带 Images 的 params 成功编码进帧，
// 解帧后 prompts[0].images[0] 的 base64 与 MIME 正确。
func TestDevinChatFrameEncodesImage(t *testing.T) {
	params := &DevinChatParams{
		Prompts: []ChatMessagePrompt{{
			MessageID: "m1",
			Source:    ChatMessageSourceUser,
			Prompt:    "see image",
			Images:    []ImageData{{Base64Data: "aGk=", MimeType: "image/png"}},
		}},
	}
	frame, _, err := NewDevinChatFrame(params, "jwt", "")
	require.NoError(t, err)
	require.NotEmpty(t, frame)

	raw, err := tryGunzipResponse(frame[5:])
	require.NoError(t, err)
	top := scanFields(t, raw)
	prompts := fieldsOfNum(top, 3) // GetChatMessageRequest.ChatMessagePrompts = field 3
	require.Len(t, prompts, 1)
	promptFields := scanFields(t, prompts[0].data)
	images := fieldsOfNum(promptFields, 10) // ChatMessagePrompt.Images = field 10
	require.Len(t, images, 1)
	imgFields := scanFields(t, images[0].data)
	require.Equal(t, "aGk=", string(fieldsOfNum(imgFields, 1)[0].data))      // ImageData.Base64Data = field 1
	require.Equal(t, "image/png", string(fieldsOfNum(imgFields, 2)[0].data)) // ImageData.MimeType = field 2
}

// TestDevinChatFrameSystemImageRejected system/developer 消息含图片时显式拒绝，不静默丢弃。
func TestDevinChatFrameSystemImageRejected(t *testing.T) {
	for _, role := range []string{"system", "developer"} {
		body := []byte(`{"messages":[{"role":"` + role + `","content":[` +
			`{"type":"text","text":"sys"},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]}]}`)
		_, err := BuildDevinChatParams(body, "m", "c")
		require.ErrorIs(t, err, ErrDevinSystemImageUnsupported, "role=%s", role)
	}
}
