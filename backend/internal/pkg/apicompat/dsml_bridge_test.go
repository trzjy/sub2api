package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChatCompletionsChunkToAnthropicEvents_DSMLStatsCounted 锁死 DSML 观测计数：
// 还原成功要计数，供 service 层统一出口落日志（dsml_restore_observability.go）。
func TestChatCompletionsChunkToAnthropicEvents_DSMLStatsCounted(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("deepseek-v4.1-flash")

	dsml := "<｜DSML｜tool_calls><｜DSML｜invoke name=\"Bash\">" +
		"<｜DSML｜parameter name=\"command\" string=\"true\">ls</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜tool_calls>"
	chunk := &ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ReasoningContent: &dsml}}},
	}
	ChatCompletionsChunkToAnthropicEvents(chunk, state)

	restored, dropped := state.DSMLStats()
	if restored != 1 {
		t.Fatalf("还原成功的调用应计数 1，实际 %d", restored)
	}
	if dropped != 0 {
		t.Fatalf("没有残片时不应计丢弃，实际 %d", dropped)
	}
}

// TestChatCompletionsChunkToAnthropicEvents_DSMLFragmentCountedAsDropped 锁死：
// 还原不出的残片要计丢弃（不外泄、也不静默）。
func TestChatCompletionsChunkToAnthropicEvents_DSMLFragmentCountedAsDropped(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("deepseek-v4.1-flash")

	fragment := "<｜DSML｜tool_calls><｜DSML｜invoke name=\"X\">"
	chunk := &ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ReasoningContent: &fragment}}},
	}
	ChatCompletionsChunkToAnthropicEvents(chunk, state)
	FinalizeChatCompletionsAnthropicStream(state)

	restored, dropped := state.DSMLStats()
	if restored != 0 {
		t.Fatalf("残片不应计入还原，实际 %d", restored)
	}
	if dropped != 1 {
		t.Fatalf("还原不出的残片应计丢弃 1，实际 %d", dropped)
	}
}

// TestChatCompletionsChunkToAnthropicEvents_DSMLInReasoning 是本次修复的核心端到端用例：
// 上游把 DSML 工具调用放在 reasoning_content 里（线上真实形态），
// 转换后必须产出标准 tool_use 事件，且 thinking 里不得残留任何 DSML 标签。
func TestChatCompletionsChunkToAnthropicEvents_DSMLInReasoning(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("deepseek-v4.1-flash-1017")

	dsml := "<｜DSML｜tool_calls>\n" +
		"<｜DSML｜invoke name=\"Bash\">\n" +
		"<｜DSML｜parameter name=\"command\" string=\"true\">ls -la /tmp</｜DSML｜parameter>\n" +
		"<｜DSML｜parameter name=\"description\" string=\"true\">列目录</｜DSML｜parameter>\n" +
		"</｜DSML｜invoke>\n" +
		"</｜DSML｜tool_calls>"

	chunk := &ChatCompletionsChunk{
		ID:    "chatcmpl-dsml",
		Model: "deepseek-v4.1-flash-1017",
		Choices: []ChatChunkChoice{
			{Delta: ChatDelta{ReasoningContent: &dsml}},
		},
	}

	events := ChatCompletionsChunkToAnthropicEvents(chunk, state)

	// 必须有 tool_use 事件。
	var toolUse *AnthropicContentBlock
	var toolInput string
	for _, e := range events {
		if e.Type == "content_block_start" && e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			toolUse = e.ContentBlock
			continue
		}
		if e.Type == "content_block_delta" && e.Delta != nil {
			if e.Delta.Type == "thinking_delta" {
				if strings.Contains(e.Delta.Thinking, dsmlMarker) {
					t.Fatalf("thinking 里仍残留 DSML 标签: %q", e.Delta.Thinking)
				}
			}
			if e.Delta.Type == "input_json_delta" {
				toolInput += e.Delta.PartialJSON
			}
		}
	}

	if toolUse == nil {
		t.Fatalf("未产出 tool_use 事件（events=%d）", len(events))
	}
	if toolUse.Name != "Bash" {
		t.Fatalf("工具名应为 Bash，实际 %q", toolUse.Name)
	}

	// 参数还原正确。
	args := map[string]any{}
	if err := json.Unmarshal([]byte(toolInput), &args); err != nil {
		t.Fatalf("参数应为合法 JSON: %v (raw=%q)", err, toolInput)
	}
	if args["command"] != "ls -la /tmp" {
		t.Fatalf("command 参数不正确: %v", args["command"])
	}
	if args["description"] != "列目录" {
		t.Fatalf("description 参数不正确: %v", args["description"])
	}
}

