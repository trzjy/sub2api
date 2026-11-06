//go:build unit

package service

// 本文件钉住 Chat Completions → AgentRun 转换层的**拒绝边界与丢弃账本**。
//
// 2026-11-06 产品口径（爹明确要求）：
//
//	遇到了不认识的条目、坏内容、协议没有落点的图片/多模态/旧字段，一律丢弃 + 记账，
//	**只有"整轮真的没有可发送内容"和"以助手消息结尾的续写请求"才拒绝**。
//	客户看到的 400 一律带得出具体原因和下一步动作（见 downstream_messages.go）。
//
// 因此这里分成两组：
//   - TestCursorProtocolRequestRejectsOnlyUnsendable：真拒绝（且断言码位是 CUR-REQ-06/07）。
//   - TestCursorProtocolRequestDropsWhatItCannotForward：全部 require.NoError，
//     并断言 cursorSkipLedger 里的丢弃原因与条数——这是唯一能看出客户端在演进什么的手段。

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestCursorProtocolRequestRejectsOnlyUnsendable(t *testing.T) {
	cases := []struct {
		name   string
		req    *apicompat.ChatCompletionsRequest
		model  string
		code   string
		status int
	}{
		// CUR-EXEC-08（500）：调用方不变量破坏（不是客户输入问题）。
		{"nil request", nil, "grok-4.7",
			infraerrors.CursorExecInternalErrorCode, http.StatusInternalServerError},
		// CUR-EXEC-07（500）：管理员模型映射为空（配置问题，不是客户输入问题）。
		{"empty upstream model", &apicompat.ChatCompletionsRequest{
			Messages: []apicompat.ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		}, "   ", infraerrors.CursorExecModelMappingInvalidCode, http.StatusInternalServerError},
		// CUR-REQ-06（400）：整轮没有可发送内容 —— 真的没东西可发。
		{"no messages at all", &apicompat.ChatCompletionsRequest{}, "grok-4.7",
			infraerrors.CursorReqNoContentCode, http.StatusBadRequest},
		{"only empty messages", &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`""`)},
			{Role: "user", Content: json.RawMessage(`"   "`)},
		}}, "grok-4.7", infraerrors.CursorReqNoContentCode, http.StatusBadRequest},
		// CUR-REQ-07（400）：以助手消息结尾的续写请求 —— 唯一因为**形状**而必须拒的入站。
		{"ends with assistant prefill", &apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`"prefill"`)},
		}}, "grok-4.7", infraerrors.CursorReqPrefillCode, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := cursorConvertRequest(c.req, c.model)
			assertCursorRequestErrorCode(t, err, c.code, c.status)
		})
	}
}

