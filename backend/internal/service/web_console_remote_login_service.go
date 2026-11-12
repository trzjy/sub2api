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
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 服务端登录会话编排（扫码 / 短信 / 滑块）。
//
// 存在的理由：longcat 只有扫码与短信登录，compshare / sensenova 的默认页也是
// 短信登录，这三家都必须由真人完成。早期实现把这一步推给用户本机安装的
// local_captcha_helper（桌面弹窗），「必须在装了 helper 的电脑上操作」是产品级的
// 硬伤。本服务把登录过程搬回服务端：worker 打开登录页并回传画面，管理端网页把
// 点击 / 输入转发回同一个页面上下文，登录成功后一次性取回 storageState 落库。
//
// 与既有 WebConsoleAuthService（本机 helper 通道）并存：helper 通道保留作滑块
// 等强风控场景的兜底，本服务是常规路径。

const (
	// webConsoleRemoteLoginSessionTTL 与 worker 侧会话寿命一致（10 分钟）加少量冗余。
	webConsoleRemoteLoginSessionTTL = 11 * time.Minute
	// webConsoleRemoteLoginHTTPTimeout 是单次画面/动作请求的超时（截图需等页面稳定）。
	webConsoleRemoteLoginHTTPTimeout = 45 * time.Second
	// webConsoleRemoteLoginMaxResponseBytes 限制 worker 响应体（base64 JPEG）。
	webConsoleRemoteLoginMaxResponseBytes = 8 << 20
	// webConsoleRemoteLoginPathPrefix 是 worker 侧登录会话路由前缀。
	webConsoleRemoteLoginPathPrefix = "/v1/login"

	// webConsoleRemoteLoginStatusPending / Authenticated 是 worker 回传的会话状态。
	webConsoleRemoteLoginStatusPending       = "pending"
	webConsoleRemoteLoginStatusAuthenticated = "authenticated"
	// webConsoleRemoteLoginStatusVerifyFailed 表示「页面看起来登上了，但拿这份会话
	// 读不到用量」——不落库，让用户换登录方式重来，绝不保存成有效票据。
	webConsoleRemoteLoginStatusVerifyFailed = "verify_failed"
)

// 对外错误：调用方据此区分「不适用」「未配置」「还在等用户操作」。
var (
	// ErrRemoteLoginNotApplicable 表示账号不属于 Web 控制台额度上游。
	ErrRemoteLoginNotApplicable = errors.New("web console remote login not applicable")
	// ErrRemoteLoginNotConfigured 表示未配置 web_quota_worker，登录通道不可用。
	ErrRemoteLoginNotConfigured = errors.New("web quota worker is not configured")
	// ErrRemoteLoginPending 表示用户尚未完成登录（仍在登录页）。
	ErrRemoteLoginPending = errors.New("web console remote login pending")
	// ErrRemoteLoginSessionUnknown 表示会话不存在、已过期或不属于该账号。
	ErrRemoteLoginSessionUnknown = errors.New("web console remote login session unknown")
	// ErrRemoteLoginStepFailed 表示 worker 侧动作失败（页面已关闭等）。
	ErrRemoteLoginStepFailed = errors.New("web console remote login step failed")
)

// WebConsoleRemoteLoginFrame 是一帧登录画面（base64 JPEG）与会话状态。
// 不回传任何 cookie / storageState —— 凭据只在 Finish 成功时由内部消费。
type WebConsoleRemoteLoginFrame struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"` // pending | authenticated
	// Phase 是页面生命周期（opening | ready | failed），与 Status 互相独立：
	// Status 说「人登进去了没有」，Phase 说「页面能不能看」。
	//
	// 为什么需要它：打开登录页要启动浏览器并等 SPA 首帧，实测 16~30 秒，
	// 超过前端请求超时。start 因此改成只建会话立即返回，页面在后台打开，
	// 由 Phase 告诉前端「还在开」还是「打不开」，不再用一个长请求硬等。
	Phase string `json:"phase,omitempty"`
	// Message 是 Phase=failed 时的原因码（worker 侧常量，仅供前端显示人话）。
	Message        string `json:"message,omitempty"`
	Screenshot     string `json:"screenshot,omitempty"`
	URL            string `json:"url,omitempty"`
	ViewportWidth  int    `json:"viewport_width"`
	ViewportHeight int    `json:"viewport_height"`
	ExpiresIn      int64  `json:"expires_in"`
	// Fields / Buttons 是页面上的可见输入框与按钮清单。
	// 存在的理由：让用户点缩略图坐标去聚焦输入框不可靠（美团 passport 的
	// 「动态码」框只有 176×18 CSS 像素，缩到面板后约 7 像素高），
	// 改为把页面表单报给前端，由用户按字段填写。
	Fields  []WebConsoleRemoteLoginField  `json:"fields,omitempty"`
	Buttons []WebConsoleRemoteLoginButton `json:"buttons,omitempty"`
	// Applied 表示本次动作是否真的落到了目标元素上（仅 fill / click 返回）。
	Applied *bool `json:"applied,omitempty"`
}

