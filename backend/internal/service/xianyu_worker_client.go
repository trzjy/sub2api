package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// baseURL 必须是 Docker 主机名或内网地址；timeout <= 0 时使用默认 15s。
// 默认 15s 必须严格大于补发链路 Worker 回执等待窗口（wait_timeout=10s）+ 转发余量，
// 否则主程序会在 Worker 等待平台回执期间取消请求，阻断同步与异步两条关闭路径。
func NewXianyuWorkerClient(baseURL, apiToken string, timeout time.Duration) *XianyuWorkerClient {
	if timeout <= 0 {
		timeout = 15 * time.Second
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

// 必须 > 补发 wait_timeout(10s) + 转发余量，避免同步取消阻断异步兜底回执。
const xianyuWorkerRequestTimeout = 15 * time.Second

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

// xianyuHealth accepts both boolean and Worker's string database status.
type xianyuHealth bool

func (v *xianyuHealth) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*v = false
		return nil
	}
	if text[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		*v = xianyuHealth(strings.EqualFold(strings.TrimSpace(value), "connected"))
		return nil
	}
	var value bool
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*v = xianyuHealth(value)
	return nil
}

type xianyuWorkerHealthStatus struct {
	Backend   bool         `json:"backend"`
	WebSocket bool         `json:"websocket"`
	Database  xianyuHealth `json:"database"`
}

// XianyuWorkerHealth 表示 Worker 健康检查结果。
type XianyuWorkerHealth struct {
	Backend   bool
	WebSocket bool
	Database  bool
}

// XianyuWorkerRenewResult 表示 Worker renew-login 批量续期响应投影（internal_api /cookies/renew-login）。
type XianyuWorkerRenewResult struct {
	Results []struct {
		AccountID string `json:"account_id"`
		Success   bool   `json:"success"`
		Message   string `json:"message,omitempty"`
	} `json:"results"`
	SuccessCount int `json:"success_count"`
	FailedCount  int `json:"failed_count"`
}

// XianyuWorkerAccountStatus 表示 Worker 侧账号状态（internal_api /cookies/details 投影）。
type XianyuWorkerAccountStatus struct {
	AccountID     string `json:"account_id"`
	Nickname      string `json:"nickname"`
	Enabled       bool   `json:"enabled"`
	Status        string `json:"status"`
	LastLoginAt   string `json:"last_login_at,omitempty"`
	LastRefreshAt string `json:"last_refresh_at,omitempty"`
}

// XianyuWorkerLoginSessionStatus 表示扫码会话状态（internal_api /qr-login 投影）。
type XianyuWorkerLoginSessionStatus struct {
	SessionID       string `json:"session_id,omitempty"`
	Status          string `json:"status"` // waiting / scanned / success / failed / expired
	QRCodeURL       string `json:"qr_code_url,omitempty"`
	QRCode          string `json:"qr_code,omitempty"`
	AccountID       string `json:"account_id,omitempty"`
	IsNew           bool   `json:"is_new_account,omitempty"`
	Message         string `json:"message,omitempty"`
	VerificationURL string `json:"verification_url,omitempty"`
}

