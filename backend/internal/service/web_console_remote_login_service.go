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
	// webConsoleRemoteLoginSessionGrace 是 Go 侧会话有效期相对 worker 的冗余。
	// 会话寿命的唯一事实源是 worker（它报 expires_in / max_age_in）；
	// Go 侧只多给一点余量，避免两边同一秒到期时 Go 先把会话删了。
	webConsoleRemoteLoginSessionGrace = 60 * time.Second
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
	// ExpiresIn 是 worker 报的「还能空闲多久」（秒），滑动窗口，Go 侧据此设有效期。
	ExpiresIn int64 `json:"expires_in"`
	// MaxAgeIn 是 worker 报的绝对上限（秒，从创建起算）。
	MaxAgeIn int64 `json:"max_age_in"`
	// StorageState 只在「会话过期但登录成果被抢救到」时出现，
	// 正常流程里由 worker 的 finish 单独返回，这里不参与常规画面回传。
	StorageState string `json:"storage_state,omitempty"`
	// Fields / Buttons 是页面上的可见输入框与按钮清单。
	// 存在的理由：让用户点缩略图坐标去聚焦输入框不可靠（美团 passport 的
	// 「动态码」框只有 176×18 CSS 像素，缩到面板后约 7 像素高），
	// 改为把页面表单报给前端，由用户按字段填写。
	Fields  []WebConsoleRemoteLoginField  `json:"fields,omitempty"`
	Buttons []WebConsoleRemoteLoginButton `json:"buttons,omitempty"`
	// FormState 说明 Fields/Buttons 这份清单可不可信，取值 ok / unavailable / opening。
	//
	// 为什么不能只看两个数组是否为空：读失败（页面跳转中、主线程卡住、超时）与
	// 「这页确实没有输入框」在空数组上长得一模一样，前端只能把后者当事实显示
	// 「这个页面没有可填的输入框」，用户被卡死且无从下手（2026-11-13 线上投诉）。
	// 读不到就说读不到，让用户点「刷新画面」重试，而不是替页面下结论。
	FormState string `json:"form_state,omitempty"`
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
	// CreatedAt 是会话创建时刻：绝对上限用它算，到点必收，与用户是否还在用无关。
	CreatedAt time.Time
	// ExpiresAt 是「空闲超时」截止点。每次交互都往后推（滑动窗口），
	// 所以只要用户还在面板上操作，Go 侧就不会先一步把会话删了。
	ExpiresAt time.Time
	// Idle 是空闲窗口的长度（每次交互后重新起算）。
	Idle time.Duration
	// MaxAge 是绝对上限的时长（来自 worker），用来限制滑动窗口最多能滑多远。
	MaxAge time.Duration
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
	// 首选登录方式：取 loginModes 的第一个（顺序即优先级，唯一事实源在上游端点表）。
	// worker 打开页面后会替用户点这一下，因为「先落到哪个 Tab」本来就是产品决策，
	// 不该让每个用户进来手动切一次（longcat 默认页是二维码，而扫码在服务端会话里过不了）。
	if len(endpoints.loginModes) > 0 {
		payload["login_mode"] = endpoints.loginModes[0]
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
	now := s.now()
	// 会话寿命以 worker 报回来的值为准（唯一事实源），Go 侧只加冗余。
	// 旧实现两边各写一个数字（worker 10 分钟 / Go 11 分钟），而且都是
	// 「从创建起算的固定窗口」：用户在面板上操作多久都不能延长它，跨线即被删，
	// 登录成果随浏览器一起蒸发，界面还一直说「未登录」（2026-11-13 线上实录）。
	idle := time.Duration(frame.ExpiresIn)*time.Second + webConsoleRemoteLoginSessionGrace
	maxAge := time.Duration(frame.MaxAgeIn)*time.Second + webConsoleRemoteLoginSessionGrace
	if idle <= 0 {
		idle = 15 * time.Minute
	}
	if maxAge < idle {
		maxAge = idle
	}
	s.sessions[frame.SessionID] = remoteLoginBinding{
		AccountID: accountID,
		Provider:  provider,
		CreatedAt: now,
		ExpiresAt: now.Add(idle),
		Idle:      idle,
		MaxAge:    maxAge,
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
	statusCode, err := s.callWorkerRaw(ctx, http.MethodGet, path, nil, &frame)
	if err != nil {
		return nil, err
	}
	if statusCode == http.StatusGone || statusCode == http.StatusNotFound {
		// 会话没了（过期 / 被回收）。但 worker 在关浏览器之前可能已经抢救到了
		// 登录成果——用户登进去过就是登进去过，不能因为会话到点把成果扔掉，
		// 逼他重收一遍短信验证码。这里照常落库，然后如实告诉前端：
		// 「会话过期了，但登录状态已经保住了」。
		if saved, saveErr := s.saveRescuedStorageState(ctx, accountID, &frame); saveErr != nil {
			slog.Warn("web_console_remote_login_rescue_persist_failed", "account_id", accountID, "error", saveErr)
		} else if saved {
			frame.Status = webConsoleRemoteLoginStatusAuthenticated
			frame.Message = "session_expired_but_saved"
			return &frame, nil
		}
		return nil, ErrRemoteLoginSessionUnknown
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, ErrRemoteLoginStepFailed
	}
	return &frame, nil
}

// saveRescuedStorageState 把 worker 抢救回来的登录态落库。
// 只有 worker 明确说「页面确实是登录态」且带回了 storage_state 才算；
// 拿不到就返回 false，由调用方按「会话没了」正常处理。
func (s *WebConsoleRemoteLoginService) saveRescuedStorageState(
	ctx context.Context,
	accountID int64,
	frame *WebConsoleRemoteLoginFrame,
) (bool, error) {
	storageState := strings.TrimSpace(frame.StorageState)
	if storageState == "" || frame.Status != webConsoleRemoteLoginStatusAuthenticated {
		return false, nil
	}
	if s.accountRepo == nil {
		return false, ErrRemoteLoginNotConfigured
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return false, err
	}
	currentUsername := account.GetCredential(webConsoleQuotaCredentialKeyUsername)
	if err := s.persistStorageState(ctx, accountID, account, currentUsername, storageState); err != nil {
		return false, err
	}
	s.forgetSession(frame.SessionID)
	return true, nil
}

// Tap 把一次点击转发到页面（坐标为视口 CSS 像素），返回点击之后的一帧。
func (s *WebConsoleRemoteLoginService) Tap(ctx context.Context, accountID int64, sessionID string, x, y float64) (*WebConsoleRemoteLoginFrame, error) {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return nil, err
	}
	return s.workerActionFrame(ctx, sessionID, "tap", map[string]any{"x": x, "y": y})
}

