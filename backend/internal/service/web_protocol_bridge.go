package service

// web_protocol_bridge.go 把网页逆向适配器（web-deepseek / web-zhipu / web-kimi）产出的
// 通用 OpenAI chat.completion / chat.completion.chunk 包络，按入站协议回桥为对应形态
// （#2，Codex 审查回执）：
//
//   - /v1/chat/completions 入站 → chat.completion 直出（现状，不变）；
//   - /v1/responses 入站       → OpenAI Responses API 对象 / 事件流；
//   - /v1/messages 入站        → Anthropic Messages 响应 / 事件流。
//
// 转换全部复用既有 apicompat 回桥（ChatCompletionsResponseToResponses /
// ChatCompletionsChunkToResponsesEvents / FinalizeChatCompletionsResponsesStream /
// ChatCompletionsResponseToAnthropic / ChatCompletionsChunkToAnthropicEvents /
// FinalizeChatCompletionsAnthropicStream），不在三个 forward 内部散落任何协议逻辑。
// 三个 forward 只负责把上游数据聚合为标准的 chat.completion 包络，再由本文件统一回桥。

import (
	"encoding/json"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// webResponseMode 选择 forwardWeb* 出站响应的协议形态，与入站协议对齐。
type webResponseMode int

const (
	// webResponseModeChat 入站 /v1/chat/completions：chat.completion 直出（现状正确）。
	webResponseModeChat webResponseMode = iota
	// webResponseModeResponses 入站 /v1/responses：回桥为 Responses API 响应。
	webResponseModeResponses
	// webResponseModeAnthropic 入站 /v1/messages：回桥为 Anthropic Messages 响应。
	webResponseModeAnthropic
)

// webClientStreamState 跨流式分片持有回桥状态，以及出站协议形态与模型名。
// chat 模式无需状态（直出），respState / anthState 仅 Responses / Anthropic 模式使用。
type webClientStreamState struct {
	mode      webResponseMode
	model     string
	respState *apicompat.ChatCompletionsToResponsesStreamState
	anthState *apicompat.ChatCompletionsToAnthropicStreamState
}

// newWebClientStreamState 按出站协议创建流式状态；chat 模式状态机置空（直出）。
func newWebClientStreamState(mode webResponseMode, model string) *webClientStreamState {
	st := &webClientStreamState{mode: mode, model: model}
	switch mode {
	case webResponseModeResponses:
		st.respState = apicompat.NewChatCompletionsToResponsesStreamState(model)
	case webResponseModeAnthropic:
		st.anthState = apicompat.NewChatCompletionsToAnthropicStreamState(model)
	}
	return st
}

// writeWebStreamChunk 把一帧 chat.completion.chunk 包络按出站协议写回客户端。
// 调用方负责在流结束（含终帧）时再调用一次 finalizeWebStream。
func writeWebStreamChunk(c *gin.Context, st *webClientStreamState, envelope map[string]any) error {
	switch st.mode {
	case webResponseModeChat:
		return writeRawWebChunk(c, envelope)
	case webResponseModeResponses:
		chunk := webEnvelopeToChatChunk(envelope)
		if chunk == nil {
			return nil
		}
		return writeResponsesEvents(c, apicompat.ChatCompletionsChunkToResponsesEvents(chunk, st.respState))
	case webResponseModeAnthropic:
		chunk := webEnvelopeToChatChunk(envelope)
		if chunk == nil {
			return nil
		}
		return writeAnthropicEvents(c, apicompat.ChatCompletionsChunkToAnthropicEvents(chunk, st.anthState))
	}
	return nil
}

// finalizeWebStream 写出流的终止符：chat 模式为 data: [DONE]；Responses / Anthropic 模式
// 由对应回桥发出 terminal 事件（response.completed / message_stop），不再写 [DONE]。
func finalizeWebStream(c *gin.Context, st *webClientStreamState) error {
	switch st.mode {
	case webResponseModeChat:
		if _, err := c.Writer.WriteString("data: [DONE]\n\n"); err != nil {
			return err
		}
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	case webResponseModeResponses:
		return writeResponsesEvents(c, apicompat.FinalizeChatCompletionsResponsesStream(st.respState))
	case webResponseModeAnthropic:
		return writeAnthropicEvents(c, apicompat.FinalizeChatCompletionsAnthropicStream(st.anthState))
	}
	return nil
}

// writeWebCompletion 把非流式 chat.completion 包络按出站协议写回客户端（JSON 响应体）。
func writeWebCompletion(c *gin.Context, st *webClientStreamState, completion map[string]any) error {
	switch st.mode {
	case webResponseModeChat:
		data, err := json.Marshal(completion)
		if err != nil {
			return err
		}
		c.Header("Content-Type", "application/json")
		c.Data(http.StatusOK, "application/json", data)
		return nil
	case webResponseModeResponses:
		out := apicompat.ChatCompletionsResponseToResponses(webEnvelopeToChatResponse(completion), st.model, nil, nil, false, nil)
		data, err := json.Marshal(out)
		if err != nil {
			return err
		}
		c.Header("Content-Type", "application/json")
		c.Data(http.StatusOK, "application/json", data)
		return nil
	case webResponseModeAnthropic:
		out := apicompat.ChatCompletionsResponseToAnthropic(webEnvelopeToChatResponse(completion), st.model)
		data, err := json.Marshal(out)
		if err != nil {
			return err
		}
		c.Header("Content-Type", "application/json")
		c.Data(http.StatusOK, "application/json", data)
		return nil
	}
	return nil
}

// writeRawWebChunk 以标准 OpenAI chat.completion.chunk 形状（data: 前缀）写一帧 SSE。
func writeRawWebChunk(c *gin.Context, chunk map[string]any) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	frame := append([]byte("data: "), data...)
	frame = append(frame, '\n', '\n')
	if _, err := c.Writer.Write(frame); err != nil {
		return err
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// writeResponsesEvents 把若干 Responses 流事件写为 SSE 帧（event: <type>\ndata: <json>）。
func writeResponsesEvents(c *gin.Context, events []apicompat.ResponsesStreamEvent) error {
	for i := range events {
		sse, err := apicompat.ResponsesEventToSSE(events[i])
		if err != nil {
			return err
		}
		if _, err := c.Writer.WriteString(sse); err != nil {
			return err
		}
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// writeAnthropicEvents 把若干 Anthropic 流事件写为 SSE 帧（event: <type>\ndata: <json>）。
func writeAnthropicEvents(c *gin.Context, events []apicompat.AnthropicStreamEvent) error {
	for i := range events {
		sse, err := apicompat.ResponsesAnthropicEventToSSE(events[i])
		if err != nil {
			return err
		}
		if _, err := c.Writer.WriteString(sse); err != nil {
			return err
		}
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// webEnvelopeToChatChunk 把 chat.completion.chunk 包络经 JSON 往返还原为
// apicompat.ChatCompletionsChunk；usage 单独以兼容解析覆盖（见 webUsageFromEnvelope）。
func webEnvelopeToChatChunk(envelope map[string]any) *apicompat.ChatCompletionsChunk {
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil
	}
	var chunk apicompat.ChatCompletionsChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil
	}
	if u := webUsageFromEnvelope(envelope); u != nil {
		chunk.Usage = u
	}
	return &chunk
}

// webEnvelopeToChatResponse 把 chat.completion 包络经 JSON 往返还原为
// apicompat.ChatCompletionsResponse；usage 单独以兼容解析覆盖。
func webEnvelopeToChatResponse(completion map[string]any) *apicompat.ChatCompletionsResponse {
	data, err := json.Marshal(completion)
	if err != nil {
		return nil
	}
	var resp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil
	}
	if u := webUsageFromEnvelope(completion); u != nil {
		resp.Usage = u
	}
	return &resp
}

// webUsageFromEnvelope 兼容两种 usage 命名形态：上游 chat.completion 的
// prompt_tokens/completion_tokens，以及本仓库网页适配器本地 OpenAIUsage 的
// input_tokens/output_tokens（Anthropic/Responses 命名）。统一归一到 apicompat.ChatUsage
// （prompt_tokens/completion_tokens 命名），保证回桥后 Responses/Anthropic usage 正确。
func webUsageFromEnvelope(envelope map[string]any) *apicompat.ChatUsage {
	raw, ok := envelope["usage"]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var u struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
	}
	if err := json.Unmarshal(data, &u); err != nil {
		return nil
	}
	prompt := u.PromptTokens
	completion := u.CompletionTokens
	if prompt == 0 && completion == 0 {
		prompt = u.InputTokens
		completion = u.OutputTokens
	}
	if prompt == 0 && completion == 0 {
		return nil
	}
	total := u.TotalTokens
	if total == 0 {
		total = prompt + completion
	}
	return &apicompat.ChatUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	}
}
