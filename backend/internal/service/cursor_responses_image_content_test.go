//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func cursorResponsesImageRequest(t *testing.T, content string) *apicompat.ResponsesRequest {
	t.Helper()
	return &apicompat.ResponsesRequest{
		Model: "gpt-6.1-sol",
		Input: json.RawMessage(`[
			{"type":"message","role":"user","content":"earlier question"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"earlier answer"}]},
			{"type":"message","role":"user","content":` + content + `}]`),
	}
}

func TestCursorResponsesImageContent(t *testing.T) {
	image := `{"type":"input_image","image_url":"data:image/png;base64,` + cursorTestPNGDataURI + `","detail":"high"}`
	cases := []struct {
		name, content, text string
		images              int
	}{
		{"text then image", `[{"type":"input_text","text":"还是不能正常使用啊？"},` + image + `]`, "还是不能正常使用啊？", 1},
		{"image then text", `[` + image + `,{"type":"input_text","text":"explain this"}]`, "explain this", 1},
		{"image only", `[` + image + `]`, "", 1},
		{"multiple images", `[` + image + `,{"type":"input_text","text":"compare"},` + image + `]`, "compare", 2},
		{"plain string unchanged", `"exact\n text"`, "exact\n text", 0},
		{"text parts unchanged", `[{"type":"input_text","text":"first\n"},{"type":"input_text","text":" second"}]`, "first\n second", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := cursorResponsesImageRequest(t, tc.content)
			sem, err := cursorDecodeResponsesSemantic(req)
			require.NoError(t, err)
			require.Len(t, sem, 3)
			require.Equal(t, cursorSemanticUser, sem[2].role)
			require.Equal(t, 2, sem[2].sourceContainerIndex)
			skipped := cursorSkipLedger{}
			body, _, err := cursorBuildAgentRunFromSemantic(sem, req.Model, nil, nil, skipped)
			require.NoError(t, err, "a real user tail must not become assistant prefill")
			require.Zero(t, skipped[cursorSkipEmptyMessage])
			msg := cursorUserMessageFieldsOf(t, cursorAgentRunOf(t, body))
			text, ok := cursorFindField(msg, cursorFldUserMsgText)
			if tc.text != "" {
				require.True(t, ok)
			}
			require.Equal(t, tc.text, text.String())
			selected, hasImages := cursorFindField(msg, cursorFldUserMsgSelectedContext)
			require.Equal(t, tc.images > 0, hasImages)
			if !hasImages {
				return
			}
			fields, err := cursorDecodeProtobuf(selected.Bytes)
			require.NoError(t, err)
			images := cursorAllFields(fields, cursorFldSelectedContextImages)
			require.Len(t, images, tc.images)
			want, err := base64.StdEncoding.DecodeString(cursorTestPNGDataURI)
			require.NoError(t, err)
			for _, image := range images {
				fields, err := cursorDecodeProtobuf(image.Bytes)
				require.NoError(t, err)
				data, ok := cursorFindField(fields, cursorFldSelectedImageData)
				require.True(t, ok)
				require.Equal(t, want, data.Bytes)
			}
		})
	}
}

func TestCursorResponsesImageContentHistoryDigest(t *testing.T) {
	item := func(text, image string) apicompat.ResponsesInputItem {
		return apicompat.ResponsesInputItem{Type: "message", Role: "user", Content: json.RawMessage(fmt.Sprintf(`[{"type":"input_text","text":%q},{"type":"input_image","image_url":%q}]`, text, image))}
	}
	base := item("question", "data:image/png;base64,"+cursorTestPNGDataURI)
	skipped := cursorSkipLedger{}
	a, err := cursorResponsesItemItem(base, skipped)
	require.NoError(t, err)
	require.Empty(t, skipped, "valid Responses image content is not malformed Chat content")
	b, err := cursorResponsesItemItem(item("changed", "data:image/png;base64,"+cursorTestPNGDataURI), nil)
	require.NoError(t, err)
	c, err := cursorResponsesItemItem(item("question", "data:image/png;base64,AQID"), nil)
	require.NoError(t, err)
	require.NotEqual(t, a.canonical, b.canonical, "text participates in history identity")
	require.NotEqual(t, a.canonical, c.canonical, "image participates in history identity")
}