// Type 把一段文字打到当前焦点元素，返回输入之后的一帧。
func (s *WebConsoleRemoteLoginService) Type(ctx context.Context, accountID int64, sessionID, text string) (*WebConsoleRemoteLoginFrame, error) {
	if len(text) > 512 {
		return nil, fmt.Errorf("web console remote login: text too long")
	}
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return nil, err
	}
	return s.workerActionFrame(ctx, sessionID, "type", map[string]any{"text": text})
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

// 所有动作都走这里：worker 在动作之后会等页面稳定再截图 + 读表单，因此这一帧
// 就是「动作生效之后」的画面。前端直接用，不必再补一次 GET —— 补的那一次
// 只在动作刚生效的瞬间截图，经常还是旧画面，用户看到的就是「点了没反应」。
func (s *WebConsoleRemoteLoginService) workerActionFrame(ctx context.Context, sessionID, action string, payload map[string]any) (*WebConsoleRemoteLoginFrame, error) {
	var frame WebConsoleRemoteLoginFrame
	path := fmt.Sprintf("%s/%s/%s", webConsoleRemoteLoginPathPrefix, sessionID, action)
	if err := s.callWorker(ctx, http.MethodPost, path, payload, &frame); err != nil {
		return nil, err
	}
	return &frame, nil
}

// PressKey 发送一个白名单按键（Enter / Backspace / Tab 等）。
func (s *WebConsoleRemoteLoginService) PressKey(ctx context.Context, accountID int64, sessionID, key string) (*WebConsoleRemoteLoginFrame, error) {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return nil, err
	}
	return s.workerActionFrame(ctx, sessionID, "key", map[string]any{"key": key})
}

// Scroll 滚动画面。
func (s *WebConsoleRemoteLoginService) Scroll(ctx context.Context, accountID int64, sessionID string, deltaY float64) (*WebConsoleRemoteLoginFrame, error) {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return nil, err
	}
	return s.workerActionFrame(ctx, sessionID, "scroll", map[string]any{"delta_y": deltaY})
}

