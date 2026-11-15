package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Devin / Codeium Cascade API 客户端常量
const (
	DevinAPIURL          = "https://server.codeium.com"
	DevinAuthPath        = "/exa.auth_pb.AuthService/GetUserJwt"
	DevinChatMessagePath = "/exa.api_server_pb.ApiServerService/GetChatMessage"
	DevinClientName      = "chisel"
	DevinIDEVersion      = "3.2.23"
	DevinExtensionVersion = "1.48.2"
	DevinDefaultTimeout  = 15 * time.Second
)

// DevinUpstreamClient 封装 Devin API 调用
type DevinUpstreamClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewDevinUpstreamClient 创建 Devin 上游客户端
func NewDevinUpstreamClient() *DevinUpstreamClient {
	return &DevinUpstreamClient{
		BaseURL:    DevinAPIURL,
		HTTPClient: &http.Client{Timeout: DevinDefaultTimeout},
	}
}

// GetUserJwt 用 session token 换取 JWT
func (c *DevinUpstreamClient) GetUserJwt(ctx context.Context, sessionToken string) (*GetUserJwtResponse, error) {
	token := NormalizeDevinToken(sessionToken)

	// 构造请求
	meta := &DevinMetadata{
		IDEName:          DevinClientName,
		IDEVersion:       DevinIDEVersion,
		ExtensionName:    DevinClientName,
		ExtensionVersion: DevinExtensionVersion,
		APIKey:           token,
		Locale:           "en",
	}
	reqBody := encodeGetUserJwtRequest(meta)

	url := c.BaseURL + DevinAuthPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "*/*")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("devin auth %d: %s", resp.StatusCode, string(respBody))
	}

	// 尝试直接解码
	decoded, err := decodeGetUserJwtResponse(respBody)
	if err != nil {
		// 尝试 gzip 解压
		uncompressed, gzErr := tryGunzipResponse(respBody)
		if gzErr != nil {
			return nil, fmt.Errorf("decode failed and not gzip: %w", err)
		}
		decoded, err = decodeGetUserJwtResponse(uncompressed)
		if err != nil {
			return nil, fmt.Errorf("decode uncompressed: %w", err)
		}
	}

	return decoded, nil
}
