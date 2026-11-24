package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// cursorDSMLSampleResponses 是一段完整、可被官方解析器还原的 DeepSeek DSML 工具调用。
func cursorDSMLSampleResponses() string {
	return "<｜DSML｜tool_calls><｜DSML｜invoke name=\"Bash\">" +
		"<｜DSML｜parameter name=\"command\" string=\"true\">ls -la /tmp</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜tool_calls>"
}

// TestCursorDSMLDisabledResponsesStream 锁定 Cursor writer 禁用 DSML 时的流式行为：
// DSML 原文（含跨帧切分与残片）按原文逐字交付，不还原成工具调用、不计数；
// 同一 chunk 里真实 Delta.ToolCalls 仍必须产出真实 function_call。
func TestCursorDSMLDisabledResponsesStream(t *testing.T) {
	t.Run("disabled_passthrough", func(t *testing.T) {
		state := NewChatCompletionsToResponsesStreamState("deepseek-v4.1-flash-1017")
		state.ConversionOptions.DisableDSMLToolCalls = true

		// 把一段完整 DSML 调用切成两帧（在标记中间切开），验证逐字跨帧拼接。
		reasoningPart1 := "思考前半 <｜DS"
		reasoningPart2 := "ML｜" + strings.TrimPrefix(cursorDSMLSampleResponses(), "<｜DSML｜") + " 思考后半"
		reasoningFull := reasoningPart1 + reasoningPart2

		// 残片：只有 invoke 开标签、没有外层 tool_calls 与闭合，默认路径会清掉。
		contentFull := "正文 <｜DSML｜invoke name=\"Read\">残片"

		var events []ResponsesStreamEvent
		events = append(events, ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
			Choices: []ChatChunkChoice{{Delta: ChatDelta{ReasoningContent: &reasoningPart1}}},
		}, state)...)
		events = append(events, ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
			Choices: []ChatChunkChoice{{Delta: ChatDelta{ReasoningContent: &reasoningPart2}}},
		}, state)...)
		events = append(events, ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
			Choices: []ChatChunkChoice{{Delta: ChatDelta{Content: &contentFull}}},
		}, state)...)
		events = append(events, FinalizeChatCompletionsResponsesStream(state)...)

		var reasoningDelta, textDelta string
		for _, e := range events {
			switch e.Type {
			case "response.reasoning_summary_text.delta":
				reasoningDelta += e.Delta
			case "response.output_text.delta":
				textDelta += e.Delta
			case "response.output_item.added":
				if e.Item != nil && e.Item.Type == "function_call" {
					t.Fatalf("禁用 DSML 时不应产出 function_call: %+v", e.Item)
				}
			}
			if strings.Contains(e.Delta, "call_dsml_") {
				t.Fatalf("不应出现 call_dsml_: %q", e.Delta)
			}
			if e.Item != nil && strings.Contains(e.Item.CallID, "call_dsml_") {
				t.Fatalf("不应出现 call_dsml_: %q", e.Item.CallID)
			}
			if e.Response != nil {
				for _, o := range e.Response.Output {
					if strings.Contains(o.CallID, "call_dsml_") {
						t.Fatalf("completed 输出里不应有 DSML 调用: %+v", o)
					}
				}
			}
		}
		if reasoningDelta != reasoningFull {
			t.Fatalf("reasoning 增量应按原文逐字交付:\n got %q\nwant %q", reasoningDelta, reasoningFull)
		}
		if textDelta != contentFull {
			t.Fatalf("正文增量应按原文逐字交付:\n got %q\nwant %q", textDelta, contentFull)
		}
		if r, d := state.DSMLStats(); r != 0 || d != 0 {
			t.Fatalf("禁用时 DSMLStats 应为 0/0，实际 %d/%d", r, d)
		}
	})

	t.Run("disabled_with_real_tool_call", func(t *testing.T) {
		state := NewChatCompletionsToResponsesStreamState("m")
		state.ConversionOptions.DisableDSMLToolCalls = true

		// 同一 chunk 同时带 DSML 正文与真实结构化工具调用。
		content := "正文含 <｜DSML｜invoke name=\"Bash\">残片"
		idx := 0
		events := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
			Choices: []ChatChunkChoice{{Delta: ChatDelta{
				Content: &content,
				ToolCalls: []ChatToolCall{{
					Index: &idx,
					ID:    "call-real-stream",
					Type:  "function",
					Function: ChatFunctionCall{
						Name:      "Bash",
						Arguments: `{"command":"echo real"}`,
					},
				}},
			}}},
		}, state)
		events = append(events, FinalizeChatCompletionsResponsesStream(state)...)

		sawReal := false
		for _, e := range events {
			if e.Item != nil && strings.Contains(e.Item.CallID, "call_dsml_") {
				t.Fatalf("禁用 DSML 时不应产出 DSML 调用: %+v", e.Item)
			}
			if e.Type == "response.output_item.added" && e.Item != nil &&
				e.Item.Type == "function_call" && e.Item.CallID == "call-real-stream" {
				sawReal = true
				if e.Item.Name != "Bash" {
					t.Fatalf("真实工具名应为 Bash，实际 %q", e.Item.Name)
				}
			}
			if e.Response != nil {
				for _, o := range e.Response.Output {
					if strings.Contains(o.CallID, "call_dsml_") {
						t.Fatalf("completed 输出里不应有 DSML 调用: %+v", o)
					}
				}
			}
		}
		if !sawReal {
			t.Fatalf("同一 chunk 的真实 Delta.ToolCalls 必须仍产出 function_call")
		}
		if r, d := state.DSMLStats(); r != 0 || d != 0 {
			t.Fatalf("禁用时 DSMLStats 应为 0/0，实际 %d/%d", r, d)
		}
	})
}

