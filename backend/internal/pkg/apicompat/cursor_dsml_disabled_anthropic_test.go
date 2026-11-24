package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件锁死 Cursor AgentRun writer 的 DSML 禁用开关：只影响显式传入
// DisableDSMLToolCalls=true 的调用方，默认零值路径（其它调用方）行为完全不变。
// DSML 样本直接沿用 dsml_bridge_test.go 的官方样本与跨帧拆分。

const cursorDSMLSample = "<｜DSML｜tool_calls>\n" +
	"<｜DSML｜invoke name=\"Bash\">\n" +
	"<｜DSML｜parameter name=\"command\" string=\"true\">ls -la /tmp</｜DSML｜parameter>\n" +
	"<｜DSML｜parameter name=\"description\" string=\"true\">列目录</｜DSML｜parameter>\n" +
	"</｜DSML｜invoke>\n" +
	"</｜DSML｜tool_calls>"

var cursorDSMLFrames = []string{
	"<｜DSML｜tool_",
	"calls>\n<｜DSML｜invoke name=\"Bash\">\n",
	"<｜DSML｜parameter name=\"command\" string=\"true\">echo hi",
	"</｜DSML｜parameter>\n</｜DSML｜invoke>\n</｜DSML｜tool_calls>",
}

func cursorDisabledAnthropicState() *ChatCompletionsToAnthropicStreamState {
	state := NewChatCompletionsToAnthropicStreamState("deepseek-v4.1-flash-1017")
	state.ConversionOptions.DisableDSMLToolCalls = true
	return state
}

// assertNoCursorDSMLToolUse 禁止任何 DSML 还原产物泄漏：既不能有 tool_use 块，
// 事件里也不能出现 call_dsml_ 前缀（那是 DSML 还原专用 ID）。
func assertNoCursorDSMLToolUse(t *testing.T, e AnthropicStreamEvent) {
	t.Helper()
	if e.Type == "content_block_start" && e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
		t.Fatalf("禁用 DSML 后不应产生 tool_use 块: %+v", e.ContentBlock)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("序列化事件失败: %v", err)
	}
	if strings.Contains(string(raw), "call_dsml_") {
		t.Fatalf("事件中出现 DSML 专用 ID 前缀 call_dsml_: %s", raw)
	}
}

func TestCursorDSMLDisabledAnthropicStream(t *testing.T) {
	t.Run("DSML 跨帧不还原且正文原样", func(t *testing.T) {
		// reasoning 跨帧：禁用后整段原文进 thinking_delta，不产生 tool_use。
		reasonState := cursorDisabledAnthropicState()
		var reasoningRaw, gotThinking string
		for _, frame := range cursorDSMLFrames {
			reasoningRaw += frame
			f := frame
			chunk := &ChatCompletionsChunk{
				Choices: []ChatChunkChoice{{Delta: ChatDelta{ReasoningContent: &f}}},
			}
			for _, e := range ChatCompletionsChunkToAnthropicEvents(chunk, reasonState) {
				assertNoCursorDSMLToolUse(t, e)
				if e.Delta != nil && e.Delta.Type == "thinking_delta" {
					gotThinking += e.Delta.Thinking
				}
			}
		}
		for _, e := range FinalizeChatCompletionsAnthropicStream(reasonState) {
			assertNoCursorDSMLToolUse(t, e)
			if e.Delta != nil && e.Delta.Type == "thinking_delta" {
				gotThinking += e.Delta.Thinking
			}
		}
		if gotThinking != reasoningRaw {
			t.Fatalf("禁用后 reasoning 应原样进入 thinking，期望 %q，实际 %q", reasoningRaw, gotThinking)
		}
		if restored, dropped := reasonState.DSMLStats(); restored != 0 || dropped != 0 {
			t.Fatalf("禁用后 DSMLStats 应为 0/0，实际 %d/%d", restored, dropped)
		}

		// content 跨帧：禁用后 DSML 原文作为正文 delta 原样交付（含跨帧拆分）。
		contentState := cursorDisabledAnthropicState()
		var contentRaw, gotText string
		for _, frame := range cursorDSMLFrames {
			contentRaw += frame
			f := frame
			chunk := &ChatCompletionsChunk{
				Choices: []ChatChunkChoice{{Delta: ChatDelta{Content: &f}}},
			}
			for _, e := range ChatCompletionsChunkToAnthropicEvents(chunk, contentState) {
				assertNoCursorDSMLToolUse(t, e)
				if e.Delta != nil && e.Delta.Type == "text_delta" {
					gotText += e.Delta.Text
				}
			}
		}
		for _, e := range FinalizeChatCompletionsAnthropicStream(contentState) {
			assertNoCursorDSMLToolUse(t, e)
			if e.Delta != nil && e.Delta.Type == "text_delta" {
				gotText += e.Delta.Text
			}
		}
		if gotText != contentRaw {
			t.Fatalf("禁用后正文应原样交付，期望 %q，实际 %q", contentRaw, gotText)
		}
		if restored, dropped := contentState.DSMLStats(); restored != 0 || dropped != 0 {
			t.Fatalf("禁用后 DSMLStats 应为 0/0，实际 %d/%d", restored, dropped)
		}
	})

	t.Run("同 chunk 的真实 ToolCalls 仍产生 tool_use", func(t *testing.T) {
		state := cursorDisabledAnthropicState()
		dsml := cursorDSMLSample
		idx := 0
		chunk := &ChatCompletionsChunk{
			ID:    "chatcmpl-cursor-disabled",
			Model: "deepseek-v4.1-flash-1017",
			Choices: []ChatChunkChoice{{Delta: ChatDelta{
				ReasoningContent: &dsml,
				ToolCalls: []ChatToolCall{{
					Index:    &idx,
					ID:       "call_real_42",
					Type:     "function",
					Function: ChatFunctionCall{Name: "Read", Arguments: `{"file_path":"/tmp/a"}`},
				}},
			}}},
		}

		var tools []*AnthropicContentBlock
		for _, e := range ChatCompletionsChunkToAnthropicEvents(chunk, state) {
			if e.Type == "content_block_start" && e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
				tools = append(tools, e.ContentBlock)
			}
		}
		FinalizeChatCompletionsAnthropicStream(state)

		if len(tools) != 1 {
			t.Fatalf("应只有真实 ToolCalls 产生的 1 个 tool_use，实际 %d", len(tools))
		}
		if tools[0].Name != "Read" {
			t.Fatalf("真实工具名应为 Read，实际 %q", tools[0].Name)
		}
		if tools[0].ID != "call_real_42" {
			t.Fatalf("真实工具 ID 应原样透传，实际 %q", tools[0].ID)
		}
		if strings.Contains(tools[0].ID, "call_dsml_") {
			t.Fatalf("真实工具 ID 不应带 DSML 前缀: %q", tools[0].ID)
		}
		if restored, dropped := state.DSMLStats(); restored != 0 || dropped != 0 {
			t.Fatalf("禁用后 DSMLStats 应为 0/0，实际 %d/%d", restored, dropped)
		}
	})
}

