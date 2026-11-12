package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"golang.org/x/sync/singleflight"
)

// 本文件实现 Web 控制台额度链的编排服务（方案 §3.2 模块 C 第 3 条 + §4.4）。
//
// 职责边界：
//   - 只调 `web-quota-worker` 的 `POST /v1/probe` 这一个上游 HTTP 接口；
//   - 只做「归一（BuildWebConsoleQuotaSnapshot，纯函数）→ 落 extra 快照 → 按归属规则停调/恢复」；
//   - 绝不实现第二份上游用量协议，也绝不把明文 api_key / 密码 / storage_state 写入日志。
//
// 五条不变式（方案 §4.4）的落点：
//  1. 停调原因统一由 BuildTempUnschedReasonPayload(webConsoleQuotaOwnedSource, ...) 生成；
//  2. 本文件只 DeleteTempUnsched，绝不 SetTempUnsched；
//  3. 写入只走 SetTempUnschedulableIfOwnedOrUnpaused（他因活跃停调零写入）；
//  4. 清除只走 ClearTempUnschedulableIfOwned（来源 + 代际双校验）；
//  5. 探针失败 / unmatched / 需人工登录时既不写停调也不清除（失败关闭）。

// ---------------------------------------------------------------------------
// 常量与 provider 端点表
// ---------------------------------------------------------------------------

const (
	// webQuotaWorkerProbePath 是 worker 唯一的采集入口。
	webQuotaWorkerProbePath = "/v1/probe"
	// webQuotaWorkerDefaultTimeoutSeconds 是配置缺省的单账号采集超时。
	webQuotaWorkerDefaultTimeoutSeconds = 60
	// webQuotaWorkerHTTPTimeoutGrace 是 HTTP 客户端超时相对 worker 内部超时的冗余。
	webQuotaWorkerHTTPTimeoutGrace = 10 * time.Second
	// webQuotaWorkerMaxResponseBytes 限制 worker 响应体大小，防异常响应打爆内存。
	webQuotaWorkerMaxResponseBytes = 4 << 20

	// webConsoleQuotaCredentialKeyStorageState / Username / Password 是控制台登录凭据键。
	webConsoleQuotaCredentialKeyStorageState = "console_storage_state"
	webConsoleQuotaCredentialKeyUsername     = "console_username"
	webConsoleQuotaCredentialKeyPassword     = "console_password"
	// webConsoleQuotaCredentialKeyAPIKey 是推理 API Key 的凭据键；本链路只取其 SHA-256。
	webConsoleQuotaCredentialKeyAPIKey = "api_key"

	// webConsoleQuotaNotConfiguredError 是 worker 未配置时的结果错误码。
	webConsoleQuotaNotConfiguredError = "web_quota_worker_not_configured"

	// webConsoleQuotaDetailMaxRunes 限制写入 reason 的中文说明长度。
	webConsoleQuotaDetailMaxRunes = 200
)

// webConsoleQuotaAuthState* 是探测结果与快照共用的认证态取值。
const (
	webConsoleQuotaAuthStateOK                  = "ok"
	webConsoleQuotaAuthStateNeedsInteractive    = "needs_interactive_auth"
	webConsoleQuotaAuthStateAuthFailed          = "auth_failed"
	webConsoleQuotaAuthStateError               = "error"
	webConsoleQuotaPlanMatchStatusUnmatched     = "unmatched"
	webConsoleQuotaWorkerStatusOK               = "ok"
	webConsoleQuotaSingleflightKeyAccountPrefix = "wcq:acct:"
	webConsoleQuotaSingleflightKeyPrefix        = "wcq:"
)

// webConsoleQuotaProviderEndpoints 是 provider → 登录页 / 控制台成功跳转前缀 / 该上游
// 实际支持的登录方式的内置常量表。
// 与 worker 侧适配器一一对应；Go 侧只做透传，不在本地推导页面结构。
//
// loginModes 是「这家上游有没有密码登录」的唯一事实源：worker 适配器据此分支，
// 管理端据此决定渲染哪些输入框。此前该事实只存在于三份 Python 适配器的 if 分支里，
// 管理端拿不到，只能对三家统一渲染「账号 + 密码」，导致 longcat（仅扫码/短信）
// 显示一个填了也不会被使用的密码框。
type webConsoleQuotaProviderEndpoints struct {
	loginURL         string
	successURLPrefix string
	// loginModes 取值见 webConsoleQuotaLoginMode* 常量，按「优先推荐」顺序排列。
	loginModes []string
}

