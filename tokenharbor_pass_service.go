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
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// TokenHarborPassSnapshotExtraKey 账号 Extra 中存放订阅快照的键。
	TokenHarborPassSnapshotExtraKey = "th_pass_snapshot"
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

	cookie, ok := s.cachedSession(account.ID)
	if !ok {
		var err error
		cookie, err = s.loginToTokenHarbor(ctx, account.ID, proxyURL, email, password)
		if err != nil {
			return TokenHarborPassSnapshot{}, err
		}
		s.storeSession(account.ID, cookie)
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
