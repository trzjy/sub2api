package service

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// 本文件锁定「工具结果里带图片」这条链路的实现（2026-11-03）：
//
//	生产实证：Claude Code 的 Read 读到图片文件后，下一轮 tool_result 内层含 image 块，
//	续轮计划在 results 循环里 `tool_result block "image"` 失败关闭 → 整轮 400
//	（24h 12 次，全部同形）。
//
//	协议取证（Cursor 客户端产物 workbench.glass.main.js）：
//	  agent.v1.McpToolResultContentItem = { f1 text | f2 image }（oneof content）
//	  agent.v1.McpImageContent         = { f1 data bytes, f2 mime_type }
//	→ 协议支持工具结果带图，缺口在本站实现。

const cursorTestImageB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func cursorTestImageBytes(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(cursorTestImageB64)
	require.NoError(t, err)
	return data
}

// TestCursorToolResultImageCarriedToUpstream 覆盖：工具结果里的图片不再整轮失败，
// 而是解成原始字节随结果一起出站（results.Images），文本部分保持原样。
func TestCursorToolResultImageCarriedToUpstream(t *testing.T) {
	req := &apicompat.AnthropicRequest{
		Model: "claude-opus-5-5",
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"read that png"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"a.png"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
				`{"type":"text","text":"image below"},` +
				`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + cursorTestImageB64 + `"}}]}]`)},
		},
	}

	plan, err := cursorPlanFromMessages(req, req.Model)
	require.NoError(t, err, "工具结果里的图片不得再让整轮失败关闭")
	require.Len(t, plan.results, 1, "尾部一批 tool_result 应形成一条结果")
	res := plan.results[0]
	require.Equal(t, "toolu_1", res.CallID)
	require.Len(t, res.Images, 1, "图片必须被收集为可出站字节")
	require.Equal(t, "image/png", res.Images[0].mimeType)
	require.Equal(t, cursorTestImageBytes(t), res.Images[0].data, "出站字节必须与原图逐字节一致")
	require.Len(t, res.Text, 1)
	require.Contains(t, res.Text[0], "image below", "文本部分必须保留")
}

// TestCursorToolResultTextMatchesHistoryRepresentation 覆盖：结果侧文本与历史项规范化
// 必须逐字节同源，否则下一次续轮重算历史前缀会以 history_mismatch 失败关闭。
func TestCursorToolResultTextMatchesHistoryRepresentation(t *testing.T) {
	blocks := []apicompat.AnthropicContentBlock{
		{Type: "text", Text: "before"},
		{Type: "image", Source: &apicompat.AnthropicImageSource{Type: "base64", MediaType: "image/png", Data: cursorTestImageB64}},
		{Type: "text", Text: "after"},
	}
	resultText, images, err := cursorToolResultContent(blocks, true, cursorSkipLedger{})
	require.NoError(t, err)
	require.Len(t, images, 1)

	// 历史项规范化：用同一批块构造 tool_result 历史项，取出其 content 字段。
	raw, err := json.Marshal(blocks)
	require.NoError(t, err)
	item, err := cursorAnthropicBlockItem("user", apicompat.AnthropicContentBlock{
		Type:      "tool_result",
		ToolUseID: "toolu_1",
		Content:   raw,
	}, cursorSkipLedger{})
	require.NoError(t, err)
	var payload struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(item.canonical, &payload))
	require.Equal(t, payload.Content, resultText,
		"本批结果的文本与历史前缀的 tool_result content 必须逐字节一致")
}

// TestCursorToolResultUnknownBlockIsKeptAsMarker 覆盖 2026-11-06 口径：
// 工具结果里的未知块类型（如 document）不再整轮失败，而是序列化成定位标记参与摘要。
// 客户多传一种块，最多少一点可读内容，不该换来一个 400。
func TestCursorToolResultUnknownBlockIsKeptAsMarker(t *testing.T) {
	req := &apicompat.AnthropicRequest{
		Model: "claude-opus-5-5",
		Messages: []apicompat.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"go"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
				`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AA=="}}]}]`)},
		},
	}
	plan, err := cursorPlanFromMessages(req, req.Model)
	require.NoError(t, err, "未知块类型不得整轮拒")
	require.Len(t, plan.results, 1, "结果本身仍要形成一条，文本里带定位标记")
	require.Equal(t, "toolu_1", plan.results[0].CallID)
	require.Contains(t, plan.results[0].Text[0], "document",
		"未知块要序列化成标记，历史与结果两侧才能对齐")
	require.GreaterOrEqual(t, plan.skippedItems[cursorSkipUnsupportedPart], 1,
		"丢块要记账，实际=%s", plan.skippedItems.summary())
}

