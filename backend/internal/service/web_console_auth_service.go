package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 本文件实现「交互式浏览器登录编排」（方案 §3.2 模块 C 第 3 条末尾）。
//
// 角色划分：
//   - WebConsoleAuthService：进程内会话绑定 + 状态轮询 + 成功后落库并立即探测；
//   - LocalCaptchaHelperWebAuth：工作站 local_captcha_helper 的 /web-auth/* 契约；
//   - localCaptchaHelperWebAuthClient：上述契约的 HTTP 实现（与 local_captcha_helper.go
//     的 /challenge/* 客户端相互独立，互不修改）。
//
// 安全纪律：密码、storageState、cookie 只进请求体 / 落库字段，绝不写日志、绝不回显。

// 交互登录会话的进程内 TTL 与 helper 侧等待上限。
const (
	// webConsoleAuthSessionTTL 是 Go 侧会话绑定的有效期（略大于 helper 默认 max_wait）。
	webConsoleAuthSessionTTL = 12 * time.Minute
	// webConsoleAuthHelperDefaultTimeout 是发给 helper 的人工等待秒数（helper 侧上限 600）。
	webConsoleAuthHelperDefaultTimeout = 600
	// webConsoleAuthHelperDefaultHTTPTimeout 是 helper HTTP 客户端缺省超时。
	webConsoleAuthHelperDefaultHTTPTimeout = 10 * time.Second
	// webConsoleAuthMaxResponseBytes 限制 helper 响应体大小。
	webConsoleAuthMaxResponseBytes = 256 << 10

	// webConsoleAuthStatusSucceeded / Failed / Pending 是对外三态。
	webConsoleAuthStatusPending   = "pending"
	webConsoleAuthStatusSucceeded = "succeeded"
	webConsoleAuthStatusFailed    = "failed"
)

// 对外错误：调用方可据此区分「不适用」与真实故障。
var (
	// ErrWebConsoleAuthNotApplicable 表示账号不属于 Web 控制台额度上游。
	ErrWebConsoleAuthNotApplicable = errors.New("web console auth not applicable")
	// errWebConsoleAuthHelperNotConfigured 表示未配置 local_captcha_helper。
	errWebConsoleAuthHelperNotConfigured = errors.New("web console auth helper is not configured")
	// errWebConsoleAuthHelperBusy 表示 helper 的单槽位被占用（HTTP 409/429）。
	errWebConsoleAuthHelperBusy = errors.New("web console auth helper is busy")
	// errWebConsoleAuthSessionBindingUnknown 表示客户端未记录该 session 的绑定信息。
	errWebConsoleAuthSessionBindingUnknown = errors.New("web console auth session binding unknown")
)

// ---------------------------------------------------------------------------
// 对外结构
// ---------------------------------------------------------------------------

// WebConsoleAuthSession 是一次交互登录的进程内会话绑定。
type WebConsoleAuthSession struct {
	ID               string // helper 返回的 session_id
	AccountID        int64
	Provider         string
	Binding          string // 传给 helper 的 session_binding_id
	ExpectedUsername string
	ExpiresAt        time.Time
}

// WebConsoleAuthStatus 轮询结果。
type WebConsoleAuthStatus struct {
	Status  string                      `json:"status"` // pending | succeeded | failed
	Message string                      `json:"message,omitempty"`
	Probe   *WebConsoleQuotaProbeResult `json:"probe,omitempty"`
}

// WebAuthStartRequest 是发起交互登录时发给 helper 的参数。
// Password 只进请求体，绝不落日志。
type WebAuthStartRequest struct {
	Provider         string
	SessionBindingID string
	LoginURL         string
	SuccessURLPrefix string
	Username         string
	Password         string
	TimeoutSeconds   int
}

// LocalCaptchaHelperWebAuth 是 local_captcha_helper /web-auth/* 的窄契约。
type LocalCaptchaHelperWebAuth interface {
	WebAuthStart(ctx context.Context, req WebAuthStartRequest) (string, error)
	WebAuthStatus(ctx context.Context, sessionID string) (string, string, error) // status, message
	WebAuthResult(ctx context.Context, sessionID, provider, binding string) (storageState, accountLabel string, err error)
}

