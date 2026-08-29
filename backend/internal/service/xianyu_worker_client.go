package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// XianyuWorkerClient 封装主程序到 Worker 的内网调用。
// 所有接口失败必须返回可展示的错误，不得静默成功。
type XianyuWorkerClient struct {
	httpClient *http.Client
	baseURL    string
	apiToken   string
	timeout    time.Duration
}

// NewXianyuWorkerClient 创建 Worker 客户端。
// baseURL 必须是 Docker 主机名或内网地址；timeout <= 0 时使用默认 8s。
func NewXianyuWorkerClient(baseURL, apiToken string, timeout time.Duration) *XianyuWorkerClient {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &XianyuWorkerClient{
		httpClient: &http.Client{Timeout: timeout, Transport: transport},
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiToken:   apiToken,
		timeout:    timeout,
	}
}

const xianyuWorkerRequestTimeout = 8 * time.Second

// XianyuWorkerError 表示 Worker 返回的可展示错误。
type XianyuWorkerError struct {
	StatusCode int
	Reason     string
	Message    string
}

func (e *XianyuWorkerError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return fmt.Sprintf("xianyu worker error %d %s: %s", e.StatusCode, e.Reason, e.Message)
	}
	return fmt.Sprintf("xianyu worker error %d %s", e.StatusCode, e.Reason)
}

// XianyuWorkerHealth 表示 Worker 健康检查结果。
type XianyuWorkerHealth struct {
	Backend   bool `json:"backend"`
	WebSocket bool `json:"websocket"`
	Database  bool `json:"database"`
}

// XianyuWorkerAccountStatus 表示 Worker 侧账号状态。
type XianyuWorkerAccountStatus struct {
	AccountID    string `json:"account_id"`
	Nickname     string `json:"nickname"`
	CookieStatus string `json:"cookie_status"`
	TaskStatus   string `json:"task_status"`
}

// XianyuWorkerLoginSessionStatus 表示扫码会话状态。
type XianyuWorkerLoginSessionStatus struct {
	Status string `json:"status"` // waiting / scanned / success / failed / expired
	QRCode string `json:"qr_code,omitempty"`
}

// XianyuWorkerProduct 表示 Worker 侧在售商品。
type XianyuWorkerProduct struct {
	ItemID    string `json:"item_id"`
	Title     string `json:"title"`
	SpecName  string `json:"spec_name,omitempty"`
	SpecValue string `json:"spec_value,omitempty"`
}

// XianyuWorkerClaims 表示 Worker 侧操作结果。
type XianyuWorkerResult struct {
	Success    bool   `json:"success"`
	Message    string `json:"message,omitempty"`
	SendStatus string `json:"send_status,omitempty"`
	Data       *struct {
		SendStatus string `json:"send_status,omitempty"`
	} `json:"data,omitempty"`
}

