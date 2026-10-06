package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LocalCaptchaHelperConfig configures the private local_captcha_helper HTTP API.
type LocalCaptchaHelperConfig struct {
	BaseURL string `mapstructure:"base_url"`
	APIKey  string `mapstructure:"api_key"`
	Timeout int    `mapstructure:"timeout_seconds"`
}

// LocalCaptchaHelper is the narrow helper contract used by the web-login challenge handler.
type LocalCaptchaHelper interface {
	// Start 创建 helper 挑战会话。sign 携带 sdk-start body 下发的签名三件套参数
	// （Lane G，任务卡 2026-09-22：zhipu 发码由 helper 在同会话内执行，签名三件套由
	// Go 侧 webZhipuComputeSign 生成下发；kimi 传零值，body 不含对应键）。
	Start(context.Context, string, string, string, string, LocalCaptchaHelperSignParams) (LocalCaptchaHelperSession, error)
	Status(context.Context, LocalCaptchaHelperSession, string, string, string) (string, error)
	Result(context.Context, LocalCaptchaHelperSession, string, string, string) (LocalCaptchaHelperResult, error)
}

// LocalCaptchaHelperSignParams sdk-start body 下发的签名三件套（键 x_timestamp /
// x_nonce / x_sign，与 helper 协议既有 snake_case 键风格一致）。零值 = 不下发
// （kimi 流程零改动；旧版 helper 忽略未知键，向后兼容）。值仅进 sdk-start 请求体，
// 绝不写日志（零凭据红线：nonce/sign 为一次性请求签名，不下发即作废）。
type LocalCaptchaHelperSignParams struct {
	XTimestamp string `json:"x_timestamp,omitempty"`
	XNonce     string `json:"x_nonce,omitempty"`
	XSign      string `json:"x_sign,omitempty"`
}

type LocalCaptchaHelperSession struct {
	ID             string
	LoginSessionID string
}

type LocalCaptchaHelperResult struct {
	Status string
	Data   map[string]string
}

type localCaptchaHelperStartRequest struct {
	Platform       string `json:"platform"`
	LoginSessionID string `json:"login_session_id"`
	Phone          string `json:"phone"`
	PhoneCode      string `json:"phone_code,omitempty"`
	Timeout        int    `json:"timeout,omitempty"`
	// 签名三件套（Lane G 下发，zhipu 同会话发码用；零值省键，kimi body 不变）。
	XTimestamp string `json:"x_timestamp,omitempty"`
	XNonce     string `json:"x_nonce,omitempty"`
	XSign      string `json:"x_sign,omitempty"`
}

