package service

// TokenHarbor 订阅窗口（hasPass/passName/renewsAt/spendAfterAllowance）探测服务。
//
// 订阅状态只存在于 TH 官网登录后的 /dashboard/billing 页（无 REST 接口），探测
// 流程移植自 Python 侧已实扫验证的实现（38 账号全量成功，见
// /home/zjy/tokenharbor-provisioner/provisioner/shared.py 与 /tmp/th_pass_scan.py）：
//
//  1. GET /login 从静态页 HTML 现抓 Next.js server action id（$ACTION_1:0）与
//     ACTION_KEY，并从页面引用的 JS chunk 里按 createServerReference(..., "signIn")
//     现抓 Next-Action 头的客户端 action id——三者部署即变，禁止写死；
//  2. POST /login 发登录 server action（multipart，边界 -thbound），必须带
//     Next-Action 头；成功判据是 HTTP 303 + 分片 Set-Cookie（Supabase 会话
//     sb-auth-auth-token.0/.1… 多片下发，须全部拼起来）；
//  3. 带会话 cookie GET /dashboard/billing，正则解析订阅字段（兼容 RSC 转义
//     形态 \"hasPass\":false 与普通形态）。
//
// 纪律（与 Python 侧同一套）：全程走账号绑定代理（TH 对数据中心 IP 有风控），
// 无代理即失败关闭；解析不到字段如实报错，禁止兜底假值；日志与错误信息不带
// 密码/cookie。

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// TokenHarborPassSnapshotExtraKey 账号 Extra 中存放订阅快照的键。
	TokenHarborPassSnapshotExtraKey = "th_pass_snapshot"
	// TokenHarborUsageSnapshotExtraKey 账号 Extra 中存放用量窗口快照的键
	//（th_usage_snapshot，§4.3 契约）。
	TokenHarborUsageSnapshotExtraKey = "th_usage_snapshot"
	// TokenHarborPassProviderName 快照 provider 字段的固定取值。
	TokenHarborPassProviderName = "tokenharbor_pass"

	tokenHarborBaseURL          = "https://tokenharbor.ai"
	tokenHarborSessionTTL       = 6 * time.Hour
	tokenHarborProbeMinInterval = 10 * time.Minute
	tokenHarborMaxBodyBytes     = 5 << 20
	tokenHarborBoundary         = "-thbound"
	tokenHarborBrowserUA        = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// TokenHarborPassSnapshot 订阅窗口探测快照（字段名冻结，前端按此消费）。
type TokenHarborPassSnapshot struct {
	Provider            string     `json:"provider"`
	HasPass             bool       `json:"has_pass"`
	PassName            string     `json:"pass_name"`
	RenewsAt            *time.Time `json:"renews_at"`
	SpendAfterAllowance bool       `json:"spend_after_allowance"`
	AutoReloadEnabled   bool       `json:"auto_reload_enabled"`
	FetchedAt           time.Time  `json:"fetched_at"`
}

// TokenHarborUsageWindowTotals 单窗口聚合（§4.3 th_usage_snapshot.windows
// 键名照抄：requests/tokens_in/tokens_out）。浮点只做计数，无金额运算（L6）。
type TokenHarborUsageWindowTotals struct {
	Requests  float64 `json:"requests"`
	TokensIn  float64 `json:"tokens_in"`
	TokensOut float64 `json:"tokens_out"`
}

// TokenHarborUsageSnapshot 用量窗口快照（§4.3 契约：windows 三键齐全 today/7d/30d）。
type TokenHarborUsageSnapshot struct {
	Windows   map[string]TokenHarborUsageWindowTotals `json:"windows"`
	FetchedAt time.Time                               `json:"fetched_at"`
}

type tokenHarborSession struct {
	cookie  string
	loginAt time.Time
}

type tokenHarborHTTPResponse struct {
	status   int
	body     string
	cookie   string
	location string
}

// TokenHarborPassService 探测 TH 账号订阅窗口并落快照。
type TokenHarborPassService struct {
	accountRepo AccountRepository
	proxyRepo   ProxyRepository
	upstream    HTTPUpstream
	now         func() time.Time
	// baseURL 站点根；生产固定 tokenHarborBaseURL，测试注入 httptest 假站点。
	baseURL string

	mu       sync.Mutex
	sessions map[int64]tokenHarborSession
}