// TestCursorProtocolRequestDropsWhatItCannotForward 钉住"能丢就丢"的每一类，
// 同时断言账本记的原因正确——账记错了，下次统计客户端在演进什么就会看错。
func TestCursorProtocolRequestDropsWhatItCannotForward(t *testing.T) {
	const pngURI = "data:image/png;base64," + cursorTestPNGDataURI
	imagePart := `{"type":"image_url","image_url":{"url":` + cursorJSONString(pngURI) + `}}`

	// 前置一条正常 user，保证"丢掉目标消息之后"整轮仍然可发送。
	withHead := func(msgs ...apicompat.ChatMessage) *apicompat.ChatCompletionsRequest {
		head := apicompat.ChatMessage{Role: "user", Content: json.RawMessage(`"hi"`)}
		return &apicompat.ChatCompletionsRequest{Messages: append([]apicompat.ChatMessage{head}, msgs...)}
	}
	msg := func(role, content string) apicompat.ChatMessage {
		return apicompat.ChatMessage{Role: role, Content: json.RawMessage(content)}
	}

	cases := []struct {
		name     string
		req      *apicompat.ChatCompletionsRequest
		kind     string
		atLeast  int
	}{
		// 未知 / 空角色：只丢这一条（原本整轮 400）。
		{"message without role", withHead(msg("", `"hi"`)), cursorSkipEmptyRole, 1},
		{"unsupported role", withHead(msg("wizard", `"hi"`)), cursorSkipUnknownRole, 1},
		// 内容读不出来：丢这条消息的内容（原本整轮 400）。
		{"content not a string", withHead(msg("user", `"unterminated`)), cursorSkipBadContent, 1},
		{"content not a valid parts array", withHead(msg("user", `[1,2]`)), cursorSkipBadContent, 1},
		{"numeric content", withHead(msg("user", `42`)), cursorSkipBadContent, 1},
		// 多模态 part：丢这个 part，保留其余文本（原本整轮 400）。
		{"file part without image", withHead(msg("user", `[{"type":"text","text":"看这个"},{"type":"file","file":{"file_id":"f"}}]`)),
			cursorSkipUnsupportedPart, 1},
		{"file part alongside image", withHead(msg("user", `[`+imagePart+`,{"type":"file","file":{"file_id":"f"}}]`)),
			cursorSkipUnsupportedPart, 1},
		// 图片不合规：丢这张图，保留文本（原本整轮 400）。
		{"image part missing url", withHead(msg("user", `[{"type":"text","text":"看图"},{"type":"image_url"}]`)),
			cursorSkipImageInvalid, 1},
		// 图片落在非 user 消息里：丢图保文本（原本整轮 400）。
		{"system message with image", withHead(msg("system", `[`+imagePart+`]`)), cursorSkipImageOutsideUser, 1},
		{"assistant message with image", withHead(
			msg("assistant", `[`+imagePart+`]`), msg("user", `"again"`)), cursorSkipImageOutsideUser, 1},
		{"tool message with image", withHead(
			msg("tool", `[`+imagePart+`]`), msg("user", `"again"`)), cursorSkipImageOutsideUser, 1},
		// legacy function_call：丢这个字段、保本文（原本整轮 400）。
		{"legacy assistant function_call", withHead(
			apicompat.ChatMessage{Role: "assistant", Content: json.RawMessage(`"x"`),
				FunctionCall: &apicompat.ChatFunctionCall{Name: "f", Arguments: "{}"}},
			msg("user", `"again"`)), cursorSkipLegacyFunctionCall, 1},
		// legacy function 消息没有 tool_call_id：不拒，落成无 id 的结果文本。
		{"legacy function without call id", withHead(
			msg("function", `"sunny"`), msg("user", `"again"`)), cursorSkipLegacyFunctionNoID, 1},
		// 尾部空 assistant（prefill 占位）：丢掉它本轮照常发。
		{"empty assistant tail", withHead(msg("assistant", `""`)), cursorSkipEmptyAssistantTail, 1},
	}
	for _, c := range cases {
		if c.atLeast == 0 {
			continue // 占位用例，真实输入见下方专门的图片表
		}
		t.Run(c.name, func(t *testing.T) {
			skipped := cursorSkipLedger{}
			_, _, err := cursorBuildAgentRun(c.req, "grok-4.7", skipped)
			require.NoError(t, err, "不该整轮拒：这类形状一律丢弃继续本轮")
			require.GreaterOrEqual(t, skipped[c.kind], c.atLeast,
				"丢弃原因要记对，实际账本=%s", skipped.summary())
		})
	}

	// 坏图片的每一种形态：都只丢图（kind 一致），不整轮拒。
	imageCases := map[string]string{
		"image part missing url":  `[{"type":"text","text":"看图"},{"type":"image_url"}]`,
		"remote image url":        `[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`,
		"data URI without comma":  `[{"type":"image_url","image_url":{"url":"data:image/png;base64"}}]`,
		"data URI not base64":     `[{"type":"image_url","image_url":{"url":"data:image/png,abcd"}}]`,
		"data URI missing mime":   `[{"type":"image_url","image_url":{"url":"data:;base64,AAAA"}}]`,
		"data URI non-image mime": `[{"type":"image_url","image_url":{"url":"data:text/html;base64,AAAA"}}]`,
		"data URI invalid base64": `[{"type":"image_url","image_url":{"url":"data:image/png;base64,@@@@"}}]`,
		"data URI empty payload":  `[{"type":"image_url","image_url":{"url":"data:image/png;base64,"}}]`,
	}
	for name, content := range imageCases {
		t.Run(name, func(t *testing.T) {
			skipped := cursorSkipLedger{}
			_, _, err := cursorBuildAgentRun(withHead(msg("user", content)), "grok-4.7", skipped)
			require.NoError(t, err, "坏图片只丢图，不能整轮拒")
			require.GreaterOrEqual(t, skipped[cursorSkipImageInvalid], 1,
				"坏图片要记 image_invalid，实际=%s", skipped.summary())
		})
	}

	// 工具 schema 非法：丢这一个工具，请求照发（原本整轮 400）。
	t.Run("invalid tool input_schema", func(t *testing.T) {
		req := withHead()
		req.Tools = []apicompat.ChatTool{{Type: "function", Function: &apicompat.ChatFunction{
			Name: "broken", Parameters: json.RawMessage(`{not json`)}}}
		skipped := cursorSkipLedger{}
		_, _, err := cursorBuildAgentRun(req, "grok-4.7", skipped)
		require.NoError(t, err, "坏 schema 只丢那个工具，不能整轮拒")
		require.Equal(t, 1, skipped[cursorSkipToolSchemaInvalid])
	})

	// 历史里 assistant 带图：cursorHistoryBlobs 直接调用时也只丢图，不报错。
	t.Run("history assistant turn with image", func(t *testing.T) {
		skipped := cursorSkipLedger{}
		blobs, err := cursorHistoryBlobs([]cursorAgentTurn{{
			role:   "assistant",
			text:   "x",
			images: []cursorTurnImage{{data: []byte{1}, mimeType: "image/png"}},
		}}, skipped)
		require.NoError(t, err)
		require.Len(t, blobs, 1, "文本仍要落成一个 blob，只丢图")
		require.Equal(t, 1, skipped[cursorSkipImageOutsideUser])
	})
}

func assertCursorRequestErrorCode(t *testing.T, err error, code string, status int) {
	t.Helper()
	require.Error(t, err)
	var reqErr *CursorRequestError
	require.True(t, errors.As(err, &reqErr), "error must be *CursorRequestError, got %T: %v", err, err)
	require.Equal(t, code, reqErr.Code)
	require.Equal(t, status, reqErr.Status)
	require.True(t, errors.Is(err, ErrCursorUnsupportedRequest), "must still unwrap to the sentinel")
}