type localCaptchaHelperStartResponse struct {
	Success   bool   `json:"success"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Message   string `json:"message"`
}

type localCaptchaHelperStatusResponse struct {
	Success bool   `json:"success"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type localCaptchaHelperResultResponse struct {
	Success bool              `json:"success"`
	Status  string            `json:"status"`
	Message string            `json:"message"`
	Data    map[string]string `json:"data"`
}

type LocalCaptchaHelperHTTPClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewLocalCaptchaHelperHTTPClient(cfg LocalCaptchaHelperConfig) *LocalCaptchaHelperHTTPClient {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &LocalCaptchaHelperHTTPClient{baseURL: base, apiKey: strings.TrimSpace(cfg.APIKey), http: &http.Client{Timeout: timeout}}
}

func (c *LocalCaptchaHelperHTTPClient) Start(ctx context.Context, platform, loginSessionID, phone, phoneCode string, sign LocalCaptchaHelperSignParams) (LocalCaptchaHelperSession, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	loginSessionID = strings.TrimSpace(loginSessionID)
	phone = strings.TrimSpace(phone)
	phoneCode = strings.TrimSpace(phoneCode)
	if platform != PlatformKimi && platform != PlatformZhipu {
		return LocalCaptchaHelperSession{}, errors.New("unsupported challenge platform")
	}
	if loginSessionID == "" || phone == "" {
		return LocalCaptchaHelperSession{}, errors.New("challenge binding is incomplete")
	}
	if platform == PlatformZhipu && phoneCode == "" {
		return LocalCaptchaHelperSession{}, errors.New("GLM challenge requires phone_code")
	}
	helperPlatform := platform
	if platform == PlatformZhipu {
		helperPlatform = "glm"
	}
	var out localCaptchaHelperStartResponse
	err := c.doJSON(ctx, http.MethodPost, "/challenge/sdk-start", localCaptchaHelperStartRequest{
		Platform: helperPlatform, LoginSessionID: loginSessionID, Phone: phone, PhoneCode: phoneCode,
		Timeout: 180,
		// 签名三件套下发（Lane G）：零值 omitempty 省键（kimi 零改动，旧 helper 兼容）。
		XTimestamp: sign.XTimestamp, XNonce: sign.XNonce, XSign: sign.XSign,
	}, &out)
	if err != nil {
		return LocalCaptchaHelperSession{}, err
	}
	if !out.Success || strings.TrimSpace(out.SessionID) == "" {
		return LocalCaptchaHelperSession{}, errors.New("helper start missing session_id")
	}
	return LocalCaptchaHelperSession{ID: strings.TrimSpace(out.SessionID), LoginSessionID: loginSessionID}, nil
}

func (c *LocalCaptchaHelperHTTPClient) Status(ctx context.Context, session LocalCaptchaHelperSession, platform, loginSessionID, phone string) (string, error) {
	loginSessionID = strings.TrimSpace(loginSessionID)
	if loginSessionID == "" {
		loginSessionID = strings.TrimSpace(session.LoginSessionID)
	}
	if strings.TrimSpace(session.ID) == "" || loginSessionID == "" || strings.TrimSpace(phone) == "" {
		return "", errors.New("challenge binding is incomplete")
	}
	path := "/challenge/" + url.PathEscape(strings.TrimSpace(session.ID)) + "/status?platform=" + url.QueryEscape(helperPlatform(platform)) + "&login_session_id=" + url.QueryEscape(loginSessionID) + "&phone=" + url.QueryEscape(strings.TrimSpace(phone))
	var out localCaptchaHelperStatusResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return "", err
	}
	if !out.Success {
		return "", errors.New("helper status failed")
	}
	status := strings.ToLower(strings.TrimSpace(out.Status))
	if status == "" {
		return "", errors.New("helper status missing status")
	}
	return status, nil
}

func (c *LocalCaptchaHelperHTTPClient) Result(ctx context.Context, session LocalCaptchaHelperSession, platform, loginSessionID, phone string) (LocalCaptchaHelperResult, error) {
	loginSessionID = strings.TrimSpace(loginSessionID)
	if loginSessionID == "" {
		loginSessionID = strings.TrimSpace(session.LoginSessionID)
	}
	if strings.TrimSpace(session.ID) == "" || loginSessionID == "" || strings.TrimSpace(phone) == "" {
		return LocalCaptchaHelperResult{}, errors.New("challenge binding is incomplete")
	}
	body := map[string]string{
		"platform": helperPlatform(platform), "login_session_id": loginSessionID, "phone": strings.TrimSpace(phone),
	}
	var out localCaptchaHelperResultResponse
	if err := c.doJSON(ctx, http.MethodPost, "/challenge/"+url.PathEscape(session.ID)+"/result", body, &out); err != nil {
		return LocalCaptchaHelperResult{}, err
	}
	if !out.Success {
		return LocalCaptchaHelperResult{}, errors.New("helper result failed")
	}
	status := strings.ToLower(strings.TrimSpace(out.Status))
	if status == "" {
		return LocalCaptchaHelperResult{}, errors.New("helper result missing status")
	}
	if len(out.Data) == 0 {
		return LocalCaptchaHelperResult{}, errors.New("helper result missing data")
	}
	return LocalCaptchaHelperResult{Status: status, Data: out.Data}, nil
}

func helperPlatform(platform string) string {
	if strings.EqualFold(strings.TrimSpace(platform), PlatformZhipu) {
		return "glm"
	}
	return strings.ToLower(strings.TrimSpace(platform))
}

func (c *LocalCaptchaHelperHTTPClient) doJSON(ctx context.Context, method, path string, body any, out any) error {
	if c == nil || c.http == nil {
		return errors.New("local captcha helper is not configured")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("helper HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256*1024)).Decode(out); err != nil {
		return fmt.Errorf("decode helper response: %w", err)
	}
	return nil
}