// TestChatCompletionsChunkToAnthropicEvents_DSMLInContent 覆盖上游把 DSML 放 content 的形态。
func TestChatCompletionsChunkToAnthropicEvents_DSMLInContent(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("deepseek-v4.1-flash-1017")

	dsml := "<｜DSML｜tool_calls><｜DSML｜invoke name=\"websearch\">" +
		"<｜DSML｜parameter name=\"query\" string=\"true\">天气</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜tool_calls>"

	content := dsml
	chunk := &ChatCompletionsChunk{
		ID:    "chatcmpl-dsml-content",
		Model: "deepseek-v4.1-flash-1017",
		Choices: []ChatChunkChoice{
			{Delta: ChatDelta{Content: &content}},
		},
	}

	events := ChatCompletionsChunkToAnthropicEvents(chunk, state)

	var toolUse *AnthropicContentBlock
	for _, e := range events {
		if e.Type == "content_block_start" && e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			toolUse = e.ContentBlock
		}
		if e.Type == "content_block_delta" && e.Delta != nil && e.Delta.Type == "text_delta" {
			if strings.Contains(e.Delta.Text, dsmlMarker) {
				t.Fatalf("正文里仍残留 DSML 标签: %q", e.Delta.Text)
			}
		}
	}
	if toolUse == nil {
		t.Fatalf("未产出 tool_use 事件（events=%d）", len(events))
	}
	if toolUse.Name != "websearch" {
		t.Fatalf("工具名应为 websearch，实际 %q", toolUse.Name)
	}
}

// TestChatCompletionsChunkToAnthropicEvents_DSMLStreamedAcrossChunks 覆盖流式跨帧：
// DSML 被切成多帧到达，凑齐后仍要还原出调用。
func TestChatCompletionsChunkToAnthropicEvents_DSMLStreamedAcrossChunks(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("deepseek-v4.1-flash-1017")

	frames := []string{
		"<｜DSML｜tool_",
		"calls>\n<｜DSML｜invoke name=\"Bash\">\n",
		"<｜DSML｜parameter name=\"command\" string=\"true\">echo hi",
		"</｜DSML｜parameter>\n</｜DSML｜invoke>\n</｜DSML｜tool_calls>",
	}

	var toolUse *AnthropicContentBlock
	for _, f := range frames {
		reasoning := f
		chunk := &ChatCompletionsChunk{
			Choices: []ChatChunkChoice{{Delta: ChatDelta{ReasoningContent: &reasoning}}},
		}
		for _, e := range ChatCompletionsChunkToAnthropicEvents(chunk, state) {
			if e.Type == "content_block_start" && e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
				toolUse = e.ContentBlock
			}
			if e.Type == "content_block_delta" && e.Delta != nil && e.Delta.Type == "thinking_delta" {
				if strings.Contains(e.Delta.Thinking, dsmlMarker) {
					t.Fatalf("thinking 里仍残留 DSML 标签: %q", e.Delta.Thinking)
				}
			}
		}
	}

	if toolUse == nil {
		t.Fatal("跨帧后未产出 tool_use 事件")
	}
	if toolUse.Name != "Bash" {
		t.Fatalf("工具名应为 Bash，实际 %q", toolUse.Name)
	}
}

// TestChatCompletionsChunkToAnthropicEvents_PlainThinkingUnaffected 确认普通思考不受影响
// （DSML 接入不能破坏既有行为）。
func TestChatCompletionsChunkToAnthropicEvents_PlainThinkingUnaffected(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("deepseek-v4.1-flash-1017")

	reasoning := "先看看目录里有什么"
	chunk := &ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ReasoningContent: &reasoning}}},
	}
	events := ChatCompletionsChunkToAnthropicEvents(chunk, state)

	got := ""
	for _, e := range events {
		if e.Type == "content_block_delta" && e.Delta != nil && e.Delta.Type == "thinking_delta" {
			got += e.Delta.Thinking
		}
	}
	if got != reasoning {
		t.Fatalf("普通思考内容应原样透传，期望 %q，实际 %q", reasoning, got)
	}
}