// webConsoleQuotaLoginMode* 是上游登录方式枚举。新增上游时在此扩枚举，
// 不得由消费方各自识别新取值。
const (
	// webConsoleQuotaLoginModePassword 账号 + 密码，worker 可自动登录并续期。
	webConsoleQuotaLoginModePassword = "password"
	// webConsoleQuotaLoginModeQRCode 手机扫码（含 App 确认），必须在会话内人工完成。
	webConsoleQuotaLoginModeQRCode = "qrcode"
	// webConsoleQuotaLoginModeSMS 手机号 + 短信验证码，必须在会话内人工完成。
	webConsoleQuotaLoginModeSMS = "sms"
)

var webConsoleQuotaProviderEndpointTable = map[string]webConsoleQuotaProviderEndpoints{
	providerCompshare: {
		loginURL:         "https://passport.compshare.cn/login",
		successURLPrefix: "https://console.compshare.cn/",
		// 联网实测（2026-11-12）：默认落在「短信登录」，右上角才切「账号登录」；
		// 适配器 _do_login 会先点该 Tab 再填密码，故密码是可选加速项而非唯一入口。
		loginModes: []string{webConsoleQuotaLoginModeSMS, webConsoleQuotaLoginModePassword},
	},
	providerSensenova: {
		loginURL:         "https://platform.sensenova.cn/login",
		successURLPrefix: "https://platform.sensenova.cn/console",
		// 联网实测（2026-11-12）：默认「手机号登录/注册」，另有「账号密码登录」Tab。
		loginModes: []string{webConsoleQuotaLoginModeSMS, webConsoleQuotaLoginModePassword},
	},
	providerLongcat: {
		loginURL:         "https://longcat.chat/platform/usage",
		successURLPrefix: "https://longcat.chat/platform/",
		loginModes:       []string{webConsoleQuotaLoginModeQRCode, webConsoleQuotaLoginModeSMS},
	},
}

// ---------------------------------------------------------------------------
// 对外结构
// ---------------------------------------------------------------------------

// WebConsoleQuotaProbeResult 是单账号一次探测的结果（管理端与周期链消费）。
type WebConsoleQuotaProbeResult struct {
	Provider      string                   `json:"provider"`
	Success       bool                     `json:"success"`
	AuthState     string                   `json:"auth_state"` // ok / needs_interactive_auth / auth_failed / error
	AuthReason    string                   `json:"auth_reason,omitempty"`
	AuthError     string                   `json:"auth_error,omitempty"`
	Exhausted     bool                     `json:"exhausted"`
	Until         string                   `json:"until,omitempty"`
	Persisted     bool                     `json:"persisted"`
	FetchedAt     int64                    `json:"fetched_at"`
	Error         string                   `json:"error,omitempty"`
	SkillSnapshot *WebConsoleQuotaSnapshot `json:"snapshot,omitempty"`
}

// WebConsoleQuotaService 编排「探测 → 快照 → 停调/恢复」。
type WebConsoleQuotaService struct {
	accountRepo AccountRepository
	httpClient  *http.Client
	cfg         *config.Config
	flight      singleflight.Group

	mu           sync.Mutex
	accountLocks map[int64]*sync.Mutex

	tempUnschedCache TempUnschedCache
}

// NewWebConsoleQuotaService 构造编排服务。httpClient 无内置超时，单次请求超时由
// worker 的 timeout_seconds + 10s 通过 context 控制，便于按配置调整。
func NewWebConsoleQuotaService(accountRepo AccountRepository, cfg *config.Config) *WebConsoleQuotaService {
	return &WebConsoleQuotaService{
		accountRepo:  accountRepo,
		httpClient:   &http.Client{},
		cfg:          cfg,
		accountLocks: make(map[int64]*sync.Mutex),
	}
}

// SetTempUnschedCache 注入 Redis 停调缓存（可选；未注入时跳过缓存失效，DB 仍为权威）。
//
// 本服务只会在所属权写入/清除真实生效后调用 DeleteTempUnsched，绝不 SetTempUnsched：
// 缓存只是读侧加速，权威值永远来自 DB 的 temp_unschedulable_until/reason。
func (s *WebConsoleQuotaService) SetTempUnschedCache(c TempUnschedCache) {
	s.tempUnschedCache = c
}

// ---------------------------------------------------------------------------
// worker 请求 / 响应
// ---------------------------------------------------------------------------

