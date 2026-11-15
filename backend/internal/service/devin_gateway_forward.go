package service

import (
	"context"
	"fmt"
)

// Devin 网关转发：OpenAI Chat Completions → Devin GetChatMessage 转换层

// DevinGatewayService 封装 Devin 特定的转发逻辑
type DevinGatewayService struct {
	client *DevinUpstreamClient
}

// NewDevinGatewayService 创建 Devin 网关服务
func NewDevinGatewayService() *DevinGatewayService {
	return &DevinGatewayService{
		client: NewDevinUpstreamClient(),
	}
}

// ForwardChatCompletion 转发 OpenAI Chat Completions 请求到 Devin
func (s *DevinGatewayService) ForwardChatCompletion(ctx context.Context, account *Account, reqBody map[string]interface{}) (map[string]interface{}, error) {
	if !account.IsDevin() {
		return nil, fmt.Errorf("not a devin account")
	}

	token := account.GetDevinSessionToken()
	if token == "" {
		return nil, fmt.Errorf("devin session token not found")
	}

	// 提取请求参数
	model, _ := reqBody["model"].(string)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	// 转换消息
	messages := []ChatMessagePrompt{}
	systemPrompt := ""

	if rawMessages, ok := reqBody["messages"].([]interface{}); ok {
		for _, msg := range rawMessages {
			if msgMap, ok := msg.(map[string]interface{}); ok {
				role, _ := msgMap["role"].(string)
				content, _ := msgMap["content"].(string)

				if role == "system" || role == "developer" {
					systemPrompt += content + "\n\n"
				} else {
					messages = append(messages, ChatMessagePrompt{
						Role:    role,
						Content: content,
					})
				}
			}
		}
	}

	// 转换工具定义（简化）
	tools := []ChatToolDefinition{}
	// TODO: 完整实现工具转换

	// 构造请求参数
	params := &StreamChatParams{
		APIKey:       token,
		ModelUID:     model,
		SystemPrompt: systemPrompt,
		Messages:     messages,
		Tools:        tools,
	}

	// 提取可选参数
	if maxTokens, ok := reqBody["max_tokens"].(float64); ok {
		params.MaxTokens = int(maxTokens)
	}
	if temperature, ok := reqBody["temperature"].(float64); ok {
		params.Temperature = temperature
	}
	if topP, ok := reqBody["top_p"].(float64); ok {
		params.TopP = topP
	}
	if stop, ok := reqBody["stop"].([]interface{}); ok {
		for _, s := range stop {
			if str, ok := s.(string); ok {
				params.StopSequences = append(params.StopSequences, str)
			}
		}
	}

	// 调用 Devin API
	resp, err := s.client.StreamChat(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("devin api call failed: %w", err)
	}

	// 转换为 OpenAI 响应格式
	result := map[string]interface{}{
		"id":      "chatcmpl-devin-" + generateUUID(),
		"object":  "chat.completion",
		"created": 0, // TODO: 实际时间戳
		"model":   model,
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": resp.ChatMessage,
				},
				"finish_reason": convertStopReason(resp.StopReason),
			},
		},
	}

	// 添加用量信息
	if resp.Usage != nil {
		result["usage"] = map[string]interface{}{
			"prompt_tokens":     resp.Usage.InputTokens,
			"completion_tokens": resp.Usage.OutputTokens,
			"total_tokens":      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
	}

	// 添加工具调用（如果有）
	if len(resp.ToolCalls) > 0 {
		toolCalls := []interface{}{}
		for _, tc := range resp.ToolCalls {
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      tc.Name,
					"arguments": tc.ArgumentsJSON,
				},
			})
		}
		if choice, ok := result["choices"].([]interface{}); ok && len(choice) > 0 {
			if choiceMap, ok := choice[0].(map[string]interface{}); ok {
				if message, ok := choiceMap["message"].(map[string]interface{}); ok {
					message["tool_calls"] = toolCalls
					choiceMap["finish_reason"] = "tool_calls"
				}
			}
		}
	}

	return result, nil
}

// convertStopReason 转换 Devin stop reason 到 OpenAI finish_reason
func convertStopReason(reason int32) string {
	switch reason {
	case 0:
		return "stop"
	case 1:
		return "length"
	default:
		return "stop"
	}
}

// TestDevinConnection 测试 Devin 账号连接
func (s *DevinGatewayService) TestDevinConnection(ctx context.Context, token string) error {
	_, err := s.client.GetUserJwt(ctx, token)
	return err
}