// TestCursorDSMLDisabledResponsesNonStream 锁定非流式 WithOptions 禁用行为：
// DSML 原文作为 message output_text 保留、reasoning 原样、无 DSML function_call；
// 真实 message.ToolCalls 按 custom/namespace/tool_search 映射照常转换。
func TestCursorDSMLDisabledResponsesNonStream(t *testing.T) {
	dsml := cursorDSMLSampleResponses()
	content, _ := json.Marshal("正文 " + dsml + " 尾")
	reasoning := "思考 " + dsml

	idx := 0
	resp := &ChatCompletionsResponse{
		ID:    "chatcmpl-cursor-disabled",
		Model: "m",
		Choices: []ChatChoice{{Message: ChatMessage{
			Role:             "assistant",
			Content:          content,
			ReasoningContent: reasoning,
			ToolCalls: []ChatToolCall{
				{
					Index: &idx,
					ID:    "call-custom",
					Type:  "function",
					Function: ChatFunctionCall{
						Name:      "my_custom",
						Arguments: `{"input":"hello"}`,
					},
				},
				{
					Index: &idx,
					ID:    "call-ns",
					Type:  "function",
					Function: ChatFunctionCall{
						Name:      "ns__child",
						Arguments: `{"x":1}`,
					},
				},
				{
					Index: &idx,
					ID:    "call-ts",
					Type:  "function",
					Function: ChatFunctionCall{
						Name:      toolSearchProxyName,
						Arguments: `{"query":"q"}`,
					},
				},
				{
					Index: &idx,
					ID:    "call-plain",
					Type:  "function",
					Function: ChatFunctionCall{
						Name:      "real_fn",
						Arguments: `{"y":2}`,
					},
				},
			},
		}}},
	}

	customTools := map[string]bool{"my_custom": true}
	functionTools := map[string]bool{"real_fn": true}
	namespaceTools := map[string]NamespacedToolName{
		"ns__child": {Namespace: "ns", Name: "child"},
	}
	out := ChatCompletionsResponseToResponsesWithOptions(resp, "m", customTools, functionTools, true, namespaceTools,
		ChatResponseConversionOptions{DisableDSMLToolCalls: true})

	sawMessage, sawReasoning := false, false
	sawCustom, sawNamespace, sawToolSearch, sawPlainFunction := false, false, false, false
	for _, o := range out.Output {
		if strings.Contains(o.CallID, "call_dsml_") {
			t.Fatalf("禁用 DSML 时不应产出 DSML 调用: %+v", o)
		}
		switch o.Type {
		case "message":
			sawMessage = true
			joined := ""
			for _, c := range o.Content {
				joined += c.Text
			}
			if want := "正文 " + dsml + " 尾"; joined != want {
				t.Fatalf("正文应保留 DSML 原文:\n got %q\nwant %q", joined, want)
			}
		case "reasoning":
			sawReasoning = true
			joined := ""
			for _, s := range o.Summary {
				joined += s.Text
			}
			if want := "思考 " + dsml; joined != want {
				t.Fatalf("reasoning 应保留原文:\n got %q\nwant %q", joined, want)
			}
		case "custom_tool_call":
			if o.CallID == "call-custom" {
				sawCustom = true
				if o.Name != "my_custom" || o.Input != "hello" {
					t.Fatalf("custom 映射参数不正确: %+v", o)
				}
			}
		case "tool_search_call":
			if o.CallID == "call-ts" {
				sawToolSearch = true
				if o.Arguments != `{"query":"q"}` {
					t.Fatalf("tool_search 映射参数不正确: %+v", o)
				}
			}
		case "function_call":
			if o.CallID == "call-ns" {
				sawNamespace = true
				if o.Name != "child" || o.Namespace != "ns" {
					t.Fatalf("namespace 映射参数不正确: %+v", o)
				}
			}
			if o.CallID == "call-plain" {
				sawPlainFunction = true
				if o.Name != "real_fn" {
					t.Fatalf("普通 function 映射参数不正确: %+v", o)
				}
			}
		}
	}
	if !sawMessage || !sawReasoning {
		t.Fatalf("正文与 reasoning 原文输出都应在: message=%v reasoning=%v", sawMessage, sawReasoning)
	}
	if !sawCustom || !sawNamespace || !sawToolSearch || !sawPlainFunction {
		t.Fatalf("真实 ToolCalls 的映射应照常转换: custom=%v namespace=%v tool_search=%v function=%v",
			sawCustom, sawNamespace, sawToolSearch, sawPlainFunction)
	}
}

