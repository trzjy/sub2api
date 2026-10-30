//go:build unit

package service

// IMG-01 卡：Cursor 协议图片输入单测。
//
// 覆盖四块改动：
//   - 当前轮图片（data URI）→ UserMessage.f3 selected_context（改动 C）；
//   - 历史轮图片 → history blob 的 content 数组（改动 D）；
//   - 纯文本路径出站字节不变（不含 f3、history user blob 仍是字符串）；
//   - 远程 URL / file part 仍失败关闭。

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// cursorUserMessageFieldsOf 拆出 AgentRun → action → user_message_action →
// user_message 的字段列表（便于断言 f3 selected_context 等）。
func cursorUserMessageFieldsOf(t *testing.T, run []cursorPBField) []cursorPBField {
	t.Helper()
	action, ok := cursorFindField(run, cursorFldRunAction)
	require.True(t, ok, "run_request must carry field 2 (action)")
	actionFields, err := cursorDecodeProtobuf(action.Bytes)
	require.NoError(t, err)
	uma, ok := cursorFindField(actionFields, cursorFldActionUserMessage)
	require.True(t, ok, "action must carry field 1 (user_message_action)")
	umaFields, err := cursorDecodeProtobuf(uma.Bytes)
	require.NoError(t, err)
	msg, ok := cursorFindField(umaFields, cursorFldUserMsgActionMessage)
	require.True(t, ok, "user_message_action must carry field 1 (user_message)")
	msgFields, err := cursorDecodeProtobuf(msg.Bytes)
	require.NoError(t, err)
	return msgFields
}

// 1×1 透明 PNG 的 base64（已知最小合法 PNG）。
const cursorTestPNGDataURI = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M8AAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

