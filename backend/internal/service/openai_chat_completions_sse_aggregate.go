package service

import (
	"encoding/json"
	"sort"
	"strings"
)

// aggregatedToolCall 是 SSE 分片合并中的单个 tool_call 累加器。
type aggregatedToolCall struct {
	ID        string
	Type      string
	Name      string
	Arguments strings.Builder
}

// aggregateOpenAIChatCompletionsSSE 把 OpenAI 兼容的 chat.completion.chunk SSE 流
// 聚合为单条 chat.completion JSON。
//
// 使用场景：入站 stream:false、但上游被强制 stream:true（CodeBuddy 即此类，见 §2.5
// 规则 1）。通用路径 handleNonStreamingResponse 对 SSE 上游调用 handleSSEToJSON，而
// 后者只覆盖 Codex/Responses 形状（依赖 response.completed 终态 + output 数组），
// 对 chat.completion.chunk 形状会落入 else 原样回写 SSE（活体验收 F9）。
//
// 返回 (聚合 JSON, true)；非 SSE 或未识别出任何 chat.completion.chunk 帧时返回 (nil,false)。
func aggregateOpenAIChatCompletionsSSE(body []byte) ([]byte, bool) {
	var (
		id, model string
		created   int64
		content   strings.Builder
		reasoning strings.Builder
		finish    string
		usage     json.RawMessage
		sawChunk  bool
	)
	tools := map[int]*aggregatedToolCall{}
	var order []int

	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var chunk struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			Model   string `json:"model"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		// 多候选（n>1）无法安全合并为单条 message，交回既有路径处理（不越界接管）。
		if len(chunk.Choices) > 1 {
			return nil, false
		}
		if chunk.Object != "chat.completion.chunk" && len(chunk.Choices) == 0 {
			continue
		}
		sawChunk = true
		if id == "" {
			id = chunk.ID
		}
		if model == "" {
			model = chunk.Model
		}
		if created == 0 {
			created = chunk.Created
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
			for pos, tc := range choice.Delta.ToolCalls {
				idx := pos
				if tc.Index != nil {
					idx = *tc.Index
				}
				acc, ok := tools[idx]
				if !ok {
					acc = &aggregatedToolCall{}
					tools[idx] = acc
					order = append(order, idx)
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Type != "" {
					acc.Type = tc.Type
				}
				// 分片语义：name/id 只在首片出现，后续片仅追加 arguments，空值不得覆盖。
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				acc.Arguments.WriteString(tc.Function.Arguments)
			}
		}
	}
	if !sawChunk {
		return nil, false
	}

	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(order) > 0 {
		sort.Ints(order)
		calls := make([]any, 0, len(order))
		for _, idx := range order {
			acc := tools[idx]
			call := map[string]any{
				"index": idx,
				"type":  firstNonEmpty(acc.Type, "function"),
				"function": map[string]any{
					"name":      acc.Name,
					"arguments": acc.Arguments.String(),
				},
			}
			if acc.ID != "" {
				call["id"] = acc.ID
			}
			calls = append(calls, call)
		}
		message["tool_calls"] = calls
	}
	if finish == "" {
		finish = "stop"
	}

	out := map[string]any{
		"id":      firstNonEmpty(id, "chatcmpl-aggregated"),
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return encoded, true
}