func TestCursorResponsesImageContentHistoryBlob(t *testing.T) {
	req := cursorResponsesImageRequest(t, `"follow up"`)
	var items []apicompat.ResponsesInputItem
	require.NoError(t, json.Unmarshal(req.Input, &items))
	items[0].Content = json.RawMessage(`[{"type":"input_text","text":"old screenshot"},{"type":"input_image","image_url":"data:image/png;base64,` + cursorTestPNGDataURI + `"}]`)
	req.Input = cursorResponsesRJ(t, items)
	sem, err := cursorDecodeResponsesSemantic(req)
	require.NoError(t, err)
	body, _, err := cursorBuildAgentRunFromSemantic(sem, req.Model, nil, nil, nil)
	require.NoError(t, err)
	blobs := cursorBlobsOf(t, cursorAgentRunOf(t, body))
	require.Len(t, blobs, 2)
	var history cursorHistoryUserRichBlob
	require.NoError(t, json.Unmarshal(blobs[0][1], &history))
	require.Equal(t, "user", history.Role)
	require.Len(t, history.Content, 2)
	require.Equal(t, "old screenshot", history.Content[0].Text)
	require.Equal(t, cursorTestPNGDataURI, history.Content[1].Data)
}

func TestCursorResponsesImageContentExecuteEntry(t *testing.T) {
	wantImage, err := base64.StdEncoding.DecodeString(cursorTestPNGDataURI)
	require.NoError(t, err)
	terminal := cursorOKStreamBody(t)
	verified := make(chan bool, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := cursorDuplexReadFrame(r.Body)
		ok := err == nil && cursorResponsesImageContentVerifyWire(payload, "inspect screenshot", wantImage)
		verified <- ok
		if !ok {
			http.Error(w, "current user text or image missing", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(terminal)
	})
	repo := newCursorOutboundRepo(map[int64]*ent.QuotaKey{11: {ID: 11, CodeEncrypted: "TEST-CODE-ELEVEN"}})
	gw, c := newCursorOutboundTestService(t, handler, repo, cursorTestDeviceIDs(repo))
	req := cursorResponsesImageRequest(t, `[{"type":"input_image","image_url":"data:image/png;base64,`+cursorTestPNGDataURI+`"},{"type":"input_text","text":"inspect screenshot"}]`)
	req.Model = "cursor-sonnet"
	body := cursorResponsesRJ(t, req)
	result, err := gw.forwardCursorResponsesEntry(context.Background(), c, cursorOutboundAccount(1, 11, "TEST-BEARER"), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	select {
	case ok := <-verified:
		require.True(t, ok, "actual request must contain the user text and original image bytes")
	default:
		t.Fatal("production Responses entry did not reach the upstream transport")
	}
}

func cursorResponsesImageContentVerifyWire(payload []byte, text string, image []byte) bool {
	// AgentClientMessage -> AgentRun -> action -> user_message_action -> UserMessage.
	for _, number := range []int{cursorFldAgentRunRequest, cursorFldRunAction, cursorFldActionUserMessage, cursorFldUserMsgActionMessage} {
		fields, err := cursorDecodeProtobuf(payload)
		if err != nil {
			return false
		}
		field, ok := cursorFindField(fields, number)
		if !ok {
			return false
		}
		payload = field.Bytes
	}
	fields, err := cursorDecodeProtobuf(payload)
	if err != nil {
		return false
	}
	message, ok := cursorFindField(fields, cursorFldUserMsgText)
	if !ok || message.String() != text {
		return false
	}
	for _, number := range []int{cursorFldUserMsgSelectedContext, cursorFldSelectedContextImages, cursorFldSelectedImageData} {
		fields, err := cursorDecodeProtobuf(payload)
		if err != nil {
			return false
		}
		field, ok := cursorFindField(fields, number)
		if !ok {
			return false
		}
		payload = field.Bytes
	}
	return bytes.Equal(image, payload)
}