// UnmarshalJSON 兼容 Worker internal_api 返回的 qr_code_url 与前端期望的 qr_code。
func (s *XianyuWorkerLoginSessionStatus) UnmarshalJSON(data []byte) error {
	var raw struct {
		SessionID       string `json:"session_id"`
		Status          string `json:"status"`
		QRCodeURL       string `json:"qr_code_url"`
		QRCode          string `json:"qr_code"`
		AccountID       string `json:"account_id"`
		IsNew           bool   `json:"is_new_account"`
		Message         string `json:"message"`
		VerificationURL string `json:"verification_url"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.SessionID = raw.SessionID
	s.Status = raw.Status
	s.QRCodeURL = raw.QRCodeURL
	s.QRCode = raw.QRCode
	s.AccountID = raw.AccountID
	s.IsNew = raw.IsNew
	s.Message = raw.Message
	s.VerificationURL = raw.VerificationURL
	return nil
}

// MarshalJSON 同时输出 qr_code 与 qr_code_url，满足前端（qr_code）与内网契约测试（qr_code_url）。
// qr_code_url 非空时优先；仅旧字段 qr_code 可用时回退。
func (s XianyuWorkerLoginSessionStatus) MarshalJSON() ([]byte, error) {
	type alias XianyuWorkerLoginSessionStatus
	qrCode := s.QRCodeURL
	if qrCode == "" {
		qrCode = s.QRCode
	}
	return json.Marshal(struct {
		alias
		QRCode string `json:"qr_code"`
	}{
		alias:  alias(s),
		QRCode: qrCode,
	})
}

// XianyuWorkerProduct 表示 Worker 侧在售商品（internal_api /items/cookie 投影）。
type XianyuWorkerProduct struct {
	ItemID    string `json:"item_id"`
	Title     string `json:"title"`
	SpecName  string `json:"spec_name,omitempty"`
	SpecValue string `json:"spec_value,omitempty"`
}

// XianyuWorkerSendResult 是 /messages/send 响应的业务级回执载荷（信封 data 内）。
// 统一回执语义由 Receipt 承载；send_status/dispatched 为过渡兼容字段（新 Worker 同时输出，
// 旧 Worker 回退用，normalizeSendReceipt 负责归一化）。
type XianyuWorkerSendResult struct {
	// Receipt 统一回执枚举：dispatched_definite_failure / sent_explicit_success / rejected / unknown_pending。
	Receipt        string `json:"receipt"`
	SendFailReason string `json:"send_fail_reason,omitempty"`
	SendStatus     string `json:"send_status,omitempty"`
	Dispatched     *bool  `json:"dispatched,omitempty"`
}

// normalizeSendReceipt 从发送结果归一化为统一回执枚举（fail-closed：不可识别一律 unknown_pending）。
// 优先 receipt；缺失时回退旧 send_status/dispatched 映射。
func normalizeSendReceipt(r *XianyuWorkerSendResult) (receipt string, reason string) {
	if r == nil {
		return "unknown_pending", ""
	}
	if r.Receipt != "" {
		return r.Receipt, r.SendFailReason
	}
	dispatched := true
	if r.Dispatched != nil {
		dispatched = *r.Dispatched
	}
	if !dispatched {
		return "dispatched_definite_failure", r.SendFailReason
	}
	switch r.SendStatus {
	case "success":
		return "sent_explicit_success", ""
	case "failed":
		return "rejected", r.SendFailReason
	default:
		return "unknown_pending", r.SendFailReason
	}
}

// Health 检查 backend 健康状态。
// backend /health 返回 { success, data: { service, version, status, database } }；
// backend 存活即视为 Backend 健康，WebSocket 状态由 backend 内部巡检承载。
func (c *XianyuWorkerClient) Health(ctx context.Context) (*XianyuWorkerHealth, error) {
	payload, err := c.doRequest(ctx, http.MethodGet, "/health", nil, "")
	if err != nil {
		// 对外保持 Unhealthy 语义（传输错误细分供补发链路判定用，Health 统一映射回 Unhealthy）。
		if errors.Is(err, ErrXianyuWorkerUnreachable) || errors.Is(err, ErrXianyuWorkerTimeout) ||
			errors.Is(err, ErrXianyuWorkerUncertain) || errors.Is(err, ErrXianyuWorkerMalformed) {
			return nil, ErrXianyuWorkerUnhealthy
		}
		return nil, err
	}
	// 全量解码：Health 需要顶层 success 与 data 并存（do 的信封解码只解 data，不适用）。
	var wrapped struct {
		Success bool                      `json:"success"`
		Data    *xianyuWorkerHealthStatus `json:"data"`
	}
	if err := json.Unmarshal(payload, &wrapped); err != nil {
		return nil, ErrXianyuWorkerMalformed
	}
	if wrapped.Data != nil {
		return &XianyuWorkerHealth{
			Backend:   wrapped.Data.Backend || wrapped.Success,
			WebSocket: wrapped.Data.WebSocket,
			Database:  bool(wrapped.Data.Database),
		}, nil
	}
	var out xianyuWorkerHealthStatus
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, ErrXianyuWorkerMalformed
	}
	return &XianyuWorkerHealth{
		Backend:   out.Backend,
		WebSocket: out.WebSocket,
		Database:  bool(out.Database),
	}, nil
}

// ListAccounts 从 Worker backend 投影账号 ID、昵称与启用状态。
func (c *XianyuWorkerClient) ListAccounts(ctx context.Context) ([]XianyuWorkerAccountStatus, error) {
	var out []XianyuWorkerAccountStatus
	if err := c.do(ctx, http.MethodGet, "/api/v1/internal/cookies/details", nil, &out, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateLoginSession 创建扫码会话，返回 session_id 与二维码地址。
// 会话由 Worker 持有；accountID 可空（首次添加账号）。
func (c *XianyuWorkerClient) CreateLoginSession(ctx context.Context, accountID string) (*XianyuWorkerLoginSessionStatus, error) {
	var out XianyuWorkerLoginSessionStatus
	if err := c.do(ctx, http.MethodPost, "/api/v1/internal/qr-login/generate", map[string]string{}, &out, accountID); err != nil {
		return nil, err
	}
	return &out, nil
}

// QueryLoginSession 按 Worker session_id 查询扫码会话状态。
func (c *XianyuWorkerClient) QueryLoginSession(ctx context.Context, sessionID string) (*XianyuWorkerLoginSessionStatus, error) {
	var out XianyuWorkerLoginSessionStatus
	if err := c.do(ctx, http.MethodGet, "/api/v1/internal/qr-login/status/"+sessionID, nil, &out, sessionID); err != nil {
		return nil, err
	}
	return &out, nil
}

// EnableAccount 通过 Worker backend 状态更新启动收消息任务。
func (c *XianyuWorkerClient) EnableAccount(ctx context.Context, accountID string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/internal/cookies/"+accountID+"/status", map[string]bool{"enabled": true}, nil, accountID)
}

// DisableAccount 通过 Worker backend 状态更新停止收消息任务。
func (c *XianyuWorkerClient) DisableAccount(ctx context.Context, accountID string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/internal/cookies/"+accountID+"/status", map[string]bool{"enabled": false}, nil, accountID)
}

// RefreshCookie 触发 Worker 侧批量续期（renew-login），并按目标账号校验续期结果。
// Worker renew-login 返回 { results: [{account_id, success, message}], success_count, failed_count }；
// 目标账号缺失或 success=false 时返回可展示错误，不把批量接口误判为单账号成功。
func (c *XianyuWorkerClient) RefreshCookie(ctx context.Context, accountID string) (*XianyuWorkerAccountStatus, error) {
	var out XianyuWorkerRenewResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/internal/cookies/renew-login", map[string]any{"account_ids": []string{accountID}}, &out, accountID); err != nil {
		return nil, err
	}
	for _, item := range out.Results {
		if item.AccountID == accountID {
			if !item.Success {
				msg := item.Message
				if msg == "" {
					msg = "cookie renewal failed"
				}
				return nil, &XianyuWorkerError{StatusCode: 500, Reason: "COOKIE_RENEW_FAILED", Message: msg}
			}
			return &XianyuWorkerAccountStatus{AccountID: accountID, Status: XianyuAccountStatusEnabled}, nil
		}
	}
	return nil, &XianyuWorkerError{StatusCode: 500, Reason: "ACCOUNT_NOT_IN_RENEW_RESULT", Message: "account not present in renew-login result"}
}

// ClearCredentials 停止 Worker 任务并删除 Worker 侧账号凭证（退出/清除凭证）。
func (c *XianyuWorkerClient) ClearCredentials(ctx context.Context, accountID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/internal/cookies/"+accountID+"/clear-credentials", map[string]string{}, nil, accountID)
}

// ListProducts 拉取指定账号在售商品（internal_api /items/cookie 投影）。
func (c *XianyuWorkerClient) ListProducts(ctx context.Context, accountID string) ([]XianyuWorkerProduct, error) {
	var out []XianyuWorkerProduct
	if err := c.do(ctx, http.MethodGet, "/api/v1/internal/items/cookie/"+accountID, nil, &out, accountID); err != nil {
		return nil, err
	}
	return out, nil
}

// ResendDelivery asks the Worker to resend an original delivery code through the
// buyer's existing chat. It waits for the platform-level send receipt so the
// caller can distinguish a real send from an accepted HTTP request.
func (c *XianyuWorkerClient) ResendDelivery(ctx context.Context, accountID, orderNo, itemID, buyerID, chatID, code string, attempt int) (*XianyuWorkerSendResult, error) {
	var out XianyuWorkerSendResult
	body := map[string]any{
		"account_id":   accountID,
		"message":      code,
		"chat_id":      chatID,
		"buyer_id":     buyerID,
		"wait_result":  true,
		"wait_timeout": 10.0,
		"order_no":     orderNo,
	}
	if attempt > 0 {
		body["attempt"] = attempt
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/internal/messages/send", body, &out, accountID); err != nil {
		return nil, err
	}
	return &out, nil
}

// doRequest 执行一次内网调用并返回原始响应体；统一传输错误细分（超时/不可达/解码）。
// 非 2xx 映射为 XianyuWorkerError；contextAccountID 用于在错误中附加账号上下文。
func (c *XianyuWorkerClient) doRequest(ctx context.Context, method, path string, body any, contextAccountID string) ([]byte, error) {
	if c == nil || c.baseURL == "" || c.httpClient == nil {
		return nil, ErrXianyuDeliveryNotConfigured
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal xianyu worker request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build xianyu worker request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiToken != "" {
		req.Header.Set("X-Worker-Token", c.apiToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// 传输错误细分：只有连接建立前（dial）失败才可判定"确定未 dispatch"；
		// 超时或请求可能已写出的错误（连接重置/TLS 等）结果不确定，保留 pending，避免重复补发。
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrXianyuWorkerTimeout
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, ErrXianyuWorkerTimeout
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return nil, ErrXianyuWorkerUnreachable
		}
		return nil, ErrXianyuWorkerUncertain
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read xianyu worker response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.mapError(resp.StatusCode, payload, contextAccountID)
	}
	return payload, nil
}

// do 执行一次内网调用并统一错误映射；out 按 {code, message, data} 信封优先解 data 子对象，其次裸载荷。
func (c *XianyuWorkerClient) do(ctx context.Context, method, path string, body any, out any, contextAccountID string) error {
	payload, err := c.doRequest(ctx, method, path, body, contextAccountID)
	if err != nil {
		return err
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(payload, &env) == nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return ErrXianyuWorkerMalformed
		}
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return ErrXianyuWorkerMalformed
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
	// Worker 对"目标账号不存在"统一返回 404 + {"detail":"账号不存在"}（清除/启停/续期都如此）。
	// 归类为域错误而非通用 500：重复退出据此可幂等成功，其余操作据此返回干净 404。
	// 检测显式命中该 detail，避免把真实 500 或路由缺失误判为账号缺失。
	if statusCode == http.StatusNotFound && bytes.Contains(payload, []byte("账号不存在")) {
		return ErrXianyuWorkerAccountNotFound
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
