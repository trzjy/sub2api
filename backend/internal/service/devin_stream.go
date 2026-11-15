package service

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Devin Connect Protocol 常量
const (
	ConnectCompressedFlag  = 0x01
	ConnectEndStreamFlag   = 0x02
	MaxFramePayload        = 16 * 1024 * 1024
	DefaultIdleTimeoutMs   = 120000 // 2 分钟
	AuthTimeoutMs          = 30000  // 30 秒
)

// ConnectFrame 是 Connect Protocol 的 frame 结构
type ConnectFrame struct {
	Flags   byte
	Payload []byte
}

// encodeConnectFrame 编码一个 Connect frame
func encodeConnectFrame(payload []byte, compressed bool) []byte {
	var flags byte
	actualPayload := payload

	if compressed {
		flags |= ConnectCompressedFlag
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		gw.Write(payload)
		gw.Close()
		actualPayload = buf.Bytes()
	}

	frame := make([]byte, 5+len(actualPayload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(actualPayload)))
	copy(frame[5:], actualPayload)

	return frame
}

// decodeConnectFrame 解码 Connect frame
func decodeConnectFrame(data []byte) (*ConnectFrame, int, error) {
	if len(data) < 5 {
		return nil, 0, fmt.Errorf("frame too short: %d bytes", len(data))
	}

	flags := data[0]
	length := binary.BigEndian.Uint32(data[1:5])

	if length > MaxFramePayload {
		return nil, 0, fmt.Errorf("frame payload too large: %d bytes", length)
	}

	frameSize := 5 + int(length)
	if len(data) < frameSize {
		return nil, 0, io.ErrUnexpectedEOF
	}

	payload := data[5:frameSize]

	// 解压缩
	if flags&ConnectCompressedFlag != 0 {
		gr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, 0, fmt.Errorf("gzip decompress: %w", err)
		}
		defer gr.Close()

		uncompressed, err := io.ReadAll(gr)
		if err != nil {
			return nil, 0, fmt.Errorf("gzip read: %w", err)
		}
		payload = uncompressed
	}

	return &ConnectFrame{
		Flags:   flags,
		Payload: payload,
	}, frameSize, nil
}

// ChatMessagePrompt 是 Devin 的消息结构（简化版）
type ChatMessagePrompt struct {
	Role    string
	Content string
}

// ChatToolDefinition 是 Devin 的工具定义（简化版）
type ChatToolDefinition struct {
	Name        string
	Description string
	Parameters  string // JSON schema
}

// ChatToolChoice 是 Devin 的工具选择策略
type ChatToolChoice struct {
	OptionName string // "auto", "none", "required"
}

// CompletionConfiguration 是 Devin 的生成配置
type CompletionConfiguration struct {
	NumCompletions        uint64
	MaxTokens             uint64
	MaxNewlines           uint64
	Temperature           float64
	FirstTemperature      float64
	TopK                  uint64
	TopP                  float64
	StopPatterns          []string
	FimEotProbThreshold   float64
}

// GetChatMessageRequest 是 Devin GetChatMessage 请求
type GetChatMessageRequest struct {
	Metadata                *DevinMetadata
	Prompt                  string // system prompt
	ChatMessagePrompts      []ChatMessagePrompt
	ChatModelUID            string
	Configuration           *CompletionConfiguration
	Tools                   []ChatToolDefinition
	DisableParallelToolCalls bool
	ToolChoice              *ChatToolChoice
	CascadeID               string
	ExecutionID             string
}

// GetChatMessageResponse 是 Devin GetChatMessage 响应
type GetChatMessageResponse struct {
	ChatMessage   string
	Thinking      string
	StopReason    int32
	ToolCalls     []ChatToolCall
	Usage         *ChatUsage
}

// ChatToolCall 是工具调用
type ChatToolCall struct {
	ID           string
	Name         string
	ArgumentsJSON string
}

// ChatUsage 是用量统计
type ChatUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// ChatStreamEvent 是流式事件
type ChatStreamEvent struct {
	Type           string // "text", "thinking", "toolcall", "usage", "done", "error"
	DeltaText      string
	DeltaThinking  string
	ToolCalls      []ChatToolCallDelta
	StopReason     int32
	Usage          *ChatUsage
	Error          string
	Code           string
}

// ChatToolCallDelta 是工具调用增量
type ChatToolCallDelta struct {
	Index         int
	ID            string
	Name          string
	ArgumentsJSON string
}

