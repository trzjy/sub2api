package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"

// stripCCResponseReasoning 清空 CC 非流式响应各 choice 的 reasoning/reasoning_content，
// 使客户端可见响应不含上游思考明文。只清这两个字段 —— Content/ToolCalls/Role/Usage
// 一律不动：tool 轮的 reasoning 缓存与 usage 统计都依赖它们。
func stripCCResponseReasoning(resp *apicompat.ChatCompletionsResponse) {
	if resp == nil {
		return
	}
	for i := range resp.Choices {
		resp.Choices[i].Message.ReasoningContent = ""
		resp.Choices[i].Message.Reasoning = ""
	}
}

// stripCCChunkReasoning 清空 CC 流式 chunk 各 choice delta 的 reasoning/reasoning_content，
// 语义同 stripCCResponseReasoning（就地修改调用方持有的同一份 chunk）。
func stripCCChunkReasoning(chunk *apicompat.ChatCompletionsChunk) {
	if chunk == nil {
		return
	}
	for i := range chunk.Choices {
		chunk.Choices[i].Delta.ReasoningContent = nil
		chunk.Choices[i].Delta.Reasoning = nil
	}
}