// Reload 重新打开登录页（页面卡住时的逃生口）。
func (s *WebConsoleRemoteLoginService) Reload(ctx context.Context, accountID int64, sessionID string) (*WebConsoleRemoteLoginFrame, error) {
	if _, err := s.boundSession(accountID, sessionID); err != nil {
		return nil, err
	}
	return s.workerActionFrame(ctx, sessionID, "reload", nil)
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
	if statusCode == http.StatusGone || statusCode == http.StatusNotFound {
		// 会话没了：这是「登录会话过期」，不是「服务器故障」。
		// 旧实现在这里一律当 StepFailed（502），前端只能显示成一句含糊的失败，
		// 用户看不出是自己操作太慢、还是系统坏了（2026-11-13 线上）。
		if saved, saveErr := s.saveRescuedStorageState(ctx, accountID, &WebConsoleRemoteLoginFrame{
			SessionID:    sessionID,
			Status:       finish.Status,
			StorageState: finish.StorageState,
		}); saveErr != nil {
			slog.Warn("web_console_remote_login_rescue_persist_failed", "account_id", accountID, "error", saveErr)
		} else if saved {
			return &WebConsoleRemoteLoginResult{
				Status:  "succeeded",
				Message: "session_expired_but_saved",
			}, nil
		}
		return nil, ErrRemoteLoginSessionUnknown
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

	// 会话是否可信，只认一件事：**能不能真的打开控制台**。
	//
	// 这个判据在 worker 的 finish 里已经做过了（verify_effective：另开一页访问
	// 控制台，看会不会被弹回登录页），能走到这里说明已经通过。所以这里不再用
	// 探测结果给会话判死。
	//
	// 探测（读用量）**不参与**保存判定：读不到用量有无数原因——接口改路径、
	// 限流、风控、适配器还没实现——任何一条都不足以推翻「控制台能打开」这个事实。
	// 旧实现在这里拿探测结果否决落库，结果用户刚登好的会话被反复丢掉，
	// 界面一直提示「登录已失效，请重新登录」，重登多少次都一样（2026-11-13 线上）。
	// 判不了就如实说「用量这次没读到」，不许拿它冒充「你没登录」。
	var verifiedProbe *WebConsoleQuotaProbeResult
	var probeWarning string
	if s.quotaService != nil {
		probe, probeErr := s.quotaService.ProbeWithStorageState(ctx, account, storageState)
		if probeErr == nil && probe != nil {
			if probe.AuthState == webConsoleQuotaAuthStateOK {
				verifiedProbe = probe
			} else {
				probeWarning = webConsoleProbeAuthStateReason(probe.AuthState)
			}
		}
	}

	currentUsername := account.GetCredential(webConsoleQuotaCredentialKeyUsername)
	// 只写 login 产物（storage_state）。不把页面标题写进 console_username：
	// 该字段是「控制台账号」语义，参与同级账号匹配与探测合并键，写标题会污染匹配。
	if err := s.persistStorageState(ctx, binding.AccountID, account, currentUsername, storageState); err != nil {
		return nil, err
	}

	s.forgetSession(sessionID)

	result := &WebConsoleRemoteLoginResult{
		Status:       "succeeded",
		AccountLabel: strings.TrimSpace(finish.AccountLabel),
		// 会话已保存；用量读没读到是另一件事，如实带一句，别让用户误以为用量也好了。
		Message: probeWarning,
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

// persistStorageState 把一份登录态写进账号凭据，并同步给同级账号。
//
// 抽出来是因为「保存登录态」现在有两个入口，但必须是同一件事、同一条路：
// - 正常流程：用户点「我已完成登录」/ 登录一生效，worker 的 finish 回传 storage_state；
// - 抢救流程：会话超时被回收，worker 在关浏览器前把成果抢救回来随 410 带出。
// 两个入口如果各写一份保存逻辑，迟早会漂移成「有的能存有的不能存」。
func (s *WebConsoleRemoteLoginService) persistStorageState(
	ctx context.Context,
	accountID int64,
	account *Account,
	currentUsername string,
	storageState string,
) error {
	patch := map[string]any{webConsoleQuotaCredentialKeyStorageState: storageState}
	if err := s.accountRepo.PatchAccountCredentials(ctx, accountID, patch, currentUsername); err != nil {
		// 只记账号与错误类型，绝不打印 storage_state。
		slog.Warn("web_console_remote_login_persist_failed", "account_id", accountID, "error", err)
		return fmt.Errorf("web console remote login: persist storage state: %w", err)
	}
	syncWebConsoleStorageStateToSiblings(ctx, s.accountRepo, account, currentUsername, storageState)
	return nil
}

// webConsoleProbeAuthStateReason 把探测的 auth state 翻成给前端的一个稳定标签
// （不是给人看的句子；前端按标签显示人话）。
//
// 只用于「会话已保存但用量没读到」的提示，绝不用于判死会话。
func webConsoleProbeAuthStateReason(authState string) string {
	switch strings.TrimSpace(authState) {
	case webConsoleQuotaAuthStateNeedsInteractive:
		return "probe_needs_interactive"
	case webConsoleQuotaAuthStateAuthFailed:
		return "probe_auth_failed"
	case "":
		return "probe_unavailable"
	default:
		return "probe_" + strings.TrimSpace(authState)
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
	// 滑动续期：只要用户还在这个面板上操作，空闲截止点就往后推。
	// 但绝不越过绝对上限（CreatedAt + MaxAge）——窗口可以滑，寿命不能无限长。
	binding.ExpiresAt = now.Add(binding.Idle)
	if hardLimit := binding.CreatedAt.Add(binding.MaxAge); binding.ExpiresAt.After(hardLimit) {
		binding.ExpiresAt = hardLimit
	}
	s.sessions[trimmed] = binding
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
		// 两条规则：空闲到期（滑动窗口没人续）或者绝对上限到了（到点必收）。
		hardLimit := binding.CreatedAt.Add(binding.MaxAge)
		if !now.Before(binding.ExpiresAt) || !now.Before(hardLimit) {
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
