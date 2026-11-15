package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Devin Connect Protocol 常量。
const (
	DevinAPIURL           = "https://server.codeium.com"
	DevinAuthPath         = "/exa.auth_pb.AuthService/GetUserJwt"
	DevinChatMessagePath  = "/exa.api_server_pb.ApiServerService/GetChatMessage"
	DevinClientName       = "chisel"
	DevinIDEVersion       = "3.2.23"
	DevinExtensionVersion = "1.48.2"

	ConnectCompressedFlag = 0x01
	ConnectEndStreamFlag  = 0x02
	MaxFramePayload       = 16 << 20 // 16 MiB

	DevinDefaultTimeout = 15 * time.Second
)

// DevinUpstreamClient 封装 Devin 鉴权请求。
type DevinUpstreamClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewDevinUpstreamClient() *DevinUpstreamClient {
	return &DevinUpstreamClient{
		BaseURL:    DevinAPIURL,
		HTTPClient: &http.Client{Timeout: DevinDefaultTimeout},
	}
}

// GetUserJwt 用 session token 换取本请求的 user JWT。
// 非 200（含空 JWT）一律失败关闭；响应支持 gzip 兜底解压。
func (c *DevinUpstreamClient) GetUserJwt(ctx context.Context, sessionToken string) (*GetUserJwtResponse, error) {
	meta := &Metadata{
		IDEName:          DevinClientName,
		IDEVersion:       DevinIDEVersion,
		ExtensionName:    DevinClientName,
		ExtensionVersion: DevinExtensionVersion,
		APIKey:           NormalizeDevinToken(sessionToken),
		Locale:           "en",
	}
	reqBody := encodeGetUserJwtRequest(meta)

	url := strings.TrimRight(c.BaseURL, "/") + DevinAuthPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("devin auth create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "*/*")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("devin auth http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("devin auth read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("devin auth %d: %s", resp.StatusCode, string(respBody))
	}

	decoded, err := decodeGetUserJwtResponse(respBody)
	if err != nil || decoded.UserJWT == "" {
		if uncompressed, gzErr := tryGunzipResponse(respBody); gzErr == nil {
			if decoded2, err2 := decodeGetUserJwtResponse(uncompressed); err2 == nil {
				decoded = decoded2
			}
		}
	}
	if decoded == nil || decoded.UserJWT == "" {
		return nil, fmt.Errorf("devin auth: empty user JWT")
	}
	return decoded, nil
}

// DevinChatParams 是构造 GetChatMessage 请求的输入参数（由 devin_convert.go 填充）。
type DevinChatParams struct {
	APIKey       string // 订阅 session token
	ModelUID     string
	SystemPrompt string
	Prompts      []ChatMessagePrompt
	Tools        []ChatToolDefinition
	ToolChoice   *ChatToolChoice
	MaxTokens    *int
	Temperature  *float64
	TopP         *float64
	Stop         []string
	CascadeID    string
}

// NewDevinChatFrame 纯函数：组装 GetChatMessageRequest → protobuf → gzip →
// 5 字节 Connect 帧头（flags=ConnectCompressedFlag）。不联网。
func NewDevinChatFrame(params *DevinChatParams, userJWT, overrideBaseURL string) (frame []byte, chatBaseURL string, err error) {
	base := DevinAPIURL
	if overrideBaseURL != "" {
		base = strings.TrimRight(overrideBaseURL, "/")
	}
	if params == nil {
		return nil, base, fmt.Errorf("devin: nil chat params")
	}
	for _, p := range params.Prompts {
		if len(p.Images) > 0 {
			return nil, base, fmt.Errorf("devin: image input is not supported")
		}
	}

	cascadeID := params.CascadeID
	if cascadeID == "" {
		cascadeID, err = newDevinUUID()
		if err != nil {
			return nil, base, fmt.Errorf("devin cascade id: %w", err)
		}
	}
	executionID, err := newDevinUUID()
	if err != nil {
		return nil, base, fmt.Errorf("devin execution id: %w", err)
	}

	stopPatterns := []string{
		"\n\nUSER:", "\n\nASSISTANT:", "<|context_request|>", "<|end_of_turn|>",
	}
	stopPatterns = append(stopPatterns, params.Stop...)

	maxTokens := 64000
	if params.MaxTokens != nil && *params.MaxTokens > 0 {
		maxTokens = *params.MaxTokens
	}

	temperature := 0.4
	if params.Temperature != nil {
		if *params.Temperature == 0 {
			temperature = 0.01 // 上游拒绝 temperature=0
		} else {
			temperature = *params.Temperature
		}
	}

	topP := 1.0
	if params.TopP != nil {
		topP = *params.TopP
	}

	toolChoice := params.ToolChoice
	if toolChoice == nil {
		toolChoice = &ChatToolChoice{OptionName: "auto"}
	}

	req := &GetChatMessageRequest{
		Metadata: &Metadata{
			IDEName:          DevinClientName,
			IDEVersion:       DevinIDEVersion,
			ExtensionName:    DevinClientName,
			ExtensionVersion: DevinExtensionVersion,
			APIKey:           NormalizeDevinToken(params.APIKey),
			Locale:           "en",
			UserJWT:          userJWT,
		},
		Prompt:             params.SystemPrompt,
		ChatMessagePrompts: params.Prompts,
		ChatModelUID:       params.ModelUID,
		Configuration: &CompletionConfiguration{
			NumCompletions:      1,
			MaxTokens:           uint64(maxTokens),
			MaxNewlines:         200,
			Temperature:         temperature,
			FirstTemperature:    temperature,
			TopK:                50,
			TopP:                topP,
			StopPatterns:        stopPatterns,
			FimEotProbThreshold: 1,
		},
		Tools:                    params.Tools,
		DisableParallelToolCalls: true,
		ToolChoice:               toolChoice,
		CascadeID:                cascadeID,
		ExecutionID:              executionID,
	}

	reqBytes := encodeGetChatMessageRequest(req)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(reqBytes); err != nil {
		return nil, base, fmt.Errorf("devin gzip request: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, base, fmt.Errorf("devin gzip close: %w", err)
	}
	compressed := buf.Bytes()

	frame = make([]byte, 5+len(compressed))
	frame[0] = ConnectCompressedFlag
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(compressed)))
	copy(frame[5:], compressed)
	return frame, base, nil
}

// DevinStreamReader 按 Connect 帧头（1 字节 flags + 4 字节大端长度）从任意 io.Reader 切帧。
// 不使用按换行符切片的读取方式：二进制帧体可能包含换行字节。
type DevinStreamReader struct {
	r       io.Reader
	pending []byte
	buf     [32 * 1024]byte
	eof     bool
}

func NewDevinStreamReader(r io.Reader) *DevinStreamReader {
	return &DevinStreamReader{r: r}
}

// Next 返回下一帧解压后的 payload 与 endStream 标志。
// 帧未读全时继续 Read；len > MaxFramePayload 直接报错；流正常结束返回 io.EOF。
func (r *DevinStreamReader) Next() (payload []byte, endStream bool, err error) {
	for {
		if len(r.pending) >= 5 {
			flags := r.pending[0]
			length := binary.BigEndian.Uint32(r.pending[1:5])
			if length > MaxFramePayload {
				return nil, false, fmt.Errorf("connect frame length %d exceeds %d", length, MaxFramePayload)
			}
			frameLen := 5 + int(length)
			if len(r.pending) >= frameLen {
				payload = make([]byte, int(length))
				copy(payload, r.pending[5:frameLen])
				r.pending = r.pending[frameLen:]

				if flags&ConnectCompressedFlag != 0 {
					decoded, gzErr := tryGunzipResponse(payload)
					if gzErr != nil {
						return nil, false, fmt.Errorf("gunzip frame: %w", gzErr)
					}
					payload = decoded
				}
				return payload, flags&ConnectEndStreamFlag != 0, nil
			}
		}

		// 已到 EOF 仍未切出完整帧：残留数据按半帧处理，否则正常结束。
		if r.eof {
			if len(r.pending) > 0 {
				return nil, false, io.ErrUnexpectedEOF // 残留半帧
			}
			return nil, false, io.EOF
		}

		n, readErr := r.r.Read(r.buf[:])
		if n > 0 {
			r.pending = append(r.pending, r.buf[:n]...)
		}
		if readErr != nil {
			if readErr == io.EOF {
				r.eof = true
				continue // 回到循环顶部，先尝试切帧再处理 EOF
			}
			return nil, false, readErr
		}
	}
}

// newDevinUUID 用 crypto/rand 生成 v4 UUID；rand 不可用时失败关闭。
func newDevinUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("devin uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