// encodeGetChatMessageRequest 编码 GetChatMessage 请求（简化版）
func encodeGetChatMessageRequest(req *GetChatMessageRequest) []byte {
	e := &ProtoEncoder{}

	// Field 1: metadata
	e.Message(1, func(sub *ProtoEncoder) {
		encodeDevinMetadata(sub, req.Metadata)
	})

	// Field 2: prompt (system)
	e.String(2, req.Prompt)

	// Field 3: chatMessagePrompts
	for _, msg := range req.ChatMessagePrompts {
		e.Message(3, func(sub *ProtoEncoder) {
			sub.String(1, msg.Role)
			sub.String(2, msg.Content)
		})
	}

	// Field 4: chatModelUid
	e.String(4, req.ChatModelUID)

	// Field 5: configuration
	if req.Configuration != nil {
		e.Message(5, func(sub *ProtoEncoder) {
			sub.Uint64(1, req.Configuration.NumCompletions)
			sub.Uint64(2, req.Configuration.MaxTokens)
			sub.Uint64(3, req.Configuration.MaxNewlines)
			sub.Double(4, req.Configuration.Temperature)
			sub.Double(5, req.Configuration.FirstTemperature)
			sub.Uint64(6, req.Configuration.TopK)
			sub.Double(7, req.Configuration.TopP)
			sub.RepeatedString(8, req.Configuration.StopPatterns)
			sub.Double(9, req.Configuration.FimEotProbThreshold)
		})
	}

	// Field 10: tools
	for _, tool := range req.Tools {
		e.Message(10, func(sub *ProtoEncoder) {
			sub.String(1, tool.Name)
			sub.String(2, tool.Description)
			sub.String(3, tool.Parameters)
		})
	}

	// Field 11: disableParallelToolCalls
	e.Bool(11, req.DisableParallelToolCalls)

	// Field 12: toolChoice
	if req.ToolChoice != nil {
		e.Message(12, func(sub *ProtoEncoder) {
			sub.String(1, req.ToolChoice.OptionName)
		})
	}

	// Field 20: cascadeId
	e.String(20, req.CascadeID)

	// Field 21: executionId
	e.String(21, req.ExecutionID)

	return e.Bytes()
}

// decodeGetChatMessageResponse 解码 GetChatMessage 响应（简化版）
func decodeGetChatMessageResponse(data []byte) (*GetChatMessageResponse, error) {
	d := NewProtoDecoder(data)
	res := &GetChatMessageResponse{}

	for !d.Done() {
		field, wire := d.ReadTag()
		switch field {
		case 1: // chatMessage
			if wire == 2 {
				res.ChatMessage = d.ReadString()
			} else {
				d.Skip(wire)
			}
		case 2: // thinking
			if wire == 2 {
				res.Thinking = d.ReadString()
			} else {
				d.Skip(wire)
			}
		case 3: // stopReason
			if wire == 0 {
				res.StopReason = int32(d.ReadUint32())
			} else {
				d.Skip(wire)
			}
		case 4: // toolCalls (repeated message)
			if wire == 2 {
				toolCallData := d.ReadBytes()
				tc := decodeToolCall(toolCallData)
				res.ToolCalls = append(res.ToolCalls, tc)
			} else {
				d.Skip(wire)
			}
		case 5: // usage
			if wire == 2 {
				usageData := d.ReadBytes()
				res.Usage = decodeUsage(usageData)
			} else {
				d.Skip(wire)
			}
		default:
			d.Skip(wire)
		}
	}

	return res, nil
}

func decodeToolCall(data []byte) ChatToolCall {
	d := NewProtoDecoder(data)
	tc := ChatToolCall{}

	for !d.Done() {
		field, wire := d.ReadTag()
		switch field {
		case 1:
			tc.ID = d.ReadString()
		case 2:
			tc.Name = d.ReadString()
		case 3:
			tc.ArgumentsJSON = d.ReadString()
		default:
			d.Skip(wire)
		}
	}

	return tc
}

func decodeUsage(data []byte) *ChatUsage {
	d := NewProtoDecoder(data)
	usage := &ChatUsage{}

	for !d.Done() {
		field, wire := d.ReadTag()
		switch field {
		case 1:
			usage.InputTokens = int64(d.ReadUint64())
		case 2:
			usage.OutputTokens = int64(d.ReadUint64())
		case 3:
			usage.CacheReadTokens = int64(d.ReadUint64())
		case 4:
			usage.CacheWriteTokens = int64(d.ReadUint64())
		default:
			d.Skip(wire)
		}
	}

	return usage
}

// StreamChatParams 是流式对话参数
type StreamChatParams struct {
	APIKey                string
	ModelUID              string
	SystemPrompt          string
	Messages              []ChatMessagePrompt
	Tools                 []ChatToolDefinition
	MaxTokens             int
	Temperature           float64
	TopP                  float64
	StopSequences         []string
	CascadeID             string
	BaseURL               string
	UpstreamIdleTimeoutMs int
	ToolChoice            *ChatToolChoice
}