func TestCursorDSMLDisabledAnthropicNonStream(t *testing.T) {
	dsml := cursorDSMLSample
	contentBytes, err := json.Marshal(dsml)
	if err != nil {
		t.Fatalf("构造 content 失败: %v", err)
	}
	opts := ChatResponseConversionOptions{DisableDSMLToolCalls: true}

	buildResp := func(withRealTool bool) *ChatCompletionsResponse {
		message := ChatMessage{
			Role:             "assistant",
			ReasoningContent: dsml,
			Content:          contentBytes,
		}
		if withRealTool {
			message.ToolCalls = []ChatToolCall{{
				ID:       "call_real_7",
				Type:     "function",
				Function: ChatFunctionCall{Name: "Read", Arguments: `{"file_path":"/tmp/a"}`},
			}}
		}
		return &ChatCompletionsResponse{
			ID:      "chatcmpl-cursor-disabled-nostream",
			Model:   "deepseek-v4.1-flash-1017",
			Choices: []ChatChoice{{Message: message}},
		}
	}

	t.Run("DSML 原文保留且无 tool_use", func(t *testing.T) {
		out := ChatCompletionsResponseToAnthropicWithOptions(buildResp(false), "deepseek-v4.1-flash-1017", opts)
		var thinking, text string
		for _, b := range out.Content {
			switch b.Type {
			case "thinking":
				thinking = b.Thinking
			case "text":
				text = b.Text
			case "tool_use":
				t.Fatalf("禁用 DSML 后不应产生 tool_use: %+v", b)
			}
		}
		if thinking != dsml {
			t.Fatalf("禁用后 reasoning 应原样进 thinking，期望 %q，实际 %q", dsml, thinking)
		}
		if text != dsml {
			t.Fatalf("禁用后 DSML 原文应作为 text 块保留，期望 %q，实际 %q", dsml, text)
		}
	})

	t.Run("真实 ToolCalls 仍转 tool_use", func(t *testing.T) {
		out := ChatCompletionsResponseToAnthropicWithOptions(buildResp(true), "deepseek-v4.1-flash-1017", opts)
		var tools []AnthropicContentBlock
		for _, b := range out.Content {
			if b.Type == "tool_use" {
				tools = append(tools, b)
			}
		}
		if len(tools) != 1 {
			t.Fatalf("应只有真实 ToolCalls 产生的 1 个 tool_use，实际 %d", len(tools))
		}
		if tools[0].Name != "Read" || tools[0].ID != "call_real_7" {
			t.Fatalf("真实工具应原样转换，实际 name=%q id=%q", tools[0].Name, tools[0].ID)
		}
		if strings.Contains(string(tools[0].ID), "call_dsml_") {
			t.Fatalf("真实工具 ID 不应带 DSML 前缀: %q", tools[0].ID)
		}
	})
}

// 默认零值对照：同一输入走 ChatCompletionsResponseToAnthropic 仍恢复 DSML 工具，
// 证明其它调用方（未传禁用开关）的既有行为完全不变。
func TestCursorDSMLDisabledAnthropicDefaultUnchanged(t *testing.T) {
	dsml := cursorDSMLSample
	contentBytes, err := json.Marshal(dsml)
	if err != nil {
		t.Fatalf("构造 content 失败: %v", err)
	}
	resp := &ChatCompletionsResponse{
		ID:    "chatcmpl-default-dsml",
		Model: "deepseek-v4.1-flash-1017",
		Choices: []ChatChoice{{Message: ChatMessage{
			Role:             "assistant",
			ReasoningContent: dsml,
			Content:          contentBytes,
		}}},
	}

	out := ChatCompletionsResponseToAnthropic(resp, "deepseek-v4.1-flash-1017")

	var tool *AnthropicContentBlock
	for i := range out.Content {
		b := &out.Content[i]
		if b.Type == "tool_use" {
			tool = b
		}
		if strings.Contains(b.Thinking, dsmlMarker) || strings.Contains(b.Text, dsmlMarker) {
			t.Fatalf("默认零值路径仍应摘净 DSML 标签: %+v", *b)
		}
	}
	if tool == nil {
		t.Fatalf("默认零值路径应恢复 DSML 工具（其它调用方行为不变），content=%d", len(out.Content))
	}
	if tool.Name != "Bash" {
		t.Fatalf("DSML 工具名应为 Bash，实际 %q", tool.Name)
	}
}