func NewTokenHarborPassService(accountRepo AccountRepository, proxyRepo ProxyRepository, upstream HTTPUpstream) *TokenHarborPassService {
	return &TokenHarborPassService{
		accountRepo: accountRepo,
		proxyRepo:   proxyRepo,
		upstream:    upstream,
		now:         time.Now,
		baseURL:     tokenHarborBaseURL,
		sessions:    map[int64]tokenHarborSession{},
	}
}

// 探测用的容错正则（移植自 /tmp/th_pass_scan.py）：\\? 兼容 RSC 转义形态
// \"hasPass\":false 与普通形态 "hasPass":false。
var (
	tokenHarborHasPassRe  = regexp.MustCompile(`hasPass\\?"\s*:\s*\\?"?(true|false)`)
	tokenHarborRenewsAtRe = regexp.MustCompile(`renewsAt\\?"\s*:\s*\\?"([^"\\]*)`)
	tokenHarborPassNameRe = regexp.MustCompile(`passName\\?"\s*:\s*\\?"([^"\\]*)`)
	// spendAfterAllowance / autoReloadEnabled 页面里有，取 true/false；缺失按
	// 关闭语义处理（无 pass 账号的页面本就不含这些字段）。
	tokenHarborSpendAfterRe = regexp.MustCompile(`spendAfterAllowance\\?"\s*:\s*\\?"?(true|false)`)
	tokenHarborAutoReloadRe = regexp.MustCompile(`autoReloadEnabled\\?"\s*:\s*\\?"?(true|false)`)

	tokenHarborActionIDTagRe  = regexp.MustCompile(`(?i)<input\b[^>]*\bname\s*=\s*["'](1_)?\$ACTION_1:0["'][^>]*>`)
	tokenHarborActionKeyTagRe = regexp.MustCompile(`(?i)<input\b[^>]*\bname\s*=\s*["'](1_)?\$ACTION_KEY["'][^>]*>`)
	tokenHarborInputValueDQRe = regexp.MustCompile(`(?is)\bvalue\s*=\s*"((?:[^"\\]|\\.)*)"`)
	tokenHarborInputValueSQRe = regexp.MustCompile(`(?is)\bvalue\s*=\s*'((?:[^'\\]|\\.)*)'`)
	tokenHarborLoginFormRe    = regexp.MustCompile(`(?i)\bname\s*=\s*["'](1_)?next["']`)
	tokenHarborScriptSrcRe    = regexp.MustCompile(`src="(/_next/static/[^"]+\.js[^"]*)"`)
	// 客户端 server action 注册形态：createServerReference("<40位hex>", ..., "<name>")。
	tokenHarborServerRefRe = regexp.MustCompile(`createServerReference\)?\("([0-9a-f]{40,})"[^)]*?"(\w+)"\)`)
)

// Probe 登录 TH 并解析订阅窗口，返回结构化快照。距上次成功探测不到 10 分钟时
// 直接返回 Extra 里的既有快照（FetchedAt 为快照时间，调用方能分辨），不发任何
// 外部请求——防止 UI 连点触发 TH 风控。快照持久化由调用方经 PersistSnapshot 触发。
func (s *TokenHarborPassService) Probe(ctx context.Context, account *Account) (TokenHarborPassSnapshot, error) {
	if s == nil || s.upstream == nil {
		return TokenHarborPassSnapshot{}, fmt.Errorf("tokenharbor pass service is not configured")
	}
	if account == nil {
		return TokenHarborPassSnapshot{}, fmt.Errorf("tokenharbor pass probe requires an account")
	}
	if !tokenHarborProbeEnabled(account) {
		return TokenHarborPassSnapshot{}, fmt.Errorf("tokenharbor pass probe is not enabled for account %d", account.ID)
	}
	email := strings.TrimSpace(account.GetCredential("th_email"))
	password := account.GetCredential("th_password")
	if email == "" || password == "" {
		return TokenHarborPassSnapshot{}, fmt.Errorf("account %d is missing tokenharbor credentials (th_email/th_password)", account.ID)
	}
	proxyURL := s.resolveProxyURL(ctx, account)
	if proxyURL == "" {
		return TokenHarborPassSnapshot{}, fmt.Errorf("account %d has no bound proxy; tokenharbor pass probe refuses direct access (TH datacenter-IP risk control)", account.ID)
	}

	if snapshot, ok := TokenHarborPassSnapshotFromExtra(account); ok && s.now().Sub(snapshot.FetchedAt) < tokenHarborProbeMinInterval {
		return *snapshot, nil
	}

	cookie, err := s.ensureSession(ctx, account.ID, proxyURL, email, password)
	if err != nil {
		return TokenHarborPassSnapshot{}, err
	}

	resp, err := s.fetchBillingPage(ctx, account.ID, proxyURL, cookie)
	if err == nil && tokenHarborSessionRejected(resp) {
		// 会话失效：清缓存重登一次；再失败按明确错误上报，不兜底。
		s.invalidateSession(account.ID)
		cookie, err = s.loginToTokenHarbor(ctx, account.ID, proxyURL, email, password)
		if err != nil {
			return TokenHarborPassSnapshot{}, err
		}
		s.storeSession(account.ID, cookie)
		resp, err = s.fetchBillingPage(ctx, account.ID, proxyURL, cookie)
	}
	if err != nil {
		return TokenHarborPassSnapshot{}, err
	}
	if resp.status != http.StatusOK {
		return TokenHarborPassSnapshot{}, fmt.Errorf("tokenharbor billing page returned status %d for account %d", resp.status, account.ID)
	}

	return parseTokenHarborPassPage(resp.body, s.now().UTC())
}