// webConsoleQuotaWorkerRequest 是发给 worker 的采集请求。
// 安全约束：只携带 api_key_sha256，绝不携带明文 api_key。
type webConsoleQuotaWorkerRequest struct {
	Provider         string `json:"provider"`
	StorageState     string `json:"storage_state,omitempty"`
	ConsoleUsername  string `json:"console_username,omitempty"`
	ConsolePassword  string `json:"console_password,omitempty"`
	ConsoleLoginURL  string `json:"console_login_url,omitempty"`
	SuccessURLPrefix string `json:"success_url_prefix,omitempty"`
	APIKeySHA256     string `json:"api_key_sha256,omitempty"`
	ProxyURL         string `json:"proxy_url,omitempty"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
}

// webConsoleQuotaWorkerResponse 是 worker 的采集响应。
// updated_storage_state 既可能是 Playwright storageState 对象，也可能是已序列化字符串，
// 故用 RawMessage 承接后再归一为字符串落库。
type webConsoleQuotaWorkerResponse struct {
	Status              string          `json:"status"`
	Provider            string          `json:"provider"`
	UpdatedStorageState json.RawMessage `json:"updated_storage_state"`
	AccountLabel        string          `json:"account_label"`
	Data                json.RawMessage `json:"data"`
	AuthReason          string          `json:"auth_reason"`
	Error               string          `json:"error"`
}

// ---------------------------------------------------------------------------
// 探测入口
// ---------------------------------------------------------------------------

// ProbeAccount 按账号 ID 加载账号后探测。加载失败是真实错误（区别于「不适用」）。
func (s *WebConsoleQuotaService) ProbeAccount(ctx context.Context, accountID int64) (*WebConsoleQuotaProbeResult, error) {
	if s == nil || s.accountRepo == nil {
		return nil, fmt.Errorf("web console quota service: nil account repository")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.ProbeLoadedAccount(ctx, account)
}

// ProbeWithStorageState 用「尚未落库」的 storageState 跑一次完整探测（含快照落库）。
//
// 存在的理由：判断「人是不是真的登进去了」，唯一可靠的判据是**能不能读到用量**。
// 早期用「当前网址落在控制台前缀内」来判断，会被上游「登录失败后跳回原页」骗过，
// 把一份未登录的会话当成有效票据存下来（2026-11-13 线上：美团扫码失败，界面却提示
// 「已检测到登录完成」）。所以登录链路必须先拿候选会话探一次，读到用量才落库。
func (s *WebConsoleQuotaService) ProbeWithStorageState(
	ctx context.Context, account *Account, storageState string,
) (*WebConsoleQuotaProbeResult, error) {
	if s == nil || account == nil {
		return nil, nil
	}
	provider := ResolveWebConsoleQuotaProvider(account)
	if provider == "" {
		return nil, nil
	}
	if s.cfg == nil || strings.TrimSpace(s.cfg.WebQuotaWorker.BaseURL) == "" {
		return &WebConsoleQuotaProbeResult{
			Provider:  provider,
			Success:   false,
			AuthState: webConsoleQuotaAuthStateError,
			FetchedAt: time.Now().Unix(),
			Error:     webConsoleQuotaNotConfiguredError,
		}, nil
	}

	apiKeySHA256 := sha256Hex([]byte(account.GetCredential(webConsoleQuotaCredentialKeyAPIKey)))
	timeoutSeconds := webQuotaWorkerDefaultTimeoutSeconds
	if s.cfg.WebQuotaWorker.TimeoutSeconds > 0 {
		timeoutSeconds = s.cfg.WebQuotaWorker.TimeoutSeconds
	}

	resp, err := s.postProbeWithState(ctx, account, provider, apiKeySHA256, timeoutSeconds, storageState)
	if err != nil {
		return &WebConsoleQuotaProbeResult{
			Provider:  provider,
			AuthState: webConsoleQuotaAuthStateError,
			FetchedAt: time.Now().Unix(),
			Error:     err.Error(),
		}, nil
	}

	lock := s.accountLock(account.ID)
	lock.Lock()
	defer lock.Unlock()
	return s.applyProbeResponse(ctx, account, provider, resp, apiKeySHA256, time.Now()), nil
}

// ProbeLoadedAccount 对已加载账号执行一次探测。
// provider 不适用时返回 (nil, nil)：这不是错误，调用方据此跳过。
func (s *WebConsoleQuotaService) ProbeLoadedAccount(ctx context.Context, account *Account) (*WebConsoleQuotaProbeResult, error) {
	if account == nil {
		return nil, nil
	}
	provider := ResolveWebConsoleQuotaProvider(account)
	if provider == "" {
		// 不属本链路：既不探测也不报错。
		return nil, nil
	}

	// worker 未配置：返回结构化错误结果，不写 extra、不动停调。
	if s.cfg == nil || strings.TrimSpace(s.cfg.WebQuotaWorker.BaseURL) == "" {
		return &WebConsoleQuotaProbeResult{
			Provider:  provider,
			Success:   false,
			AuthState: webConsoleQuotaAuthStateError,
			FetchedAt: time.Now().Unix(),
			Error:     webConsoleQuotaNotConfiguredError,
		}, nil
	}

	apiKeySHA256 := sha256Hex([]byte(account.GetCredential(webConsoleQuotaCredentialKeyAPIKey)))
	timeoutSeconds := webQuotaWorkerDefaultTimeoutSeconds
	if s.cfg.WebQuotaWorker.TimeoutSeconds > 0 {
		timeoutSeconds = s.cfg.WebQuotaWorker.TimeoutSeconds
	}

	key := s.probeSingleflightKey(provider, account, apiKeySHA256)

	v, err, _ := s.flight.Do(key, func() (any, error) {
		// 单飞只合并 worker 网络往返；账号级归一/落库/调度必须在 Do 返回后
		// 由每个调用账号各自执行，否则同控制台兄弟账号会拿到首个账号的编排结果。
		return s.fetchProbeResponse(ctx, account, provider, apiKeySHA256, timeoutSeconds)
	})
	if err != nil {
		// 传输/协议层失败：失败关闭，不写 extra、不动停调。
		return &WebConsoleQuotaProbeResult{
			Provider:  provider,
			AuthState: webConsoleQuotaAuthStateError,
			FetchedAt: time.Now().Unix(),
			Error:     err.Error(),
		}, nil
	}

	resp, ok := v.(*webConsoleQuotaWorkerResponse)
	if !ok || resp == nil {
		return &WebConsoleQuotaProbeResult{
			Provider:  provider,
			AuthState: webConsoleQuotaAuthStateError,
			FetchedAt: time.Now().Unix(),
			Error:     "web_quota_worker_probe_failed",
		}, nil
	}

	// 同账号调度写入串行化：同一账号仍可能被不同调用方以不同 key 命中
	// （username / api_key 变更竞态），这里按 accountID 加锁，保证停调读-判-写
	// 不被同账号并发探测交织。兄弟账号 ID 不同，互不阻塞。
	lock := s.accountLock(account.ID)
	lock.Lock()
	defer lock.Unlock()
	return s.applyProbeResponse(ctx, account, provider, resp, apiKeySHA256, time.Now()), nil
}

// probeSingleflightKey 构造单飞键。
//
// key 含 provider + lower(username) + api_key 哈希三个维度：同控制台账号的兄弟账号
// 各自持有不同 api_key，compshare 多套餐需按 key 精确匹配套餐，若只按账号合并，
// 兄弟账号会拿到首个账号的套餐判定（错配）且只有首个账号被归一/落库/调度。
// 合并只应发生在「同上游 + 同控制台账号 + 同 API Key」的重复探测（周期 + 手动刷新）。
// 缺 username 时按 accountID 隔离，不再叠加 key 维度。
//
// apiKeySHA256 用可变参数：生产路径（ProbeLoadedAccount）恒传哈希；既有按键用例仍以
// 两参数调用（等价于「不区分 key」），据此保持既有断言不变。
func (s *WebConsoleQuotaService) probeSingleflightKey(provider string, account *Account, apiKeySHA256 ...string) string {
	username := strings.TrimSpace(account.GetCredential(webConsoleQuotaCredentialKeyUsername))
	if username == "" {
		return fmt.Sprintf("%s%d", webConsoleQuotaSingleflightKeyAccountPrefix, account.ID)
	}
	key := webConsoleQuotaSingleflightKeyPrefix + provider + ":" + strings.ToLower(username)
	if len(apiKeySHA256) > 0 {
		if hash := strings.TrimSpace(apiKeySHA256[0]); hash != "" {
			key += ":" + hash
		}
	}
	return key
}

// accountLock 懒创建并返回 accountID 对应的互斥锁。
func (s *WebConsoleQuotaService) accountLock(accountID int64) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accountLocks == nil {
		s.accountLocks = make(map[int64]*sync.Mutex)
	}
	lock, ok := s.accountLocks[accountID]
	if !ok {
		lock = &sync.Mutex{}
		s.accountLocks[accountID] = lock
	}
	return lock
}

// ---------------------------------------------------------------------------
// 探测主流程
// ---------------------------------------------------------------------------

// fetchProbeResponse 执行一次 worker 请求（可被 singleflight 合并）。
// 只做网络往返，不做任何账号级归一/落库/调度动作——后者必须对每个调用账号各自执行。
func (s *WebConsoleQuotaService) fetchProbeResponse(
	ctx context.Context, account *Account, provider, apiKeySHA256 string, timeoutSeconds int,
) (*webConsoleQuotaWorkerResponse, error) {
	return s.postProbe(ctx, account, provider, apiKeySHA256, timeoutSeconds)
}

// applyProbeResponse 把 worker 响应应用到「当前账号」：按状态决定是否回写 storage_state，
// 成功时归一快照、落库并执行调度联动。resp 可能被多个调用方共享，**只读，不得修改**。
func (s *WebConsoleQuotaService) applyProbeResponse(
	ctx context.Context, account *Account, provider string, resp *webConsoleQuotaWorkerResponse,
	apiKeySHA256 string, now time.Time,
) *WebConsoleQuotaProbeResult {
	result := &WebConsoleQuotaProbeResult{
		Provider:  provider,
		FetchedAt: now.Unix(),
	}

	status := strings.TrimSpace(resp.Status)
	if status == webConsoleQuotaWorkerStatusOK {
		// 步骤 8：storage_state 回写（失败仅记日志，不中断探测）。
		// 只有成功响应才刷新会话凭据：需人工登录 / 认证失败 / 未知状态时凭据不可信。
		s.persistUpdatedStorageState(ctx, account, resp)
		return s.handleProbeOK(ctx, account, provider, resp, apiKeySHA256, now, result)
	}

	// 步骤 10：非 ok（needs_interactive_auth / auth_failed / error）——保留历史数值字段，
	// 只更新认证态；绝不回写凭据，也绝不调用任何停调写入/清除原语（失败关闭）。
	return s.handleProbeNotOK(ctx, account, provider, resp, status, now, result)
}

// postProbe 调用 worker 的 POST /v1/probe。
func (s *WebConsoleQuotaService) postProbe(ctx context.Context, account *Account, provider, apiKeySHA256 string, timeoutSeconds int) (*webConsoleQuotaWorkerResponse, error) {
	return s.postProbeWithState(ctx, account, provider, apiKeySHA256, timeoutSeconds, "")
}

// postProbeWithState 同上，但允许用「候选 storageState」覆盖账号里已保存的那份。
// storageState 为空时仍取账号凭据（生产探测路径行为不变）。
func (s *WebConsoleQuotaService) postProbeWithState(
	ctx context.Context, account *Account, provider, apiKeySHA256 string, timeoutSeconds int, storageState string,
) (*webConsoleQuotaWorkerResponse, error) {
	stateToUse := strings.TrimSpace(storageState)
	if stateToUse == "" {
		stateToUse = account.GetCredential(webConsoleQuotaCredentialKeyStorageState)
	}
	payload := webConsoleQuotaWorkerRequest{
		Provider:        provider,
		StorageState:    stateToUse,
		ConsoleUsername: account.GetCredential(webConsoleQuotaCredentialKeyUsername),
		ConsolePassword: account.GetCredential(webConsoleQuotaCredentialKeyPassword),
		APIKeySHA256:    apiKeySHA256,
		TimeoutSeconds:  timeoutSeconds,
	}
	if endpoints, ok := webConsoleQuotaProviderEndpointTable[provider]; ok {
		payload.ConsoleLoginURL = endpoints.loginURL
		payload.SuccessURLPrefix = endpoints.successURLPrefix
	}
	if account.Proxy != nil {
		payload.ProxyURL = account.Proxy.URL()
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("web console quota: marshal probe request: %w", err)
	}

	timeout := time.Duration(timeoutSeconds)*time.Second + webQuotaWorkerHTTPTimeoutGrace
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := strings.TrimRight(strings.TrimSpace(s.cfg.WebQuotaWorker.BaseURL), "/") + webQuotaWorkerProbePath
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("web console quota: build probe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Worker-Token", s.cfg.WebQuotaWorker.APIKey)

	httpResp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web console quota: probe request failed: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, webQuotaWorkerMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("web console quota: read probe response: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// 不把响应体拼进错误：worker 可能回显上游报错，体量不可控。
		return nil, fmt.Errorf("web console quota: probe returned HTTP %d", httpResp.StatusCode)
	}

	var parsed webConsoleQuotaWorkerResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("web console quota: decode probe response: %w", err)
	}
	return &parsed, nil
}

// persistUpdatedStorageState 用事务级局部更新原语回写刷新后的 storageState。
// 仅当 worker 返回非空值时才写；console_username 只在本地为空时补写（绝不覆盖已有非空值）。
// 本账号写入成功后，若新值确与库内现值不同，再把新会话同步给同控制台账号的兄弟（§3.1-4）。
func (s *WebConsoleQuotaService) persistUpdatedStorageState(ctx context.Context, account *Account, resp *webConsoleQuotaWorkerResponse) {
	newState := normalizeWebConsoleQuotaStorageState(resp.UpdatedStorageState)
	if newState == "" || s.accountRepo == nil {
		return
	}
	currentState := strings.TrimSpace(account.GetCredential(webConsoleQuotaCredentialKeyStorageState))
	currentUsername := account.GetCredential(webConsoleQuotaCredentialKeyUsername)
	patch := map[string]any{webConsoleQuotaCredentialKeyStorageState: newState}
	if strings.TrimSpace(currentUsername) == "" {
		if label := strings.TrimSpace(resp.AccountLabel); label != "" {
			patch[webConsoleQuotaCredentialKeyUsername] = label
		}
	}
	if err := s.accountRepo.PatchAccountCredentials(ctx, account.ID, patch, currentUsername); err != nil {
		// 只记账号与错误，绝不打印 storage_state / 密码。
		slog.Warn("web_console_quota_patch_storage_state_failed", "account_id", account.ID, "error", err)
		return
	}
	if currentState == newState {
		// 会话未变：跳过兄弟同步，避免每轮探测对全表 ListActive 做无谓扫描。
		return
	}
	syncWebConsoleStorageStateToSiblings(ctx, s.accountRepo, account, currentUsername, newState)
}

// syncWebConsoleStorageStateToSiblings 把新会话补写到同 (provider, lower(console_username))
// 的活跃兄弟账号（方案 §3.1-4）。仅写 console_storage_state 一个键，绝不触碰
// api_key / base_url / model_mapping / console_password。失败只记日志，不回滚本账号写入。
func syncWebConsoleStorageStateToSiblings(
	ctx context.Context,
	repo AccountRepository,
	origin *Account,
	username string,
	storageState string,
) {
	if repo == nil || origin == nil {
		return
	}
	username = strings.TrimSpace(username)
	if username == "" || strings.TrimSpace(storageState) == "" {
		return // 空用户名严格按 account_id 隔离
	}
	provider := ResolveWebConsoleQuotaProvider(origin)
	if provider == "" {
		return
	}
	accounts, err := repo.ListActive(ctx)
	if err != nil {
		slog.Warn("web_console_quota_list_siblings_failed", "account_id", origin.ID, "error", err)
		return
	}
	for i := range accounts {
		sibling := &accounts[i]
		if sibling.ID == origin.ID {
			continue
		}
		if ResolveWebConsoleQuotaProvider(sibling) != provider {
			continue
		}
		siblingUsername := strings.TrimSpace(sibling.GetCredential(webConsoleQuotaCredentialKeyUsername))
		if siblingUsername == "" || !strings.EqualFold(siblingUsername, username) {
			continue
		}
		patch := map[string]any{webConsoleQuotaCredentialKeyStorageState: storageState}
		// expectedUsername 传兄弟账号自己的库内值（大小写可能不同），命中才写。
		if err := repo.PatchAccountCredentials(ctx, sibling.ID, patch, siblingUsername); err != nil {
			slog.Warn("web_console_quota_sync_sibling_failed", "account_id", sibling.ID, "error", err)
		}
	}
}

// normalizeWebConsoleQuotaStorageState 把 worker 的 updated_storage_state 归一为落库字符串。
// 对象形态序列化为紧凑 JSON；字符串形态取其字面值；其余（缺失/null）返回空。
func normalizeWebConsoleQuotaStorageState(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var str string
		if err := json.Unmarshal(trimmed, &str); err != nil {
			return ""
		}
		return strings.TrimSpace(str)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err != nil {
		return ""
	}
	return compact.String()
}

// handleProbeOK 处理 worker status == "ok"：归一快照 → 落 extra → 调度联动。
func (s *WebConsoleQuotaService) handleProbeOK(
	ctx context.Context,
	account *Account,
	provider string,
	resp *webConsoleQuotaWorkerResponse,
	apiKeySHA256 string,
	now time.Time,
	result *WebConsoleQuotaProbeResult,
) *WebConsoleQuotaProbeResult {
	snap, err := BuildWebConsoleQuotaSnapshot(provider, resp.Data, apiKeySHA256, account.GetModelMapping(), now)
	if err != nil {
		// 归一失败：不写 extra、不动停调（失败关闭）。
		result.AuthState = webConsoleQuotaAuthStateError
		result.Error = "web_console_quota_snapshot_build_failed"
		slog.Warn("web_console_quota_snapshot_build_failed", "account_id", account.ID, "provider", provider, "error", err)
		return result
	}
	snap.AuthState = webConsoleQuotaAuthStateOK
	if label := strings.TrimSpace(resp.AccountLabel); label != "" {
		snap.AccountLabel = label
	}

	result.Success = true
	result.AuthState = webConsoleQuotaAuthStateOK
	result.SkillSnapshot = snap
	result.Exhausted = snap.Exhausted
	result.Until = snap.Until
	result.Persisted = s.persistSnapshot(ctx, account.ID, snap)
	if !result.Persisted {
		// 失败关闭：本次判定无法留痕时不得写停调/清除，避免「有停调无快照」的不可观测状态。
		slog.Warn("web_console_quota_snapshot_persist_failed_skip_scheduling", "account_id", account.ID)
		return result
	}

	s.applyScheduling(ctx, account, snap)
	return result
}

// handleProbeNotOK 处理 worker status != "ok"：保留历史数值字段，只更新认证态。
func (s *WebConsoleQuotaService) handleProbeNotOK(
	ctx context.Context,
	account *Account,
	provider string,
	resp *webConsoleQuotaWorkerResponse,
	status string,
	now time.Time,
	result *WebConsoleQuotaProbeResult,
) *WebConsoleQuotaProbeResult {
	authState := status
	switch status {
	case webConsoleQuotaAuthStateNeedsInteractive, webConsoleQuotaAuthStateAuthFailed, webConsoleQuotaAuthStateError:
		// 合法取值，直接使用。
	default:
		// 未知/空状态一律归入 error，避免把无意义字符串写进快照。
		authState = webConsoleQuotaAuthStateError
	}
	// resp 可能被多个调用账号共享：错误文案写本地副本，绝不改写 resp。
	authError := resp.Error
	if authState == webConsoleQuotaAuthStateError && authError == "" {
		authError = "web_quota_worker_status_" + status
	}

	snap := s.loadExistingSnapshot(account)
	if snap == nil {
		// 无历史快照：建立一个只有认证态的骨架，供前端提示「需要登录」。
		snap = &WebConsoleQuotaSnapshot{Provider: provider}
	}
	snap.Provider = provider
	snap.AuthState = authState
	snap.AuthReason = resp.AuthReason
	snap.AuthError = authError
	snap.FetchedAt = now.UTC().Format(time.RFC3339)
	if label := strings.TrimSpace(resp.AccountLabel); label != "" {
		snap.AccountLabel = label
	}

	// 只写快照，绝不触碰停调写入/清除原语。
	result.Success = false
	result.AuthState = authState
	result.AuthReason = snap.AuthReason
	result.AuthError = snap.AuthError
	result.Error = authError
	result.Exhausted = snap.Exhausted
	result.Until = snap.Until
	result.SkillSnapshot = snap
	result.Persisted = s.persistSnapshot(ctx, account.ID, snap)
	return result
}

// loadExistingSnapshot 从账号 extra 读取现有快照；缺失/结构不符返回 nil。
func (s *WebConsoleQuotaService) loadExistingSnapshot(account *Account) *WebConsoleQuotaSnapshot {
	extra := account.SnapshotExtra()
	if extra == nil {
		return nil
	}
	raw, ok := extra[webConsoleQuotaSnapshotExtraKey]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var snap WebConsoleQuotaSnapshot
	if err := json.Unmarshal(encoded, &snap); err != nil {
		return nil
	}
	return &snap
}

// persistSnapshot 通过 extra 合并写快照，返回是否写入成功。
func (s *WebConsoleQuotaService) persistSnapshot(ctx context.Context, accountID int64, snap *WebConsoleQuotaSnapshot) bool {
	if s.accountRepo == nil || snap == nil {
		return false
	}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{webConsoleQuotaSnapshotExtraKey: snap}); err != nil {
		slog.Warn("web_console_quota_snapshot_persist_failed", "account_id", accountID, "error", err)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 调度联动（§4.4）
// ---------------------------------------------------------------------------

// applyScheduling 依据快照执行停调/恢复。
//
// 逐条实现 §4.4 不变式：
//   - unmatched：失败关闭，不写停调也不清除；
//   - 未耗尽：仅当当前停调来源属本链且 until 代际匹配时才清除；
//   - 耗尽：只走 SetTempUnschedulableIfOwnedOrUnpaused（他因活跃停调零写入）。
//
// 缓存一律只 Delete（让后续读取回落到 DB 权威值），绝不 Set。
func (s *WebConsoleQuotaService) applyScheduling(ctx context.Context, account *Account, snap *WebConsoleQuotaSnapshot) {
	if account == nil || snap == nil {
		return
	}
	if snap.PlanMatchStatus == webConsoleQuotaPlanMatchStatusUnmatched {
		return
	}

	if !snap.Exhausted {
		s.applyRecovery(ctx, account)
		return
	}

	until := webConsoleQuotaResolveUntil(snap)
	reason := BuildTempUnschedReasonPayload(webConsoleQuotaOwnedSource, webConsoleQuotaBuildDetail(snap))
	applied, err := s.accountRepo.SetTempUnschedulableIfOwnedOrUnpaused(ctx, account.ID, until, reason, webConsoleQuotaOwnedSource)
	if err != nil {
		slog.Warn("web_console_quota_set_temp_unsched_failed", "account_id", account.ID, "error", err)
		return
	}
	if !applied {
		// 他因活跃停调：零写入，缓存本就不变。
		return
	}
	s.deleteTempUnschedCache(ctx, account.ID)
}

// applyRecovery 在探针确认有额度时清除本链自己写下的停调。
func (s *WebConsoleQuotaService) applyRecovery(ctx context.Context, account *Account) {
	if account.TempUnschedulableUntil == nil {
		return
	}
	if !IsWebConsoleQuotaTempUnschedReason(account.TempUnschedulableReason) {
		// 他因停调（401/429/人工等）一律不动。
		return
	}
	cleared, err := s.accountRepo.ClearTempUnschedulableIfOwned(ctx, account.ID, *account.TempUnschedulableUntil, webConsoleQuotaOwnedSource)
	if err != nil {
		slog.Warn("web_console_quota_clear_temp_unsched_failed", "account_id", account.ID, "error", err)
		return
	}
	if !cleared {
		return
	}
	s.deleteTempUnschedCache(ctx, account.ID)
}

// deleteTempUnschedCache 在所属权写入/清除真实生效后失效缓存。
func (s *WebConsoleQuotaService) deleteTempUnschedCache(ctx context.Context, accountID int64) {
	if s.tempUnschedCache == nil {
		return
	}
	if err := s.tempUnschedCache.DeleteTempUnsched(ctx, accountID); err != nil {
		slog.Warn("web_console_quota_delete_temp_unsched_cache_failed", "account_id", accountID, "error", err)
	}
}

// webConsoleQuotaResolveUntil 把快照的停调截止时间换算为 time.Time。
// 无限期哨兵或解析失败（官方未给出可用 reset_at）统一取 tempUnschedIndefiniteUntil，
// 与快照层「无未来重置时间 → 无限期直到探针确认恢复」的语义一致。
func webConsoleQuotaResolveUntil(snap *WebConsoleQuotaSnapshot) time.Time {
	if snap.UntilIndefinite {
		return tempUnschedIndefiniteUntil
	}
	if t, ok := parseWebConsoleQuotaResetAt(snap.Until); ok {
		return t
	}
	return tempUnschedIndefiniteUntil
}

// webConsoleQuotaWindowLabels 把窗口标识映射为中文展示名。
var webConsoleQuotaWindowLabels = map[string]string{
	"5h":    "5h",
	"7d":    "7d",
	"week":  "周",
	"month": "月",
}

// webConsoleQuotaBuildDetail 生成简短中文停调说明（≤200 字符，不含任何凭据）。
func webConsoleQuotaBuildDetail(snap *WebConsoleQuotaSnapshot) string {
	if snap == nil {
		return ""
	}
	var b strings.Builder
	if snap.Provider != "" {
		b.WriteString(snap.Provider)
		b.WriteString(" ")
	}

	exhaustedWindow := ""
	for _, tier := range snap.Tiers {
		if tier.Limit > 0 && tier.Used >= tier.Limit {
			exhaustedWindow = tier.Window
			break
		}
	}

	switch {
	case exhaustedWindow != "":
		label := webConsoleQuotaWindowLabels[exhaustedWindow]
		if label == "" {
			label = exhaustedWindow
		}
		b.WriteString(label)
		b.WriteString(" 窗口已用满")
	case snap.PlanMatchStatus == "no_active_plan":
		b.WriteString("无有效订阅套餐")
	case snap.Provider == providerLongcat:
		b.WriteString("Token 资源包与按量余额均已用尽")
	default:
		b.WriteString("额度已用尽")
	}

	if snap.UntilIndefinite {
		b.WriteString("，停调至探活确认恢复")
	} else if t, ok := parseWebConsoleQuotaResetAt(snap.Until); ok {
		b.WriteString("，停调至 ")
		b.WriteString(t.UTC().Format(time.RFC3339))
	} else {
		b.WriteString("，停调至探活确认恢复")
	}
	return truncateWebConsoleQuotaDetail(b.String())
}

// truncateWebConsoleQuotaDetail 按 rune 截断，保证中文说明不超长。
func truncateWebConsoleQuotaDetail(detail string) string {
	runes := []rune(detail)
	if len(runes) <= webConsoleQuotaDetailMaxRunes {
		return detail
	}
	return string(runes[:webConsoleQuotaDetailMaxRunes])
}