// WebConsoleRemoteLoginField 是页面上的一个可见输入框。
type WebConsoleRemoteLoginField struct {
	Index  int    `json:"index"`
	Type   string `json:"type"`
	Label  string `json:"label"`
	Filled bool   `json:"filled"`
	Masked bool   `json:"masked"`
}

// WebConsoleRemoteLoginButton 是页面上的一个可点按钮。
type WebConsoleRemoteLoginButton struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// WebConsoleRemoteLoginResult 是 Finish 的结果：登录成功并已落库，附带一次探测。
type WebConsoleRemoteLoginResult struct {
	Status       string                      `json:"status"` // succeeded | pending
	AccountLabel string                      `json:"account_label,omitempty"`
	Probe        *WebConsoleQuotaProbeResult `json:"probe,omitempty"`
	Message      string                      `json:"message,omitempty"`
}

type remoteLoginBinding struct {
	AccountID int64
	Provider  string
	ExpiresAt time.Time
}

// WebConsoleRemoteLoginService 编排「服务端打开登录页 → 用户远程操作 → 落库 → 探测」。
type WebConsoleRemoteLoginService struct {
	accountRepo  AccountRepository
	quotaService *WebConsoleQuotaService
	cfg          *config.Config
	httpClient   *http.Client

	mu       sync.Mutex
	sessions map[string]remoteLoginBinding
	now      func() time.Time
}

// NewWebConsoleRemoteLoginService 构造登录编排服务。httpClient 由调用方注入（便于测试）。
func NewWebConsoleRemoteLoginService(
	accountRepo AccountRepository,
	quotaService *WebConsoleQuotaService,
	cfg *config.Config,
	httpClient *http.Client,
) *WebConsoleRemoteLoginService {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &WebConsoleRemoteLoginService{
		accountRepo:  accountRepo,
		quotaService: quotaService,
		cfg:          cfg,
		httpClient:   httpClient,
		sessions:     make(map[string]remoteLoginBinding),
		now:          time.Now,
	}
}

// workerReady 报告 worker 是否配置完整。
func (s *WebConsoleRemoteLoginService) workerReady() bool {
	return s != nil && s.cfg != nil && strings.TrimSpace(s.cfg.WebQuotaWorker.BaseURL) != ""
}

// Start 为账号打开一扇登录画面。
func (s *WebConsoleRemoteLoginService) Start(ctx context.Context, accountID int64) (*WebConsoleRemoteLoginFrame, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrRemoteLoginNotConfigured
	}
	if !s.workerReady() {
		return nil, ErrRemoteLoginNotConfigured
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	provider := ResolveWebConsoleQuotaProvider(account)
	if provider == "" {
		return nil, ErrRemoteLoginNotApplicable
	}
	endpoints, ok := webConsoleQuotaProviderEndpointTable[provider]
	if !ok || strings.TrimSpace(endpoints.loginURL) == "" {
		return nil, ErrRemoteLoginNotApplicable
	}

	payload := map[string]any{
		"provider":           provider,
		"login_url":          endpoints.loginURL,
		"success_url_prefix": endpoints.successURLPrefix,
	}
	if account.Proxy != nil {
		payload["proxy_url"] = account.Proxy.URL()
	}

	var frame WebConsoleRemoteLoginFrame
	if err := s.callWorker(ctx, http.MethodPost, webConsoleRemoteLoginPathPrefix+"/start", payload, &frame); err != nil {
		return nil, err
	}
	if strings.TrimSpace(frame.SessionID) == "" {
		return nil, ErrRemoteLoginStepFailed
	}

	s.mu.Lock()
	s.pruneExpiredLocked(s.now())
	s.sessions[frame.SessionID] = remoteLoginBinding{
		AccountID: accountID,
		Provider:  provider,
		ExpiresAt: s.now().Add(webConsoleRemoteLoginSessionTTL),
	}
	s.mu.Unlock()

	return &frame, nil
}