// WebConsoleAuthService 编排「本地有头浏览器过一次验证 → 回传 storageState → 落库 → 立即探测」。
type WebConsoleAuthService struct {
	quotaService *WebConsoleQuotaService
	helper       LocalCaptchaHelperWebAuth
	mu           sync.Mutex
	sessions     map[string]*WebConsoleAuthSession
	accountRepo  AccountRepository
	now          func() time.Time
}

// NewWebConsoleAuthService 构造交互登录编排服务。
// accountRepo 与 quotaService 通常共享同一仓储实例。
func NewWebConsoleAuthService(quotaService *WebConsoleQuotaService, helper LocalCaptchaHelperWebAuth, accountRepo AccountRepository) *WebConsoleAuthService {
	return &WebConsoleAuthService{
		quotaService: quotaService,
		helper:       helper,
		sessions:     make(map[string]*WebConsoleAuthSession),
		accountRepo:  accountRepo,
		now:          time.Now,
	}
}

// ---------------------------------------------------------------------------
// 编排：发起
// ---------------------------------------------------------------------------

// StartInteractiveLogin 读取账号并调用 helper 打开有头浏览器，返回进程内会话绑定。
func (s *WebConsoleAuthService) StartInteractiveLogin(ctx context.Context, accountID int64) (*WebConsoleAuthSession, error) {
	if s == nil || s.accountRepo == nil {
		return nil, errors.New("web console auth service: nil account repository")
	}
	if s.helper == nil {
		return nil, errWebConsoleAuthHelperNotConfigured
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	provider := ResolveWebConsoleQuotaProvider(account)
	if provider == "" {
		return nil, ErrWebConsoleAuthNotApplicable
	}

	now := s.now()
	binding := fmt.Sprintf("acct-%d-%d", accountID, now.UnixNano())

	req := WebAuthStartRequest{
		Provider:         provider,
		SessionBindingID: binding,
		Username:         strings.TrimSpace(account.GetCredential(webConsoleQuotaCredentialKeyUsername)),
		Password:         account.GetCredential(webConsoleQuotaCredentialKeyPassword),
		TimeoutSeconds:   webConsoleAuthHelperDefaultTimeout,
	}
	if endpoints, ok := webConsoleQuotaProviderEndpointTable[provider]; ok {
		req.LoginURL = endpoints.loginURL
		req.SuccessURLPrefix = endpoints.successURLPrefix
	}

	sessionID, err := s.helper.WebAuthStart(ctx, req)
	if err != nil {
		return nil, err
	}

	session := &WebConsoleAuthSession{
		ID:               sessionID,
		AccountID:        accountID,
		Provider:         provider,
		Binding:          binding,
		ExpectedUsername: req.Username,
		ExpiresAt:        now.Add(webConsoleAuthSessionTTL),
	}

	s.mu.Lock()
	s.pruneExpiredLocked(now)
	s.sessions[sessionID] = session
	s.mu.Unlock()

	out := *session
	return &out, nil
}

// ---------------------------------------------------------------------------
// 编排：轮询
// ---------------------------------------------------------------------------

// GetInteractiveLoginStatus 轮询 helper 状态；成功后落库 storageState 并立即探测。
//
// 三态映射：pending（含 helper 的 running）→ pending；ok/succeeded → succeeded；
// context_gap/failed 及未知状态 → failed。
func (s *WebConsoleAuthService) GetInteractiveLoginStatus(ctx context.Context, accountID int64, sessionID string) (*WebConsoleAuthStatus, error) {
	if s == nil {
		return nil, errors.New("web console auth service is nil")
	}
	session, status := s.boundSession(accountID, sessionID)
	if status != nil {
		return status, nil
	}
	if s.helper == nil {
		return nil, errWebConsoleAuthHelperNotConfigured
	}

	rawStatus, message, err := s.helper.WebAuthStatus(ctx, session.ID)
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(strings.TrimSpace(rawStatus)) {
	case webConsoleAuthStatusPending, "running":
		return &WebConsoleAuthStatus{Status: webConsoleAuthStatusPending}, nil
	case webConsoleAuthStatusFailed, "context_gap":
		return &WebConsoleAuthStatus{Status: webConsoleAuthStatusFailed, Message: message}, nil
	case webConsoleAuthStatusSucceeded, "ok":
		return s.completeInteractiveLogin(ctx, session)
	default:
		// 未知状态不放进 pending：避免前端无限轮询。
		return &WebConsoleAuthStatus{Status: webConsoleAuthStatusFailed, Message: "unknown helper status: " + strings.TrimSpace(rawStatus)}, nil
	}
}

// completeInteractiveLogin 拉取 helper 结果 → 落库 → 立即探测（顺序不可颠倒）。
func (s *WebConsoleAuthService) completeInteractiveLogin(ctx context.Context, session *WebConsoleAuthSession) (*WebConsoleAuthStatus, error) {
	if s.accountRepo == nil {
		return nil, errors.New("web console auth service: nil account repository")
	}
	account, err := s.accountRepo.GetByID(ctx, session.AccountID)
	if err != nil {
		return nil, err
	}

	storageState, accountLabel, err := s.helper.WebAuthResult(ctx, session.ID, session.Provider, session.Binding)
	if err != nil {
		// helper 明确回传失败（如 context_gap）或传输失败：终态为 failed，不落库也不探测。
		return &WebConsoleAuthStatus{Status: webConsoleAuthStatusFailed, Message: err.Error()}, nil
	}
	storageState = strings.TrimSpace(storageState)
	if storageState == "" {
		return &WebConsoleAuthStatus{Status: webConsoleAuthStatusFailed, Message: "web console auth result missing storage_state"}, nil
	}

	currentUsername := account.GetCredential(webConsoleQuotaCredentialKeyUsername)
	patch := map[string]any{webConsoleQuotaCredentialKeyStorageState: storageState}
	if strings.TrimSpace(currentUsername) == "" {
		if label := strings.TrimSpace(accountLabel); label != "" {
			patch[webConsoleQuotaCredentialKeyUsername] = label
		}
	}
	if err := s.accountRepo.PatchAccountCredentials(ctx, session.AccountID, patch, currentUsername); err != nil {
		// 只记账号与错误，绝不打印 storage_state / 密码。
		slog.Warn("web_console_auth_patch_storage_state_failed", "account_id", session.AccountID, "error", err)
		return &WebConsoleAuthStatus{Status: webConsoleAuthStatusFailed, Message: "web console auth storage state persist failed"}, nil
	}

	s.forgetSession(session.ID)

	var probe *WebConsoleQuotaProbeResult
	if s.quotaService != nil {
		// 探测失败不影响「登录已成功」事实：结果里的 probe 留空由前端按需重试。
		probe, _ = s.quotaService.ProbeAccount(ctx, session.AccountID)
	}
	return &WebConsoleAuthStatus{Status: webConsoleAuthStatusSucceeded, Probe: probe}, nil
}

// ---------------------------------------------------------------------------
// 会话表
// ---------------------------------------------------------------------------

// boundSession 校验 sessionID 属于 accountID 且未过期。
// 校验失败时返回 (nil, failed 状态)；过期项顺手清理。
func (s *WebConsoleAuthService) boundSession(accountID int64, sessionID string) (*WebConsoleAuthSession, *WebConsoleAuthStatus) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.sessions[strings.TrimSpace(sessionID)]
	if !ok {
		return nil, &WebConsoleAuthStatus{Status: webConsoleAuthStatusFailed, Message: "web console auth session not found"}
	}
	if session.AccountID != accountID {
		return nil, &WebConsoleAuthStatus{Status: webConsoleAuthStatusFailed, Message: "web console auth session does not belong to this account"}
	}
	if !now.Before(session.ExpiresAt) {
		delete(s.sessions, session.ID)
		return nil, &WebConsoleAuthStatus{Status: webConsoleAuthStatusFailed, Message: "web console auth session expired"}
	}
	out := *session
	return &out, nil
}