// TestChatCompletionsChunkToAnthropicEvents_PlainToolCallUnaffected 确认既有结构化
// tool_calls 不受影响（DSML 与标准 tool_calls 可以共存）。
func TestChatCompletionsChunkToAnthropicEvents_PlainToolCallUnaffected(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("gpt-x")

	idx := 0
	chunk := &ChatCompletionsChunk{
		Choices: []ChatChunkChoice{
			{Delta: ChatDelta{ToolCalls: []ChatToolCall{
				{Index: &idx, ID: "call_1", Type: "function",
					Function: ChatFunctionCall{Name: "Bash", Arguments: `{"command":"ls"}`}},
			}}},
		},
	}
	events := ChatCompletionsChunkToAnthropicEvents(chunk, state)

	found := false
	for _, e := range events {
		if e.Type == "content_block_start" && e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			found = true
			if e.ContentBlock.Name != "Bash" {
				t.Fatalf("工具名应为 Bash，实际 %q", e.ContentBlock.Name)
			}
		}
	}
	if !found {
		t.Fatal("标准 tool_calls 未产出 tool_use 事件")
	}
}

// TestFinalizeChatCompletionsAnthropicStream_DSMLFragmentDiscarded 确认流尾残片
// 不外泄给客户端（官方失败分支：丢弃，不泄漏协议标签）。
func TestFinalizeChatCompletionsAnthropicStream_DSMLFragmentDiscarded(t *testing.T) {
	state := NewChatCompletionsToAnthropicStreamState("deepseek-v4.1-flash-1017")

	fragment := "<｜DSML｜tool_calls><｜DSML｜invoke name=\"X\">"
	chunk := &ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{ReasoningContent: &fragment}}},
	}
	ChatCompletionsChunkToAnthropicEvents(chunk, state)

	events := FinalizeChatCompletionsAnthropicStream(state)
	for _, e := range events {
		if e.Type == "content_block_delta" && e.Delta != nil {
			if strings.Contains(e.Delta.Thinking, dsmlMarker) {
				t.Fatalf("收口时泄漏了 DSML 残片: %q", e.Delta.Thinking)
			}
			if strings.Contains(e.Delta.Text, dsmlMarker) {
				t.Fatalf("收口时泄漏了 DSML 残片: %q", e.Delta.Text)
			}
		}
	}
}

// TestChatCompletionsResponseToAnthropic_DSMLInReasoning 覆盖非流式路径：
// 上游把 DSML 放在 reasoning_content 里时，也必须还原成 tool_use 且不外泄标签。
func TestChatCompletionsResponseToAnthropic_DSMLInReasoning(t *testing.T) {
	dsml := "<｜DSML｜tool_calls><｜DSML｜invoke name=\"Bash\">" +
		"<｜DSML｜parameter name=\"command\" string=\"true\">ls -la /tmp</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜tool_calls>"

	resp := &ChatCompletionsResponse{
		ID:    "chatcmpl-dsml-nostream",
		Model: "deepseek-v4.1-flash-1017",
		Choices: []ChatChoice{
			{Message: ChatMessage{Role: "assistant", ReasoningContent: dsml}},
		},
	}

	out := ChatCompletionsResponseToAnthropic(resp, "deepseek-v4.1-flash-1017")

	var toolUse *AnthropicContentBlock
	for i := range out.Content {
		b := out.Content[i]
		if b.Type == "tool_use" {
			toolUse = &out.Content[i]
		}
		if b.Type == "thinking" && strings.Contains(b.Thinking, dsmlMarker) {
			t.Fatalf("thinking 里仍残留 DSML 标签: %q", b.Thinking)
		}
		if b.Type == "text" && strings.Contains(b.Text, dsmlMarker) {
			t.Fatalf("正文里仍残留 DSML 标签: %q", b.Text)
		}
	}
	if toolUse == nil {
		t.Fatalf("未产出 tool_use 块（content=%d）", len(out.Content))
	}
	if toolUse.Name != "Bash" {
		t.Fatalf("工具名应为 Bash，实际 %q", toolUse.Name)
	}
	cmd := map[string]any{}
	if err := json.Unmarshal(toolUse.Input, &cmd); err != nil {
		t.Fatalf("tool_use.input 应为合法 JSON: %v (raw=%q)", err, string(toolUse.Input))
	}
	if cmd["command"] != "ls -la /tmp" {
		t.Fatalf("command 参数不正确: %v", cmd["command"])
	}
}