// Frame 拉一帧最新画面。会话必须是该账号自己发起的。
func (s *WebConsoleRemoteLoginService) Frame(ctx context.Context, accountID int64, sessionID string) (*WebConsoleRemoteLoginFrame, error) {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return nil, err
	}
	var frame WebConsoleRemoteLoginFrame
	path := fmt.Sprintf("%s/%s/state", webConsoleRemoteLoginPathPrefix, sessionID)
	if err := s.callWorker(ctx, http.MethodGet, path, nil, &frame); err != nil {
		return nil, err
	}
	return &frame, nil
}

// Tap 把一次点击转发到页面（坐标为视口 CSS 像素）。
func (s *WebConsoleRemoteLoginService) Tap(ctx context.Context, accountID int64, sessionID string, x, y float64) error {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return err
	}
	return s.workerAction(ctx, sessionID, "tap", map[string]any{"x": x, "y": y})
}

// Type 把一段文字打到当前焦点元素。
func (s *WebConsoleRemoteLoginService) Type(ctx context.Context, accountID int64, sessionID, text string) error {
	if len(text) > 512 {
		return fmt.Errorf("web console remote login: text too long")
	}
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return err
	}
	return s.workerAction(ctx, sessionID, "type", map[string]any{"text": text})
}

// FillField 把文字填进页面上的指定输入框（按 label 定位，不依赖坐标）。
//
// 为什么不是「先点坐标聚焦再打字」：登录取证页的输入框可能只有十几像素高，
// 缩放到管理端面板后鼠标几乎点不中，点不中就等于没聚焦、输入全部丢失
// （2026-11-12 线上故障）。按字段填写与元素大小无关。
func (s *WebConsoleRemoteLoginService) FillField(ctx context.Context, accountID int64, sessionID, label, text string, index int) (*WebConsoleRemoteLoginFrame, error) {
	if len(text) > 512 {
		return nil, fmt.Errorf("web console remote login: text too long")
	}
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return nil, err
	}
	return s.workerActionFrame(ctx, sessionID, "fill", map[string]any{
		"index": index,
		"label": label,
		"text":  text,
	})
}

// ClickButton 点击页面上的指定按钮（按按钮文字定位，index 仅作兜底提示）。
func (s *WebConsoleRemoteLoginService) ClickButton(ctx context.Context, accountID int64, sessionID, text string, index int) (*WebConsoleRemoteLoginFrame, error) {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return nil, err
	}
	return s.workerActionFrame(ctx, sessionID, "click", map[string]any{
		"index": index,
		"text":  text,
	})
}

// ToggleField 勾选 / 取消勾选页面上的勾选项（如「我已阅读并同意用户协议」）。
//
// 这一项漏了会直接把登录拦死：点登录只弹「请先阅读并勾选用户协议」。
// 页面上这个勾选框往往是个十几像素的小圆圈，缩到面板里点不中，故必须由后端代点。
func (s *WebConsoleRemoteLoginService) ToggleField(ctx context.Context, accountID int64, sessionID, label string, index int) (*WebConsoleRemoteLoginFrame, error) {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return nil, err
	}
	return s.workerActionFrame(ctx, sessionID, "toggle", map[string]any{
		"index": index,
		"label": label,
	})
}

// workerActionFrame 执行一次动作并取回 worker 刷新后的整帧。
func (s *WebConsoleRemoteLoginService) workerActionFrame(ctx context.Context, sessionID, action string, payload map[string]any) (*WebConsoleRemoteLoginFrame, error) {
	var frame WebConsoleRemoteLoginFrame
	path := fmt.Sprintf("%s/%s/%s", webConsoleRemoteLoginPathPrefix, sessionID, action)
	if err := s.callWorker(ctx, http.MethodPost, path, payload, &frame); err != nil {
		return nil, err
	}
	return &frame, nil
}

// PressKey 发送一个白名单按键（Enter / Backspace / Tab 等）。
func (s *WebConsoleRemoteLoginService) PressKey(ctx context.Context, accountID int64, sessionID, key string) error {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return err
	}
	return s.workerAction(ctx, sessionID, "key", map[string]any{"key": key})
}

// Scroll 滚动画面。
func (s *WebConsoleRemoteLoginService) Scroll(ctx context.Context, accountID int64, sessionID string, deltaY float64) error {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return err
	}
	return s.workerAction(ctx, sessionID, "scroll", map[string]any{"delta_y": deltaY})
}

// Reload 重新打开登录页（页面卡住时的逃生口）。
func (s *WebConsoleRemoteLoginService) Reload(ctx context.Context, accountID int64, sessionID string) error {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return err
	}
	return s.workerAction(ctx, sessionID, "reload", nil)
}