// forgetSession 在结果一次性消费后移除会话绑定。
func (s *WebConsoleAuthService) forgetSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// pruneExpiredLocked 清理过期会话，调用方必须持有 s.mu。
func (s *WebConsoleAuthService) pruneExpiredLocked(now time.Time) {
	for id, session := range s.sessions {
		if !now.Before(session.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP 客户端
// ---------------------------------------------------------------------------

// webConsoleAuthBinding 记录 sessionID → (provider, session_binding_id)。
// helper 的 status 接口要求这两个查询参数做绑定校验，而 WebAuthStatus 契约只给
// sessionID，故在 Start 成功时记住、Result 消费或 404 后丢弃。
type webConsoleAuthBinding struct {
	provider string
	binding  string
	expires  time.Time
}

type localCaptchaHelperWebAuthClient struct {
	baseURL string
	apiKey  string
	http    *http.Client

	mu       sync.Mutex
	bindings map[string]webConsoleAuthBinding
}

// NewLocalCaptchaHelperWebAuthClient 构造 /web-auth/* 客户端；base_url 为空时返回 nil。
func NewLocalCaptchaHelperWebAuthClient(cfg LocalCaptchaHelperConfig) LocalCaptchaHelperWebAuth {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = webConsoleAuthHelperDefaultHTTPTimeout
	}
	return &localCaptchaHelperWebAuthClient{
		baseURL:  base,
		apiKey:   strings.TrimSpace(cfg.APIKey),
		http:     &http.Client{Timeout: timeout},
		bindings: make(map[string]webConsoleAuthBinding),
	}
}

var _ LocalCaptchaHelperWebAuth = (*localCaptchaHelperWebAuthClient)(nil)

// webConsoleAuthStartRequest 是 POST /web-auth/start 的请求体（snake_case 与 helper 对齐）。
type webConsoleAuthStartRequest struct {
	Provider         string `json:"provider"`
	SessionBindingID string `json:"session_binding_id"`
	LoginURL         string `json:"login_url"`
	SuccessURLPrefix string `json:"success_url_prefix"`
	Username         string `json:"username,omitempty"`
	Password         string `json:"password,omitempty"`
	Timeout          int    `json:"timeout"`
}

// webConsoleAuthResultRequest 是 POST /web-auth/{id}/result 的请求体。
type webConsoleAuthResultRequest struct {
	Provider         string `json:"provider"`
	SessionBindingID string `json:"session_binding_id"`
}

// webConsoleAuthHelperResponse 兼容 start/status/result 三个接口的公共字段。
type webConsoleAuthHelperResponse struct {
	Success   bool              `json:"success"`
	SessionID string            `json:"session_id"`
	Status    string            `json:"status"`
	Message   string            `json:"message"`
	Data      map[string]string `json:"data"`
}

// WebAuthStart 调 POST /web-auth/start；409/429 归一为 busy 错误。
func (c *localCaptchaHelperWebAuthClient) WebAuthStart(ctx context.Context, req WebAuthStartRequest) (string, error) {
	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = webConsoleAuthHelperDefaultTimeout
	}
	payload := webConsoleAuthStartRequest{
		Provider:         req.Provider,
		SessionBindingID: req.SessionBindingID,
		LoginURL:         req.LoginURL,
		SuccessURLPrefix: req.SuccessURLPrefix,
		Username:         req.Username,
		Password:         req.Password,
		Timeout:          timeout,
	}

	code, raw, err := c.do(ctx, http.MethodPost, "/web-auth/start", payload)
	if err != nil {
		return "", err
	}
	var out webConsoleAuthHelperResponse
	_ = json.Unmarshal(raw, &out)

	if code == http.StatusConflict || code == http.StatusTooManyRequests {
		return "", fmt.Errorf("%w: %s", errWebConsoleAuthHelperBusy, strings.TrimSpace(out.Message))
	}
	if code < 200 || code >= 300 || !out.Success {
		return "", fmt.Errorf("web console auth start failed: helper HTTP %d", code)
	}
	sessionID := strings.TrimSpace(out.SessionID)
	if sessionID == "" {
		return "", errors.New("web console auth start failed: helper returned empty session_id")
	}

	now := time.Now()
	c.mu.Lock()
	if c.bindings == nil {
		c.bindings = make(map[string]webConsoleAuthBinding)
	}
	for id, b := range c.bindings {
		if !now.Before(b.expires) {
			delete(c.bindings, id)
		}
	}
	c.bindings[sessionID] = webConsoleAuthBinding{
		provider: req.Provider,
		binding:  req.SessionBindingID,
		expires:  now.Add(2 * webConsoleAuthSessionTTL),
	}
	c.mu.Unlock()

	return sessionID, nil
}

// WebAuthStatus 调 GET /web-auth/{id}/status。
// helper 用 provider + session_binding_id 做会话归属校验，故以 Start 时记录的绑定回填查询参数。
func (c *localCaptchaHelperWebAuthClient) WebAuthStatus(ctx context.Context, sessionID string) (string, string, error) {
	binding, ok := c.bindingOf(sessionID)
	if !ok {
		return "", "", errWebConsoleAuthSessionBindingUnknown
	}
	path := "/web-auth/" + url.PathEscape(strings.TrimSpace(sessionID)) + "/status?provider=" +
		url.QueryEscape(binding.provider) + "&session_binding_id=" + url.QueryEscape(binding.binding)

	code, raw, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", "", err
	}
	var out webConsoleAuthHelperResponse
	_ = json.Unmarshal(raw, &out)

	switch {
	case code == http.StatusNotFound:
		c.forgetBinding(sessionID)
		return "", "", errors.New("web console auth session not found or already consumed")
	case code == http.StatusConflict:
		return "", "", fmt.Errorf("web console auth session binding mismatch: %s", strings.TrimSpace(out.Message))
	case code < 200 || code >= 300 || !out.Success:
		return "", "", fmt.Errorf("web console auth status failed: helper HTTP %d", code)
	}

	status := strings.ToLower(strings.TrimSpace(out.Status))
	if status == "" {
		return "", "", errors.New("web console auth status failed: helper returned empty status")
	}
	return status, strings.TrimSpace(out.Message), nil
}

