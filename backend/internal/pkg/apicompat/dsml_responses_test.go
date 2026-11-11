package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChatCompletionsChunkToResponsesEvents_DSMLInReasoning 覆盖 Responses 路径：
// 上游把 DSML 放在 reasoning_content 里时，必须还原成 function_call 输出项，
// 且 reasoning 里不得残留 DSML 标签。
func TestChatCompletionsChunkToResponsesEvents_DSMLInReasoning(t *testing.T) {
	state := NewChatCompletionsToResponsesStreamState("deepseek-v4.1-flash-1017")

	dsml := "<｜DSML｜tool_calls><｜DSML｜invoke name=\"Bash\">" +
		"<｜DSML｜parameter name=\"command\" string=\"true\">ls -la /tmp</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜tool_calls>"

	chunk := &ChatCompletionsChunk{
		ID:    "chatcmpl-dsml-resp",
		Model: "deepseek-v4.1-flash-1017",
		Choices: []ChatChunkChoice{
			{Delta: ChatDelta{ReasoningContent: &dsml}},
		},
	}

	events := ChatCompletionsChunkToResponsesEvents(chunk, state)

	sawToolItem := false
	var args string
	for _, e := range events {
		if e.Type == "response.output_item.added" && e.Item != nil && e.Item.Type == "function_call" {
			sawToolItem = true
			if e.Item.Name != "Bash" {
				t.Fatalf("工具名应为 Bash，实际 %q", e.Item.Name)
			}
		}
		if e.Type == "response.function_call_arguments.delta" {
			args += e.Delta
		}
		if e.Type == "response.reasoning_summary_text.delta" {
			if strings.Contains(e.Delta, dsmlMarker) {
				t.Fatalf("reasoning 里仍残留 DSML 标签: %q", e.Delta)
			}
		}
	}

	if !sawToolItem {
		t.Fatalf("未产出 function_call 输出项（events=%d）", len(events))
	}
	parsed := map[string]any{}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("参数应为合法 JSON: %v (raw=%q)", err, args)
	}
	if parsed["command"] != "ls -la /tmp" {
		t.Fatalf("command 参数不正确: %v", parsed["command"])
	}
}

// TestChatCompletionsResponseToResponses_DSMLInReasoning 覆盖 Responses 非流式路径。
func TestChatCompletionsResponseToResponses_DSMLInReasoning(t *testing.T) {
	dsml := "<｜DSML｜tool_calls><｜DSML｜invoke name=\"Bash\">" +
		"<｜DSML｜parameter name=\"command\" string=\"true\">ls -la /tmp</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜tool_calls>"

	resp := &ChatCompletionsResponse{
		ID:    "chatcmpl-dsml-resp-nostream",
		Model: "deepseek-v4.1-flash-1017",
		Choices: []ChatChoice{
			{Message: ChatMessage{Role: "assistant", ReasoningContent: dsml}},
		},
	}

	out := chatMessageToResponsesOutput(resp.Choices[0].Message, nil, nil, false, nil)

	var fnCall *ResponsesOutput
	for i := range out {
		o := out[i]
		if o.Type == "function_call" {
			fnCall = &out[i]
		}
		if o.Type == "reasoning" {
			for _, s := range o.Summary {
				if strings.Contains(s.Text, dsmlMarker) {
					t.Fatalf("reasoning 里仍残留 DSML 标签: %q", s.Text)
				}
			}
		}
		if o.Type == "message" {
			for _, c := range o.Content {
				if strings.Contains(c.Text, dsmlMarker) {
					t.Fatalf("正文里仍残留 DSML 标签: %q", c.Text)
				}
			}
		}
	}
	if fnCall == nil {
		t.Fatalf("未产出 function_call 输出项（outputs=%d）", len(out))
	}
	if fnCall.Name != "Bash" {
		t.Fatalf("工具名应为 Bash，实际 %q", fnCall.Name)
	}
	parsed := map[string]any{}
	if err := json.Unmarshal([]byte(fnCall.Arguments), &parsed); err != nil {
		t.Fatalf("参数应为合法 JSON: %v", err)
	}
	if parsed["command"] != "ls -la /tmp" {
		t.Fatalf("command 参数不正确: %v", parsed["command"])
	}
}