// Close 主动关闭会话（用户取消时释放服务端浏览器上下文）。
func (s *WebConsoleRemoteLoginService) Close(ctx context.Context, accountID int64, sessionID string) error {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return err
	}
	s.forgetSession(sessionID)
	path := fmt.Sprintf("%s/%s", webConsoleRemoteLoginPathPrefix, sessionID)
	// 关闭失败不影响前端结果：worker 侧还有 TTL 兜底。
	_ = s.callWorker(ctx, http.MethodDelete, path, nil, nil)
	return nil
}

// Finish 取回登录后的 storageState → 落库 → 立即探测。
//
// 未登录时返回 ErrRemoteLoginPending（前端继续轮询），绝不伪造成功。
func (s *WebConsoleRemoteLoginService) Finish(ctx context.Context, accountID int64, sessionID string) (*WebConsoleRemoteLoginResult, error) {
	binding, err := s.boundSession(accountID, sessionID)
	if err != nil {
		return nil, err
	}
	if s.accountRepo == nil {
		return nil, ErrRemoteLoginNotConfigured
	}

	var finish struct {
		Status       string `json:"status"`
		StorageState string `json:"storage_state"`
		AccountLabel string `json:"account_label"`
		Error        string `json:"error"`
	}
	path := fmt.Sprintf("%s/%s/finish", webConsoleRemoteLoginPathPrefix, sessionID)
	statusCode, err := s.callWorkerRaw(ctx, http.MethodPost, path, nil, &finish)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusConflict || finish.Status == webConsoleRemoteLoginStatusPending {
		// 「页面看着像登上了、实际没生效」要单独告诉用户，而不是笼统说「还没登录」：
		// 前者要换登录方式，后者只是继续操作。
		if strings.TrimSpace(finish.Error) == "login_not_effective" {
			return &WebConsoleRemoteLoginResult{
				Status:  webConsoleRemoteLoginStatusVerifyFailed,
				Message: finish.Error,
			}, nil
		}
		return &WebConsoleRemoteLoginResult{Status: webConsoleRemoteLoginStatusPending}, nil
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, ErrRemoteLoginStepFailed
	}

	storageState := strings.TrimSpace(finish.StorageState)
	if storageState == "" {
		return nil, ErrRemoteLoginStepFailed
	}

	account, err := s.accountRepo.GetByID(ctx, binding.AccountID)
	if err != nil {
		return nil, err
	}

	// 关键：先验证，再落库。
	//
	// 「当前网址落在控制台前缀内」只是页面位置，不是身份证据 —— 上游登录失败后
	// 常常跳回原页面，这个判据会把未登录的会话当成成功（2026-11-13 线上：
	// 美团扫码失败，界面却提示「已检测到登录完成」）。
	//
	// 只有**明确的否定**才拦：worker 说 needs_interactive_auth / auth_failed。
	// 探测报 error（如该上游适配器还没实现）属于「判不了」，不能据此判死，
	// 否则一条链路永远登不上——判不了就交给 worker 的跳转校验和用户自己判断。
	var verifiedProbe *WebConsoleQuotaProbeResult
	if s.quotaService != nil {
		probe, probeErr := s.quotaService.ProbeWithStorageState(ctx, account, storageState)
		if probeErr == nil && probe != nil && webConsoleProbeSaysNotLoggedIn(probe.AuthState) {
			// 不落库、不关会话：用户可以直接换「手机验证码登录」再来一次。
			return &WebConsoleRemoteLoginResult{
				Status:  webConsoleRemoteLoginStatusVerifyFailed,
				Message: probe.AuthState,
			}, nil
		}
		if probeErr == nil && probe != nil && probe.AuthState == webConsoleQuotaAuthStateOK {
			verifiedProbe = probe
		}
	}

	currentUsername := account.GetCredential(webConsoleQuotaCredentialKeyUsername)
	// 只写 login 产物（storage_state）。不把页面标题写进 console_username：
	// 该字段是「控制台账号」语义，参与同级账号匹配与探测合并键，写标题会污染匹配。
	patch := map[string]any{webConsoleQuotaCredentialKeyStorageState: storageState}
	if err := s.accountRepo.PatchAccountCredentials(ctx, binding.AccountID, patch, currentUsername); err != nil {
		// 只记账号与错误类型，绝不打印 storage_state。
		slog.Warn("web_console_remote_login_persist_failed", "account_id", binding.AccountID, "error", err)
		return nil, fmt.Errorf("web console remote login: persist storage state: %w", err)
	}
	syncWebConsoleStorageStateToSiblings(ctx, s.accountRepo, account, currentUsername, storageState)

	s.forgetSession(sessionID)

	result := &WebConsoleRemoteLoginResult{
		Status:       "succeeded",
		AccountLabel: strings.TrimSpace(finish.AccountLabel),
	}
	// 验证阶段已经探过一次并落库快照，这里直接复用，不再重复打上游。
	if verifiedProbe != nil {
		result.Probe = verifiedProbe
	} else if s.quotaService != nil {
		// 未做验证（quotaService 未配置）时保留原有行为：探测失败不影响「登录成功」。
		result.Probe, _ = s.quotaService.ProbeAccount(ctx, binding.AccountID)
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// 内部：会话绑定与 worker 调用
// ---------------------------------------------------------------------------

// webConsoleProbeSaysNotLoggedIn 判断探测结果是否**明确**表示「没登录」。
//
// 只认 needs_interactive_auth / auth_failed 两个否定态：这两种是适配器真的
// 走到上游、看到「要登录」才得出的结论。error 代表「没探成」（适配器未实现、
// worker 不可用、超时），不能当未登录——否则该上游永远无法完成登录。
func webConsoleProbeSaysNotLoggedIn(authState string) bool {
	switch strings.TrimSpace(authState) {
	case webConsoleQuotaAuthStateNeedsInteractive, webConsoleQuotaAuthStateAuthFailed:
		return true
	default:
		return false
	}
}

func (s *WebConsoleRemoteLoginService) boundSession(accountID int64, sessionID string) (*remoteLoginBinding, error) {
	trimmed := strings.TrimSpace(sessionID)
	if trimmed == "" {
		return nil, ErrRemoteLoginSessionUnknown
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(now)
	binding, ok := s.sessions[trimmed]
	if !ok {
		return nil, ErrRemoteLoginSessionUnknown
	}
	if binding.AccountID != accountID {
		// 会话与账号不匹配：不删除（可能属于另一账号），直接拒绝。
		return nil, ErrRemoteLoginSessionUnknown
	}
	out := binding
	return &out, nil
}

func (s *WebConsoleRemoteLoginService) forgetSession(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

func (s *WebConsoleRemoteLoginService) pruneExpiredLocked(now time.Time) {
	for id, binding := range s.sessions {
		if !now.Before(binding.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
}

func (s *WebConsoleRemoteLoginService) workerAction(ctx context.Context, sessionID, action string, payload map[string]any) error {
	path := fmt.Sprintf("%s/%s/%s", webConsoleRemoteLoginPathPrefix, sessionID, action)
	if err := s.callWorker(ctx, http.MethodPost, path, payload, nil); err != nil {
		return err
	}
	return nil
}

func (s *WebConsoleRemoteLoginService) callWorker(ctx context.Context, method, path string, payload any, out any) error {
	statusCode, err := s.callWorkerRaw(ctx, method, path, payload, out)
	if err != nil {
		return err
	}
	if statusCode < 200 || statusCode >= 300 {
		if statusCode == http.StatusNotFound || statusCode == http.StatusGone {
			return ErrRemoteLoginSessionUnknown
		}
		return ErrRemoteLoginStepFailed
	}
	return nil
}

// callWorkerRaw 执行一次 worker 请求并解码 JSON；返回 HTTP 状态码供调用方分支。
func (s *WebConsoleRemoteLoginService) callWorkerRaw(ctx context.Context, method, path string, payload any, out any) (int, error) {
	if !s.workerReady() {
		return 0, ErrRemoteLoginNotConfigured
	}

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("web console remote login: marshal payload: %w", err)
		}
		body = bytes.NewReader(raw)
	}

	url := strings.TrimRight(strings.TrimSpace(s.cfg.WebQuotaWorker.BaseURL), "/") + path
	reqCtx, cancel := context.WithTimeout(ctx, webConsoleRemoteLoginHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, url, body)
	if err != nil {
		return 0, fmt.Errorf("web console remote login: build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Worker-Token", s.cfg.WebQuotaWorker.APIKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("web console remote login: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, webConsoleRemoteLoginMaxResponseBytes))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("web console remote login: read response: %w", err)
	}
	if out != nil && len(raw) > 0 {
		// 非 2xx 的响应体可能是错误对象，解码失败不视为传输错误。
		if decodeErr := json.Unmarshal(raw, out); decodeErr != nil && resp.StatusCode < 300 {
			return resp.StatusCode, fmt.Errorf("web console remote login: decode response: %w", decodeErr)
		}
	}
	return resp.StatusCode, nil
}