// PersistSnapshot 把快照写进账号 Extra（键 th_pass_snapshot），由调用方触发，
// 与 AccountBalanceProbeCheckService 的 UpdateExtra 落库口径一致。
func (s *TokenHarborPassService) PersistSnapshot(ctx context.Context, accountID int64, snapshot TokenHarborPassSnapshot) error {
	if s == nil || s.accountRepo == nil {
		return fmt.Errorf("tokenharbor pass service is not configured")
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{TokenHarborPassSnapshotExtraKey: snapshot})
}

// TokenHarborPassSnapshotFromExtra 从账号 Extra 读回订阅快照（JSON 兼容任意存法）。
func TokenHarborPassSnapshotFromExtra(account *Account) (*TokenHarborPassSnapshot, bool) {
	if account == nil || account.Extra == nil {
		return nil, false
	}
	raw, ok := account.Extra[TokenHarborPassSnapshotExtraKey]
	if !ok || raw == nil {
		return nil, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var snapshot TokenHarborPassSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return nil, false
	}
	return &snapshot, true
}

// PersistUsageSnapshot 把用量窗口快照写进账号 Extra（键 th_usage_snapshot），
// 由调用方触发，与 PersistSnapshot 同一落库口径。
func (s *TokenHarborPassService) PersistUsageSnapshot(ctx context.Context, accountID int64, snapshot TokenHarborUsageSnapshot) error {
	if s == nil || s.accountRepo == nil {
		return fmt.Errorf("tokenharbor pass service is not configured")
	}
	return s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{TokenHarborUsageSnapshotExtraKey: snapshot})
}

// TokenHarborUsageSnapshotFromExtra 从账号 Extra 读回用量窗口快照。
func TokenHarborUsageSnapshotFromExtra(account *Account) (*TokenHarborUsageSnapshot, bool) {
	if account == nil || account.Extra == nil {
		return nil, false
	}
	raw, ok := account.Extra[TokenHarborUsageSnapshotExtraKey]
	if !ok || raw == nil {
		return nil, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var snapshot TokenHarborUsageSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return nil, false
	}
	return &snapshot, true
}