// TestCursorMcpResultFrameCarriesImageItem 覆盖出站编码：图片独立成一条
// McpSuccess.f1 内容项，走 ContentItem.f2 = McpImageContent{f1 data, f2 mime_type}，
// 且排在文本项之后；无图时字节形状与旧实现完全一致。
func TestCursorMcpResultFrameCarriesImageItem(t *testing.T) {
	img := cursorTurnImage{data: cursorTestImageBytes(t), mimeType: "image/png"}
	frame := cursorEncodeMcpToolResult(11, []byte("exec-11"), "call_11",
		[]string{"see attached"}, []cursorTurnImage{img}, nil, false)

	outer, err := cursorDecodeProtobuf(frame)
	require.NoError(t, err)
	mcp, ok := cursorFindField(outer, cursorFldClientMsgMcpResult)
	require.True(t, ok)
	mcpFields, err := cursorDecodeProtobuf(mcp.Bytes)
	require.NoError(t, err)
	succ, ok := cursorFindField(mcpFields, cursorFldMcpResultSuccess)
	require.True(t, ok)
	inner, err := cursorDecodeProtobuf(succ.Bytes)
	require.NoError(t, err)

	var textItems, imageItems int
	for _, f := range inner {
		if f.Number != cursorFldMcpSuccessContent {
			continue
		}
		itemFields, ierr := cursorDecodeProtobuf(f.Bytes)
		require.NoError(t, ierr)
		if tf, hasText := cursorFindField(itemFields, cursorFldContentItemText); hasText {
			textItems++
			textFields, terr := cursorDecodeProtobuf(tf.Bytes)
			require.NoError(t, terr)
			textF, _ := cursorFindField(textFields, cursorFldTextContentText)
			require.Equal(t, "see attached", string(textF.Bytes))
			continue
		}
		if imf, hasImage := cursorFindField(itemFields, cursorFldContentItemImage); hasImage {
			imageItems++
			imageFields, imerr := cursorDecodeProtobuf(imf.Bytes)
			require.NoError(t, imerr)
			dataF, _ := cursorFindField(imageFields, cursorFldImageContentData)
			require.Equal(t, cursorTestImageBytes(t), dataF.Bytes, "McpImageContent.f1 必须是原图字节")
			mimeF, _ := cursorFindField(imageFields, cursorFldImageContentMimeType)
			require.Equal(t, "image/png", string(mimeF.Bytes))
		}
	}
	require.Equal(t, 1, textItems)
	require.Equal(t, 1, imageItems, "每张图必须独立成一条 ContentItem 的 f2 image")

	// 无图：字节形状必须与旧实现（只有 text 项）完全一致。
	plain := cursorEncodeMcpToolResult(11, []byte("exec-11"), "call_11", []string{"see attached"}, nil, nil, false)
	plainOuter, err := cursorDecodeProtobuf(plain)
	require.NoError(t, err)
	require.NotEqual(t, frame, plain, "带图与不带图的编码必须不同")
	plainMcp, _ := cursorFindField(plainOuter, cursorFldClientMsgMcpResult)
	require.NotContains(t, string(plainMcp.Bytes), string(img.data[:8]),
		"无图结果不得携带图片字节")
}

// TestCursorAnthropicImageRejectsRemoteURL 覆盖：远程 url 图片仍失败关闭（不新增服务端抓取）。
func TestCursorAnthropicImageRejectsRemoteURL(t *testing.T) {
	_, err := cursorAnthropicImage(apicompat.AnthropicContentBlock{
		Type:   "image",
		Source: &apicompat.AnthropicImageSource{Type: "url", MediaType: "image/png", Data: "https://example.com/a.png"},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCursorResumeUnsupportedContent)

	// 非 image/* 的 mime 同样拒绝。
	_, err = cursorAnthropicImage(apicompat.AnthropicContentBlock{
		Type:   "image",
		Source: &apicompat.AnthropicImageSource{Type: "base64", MediaType: "text/html", Data: "AA=="},
	})
	require.Error(t, err)
}