// Health 检查后端、WebSocket 和数据库连通性。Worker 可能返回标准包装响应。
func (c *XianyuWorkerClient) Health(ctx context.Context) (*XianyuWorkerHealth, error) {
	var out XianyuWorkerHealth
	var wrapped struct {
		Success bool                `json:"success"`
		Data    *XianyuWorkerHealth `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/health", nil, &wrapped, ""); err != nil {
		return nil, err
	}
	if wrapped.Data != nil {
		out = *wrapped.Data
	}
	out.Backend = out.Backend || wrapped.Success
	return &out, nil
}

// ListAccounts 拉取账号 ID、昵称、Cookie 状态、任务状态。
func (c *XianyuWorkerClient) ListAccounts(ctx context.Context) ([]XianyuWorkerAccountStatus, error) {
	var out []XianyuWorkerAccountStatus
	if err := c.do(ctx, http.MethodGet, "/api/accounts", nil, &out, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateLoginSession 创建扫码会话，返回二维码和状态。
// accountID 可空：Worker 空占位创建新会话。
func (c *XianyuWorkerClient) CreateLoginSession(ctx context.Context, accountID string) (*XianyuWorkerLoginSessionStatus, error) {
	var out XianyuWorkerLoginSessionStatus
	if err := c.do(ctx, http.MethodPost, "/api/login-sessions", map[string]string{}, &out, accountID); err != nil {
		return nil, err
	}
	return &out, nil
}

// QueryLoginSession 查询登录会话状态。
func (c *XianyuWorkerClient) QueryLoginSession(ctx context.Context, accountID string) (*XianyuWorkerLoginSessionStatus, error) {
	var out XianyuWorkerLoginSessionStatus
	if err := c.do(ctx, http.MethodGet, "/api/accounts/"+accountID+"/login-session", nil, &out, accountID); err != nil {
		return nil, err
	}
	return &out, nil
}

// EnableAccount 加载 Cookie 并启动收消息任务。
func (c *XianyuWorkerClient) EnableAccount(ctx context.Context, accountID string) error {
	return c.do(ctx, http.MethodPost, "/api/accounts/"+accountID+"/enable", nil, nil, accountID)
}

// DisableAccount 停止收消息任务。
func (c *XianyuWorkerClient) DisableAccount(ctx context.Context, accountID string) error {
	return c.do(ctx, http.MethodPost, "/api/accounts/"+accountID+"/disable", nil, nil, accountID)
}

// RefreshCookie 触发续期并返回状态。
func (c *XianyuWorkerClient) RefreshCookie(ctx context.Context, accountID string) (*XianyuWorkerAccountStatus, error) {
	var out XianyuWorkerAccountStatus
	if err := c.do(ctx, http.MethodPost, "/api/accounts/"+accountID+"/refresh-cookie", nil, &out, accountID); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListProducts 拉取在售商品、商品 ID、标题、规格。
func (c *XianyuWorkerClient) ListProducts(ctx context.Context, accountID string) ([]XianyuWorkerProduct, error) {
	var out []XianyuWorkerProduct
	if err := c.do(ctx, http.MethodGet, "/api/accounts/"+accountID+"/products", nil, &out, accountID); err != nil {
		return nil, err
	}
	return out, nil
}

// ResendDelivery asks the Worker to resend an original delivery code through the
// buyer's existing chat. It waits for the platform-level send receipt so the
// caller can distinguish a real send from an accepted HTTP request.
func (c *XianyuWorkerClient) ResendDelivery(ctx context.Context, accountID, orderNo, itemID, buyerID, chatID, code string) (*XianyuWorkerResult, error) {
	var out XianyuWorkerResult
	body := map[string]any{
		"order_no":     orderNo,
		"item_id":      itemID,
		"buyer_id":     buyerID,
		"chat_id":      chatID,
		"card_id":      0,
		"content":      code,
		"wait_result":  true,
		"wait_timeout": 10.0,
	}
	if err := c.do(ctx, http.MethodPost, "/internal/accounts/"+accountID+"/send-message", body, &out, accountID); err != nil {
		return nil, err
	}
	return &out, nil
}

// do 执行一次内网调用并统一错误映射。
// expect accountID 用于需要在错误中附加账号上下文的操作。
func (c *XianyuWorkerClient) do(ctx context.Context, method, path string, body any, out any, contextAccountID string) error {
	if c == nil || c.baseURL == "" || c.httpClient == nil {
		return ErrXianyuDeliveryNotConfigured
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal xianyu worker request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build xianyu worker request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiToken != "" {
		req.Header.Set("X-Worker-Token", c.apiToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ErrXianyuWorkerUnhealthy
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read xianyu worker response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.mapError(resp.StatusCode, payload, contextAccountID)
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	// Worker 可能返回标准包装 { code, message, data } 或裸对象。
	if err := json.Unmarshal(payload, out); err != nil {
		var wrapped struct {
			Code int `json:"code"`
			Data any `json:"data"`
		}
		if jsonErr := json.Unmarshal(payload, &wrapped); jsonErr == nil && wrapped.Data != nil {
			raw, _ := json.Marshal(wrapped.Data)
			return json.Unmarshal(raw, out)
		}
		return fmt.Errorf("decode xianyu worker response: %w", err)
	}
	return nil
}

func (c *XianyuWorkerClient) mapError(statusCode int, payload []byte, accountID string) error {
	var wrapped struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	msg := strings.TrimSpace(string(payload))
	if len(payload) > 0 && json.Unmarshal(payload, &wrapped) == nil {
		if wrapped.Message != "" {
			msg = strings.TrimSpace(wrapped.Message)
		}
	}
	reason := wrapped.Reason
	if reason == "" {
		reason = http.StatusText(statusCode)
	}
	if accountID != "" {
		msg = strings.TrimSpace(fmt.Sprintf("account %s: %s", accountID, msg))
	}
	return &XianyuWorkerError{StatusCode: statusCode, Reason: reason, Message: msg}
}

// newXianyuWorkerClientForService 构建 Worker 客户端（默认超时）。
func newXianyuWorkerClientForService(baseURL, token string) *XianyuWorkerClient {
	return NewXianyuWorkerClient(baseURL, token, xianyuWorkerRequestTimeout)
}

// validateWorkerBaseURL 校验 base_url 只允许 Docker 主机名或内网地址。
// Compose 部署下禁止 127.0.0.1（容器内 loopback 指向主程序自身）。
func validateWorkerBaseURL(raw string, forbidLoopback bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ErrXianyuBaseURLInvalid
	}
	u, err := parseWorkerBaseURL(raw)
	if err != nil {
		return ErrXianyuBaseURLInvalid
	}
	if forbidLoopback && isLoopbackHost(u.Hostname()) {
		return ErrXianyuBaseURLLoopbackInvalid
	}
	return nil
}