// ProbeUsageSnapshot 登录态 GET /api/usage/export.csv（session 复用 pass 服务的
// 登录链；出站带浏览器 UA、走账号绑定代理，与 Probe 同一套纪律），按
// today/7d/30d 窗聚合官方逐笔行的 requests/tokens_in/tokens_out（浮点只做计数，
// 无金额运算，L6）。距上次成功探测不到 10 分钟时直接返回 Extra 既有快照——
// 防止 UI 连点触发 TH 风控。解析失败=失败关闭返回明确错误，不落快照。
func (s *TokenHarborPassService) ProbeUsageSnapshot(ctx context.Context, account *Account) (TokenHarborUsageSnapshot, error) {
	if s == nil || s.upstream == nil {
		return TokenHarborUsageSnapshot{}, fmt.Errorf("tokenharbor pass service is not configured")
	}
	if account == nil {
		return TokenHarborUsageSnapshot{}, fmt.Errorf("tokenharbor usage probe requires an account")
	}
	if !tokenHarborProbeEnabled(account) {
		return TokenHarborUsageSnapshot{}, fmt.Errorf("tokenharbor usage probe is not enabled for account %d", account.ID)
	}
	email := strings.TrimSpace(account.GetCredential("th_email"))
	password := account.GetCredential("th_password")
	if email == "" || password == "" {
		return TokenHarborUsageSnapshot{}, fmt.Errorf("account %d is missing tokenharbor credentials (th_email/th_password)", account.ID)
	}
	proxyURL := s.resolveProxyURL(ctx, account)
	if proxyURL == "" {
		return TokenHarborUsageSnapshot{}, fmt.Errorf("account %d has no bound proxy; tokenharbor usage probe refuses direct access (TH datacenter-IP risk control)", account.ID)
	}

	if snapshot, ok := TokenHarborUsageSnapshotFromExtra(account); ok && s.now().Sub(snapshot.FetchedAt) < tokenHarborProbeMinInterval {
		return *snapshot, nil
	}

	cookie, err := s.ensureSession(ctx, account.ID, proxyURL, email, password)
	if err != nil {
		return TokenHarborUsageSnapshot{}, err
	}

	resp, err := s.fetchUsageCSV(ctx, account.ID, proxyURL, cookie)
	if err == nil && tokenHarborSessionRejected(resp) {
		// 会话失效：清缓存重登一次；再失败按明确错误上报，不兜底。
		s.invalidateSession(account.ID)
		cookie, err = s.loginToTokenHarbor(ctx, account.ID, proxyURL, email, password)
		if err != nil {
			return TokenHarborUsageSnapshot{}, err
		}
		s.storeSession(account.ID, cookie)
		resp, err = s.fetchUsageCSV(ctx, account.ID, proxyURL, cookie)
	}
	if err != nil {
		return TokenHarborUsageSnapshot{}, err
	}
	if resp.status != http.StatusOK {
		return TokenHarborUsageSnapshot{}, fmt.Errorf("tokenharbor usage export returned status %d for account %d", resp.status, account.ID)
	}
	return parseTokenHarborUsageCSV(resp.body, s.now().UTC())
}

func (s *TokenHarborPassService) fetchUsageCSV(ctx context.Context, accountID int64, proxyURL, cookie string) (*tokenHarborHTTPResponse, error) {
	return s.do(ctx, accountID, proxyURL, cookie, http.MethodGet, s.baseURL+"/api/usage/export.csv", nil, "", map[string]string{
		"Accept": "text/csv,application/json;q=0.9,*/*;q=0.8",
	})
}

// tokenHarborProbeEnabled 读 th_probe_enabled 非密标记（bool，语义同
// IsTempUnschedulableEnabled 的凭据布尔读取）。
func tokenHarborProbeEnabled(account *Account) bool {
	if account == nil || account.Credentials == nil {
		return false
	}
	raw, ok := account.Credentials["th_probe_enabled"]
	if !ok || raw == nil {
		return false
	}
	enabled, ok := raw.(bool)
	return ok && enabled
}

func (s *TokenHarborPassService) resolveProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil {
		return ""
	}
	if account.Proxy != nil {
		return account.Proxy.URL()
	}
	if s != nil && s.proxyRepo != nil {
		if proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && proxy != nil {
			account.Proxy = proxy
			return proxy.URL()
		}
	}
	return ""
}

func (s *TokenHarborPassService) cachedSession(accountID int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[accountID]
	if !ok {
		return "", false
	}
	if s.now().Sub(session.loginAt) >= tokenHarborSessionTTL {
		delete(s.sessions, accountID)
		return "", false
	}
	return session.cookie, true
}

// ensureSession 返回可用会话 cookie：缓存命中直接复用，否则走完整登录链
// （pass 探测与 usage CSV 探测共用同一登录链，§4.3）。
func (s *TokenHarborPassService) ensureSession(ctx context.Context, accountID int64, proxyURL, email, password string) (string, error) {
	if cookie, ok := s.cachedSession(accountID); ok {
		return cookie, nil
	}
	cookie, err := s.loginToTokenHarbor(ctx, accountID, proxyURL, email, password)
	if err != nil {
		return "", err
	}
	s.storeSession(accountID, cookie)
	return cookie, nil
}

func (s *TokenHarborPassService) storeSession(accountID int64, cookie string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[accountID] = tokenHarborSession{cookie: cookie, loginAt: s.now()}
}

