package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// collectCCStreamAsResponse 读取强制流式上游的 CC SSE 流，聚合为完整
// chat.completion 响应（非流式契约）。聚合期间零字节写客户端。
//
// 读循环复用 scanCCStream（共享 SSE 骨架：maxLineSize、[DONE] 哨兵、usage 最新
// 值、首 token 时延、读错误过滤），聚合逻辑在 emit 回调中累积。本函数不写任何
// 响应头/体；所有失败语义都显式上抛错误，由调用方既有分支处理（fail closed，
// 禁半个响应）。
func (s *OpenAIGatewayService) collectCCStreamAsResponse(
	resp *http.Response, c *gin.Context, logPrefix string,
) (*apicompat.ChatCompletionsResponse, OpenAIUsage, error) {
	// choiceAcc 是按 choice.index 累积一组 content/reasoning/tool_calls/
	// finish_reason 的累加器（n>1 时每 index 一组）。
	type choiceAcc struct {
		content     strings.Builder
		contentSeen bool // 是否收到过任何 content 增量（含显式空串 ""）
		reasoning   strings.Builder
		finish      string
		tools       map[int]*aggregatedToolCall
		toolOrder   []int
	}
	accs := map[int]*choiceAcc{}
	var accOrder []int

	var (
		id, model string
		created   int64
		sawChunk  bool // 仅在 len(chunk.Choices) > 0 时置位（usage-only 帧不计）
		sawUsage  bool
		lastUsage *apicompat.ChatUsage
	)

	st := s.scanCCStream(c, resp, logPrefix, resp.Header.Get("x-request-id"), time.Now(),
		func(chunk *apicompat.ChatCompletionsChunk) {
			// usage-only 帧（choices:[]）仅记录 usage 存在性，不计入有效 chunk。
			if chunk.Usage != nil {
				sawUsage = true
				lastUsage = chunk.Usage
			}
			if len(chunk.Choices) == 0 {
				return
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
			for _, ch := range chunk.Choices {
				acc, ok := accs[ch.Index]
				if !ok {
					acc = &choiceAcc{tools: map[int]*aggregatedToolCall{}}
					accs[ch.Index] = acc
					accOrder = append(accOrder, ch.Index)
				}
				if ch.Delta.Content != nil {
					acc.contentSeen = true
					acc.content.WriteString(*ch.Delta.Content)
				}
				if ch.Delta.ReasoningContent != nil {
					acc.reasoning.WriteString(*ch.Delta.ReasoningContent)
				}
				if ch.FinishReason != nil && *ch.FinishReason != "" {
					acc.finish = *ch.FinishReason
				}
				// tool_calls 按 delta.tool_calls[].index 合并分片：id/type/name
				// 首片赋值（空值不覆盖）、arguments 跨片拼接。
				for _, tc := range ch.Delta.ToolCalls {
					idx := 0
					if tc.Index != nil {
						idx = *tc.Index
					}
					tacc, ok := acc.tools[idx]
					if !ok {
						tacc = &aggregatedToolCall{}
						acc.tools[idx] = tacc
						acc.toolOrder = append(acc.toolOrder, idx)
					}
					if tc.ID != "" {
						tacc.ID = tc.ID
					}
					if tc.Type != "" {
						tacc.Type = tc.Type
					}
					if tc.Function.Name != "" {
						tacc.Name = tc.Function.Name
					}
					tacc.Arguments.WriteString(tc.Function.Arguments)
				}
			}
		})

	// ---- 失败语义（fail closed，聚合期间零字节写客户端） ----

	// 中段读错误：scanner 已带出（context 取消类噪声已过滤），原样上抛。
	if st.Err != nil {
		return nil, OpenAIUsage{}, fmt.Errorf("upstream chat stream read error: %w", st.Err)
	}
	// 畸形 data 帧：非流式桥不得容忍，否则产出缺字 200 假完整响应。
	if st.MalformedFrames > 0 {
		return nil, OpenAIUsage{}, fmt.Errorf("upstream chat stream had %d malformed frame(s): %v",
			st.MalformedFrames, st.FirstMalformedErr)
	}
	// 缺 [DONE] 哨兵的干净 EOF：非流式响应声称完整性，不得默认 finish_reason。
	if !st.SawDone {
		return nil, OpenAIUsage{}, fmt.Errorf("upstream chat stream ended without [DONE] sentinel")
	}
	// 缺 usage 帧：零值 Usage 入账=漏计费（资金边界），不以 token 零值代替存在性。
	if !sawUsage {
		return nil, OpenAIUsage{}, fmt.Errorf("upstream chat stream ended without usage frame")
	}
	// 零有效 chunk（含全程 usage-only 帧 + [DONE]）→ 对齐
	// handleCCBufferedFromAnthropic:343 "upstream stream ended without response"。
	if !sawChunk {
		return nil, OpenAIUsage{}, fmt.Errorf("upstream chat stream ended without response")
	}

	// ---- 组装（按 choice.index 升序输出全部 choices） ----

	sort.Ints(accOrder)
	choices := make([]apicompat.ChatChoice, 0, len(accOrder))
	for _, idx := range accOrder {
		acc := accs[idx]
		msg := apicompat.ChatMessage{
			// envelope 常量：非流式契约，不依赖上游 chunk 回显。
			Role: "assistant",
		}
		// 纯工具调用 choice 从不携带 content 增量（contentSeen==false）：
		// content 组装为 JSON null，与原生非流式工具调用响应
		// （message.content: null）一致。收到过任何 content 增量（含显式空串
		// delta.content: ""）都 marshal 为 JSON 字符串，空串增量 → ""（非 null）。
		if !acc.contentSeen {
			msg.Content = json.RawMessage("null")
		} else {
			contentJSON, err := json.Marshal(acc.content.String())
			if err != nil {
				return nil, OpenAIUsage{}, fmt.Errorf("marshal aggregated content: %w", err)
			}
			msg.Content = contentJSON
		}
		if acc.reasoning.Len() > 0 {
			msg.ReasoningContent = acc.reasoning.String()
		}
		if len(acc.toolOrder) > 0 {
			sort.Ints(acc.toolOrder)
			calls := make([]apicompat.ChatToolCall, 0, len(acc.toolOrder))
			for _, ti := range acc.toolOrder {
				t := acc.tools[ti]
				calls = append(calls, apicompat.ChatToolCall{
					// 非流式契约省略 index（流式分片才有）。
					Index: nil,
					ID:    t.ID,
					Type:  t.Type,
					Function: apicompat.ChatFunctionCall{
						Name:      t.Name,
						Arguments: t.Arguments.String(),
					},
				})
			}
			msg.ToolCalls = calls
		}
		// [DONE] 只证传输终止；任一输出 choice 缺非空 finish_reason → 整桥失败。
		if acc.finish == "" {
			return nil, OpenAIUsage{}, fmt.Errorf("upstream chat stream choice %d ended without finish_reason", idx)
		}
		choices = append(choices, apicompat.ChatChoice{
			Index:        idx,
			Message:      msg,
			FinishReason: acc.finish,
		})
	}

	ccResp := &apicompat.ChatCompletionsResponse{
		// envelope 常量：非流式契约 object="chat.completion"。
		Object:  "chat.completion",
		ID:      id,
		Created: created,
		Model:   model,
		Choices: choices,
	}
	if lastUsage != nil {
		ccResp.Usage = lastUsage
	}

	// usage 末帧最新值（scan.Usage）供计费；与 ccResp.Usage 同源。
	return ccResp, st.Usage, nil
}