// TestCursorImageDataURIEncodedIntoSelectedImages 覆盖改动 C：当前轮带图请求，
// 图片以原生字节写进 UserMessage.f3 selected_context。
func TestCursorImageDataURIEncodedIntoSelectedImages(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(cursorTestPNGDataURI)
	require.NoError(t, err)

	content := json.RawMessage(fmt.Sprintf(
		`[{"type":"text","text":"what is in this image?"},`+
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]`,
		cursorTestPNGDataURI))
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: content}},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	msgFields := cursorUserMessageFieldsOf(t, run)

	// f1 text 仍正确（图片不得覆盖文本落点）。
	text, ok := cursorFindField(msgFields, cursorFldUserMsgText)
	require.True(t, ok)
	require.Equal(t, "what is in this image?", text.String())

	// f3 selected_context 必须出现（带图时）。
	selCtxField, ok := cursorFindField(msgFields, cursorFldUserMsgSelectedContext)
	require.True(t, ok, "selected_context (f3) must be present when current turn has images")
	selCtxFields, err := cursorDecodeProtobuf(selCtxField.Bytes)
	require.NoError(t, err)

	// SelectedContext.f1 = repeated SelectedImage（本请求恰 1 张）。
	selImages := cursorAllFields(selCtxFields, cursorFldSelectedContextImages)
	require.Len(t, selImages, 1)

	imgFields, err := cursorDecodeProtobuf(selImages[0].Bytes)
	require.NoError(t, err)

	// SelectedImage.f8 = data（原始图片字节，逐字节相等）。
	dataField, ok := cursorFindField(imgFields, cursorFldSelectedImageData)
	require.True(t, ok)
	require.Equal(t, pngBytes, dataField.Bytes, "SelectedImage.f8 must equal the original image bytes")

	// SelectedImage.f7 = mime_type。
	mimeField, ok := cursorFindField(imgFields, cursorFldSelectedImageMimeType)
	require.True(t, ok)
	require.Equal(t, "image/png", mimeField.String())
}

// TestCursorImageHistoryBlobCarriesImagePart 覆盖改动 D：历史轮带图，blob content
// 变成数组且含 image 项（data + mime_type）。
func TestCursorImageHistoryBlobCarriesImagePart(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(cursorTestPNGDataURI)
	require.NoError(t, err)

	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(fmt.Sprintf(
				`[{"type":"text","text":"here is a screenshot"},`+
					`{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]`,
				cursorTestPNGDataURI))},
			{Role: "user", Content: json.RawMessage(`"what is shown?"`)},
		},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	blobs := cursorBlobsOf(t, run)
	// 第一轮进历史（1 条 blob），第二轮是当前轮 action。
	require.Len(t, blobs, 1)

	var blob struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	require.NoError(t, json.Unmarshal(blobs[0][1], &blob))
	require.Equal(t, "user", blob.Role)
	require.True(t, len(blob.Content) > 0 && blob.Content[0] == '[',
		"history user blob content must be an array when the turn carries an image")

	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	require.NoError(t, json.Unmarshal(blob.Content, &parts))
	require.Len(t, parts, 2, "text part then image part, in order")
	require.Equal(t, "text", parts[0].Type)
	require.Equal(t, "here is a screenshot", parts[0].Text)
	require.Equal(t, "image", parts[1].Type)
	require.Equal(t, "image/png", parts[1].MimeType)
	decoded, err := base64.StdEncoding.DecodeString(parts[1].Data)
	require.NoError(t, err)
	require.Equal(t, pngBytes, decoded, "history blob image data must equal the original image bytes")
}

// TestCursorAssistantImageHistoryFailsClosed 覆盖缺陷 2：assistant 历史轮带图时
// 必须失败关闭（assistant 侧无 image 落点，不得静默丢图）。
func TestCursorAssistantImageHistoryFailsClosed(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
			{Role: "assistant", Content: json.RawMessage(fmt.Sprintf(
				`[{"type":"text","text":"see"},`+
					`{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]`,
				cursorTestPNGDataURI))},
			{Role: "user", Content: json.RawMessage(`"describe it"`)},
		},
	}
	_, err := cursorConvertRequest(req, "grok-4.7")
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
	require.Contains(t, err.Error(), "assistant")
}

// TestCursorPlainTextBytesUnchanged 覆盖纯文本路径字节稳定：不含 f3，且 history
// user blob 的 content 仍是 JSON 字符串（不得变成数组）。
func TestCursorPlainTextBytesUnchanged(t *testing.T) {
	cursorWithFixedUUID(t, "conv-fixed-pt", "msg-fixed-pt")
	req := &apicompat.ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"what is 2+2?"`)},
			{Role: "assistant", Content: json.RawMessage(`"4"`)},
			{Role: "user", Content: json.RawMessage(`"and 3+3?"`)},
		},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)

	// 当前轮 user_message 不得写 f3 selected_context（纯文本路径字节不变）。
	msgFields := cursorUserMessageFieldsOf(t, run)
	_, ok := cursorFindField(msgFields, cursorFldUserMsgSelectedContext)
	require.False(t, ok, "pure-text request must not write UserMessage.f3 selected_context")

	// 历史 user blob 的 content 仍是 JSON 字符串（不得变成数组）。
	blobs := cursorBlobsOf(t, run)
	for _, b := range blobs {
		var blob struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		require.NoError(t, json.Unmarshal(b[1], &blob))
		if blob.Role == "user" {
			require.True(t, len(blob.Content) > 0 && blob.Content[0] == '"',
				"history user blob content must remain a JSON string for pure text")
		}
	}
}

// TestCursorRemoteImageURLFailsClosed 覆盖改动 B：远程图片 URL 失败关闭（不抓取）。
func TestCursorRemoteImageURLFailsClosed(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(
			`[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]`)}},
	}
	_, err := cursorConvertRequest(req, "grok-4.7")
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
}

// TestCursorFilePartStillFailsClosed 覆盖改动 B：file part 仍失败关闭（不扩 file）。
func TestCursorFilePartStillFailsClosed(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(
			`[{"type":"file","file":{"file_data":"data:application/pdf;base64,abc"}}]`)}},
	}
	_, err := cursorConvertRequest(req, "grok-4.7")
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
}