// TestCursorDSMLDisabledResponsesDefaultZeroValue 证明禁用开关的零值对照：
// 原入口（不传选项）仍按既有行为恢复 DSML，其它平台零值路径不变；
// 同时锁定流式 content 分支在 DSML 解析出空 plain 时不得跳过同一 chunk 的真实 ToolCalls。
func TestCursorDSMLDisabledResponsesDefaultZeroValue(t *testing.T) {
	dsml := cursorDSMLSampleResponses()

	resp := &ChatCompletionsResponse{
		ID:    "chatcmpl-default-zero",
		Model: "m",
		Choices: []ChatChoice{{Message: ChatMessage{
			Role:             "assistant",
			ReasoningContent: dsml,
		}}},
	}
	out := ChatCompletionsResponseToResponses(resp, "m", nil, nil, false, nil)
	sawDSML := false
	for _, o := range out.Output {
		if o.Type == "function_call" && o.Name == "Bash" {
			sawDSML = true
			if !strings.HasPrefix(o.CallID, "call_dsml_") {
				t.Fatalf("零值入口应产出 call_dsml_ 调用，实际 %q", o.CallID)
			}
		}
		if o.Type == "reasoning" {
			for _, s := range o.Summary {
				if strings.Contains(s.Text, dsmlMarker) {
					t.Fatalf("零值入口的 reasoning 不应残留 DSML: %q", s.Text)
				}
			}
		}
	}
	if !sawDSML {
		t.Fatalf("默认零值入口仍应恢复 DSML 调用（其它平台不变）")
	}

	// DSML 解析后 plain == ""，但同一 chunk 里的真实 ToolCalls 仍必须产出。
	// 用 Index=1 避开 DSML 还原调用占用的 0 号槽位。
	state := NewChatCompletionsToResponsesStreamState("m")
	content := dsml
	idx := 1
	events := ChatCompletionsChunkToResponsesEvents(&ChatCompletionsChunk{
		Choices: []ChatChunkChoice{{Delta: ChatDelta{
			Content: &content,
			ToolCalls: []ChatToolCall{{
				Index: &idx,
				ID:    "call-real-zero",
				Type:  "function",
				Function: ChatFunctionCall{
					Name:      "Bash",
					Arguments: `{"command":"echo real"}`,
				},
			}},
		}}},
	}, state)
	events = append(events, FinalizeChatCompletionsResponsesStream(state)...)

	sawReal := false
	for _, e := range events {
		if e.Type == "response.output_item.added" && e.Item != nil &&
			e.Item.Type == "function_call" && e.Item.CallID == "call-real-zero" {
			sawReal = true
		}
	}
	if !sawReal {
		t.Fatalf("DSML 空 plain 不得跳过同一 chunk 的真实 Delta.ToolCalls")
	}
}
