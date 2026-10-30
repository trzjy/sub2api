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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

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