// ---------------------------------------------------------------------------
// IMG-03：外审 must_fix + 健壮性/归属修正的回归用例
// ---------------------------------------------------------------------------

// TestCursorBlankTextPartWithoutImageFailsClosed 锁死必修项：无图时空白 text part
// 必须失败关闭（不能静默跳过改成"Helloworld"），纯文本路径行为回到基线。
func TestCursorBlankTextPartWithoutImageFailsClosed(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(
			`[{"type":"text","text":"a"},{"type":"text","text":" "},{"type":"text","text":"b"}]`)}},
	}
	_, err := cursorConvertRequest(req, "grok-4.7")
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
}

// TestCursorBlankTextPartWithImageAllowed 含图时空白 text part 静默跳过，其余拼接。
func TestCursorBlankTextPartWithImageAllowed(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(cursorTestPNGDataURI)
	require.NoError(t, err)
	content := json.RawMessage(fmt.Sprintf(
		`[{"type":"text","text":"a"},{"type":"text","text":" "},{"type":"text","text":"b"},`+
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]`, cursorTestPNGDataURI))
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: content}},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	msgFields := cursorUserMessageFieldsOf(t, run)

	text, ok := cursorFindField(msgFields, cursorFldUserMsgText)
	require.True(t, ok)
	require.Equal(t, "ab", text.String(), "blank text part must be skipped, others concatenated")

	selCtxField, ok := cursorFindField(msgFields, cursorFldUserMsgSelectedContext)
	require.True(t, ok, "selected_context (f3) must be present when current turn has images")
	selCtxFields, err := cursorDecodeProtobuf(selCtxField.Bytes)
	require.NoError(t, err)
	selImages := cursorAllFields(selCtxFields, cursorFldSelectedContextImages)
	require.Len(t, selImages, 1)
	imgFields, err := cursorDecodeProtobuf(selImages[0].Bytes)
	require.NoError(t, err)
	dataField, ok := cursorFindField(imgFields, cursorFldSelectedImageData)
	require.True(t, ok)
	require.Equal(t, pngBytes, dataField.Bytes, "SelectedImage.f8 must equal the original image bytes")
	mimeField, ok := cursorFindField(imgFields, cursorFldSelectedImageMimeType)
	require.True(t, ok)
	require.Equal(t, "image/png", mimeField.String())
}

// TestCursorNonImageMimeFailsClosed 非 image/ 的 mime（如 data:text/html）必须失败关闭，
// 防 html 等被透传进 selected_context。
func TestCursorNonImageMimeFailsClosed(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("<html>hi</html>"))
	content := json.RawMessage(fmt.Sprintf(
		`[{"type":"image_url","image_url":{"url":"data:text/html;base64,%s"}}]`, payload))
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: content}},
	}
	_, err := cursorConvertRequest(req, "grok-4.7")
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
}

// TestCursorOversizedImageFailsClosed 解码后字节数超过既有请求体上限必须失败关闭。
func TestCursorOversizedImageFailsClosed(t *testing.T) {
	// 构造一个超过 cursorUpstreamStreamMaxBytes 的 payload（既有请求体上限常量）。
	big := make([]byte, cursorUpstreamStreamMaxBytes+1)
	for i := range big {
		big[i] = 0xAB
	}
	payload := base64.StdEncoding.EncodeToString(big)
	content := json.RawMessage(fmt.Sprintf(
		`[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]`, payload))
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: content}},
	}
	_, err := cursorConvertRequest(req, "grok-4.7")
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
	require.Contains(t, err.Error(), "exceeds request body size limit")
}

// TestCursorBase64WithLineBreaksDecodes payload 含换行（76 列包装）时必须能解出。
func TestCursorBase64WithLineBreaksDecodes(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(cursorTestPNGDataURI)
	require.NoError(t, err)
	// 每 76 列加一个换行（标准 PEM 包装）。
	raw := base64.StdEncoding.EncodeToString(pngBytes)
	var wrapped strings.Builder
	for i := 0; i < len(raw); i += 76 {
		end := i + 76
		if end > len(raw) {
			end = len(raw)
		}
		wrapped.WriteString(raw[i:end])
		wrapped.WriteByte('\n')
	}
	payload := wrapped.String()
	require.Contains(t, payload, "\n", "test setup must include line breaks")
	// JSON 字符串里换行必须转义；解析后 url 内含真实换行，由 cursorDecodeImageURL 的解码前剔除逻辑处理。
	jsonPayload := strings.ReplaceAll(payload, "\n", "\\n")
	content := json.RawMessage(fmt.Sprintf(
		`[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]`, jsonPayload))
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: content}},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	msgFields := cursorUserMessageFieldsOf(t, run)
	selCtxField, ok := cursorFindField(msgFields, cursorFldUserMsgSelectedContext)
	require.True(t, ok)
	selCtxFields, err := cursorDecodeProtobuf(selCtxField.Bytes)
	require.NoError(t, err)
	selImages := cursorAllFields(selCtxFields, cursorFldSelectedContextImages)
	require.Len(t, selImages, 1)
	imgFields, err := cursorDecodeProtobuf(selImages[0].Bytes)
	require.NoError(t, err)
	dataField, ok := cursorFindField(imgFields, cursorFldSelectedImageData)
	require.True(t, ok)
	require.Equal(t, pngBytes, dataField.Bytes)
}

// TestCursorBase64URLSafeDecodes URL-safe 字母表（-/_）必须能解出。
func TestCursorBase64URLSafeDecodes(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(cursorTestPNGDataURI)
	require.NoError(t, err)
	payload := base64.URLEncoding.EncodeToString(pngBytes)
	require.NotContains(t, payload, "+")
	require.NotContains(t, payload, "/")
	content := json.RawMessage(fmt.Sprintf(
		`[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]`, payload))
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: content}},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	msgFields := cursorUserMessageFieldsOf(t, run)
	selCtxField, ok := cursorFindField(msgFields, cursorFldUserMsgSelectedContext)
	require.True(t, ok)
	selCtxFields, err := cursorDecodeProtobuf(selCtxField.Bytes)
	require.NoError(t, err)
	selImages := cursorAllFields(selCtxFields, cursorFldSelectedContextImages)
	require.Len(t, selImages, 1)
	imgFields, err := cursorDecodeProtobuf(selImages[0].Bytes)
	require.NoError(t, err)
	dataField, ok := cursorFindField(imgFields, cursorFldSelectedImageData)
	require.True(t, ok)
	require.Equal(t, pngBytes, dataField.Bytes)
}

// TestCursorDataURIWithCharsetParamDecodes 带 charset 参数的 data URI
// （data:image/svg+xml;charset=utf-8;base64,…）必须能过。
func TestCursorDataURIWithCharsetParamDecodes(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(cursorTestPNGDataURI)
	require.NoError(t, err)
	content := json.RawMessage(fmt.Sprintf(
		`[{"type":"image_url","image_url":{"url":"data:image/svg+xml;charset=utf-8;base64,%s"}}]`, cursorTestPNGDataURI))
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{{Role: "user", Content: content}},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)

	run := cursorAgentRunOf(t, body)
	msgFields := cursorUserMessageFieldsOf(t, run)
	selCtxField, ok := cursorFindField(msgFields, cursorFldUserMsgSelectedContext)
	require.True(t, ok)
	selCtxFields, err := cursorDecodeProtobuf(selCtxField.Bytes)
	require.NoError(t, err)
	selImages := cursorAllFields(selCtxFields, cursorFldSelectedContextImages)
	require.Len(t, selImages, 1)
	imgFields, err := cursorDecodeProtobuf(selImages[0].Bytes)
	require.NoError(t, err)
	dataField, ok := cursorFindField(imgFields, cursorFldSelectedImageData)
	require.True(t, ok)
	require.Equal(t, pngBytes, dataField.Bytes)
	mimeField, ok := cursorFindField(imgFields, cursorFldSelectedImageMimeType)
	require.True(t, ok)
	require.Equal(t, "image/svg+xml", mimeField.String())
}

// TestCursorToolMessageImageFailsClosed tool/function 轮若含图片必须失败关闭：
// 协议里 tool 结果无图片落点，避免把工具产物当成用户上传的图写进 selected_context。
func TestCursorToolMessageImageFailsClosed(t *testing.T) {
	content := json.RawMessage(fmt.Sprintf(
		`[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]`, cursorTestPNGDataURI))
	req := &apicompat.ChatCompletionsRequest{
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "tool", ToolCallID: "call_1", Name: "get_weather", Content: content},
			{Role: "user", Content: json.RawMessage(`"next"`)},
		},
	}
	_, err := cursorConvertRequest(req, "grok-4.7")
	require.ErrorIs(t, err, ErrCursorUnsupportedRequest)
	require.Contains(t, err.Error(), "tool")
}

// ---------------------------------------------------------------------------
// IMG-04：修 cursorSafeURLForLog 越界 panic 的回归用例
// ---------------------------------------------------------------------------

// TestCursorSafeURLForLogNoPanic 锁死 IMG-04 must_fix：畸形/任意 URL 都不得 panic，
// 且输出符合预期（不含凭据、不含 query、合法 UTF-8、≤120）。
func TestCursorSafeURLForLogNoPanic(t *testing.T) {
	// 构造一段 ≥200 字符（rune）的超长 URL，用于验证截断与多字节边界。
	longURL := "https://user:pass@例え.jp/" + strings.Repeat("路径/", 200)
	require.GreaterOrEqual(t, len([]rune(longURL)), 200, "longURL must be at least 200 runes")

	tests := []struct {
		name       string
		in         string
		want       *string  // 期望精确输出；用 strPtr("") 表示期望空串；省略（nil）表示仅校验约束不比对精确值
		noContains []string // 输出不得包含的子串
		check      func(t *testing.T, out string)
	}{
		{
			name: "query before scheme",
			in:   "a?b://c",
			// 去掉 query 后只剩 "a"，不再有 "://"，原样返回 "a"。
			want: strPtr("a"),
		},
		{
			name: "only scheme marker",
			in:   "?://",
			// query 在 0，整串被清空。
			want: strPtr(""),
		},
		{
			name:       "userinfo and query stripped",
			in:         "https://user:pass@host/x?a=1",
			want:       strPtr("host/x"),
			noContains: []string{"user:pass", "a=1"},
		},
		{
			name: "multibyte host stays valid utf8",
			in:   "https://例え.jp/路径?a=1",
			want: strPtr("例え.jp/路径"),
			check: func(t *testing.T, out string) {
				require.True(t, utf8.ValidString(out), "output must be valid UTF-8, got %q", out)
			},
		},
		{
			name: "empty input",
			in:   "",
			want: strPtr(""),
		},
		{
			name: "long input truncated and valid",
			in:   longURL,
			check: func(t *testing.T, out string) {
				require.LessOrEqual(t, len(out), 120, "length must be ≤120, got %d", len(out))
				require.True(t, utf8.ValidString(out), "truncated output must be valid UTF-8, got %q", out)
				require.NotContains(t, out, "user:pass", "credentials must not leak into log")
			},
		},
		{
			name: "not a url returned as-is",
			in:   "not-a-url",
			want: strPtr("not-a-url"),
		},
		{
			name:       "multiple @ picks last",
			in:         "https://user:p@ss@host/x",
			want:       strPtr("host/x"),
			noContains: []string{"user", "ss@"},
		},
		{
			name: "at in path preserved",
			in:   "https://host/a@b",
			want: strPtr("host/a@b"),
		},
		{
			// 缺陷 2：协议相对的 userinfo（无 scheme）必须被剥离。
			name: "protocol relative with userinfo",
			in:   "//user:pass@host/x",
			want: strPtr("host/x"),
		},
		{
			// 缺陷 2：裸 userinfo（无任何 scheme）必须被剥离。
			name: "bare userinfo",
			in:   "user:pass@host",
			want: strPtr("host"),
		},
		{
			// 缺陷 1：长度 ≤120 但含非法字节的输入，输出必须是合法 UTF-8。
			name: "short invalid utf8 made valid",
			in:   "host/\xff\xfe",
			check: func(t *testing.T, out string) {
				require.True(t, utf8.ValidString(out),
					"output must be valid UTF-8 even for short inputs with invalid bytes, got %q", out)
				require.Equal(t, "host/", out, "invalid bytes must be dropped, leaving only the valid prefix")
			},
		},
		{
			// path 中的 '@' 必须保留（已有断言的二次确认，不放开既有行为）。
			name: "path at preserved",
			in:   "https://host/a@b",
			want: strPtr("host/a@b"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out string
			require.NotPanics(t, func() {
				out = cursorSafeURLForLog(tc.in)
			}, "cursorSafeURLForLog must never panic")
			if tc.want != nil {
				require.Equal(t, *tc.want, out)
			}
			for _, s := range tc.noContains {
				require.NotContains(t, out, s)
			}
			if tc.check != nil {
				tc.check(t, out)
			}
		})
	}
}

// goldenCursorPlainTextAgentRunHex 是纯文本请求出站字节的冻结黄金值（十六进制）。
//
// 固定 UUID 来源（conv-fixed-pt / msg-fixed-pt）下，纯文本请求的整段 AgentRun
// 出站字节必须与此逐字节相等。纯文本路径本轮任何改动都不得让它变化
// （锁死「纯文本逐字节不变」）。由 TestCursorDumpGoldenTmp 在改动前生成。
const goldenCursorPlainTextAgentRunHex = "00000001420abf020a4650010a20af4a000f139976ef7fd08bf5f05d5477c3643ab14225814cc70020ff262989170a2041b0ba5a1d50f98ef26cc1b234744e9566c9a9d85d4e32387dfdfec86f7f1bdc121e0a1c0a1a0a08616e6420332b333f120c6d73672d66697865642d707420011a0a0a0867726f6b2d342e3722002a0d636f6e762d66697865642d70748a014c0a20af4a000f139976ef7fd08bf5f05d5477c3643ab14225814cc70020ff2629891712287b22726f6c65223a2275736572222c22636f6e74656e74223a227768617420697320322b323f227d8a01680a2041b0ba5a1d50f98ef26cc1b234744e9566c9a9d85d4e32387dfdfec86f7f1bdc12447b22726f6c65223a22617373697374616e74222c22636f6e74656e74223a5b7b2274797065223a2274657874222c2274657874223a2234227d5d2c226964223a2231227d"

// TestCursorPlainTextAgentRunGoldenBytes 黄金字节回归：纯文本请求的整段出站字节
// 与冻结 golden 逐字节相等。
func TestCursorPlainTextAgentRunGoldenBytes(t *testing.T) {
	golden, err := hex.DecodeString(goldenCursorPlainTextAgentRunHex)
	require.NoError(t, err)
	cursorWithFixedUUID(t, "conv-fixed-pt", "msg-fixed-pt")
	req := &apicompat.ChatCompletionsRequest{
		Model: "gpt-4o",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"what is 2+2?"`)},
			{Role: "assistant", Content: json.RawMessage(`"4"`)},
			{Role: "user", Content: json.RawMessage(`"and 3+3?"`)},
		},
	}
	body, err := cursorConvertRequest(req, "grok-4.7")
	require.NoError(t, err)
	require.True(t, bytes.Equal(golden, body),
		"pure-text AgentRun bytes must be byte-identical to the frozen golden; got %x", body)
}