func (s *TokenHarborPassService) invalidateSession(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, accountID)
}

// do 经共享上游（带账号代理）发一次请求。一律禁用重定向：登录的 303 成功判据
// 必须原样拿到，billing 的 3xx 跳登录要当作会话失效信号。
func (s *TokenHarborPassService) do(ctx context.Context, accountID int64, proxyURL, cookie, method, rawURL string, body []byte, contentType string, extraHeaders map[string]string) (*tokenHarborHTTPResponse, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, fmt.Errorf("tokenharbor request build failed: %w", err)
	}
	req.Header.Set("User-Agent", tokenHarborBrowserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range extraHeaders {
		req.Header.Set(key, value)
	}
	resp, err := s.upstream.Do(req.WithContext(WithHTTPUpstreamRedirectsDisabled(ctx)), proxyURL, accountID, 1)
	if err != nil {
		return nil, fmt.Errorf("tokenharbor request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, tokenHarborMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("tokenharbor response read failed: %w", err)
	}
	if len(raw) > tokenHarborMaxBodyBytes {
		return nil, fmt.Errorf("tokenharbor response exceeds %d bytes, refusing to read", tokenHarborMaxBodyBytes)
	}
	return &tokenHarborHTTPResponse{
		status:   resp.StatusCode,
		body:     string(raw),
		cookie:   mergeTokenHarborSetCookies(resp.Header.Values("Set-Cookie")),
		location: resp.Header.Get("Location"),
	}, nil
}

// mergeTokenHarborSetCookies 把一次响应的多片 Set-Cookie 逐片取 "name=value" 后
// 以 "; " 拼接（Supabase 会话分片下发，漏片=后续请求 401）。
func mergeTokenHarborSetCookies(headers []string) string {
	shards := make([]string, 0, len(headers))
	for _, header := range headers {
		first := strings.TrimSpace(strings.SplitN(header, ";", 2)[0])
		if first != "" {
			shards = append(shards, first)
		}
	}
	return strings.Join(shards, "; ")
}

// loginToTokenHarbor 走完整登录流程，成功返回拼接好的会话 cookie。
func (s *TokenHarborPassService) loginToTokenHarbor(ctx context.Context, accountID int64, proxyURL, email, password string) (string, error) {
	loginURL := s.baseURL + "/login"
	page, err := s.do(ctx, accountID, proxyURL, "", http.MethodGet, loginURL, nil, "", nil)
	if err != nil {
		return "", err
	}
	if page.status != http.StatusOK {
		return "", fmt.Errorf("tokenharbor login page returned status %d", page.status)
	}
	actionID := extractTokenHarborActionID(page.body)
	if actionID == "" {
		return "", fmt.Errorf("tokenharbor login page has no action id (site shape changed?)")
	}
	actionKey := extractTokenHarborActionKey(page.body)
	if actionKey == "" {
		return "", fmt.Errorf("tokenharbor login page has no ACTION_KEY (site shape changed?)")
	}
	if !tokenHarborLoginFormRe.MatchString(page.body) {
		return "", fmt.Errorf("tokenharbor login page has no login form (next field missing)")
	}
	nextAction, err := s.resolveTokenHarborSignInActionID(ctx, accountID, proxyURL, page.body)
	if err != nil {
		return "", err
	}

	body := tokenHarborBuildActionBody(actionID, actionKey, email, password)
	resp, err := s.do(ctx, accountID, proxyURL, "", http.MethodPost, loginURL, body,
		"multipart/form-data; boundary="+tokenHarborBoundary,
		map[string]string{
			"Origin":      s.baseURL,
			"Referer":     loginURL,
			"Next-Action": nextAction,
		})
	if err != nil {
		return "", err
	}
	switch resp.status {
	case http.StatusSeeOther:
		// 成功判据：303 + 分片会话 cookie（响应体可能是空或 RSC，不作要求）。
		if resp.cookie == "" {
			return "", fmt.Errorf("tokenharbor login action returned 303 without session cookie")
		}
		return resp.cookie, nil
	case http.StatusOK:
		// Next.js server action 密码错也是 200，错误藏在 RSC 流的 error 行里。
		if rscErr := parseTokenHarborRSCError(resp.body); rscErr != "" {
			return "", fmt.Errorf("tokenharbor login rejected: %s", rscErr)
		}
		return "", fmt.Errorf("tokenharbor login action returned 200 without a session")
	default:
		return "", fmt.Errorf("tokenharbor login action returned status %d", resp.status)
	}
}

// resolveTokenHarborSignInActionID 从登录页引用的 JS chunk 里现抓 signIn 的客户端
// action id（Next-Action 头的值；风控收紧后缺失即 500）。
func (s *TokenHarborPassService) resolveTokenHarborSignInActionID(ctx context.Context, accountID int64, proxyURL, pageBody string) (string, error) {
	for _, src := range tokenHarborScriptSrcRe.FindAllStringSubmatch(pageBody, -1) {
		chunk, err := s.do(ctx, accountID, proxyURL, "", http.MethodGet, s.baseURL+src[1], nil, "", nil)
		if err != nil {
			return "", err
		}
		for _, match := range tokenHarborServerRefRe.FindAllStringSubmatch(chunk.body, -1) {
			if match[2] == "signIn" {
				return match[1], nil
			}
		}
	}
	return "", fmt.Errorf("tokenharbor login page JS chunks contain no signIn action id (site redeployed?)")
}

func (s *TokenHarborPassService) fetchBillingPage(ctx context.Context, accountID int64, proxyURL, cookie string) (*tokenHarborHTTPResponse, error) {
	return s.do(ctx, accountID, proxyURL, cookie, http.MethodGet, s.baseURL+"/dashboard/billing", nil, "", nil)
}

// tokenHarborSessionRejected 判定 billing 响应是否说明会话已失效：401，或 3xx
// 跳回登录页。
func tokenHarborSessionRejected(resp *tokenHarborHTTPResponse) bool {
	if resp == nil {
		return false
	}
	if resp.status == http.StatusUnauthorized {
		return true
	}
	return resp.status >= 300 && resp.status < 400 && strings.Contains(resp.location, "/login")
}

// tokenHarborBuildActionBody 构造 Next.js server action 的 multipart 请求体
// （移植自 build_action_roundtrip：1_$ACTION_REF_1 → 空；1_$ACTION_1:0 → action
// JSON；1_$ACTION_1:1 → 值数组；1_$ACTION_KEY → 会话键；登录表单字段带 1_ 前缀；
// 收尾字段 0 用 K1）。
func tokenHarborBuildActionBody(actionID, actionKey, email, password string) []byte {
	var buf bytes.Buffer
	part := func(name, value string) {
		fmt.Fprintf(&buf, "--%s\r\n", tokenHarborBoundary)
		fmt.Fprintf(&buf, "Content-Disposition: form-data; name=\"%s\"\r\n\r\n", name)
		buf.WriteString(value)
		buf.WriteString("\r\n")
	}
	part("1_$ACTION_REF_1", "")
	part("1_$ACTION_1:0", fmt.Sprintf(`{"id":"%s","bound":"$@1"}`, actionID))
	part("1_$ACTION_1:1", `["$undefined"]`)
	part("1_$ACTION_KEY", actionKey)
	part("1_email", email)
	part("1_password", password)
	part("1_next", "")
	part("0", `["$undefined","$K1"]`)
	fmt.Fprintf(&buf, "--%s--\r\n", tokenHarborBoundary)
	return buf.Bytes()
}

func extractTokenHarborActionID(pageBody string) string {
	for _, match := range tokenHarborActionIDTagRe.FindAllString(pageBody, -1) {
		value, ok := tokenHarborInputTagValue(match)
		if !ok {
			continue
		}
		var payload struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(value), &payload); err != nil || payload.ID == "" {
			continue
		}
		return payload.ID
	}
	return ""
}

