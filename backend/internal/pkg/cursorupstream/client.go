package cursorupstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	DefaultBaseURL = "http://82.157.20.83:9098"
	DefaultBaseAPI = "/api/cursorLoginZs"
	DefaultTimeout = 15 * time.Second
)

var (
	ErrAccountQuotaUsedUp = errors.New("cursorupstream: quota used up")
	ErrAccountCooling     = errors.New("cursorupstream: account in cooldown / forced reused")
	ErrDeviceBlacklisted  = errors.New("cursorupstream: device blacklisted")
)

type Client struct {
	httpClient *http.Client
	baseURL    string
	baseAPI    string
	crypto     *Crypto
}

func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		httpClient: &http.Client{Timeout: DefaultTimeout},
		baseURL:    baseURL,
		baseAPI:    DefaultBaseAPI,
		crypto:     NewCrypto(),
	}
}

type CredentialsResponse struct {
	Email             string `json:"email"`
	Token             string `json:"token"`
	Pwd               string `json:"pwd"`
	ForceReused       bool   `json:"force_reused"`
	ForceReuseReason  string `json:"force_reuse_reason"`
}

type CountResponse struct {
	OverCount int `json:"over_count"`
	SumCount  int `json:"sum_count"`
}

// ValidateActivationCode 绑定并校验激活码
func (c *Client) ValidateActivationCode(ctx context.Context, activationCode, deviceCode, deviceMD5 string) error {
	reqBody := map[string]string{
		"activation_code": activationCode,
		"device_code":     deviceCode,
		"device_code_md5": deviceMD5,
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	endpoint := fmt.Sprintf("%s%s/validateActivationCode", c.baseURL, c.baseAPI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json;charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cursorupstream: validate activation request: %w", err)
	}
	defer resp.Body.Close()

	var res struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("cursorupstream: decode response: %w", err)
	}

	codeStr := fmt.Sprintf("%v", res.Code)
	if codeStr != "200" {
		return fmt.Errorf("cursorupstream: validate activation failed (%s): %s", codeStr, res.Msg)
	}
	return nil
}

// GetCredentialsSecure 拉取新 Token (AES 加密传输)
func (c *Client) GetCredentialsSecure(ctx context.Context, deviceCode, deviceMD5 string) (*CredentialsResponse, error) {
	reqMap := map[string]string{
		"device_code":     deviceCode,
		"device_code_md5": deviceMD5,
	}
	reqBytes, err := json.Marshal(reqMap)
	if err != nil {
		return nil, err
	}

	encData, err := c.crypto.Encrypt(reqBytes)
	if err != nil {
		return nil, fmt.Errorf("cursorupstream: encrypt request: %w", err)
	}

	postPayload, _ := json.Marshal(map[string]string{"data": encData})
	endpoint := fmt.Sprintf("%s%s/getCredentialsSecure", c.baseURL, c.baseAPI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(postPayload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json;charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cursorupstream: get credentials request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var res struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return nil, fmt.Errorf("cursorupstream: decode response: %w", err)
	}

	codeStr := fmt.Sprintf("%v", res.Code)
	if codeStr != "200" {
		return nil, fmt.Errorf("cursorupstream: get credentials failed (%s): %s", codeStr, res.Msg)
	}

	decryptedBytes, err := c.crypto.Decrypt(res.Data)
	if err != nil {
		return nil, fmt.Errorf("cursorupstream: decrypt response: %w", err)
	}

	var creds struct {
		Email            string `json:"email"`
		Token            string `json:"token"`
		Pwd              string `json:"pwd"`
		ForceReused      any    `json:"forceReused"`
		ForceReused2     any    `json:"force_reused"`
		ForceReuseReason string `json:"forceReuseReason"`
	}
	if err := json.Unmarshal(decryptedBytes, &creds); err != nil {
		return nil, fmt.Errorf("cursorupstream: parse decrypted credentials: %w", err)
	}

	isReused := false
	if b, ok := creds.ForceReused.(bool); ok && b {
		isReused = true
	} else if b2, ok := creds.ForceReused2.(bool); ok && b2 {
		isReused = true
	}

	return &CredentialsResponse{
		Email:            creds.Email,
		Token:            creds.Token,
		Pwd:              creds.Pwd,
		ForceReused:      isReused,
		ForceReuseReason: creds.ForceReuseReason,
	}, nil
}

// RefreshCount 查询卡密剩余额度与总额度
func (c *Client) RefreshCount(ctx context.Context, deviceCode, deviceMD5 string) (*CountResponse, error) {
	params := url.Values{}
	params.Set("device_code", deviceCode)
	params.Set("device_code_md5", deviceMD5)

	endpoint := fmt.Sprintf("%s%s/refreshCount?%s", c.baseURL, c.baseAPI, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cursorupstream: refresh count request: %w", err)
	}
	defer resp.Body.Close()

	var res struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			OverCount int `json:"over_count"`
			SumCount  int `json:"sum_count"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("cursorupstream: decode count response: %w", err)
	}

	codeStr := fmt.Sprintf("%v", res.Code)
	if codeStr != "200" {
		return nil, fmt.Errorf("cursorupstream: refresh count failed (%s): %s", codeStr, res.Msg)
	}

	return &CountResponse{
		OverCount: res.Data.OverCount,
		SumCount:  res.Data.SumCount,
	}, nil
}