// StreamChat 流式对话（简化实现，返回完整响应而非真正流式）
func (c *DevinUpstreamClient) StreamChat(ctx context.Context, params *StreamChatParams) (*GetChatMessageResponse, error) {
	// 1. 获取 JWT
	authCtx, authCancel := context.WithTimeout(ctx, time.Duration(AuthTimeoutMs)*time.Millisecond)
	defer authCancel()

	auth, err := c.GetUserJwt(authCtx, params.APIKey)
	if err != nil {
		return nil, fmt.Errorf("auth failed: %w", err)
	}

	chatBaseURL := c.BaseURL
	if auth.CustomAPIServerURL != "" {
		chatBaseURL = auth.CustomAPIServerURL
	}
	if params.BaseURL != "" {
		chatBaseURL = params.BaseURL
	}

	// 2. 构造请求
	cascadeID := params.CascadeID
	if cascadeID == "" {
		cascadeID = generateUUID()
	}

	defaultStopPatterns := []string{"\n\nUSER:", "\n\nASSISTANT:", "<|context_request|>", "<|end_of_turn|>"}
	stopPatterns := append(defaultStopPatterns, params.StopSequences...)

	maxTokens := params.MaxTokens
	if maxTokens == 0 {
		maxTokens = 64000
	}

	temperature := params.Temperature
	if temperature == 0 {
		temperature = 0.01 // Devin 不接受 0
	}

	configuration := &CompletionConfiguration{
		NumCompletions:      1,
		MaxTokens:           uint64(maxTokens),
		MaxNewlines:         200,
		Temperature:         temperature,
		FirstTemperature:    temperature,
		TopK:                50,
		TopP:                params.TopP,
		StopPatterns:        stopPatterns,
		FimEotProbThreshold: 1,
	}

	if configuration.TopP == 0 {
		configuration.TopP = 1
	}

	toolChoice := params.ToolChoice
	if toolChoice == nil {
		toolChoice = &ChatToolChoice{OptionName: "auto"}
	}

	request := &GetChatMessageRequest{
		Metadata:                 &DevinMetadata{
			IDEName:          DevinClientName,
			IDEVersion:       DevinIDEVersion,
			ExtensionName:    DevinClientName,
			ExtensionVersion: DevinExtensionVersion,
			APIKey:           NormalizeDevinToken(params.APIKey),
			Locale:           "en",
			UserJWT:          auth.UserJWT,
		},
		Prompt:                   params.SystemPrompt,
		ChatMessagePrompts:       params.Messages,
		ChatModelUID:             params.ModelUID,
		Configuration:            configuration,
		Tools:                    params.Tools,
		DisableParallelToolCalls: true,
		ToolChoice:               toolChoice,
		CascadeID:                cascadeID,
		ExecutionID:              generateUUID(),
	}

	// 3. 编码并压缩请求
	reqBytes := encodeGetChatMessageRequest(request)
	frame := encodeConnectFrame(reqBytes, true)

	// 4. 发送请求
	url := chatBaseURL + DevinChatMessagePath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(frame))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Connect-Content-Encoding", "gzip")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "connect-go/1.18.1 (go1.26.3)")
	req.Header.Set("Connect-Accept-Encoding", "gzip")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("devin api %d: %s", resp.StatusCode, string(body))
	}

	// 5. 读取流式响应
	result := &GetChatMessageResponse{}
	reader := bufio.NewReader(resp.Body)
	pending := []byte{}

	for {
		chunk, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("read stream: %w", err)
		}

		if len(chunk) > 0 {
			pending = append(pending, chunk...)
		}

		// 尝试解码 frame
		for len(pending) >= 5 {
			frame, consumed, err := decodeConnectFrame(pending)
			if err == io.ErrUnexpectedEOF {
				break // 需要更多数据
			}
			if err != nil {
				return nil, fmt.Errorf("decode frame: %w", err)
			}

			pending = pending[consumed:]

			// 解码消息
			msg, err := decodeGetChatMessageResponse(frame.Payload)
			if err != nil {
				return nil, fmt.Errorf("decode message: %w", err)
			}

			// 累积结果
			result.ChatMessage += msg.ChatMessage
			result.Thinking += msg.Thinking
			result.StopReason = msg.StopReason
			result.ToolCalls = append(result.ToolCalls, msg.ToolCalls...)
			if msg.Usage != nil {
				result.Usage = msg.Usage
			}

			// END_STREAM flag
			if frame.Flags&ConnectEndStreamFlag != 0 {
				return result, nil
			}
		}

		if err == io.EOF {
			break
		}
	}

	return result, nil
}

// generateUUID 生成简单的 UUID（生产环境应使用 google/uuid）
func generateUUID() string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		time.Now().UnixNano()&0xffffffff,
		(time.Now().UnixNano()>>32)&0xffff,
		0x4000|(time.Now().UnixNano()&0x0fff),
		0x8000|(time.Now().UnixNano()&0x3fff),
		time.Now().UnixNano()&0xffffffffffff)
}