// WebAuthResult 调 POST /web-auth/{id}/result 取 storageState 与账号标签。
func (c *localCaptchaHelperWebAuthClient) WebAuthResult(ctx context.Context, sessionID, provider, binding string) (string, string, error) {
	path := "/web-auth/" + url.PathEscape(strings.TrimSpace(sessionID)) + "/result"
	payload := webConsoleAuthResultRequest{Provider: provider, SessionBindingID: binding}

	code, raw, err := c.do(ctx, http.MethodPost, path, payload)
	if err != nil {
		return "", "", err
	}
	var out webConsoleAuthHelperResponse
	_ = json.Unmarshal(raw, &out)

	switch {
	case code == http.StatusNotFound:
		c.forgetBinding(sessionID)
		return "", "", errors.New("web console auth session not found or already consumed")
	case code == http.StatusConflict:
		return "", "", fmt.Errorf("web console auth session binding mismatch: %s", strings.TrimSpace(out.Message))
	case code == http.StatusUnprocessableEntity:
		// 422 = context_gap（人工未在限时内完成验证）。
		msg := strings.TrimSpace(out.Message)
		if msg == "" {
			msg = "web console auth not completed"
		}
		return "", "", errors.New(msg)
	case code < 200 || code >= 300 || !out.Success:
		msg := strings.TrimSpace(out.Message)
		if msg == "" {
			msg = fmt.Sprintf("helper HTTP %d", code)
		}
		return "", "", fmt.Errorf("web console auth result failed: %s", msg)
	}

	storageState := strings.TrimSpace(out.Data["storage_state"])
	if storageState == "" {
		return "", "", errors.New("web console auth result missing storage_state")
	}
	c.forgetBinding(sessionID)
	return storageState, strings.TrimSpace(out.Data["account_label"]), nil
}

// bindingOf 读取 sessionID 的绑定信息。
func (c *localCaptchaHelperWebAuthClient) bindingOf(sessionID string) (webConsoleAuthBinding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	binding, ok := c.bindings[strings.TrimSpace(sessionID)]
	if !ok {
		return webConsoleAuthBinding{}, false
	}
	if !time.Now().Before(binding.expires) {
		delete(c.bindings, strings.TrimSpace(sessionID))
		return webConsoleAuthBinding{}, false
	}
	return binding, true
}

// forgetBinding 丢弃已消费/已失效的绑定。
func (c *localCaptchaHelperWebAuthClient) forgetBinding(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.bindings, strings.TrimSpace(sessionID))
}

// do 发送请求并返回状态码与原始响应体（非 2xx 也返回，由调用方按状态码分流）。
func (c *localCaptchaHelperWebAuthClient) do(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	if c == nil || c.http == nil {
		return 0, nil, errWebConsoleAuthHelperNotConfigured
	}
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("web console auth helper request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, webConsoleAuthMaxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("web console auth helper read response: %w", err)
	}
	return resp.StatusCode, raw, nil
}