func extractTokenHarborActionKey(pageBody string) string {
	for _, match := range tokenHarborActionKeyTagRe.FindAllString(pageBody, -1) {
		if value, ok := tokenHarborInputTagValue(match); ok {
			return value
		}
	}
	return ""
}

// tokenHarborInputTagValue 从单个 <input ...> 标签解 value 属性（兼容单双引号，
// 自动反转义 HTML 实体）。
func tokenHarborInputTagValue(tag string) (string, bool) {
	match := tokenHarborInputValueDQRe.FindStringSubmatch(tag)
	if match == nil {
		match = tokenHarborInputValueSQRe.FindStringSubmatch(tag)
	}
	if match == nil {
		return "", false
	}
	return html.UnescapeString(match[1]), true
}

// parseTokenHarborRSCError 从 RSC 流解析服务端 error 文案（行首 <序号>: 后是 JSON）。
func parseTokenHarborRSCError(text string) string {
	for _, line := range strings.Split(text, "\n") {
		chunk := strings.TrimSpace(line)
		if !strings.Contains(chunk, "error") {
			continue
		}
		if prefix, rest, found := strings.Cut(chunk, ":"); found && (isAllDigits(prefix) || strings.HasPrefix(prefix, "$")) {
			chunk = strings.TrimSpace(rest)
		}
		start := strings.Index(chunk, "{")
		end := strings.LastIndex(chunk, "}")
		if start < 0 || end <= start {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(chunk[start:end+1]), &obj); err != nil {
			continue
		}
		if errText, ok := obj["error"].(string); ok && errText != "" {
			return errText
		}
	}
	return ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseTokenHarborPassPage 从 /dashboard/billing 页面文本解析订阅字段。
// hasPass 缺失即报错（站点形态变了或页面没渲染出订阅数据），禁止兜底假值。
func parseTokenHarborPassPage(text string, fetchedAt time.Time) (TokenHarborPassSnapshot, error) {
	match := tokenHarborHasPassRe.FindStringSubmatch(text)
	if match == nil {
		return TokenHarborPassSnapshot{}, fmt.Errorf("tokenharbor billing page has no hasPass field (page len=%d)", len(text))
	}
	snapshot := TokenHarborPassSnapshot{
		Provider:            TokenHarborPassProviderName,
		HasPass:             match[1] == "true",
		FetchedAt:           fetchedAt,
		SpendAfterAllowance: tokenHarborBoolMatch(text, tokenHarborSpendAfterRe),
		AutoReloadEnabled:   tokenHarborBoolMatch(text, tokenHarborAutoReloadRe),
	}
	if m := tokenHarborPassNameRe.FindStringSubmatch(text); m != nil {
		snapshot.PassName = m[1]
	}
	if m := tokenHarborRenewsAtRe.FindStringSubmatch(text); m != nil && m[1] != "" {
		renewsAt, err := time.Parse(time.RFC3339, m[1])
		if err != nil {
			return TokenHarborPassSnapshot{}, fmt.Errorf("tokenharbor renewsAt %q is not RFC3339: %w", m[1], err)
		}
		snapshot.RenewsAt = &renewsAt
	}
	return snapshot, nil
}

func tokenHarborBoolMatch(text string, re *regexp.Regexp) bool {
	match := re.FindStringSubmatch(text)
	return match != nil && match[1] == "true"
}

// tokenHarborUsageColumnAliases usage/export.csv 的表头驱动列匹配别名集
// （列名大小写不敏感、trim 后精确等于任一别名）。官方逐笔行：时间/模型/
// tokens 输入输出/缓存/状态/钱包 Cost——只取时间与 tokens 两列计数，
// 不碰金额列（L6：无 $、无剩余估算）。别名集按实测形态收窄，未命中即失败关闭。
var tokenHarborUsageColumnAliases = map[string][]string{
	"time":       {"timestamp", "time", "created_at", "date", "timestamp (iso utc)"},
	"tokens_in":  {"tokens_in", "input_tokens", "prompt_tokens", "tokens in"},
	"tokens_out": {"tokens_out", "output_tokens", "completion_tokens", "tokens out"},
}

// parseTokenHarborUsageCSV 解析 usage/export.csv 并按窗口聚合（§4.3 契约：
// windows 三键 today/7d/30d 齐全）。
//
// 窗口口径（推导规则，与面板一致）：
//   - today：UTC 当日零点起的日历窗口；
//   - 7d / 30d：滚动 [now-7d/30d, now]；
//   - 一行计入窗口当且仅当 窗口起点 ≤ 行时间 ≤ now；早于 30d 的行不计入任何窗口。
//
// 失败关闭（外审 F3 同口径）：必需列缺失、行时间无法解析/超前于 now、
// tokens 数值无法解析或为负数 → 返回明确错误，调用方不得落快照。
func parseTokenHarborUsageCSV(body string, now time.Time) (TokenHarborUsageSnapshot, error) {
	records, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		return TokenHarborUsageSnapshot{}, fmt.Errorf("tokenharbor usage csv parse failed: %w", err)
	}
	if len(records) == 0 {
		return TokenHarborUsageSnapshot{}, fmt.Errorf("tokenharbor usage csv is empty")
	}
	header := make([]string, len(records[0]))
	for i, name := range records[0] {
		header[i] = strings.ToLower(strings.TrimSpace(name))
	}
	column := make(map[string]int, len(tokenHarborUsageColumnAliases))
	for key, aliases := range tokenHarborUsageColumnAliases {
		for i, name := range header {
			if name == "" {
				continue
			}
			for _, alias := range aliases {
				if name == alias {
					column[key] = i
					break
				}
			}
			if _, ok := column[key]; ok {
				break
			}
		}
		if _, ok := column[key]; !ok {
			return TokenHarborUsageSnapshot{}, fmt.Errorf("tokenharbor usage csv has no %q column (header %q)", key, strings.Join(header, ","))
		}
	}

	now = now.UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	start7d := now.Add(-7 * 24 * time.Hour)
	start30d := now.Add(-30 * 24 * time.Hour)
	windows := map[string]TokenHarborUsageWindowTotals{
		"today": {},
		"7d":    {},
		"30d":   {},
	}
	add := func(w *TokenHarborUsageWindowTotals, tokensIn, tokensOut float64) {
		w.Requests++
		w.TokensIn += tokensIn
		w.TokensOut += tokensOut
	}
	for _, row := range records[1:] {
		if len(row) < len(header) {
			// csv.Reader 默认按表头列数严格校验，走到这里说明首行就是宽行；
			// 直接失败关闭。
			return TokenHarborUsageSnapshot{}, fmt.Errorf("tokenharbor usage csv row width %d mismatches header %d", len(row), len(header))
		}
		if strings.TrimSpace(strings.Join(row, "")) == "" {
			continue
		}
		ts, err := parseTokenHarborUsageTime(row[column["time"]])
		if err != nil {
			return TokenHarborUsageSnapshot{}, err
		}
		if ts.After(now) {
			return TokenHarborUsageSnapshot{}, fmt.Errorf("tokenharbor usage csv row timestamp %q is in the future", row[column["time"]])
		}
		tokensIn, err := parseTokenHarborUsageCount(row[column["tokens_in"]])
		if err != nil {
			return TokenHarborUsageSnapshot{}, err
		}
		tokensOut, err := parseTokenHarborUsageCount(row[column["tokens_out"]])
		if err != nil {
			return TokenHarborUsageSnapshot{}, err
		}
		if !ts.Before(todayStart) {
			w := windows["today"]
			add(&w, tokensIn, tokensOut)
			windows["today"] = w
		}
		if !ts.Before(start7d) {
			w := windows["7d"]
			add(&w, tokensIn, tokensOut)
			windows["7d"] = w
		}
		if !ts.Before(start30d) {
			w := windows["30d"]
			add(&w, tokensIn, tokensOut)
			windows["30d"] = w
		}
	}
	return TokenHarborUsageSnapshot{Windows: windows, FetchedAt: now}, nil
}

// parseTokenHarborUsageTime 解析逐笔行时间：RFC3339 优先，兼容
// "2006-01-02 15:04:05"（按 UTC 解读——CSV 导出无时区标注时官方为 UTC 计数）。
func parseTokenHarborUsageTime(raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, fmt.Errorf("tokenharbor usage csv row has empty timestamp")
	}
	if ts, err := time.Parse(time.RFC3339, value); err == nil {
		return ts.UTC(), nil
	}
	if ts, err := time.ParseInLocation("2006-01-02 15:04:05", value, time.UTC); err == nil {
		return ts, nil
	}
	return time.Time{}, fmt.Errorf("tokenharbor usage csv row timestamp %q is not RFC3339 or UTC datetime", value)
}

// parseTokenHarborUsageCount 解析计数列（tokens 只做计数）。
//
// 空串按官方 CSV 语义计 0（D-TH-03）：新表头错误行的 tokens in/out 为空是上游
// "该请求失败无 token 计数"的确定性表达，列存在且值为空非数据损坏，应如实解析
// （请求计数 +1、tokens 计 0），不失败关闭。非数字、负数仍失败关闭（与外审 F3
// 范围校验口径一致）。
func parseTokenHarborUsageCount(raw string) (float64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	count, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("tokenharbor usage csv token count %q is not a number", value)
	}
	if count < 0 {
		return 0, fmt.Errorf("tokenharbor usage csv token count %q is negative", value)
	}
	return count, nil
}
